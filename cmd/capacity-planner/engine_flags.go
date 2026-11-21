package main

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The engine configurations the planner's decode model does not cover
// (issue llm-d-incubation/llm-d-scaling-manager#313). Each is a flag on every
// figure of the variant, with what it rests on: the figure is printed, and the
// operator is told which part of it the model cannot vouch for. None changes
// a figure.
const (
	// moeDataParallel is a MoE engine with expert parallelism and more than
	// one data-parallel rank. The ranks step in lockstep: every step is padded
	// to the busiest rank, idle ranks run dummy batches, and the expert
	// all-to-all syncs every MoE layer. The line and the band read the mean
	// fill over the ranks, so the busiest rank can preempt while the mean sits
	// inside the band (issue #312).
	moeDataParallel whatif.Flag = "MoEDataParallel"
	// stepBudgetBindsBatch is a per-rank batch, times 1 + k with k
	// speculative tokens, past the engine's step token budget: vLLM's
	// scheduler takes 1 + k tokens of the budget per running request and
	// skips the rest once it is spent, so the batch the knee is priced at is
	// not one the engine runs (issue #302).
	stepBudgetBindsBatch whatif.Flag = "StepBudgetBindsBatch"
	// pastCUDAGraphCeiling is a per-rank batch past the largest the engine
	// captures a CUDA graph for, or an engine with no graphs at all: those
	// steps run eager, slower than the line read where they ran in a graph.
	pastCUDAGraphCeiling whatif.Flag = "PastCUDAGraphCeiling"
	// hybridAttention is a model whose layers are not all full attention
	// (sliding window, chunked local, Mamba or linear): the KV a request
	// holds is not its context, and the ITL is not linear in fill.
	hybridAttention whatif.Flag = "HybridAttention"
	// mlaAttention is a model with multi-head latent attention: its KV per
	// token is small, so the batch cap or the step budget, not the KV, is
	// what usually bounds the batch. Priced; the detail says which binds.
	mlaAttention whatif.Flag = "MLAAttention"
	// hostBoundTPOTTail is a percentile TPOT on an engine whose per-token
	// latency with an empty cache is under hostBoundITL: the tail is host
	// jitter the histogram's coarse buckets do not resolve, and reads low.
	hostBoundTPOTTail whatif.Flag = "HostBoundTPOTTail"
	// kvConnectorUnmodeled is a KV connector other than NIXL: the transfer
	// and its effect on the decode line were measured on NIXL only.
	kvConnectorUnmodeled whatif.Flag = "KVConnectorUnmodeled"
	// kvOffload is KV offloading past GPU memory: the capacity priced is the
	// GPU's, and a request served from the offload tier pays a latency the
	// line does not hold.
	kvOffload whatif.Flag = "KVOffload"
	// sglangNoMixedChunk is an aggregated SGLang pod without
	// --enable-mixed-chunk: a prefill step stalls every running sequence.
	sglangNoMixedChunk whatif.Flag = "SGLangNoMixedChunk"
	// extensibleKVCache is vLLM v0.34's extensible KV cache: the capacity a
	// recording read at start-up can grow while the engine runs.
	extensibleKVCache whatif.Flag = "ExtensibleKVCache"
	// defaultBatchCapBelowRelease is a batch cap priced at the parser's
	// default of 256 where the engine's release runs 1024 on its accelerator:
	// the decode figures can over-ask up to four times.
	defaultBatchCapBelowRelease whatif.Flag = "DefaultBatchCapBelowRelease"
	// batchBeyondFit is a shape whose KV per request is under 1/shapeFitRatio
	// of the rows' the line was read on, priced at a
	// per-rank batch past the largest those rows held: ITL also rises with the
	// batch's tokens past the GEMM roofline, which a line in fill alone does
	// not carry across shapes (issue #309).
	batchBeyondFit whatif.Flag = "BatchBeyondFit"
)

// batchCapSourceDefault is the analyzer's source for a batch cap that is the
// parser's default (learnreplay.VariantState.BatchCapSource).
const batchCapSourceDefault = "default"

// hostBoundITL is the per-token latency with an empty cache under which an
// engine's TPOT tail is host jitter (hostBoundTPOTTail).
const hostBoundITL = 0.005

// shapeFitRatio is how far apart, as a ratio, the KV per request asked and
// the line's rows' may be before a batch past the rows' is flagged
// (batchBeyondFit).
const shapeFitRatio = 2.0

