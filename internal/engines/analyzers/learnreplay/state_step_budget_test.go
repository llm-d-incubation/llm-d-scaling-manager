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
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
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
	return stepBudgetRecordingWith(t, steps, nil)
}

// stepBudgetRecordingWith is stepBudgetRecording with each row carrying
// computed, its computed-prompt histogram (nil: absent).
func stepBudgetRecordingWith(t *testing.T, steps, computed []domain.LengthBucket) []byte {
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
				IterationTokensBuckets: steps, ComputedTokensBuckets: computed})
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
		over   float64
	}{
		{"binds", cumulativeSteps(0, 0, 0, 0, 10, 20, 10, 5, 5, 10, 10, 20, 6, 4), capacity.StepBudgetBinds, "", 0.1},
		// Iterations near the budget, none above it: nothing was chunked.
		{"near the budget, none above", cumulativeSteps(0, 0, 0, 0, 5, 5, 5, 5, 5, 5, 10, 60),
			capacity.StepBudgetDoesNotBind, "", 0},
		{"does not bind", cumulativeSteps(0, 0, 0, 0, 40, 30, 15, 8, 4, 2, 0.76, 0.2, 0.03, 0.01),
			capacity.StepBudgetDoesNotBind, "", 0.0004},
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
			assert.InDelta(t, c.over, got.OverShare, 1e-9)
			assert.Equal(t, int64(8192), got.Budget)
			if c.steps != nil {
				// 2 pods for 6 cycles 15 s apart, the first standing for the
				// minute its rate is taken over: 2 x (60 + 5 x 15) seconds.
				assert.Equal(t, 12, v.StepTokens.Samples)
				assert.InDelta(t, 270*c.steps[len(c.steps)-1].Rate, got.Iterations, 1e-6)
			} else {
				assert.Zero(t, v.StepTokens.Samples)
			}
		})
	}
}

// The early states a replay reads in the same pass (StatesFrom) pool the
// steps of their own cycles only.
func TestStatesFrom_StepTokensUpToTheCycle(t *testing.T) {
	steps := cumulativeSteps(0, 0, 0, 0, 10, 20, 10, 5, 5, 10, 10, 20, 6, 4)
	last, early, err := StatesFrom(context.Background(), BytesSource(stepBudgetRecording(t, steps)), Options{}, StateQuery{},
		func(int) []int { return []int{2} })
	require.NoError(t, err)
	require.Contains(t, early, 2)
	assert.Equal(t, 4, early[2].Variants[0].StepTokens.Samples, "2 pods for 2 cycles")
	assert.Equal(t, 12, last.Variants[0].StepTokens.Samples, "2 pods for 6 cycles")
	assert.InDelta(t, 2*(60+15)*100.0, early[2].Variants[0].StepTokens.Buckets[len(steps)-1].Rate, 1e-6)
	assert.InDelta(t, 2*(60+5*15)*100.0, last.Variants[0].StepTokens.Buckets[len(steps)-1].Rate, 1e-6)
}

// computedPrompts is a cumulative computed-prompt histogram on vLLM's 1-2-5
// bounds (1 to 20000, then +Inf) from the prompts a second in each bucket,
// lowest first.
func computedPrompts(per ...float64) []domain.LengthBucket {
	bounds := []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 20000, math.Inf(1)}
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

// fullReadingOff is the replay's switches with
// saturation.SwitchStepBudgetFullReading off, as the controller's
// constructor has it.
func fullReadingOff() Options {
	return Options{Switches: map[string]map[string]bool{PackageSaturation: {saturation.SwitchStepBudgetFullReading: false}}}
}

