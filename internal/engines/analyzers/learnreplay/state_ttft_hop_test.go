package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The planner's own decode term windows read slo.SwitchTTFTHopUnqueued: with
// it, a router window over prefill rows that held a capacity queue teaches no
// hop; without it, it does, as S1 reads it with the switch off.
func TestTerm_TheHopUnqueuedSwitch(t *testing.T) {
	hop := func(on bool) prefill.HopFit {
		s := newTTFTSamples(on)
		args := func(string) string { return "" }
		for i := range 80 {
			pre := prefillRow("p0", 8000, 1)
			pre.AvgTTFT, pre.AvgTTFTKnown, pre.AvgInputTokens, pre.AvgInputTokensRecent = 2, true, 8000, 8000
			pre.CapacityQueueKnown, pre.CapacityQueueLength, pre.QueueLength = true, 6, 6
			in := pdInput(pre, decodeRow("d0", 0.05, 1))
			in.RouterLatency = &domain.RouterLatency{WindowSeconds: 300, TTFTSumRate: 2.4, TTFTCountRate: 1}
			s.noteTerm(in, time.Unix(int64(15*i), 0), args)
		}
		return s.terms["dec"].Estimate().Hop
	}
	off := hop(false)
	require.Empty(t, off.Refused)
	assert.InDelta(t, 0.35, off.Base, 1e-9, "the queue's timing read as hop: 2.4 s less 2 and 0.05")
	assert.Equal(t, prefill.TooFewHopWindows, hop(true).Refused)
}

// StateAt turns the switch on, and a recording that ran no slo analyzer
// still replays: its slo switches are for the planner's own windows.
func TestPlannerSwitches_TheSLOSwitchNeedsNoSLOAnalyzer(t *testing.T) {
	assert.True(t, PlannerSwitches()[PackageSLO][slo.SwitchTTFTHopUnqueued])
	sw := map[string]map[string]bool{PackageSLO: PlannerSwitches()[PackageSLO]}
	r := &replayer{opts: Options{Switches: sw, sloOptional: true}}
	assert.NoError(t, r.setSwitches())
	r.opts.Switches = map[string]map[string]bool{PackageSLO: {"slo-no-such-switch": true}}
	assert.Error(t, r.setSwitches(), "a misspelled slo switch is an error with no slo analyzer too")
	r.opts.Switches = sw
	r.opts.sloOptional = false
	assert.Error(t, r.setSwitches(), "outside the planner, slo switches on a run with no slo analyzer are an error")
}
