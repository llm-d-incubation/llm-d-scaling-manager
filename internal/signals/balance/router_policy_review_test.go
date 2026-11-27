package balance

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The zero RouterPolicy is no reading: LoadAwareRouter does not call it
// decided, so a planner holding one keeps --load-aware-router silently.
func TestRouterPolicy_ZeroValueIsNotDetected(t *testing.T) {
	aware, detected := RouterPolicy{}.LoadAwareRouter()
	assert.False(t, aware)
	assert.False(t, detected)
	for v, want := range map[RouterVerdict]bool{RouterYes: true, RouterNo: true, RouterNotDetected: false, "": false} {
		_, detected := RouterPolicy{LoadAware: v}.LoadAwareRouter()
		assert.Equal(t, want, detected, "%q", v)
	}
}

// A load-blind router that places at random spreads its placements by
// sampling noise alone, about 1/sqrt(n) for n placements per endpoint:
// beside an even in-flight load that is no balancing, and does not read as
// load-aware.
func TestDetectRouterPolicy_RandomPlacementIsNotLoadAware(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	random := func(load float64) []domain.RouterEndpoint {
		counts := make([]float64, 4)
		for range int(load * 300) {
			counts[rng.IntN(4)]++
		}
		out := make([]domain.RouterEndpoint, 4)
		for i, c := range counts {
			out[i] = domain.RouterEndpoint{Pod: "d", PlacementRate: c / 300, PlacementKnown: true,
				InFlightRequests: 4, InFlightRequestsKnown: true}
		}
		return out
	}
	ins := policyCycles(60, 0.3, 0.5, nil, random)
	cvs := make([]float64, 0, len(ins))
	for _, in := range ins {
		cvs = append(cvs, RouterCycleOf(in, t0).PlacementCV)
	}
	slices.Sort(cvs)
	require.Greater(t, cvs[len(cvs)/2], SpreadMinCV, "precondition: the noise alone clears the CV floor")
	p := detect(ins, policyStep)
	assert.NotEqual(t, RouterYes, p.LoadAwareBySpread, "%+v", p)
	assert.NotEqual(t, RouterYes, p.LoadAware, "%+v", p)
	assert.Less(t, p.SpreadBalancing, RouterPolicyMinCycles, "%+v", p)
}

// A fall that comes with the time, not the load, is not read as one: the
// first half of the window is all light and keeps the hit, the second all
// heavy and keeps less (a cache that went cold, or a mix that changed),
// and neither half alone shows the share falling with its load.
func TestDetectRouterPolicy_FallOnlyAcrossTheHalvesIsNotLoadAware(t *testing.T) {
	ins := make([]domain.AnalyzerInput, 60)
	for i := range ins {
		load, kept := 1+0.2*float64(i%5), 0.99
		if i >= 30 {
			load, kept = 6+0.2*float64(i%5), 0.90
		}
		r := &domain.RouterLatency{WindowSeconds: 300, PrefixBestAvailableTokenRate: 1000 * load}
		r.PrefixPredictedTokenRate = kept * r.PrefixBestAvailableTokenRate
		ins[i] = pdInput(4, 4, load, r)
	}
	p := detect(ins, 5*time.Minute)
	assert.Equal(t, RouterYes, p.CacheAware, "%+v", p)
	assert.Greater(t, p.KeptLight-p.KeptHeavy, KeptShareFallMin, "precondition: the whole window falls")
	assert.Equal(t, RouterNotDetected, p.LoadAwareByHit, "%+v", p)
	assert.Equal(t, routerPolicyReasonNotBothHalves, p.HitReason)
	assert.Zero(t, p.HalvesFalling)
}

// withProducer sets on every input one producer row, plugin(i) for
// prefill, carrying the cycle's model-level pair.
func withProducer(ins []domain.AnalyzerInput, plugin func(i int) string) {
	for i := range ins {
		r := ins[i].RouterLatency
		r.PrefixProducers = []domain.RouterPrefixProducer{{Plugin: plugin(i), Role: domain.RolePrefill,
			PredictedTokenRate: r.PrefixPredictedTokenRate, BestAvailableTokenRate: r.PrefixBestAvailableTokenRate}}
	}
}

// The hit is read from one producer: where the producer that predicted the
// most changes between cycles, as often as not, nothing is read from the
// hit; a producer that leads on most cycles is pinned and only its cycles
// are read.
func TestDetectRouterPolicy_ProducerIsPinned(t *testing.T) {
	flip := policyCycles(60, 1, 8, func(float64) float64 { return 0.98 }, nil)
	withProducer(flip, func(i int) string { return []string{"a", "b"}[i%2] })
	p := detect(flip, policyStep)
	assert.Equal(t, PolicyNotDetected, p.Policy, "%+v", p)
	assert.Equal(t, routerPolicyReasonProducerChanged, p.CacheReason)

	pinned := policyCycles(60, 1, 8, func(float64) float64 { return 0.98 }, nil)
	withProducer(pinned, func(i int) string {
		if i%10 == 0 {
			return "b"
		}
		return "a"
	})
	for i := 0; i < len(pinned); i += 10 {
		// The minority producer's cycles keep little: read, they would
		// pull the light third under the bound.
		r := pinned[i].RouterLatency
		r.PrefixProducers[0].PredictedTokenRate = 0.1 * r.PrefixProducers[0].BestAvailableTokenRate
	}
	p = detect(pinned, policyStep)
	assert.Equal(t, RouterYes, p.CacheAware, "%+v", p)
	assert.Equal(t, "a/"+domain.RolePrefill, p.Producer)
	assert.Equal(t, 54, p.HitCycles)
}

