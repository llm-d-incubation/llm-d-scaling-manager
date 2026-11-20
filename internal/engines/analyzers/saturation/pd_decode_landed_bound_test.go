package saturation

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Issue llm-d-incubation/llm-d-scaling-manager#201, the second reading. The
// shape: a P/D decode pod of four ranks, a stated cap of 64 per rank and 9
// sequences running over all four, its busiest rank at 24 % KV. Five
// requests whose KV has landed wait on that rank for the next step; none
// waits for capacity. The landed five met the queue threshold of 5, the
// saturation test recorded a k2 at the busiest rank's 24 %, scaled to the
// pod, and the variant was priced there from then on.

// landedRow is that pod: five landed requests on its busiest rank, running
// sequences over its ranks, the busiest rank at busiestKV. It computes 1,600
// prompt tokens a request and accepts 1.8 draft tokens a step (budgetRow), so
// one rank's 256-token budget holds 256 x 250 / (1,600 + 4 x 250) = 24.6
// requests in steady state (steadyBatch). Its mean KV is 0.15, so its busiest
// rank runs 0.24 / 0.15 = 1.6 times the mean a rank of four: 0.4 x running
// (busiestRankRunning).
func landedRow(running int, busiestKV float64) domain.ReplicaMetrics {
	const n = 5
	total := budgetRanks * budgetKVPerRank
	rm := makeReplicaMetrics("decode-0", decodeVariant, total*15/100, total, n, budgetIn, budgetOut)
	rm.EngineRanks = budgetRanks
	rm.MaxRankQueueLength = n
	rm.MaxRankKvCacheUsage = busiestKV
	rm.RunningRequests, rm.RunningRequestsKnown = running, true
	rm.RequestRate, rm.RequestRateKnown = budgetReqRate, true
	rm.PrefillComputedTokenRate, rm.PrefillComputedTokenRateKnown = budgetPromptRate, true
	rm.SpecAcceptedPerStep, rm.SpecAcceptedPerStepKnown = budgetAccepted, true
	return withLanded(withReason(rm, 0), n)
}

// analyzeLanded runs four cycles of row beside a prefill replica with the
// switch on or off, and reports whether decode recorded a k2.
func analyzeLanded(t *testing.T, row domain.ReplicaMetrics, on bool) bool {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", decodeVariant, capacity.Record{
		GpuCount: 32, EngineParams: budgetParams(), LearnedFrom: "deployment",
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchDecodeLandedNeedsABound: on}))
	clock := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	in := makeAnalyzerInput([]domain.ReplicaMetrics{row, pdPrefillRow()}, pdStates)
	for range 4 {
		_, err := a.Analyze(context.Background(), in)
		require.NoError(t, err)
		clock = clock.Add(15 * time.Second)
	}
	return recordedK2(a)
}

func TestDecodeLandedNeedsABound_ARoomyDecodeRecordsNoK2(t *testing.T) {
	row := landedRow(9, 0.24)
	require.True(t, analyzeLanded(t, row, false),
		"the defect: five landed requests on a rank at 24 % KV, 9 of 256 sequences, record a k2")
	assert.False(t, analyzeLanded(t, row, true), "on: a decode with room has no backlog to record")
}

// Controls: where the replica can be at a bound, the landed requests are its
// backlog with the switch on, as before.
func TestDecodeLandedNeedsABound_ABoundReplicaStillRecords(t *testing.T) {
	for name, row := range map[string]domain.ReplicaMetrics{
		"a rank may be at its cap": landedRow(200, 0.24),
		"the busiest rank is full": landedRow(9, 0.85),
		"a request waits for capacity": func() domain.ReplicaMetrics {
			return withReason(landedRow(9, 0.24), 5)
		}(),
		"the running count is not read": func() domain.ReplicaMetrics {
			rm := landedRow(9, 0.24)
			rm.RunningRequestsKnown = false
			return rm
		}(),
		// 62 running is under the cap of 64, and its busiest rank's 24.8 is
		// over the 24.6 the budget holds (TestDecodeLandedNeedsABound_TheBudget).
		"the budget may be spent": landedRow(62, 0.24),
	} {
		t.Run(name, func(t *testing.T) {
			assert.True(t, analyzeLanded(t, row, true))
		})
	}
}

// The rule on one row: a stated cap is needed to rule the cap out.
func TestDecodeLandedNeedsABound_TheRule(t *testing.T) {
	raw := landedRow(9, 0.24)
	p := budgetParams()
	view := decodeCapacityView(raw, domain.RoleDecode, 64)
	require.Equal(t, 5, view.BusiestRankQueue(), "precondition: the view counts the landed five")
	assert.Equal(t, 0, landedWithoutBound(view, raw, domain.RoleDecode, 64, 0.8, p).BusiestRankQueue())
	assert.Equal(t, 0, landedWithoutBound(view, raw, domain.RoleDecode, 64, 0.8, p).QueueLength)
	assert.Equal(t, view, landedWithoutBound(view, raw, domain.RoleDecode, 0, 0.8, p), "no stated cap: as before")
	assert.Equal(t, view, landedWithoutBound(view, raw, domain.RolePrefill, 64, 0.8, p), "not a decode: as before")
}

