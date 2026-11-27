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
// one, but for the floor's B and the held floor B, which are off, and every
// one is a switch its package accepts.
func TestPlannerSwitches_StateAtTurnsThemOn(t *testing.T) {
	got := withPlannerSwitches(nil)
	require.Equal(t, PlannerSwitches(), got)
	off := map[string]bool{saturation.SwitchITLBaselineFromFloor: true, saturation.SwitchITLFloorBaselineHeld: true}
	for name, on := range got[PackageSaturation] {
		assert.Equal(t, !off[name], on, name)
	}
	for name := range off {
		assert.Contains(t, got[PackageSaturation], name, "the planner sets it off, whatever the constructor says")
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

// The TTFT model's switches as v0.12.3 ships them: the served peak on a
// fleet that never queued and the speculating decode row's held TTFT are
// on; every other TTFT switch is off, and only an explicit replay option
// (the capacity planner's --experimental-ttft) turns one on.
func TestPlannerSwitches_TheTTFTModelStates(t *testing.T) {
	sw := PlannerSwitches()
	assert.True(t, sw[PackageSaturation][saturation.SwitchPrefillVPServedUnqueued])
	assert.True(t, sw[PackageSLO][slo.SwitchTTFTDecodeSpeculatingHeld])
	for _, name := range []string{saturation.SwitchPrefillVPWindowBest, saturation.SwitchPrefillVPAtServedWork,
		saturation.SwitchPrefillVPOccupancyPlateau, saturation.SwitchPrefillVPPromptCoverage,
		saturation.SwitchPrefillVPLoneRate, saturation.SwitchPrefillVPServiceLine, saturation.SwitchPrefillHitAtCount,
		saturation.SwitchPrefillFLoadCurve} {
		assert.NotContains(t, sw[PackageSaturation], name, "off in the planner by default")
	}
	for _, name := range []string{slo.SwitchTTFTComputedHit, slo.SwitchTTFTTierLoad, slo.SwitchTTFTTierKVWait,
		slo.SwitchTTFTPSByEngine, slo.SwitchTTFTHopWindowMeans, slo.SwitchTTFTCachedTokenCost} {
		assert.NotContains(t, sw[PackageSLO], name, "off in the planner by default")
	}
}

// The planner prices each backtest step at its own output spread by default
// (saturation.SwitchBacktestStepOutputDispersion, issue
// llm-d-incubation/llm-d-scaling-manager#274), and StateAt's replay turns it
// on unless the caller sets it; the controller's constructor keeps it off
// (TestPlannerSwitches_OffFromTheConstructor).
func TestPlannerSwitches_TheStepOutputDispersionIsOn(t *testing.T) {
	on, set := PlannerSwitches()[PackageSaturation][saturation.SwitchBacktestStepOutputDispersion]
	assert.True(t, set, "PlannerSwitches names the step spread")
	assert.True(t, on, "the planner prices each backtest step at its own output spread")
	assert.True(t, withPlannerSwitches(nil)[PackageSaturation][saturation.SwitchBacktestStepOutputDispersion])
}
