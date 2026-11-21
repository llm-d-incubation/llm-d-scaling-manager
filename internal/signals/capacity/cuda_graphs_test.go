package capacity

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// The capture ceiling is read from every spelling vLLM takes, and none of
// them enters an identity: an unreadable value records nothing.
func TestCUDAGraphCaptureMaxFromFlags(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want int64
	}{
		{"unstated", []string{"serve", "m"}, 0},
		{"max capture", []string{"--max-cudagraph-capture-size", "128"}, 128},
		{"max capture zero is no graphs", []string{"--max-cudagraph-capture-size=0"}, CUDAGraphsOff},
		{"sizes as tokens", []string{"--cuda-graph-sizes", "1", "2", "64", "8", "serve-model"}, 64},
		{"sizes as one value", []string{"--cuda-graph-sizes=[1,2,4,256]"}, 256},
		{"compilation config", []string{"--compilation-config", `{"cudagraph_capture_sizes": [1, 4, 96]}`}, 96},
		{"compilation config max", []string{"--compilation-config", `{"max_cudagraph_capture_size": 64}`}, 64},
		{"compilation config none", []string{"--compilation-config", `{"cudagraph_mode": "NONE"}`}, CUDAGraphsOff},
		{"dotted", []string{"--compilation-config.max_cudagraph_capture_size", "32"}, 32},
		{"O0", []string{"-O0"}, CUDAGraphsOff},
		{"O0 then a mode", []string{"-O0", "--compilation-config", `{"cudagraph_mode": "FULL"}`}, 0},
		{"O3", []string{"-O3"}, 0},
		{"optimization level 0", []string{"--optimization-level", "0"}, CUDAGraphsOff},
		{"unreadable", []string{"--max-cudagraph-capture-size", "lots"}, 0},
	} {
		var p EngineParams
		parseArgs(c.args, &p, nil)
		assert.Equal(t, c.want, p.CUDAGraphCaptureMax, c.name)
		assert.Empty(t, p.Unresolved, "%s: a capture flag is in no identity", c.name)
	}
}

// The list's numbers are taken, and the positional model after it is not.
func TestCUDAGraphSizesStopAtTheModel(t *testing.T) {
	var p EngineParams
	parseArgs([]string{"--cuda-graph-sizes", "8", "16", "Qwen/Qwen3-0.6B", "--max-num-seqs", "64"}, &p, nil)
	assert.Equal(t, int64(16), p.CUDAGraphCaptureMax)
	assert.Equal(t, int64(64), p.MaxNumSeqs)
}

// SGLang's capture is in requests, and its mixed chunk is read.
func TestSGLangCUDAGraphsAndMixedChunk(t *testing.T) {
	p := EngineParams{Engine: inferenceengine.EngineSGLang}
	parseArgsWith([]string{"--cuda-graph-max-bs", "160", "--enable-mixed-chunk"}, &p, nil, applySGLangParam)
	assert.Equal(t, int64(160), p.CUDAGraphCaptureMax)
	assert.True(t, p.MixedChunk)
	ceiling, ok := p.GraphCeiling(256)
	assert.True(t, ok)
	assert.Equal(t, int64(160), ceiling)

	unstated := EngineParams{Engine: inferenceengine.EngineSGLang}
	_, ok = unstated.GraphCeiling(256)
	assert.False(t, ok, "SGLang's default capture depends on the GPU: not told")
}

// vLLM's ceiling in sequences: the capture in tokens over 1 + k, the default
// min(2 x max_num_seqs x (1 + k), 512).
func TestGraphCeiling(t *testing.T) {
	for _, c := range []struct {
		name    string
		p       EngineParams
		maxSeqs int64
		want    int64
	}{
		{"default at 256", EngineParams{}, 256, 512},
		{"default at 1024", EngineParams{}, 1024, 512},
		{"default at 64", EngineParams{}, 64, 128},
		{"MTP k=3", EngineParams{Engine: inferenceengine.EngineVLLM, SpeculativeConfig: `{"method":"mtp","num_speculative_tokens":3}`}, 256, 128},
		{"stated", EngineParams{CUDAGraphCaptureMax: 96}, 256, 96},
		{"stated with k=1", EngineParams{Engine: inferenceengine.EngineVLLM, CUDAGraphCaptureMax: 96, SpeculativeConfig: `{"num_speculative_tokens":1}`}, 256, 48},
		{"eager", EngineParams{EnforceEager: true}, 256, 0},
		{"off", EngineParams{CUDAGraphCaptureMax: CUDAGraphsOff}, 256, 0},
	} {
		got, ok := c.p.GraphCeiling(c.maxSeqs)
		assert.True(t, ok, c.name)
		assert.Equal(t, c.want, got, c.name)
	}
}

// vLLM's max_num_seqs default follows the step budget's tiers: 1024 where
// the budget is 8192 or more, 256 below, and 256 on an accelerator this
// package cannot size.
func TestVLLMDefaultMaxNumSeqs(t *testing.T) {
	v := VLLMVersion{Major: 0, Minor: 34}
	for acc, want := range map[string]int64{
		"NVIDIA-H100-80GB-HBM3": 1024, "H200": 1024, "B200": 1024,
		"NVIDIA-A100-SXM4-80GB": 256, "L4": 256, "GB10": 256, "": 256, "mystery-gpu": 256,
	} {
		assert.Equal(t, want, VLLMDefaultMaxNumSeqs(v, acc), acc)
	}
	assert.Equal(t, int64(1024), VLLMDefaultMaxNumSeqs(VLLMVersion{}, "H100"), "every release runs 1024 on an H100")
}

// The extensible KV cache: on by default from v0.34 unless a flag or a KV
// connector keeps it off; never on before v0.34 without the flag.
func TestExtensibleKVCacheOn(t *testing.T) {
	v34, v33 := VLLMVersion{Major: 0, Minor: 34}, VLLMVersion{Major: 0, Minor: 33}
	assert.True(t, (&EngineParams{VLLMVersion: v34}).ExtensibleKVCacheOn())
	assert.False(t, (&EngineParams{VLLMVersion: v33}).ExtensibleKVCacheOn())
	assert.True(t, (&EngineParams{VLLMVersion: v33, ExtensibleKVCache: 1}).ExtensibleKVCacheOn())
	assert.False(t, (&EngineParams{VLLMVersion: v34, ExtensibleKVCache: -1}).ExtensibleKVCacheOn())
	assert.False(t, (&EngineParams{VLLMVersion: v34, KVConnector: "NixlConnector"}).ExtensibleKVCacheOn())
	assert.False(t, (&EngineParams{Engine: inferenceengine.EngineSGLang, VLLMVersion: v34}).ExtensibleKVCacheOn())
}
