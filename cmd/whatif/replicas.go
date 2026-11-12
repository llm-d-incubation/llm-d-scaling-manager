package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// replicasAnswer is question 2's answer for one decode variant.
type replicasAnswer struct {
	pv  pricedVariant
	slo whatif.DecodeSLO
	// ratio is R_q, when a percentile was asked and resolved.
	ratio whatif.TPOTRatio
	// query is the question as priced: the prefill knee may set its rate.
	// ttft is the TTFT asked, nil with none; headroom the operating point's
	// fraction of the knee.
	query    whatif.ReplicasQuery
	ttft     *operatingTTFT
	headroom float64
}

// runReplicasToSLO answers question 2 (docs/proposals/slo-capacity-planner.md,
// "Question 2: from replicas to an SLO") for decode: the knee the replicas
// saturate at, the operating rate below it, and the TPOT held there.
func runReplicasToSLO(ctx context.Context, args []string, out, warn io.Writer) error {
	return asJSONError(replicasQuestion, args, out, replicasToSLO(ctx, args, out, warn))
}

func replicasToSLO(ctx context.Context, args []string, out, warn io.Writer) error {
	fs := newAnswerFlags(replicasQuestion, args, out)
	f := answerFlags{question: replicasQuestion, warn: warn}
	f.register(fs)
	rate := fs.Float64("rate", 0, "the operating rate, requests per second over the fleet, in place of headroom x knee")
	pct := fs.String("tpot-percentile", "", "report the TPOT at this per-request percentile too, in percent (90)")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	if err := f.check(fs); err != nil {
		return err
	}
	if f.set["rate"] && !(*rate > 0 && finite(*rate)) {
		return usagef("--rate %g, want a finite rate above 0", *rate)
	}
	if f.set["assume-tpot-ratio"] && *pct == "" {
		return usagef("--assume-tpot-ratio needs --tpot-percentile")
	}
	var q float64
	if *pct != "" {
		var err error
		if q, err = parsePercent(*pct); err != nil {
			return usagef("--tpot-percentile %q: %v", *pct, err)
		}
	}
	ttft, err := f.replicasTTFT()
	if err != nil {
		return err
	}
	st, err := f.readState(ctx)
	if err != nil {
		return err
	}
	variants, err := f.decodeVariants(st)
	if err != nil {
		return err
	}
	ttft.policyPercentile(st)
	var fleets []*pricedFleet
	if ttft != nil {
		if fleets, err = f.ttftFleets(st, *ttft, variants); err != nil {
			return err
		}
	}

	query := whatif.ReplicasQuery{}
	if f.set["rate"] {
		query.Rate = *rate
		if f.set["headroom"] {
			query.Headroom = f.headroom // both set: the library refuses it
		}
	} else {
		query.Headroom = f.headroomFor(st.Policy)
	}
	var r resolvedRatio
	if *pct != "" {
		r = f.ratioFor(st, q)
		query.PercentileTarget = percentileTarget(q, r)
	}

	answers := make([]replicasAnswer, 0, len(variants))
	var results []whatif.Result
	h := f.headroomFor(st.Policy)
	var given *float64
	if f.set["rate"] {
		given = rate
	}
	for i, pv := range variants {
		var fleet *pricedFleet
		if ttft != nil {
			fleet = fleets[i]
		}
		a, priced := replicasAt(pv, fleet, ttft, query, given, h, r)
		answers = append(answers, a)
		results = append(results, priced...)
	}
	none := noDecode(st, variants)
	if none != nil {
		results = append(results, *none)
	}
	// With no decode variant the prefill role still answers.
	var alone *operatingTTFT
	if ttft != nil && len(variants) == 0 {
		o, _ := fleets[0].atOperatingPoint(*ttft, query, given, h, nil)
		alone = &o
		results = append(results, o.results()...)
	}
	code := whatif.ExitCode(results...)

	if f.output == outputJSON {
		w := newWireOutput(replicasQuestion, &f, st, variants, code)
		w.addTTFTNote(fleets)
		for _, a := range answers {
			in := inputsOf(&f, a.pv)
			if f.set["rate"] {
				in.Rate = rate
			} else {
				h := a.headroom
				in.Headroom = &h
			}
			if a.ttft != nil && a.ttft.prefillBinds {
				in.Rate, in.RateSource = &a.ttft.rate, sourcePrefillKnee
			}
			if query.PercentileTarget != nil {
				in.TPOT = f.tpotInputs(st, 0, q, a.ratio)
			}
			w.Roles = append(w.Roles, wireRole{Role: "decode", Variant: a.pv.state.Variant, Inputs: in, Results: wireResults{
				Knee: toWire(a.slo.KneeRate, unitRate), OperatingRate: toWire(a.slo.Rate, unitRate),
				MeanTPOT: toWire(a.slo.MeanTPOT, unitSeconds), PercentileTPOT: toWire(a.slo.PercentileTPOT, unitSeconds),
			}})
			if a.ttft != nil {
				w.Roles = append(w.Roles, a.ttft.wire())
			}
		}
		if none != nil {
			w.Roles = append(w.Roles, wireRole{Role: "decode", Results: wireResults{Knee: toWire(*none, unitRate)}})
		}
		if alone != nil {
			w.Roles = append(w.Roles, alone.wire())
		}
		if err := writeJSON(out, w); err != nil {
			return err
		}
	} else if _, err := io.WriteString(out, f.sourceLine(st)); err != nil {
		return err
	} else if err := writeReplicasText(out, &f, st, variants, answers, none, alone, fleets, *pct); err != nil {
		return err
	}
	if code != whatif.ExitAnswered {
		return exitStatus(code)
	}
	return nil
}

