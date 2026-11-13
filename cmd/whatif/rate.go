package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// rateAnswer is question 1's answer for one decode variant: nil result with
// no TPOT asked. fleet is its P/D fleet when a TTFT was asked, ttft the
// prefill role's rate and fleetRate the fleet's.
type rateAnswer struct {
	pv              pricedVariant
	result          whatif.Result
	fleet           *pricedFleet
	ttft, fleetRate whatif.Result
}

// runSLOToRate answers question 1 (docs/proposals/slo-capacity-planner.md,
// "Question 1: from an SLO to a rate") for decode: the largest rate the
// replicas carry at the TPOT target, capped at the operating point.
func runSLOToRate(ctx context.Context, args []string, out, warn io.Writer) error {
	return asJSONError(rateQuestion, args, out, sloToRate(ctx, args, out, warn))
}

func sloToRate(ctx context.Context, args []string, out, warn io.Writer) error {
	fs := newAnswerFlags(rateQuestion, args, out)
	f := answerFlags{question: rateQuestion, warn: warn}
	f.register(fs)
	tpot := fs.String("tpot", "", "the TPOT target: a mean (4ms) or a per-request percentile (4ms@p90) (required, unless --ttft is given)")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	if err := f.check(fs); err != nil {
		return err
	}
	ttft, err := f.rateTTFT()
	if err != nil {
		return err
	}
	var target whatif.TPOTTarget
	asksTPOT := *tpot != "" || ttft == nil
	if asksTPOT {
		if target, err = parseTPOT(*tpot); err != nil {
			return err
		}
	}
	if f.set["assume-tpot-ratio"] && target.Percentile == 0 {
		return usagef("--assume-tpot-ratio needs a percentile TPOT target (--tpot 4ms@p90)")
	}
	st, err := f.readState(ctx)
	if err != nil {
		return err
	}
	variants, err := f.decodeVariants(st)
	if err != nil {
		return err
	}
	var fleets []*pricedFleet
	if ttft != nil {
		if fleets, err = f.ttftFleets(st, *ttft, variants); err != nil {
			return err
		}
	}

	var r resolvedRatio
	if q := target.Percentile; q != 0 {
		r = f.ratioFor(st, q)
		target.Ratio = r.ratio
	}
	query := rateQuery(target, f.headroomFor(st.Policy), r)

	answers := make([]rateAnswer, 0, len(variants))
	results := make([]whatif.Result, 0, len(variants)+1)
	for i, pv := range variants {
		a := rateAnswer{pv: pv}
		if asksTPOT {
			a.result = decodeRate(pv, query, r)
			results = append(results, a.result)
		}
		if ttft != nil {
			a.fleet = fleets[i]
			a.priceTTFT(*ttft, query.Headroom)
			results = append(results, a.ttft, a.fleetRate)
		}
		answers = append(answers, a)
	}
	none := noDecode(st, variants)
	if none != nil && asksTPOT {
		results = append(results, *none)
	} else {
		none = nil
	}
	// With no decode variant the prefill role still answers.
	var alone *rateAnswer
	if ttft != nil && len(variants) == 0 {
		alone = &rateAnswer{fleet: fleets[0]}
		alone.priceTTFT(*ttft, query.Headroom)
		results = append(results, alone.ttft, alone.fleetRate)
	}
	code := whatif.ExitCode(results...)

	if f.output == outputJSON {
		w := newWireOutput(rateQuestion, &f, st, variants, code)
		w.addTTFTNote(fleets)
		for _, a := range answers {
			in := inputsOf(&f, a.pv)
			h := query.Headroom
			in.Headroom = &h
			if asksTPOT {
				in.TPOT = f.tpotInputs(st, target.Seconds, target.Percentile, target.Ratio)
				w.Roles = append(w.Roles, wireRole{Role: "decode", Variant: a.pv.state.Variant, Inputs: in,
					Results: wireResults{Rate: toWire(a.result, unitRate)}})
			}
			w.Roles = append(w.Roles, a.wireTTFT()...)
		}
		if none != nil {
			w.Roles = append(w.Roles, wireRole{Role: "decode", Results: wireResults{Rate: toWire(*none, unitRate)}})
		}
		if alone != nil {
			w.Roles = append(w.Roles, alone.wireTTFT()...)
		}
		if err := writeJSON(out, w); err != nil {
			return err
		}
	} else if _, err := io.WriteString(out, f.sourceLine(st)); err != nil {
		return err
	} else if err := writeRateText(out, st, variants, answers, none, alone, fleets, ttft); err != nil {
		return err
	}
	if code != whatif.ExitAnswered {
		return exitStatus(code)
	}
	return nil
}

// rateQuery is question 1's decode query: the target at R_q, priced with a
// placeholder R_q where R_q was refused, so that the library still judges the
// rest of the question; decodeRate replaces the answer by the refusal.
func rateQuery(target whatif.TPOTTarget, headroom float64, r resolvedRatio) whatif.RateQuery {
	query := whatif.RateQuery{Target: target, Headroom: headroom}
	if r.refused != nil {
		query.Target.Ratio = placeholderRatio
	}
	return query
}

// decodeRate is question 1's decode rate on one variant: the rate at the
// target, banded, qualified, and saying where R_q came from.
func decodeRate(pv pricedVariant, query whatif.RateQuery, r resolvedRatio) whatif.Result {
	res := withRatioRefusal(whatif.RateForTPOT(pv.decode, query), r.refused)
	res = pv.band(res, func(d whatif.Decode) whatif.Result { return whatif.RateForTPOT(d, query) })
	return pv.qualify(onRatio(res, r.ratio, r.flags))
}

