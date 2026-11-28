package main

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The report on one fleet (capacity-planner at-replicas, POST /v1/report):
// one replay of the recording, then every figure, for every shape and every
// load, priced from the replayed state on the paths replicas-to-slo and
// slo-to-rate take (servedState, servedFleet). A stated shape is priced as
// --assume-input-tokens and --assume-output-tokens price one. The report adds
// no model of its own: the verdict compares those figures.

// reportQuestion is the command's name, as the JSON names its question.
const reportQuestion = "at-replicas"

// defaultLoads are the tables' rates when none is asked.
var defaultLoads = []string{"50%", "80%", "100%", "seen"}

// tailPastHistogram is a TTFT percentile above maxPromptTailQ on a prompt
// spread no histogram measured: the tail rests on the assumed spread.
const tailPastHistogram whatif.Flag = "TailPastPromptHistogram"

// extrapolatedShape is a stated shape whose IL or OL lies outside the range
// of per-cycle means the recording saw.
const extrapolatedShape whatif.Flag = "ExtrapolatedShape"

// decodeOnly is a report whose prefill side does not price: its verdict and
// its figures judge decode alone, and it gives no fleet rate at the SLO.
const decodeOnly whatif.Flag = "DecodeOnly"

// assumeInputTokensWords names the flag a stated prompt length came from in
// the assumed words (ttft.go); the report says --shape in its place.
const assumeInputTokensWords = "(--assume-input-tokens)"

// maxPromptTailQ is the highest TTFT percentile priced without the flag on an
// assumed or clamped prompt spread: p95, the highest the SLO analyzer's
// targets are judged at on the goldens.
const maxPromptTailQ = 0.95

// cellTTFTQ is the TTFT percentile a summary cell prints with no TTFT target.
const cellTTFTQ = 0.9

// decodeKneeGoldensLow and decodeKneeGoldensHigh are the lowest and the
// highest of the decode knees that read high on the 0.6B goldens and past
// the table's own bound (OUT in testdata/decode-validation.golden,
// docs/proposals/slo-capacity-planner.md, "Measured on the goldens"; a test
// pins them to the file): the error a decode knee outside its region is
// sized at.
const (
	decodeKneeGoldensLow  = 0.43
	decodeKneeGoldensHigh = 0.991
)

// decodeKneeOutside says what a decode knee outside its validated region
// is worth: what it read inside the region the envelope measured, from the
// shipped rows, and what it read on the 0.6B goldens outside it. Each
// figure's own label says which applies (#297).
func decodeKneeOutside() string {
	in := "no run validated it"
	for _, row := range whatif.DefaultEnvelope().Rows {
		if row.Figure == whatif.FigureDecodeKnee {
			in = fmt.Sprintf("inside the region the %s measured (%s) it read %s", row.Source, row.Shape,
				whatif.ErrorText(row.ErrorLo, row.ErrorHi))
			break
		}
	}
	return fmt.Sprintf("decode knee: outside its validated region (see its label); %s, and outside it, on the 0.6B goldens, "+
		"%.0f%% to %.0f%% high (docs/proposals/slo-capacity-planner.md, Measured on the goldens)", in,
		decodeKneeGoldensLow*100, decodeKneeGoldensHigh*100)
}

// e2eNote says what the e2e column is.
const e2eNote = "e2e mean is a sum, TTFT mean + (OL - 1) x TPOT mean, not a percentile; in system is the rate times it (Little's law), the requests in the system on average"

// noSLONote is the verdict's basis with no target at all.
const noSLONote = "no SLO target (the policy sets none; pass --ttft or --tpot): judged on headroom alone"

// reporter prices one report from one replayed state.
type reporter struct {
	// ctx is the report's context: every fleet priced reads it first
	// (evaluateAt), so the needed search and the verdict stop when it ends.
	// Nil reads as never ending.
	ctx context.Context
	st  *learnreplay.State
	s   *servedState
	q   whatifapi.ReportQuery
	// headroom is the operating point's fraction of the fleet knee, and
	// headroomSource where it came from.
	headroom       float64
	headroomSource string
	// ttft and tpot are the targets judged by; nil with none.
	ttft, tpot             *whatifapi.ReportTarget
	ttftSource, tpotSource string
	// load is the rate judged at, 0 with none, and loadWords it in words
	// ("the p95 load of 41.7 r/s").
	load      float64
	loadWords string
	// loadStat is the statistic the load was read at ("p95", "stated").
	loadStat string
	// loadRefused is why the observed load was refused (gapAtPeak), nil
	// otherwise; load is then 0.
	loadRefused *whatif.Unanswerable
	// loadShape is the shape of the cycles the load was read from, nil with
	// a stated load or none read there (load_shape.go).
	loadShape *whatifapi.ReportLoadShape
	// peak is the window's peak a shrink must also hold at, 0 with a stated
	// load; brief shortens the verdict's sentence, for a growth line.
	peak   float64
	brief  bool
	fleets map[fleetKey]*servedFleet
	// shape is the index of the shape priced, sh the shape.
	shape int
	sh    whatifapi.ReportShapeAsk
	// results is every figure the shape prints, for its flags and the exit
	// code; assumed is every assumed input, once each.
	results []whatif.Result
	assumed []string
	pv      pricedVariant
	// fixedF, above zero, prices every fleet at that f on more than one
	// prefill pod: the needed search's sensitivity to f (fSensitivity).
	fixedF float64
	// pfVariant is the prefill variant the shape priced, "" with none.
	pfVariant string
	// kneeV is the envelope's verdict on the shape's decode knee, and
	// kneeOutside whether any shape's decode knee priced outside its region.
	kneeV       whatif.Validation
	kneeOutside bool
	// untranslated is each concurrency load the shape found no rate for.
	untranslated []whatifapi.ReportUntranslated
}

// fleetKey names a fleet priced: the shape, the counts and the statistics.
type fleetKey struct {
	shape        int
	np, nd       int
	ttftQ, tpotQ float64
	fixedF       float64
}

// Report is POST /v1/report on the replayed state.
func (s *servedState) Report(ctx context.Context, q whatifapi.ReportQuery) (*whatifapi.ReportResponse, error) {
	return buildReport(ctx, s.st, s, q)
}

// buildReport prices the report q asks of st: every shape and every load
// from the one replayed state.
func buildReport(ctx context.Context, st *learnreplay.State, s *servedState, q whatifapi.ReportQuery) (*whatifapi.ReportResponse, error) {
	r := &reporter{ctx: ctx, st: st, s: s, q: q, fleets: map[fleetKey]*servedFleet{}}
	r.resolveTargets()
	if len(r.q.Loads) == 0 {
		for _, l := range defaultLoads {
			a, _ := whatifapi.ParseReportLoad(l)
			r.q.Loads = append(r.q.Loads, a)
		}
	}
	// LoadSeenX is an alias for a growth load: a column and a verdict of its
	// own, never the load the fleet is judged at.
	if x := r.q.LoadSeenX; x > 0 && x != 1 && !slices.ContainsFunc(r.q.Loads, func(l whatifapi.ReportLoadAsk) bool {
		return l.Grown() && l.X == x
	}) {
		r.q.Loads = append(slices.Clone(r.q.Loads), whatifapi.GrowthLoad(x))
	}
	// The load is read first: a shape asked as the load's takes its lengths
	// from the cycles the load was read at.
	var load whatifapi.ReportLoad
	r.loadOf(&load)
	if len(r.q.Shapes) == 0 {
		r.q.Shapes = []whatifapi.ReportShapeAsk{defaultShape(r.loadShape)}
	}
	r.q.Shapes = slices.Clone(r.q.Shapes)
	if err := r.resolveLoadShapes(); err != nil {
		return nil, err
	}
	r.sh = r.q.Shapes[0]
	base, err := r.fleet(q.Prefill, q.Decode, r.sloTTFTQ(), r.sloTPOTQ())
	if err != nil {
		return nil, err
	}
	np, nd := r.counts(base)
	out := &whatifapi.ReportResponse{Question: reportQuestion, Shapes: []whatifapi.ReportShape{}, NotValidated: []string{}}
	out.Source = whatifapi.ReportSource{Namespace: st.Namespace, Model: st.ModelID, Cycle: st.Cycle, TotalCycles: st.Cycles,
		FirstAt: st.FirstAt, LastAt: st.At, Gaps: whatifapi.WindowGapsOf(st.CycleTimes)}
	out.Fleet = whatifapi.ReportFleet{Prefill: np, Decode: nd, PrefillVariant: base.pf.variant, DecodeVariant: base.pv.state.Variant,
		PrefillFrom: from(q.Prefill), DecodeFrom: from(q.Decode),
		DecodeEngine: engineInfo(&base.pv.state)}
	if base.pf.variant != "" {
		out.Fleet.PrefillEngine = engineInfo(variantState(st, base.pf.variant))
	}
	out.Load = load
	out.SLO = r.sloOf()
	for _, l := range r.q.Loads {
		out.Loads = append(out.Loads, l.Label)
	}
	code := whatif.ExitAnswered
	ttftPriced := false
	for i, sh := range r.q.Shapes {
		r.shape, r.sh, r.results, r.assumed = i, sh, nil, nil
		rs, c, priced, err := r.shapeReport(ctx, np, nd)
		if err != nil {
			return nil, err
		}
		out.Shapes = append(out.Shapes, rs)
		code = max(code, c)
		ttftPriced = ttftPriced || priced
	}
	if r.kneeOutside {
		out.NotValidated = append(out.NotValidated, decodeKneeOutside())
	}
	if ttftPriced {
		out.NotValidated = append(out.NotValidated, footerNote([]*pricedFleet{base.pf}))
		if n := busyFleetNote([]*pricedFleet{base.pf}); n != "" {
			out.NotValidated = append(out.NotValidated, n)
		}
	}
	out.NotValidated = append(out.NotValidated, e2eNote)
	out.ExitCode = code
	return out, nil
}

// defaultShape is the shape a report prices when none is asked: the load's
// own (whatifapi.ShapeLoad), the lengths of the cycles the load judged at was
// read from, so that the verdict prices the load at the traffic that made it;
// the measured shape where no cycle of the load read one (a stated load, or
// none read a shape).
func defaultShape(loadShape *whatifapi.ReportLoadShape) whatifapi.ReportShapeAsk {
	if loadShape == nil {
		return whatifapi.ReportShapeAsk{Measured: true, Label: "measured"}
	}
	return whatifapi.ReportShapeAsk{Load: true, Label: whatifapi.ShapeLoad}
}

// shapeReport prices the fleet at the shape r.sh: its exit code, and whether
// its TTFT priced.
func (r *reporter) shapeReport(ctx context.Context, np, nd int) (whatifapi.ReportShape, int, bool, error) {
	base, err := r.fleet(r.q.Prefill, r.q.Decode, r.sloTTFTQ(), r.sloTPOTQ())
	if err != nil {
		return whatifapi.ReportShape{}, 0, false, err
	}
	r.pv, r.pfVariant = base.pv, base.pf.variant
	if r.pv.bandFloor > 0 {
		r.addAssumed(adaptedBandFloorWords(r.pv))
	}
	if w := r.pv.busiestRankAssumed(); w != "" {
		r.addAssumed(w)
	}
	if w := r.pv.itlBWords(); w != "" {
		r.addAssumed(w)
	}
	if w := r.pv.itlBNotUsed(); w != "" {
		r.addAssumed(w)
	}
	rs := whatifapi.ReportShape{Label: r.sh.Label, InputTokens: r.pv.shape.AvgInputTokens, OutputTokens: r.pv.shape.AvgOutputTokens,
		PrefixHitRate: r.pv.shape.PrefixHitRate, Source: measuredText(r.pv), Flags: []string{}, Assumed: []string{}}
	if !r.sh.Measured {
		rs.Source = sourceAsked
		if r.sh.Load {
			rs.Source = sourceLoadCycles
		}
		if why := r.extrapolated(); why != "" {
			rs.Extrapolated, rs.ExtrapolatedWhy = true, why
			r.results = append(r.results, whatif.Answer{Flags: []whatif.Flag{extrapolatedShape}})
		}
	}
	r.addAssumed(base.Assumed()...)
	ev, err := r.evaluate(np, nd)
	if err != nil {
		return rs, 0, false, err
	}
	if err := ctx.Err(); err != nil {
		return rs, 0, false, err
	}
	if ev.kneeOK && !ev.prefillPriced {
		r.results = append(r.results, whatif.Answer{Flags: []whatif.Flag{decodeOnly}})
	}
	r.capacity(&rs.Capacity, base, ev)
	r.use(&rs.Utilization, base, ev)
	if rs.Table, err = r.table(ctx, ev); err != nil {
		return rs, 0, false, err
	}
	rs.Untranslated = r.untranslated
	if err := r.capacityConcurrency(&rs.Capacity, ev); err != nil {
		return rs, 0, false, err
	}
	if rs.Verdict, err = r.verdict(np, nd, ev); err != nil {
		return rs, 0, false, err
	}
	if rs.Growth, err = r.growth(np, nd); err != nil {
		return rs, 0, false, err
	}
	rs.Flags = flagNamesOf(r.results)
	r.shapeLoad(&rs)
	for _, w := range r.assumed {
		// A stated shape is priced as --assume-input-tokens prices a prompt.
		rs.Assumed = append(rs.Assumed, strings.ReplaceAll(w, assumeInputTokensWords, "(--shape)"))
	}
	if rs.Assumed == nil {
		rs.Assumed = []string{}
	}
	// The report answered unless the verdict could not be given: a
	// saturated figure is an answer (the fleet saturates), not a refusal.
	code := whatif.ExitAnswered
	if rs.Verdict.Verdict == whatifapi.VerdictNotJudged {
		code = whatif.ExitRefused
	}
	return rs, code, base.pf.refused == nil, nil
}

