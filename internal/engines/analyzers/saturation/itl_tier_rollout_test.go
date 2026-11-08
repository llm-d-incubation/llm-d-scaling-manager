package saturation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The window across a rollout, through Analyze (SwitchITLClearsOnNewRevision):
// the first rows of a new revision clear it, B and the slope stand, and the
// tier latch goes back. A status with no new revision clears nothing.
func TestITLWindow_ANewRevisionClearsIt(t *testing.T) {
	run := func(switches map[string]bool, after string) (int, bool) {
		store := capacity.NewStore()
		file := func(rolling bool) {
			store.Update("test-ns", "test-model", "decode-a", capacity.Record{
				EngineParams: mtParams(256, "vllm:v1"), RollingOut: rolling,
			}, time.Now())
		}
		file(false)
		a := NewSaturationAnalyzer(store)
		require.NoError(t, a.SetSwitchesForReplay(switches))
		now := restartT0
		a.now = func() time.Time { return now }
		cycle := func(revision string) {
			rows := make([]domain.ReplicaMetrics, 0, 10)
			for i := range 10 {
				k := 0.20 + 0.04*float64(i)
				rm := makeReplicaMetrics(fmt.Sprintf("pod-%s-%d", revision, i), "decode-a", int64(k*16000), 16000, 0, 100, 50)
				rm.KvUsageInstant, rm.Revision = k, revision
				rm.AvgITL, rm.AvgITLKnown = 0.01+0.02*k, true
				rm.Ready, rm.ReadySince = true, restartT0.Add(-time.Hour)
				rows = append(rows, rm)
			}
			_, err := a.Analyze(context.Background(), makeAnalyzerInput(rows, []domain.VariantReplicaState{{
				VariantName: "decode-a", AcceleratorName: "H100", CurrentReplicas: 10, GPUsPerReplica: 1, Role: domain.RoleDecode,
			}}))
			require.NoError(t, err)
			now = now.Add(15 * time.Second)
		}
		key := a.itlWindowKey("test-ns", "test-model", "decode-a", "H100", 1)
		for range 4 {
			cycle("rev-1")
		}
		require.True(t, a.itlTracker.Readings(key).OLSLatched, "premise: the line stands at the full fit")
		b, _ := a.itlTracker.Baseline(key)
		file(true)
		cycle("rev-1")
		require.True(t, a.itlTracker.Readings(key).OLSLatched, "the tier moved mid-rollout, with no new reading")
		file(false)
		cycle(after)
		kept, _ := a.itlTracker.Baseline(key)
		require.Equal(t, b, kept, "B moved")
		return a.itlWindowLen(key), a.itlTracker.Readings(key).OLSLatched
	}
	held, latched := run(nil, "rev-2")
	assert.Equal(t, 10, held, "only the new revision's readings")
	assert.False(t, latched)

	held, latched = run(nil, "rev-1")
	assert.Greater(t, held, 10, "a status with no new revision clears nothing")
	assert.True(t, latched)

	held, latched = run(map[string]bool{SwitchITLClearsOnNewRevision: false}, "rev-2")
	assert.Greater(t, held, 10, "negative control: off, the readings join")
	assert.True(t, latched, "negative control")
}
