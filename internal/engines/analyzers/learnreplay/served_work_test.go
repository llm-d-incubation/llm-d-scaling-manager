package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// servedAt is 21 cycles 15 s apart from t, spanning
// prefill.ServedWorkWindow, each the sum at pods, every pod read, no backlog.
// The rule itself is tested in prefill (served_work_test.go).
func servedAt(w *prefill.ServedWork, t time.Time, pods int, sum float64) {
	for range 21 {
		w.Note(prefill.ServedWorkSample{At: t, Pods: pods, Sum: sum, OK: true, QueueOK: true})
		t = t.Add(15 * time.Second)
	}
}

// The prefill state reads the window's peaks for the V_P raise only with
// saturation.SwitchPrefillVPAtServedWork, and the retention's for the knee
// floor either way.
func TestServedWork_TheStateReadsTheWindowWithTheSwitch(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, on := range []bool{false, true} {
		s := newTTFTSamples(false)
		s.vpAtServed = on
		w := &prefill.ServedWork{}
		servedAt(w, t0, 5, 90)
		servedAt(w, t0.Add(prefill.Retention+time.Hour), 5, 20)
		s.served["pre"] = w
		peaks, window, unsteady := s.servedWorkOf("pre")
		assert.InDelta(t, 20.0, peaks[5], 1e-9)
		assert.Empty(t, unsteady)
		if on {
			assert.InDelta(t, 90.0, window[5], 1e-9)
		} else {
			assert.Nil(t, window)
		}
	}
	none, window, unsteady := newTTFTSamples(false).servedWorkOf("pre")
	assert.Nil(t, none)
	assert.Nil(t, window)
	assert.Nil(t, unsteady)
}
