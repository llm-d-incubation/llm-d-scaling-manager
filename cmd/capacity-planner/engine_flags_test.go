package main

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// denseVariant is a dense vLLM decode variant the decode model covers: one
// rank, a cap of 256 stated by the flags, 8192 tokens a step, 1M tokens of
// KV and 4,000 a request, a line with B 12 ms.
func denseVariant() pricedVariant {
	p := &capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 256, MaxNumSeqsSet: true,
		EffectiveMaxBatchedTokens: 8192, VLLMVersion: capacity.VLLMVersion{Minor: 30}, VLLMVersionFrom: "image tag",
		ModelPath: "Qwen/Qwen3-32B"}
	return pricedVariant{
		state: learnreplay.VariantState{Variant: "decode", Role: domain.RoleDecode, Accelerator: "H200",
			BatchCap: 256, BatchCapSource: "flags", Ranks: 1, Params: p},
		perRank: 256,
		model:   "Qwen/Qwen3-32B",
		decode: whatif.Decode{Line: itl.Model{A: 0.02, B: 0.012}, Band: whatif.FillBand{Lo: 0.15, Hi: 0.8},
			CapacityTokens: 1e6, KVPerRequest: 4000, ReplicaCap: 256, Replicas: 1},
	}
}

// flagsOf is the engine flags of pv, in order.
func flagsOf(pv pricedVariant) []whatif.Flag {
	out := make([]whatif.Flag, 0, 4)
	for _, n := range pv.engineNotes() {
		out = append(out, n.flag)
	}
	return out
}

// A dense engine the model covers is flagged with nothing: the flags are not
// noise on every answer.
func TestEngineNotes_ADenseEngineIsNotFlagged(t *testing.T) {
	assert.Empty(t, flagsOf(denseVariant()))
	pv := denseVariant()
	pv.state.Params = nil
	assert.Empty(t, flagsOf(pv), "no parameters recorded: nothing to say")
}

// Each configuration the model does not cover raises its own flag, with what
// it rests on.
func TestEngineNotes_EachUnmodeledConfiguration(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*pricedVariant)
		want   whatif.Flag
		detail string
	}{
		{"MoE DP with EP", func(pv *pricedVariant) {
			pv.state.Params.ExpertParallel, pv.state.Params.DataParallelSize = true, 8
			pv.state.Params.MemoryLayout = "all2all_backend=deepep_low_latency"
		}, moeDataParallel, "DP 8 with expert parallelism, batched all-to-all: ranks step with the busiest, fill is the mean"},
		{"MTP past a wide-EP budget", func(pv *pricedVariant) {
			pv.state.Params.EffectiveMaxBatchedTokens = 256
			pv.state.Params.CUDAGraphCaptureMax = 1024 // the graphs hold the batch: the budget alone binds
			pv.state.Params.SpeculativeConfig = `{"method":"mtp","num_speculative_tokens":3}`
		}, stepBudgetBindsBatch, "200 sequences a rank x 4 tokens a step past a step budget of 256; full drafts for at most 64"},
		{"enforce eager", func(pv *pricedVariant) { pv.state.Params.EnforceEager = true },
			pastCUDAGraphCeiling, "no CUDA graphs: every step runs eager"},
		{"capture below the batch", func(pv *pricedVariant) { pv.state.Params.CUDAGraphCaptureMax = 64 },
			pastCUDAGraphCeiling, "graphs to 64 sequences a rank, priced at up to 200"},
		{"hybrid attention", func(pv *pricedVariant) { pv.state.Params.ModelPath = "openai/gpt-oss-120b" },
			hybridAttention, "gpt-oss: KV per request and ITL are not those of full attention"},
		{"MLA, the cap binds", func(pv *pricedVariant) {
			pv.state.Params.ModelPath = "deepseek-ai/DeepSeek-V3"
			pv.perRank = 64
		}, mlaAttention, "deepseek-v3: the batch cap binds"},
		{"MLA, the KV binds", func(pv *pricedVariant) { pv.state.Params.ModelPath = "moonshotai/Moonlight-16B-A3B" },
			mlaAttention, "moonlight: the KV binds"},
		{"a connector other than NIXL", func(pv *pricedVariant) { pv.state.Params.KVConnector = "LMCacheConnectorV1" },
			kvConnectorUnmodeled, "LMCacheConnectorV1"},
		{"KV offload", func(pv *pricedVariant) {
			pv.state.Params.KVOffloadingSize, pv.state.Params.KVOffloadingBackend = 64, "native"
		}, kvOffload, "native, 64 GiB"},
		{"SGLang aggregated without mixed chunk", func(pv *pricedVariant) {
			pv.state.Params.Engine, pv.state.Role = inferenceengine.EngineSGLang, domain.RoleBoth
		}, sglangNoMixedChunk, "prefill steps stall the decode batch"},
		// Every dense v0.34 engine that keeps the default is flagged, on
		// purpose: the capacity a recording read at start-up may not be what
		// the engine holds later.
		{"extensible KV cache, a dense v0.34 default", func(pv *pricedVariant) { pv.state.Params.VLLMVersion = capacity.VLLMVersion{Minor: 34} },
			extensibleKVCache, "vLLM 0.34: KV capacity can grow past what start-up read"},
		{"a budget under the batch without speculation", func(pv *pricedVariant) {
			pv.state.Params.EffectiveMaxBatchedTokens = 128
		}, stepBudgetBindsBatch, "200 sequences a rank past a step budget of 128 tokens"},
		{"MLA, the band unknown", func(pv *pricedVariant) {
			pv.state.Params.ModelPath = "deepseek-ai/DeepSeek-V3"
			pv.decode.Band.Hi, pv.perRank, pv.state.Params.EffectiveMaxBatchedTokens = 0, 0, 0
		}, mlaAttention, "deepseek-v3: nothing known binds"},
		{"the parser's default cap on an H200", func(pv *pricedVariant) {
			pv.state.Params.MaxNumSeqsSet, pv.state.BatchCapSource = false, batchCapSourceDefault
		}, defaultBatchCapBelowRelease, "priced at 256 a rank; vLLM on H200 defaults to 1024: state --max-num-seqs or --batch-cap"},
	} {
		pv := denseVariant()
		p := *pv.state.Params
		pv.state.Params = &p
		c.change(&pv)
		got := flagsOf(pv)
		require.Equal(t, []whatif.Flag{c.want}, got, c.name)
		pv.notes = pv.engineNotes()
		assert.Equal(t, c.detail, pv.engineNoteText(c.want), c.name)
	}
}

