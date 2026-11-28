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
//
// With prompts (saturation.SwitchStepBudgetPromptReading), a row also adds
// its computed-prompt histogram (capacity.StepTokens.AddPrompts), over the
// same [1m] window. vLLM observes that histogram once per request, when it
// finishes, not when its prefill ran: a minute's prompts are those that
// finished in it, and a request that was aborted is not counted. Pooled over
// the whole replay that is the same set of prompts, a few seconds late. A row
// with prompts and no iteration series pools its prompts, but the class is
// still reported only where an iteration series was read
// (cmd/capacity-planner, stepBudgetBindingOf).
func noteStepTokens(steps map[string]*capacity.StepTokens, rows []domain.ReplicaMetrics, since time.Duration, prompts bool) {
	seconds := min(since, stepTokensWindow).Seconds()
	for _, rm := range rows {
		if len(rm.IterationTokensBuckets) == 0 && (!prompts || len(rm.ComputedTokensBuckets) == 0) {
			continue
		}
		s := steps[rm.VariantName]
		if s == nil {
			s = &capacity.StepTokens{}
			steps[rm.VariantName] = s
		}
		s.Add(rm.IterationTokensBuckets, seconds)
		if prompts {
			s.AddPrompts(rm.ComputedTokensBuckets, seconds)
		}
	}
}

// StepBudgetBinding is whether the variant's step budget binds: its engine
// parameters' budget (capacity.EngineParams.StepBudget) against its pooled
// iterations (StepTokens). It is the engine class the queue model's prior
// (issue llm-d-incubation/llm-d-scaling-manager#387) and the aggregated TTFT
// model will key on; no figure reads it yet.
//
// With saturation.SwitchStepBudgetPromptReading on in the replay, it adds the
// reading of capacity.BindingOptions: the computed prompts above the budget.
func (v *VariantState) StepBudgetBinding() capacity.StepBudgetBinding {
	if !v.stepBudgetPrompts {
		return v.Params.StepBudgetBinding(v.StepTokens)
	}
	return v.StepTokens.BindingWith(v.Params.StepBudget(), capacity.BindingOptions{Prompts: true})
}
