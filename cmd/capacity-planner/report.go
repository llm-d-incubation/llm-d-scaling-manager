package main

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
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

// decodeKneeNotValidated is the decode knee's standing on the goldens
// (docs/proposals/slo-capacity-planner.md, "Measured on the goldens").
const decodeKneeNotValidated = "decode knee: not validated; it read 44% to 103% high on the goldens " +
	"(docs/proposals/slo-capacity-planner.md, Measured on the goldens)"

// e2eNote says what the e2e column is.
const e2eNote = "e2e mean is a sum, TTFT mean + (OL - 1) x TPOT mean, not a percentile"

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
}

// fleetKey names a fleet priced: the shape, the counts and the statistics.
type fleetKey struct {
	shape        int
	np, nd       int
	ttftQ, tpotQ float64
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
	if len(r.q.Shapes) == 0 {
		r.q.Shapes = []whatifapi.ReportShapeAsk{{Measured: true, Label: "measured"}}
	}
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
		FirstAt: st.FirstAt, LastAt: st.At}
	out.Fleet = whatifapi.ReportFleet{Prefill: np, Decode: nd, PrefillVariant: base.pf.variant, DecodeVariant: base.pv.state.Variant,
		PrefillFrom: from(q.Prefill), DecodeFrom: from(q.Decode),
		DecodeEngine: engineInfo(base.pv.state.Params)}
	if base.pf.variant != "" {
		out.Fleet.PrefillEngine = engineInfo(variantParams(st, base.pf.variant))
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
	out.NotValidated = append(out.NotValidated, decodeKneeNotValidated)
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

// shapeReport prices the fleet at the shape r.sh: its exit code, and whether
// its TTFT priced.
func (r *reporter) shapeReport(ctx context.Context, np, nd int) (whatifapi.ReportShape, int, bool, error) {
	base, err := r.fleet(r.q.Prefill, r.q.Decode, r.sloTTFTQ(), r.sloTPOTQ())
	if err != nil {
		return whatifapi.ReportShape{}, 0, false, err
	}
	r.pv = base.pv
	if r.pv.bandFloor > 0 {
		r.addAssumed(adaptedBandFloorWords(r.pv))
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
	k := fleetKey{r.shape, np, nd, ttftQ, tpotQ}
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
		kn.PromptTokens, kn.OutputTokens, kn.PrefixHit = r.sh.IL, r.sh.OL, r.sh.Hit
	}
	sf, err := r.s.fleetAt(kn)
	if err != nil {
		return nil, err
	}
	r.fleets[k] = sf
	return sf, nil
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
// asked (p95 by default) of the per-cycle arrival rate over the window's
// cycles that read one above 0. A growth load is a what-if of its own
// (shapeReport), never the load judged at. The mean, p95 and
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
		mod := whatifapi.ModulationP95(rates, mean, p95)
		stats[whatifapi.LoadP95Modulation] = mod
		l.P95Modulation = &mod
		l.P95IsPeak = len(rates) < minP95Cycles
		l.Rolling = rollingPeak(r.st.ArrivalRates, cycleSeconds(r.st), whatifapi.RollingWindow.Seconds())
	}
	var seen float64
	switch {
	case r.q.LoadSeen > 0:
		seen, l.Source, l.Statistic = r.q.LoadSeen, sourceAsked, loadStated
	case len(rates) > 0:
		seen, l.Source = stats[l.Statistic], "observed"
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
	if len(rates) < k {
		return nil
	}
	var sum, peak float64
	for i, x := range rates {
		if whatif.Finite(x) && x > 0 {
			sum += x
		}
		if i >= k {
			if y := rates[i-k]; whatif.Finite(y) && y > 0 {
				sum -= y
			}
		}
		if i >= k-1 {
			peak = max(peak, sum/float64(k))
		}
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
	slo           sloAt
	reason        string
	// decodeOK and prefillOK are each role's own check at the rate: its knee
	// with headroom (dOp, pOp) and its own target (TPOT for decode, TTFT
	// for prefill).
	decodeOK, prefillOK bool
	dOp, pOp            float64
	// fAssumed names an f the prefill side was priced with that no run
	// learned at its count, "" with none.
	fAssumed string
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
		e.fAssumed = fmt.Sprintf("prefill f %s assumed at %d pods", num(sf.pf.fleet.Prefill.Imbalance), sf.pf.fleet.Prefill.Replicas)
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
	if rate > e.op {
		e.holds, e.under, e.pastOperating = false, true, true
		if rate >= e.knee {
			parts = append(parts, fmt.Sprintf("%.2f r/s is %.1fx the fleet knee of %.2f r/s", rate, rate/e.knee, e.knee))
		} else {
			parts = append(parts, fmt.Sprintf("%.2f r/s is above %s x the fleet knee %.2f r/s = %.2f r/s", rate, num(r.headroom), e.knee, e.op))
		}
	} else {
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

// judged is one target's check in words, with its numbers.
func (r *reporter) judged(label string, fig whatif.Result, target float64, met, known, onset bool) string {
	switch {
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
	return plainReason(string(u.Reason))
}

// plainReasons are the reasons a report's text names, in words.
var plainReasons = map[string]string{
	string(whatif.Saturated):             "saturated",
	string(whatif.AboveDecodeOnset):      "saturated",
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
	c.DecodeKnee = r.rate(ev.decodeKnee, ol, decodeBinds(ev.decodeKnee))
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
	maxRate, binds := r.maxAtSLO(base, ev)
	r.results = append(r.results, maxRate)
	c.MaxAtSLO = r.rate(maxRate, ol, binds)
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
	margin := ""
	if p.VPMargin > 0 {
		margin = fmt.Sprintf(" x (1 - %.2f margin)", p.VPMargin)
	}
	return fmt.Sprintf("prefill compute: N_p %d x V_P%s / (f %.3f x %s tokens)", p.Replicas, margin, sf.pf.in.Imbalance, num(round(p.PromptTokens)))
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
		f := whatifapi.FigureOf(whatif.Unanswerable{Reason: whatif.NoTraffic, Detail: "no load to judge at"}, "", false)
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
	pk, ok := ev.prefillKnee.(whatif.Answer)
	if !ok {
		f := whatifapi.FigureOf(ev.prefillKnee, "", false)
		u.Prefill.Pooled, u.Prefill.WorstPod = f, f
		return
	}
	// The busiest pod's utilization is the load over the prefill knee, which
	// is N_p x V_P (less the margin) / (f x the prompt); pooled, f is 1.
	f := max(base.pf.in.Imbalance, 1)
	worst := r.load / pk.Value
	u.Prefill.Imbalance = f
	u.Prefill.WorstPod = whatifapi.FigureOf(whatif.Answer{Value: worst}, "", false)
	u.Prefill.Pooled = whatifapi.FigureOf(whatif.Answer{Value: worst / f}, "", false)
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
		row.Cell = r.cell(slo, cellTTFT, row)
		rows = append(rows, row)
	}
	return rows, nil
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
func (r *reporter) cell(slo, ttft *servedFleet, row whatifapi.ReportRow) whatifapi.ReportCell {
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
	switch {
	case s.saturated || (row.ShareOfKnee != nil && *row.ShareOfKnee >= 1):
		c.Mark = whatifapi.CellSaturated
	case s.targetCount == 0:
		c.Mark = whatifapi.CellNoTarget
	case s.met:
		c.Mark = whatifapi.CellMet
	case s.onset && !s.tpotMissed:
		c.Mark = whatifapi.CellOnset
	case s.known:
		c.Mark = whatifapi.CellMissed
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
//   - NOT JUDGED when its own check is not judged, or when the prefill side
//     does not price and decode alone holds;
//   - OVER-PROVISIONED when a role has a replica to spare: one fewer of it
//     holds the whole fleet at the load judged, and the role's own check
//     (knee and target) at the peak too, since removing capacity is the
//     costly mistake;
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
	switch {
	case !base.kneeOK:
		v.Verdict = whatifapi.VerdictNotJudged
		v.Summary = fmt.Sprintf("%s: the decode knee is not priced (%s).", v.Verdict, refusalWords(base.decodeKnee))
		return v, nil
	case !(r.load > 0):
		v.Verdict = whatifapi.VerdictNotJudged
		v.Summary = v.Verdict + ": the window saw no traffic to judge at; state a load (--load-seen)."
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
	case !base.known:
		v.Verdict = whatifapi.VerdictNotJudged
		v.Summary = fmt.Sprintf("%s: %s%s cannot be priced (%s).", v.Verdict, lead, base.slo.unknown,
			refusalWords(base.slo.unknownFig))
		return v, nil
	case !base.prefillPriced:
		v.Verdict = whatifapi.VerdictNotJudged
		v.Reason += ". The prefill side is not priced: decode alone cannot show the fleet right-sized or over-provisioned"
		v.Summary = fmt.Sprintf("%s: the prefill side is not priced (%s); decode alone holds %s%s.", v.Verdict,
			refusalWords(base.prefillKnee), r.targetWords(), strings.TrimSuffix(" "+lead, ", "))
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
		if restsOnDecode {
			// The decode knee reads high on the goldens: a decode replica
			// removed on it is the step most exposed to that.
			v.Reason += ". " + decodeKneeNotValidated
			notes = append([]string{"decode knee unvalidated"}, notes...)
		}
		v.Summary = fmt.Sprintf("%s: %s%s; %s%s.", v.Verdict, lead, roles, need, noteText(notes))
		return v, nil
	}
	v.Verdict = whatifapi.VerdictRight
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

// noteText is notes in parentheses, "" with none.
func noteText(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	return " (" + strings.Join(notes, "; ") + ")"
}

// roles judges each role's own standing at the load: a role has a replica
// to spare when one fewer of it holds the whole fleet at the load judged,
// and its own check (knee and target) at the peak as well. It fills
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
// headroom, the fewest replicas in all (ties to fewer decode), by POST
// /v1/size's search (whatifapi.SmallestFleet) on the report's own check,
// and says it: "needs 3 prefill + 3 decode (has 2 + 2)".
// A role it cuts, below np prefill or nd decode, must also hold its own
// check at the peak, the test roles judges a spare by: removing capacity
// is the costly mistake. Both tests being monotone in each count, it then
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
	decodeHolds := func(n whatifapi.FleetCounts) (bool, error) {
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
		e, err := at(n)
		if err != nil || !e.holds {
			return false, err
		}
		return cutHolds(n)
	}
	if !base.prefillPriced {
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
		if d > nd {
			v.Grow = append(v.Grow, roleDecodeWord)
		}
		return fmt.Sprintf("needs at least %d decode (has %d)", d, nd), nil, nil
	}
	n, unmet, err := whatifapi.SmallestFleet(whatifapi.FleetCounts{Prefill: bound, Decode: bound}, decodeHolds, holds)
	switch {
	case err != nil:
		return "", nil, err
	case unmet != "":
		out.Unmet = unmet
		return fmt.Sprintf("no fleet up to %d replicas of each role holds", bound), nil, nil
	}
	out.Prefill, out.Decode = n.Prefill, n.Decode
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
	words := []string{fmt.Sprintf("needs %d prefill + %d decode (has %d + %d)", n.Prefill, n.Decode, np, nd)}
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
	return strings.Join(words, ", "), notes, nil
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
	switch {
	case e.pastOperating && r.load >= e.knee:
		if r.brief {
			return fmt.Sprintf("%.1fx the fleet knee (%s)", r.load/e.knee, rateWords(e.knee)), e.binds, knee
		}
		return fmt.Sprintf("%.1fx the fleet knee of %s, the fleet saturates", r.load/e.knee, rateWords(e.knee)), e.binds, knee
	case e.pastOperating:
		return fmt.Sprintf("the fleet runs past its operating point of %s (%s of its %s knee)",
			rateWords(e.op), num(r.headroom), rateWords(e.knee)), e.binds, knee
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
// prefill queue or decode admission, whichever mean wait is the larger; the
// knee's role when the TTFT is refused for saturation.
func ttftRole(t whatif.Result, e fleetEval) (role, why string) {
	v, ok := t.(whatif.TTFT)
	if !ok {
		return e.binds, e.binds + " sets the knee, and TTFT saturates there"
	}
	p := v.Parts
	prefillWait, decodeWait := p.EngineWait+p.RouterWait+p.BurstWait, p.DecodeWait+p.KVPullWait
	if decodeWait > prefillWait {
		return roleDecodeWord, fmt.Sprintf("decode admission, a %s mean wait, is the larger wait in TTFT", secondsWords(decodeWait))
	}
	return rolePrefill, fmt.Sprintf("the prefill queue, a %s mean wait, is the larger wait in TTFT", secondsWords(prefillWait))
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
		return "the recording learned no prefill speed; --assume-vp states one"
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
	case whatif.NoTPOTHistogram, whatif.TPOTBucketTooWide, whatif.TPOTQuantileUnbounded:
		return "the TPOT histogram cannot give that percentile; judge TPOT at the mean"
	case whatif.NoTraffic:
		return "the recording saw no traffic of this shape"
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

// sourceLoadCycles is a shape asked as the load's: read at the cycles the
// load was read from.
const sourceLoadCycles = "the load's cycles"
