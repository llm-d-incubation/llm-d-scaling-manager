package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The fixtures below are synthetic: no recording holds an MTP or a wide-EP
// engine. They follow vLLM's Scheduler.schedule(): each running request takes
// 1 + num_speculative_tokens of max_num_batched_tokens a step, and the rest of
// the running requests wait once the budget is spent.

// wideEPMTP is a wide-EP decode rank with the batched all-to-all's default
// budget of 256 tokens and MTP k = 3, under the parser's cap of 256.
func wideEPMTP() *capacity.EngineParams {
	p := deploymentParams()
	p.EffectiveMaxBatchedTokens = 256
	p.SpeculativeConfig = `{"method":"mtp","num_speculative_tokens":3}`
	return p
}

// Both switches are off in a new analyzer: the controller's mu is as before.
func TestStepBudgetCap_OffFromTheConstructor(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	assert.False(t, a.muAtStepBudget)
	assert.False(t, a.plannerCapAtStepBudget)
	require.Contains(t, ReplaySwitchNames(), SwitchMuAtStepBudget)
	require.Contains(t, ReplaySwitchNames(), SwitchPlannerCapAtStepBudget)
}

// At MTP k = 3 and a budget of 256, the wide-EP default: 64 sequences on, a
// quarter of the mu off; at k = 1, 128.
func TestStepBudgetCap_DerivedMuAtSpeculativeTokens(t *testing.T) {
	on := func(a *SaturationAnalyzer) { a.muAtStepBudget = true }
	off := stepDerivedMu(t, wideEPMTP(), nil)
	assert.InDelta(t, 256, off["seqs"], 1e-9)
	got := stepDerivedMu(t, wideEPMTP(), on)
	assert.InDelta(t, 64, got["seqs"], 1e-9)
	assert.Equal(t, batchCapStepBudget, got["batchCapSource"])
	assert.InDelta(t, off["rate"].(float64)/4, got["rate"], 1e-9)

	k1 := wideEPMTP()
	k1.SpeculativeConfig = `{"num_speculative_tokens":1}`
	assert.InDelta(t, 128, stepDerivedMu(t, k1, on)["seqs"], 1e-9)
}

// stepDerivedMu is derivedMuSeqs with the line published as one fitted on
// an engine that speculates per step where params' does, so a speculative
// engine borrows it.
func stepDerivedMu(t *testing.T, params *capacity.EngineParams, edit func(*SaturationAnalyzer)) map[string]any {
	t.Helper()
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		AcceleratorName: "H200", GpuCount: 1, EngineParams: params,
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	a.publishLineFor(a.itlPhysicsKey("test-ns", "test-model", "variant-d", "H200", 1, ""),
		"a fitted line", itl.Model{A: 0.08, B: 0.01}, time.Now(), params.SpeculatesPerStep())
	if edit != nil {
		edit(a)
	}
	rm := makeReplicaMetrics("pod-1", "variant-d", 5000, 100_000, 0, 100, 50)
	in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H200",
			CurrentReplicas: 1, GPUsPerReplica: 1},
	})
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	fields := requireLogged(t, logs, "derived-mu")
	require.Equal(t, true, fields["ok"], "the line prices mu: %v", fields)
	return fields
}

// The switch moves nothing where the budget holds the cap: a dense engine at
// 8192 tokens a step against a cap of 256.
func TestStepBudgetCap_DenseEngineUnmoved(t *testing.T) {
	dense := deploymentParams()
	on := stepDerivedMu(t, dense, func(a *SaturationAnalyzer) { a.muAtStepBudget = true })
	off := stepDerivedMu(t, dense, nil)
	assert.Equal(t, off["seqs"], on["seqs"])
	assert.Equal(t, off["batchCapSource"], on["batchCapSource"])
}

// The planner's read of the cap: held to the budget's batch on, the cap
// alone off; SGLang, whose chunked prefill size is no decode step budget, and
// an unknown budget are never held.
func TestStepBudgetCap_PlannerCapAtTheBudgetsBatch(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	perRank, source := a.PricingBatchCap("ns", "m", "v", wideEPMTP(), false)
	assert.EqualValues(t, 256, perRank)
	assert.Equal(t, batchCapDefault, source)

	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchPlannerCapAtStepBudget: true}))
	perRank, source = a.PricingBatchCap("ns", "m", "v", wideEPMTP(), false)
	assert.EqualValues(t, 64, perRank)
	assert.Equal(t, batchCapStepBudget, source)

	sg := wideEPMTP()
	sg.Engine = inferenceengine.EngineSGLang
	perRank, _ = a.PricingBatchCap("ns", "m", "v", sg, false)
	assert.EqualValues(t, 256, perRank)

	unknown := wideEPMTP()
	unknown.EffectiveMaxBatchedTokens = 0
	perRank, _ = a.PricingBatchCap("ns", "m", "v", unknown, false)
	assert.EqualValues(t, 256, perRank)
}