// On an aggregated variant whose prompts are half longer than the budget,
// the iterations above it are diluted by the decode steps: 0.5% of them, and
// the iteration share alone reads does not bind. The planner's replay
// (saturation.SwitchStepBudgetFullReading on in PlannerSwitches) reads the
// computed prompts too, and the budget binds; off, the class is as before.
func TestStateAt_StepBudgetBindsOnPromptsOverTheBudget(t *testing.T) {
	steps := cumulativeSteps(0, 0, 0, 0, 0, 990, 0, 0, 0, 0, 5, 0, 5)
	prompts := computedPrompts(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 1)
	rec := stepBudgetRecordingWith(t, steps, prompts)

	st, err := StateAt(context.Background(), rec, Options{}, StateQuery{})
	require.NoError(t, err)
	got := st.Variants[0].StepBudgetBinding()
	assert.Equal(t, capacity.StepBudgetBinds, got.Class)
	assert.InDelta(t, 0.005, got.OverShare, 1e-9)
	assert.InDelta(t, 0.5, got.PromptOverShare, 1e-9)
	assert.InDelta(t, 10000.0, got.PromptBoundTokens, 1e-9)
	// 2 pods for 2 x (60 + 5 x 15) seconds at 2 prompts a second.
	assert.InDelta(t, 540, got.Prompts, 1e-6)

	off, err := StateAt(context.Background(), rec, fullReadingOff(), StateQuery{})
	require.NoError(t, err)
	old := off.Variants[0].StepBudgetBinding()
	assert.Equal(t, capacity.StepBudgetDoesNotBind, old.Class)
	assert.Zero(t, old.PromptOverShare)
	assert.Empty(t, off.Variants[0].StepTokens.Prompts, "off, the prompts are not pooled")
}

// A recording whose rows carry no computed-prompt histogram, as every one
// made before it was recorded, reads the same class with the full reading
// on as off.
func TestStateAt_StepBudgetWithoutPromptsIsUnchanged(t *testing.T) {
	for _, steps := range [][]domain.LengthBucket{
		cumulativeSteps(0, 0, 0, 0, 10, 20, 10, 5, 5, 10, 10, 20, 6, 4),
		cumulativeSteps(0, 0, 0, 0, 40, 30, 15, 8, 4, 2, 0.76, 0.2, 0.03, 0.01),
		nil,
	} {
		rec := stepBudgetRecording(t, steps)
		on, err := StateAt(context.Background(), rec, Options{}, StateQuery{})
		require.NoError(t, err)
		off, err := StateAt(context.Background(), rec, fullReadingOff(), StateQuery{})
		require.NoError(t, err)
		assert.Equal(t, off.Variants[0].StepBudgetBinding(), on.Variants[0].StepBudgetBinding())
		assert.Equal(t, off.Variants[0].StepTokens, on.Variants[0].StepTokens)
	}
}

// A decode variant that speculates (k = 3) reads its iterations against the
// budget in emitted tokens at its pooled acceptance; an aggregated one, whose
// iterations mix prompt tokens with emitted ones, reads the budget as it
// stands.
func TestVariantState_StepBudgetSpeculatingDecode(t *testing.T) {
	params := &capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumBatchedTokens: 8192,
		SpeculativeConfig: `{"method":"mtp","num_speculative_tokens":3}`}
	steps := cumulativeSteps(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 900, 100)
	var pool capacity.StepTokens
	pool.Add(steps, 1)
	pool.AddAcceptance(steps, 1, 1)

	decode := VariantState{Role: domain.RoleDecode, Params: params, StepTokens: pool, stepBudgetFull: true}
	got := decode.StepBudgetBinding()
	assert.Equal(t, capacity.StepBudgetBinds, got.Class)
	assert.InDelta(t, 4096, got.EmittedBudget, 1e-9)

	both := decode
	both.Role = domain.RoleBoth
	assert.Equal(t, capacity.StepBudgetDoesNotBind, both.StepBudgetBinding().Class)
	assert.Zero(t, both.StepBudgetBinding().EmittedBudget)

	off := decode
	off.stepBudgetFull = false
	assert.Equal(t, params.StepBudgetBinding(pool), off.StepBudgetBinding())
	assert.Equal(t, capacity.StepBudgetDoesNotBind, off.StepBudgetBinding().Class)
}
