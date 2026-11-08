package saturation

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// A stored slope is installed only on the terms of the ITL baseline beside it,
// and only when it is a slope at all, fitted at a time that can be trusted.
func TestARestoredSlopeIsInstalledOnlyWhenItCanBeTrusted(t *testing.T) {
	running := mtParams(64, "vllm:v1")
	now := time.Now()
	good := SlopeSnapshot{A: 0.088, FittedAt: now.Add(-time.Hour), Args: running.ArgsFingerprint()}
	for _, tc := range []struct {
		name    string
		learned *slopeTestConfig // the configuration the document was written under
		slope   SlopeSnapshot
		want    bool
	}{
		{"same configuration", nil, good, true},
		{"another engine image", &slopeTestConfig{64, "vllm:v0"}, good, false},
		{"other engine arguments", &slopeTestConfig{256, "vllm:v1"}, good, false},
		{"a NaN slope", nil, SlopeSnapshot{A: math.NaN(), FittedAt: good.FittedAt, Args: good.Args}, false},
		{"a zero slope", nil, SlopeSnapshot{A: 0, FittedAt: good.FittedAt, Args: good.Args}, false},
		{"a negative slope", nil, SlopeSnapshot{A: -0.01, FittedAt: good.FittedAt, Args: good.Args}, false},
		{"no fit time", nil, SlopeSnapshot{A: good.A, Args: good.Args}, false},
		{"a fit time in the future", nil, SlopeSnapshot{A: good.A, FittedAt: now.Add(time.Hour), Args: good.Args}, false},
		{"fitted before the bucket retention", nil,
			SlopeSnapshot{A: good.A, FittedAt: now.Add(-capacity.HistoryRetention - time.Hour), Args: good.Args}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := waProcess(running)
			key, _, _ := itlStashKeyAndArgs(a)
			learned := running
			if tc.learned != nil {
				learned = mtParams(tc.learned.maxNumSeqs, tc.learned.image)
			}
			a.RestoreLearnedState(LearnedState{
				Config:        map[string]string{"test-ns/variant-a": learned.Fingerprint()},
				Args:          map[string]string{"test-ns/variant-a": learned.ArgsFingerprint()},
				VariantSeenAt: map[string]time.Time{key: now},
				ITLBaseline:   map[string]float64{key: 0.006},
				ITLSlopes:     map[string]SlopeSnapshot{key: tc.slope},
			})
			_, done := a.installRestored("test-ns", "test-model", "variant-a")
			require.True(t, done, "precondition: the variant's entries were pending")
			got, installed := a.itlTracker.Slopes()[key]
			require.Equal(t, tc.want, installed, "installed: %+v", got)
			if tc.want {
				assert.InDelta(t, tc.slope.A, got.A, 0)
				assert.True(t, got.FittedAt.Equal(tc.slope.FittedAt), "the original fit time, not the restore's")
			}
		})
	}
}

// slopeTestConfig names a configuration for the table above.
type slopeTestConfig struct {
	maxNumSeqs int64
	image      string
}

// Forgetting learned state drops the held slopes: they are exported, so one
// kept would be written back and undo the deletion.
func TestForgettingLearnedStateDropsTheHeldSlopes(t *testing.T) {
	a, _, _ := waProcess(mtParams(64, "vllm:v1"))
	key, _, _ := itlStashKeyAndArgs(a)
	now := time.Now()
	a.RestoreLearnedState(LearnedState{
		Config:        map[string]string{"test-ns/variant-a": mtParams(64, "vllm:v1").Fingerprint()},
		Args:          map[string]string{"test-ns/variant-a": mtParams(64, "vllm:v1").ArgsFingerprint()},
		VariantSeenAt: map[string]time.Time{key: now},
		ITLSlopes:     map[string]SlopeSnapshot{key: {A: 0.088, FittedAt: now.Add(-time.Hour)}},
	})
	_, done := a.installRestored("test-ns", "test-model", "variant-a")
	require.True(t, done)
	require.Contains(t, a.ExportLearnedState().ITLSlopes, key, "precondition: the slope is exported")

	a.ForgetLearnedState()
	assert.Empty(t, a.ExportLearnedState().ITLSlopes, "a forget leaves no slope to write back")
}