// The edges each flag stops at: NIXL is modeled, a cap the operator set or
// typed is not the parser's default, and a batch the budget holds is not past
// it.
func TestEngineNotes_TheEdgesAreNotFlagged(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*pricedVariant)
	}{
		{"NIXL", func(pv *pricedVariant) { pv.state.Params.KVConnector = "NixlConnector" }},
		{"dense DP without EP", func(pv *pricedVariant) { pv.state.Params.DataParallelSize = 8 }},
		{"--batch-cap typed over the default", func(pv *pricedVariant) {
			pv.state.Params.MaxNumSeqsSet, pv.state.BatchCapSource = false, batchCapSourceDefault
			pv.perRank = 128
		}},
		{"the default cap on an A100", func(pv *pricedVariant) {
			pv.state.Params.MaxNumSeqsSet, pv.state.BatchCapSource, pv.state.Accelerator = false, batchCapSourceDefault, "NVIDIA-A100-SXM4-80GB"
		}},
		{"k=1 inside the budget", func(pv *pricedVariant) {
			pv.state.Params.SpeculativeConfig = `{"num_speculative_tokens":1}`
		}},
		{"SGLang aggregated with mixed chunk", func(pv *pricedVariant) {
			pv.state.Params.Engine, pv.state.Role, pv.state.Params.MixedChunk = inferenceengine.EngineSGLang, domain.RoleBoth, true
		}},
		{"a distilled R1 is dense", func(pv *pricedVariant) { pv.state.Params.ModelPath = "deepseek-ai/DeepSeek-R1-Distill-Qwen-32B" }},
		{"R1-0528-Qwen3 is dense", func(pv *pricedVariant) { pv.state.Params.ModelPath = "deepseek-ai/DeepSeek-R1-0528-Qwen3-8B" }},
		{"Gemma 1 2B is full attention", func(pv *pricedVariant) { pv.state.Params.ModelPath = "google/gemma-2b-it" }},
		{"a dense Granite 4", func(pv *pricedVariant) { pv.state.Params.ModelPath = "ibm-granite/granite-4.0-micro" }},
	} {
		pv := denseVariant()
		p := *pv.state.Params
		pv.state.Params = &p
		c.change(&pv)
		assert.Empty(t, flagsOf(pv), c.name)
	}
}

// A host-bound engine's percentile TPOT is flagged where R_q was read; its
// mean TPOT, and a percentile on an assumed R_q, are not.
func TestTPOTFlags_HostBound(t *testing.T) {
	pv := denseVariant()
	read := resolvedRatio{ratio: whatif.TPOTRatio{Value: 1.3}, flags: []whatif.Flag{whatif.CoarseTPOTBucket}}
	assert.Equal(t, read.flags, pv.tpotFlags(read, true), "B 12 ms is not host-bound")

	pv.decode.Line.B = 0.003
	got := pv.tpotFlags(read, true)
	assert.Equal(t, []whatif.Flag{whatif.CoarseTPOTBucket, hostBoundTPOTTail}, got)
	assert.Len(t, read.flags, 1, "R_q's own flags are not written through")
	assert.Equal(t, "B 3.0 ms: the tail is host jitter", pv.engineNoteText(hostBoundTPOTTail))
	assert.Equal(t, read.flags, pv.tpotFlags(read, false), "a mean TPOT is not a tail")
	assumed := resolvedRatio{ratio: whatif.TPOTRatio{Value: 1.3, Assumed: true}}
	assert.Empty(t, pv.tpotFlags(assumed, true), "an assumed R_q carries its own flag")

	pv.decode.Line.B = 0.012
	pv.decode.BelowBand.LeastITL = 0.004
	assert.True(t, slices.Contains(pv.tpotFlags(read, true), hostBoundTPOTTail), "the floor below the band is the lower reading")
}
