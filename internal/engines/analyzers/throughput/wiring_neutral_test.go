package throughput

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// neutralCycles is how many 15 s cycles the scripted run lasts: 10 minutes.
const neutralCycles = 40

// neutralRows is cycle i of a scripted fleet of one variant whose replicas
// cover every kind of row the admission will judge: two in the band, one of
// them stepping between two levels so the window reaches a full fit; one
// below the band; one above it; one at its stated sequence cap above the band,
// with requests waiting and no preemption; one that joins at cycle 12; and
// one that is not Ready on cycles 20 and 21 and comes back with a new Ready
// time, so it settles again. ITL is 6 ms plus 50 ms per unit of k.
func neutralRows(start time.Time, i int) []domain.ReplicaMetrics {
	now := start.Add(time.Duration(i) * 15 * time.Second)
	row := func(pod string, k float64, readySince time.Time) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName: pod, VariantName: "v1", Ready: true, ReadySince: readySince,
			KvCacheUsage: k, KvUsageInstant: k, TotalKvCapacityTokens: 65536,
			AvgInputTokens: 1024, AvgOutputTokens: 256,
			// No generation rate: the GPS check, which clears the window
			// after three mismatches, would keep it from filling.
			AvgITL: 0.006 + 0.05*k, AvgITLKnown: true, RequestRate: 2,
		}
	}
	early := start.Add(-time.Hour)
	stepK := 0.25
	if (i/16)%2 == 1 {
		stepK = 0.65
	}
	capped := row("pod-cap", 0.88, early)
	capped.RunningRequests, capped.RunningRequestsKnown, capped.QueueLength = 64, true, 5
	capped.PreemptionRate, capped.PreemptionRateKnown = 0, true
	rows := []domain.ReplicaMetrics{
		row("pod-step", stepK, early),
		row("pod-mid", 0.40, early),
		row("pod-low", 0.05, early),
		row("pod-high", 0.92, early),
		capped,
	}
	if joined := start.Add(12 * 15 * time.Second); !now.Before(joined) {
		rows = append(rows, row("pod-join", 0.50, joined))
	}
	back := start.Add(22 * 15 * time.Second)
	switch {
	case i == 20 || i == 21:
		gone := row("pod-settle", 0.45, early)
		gone.Ready, gone.AvgITL = false, 0
		rows = append(rows, gone)
	case i >= 22:
		rows = append(rows, row("pod-settle", 0.45, back))
	default:
		rows = append(rows, row("pod-settle", 0.45, early))
	}
	return rows
}

// The wiring is neutral: an analyzer given the engine's settle tracker,
// a capacity store stating the capped replica's cap, the optimize interval
// and a slope hold reads the scripted fleet exactly as a bare one does --
// the same window and line every cycle, and the same result, with the
// settle switches of PR 4b-2 and the cap admission of PR 4b-3 off: beyond
// those named changes, the store, the interval and the hold may change
// nothing it admits. The run must reach a full fit, so the equality is not
// one of two empty windows. With the cap admission on, the wired line
// admits pod-cap and the two differ (TestITLCap_TheWiredLineAdmitsTheCappedReplica).
func TestTheWiredAnalyzerReadsTheFleetAsABareOneDoes(t *testing.T) {
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	bareClock, wiredClock := clocktesting.NewFakePassiveClock(start), clocktesting.NewFakePassiveClock(start)
	store := capacity.NewStore()
	store.FileScaleTarget("ns", "m", "v1", "H100", 1,
		capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, MaxNumSeqsSet: true, Image: "img:1"},
		false, start)
	tracker := settle.NewTracker()
	bare := NewThroughputAnalyzer().WithClock(bareClock)
	wired := NewThroughputAnalyzer().WithClock(wiredClock).WithSettleView(tracker).WithCapacityStore(store).
		WithOptimizationInterval(15 * time.Second).WithSlopeHoldFor(24 * time.Hour)
	// The settle view is read from learner PR 4b-2 on, under its switches
	// (settle.go, settle_test.go): off on both here, each line judges the
	// settle from its own trace, and the rest of the wiring is what the
	// equality holds.
	off := map[string]bool{SwitchITLAdmitsAtSequenceCap: false}
	for _, name := range settleSwitches {
		off[name] = false
	}
	require.NoError(t, bare.SetSwitchesForReplay(off))
	require.NoError(t, wired.SetSwitchesForReplay(off))
	policy := &config.ScalingPolicy{}
	ctx := context.Background()

	fits, maxSamples := 0, 0
	for i := range neutralCycles {
		now := start.Add(time.Duration(i) * 15 * time.Second)
		bareClock.SetTime(now)
		wiredClock.SetTime(now)
		rows := neutralRows(start, i)
		// The engine observes its settle tracker before any analyzer runs.
		tracker.Observe("ns/m", settlerow.Of(rows, nil), now)
		in := domain.AnalyzerInput{ModelID: "m", Namespace: "ns", ReplicaMetrics: rows, Config: policy, ArrivalRate: 8,
			VariantStates: []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: len(rows), GPUsPerReplica: 1}}}

		got, err := wired.Analyze(ctx, in)
		require.NoError(t, err)
		want, err := bare.Analyze(ctx, in)
		require.NoError(t, err)
		require.Equal(t, want, got, "cycle %d: the wired analyzer's result differs", i)

		ws, ok := bare.VariantState("m", "ns", "v1")
		require.True(t, ok)
		gs, ok := wired.VariantState("m", "ns", "v1")
		require.True(t, ok)
		require.Equal(t, ws, gs, "cycle %d: the wired analyzer's window or line differs", i)
		if len(want.VariantCapacities) == 1 && want.VariantCapacities[0].Reason == itlReasonT1OLS {
			fits++
		}
		maxSamples = max(maxSamples, ws.SampleCount)
	}
	require.Positive(t, fits, "the scripted run never reached a full fit: the equality compares nothing")
	require.Equal(t, 20, maxSamples, "the window never filled: the equality compares little")
	t.Logf("%d cycles, %d full fits, window up to %d readings", neutralCycles, fits, maxSamples)
}
