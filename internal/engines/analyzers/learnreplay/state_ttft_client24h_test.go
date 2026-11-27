package learnreplay

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
)

// stateWith is the saturated TTFT golden's prefill state at its first cycle,
// with the switches given.
func stateWith(t *testing.T, sw map[string]map[string]bool) PrefillState {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join(goldenDir, ttftSaturatedFixture, RecordingName))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	st, err := StateAt(context.Background(), raw, Options{Switches: sw}, StateQuery{Cycle: 0})
	require.NoError(t, err)
	require.Len(t, st.Prefill, 1)
	return st.Prefill[0]
}

// saturation.SwitchPrefillVPAtServedWork reaches the planner through the
// prefill state, off unless the replay's options turn it on, and is not among
// the planner's switches.
func TestStateAt_TheVPAtServedWorkSwitchReachesThePrefillState(t *testing.T) {
	assert.NotContains(t, PlannerSwitches()[PackageSaturation], saturation.SwitchPrefillVPAtServedWork, "planner-only and off")
	assert.False(t, stateWith(t, nil).VPAtServedWork)
	assert.True(t, stateWith(t, map[string]map[string]bool{PackageSaturation: {saturation.SwitchPrefillVPAtServedWork: true}}).VPAtServedWork)
}

// saturation.SwitchPrefillHitAtCount reaches the planner through the prefill
// state, off unless the replay's options turn it on, and is not among the
// planner's switches.
func TestStateAt_TheHitAtCountSwitchReachesThePrefillState(t *testing.T) {
	assert.NotContains(t, PlannerSwitches()[PackageSaturation], saturation.SwitchPrefillHitAtCount, "planner-only and off")
	assert.False(t, stateWith(t, nil).HitAtCount)
	assert.True(t, stateWith(t, map[string]map[string]bool{PackageSaturation: {saturation.SwitchPrefillHitAtCount: true}}).HitAtCount)
}

// slo.SwitchTTFTTierKVWait reaches the planner through the prefill state, off
// unless the replay's options turn it on, and is not among the planner's
// switches.
func TestStateAt_TheTierKVWaitSwitchReachesThePrefillState(t *testing.T) {
	assert.NotContains(t, PlannerSwitches()[PackageSLO], slo.SwitchTTFTTierKVWait, "planner-only and off")
	assert.False(t, stateWith(t, nil).TierKVWait)
	assert.True(t, stateWith(t, map[string]map[string]bool{PackageSLO: {slo.SwitchTTFTTierKVWait: true}}).TierKVWait)
}

// slo.SwitchTTFTPSByEngine reaches the planner through the prefill state, off
// unless the replay's options turn it on, and is not among the planner's
// switches.
func TestStateAt_ThePSByEngineSwitchReachesThePrefillState(t *testing.T) {
	assert.NotContains(t, PlannerSwitches()[PackageSLO], slo.SwitchTTFTPSByEngine, "planner-only and off")
	assert.False(t, stateWith(t, nil).PSByEngine)
	assert.True(t, stateWith(t, map[string]map[string]bool{PackageSLO: {slo.SwitchTTFTPSByEngine: true}}).PSByEngine)
}