// priceTTFT prices the prefill role's rate at the target, and the fleet's.
func (a *rateAnswer) priceTTFT(t ttftQuestion, headroom float64) {
	corners := a.priceTTFTRate(t)
	a.fleetRate = a.fleetAt(t, headroom, a.result, corners)
}

// priceTTFTRate prices the prefill role's rate at the target, with its span,
// and returns the rate at each corner of the span.
func (a *rateAnswer) priceTTFTRate(t ttftQuestion) []whatif.Result {
	price := func(fl whatif.TTFTFleet) whatif.Result {
		return whatif.RateForTTFT(fl, whatif.TTFTTarget{Seconds: t.seconds, Percentile: t.q})
	}
	corners := a.fleet.corners(price)
	a.ttft = withSpan(a.fleet.priced(price), corners)
	a.fleet.noteAssumedTerm(a.ttft, price)
	return corners
}

// fleetAt is the fleet's rate at headroom, from decode's rate (nil with no
// TPOT asked) and the prefill role's, spanned over the prefill role's corners.
func (a *rateAnswer) fleetAt(t ttftQuestion, headroom float64, decode whatif.Result, corners []whatif.Result) whatif.Result {
	spans := make([]whatif.Result, len(corners))
	for i, c := range corners {
		spans[i] = fleetRate(t, headroom, decode, c)
	}
	return withSpan(fleetRate(t, headroom, decode, a.ttft), spans)
}

// wireTTFT is the answer's prefill and fleet roles, none with no TTFT asked.
func (a *rateAnswer) wireTTFT() []wireRole {
	if a.fleet == nil {
		return nil
	}
	decode := ""
	if a.fleet.pv != nil {
		decode = a.fleet.pv.state.Variant
	}
	return []wireRole{
		{Role: rolePrefill, Variant: a.fleet.variant, TTFT: &a.fleet.in, Results: wireResults{Rate: toWire(a.ttft, unitRate)}},
		{Role: "fleet", Variant: decode, Results: wireResults{Rate: toWire(a.fleetRate, unitRate)}},
	}
}

// parseTPOT reads --tpot: a duration, and optionally "@p" and a percentile
// in percent.
func parseTPOT(s string) (whatif.TPOTTarget, error) {
	if s == "" {
		return whatif.TPOTTarget{}, usagef("--tpot is required: a mean (4ms) or a per-request percentile (4ms@p90)")
	}
	value, pct, hasPct := strings.Cut(s, "@")
	d, err := time.ParseDuration(value)
	if err != nil {
		return whatif.TPOTTarget{}, usagef("--tpot %q: %v", s, err)
	}
	t := whatif.TPOTTarget{Seconds: d.Seconds()}
	if hasPct {
		if !strings.HasPrefix(pct, "p") {
			return whatif.TPOTTarget{}, usagef("--tpot %q: a percentile is written @p90", s)
		}
		if t.Percentile, err = parsePercent(pct); err != nil {
			return whatif.TPOTTarget{}, usagef("--tpot %q: %v", s, err)
		}
	}
	return t, nil
}

// writeRateText is question 1's human output, in the page's layout.
func writeRateText(out io.Writer, st *learnreplay.State, variants []pricedVariant, answers []rateAnswer, none *whatif.Unanswerable,
	alone *rateAnswer, fleets []*pricedFleet, ttft *ttftQuestion) error {
	var b strings.Builder
	const layout = "%-9s%-19s%-41s%s\n"
	fmt.Fprintf(&b, layout, "role", "est. rate at SLO", "bound by", "basis")
	for _, a := range answers {
		role := "decode"
		if len(answers) > 1 {
			role += "/" + a.pv.state.Variant
		}
		if a.result == nil {
			writeRateRows(&b, layout, a.fleet, len(fleets), *ttft, a.ttft, a.fleetRate)
			flagsLine(&b, a.pv, a.ttft, a.fleetRate)
			continue
		}
		switch r := a.result.(type) {
		case whatif.Answer:
			by := boundText(r.Basis.BoundBy)
			if len(r.Basis.BoundBy) > 0 && r.Basis.BoundBy[0] == whatif.BoundOperatingPoint && r.Basis.Uncapped > r.Value {
				by += fmt.Sprintf(" (TPOT alone: %.2f)", r.Basis.Uncapped)
			}
			basis := r.Basis.Tier
			if r.Band != nil {
				basis += fmt.Sprintf(" [%.1f .. %.1f]", r.Band.Lo, r.Band.Hi)
			}
			fmt.Fprintf(&b, layout, role, rateText(r.Value), by, strings.TrimSpace(basis))
		case whatif.Unanswerable:
			fmt.Fprintf(&b, "%-9s%s\n", role, refusedText(r))
		}
		fmt.Fprintf(&b, "%-9sshape %s\n", "", a.pv.shapeText())
		checkRow(&b, "%-9sline check %s\n", "", a.pv)
		if a.fleet != nil {
			writeRateRows(&b, layout, a.fleet, len(fleets), *ttft, a.ttft, a.fleetRate)
			flagsLine(&b, a.pv, a.result, a.ttft, a.fleetRate)
			continue
		}
		flagsLine(&b, a.pv, a.result)
	}
	if none != nil {
		fmt.Fprintf(&b, "%-9s%s\n", "decode", refusedText(*none))
	}
	if alone != nil {
		writeRateRows(&b, layout, alone.fleet, len(fleets), *ttft, alone.ttft, alone.fleetRate)
		flagsLine(&b, pricedVariant{}, alone.ttft, alone.fleetRate)
	}
	b.WriteString(footer(st, variants) + "\n")
	if len(fleets) > 0 {
		b.WriteString(footerNote(fleets) + "\n")
	}
	_, err := io.WriteString(out, b.String())
	return err
}