// A RouterLatency with no window is read at the collector's five minutes,
// never as cycles with no overlap: one-minute cycles then count as one
// independent reading per five.
func TestRouterCycleOf_NoWindowIsTheDefault(t *testing.T) {
	r := &domain.RouterLatency{PrefixPredictedTokenRate: 1, PrefixBestAvailableTokenRate: 2}
	assert.InDelta(t, DefaultRouterWindowSeconds, RouterCycleOf(pdInput(2, 2, 1, r), t0).WindowSeconds, 0)
	r.WindowSeconds = math.NaN()
	assert.InDelta(t, DefaultRouterWindowSeconds, RouterCycleOf(pdInput(2, 2, 1, r), t0).WindowSeconds, 0)
	cycles := make([]RouterCycle, 10)
	for i := range cycles {
		cycles[i] = RouterCycle{At: t0.Add(time.Duration(i) * time.Minute)}
	}
	assert.InDelta(t, 1+9.0/5, independent(cycles), 1e-9, "the first whole, then a fifth a minute")
}

// The window forgets cycles older than RouterPolicyMaxAge behind the
// newest, and everything once it has read no series for
// RouterPolicyStaleAfter; a shorter gap keeps them.
func TestRouterPolicyWindow_AgesAndGoesStale(t *testing.T) {
	read := func(at time.Time) RouterCycle {
		return RouterCycle{At: at, KeptKnown: true, PredictedTokenRate: 1, BestAvailableTokenRate: 1}
	}
	var w RouterPolicyWindow
	w.Note(read(t0))
	w.Note(read(t0.Add(RouterPolicyMaxAge)))
	assert.Equal(t, 2, w.Len(), "exactly the max age is kept")
	last := t0.Add(RouterPolicyMaxAge + time.Minute)
	w.Note(read(last))
	assert.Equal(t, 2, w.Len(), "older than the max age is dropped")

	w.Note(RouterCycle{At: last.Add(RouterPolicyStaleAfter - time.Minute)})
	assert.Equal(t, 2, w.Len(), "a gap under the stale time keeps the cycles")
	w.Note(RouterCycle{At: last.Add(RouterPolicyStaleAfter)})
	assert.Zero(t, w.Len(), "an hour with no series forgets them")
	assert.Equal(t, PolicyNotDetected, w.Policy().Policy)
}

// A policy that flickers is not reported each time: a name read for less
// than RouterPolicyDwell is not a change. When the series stop, not
// detected is reported once, no sooner than the stale time and the dwell.
func TestTracker_ObserveRouterDwellsAndExpires(t *testing.T) {
	tr := NewTracker(nil)
	ins := policyCycles(60, 1, 8, func(load float64) float64 { return 0.99 - 0.015*load }, nil)
	at := t0
	var reported []string
	for _, in := range ins {
		if p, changed := tr.ObserveRouter("ns/m", in, at); changed {
			reported = append(reported, p.Policy)
		}
		at = at.Add(policyStep)
	}
	require.NotEmpty(t, reported)
	require.Equal(t, PolicyCacheAndLoadAware, reported[len(reported)-1], "%v", reported)
	lastRead := at.Add(-policyStep)

	var stopped []string
	for range 90 {
		if p, changed := tr.ObserveRouter("ns/m", pdInput(4, 4, 1, nil), at); changed {
			stopped = append(stopped, p.Policy)
			assert.False(t, at.Before(lastRead.Add(RouterPolicyStaleAfter+RouterPolicyDwell)),
				"reported at %v, before the stale time and the dwell", at.Sub(lastRead))
		}
		at = at.Add(policyStep)
	}
	assert.Equal(t, []string{PolicyNotDetected}, stopped)

	// The last change was reported RouterPolicyDwell after the window first
	// read its name, not on that cycle.
	var w RouterPolicyWindow
	at = t0
	var first time.Time
	for _, in := range ins {
		w.Note(RouterCycleOf(in, at))
		if first.IsZero() && w.Policy().Policy == PolicyCacheAndLoadAware {
			first = at
		}
		at = at.Add(policyStep)
	}
	require.False(t, first.IsZero())
	tr = NewTracker(nil)
	at = t0
	for _, in := range ins {
		if p, changed := tr.ObserveRouter("ns/f", in, at); changed && p.Policy == PolicyCacheAndLoadAware {
			assert.Equal(t, first.Add(RouterPolicyDwell), at, "reported a dwell after it was first read")
		}
		at = at.Add(policyStep)
	}
}
