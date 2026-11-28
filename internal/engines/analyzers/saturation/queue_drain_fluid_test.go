package saturation

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/floor"
)

// The drain price counted from when the capacity it orders lands
// (SwitchQueueDrainFluidAfterStart, queue_drain.go; queueing review F7).

// fluidClearedAt is when a fluid queue of backlog requests clears: the
// fleet serves now requests a second from now, and after from start on,
// against arrival. +Inf where it never clears.
func fluidClearedAt(arrival, backlog, now, after, start float64) float64 {
	b := backlog + (arrival-now)*start
	if b <= 0 {
		// Drained by the replicas already serving, before landing.
		return backlog / (now - arrival)
	}
	surplus := after - arrival
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
			got, landing := drainReplicasFluid(arrival, backlog, tc.ready, 0, tc.start, horizon, mu, nil)
			fromNow := drainReplicas(arrival, backlog, horizon, mu)
			clears := fluidClearedAt(arrival, backlog, tc.ready*mu, got*mu, tc.start)
			late := fluidClearedAt(arrival, backlog, tc.ready*mu, fromNow*mu, tc.start)
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
			got, landing := drainReplicasFluid(arrival, backlog, 2, 0, start, 60, mu, nil)
			assert.InDelta(t, drainReplicas(arrival, backlog, 60, mu), got, 1e-12, "start %v", start)
			assert.InDelta(t, backlog, landing, 1e-12)
		}
	})
	t.Run("prices nothing where the price from now prices nothing", func(t *testing.T) {
		for _, c := range [][3]float64{{arrival, backlog, 0}, {math.NaN(), backlog, mu}, {arrival, math.Inf(1), mu}} {
			got, _ := drainReplicasFluid(c[0], c[1], 2, 0, 60, 60, c[2], nil)
			assert.Zero(t, got)
		}
	})
}

// pd-restart's decode role at 12:28:02 with a 60 s start: two replicas at
// their cap completing 12.40 req/s, 130 queued, 14.18 req/s arriving,
// first-read mu 6.20 haircut to 5.27. The ready replicas are charged at what
// they complete, not at 2 x 5.27, so the queue grows 1.78 req/s through the
// start and lands at 237. Priced from landing, 14.18 / 5.27 + 237 / (5.27 x
// 60) = 3.44 replicas at 100 %, which clears it by 120 s. Priced from now,
// (14.18 + 130 / 60) / 5.27 = 3.10, which clears it at about 119 s here: at
// a 60 s start the two prices are close, and the gap is the longer starts'
// (TestQueueDrainFluid_TheFluidCountClearsByTheHorizon). Off, the price from
// now stands.
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
		clears := fluidClearedAt(lambda, 130, d.throughput, d.throughput+(replicas-2)*mu, start)
		t.Logf("switch %v: %.2f replicas at 100 %%, landing backlog %.0f, the queue clears at %.0f s", on, replicas,
			priced.landing, clears)
		require.Equal(t, drainBeforeTwoReadings, priced.region)
		require.Equal(t, start, priced.horizon)
		if on {
			landing := 130 + (lambda-d.throughput)*start
			assert.InDelta(t, landing, priced.landing, 1e-9, "the ready replicas charged at what they complete")
			assert.InDelta(t, 3.44, replicas, 0.005)
			assert.InDelta(t, (lambda+landing/start)/mu, replicas, 1e-9)
			assert.LessOrEqual(t, clears, 2*start+1e-6, "clears within the horizon after landing")
			continue
		}
		assert.Zero(t, priced.landing)
		assert.InDelta(t, (lambda+130/start)/mu, replicas, 1e-9, "off: the price from now")
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
			// The ready replicas complete 2 req/s, over their 2 x 0.85 at
			// the haircut mu.
			landing := 120 + (lambda-d.throughput)*start
			assert.InDelta(t, (lambda+landing/start)/mu*p, held, 1)
			continue
		}
		assert.InDelta(t, (lambda+120/start)/mu*p, held, 1, "off: the price from now")
	}
}

