package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The planner's prefill rules that live in shared packages, so the
// controller can call them (issue llm-d-incubation/llm-d-scaling-manager#169).
// Each test calls the shared rule from this package, and holds the switch
// that gates it off in the analyzer the constructor builds: no rule is used
// here yet.

// The prefill pods' served work by pod count (prefill.ServedWork, #494)
// keeps from this package, and the switch the planner keeps it behind,
// SwitchPrefillVPServedUnqueued, is off from the constructor.
func TestParity_TheServedWorkIsSharedAndOff(t *testing.T) {
	var w prefill.ServedWork
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for range 21 {
		w.Note(prefill.ServedWorkSample{At: at, Pods: 2, Sum: 50, OK: true, QueueOK: true})
		at = at.Add(15 * time.Second)
	}
	assert.Equal(t, map[int]float64{2: 50}, w.Peaks(), "21 cycles 15 s apart span prefill.ServedWorkWindow")

	a := NewSaturationAnalyzer(capacity.NewStore())
	fields := a.switchFields()
	require.Contains(t, fields, SwitchPrefillVPServedUnqueued)
	assert.False(t, *fields[SwitchPrefillVPServedUnqueued], "the controller keeps no served work")
}

// The prefix hit at another prefill count (prefill.HitAtCountOf and
// HitAtCount.GPUHitAt, #425/#469/#472) and f against the load
// (prefill.FLoadCurve, #493) price from this package; their switches,
// SwitchPrefillHitAtCount and SwitchPrefillFLoadCurve, are off from the
// constructor, so the controller fits no curve and moves no hit.
func TestParity_TheHitAtCountAndTheFLoadCurveAreSharedAndOff(t *testing.T) {
	r, ok := prefill.HitAtCountOf(0.5, 5000, 2, 1, nil)
	require.True(t, ok)
	assert.InDelta(t, 0.2, r.GPUHitAt(0.4), 1e-12)
	assert.False(t, prefill.FLoadCurve{}.Identified())

	a := NewSaturationAnalyzer(capacity.NewStore())
	fields := a.switchFields()
	for _, name := range []string{SwitchPrefillHitAtCount, SwitchPrefillFLoadCurve} {
		require.Contains(t, fields, name)
		assert.False(t, *fields[name], "%s: off from the constructor", name)
	}
}
