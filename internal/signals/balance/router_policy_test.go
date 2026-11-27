package balance

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// policyCycles is n cycles, the load rising and falling between lo and hi
// requests a second four times over them (loadAt), over 4 prefill
// candidates, and the kept share at each load from kept. endpoints, when not nil, gives each cycle's
// decode endpoints at its load.
func policyCycles(n int, lo, hi float64, kept func(load float64) float64,
	endpoints func(load float64) []domain.RouterEndpoint) []domain.AnalyzerInput {
	out := make([]domain.AnalyzerInput, n)
	for i := range out {
		load := loadAt(i, n, lo, hi)
		r := &domain.RouterLatency{WindowSeconds: 300}
		if kept != nil {
			r.PrefixBestAvailableTokenRate = 1000 * load
			r.PrefixPredictedTokenRate = kept(load) * r.PrefixBestAvailableTokenRate
		}
		if endpoints != nil {
			r.Endpoints = endpoints(load)
		}
		out[i] = pdInput(4, 4, load, r)
	}
	return out
}

// loadAt is cycle i of n's load: a triangle wave between lo and hi, four
// periods over the n cycles, so each half of the window by time spans the
// whole range as a day of traffic does.
func loadAt(i, n int, lo, hi float64) float64 {
	phase := math.Mod(4*float64(i)/float64(n), 1)
	return lo + (hi-lo)*(1-math.Abs(2*phase-1))
}

// pdInput is a cycle of a P/D model with pre prefill and dec decode pods
// serving, at load requests a second, with the router's reading r.
func pdInput(pre, dec int, load float64, r *domain.RouterLatency) domain.AnalyzerInput {
	in := domain.AnalyzerInput{ArrivalRate: load, RouterLatency: r, VariantStates: []domain.VariantReplicaState{
		{VariantName: "p", Role: domain.RolePrefill}, {VariantName: "d", Role: domain.RoleDecode}}}
	for range pre {
		in.ReplicaMetrics = append(in.ReplicaMetrics, domain.ReplicaMetrics{VariantName: "p", Ready: true})
	}
	for range dec {
		in.ReplicaMetrics = append(in.ReplicaMetrics, domain.ReplicaMetrics{VariantName: "d", Ready: true})
	}
	return in
}

// detect is the policy the inputs read, one cycle a step apart from t0.
func detect(ins []domain.AnalyzerInput, step time.Duration) RouterPolicy {
	var w RouterPolicyWindow
	for i, in := range ins {
		w.Note(RouterCycleOf(in, t0.Add(time.Duration(i)*step)))
	}
	return w.Policy()
}

// balancing is four decode endpoints a load-aware router places on: one
// slow endpoint gets a quarter of the placements of the others, and the in-flight
// requests come out even.
func balancing(load float64) []domain.RouterEndpoint {
	share := []float64{1, 1, 1, 0.25}
	return endpointsOf(load, share, []float64{4, 4, 4, 4})
}

// evenPlacements is four decode endpoints a load-blind router places on
// evenly, the slow one holding twice the in-flight requests of the others.
func evenPlacements(load float64) []domain.RouterEndpoint {
	return endpointsOf(load, []float64{1, 1, 1, 1}, []float64{4, 4, 4, 8})
}

func endpointsOf(load float64, share, inFlight []float64) []domain.RouterEndpoint {
	var total float64
	for _, s := range share {
		total += s
	}
	out := make([]domain.RouterEndpoint, len(share))
	for i := range share {
		out[i] = domain.RouterEndpoint{Pod: "d-" + string(rune('a'+i)), PlacementRate: load * share[i] / total, PlacementKnown: true,
			InFlightRequests: inFlight[i], InFlightRequestsKnown: true}
	}
	return out
}

// policyStep is the cycles' spacing in the detection tests.
const policyStep = time.Minute

