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
}

func newLineSamples() *lineSamples {
	return &lineSamples{steady: map[string][]whatif.LineRow{}, ramps: map[string][]whatif.LineRow{},
		above: map[string][]whatif.LineRow{}, fills: map[string][]fillAt{}}
}

// rows is the variant's rows, of all three.
func (l *lineSamples) rows(variant string) []whatif.LineRow {
	return slices.Concat(l.steady[variant], l.ramps[variant], l.above[variant])
}

// note reads one cycle: every row of a variant that is not prefill, with a
// token rate, at a fill the check compares. A row above the band is kept to
// be counted, and is not compared: no answer reads the line there
// (whatif.ReachableFill, and the band's clamp).
func (l *lineSamples) note(in domain.AnalyzerInput, cycle int, at time.Time) {
	policy, _ := in.Config.(*config.ScalingPolicy)
	steady := policy.ITLSteadiness()
	_, top := policy.ITLBand()
	roles := rolesOf(in)
	for _, rm := range in.ReplicaMetrics {
		if !prefill.Serving(rm) || domain.CanonicalRole(roles[rm.VariantName]) == domain.RolePrefill {
			continue
		}
		k := rm.KvUsageForITL()
		s := shape.New(rm.AvgInputTokens, rm.AvgOutputTokens, rm.PrefixCacheHitRate)
		if rm.AvgOutputTokensSquaredKnown {
			s = s.WithOutputDispersion(shape.Dispersion(rm.AvgOutputTokens, rm.AvgOutputTokensSquared))
		}
		ramp := l.onRamp(rm.PodName, fillAt{k: k, kvReq: s.KVreq, at: at}, steady)
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
		kept[rm.VariantName] = keepCycles(append(kept[rm.VariantName], whatif.LineRow{Cycle: cycle, At: at, K: k, KVPerRequest: s.KVreq,
			TokenRate: rm.GenerationTokenRate, Ramp: ramp, AboveBand: k > top}), shapeCycles)
	}
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
