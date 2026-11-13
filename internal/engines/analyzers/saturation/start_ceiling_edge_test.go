package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStartBlend_TheRestoredMeanCeilingAtItsEdge holds the 3600 s ceiling a
// restored mean start is clamped at (maxRestoredStartSeconds) at its edge:
// a stored mean of 3599 s or 3600 s is installed as it is, one of 3601 s at
// 3600 s. TestStartBlend_TheCountSurvivesARestart reads the clamp only at
// 1e6 s, where a ceiling one second off passes. The collector's own ceiling,
// the same hour, is held at its edge by collector's
// TestPodStartSecondsCeilingEdge.
func TestStartBlend_TheRestoredMeanCeilingAtItsEdge(t *testing.T) {
	for _, c := range []struct{ stored, want float64 }{{3599, 3599}, {3600, 3600}, {3601, 3600}} {
		a := blendAnalyzer(t, true, true)
		a.mu.Lock()
		a.installStartLearnerLocked(confirmKey, LearnedState{
			StartSamples: map[string]StartSampleSnapshot{confirmKey: {Pods: 1, MeanSeconds: c.stored}},
		}, a.now())
		got, ok := a.startSamples[confirmKey]
		a.mu.Unlock()
		if assert.True(t, ok, "a stored mean of %v s is installed", c.stored) {
			assert.Equal(t, c.want, got.mean, "a stored mean of %v s", c.stored)
		}
	}
}
