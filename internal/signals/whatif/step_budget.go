package whatif

// StepBudgetCap is the most sequences one engine rank runs in a decode step:
// its batch cap, perRank, never more than its step token budget holds.
//
// vLLM's Scheduler.schedule() takes 1 + k tokens of max_num_batched_tokens
// for each running request, k the speculative tokens a step proposes
// (num_speculative_tokens). The last request it reaches is cut to what is
// left, its draft tokens trimmed, and the requests after it wait for the next
// step. So a rank with budget B runs at most ceil(B / (1 + k)) sequences a
// step, whatever its max_num_seqs: a wide-EP decode with the batched
// all-to-all's default budget of 256 and MTP k = 3 runs 64, against a cap of
// 256 (issue llm-d-incubation/llm-d-scaling-manager#302).
//
// k is what the flags state. The rule is exact for drafters that always
// propose k tokens (MTP, EAGLE, a draft model); an n-gram or suffix drafter
// often proposes fewer, so for it the batch is a floor, the safe side.
//
// The accepted draft tokens do not enter: they set how many tokens a running
// sequence emits a step, which the measured ITL (seconds per emitted token)
// already carries, not how many sequences run.
//
// A budget of 0 or below is unknown and caps nothing; a perRank of 0 or below
// is no cap and stays no cap. k below 0 is read as 0.
func StepBudgetCap(perRank, budget int64, specTokens int) int64 {
	if budget <= 0 || perRank <= 0 {
		return perRank
	}
	step := int64(1 + max(specTokens, 0))
	return min(perRank, (budget+step-1)/step)
}
