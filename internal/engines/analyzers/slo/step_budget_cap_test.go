package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// TestCapAtStepBudget_TheSwitch: off from the constructor, the replica is
// priced at its cap alone; on, at the batch its step budget runs (issue
// llm-d-incubation/llm-d-scaling-manager#302). Synthetic: no recording holds
// an MTP or a wide-EP engine. A rank with the batched all-to-all's budget of
// 256 tokens and MTP k = 3 runs ceil(256 / 4) = 64 sequences, under its stated
// cap of 256; a dense rank at 8192 tokens runs its cap.
func TestCapAtStepBudget_TheSwitch(t *testing.T) {
	assert.False(t, New(nil, capacity.NewStore()).capAtStepBudget, "off from the constructor")
	on := func(a *Analyzer) { a.capAtStepBudget = true }
	wide := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 256, MaxNumSeqsSet: true,
		EffectiveMaxBatchedTokens: 256, SpeculativeConfig: `{"method":"mtp","num_speculative_tokens":3}`}

	off := pricedDetail(t, wide, nil)
	assert.EqualValues(t, 256, off.batchCap)
	assert.Equal(t, batchCapFlags, off.batchCapSource)

	got := pricedDetail(t, wide, on)
	assert.EqualValues(t, 64, got.batchCap)
	assert.Equal(t, batchCapStepBudget, got.batchCapSource)

	dense := wide
	dense.EffectiveMaxBatchedTokens, dense.SpeculativeConfig = 8192, ""
	assert.EqualValues(t, 256, pricedDetail(t, dense, on).batchCap, "a dense engine at the default budget is unmoved")

	sg := wide
	sg.Engine = inferenceengine.EngineSGLang
	assert.EqualValues(t, 256, pricedDetail(t, sg, on).batchCap, "SGLang's chunked prefill size is no decode step budget")
}
