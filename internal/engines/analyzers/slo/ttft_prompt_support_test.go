package slo

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// SwitchTTFTPromptSupport is planner-only: off from the constructor.
func TestTTFT_ThePromptSupportOffFromTheConstructor(t *testing.T) {
	assert.False(t, New(nil, nil).ttftPromptSupport)
	assert.Contains(t, ReplaySwitchNames(), SwitchTTFTPromptSupport)
}

// SwitchTTFTPromptSupport on carries the longest prompt the histogram allows
// into the spread S1 prices; off, the spread bounds none, as before (issue
// llm-d-incubation/llm-d-scaling-manager#304). 2000-token prompts in a
// (1000, 2500] bucket: the longest is 1.25 times the mean.
func TestTTFT_ThePromptSupportSwitch(t *testing.T) {
	spread := func(on bool) float64 {
		rows := prefillRows("pre", 4000)
		rows[0].ComputedTokensBuckets = []domain.LengthBucket{bucket(1000, 0), bucket(2500, 2), bucket(math.Inf(1), 2)}
		r := newTTFTRig(ttftEstimate(), rows)
		require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchTTFTPromptSupport: on}))
		r.warmPromptWindow(t)
		return r.a.promptSpread("ns|m|pre", r.in.ReplicaMetrics, r.now, 0).MaxRatio
	}
	assert.InDelta(t, 1.25, spread(true), 1e-12)
	assert.Zero(t, spread(false))
}
