package capacity

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// CUDAGraphsOff is EngineParams.CUDAGraphCaptureMax for an engine that
// captures no CUDA graph: vLLM -O0, or a cudagraph_mode of NONE.
const CUDAGraphsOff = -1

// The flags the capture ceiling is read from (applyCUDAGraphs).
const (
	cudaGraphSizesKey = "cuda_graph_sizes"
	compilationDotted = "compilation_config."
)

// maxDefaultCaptureTokens is vLLM's default largest capture size, in tokens:
// min(2 x max_num_seqs x (1 + num_speculative_tokens), 512).
const maxDefaultCaptureTokens = 512

// optimizationLevelShort reads vLLM's -O0 to -O3, the short spelling of
// --optimization-level, as the level; ok is false for any other token.
func optimizationLevelShort(tok string) (string, bool) {
	level, ok := strings.CutPrefix(tok, "-O")
	if !ok || len(level) != 1 || level[0] < '0' || level[0] > '3' {
		return "", false
	}
	return level, true
}

// joinSizes rejoins `--cuda-graph-sizes 1 2 4 8`, a list argparse takes as
// one value per token, so the value is every size; i is the index of the last
// token read and is returned moved past what was consumed. Only bare numbers
// are taken, so the positional model after the list is never swallowed.
func joinSizes(value string, args []string, i int) (string, int) {
	parts := []string{value}
	for i+1 < len(args) {
		if _, err := strconv.ParseInt(args[i+1], 10, 64); err != nil {
			break
		}
		parts = append(parts, args[i+1])
		i++
	}
	return strings.Join(parts, " "), i
}

// applyCUDAGraphs reads one of vLLM's capture flags into
// CUDAGraphCaptureMax. key is the flag, or the key of a dotted
// --compilation-config.<key>; a key that says nothing of the capture is
// applyUnknown. An unreadable value leaves the field as it was: the flag is
// read for a warning only, and is in no identity.
func applyCUDAGraphs(key, value string, params *EngineParams) applyResult {
	switch key {
	case "max_cudagraph_capture_size":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil || v < 0 {
			return applyUnusable
		}
		params.CUDAGraphCaptureMax = v
		if v == 0 {
			params.CUDAGraphCaptureMax = CUDAGraphsOff
		}
	case cudaGraphSizesKey, "cudagraph_capture_sizes":
		v, ok := largestSize(value)
		if !ok {
			return applyUnusable
		}
		params.CUDAGraphCaptureMax = v
	case "cudagraph_mode":
		switch {
		case strings.EqualFold(strings.Trim(value, `"' `), "none"):
			params.CUDAGraphCaptureMax = CUDAGraphsOff
		case params.CUDAGraphCaptureMax == CUDAGraphsOff:
			// A later mode turns the graphs back on after -O0: the
			// default capture again.
			params.CUDAGraphCaptureMax = 0
		}
	case "optimization_level":
		if strings.TrimSpace(value) == "0" {
			params.CUDAGraphCaptureMax = CUDAGraphsOff
		}
	case "compilation_config":
		var cfg map[string]any
		if err := json.Unmarshal([]byte(value), &cfg); err != nil {
			return applyUnusable
		}
		for _, k := range []string{"max_cudagraph_capture_size", "cudagraph_capture_sizes", "cudagraph_mode"} {
			if v, ok := cfg[k]; ok {
				b, _ := json.Marshal(v)
				applyCUDAGraphs(k, string(b), params)
			}
		}
	default:
		return applyUnknown
	}
	return applyOK
}

// largestSize is the largest positive integer in a list of capture sizes,
// written as JSON ("[1, 2, 4]") or separated by spaces or commas; ok is false
// with none.
func largestSize(value string) (int64, bool) {
	var best int64
	for _, f := range strings.FieldsFunc(value, func(r rune) bool { return r < '0' || r > '9' }) {
		if v, err := strconv.ParseInt(f, 10, 64); err == nil && v > best {
			best = v
		}
	}
	return best, best > 0
}

// GraphCeiling is the most sequences one engine rank runs inside a captured
// CUDA graph: past it a decode step runs eager. 0 with no graphs at all
// (--enforce-eager, -O0, a cudagraph_mode of NONE, SGLang's
// --disable-cuda-graph), and ok false where it cannot be told: SGLang with
// no --cuda-graph-max-bs (its default depends on the GPU and the parallel
// sizes), or an engine with no parameters.
//
// On vLLM the capture is in tokens and a decode step of n sequences takes
// n x (1 + k) of them with k speculative tokens, so the ceiling is the
// capture maximum over 1 + k. Unstated, the maximum is vLLM's default,
// min(2 x max_num_seqs x (1 + k), 512); maxNumSeqs is the cap the engine
// runs (a stated one, or the release's default).
func (p *EngineParams) GraphCeiling(maxNumSeqs int64) (int64, bool) {
	if p == nil {
		return 0, false
	}
	if p.EnforceEager || p.CUDAGraphCaptureMax == CUDAGraphsOff {
		return 0, true
	}
	if p.Engine == inferenceengine.EngineSGLang {
		return p.CUDAGraphCaptureMax, p.CUDAGraphCaptureMax > 0
	}
	step := int64(1 + p.SpeculativeTokens())
	tokens := p.CUDAGraphCaptureMax
	if tokens <= 0 {
		tokens = min(2*max(maxNumSeqs, 1)*step, maxDefaultCaptureTokens)
	}
	return tokens / step, true
}

// StepBudget is the tokens one engine rank schedules per step, its
// max_num_batched_tokens as resolved (EffectiveMaxBatchedTokens: the flag
// where stated, else the release's default), else the flag; 0 where neither
// is known.
func (p *EngineParams) StepBudget() int64 {
	switch {
	case p == nil:
		return 0
	case p.EffectiveMaxBatchedTokens > 0:
		return p.EffectiveMaxBatchedTokens
	}
	return max(p.MaxNumBatchedTokens, 0)
}
