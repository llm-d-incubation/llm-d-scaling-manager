package learnreplay

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// stepBudgetRecording is a recording of one aggregated variant "v" on two
// pods with a step budget of 8192, its rows carrying steps, the
// tokens-per-step histogram each pod read (nil: the series absent, as in a
// recording made before it was recorded).
func stepBudgetRecording(t *testing.T, steps []domain.LengthBucket) []byte {
	t.Helper()
	f := newFixture(replayAt)
	f.buf.Reset()
	rec := learnrec.New(f.buf, 0)
	f.attach(rec)
	rec.Record(learnrec.KindStart, replayAt, defaultStart())
	params := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 256, MaxNumBatchedTokens: 8192,
		ChunkedPrefillEnabled: true, Image: "img:1"}
	f.store.FileScaleTarget("ns", "m", "v", "H200", 1, params, false, replayAt)
	states := []domain.VariantReplicaState{
		{VariantName: "v", Role: domain.RoleBoth, AcceleratorName: "H200", GPUsPerReplica: 1, CurrentReplicas: 2}}
	now := replayAt
	for range 6 {
		f.clk.SetTime(now)
		rows := make([]domain.ReplicaMetrics, 0, 2)
		for i := range 2 {
			rows = append(rows, domain.ReplicaMetrics{PodName: fmt.Sprintf("v-%d", i), VariantName: "v", Ready: true,
				ReadySince: replayAt.Add(-time.Hour), KvCacheUsage: 0.4, KvCacheUsageKnown: true, KvUsageInstant: 0.4,
				KvUsageInstantKnown: true, AvgITL: 0.03, AvgITLKnown: true, TotalKvCapacityTokens: 100000,
				TokensInUse: 40000, AvgInputTokens: 1000, AvgOutputTokens: 500, RequestRate: 2, RequestRateKnown: true,
				IterationTokensBuckets: steps})
		}
		_, err := f.sat.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns",
			Config:        &config.ScalingPolicy{KvCacheThreshold: 0.8, ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.7},
			VariantStates: states, ReplicaMetrics: rows})
		require.NoError(t, err)
		now = now.Add(15 * time.Second)
	}
	return bytes.Clone(f.buf.Bytes())
}

// cumulativeSteps is a cumulative histogram on vLLM's default bounds from
// the steps a second in each bucket, lowest first.
func cumulativeSteps(per ...float64) []domain.LengthBucket {
	bounds := []float64{1, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, math.Inf(1)}
	out := make([]domain.LengthBucket, len(bounds))
	running := 0.0
	for i, b := range bounds {
		if i < len(per) {
			running += per[i]
		}
		out[i] = domain.LengthBucket{UpperBound: b, Rate: running}
	}
	return out
}

// The replay reads the class of the variant's step budget from the steps its
// rows recorded, pooled over its pods and cycles, against the budget its
// engine arguments set: binds, does not bind, and unknown where the
// recording carries no series.
func TestStateAt_StepBudgetBindingFromTheRecording(t *testing.T) {
	for _, c := range []struct {
		name   string
		steps  []domain.LengthBucket
		class  capacity.StepBudgetClass
		reason string
		share  float64
	}{
		{"binds", cumulativeSteps(0, 0, 0, 0, 10, 20, 10, 5, 5, 10, 10, 30), capacity.StepBudgetBinds, "", 0.3},
		{"does not bind", cumulativeSteps(0, 0, 0, 0, 40, 30, 15, 8, 4, 2, 0.76, 0.2, 0.03, 0.01),
			capacity.StepBudgetDoesNotBind, "", 0.0024},
		{"no series", nil, capacity.StepBudgetUnknown, capacity.StepBudgetNoSeries, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, err := StateAt(context.Background(), stepBudgetRecording(t, c.steps), Options{}, StateQuery{})
			require.NoError(t, err)
			require.Len(t, st.Variants, 1)
			v := st.Variants[0]
			got := v.StepBudgetBinding()
			assert.Equal(t, c.class, got.Class)
			assert.Equal(t, c.reason, got.Reason)
			assert.InDelta(t, c.share, got.Share, 1e-9)
			assert.Equal(t, int64(8192), got.Budget)
			if c.steps != nil {
				// 2 pods for 6 cycles: the pooled rate is 12 times a row's.
				assert.InDelta(t, 12*c.steps[len(c.steps)-1].Rate, got.StepRate, 1e-9)
			} else {
				assert.Nil(t, v.StepTokens)
			}
		})
	}
}

// The early states a replay reads in the same pass (StatesFrom) pool the
// steps of their own cycles only.
func TestStatesFrom_StepTokensUpToTheCycle(t *testing.T) {
	steps := cumulativeSteps(0, 0, 0, 0, 10, 20, 10, 5, 5, 10, 10, 30)
	last, early, err := StatesFrom(context.Background(), BytesSource(stepBudgetRecording(t, steps)), Options{}, StateQuery{},
		func(int) []int { return []int{2} })
	require.NoError(t, err)
	require.Contains(t, early, 2)
	assert.InDelta(t, 4*100.0, early[2].Variants[0].StepBudgetBinding().StepRate, 1e-9, "2 pods for 2 cycles")
	assert.InDelta(t, 12*100.0, last.Variants[0].StepBudgetBinding().StepRate, 1e-9, "2 pods for 6 cycles")
}
