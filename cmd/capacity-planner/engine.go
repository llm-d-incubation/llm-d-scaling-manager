package main

import (
	"fmt"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// What the engine figures were read from: the engine's flag, the default
// its release runs (docs/reference/vllm-versions.md), or nothing a recording
// made before the release was read can say.
const (
	engineSetByFlag     = "flag"
	engineDefault       = "default"
	engineNotRecorded   = "not recorded"
	notPricedYetWording = "for reference, not priced yet"
	// stepBudgetPricedWording says how the answers read the step budget.
	stepBudgetPricedWording = "priced as a cap on the batch per rank"
)

// wireEngine is the engine a variant runs: which one, for vLLM the release
// and where it was read from, and the figures that release's defaults decide
// where the engine's flags set none. Of these, the answers read only the step
// budget, as a cap on the batch per rank (saturation's
// SwitchPlannerCapAtStepBudget); the rest are reported for reference.
type wireEngine struct {
	Engine string `json:"engine"`
	// VLLMVersion is the release ("0.34"), absent when unknown, and
	// VLLMVersionFrom where it was read: annotation, image tag,
	// --vllm-version or --engine-image.
	VLLMVersion     string `json:"vllmVersion,omitempty"`
	VLLMVersionFrom string `json:"vllmVersionFrom,omitempty"`
	// StepBudget is max_num_batched_tokens per step, GPUMemoryUtilization
	// the memory share and BlockSize the KV block, each with its source:
	// flag or default.
	StepBudget                 int64   `json:"stepBudget"`
	StepBudgetSource           string  `json:"stepBudgetSource"`
	GPUMemoryUtilization       float64 `json:"gpuMemoryUtilization"`
	GPUMemoryUtilizationSource string  `json:"gpuMemoryUtilizationSource"`
	BlockSize                  int64   `json:"blockSize"`
	BlockSizeSource            string  `json:"blockSizeSource"`
	// StepBudgetBinding is whether the step budget binds, read from the
	// engine's tokens per iteration (learnreplay.VariantState.StepBudgetBinding).
	// Reported only: no figure reads it yet.
	StepBudgetBinding *whatifapi.StepBudgetBinding `json:"stepBudgetBinding,omitempty"`
}

// engineOf is the wire's account of the engine v runs; nil with no variant
// or no parameters.
func engineOf(v *learnreplay.VariantState) *wireEngine {
	if v == nil || v.Params == nil {
		return nil
	}
	p := v.Params
	source := func(set bool) string {
		switch {
		case notRecorded(p):
			return engineNotRecorded
		case set:
			return engineSetByFlag
		}
		return engineDefault
	}
	return &wireEngine{Engine: p.Engine.String(),
		VLLMVersion: p.VLLMVersion.String(), VLLMVersionFrom: p.VLLMVersionFrom,
		StepBudget: p.EffectiveMaxBatchedTokens, StepBudgetSource: source(p.MaxNumBatchedTokens > 0),
		GPUMemoryUtilization: p.GpuMemoryUtilization, GPUMemoryUtilizationSource: source(p.GpuMemoryUtilizationSet),
		BlockSize: p.BlockSize, BlockSizeSource: source(p.BlockSizeSet),
		StepBudgetBinding: stepBudgetBindingOf(v)}
}

// stepBudgetBindingOf is the API's account of whether v's step budget binds;
// nil where v's rows carried no tokens-per-iteration series, so the answers of
// a recording made before the series was recorded, or of an SGLang engine,
// read as before. The class is then unknown (capacity.StepBudgetNoSeries).
func stepBudgetBindingOf(v *learnreplay.VariantState) *whatifapi.StepBudgetBinding {
	if len(v.StepTokens) == 0 {
		return nil
	}
	b := v.StepBudgetBinding()
	return &whatifapi.StepBudgetBinding{Class: string(b.Class), Reason: b.Reason, NearTokens: b.NearTokens,
		Share: b.Share, OverShare: b.OverShare, TopTokens: b.TopTokens, TopOpen: b.TopOpen}
}

// stepBudgetBindingText is b in the report's words: "binds (12% of
// iterations above 4096 tokens)", "does not bind (0.2% of iterations above
// 4096 tokens)", "unknown (no tokens-per-iteration series)"; with the
// observed top beside it where read.
func stepBudgetBindingText(b *whatifapi.StepBudgetBinding) string {
	top := ""
	if b.TopTokens > 0 {
		top = fmt.Sprintf("; 99.9%% of iterations at or under %s tokens", num(b.TopTokens))
		if b.TopOpen {
			top = fmt.Sprintf("; iterations above the top bound, %s tokens", num(b.TopTokens))
		}
	}
	switch capacity.StepBudgetClass(b.Class) {
	case capacity.StepBudgetBinds:
		return fmt.Sprintf("binds (%s%% of iterations above %s tokens%s)", num(100*b.Share), num(b.NearTokens), top)
	case capacity.StepBudgetDoesNotBind:
		return fmt.Sprintf("does not bind (%s%% of iterations above %s tokens%s)", num(100*b.Share), num(b.NearTokens), top)
	}
	return fmt.Sprintf("unknown (%s%s)", b.Reason, top)
}

// variantParams is the engine parameters the state holds for the named
// variant; nil with none.
func variantParams(st *learnreplay.State, variant string) *capacity.EngineParams {
	for i := range st.Variants {
		if st.Variants[i].Variant == variant {
			return st.Variants[i].Params
		}
	}
	return nil
}

// variantGPUs is the named variant's GPUs per replica as the state holds
// them; 0 where it holds none.
func variantGPUs(st *learnreplay.State, variant string) int {
	for i := range st.Variants {
		if st.Variants[i].Variant == variant {
			return st.Variants[i].GPUsPerReplica
		}
	}
	return 0
}

// variantState is the state of the named variant; nil where the state holds
// none.
func variantState(st *learnreplay.State, variant string) *learnreplay.VariantState {
	for i := range st.Variants {
		if st.Variants[i].Variant == variant {
			return &st.Variants[i]
		}
	}
	return nil
}

// variantEngine is the wire's account of the named variant's engine; nil
// where the state holds no parameters for it.
func variantEngine(st *learnreplay.State, variant string) *wireEngine {
	return engineOf(variantState(st, variant))
}

// engineInfo is the API's account of the engine v runs, the CLI's figures
// with engineText as its summary; nil with none.
func engineInfo(v *learnreplay.VariantState) *whatifapi.EngineInfo {
	w := engineOf(v)
	if w == nil {
		return nil
	}
	return &whatifapi.EngineInfo{Engine: w.Engine, Summary: engineText(v.Params),
		VLLMVersion: w.VLLMVersion, VLLMVersionFrom: w.VLLMVersionFrom,
		StepBudget: w.StepBudget, StepBudgetSource: w.StepBudgetSource,
		GPUMemoryUtilization: w.GPUMemoryUtilization, GPUMemoryUtilizationSource: w.GPUMemoryUtilizationSource,
		BlockSize: w.BlockSize, BlockSizeSource: w.BlockSizeSource, StepBudgetBinding: w.StepBudgetBinding}
}

// notRecorded reports whether p is a vLLM engine's from a recording made
// before the release was read: it names no release and no reason why, and
// its figures' sources were not recorded either.
func notRecorded(p *capacity.EngineParams) bool {
	return p.Engine != inferenceengine.EngineSGLang && !p.VLLMVersion.Known() && p.VLLMVersionFrom == ""
}

// engineText names the engine p describes and, for vLLM, the release and
// where it was read from: "vLLM 0.34 (from image tag)".
func engineText(p *capacity.EngineParams) string {
	switch {
	case p == nil:
		return "engine not recorded"
	case p.Engine == inferenceengine.EngineSGLang:
		return "SGLang"
	case notRecorded(p):
		return "vLLM release not recorded"
	case !p.VLLMVersion.Known():
		return "vLLM release unknown"
	case p.VLLMVersionFrom == "":
		return "vLLM " + p.VLLMVersion.String()
	}
	return fmt.Sprintf("vLLM %s (from %s)", p.VLLMVersion, p.VLLMVersionFrom)
}

// engineRowText is engineText with the figures the release decides where
// the flags do not: the step budget, the memory share and the block size,
// said to be reported only, as no answer reads them yet. A recording made
// before the release was read gets engineText alone: its figures' sources
// are not known.
func engineRowText(v *learnreplay.VariantState) string {
	if e := engineInfo(v); e != nil {
		return engineInfoText(e)
	}
	return engineText(nil)
}

// engineInfoText is engineRowText from the API's account of an engine, as
// the at-replicas report prints it.
func engineInfoText(e *whatifapi.EngineInfo) string {
	if e.StepBudgetSource == engineNotRecorded {
		return e.Summary
	}
	s := fmt.Sprintf("%s; step budget %d (%s), %s; %s: GPU memory %s (%s), block size %d (%s)", e.Summary,
		e.StepBudget, e.StepBudgetSource, stepBudgetPricedWording, notPricedYetWording, num(e.GPUMemoryUtilization),
		e.GPUMemoryUtilizationSource, e.BlockSize, e.BlockSizeSource)
	if b := e.StepBudgetBinding; b != nil {
		s += "; step budget binding " + stepBudgetBindingText(b) + ", " + notPricedYetWording
	}
	return s
}
