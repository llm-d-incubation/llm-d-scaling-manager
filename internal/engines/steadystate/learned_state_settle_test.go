package steadystate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
)

// The snapshot's sightings (analyzer-learner.md, PR 5, change E): the engine
// owns the settle tracker, so it writes them beside the analyzer's state and
// hands them back on restore. Only with LEARNED_STATE_SNAPSHOT on.

var settleT0 = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

// readyRow is a decode replica of model m, Ready for an hour.
func readyRow(pod string) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{PodName: pod, VariantName: "v", Ready: true, ReadySince: settleT0.Add(-time.Hour)}
}

func settleStates() []domain.VariantReplicaState {
	return []domain.VariantReplicaState{{VariantName: "v", Role: domain.RoleDecode}}
}

// observed is an engine whose tracker has seen rows at settleT0.
func observed(t *testing.T, c client.Client, rows ...domain.ReplicaMetrics) *Engine {
	t.Helper()
	e := snapshotEngine(t, c, &stateSpy{state: sampleState(1000)})
	e.observeSettle("ns", "m", rows, settleStates(), settleT0)
	return e
}

// A controller writes its sightings and the next one restores them: a row
// with the stored Ready time keeps its first sight.
func TestLearnedStateSettle_ARestartKeepsTheFirstSight(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	old := observed(t, c, readyRow("p0"), readyRow("p1"))
	old.restoreLearnedState(context.Background(), settleT0)
	old.writeLearnedState(context.Background(), settleT0)
	doc, _ := storedDoc(t, c)
	require.Len(t, doc.Settle["ns/m"], 2)

	at := settleT0.Add(time.Minute)
	next := snapshotEngine(t, c, &stateSpy{})
	var lines []string
	ctx := logr.NewContext(context.Background(), funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{}))
	next.restoreLearnedState(ctx, at)
	moved := readyRow("p1")
	moved.ReadySince = at.Add(-time.Second)
	next.observeSettle("ns", "m", []domain.ReplicaMetrics{readyRow("p0"), moved}, settleStates(), at)

	r, ok := next.settleTracker().Replica("ns/m", settle.Key{Pod: "p0", Variant: "v"})
	require.True(t, ok)
	assert.True(t, r.Restored, "the same Ready time keeps the sighting")
	assert.True(t, r.FirstSeen.Equal(settleT0))
	r, _ = next.settleTracker().Replica("ns/m", settle.Key{Pod: "p1", Variant: "v"})
	assert.False(t, r.Restored, "a moved Ready time waits")
	assert.True(t, r.FirstSeen.Equal(at))

	restored := ""
	for _, l := range lines {
		if strings.Contains(l, "Learned state: snapshot restored") {
			restored = l
		}
	}
	require.NotEmpty(t, restored, "%v", lines)
	assert.Contains(t, restored, `"sightings"=2`)
	assert.Contains(t, restored, `"startKeys"=0`)
}

// The log's start keys are the analyzer's report.
func TestLearnedStateSettle_TheLineCountsStartKeys(t *testing.T) {
	const key = "ns|m|v|H100|1"
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: learnedStateConfigMap, Namespace: config.SystemNamespace()}}
	raw, _, err := encodeLearnedState(saturation.LearnedState{
		StartSeconds:  map[string]float64{key: 70},
		VariantSeenAt: map[string]time.Time{key: settleT0},
		Config:        map[string]string{"ns/v": "abc"},
	}, nil, settleT0, learnedStateMaxBytes)
	require.NoError(t, err)
	cm.Data = map[string]string{learnedStateKey(): raw}
	var lines []string
	ctx := logr.NewContext(context.Background(), funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{}))
	sat := saturation.NewSaturationAnalyzer(nil)
	cfg := config.NewTestConfig()
	config.SetLearnedStateSnapshotForTest(cfg, true)
	e := &Engine{Config: cfg, client: fake.NewClientBuilder().WithObjects(cm).Build(), analyzers: testAnalyzers(sat)}
	e.restoreLearnedState(ctx, settleT0.Add(time.Minute))
	assert.Contains(t, strings.Join(lines, "\n"), `"startKeys"=1`)
}

