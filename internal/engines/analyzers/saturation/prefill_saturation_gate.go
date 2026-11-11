package saturation

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Prefill saturated without an engine queue (llm-d-incubation/llm-d-scaling-manager#51).
//
// The saturation gate is the waiting queue: a replica's reading is evidence
// of its limit (a k2 reading, a mu sample, a prefill-bound sample) only when
// its busiest rank holds queueLengthThreshold requests waiting with tokens
// resident. A prefill engine with a large per-step token budget admits every
// prompt it is handed into its running batch, so on long prompts its waiting
// queue reads zero until the fleet runs away: at --max-num-batched-tokens
// 65536 and 8000-token prompts, about eight prompts run at once and none
// waits. Under the router's flow control the backlog sits in the router
// instead, and the engine reads empty for the same reason. Either way prefill
// never learns k2, mu or its bound, and is priced at k1.
//
// Two more ways to open the gate for a prefill row, each a replay switch.
// Each only stands in for the queue half of the gate: the resident-token
// half, and every refusal past the gate (a saturated decode downstream, a
// row that is not admitted, a replica settling a load step), are unchanged.
//
//   - The running batch at its token budget (SwitchPrefillSaturatedAtBatchBudget):
//     the prompt tokens the replica's running requests hold, RunningRequests
//     x the prompt length it computes, at prefillBatchBudgetFraction of its
//     per-step budget or more. The budget is the engine's stated one
//     (EngineParams.MaxNumBatchedTokens: vLLM --max-num-batched-tokens, SGLang
//     --chunked-prefill-size), per rank; a budget the flags do not state is
//     not known, and the gate does not open on it. The resolved default
//     (EffectiveMaxBatchedTokens) is not read: vLLM's default moves with the
//     version, the device and the entry point, and a budget read low would
//     call a replica full that is not, the side that over-orders.
//   - The router's flow-control queue (SwitchPrefillSaturatedOnRouterQueue):
//     with flow control on (domain.AnalyzerInput.SchedulerQueue), a router
//     backlog of queueLengthThreshold requests per Ready prefill replica or
//     more opens the gate for every prefill row of the model. The router
//     queue is the model's, not a role's: it is read as prefill's because
//     prefill is the first role a request reaches.
//
// Both are off from the constructor: the replay of the TTFT staircases did
// not support turning either on. On the 2+2 fleet of the r2 staircases, at
// every step that missed the TTFT target, the prefill running batch read 0
// requests at the median and at most 0.85 of its budget, and the router
// queue read 0, so neither opens the gate below the runaway and neither
// changes what is ordered there. The prefill engines were not full; they
// were slow for the target. Where the batch did read full, it was for single
// cycles: the running gauge is an instant, and the completion rate the
// same reading writes as mu is a minute's mean, so one full instant on a
// half-busy replica wrote a mu half the saturated one (15.4 req/s against
// 27-31 measured under a queue) and priced prefill on it for the next half
// hour (ttft-staircase-c-on).

// SwitchPrefillSaturatedAtBatchBudget is the replay switch for a prefill row
// whose running prompt tokens are at its stated per-step token budget
// (prefillAtBatchBudget) reading saturated for the gate. Off, the gate is the
// waiting queue alone, as before.
const SwitchPrefillSaturatedAtBatchBudget = "prefill-saturated-at-batch-budget"

// SwitchPrefillSaturatedOnRouterQueue is the replay switch for the router's
// flow-control backlog (routerQueueSaturatesPrefill) reading every prefill row
// of the model saturated for the gate. Off, the gate is the waiting queue
// alone, as before.
const SwitchPrefillSaturatedOnRouterQueue = "prefill-saturated-on-router-queue"

// prefillBatchBudgetFraction is the share of its per-step token budget a
// prefill replica's running prompts must hold for its batch to read full.
// Under 1: the prompts of one batch do not fill the budget to the token, and
// a budget of 65536 at 8000-token prompts is full at eight (64000, 0.98).
const prefillBatchBudgetFraction = 0.9

// Why a prefill row's gate opened without its own queue (gateQueue).
const (
	prefillGateBatchBudget = "running batch at its token budget"
	prefillGateRouterQueue = "router flow-control queue"
)

// prefillAtBatchBudget reports whether a prefill row's running requests hold
// at least prefillBatchBudgetFraction of its per-step token budget: the
// stated --max-num-batched-tokens (or --chunked-prefill-size) times its
// ranks. The prompt is what prefill computes, the recent prompt length
// (AvgInputTokensRecent, else AvgInputTokens) less its prefix-cache hits.
// False when the budget is not stated, the running count was not read, or
// the prompt length is unknown: the gate is then the queue alone.
func prefillAtBatchBudget(rm domain.ReplicaMetrics, params *capacity.EngineParams) bool {
	if params == nil || params.MaxNumBatchedTokens <= 0 || !rm.RunningRequestsKnown || rm.RunningRequests <= 0 {
		return false
	}
	prompt := rm.AvgInputTokensRecent
	if !(prompt > 0) || math.IsInf(prompt, 1) {
		prompt = rm.AvgInputTokens
	}
	if !(prompt > 0) || math.IsInf(prompt, 1) {
		return false
	}
	hit := rm.PrefixCacheHitRate
	if !(hit > 0 && hit < 1) {
		// NaN, negative or a whole hit: none discounted, so no budget is
		// read full on a figure that is not one.
		hit = 0
	}
	running := float64(rm.RunningRequests) * prompt * (1 - hit)
	budget := float64(params.MaxNumBatchedTokens) * float64(rm.Ranks())
	return running >= prefillBatchBudgetFraction*budget
}

// routerQueueSaturatesPrefill reports whether the router's flow-control
// backlog holds at least queueThreshold requests per Ready prefill replica:
// what the engine gate asks of one replica's own queue, over the replicas the
// backlog waits for. False with flow control off (q nil), no Ready prefill
// replica, or no threshold.
func routerQueueSaturatesPrefill(q *domain.SchedulerQueueMetrics, readyPrefill int, queueThreshold float64) bool {
	if q == nil || readyPrefill <= 0 || !(queueThreshold > 0) {
		return false
	}
	return float64(q.QueueSize) >= queueThreshold*float64(readyPrefill)
}

// readyPrefillReplicas is how many Ready rows of the prefill role there are,
// warm-pool bridges included: a bridge serves the backlog too.
func readyPrefillReplicas(rows []domain.ReplicaMetrics, rolesByVariant map[string]string) int {
	n := 0
	for _, rm := range rows {
		if rm.Ready && domain.CanonicalRole(rolesByVariant[rm.VariantName]) == domain.RolePrefill {
			n++
		}
	}
	return n
}

// prefillGateOpenedBy is why a prefill row reads saturated for the gate
// without its own queue, under the switches that are on, or "" when it does
// not: a row of another role, a row whose queue is already over the
// threshold, or neither signal. routerOpen is the cycle's router test
// (routerQueueSaturatesPrefill), read only with its switch on.
func (a *SaturationAnalyzer) prefillGateOpenedBy(rm domain.ReplicaMetrics, role string, queueThreshold float64,
	params *capacity.EngineParams, routerOpen bool) string {
	if domain.CanonicalRole(role) != domain.RolePrefill || rm.BusiestRankQueue() >= int(queueThreshold) {
		return ""
	}
	switch {
	case a.prefillBatchBudget && prefillAtBatchBudget(rm, params):
		return prefillGateBatchBudget
	case a.prefillRouterQueue && routerOpen:
		return prefillGateRouterQueue
	}
	return ""
}

// gateQueue is the queue the saturation gate reads for a row: its busiest
// rank's, or the threshold itself where the row's gate opened without one
// (routing.prefillGate), so every reader past the gate -- the settle test,
// k2's chain, the unadmitted price, the prefill bound -- sees the same
// saturated row. Demand never reads it: a queue that is not there is not work.
func gateQueue(rm domain.ReplicaMetrics, queueThreshold float64, routing replicaRouting) int {
	q := rm.BusiestRankQueue()
	if routing.prefillGate == "" {
		return q
	}
	return max(q, int(math.Ceil(queueThreshold)))
}