// extrapolated says how a stated shape lies outside the per-cycle means the
// recording saw; "" inside them.
func (r *reporter) extrapolated() string {
	rg := r.pv.state.ShapeRange
	if rg.Cycles == 0 {
		return "the recording saw no traffic to compare it with"
	}
	var out []string
	if il := r.sh.IL; il < rg.ILMin || il > rg.ILMax {
		out = append(out, fmt.Sprintf("IL %s outside the %s-%s seen", num(round(il)), num(round(rg.ILMin)), num(round(rg.ILMax))))
	}
	if ol := r.sh.OL; ol < rg.OLMin || ol > rg.OLMax {
		out = append(out, fmt.Sprintf("OL %s outside the %s-%s seen", num(round(ol)), num(round(rg.OLMin)), num(round(rg.OLMax))))
	}
	// A stated hit rate is extrapolated past the one seen as a length is; the
	// tolerance only absorbs the per-cycle means' rounding.
	if h := r.sh.Hit; h != nil && (*h < rg.HitMin-hitTolerance || *h > rg.HitMax+hitTolerance) {
		out = append(out, fmt.Sprintf("prefix hit %.2f outside the %.2f-%.2f seen", *h, rg.HitMin, rg.HitMax))
	}
	return strings.Join(out, "; ")
}

// growth judges the fleet at each growth load asked, the load seen times X:
// a what-if beside the verdict at the load seen, which it never changes.
func (r *reporter) growth(np, nd int) ([]whatifapi.ReportGrowth, error) {
	out := []whatifapi.ReportGrowth{}
	if !(r.load > 0) {
		return out, nil
	}
	seen, words, peak := r.load, r.loadWords, r.peak
	defer func() { r.load, r.loadWords, r.peak, r.brief = seen, words, peak, false }()
	r.brief = true
	for _, l := range r.q.Loads {
		if !l.Grown() {
			continue
		}
		r.load, r.peak = l.X*seen, l.X*peak
		r.loadWords = fmt.Sprintf("%s x the %s load (%s)", num(l.X), statWords(r.loadStat), rateWords(r.load))
		ev, err := r.evaluate(np, nd)
		if err != nil {
			return nil, err
		}
		v, err := r.verdict(np, nd, ev)
		if err != nil {
			return nil, err
		}
		out = append(out, whatifapi.ReportGrowth{Load: l.Label, X: l.X, Rate: r.load, Verdict: v})
	}
	return out, nil
}

// hitTolerance is how far a stated prefix-hit rate may lie outside the range
// seen and not be extrapolated.
const hitTolerance = 0.005

// from names where a count came from: 0 is the recording's.
func from(asked int) string {
	if asked > 0 {
		return sourceAsked
	}
	return "recording"
}

// resolveTargets reads the headroom and the targets: the query's, else the
// policy's.
func (r *reporter) resolveTargets() {
	p := r.st.Policy
	switch {
	case r.q.Headroom > 0:
		r.headroom, r.headroomSource = r.q.Headroom, sourceAsked
	case p != nil && p.ScaleUpThreshold > 0:
		r.headroom, r.headroomSource = p.ScaleUpThreshold, "scaleUpThreshold"
	default:
		r.headroom, r.headroomSource = config.DefaultScaleUpThreshold, "default"
	}
	r.ttft, r.ttftSource = r.q.TTFT, sourceAsked
	if r.ttft == nil && p != nil && p.SLO != nil && p.SLO.TTFT != nil {
		r.ttft = &whatifapi.ReportTarget{Seconds: p.SLO.TTFT.Target.Seconds(), Percentile: float64(p.SLO.TTFTPercentile()) / 100}
		r.ttftSource = "policy"
	}
	r.tpot, r.tpotSource = r.q.TPOT, sourceAsked
	if r.tpot == nil && p != nil && p.SLO != nil && p.SLO.TPOT != nil {
		r.tpot = &whatifapi.ReportTarget{Seconds: p.SLO.TPOT.Target.Seconds()}
		r.tpotSource = "policy"
	}
	if len(r.q.TTFTPercentiles) == 0 {
		r.q.TTFTPercentiles = []float64{0.5, 0.9}
	}
}

// sloTTFTQ is the TTFT statistic the target is judged at: the target's, else
// the first the table asks for.
func (r *reporter) sloTTFTQ() float64 {
	if r.ttft != nil {
		return r.ttft.Percentile
	}
	return r.q.TTFTPercentiles[0]
}

// sloTPOTQ is the TPOT statistic the target is judged at: 0 for the mean.
func (r *reporter) sloTPOTQ() float64 {
	if r.tpot != nil {
		return r.tpot.Percentile
	}
	return r.q.TPOTPercentile
}

// fleet is the fleet at the shape priced, np prefill and nd decode replicas
// (0 the recorded count), at the statistics asked; one per key.
func (r *reporter) fleet(np, nd int, ttftQ, tpotQ float64) (*servedFleet, error) {
	k := fleetKey{r.shape, np, nd, ttftQ, tpotQ, r.fixedF}
	if sf, ok := r.fleets[k]; ok {
		return sf, nil
	}
	assume := r.q.Assume
	if tpotQ == 0 {
		// R_q prices a percentile only: on the mean it is not read.
		assume.TPOTRatio = nil
	}
	kn := whatifapi.Knobs{Prefill: np, Decode: nd, TTFTPercentile: ttftQ, TPOTPercentile: tpotQ,
		Assume: assume, Headroom: r.headroom, ArrivalCV2: r.q.ArrivalCV2}
	if !r.sh.Measured {
		kn.PromptTokens, kn.OutputTokens, kn.PrefixHit, kn.PrefixHitRead = r.sh.IL, r.sh.OL, r.sh.Hit, r.sh.Load
	}
	sf, err := r.s.fleetAtImbalance(kn, r.fixedF)
	if err != nil {
		return nil, err
	}
	r.fleets[k] = sf
	return sf, nil
}

// gpusPerReplica is each priced role's GPUs per replica as the recording
// states them, 0 for a role it states none for.
func (r *reporter) gpusPerReplica() (whatifapi.FleetCounts, error) {
	sf, err := r.fleet(r.q.Prefill, r.q.Decode, r.sloTTFTQ(), r.sloTPOTQ())
	if err != nil {
		return whatifapi.FleetCounts{}, err
	}
	out := whatifapi.FleetCounts{Decode: sf.pv.state.GPUsPerReplica}
	if sf.pf != nil {
		out.Prefill = variantGPUs(r.st, sf.pf.variant)
	}
	return out, nil
}

// counts is the base fleet's replica counts as priced.
func (r *reporter) counts(base *servedFleet) (np, nd int) {
	nd = base.pv.decode.Replicas
	switch {
	case r.q.Prefill > 0:
		np = r.q.Prefill
	case base.pf.refused == nil:
		np = base.pf.fleet.Prefill.Replicas
	default:
		for _, ps := range r.st.Prefill {
			if base.pf.variant == "" || ps.Variant == base.pf.variant {
				np = ps.Pods
				break
			}
		}
	}
	return np, nd
}

// adaptedBandFloorWords names the ITL band floor a decode line was read
// over, where the replay lowered it (AdaptedBandFloor), with the policy's.
func adaptedBandFloorWords(pv pricedVariant) string {
	return fmt.Sprintf("%s (%.2f, the policy's %.2f): the decode line was read over a lowered KV-fill floor",
		adaptedBandFloor, pv.bandFloor, pv.bandPolicyFloor)
}

// addAssumed adds words to the assumed list, once each.
func (r *reporter) addAssumed(words ...string) {
	for _, w := range words {
		if !slices.Contains(r.assumed, w) {
			r.assumed = append(r.assumed, w)
		}
	}
}

// loadOf is the load the fleet is judged at: the query's, else the statistic
// asked (the p95 less counting noise by default) of the per-cycle arrival rate over the window's
// cycles that read one above 0, refused (WindowGapAtPeak) when a gap in the
// window could hide its peak (gapAtPeak). A growth load is a what-if of
// its own (shapeReport), never the load judged at. The mean, p95 and
// peak are reported whichever is judged, and the busiest RollingWindow.
func (r *reporter) loadOf(l *whatifapi.ReportLoad) {
	l.Cycles = len(r.st.ArrivalRates)
	l.Statistic = r.q.LoadStatistic
	if l.Statistic == "" {
		l.Statistic = whatifapi.DefaultLoadStatistic
	}
	var rates []float64
	for _, x := range r.st.ArrivalRates {
		if whatif.Finite(x) && x > 0 {
			rates = append(rates, x)
		}
	}
	l.CyclesWithTraffic = len(rates)
	stats := map[string]float64{}
	if len(rates) > 0 {
		stats = loadStats(rates)
		mean, p95, peak := stats[whatifapi.LoadMean], stats[whatifapi.LoadP95], stats[whatifapi.LoadPeak]
		l.Mean, l.P95, l.Peak = &mean, &p95, &peak
		mod := whatifapi.ModulationP95Every(rates, mean, p95, cycleSeconds(r.st))
		stats[whatifapi.LoadP95Modulation] = mod
		l.P95Modulation = &mod
		l.P95IsPeak = len(rates) < minP95Cycles
		gaps := whatifapi.WindowGapsOf(r.st.CycleTimes)
		l.Rolling = rollingPeak(r.st.ArrivalRates, rollingCycle(r.st, gaps), whatifapi.RollingWindow.Seconds())
		if !(r.q.LoadSeen > 0) {
			r.loadRefused = gapAtPeak(r.st.ArrivalRates, r.st.CycleTimes, gaps, l.Rolling)
		}
	}
	var seen float64
	switch {
	case r.q.LoadSeen > 0:
		seen, l.Source, l.Statistic = r.q.LoadSeen, sourceAsked, loadStated
	case r.loadRefused != nil:
		l.Source = sourceObserved
		l.Rate = whatifapi.FigureOf(*r.loadRefused, unitRate, false)
		return
	case len(rates) > 0:
		seen, l.Source = stats[l.Statistic], sourceObserved
		r.peak = stats[whatifapi.LoadPeak]
		r.loadShape = loadShapeOf(r.decodeShapeSeries(), loadCycles(r.st.ArrivalRates, l.Statistic, stats), l.Statistic)
	default:
		l.Source = "none"
		l.Rate = whatifapi.FigureOf(whatif.Unanswerable{Reason: whatif.NoTraffic,
			Detail: fmt.Sprintf("no arrival rate above 0 in the %d cycles read; pass --load-seen", l.Cycles)}, unitRate, false)
		return
	}
	l.Seen = &seen
	r.load, r.loadStat = seen, l.Statistic
	l.Rate = whatifapi.FigureOf(whatif.Answer{Value: r.load}, unitRate, false)
	r.loadWords = fmt.Sprintf("the %s load of %s", statWords(l.Statistic), rateWords(r.load))
	if l.P95IsPeak && l.Statistic == whatifapi.LoadP95 {
		r.loadWords = "the p95 load (= peak, few cycles) of " + rateWords(r.load)
	}
	if l.P95IsPeak && l.Statistic == whatifapi.LoadP95Modulation {
		r.loadWords = "the p95 load less counting noise (from the peak, few cycles) of " + rateWords(r.load)
	}
}

// minP95Cycles is the fewest cycles whose nearest-rank p95 is not their peak.
const minP95Cycles = 20

// loadStated is the load's statistic when the question stated the load.
const loadStated = "stated"

// loadStats is the mean, the p95 (nearest rank) and the highest of rates,
// by statistic name.
func loadStats(rates []float64) map[string]float64 {
	s := slices.Clone(rates)
	slices.Sort(s)
	var sum float64
	for _, x := range s {
		sum += x
	}
	rank := int(math.Ceil(0.95 * float64(len(s))))
	return map[string]float64{whatifapi.LoadMean: sum / float64(len(s)), whatifapi.LoadP95: s[max(rank, 1)-1],
		whatifapi.LoadPeak: s[len(s)-1]}
}

// cycleSeconds is the mean time between the state's cycles, 0 with fewer
// than two.
func cycleSeconds(st *learnreplay.State) float64 {
	n := len(st.ArrivalRates)
	if n < 2 || !st.At.After(st.FirstAt) {
		return 0
	}
	return st.At.Sub(st.FirstAt).Seconds() / float64(n-1)
}

// rollingPeak is the highest mean of rates over consecutive cycles spanning
// window seconds, a cycle every cycle seconds; idle cycles count as 0. Nil
// when the cycle time is unknown or the window holds fewer cycles than that.
func rollingPeak(rates []float64, cycle, window float64) *whatifapi.ReportRolling {
	if !(cycle > 0) {
		return nil
	}
	k := max(int(math.Round(window/cycle)), 1)
	end, peak := busiestStretch(rates, k)
	if end < 0 {
		return nil
	}
	return &whatifapi.ReportRolling{WindowSeconds: float64(k) * cycle, Cycles: k, Peak: peak}
}

// rateWords is a rate in words: "41.7 r/s".
func rateWords(x float64) string { return fmt.Sprintf("%.1f r/s", x) }

// secondsWords is a latency in words: "3.9 s", or "640 ms" below a second.
func secondsWords(x float64) string {
	if x >= 1 {
		return fmt.Sprintf("%.1f s", x)
	}
	return fmt.Sprintf("%.0f ms", 1000*x)
}

// fleetEval is one fleet checked at the load: its knees, the SLO figures at
// the load, and whether it holds them with headroom.
type fleetEval struct {
	np, nd                  int
	prefillKnee, decodeKnee whatif.Result
	// knee is the fleet knee, the lower of the roles' that priced; kneeOK
	// says one priced, and prefillPriced that the prefill knee did. binds
	// names the role that sets it.
	knee          float64
	kneeOK        bool
	prefillPriced bool
	binds         string
	// ttftAt and tpotAt are the targets' statistics at the load, nil with no
	// target or no load.
	ttftAt, tpotAt whatif.Result
	// holds and known are the check: with headroom, every target met; known
	// false when a figure it needs was refused for its data.
	holds, known bool
	// under is a known miss: the load past the headroom (pastOperating, the
	// operating point op), or a target missed (slo says which).
	under         bool
	pastOperating bool
	op            float64
	// lowerBound says the fleet knee is a prefill knee priced from a lower
	// bound of V_P (whatif.VPLowerBound): the true knee may be higher, so the
	// fleet's knee is at least that rate. pastLowerBound is a load past its
	// operating point while decode's own holds: whether the fleet saturates
	// there is not priced, and the check is not judged unless a target is
	// missed. satKnee and satRole are the knee a load past the operating
	// point is judged against, and its role: the fleet knee, or decode's
	// where the fleet knee is a lower bound.
	lowerBound, pastLowerBound bool
	satKnee                    float64
	satRole                    string
	slo                        sloAt
	reason                     string
	// decodeOK and prefillOK are each role's own check at the rate: its knee
	// with headroom (dOp, pOp) and its own target (TPOT for decode, TTFT
	// for prefill).
	decodeOK, prefillOK bool
	dOp, pOp            float64
	// fAssumed names an f the prefill side was priced with that no run
	// learned at its count, "" with none.
	fAssumed string
}