// hybridModels and mlaModels are fragments of a model's name, lowercased, that
// name a hybrid-attention or an MLA family.
var (
	hybridModels = []string{"gpt-oss", "gemma-2-", "gemma-3-", "gemma2-", "gemma3-", "gemma-3n", "llama-4", "llama4",
		"qwen3-next", "nemotron-h", "jamba", "granite-4.0-h", "mamba", "falcon-h1", "kimi-linear",
		"minimax-m1", "minimax-text-01", "command-r7b", "command-a"}
	mlaModels = []string{"deepseek-v2", "deepseek-v3", "deepseek-r1", "deepseek-coder-v2", "kimi-k2", "moonlight",
		"mistral-large-3", "longcat"}
	// notMLA are fragments that name a dense model inside an MLA family's
	// name: DeepSeek-R1-Distill-Qwen and -Llama, and DeepSeek-R1-0528-Qwen3,
	// are dense.
	notMLA = []string{"distill", "qwen", "llama"}
)

// engineNote is one engine flag and what it rests on.
type engineNote struct {
	flag   whatif.Flag
	detail string
}

// engineNotes are the engine configurations of pv the planner's decode model
// does not cover (the flags above), in a fixed order; none with no engine
// parameters. hostBoundTPOTTail is not among them: it qualifies a percentile
// TPOT only (tpotFlags).
func (pv pricedVariant) engineNotes() []engineNote {
	p := pv.state.Params
	if p == nil {
		return nil
	}
	var out []engineNote
	add := func(f whatif.Flag, format string, args ...any) {
		out = append(out, engineNote{flag: f, detail: fmt.Sprintf(format, args...)})
	}
	batch := pv.pricedBatchPerRank()
	if dp := max(p.DataParallelSize, 1); p.ExpertParallel && dp > 1 {
		a2a := ""
		if p.BatchedDPMoE() {
			a2a = ", batched all-to-all"
		}
		add(moeDataParallel, "DP %d with expert parallelism%s: ranks step with the busiest, fill is the mean", dp, a2a)
	}
	k := p.SpeculativeTokens()
	if budget := p.StepBudget(); budget > 0 && batch > 0 && batch*int64(1+k) > budget {
		if k > 0 {
			add(stepBudgetBindsBatch, "%d sequences a rank x %d tokens a step past a step budget of %d; full drafts for at most %d",
				batch, 1+k, budget, budget/int64(1+k))
		} else {
			add(stepBudgetBindsBatch, "%d sequences a rank past a step budget of %d tokens", batch, budget)
		}
	}
	if ceiling, ok := p.GraphCeiling(pv.state.BatchCap); ok && batch > ceiling {
		if ceiling == 0 {
			add(pastCUDAGraphCeiling, "no CUDA graphs: every step runs eager")
		} else {
			add(pastCUDAGraphCeiling, "graphs to %d sequences a rank, priced at up to %d", ceiling, batch)
		}
	}
	model := strings.ToLower(p.ModelPath)
	if model == "" {
		model = strings.ToLower(pv.model)
	}
	if fam := familyOf(model, hybridModels); fam != "" {
		add(hybridAttention, "%s: KV per request and ITL are not those of full attention", fam)
	}
	if fam := familyOf(model, mlaModels); fam != "" && familyOf(model, notMLA) == "" {
		add(mlaAttention, "%s: %s", fam, pv.mlaBinds())
	}
	if c := p.KVConnector; c != "" && !strings.Contains(strings.ToLower(c), "nixl") {
		add(kvConnectorUnmodeled, "%s", c)
	}
	if p.KVOffloadingSize > 0 || p.KVOffloadingBackend != "" {
		add(kvOffload, "%s", offloadText(p))
	}
	if p.Engine == inferenceengine.EngineSGLang && domain.CanonicalRole(pv.state.Role) == domain.RoleBoth && !p.MixedChunk {
		add(sglangNoMixedChunk, "prefill steps stall the decode batch")
	}
	if p.ExtensibleKVCacheOn() {
		add(extensibleKVCache, "vLLM %s: KV capacity can grow past what start-up read", p.VLLMVersion)
	}
	if def := capacity.VLLMDefaultMaxNumSeqs(p.VLLMVersion, pv.state.Accelerator); p.Engine == inferenceengine.EngineVLLM &&
		pv.state.BatchCapSource == batchCapSourceDefault && pv.perRank == pv.state.BatchCap && def > pv.perRank {
		add(defaultBatchCapBelowRelease, "priced at %d a rank; vLLM on %s defaults to %d: state --max-num-seqs or --batch-cap",
			pv.perRank, pv.state.Accelerator, def)
	}
	if seen, fitKV, ok := pv.fitBatch(); ok && batch > seen && pv.decode.KVPerRequest > 0 {
		// Only a shorter shape: with more KV a request than the rows, a
		// fill holds fewer sequences than the line was read with, and the
		// line reads the ITL high, the safe side.
		if r := pv.decode.KVPerRequest / fitKV; r < 1/shapeFitRatio {
			add(batchBeyondFit, "KV %s a request against the rows' %s; up to %d sequences a rank priced, %d seen",
				num(round(pv.decode.KVPerRequest)), num(round(fitKV)), batch, seen)
		}
	}
	return out
}

// familyOf is the first of families model's name contains, "" with none.
func familyOf(model string, families []string) string {
	for _, f := range families {
		if strings.Contains(model, f) {
			return f
		}
	}
	return ""
}

