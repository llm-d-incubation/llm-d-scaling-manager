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
	// ratios is each variant's latest busiest/mean rank fill ratios
	// (domain.ReplicaMetrics.BusiestRankFillRatio), at most
	// busiestRatioRows of them, from rows whose busiest rank fills at least
	// the band's bottom; kept only for a variant whose line is read at its
	// busiest rank (saturation.SwitchMoEBusiestRank).
	ratios map[string][]float64
}

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
		ratios: map[string][]float64{}}
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
		if !ramp && k > 0 && k < bottom && rm.ITLState() == domain.ITLFinite && rm.AvgITL > 0 && !math.IsInf(rm.AvgITL, 0) {
			b := l.below[rm.VariantName]
			b = append(b, itl.Observation{K: k, ITLSec: rm.AvgITL, Timestamp: at})
			if len(b) > itl.BelowBandWindow {
				b = b[len(b)-itl.BelowBandWindow:]
			}
			l.below[rm.VariantName] = b
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
		kept[rm.VariantName] = keepCycles(append(kept[rm.VariantName], whatif.LineRow{Cycle: cycle, At: at, K: k, KVPerRequest: rowResidentKV(rm, s),
			TokenRate: rm.GenerationTokenRate, Ramp: ramp, AboveBand: k > top}), shapeCycles)
	}
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
