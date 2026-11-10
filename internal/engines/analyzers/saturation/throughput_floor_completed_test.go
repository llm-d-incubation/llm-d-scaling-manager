package saturation

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The floor's trust read against what the role completes, the swap's drain
// price at the rate the queued replicas serve, and the standing queue's
// price (SwitchFloorTrustReadsCompletions,
// llm-d-incubation/llm-d-scaling-manager#54).

// floorFleet is a decode variant of len(served) replicas, each holding
// resident tokens and queue requests charged perReq tokens each, replica i
// completing served[i] req/s (RequestRate), all at a learned mu of mu with
// two readings, under arrival rate lambda. It returns the model's demand
// after the floor and occupancy's figure.
type floorFleet struct {
	lambda, mu, resident, perReq float64
	served                       []float64
	queue                        int
	switches                     map[string]bool
}

const floorFleetP = 260_622.0

func (f floorFleet) price(t *testing.T) (got, occupancy float64) {
	t.Helper()
	a := NewSaturationAnalyzer(capacity.NewStore())
	if f.switches != nil {
		require.NoError(t, a.SetSwitchesForReplay(f.switches))
	}
	rows := make([]domain.ReplicaMetrics, len(f.served))
	rcs := make([]capacity.ReplicaCapacity, len(f.served))
	for i, s := range f.served {
		pod := fmt.Sprintf("p%d", i)
		rows[i] = domain.ReplicaMetrics{PodName: pod, VariantName: "v", RequestRate: s, RequestRateKnown: true}
		charge := float64(f.queue) * f.perReq
		occupancy += f.resident + charge
		rcs[i] = capacity.ReplicaCapacity{VariantName: "v", PodName: pod, QueueLength: f.queue,
			LocalQueueDemand: int64(charge), SaturatedThroughput: f.mu, SaturatedThroughputSamples: 2}
	}
	in := makeAnalyzerInput(rows, nil)
	in.ArrivalRate = f.lambda
	vcs := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: len(f.served), PerReplicaCapacity: floorFleetP}}
	got = a.applyThroughputFloor(in, in.Config.(*config.ScalingPolicy), rcs, vcs, occupancy, nil, nil, 0, false, logr.Discard())
	return got, occupancy
}

// muCappedAtServed caps each replica's mu at what its role's queued replicas
// complete, only where that is under the mu, leaves warm-pool replicas, and
// returns nothing when it caps none.
func TestMuCappedAtServed(t *testing.T) {
	roleOf := map[string]string{"d": domain.RoleDecode, "p": domain.RolePrefill}
	rcs := []capacity.ReplicaCapacity{
		{VariantName: "d", PodName: "d0", SaturatedThroughput: 6.22},
		{VariantName: "d", PodName: "d1", SaturatedThroughput: 2.5},
		{VariantName: "d", PodName: "dw", SaturatedThroughput: 6.22, FromWarmPool: true},
		{VariantName: "p", PodName: "p0", SaturatedThroughput: 4},
	}
	completed := map[string]completedRate{domain.RoleDecode: {queuedEach: 2.89}, domain.RolePrefill: {queuedEach: 5}}
	got, ok := muCappedAtServed(rcs, roleOf, completed)
	require.True(t, ok)
	assert.Equal(t, 2.89, got[0].SaturatedThroughput, "over what the queued replicas serve: capped")
	assert.Equal(t, 2.5, got[1].SaturatedThroughput, "under it: kept")
	assert.Equal(t, 6.22, got[2].SaturatedThroughput, "a warm-pool replica serves nothing of the role's")
	assert.Equal(t, 4.0, got[3].SaturatedThroughput, "prefill serves over its mu: kept")
	assert.Equal(t, 6.22, rcs[0].SaturatedThroughput, "the input is not changed")

	_, ok = muCappedAtServed(rcs, roleOf, map[string]completedRate{domain.RoleDecode: {queuedEach: 7}})
	assert.False(t, ok, "nothing over what is served: no second floor")
	_, ok = muCappedAtServed(rcs, roleOf, map[string]completedRate{domain.RoleDecode: {}})
	assert.False(t, ok, "no served figure (too few queued): no cap")
}

// pd-restart at 12:46:35: queued replicas served 2.89 req/s against a
// learned 6.22. The swap prices the queue at the floor at the rate served,
// exactly the floor of a fleet whose mu is 2.89, and more than the floor at
// 6.22.
func TestThroughputFloor_AFleetServingUnderItsMuIsPricedAtTheServedRate(t *testing.T) {
	base := floorFleet{lambda: 2 * 2.89, mu: 6.22, resident: 0.9 * floorFleetP, perReq: 5000,
		served: []float64{2.89, 2.89}, queue: 30}
	got, occupancy := base.price(t)
	atServed := base
	atServed.mu = 2.89
	want, _ := atServed.price(t)
	atMu := base
	atMu.served = []float64{6.22, 6.22}
	atMu.lambda = 2 * 6.22
	t.Logf("served 2.89 under mu 6.22: %.0f; a fleet of mu 2.89: %.0f; occupancy %.0f", got, want, occupancy)
	assert.InDelta(t, want, got, 1, "the floor at the served rate")
	assert.Less(t, got, occupancy-1, "under occupancy: the swap ran")
	uncapped := floorFleet{lambda: base.lambda, mu: 6.22, resident: base.resident, perReq: base.perReq,
		served: []float64{2.89, 2.89}, queue: 30, switches: map[string]bool{SwitchFloorTrustReadsCompletions: false}}
	off, _ := uncapped.price(t)
	t.Logf("switch off (mu's ratio, the floor at 6.22): %.0f", off)
	assert.Greater(t, got, (2*2.89+60.0/60)/6.22*floorFleetP, "over the floor at mu 6.22")
}

