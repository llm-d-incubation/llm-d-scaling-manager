package prefill

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadCurveMinutes is n minutes at pods, their loads spread from lo to hi
// computed tokens a second a pod, each reading f = floor + excess/load.
func loadCurveMinutes(pods, n int, lo, hi, floor, excess float64) []fSample {
	out := make([]fSample, n)
	for i := range out {
		load := lo + (hi-lo)*float64(i)/float64(n-1)
		out[i] = fSample{at: t0.Add(time.Duration(i) * minute), f: floor + excess/load, pods: pods, load: load}
	}
	return out
}

// The fit is least squares of f on 1/load at each pod count, over every
// minute kept there: a curve read exactly is returned, each count its own.
func TestFLoadCurve_FitsTheInverseOfTheLoad(t *testing.T) {
	fs := append(loadCurveMinutes(2, 24, 570, 13220, 1.2, 800), loadCurveMinutes(4, 10, 2000, 6000, 1.05, 200)...)
	got := fLoadCurves(fs)
	require.Len(t, got, 2)
	two, four := got[2], got[4]
	assert.InDelta(t, 1.2, two.Floor, 1e-9)
	assert.InDelta(t, 800, two.Excess, 1e-6)
	assert.Equal(t, 24, two.Minutes)
	assert.True(t, two.Identified())
	assert.InDelta(t, 1.05, four.Floor, 1e-9)
	assert.InDelta(t, 200, four.Excess, 1e-6)
	assert.InDelta(t, 2.2, two.At(800), 1e-9)
	assert.Equal(t, 1.0, FLoadCurve{Floor: 0.9, Excess: 10, Minutes: 8}.At(1e6), "never under 1")
	assert.True(t, math.IsInf(two.At(0), 1), "no load")
}

// The fit is not priced on fewer than MinFLoadCurveMinutes minutes, on an f
// that rises with the load, on one load (no slope to read), or on a floor at
// or under 0; a count still reports its minutes. No minute, no curve.
func TestFLoadCurve_NotIdentified(t *testing.T) {
	few := fLoadCurves(loadCurveMinutes(2, MinFLoadCurveMinutes-1, 1000, 9000, 1, 500))[2]
	assert.False(t, few.Identified())
	assert.Equal(t, MinFLoadCurveMinutes-1, few.Minutes)
	assert.Zero(t, few.Excess, "not fitted under the minimum")
	assert.True(t, fLoadCurves(loadCurveMinutes(2, MinFLoadCurveMinutes, 1000, 9000, 1, 500))[2].Identified(), "at the minimum")

	assert.False(t, fLoadCurves(loadCurveMinutes(2, 12, 1000, 9000, 1.3, -500))[2].Identified(), "f rising with the load")
	assert.False(t, fLoadCurves(loadCurveMinutes(2, 12, 5000, 5000, 1.3, 500))[2].Identified(), "one load")
	assert.False(t, fLoadCurves(loadCurveMinutes(2, 12, 1000, 9000, -0.2, 2000))[2].Identified(), "a floor under 0")
	assert.Nil(t, fLoadCurves(nil))
}

// Through Observe: the curve is fitted only with Cycle.FLoadCurve, and the
// minutes it reads are the ones f is read from.
func TestFLoadCurve_OnlyWithTheSwitch(t *testing.T) {
	on, off := lightThenBusy(3, false), lightThenBusy(3, false)
	for i := range on {
		on[i].FLoadCurve = true
	}
	estOn, estOff := run(NewObserver(), on...), run(NewObserver(), off...)
	assert.Nil(t, estOff.FCurveByPods)
	c := estOn.FCurveByPods[2]
	assert.Equal(t, estOn.FSamplesByPods[2], c.Minutes)
	assert.True(t, c.Identified(), "%+v", c)
	assert.Equal(t, estOff.F, estOn.F, "f itself does not move")
}
