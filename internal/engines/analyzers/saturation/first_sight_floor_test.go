package saturation

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What bounds the fleet change E can make (analyzer-learner.md, "5d as
// built"). The size has no ceiling: against the parent's figure the order
// falls by demand x (1/stored - 1/min(k1, readings)), so a stored k2 far from
// the readings moves it by as much as it is wrong. The duration is bounded,
// and so is where it stops: E never orders under demand / min(k1, readings),
// where the parent lands a minute later from the same readings.
//
// At a very stale k2 the parent's figure is the wrong one, a burst the wait
// holds for a minute. E under-orders against true need only if the readings
// overstate capacity, and k1 caps those.
func TestFirstSight_TheSizeIsTheStoredErrorAndTheFloorIsTheReadings(t *testing.T) {
	const readings = 260_000
	for _, c := range []struct {
		stored int64
		lower  float64
	}{
		{200_000, 2.71},
		{100_000, 14.48},
		{50_000, 38.01},
		{25_000, 85.07},
		{10_000, 226.24},
	} {
		t.Run(fmt.Sprintf("stored-%d", c.stored), func(t *testing.T) {
			before := snapshotRig(t, restartT0, "v")
			for range waitWindow + 2 {
				before.cycle(tokenRows(c.stored, 160), fourStates())
			}
			on, off := firstSightPrices(t, before, tokenRows(readings, 160))

			worst, cycles := 0.0, 0
			floor := on[len(on)-1]
			require.Equal(t, off[len(off)-1], floor, "the two end at one figure")
			require.EqualValues(t, readings, floor.capacity, "the end is the readings, under k1")
			for i := range on {
				assert.GreaterOrEqual(t, on[i].replicas, floor.replicas-1e-9, "cycle %d: E went under the floor", i)
				if gap := off[i].replicas - on[i].replicas; gap > 1e-9 {
					cycles++
					worst = max(worst, gap)
				}
			}
			t.Logf("stored %d: lower by %.4f replicas, on %d cycles", c.stored, worst, cycles)
			assert.InDelta(t, c.lower, worst, 0.01)
			assert.LessOrEqual(t, cycles, 6, "at most 6 cycles, 90 s at a 15 s cycle")
		})
	}
}
