package allocation

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestOtherFramesDemand: the demand another analyzer's frame keeps, for an
// optimizer sizing at utilization u.
func TestOtherFramesDemand(t *testing.T) {
	one := func(supply, demand, ratio float64) OtherFrames {
		return OtherFrames{Supply: 1000, Frames: []FrameReading{{Supply: supply, Demand: demand, Ratio: ratio}}}
	}
	// A frame that agrees keeps exactly the role's own demand at any u.
	assert.InDelta(t, 600, one(1000, 600, 1).Demand(0.85), 1e-9)
	assert.InDelta(t, 600, one(1000, 600, 1).Demand(1), 1e-9)
	// Worth twice as much per unit (ratio 2), 400 of its 2000 spare at u=1 is
	// 200 of the role's: it keeps 800.
	assert.InDelta(t, 800, one(2000, 1600, 2).Demand(1), 1e-9)
	assert.InDelta(t, 200, one(2000, 1600, 2).Releasable(1), 1e-9)
	// A frame already above u keeps all the role has, and never more.
	assert.InDelta(t, 850, one(1000, 2000, 1).Demand(0.85), 1e-9)
	// The tightest frame decides.
	two := OtherFrames{Supply: 1000, Frames: []FrameReading{{1000, 300, 1}, {1000, 700, 1}}}
	assert.InDelta(t, 700, two.Demand(1), 1e-9)
	// No frame, a frame with no ratio, or a NaN reading constrains nothing.
	assert.Zero(t, OtherFrames{}.Demand(1))
	assert.Zero(t, one(1000, 600, 0).Demand(1))
	assert.Zero(t, one(math.NaN(), 600, 1).Demand(1))
	assert.Zero(t, one(1000, math.NaN(), 1).Demand(1))
	assert.True(t, math.IsInf(OtherFrames{}.Releasable(1), 1))
}
