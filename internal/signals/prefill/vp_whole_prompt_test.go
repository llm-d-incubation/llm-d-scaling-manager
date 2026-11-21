package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// V_P records the whole prompt and the prefix-cache hit rate of the minutes
// it rests on beside their effective prompt (issue
// llm-d-incubation/llm-d-scaling-manager#311), so the planner sees a
// question whose hits moved. Synthetic rows: no committed recording has a
// prefix hit.
func TestCapacity_VPRecordsTheWholePromptAndTheHit(t *testing.T) {
	// hitAt is a queued row computing 24000 tokens/s of eff-token prompts
	// out of whole-token prompts at hit rate hit, with its queue minimum read
	// above zero (a busy minute).
	hitAt := func(eff, whole, hit float64) Cycle {
		rm := base("p-0")
		rm.PrefillComputedTokenRate, rm.RequestRate = 24000, 24000/eff
		rm.AvgInputTokensRecent, rm.PrefixCacheHitRate = whole, hit
		rm.QueueLength, rm.QueueMin, rm.QueueMinKnown = 3, 1, true
		rm.AvgServiceTime, rm.AvgTTFT = 0.05, 0.5
		c := cycle(time.Time{}, rm)
		c.QueueMin = map[string]float64{"p-0": 1}
		return c
	}

	t.Run("busy minutes", func(t *testing.T) {
		cs := []Cycle{hitAt(2000, 8000, 0.75), hitAt(2000, 8000, 0.75), hitAt(4000, 16000, 0.75),
			hitAt(2000, 4000, 0.5), hitAt(2000, 4000, 0.5)}
		est := run(NewObserver(), cs...)
		require.Equal(t, SourceBusyMinutes, est.VPSource)
		assert.InDelta(t, 2400.0, est.VPPromptTokens, 1e-9)
		assert.InDelta(t, 8000.0, est.VPWholePromptTokens, 1e-9, "the mean of the five minutes' whole prompts")
		assert.InDelta(t, 0.65, est.VPPromptHitRate, 1e-12, "and of their hit rates")
	})

	t.Run("the peak's last PeakRun minutes", func(t *testing.T) {
		cs := make([]Cycle, 0, 4)
		for i, w := range []float64{1000, 3000, 6000, 9000} {
			c := hitAt(1500, w, 0.5)
			c.QueueMin = nil
			c.Rows[0].PrefillComputedTokenRate = float64(20000 + 1000*i)
			c.Rows[0].RequestRate = c.Rows[0].PrefillComputedTokenRate / 1500
			cs = append(cs, c)
		}
		est := run(NewObserver(), cs...)
		require.Equal(t, SourcePeakUnderQueue, est.VPSource)
		assert.InDelta(t, 6000.0, est.VPWholePromptTokens, 1e-9)
		assert.InDelta(t, 0.5, est.VPPromptHitRate, 1e-12)
	})

	t.Run("a minute with no whole prompt: both unknown", func(t *testing.T) {
		cs := []Cycle{hitAt(2000, 8000, 0.75), hitAt(2000, 0, 0.75), hitAt(2000, 8000, 0.75),
			hitAt(2000, 8000, 0.75), hitAt(2000, 8000, 0.75)}
		est := run(NewObserver(), cs...)
		require.Equal(t, SourceBusyMinutes, est.VPSource)
		assert.Equal(t, 2000.0, est.VPPromptTokens, "the effective prompt still reads")
		assert.Zero(t, est.VPWholePromptTokens)
		assert.Zero(t, est.VPPromptHitRate)
	})

	// No presence is carried for the hit-rate series: an absent one reads
	// 0 beside the whole prompt, the careful side (rowWholePrompt).
	t.Run("a minute with no hit series reads hit 0", func(t *testing.T) {
		cs := []Cycle{hitAt(2000, 2000, 0), hitAt(2000, 2000, 0), hitAt(2000, 2000, 0),
			hitAt(2000, 2000, 0), hitAt(2000, 2000, 0)}
		est := run(NewObserver(), cs...)
		require.Equal(t, SourceBusyMinutes, est.VPSource)
		assert.Equal(t, 2000.0, est.VPWholePromptTokens)
		assert.Zero(t, est.VPPromptHitRate)
	})

	t.Run("the back-pressure bound records none", func(t *testing.T) {
		c := cycle(t0, lowLoad(1000, 0.01, 1, 0.003))
		c.Rows[0].AvgInputTokensRecent, c.Rows[0].PrefixCacheHitRate = 4000, 0.75
		c.Bound = Bound{Tokens: 30000, Samples: MinBoundSamples}
		est := NewObserver().Observe(c)
		require.Equal(t, SourceBackPressureBound, est.VPSource)
		assert.Zero(t, est.VPWholePromptTokens)
		assert.Zero(t, est.VPPromptHitRate)
	})
}