// With the snapshot off nothing moves: no document, so nothing to restore,
// and a tracker that sees rows writes nothing. Every path to the document
// and from it is the snapshot's own: this is the guard the claim rests on.
func TestLearnedStateSettle_SnapshotOffMovesNothing(t *testing.T) {
	stored := map[string][]settle.Sighting{"ns/m": {{PodName: "p0", VariantName: "v", FirstSeen: settleT0,
		ReadySince: settleT0.Add(-time.Hour), ReadySeen: settleT0}}}
	raw, _, err := encodeLearnedState(saturation.LearnedState{}, stored, settleT0, learnedStateMaxBytes)
	require.NoError(t, err)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: learnedStateConfigMap, Namespace: config.SystemNamespace()},
		Data: map[string]string{learnedStateKey(): raw}}
	c := fake.NewClientBuilder().WithObjects(cm).Build()
	cfg := config.NewTestConfig()
	config.SetLearnedStateSnapshotForTest(cfg, false)
	e := &Engine{Config: cfg, client: c, analyzers: testAnalyzers(&stateSpy{})}
	at := settleT0.Add(time.Minute)
	e.restoreLearnedState(context.Background(), at)
	e.observeSettle("ns", "m", []domain.ReplicaMetrics{readyRow("p0")}, settleStates(), at)
	r, ok := e.settleTracker().Replica("ns/m", settle.Key{Pod: "p0", Variant: "v"})
	require.True(t, ok)
	assert.False(t, r.Restored)
	assert.True(t, r.FirstSeen.Equal(at), "first seen now, as on the parent")
	e.writeLearnedState(context.Background(), at)
	doc, _ := storedDoc(t, c)
	assert.True(t, doc.WrittenAt.Equal(settleT0), "the snapshot off writes nothing")
}

// A document the controller rejects restores no sightings either.
func TestLearnedStateSettle_ARejectedDocumentRestoresNothing(t *testing.T) {
	doc := learnedStateDoc{Version: learnedStateVersion, WrittenAt: settleT0,
		Writer: learnedStateWriter{Namespace: "elsewhere", Identity: "x"},
		Settle: map[string][]settle.Sighting{"ns/m": {{PodName: "p0", VariantName: "v", FirstSeen: settleT0,
			ReadySince: settleT0, ReadySeen: settleT0}}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: learnedStateConfigMap, Namespace: config.SystemNamespace()},
		Data: map[string]string{learnedStateKey(): string(raw)}}
	e := snapshotEngine(t, fake.NewClientBuilder().WithObjects(cm).Build(), &stateSpy{})
	e.restoreLearnedState(context.Background(), settleT0.Add(time.Minute))
	assert.Nil(t, e.settleTracker().Sightings())
}

// The sightings are in the digest: a replica joining rewrites the document
// at the next interval, and a fleet that does not change does not.
func TestLearnedStateSettle_ADigestOnTheSightings(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	e := observed(t, c, readyRow("p0"))
	e.restoreLearnedState(context.Background(), settleT0)
	e.writeLearnedState(context.Background(), settleT0)
	first, cm := storedDoc(t, c)
	version := cm.ResourceVersion

	at := settleT0.Add(learnedStateInterval)
	e.observeSettle("ns", "m", []domain.ReplicaMetrics{readyRow("p0")}, settleStates(), at)
	e.writeLearnedState(context.Background(), at)
	_, cm = storedDoc(t, c)
	assert.Equal(t, version, cm.ResourceVersion, "the same fleet a minute on rewrites nothing")

	at = at.Add(learnedStateInterval)
	e.observeSettle("ns", "m", []domain.ReplicaMetrics{readyRow("p0"), readyRow("p1")}, settleStates(), at)
	e.writeLearnedState(context.Background(), at)
	doc, cm := storedDoc(t, c)
	assert.NotEqual(t, version, cm.ResourceVersion, "a replica joining is written")
	assert.Len(t, doc.Settle["ns/m"], 2)
	assert.Len(t, first.Settle["ns/m"], 1)
	none, err := settleDigest(nil)
	require.NoError(t, err)
	assert.Empty(t, none)
}