// percentileTarget is question 2's percentile at q, priced on R_q, or on a
// placeholder R_q where R_q was refused, so that the library still judges the
// rest of the question (withRatioRefusal).
func percentileTarget(q float64, r resolvedRatio) *whatif.TPOTTarget {
	t := &whatif.TPOTTarget{Percentile: q, Ratio: r.ratio}
	if r.refused != nil {
		t.Ratio = placeholderRatio
	}
	return t
}

// replicasAt prices question 2 on one decode variant: with a TTFT asked, the
// fleet at its operating point first (fleet and ttft nil with none), then
// decode's knee, rate and TPOTs at the rate that sets, or at query's. priced
// is every Result the exit code reads, in the order the command reads them.
func replicasAt(pv pricedVariant, fleet *pricedFleet, ttft *ttftQuestion, query whatif.ReplicasQuery, given *float64, h float64,
	r resolvedRatio) (replicasAnswer, []whatif.Result) {
	var priced []whatif.Result
	var at *operatingTTFT
	if ttft != nil {
		var o operatingTTFT
		o, query = fleet.atOperatingPoint(*ttft, query, given, h, whatif.KneeRate(pv.decode))
		at = &o
		priced = append(priced, o.results()...)
	}
	slo := whatif.SLOAtReplicas(pv.decode, query)
	if slo.PercentileTPOT != nil {
		slo.PercentileTPOT = withRatioRefusal(slo.PercentileTPOT, r.refused)
	}
	on := func(pick func(whatif.DecodeSLO) whatif.Result) func(whatif.Decode) whatif.Result {
		return func(d whatif.Decode) whatif.Result { return pick(whatif.SLOAtReplicas(d, query)) }
	}
	slo.KneeRate = pv.qualify(pv.band(slo.KneeRate, whatif.KneeRate))
	// A rate --rate gives is an input, not an estimate: it has no band.
	// A rate the prefill knee sets has none either: V_P is not resampled.
	if query.Rate == 0 {
		slo.Rate = pv.band(slo.Rate, on(func(s whatif.DecodeSLO) whatif.Result { return s.Rate }))
	}
	slo.Rate = pv.qualify(slo.Rate)
	slo.MeanTPOT = pv.qualify(pv.band(slo.MeanTPOT, on(func(s whatif.DecodeSLO) whatif.Result { return s.MeanTPOT })))
	if at != nil {
		slo.Rate, slo.MeanTPOT = at.onRate(at.onDecode(slo.Rate)), at.onDecode(slo.MeanTPOT)
	}
	if slo.PercentileTPOT != nil {
		p := pv.band(slo.PercentileTPOT, on(func(s whatif.DecodeSLO) whatif.Result { return s.PercentileTPOT }))
		slo.PercentileTPOT = pv.qualify(onRatio(p, r.ratio, r.flags))
		if at != nil {
			slo.PercentileTPOT = at.onDecode(slo.PercentileTPOT)
		}
	}
	a := replicasAnswer{pv: pv, slo: slo, ratio: r.ratio, query: query, ttft: at, headroom: h}
	return a, append(priced, slo.KneeRate, slo.Rate, slo.MeanTPOT, slo.PercentileTPOT)
}

// noDecode is the refusal for a cycle with no decode variant to price,
// nil when there is one.
func noDecode(st *learnreplay.State, variants []pricedVariant) *whatif.Unanswerable {
	if len(variants) > 0 {
		return nil
	}
	return &whatif.Unanswerable{Reason: whatif.NoDecodeReplicas,
		Detail: fmt.Sprintf("no decode variant priced at cycle %d of %s/%s", st.Cycle, st.Namespace, st.ModelID)}
}

// tpotInputs is the wire's account of the TPOT question.
func (f *answerFlags) tpotInputs(st *learnreplay.State, target, q float64, r whatif.TPOTRatio) *wireTPOT {
	t := &wireTPOT{TargetSeconds: target, Percentile: q, Ratio: r.Value, Windows: f.tpotWindowCount(st)}
	switch {
	case r.Value == 0:
	case r.Assumed:
		t.RatioSource = "assumed"
	case f.engineTPOT:
		t.RatioSource = "engine histogram"
	default:
		t.RatioSource = sourceLearned
	}
	return t
}