// vLLM's waiting loop also stops when the step's token budget is spent. One
// rank of the pod holds 24.6 requests at its budget of 256 (landedRow): at
// 60 running its busiest rank runs 24 and is clear of it; at 62, 24.8, it may
// be at it, and the landed requests stay its backlog. Priced at the whole
// prompt (no computed rate read) the budget holds 2.2, and 9 running, 3.6 on
// the busiest rank, may be at it. With no budget stated, B is unknown and only
// the cap and the KV rule.
func TestDecodeLandedNeedsABound_TheBudget(t *testing.T) {
	p := budgetParams()
	out := func(raw domain.ReplicaMetrics, params *capacity.EngineParams) bool {
		view := decodeCapacityView(raw, domain.RoleDecode, 64)
		require.Equal(t, 5, view.BusiestRankQueue(), "precondition: the view counts the landed five")
		return landedWithoutBound(view, raw, domain.RoleDecode, 64, 0.8, params).BusiestRankQueue() == 0
	}
	steady := steadyBatch(256, 64, budgetIn, budgetOut, decodeStepBudget(landedRow(9, 0.24), domain.RoleDecode, p))
	require.InDelta(t, 24.6, steady, 0.05)
	assert.True(t, out(landedRow(60, 0.24), p), "60 running, 24 on the busiest rank: room in the budget")
	assert.False(t, out(landedRow(62, 0.24), p), "62 running, 24.8 on the busiest rank: it may have spent its budget")

	whole := landedRow(9, 0.24)
	whole.PrefillComputedTokenRateKnown = false
	assert.False(t, out(whole, p), "priced at the whole prompt the budget holds 2.2")

	unstated := budgetParams()
	unstated.MaxNumBatchedTokens, unstated.EffectiveMaxBatchedTokens = 0, 0
	assert.True(t, out(landedRow(30, 0.24), unstated), "no budget: the cap and the KV decide")
	assert.True(t, out(landedRow(30, 0.24), nil), "no params: the cap and the KV decide")

	nan := landedRow(9, 0.24)
	nan.AvgOutputTokens = math.NaN()
	assert.False(t, out(nan, p), "an unread shape is no room")
}

// The busiest rank's running count: the mean a rank, scaled by the busiest
// rank's KV over the mean, never above the sum; the sum where the mean KV is
// not read; the count on one rank.
func TestDecodeLandedNeedsABound_BusiestRankRunning(t *testing.T) {
	assert.InDelta(t, 4.0, busiestRankRunning(landedRow(10, 0.24)), 1e-9, "10 / 4 x 1.6")
	assert.InDelta(t, 2.5, busiestRankRunning(landedRow(10, 0.10)), 1e-9, "a busiest rank under the mean reads the mean")
	assert.InDelta(t, 10.0, busiestRankRunning(landedRow(10, 0.90)), 1e-9, "6x the mean: held at the sum")
	unread := landedRow(10, 0.24)
	unread.KvCacheUsage = 0
	assert.InDelta(t, 10.0, busiestRankRunning(unread), 1e-9, "no mean KV: the sum")
	noBusiest := landedRow(10, 0.24)
	noBusiest.MaxRankKvCacheUsage = 0
	assert.InDelta(t, 10.0, busiestRankRunning(noBusiest), 1e-9, "no busiest rank's KV: the sum, not the mean")
	one := landedRow(10, 0.24)
	one.EngineRanks = 1
	assert.InDelta(t, 10.0, busiestRankRunning(one), 1e-9, "one rank: the count")
}

// The cap is one rank's; the running count is the pod's, summed over its
// four ranks. With no budget stated only the cap and the KV decide. 80
// running, 32 on the busiest rank (80 / 4 x 1.6), is under a cap of 64: no
// rank is at it, and the landed requests leave the queue. Read against the
// sum, 80 was over 64. 200 running, 80 on the busiest rank, may be at it.
func TestDecodeLandedNeedsABound_TheCapIsOneRanks(t *testing.T) {
	unstated := budgetParams()
	unstated.MaxNumBatchedTokens, unstated.EffectiveMaxBatchedTokens = 0, 0
	out := func(raw domain.ReplicaMetrics) bool {
		view := decodeCapacityView(raw, domain.RoleDecode, 64)
		require.Equal(t, 5, view.BusiestRankQueue(), "precondition: the view counts the landed five")
		return landedWithoutBound(view, raw, domain.RoleDecode, 64, 0.8, unstated).BusiestRankQueue() == 0
	}
	require.InDelta(t, 32, busiestRankRunning(landedRow(80, 0.24)), 1e-9)
	assert.True(t, out(landedRow(80, 0.24)), "80 over four ranks, 32 on the busiest: under one rank's cap")
	assert.False(t, out(landedRow(200, 0.24)), "80 on the busiest rank: it may be at its cap")
	one := landedRow(80, 0.24)
	one.EngineRanks = 1
	assert.False(t, out(one), "one rank running 80: over its cap")
}

func TestDecodeBudgetSwitches_OffFromTheConstructor(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	assert.False(t, a.k2DecodeBudget)
	assert.False(t, a.decodeLandedNeedsBound)
}
