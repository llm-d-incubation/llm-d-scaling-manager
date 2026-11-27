package whatifsource

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// A capture files the model config with every variant of the one model it
// describes, and refuses a window of more than one model rather than guess
// which it describes. No config files nothing.
func TestCapture_FilesTheModelConfigWithEveryVariant(t *testing.T) {
	m := capacity.ParseModelConfig([]byte(`{"num_hidden_layers": 61, "num_attention_heads": 128, "kv_lora_rank": 512,
		"n_routed_experts": 256, "num_experts_per_tok": 8}`), "config.json")
	require.True(t, m.Read())

	one := []learnreplay.CaptureVariant{{ModelID: "m", Name: "decode"}, {ModelID: "m", Name: "prefill"}}
	require.NoError(t, fileModelConfig(one, m))
	for _, v := range one {
		assert.Equal(t, capacity.AttentionMLA, v.Params.AttentionKind(), v.Name)
		assert.Equal(t, capacity.MoETensorParallel, v.Params.MoEClass(), v.Name)
	}

	two := []learnreplay.CaptureVariant{{ModelID: "m", Name: "a"}, {ModelID: "n", Name: "b"}}
	err := fileModelConfig(two, m)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidOptions), err)
	assert.True(t, IsInputError(err))
	assert.Contains(t, err.Error(), "serves 2 (m, n)")
	assert.Equal(t, capacity.ModelConfig{}, two[0].Params.ModelConfig, "a refused config is filed with no variant")

	require.NoError(t, fileModelConfig(two, capacity.ModelConfig{}))
	assert.Equal(t, capacity.AttentionUnknown, two[0].Params.AttentionKind())
}