// A router that scores the prefix and the load (the default
// epp-optimized-baseline: prefix-cache 3, queue 2, kv-cache-utilization 2)
// keeps nearly all the hit at light load and gives some of it up as the
// load rises: cache-aware, and load-aware by the hit.
func TestDetectRouterPolicy_CacheAndLoadAwareByHit(t *testing.T) {
	p := detect(policyCycles(60, 1, 8, func(load float64) float64 { return 0.99 - 0.015*load }, nil), policyStep)
	assert.Equal(t, PolicyCacheAndLoadAware, p.Policy, "%+v", p)
	assert.Equal(t, RouterYes, p.CacheAware)
	assert.Equal(t, RouterYes, p.LoadAwareByHit)
	assert.Equal(t, RouterNotDetected, p.LoadAwareBySpread, "no endpoint series")
	assert.Equal(t, routerPolicyReasonSpreadNoEndpoint, p.SpreadReason)
	assert.Equal(t, 4, p.Candidates)
	assert.Greater(t, p.KeptLight, p.KeptHeavy)
	assert.Greater(t, p.LoadHeavy, LoadRangeMin*p.LoadLight)
	aware, detected := p.LoadAwareRouter()
	assert.True(t, aware)
	assert.True(t, detected)
}

// A router that scores only the prefix keeps the same share at every load:
// cache-aware, and with even placements beside uneven in-flight requests,
// load-blind.
func TestDetectRouterPolicy_CacheAwareLoadBlind(t *testing.T) {
	p := detect(policyCycles(60, 1, 8, func(float64) float64 { return 0.98 }, evenPlacements), policyStep)
	assert.Equal(t, PolicyCacheAware, p.Policy, "%+v", p)
	assert.Equal(t, RouterYes, p.CacheAware)
	assert.Equal(t, RouterNotDetected, p.LoadAwareByHit)
	assert.Equal(t, routerPolicyReasonFlat, p.HitReason)
	assert.Equal(t, RouterNo, p.LoadAwareBySpread)
	assert.Equal(t, RouterNo, p.LoadAware)
	assert.Equal(t, 60, p.SpreadEven)
	aware, detected := p.LoadAwareRouter()
	assert.False(t, aware)
	assert.True(t, detected)
}

// A router that scores only the load keeps little of the hit even at light
// load, and evens the in-flight requests out by placing unevenly.
func TestDetectRouterPolicy_LoadAwareOnly(t *testing.T) {
	p := detect(policyCycles(60, 1, 8, func(float64) float64 { return 0.3 }, balancing), policyStep)
	assert.Equal(t, PolicyLoadAware, p.Policy, "%+v", p)
	assert.Equal(t, RouterNo, p.CacheAware)
	assert.Equal(t, RouterYes, p.LoadAwareBySpread)
	assert.Equal(t, 60, p.SpreadBalancing)
	assert.InDelta(t, 0.3, p.Kept, 1e-9)
}

// A router that scores neither: little hit kept, placements even.
func TestDetectRouterPolicy_Neither(t *testing.T) {
	p := detect(policyCycles(60, 1, 8, func(float64) float64 { return 0.3 }, evenPlacements), policyStep)
	assert.Equal(t, PolicyNeither, p.Policy, "%+v", p)
}