// No completion on record is no evidence lambda is current: no trust, and
// occupancy's charge stands, however lambda reads.
func TestThroughputFloor_NoCompletionsKeepOccupancy(t *testing.T) {
	assert.Zero(t, defaultTuning.completedTrust(5, 0))
	assert.Zero(t, defaultTuning.completedTrust(5, math.NaN()))
	assert.Zero(t, defaultTuning.completedTrust(math.NaN(), 5))
	f := floorFleet{lambda: 6, mu: 6, resident: 260_123, perReq: 239_323.0 / 160, served: []float64{0}, queue: 160}
	got, occupancy := f.price(t)
	assert.InDelta(t, occupancy, got, 1)
}

// The lag still reads against what the replica completes when that is under
// its mu: a replica completing 3 req/s at a learned 6, with lambda at 0.8,
// is lagging (0.27 of 3), and occupancy stands.
func TestThroughputFloor_ALaggingLambdaAgainstAReplicaUnderItsMu(t *testing.T) {
	f := floorFleet{lambda: 0.8, mu: 6, resident: 260_123, perReq: 239_323.0 / 160, served: []float64{3}, queue: 160}
	got, occupancy := f.price(t)
	assert.InDelta(t, occupancy, got, 1)
}

// completedTrust is floorTrust of lambda over completions, and floorBlend
// moves the demand that share of the way to the drain price.
func TestFloorBlend_APartialTrustBlendsInProportion(t *testing.T) {
	trust := defaultTuning.completedTrust(0.65*10, 10)
	assert.InDelta(t, 0.5, trust, 1e-12)
	const measured, resident, tokens, drain = 500_000.0, 260_000.0, 300_000.0, 350_000.0
	assert.InDelta(t, measured-0.5*(measured-drain), floorBlend(measured, resident, tokens, drain, trust, true), 1e-6)
	assert.InDelta(t, measured, floorBlend(measured, resident, tokens, drain, trust, false), 1e-6, "no swap")
	assert.InDelta(t, measured, floorBlend(measured, resident, tokens, 600_000, 1, true), 1e-6,
		"a drain price over occupancy never raises it")
}

// completedRateByRole: per replica the larger of RequestRate and Little's
// law, summed per role; warm-pool replicas left out; the median over the
// queued replicas, from at least half of the role's.
func TestCompletedRateByRole(t *testing.T) {
	roleOf := map[string]string{"d": domain.RoleDecode, "p": domain.RolePrefill}
	row := func(variant, pod string, rate float64, running int, svc float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{VariantName: variant, PodName: pod, RequestRate: rate, RunningRequests: running, AvgServiceTime: svc}
	}
	rc := func(variant, pod string, q int) capacity.ReplicaCapacity {
		return capacity.ReplicaCapacity{VariantName: variant, PodName: pod, QueueLength: q}
	}
	in := makeAnalyzerInput([]domain.ReplicaMetrics{
		row("d", "a", 1, 12, 2),  // a stale one-minute rate; the batch completes 6
		row("d", "b", 4, 4, 2),   // the rate is the larger
		row("d", "c", 9, 0, 0),   // no Little's law read
		row("p", "x", 3, 0, 0),   // prefill, its own role
		row("d", "w", 50, 50, 1), // warm pool
	}, nil)
	in.ReplicaMetrics[4].FromWarmPool = true
	got := completedRateByRole(in, []capacity.ReplicaCapacity{rc("d", "a", 5), rc("d", "b", 5), rc("d", "c", 5), rc("p", "x", 0)}, roleOf)
	assert.InDelta(t, 6+4+9, got[domain.RoleDecode].total, 1e-12)
	assert.InDelta(t, 3, got[domain.RolePrefill].total, 1e-12)
	assert.InDelta(t, 6, got[domain.RoleDecode].queuedEach, 1e-12, "the median of 6, 4, 9: not the mean")
	assert.Zero(t, got[domain.RolePrefill].queuedEach, "no queued replica")

	// Three queued completing 2, 2 and 9: the median, 2, not the mean 4.33.
	in = makeAnalyzerInput([]domain.ReplicaMetrics{row("d", "a", 2, 0, 0), row("d", "b", 2, 0, 0), row("d", "c", 9, 0, 0)}, nil)
	got = completedRateByRole(in, []capacity.ReplicaCapacity{rc("d", "a", 3), rc("d", "b", 3), rc("d", "c", 3)}, roleOf)
	assert.InDelta(t, 2, got[domain.RoleDecode].queuedEach, 1e-12)
	// One of three queued: under half, no figure.
	got = completedRateByRole(in, []capacity.ReplicaCapacity{rc("d", "a", 3), rc("d", "b", 0), rc("d", "c", 0)}, roleOf)
	assert.Zero(t, got[domain.RoleDecode].queuedEach)
	// Two of four queued: half counts.
	in = makeAnalyzerInput([]domain.ReplicaMetrics{row("d", "a", 2, 0, 0), row("d", "b", 4, 0, 0), row("d", "c", 9, 0, 0), row("d", "e", 9, 0, 0)}, nil)
	got = completedRateByRole(in, []capacity.ReplicaCapacity{rc("d", "a", 3), rc("d", "b", 3), rc("d", "c", 0), rc("d", "e", 0)}, roleOf)
	assert.InDelta(t, 3, got[domain.RoleDecode].queuedEach, 1e-12)
}

