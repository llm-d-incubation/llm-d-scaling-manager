package steadystate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// A document one entry over its bound loses one entry, not a tenth of what
// was learned.
func TestTheSizeBoundLeavesOutTheFewestEntries(t *testing.T) {
	now := time.Now()
	s := saturation.LearnedState{K2: map[string]capacity.RollingAverageSnapshot{}}
	for i := range 100 {
		at := now.Add(-time.Duration(100-i) * time.Minute)
		s.K2[fmt.Sprintf("ns|v|m|H100|1|both|b%03d|q5", i)] = capacity.RollingAverageSnapshot{
			Values: []float64{4000}, LastUsed: at, LastWritten: at,
		}
	}
	full, dropped, err := encodeLearnedState(s, nil, now, 1<<30)
	require.NoError(t, err)
	require.Zero(t, dropped)

	raw, dropped, err := encodeLearnedState(s, nil, now, len(full)-1)
	require.NoError(t, err)
	assert.Equal(t, 1, dropped, "one entry over the bound")
	var doc learnedStateDoc
	require.NoError(t, json.Unmarshal([]byte(raw), &doc))
	assert.Len(t, doc.State.K2, 99)
	assert.NotContains(t, doc.State.K2, "ns|v|m|H100|1|both|b000|q5", "the least recently used goes")
	assert.Len(t, s.K2, 100, "the caller's state is not trimmed")
}

// Among the spent bounds, the one whose bucket was served longest ago goes
// first; a running bound goes by its own last serving cycle, not by its
// variant's liveness, which every cycle refreshes for keys it no longer
// serves.
func TestTheSizeBoundOrdersLowerBoundsByTheirOwnUse(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	spent := func(tick time.Time) saturation.LowerBoundSnapshot {
		return saturation.LowerBoundSnapshot{Armed: true, Since: tick.Add(-7 * time.Hour), Tick: tick, Seen: now, DonorK2: 4000}
	}
	s := saturation.LearnedState{
		LowerBounds: map[string]saturation.LowerBoundSnapshot{
			"ns|v|m|H100|1|both|b1|q5": spent(now),
			"ns|v|m|H100|1|both|b2|q5": spent(now.Add(-48 * time.Hour)),
			// Running, last served two days ago under a retired key.
			"ns|v|m|H100|1|both|b3|q9": {Armed: true, Since: now.Add(-48*time.Hour - time.Minute), Tick: now.Add(-48 * time.Hour), Seen: now, DonorK2: 4000},
		},
		K2: map[string]capacity.RollingAverageSnapshot{
			"ns|v|m|H100|1|both|b4|q5": {Values: []float64{4000}, LastUsed: now.Add(-time.Hour), LastWritten: now.Add(-time.Hour)},
		},
	}
	entries := learnedStateEntries(s)
	require.Len(t, entries, 4)
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, e.key)
	}
	assert.Equal(t, []string{
		"bound|ns|v|m|H100|1|both|b2|q5",
		"bound|ns|v|m|H100|1|both|b1|q5",
		"bound|ns|v|m|H100|1|both|b3|q9",
		"k2|ns|v|m|H100|1|both|b4|q5",
	}, keys)
}

// While the snapshot cannot be read the lower bounds are held, for at most
// learnedStateBoundHold, and released by the read that succeeds.
func TestTheLowerBoundsAreHeldUntilTheSnapshotIsRead(t *testing.T) {
	base := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()
	denied := true
	flaky := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if denied && key.Name == learnedStateConfigMap {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, nil)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	spy := &stateSpy{}
	e := snapshotEngine(t, flaky, spy)
	e.restoreLearnedState(ctx, now)
	require.Equal(t, []bool{true}, spy.held, "held from the first failed read")
	denied = false
	e.restoreLearnedState(ctx, now.Add(2*time.Minute))
	assert.Equal(t, []bool{true, true, false}, spy.held, "released by the read that succeeds")

	spy = &stateSpy{}
	denied = true
	e = snapshotEngine(t, flaky, spy)
	e.restoreLearnedState(ctx, now)
	e.restoreLearnedState(ctx, now.Add(learnedStateBoundHold))
	assert.Equal(t, []bool{true, false}, spy.held, "a store that stays unreadable does not hold them for good")
}

