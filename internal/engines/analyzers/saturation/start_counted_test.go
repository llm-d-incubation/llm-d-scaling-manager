package saturation

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Counted Pods through a controller restart (analyzer-learner.md, PR 5,
// change D, SwitchStartCountedPodsRestored).

// countedT0 is when countedRow's Pods became Ready.
var countedT0 = time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)

// countedRow is a Ready Pod reporting start seconds, Ready at countedT0.
func countedRow(pod string, start float64) domain.ReplicaMetrics {
	rm := confirmReplica(pod, start)
	rm.ReadySince = countedT0
	return rm
}

// countedDoc is a document holding the estimate 70 s of confirmKey, its four
// Pods counted with their Ready times.
func countedDoc(at time.Time) LearnedState {
	s := LearnedState{StartSeconds: map[string]float64{confirmKey: 70},
		VariantSeenAt: map[string]time.Time{confirmKey: at}, StartCounted: map[string]time.Time{}}
	for i := range 4 {
		s.StartCounted[countedKey(fmt.Sprintf("d%d", i))] = countedT0
	}
	return s
}

func countedKey(pod string) string { return confirmKey + "|ns|" + pod }

// countedProcess is a new controller, with switch set, that restored s.
func countedProcess(t *testing.T, on bool, s LearnedState) *SaturationAnalyzer {
	t.Helper()
	a := NewSaturationAnalyzer(capacity.NewStore())
	at := countedT0.Add(2 * time.Hour)
	a.now = func() time.Time { return at }
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchStartCountedPodsRestored: on}))
	a.mu.Lock()
	defer a.mu.Unlock()
	a.installLocked(s, "", true)
	return a
}

func fourRows(start float64) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, 4)
	for i := range 4 {
		out = append(out, countedRow(fmt.Sprintf("d%d", i), start))
	}
	return out
}

// The design's negative control: restored 70, four running Pods reporting
// 90 s with their stored Ready times. The parent folds all four, 85.2 s; the
// PR stays at 70.
func TestStartCounted_RunningPodsAreNotFoldedAgain(t *testing.T) {
	s := countedDoc(countedT0.Add(time.Hour))
	on := countedProcess(t, true, s)
	confirmCycle(on, logr.Discard(), fourRows(90)...)
	assert.Equal(t, 70.0, on.startSeconds[confirmKey], "on: counted already")
	confirmCycle(on, logr.Discard(), fourRows(90)...)
	assert.Equal(t, 70.0, on.startSeconds[confirmKey], "on: and on the next cycle")
	assert.Empty(t, on.startCountedRestored, "each row claimed its stored time")
	assert.Len(t, on.startSeenPods, 4, "the Pods are counted")

	off := countedProcess(t, false, s)
	confirmCycle(off, logr.Discard(), fourRows(90)...)
	assert.InDelta(t, 85.2, off.startSeconds[confirmKey], 0.01, "off: every running Pod is folded again")
}

// A Pod with another Ready time, or none, is folded as a new one: a restart
// under the same name while the controller was down is a new start.
func TestStartCounted_AnotherReadyTimeIsFolded(t *testing.T) {
	for name, change := range map[string]func(*domain.ReplicaMetrics){
		"another Ready time": func(rm *domain.ReplicaMetrics) { rm.ReadySince = countedT0.Add(time.Second) },
		"no Ready time":      func(rm *domain.ReplicaMetrics) { rm.ReadySince = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			a := countedProcess(t, true, countedDoc(countedT0.Add(time.Hour)))
			rows := fourRows(90)
			change(&rows[0])
			confirmCycle(a, logr.Discard(), rows...)
			assert.InDelta(t, 0.7*70+0.3*90, a.startSeconds[confirmKey], 1e-9, "only that Pod is folded")
		})
	}
}

// A Pod the document does not name is folded, and an empty document folds
// them all: a document from before the field, which moves no estimate
// differently from the parent.
func TestStartCounted_ADocumentWithoutItFoldsThePods(t *testing.T) {
	s := countedDoc(countedT0.Add(time.Hour))
	s.StartCounted = nil
	a := countedProcess(t, true, s)
	confirmCycle(a, logr.Discard(), fourRows(90)...)
	assert.InDelta(t, 85.2, a.startSeconds[confirmKey], 0.01)
}