// writeReplicasText is question 2's human output, in the page's layout.
func writeReplicasText(out io.Writer, f *answerFlags, st *learnreplay.State, variants []pricedVariant, answers []replicasAnswer,
	none *whatif.Unanswerable, alone *operatingTTFT, fleets []*pricedFleet, pct string) error {
	var b strings.Builder
	for i, a := range answers {
		if len(answers) > 1 {
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "variant %s (%s)\n", a.pv.state.Variant, a.pv.state.Role)
		}
		if u, ok := a.slo.KneeRate.(whatif.Unanswerable); ok {
			fmt.Fprintf(&b, "%-16s%s\n", "decode", refusedText(u))
			fmt.Fprintf(&b, "%-16s%s\n", "shape", a.pv.shapeText())
			checkRow(&b, "%-16s%s\n", "line check", a.pv)
			if a.ttft != nil {
				a.ttft.writeRows(&b)
				flagsLine(&b, a.pv, a.ttft.results()...)
			}
			continue
		}
		knee := a.slo.KneeRate.(whatif.Answer)
		row(&b, "knee", rateText(knee.Value), "", fmt.Sprintf("decode, %s binds at fill %.3f", boundText(knee.Basis.BoundBy), knee.Basis.Fill))
		switch r := a.slo.Rate.(type) {
		case whatif.Answer:
			why := "--rate"
			if !f.set["rate"] {
				src := "scaleUpThreshold"
				if f.set["headroom"] {
					src = "--headroom"
				}
				of := "knee"
				if a.ttft != nil && a.ttft.prefillBinds {
					of = "prefill knee"
				}
				why = fmt.Sprintf("%s x %s (%s)", num(a.headroom), of, src)
				if a.ttft != nil && a.ttft.prefillBinds {
					why += "; " + a.ttft.fleet.verdict()
				}
			}
			row(&b, "operating rate", rateText(r.Value), spanText(r.Sensitivity, "%.2f", 1, false), why)
		case whatif.Unanswerable:
			fmt.Fprintf(&b, "%-16s%s\n", "operating rate", refusedText(r))
		}
		tpotRow(&b, "mean TPOT", a.slo.MeanTPOT, lineText(a.pv)+a.ttft.decodeNote())
		if a.slo.PercentileTPOT != nil {
			tpotRow(&b, "TPOT p"+strings.TrimPrefix(pct, "p"), a.slo.PercentileTPOT, "per request; "+f.ratioNote(st, a.ratio)+a.ttft.decodeNote())
		}
		if a.ttft != nil {
			a.ttft.writeRows(&b)
		}
		fmt.Fprintf(&b, "%-16s%s\n", "shape", a.pv.shapeText())
		checkRow(&b, "%-16s%s\n", "line check", a.pv)
		results := []whatif.Result{a.slo.KneeRate, a.slo.Rate, a.slo.MeanTPOT, a.slo.PercentileTPOT}
		if a.ttft != nil {
			results = append(results, a.ttft.results()...)
		}
		flagsLine(&b, a.pv, results...)
	}
	if none != nil {
		fmt.Fprintf(&b, "%-16s%s\n", "decode", refusedText(*none))
	}
	if alone != nil {
		alone.writeRows(&b)
		flagsLine(&b, pricedVariant{}, alone.results()...)
	}
	b.WriteString(footer(st, variants) + "\n")
	if len(fleets) > 0 {
		b.WriteString(footerNote(fleets) + "\n")
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// checkRow prints the line check's row, none with no line.
func checkRow(b *strings.Builder, layout, label string, pv pricedVariant) {
	if s := pv.checkText(); s != "" {
		fmt.Fprintf(b, layout, label, s)
	}
}

// tpotRow is one TPOT row: the value in ms, its band, and its basis.
func tpotRow(b *strings.Builder, label string, r whatif.Result, basis string) {
	switch v := r.(type) {
	case whatif.Answer:
		band := ""
		if v.Band != nil {
			band = fmt.Sprintf("[%.2f .. %.2f]", 1000*v.Band.Lo, 1000*v.Band.Hi)
		}
		row(b, label, fmt.Sprintf("%.2f ms", 1000*v.Value), band, basis)
	case whatif.Unanswerable:
		fmt.Fprintf(b, "%-16s%s\n", label, refusedText(v))
	}
}

// row is one line of question 2's output: the label, the value, and, for a
// banded figure, the band, then the basis.
func row(b *strings.Builder, label, value, band, basis string) {
	if band == "" && !strings.HasPrefix(label, "mean") && !strings.HasPrefix(label, "TPOT") && !strings.HasPrefix(label, "TTFT") {
		fmt.Fprintf(b, "%-16s%-12s%s\n", label, value, basis)
		return
	}
	fmt.Fprintf(b, "%-16s%-12s%-16s %s\n", label, value, band, basis)
}