// A running bound holding a figure goes by the figure's last use when that is
// later than its clock: the clock waits while the variant idles, the figure is
// still in use.
func TestTheSizeBoundKeepsABoundWhoseFigureIsInUse(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s := saturation.LearnedState{
		LowerBounds: map[string]saturation.LowerBoundSnapshot{
			"ns|v|m|H100|1|both|b1|q5": {Armed: true, Since: now.Add(-11 * time.Hour), Tick: now.Add(-10 * time.Hour), Seen: now,
				Own: true, DonorK2: 4000, Prior: 4000, PriorSince: now.Add(-11 * time.Hour), PriorUsed: now},
		},
		K2: map[string]capacity.RollingAverageSnapshot{
			"ns|v|m|H100|1|both|b2|q5": {Values: []float64{4000}, LastUsed: now.Add(-time.Hour), LastWritten: now.Add(-time.Hour)},
		},
	}
	entries := learnedStateEntries(s)
	require.Len(t, entries, 2)
	assert.Equal(t, "k2|ns|v|m|H100|1|both|b2|q5", entries[0].key, "the bound in use went before an hour-old window")
}

// gaugeValue reads one unlabelled gauge.
func gaugeValue(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := registry.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == name && len(mf.GetMetric()) == 1 {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("no %s", name)
	return 0
}

// The snapshot's size, its bound and what a write left out are published, so
// a fleet at the bound -- each write dropping the windows of the variants idle
// longest -- is seen before a restart prices them at derived or k1.
func TestTheSnapshotSizeAndWhatItLeftOutArePublished(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	ctx := context.Background()
	now := time.Now()
	state := saturation.LearnedState{K2: map[string]capacity.RollingAverageSnapshot{}}
	for i := range 50 {
		state.K2[fmt.Sprintf("ns|v|m|H100|1|both|b%03d|q5", i)] = capacity.RollingAverageSnapshot{
			Values: []float64{4000}, LastUsed: now.Add(-time.Duration(50-i) * time.Minute), LastWritten: now,
		}
	}
	c := fake.NewClientBuilder().Build()
	e := snapshotEngine(t, c, &stateSpy{state: state})
	e.metricsEmitter = metrics.NewMetricsEmitter()
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)
	full := gaugeValue(t, registry, constants.LLMSMLearnedStateSnapshotBytes)
	assert.Positive(t, full)
	assert.Equal(t, float64(learnedStateMaxBytes), gaugeValue(t, registry, constants.LLMSMLearnedStateSnapshotBudgetBytes))
	assert.Zero(t, gaugeValue(t, registry, constants.LLMSMLearnedStateSnapshotDroppedEntries))

	// Another version's document leaves this one just short of its size.
	other := learnedStateMaxData - int(full) + 100
	squeezed := fake.NewClientBuilder().WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: learnedStateConfigMap, Namespace: config.SystemNamespace()},
		Data:       map[string]string{"v0": strings.Repeat("x", other-2)},
	}).Build()
	e2 := snapshotEngine(t, squeezed, &stateSpy{state: state})
	e2.metricsEmitter = metrics.NewMetricsEmitter()
	e2.restoreLearnedState(ctx, now)
	e2.writeLearnedState(ctx, now)
	assert.Positive(t, gaugeValue(t, registry, constants.LLMSMLearnedStateSnapshotDroppedEntries),
		"a write that left entries out did not say so")
	assert.Less(t, gaugeValue(t, registry, constants.LLMSMLearnedStateSnapshotBytes), full)
}