// decodeKneeRate is the decode knee's rate, +Inf where it did not price.
func (e fleetEval) decodeKneeRate() float64 {
	if v, ok := valueOf(e.decodeKnee); ok {
		return v
	}
	return math.Inf(1)
}

// sloAt is the targets judged at rate on sf: each target's figure, whether
// every one is met and whether that is known, and the words.
type sloAt struct {
	ttft, tpot  whatif.Result
	met, known  bool
	saturated   bool
	missed      bool
	parts       []string
	targetCount int
	// ttftMissed and tpotMissed say which target was missed; onset that the
	// TTFT miss is the flow-control onset's, its figure within the target.
	ttftMissed, tpotMissed, onset bool
	// ttftLowerBoundVP says the TTFT is over its target, priced from a lower
	// bound of V_P (whatifapi.TTFTFromLowerBoundVP): the miss is not known.
	ttftLowerBoundVP bool
	// unknown is the first target not judged, and its figure.
	unknown    string
	unknownFig whatif.Result
}

// sloAtRate judges the targets at rate on sf.
func (r *reporter) sloAtRate(sf *servedFleet, rate float64, words bool) sloAt {
	s := sloAt{met: true, known: true}
	fold := func(label string, fig whatif.Result, target float64, met, known, onset bool) bool {
		s.targetCount++
		if u, ok := fig.(whatif.Unanswerable); ok && (u.Reason == whatif.Saturated || u.Reason == whatif.AboveDecodeOnset) {
			s.saturated = true
		}
		missed := false
		switch {
		case !known:
			s.met, s.known = false, false
			if s.unknown == "" {
				s.unknown, s.unknownFig = label, fig
			}
		case !met:
			s.met, s.missed, missed = false, true, true
		}
		s.parts = append(s.parts, r.judged(label, fig, target, met, known, onset))
		return missed
	}
	if r.ttft != nil {
		t, w := sf.TTFTPoint(rate)
		if words {
			r.addAssumed(w...)
		}
		s.ttft = t
		met, known := whatif.TTFTMeets(t, whatif.TTFTTarget{Seconds: r.ttft.Seconds, Percentile: r.ttft.Percentile})
		onset := false
		if v, ok := t.(whatif.TTFT); ok && !met && v.Seconds <= r.ttft.Seconds && v.HasFlag(whatif.AboveFlowControlOnset) {
			onset = true
		}
		if !met && known && !onset && lowerBoundVPTTFT(t) {
			// Over the target from a lower bound of V_P: not known (#515).
			known, s.ttftLowerBoundVP = false, true
		}
		s.ttftMissed = fold("TTFT "+whatifapi.ReportStatisticName(r.ttft.Percentile), t, r.ttft.Seconds, met, known, onset)
		s.onset = onset
	}
	if r.tpot != nil {
		mean, pct := sf.TPOTAt(rate)
		t := mean
		if r.tpot.Percentile > 0 {
			t = pct
		}
		s.tpot = t
		met, known := tpotMeets(t, r.tpot.Seconds)
		s.tpotMissed = fold("TPOT "+whatifapi.ReportStatisticName(r.tpot.Percentile), t, r.tpot.Seconds, met, known, false)
	}
	// A known miss decides whatever else was not judged.
	if s.missed {
		s.known = true
	}
	return s
}

// evaluate prices the fleet at np prefill and nd decode replicas at the load.
// An error is a fleet that could not be priced at all, not a refused figure.
func (r *reporter) evaluate(np, nd int) (fleetEval, error) {
	return r.evaluateAt(np, nd, r.load, true)
}

// evaluateAt prices the fleet at np prefill and nd decode replicas at rate;
// words adds what an assumed term does there to the shape's assumed list.
func (r *reporter) evaluateAt(np, nd int, rate float64, words bool) (fleetEval, error) {
	e := fleetEval{np: np, nd: nd}
	if r.ctx != nil {
		if err := r.ctx.Err(); err != nil {
			return e, err
		}
	}
	sf, err := r.fleet(np, nd, r.sloTTFTQ(), r.sloTPOTQ())
	if err != nil {
		return e, fmt.Errorf("pricing %s: %w", fleetText(np, nd), err)
	}
	if sf.pf.refused == nil && sf.pf.fleet.Prefill.ImbalanceAssumed {
		e.fAssumed = fmt.Sprintf("prefill f %.3g assumed at %d pods", sf.pf.fleet.Prefill.Imbalance, sf.pf.fleet.Prefill.Replicas)
	}
	e.prefillKnee, e.decodeKnee = sf.PrefillKnee(), sf.DecodeKnee()
	pk, pok := e.prefillKnee.(whatif.Answer)
	dk, dok := e.decodeKnee.(whatif.Answer)
	e.prefillPriced = pok
	switch {
	case dok && pok && pk.Value < dk.Value:
		e.knee, e.kneeOK, e.binds = pk.Value, true, rolePrefill
	case dok:
		e.knee, e.kneeOK, e.binds = dk.Value, true, roleDecodeWord
	}
	if !e.kneeOK {
		e.reason = "the decode knee is not priced: " + refusedText(e.decodeKnee.(whatif.Unanswerable))
		return e, nil
	}
	if !(rate > 0) {
		e.reason = "no load to judge at"
		return e, nil
	}
	var parts []string
	e.known, e.holds = true, true
	e.op = r.headroom * e.knee
	e.satKnee, e.satRole = e.knee, e.binds
	e.lowerBound = e.binds == rolePrefill && slices.Contains(pk.Flags, whatif.VPLowerBound)
	switch {
	case e.lowerBound && rate > e.op && rate <= r.headroom*dk.Value:
		// A knee priced from a lower bound of V_P reads low by construction:
		// a load past it does not show the fleet saturated (#280).
		e.holds, e.known, e.pastLowerBound = false, false, true
		past := fmt.Sprintf("past %s x", num(r.headroom))
		if rate >= e.knee {
			past = fmt.Sprintf("%.1fx", rate/e.knee)
		}
		parts = append(parts, fmt.Sprintf("%.2f r/s is %s a prefill knee of at least %.2f r/s (V_P is a lower bound): "+
			"whether the fleet saturates is not priced", rate, past, e.knee))
	case e.lowerBound && rate > e.op:
		// Decode's own operating point is crossed too: that is known
		// whatever V_P is, so the load is judged against decode's knee.
		e.holds, e.under, e.pastOperating = false, true, true
		e.satKnee, e.satRole, e.op = dk.Value, roleDecodeWord, r.headroom*dk.Value
		parts = append(parts, fmt.Sprintf("%.2f r/s is %.1fx the decode knee of %.2f r/s (the prefill knee, priced from a lower bound of V_P, is at least %.2f r/s)",
			rate, rate/dk.Value, dk.Value, e.knee))
	case rate > e.op:
		e.holds, e.under, e.pastOperating = false, true, true
		if rate >= e.knee {
			parts = append(parts, fmt.Sprintf("%.2f r/s is %.1fx the fleet knee of %.2f r/s", rate, rate/e.knee, e.knee))
		} else {
			parts = append(parts, fmt.Sprintf("%.2f r/s is above %s x the fleet knee %.2f r/s = %.2f r/s", rate, num(r.headroom), e.knee, e.op))
		}
	default:
		parts = append(parts, fmt.Sprintf("%.2f r/s within %s x the fleet knee %.2f r/s = %.2f r/s", rate, num(r.headroom), e.knee, e.op))
	}
	if !e.prefillPriced {
		parts = append(parts, "prefill not priced ("+reasonOf(e.prefillKnee)+")")
	}
	s := r.sloAtRate(sf, rate, words)
	e.ttftAt, e.tpotAt, e.slo = s.ttft, s.tpot, s
	parts = append(parts, s.parts...)
	switch {
	case s.targetCount == 0:
		parts = append(parts, "no SLO target")
	case !s.met && s.known:
		e.holds, e.under = false, true
	case !s.met:
		e.holds, e.known = false, false
	}
	// A known miss decides the check whatever else was not judged.
	e.known = e.known || e.under
	e.reason = strings.Join(parts, "; ")

	e.dOp = r.headroom * dk.Value
	e.decodeOK = rate <= e.dOp
	if r.tpot != nil {
		met, known := tpotMeets(s.tpot, r.tpot.Seconds)
		e.decodeOK = e.decodeOK && met && known
	}
	if pok {
		e.pOp = r.headroom * pk.Value
		e.prefillOK = rate <= e.pOp
		if r.ttft != nil {
			met, known := whatif.TTFTMeets(s.ttft, whatif.TTFTTarget{Seconds: r.ttft.Seconds, Percentile: r.ttft.Percentile})
			e.prefillOK = e.prefillOK && met && known
		}
	}
	return e, nil
}

// roleDecodeWord is the decode role as the report names it.
const roleDecodeWord = "decode"

// tpotMeets reads a TPOT figure against a target: a refusal for saturation
// is a known miss, any other an unknown.
func tpotMeets(r whatif.Result, target float64) (met, known bool) {
	switch v := r.(type) {
	case whatif.Answer:
		return v.Value <= target, true
	case whatif.Unanswerable:
		if v.Reason == whatif.Saturated {
			return false, true
		}
	}
	return false, false
}

// judged is one target's check in words, with its numbers. A TTFT priced
// from a lower bound of V_P over its target is not judged a miss (#515).
func (r *reporter) judged(label string, fig whatif.Result, target float64, met, known, onset bool) string {
	switch {
	case !known && lowerBoundVPTTFT(fig):
		return fmt.Sprintf("%s is %s, over %s, but not judged: %s", label, secondsText(fig), msText(target), lowerBoundVPWords)
	case !known:
		return fmt.Sprintf("%s not judged (%s)", label, reasonOf(fig))
	case onset:
		return fmt.Sprintf("%s %s is within %s but misses: the rate is above the prefill flow-control onset, where the router queues",
			label, secondsText(fig), msText(target))
	case met:
		return fmt.Sprintf("%s %s meets %s", label, secondsText(fig), msText(target))
	}
	return fmt.Sprintf("%s %s misses %s", label, secondsText(fig), msText(target))
}

// lowerBoundVPWords says why a TTFT over its target priced from a lower
// bound of V_P is not judged a miss. It claims no bound (#515).
const lowerBoundVPWords = "it is priced from a lower bound of V_P, which can read it high, so the miss is not known"

// lowerBoundVPTTFT says r is a TTFT priced from a lower bound of V_P
// (whatifapi.TTFTFromLowerBoundVP).
func lowerBoundVPTTFT(r whatif.Result) bool {
	v, ok := r.(whatif.TTFT)
	return ok && whatifapi.TTFTFromLowerBoundVP(v.Flags)
}

// secondsText is a latency figure in ms, or its refusal's reason.
func secondsText(r whatif.Result) string {
	if v, ok := valueOf(r); ok {
		return msText(v)
	}
	return reasonOf(r)
}

// valueOf is an answered figure's value.
func valueOf(r whatif.Result) (float64, bool) {
	switch v := r.(type) {
	case whatif.Answer:
		return v.Value, true
	case whatif.TTFT:
		return v.Seconds, true
	}
	return 0, false
}

// reasonOf is a refusal's reason in plain words (plainReason), "" on an
// answer: the codes are the JSON's.
func reasonOf(r whatif.Result) string {
	u, ok := r.(whatif.Unanswerable)
	if !ok {
		return ""
	}
	if servedOnlyRefusal(string(u.Reason), slices.Contains(u.Flags, whatif.VPLowerBound)) {
		return servedOnlyWords
	}
	return plainReason(string(u.Reason))
}

// servedOnlyWords stands for a TTFT refused on a V_P that is only the rate a
// prefill pod was seen to compute (whatif.Prefill.VPServedOnly): a speed was
// read, as a lower bound, and the TTFT is not priced from it.
const servedOnlyWords = "TTFT not priced (V_P a lower bound)"

// servedOnlyRefusal reports whether a refusal of reason, flagged
// whatif.VPLowerBound or not, is a TTFT refused on a V_P known only as the
// rate a pod computed: NoPrefillCapacity carries that flag there alone.
func servedOnlyRefusal(reason string, lowerBound bool) bool {
	return reason == string(whatif.NoPrefillCapacity) && lowerBound
}

// plainReasons are the reasons a report's text names, in words.
var plainReasons = map[string]string{
	string(whatif.Saturated):             "saturated",
	string(whatif.AboveDecodeOnset):      "saturated",
	string(whatif.PastLowerBoundKnee):    "past a lower-bound knee",
	string(whatif.NoPrefillCapacity):     "no prefill speed learned",
	string(whatif.NoFixedLatency):        "no fixed prefill latency learned",
	string(whatif.NoImbalance):           "no prefill imbalance learned",
	string(whatif.NoPrefillReplicas):     "no prefill replica",
	string(whatif.FlowControlUnknown):    "flow-control thresholds unknown",
	string(whatif.TPOTBucketTooWide):     "TPOT bucket too wide",
	string(whatif.NoTPOTHistogram):       "no TPOT histogram",
	string(whatif.TPOTQuantileUnbounded): "TPOT past the histogram",
	string(whatif.TokenGapPercentile):    "not a per-request percentile",
	string(whatif.NoTraffic):             "no traffic",
	whatifapi.ReasonWindowGapAtPeak:      "a window gap at the peak",
	string(whatif.NoLine):                "no decode line",
	string(whatif.PinnedDefaultB):        "no decode line",
	string(whatif.NoCapacity):            "no KV capacity",
	string(whatif.NoBatchCap):            "no batch cap",
	string(whatif.TargetBelowFloor):      "target below the floor",
	string(whatif.NoDecodeReplicas):      "no decode replica",
	string(whatif.MalformedHistogram):    "unreadable histogram",
	string(whatif.TooManySlots):          "too many slots",
	string(whatif.InvalidInput):          "invalid input",
}

