package learnreplay

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// noteStepTokens adds a cycle's rows' tokens-per-iteration histograms into steps,
// by variant (VariantState.StepTokens). A row with no series adds nothing,
// so a recording made before the series was recorded leaves steps empty.
func noteStepTokens(steps map[string][]domain.LengthBucket, rows []domain.ReplicaMetrics) {
	for _, rm := range rows {
		if len(rm.IterationTokensBuckets) == 0 {
			continue
		}
		steps[rm.VariantName] = capacity.PoolStepTokens(steps[rm.VariantName], rm.IterationTokensBuckets)
	}
}

// StepBudgetBinding is whether the variant's step budget binds: its engine
// parameters' budget (capacity.EngineParams.StepBudget) against its pooled
// iterations (StepTokens). It is the engine class the queue model's prior (issue
// llm-d-incubation/llm-d-scaling-manager#387) and the aggregated TTFT model
// key on; no figure reads it yet.
func (v *VariantState) StepBudgetBinding() capacity.StepBudgetBinding {
	return v.Params.StepBudgetBinding(v.StepTokens)
}
