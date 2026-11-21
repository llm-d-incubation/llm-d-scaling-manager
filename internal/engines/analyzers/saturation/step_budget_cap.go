package saturation

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// SwitchMuAtStepBudget is the replay switch for the batch the derived mu is
// priced at (issue llm-d-incubation/llm-d-scaling-manager#302).
//
// On, the cap the derived mu prices one vLLM engine rank at is held to what
// its step token budget runs: ceil(budget / (1 + k)) sequences with k
// speculative tokens (whatif.StepBudgetCap). vLLM's scheduler takes 1 + k
// tokens of the budget per running request until it is spent, so a rank
// priced at its max_num_seqs above that batch is priced at a
// throughput it cannot run. Off, as before, at the batch cap alone.
//
// Off from the constructor. The expected effect on: nothing on a dense engine
// with the default budget (8192 tokens against a cap of 256 or 1024, k up to
// 7); a wide-EP decode with the batched all-to-all's budget of 256 and MTP
// k = 3 is priced at 64 sequences a rank where it was priced at 256, so its
// derived mu, and the capacity it sells, falls up to four times (two times at
// k = 1) where the KV would have held the larger batch. That is the safe
// direction, but no recording holds an MTP or a wide-EP engine to check it
// on: the evidence is synthetic (the unit tests, from the scheduler's rule).
const SwitchMuAtStepBudget = "mu-at-step-budget"

// SwitchPlannerCapAtStepBudget is SwitchMuAtStepBudget for the capacity
// planner: the batch cap per rank the planner reads (PricingBatchCap), and so
// its decode knee, held to the step budget's batch. Planner-only and off from
// the constructor; the planner's replay turns it on
// (learnreplay.PlannerSwitches): it only lowers the cap, and moves no
// committed recording, whose budgets all run their caps.
const SwitchPlannerCapAtStepBudget = "planner-cap-at-step-budget"

// atStepBudget is p held to the batch params' step budget runs, with its
// source batchCapStepBudget where that lowers it; p itself with on false, on
// an engine other than vLLM (SGLang's chunked prefill size is not a decode
// step budget), where the budget is unknown, or where p is no cap (a zero
// perRank stays zero: the budget alone is not a cap any reader prices).
//
// k is num_speculative_tokens as the flags state it; an MTP engine that
// leaves it to the model's default reads k = 0 and is held to the budget
// alone, the lenient side.
func atStepBudget(p pricedBatchCap, params *capacity.EngineParams, on bool) pricedBatchCap {
	if !on || params == nil || params.Engine == inferenceengine.EngineSGLang {
		return p
	}
	// The resolved budget, as every other reader of it (steadyBatch, the
	// k2 ITL bound): 0, unknown, caps nothing.
	capped := whatif.StepBudgetCap(p.perRank, params.EffectiveMaxBatchedTokens, params.SpeculativeTokens())
	if p.perRank > 0 && capped < p.perRank {
		p.perRank, p.source = capped, batchCapStepBudget
	}
	return p
}