// plainReason is a reason code in words; an unknown code is "refused".
func plainReason(code string) string {
	if w, ok := plainReasons[code]; ok {
		return w
	}
	return "refused"
}

// plainText is s with every reason code it names put in words.
func plainText(s string) string {
	for code, w := range plainReasons {
		s = strings.ReplaceAll(s, code, w)
	}
	return s
}

// capacity fills the capacity rows.
func (r *reporter) capacity(c *whatifapi.ReportLimits, base *servedFleet, ev fleetEval) {
	c.Headroom, c.HeadroomSource = r.headroom, r.headroomSource
	ol := r.pv.shape.AvgOutputTokens
	r.results = append(r.results, ev.prefillKnee, ev.decodeKnee)
	c.PrefillKnee = r.rate(ev.prefillKnee, ol, prefillBinds(base))
	if a, ok := ev.prefillKnee.(whatif.Answer); ok && slices.Contains(a.Flags, whatif.VPLowerBound) {
		c.PrefillKnee.LowerBound = true
	}
	plateauValidation(&c.PrefillKnee, whatif.FigurePrefillKnee, ev.prefillKnee)
	c.DecodeKnee = r.rate(ev.decodeKnee, ol, decodeBinds(ev.decodeKnee))
	r.kneeV = decodeKneeValidation(r.pv.class(), ev.decodeKnee)
	if c.DecodeKnee.Value != nil {
		c.DecodeKnee.Validation = whatifapi.ValidationOf(r.kneeV)
		r.kneeOutside = r.kneeOutside || !r.kneeV.Validated()
	}
	if !ev.kneeOK {
		c.FleetKnee = r.rate(ev.decodeKnee, ol, "")
		c.Operating = c.FleetKnee
		c.MaxAtSLO = c.FleetKnee
		return
	}
	binds := ev.binds
	if !ev.prefillPriced {
		binds = "decode (prefill not priced)"
	}
	c.FleetKnee = r.rate(whatif.Answer{Value: ev.knee}, ol, binds)
	c.Operating = r.rate(whatif.Answer{Value: r.headroom * ev.knee}, ol, fmt.Sprintf("%s x fleet knee (%s)", num(r.headroom), r.headroomSource))
	// A fleet knee a lower-bound prefill knee sets is at least its rate, and
	// so is the operating point on it (#280).
	c.FleetKnee.LowerBound, c.Operating.LowerBound = ev.lowerBound, ev.lowerBound
	maxRate, binds := r.maxAtSLO(base, ev)
	r.results = append(r.results, maxRate)
	c.MaxAtSLO = r.rate(maxRate, ol, binds)
	// A TTFT floor over the target priced from a lower bound of V_P can read
	// high: whether the target is reachable is not known. The rate is not a
	// bound either way (#515).
	if v, ok := maxRate.(whatif.Unanswerable); ok && v.Reason == whatif.TargetBelowFloor && whatifapi.TTFTFromLowerBoundVP(v.Flags) &&
		c.MaxAtSLO.Refused != nil {
		c.MaxAtSLO.Refused.Detail += "; the floor is priced from a lower bound of V_P, which can read it high, so the target may be reachable"
	}
	if ev.binds == rolePrefill {
		plateauValidation(&c.FleetKnee, whatif.FigurePrefillKnee, ev.prefillKnee)
		plateauValidation(&c.Operating, whatif.FigureOperatingCap, ev.prefillKnee)
	}
	plateauValidation(&c.MaxAtSLO, whatif.FigureRateAtTTFT, maxRate)
}

// capacityConcurrency adds the mean concurrency to each capacity rate: at a
// knee, unbounded; at the operating point and the most at the SLO, priced on
// the fleet at the mean TTFT.
func (r *reporter) capacityConcurrency(c *whatifapi.ReportLimits, ev fleetEval) error {
	mean, err := r.fleet(r.q.Prefill, r.q.Decode, 0, r.q.TPOTPercentile)
	if err != nil {
		return err
	}
	ol := r.pv.shape.AvgOutputTokens
	for _, k := range []*whatifapi.ReportRate{&c.PrefillKnee, &c.DecodeKnee, &c.FleetKnee} {
		switch {
		case k.Value == nil:
		case k.LowerBound:
			// At least the knee: the true knee, past which requests queue
			// without bound, may be higher (#280).
			k.Concurrency = whatifapi.LowerBoundKneeConcurrency()
		default:
			k.Concurrency = whatifapi.ConcurrencyAt(*k.Value, nil, nil, ol, 0, true)
		}
	}
	if !ev.kneeOK {
		// The operating point and the most at the SLO are the fleet knee's
		// figure here (capacity).
		c.Operating.Concurrency, c.MaxAtSLO.Concurrency = c.FleetKnee.Concurrency, c.FleetKnee.Concurrency
		return nil
	}
	for _, x := range []*whatifapi.ReportRate{&c.Operating, &c.MaxAtSLO} {
		if x.Value == nil {
			continue
		}
		rate := *x.Value
		t, _ := mean.TTFTPoint(rate)
		tm, _ := mean.TPOTAt(rate)
		x.Concurrency = whatifapi.ConcurrencyAt(rate, t, tm, ol, mean.pv.decode.Replicas, r.pastKnee(rate, ev))
	}
	return nil
}

// rate is a rate row: the figure, the output tokens it carries, what binds.
func (r *reporter) rate(res whatif.Result, ol float64, binds string) whatifapi.ReportRate {
	out := whatifapi.ReportRate{Figure: whatifapi.FigureOf(res, unitRate, false), Binds: binds}
	if v, ok := valueOf(res); ok {
		tok := v * ol
		out.OutputTokensPerSecond = &tok
	}
	return out
}

// prefillBinds says what sets the prefill knee.
func prefillBinds(sf *servedFleet) string {
	if sf.pf.refused != nil {
		return ""
	}
	p := sf.pf.fleet.Prefill
	if k, ok := sf.PrefillKnee().(whatif.Answer); ok && k.HasFlag(whatif.KneeAtServedWork) {
		return servedWorkWords(k.Value, p.PromptTokens)
	}
	margin := ""
	if p.VPMargin > 0 {
		margin = fmt.Sprintf(" x (1 - %.2f margin)", p.VPMargin)
	}
	return fmt.Sprintf("prefill compute: N_p %d x V_P%s / (f %.3f x %s tokens)", p.Replicas, margin, sf.pf.in.Imbalance, num(round(p.PromptTokens)))
}

// servedWorkWords says a prefill knee is the work the prefill pods sustained
// (whatif.KneeAtServedWork): knee x the prompt priced, over that prompt.
func servedWorkWords(knee, prompt float64) string {
	return fmt.Sprintf("prefill work served: the %s computed tokens/s the prefill pods sustained over 5 min, backlog steady / %s tokens",
		num(round(knee*prompt)), num(round(prompt)))
}

// decodeBinds says what sets the decode knee.
func decodeBinds(r whatif.Result) string {
	a, ok := r.(whatif.Answer)
	if !ok {
		return ""
	}
	return fmt.Sprintf("decode %s at fill %.3f", boundText(a.Basis.BoundBy), a.Basis.Fill)
}

// maxAtSLO is slo-to-rate's fleet rate at the targets with no headroom: the
// highest rate that holds them. With no TTFT target, decode's rate at the
// TPOT target bounded by the prefill knee. With a prefill side that does not
// price there is none, as slo-to-rate gives no fleet row: decode alone does
// not bound the fleet.
func (r *reporter) maxAtSLO(base *servedFleet, ev fleetEval) (whatif.Result, string) {
	tpotSeconds := 0.0
	if r.tpot != nil {
		tpotSeconds = r.tpot.Seconds
	}
	if u, ok := ev.prefillKnee.(whatif.Unanswerable); ok {
		u.Detail = "no fleet rate: the prefill side is not priced, and decode alone does not bound the fleet (slo-to-rate gives none either): " + u.Detail
		return u, ""
	}
	if r.ttft != nil && base.pf.refused == nil {
		lim := base.Limits(r.ttft.Seconds, tpotSeconds, r.headroom)
		r.addAssumed(lim.Notes...)
		if a, ok := lim.WithoutHeadroom.(whatif.Answer); ok {
			return a, boundText(a.Basis.BoundBy)
		}
		return lim.WithoutHeadroom, ""
	}
	switch {
	case r.tpot == nil && r.ttft != nil:
		// The TTFT target alone, on a prefill side that does not price.
		return *base.pf.refused, ""
	case r.tpot == nil:
		return whatif.Unanswerable{Reason: whatif.InvalidInput, Detail: "no SLO target to bound a rate"}, ""
	}
	d := base.decodeRate(tpotSeconds, 1)
	a, ok := d.(whatif.Answer)
	if !ok {
		return d, ""
	}
	binds := "decode " + boundText(a.Basis.BoundBy)
	if pk, ok := ev.prefillKnee.(whatif.Answer); ok && pk.Value < a.Value {
		a.Value, binds = pk.Value, "prefill knee"
	}
	return a, binds
}

// use fills the utilization at the load.
func (r *reporter) use(u *whatifapi.ReportUse, base *servedFleet, ev fleetEval) {
	d := base.pv.decode
	u.Decode.BandTop = d.Band.Hi
	slots := slotsPerReplica(d)
	u.Decode.Slots = slots * int64(d.Replicas)
	if !(r.load > 0) {
		no := whatif.Unanswerable{Reason: whatif.NoTraffic, Detail: "no load to judge at"}
		if r.loadRefused != nil {
			no = *r.loadRefused
		}
		f := whatifapi.FigureOf(no, "", false)
		u.Decode.KVFill, u.Decode.SlotsOccupied, u.Decode.WaitProbability = f, f, f
		u.Prefill.Pooled, u.Prefill.WorstPod = f, f
		return
	}
	fill := whatif.FillAtRate(d, r.load)
	u.Decode.KVFill = whatifapi.FigureOf(fill, "", false)
	if a, ok := fill.(whatif.Answer); ok {
		seqs := math.Min(a.Value*d.CapacityTokens/d.KVPerRequest, float64(slots)) * float64(d.Replicas)
		u.Decode.SlotsOccupied = whatifapi.FigureOf(whatif.Answer{Value: seqs}, "", false)
	} else {
		u.Decode.SlotsOccupied = u.Decode.KVFill
	}
	switch a := whatif.DecodeAdmission(d, r.load).(type) {
	case whatif.Admission:
		u.Decode.WaitProbability = whatifapi.FigureOf(whatif.Answer{Value: a.WaitProbability, Flags: a.Flags}, "", false)
		r.results = append(r.results, whatif.Answer{Value: a.WaitProbability, Flags: a.Flags})
	case whatif.Unanswerable:
		u.Decode.WaitProbability = whatifapi.FigureOf(a, "", false)
	}
	// The KV pull's utilization stands where the TTFT is refused, the pull
	// saturating among the reasons; its wait is the mean TTFT's part, with
	// no span (a mean whatever the percentile judged), and absent where that
	// TTFT is refused, or priced first come at a prompt spread past
	// processor sharing's (whatif.SpreadBeyondProcessorSharing), which prices
	// no pull stage: its wait reads 0 there, which is not a measurement.
	if base.pf.refused == nil {
		if rho := whatif.KVPullUtilization(base.pf.fleet, r.load); rho > 0 {
			kv := &whatifapi.ReportKVPull{Utilization: rho}
			mean := base.pf.priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, r.load, 0) })
			t, ok := mean.(whatif.TTFT)
			switch {
			case !ok:
				kv.Unpriced = whatifapi.KVPullTTFTRefused
			case !(t.Parts.KVPullUtilization > 0):
				kv.Unpriced = whatifapi.KVPullFirstCome
			default:
				w := t.Parts.KVPullWait
				kv.Wait, kv.SetsTTFT = &w, pullSetsTTFT(t.Parts)
			}
			u.Decode.KVPull = kv
		}
	}
	pk, ok := ev.prefillKnee.(whatif.Answer)
	if !ok {
		f := whatifapi.FigureOf(ev.prefillKnee, "", false)
		u.Prefill.Pooled, u.Prefill.WorstPod = f, f
		return
	}
	// The busiest pod's utilization is the load over the prefill knee, which
	// is N_p x V_P (less the margin) / (f x the prompt); pooled, f is 1.
	// With --load-aware-router f falls with the load and the knee is no
	// longer N_p x V_P / (f x the prompt): both are read at the load.
	f := max(base.pf.in.Imbalance, 1)
	worst := r.load / pk.Value
	pooled := worst / f
	if p := base.pf.fleet.Prefill; p.LoadAwareRouter {
		if fl, pl, ok := whatif.PrefillLoadAt(p, r.load); ok {
			f, worst, pooled = fl, fl*pl, pl
		}
	}
	u.Prefill.Imbalance = f
	u.Prefill.WorstPod = whatifapi.FigureOf(whatif.Answer{Value: worst}, "", false)
	u.Prefill.Pooled = whatifapi.FigureOf(whatif.Answer{Value: pooled}, "", false)
}

// slotsPerReplica is the batch slots of one decode replica: the cap, or the
// whole requests the band's top holds where that is fewer, as the knee and
// the admission chain read it.
func slotsPerReplica(d whatif.Decode) int64 {
	if !(d.KVPerRequest > 0) {
		return d.ReplicaCap
	}
	kv := int64(math.Floor(d.Band.Hi * d.CapacityTokens / d.KVPerRequest))
	if d.ReplicaCap > 0 && d.ReplicaCap < kv {
		return d.ReplicaCap
	}
	return kv
}

// sloOf is the targets judged by.
func (r *reporter) sloOf() whatifapi.ReportSLO {
	var s whatifapi.ReportSLO
	if r.ttft != nil {
		s.TTFT = &whatifapi.ReportObjective{Statistic: whatifapi.ReportStatisticName(r.ttft.Percentile), Target: r.ttft.Seconds, Source: r.ttftSource}
	}
	if r.tpot != nil {
		s.TPOT = &whatifapi.ReportObjective{Statistic: whatifapi.ReportStatisticName(r.tpot.Percentile), Target: r.tpot.Seconds, Source: r.tpotSource}
	}
	return s
}

