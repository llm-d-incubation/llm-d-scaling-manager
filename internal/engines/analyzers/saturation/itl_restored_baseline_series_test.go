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
