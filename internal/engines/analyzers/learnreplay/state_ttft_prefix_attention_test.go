package learnreplay

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
)

// slo.SwitchTTFTPrefixAttention reaches the planner through the prefill
// state, off unless the replay's options turn it on: it is not in
// PlannerSwitches (issue llm-d-incubation/llm-d-scaling-manager#311).
func TestStateAt_ThePrefixAttentionSwitchReachesThePrefillState(t *testing.T) {
	gz, err := os.ReadFile(filepath.Join(goldenDir, ttftSaturatedFixture, RecordingName))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	assert.NotContains(t, PlannerSwitches()[PackageSLO], slo.SwitchTTFTPrefixAttention, "planner-only and off")
	for _, on := range []bool{false, true} {
		opts := Options{}
		if on {
			opts.Switches = map[string]map[string]bool{PackageSLO: {slo.SwitchTTFTPrefixAttention: true}}
		}
		st, err := StateAt(context.Background(), raw, opts, StateQuery{Cycle: 0})
		require.NoError(t, err)
		require.Len(t, st.Prefill, 1)
		assert.Equal(t, on, st.Prefill[0].PrefixAttention)
		// The recording has no prefix hit, as none committed has: V_P's
		// whole prompt, where read, carries a hit rate of 0.
		assert.Zero(t, st.Prefill[0].Estimate.VPPromptHitRate)
	}
}
