package capacity

import (
	"math"
	"time"
)

// RollingAverageSnapshot is a RollingAverage as it is carried across a
// controller restart: its values and BOTH clocks.
//
// The clocks are the point. lastUsed decides retention and lastWritten decides
// whether the next reading starts a new episode, so a restored window that
// took the restore time as either would be younger than the figure it holds:
// a measurement from last Tuesday would read as just used, survive a sweep it
// should not, and have today's first saturated reading blended into it as a
// continuation of an episode that ended days ago.
type RollingAverageSnapshot struct {
	Values      []float64 `json:"values"`
	LastUsed    time.Time `json:"lastUsed"`
	LastWritten time.Time `json:"lastWritten"`
}

// Snapshot returns the window's values and clocks. The values are copied, so
// the snapshot does not alias the live window.
func (r *RollingAverage) Snapshot() RollingAverageSnapshot {
	return RollingAverageSnapshot{
		Values:      append([]float64(nil), r.values...),
		LastUsed:    r.lastUsed,
		LastWritten: r.lastWritten,
	}
}

// RestoreRollingAverage rebuilds a window of the given size from a snapshot,
// or reports false when nothing in it is usable.
//
// Only finite, positive values are kept: every window the analyzer stores is
// a capacity or a rate, and a stored document is input the controller did not
// just compute -- a hand edit, a truncated write, a value from a build with a
// bug. A NaN in particular would pass every comparison guard downstream. When
// the snapshot holds more values than the window takes, the newest are kept,
// as Add would have kept them. A snapshot with no usable value, or with no
// clocks, restores nothing: a window with no age cannot be retained honestly.
func RestoreRollingAverage(s RollingAverageSnapshot, maxSize int) (*RollingAverage, bool) {
	if maxSize <= 0 || s.LastUsed.IsZero() || s.LastWritten.IsZero() {
		return nil, false
	}
	values := make([]float64, 0, maxSize)
	for _, v := range s.Values {
		if v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) {
			values = append(values, v)
		}
	}
	if len(values) == 0 {
		return nil, false
	}
	if len(values) > maxSize {
		values = values[len(values)-maxSize:]
	}
	return &RollingAverage{
		values:      values,
		maxSize:     maxSize,
		lastUsed:    s.LastUsed,
		lastWritten: s.LastWritten,
		restored:    true,
	}, true
}
