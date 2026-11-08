package capacity

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func restoredWindow(t *testing.T) *RollingAverage {
	t.Helper()
	at := time.Unix(1_700_000_000, 0)
	r := NewRollingAverage(10, at)
	r.AddAt(5, at)
	got, ok := RestoreRollingAverage(r.Snapshot(), 10)
	require.True(t, ok)
	return got
}

// A window from a snapshot is restored until it is written: every write
// clears it, and a read does not.
func TestRollingAverage_RestoredUntilItsFirstWrite(t *testing.T) {
	at := time.Unix(1_700_000_100, 0)
	assert.False(t, NewRollingAverage(10, at).Restored(), "a new window is this process's own")

	r := restoredWindow(t)
	assert.True(t, r.Restored())
	r.TouchAt(at)
	_ = r.Average()
	_ = r.Median()
	assert.True(t, r.Restored(), "reading a window cannot make it this process's own")

	for name, write := range map[string]func(*RollingAverage){
		"AddAt":       func(r *RollingAverage) { r.AddAt(6, at) },
		"ObservedAt":  func(r *RollingAverage) { r.ObservedAt(at) },
		"SetLastAt":   func(r *RollingAverage) { r.SetLastAt(6, at) },
		"RaiseLastAt": func(r *RollingAverage) { r.RaiseLastAt(6, at) },
	} {
		r := restoredWindow(t)
		write(r)
		assert.False(t, r.Restored(), name)
	}
}

// The mark is not stored: a window exported with nothing written since its
// restore is restored again, and one written before the export is too, by
// the next process. The document does not change shape.
func TestRollingAverage_TheRestoredMarkIsNotInTheSnapshot(t *testing.T) {
	raw, err := json.Marshal(restoredWindow(t).Snapshot())
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "estored")
}
