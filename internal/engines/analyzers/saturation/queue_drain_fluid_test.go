package saturation

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/floor"
)

// The drain price counted from when the capacity it orders lands
// (SwitchQueueDrainFluidAfterStart, queue_drain.go; queueing review F7).

// fluidClearedAt is when a fluid queue of backlog requests clears: ready
// replicas serve mu each from now, total replicas from start on, against
// arrival. +Inf where it never clears.
func fluidClearedAt(arrival, backlog, ready, total, start, mu float64) float64 {
	b := backlog + (arrival-ready*mu)*start
	if b <= 0 {
		// Drained by the replicas already serving, before landing.
		return backlog / (ready*mu - arrival)
	}
	surplus := total*mu - arrival
	if surplus <= 0 {
		return math.Inf(1)
	}
	return start + b/surplus
}

// The fluid count clears the queue by start + horizon, whatever the start;
// the price from now clears it only once the start is short beside the
// horizon. With no start, or the ready fleet draining the queue before
// landing, it never prices under the arrivals.
func TestQueueDrainFluid_TheFluidCountClearsByTheHorizon(t *testing.T) {
	const arrival, backlog, mu = 6.0, 120.0, 1.0
	for _, tc := range []struct {
		name              string
		ready, start      float64
		want              float64
		wantLanding       float64
		fromNowClearsLate bool
	}{
		// landing 120 + (6 - 2) x 30 = 240; N = 6 + 240 / 60 = 10, which is
		// 2 + (120 + 4 x 90) / 60 in the review's form, T = 90.
		{name: "start under the drain", ready: 2, start: 30, want: 10, wantLanding: 240, fromNowClearsLate: true},
		// landing 120 + 4 x 300 = 1320 over a horizon of 300: 10.4, the
		// review's 2 + (120 + 4 x 600) / 300 at T = 600.
		{name: "start past the drain", ready: 2, start: 300, want: 10.4, wantLanding: 1320, fromNowClearsLate: true},
		// Eight replicas drain 2 a second over the arrivals: the queue is
		// gone in 60 s, before the 90 s start, so only the arrivals are
		// priced.
		{name: "drained before landing", ready: 8, start: 90, want: 6, wantLanding: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			horizon := floor.DrainHorizon(floor.BacklogDrainSeconds, tc.start)
			got, landing := drainReplicasFluid(arrival, backlog, tc.ready, tc.start, horizon, mu)
			fromNow := drainReplicas(arrival, backlog, horizon, mu)
			clears := fluidClearedAt(arrival, backlog, tc.ready, got, tc.start, mu)
			late := fluidClearedAt(arrival, backlog, tc.ready, fromNow, tc.start, mu)
			t.Logf("fluid %.2f replicas (landing %.0f) clears at %.0f s; from now %.2f clears at %.0f s; horizon ends at %.0f s",
				got, landing, clears, fromNow, late, tc.start+horizon)
			assert.InDelta(t, tc.want, got, 1e-9)
			assert.InDelta(t, tc.wantLanding, landing, 1e-9)
			assert.GreaterOrEqual(t, got, arrival/mu, "never under the arrivals")
			assert.LessOrEqual(t, clears, tc.start+horizon+1e-6, "the fluid count clears by the horizon after landing")
			if tc.fromNowClearsLate {
				assert.Greater(t, late, tc.start+horizon+1, "control: the price from now clears after it")
			}
		})
	}
	t.Run("no start is the price from now", func(t *testing.T) {
		for _, start := range []float64{0, math.NaN(), math.Inf(1), -5} {
			got, landing := drainReplicasFluid(arrival, backlog, 2, start, 60, mu)
			assert.InDelta(t, drainReplicas(arrival, backlog, 60, mu), got, 1e-12, "start %v", start)
			assert.InDelta(t, backlog, landing, 1e-12)
		}
	})
	t.Run("prices nothing where the price from now prices nothing", func(t *testing.T) {
		for _, c := range [][3]float64{{arrival, backlog, 0}, {math.NaN(), backlog, mu}, {arrival, math.Inf(1), mu}} {
			got, _ := drainReplicasFluid(c[0], c[1], 2, 60, 60, c[2])
			assert.Zero(t, got)
		}
	})
}

