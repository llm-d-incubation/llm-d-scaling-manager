package saturation

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// A B restored on its own shows its age on the last-admitted series, and a B
// put aside under other engine arguments keeps its stamp (issue #366).

// itlSeriesValue is the variant's ITL last-admitted series in registry.
func itlSeriesValue(t *testing.T, registry *prometheus.Registry, variant string) (float64, bool) {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != constants.LLMSMLearnedStateLastAdmittedTimestampSeconds {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels[constants.LabelVariantName] == variant && labels[constants.LabelLearner] == learn.ITL {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// A snapshot that holds only B and its stamp, restored into a process that
// keeps no reading: the series shows the stamp before any reading arrives.
func TestITLBaselineStamp_ARestoredBaselineAloneShowsOnTheSeries(t *testing.T) {
	r, first := newBaselineStampRig(t)
	doc, key, stamp := baselineAlone(t, exported(t, first))
	require.False(t, stamp.IsZero(), "premise: the export carries the baseline's stamp")

	second := r.process()
	second.RestoreLearnedState(doc)
	r.clock = r.clock.Add(time.Minute)
	r.cycle(t, second, r.replica(0.06, 0.01)) // under the band: nothing kept
	require.True(t, second.itlTracker.Restored(key), "premise: the baseline came from the snapshot")
	rep := r.itlReport(t, second)
	require.Zero(t, rep.Samples, "premise: the window kept nothing")
	require.Empty(t, rep.PricingKey, "premise: B alone prices no line")

	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	metrics.NewMetricsEmitter().RecordLearnerReports(second.LearnerReports()["test-ns|test-model"], nil)
	v, ok := itlSeriesValue(t, registry, "variant-a")
	require.True(t, ok, "a B restored on its own has no last-admitted series")
	assert.InDelta(t, float64(stamp.UnixMilli())/1000, v, 1e-3, "the series shows the restored B's stamp")
}

// A B put aside under the arguments it was learned under and taken back on a
// rollback keeps the stamp it had: its age does not become unknown.
func TestITLBaselineStamp_AStashRoundTripKeepsTheStamp(t *testing.T) {
	_, first := newBaselineStampRig(t)
	doc := exported(t, first)
	require.Len(t, doc.ITLBaseline, 1, "premise: the line's full fit set a baseline")
	var key string
	for k := range doc.ITLBaseline {
		key = k
	}
	stamp := first.itlTracker.BaselineAdmittedAt(key)
	require.False(t, stamp.IsZero(), "premise: the live baseline is stamped")
	first.mu.Lock()
	args := first.itlArgs[key]
	first.mu.Unlock()
	require.NotEmpty(t, args, "premise: the key's arguments are recorded")

	first.itlUnderArgs(key, "other-args")
	_, held := first.itlTracker.Baseline(key)
	require.False(t, held, "premise: the baseline was put aside")
	first.itlUnderArgs(key, args)
	b, ok := first.itlTracker.Baseline(key)
	require.True(t, ok, "premise: the rollback took the baseline back")
	assert.Equal(t, doc.ITLBaseline[key], b)
	assert.True(t, stamp.Equal(first.itlTracker.BaselineAdmittedAt(key)),
		"the stash round trip lost the stamp: %v, want %v", first.itlTracker.BaselineAdmittedAt(key), stamp)
	assert.True(t, stamp.Equal(exported(t, first).ITLBaselineAdmittedAt[key]), "and the next export carries it")
}

// A B restored on its own from a document with no stamp is of unknown age: it
// names no RestoredKey, and no series is published at 0 for it.
func TestITLBaselineStamp_ARestoredBaselineWithNoStampShowsNoSeries(t *testing.T) {
	r, first := newBaselineStampRig(t)
	doc, key, _ := baselineAlone(t, exported(t, first))
	doc.ITLBaselineAdmittedAt = nil

	second := r.process()
	second.RestoreLearnedState(doc)
	r.clock = r.clock.Add(time.Minute)
	r.cycle(t, second, r.replica(0.06, 0.01)) // under the band: nothing kept
	require.True(t, second.itlTracker.Restored(key), "premise: the baseline came from the snapshot")
	rep := r.itlReport(t, second)
	require.Empty(t, rep.PricingKey, "premise: B alone prices no line")
	require.True(t, rep.LastAdmitted.IsZero(), "premise: the baseline's age is unknown")
	assert.Empty(t, rep.RestoredKey, "a B of unknown age named as restored")

	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	metrics.NewMetricsEmitter().RecordLearnerReports(second.LearnerReports()["test-ns|test-model"], nil)
	_, ok := itlSeriesValue(t, registry, "variant-a")
	assert.False(t, ok, "a B of unknown age published a last-admitted series")
}

// A document's baseline that a restore under other arguments puts aside keeps
// the document's stamp, and a rollback to the arguments it was learned under
// takes the stamp back with it.
func TestITLBaselineStamp_ABaselinePutAsideAtRestoreKeepsTheStamp(t *testing.T) {
	a, _, clock := waProcess(mtParams(64, "vllm:v1"))
	key, argsA, argsB := itlStashKeyAndArgs(a)
	stamp := clock.Add(-time.Hour).UTC()
	a.RestoreLearnedState(LearnedState{
		Config:                map[string]string{"test-ns/variant-a": mtParams(256, "vllm:v1").Fingerprint()},
		Args:                  map[string]string{"test-ns/variant-a": argsA},
		VariantSeenAt:         map[string]time.Time{key: *clock},
		ITLBaseline:           map[string]float64{key: 0.0012},
		ITLBaselineAdmittedAt: map[string]time.Time{key: stamp},
	})
	r, done := a.installRestored("test-ns", "test-model", "variant-a")
	require.True(t, done && r.Outcome == RestoredPrior, "premise: a prior restore, got %+v done=%v", r, done)
	_, held := a.itlTracker.Baseline(key)
	require.False(t, held, "premise: the baseline of other arguments was put aside")

	a.itlUnderArgs(key, argsB)
	a.itlUnderArgs(key, argsA)
	b, ok := a.itlTracker.Baseline(key)
	require.True(t, ok && b == 0.0012, "premise: the rollback took the baseline back: %v %v", b, ok)
	assert.True(t, stamp.Equal(a.itlTracker.BaselineAdmittedAt(key)),
		"the put-aside baseline lost the document's stamp: %v, want %v", a.itlTracker.BaselineAdmittedAt(key), stamp)
}

// The stamp of a put-aside B goes with it: a forget and an eviction of the
// stash leave no stamp behind.
func TestITLBaselineStashAt_GoesWithItsStash(t *testing.T) {
	stashed := func() (*SaturationAnalyzer, *time.Time, string) {
		a, _, clock := waProcess(mtParams(64, "vllm:v1"))
		key, argsA, argsB := itlStashKeyAndArgs(a)
		a.itlUnderArgs(key, argsA)
		a.itlTracker.SetBaselineAt(key, 0.0012, clock.Add(-time.Minute))
		a.itlUnderArgs(key, argsB)
		sk := stashKey(key, argsA)
		a.mu.Lock()
		defer a.mu.Unlock()
		require.Contains(t, a.itlBaselineStash, sk, "premise: the baseline was put aside")
		require.Contains(t, a.itlBaselineStashAt, sk, "premise: with its stamp")
		a.variantSeenAt[key] = *clock
		return a, clock, sk
	}

	a, _, sk := stashed()
	a.ForgetVariant("test-ns", "test-model", "variant-a", "t1")
	a.mu.Lock()
	assert.NotContains(t, a.itlBaselineStash, sk)
	assert.NotContains(t, a.itlBaselineStashAt, sk, "a forget left the put-aside stamp behind")
	a.mu.Unlock()

	a, clock, sk := stashed()
	a.evictStaleHistory()
	a.mu.Lock()
	require.Contains(t, a.itlBaselineStashAt, sk, "premise: a live variant's stamp is kept")
	a.mu.Unlock()
	*clock = clock.Add(a.horizons.VariantTimeout + time.Minute)
	a.evictStaleHistory()
	a.mu.Lock()
	assert.NotContains(t, a.itlBaselineStash, sk)
	assert.NotContains(t, a.itlBaselineStashAt, sk, "an eviction left the put-aside stamp behind")
	a.mu.Unlock()
}
