package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SwitchTTFTPrefixAttention is planner-only: off from the constructor, and a
// replay can turn it on (issue llm-d-incubation/llm-d-scaling-manager#311).
func TestTTFT_ThePrefixAttentionOffFromTheConstructor(t *testing.T) {
	assert.False(t, New(nil, nil).ttftPrefixAttention)
	assert.Contains(t, ReplaySwitchNames(), SwitchTTFTPrefixAttention)
	a := New(nil, nil)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchTTFTPrefixAttention: true}))
	assert.True(t, a.ttftPrefixAttention)
}