// A row that says nothing (the Pod list was not read: no Ready time and no
// start) leaves the stored time for the next row; the Pod is not folded then.
func TestStartCounted_ARowThatSaysNothingLeavesTheStoredTime(t *testing.T) {
	a := countedProcess(t, true, countedDoc(countedT0.Add(time.Hour)))
	blind := fourRows(0)
	for i := range blind {
		blind[i].ReadySince = time.Time{}
	}
	confirmCycle(a, logr.Discard(), blind...)
	require.Len(t, a.startCountedRestored, 4)
	confirmCycle(a, logr.Discard(), fourRows(90)...)
	assert.Equal(t, 70.0, a.startSeconds[confirmKey])
	assert.Empty(t, a.startCountedRestored)
}

// A matching row with no start to report (a Pod past the collector's
// credible start, say) is counted all the same.
func TestStartCounted_AMatchingRowWithNoStartIsCounted(t *testing.T) {
	a := countedProcess(t, true, countedDoc(countedT0.Add(time.Hour)))
	confirmCycle(a, logr.Discard(), fourRows(0)...)
	assert.Len(t, a.startSeenPods, 4)
	assert.Empty(t, a.startCountedRestored)
	assert.Len(t, a.exportLearnedState().StartCounted, 4, "and stored again")
}

// A Pod that blipped before the restart is not read as a new start: the
// document holds the Ready time it reports now, not the one it was counted
// at. The estimate stays; the Pod reports its age after the restart, 2700 s,
// and is not refused or folded, so it opens no run.
func TestStartCounted_ABlipBeforeTheRestartIsNotANewStart(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	at := countedT0
	a.now = func() time.Time { return at }
	note := startNote{key: confirmKey, namespace: "ns", variant: confirmVariant}
	row := countedRow("d0", 70)
	a.noteReplicaStartFor(note, confirmRows(row), logr.Discard())
	at = at.Add(45 * time.Minute)
	row.ReadySince, row.StartSeconds = at, 2700
	a.noteReplicaStartFor(note, confirmRows(row), logr.Discard())
	s := a.exportLearnedState()
	require.Equal(t, 70.0, s.StartSeconds[confirmKey], "premise: live, a blip of a counted Pod folds nothing")
	require.True(t, s.StartCounted[countedKey("d0")].Equal(at), "the document holds the latest Ready time")
	s.VariantSeenAt = map[string]time.Time{confirmKey: at}

	b := countedProcess(t, true, s)
	confirmCycle(b, logr.Discard(), row)
	assert.Equal(t, 70.0, b.startSeconds[confirmKey])
	assert.Empty(t, b.startOutlierPods, "not refused either")
	assert.Equal(t, 1, len(b.startSeenPods))
}

// A slow Pod refused before the restart is not counted, so not stored: it
// joins a run after the restart, as without the snapshot, and three of them
// still confirm a slower image.
func TestStartCounted_ARefusedPodIsNotStored(t *testing.T) {
	a := confirmAnalyzer(t, true)
	confirmCycle(a, logr.Discard(), countedRow("slow", 300))
	s := a.exportLearnedState()
	assert.NotContains(t, s.StartCounted, countedKey("slow"))
}

// A run's Pods the guard opened on are counted, so stored: a restart does
// not fold them again.
func TestStartCounted_AConfirmedRunIsStored(t *testing.T) {
	a := confirmAnalyzer(t, true)
	rows := []domain.ReplicaMetrics{countedRow("s0", 300), countedRow("s1", 300), countedRow("s2", 300)}
	confirmCycle(a, logr.Discard(), rows...)
	require.Equal(t, 300.0, a.startSeconds[confirmKey], "premise: the guard opened")
	s := a.exportLearnedState()
	for _, pod := range []string{"s0", "s1", "s2"} {
		assert.Contains(t, s.StartCounted, countedKey(pod))
	}
}

// Stored beside an estimate only: a Pod counted into an estimate that was
// not installed has to be measured again.
func TestStartCounted_NoEstimateNoCount(t *testing.T) {
	s := countedDoc(countedT0.Add(time.Hour))
	delete(s.StartSeconds, confirmKey)
	a := countedProcess(t, true, s)
	assert.Empty(t, a.startCountedRestored)
	confirmCycle(a, logr.Discard(), fourRows(90)...)
	assert.InDelta(t, 90.0, a.startSeconds[confirmKey], 0.01, "the Pods are measured: 90 s is the first sample")

	// A live estimate is kept, and the document's count with it is not read.
	b := countedProcess(t, true, LearnedState{})
	b.startSeconds[confirmKey] = 50
	b.mu.Lock()
	b.installLocked(countedDoc(countedT0.Add(time.Hour)), "", true)
	b.mu.Unlock()
	assert.Empty(t, b.startCountedRestored)

	// And export never writes a count with no estimate beside it.
	c := countedProcess(t, true, countedDoc(countedT0.Add(time.Hour)))
	delete(c.startSeconds, confirmKey)
	assert.Empty(t, c.exportLearnedState().StartCounted)
}

