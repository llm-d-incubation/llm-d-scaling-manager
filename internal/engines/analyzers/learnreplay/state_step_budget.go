package learnreplay

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// stepTokensWindow is the window the collector takes the tokens-per-iteration
// rates over (registration.TTFTInputWindow, [1m]): the most seconds one row
// stands for, and what the first cycle's rows stand for.
const stepTokensWindow = time.Minute

// noteStepTokens adds a cycle's rows' tokens-per-iteration histograms into
// steps, by variant (VariantState.StepTokens), each standing for since, the
// time since the cycle before (stepTokensWindow at the first), at most
// stepTokensWindow. A row with no series adds nothing, so a recording made
// before the series was recorded leaves steps empty.
func noteStepTokens(steps map[string]*capacity.StepTokens, rows []domain.ReplicaMetrics, since time.Duration) {
	seconds := min(since, stepTokensWindow).Seconds()
	for _, rm := range rows {
		if len(rm.IterationTokensBuckets) == 0 {
			continue
		}
		s := steps[rm.VariantName]
		if s == nil {
			s = &capacity.StepTokens{}
			steps[rm.VariantName] = s
		}
		s.Add(rm.IterationTokensBuckets, seconds)
	}
}

// StepBudgetBinding is whether the variant's step budget binds: its engine
// parameters' budget (capacity.EngineParams.StepBudget) against its pooled
// iterations (StepTokens). It is the engine class the queue model's prior
// (issue llm-d-incubation/llm-d-scaling-manager#387) and the aggregated TTFT
// model will key on; no figure reads it yet.
func (v *VariantState) StepBudgetBinding() capacity.StepBudgetBinding {
	return v.Params.StepBudgetBinding(v.StepTokens)
}