// offloadText names the offload tier of p.
func offloadText(p *capacity.EngineParams) string {
	switch {
	case p.KVOffloadingBackend != "" && p.KVOffloadingSize > 0:
		return fmt.Sprintf("%s, %s GiB", p.KVOffloadingBackend, num(p.KVOffloadingSize))
	case p.KVOffloadingBackend != "":
		return p.KVOffloadingBackend
	}
	return num(p.KVOffloadingSize) + " GiB"
}

// pricedBatchPerRank is the most sequences one engine rank runs at the band's
// top: the KV there holds band.Hi x C / KVreq a replica, never more than the
// batch cap. The cap alone where the KV is unknown; 0 with neither.
func (pv pricedVariant) pricedBatchPerRank() int64 {
	ranks := int64(max(pv.state.Ranks, 1))
	d := pv.decode
	kv := int64(0)
	if d.CapacityTokens > 0 && d.KVPerRequest > 0 && d.Band.Hi > 0 {
		kv = int64(math.Floor(d.Band.Hi * d.CapacityTokens / d.KVPerRequest / float64(ranks)))
	}
	switch {
	case pv.perRank <= 0:
		return kv
	case kv > 0:
		return min(kv, pv.perRank)
	}
	return pv.perRank
}

// mlaBinds says what bounds an MLA engine's batch at the band's top: the
// batch cap, the step budget or the KV.
func (pv pricedVariant) mlaBinds() string {
	d := pv.decode
	ranks := float64(max(pv.state.Ranks, 1))
	kv, by := math.Inf(1), "nothing known binds"
	if d.CapacityTokens > 0 && d.KVPerRequest > 0 && d.Band.Hi > 0 {
		kv, by = d.Band.Hi*d.CapacityTokens/d.KVPerRequest/ranks, "the KV binds"
	}
	bound := kv
	if pv.perRank > 0 && float64(pv.perRank) < bound {
		bound, by = float64(pv.perRank), "the batch cap binds"
	}
	if p := pv.state.Params; p != nil {
		if b := p.StepBudget(); b > 0 {
			if steps := float64(b) / float64(1+p.SpeculativeTokens()); steps < bound {
				by = "the step budget binds"
			}
		}
	}
	return by
}

// fitBatch is the most sequences one engine rank held on the rows the line is
// checked on, and their median KV per request; ok false with no such row.
// A row on a ramp or above the band says nothing of the line and is left out.
func (pv pricedVariant) fitBatch() (seen int64, kv float64, ok bool) {
	c := pv.decode.CapacityTokens
	if !(c > 0) {
		return 0, 0, false
	}
	ranks := float64(max(pv.state.Ranks, 1))
	var kvs []float64
	most := 0.0
	for _, r := range pv.state.LineRows {
		if r.Ramp || r.AboveBand || !(r.KVPerRequest > 0) || !(r.K > 0) {
			continue
		}
		kvs = append(kvs, r.KVPerRequest)
		most = max(most, r.K*c/r.KVPerRequest/ranks)
	}
	if len(kvs) == 0 {
		return 0, 0, false
	}
	slices.Sort(kvs)
	mid := len(kvs) / 2
	kv = kvs[mid]
	if len(kvs)%2 == 0 {
		kv = (kvs[mid-1] + kvs[mid]) / 2
	}
	return int64(math.Ceil(most)), kv, true
}

// hostBound reports whether pv's per-token latency with an empty cache, its
// line's B or the floor read below the band where lower, is under
// hostBoundITL, and that latency.
func (pv pricedVariant) hostBound() (float64, bool) {
	b := pv.decode.Line.B
	if f := pv.decode.BelowBand.LeastITL; f > 0 && (b <= 0 || f < b) {
		b = f
	}
	return b, b > 0 && b < hostBoundITL
}

// tpotFlags are the flags a TPOT figure carries: R_q's, and for a percentile
// hostBoundTPOTTail on a host-bound engine where R_q was read: not assumed,
// not refused.
func (pv pricedVariant) tpotFlags(r resolvedRatio, percentile bool) []whatif.Flag {
	if !percentile || r.ratio.Assumed || r.refused != nil {
		return r.flags
	}
	if _, ok := pv.hostBound(); !ok || slices.Contains(r.flags, hostBoundTPOTTail) {
		return r.flags
	}
	return append(slices.Clone(r.flags), hostBoundTPOTTail)
}

// engineNoteText is the detail of an engine flag f on pv, "" for any other
// flag.
func (pv pricedVariant) engineNoteText(f whatif.Flag) string {
	if f == hostBoundTPOTTail {
		if b, ok := pv.hostBound(); ok {
			return fmt.Sprintf("B %s: the tail is host jitter", ms(b))
		}
		return ""
	}
	for _, n := range pv.notes {
		if n.flag == f {
			return n.detail
		}
	}
	return ""
}
