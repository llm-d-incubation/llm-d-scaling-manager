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
// With full (saturation.SwitchStepBudgetFullReading), a row also adds its
// computed-prompt histogram (capacity.StepTokens.AddPrompts), over the same
// [1m] window, and, beside its iterations, its accepted draft tokens per step
// (capacity.StepTokens.AddAcceptance). A row with prompts and no iteration
// series pools its prompts, but the class is still reported only where an
// iteration series was read (cmd/capacity-planner, stepBudgetBindingOf).
func noteStepTokens(steps map[string]*capacity.StepTokens, rows []domain.ReplicaMetrics, since time.Duration, full bool) {
	seconds := min(since, stepTokensWindow).Seconds()
	for _, rm := range rows {
		prompts := full && len(rm.ComputedTokensBuckets) > 0
		if len(rm.IterationTokensBuckets) == 0 && !prompts {
			continue
		}
		s := steps[rm.VariantName]
		if s == nil {
			s = &capacity.StepTokens{}
			steps[rm.VariantName] = s
		}
		s.Add(rm.IterationTokensBuckets, seconds)
		if !full {
			continue
		}
		s.AddPrompts(rm.ComputedTokensBuckets, seconds)
		if rm.SpecAcceptedPerStepKnown {
			s.AddAcceptance(rm.IterationTokensBuckets, rm.SpecAcceptedPerStep, seconds)
		}
	}
}

// StepBudgetBinding is whether the variant's step budget binds: its engine
// parameters' budget (capacity.EngineParams.StepBudget) against its pooled
// iterations (StepTokens). It is the engine class the queue model's prior
// (issue llm-d-incubation/llm-d-scaling-manager#387) and the aggregated TTFT
// model will key on; no figure reads it yet.
//
// With saturation.SwitchStepBudgetFullReading on in the replay, it adds the
// readings of capacity.BindingOptions: the computed prompts above the budget,
// and, on a decode variant whose flags set num_speculative_tokens, its
// iterations read in emitted tokens.
func (v *VariantState) StepBudgetBinding() capacity.StepBudgetBinding {
	if !v.stepBudgetFull {
		return v.Params.StepBudgetBinding(v.StepTokens)
	}
	o := capacity.BindingOptions{Prompts: true}
	if domain.CanonicalRole(v.Role) == domain.RoleDecode {
		o.SpeculativeTokens = v.Params.SpeculativeTokens()
	}
	return v.StepTokens.BindingWith(v.Params.StepBudget(), o)
}