// table is the shape's SLO against load at the base fleet, one row per load
// asked, in the order asked: every row from the same replayed state.
func (r *reporter) table(ctx context.Context, ev fleetEval) ([]whatifapi.ReportRow, error) {
	np0, nd0, tq := r.q.Prefill, r.q.Decode, r.q.TPOTPercentile
	mean, err := r.fleet(np0, nd0, 0, tq)
	if err != nil {
		return nil, err
	}
	slo, err := r.fleet(np0, nd0, r.sloTTFTQ(), r.sloTPOTQ())
	if err != nil {
		return nil, err
	}
	cellTTFT, err := r.fleet(np0, nd0, cellTTFTQ, tq)
	if err != nil {
		return nil, err
	}
	rows := []whatifapi.ReportRow{}
	r.untranslated = nil
	for _, l := range r.q.Loads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row := whatifapi.ReportRow{Load: l.Label, TTFT: []whatifapi.ReportStat{}}
		switch {
		case l.Grown() && r.load > 0:
			row.Rate, row.Label = l.X*r.load, "load "+row.Load
		case l.Seen && r.load > 0:
			row.Rate, row.Label = r.load, "load "+row.Load
		case l.Seen:
			continue
		case l.Share > 0 && ev.kneeOK:
			row.Rate, row.Label = l.Share*ev.knee, l.Label+" of knee"
			if l.Share == 1 {
				// The 100% row is the knee itself, not a product that rounds off it.
				row.Rate = ev.knee
			}
		case l.Share > 0:
			continue
		case l.Concurrency > 0:
			cr, why := r.rateAtConcurrency(mean, l.Concurrency, ev)
			if why != "" {
				r.untranslated = append(r.untranslated, whatifapi.ReportUntranslated{Load: l.Label, Reason: why})
				continue
			}
			row.Rate, row.Label, row.FromConcurrency = cr.Rate, l.Label+" concurrent", &cr
			if cr.AtKnee {
				row.Label += ", saturates"
			}
		default:
			row.Rate, row.Label = l.Rate, l.Label
		}
		if ev.kneeOK {
			share := row.Rate / ev.knee
			if l.Share > 0 {
				share = l.Share
			}
			row.ShareOfKnee = &share
		}
		for _, q := range r.q.TTFTPercentiles {
			sf, err := r.fleet(np0, nd0, q, tq)
			if err != nil {
				return nil, err
			}
			row.TTFT = append(row.TTFT, r.ttftStat(sf, q, row.Rate))
		}
		tm, tp := mean.TPOTAt(row.Rate)
		r.results = append(r.results, tm)
		row.TPOTMean = whatifapi.FigureOf(tm, unitSeconds, true)
		if tq > 0 {
			r.results = append(r.results, tp)
			row.TPOTPercentile = &whatifapi.ReportStat{Statistic: whatifapi.ReportStatisticName(tq), Figure: whatifapi.FigureOf(tp, unitSeconds, true)}
		}
		// What an assumed term does to one point is said once, at the load
		// (evaluate), not at every row.
		ttftMean, _ := mean.TTFTPoint(row.Rate)
		row.E2EMeanSum = whatifapi.FigureOf(e2eSum(ttftMean, tm, r.pv.shape.AvgOutputTokens), unitSeconds, true)
		row.Concurrency = whatifapi.ConcurrencyAt(row.Rate, ttftMean, tm, r.pv.shape.AvgOutputTokens, mean.pv.decode.Replicas,
			r.pastKnee(row.Rate, ev))
		row.Cell = r.cell(slo, cellTTFT, row, ev)
		rows = append(rows, row)
	}
	return rows, nil
}

// pastKnee says rate is at or past the fleet knee, where requests queue
// without bound; past a knee that is only a lower bound, short of decode's,
// it is not known to be (#280).
func (r *reporter) pastKnee(rate float64, ev fleetEval) bool {
	if !ev.kneeOK || rate < ev.knee {
		return false
	}
	return !ev.lowerBound || rate >= ev.decodeKneeRate()
}

// rateAtConcurrency is the rate at which the fleet holds n requests in the
// system on average, on mean, the fleet priced at the mean TTFT: the rate
// at which rate x (TTFT mean + (OL - 1) x TPOT mean) is n, below the fleet
// knee. why says, in words, why there is none.
func (r *reporter) rateAtConcurrency(mean *servedFleet, n float64, ev fleetEval) (whatifapi.ConcurrencyRate, string) {
	if !ev.kneeOK {
		return whatifapi.ConcurrencyRate{}, "no fleet knee to translate a concurrency below"
	}
	ol := r.pv.shape.AvgOutputTokens
	var refused whatif.Result
	w := func(rate float64) (float64, bool) {
		t, _ := mean.TTFTPoint(rate)
		tm, _ := mean.TPOTAt(rate)
		sum := e2eSum(t, tm, ol)
		v, ok := valueOf(sum)
		if !ok && refused == nil {
			refused = sum
		}
		return v, ok
	}
	cr, err := whatifapi.RateAtConcurrency(n, ev.knee, w)
	switch {
	case whatifapi.ConcurrencyUnpriced(err):
		return cr, "the mean time in the system is refused: " + refusalWords(refused)
	case err != nil:
		return cr, err.Error()
	case cr.AtKnee && ev.lowerBound && cr.Rate >= ev.knee:
		// The fleet holds fewer below a knee that is only a lower bound:
		// whether it saturates there is not known (#280).
		return whatifapi.ConcurrencyRate{}, fmt.Sprintf("the fleet holds fewer below its prefill knee of at least %.2f r/s, "+
			"which rests on a lower bound of V_P: the true knee may be higher, so the rate that holds them is not priced; "+
			"--assume-vp states a V_P", ev.knee)
	}
	return cr, ""
}

// ttftStat is the TTFT at q and rate on sf, flagged where the tail rests on
// an assumed prompt spread.
func (r *reporter) ttftStat(sf *servedFleet, q, rate float64) whatifapi.ReportStat {
	t, _ := sf.TTFTPoint(rate)
	if q > maxPromptTailQ && sf.pf.refused == nil && sf.pf.in.PromptCVSource != shapeMeasured {
		t = withFlags(t, []whatif.Flag{tailPastHistogram})
	}
	r.results = append(r.results, t)
	return whatifapi.ReportStat{Statistic: whatifapi.ReportStatisticName(q), Figure: whatifapi.FigureOf(t, unitSeconds, true)}
}

// cell is a row's summary cell: each latency at the statistic its target is
// judged at (TTFT p90 and the table's TPOT percentile where no target is
// set), and the targets judged at the row's rate. A TPOT percentile that is
// refused leaves the mean, which TPOT.Statistic names.
func (r *reporter) cell(slo, ttft *servedFleet, row whatifapi.ReportRow, ev fleetEval) whatifapi.ReportCell {
	c := whatifapi.ReportCell{}
	ttftQ, ttftFleet := cellTTFTQ, ttft
	if r.ttft != nil {
		ttftQ, ttftFleet = r.ttft.Percentile, slo
	}
	name := whatifapi.ReportStatisticName(ttftQ)
	i := slices.IndexFunc(row.TTFT, func(s whatifapi.ReportStat) bool { return s.Statistic == name })
	if i >= 0 {
		c.TTFT = row.TTFT[i]
	} else {
		c.TTFT = r.ttftStat(ttftFleet, ttftQ, row.Rate)
	}
	c.TPOT = whatifapi.ReportStat{Statistic: "mean", Figure: row.TPOTMean}
	// The percentile where it priced; else the mean, which a refused R_q
	// leaves answered.
	switch {
	case r.tpot != nil && r.tpot.Percentile == 0:
	case r.tpot != nil:
		_, tp := slo.TPOTAt(row.Rate)
		r.results = append(r.results, tp)
		if f := whatifapi.FigureOf(tp, unitSeconds, true); f.Value != nil {
			c.TPOT = whatifapi.ReportStat{Statistic: whatifapi.ReportStatisticName(r.tpot.Percentile), Figure: f}
		}
	case row.TPOTPercentile != nil && row.TPOTPercentile.Value != nil:
		c.TPOT = *row.TPOTPercentile
	}
	s := r.sloAtRate(slo, row.Rate, false)
	pastKnee := row.ShareOfKnee != nil && *row.ShareOfKnee >= 1
	// At or past a lower-bound prefill knee, short of decode's, the rate is
	// not shown saturated (#280); a known miss there is still a miss.
	pastLowerBound := pastKnee && ev.lowerBound && !(row.Rate >= ev.decodeKneeRate())
	switch {
	case s.saturated || (pastKnee && !pastLowerBound):
		c.Mark = whatifapi.CellSaturated
	case pastLowerBound && (!s.known || s.met):
		c.Mark = whatifapi.CellPastLowerBound
	case s.targetCount == 0:
		c.Mark = whatifapi.CellNoTarget
	case s.met:
		c.Mark = whatifapi.CellMet
	case s.onset && !s.tpotMissed:
		c.Mark = whatifapi.CellOnset
	case s.known:
		c.Mark = whatifapi.CellMissed
	case s.ttftLowerBoundVP:
		c.Mark = whatifapi.CellVPLowerBound
	default:
		c.Mark = whatifapi.CellUnknown
	}
	return c
}

// e2eSum is the mean e2e as a sum of means: TTFT mean + (OL - 1) x TPOT mean.
// A refused part refuses the sum with its reason.
func e2eSum(ttft, tpot whatif.Result, ol float64) whatif.Result {
	t, tok := valueOf(ttft)
	p, pok := valueOf(tpot)
	switch {
	case !tok:
		return ttft
	case !pok:
		return tpot
	}
	return whatif.Answer{Value: t + max(ol-1, 0)*p}
}

// verdict judges the fleet at the load:
//   - UNDER-PROVISIONED on a known miss at the load judged;
//   - NOT JUDGED where it would be UNDER-PROVISIONED on a load of other
//     traffic than the shape priced, or one the window's own fleet served
//     past the knee (notJudgedUnder), when its own check is not judged,
//     when the prefill side does not price and decode alone holds, or where
//     the fleet holds on a measured shape priced lighter than the load's
//     cycles, whose knee reads high for the load (phasesLighter, #361);
//   - OVER-PROVISIONED when a role has a replica to spare: one fewer of it
//     holds the whole fleet at the load judged, and the role's own check
//     (knee and target) at the peak too, since removing capacity is the
//     costly mistake; a decode replica is spare only where one fewer is
//     still the count the load needs if the decode knee reads as high as
//     its label allows (decodeAtBound, #363);
//   - RIGHT-SIZED otherwise.
//
// Each role's standing is reported (Roles), and on UNDER and OVER the
// smallest fleet that holds (Needed), a role it cuts holding at the peak
// as a spare must. Summary says it in one plain sentence.
func (r *reporter) verdict(np, nd int, base fleetEval) (whatifapi.ReportVerdict, error) {
	v := whatifapi.ReportVerdict{Scope: whatifapi.ScopeFleet, Roles: []whatifapi.ReportRole{}, Shrink: []string{}, Grow: []string{},
		Checks: []whatifapi.ReportCheck{check(base)}}
	scope := ""
	if base.kneeOK && !base.prefillPriced {
		v.Scope, scope = whatifapi.ScopeDecodeOnly, " (decode only)"
	}
	r.results = append(r.results, base.ttftAt, base.tpotAt)
	v.Reason = fleetText(np, nd) + ": " + base.reason
	lead := ""
	if !r.brief {
		lead = "at " + r.loadWords + ", "
	}
	lighter := r.phasesLighter()
	switch {
	case !base.kneeOK:
		v.Verdict = whatifapi.VerdictNotJudged
		v.Summary = fmt.Sprintf("%s: the decode knee is not priced (%s).", v.Verdict, refusalWords(base.decodeKnee))
		return v, nil
	case r.loadRefused != nil:
		v.Verdict = whatifapi.VerdictNotJudged
		v.Summary = fmt.Sprintf("%s: %s.", v.Verdict, refusalWords(*r.loadRefused))
		return v, nil
	case !(r.load > 0):
		v.Verdict = whatifapi.VerdictNotJudged
		v.Summary = v.Verdict + ": the window saw no traffic to judge at; state a load (--load-seen)."
		return v, nil
	case base.under && r.notJudgedUnder(np, nd, base, &v):
		return v, nil
	case base.under:
		v.Verdict = whatifapi.VerdictUnder
		cause, role, why := r.underWords(base)
		v.Reason += ". Add " + role + ": " + why
		need, notes, err := r.needed(np, nd, base, &v)
		if err != nil {
			return v, err
		}
		v.Summary = fmt.Sprintf("%s%s: %s%s; %s%s.", v.Verdict, scope, lead, cause, need, noteText(notes))
		return v, nil
	case base.pastLowerBound:
		r.lowerBoundVerdict(base, lead, &v)
		return v, nil
	case !base.known && lowerBoundVPTTFT(base.slo.unknownFig):
		// The TTFT priced is over its target from a lower bound of V_P (#515).
		v.Verdict = whatifapi.VerdictNotJudged
		v.Reason += ". " + lowerBoundVPWords
		at, _ := valueOf(base.slo.unknownFig)
		v.Summary = fmt.Sprintf("%s: %s%s is %s against a %s target: %s; --assume-vp states a V_P to price it.", v.Verdict, lead,
			base.slo.unknown, secondsWords(at), secondsWords(r.ttft.Seconds), lowerBoundVPWords)
		return v, nil
	case !base.known:
		v.Verdict = whatifapi.VerdictNotJudged
		v.Summary = fmt.Sprintf("%s: %s%s cannot be priced (%s).", v.Verdict, lead, base.slo.unknown,
			refusalWords(base.slo.unknownFig))
		return v, nil
	case lighter != "":
		// Ahead of decode alone holding: its decode-only need is sized on
		// the same knee, and can name fewer decode than the fleet has.
		v.Verdict = whatifapi.VerdictNotJudged
		v.Reason += ". " + lighter
		v.Summary = fmt.Sprintf("%s: %s %s.", v.Verdict, phasesWords, lighter)
		return v, nil
	case !base.prefillPriced:
		v.Verdict = whatifapi.VerdictNotJudged
		v.Reason += ". The prefill side is not priced: decode alone cannot show the fleet right-sized or over-provisioned"
		need, err := r.decodeOnlyNeed(np, nd, base, &v)
		if err != nil {
			return v, err
		}
		v.Summary = fmt.Sprintf("%s: decode only: decode alone holds %s%s; %s; prefill was not sized (%s), so this is not a fleet recommendation.",
			v.Verdict, r.targetWords(), strings.TrimSuffix(" "+lead, ", "), need, refusalWords(base.prefillKnee))
		return v, nil
	}
	roles, restsOnDecode, notes, err := r.roles(np, nd, base, &v)
	if err != nil {
		return v, err
	}
	if len(v.Shrink) > 0 {
		v.Verdict = whatifapi.VerdictOver
		need, more, err := r.needed(np, nd, base, &v)
		if err != nil {
			return v, err
		}
		for _, n := range more {
			if !slices.Contains(notes, n) {
				notes = append(notes, n)
			}
		}
		if restsOnDecode && !r.kneeV.Validated() {
			// Outside its region the decode knee read high on the goldens: a
			// decode replica removed on it is the step most exposed to that.
			v.Reason += ". " + decodeKneeOutside()
			notes = append([]string{"decode knee outside its validated region"}, notes...)
		}
		v.Summary = fmt.Sprintf("%s: %s%s; %s%s.", v.Verdict, lead, roles, need, noteText(notes))
		return v, nil
	}
	v.Verdict = whatifapi.VerdictRight
	if err := r.rightSizedNeed(np, nd, base, &v); err != nil {
		return v, err
	}
	if r.brief {
		v.Summary = v.Verdict + ": holds, no replica to spare."
	} else {
		v.Summary = fmt.Sprintf("%s: %sthe fleet holds %s with headroom; %s%s.", v.Verdict, lead, r.targetWords(), roles, noteText(notes))
	}
	if r.ttft == nil && r.tpot == nil {
		v.Reason += ". " + noSLONote
	}
	return v, nil
}

