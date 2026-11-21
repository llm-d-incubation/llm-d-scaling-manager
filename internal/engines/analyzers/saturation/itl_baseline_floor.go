package saturation

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// SwitchITLBaselineFromFloor is the replay switch for the ITL line's
// intercept while the key has no learned B (issue
// llm-d-incubation/llm-d-scaling-manager#169, item 1). A key learns B only
// from a full fit, and many never make one: on the e1 and staircase
// recordings nearly every published line sits on the 6 ms default
// (itl.DefaultBaselineSec). Through readings at a fill k, a fit through a B
// under the true one prices ITL high past them, so the derived mu is low and
// the fleet over-provisioned.
//
// On, such a key is pinned through the floor read below the band instead
// (itl.Admission.BaselineFromFloor): the 10th percentile of the steady mean
// ITLs its replicas read at a fill under the band's bottom, the readings the
// fit drops as idle, once there are itl.BelowBandMinReadings of them
// (itl.BelowBandFloor, the floor the capacity planner reads too,
// whatif.BelowBand.LeastITL). The floor is read at its own fill: B is the
// intercept of the line through it and the mean of the pinned fit's
// readings, which on a true line is its B. Pinned at the floor's ITL as if
// read at no fill, B would be high by A times that fill, and the line would
// price under the truth past its readings. The one-sided rule
// (SwitchITLOneSidedAtCap) is set against the pinned fit as against the
// default. A learned B ends it. Off, as before, the default. On from the
// constructor since a GPU check on a Qwen3-32B P/D fleet on H200, where the
// B it pinned sat 4-6 % under the measured low-load TPOT floor; a replay
// turns it off. The floor is read each cycle from readings at most
// itl.DefaultObservationMaxAge old: a fleet held in the band past that, none
// of its replicas below it, reads no floor, and is back on the default B
// unless SwitchITLFloorBaselineHeld holds the last one.
const SwitchITLBaselineFromFloor = "saturation-itl-baseline-from-floor"

// SwitchITLFloorBaselineHeld is the replay switch for a floor-pinned B on the
// cycles that read no floor (issue llm-d-incubation/llm-d-scaling-manager#333).
// With SwitchITLBaselineFromFloor on, a fleet held inside the band longer
// than itl.DefaultObservationMaxAge, none of its replicas below it, has no
// floor left to read: off, its line goes back to the 6 ms default B and its
// slope steps up, about 40 % in a unit probe, with no change in load. On, the
// pinned fit keeps the B the key last read from its floor
// (itl.Admission.HoldsFloorBaseline): kept on the key's held slope, exported
// with it (SlopeSnapshot.FloorB, FloorBAt) and restored with it, and swept
// with the key, as a learned B is. A newer floor or a full fit replaces it; a
// pinned reading under it, or a slope fitted under other engine arguments,
// ends it. Nothing without SwitchITLBaselineFromFloor. On from the
// constructor; a replay turns it off.
const SwitchITLFloorBaselineHeld = "saturation-itl-floor-baseline-held"

// baselineFromFloorOn reports SwitchITLBaselineFromFloor.
func (a *SaturationAnalyzer) baselineFromFloorOn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.itlBaselineFromFloor
}

// floorBaselineHeldOn reports SwitchITLFloorBaselineHeld.
func (a *SaturationAnalyzer) floorBaselineHeldOn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.itlFloorBaselineHeld
}

// floorBAge is how long before now a floor-pinned B was read from its floor
// (itl.Reading.FloorBAt), for the itl-fit line; -1s when that is unknown.
func floorBAge(at, now time.Time) time.Duration {
	if at.IsZero() {
		return -time.Second
	}
	return now.Sub(at)
}

// floorOnShapeChange drops a decode variant's floor below the band while the
// model's request shape changes (shapeChangedWithin), with
// SwitchITLBaselineFromFloor on. A decode window is kept through a shape
// change (noteITL), but a floor read at the old shape lies on the old shape's
// line, and paired with readings at the new one it sets a B under the truth:
// 19 % under at k 0.8 in the round-2 review's repro. Dropped each cycle of
// the change, the floor is read again from the new shape's readings after it.
// The model's shape tracker is the decode path's own signal; an aggregated
// variant clears its window, and the floor with it, on its own shape move
// (noteAggregatedShape).
func (a *SaturationAnalyzer) floorOnShapeChange(key, role string, adm itl.Admission, shapeChanging bool) {
	if adm.BaselineFromFloor && shapeChanging && domain.CanonicalRole(role) == domain.RoleDecode {
		a.itlTracker.ForgetFloor(key)
	}
}
