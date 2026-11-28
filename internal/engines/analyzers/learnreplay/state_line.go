package learnreplay

import (
	"math"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// What a recording says of the line an answer rests on: the rows the line
// check compares it with (whatif.CheckLine). Read, not fed back, as the TTFT
// half is (state_ttft.go), so it needs no replay switch.

// fillAt is one reading of a replica's fill and the KV a request held.
type fillAt struct {
	k, kvReq float64
	at       time.Time
}

// lineSamples keeps each variant's rows off a ramp, on one and above the band,
// each of its latest cycles that have one, and each pod's fill over the steady
// gate's time. They are kept apart so that a recording that ends on a ramp, or
// above the band, is checked on the rows it last held still at in it, as a
// shape is taken from the traffic last served (shapeOf).
type lineSamples struct {
	steady, ramps, above map[string][]whatif.LineRow
	fills                map[string][]fillAt
	// below is each variant's latest mean ITLs read below the band, off a
	// ramp, at most itl.BelowBandWindow of them: the readings the line's fit
	// drops as idle, and the floor below the band is read from
	// (whatif.BelowBand.LeastITL, itl.BelowBandFloor), each with its fill.
	below map[string][]itl.Observation
	// belowSequences is the sequences each of below's readings ran on its
	// replica (rowSequences), index for index, 0 where that was not read: the
	// count the floor was read at, as its fill is the fill (issue
	// llm-d-incubation/llm-d-scaling-manager#286).
	belowSequences map[string][]float64
	// maxFill is each variant's highest fill a steady row read, off a ramp
	// (VariantState.MaxFill).
	maxFill map[string]float64
	// ratios is each variant's latest busiest/mean rank fill ratios
	// (domain.ReplicaMetrics.BusiestRankFillRatio), at most
	// busiestRatioRows of them, from rows whose busiest rank fills at least
	// the band's bottom; kept only for a variant whose line is read at its
	// busiest rank (saturation.SwitchMoEBusiestRank).
	ratios map[string][]float64
	// fit is each variant's steady rows, off a ramp, in the band and above
	// it (an above-band line's are there), over a line's whole window
	// (fitRowsAge) and not its latest shapeCycles cycles alone: the rows a
	// line's fit rests on, and the batch it held (fitRows). ownFitAt is when
	// each variant's line was last its own fit (noteOwnFit): a held line's
	// fit was made then.
	fit      map[string][]whatif.LineRow
	ownFitAt map[string]time.Time
}

// fitRowsAge is how far back a variant's fit rows reach: a line's window
// (itl.DefaultObservationMaxAge), and the steady readings it carries past it
// (itl.DefaultSteadyCarry).
const fitRowsAge = itl.DefaultObservationMaxAge + itl.DefaultSteadyCarry

// busiestRatioRows is how many rows a variant's busiest/mean rank fill ratio
// is the median of: one row a replica a cycle, so an hour of one replica at
// the default 15 s cycle, 7.5 minutes of eight.
const busiestRatioRows = 240

// busiestRatio is the median of the variant's ratios, 0 with none.
func (l *lineSamples) busiestRatio(variant string) float64 {
	rs := l.ratios[variant]
	if len(rs) == 0 {
		return 0
	}
	s := slices.Sorted(slices.Values(rs))
	if n := len(s); n%2 == 1 {
		return s[n/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// fillForITL is the k a row's ITL pairs with as the line was read: at the
// busiest rank where busiest says the variant's line is
// (saturation.SwitchMoEBusiestRank), the mean over the ranks otherwise. A nil
// busiest reads the mean.
func fillForITL(rm domain.ReplicaMetrics, busiest func(variant string) bool) float64 {
	if busiest != nil && busiest(rm.VariantName) {
		return rm.BusiestRankKvUsageForITL()
	}
	return rm.KvUsageForITL()
}

func newLineSamples() *lineSamples {
	return &lineSamples{steady: map[string][]whatif.LineRow{}, ramps: map[string][]whatif.LineRow{},
		above: map[string][]whatif.LineRow{}, fills: map[string][]fillAt{}, below: map[string][]itl.Observation{},
		ratios: map[string][]float64{}, fit: map[string][]whatif.LineRow{}, ownFitAt: map[string]time.Time{},
		belowSequences: map[string][]float64{}, maxFill: map[string]float64{}}
}

// noteOwnFit notes that variant's line at is its own fit, not held or
// borrowed (whatif.TierOLS, whatif.TierPinnedB): a line held later was
// fitted from the rows up to it.
func (l *lineSamples) noteOwnFit(variant string, at time.Time) {
	l.ownFitAt[variant] = at
}

// fitRows is the rows the line of vs was fitted on, for the batch its fit
// held (VariantState.FitRows): a refitted line's, the rows of the cycles its
// readings span (VariantState.ReadingsFrom to ReadingsTo), in the band, and
// above it too for a line fitted there (whatif.TierAboveBand), whose
// readings are all above it; a held line's, the rows in the band of the
// window that ends at the cycle its line was last its own fit.
// Nil for any other line, or with no such row: the planner then reads the
// latest rows (VariantState.LineRows).
func (l *lineSamples) fitRows(vs VariantState) []whatif.LineRow {
	rows := l.fit[vs.Variant]
	var out []whatif.LineRow
	switch {
	case vs.Refitted && vs.ReadingsFrom > 0:
		above := vs.Tier == whatif.TierAboveBand
		for _, r := range rows {
			if r.Cycle >= vs.ReadingsFrom && r.Cycle <= vs.ReadingsTo && (above || !r.AboveBand) {
				out = append(out, r)
			}
		}
	case vs.Tier == whatif.TierHeld:
		at, ok := l.ownFitAt[vs.Variant]
		if !ok {
			return nil
		}
		for _, r := range rows {
			if !r.AboveBand && !r.At.After(at) && !r.At.Before(at.Add(-fitRowsAge)) {
				out = append(out, r)
			}
		}
	}
	return out
}

// leastBelowBand is the variant's floor below the band, in seconds
// (itl.BelowBandFloor), and the fill it was read at (itl.BelowBandPoint);
// both 0 with too few readings. The tracker's floor under
// itl.Admission.BaselineFromFloor is read by the same quantile, but over its
// last DefaultObservationMaxAge only; this one keeps the latest
// itl.BelowBandWindow readings of any age.
func (l *lineSamples) leastBelowBand(variant string) (itlSec, fill float64) {
	p, ok := itl.BelowBandPoint(l.below[variant])
	if !ok {
		return 0, 0
	}
	return p.ITLSec, p.K
}

// belowBand is a variant's floor below the band as a state reads it: the
// floor and its fill (leastBelowBand), the fewest sequences its readings ran
// and the floor kept by them (belowBandLeastSequences).
type belowBand struct {
	itlSec, fill, least float64
	byCount             itl.Model
}

// belowBandLeastSequences is the fewest sequences a replica ran in the
// readings the variant's floor below the band is read from (leastBelowBand),
// and the floor kept by those sequences (whatif.BelowBand.ByCount), as
// itl.BelowBandByCount reads them; a reading's count is rowSequences.
func (l *lineSamples) belowBandLeastSequences(variant string) (least float64, byCount itl.Model) {
	return itl.BelowBandByCount(l.below[variant], l.belowSequences[variant])
}

// rows is the variant's rows, of all three.
func (l *lineSamples) rows(variant string) []whatif.LineRow {
	return slices.Concat(l.steady[variant], l.ramps[variant], l.above[variant])
}

// note reads one cycle: every row of a variant that is not prefill, with a
// token rate, at a fill the check compares. A row above the band is kept to
// be counted, and is not compared: no answer reads the line there
// (whatif.ReachableFill, and the band's clamp). busiest names the variants
// whose line is read at the busiest rank (fillForITL): their rows are read
// there too, and their busiest/mean ratios kept.
func (l *lineSamples) note(in domain.AnalyzerInput, cycle int, at time.Time, busiest func(variant string) bool) {
	policy, _ := in.Config.(*config.ScalingPolicy)
	steady := policy.ITLSteadiness()
	bottom, top := policy.ITLBand()
	roles := rolesOf(in)
	for _, rm := range in.ReplicaMetrics {
		if !prefill.Serving(rm) || domain.CanonicalRole(roles[rm.VariantName]) == domain.RolePrefill {
			continue
		}
		k := fillForITL(rm, busiest)
		// The ratio's filter reads the busiest rank's peak, not fillForITL's
		// k, on purpose: it asks whether the pod was loaded as the band's
		// bottom counts load, at the rank that sets its step.
		if r, ok := rm.BusiestRankFillRatio(); ok && busiest != nil && busiest(rm.VariantName) &&
			rm.BusiestRankKvUsage() >= bottom {
			l.ratios[rm.VariantName] = append(l.ratios[rm.VariantName], r)
			if n := len(l.ratios[rm.VariantName]); n > busiestRatioRows {
				l.ratios[rm.VariantName] = l.ratios[rm.VariantName][n-busiestRatioRows:]
			}
		}
		s := shape.New(rm.AvgInputTokens, rm.AvgOutputTokens, rm.PrefixCacheHitRate)
		if rm.AvgOutputTokensSquaredKnown {
			s = s.WithOutputDispersion(shape.Dispersion(rm.AvgOutputTokens, rm.AvgOutputTokensSquared))
		}
		ramp := l.onRamp(rm.PodName, fillAt{k: k, kvReq: s.KVreq, at: at}, steady)
		if !ramp && k > l.maxFill[rm.VariantName] {
			l.maxFill[rm.VariantName] = k
		}
		if !ramp && k > 0 && k < bottom && rm.ITLState() == domain.ITLFinite && rm.AvgITL > 0 && !math.IsInf(rm.AvgITL, 0) {
			b := l.below[rm.VariantName]
			b = append(b, itl.Observation{K: k, ITLSec: rm.AvgITL, Timestamp: at})
			n := l.belowSequences[rm.VariantName]
			n = append(n, rowSequences(rm))
			if len(b) > itl.BelowBandWindow {
				b, n = b[len(b)-itl.BelowBandWindow:], n[len(n)-itl.BelowBandWindow:]
			}
			l.below[rm.VariantName], l.belowSequences[rm.VariantName] = b, n
		}
		if !rm.GenerationTokenRateKnown || !(rm.GenerationTokenRate > 0) || !(k >= itl.DefaultGPSMinKForVerification) {
			continue
		}
		kept := l.steady
		switch {
		case k > top:
			kept = l.above
		case ramp:
			kept = l.ramps
		}
		row := whatif.LineRow{Cycle: cycle, At: at, K: k, KVPerRequest: rowResidentKV(rm, s),
			TokenRate: rm.GenerationTokenRate, Sequences: rowSequences(rm), Ramp: ramp, AboveBand: k > top}
		kept[rm.VariantName] = keepCycles(append(kept[rm.VariantName], row), shapeCycles)
		if !ramp {
			l.fit[rm.VariantName] = append(l.fit[rm.VariantName], row)
		}
	}
	l.pruneFit(at)
}

// pruneFit drops the fit rows older than fitRowsAge before now, or before
// the cycle a variant's line was last its own fit where that is earlier: a
// line held since keeps the rows its fit was made on.
func (l *lineSamples) pruneFit(now time.Time) {
	for v, rows := range l.fit {
		from := now
		if at, ok := l.ownFitAt[v]; ok && at.Before(from) {
			from = at
		}
		cutoff := from.Add(-fitRowsAge)
		i := 0
		for i < len(rows) && rows[i].At.Before(cutoff) {
			i++
		}
		if i > 0 {
			l.fit[v] = slices.Clone(rows[i:])
		}
	}
}

// rowSequences is the sequences a replica ran over the row's window
// (whatif.LineRow.Sequences): its generated tokens per second times its mean
// ITL (Little), a window mean as the row's fill is, over the tokens a step
// emits where speculation accepted drafts (vLLM's ITL is the step's time);
// else the running-requests gauge; 0 with neither. The fill times the
// capacity over the KV a request is not used: the KV a request is read off
// one scrape, the fill over a minute, and on the Qwen3-32B warm-up their
// ratio reads 271 sequences a pod where Little reads 210.
func rowSequences(rm domain.ReplicaMetrics) float64 {
	if rm.ITLState() == domain.ITLFinite && rm.AvgITL > 0 && !math.IsInf(rm.AvgITL, 0) && rm.GenerationTokenRate > 0 {
		n := rm.GenerationTokenRate * rm.AvgITL
		if rm.SpecAcceptedPerStepKnown && rm.SpecAcceptedPerStep > 0 {
			n /= 1 + rm.SpecAcceptedPerStep
		}
		return n
	}
	if rm.RunningRequestsKnown && rm.RunningRequests > 0 {
		return float64(rm.RunningRequests)
	}
	return 0
}

// rowResidentKV is the KV a request held on the row, as the line check prices
// the row at (whatif.LineRow.KVPerRequest): the row's tokens in use over its
// running requests, read from the same scrape (state_resident.go), else the
// row's shape s with no prefix-cache hit. A hit saves no decode-resident KV
// unless another running request shares the block, and a check against the
// discount prices more sequences than the row held. A reading is held above
// the discount, as the planner holds its price (pricedVariant.residentKV).
func rowResidentKV(rm domain.ReplicaMetrics, s shape.Shape) float64 {
	if rm.RunningRequestsKnown && rm.KvUsageInstantKnown && rm.RunningRequests > 0 && rm.KvUsageInstant > 0 &&
		rm.TotalKvCapacityTokens > 0 && rm.TotalKvCapacityTokens != math.MaxInt64 {
		return max(rm.KvUsageInstant*float64(rm.TotalKvCapacityTokens)/float64(rm.RunningRequests), s.KVreq)
	}
	return shape.New(s.AvgInputTokens, s.AvgOutputTokens, 0).WithOutputDispersion(s.OutputDispersion).KVreq
}

// onRamp notes the pod's reading now and reports whether the row is on a ramp:
// read for less than the gate's time, its fill moved past the gate's drift
// within it (itl's replicaTrace.still, the rule the line's readings pass), or
// the KV a request holds moved past shape.DefaultChangeTolerance. The last is
// the row's lengths, means over minutes, lagging a change of shape while the
// fill is already at the new one: without it ols-sweep reads 39.8% high, its
// worst row 237%, and with it 3.0%.
func (l *lineSamples) onRamp(pod string, now fillAt, s itl.Steadiness) bool {
	cutoff := now.at.Add(-s.For)
	trace := l.fills[pod]
	ramp := len(trace) == 0 || trace[0].at.After(cutoff)
	for _, f := range trace {
		if f.at.Before(cutoff) {
			continue
		}
		moved := math.Abs(f.kvReq-now.kvReq) > shape.DefaultChangeTolerance*now.kvReq && f.kvReq > 0 && now.kvReq > 0
		if math.Abs(f.k-now.k) > s.MaxKDrift || moved {
			ramp = true
		}
	}
	trace = append(trace, now)
	// One reading at or before the cutoff stays, so a span of exactly the
	// gate's time reads as read for that long at the next cycle too.
	for len(trace) > 1 && !trace[1].at.After(cutoff) {
		trace = trace[1:]
	}
	l.fills[pod] = trace
	return ramp
}

// keepCycles is rows, oldest first, cut to its latest n distinct cycles.
func keepCycles(rows []whatif.LineRow, n int) []whatif.LineRow {
	seen := 0
	for i := len(rows) - 1; i >= 0; i-- {
		if i == len(rows)-1 || rows[i].Cycle != rows[i+1].Cycle {
			seen++
		}
		if seen > n {
			return slices.Clone(rows[i+1:])
		}
	}
	return rows
}
