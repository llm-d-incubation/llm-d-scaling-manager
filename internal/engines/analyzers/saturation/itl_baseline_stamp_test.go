package saturation

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// The ITL baseline's last-admitted stamp across a restart (issue #495,
// SwitchITLBaselineStamped).

// baselineStampRig is a process that learned a decode variant's line at load
// and then sat under the band until its window aged out, and how to start
// another process on its document.
type baselineStampRig struct {
	clock   time.Time
	states  []domain.VariantReplicaState
	process func() *SaturationAnalyzer
	replica func(k, itlSec float64) []domain.ReplicaMetrics
	// fitEnd is the clock of the last cycle at load: the baseline's last
	// full fit is at or before it.
	fitEnd time.Time
}

func newBaselineStampRig(t *testing.T) (*baselineStampRig, *SaturationAnalyzer) {
	t.Helper()
	const variant = "variant-a"
	r := &baselineStampRig{
		clock: time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC),
		states: []domain.VariantReplicaState{{VariantName: variant, Role: domain.RoleDecode, AcceleratorName: "H200",
			CurrentReplicas: 4, GPUsPerReplica: 1}},
	}
	pod := func(name string, k, itlSec float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(name, variant, int64(k*16000), 16000, 0, 1000, 500)
		rm.KvUsageInstant = k
		rm.AvgITL, rm.AvgITLKnown = itlSec, itlSec > 0
		return rm
	}
	r.replica = func(k, itlSec float64) []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{pod("pod-0", k, itlSec)}
	}
	// Four replicas each holding its load still, spread across the band:
	// the steady readings a full fit, and so a baseline, is taken from.
	steady := func() []domain.ReplicaMetrics {
		loads := []float64{0.20, 0.35, 0.50, 0.65}
		rows := make([]domain.ReplicaMetrics, 0, len(loads))
		for i, k := range loads {
			rows = append(rows, pod(fmt.Sprintf("pod-%d", i), k, 0.01+0.05*k))
		}
		return rows
	}
	r.process = func() *SaturationAnalyzer {
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", variant, capacity.Record{EngineParams: mtParams(64, "vllm:v1")}, time.Now())
		a := NewSaturationAnalyzer(store)
		a.now = func() time.Time { return r.clock }
		return a
	}
	first := r.process()
	for range 40 {
		r.clock = r.clock.Add(15 * time.Second)
		r.cycle(t, first, steady())
	}
	r.fitEnd = r.clock
	for r.clock.Sub(r.fitEnd) < itl.DefaultObservationMaxAge+2*time.Minute {
		r.clock = r.clock.Add(time.Minute)
		r.cycle(t, first, r.replica(0.06, 0.01))
	}
	return r, first
}

func (r *baselineStampRig) cycle(t *testing.T, a *SaturationAnalyzer, rows []domain.ReplicaMetrics) {
	t.Helper()
	_, err := a.Analyze(t.Context(), makeAnalyzerInput(rows, r.states))
	require.NoError(t, err)
}

// itlReport is the variant's ITL report of the last cycle.
func (r *baselineStampRig) itlReport(t *testing.T, a *SaturationAnalyzer) learn.Report {
	t.Helper()
	reports := reportsOf(a.LearnerReports()["test-ns|test-model"], learn.ITL)
	require.Len(t, reports, 1)
	return reports[0]
}

// exported is a's document as the store writes and reads it.
func exported(t *testing.T, a *SaturationAnalyzer) LearnedState {
	t.Helper()
	raw, err := json.Marshal(a.ExportLearnedState())
	require.NoError(t, err)
	var doc LearnedState
	require.NoError(t, json.Unmarshal(raw, &doc))
	return doc
}

// A v1 document holding a baseline alone, with its itlBaselineAdmittedAt
// stamp and no slope, restored into a process that keeps no reading of the
// key: the ITL report publishes the persisted stamp. Written against the
// document's JSON, so it reads the field by its wire name.
func TestITLBaselineStamp_ARestoredBaselineReportsThePersistedStamp(t *testing.T) {
	r, first := newBaselineStampRig(t)
	raw, err := json.Marshal(first.ExportLearnedState())
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	baselines, _ := m["itlBaseline"].(map[string]any)
	require.Len(t, baselines, 1, "premise: the line's full fit set a baseline")
	var key string
	for k := range baselines {
		key = k
	}
	// A stamp no process here computed: the restore must carry it through.
	stamp := r.fitEnd.Add(-7 * time.Minute).UTC()
	delete(m, "itlSlopes")
	m["itlBaselineAdmittedAt"] = map[string]any{key: stamp.Format(time.RFC3339Nano)}
	raw, err = json.Marshal(m)
	require.NoError(t, err)
	var doc LearnedState
	require.NoError(t, json.Unmarshal(raw, &doc))

	second := r.process()
	second.RestoreLearnedState(doc)
	r.clock = r.clock.Add(time.Minute)
	r.cycle(t, second, r.replica(0.06, 0.01)) // under the band: nothing kept
	require.True(t, second.itlTracker.Restored(key), "premise: the baseline came from the snapshot")
	_, held := second.itlTracker.Slopes()[key]
	require.False(t, held, "premise: no slope dates the key")
	rep := r.itlReport(t, second)
	require.Zero(t, rep.Samples, "premise: the window kept nothing")
	assert.True(t, stamp.Equal(rep.LastAdmitted), "the report publishes the persisted stamp: got %v, want %v",
		rep.LastAdmitted, stamp)
}