// Deleting the snapshot is a flush: the sightings no row claimed go with it,
// and the next document does not bring them back.
func TestLearnedStateSettle_AFlushDropsTheUnclaimed(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	old := observed(t, c, readyRow("p0"))
	old.restoreLearnedState(context.Background(), settleT0)
	old.writeLearnedState(context.Background(), settleT0)
	next := snapshotEngine(t, c, &stateSpy{})
	next.restoreLearnedState(context.Background(), settleT0.Add(time.Minute))
	require.NotNil(t, next.settleTracker().Sightings(), "premise: unclaimed")
	_, cm := storedDoc(t, c)
	require.NoError(t, c.Delete(context.Background(), cm))
	next.writeLearnedState(context.Background(), settleT0.Add(2*time.Minute))
	assert.Nil(t, next.settleTracker().Sightings())
}

// At the bound the per-Pod entries go first: a sighting costs one replica
// one wait and a counted Pod one more fold, where a k2 window dropped for
// either prices a bucket at derived or k1. Each is about 150 B or under.
func TestLearnedStateSettle_ThePerPodEntriesAreSpare(t *testing.T) {
	const pods = 64
	now := settleT0
	s := sampleState(1000)
	// Older than every per-Pod time: spare goes first whatever the clocks.
	for k, w := range s.K2 {
		w.LastUsed, w.LastWritten = now.Add(-48*time.Hour), now.Add(-48*time.Hour)
		s.K2[k] = w
	}
	key := "ns|m|v|H100|1"
	s.StartSeconds = map[string]float64{key: 70}
	s.VariantSeenAt = map[string]time.Time{key: now}
	s.StartCounted = map[string]time.Time{}
	sightings := map[string][]settle.Sighting{"ns/m": nil}
	for i := range pods {
		pod := fmt.Sprintf("llm-d-model-service-decode-7c9d8f6b5-%05d", i)
		s.StartCounted[key+"|ns|"+pod] = now.Add(-time.Hour)
		sightings["ns/m"] = append(sightings["ns/m"], settle.Sighting{PodName: pod, VariantName: "decode-variant",
			FirstSeen: now.Add(-time.Hour), ReadySince: now.Add(-time.Hour), ReadySeen: now.Add(-time.Hour)})
	}
	lean, _, err := encodeLearnedState(sampleStateFor(s), nil, now, 1<<30)
	require.NoError(t, err)
	full, dropped, err := encodeLearnedState(s, sightings, now, 1<<30)
	require.NoError(t, err)
	require.Zero(t, dropped)
	perPod := (len(full) - len(lean)) / (2 * pods)
	t.Logf("%d bytes for %d Pods in each field: %d B an entry", len(full)-len(lean), pods, perPod)
	assert.LessOrEqual(t, perPod, 180, "the design's budget is about 150 B an entry")

	raw, dropped, err := encodeLearnedState(s, sightings, now, len(lean))
	require.NoError(t, err)
	assert.Equal(t, 2*pods, dropped, "every per-Pod entry, and nothing else")
	var doc learnedStateDoc
	require.NoError(t, json.Unmarshal([]byte(raw), &doc))
	assert.Empty(t, doc.State.StartCounted)
	assert.Empty(t, doc.Settle)
	assert.NotEmpty(t, doc.State.K2)
	assert.NotEmpty(t, doc.State.StartSeconds)

	// The trim works on a copy.
	assert.Len(t, s.StartCounted, pods)
	assert.Len(t, sightings["ns/m"], pods)

	// One byte over: the fewest go, the oldest first, the same ones each time.
	again, dropped, err := encodeLearnedState(s, sightings, now, len(full)-1)
	require.NoError(t, err)
	assert.Equal(t, 1, dropped)
	var one learnedStateDoc
	require.NoError(t, json.Unmarshal([]byte(again), &one))
	assert.Len(t, one.Settle["ns/m"], pods-1, "ties broken by key: settle sorts before startcounted")
	assert.Len(t, one.State.StartCounted, pods)
}