// ttftNeverPriced reports whether a TTFT is refused for a reason no replica
// count lifts: a prefill speed, fixed latency or imbalance the source did not
// learn, or one known only as the rate a pod was seen to compute (both
// NoPrefillCapacity). A rate past a lower-bound knee (PastLowerBoundKnee)
// is not one: more prefill replicas raise that knee.
func ttftNeverPriced(r whatif.Result) bool {
	u, ok := r.(whatif.Unanswerable)
	if !ok {
		return false
	}
	switch u.Reason {
	case whatif.NoPrefillCapacity, whatif.NoFixedLatency, whatif.NoImbalance:
		return true
	}
	return false
}

// decodeOnlyNeed is, on a fleet whose prefill side is not priced and whose
// decode holds, how many decode replicas hold decode's own check (needed's
// decode-only search), in words: "4-6 decode would hold it", from the
// fewest to the fleet's own. It sets v.Needed, decode only.
func (r *reporter) decodeOnlyNeed(np, nd int, base fleetEval, v *whatifapi.ReportVerdict) (string, error) {
	words, _, err := r.needed(np, nd, base, v)
	if err != nil || v.Needed == nil || v.Needed.Unmet != "" {
		return words, err
	}
	if d := v.Needed.Decode; d < nd {
		return fmt.Sprintf("%d-%d decode would hold it", d, nd), nil
	}
	return fmt.Sprintf("no fewer than its %d decode would hold it", nd), nil
}

// A window contradicts the model when the fleet priced, or a smaller one,
// served more than contradictShare times the fleet knee the model prices
// for it, at the shape priced, for contradictSpan or longer without a
// break: a load the model says saturates the fleet was served for
// minutes. A verdict priced on that knee would say UNDER-PROVISIONED of a
// fleet the window shows holding, so none is given (#297).
const (
	contradictShare = 1.25
	contradictSpan  = 180.0 // seconds
	// contradictGapCycles is how many cycle times apart two consecutive
	// cycles may sit and still count as one run (#358).
	contradictGapCycles = 2.0
)

// untimedRunWords says how a run was measured where the recording holds no
// per-cycle clock: a gap in it cannot be seen.
const untimedRunWords = "counted at the mean cycle time: the recording holds no per-cycle clock"

// contradictsWords opens the verdict's summary when the window contradicts
// the model, and phasesWords when the load and the shape priced are of
// different traffic.
const (
	contradictsWords = "the model contradicts this window:"
	phasesWords      = "the load and the shape priced are of different traffic:"
)

// phasesOutputFloor is the fewest output tokens a shape priced must run
// over the load's, beside the relative tolerance, to withhold an UNDER
// whose knee prefill sets (phasesDiffer).
const phasesOutputFloor = 64

// notJudgedUnder makes v NOT JUDGED, and says so, where an UNDER-PROVISIONED
// verdict would rest on a reading the window itself does not support: the
// load judged ran at another shape than the measured one priced
// (whatifapi.ShapeLoadMismatch, #294), so the knee is of other traffic; or
// the window's own fleet served past the knee at the shape priced
// (contradicted). Only an UNDER is withheld: neither says the fleet holds.
func (r *reporter) notJudgedUnder(np, nd int, base fleetEval, v *whatifapi.ReportVerdict) bool {
	why, lead := r.phasesDiffer(base.binds), phasesWords
	if why == "" && base.pastOperating {
		// Only a load past the knee is in question: a latency target missed
		// under it is the queue model's, which a window served past the knee
		// says nothing of.
		why, lead = r.contradicted(np, nd, base), contradictsWords
	}
	if why == "" {
		return false
	}
	v.Verdict = whatifapi.VerdictNotJudged
	v.Reason += ". " + why
	v.Summary = fmt.Sprintf("%s: %s %s.", v.Verdict, lead, why)
	return true
}

// lowerBoundKneeWords says why a load past a lower-bound prefill knee is not
// judged.
const lowerBoundKneeWords = "the prefill knee rests on a lower bound of V_P, and the true knee may be higher, " +
	"so whether the fleet saturates is not priced"

// lowerBoundVerdict makes v NOT JUDGED where the load is past the operating
// point of a prefill knee priced from a lower bound of V_P while decode's own
// holds (fleetEval.pastLowerBound): that knee reads low by construction, so
// it never shows the fleet saturated, and the summary gives it as "at least"
// its rate (#280). A load of other traffic than the shape priced says so
// first, as it does where it withholds an UNDER (notJudgedUnder).
func (r *reporter) lowerBoundVerdict(base fleetEval, lead string, v *whatifapi.ReportVerdict) {
	v.Verdict = whatifapi.VerdictNotJudged
	if why := r.phasesDiffer(base.binds); why != "" {
		v.Reason += ". " + why
		v.Summary = fmt.Sprintf("%s: %s %s.", v.Verdict, phasesWords, why)
		return
	}
	past := "past the operating point of a prefill knee of at least " + rateWords(base.knee)
	if r.load >= base.knee {
		past = fmt.Sprintf("%.1fx a prefill knee of at least %s", r.load/base.knee, rateWords(base.knee))
	}
	v.Reason += ". " + lowerBoundKneeWords
	v.Summary = fmt.Sprintf("%s: %s%s: %s; decode holds its operating point; --assume-vp states a V_P to price it.",
		v.Verdict, lead, past, lowerBoundKneeWords)
}

// phasesDiffer says how the load's cycles ran at another shape than the
// measured shape priced, "" where they did not or a shape was stated. Only
// a shape priced heavier than the load's in a length (more than
// whatifapi.ShapeLoadTolerance over it) withholds: its knee reads low for
// the load, while an UNDER priced on a lighter shape's knee holds the more.
// Where prefill sets the knee, an output longer by fewer than
// phasesOutputFloor tokens does not withhold: a few output tokens move a
// prefill knee little (8 against 21 on the 32B warm-ups).
func (r *reporter) phasesDiffer(binds string) string {
	ls := r.loadShape
	il, ol := r.pv.shape.AvgInputTokens, r.pv.shape.AvgOutputTokens
	heavier := func(x, ref float64) bool { return x > 0 && ref > 0 && x/ref-1 > whatifapi.ShapeLoadTolerance }
	olHeavier := ls != nil && heavier(ol, ls.OutputTokens) && (binds != rolePrefill || ol-ls.OutputTokens >= phasesOutputFloor)
	if !r.sh.Measured || ls == nil || !(heavier(il, ls.InputTokens) || olHeavier) {
		return ""
	}
	return r.shapesWords("so the knee is of other traffic and no verdict is priced on it")
}

// phasesLighter says how the load's cycles ran heavier than the measured
// shape priced, "" where they did not or a shape was stated: a shape priced
// lighter than the load's in a length (more than
// whatifapi.ShapeLoadTolerance under it) reads its knee high for the load,
// so a verdict that the fleet holds, RIGHT-SIZED or OVER-PROVISIONED, is not
// given on it (#361). It can cut replicas the load needs. An UNDER on such a
// knee holds the more, and keeps its verdict.
func (r *reporter) phasesLighter() string {
	ls := r.loadShape
	il, ol := r.pv.shape.AvgInputTokens, r.pv.shape.AvgOutputTokens
	lighter := func(x, ref float64) bool { return x > 0 && ref > 0 && 1-x/ref > whatifapi.ShapeLoadTolerance }
	if !r.sh.Measured || ls == nil || !(lighter(il, ls.InputTokens) || lighter(ol, ls.OutputTokens)) {
		return ""
	}
	return r.shapesWords("so the knee reads high for the load and no verdict that the fleet holds is priced on it")
}

// shapesWords says the load's cycles' shape and the shape priced, then so,
// and how to price the load at its own shape.
func (r *reporter) shapesWords(so string) string {
	ls := r.loadShape
	return fmt.Sprintf("the load's cycles ran at IL %s OL %s, the shape priced is IL %s OL %s, %s; --shape load prices the load at its own shape",
		num(round(ls.InputTokens)), num(round(ls.OutputTokens)), num(round(r.pv.shape.AvgInputTokens)), num(round(r.pv.shape.AvgOutputTokens)), so)
}

// decodeKneeScale is the decode knee at the shape priced over the knee at
// cycle c's shape: a rate c served, times it, is that rate at the shape
// priced. A decode knee is the sequences resident, the fill times the KV
// capacity over the KV one request holds (KVreq = ILeff + D*OL/2), each
// served once every OL steps: it goes as 1/(OL*KVreq). The prompt's KV is
// resident on the decode pods, P/D or not, so a shorter prompt raises the
// knee too. Both KVreq are read with the shape priced's prefix hit and
// output dispersion, so only the lengths move the ratio. Where the batch
// cap binds, the knee moves less with KVreq than that, so the KVreq ratio
// is taken only where it is below 1: a cycle then counts at or below its
// share at the shape priced, never past it. Callers check both lengths are
// above 0.
func (r *reporter) decodeKneeScale(c learnreplay.CycleShape) float64 {
	s := r.pv.shape
	kv := func(il, ol float64) float64 {
		return shape.New(il, ol, s.PrefixHitRate).WithOutputDispersion(s.OutputDispersion).KVreq
	}
	return c.OutputTokens / s.AvgOutputTokens * min(1, kv(c.InputTokens, c.OutputTokens)/kv(s.AvgInputTokens, s.AvgOutputTokens))
}

