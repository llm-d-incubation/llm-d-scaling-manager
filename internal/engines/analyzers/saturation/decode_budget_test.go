package saturation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// Issue llm-d-incubation/llm-d-scaling-manager#201, the first reading. The
// shape: a P/D decode pod of four data-parallel ranks, 8M KV tokens each,
// --max-num-batched-tokens 256 and --max-num-seqs 64 per rank, MTP with three
// draft tokens, long agentic prompts (80,000 tokens) and short outputs (700).
// Its KV arrives from prefill; it computes 1,600 prompt tokens a request
// itself. Charged the whole prompt against its 256-token budget, the derived
// k2 holds 2.2 requests per rank, and the pod is priced at 713,688 tokens --
// a fortieth of its cache -- so a fleet at 10 % KV reads several times over
// capacity.

const (
	budgetRanks      = 4
	budgetKVPerRank  = int64(8_000_000)
	budgetIn         = 80_000.0
	budgetOut        = 700.0
	budgetPromptRate = 3_200.0 // computed prompt tokens/s at 2 req/s: 1,600 a request
	budgetReqRate    = 2.0
	budgetAccepted   = 1.8
)

// budgetParams is the decode's flags: B = 256 and S = 64 per rank, MTP with
// three draft tokens.
func budgetParams() *capacity.EngineParams {
	return &capacity.EngineParams{
		Engine:                    inferenceengine.EngineVLLM,
		BlockSize:                 64,
		MaxNumSeqs:                64,
		MaxNumSeqsSet:             true,
		MaxNumBatchedTokens:       256,
		EffectiveMaxBatchedTokens: 256,
		DataParallelSize:          budgetRanks,
		SpeculativeConfig:         `{"method":"mtp","num_speculative_tokens":3}`,
	}
}

// budgetRow is the decode pod at 10 % of its cache, nothing waiting.
func budgetRow() domain.ReplicaMetrics {
	total := budgetRanks * budgetKVPerRank
	rm := makeReplicaMetrics("decode-0", decodeVariant, total/10, total, 0, budgetIn, budgetOut)
	rm.EngineRanks = budgetRanks
	rm.RequestRate, rm.RequestRateKnown = budgetReqRate, true
	rm.PrefillComputedTokenRate, rm.PrefillComputedTokenRateKnown = budgetPromptRate, true
	rm.SpecAcceptedPerStep, rm.SpecAcceptedPerStepKnown = budgetAccepted, true
	rm.RunningRequests, rm.RunningRequestsKnown = 40, true
	return rm
}

// priceBudget prices rm as a replica of role with the switch on or off.
func priceBudget(t *testing.T, rm domain.ReplicaMetrics, role string, on bool) *capacity.ReplicaCapacity {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", rm.VariantName, capacity.Record{EngineParams: budgetParams()}, time.Now())
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchK2DecodeBudgetAtComputedPrompt: on}))
	cfg := &config.ScalingPolicy{KvCacheThreshold: 0.8, ScaleUpThreshold: 0.85, QueueLengthThreshold: 5}
	rc := a.computeReplicaCapacity(rm, cfg, tuningOf(cfg), "test-model", "test-ns", 32, role, "H100",
		outputKeys{k2: budgetOut, mu: budgetOut}, budgetOut, budgetIn, 0, whatif.DerivedMu{}, servedRates{}, false, false, replicaRouting{}, false, residentKVLine{}, logr.Discard())
	require.NotNil(t, rc)
	return rc
}

func TestK2DecodeBudget_ADecodeIsNotChargedThePromptItDidNotCompute(t *testing.T) {
	// Off: N = 256 x 700 / 80,700 = 2.22 a rank, at 80,000 + 700/2 each:
	// 178,422 a rank, 713,688 a pod.
	off := priceBudget(t, budgetRow(), domain.RoleDecode, false)
	require.Equal(t, capacity.K2SrcDerived, off.K2Priority)
	assert.Equal(t, int64(713_688), off.EffectiveCapacity, "the defect: a fortieth of the pod's cache")

	// On: a request is resident 700 / 2.8 = 250 steps and takes 1,600 + 4 x
	// 250 = 2,600 tokens of budget, so N = 256 x 250 / 2,600 = 24.6 a rank:
	// 1,977,846 a rank, 7,911,384 a pod.
	on := priceBudget(t, budgetRow(), domain.RoleDecode, true)
	require.Equal(t, capacity.K2SrcDerived, on.K2Priority)
	assert.Equal(t, int64(7_911_384), on.EffectiveCapacity)

	// What the controller asks for on eleven such pods at the pod's demand:
	// ceil(D / (0.85 x P)) once over the fleet. Off, the 3.2M resident a pod
	// reads as five replicas each; on, under one.
	demand := float64(11 * budgetRow().TokensInUse)
	assert.Equal(t, 59, ceilReplicas(demand, 0.85, off.EffectiveCapacity), "off: 59 asked for 11 at 10 % KV")
	assert.Equal(t, 6, ceilReplicas(demand, 0.85, on.EffectiveCapacity), "on: 6")
}

