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
	// batchBeyondFit is a decode figure whose fill runs more sequences an
	// engine rank than the most the rows the ITL line was fitted on held (the
	// held fit's rows for a held line), whatever the KV a request: ITL also
	// rises with the batch's tokens past the GEMM roofline, which a line in
	// fill alone does not carry, so the TPOT reads low and the knee high
	// (issues #309, #354). Not an engine note: each figure is checked at its
	// own fill (pricedVariant.qualify), and the figure is printed unchanged.
	// Until #354 it was raised on every figure of a variant, and only for a
	// shape under half the rows' KV a request, priced at the band's top.
	batchBeyondFit whatif.Flag = "BatchBeyondFit"
)

// batchCapSourceDefault is the analyzer's source for a batch cap that is the
// parser's default (learnreplay.VariantState.BatchCapSource).
const batchCapSourceDefault = "default"

// hostBoundITL is the per-token latency with an empty cache under which an
// engine's TPOT tail is host jitter (scheduling, sampling, detokenizing),
// not the fill: on a sub-1B model the line's intercept and its floor read
// 1.5 to 2.5 ms. It flags a percentile (hostBoundTPOTTail) and, where R_q
// was read off coarse buckets, refuses it (hostBoundRefusal).
const hostBoundITL = 0.005

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
	hybrid, mla := pv.attentionFamily()
	if hybrid != "" {
		add(hybridAttention, "%s: KV per request and ITL are not those of full attention", hybrid)
	}
	if mla != "" {
		add(mlaAttention, "%s: %s", mla, pv.mlaBinds())
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
	return out
}

// attentionFamily is what makes pv's model hybrid-attention or MLA, each ""
// where it is not: the attention kind its model config states (issue #501),
// and, with no config read, the family its name falls in, as before the
// config was read.
//
// A config never clears a hybrid the name says: a hybrid the reader does not
// know reads as full attention, and clearing the flag on that would put a
// hybrid's figures inside the envelope on the reader's miss. The name's family
// is kept, and the detail says the config reads otherwise. A config does
// clear an MLA family's name (an MLA family's dense distillation is common),
// and names MLA or a window or hybrid layers wherever it reads one.
func (pv pricedVariant) attentionFamily() (hybrid, mla string) {
	p := pv.state.Params
	model := strings.ToLower(p.ModelPath)
	if model == "" {
		model = strings.ToLower(pv.model)
	}
	byName := familyOf(model, hybridModels)
	if !p.ModelConfig.Read() {
		if fam := familyOf(model, mlaModels); fam != "" && familyOf(model, notMLA) == "" {
			mla = fam
		}
		return byName, mla
	}
	switch p.AttentionKind() {
	case capacity.AttentionSlidingWindow, capacity.AttentionHybrid:
		return p.ModelConfig.AttentionText(), ""
	case capacity.AttentionMLA:
		mla = p.ModelConfig.AttentionText()
	}
	if byName != "" {
		hybrid = fmt.Sprintf("%s by its name, though the model config reads %s", byName, p.ModelConfig.AttentionText())
	}
	return hybrid, mla
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

// fitBatch is the most sequences one engine rank held on the rows the line
// was fitted on (learnreplay.VariantState.FitRows: the line's own, or the
// held fit's for a held line), else on the latest rows the line is checked
// on (LineRows); ok false with no such row. A row's batch is the sequences it
// ran (whatif.LineRow.Sequences), else its fill over the KV a request held.
// A row on a ramp is left out, and a latest row above the band: it says
// nothing of the line (a fit row above it is an above-band line's own).
func (pv pricedVariant) fitBatch() (seen int64, ok bool) {
	c := pv.decode.CapacityTokens
	if !(c > 0) {
		return 0, false
	}
	rows, fit := pv.state.FitRows, true
	if len(rows) == 0 {
		rows, fit = pv.state.LineRows, false
	}
	ranks := float64(max(pv.state.Ranks, 1))
	most := 0.0
	for _, r := range rows {
		// The fit rows are the line's own, above the band only where
		// it was fitted there; of the latest rows, one above the band
		// says nothing of a line fitted in it.
		if r.Ramp || (r.AboveBand && !fit) || !(r.K > 0) {
			continue
		}
		switch {
		case r.Sequences > 0:
			most = max(most, r.Sequences/ranks)
		case r.KVPerRequest > 0:
			// A row that read no sequences: its fill over the KV a
			// request held.
			most = max(most, r.K*c/r.KVPerRequest/ranks)
		default:
			continue
		}
		ok = true
	}
	return int64(math.Ceil(most)), ok
}

// batchAt is the sequences one engine rank runs at fill k: k x C / KVreq a
// replica over its ranks, never past the batch cap; 0 where the KV is
// unknown.
func (pv pricedVariant) batchAt(k float64) int64 {
	d := pv.decode
	if !(d.CapacityTokens > 0 && d.KVPerRequest > 0 && k > 0) {
		return 0
	}
	n := int64(math.Floor(k * d.CapacityTokens / d.KVPerRequest / float64(max(pv.state.Ranks, 1))))
	if pv.perRank > 0 {
		n = min(n, pv.perRank)
	}
	return n
}

// beyondFit is the batch a decode figure at fill k runs against the most its
// line's fit rows held, where it is past them (batchBeyondFit); ok false
// where it is not, or either is unknown.
func (pv pricedVariant) beyondFit(k float64) (whatif.BatchBeyondFit, bool) {
	seen, ok := pv.fitBatch()
	if !ok {
		return whatif.BatchBeyondFit{}, false
	}
	if n := pv.batchAt(k); n > seen {
		b := whatif.BatchBeyondFit{Priced: n, Fitted: seen}
		if pv.state.Ranks > 1 {
			b.Ranks = pv.state.Ranks
		}
		return b, true
	}
	return whatif.BatchBeyondFit{}, false
}

// batchBeyondFitText is what a batchBeyondFit flag rests on, in plain words:
// the batch the line was fitted on and the most the figures flagged run.
func (pv pricedVariant) batchBeyondFitText(b whatif.BatchBeyondFit) string {
	return fmt.Sprintf("the decode line was fitted on up to %d sequences %s; this load runs about %d, so TPOT may read low and the decode knee high",
		b.Fitted, whatif.BatchUnit(b.Ranks), b.Priced)
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
	// The experimental TPOT model's spread is fitted, not host jitter.
	if _, ok := pv.hostBound(); !ok || slices.Contains(r.flags, hostBoundTPOTTail) || slices.Contains(r.flags, whatif.TPOTDistribution) {
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
