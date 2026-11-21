package whatifapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cyclesAt is n cycles every 15 s from t0, the cycles in skip left out.
func cyclesAt(t0 time.Time, n int, skip func(i int) bool) []time.Time {
	var out []time.Time
	for i := range n {
		if skip == nil || !skip(i) {
			out = append(out, t0.Add(time.Duration(i)*15*time.Second))
		}
	}
	return out
}

func TestWindowGapsOf(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 2, 18, 0, 0, time.UTC)

	whole := WindowGapsOf(cyclesAt(t0, 289, nil))
	require.NotNil(t, whole)
	assert.Equal(t, 289, whole.ExpectedCycles)
	assert.Equal(t, 289, whole.PresentCycles)
	assert.Zero(t, whole.LargestGapSeconds)
	assert.False(t, whole.Flagged)
	assert.Empty(t, whole.Text(), "no hole, no words")

	// 60 cycles (15 minutes) out of 289: 21% missing, flagged.
	holed := WindowGapsOf(cyclesAt(t0, 289, func(i int) bool { return i >= 100 && i < 160 }))
	require.NotNil(t, holed)
	assert.Equal(t, 289, holed.ExpectedCycles)
	assert.Equal(t, 229, holed.PresentCycles)
	assert.InDelta(t, 60.0/289, holed.MissingShare, 1e-9)
	assert.InDelta(t, 15*60, holed.LargestGapSeconds, 1e-9)
	assert.True(t, holed.Flagged)
	assert.Equal(t, "289 cycles expected, 229 present (21% missing), largest gap 15m0s from 02:42 to 02:58Z WindowGaps", holed.Text())

	// One cycle in 100 missing: 1%, reported, not flagged.
	one := WindowGapsOf(cyclesAt(t0, 100, func(i int) bool { return i == 50 }))
	require.NotNil(t, one)
	assert.Equal(t, 100, one.ExpectedCycles)
	assert.False(t, one.Flagged)
	assert.NotContains(t, one.Text(), FlagWindowGaps)

	// Jitter under half a cycle is not a gap.
	jit := cyclesAt(t0, 50, nil)
	jit[10] = jit[10].Add(5 * time.Second)
	assert.Equal(t, 50, WindowGapsOf(jit).ExpectedCycles)

	// A time read twice counts once, and a 3-cycle window still shows its gap.
	dup := cyclesAt(t0, 50, nil)
	dup = append(dup[:20], append([]time.Time{dup[19]}, dup[20:]...)...)
	assert.Equal(t, 50, WindowGapsOf(dup).PresentCycles)
	three := []time.Time{t0, t0.Add(15 * time.Second), t0.Add(60 * time.Second)}
	assert.Equal(t, 5, WindowGapsOf(three).ExpectedCycles)

	assert.Nil(t, WindowGapsOf(cyclesAt(t0, 2, nil)), "too few cycles for a cycle time")
	var none *WindowGaps
	assert.Empty(t, none.Text())
}

// GapAt names a gap at least RollingWindow long with a cycle either side of
// it in the stretch, whether or not it is the largest; a shorter gap, or one
// away from the stretch, is not named.
func TestGapAt(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 2, 18, 0, 0, time.UTC)
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * 15 * time.Second) }
	// Cycles 100-159 missing: the gap runs from cycle 99 to cycle 160.
	holed := WindowGapsOf(cyclesAt(t0, 289, func(i int) bool { return i >= 100 && i < 160 }))
	const words = "15m0s from 02:42 to 02:58Z"
	assert.Equal(t, words, holed.GapAt(at(92), at(99)), "the stretch ends on the cycle before the gap")
	assert.Equal(t, words, holed.GapAt(at(160), at(167)), "the stretch starts on the cycle after the gap")
	assert.Equal(t, words, holed.GapAt(at(99), at(99)), "the peak cycle is the one before the gap")
	assert.Empty(t, holed.GapAt(at(10), at(17)), "a stretch away from the gap")
	assert.Empty(t, holed.GapAt(at(90), at(98)), "a stretch that ends a cycle before the gap")

	// Seven cycles missing (1m45s) is shorter than RollingWindow: it could
	// not hold a busiest stretch of its own.
	short := WindowGapsOf(cyclesAt(t0, 289, func(i int) bool { return i >= 100 && i < 107 }))
	require.Less(t, short.LargestGapSeconds, RollingWindow.Seconds())
	assert.Empty(t, short.GapAt(at(92), at(99)))
	// Eight (2m0s) is as long as it, and is named.
	eight := WindowGapsOf(cyclesAt(t0, 289, func(i int) bool { return i >= 100 && i < 108 }))
	assert.Equal(t, "2m0s from 02:42 to 02:45Z", eight.GapAt(at(92), at(99)))

	// Two gaps: the 5-minute one beside the stretch is named, though the
	// 15-minute one elsewhere is the largest.
	two := WindowGapsOf(cyclesAt(t0, 289, func(i int) bool { return (i >= 20 && i < 80) || (i >= 200 && i < 220) }))
	require.InDelta(t, 15*60, two.LargestGapSeconds, 1e-9)
	assert.Equal(t, "5m0s from 03:07 to 03:13Z", two.GapAt(at(192), at(199)))

	var none *WindowGaps
	assert.Empty(t, none.GapAt(t0, at(288)))
	assert.Empty(t, WindowGapsOf(cyclesAt(t0, 289, nil)).GapAt(t0, at(288)), "no gap")
}