// The replicas already starting are credited as floor.Estimate credits them:
// a cycle with the order pending prices no more than the cycle that placed
// it, though the queue has grown by what the ready replicas fall behind. The
// review's case: 6 req/s, mu 1, two ready replicas, 120 queued, 15 s cycles.
// Without the credit each cycle re-counted the start's arrivals and asked for
// more (12 replicas at 15 s on a 90 s start, against 11.33).
func TestQueueDrainFluid_APendingOrderIsCredited(t *testing.T) {
	const p, lambda, mu, cycle = 100_000.0, 6.0, 1.0, 15.0
	for _, start := range []float64{60, 90, 300} {
		t.Run(fmt.Sprintf("start %.0f s", start), func(t *testing.T) {
			a := NewSaturationAnalyzer(drainStore(propStart))
			require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchQueueDrainFluidAfterStart: true}))
			starts := map[string]float64{domain.RoleDecode: start}
			// mu0 is the first read the haircut takes to mu 1; the two ready
			// replicas complete 2 req/s.
			d := drainRead{mu0: mu / FirstReadHaircut, throughput: 2 * mu, queues: []float64{60, 60}, perReplica: p}
			first := 0.0
			var pending int
			for k := range 4 {
				var variants []domain.VariantCapacity
				if k > 0 {
					ages := make([]float64, pending)
					for i := range ages {
						ages[i] = float64(k) * cycle
					}
					variants = []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode,
						ReplicaCount: 2, PendingReplicas: pending, PendingAges: ages}}
				}
				now := propStart.Add(time.Duration(k) * time.Duration(cycle) * time.Second)
				c := newDrainContext(lambda, starts, variants, nil, now, MinMeasuredOfDerived)
				want, priced := a.priceDrain("test-ns", "test-model", domain.RoleDecode, d, c,
					drainTerm{want: 2 * p, resident: 2 * p})
				n := want / p
				t.Logf("cycle %d: queue %.0f, %d pending, %.2f replicas at 100 %% (landing %.0f, %s)",
					k, d.queues[0]+d.queues[1], pending, n, priced.landing, priced.region)
				require.Equal(t, drainBeforeTwoReadings, priced.region)
				if k == 0 {
					first = n
					pending = int(math.Ceil(n-1e-9)) - 2
					require.Positive(t, pending)
				} else {
					assert.LessOrEqual(t, n, first+1e-9, "a pending order is credited, not re-ordered")
				}
				// The queue grows by what the two ready replicas fall behind.
				for i := range d.queues {
					d.queues[i] += (lambda - 2*mu) * cycle / 2
				}
			}
		})
	}
}

// The review's fluid simulation, closed loop: every 15 s the price is taken
// and the engine orders ceil(N) - (ready + pending); ordered replicas land a
// start later. With the starting replicas credited the fleet peaks no higher
// than the price from now peaks, and still clears the queue by start + T
// after the first order; without the credit it ordered four more on every
// start.
func TestQueueDrainFluid_TheClosedLoopDoesNotRatchet(t *testing.T) {
	const lambda, mu, backlog0, cycle, until = 6.0, 1.0, 120.0, 15.0, 600.0
	run := func(fluid bool, start float64) (peak int, cleared float64) {
		horizon := floor.DrainHorizon(floor.BacklogDrainSeconds, start)
		ready, b := 2, backlog0
		var landsAt []float64
		peak, cleared = ready, math.Inf(1)
		next := 0.0
		for now := 0.0; now < until; now++ {
			if now >= next-1e-9 {
				next += cycle
				if b > 0 {
					n := drainReplicas(lambda, b, horizon, mu)
					if fluid {
						ages := make([]float64, len(landsAt))
						for i, at := range landsAt {
							ages[i] = start - (at - now)
						}
						n, _ = drainReplicasFluid(lambda, b, float64(ready), 0, start, horizon, mu,
							[]floor.StartingReplicas{{Count: len(landsAt), Ages: ages}})
					}
					for range max(int(math.Ceil(n-1e-9))-(ready+len(landsAt)), 0) {
						landsAt = append(landsAt, now+start)
					}
				}
			}
			still := landsAt[:0]
			for _, at := range landsAt {
				if at <= now {
					ready++
				} else {
					still = append(still, at)
				}
			}
			landsAt = still
			peak = max(peak, ready+len(landsAt))
			b = max(b+(lambda-float64(ready)*mu), 0)
			if b == 0 && math.IsInf(cleared, 1) && now > 0 {
				cleared = now
			}
		}
		return peak, cleared
	}
	for _, start := range []float64{60, 90, 300} {
		deadline := start + floor.DrainHorizon(floor.BacklogDrainSeconds, start)
		offPeak, offCleared := run(false, start)
		peak, cleared := run(true, start)
		t.Logf("start %.0f s: from now peak %d, cleared at %.0f s; fluid peak %d, cleared at %.0f s; deadline %.0f s",
			start, offPeak, offCleared, peak, cleared, deadline)
		assert.LessOrEqual(t, peak, offPeak, "start %.0f: no more replicas than the price from now", start)
		assert.LessOrEqual(t, cleared, deadline, "start %.0f: cleared by start + T", start)
	}
}

// The starting set is the pending replicas less the stuck ones, per
// variant, with their ages: a stuck Pod is not on its way, and the engine
// leaves it out of anticipated supply too.
func TestQueueDrainFluid_StartingSetLeavesStuckOut(t *testing.T) {
	got := startingByRole([]domain.VariantCapacity{
		{VariantName: "a", Role: domain.RoleDecode, PendingReplicas: 3, StuckReplicas: 1, PendingAges: []float64{10, 20}},
		{VariantName: "b", Role: domain.RoleDecode, PendingReplicas: 2, StuckReplicas: 2},
		{VariantName: "c", Role: domain.RolePrefill},
	})
	assert.Equal(t, map[string][]floor.StartingReplicas{
		domain.RoleDecode: {{Count: 2, Ages: []float64{10, 20}}},
	}, got)
}