// contradicted says how the window's own fleet served more than the model
// prices it to, "" where it did not or cannot say: a shape stated by the
// question, a cycle time, per-cycle fleet or shape the recording does not
// hold, no knee, or a prefill knee read off a lower bound of V_P (it reads
// low by construction). A cycle counts when it ran at the shape priced
// (within whatifapi.ShapeLoadTolerance in each length), every role served
// at least one replica and at most the replicas priced (CurrentReplicas
// less PendingReplicas, summed over the role's variants), and the decode
// replicas served above the share times the fleet knee; a cycle lighter
// than the shape priced counts its rate scaled down to the shape priced, as
// its own knee is that much higher: by the prompt ratio where prefill binds,
// and by decodeKneeScale where decode does. An aggregated fleet, with no
// prefill variant, is one pool judged on its own knee: the decode knee its
// line prices, fitted on pods that prefill too, with the pool's run counted
// on its replicas; its scale is the lower of decodeKneeScale and the prompt
// ratio, since a shorter prompt also shortens the prefill work in its steps
// (#357). The share is
// contradictShare, over the band's top where that sets the decode knee:
// such a knee stops at the highest fill the line was fitted to, short of a
// full replica. Where the recording holds every cycle's clock
// (learnreplay.State.CycleTimes), a run breaks where two consecutive cycles
// sit more than contradictGapCycles cycle times apart (a controller restart,
// a capture stopped and resumed), and its length is clock time: first cycle
// to last, plus one cycle. Else cycles are counted at the mean cycle time,
// and the words say so (#358).
func (r *reporter) contradicted(np, nd int, base fleetEval) string {
	sec := cycleSeconds(r.st)
	il, ol := r.pv.shape.AvgInputTokens, r.pv.shape.AvgOutputTokens
	if !r.sh.Measured || !base.kneeOK || !(base.knee > 0) || !(sec > 0) || !(il > 0) || !(ol > 0) {
		return ""
	}
	share := contradictShare
	switch base.binds {
	case rolePrefill:
		if a, ok := base.prefillKnee.(whatif.Answer); !ok || slices.Contains(a.Flags, whatif.VPLowerBound) {
			return ""
		}
	case roleDecodeWord:
		if a, ok := base.decodeKnee.(whatif.Answer); ok && slices.Contains(a.Basis.BoundBy, whatif.BoundBandTop) && r.pv.decode.Band.Hi > 0 && r.pv.decode.Band.Hi <= 1 {
			share /= r.pv.decode.Band.Hi
		}
	}
	shapeAt := map[int]learnreplay.CycleShape{}
	for _, c := range r.decodeShapeSeries() {
		shapeAt[c.Cycle] = c
	}
	n := len(r.st.ArrivalRates)
	dec, pre, served := make([]int, n), make([]int, n), make([]float64, n)
	// unknown marks a cycle whose fleet cannot be read (a variant not named
	// there, -1), or where more than one decode variant served: the knee and
	// the shape are the priced variant's alone.
	unknown, decodeVariants := make([]bool, n), make([]int, n)
	hasPrefill := false
	for _, v := range r.st.Variants {
		rows := r.st.ServingReplicas[v.Variant]
		if len(rows) == 0 {
			return ""
		}
		into := dec
		if domain.CanonicalRole(v.Role) == domain.RolePrefill {
			into, hasPrefill = pre, true
		} else {
			for _, c := range v.ShapeSeries {
				if c.Cycle >= 1 && c.Cycle <= n {
					served[c.Cycle-1] += c.RequestRate
				}
			}
		}
		for i := range n {
			switch {
			case i >= len(rows) || rows[i] < 0:
				unknown[i] = true
				continue
			case domain.CanonicalRole(v.Role) != domain.RolePrefill && rows[i] > 0:
				if decodeVariants[i]++; decodeVariants[i] > 1 {
					unknown[i] = true
				}
			}
			into[i] += rows[i]
		}
	}
	checkPrefill := hasPrefill && np > 0
	within := func(x, most int) bool { return x >= 1 && x <= most }
	over := share * base.knee
	times := r.st.CycleTimes
	timed := len(times) == n && !slices.ContainsFunc(times, time.Time.IsZero)
	step := sec
	if g := whatifapi.WindowGapsOf(times); timed && g != nil {
		step = g.CycleSeconds
	}
	run, best, start := 0, 0, 0
	var runPeak, bestPeak, span float64
	for i := range n {
		c, ok := shapeAt[i+1]
		atShape := ok && c.InputTokens > 0 && c.OutputTokens > 0 &&
			!whatifapi.ShapeLoadMismatch(c.InputTokens, c.OutputTokens, whatifapi.ReportLoadShape{InputTokens: il, OutputTokens: ol})
		x := served[i]
		if atShape {
			// A cycle lighter than the shape priced, within the tolerance,
			// has a knee higher by up to its share: its served rate counts
			// at the shape priced, so it does not pass for a contradiction.
			switch base.binds {
			case rolePrefill:
				x *= min(1, c.InputTokens/il)
			case roleDecodeWord:
				s := r.decodeKneeScale(c)
				if !hasPrefill {
					// An aggregated pool also prefills on the same pods,
					// in the steps its line was fitted on: a shorter
					// prompt shortens them too (#357).
					s = min(s, c.InputTokens/il)
				}
				x *= min(1, s)
			}
		}
		if unknown[i] || !atShape || !(whatif.Finite(x) && x > over) || !within(dec[i], nd) || (checkPrefill && !within(pre[i], np)) {
			run, runPeak = 0, 0
			continue
		}
		if timed && run > 0 && times[i].Sub(times[i-1]).Seconds() > contradictGapCycles*step {
			run, runPeak = 0, 0
		}
		if run == 0 {
			start = i
		}
		run++
		runPeak = max(runPeak, x)
		s := float64(run) * sec
		if timed {
			s = times[i].Sub(times[start]).Seconds() + step
		}
		if s > span {
			span, best, bestPeak = s, run, runPeak
		}
	}
	if span < contradictSpan {
		return ""
	}
	fleet := fmt.Sprintf("%d decode", nd)
	if checkPrefill {
		fleet = fleetText(np, nd)
	}
	untimed := ""
	if !timed {
		untimed = ", " + untimedRunWords
	}
	return fmt.Sprintf("%s or fewer served above %.2fx the fleet knee of %s at the shape priced for %s without a break (%d cycles%s), up to %s (%.1fx); "+
		"the knee reads low for this window, so no verdict is priced on it", fleet, share, rateWords(base.knee),
		minutesWords(span), best, untimed, rateWords(bestPeak), bestPeak/base.knee)
}

// minutesWords is seconds as whole minutes, at least one.
func minutesWords(sec float64) string {
	return fmt.Sprintf("%d min", max(int(math.Round(sec/60)), 1))
}

// rightSizedNeed fills v.Needed on a right-sized fleet, which has no
// replica of either role to spare. With the roles' GPUs per replica unequal
// a mix of fewer GPUs may still hold (9 prefill + 4 decode right-sized while
// 8 + 5 holds on fewer GPUs at 8 and 1 a replica): the search runs, and
// Needed is its fleet when that takes fewer GPUs than the fleet judged. Else,
// with a spare asked, Needed is the fleet judged, not searched (Minimizes
// empty), for its spare; else nil. The verdict's grow and words are not
// touched: the fleet judged holds.
func (r *reporter) rightSizedNeed(np, nd int, base fleetEval, v *whatifapi.ReportVerdict) error {
	g, err := r.gpusPerReplica()
	if err != nil {
		return err
	}
	judged := whatifapi.FleetCounts{Prefill: np, Decode: nd}
	if whatifapi.WeighsGPUs(g) {
		grow := v.Grow
		words, notes, err := r.needed(np, nd, base, v)
		if err != nil {
			return err
		}
		v.Grow = grow
		n := v.Needed
		if n.Unmet == "" && (whatifapi.FleetCounts{Prefill: n.Prefill, Decode: n.Decode}).Cost(g) < judged.Cost(g) {
			// The sentence is the right-sized one; the cheaper mix, and any
			// f it rests on, are in the reason.
			v.Reason += ". A mix of fewer GPUs also holds: " + strings.TrimPrefix(words, "needs ") + noteText(notes)
			return nil
		}
		v.Needed = nil
	}
	if r.q.Spare > 0 {
		v.Needed = &whatifapi.ReportNeeded{Prefill: np, Decode: nd, MaxReplicas: whatifapi.DefaultSizeBound,
			GPUs: whatifapi.FleetGPUs(judged, g), WithSpare: whatifapi.WithSpare(judged, r.q.Spare, g)}
		if v.Needed.GPUs != nil {
			v.Needed.GPUsPerReplica = &g
		}
	}
	return nil
}

// noteText is notes in parentheses, "" with none.
func noteText(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	return " (" + strings.Join(notes, "; ") + ")"
}

// roles judges each role's own standing at the load: a role has a replica
// to spare when one fewer of it holds the whole fleet at the load judged,
// and its own check (knee and target) at the peak as well; decode, also
// where one fewer still meets the decode knee's error bound. It fills
// v.Roles, v.Shrink and v.Checks, and says the roles in words, whether a
// spare rests on the decode knee, and the assumed f any of it rests on.
func (r *reporter) roles(np, nd int, base fleetEval, v *whatifapi.ReportVerdict) (string, bool, []string, error) {
	shrinkAt := max(r.load, r.peak)
	var words, notes []string
	note := func(e fleetEval) {
		if e.fAssumed != "" && !slices.Contains(notes, e.fAssumed) {
			notes = append(notes, e.fAssumed)
		}
	}
	note(base)
	restsOnDecode := false
	for _, role := range []string{roleDecodeWord, rolePrefill} {
		n, fp, fd, op := nd, np, nd-1, base.dOp
		if role == rolePrefill {
			n, fp, fd, op = np, np-1, nd, base.pOp
		}
		rr := whatifapi.ReportRole{Role: role}
		switch {
		case n <= 1:
			rr.Detail = "no spare (one replica)"
		default:
			e, err := r.evaluateAt(fp, fd, r.load, false)
			if err != nil {
				return "", false, nil, err
			}
			v.Checks = append(v.Checks, check(e))
			rr.Spare = e.holds
			peak := ""
			if rr.Spare && shrinkAt > r.load {
				ep, err := r.evaluateAt(fp, fd, shrinkAt, false)
				if err != nil {
					return "", false, nil, err
				}
				rr.Spare = (role == roleDecodeWord && ep.decodeOK) || (role == rolePrefill && ep.prefillOK)
				peak = " even at the peak " + rateWords(shrinkAt)
				if !rr.Spare {
					rr.Detail = "no spare: one fewer misses at the peak " + rateWords(shrinkAt)
				}
			}
			if d, eb, ok := r.decodeAtBound(base, nd); rr.Spare && role == roleDecodeWord && ok && d > nd-1 {
				// One fewer holds at the point estimate, but not if the
				// decode knee reads as high as its label allows (#363).
				rr.Spare, rr.Detail = false, "no spare: "+boundWords(d, eb)
			}
			switch {
			case rr.Spare:
				rr.Detail = "one fewer holds" + peak
				v.Shrink = append(v.Shrink, role)
				restsOnDecode = restsOnDecode || role == roleDecodeWord || e.binds == roleDecodeWord
				note(e)
			case rr.Detail == "":
				rr.Detail = "no spare" + r.marginWords(op)
			}
		}
		if r.brief {
			rr.Detail, _, _ = strings.Cut(rr.Detail, " (")
		}
		v.Roles = append(v.Roles, rr)
		words = append(words, role+": "+rr.Detail)
	}
	return strings.Join(words, "; "), restsOnDecode, notes, nil
}

// marginWords is how close the load is to a role's operating point op.
func (r *reporter) marginWords(op float64) string {
	switch {
	case !(op > 0):
		return ""
	case r.load <= op:
		return fmt.Sprintf(" (%.1f%% under its operating point)", 100*(op-r.load)/op)
	}
	return " (past its operating point)"
}

// needed finds the smallest fleet that holds every target at the load with
// headroom, the fewest GPUs where the recording states each role's GPUs per
// replica, else the fewest replicas (ties to fewer replicas, then fewer
// decode), by POST /v1/size's search (whatifapi.SmallestFleet) on the
// report's own check, and says it: "needs 3 prefill + 3 decode (has 2 + 2)".
// Its GPUs, and the fleet with the spare asked, are in v.Needed (the text's
// needed line).
// A role it cuts, below np prefill or nd decode, must also hold its own
// check at the peak, the test roles judges a spare by: removing capacity
// is the costly mistake. Decode it cuts no lower than the count the load
// needs if the decode knee reads as high as its label allows (#363), as a
// decode spare must. Both tests being monotone in each count, it then
// never cuts a role roles says has no spare unless it trades the role for
// more of the other, which v.Grow names and the sentence says. Such a trade is not capped at the
// fleet's count of the other role: an under-provisioned fleet needs more
// of it anyway.
// With the prefill side not priced, it is the fewest decode replicas whose
// own check holds. It fills v.Needed and adds the fleets tried to nothing:
// the checks list the fleets the verdict rests on. notes are the assumed f
// the answer rests on.
func (r *reporter) needed(np, nd int, base fleetEval, v *whatifapi.ReportVerdict) (string, []string, error) {
	bound := whatifapi.DefaultSizeBound
	out := &whatifapi.ReportNeeded{MaxReplicas: bound}
	v.Needed = out
	gpus, err := r.gpusPerReplica()
	if err != nil {
		return "", nil, err
	}
	// sized fills the needed fleet's GPUs and its spare, once its counts
	// are set.
	sized := func() {
		n := whatifapi.FleetCounts{Prefill: out.Prefill, Decode: out.Decode}
		if out.GPUs = whatifapi.FleetGPUs(n, gpus); out.GPUs != nil {
			out.GPUsPerReplica = &gpus
		}
		out.WithSpare = whatifapi.WithSpare(n, r.q.Spare, gpus)
	}
	at := func(n whatifapi.FleetCounts) (fleetEval, error) {
		return r.evaluateAt(n.Prefill, n.Decode, r.load, false)
	}
	// cutHolds is whether each role n cuts holds its own check at the peak.
	// True for a fleet that cuts no role, or with the peak no higher than
	// the load. It is monotone in each count, as the search takes holding
	// to be.
	shrinkAt := max(r.load, r.peak)
	cutHolds := func(n whatifapi.FleetCounts) (bool, error) {
		cutP, cutD := n.Prefill < np, n.Decode < nd
		if !(shrinkAt > r.load) || (!cutP && !cutD) {
			return true, nil
		}
		e, err := r.evaluateAt(n.Prefill, n.Decode, shrinkAt, false)
		if err != nil {
			return false, err
		}
		return (!cutP || e.prefillOK) && (!cutD || e.decodeOK), nil
	}
	// decodeHolds is decode's own check, which the prefill count does not
	// move: at the load, and at the peak on a cut.
	// boundHolds is whether n cuts decode no lower than the count the load
	// needs if the decode knee reads as high as its label allows (#363):
	// a cut must hold at that bound, as a decode spare must. Monotone in the
	// decode count.
	dBound, _, bounded := r.decodeAtBound(base, nd)
	boundHolds := func(n whatifapi.FleetCounts) bool {
		return !bounded || n.Decode >= nd || n.Decode >= dBound
	}
	decodeHolds := func(n whatifapi.FleetCounts) (bool, error) {
		if !boundHolds(n) {
			return false, nil
		}
		e, err := at(n)
		if err != nil || !e.decodeOK {
			return false, err
		}
		if n.Decode >= nd || !(shrinkAt > r.load) {
			return true, nil
		}
		e, err = r.evaluateAt(n.Prefill, n.Decode, shrinkAt, false)
		return err == nil && e.decodeOK, err
	}
	holds := func(n whatifapi.FleetCounts) (bool, error) {
		if !boundHolds(n) {
			return false, nil
		}
		e, err := at(n)
		if err != nil || !e.holds {
			return false, err
		}
		return cutHolds(n)
	}
	// decodeOnly is the need on decode's own check alone: the fewest decode
	// replicas, a lower bound of the fleet's need.
	decodeOnly := func() (string, []string, error) {
		d, ok, err := whatifapi.SmallestCount(1, bound, func(d int) (bool, error) {
			return decodeHolds(whatifapi.FleetCounts{Prefill: max(np, 1), Decode: d})
		})
		switch {
		case err != nil:
			return "", nil, err
		case !ok:
			out.Unmet = roleDecodeWord
			return fmt.Sprintf("no count up to %d decode replicas holds", bound), nil, nil
		}
		out.Decode, out.DecodeOnly = d, true
		sized()
		if d > nd {
			v.Grow = append(v.Grow, roleDecodeWord)
		}
		return fmt.Sprintf("needs at least %d decode (has %d)", d, nd) + r.atBound(base, nd, out), nil, nil
	}
	// A TTFT the source cannot price at any fleet (ttftNeverPriced) holds at
	// no fleet the search would try: the need is decode's alone, a lower
	// bound, never "no fleet holds".
	if !base.prefillPriced || (r.ttft != nil && ttftNeverPriced(base.ttftAt)) {
		return decodeOnly()
	}
	cost, minimizes := whatifapi.ReplicaCost(gpus)
	out.Minimizes = minimizes
	search := func() (whatifapi.FleetCounts, string, error) {
		return whatifapi.SmallestFleet(whatifapi.FleetCounts{Prefill: bound, Decode: bound}, cost, decodeHolds, holds)
	}
	n, unmet, err := search()
	switch {
	case err != nil:
		return "", nil, err
	case unmet != "" && r.ttft != nil && lowerBoundVPTTFT(base.ttftAt):
		// The TTFT is priced from a lower bound of V_P, so no fleet is shown
		// to hold it, nor to miss it: the need is decode's.
		return decodeOnly()
	case unmet != "":
		out.Unmet = unmet
		return fmt.Sprintf("no fleet up to %d replicas of each role holds", bound), nil, nil
	}
	out.Prefill, out.Decode = n.Prefill, n.Decode
	sized()
	if n.Prefill > np {
		v.Grow = append(v.Grow, rolePrefill)
	}
	if n.Decode > nd {
		v.Grow = append(v.Grow, roleDecodeWord)
	}
	var notes []string
	if e, err := at(n); err != nil {
		return "", nil, err
	} else if e.fAssumed != "" {
		notes = append(notes, e.fAssumed)
	}
	words := []string{fmt.Sprintf("needs %d prefill + %d decode (has %d + %d)", n.Prefill, n.Decode, np, nd) + r.atBound(base, nd, out)}
	// A role the roles say has no spare is cut only in trade for more of
	// the other: say so, or the sentence reads as cutting what it just
	// said it cannot.
	for _, rr := range v.Roles {
		cut, other, grown := n.Prefill < np, roleDecodeWord, n.Decode > nd
		if rr.Role == roleDecodeWord {
			cut, other, grown = n.Decode < nd, rolePrefill, n.Prefill > np
		}
		if !rr.Spare && cut && grown {
			words[0] += fmt.Sprintf(", trading %s for %s", rr.Role, other)
		}
	}
	for _, alt := range []whatifapi.FleetCounts{{Prefill: n.Prefill - 1, Decode: n.Decode + 1}, {Prefill: n.Prefill + 1, Decode: n.Decode - 1}} {
		if alt.Prefill < 1 || alt.Decode < 1 || alt.Prefill > bound || alt.Decode > bound {
			continue
		}
		ok, err := holds(alt)
		if err != nil {
			return "", nil, err
		}
		if ok {
			out.Alternatives = append(out.Alternatives, alt)
			words = append(words, fmt.Sprintf("or %d + %d", alt.Prefill, alt.Decode))
		}
	}
	if r.brief || n.Prefill < 2 {
		// A growth line is short, and its own search is enough; on one
		// prefill pod there is no f the answer rests on.
		return strings.Join(words, ", "), notes, nil
	}
	sens, err := r.fSensitivity(search)
	if err != nil {
		return "", nil, err
	}
	out.FSensitivity = sens
	return strings.Join(words, ", ") + fSensitivityWords(n, sens), notes, nil
}

