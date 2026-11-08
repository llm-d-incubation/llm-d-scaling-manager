package capacity

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// SpeculativeTokens reads num_speculative_tokens from the flags as the parser
// stores them, in both vLLM spellings, and gives 0 (no bound) for anything it
// cannot read as a positive integer. The saturation analyzer withholds an ITL
// reading whose accepted count is over it (ev-shindin/llm-scaling-manager#274), so a parser change that
// renamed the stored key would turn that bound off without a word.
func TestSpeculativeTokens(t *testing.T) {
	vllm := func(args ...string) *EngineParams {
		p := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
			append([]string{"vllm", "serve", "m"}, args...))))
		return &p
	}
	for _, tc := range []struct {
		name string
		p    *EngineParams
		want int
	}{
		{"json", vllm("--speculative-config", `{"method":"mtp","num_speculative_tokens":3}`), 3},
		{"dotted", vllm("--speculative-config.num_speculative_tokens", "3", "--speculative-config.method", "mtp"), 3},
		{"json zero", vllm("--speculative-config", `{"method":"mtp","num_speculative_tokens":0}`), 0},
		{"json fraction", vllm("--speculative-config", `{"method":"mtp","num_speculative_tokens":2.5}`), 0},
		{"json string", vllm("--speculative-config", `{"method":"mtp","num_speculative_tokens":"3"}`), 0},
		{"json not stated", vllm("--speculative-config", `{"method":"ngram"}`), 0},
		{"malformed json", vllm("--speculative-config", `{"method":`), 0},
		{"dotted zero", vllm("--speculative-config.num_speculative_tokens", "0"), 0},
		{"no speculation", vllm(), 0},
		{"sglang", &EngineParams{Engine: inferenceengine.EngineSGLang, SpeculativeConfig: "num_draft_tokens=4"}, 0},
		{"nil", nil, 0},
	} {
		assert.Equal(t, tc.want, tc.p.SpeculativeTokens(), tc.name)
	}
}