// pd-restart's decode role at 12:28:02 with a 60 s start: two replicas at
// their cap, 130 queued, 14.18 req/s arriving, first-read mu 6.20 haircut to
// 5.27. Priced from now, (14.18 + 130 / 60) / 5.27 = 3.10 replicas at 100 %,
// which lands into a backlog of 348 and clears it 161 s later, at 221 s.
// Priced from landing, 14.18 / 5.27 + 348 / (5.27 x 60) = 3.79, clearing it
// by 120 s. Off, the price from now stands.
func TestQueueDrainFluid_PricesTheQueueAtLanding(t *testing.T) {
	const p, lambda, start, mu0 = 46_556.0, 14.18, 60.0, 6.20
	d := drainRead{mu0: mu0, throughput: 2 * mu0, queues: []float64{67, 63}, perReplica: p}
	mu := FirstReadHaircut * mu0
	for _, on := range []bool{true, false} {
		a := NewSaturationAnalyzer(drainStore(propStart))
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchQueueDrainFluidAfterStart: on}))
		c := newDrainContext(lambda, map[string]float64{domain.RoleDecode: start}, nil, nil, propStart, MinMeasuredOfDerived)
		want, priced := a.priceDrain("test-ns", "test-model", domain.RoleDecode, d, c,
			drainTerm{want: 2 * p, resident: 2 * p})
		replicas := want / p
		clears := fluidClearedAt(lambda, 130, 2, replicas, start, mu)
		t.Logf("switch %v: %.2f replicas at 100 %%, landing backlog %.0f, the queue clears at %.0f s", on, replicas,
			priced.landing, clears)
		require.Equal(t, drainBeforeTwoReadings, priced.region)
		require.Equal(t, start, priced.horizon)
		if on {
			landing := 130 + (lambda-2*mu)*start
			assert.InDelta(t, landing, priced.landing, 1e-9)
			assert.InDelta(t, (lambda+landing/start)/mu, replicas, 1e-9)
			assert.LessOrEqual(t, clears, 2*start+1e-6, "clears within the horizon after landing")
			continue
		}
		assert.Zero(t, priced.landing)
		assert.InDelta(t, (lambda+130/start)/mu, replicas, 1e-9, "off: the price from now")
		assert.Greater(t, clears, 2*start+60, "off: the queue outlives the horizon by more than a minute")
	}
}

// The haircut hold prices at landing too: a held first read with the queue
// standing and a fresh read the floor's swap would take prices the same
// fluid count as the region that set it.
func TestQueueDrainFluid_TheHeldPriceIsCountedAtLanding(t *testing.T) {
	const p, lambda, start, mu0 = 100_000.0, 6.0, 300.0, 1.0
	for _, on := range []bool{true, false} {
		a := NewSaturationAnalyzer(drainStore(propStart))
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchQueueDrainFluidAfterStart: on}))
		starts := map[string]float64{domain.RoleDecode: start}
		d := drainRead{mu0: mu0, throughput: 2 * mu0, queues: []float64{60, 60}, perReplica: p}
		set := newDrainContext(lambda, starts, nil, nil, propStart, MinMeasuredOfDerived)
		first, _ := a.priceDrain("test-ns", "test-model", domain.RoleDecode, d, set, drainTerm{want: p, resident: p})
		// The next cycle: the floor's term may order and the backlog stands,
		// the swap's case, which leaves the role at the held price.
		next := newDrainContext(lambda, starts, nil, nil, propStart.Add(15e9), MinMeasuredOfDerived)
		held, priced := a.priceDrain("test-ns", "test-model", domain.RoleDecode, d, next,
			drainTerm{want: p, resident: p, inFloor: true, term: floor.Term{MayOrder: true, Mu: 10, PerReplica: p}})
		t.Logf("switch %v: first %.2f, held %.2f replicas at 100 %% (%s)", on, first/p, held/p, priced.region)
		require.Equal(t, drainHaircutHeld, priced.region)
		assert.InDelta(t, first, held, 1)
		mu := FirstReadHaircut * mu0
		if on {
			landing := 120 + (lambda-2*mu)*start
			assert.InDelta(t, (lambda+landing/start)/mu*p, held, 1)
			continue
		}
		assert.InDelta(t, (lambda+120/start)/mu*p, held, 1, "off: the price from now")
	}
}