// atBound sizes decode at the decode knee's error bound and fills
// out.DecodeAtErrorBound and out.DecodeKneeError: the decode replicas the
// load needs at the operating point when the knee reads as high as its label
// allows, ceil(load x (1 + e) / (headroom x knee per replica)), e the upper
// error of the envelope row that validates the knee, or the 0.6B goldens'
// decodeKneeGoldensHigh outside every row (#297), capped at out.MaxReplicas
// as the search is. It says the count in words where it is above the point
// estimate out.Decode, "" otherwise.
func (r *reporter) atBound(base fleetEval, nd int, out *whatifapi.ReportNeeded) string {
	d, e, ok := r.decodeAtBound(base, nd)
	if !ok {
		return ""
	}
	d = max(d, out.Decode)
	more := ""
	if out.MaxReplicas > 0 && d > out.MaxReplicas {
		// The search sizes no fleet past its bound; nor does the bound count.
		d, more = out.MaxReplicas, "more than "
	}
	out.DecodeAtErrorBound, out.DecodeKneeError = d, e
	if d <= out.Decode {
		return ""
	}
	return fmt.Sprintf(", %s%d decode if the decode knee reads %.0f%% high", more, d, e*100)
}

// boundWords says the decode count d the load needs when the decode knee
// reads e high, "more than" the search's bound past it, as atBound does.
func boundWords(d int, e float64) string {
	more := ""
	if d > whatifapi.DefaultSizeBound {
		d, more = whatifapi.DefaultSizeBound, "more than "
	}
	return fmt.Sprintf("%s%d decode if the decode knee reads %.0f%% high", more, d, e*100)
}

// decodeAtBound is the decode replicas the load needs at the operating
// point when the decode knee, priced at nd replicas, reads as high as its
// label allows, and that error e; ok is false with no knee, fleet, load or
// headroom to size by. It sizes by the knee alone, not by a TPOT target.
func (r *reporter) decodeAtBound(base fleetEval, nd int) (int, float64, bool) {
	k, ok := valueOf(base.decodeKnee)
	if !ok || !(k > 0) || nd < 1 || !(r.load > 0) || !(r.headroom > 0) {
		return 0, 0, false
	}
	e := decodeKneeGoldensHigh
	if r.kneeV.Validated() {
		// A row with no upper error is unbounded on that side: no count.
		hi := r.kneeV.Row.ErrorHi
		if hi == nil || !whatif.Finite(*hi) {
			return 0, 0, false
		}
		e = max(*hi, 0)
	}
	// Tolerance: a load exactly at a count's operating point needs that
	// count. A tiny knee is held short of the int range; atBound caps it.
	x := math.Ceil(r.load*(1+e)/(r.headroom*k/float64(nd)) - 1e-9)
	return int(min(x, math.MaxInt32)), e, true
}

// targetWords names what a holding fleet holds: every target, or its
// operating point with none set.
func (r *reporter) targetWords() string {
	if r.ttft == nil && r.tpot == nil {
		return "its operating point (no SLO target is set)"
	}
	return "every target"
}

// underWords is why an under-provisioned fleet misses, in words, the role
// to add replicas of, and why that role: the knee's role when the miss is
// saturation, decode for TPOT, and for TTFT the role whose wait is the larger
// part of it at the load. Brief, the cause leaves out the load it is at.
func (r *reporter) underWords(e fleetEval) (cause, role, why string) {
	s := e.slo
	knee := e.binds + " sets the knee"
	// Past a lower-bound prefill knee, a load is judged against decode's
	// (satKnee): the words name that knee, never the lower bound's.
	sat, satName, its := e.satKnee, "the fleet knee", ""
	if e.satRole != e.binds {
		satName, its = "the "+e.satRole+" knee", " "+e.satRole
		knee = e.satRole + " sets the knee (the prefill knee rests on a lower bound of V_P)"
	}
	switch {
	case e.pastOperating && r.load >= sat:
		if r.brief {
			return fmt.Sprintf("%.1fx %s (%s)", r.load/sat, satName, rateWords(sat)), e.satRole, knee
		}
		return fmt.Sprintf("%.1fx %s of %s, the fleet saturates", r.load/sat, satName, rateWords(sat)), e.satRole, knee
	case e.pastOperating:
		return fmt.Sprintf("the fleet runs past its operating point of %s (%s of its %s%s knee)",
			rateWords(e.op), num(r.headroom), rateWords(sat), its), e.satRole, knee
	case s.ttftMissed && s.onset:
		return fmt.Sprintf("TTFT %s misses, past the prefill flow-control onset", whatifapi.ReportStatisticName(r.ttft.Percentile)),
			rolePrefill, "the rate is past the prefill flow-control onset, where the router queues"
	case s.ttftMissed:
		role, why := ttftRole(s.ttft, e)
		return missWords("TTFT", r.ttft, s.ttft), role, why
	case s.tpotMissed:
		return missWords("TPOT", r.tpot, s.tpot), roleDecodeWord, "decode sets TPOT"
	}
	return "a target is missed", e.binds, knee
}

// ttftRole is the role that sets a missed TTFT at the load, and why: the
// prefill queue or the decode side, whichever mean wait is the larger, the
// decode side named by its larger part, admission or the KV pull; the knee's
// role when the TTFT is refused for saturation.
func ttftRole(t whatif.Result, e fleetEval) (role, why string) {
	v, ok := t.(whatif.TTFT)
	if !ok {
		return e.binds, e.binds + " sets the knee, and TTFT saturates there"
	}
	p := v.Parts
	prefillWait, decodeWait := ttftWaits(p)
	switch {
	case pullSetsTTFT(p):
		return roleDecodeWord, fmt.Sprintf("the KV pull on the decode pods, a %s mean wait (decode admission %s, the prefill side %s), is the larger wait in TTFT",
			secondsWords(p.KVPullWait), secondsWords(p.DecodeWait), secondsWords(prefillWait))
	case decodeWait > prefillWait && p.KVPullWait > 0:
		return roleDecodeWord, fmt.Sprintf("the decode side, a %s mean wait (admission %s, the KV pull %s), is the larger wait in TTFT",
			secondsWords(decodeWait), secondsWords(p.DecodeWait), secondsWords(p.KVPullWait))
	case decodeWait > prefillWait:
		return roleDecodeWord, fmt.Sprintf("decode admission, a %s mean wait, is the larger wait in TTFT", secondsWords(decodeWait))
	}
	return rolePrefill, fmt.Sprintf("the prefill queue, a %s mean wait, is the larger wait in TTFT", secondsWords(prefillWait))
}

// ttftWaits are TTFT's mean waits on the prefill side (the engine, the
// router's gate and its burst) and on the decode side (admission and the KV
// pull).
func ttftWaits(p whatif.TTFTParts) (prefill, decode float64) {
	return p.EngineWait + p.RouterWait + p.BurstWait, p.DecodeWait + p.KVPullWait
}

// pullSetsTTFT says the KV pull is the larger wait in TTFT: on its own
// larger than the prefill side's wait and than decode admission's. The
// larger mean wait in TTFT then sits on the decode pods, while decode's KV
// fill and slots read near idle.
func pullSetsTTFT(p whatif.TTFTParts) bool {
	prefill, _ := ttftWaits(p)
	return p.KVPullWait > prefill && p.KVPullWait > p.DecodeWait
}

// missWords is one missed target in words.
func missWords(name string, t *whatifapi.ReportTarget, fig whatif.Result) string {
	label := name + " " + whatifapi.ReportStatisticName(t.Percentile)
	if v, ok := valueOf(fig); ok {
		return fmt.Sprintf("%s is %s against a %s target", label, secondsWords(v), secondsWords(t.Seconds))
	}
	return label + " is saturated"
}

// refusalWords is why a figure is refused, in words a reader acts on and
// with no reason code; "" on an answer.
func refusalWords(res whatif.Result) string {
	u, ok := res.(whatif.Unanswerable)
	if !ok {
		return ""
	}
	switch u.Reason {
	case whatif.NoPrefillCapacity:
		if slices.Contains(u.Flags, whatif.VPLowerBound) {
			return "prefill capacity unknown: the prefill pods never queued, so their speed is only a lower bound; --assume-vp states one"
		}
		return "prefill capacity unknown: the recording learned no prefill speed; --assume-vp states one"
	case whatif.NoFixedLatency:
		return "the recording learned no fixed prefill latency; --assume-a states one"
	case whatif.NoImbalance:
		return "the recording learned no prefill imbalance between pods"
	case whatif.NoPrefillReplicas:
		return "the fleet has no prefill replica"
	case whatif.FlowControlUnknown:
		return "the router's flow-control thresholds are not known"
	case whatif.Saturated, whatif.AboveDecodeOnset:
		return "the fleet is saturated there"
	case whatif.PastLowerBoundKnee:
		return "the prefill knee rests on a lower bound of V_P, and the true knee may be higher; --assume-vp states one"
	case whatif.NoTPOTHistogram, whatif.TPOTBucketTooWide, whatif.TPOTQuantileUnbounded:
		return "the TPOT histogram cannot give that percentile; judge TPOT at the mean"
	case whatif.NoTraffic:
		return "the recording saw no traffic of this shape"
	case whatifapi.ReasonWindowGapAtPeak:
		return "a gap in the window borders its busiest stretch, so the load may have been higher than the window shows; " +
			"capture the window again, or state a load (--load-seen)"
	case whatif.NoLine:
		return "the recording learned no usable decode latency line"
	case whatif.PinnedDefaultB:
		return "the decode latency line rests on the " + ms(itl.DefaultBaselineSec) + " default intercept B, which no engine measured; " +
			"--assume-itl-b (API assume.itlB or assume.itlBFromFloor) states one"
	case whatif.NoCapacity, whatif.NoBatchCap:
		return "the decode replicas' capacity is not known"
	}
	return "the figure is refused; the checks give the reason"
}

// check is one fleet's check on the wire.
func check(e fleetEval) whatifapi.ReportCheck {
	return whatifapi.ReportCheck{Prefill: e.np, Decode: e.nd, Holds: e.holds, Known: e.known, Reason: e.reason}
}

// fleetText names a fleet.
func fleetText(np, nd int) string {
	return fmt.Sprintf("%d prefill + %d decode", np, nd)
}

// flagNamesOf is every flag the results carry, once each, in the order they
// first appear.
func flagNamesOf(results []whatif.Result) []string {
	out := []string{}
	for _, r := range results {
		var from []whatif.Flag
		switch v := r.(type) {
		case whatif.Answer:
			from = v.Flags
		case whatif.TTFT:
			from = v.Flags
		case whatif.Unanswerable:
			from = v.Flags
		}
		for _, f := range from {
			if !slices.Contains(out, string(f)) {
				out = append(out, string(f))
			}
		}
	}
	return out
}

// measuredText says where a measured shape came from.
func measuredText(pv pricedVariant) string {
	m := pv.state.Shape
	if m.Cycles == 0 {
		return fmt.Sprintf("no traffic by cycle %d", pv.cycle)
	}
	return fmt.Sprintf("measured, cycles %d-%d", m.FromCycle, m.ToCycle)
}

// sourceAsked is a figure the question stated rather than the source read.
const sourceAsked = "asked"

// sourceObserved is a load the source read.
const sourceObserved = "observed"

// sourceLoadCycles is a shape asked as the load's: read at the cycles the
// load was read from.
const sourceLoadCycles = "the load's cycles"