// A Ready time ahead of the clock past the skew is not installed: that Pod
// is folded as a new one. A key too short to be one is ignored.
func TestStartCounted_AnUnusableTimeIsNotInstalled(t *testing.T) {
	s := countedDoc(countedT0.Add(time.Hour))
	s.StartCounted[countedKey("d0")] = countedT0.Add(2*time.Hour + maxClockSkew + time.Second)
	s.StartCounted[countedKey("d1")] = time.Time{}
	s.StartCounted["no-bars"] = countedT0
	a := countedProcess(t, true, s)
	assert.Len(t, a.startCountedRestored, 2)
}

// Off, nothing is stored, nothing is claimed: the document the parent wrote.
func TestStartCounted_OffStoresNothing(t *testing.T) {
	a := countedProcess(t, false, countedDoc(countedT0.Add(time.Hour)))
	assert.Empty(t, a.startCountedRestored)
	confirmCycle(a, logr.Discard(), fourRows(90)...)
	assert.Empty(t, a.startCountedReady)
	assert.Nil(t, a.exportLearnedState().StartCounted)
}

// A Pod that left is forgotten, stored or live, so the document holds the
// running fleet; the variant's other Pods stay; another key's stay too.
func TestStartCounted_APodThatLeftGoes(t *testing.T) {
	a := countedProcess(t, true, countedDoc(countedT0.Add(time.Hour)))
	other := "ns|model|other|H100|1|ns|d0"
	a.startCountedRestored[other] = countedT0
	a.startCountedReady[other] = countedT0
	a.startSeenPods[other] = a.now()
	confirmCycle(a, logr.Discard(), fourRows(90)[:2]...)
	assert.Len(t, a.startSeenPods, 3, "two here and the other key's")
	assert.Equal(t, map[string]time.Time{other: countedT0}, a.startCountedRestored)
	assert.Len(t, a.startCountedReady, 3)
}

// The sweep: a stale key's stored times go with it, and a live Pod past the
// timeout goes with its counted record.
func TestStartCounted_TheSweepBoundsThem(t *testing.T) {
	a := countedProcess(t, true, countedDoc(countedT0.Add(time.Hour)))
	a.startCountedRestored["gone-key|ns|p"] = countedT0
	a.startCountedRestored[countedKey("d0")] = countedT0
	now := a.now()
	a.startSeenPods[countedKey("seen")] = now.Add(-2 * time.Hour)
	a.startCountedReady[countedKey("seen")] = countedT0
	a.mu.Lock()
	dropped := a.evictStartSeenPods(now, time.Hour)
	a.mu.Unlock()
	assert.Equal(t, 1, dropped)
	assert.NotContains(t, a.startCountedReady, countedKey("seen"))
	assert.NotContains(t, a.startCountedRestored, "gone-key|ns|p")
	assert.Contains(t, a.startCountedRestored, countedKey("d0"), "a live key's stored time stays")
}

// A forget of the variant drops the stored times, so its running Pods are
// measured afresh: the operator's remedy (troubleshooting, "Reset one
// variant's learned state") still works after a restart. Its key prefix is
// the analyzer's, so the document's keys are built for it.
func TestStartCounted_AForgetDropsThem(t *testing.T) {
	a := countedProcess(t, true, countedDoc(countedT0.Add(time.Hour)))
	a.ForgetVariant("ns", "model", confirmVariant, "token")
	assert.Empty(t, a.startCountedRestored)
	confirmCycle(a, logr.Discard(), fourRows(90)...)
	assert.InDelta(t, 90.0, a.startSeconds[confirmKey], 0.01)
}

// ForgetLearnedState, the snapshot deleted, drops what is stored and not yet
// claimed.
func TestStartCounted_AFlushDropsThem(t *testing.T) {
	a := countedProcess(t, true, countedDoc(countedT0.Add(time.Hour)))
	a.ForgetLearnedState()
	assert.Empty(t, a.startCountedRestored)
}