// A queue that stops growing is priced at least as the growing region priced
// it, net of the growth it no longer has, and never above occupancy
// (drainStanding): one replica at mu 6 completing 6 (RunningRequests /
// AvgServiceTime), lambda 6, the queue 100 -> 160 over a minute, then
// standing at 160 for a minute.
func TestQueueDrain_AQueueThatStopsGrowingIsPricedAsDrain(t *testing.T) {
	const mu, resident, perReq = 6.0, 260_123.0, 239_323.0 / 160
	a := NewSaturationAnalyzer(capacity.NewStore())
	clock := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	price := func(q int) (got, occupancy float64) {
		row := domain.ReplicaMetrics{PodName: "p", VariantName: "v", QueueLength: q, KvCacheUsage: 0.9,
			RunningRequests: 16, AvgServiceTime: 16 / mu, RequestRate: mu, RequestRateKnown: true}
		in := makeAnalyzerInput([]domain.ReplicaMetrics{row}, nil)
		in.ArrivalRate = 6
		charge := perReq * float64(q)
		rcs := []capacity.ReplicaCapacity{{VariantName: "v", PodName: "p", QueueLength: q, LocalQueueDemand: int64(charge),
			SaturatedThroughput: mu, SaturatedThroughputSamples: 2}}
		vcs := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: floorFleetP}}
		occupancy = resident + float64(int64(charge))
		got = a.applyThroughputFloor(in, in.Config.(*config.ScalingPolicy), rcs, vcs, occupancy, nil, nil, 0, false, logr.Discard())
		return got, occupancy
	}
	var growing float64
	for i, q := range []int{100, 120, 140, 160, 160, 160, 160, 160, 160} {
		got, occupancy := price(q)
		t.Logf("t=%3ds queue %3d: %.0f (%.2f replicas), occupancy %.0f", 15*i, q, got, got/floorFleetP, occupancy)
		assert.LessOrEqual(t, got, max(occupancy, (6+float64(q)/60)/mu*floorFleetP)+1, "never over occupancy or the drain price")
		if i == 3 {
			growing = got
		}
		if i >= 4 {
			// The drain price of the standing queue: the arrivals the replica
			// completes and the queue over the drain time.
			assert.GreaterOrEqual(t, got, min(occupancy, (6+160.0/60)/mu*floorFleetP)-1, "t=%ds: under the standing queue's drain price", 15*i)
		}
		clock = clock.Add(15 * time.Second)
	}
	assert.Greater(t, growing, 0.0)
}

// The standing price (drainStanding) reads its arrivals from what the role
// completes, where lambda may read under it and still be trusted: lambda 5
// against 6 completed is 0.83, full trust, and the swap alone prices the
// queue at 5 req/s. The standing price is (6 + 160 / 60) / 6 replicas,
// capped at occupancy.
func TestQueueDrain_AStandingQueueIsPricedAtWhatTheRoleCompletes(t *testing.T) {
	f := floorFleet{lambda: 5, mu: 6, resident: 260_123, perReq: 239_323.0 / 160, served: []float64{6}, queue: 160}
	got, occupancy := f.price(t)
	want := (6 + 160.0/60) / 6 * floorFleetP
	t.Logf("standing at 160, lambda 5, completing 6: %.0f (drain price %.0f, occupancy %.0f, the swap at lambda %.0f)",
		got, want, occupancy, (5+160.0/60)/6*floorFleetP)
	assert.InDelta(t, want, got, 1)
	// Never over occupancy: the same queue charged 1000 tokens a request.
	f.perReq, f.resident = 1000, 200_000
	got, occupancy = f.price(t)
	require.Less(t, occupancy, want, "premise: occupancy under the drain price")
	assert.InDelta(t, occupancy, got, 1)
}