// ceilReplicas is the replicas the optimizer sizes a role to on demand D at
// the scale-up threshold: ceil(D / (scaleUp x P)).
func ceilReplicas(demand, scaleUp float64, perReplica int64) int {
	n := demand / (scaleUp * float64(perReplica))
	c := int(n)
	if float64(c) < n {
		c++
	}
	return c
}

func TestK2DecodeBudget_SpeculationIsPricedOnlyWhereItIsRead(t *testing.T) {
	// No acceptance read: a step takes one token and emits one, so N = 256 x
	// 700 / (1,600 + 700) = 77.9, capped at 64: 64 x 80,350 x 4.
	rm := budgetRow()
	rm.SpecAcceptedPerStep, rm.SpecAcceptedPerStepKnown = 0, false
	assert.Equal(t, int64(64*80_350*budgetRanks), priceBudget(t, rm, domain.RoleDecode, true).EffectiveCapacity)
}

func TestK2DecodeBudget_NoComputedPromptIsPricedAsBefore(t *testing.T) {
	off := priceBudget(t, budgetRow(), domain.RoleDecode, false).EffectiveCapacity
	for name, mutate := range map[string]func(*domain.ReplicaMetrics){
		"no computed prompt": func(rm *domain.ReplicaMetrics) { rm.PrefillComputedTokenRateKnown = false },
		"no completions":     func(rm *domain.ReplicaMetrics) { rm.RequestRate = 0 },
		"no request rate":    func(rm *domain.ReplicaMetrics) { rm.RequestRateKnown = false },
	} {
		t.Run(name, func(t *testing.T) {
			rm := budgetRow()
			mutate(&rm)
			assert.Equal(t, off, priceBudget(t, rm, domain.RoleDecode, true).EffectiveCapacity)
		})
	}
}

// Control: an aggregated replica prefills its prompts, and is priced as
// before with the switch on.
func TestK2DecodeBudget_AnAggregatedReplicaIsPricedAsBefore(t *testing.T) {
	assert.Equal(t,
		priceBudget(t, budgetRow(), domain.RoleBoth, false).EffectiveCapacity,
		priceBudget(t, budgetRow(), domain.RoleBoth, true).EffectiveCapacity)
}

func TestK2DecodeBudget_TheComputedPromptIsHeldToThePrompt(t *testing.T) {
	rm := budgetRow()
	rm.PrefillComputedTokenRate = 10 * budgetIn * budgetReqRate
	b := decodeStepBudget(rm, domain.RoleDecode, budgetParams())
	assert.Equal(t, budgetIn, b.prompt)
}

// Through Analyze, on a P/D model: the decode variant's PerReplicaCapacity.
func TestK2DecodeBudget_ThroughAnalyze(t *testing.T) {
	states := []domain.VariantReplicaState{
		{VariantName: decodeVariant, Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 32},
		{VariantName: pdPrefillVariant, Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 32},
	}
	run := func(on bool) float64 {
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", decodeVariant, capacity.Record{
			GpuCount: 32, EngineParams: budgetParams(), LearnedFrom: "deployment",
		}, time.Now())
		a := NewSaturationAnalyzer(store)
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchK2DecodeBudgetAtComputedPrompt: on}))
		clock := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
		a.now = func() time.Time { return clock }
		in := makeAnalyzerInput([]domain.ReplicaMetrics{budgetRow(), pdPrefillRow()}, states)
		var res *domain.AnalyzerResult
		for range 3 {
			var err error
			res, err = a.Analyze(context.Background(), in)
			require.NoError(t, err)
			clock = clock.Add(15 * time.Second)
		}
		for _, vc := range res.VariantCapacities {
			if vc.VariantName == decodeVariant {
				require.True(t, strings.HasPrefix(vc.Reason, "P3"), "the derived k2 prices it: %s", vc.Reason)
				return vc.PerReplicaCapacity
			}
		}
		t.Fatal("no decode capacity")
		return 0
	}
	assert.Equal(t, 713_688.0, run(false))
	assert.Equal(t, 7_911_384.0, run(true))
}