// A second restart inside the first keeps every count: the stored times no
// row claimed go out with the ones claimed, and the pending path keeps
// a variant's that never ran.
func TestStartCounted_ASecondRestartKeepsThem(t *testing.T) {
	doc := countedDoc(countedT0.Add(time.Hour))
	a := countedProcess(t, true, doc)
	confirmCycle(a, logr.Discard(), fourRows(90)[:0]...)
	a = countedProcess(t, true, doc)
	assert.Equal(t, doc.StartCounted, a.exportLearnedState().StartCounted, "unclaimed: exported as stored")
	confirmCycle(a, logr.Discard(), fourRows(90)...)
	assert.Equal(t, doc.StartCounted, a.exportLearnedState().StartCounted, "claimed: the same times")

	b := NewSaturationAnalyzer(capacity.NewStore())
	b.now = a.now
	b.mu.Lock()
	b.pendingRestore["ns/"+confirmVariant] = &pendingVariant{expect: "fp", state: doc}
	b.mu.Unlock()
	assert.Equal(t, doc.StartCounted, b.exportLearnedState().StartCounted, "pending: out with its estimate")
}

// Through the document and Analyze: an export, its JSON, a new process and
// the restore's report. The variant's pending entries carry the counted Pods
// to the first cycle, and a key that names no variant is ignored. The
// estimate holds at the running Pods' first figure.
func TestStartCounted_ThroughTheDocumentAndAnalyze(t *testing.T) {
	const variant = "variant-a"
	states := []domain.VariantReplicaState{{VariantName: variant, Role: domain.RoleDecode, AcceleratorName: "H200",
		CurrentReplicas: 2, GPUsPerReplica: 1}}
	row := func(pod string, start float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, variant, 8000, 16000, 0, 1000, 500)
		rm.Namespace = "test-ns"
		rm.StartSeconds = start
		rm.ReadySince = countedT0
		return rm
	}
	clock := countedT0.Add(time.Hour)
	process := func(on bool) *SaturationAnalyzer {
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", variant, capacity.Record{EngineParams: mtParams(64, "vllm:v1")}, time.Now())
		a := NewSaturationAnalyzer(store)
		a.now = func() time.Time { return clock }
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchStartCountedPodsRestored: on}))
		return a
	}
	run := func(on bool) (float64, RestoreReport) {
		first := process(on)
		ctx, _ := observedCtx(t)
		_, err := first.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{row("p0", 70), row("p1", 70)}, states))
		require.NoError(t, err)
		raw, err := json.Marshal(first.ExportLearnedState())
		require.NoError(t, err)
		var doc LearnedState
		require.NoError(t, json.Unmarshal(raw, &doc))
		if on {
			require.Len(t, doc.StartCounted, 2)
		} else {
			require.Empty(t, doc.StartCounted)
		}
		doc.StartCounted = addTo(doc.StartCounted, "no-variant|x|y", countedT0)

		second := process(on)
		rep := second.RestoreLearnedState(doc)
		clock = clock.Add(15 * time.Second)
		for range 3 {
			_, err = second.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{row("p0", 90), row("p1", 90)}, states))
			require.NoError(t, err)
			clock = clock.Add(15 * time.Second)
		}
		s := second.ExportLearnedState()
		require.Len(t, s.StartSeconds, 1)
		for _, v := range s.StartSeconds {
			return v, rep
		}
		return 0, rep
	}
	got, rep := run(true)
	assert.Equal(t, 70.0, got, "on: the stored Ready times count the Pods")
	assert.Equal(t, 1, rep.StartKeys)
	assert.Zero(t, rep.Unattributable, "a counted key is no entry of its own")
	got, _ = run(false)
	assert.InDelta(t, 0.7*(0.7*70+0.3*90)+0.3*90, got, 1e-9, "off: both are folded again")
}

func addTo(m map[string]time.Time, k string, v time.Time) map[string]time.Time {
	if m == nil {
		m = map[string]time.Time{}
	}
	m[k] = v
	return m
}

// The key helper, over the shapes a key takes.
func TestStartCountedKeyOf(t *testing.T) {
	for in, want := range map[string]string{
		countedKey("p"):       confirmKey,
		"a|ns|p":              "a",
		"|ns|p":               "",
		"ns|p":                "",
		"p":                   "",
		"":                    "",
		"a|b|c|d|e|ns|pod-0":  "a|b|c|d|e",
		"a|b|c|d|e||":         "a|b|c|d|e",
		"ns|m/x|v|H100|1|n|p": "ns|m/x|v|H100|1",
	} {
		got, ok := startCountedKeyOf(in)
		assert.Equal(t, want, got, in)
		assert.Equal(t, want != "", ok, in)
	}
}
