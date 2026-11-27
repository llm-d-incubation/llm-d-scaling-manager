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
	// Attention is the model's attention kind (full, sliding-window, mla,
	// hybrid) and MoE its MoE class (dense, moe-tensor-parallel,
	// moe-expert-parallel), "unknown" where no model config was read and,
	// for MoE, no expert-parallel flag says it. ModelConfigSource is where
	// the config was read, absent with none. Reported, not priced.
	Attention         string `json:"attention"`
	MoE               string `json:"moe"`
	ModelConfigSource string `json:"modelConfigSource,omitempty"`
}

// engineOf is the wire's account of the engine p describes; nil with none.
func engineOf(p *capacity.EngineParams) *wireEngine {
	if p == nil {
		return nil
	}
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
		Attention: p.AttentionKind().String(), MoE: p.MoEClass().String(), ModelConfigSource: p.ModelConfig.Source}
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

// variantEngine is the wire's account of the named variant's engine; nil
// where the state holds no parameters for it.
func variantEngine(st *learnreplay.State, variant string) *wireEngine {
	return engineOf(variantParams(st, variant))
}

// engineInfo is the API's account of the engine p describes, the CLI's
// figures with engineText as its summary; nil with none.
func engineInfo(p *capacity.EngineParams) *whatifapi.EngineInfo {
	w := engineOf(p)
	if w == nil {
		return nil
	}
	return &whatifapi.EngineInfo{Engine: w.Engine, Summary: engineText(p),
		VLLMVersion: w.VLLMVersion, VLLMVersionFrom: w.VLLMVersionFrom,
		StepBudget: w.StepBudget, StepBudgetSource: w.StepBudgetSource,
		GPUMemoryUtilization: w.GPUMemoryUtilization, GPUMemoryUtilizationSource: w.GPUMemoryUtilizationSource,
		BlockSize: w.BlockSize, BlockSizeSource: w.BlockSizeSource,
		Attention: w.Attention, MoE: w.MoE, ModelConfigSource: w.ModelConfigSource, Model: modelText(p)}
}

// modelText is what the model config says of the model p serves, as the
// engine row prints it: "model mla attention (kv_lora_rank 512),
// moe-expert-parallel, DP 8 (model config annotation)". Empty where neither
// class is known, so a recording without a config prints the row it did.
func modelText(p *capacity.EngineParams) string {
	attention, moe := p.AttentionKind(), p.MoEClass()
	if attention == capacity.AttentionUnknown && moe == capacity.MoEUnknown {
		return ""
	}
	text := "model " + attention.String() + " attention"
	if attention != capacity.AttentionUnknown {
		text = "model " + p.ModelConfig.AttentionText()
	}
	text += ", " + p.MoEText()
	if src := p.ModelConfig.Source; src != "" {
		return fmt.Sprintf("%s (model config %s)", text, src)
	}
	return text + " (from --enable-expert-parallel; no model config)"
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
func engineRowText(p *capacity.EngineParams) string {
	if e := engineInfo(p); e != nil {
		return engineInfoText(e)
	}
	return engineText(p)
}

// engineInfoText is engineRowText from the API's account of an engine, as
// the at-replicas report prints it.
func engineInfoText(e *whatifapi.EngineInfo) string {
	model := ""
	if e.Model != "" {
		model = "; " + e.Model + ", " + notPricedYetWording
	}
	if e.StepBudgetSource == engineNotRecorded {
		return e.Summary + model
	}
	return fmt.Sprintf("%s; step budget %d (%s), %s; %s: GPU memory %s (%s), block size %d (%s)%s", e.Summary,
		e.StepBudget, e.StepBudgetSource, stepBudgetPricedWording, notPricedYetWording, num(e.GPUMemoryUtilization),
		e.GPUMemoryUtilizationSource, e.BlockSize, e.BlockSizeSource, model)
}
