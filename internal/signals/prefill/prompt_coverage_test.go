package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The prompts a V_P speaks for (Cycle.VPPromptCoverage; issue
// llm-d-incubation/llm-d-scaling-manager#405).

// coverageMinutes is one busy minute of p-0 per prompt, each computing 30k
// tok/s, with the coverage switch set to on.
func coverageMinutes(on bool, prompts ...float64) []Cycle {
	out := make([]Cycle, len(prompts))
	for i, p := range prompts {
		rm := queued("p-0", 30000, 3)
		rm.RequestRate = 30000 / p
		out[i] = cycle(t0.Add(minute*time.Duration(i)), rm)
		out[i].QueueMin = map[string]float64{"p-0": 1}
		out[i].VPPromptCoverage = on
	}
	return out
}

// Busy minutes carry the 10th and 90th percentiles of their prompts, by the
// nearest rank: of ten minutes at 1k to 10k, the first and the ninth.
func TestPromptCoverage_BusyMinutesCarryTheirRange(t *testing.T) {
	est := run(NewObserver(), coverageMinutes(true, 4000, 1000, 9000, 2000, 6000, 3000, 10000, 5000, 7000, 8000)...)
	require.Equal(t, SourceBusyMinutes, est.VPSource)
	assert.InDelta(t, 1000.0, est.VPPromptLow, 1e-6)
	assert.InDelta(t, 9000.0, est.VPPromptHigh, 1e-6)
	assert.InDelta(t, 5500.0, est.VPPromptTokens, 1e-6, "the mean is kept beside the range")

	// A peak under a queue carries its run's minutes: the last three, the
	// fastest run.
	cs := coverageMinutes(true, 1000, 2000, 8000, 8100, 7900)
	for i := range cs {
		cs[i].QueueMin = nil
		if i < 2 {
			cs[i].Rows[0].PrefillComputedTokenRate = 20000
			cs[i].Rows[0].RequestRate = 20000 / []float64{1000, 2000}[i]
		}
	}
	pk := run(NewObserver(), cs...)
	require.Equal(t, SourcePeakUnderQueue, pk.VPSource)
	assert.InDelta(t, 7900.0, pk.VPPromptLow, 1e-6)
	assert.InDelta(t, 8100.0, pk.VPPromptHigh, 1e-6)
}

// Off, no range is carried, and V_P is unchanged.
func TestPromptCoverage_OffCarriesNone(t *testing.T) {
	prompts := []float64{4000, 1000, 9000, 2000, 6000}
	on := run(NewObserver(), coverageMinutes(true, prompts...)...)
	off := run(NewObserver(), coverageMinutes(false, prompts...)...)
	assert.Zero(t, off.VPPromptLow)
	assert.Zero(t, off.VPPromptHigh)
	assert.Equal(t, on.VP, off.VP)
	assert.Equal(t, on.VPPromptTokens, off.VPPromptTokens)
	assert.Positive(t, on.VPPromptHigh)
}

// A range is a reading of every minute: one minute with no prompt read makes
// it unknown, as the mean is.
func TestPromptCoverage_AnUnreadPromptIsNoRange(t *testing.T) {
	lo, hi := promptRange([]float64{1000, 0, 3000})
	assert.Zero(t, lo)
	assert.Zero(t, hi)
	lo, hi = promptRange([]float64{3000})
	assert.Equal(t, 3000.0, lo)
	assert.Equal(t, 3000.0, hi)
}

// The occupancy plateau carries the prompts of its top bin's minutes, as the
// other sources carry theirs.
func TestPromptCoverage_PlateauCarriesItsRange(t *testing.T) {
	cs := flatRamp()
	for i := range cs {
		cs[i].VPPromptCoverage = true
	}
	est := run(NewObserver(), cs...)
	require.Equal(t, SourceOccupancyPlateau, est.VPSource)
	assert.InDelta(t, plateauPrompt, est.VPPromptLow, 1e-6)
	assert.InDelta(t, plateauPrompt, est.VPPromptHigh, 1e-6)
}

// A lone-rate V_P carries the range of the lone minutes it was timed at,
// not that of the source it replaced: three queued minutes at 8000-token
// prompts give a lower bound of 10k tok/s, then thirteen lone minutes at
// 1800 to 2350 tokens time 20k. V_P is the lone rate, and speaks for 1850
// to 2300 (the nearest-rank 10th and 90th of the thirteen).
func TestPromptCoverage_LoneRateCarriesItsRange(t *testing.T) {
	cs := make([]Cycle, 0, 16)
	for range 3 {
		rm := queued("p-0", 10000, 3)
		rm.RequestRate = 10000.0 / 8000
		cs = append(cs, cycle(time.Time{}, rm))
	}
	for i := range 12 {
		cs = append(cs, cycle(time.Time{}, atOccupancy(1800+50*float64(i), 20000, 0.1, 1)))
	}
	cs = append(cs, cycle(time.Time{}, atOccupancy(2000, 20000, 0.1, 1)))
	for i := range cs {
		cs[i].VPLoneRate, cs[i].VPPromptCoverage = true, true
	}
	est := run(NewObserver(), cs...)
	require.Equal(t, SourceLoneRate, est.VPSource, "%+v", est)
	assert.InDelta(t, 20000.0, est.VP, 1e-6)
	assert.InDelta(t, 1850.0, est.VPPromptLow, 1e-6)
	assert.InDelta(t, 2300.0, est.VPPromptHigh, 1e-6)

	for i := range cs {
		cs[i].VPPromptCoverage = false
	}
	off := run(NewObserver(), cs...)
	assert.Equal(t, SourceLoneRate, off.VPSource)
	assert.Zero(t, off.VPPromptLow, "no range without the coverage switch")
	assert.Zero(t, off.VPPromptHigh)
}