// sampleStateFor is s without the field under test.
func sampleStateFor(s saturation.LearnedState) saturation.LearnedState {
	s.StartCounted = nil
	return s
}

// A variant's estimate left out takes its counted Pods with it.
func TestLearnedStateSettle_AnEstimateLeftOutTakesItsPods(t *testing.T) {
	const key = "ns|m|v|H100|1"
	s := saturation.LearnedState{
		StartSeconds:  map[string]float64{key: 70},
		VariantSeenAt: map[string]time.Time{key: settleT0},
		StartCounted:  map[string]time.Time{key + "|ns|p0": settleT0, "ns|m|v2|H100|1|ns|p0": settleT0},
	}
	for _, e := range learnedStateEntries(s) {
		if strings.HasPrefix(e.key, "seen|") {
			e.drop()
		}
	}
	assert.Equal(t, map[string]time.Time{"ns|m|v2|H100|1|ns|p0": settleT0}, s.StartCounted)
}

// A document gives back its sightings only while a process would have
// tolerated the gap: the settle tolerance plus the time between two writes,
// 2 min at the default interval. Past that load may have come back unread,
// which is the one case the wait exists for, so every replica waits and the
// log says why.
func TestLearnedStateSettle_ADocumentTooOldGivesBackNoSightings(t *testing.T) {
	limit := settle.Tolerance(config.DefaultOptimizationInterval) + learnedStateInterval
	require.Equal(t, 2*time.Minute, limit, "the restart section 4.4 reasons about")
	for name, c := range map[string]struct {
		age  time.Duration
		kept bool
	}{
		"one minute":        {time.Minute, true},
		"the bound exactly": {2 * time.Minute, true},
		"a second past it":  {2*time.Minute + time.Second, false},
		"six minutes":       {6 * time.Minute, false},
		"an hour":           {time.Hour, false},
		"three days":        {72 * time.Hour, false},
	} {
		t.Run(name, func(t *testing.T) {
			c2 := fake.NewClientBuilder().Build()
			old := observed(t, c2, readyRow("p0"), readyRow("p1"))
			old.restoreLearnedState(context.Background(), settleT0)
			old.writeLearnedState(context.Background(), settleT0)

			at := settleT0.Add(c.age)
			next := snapshotEngine(t, c2, &stateSpy{})
			var lines []string
			ctx := logr.NewContext(context.Background(), funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{}))
			next.restoreLearnedState(ctx, at)
			next.observeSettle("ns", "m", []domain.ReplicaMetrics{readyRow("p0"), readyRow("p1")}, settleStates(), at)
			r, ok := next.settleTracker().Replica("ns/m", settle.Key{Pod: "p0", Variant: "v"})
			require.True(t, ok)
			all := strings.Join(lines, "\n")
			if c.kept {
				assert.True(t, r.Restored, "inside the bound the sighting comes back")
				assert.Contains(t, all, `"sightings"=2`)
				assert.NotContains(t, all, "too old")
				return
			}
			assert.False(t, r.Restored, "past the bound every replica waits")
			assert.True(t, r.FirstSeen.Equal(at))
			assert.Contains(t, all, `"sightings"=0`)
			assert.Contains(t, all, "too old to keep its replica sightings")
		})
	}
}

// The bound follows the optimize interval: a slow loop tolerates more.
func TestLearnedStateSettle_TheBoundFollowsTheInterval(t *testing.T) {
	cfg := config.NewTestConfig()
	e := &Engine{Config: cfg}
	want := settle.Tolerance(e.sloMissedAfter()) + learnedStateInterval
	assert.Equal(t, want, e.sightingsMaxAge())
}