// Where the series are missing, or do not decide, each side is "not
// detected" with its reason, and LoadAwareRouter says so: the planner keeps
// --load-aware-router.
func TestDetectRouterPolicy_NotDetected(t *testing.T) {
	t.Run("no series", func(t *testing.T) {
		p := detect(policyCycles(60, 1, 8, nil, nil), policyStep)
		assert.Equal(t, PolicyNotDetected, p.Policy)
		assert.Equal(t, 0, p.Cycles)
		assert.Equal(t, routerPolicyReasonNoSeries, p.CacheReason)
		assert.Equal(t, routerPolicyReasonNoSeries, p.SpreadReason)
		_, detected := p.LoadAwareRouter()
		assert.False(t, detected)
	})
	t.Run("nil input", func(t *testing.T) {
		var w *RouterPolicyWindow
		assert.Equal(t, PolicyNotDetected, w.Policy().Policy)
		assert.Equal(t, PolicyNotDetected, DetectRouterPolicy(nil).Policy)
	})
	t.Run("one candidate", func(t *testing.T) {
		ins := policyCycles(60, 1, 8, func(float64) float64 { return 1 }, nil)
		for i := range ins {
			ins[i] = pdInput(1, 1, ins[i].ArrivalRate, ins[i].RouterLatency)
		}
		p := detect(ins, policyStep)
		assert.Equal(t, PolicyNotDetected, p.Policy)
		assert.Equal(t, routerPolicyReasonOneCandidate, p.CacheReason)
	})
	t.Run("short window", func(t *testing.T) {
		p := detect(policyCycles(10, 1, 8, func(float64) float64 { return 0.98 }, nil), 15*time.Second)
		assert.Equal(t, PolicyNotDetected, p.Policy, "%+v", p)
		assert.Equal(t, routerPolicyReasonShortWindow, p.CacheReason)
	})
	t.Run("between the bounds", func(t *testing.T) {
		p := detect(policyCycles(60, 1, 8, func(float64) float64 { return 0.75 }, nil), policyStep)
		assert.Equal(t, PolicyNotDetected, p.Policy)
		assert.Equal(t, routerPolicyReasonBetween, p.CacheReason)
	})
	t.Run("no load range", func(t *testing.T) {
		p := detect(policyCycles(60, 4, 5, func(load float64) float64 { return 1.2 - 0.06*load }, nil), policyStep)
		assert.Equal(t, PolicyCacheAwareLoadNotDetected, p.Policy, "%+v", p)
		assert.Equal(t, routerPolicyReasonNoLoadRange, p.HitReason)
	})
	t.Run("even spreads", func(t *testing.T) {
		even := func(load float64) []domain.RouterEndpoint {
			return endpointsOf(load, []float64{1, 1, 1, 1}, []float64{4, 4, 4, 4})
		}
		p := detect(policyCycles(60, 1, 8, nil, even), policyStep)
		assert.Equal(t, PolicyNotDetected, p.Policy)
		assert.Equal(t, routerPolicyReasonNoSpread, p.SpreadReason)
		assert.Equal(t, 60, p.Cycles)
	})
}

// A fall that one outlier window makes is not read as one: the share is
// flat but for the heaviest-load window's, and the fall it gives the heaviest
// third is inside two standard errors of the third's scatter.
func TestDetectRouterPolicy_OneOutlierIsNoFall(t *testing.T) {
	ins := policyCycles(60, 1, 8, func(float64) float64 { return 0.95 }, nil)
	heaviest := 0
	for i, in := range ins {
		if in.ArrivalRate > ins[heaviest].ArrivalRate {
			heaviest = i
		}
	}
	r := ins[heaviest].RouterLatency
	r.PrefixPredictedTokenRate = 0.2 * r.PrefixBestAvailableTokenRate
	p := detect(ins, 5*time.Minute)
	assert.Equal(t, RouterYes, p.CacheAware, "%+v", p)
	assert.Greater(t, p.KeptLight-p.KeptHeavy, KeptShareFallMin, "precondition: the fall clears the floor")
	assert.Equal(t, RouterNotDetected, p.LoadAwareByHit, "%+v", p)
	assert.Equal(t, routerPolicyReasonFlat, p.HitReason)
}

