package learnreplay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The planner's switches are on in StateAt's replay unless the caller sets
// one, and every one is a switch its package accepts.
func TestPlannerSwitches_StateAtTurnsThemOn(t *testing.T) {
	got := withPlannerSwitches(nil)
	require.Equal(t, PlannerSwitches(), got)
	for name, on := range got[PackageSaturation] {
		assert.True(t, on, name)
	}
	a := saturation.NewSaturationAnalyzer(capacity.NewStore())
	require.NoError(t, a.SetSwitchesForReplay(got[PackageSaturation]))
	for name, on := range got[PackageSLO] {
		assert.True(t, on, name)
	}
	require.NoError(t, slo.New(nil, nil).SetSwitchesForReplay(got[PackageSLO]))

	// A caller's own setting wins, and its other switches are kept.
	mine := map[string]map[string]bool{
		PackageSaturation: {saturation.SwitchPrefillAEngineTiming: false, saturation.SwitchPrefillAQuadratic: false},
	}
	got = withPlannerSwitches(mine)
	assert.False(t, got[PackageSaturation][saturation.SwitchPrefillAEngineTiming])
	assert.False(t, got[PackageSaturation][saturation.SwitchPrefillAQuadratic])
	assert.True(t, got[PackageSaturation][saturation.SwitchITLAdaptiveBandFloor])
	assert.True(t, mine[PackageSaturation][saturation.SwitchPrefillAEngineTiming] == false &&
		len(mine[PackageSaturation]) == 2, "the caller's map is not written")
}