// The cycle reader takes the producer that predicted the most, counts the
// candidates of its role, and leaves a pair that is no reading unread.
func TestRouterCycleOf(t *testing.T) {
	r := &domain.RouterLatency{WindowSeconds: 300, PrefixPredictedTokenRate: 1, PrefixBestAvailableTokenRate: 2,
		PrefixProducers: []domain.RouterPrefixProducer{
			{Plugin: "decode-prefix-producer", Role: domain.RoleDecode, PredictedTokenRate: 10, BestAvailableTokenRate: 20},
			{Plugin: "approx-prefix-cache-producer", Role: domain.RolePrefill, PredictedTokenRate: 90, BestAvailableTokenRate: 100},
		}}
	c := RouterCycleOf(pdInput(3, 5, 7, r), t0)
	assert.True(t, c.KeptKnown)
	assert.InDelta(t, 0.9, c.Kept(), 1e-12)
	assert.Equal(t, 3, c.Candidates, "the prefill producer routes the prefill pods")
	assert.InDelta(t, 7, c.Load, 1e-12)

	r.PrefixProducers = nil
	c = RouterCycleOf(pdInput(3, 5, 7, r), t0)
	assert.InDelta(t, 0.5, c.Kept(), 1e-12, "the model-level pair with no producer")
	assert.Equal(t, 3, c.Candidates)

	for name, pair := range map[string][2]float64{
		"NaN predicted": {math.NaN(), 2}, "best under predicted": {3, 2}, "none available": {0, 0}, "Inf best": {1, math.Inf(1)},
	} {
		r := &domain.RouterLatency{WindowSeconds: 300, PrefixPredictedTokenRate: pair[0], PrefixBestAvailableTokenRate: pair[1]}
		assert.False(t, RouterCycleOf(pdInput(3, 5, 7, r), t0).KeptKnown, name)
	}

	// The load falls back to the placements; the spread reads only the
	// endpoints with both figures, a warm-pool bridge is no candidate.
	in := pdInput(2, 0, 0, &domain.RouterLatency{WindowSeconds: 300, Endpoints: []domain.RouterEndpoint{
		{Pod: "a", PlacementRate: 1, PlacementKnown: true, InFlightRequests: 2, InFlightRequestsKnown: true},
		{Pod: "b", PlacementRate: 3, PlacementKnown: true, InFlightRequests: 2, InFlightRequestsKnown: true},
		{Pod: "prefill-0", InFlightRequests: 9, InFlightRequestsKnown: true},
	}})
	in.ReplicaMetrics = append(in.ReplicaMetrics, domain.ReplicaMetrics{VariantName: "p", Ready: true, FromWarmPool: true})
	c = RouterCycleOf(in, t0)
	assert.InDelta(t, 4, c.Load, 1e-12)
	assert.True(t, c.SpreadKnown)
	assert.Equal(t, 2, c.SpreadEndpoints)
	assert.InDelta(t, 0.5, c.PlacementCV, 1e-12)
	assert.InDelta(t, 600, c.PlacementsPerEndpoint, 1e-9, "2 a second over the 300 s window")
	assert.InDelta(t, 0, c.InFlightCV, 1e-12)
	assert.Equal(t, 2, candidates(in, ""))

	assert.Equal(t, RouterCycle{At: t0}, RouterCycleOf(domain.AnalyzerInput{}, t0))
}

// The window keeps RouterPolicyWindowCycles cycles that read a series and
// skips those that read none.
func TestRouterPolicyWindow_Bounds(t *testing.T) {
	var w RouterPolicyWindow
	w.Note(RouterCycle{At: t0})
	assert.Equal(t, 0, w.Len())
	for i := range RouterPolicyWindowCycles + 10 {
		w.Note(RouterCycle{At: t0.Add(time.Duration(i) * time.Second), KeptKnown: true, PredictedTokenRate: 1, BestAvailableTokenRate: 1})
	}
	assert.Equal(t, RouterPolicyWindowCycles, w.Len())
	assert.Equal(t, t0.Add(10*time.Second), w.cycles[0].At)
}

// The tracker reads the policy per model, says on which cycle its name
// changed, and forgets it with the model.
func TestTracker_ObserveRouter(t *testing.T) {
	tr := NewTracker(nil)
	ins := policyCycles(60, 1, 8, func(load float64) float64 { return 0.99 - 0.015*load }, nil)
	var changes []string
	for i, in := range ins {
		p, changed := tr.ObserveRouter("ns/m", in, t0.Add(time.Duration(i)*policyStep))
		if changed {
			changes = append(changes, p.Policy)
		}
	}
	require.NotEmpty(t, changes)
	assert.Equal(t, PolicyCacheAndLoadAware, changes[len(changes)-1], "%v", changes)
	assert.NotContains(t, changes, PolicyNotDetected, "not detected at the start is no change")
	assert.LessOrEqual(t, len(changes), 2, "each name reported once, after the dwell: %v", changes)

	_, changed := tr.ObserveRouter("ns/other", pdInput(4, 4, 1, nil), t0)
	assert.False(t, changed, "a model with no series logs nothing")

	tr.Forget("ns/m")
	p, _ := tr.ObserveRouter("ns/m", pdInput(4, 4, 1, nil), t0)
	assert.Equal(t, PolicyNotDetected, p.Policy, "forgotten with the model")
}
