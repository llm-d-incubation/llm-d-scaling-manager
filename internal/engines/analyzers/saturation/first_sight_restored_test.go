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
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
)

// First sight through a controller restart, on the readers (analyzer-learner.md,
// PR 5, change E; settle.SwitchFirstSightRestored): the restart rig with the
// new process given the old tracker's sightings, through their JSON, as the
// snapshot gives them.

// snapshotRig is a restart rig whose variants have their configuration on
// record, so a restored document installs on their first cycle.
func snapshotRig(t *testing.T, start time.Time, variants ...string) *restartRig {
	t.Helper()
	r := newRestartRig(t, start)
	for _, v := range variants {
		r.a.capacityStore.Update("test-ns", "test-model", v, capacity.Record{EngineParams: mtParams(1024, "vllm:v1")}, start)
	}
	return r
}

// restartFromSnapshot is the controller restarting at the rig's clock, a
// new rig holding what the old one exported, through its JSON: the snapshot
// on. firstSight is the switch.
func restartFromSnapshot(t *testing.T, r *restartRig, firstSight bool, variants ...string) *restartRig {
	t.Helper()
	raw, err := json.Marshal(r.tr.Sightings())
	require.NoError(t, err)
	var sightings map[string][]settle.Sighting
	require.NoError(t, json.Unmarshal(raw, &sightings))
	raw, err = json.Marshal(r.a.ExportLearnedState())
	require.NoError(t, err)
	var state LearnedState
	require.NoError(t, json.Unmarshal(raw, &state))

	next := snapshotRig(t, r.now, variants...)
	require.NoError(t, next.tr.SetSwitchesForReplay(map[string]bool{settle.SwitchFirstSightRestored: firstSight}))
	next.a.RestoreLearnedState(state)
	next.tr.Restore(sightings, next.now)
	return next
}

// fourFull is four replicas Ready for an hour before restartT0, full and
// queued.
func fourFull() []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, 4)
	for i := range 4 {
		out = append(out, fullRow(fmt.Sprintf("d%d", i), "v", restartT0.Add(-time.Hour)))
	}
	return out
}

func fourStates() []domain.VariantReplicaState {
	s := decodeVariants("v")
	s[0].CurrentReplicas = 4
	return s
}

// The design's negative control: four replicas Ready for 1 h, a controller
// restart with the snapshot on. The parent refuses every k2 reading
// (P1-obs-unsettled) for 1 min; the PR admits them on the first cycle.
func TestFirstSight_ARestartWithTheSnapshotDoesNotWait(t *testing.T) {
	before := snapshotRig(t, restartT0, "v")
	settled(t, before, fourFull, fourStates())

	on := restartFromSnapshot(t, before, true, "v")
	for i := range waitWindow + 1 {
		assert.Equal(t, []string{p1Obs, p1Obs, p1Obs, p1Obs}, on.cycle(fourFull(), fourStates()), "on, cycle %d", i)
	}

	off := restartFromSnapshot(t, before, false, "v")
	for i := range waitWindow {
		d := off.cycle(fourFull(), fourStates())
		assert.Equal(t, []string{k2ReasonObsUnsettled, k2ReasonObsUnsettled, k2ReasonObsUnsettled, k2ReasonObsUnsettled},
			d, "off, cycle %d: under a window", i)
	}
	assert.Equal(t, []string{p1Obs, p1Obs, p1Obs, p1Obs}, off.cycle(fourFull(), fourStates()), "off: a window on")
}

// With the snapshot off nothing is restored: the same tracker, handed
// nothing, waits a window. Every path to a restored sighting is the snapshot's.
func TestFirstSight_NoSnapshotNoSighting(t *testing.T) {
	r := newRestartRig(t, restartT0)
	for i := range waitWindow {
		assert.Equal(t, k2ReasonObsUnsettled, r.cycle(fourFull(), fourStates())[0], "cycle %d", i)
	}
}

// What bounds the lower fleet: a replica that really restarted still waits.
// An engine restart under the same name while the controller was down, on
// one of four replicas: its row carries a new Ready time, so it waits a
// window from the later of that and first sight, and the other three do not.
func TestFirstSight_AnEngineRestartUnderTheSameNameStillWaits(t *testing.T) {
	before := snapshotRig(t, restartT0, "v")
	settled(t, before, fourFull, fourStates())
	after := restartFromSnapshot(t, before, true, "v")
	seen := after.now
	rows := func() []domain.ReplicaMetrics {
		out := fourFull()
		out[1].ReadySince = seen.Add(-10 * time.Second)
		return out
	}
	for i := range waitWindow + 1 {
		d := after.cycle(rows(), fourStates())
		require.Len(t, d, 4)
		at := seen.Add(time.Duration(i) * restartCycle)
		assert.Equal(t, p1Obs, d[0], "cycle %d", i)
		assert.Equal(t, verdictAt(at, seen), d[1], "the restarted engine waits from first sight, cycle %d", i)
		assert.Equal(t, p1Obs, d[2], "cycle %d", i)
		assert.Equal(t, p1Obs, d[3], "cycle %d", i)
	}
}

// An LWS group restart while the controller was down: leader and workers
// recreated, Pod names kept, the group's row carrying the latest Ready time
// over its Pods (#361, TestLWSGroupReadyTimeIsTheLatestOfItsPods). Group 0
// is recreated and waits, group 1 is not reached and does not; group 2's
// leader kept its Ready time and a worker restarted alone, so the row's
// time moved: it waits too.
func TestFirstSight_AnLWSGroupRestartStillWaits(t *testing.T) {
	old := restartT0.Add(-time.Hour)
	states := decodeVariants("lws")
	states[0].CurrentReplicas = 3
	leaders := func(g0, g1, g2 time.Time) []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{fullRow("lws-0", "lws", g0), fullRow("lws-1", "lws", g1), fullRow("lws-2", "lws", g2)}
	}
	before := snapshotRig(t, restartT0, "lws")
	settled(t, before, func() []domain.ReplicaMetrics { return leaders(old, old, old) }, states)
	after := restartFromSnapshot(t, before, true, "lws")
	seen := after.now
	g0, g2 := seen.Add(-20*time.Second), seen.Add(-5*time.Second)
	for i := range waitWindow + 1 {
		d := after.cycle(leaders(g0, old, g2), states)
		require.Len(t, d, 3)
		at := seen.Add(time.Duration(i) * restartCycle)
		assert.Equal(t, verdictAt(at, seen), d[0], "the recreated group, cycle %d", i)
		assert.Equal(t, p1Obs, d[1], "the group not reached, cycle %d", i)
		assert.Equal(t, verdictAt(at, seen), d[2], "the worker restarted alone, cycle %d", i)
	}
}

// A leader not Ready on the controller's first row loses its sighting, and
// when it comes back it waits, though its worker-less row could read the
// stored time again: the first row claimed it.
func TestFirstSight_ANotReadyFirstRowLosesTheSighting(t *testing.T) {
	before := snapshotRig(t, restartT0, "v")
	settled(t, before, fourFull, fourStates())
	after := restartFromSnapshot(t, before, true, "v")
	rows := fourFull()
	rows[0].Ready = false
	after.cycle(rows, fourStates())
	back := after.now
	for i := range waitWindow + 1 {
		d := after.cycle(fourFull(), fourStates())
		require.Len(t, d, 4)
		assert.Equal(t, verdictAt(back.Add(time.Duration(i)*restartCycle), back), d[0], "cycle %d", i)
		assert.Equal(t, p1Obs, d[1], "cycle %d", i)
	}
}

// A new lend across the restart: the Pod keeps its Ready time from the pool
// and its key, so only its absence from the snapshot makes it start again.
// The balance tracker's grace (#283) reads LentSeen, which a bridge first
// seen now sets; and k2 refuses a bridge whatever its age.
func TestFirstSight_ANewLendIsABridgeFirstSeen(t *testing.T) {
	poolReady := restartT0.Add(-3 * time.Hour)
	lent := func(since time.Time) domain.ReplicaMetrics {
		rm := fullRow("pool-0", "v", poolReady)
		rm.FromWarmPool, rm.LentSince = true, since
		return rm
	}
	before := snapshotRig(t, restartT0, "v")
	rows := func() []domain.ReplicaMetrics { return append(fourFull(), lent(restartT0)) }
	states := fourStates()
	states[0].CurrentReplicas = 5
	for range waitWindow + 2 {
		before.cycle(rows(), states)
	}

	after := restartFromSnapshot(t, before, true, "v")
	seen := after.now
	d := after.cycle(append(fourFull(), lent(seen.Add(-30*time.Second))), states)
	require.Len(t, d, 5)
	assert.Equal(t, []string{p1Obs, p1Obs, p1Obs, p1Obs, k2ReasonObsUnadmitted}, d)
	r, ok := after.tr.Replica(settleModel("test-ns", "test-model"), settle.Key{Pod: "pool-0", Variant: "v", Bridge: true})
	require.True(t, ok)
	assert.False(t, r.Restored, "a bridge is not kept")
	assert.Equal(t, settle.Lent, r.Reason)
	assert.True(t, r.LentSeen.Equal(seen), "the lend's grace starts at first sight")
}

// A replica that joined under a window before the restart keeps only what
// it has left of its wait, and not a cycle less.
func TestFirstSight_AWaitInProgressKeepsItsEnd(t *testing.T) {
	before := snapshotRig(t, restartT0, "v")
	settled(t, before, fourFull, fourStates())
	joined := before.now
	rows := func() []domain.ReplicaMetrics {
		out := fourFull()
		out[0] = fullRow("new-0", "v", joined)
		return out
	}
	for range 2 {
		before.cycle(rows(), fourStates())
	}
	after := restartFromSnapshot(t, before, true, "v")
	for i := range waitWindow {
		at := after.now
		d := after.cycle(rows(), fourStates())
		require.Len(t, d, 4)
		assert.Equal(t, verdictAt(at, joined), d[0], "cycle %d, %v into the wait", i, at.Sub(joined))
		assert.Equal(t, p1Obs, d[1], "cycle %d", i)
	}
}

// The ITL line reads the restored settle too: on a key's first Observe, a
// replica waits steadyFor only if its settle began under steadyFor ago. The
// parent's line refuses each reading for 2 min after a restart; the PR's
// takes it. Read from the line's own report of its settling rows.
func TestFirstSight_TheITLLineDoesNotWaitAgain(t *testing.T) {
	const variant = "v"
	states := decodeVariants(variant)
	states[0].CurrentReplicas = 1
	row := func(k float64) domain.ReplicaMetrics {
		rm := fullRow("d0", variant, restartT0.Add(-time.Hour))
		rm.KvUsageInstant, rm.AvgITL, rm.AvgITLKnown = k, 0.05, true
		return rm
	}
	before := snapshotRig(t, restartT0, "v")
	for range 3 * waitWindow {
		before.cycle([]domain.ReplicaMetrics{row(0.5)}, states)
	}
	settling := func(firstSight bool) int64 {
		r := restartFromSnapshot(t, before, firstSight, "v")
		r.cycle([]domain.ReplicaMetrics{row(0.5)}, states)
		var n int64
		for _, rep := range r.a.LearnerReports()["test-ns|test-model"] {
			if rep.Learner == learn.ITL {
				n += rep.Counts["settling"]
			}
		}
		return n
	}
	assert.Zero(t, settling(true), "on: the line's reading is not a settling one")
	assert.Positive(t, settling(false), "off: the reading waits")
}

// learner-start says what came back. Four replicas, all restored: no
// replica waits, so settleUntil is this cycle; one cold: a window from it.
// The sightings and the start keys are counted.
func TestFirstSight_TheLearnerStartLine(t *testing.T) {
	before := snapshotRig(t, restartT0, "v")
	settled(t, before, fourFull, fourStates())

	line := func(r *restartRig, rows []domain.ReplicaMetrics) map[string]any {
		ctx, logs := observedCtx(t)
		r.cycleCtx(ctx, rows, fourStates())
		e := logs.FilterMessage("learner-start").All()
		require.Len(t, e, 1)
		return e[0].ContextMap()
	}
	all := restartFromSnapshot(t, before, true, "v")
	now := all.now
	m := line(all, fourFull())
	assert.EqualValues(t, 4, m["sightings"])
	assert.Equal(t, now.UTC().Format(time.RFC3339), m["settleUntil"], "no replica waits")
	assert.Contains(t, m["reason"], "restored")

	mixed := restartFromSnapshot(t, before, true, "v")
	rows := fourFull()
	rows[2].ReadySince = now.Add(-5 * time.Second)
	m = line(mixed, rows)
	assert.EqualValues(t, 3, m["sightings"])
	assert.Equal(t, now.Add(JoinedReplicaWindow).UTC().Format(time.RFC3339), m["settleUntil"], "the cold replica's window")

	off := restartFromSnapshot(t, before, false, "v")
	m = line(off, fourFull())
	assert.EqualValues(t, 0, m["sightings"])
	assert.Equal(t, now.Add(JoinedReplicaWindow).UTC().Format(time.RFC3339), m["settleUntil"])
	assert.NotContains(t, m["reason"], "restored")
}

// A wait in progress, every replica restored: settleUntil is when the last
// one ends, not this cycle. settleUntil covers the replicas that wait.
func TestFirstSight_SettleUntilCoversAWaitInProgress(t *testing.T) {
	before := snapshotRig(t, restartT0, "v")
	row := func() []domain.ReplicaMetrics { return []domain.ReplicaMetrics{fullRow("d0", "v", restartT0)} }
	before.cycle(row(), decodeVariants("v"))
	after := restartFromSnapshot(t, before, true, "v")
	ctx, logs := observedCtx(t)
	after.cycleCtx(ctx, row(), decodeVariants("v"))
	e := logs.FilterMessage("learner-start").All()
	require.Len(t, e, 1)
	m := e[0].ContextMap()
	assert.EqualValues(t, 1, m["sightings"])
	assert.Equal(t, restartT0.Add(JoinedReplicaWindow).UTC().Format(time.RFC3339), m["settleUntil"])
}

// learner-start counts the ITL lines a snapshot brought back, by model.
func TestFirstSight_TheLineCountsRestoredITLKeys(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	now := restartT0
	a.now = func() time.Time { return now }
	a.itlTracker.RestoreBaseline("test-ns|test-model|v|H200|1", 0.01)
	a.itlTracker.RestoreBaseline("test-ns|other|v|H200|1", 0.01)
	ctx, logs := observedCtx(t)
	_, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{fullRow("d0", "v", now.Add(-time.Hour))}, decodeVariants("v")))
	require.NoError(t, err)
	e := logs.FilterMessage("learner-start").All()
	require.Len(t, e, 1)
	assert.Equal(t, map[string]int{learn.K2: 0, learn.Mu: 0, learn.ITL: 1}, e[0].ContextMap()["restoredKeys"])
}

// learner-start counts the start keys a snapshot brought back, by model.
func TestFirstSight_TheLineCountsStartKeys(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	now := restartT0
	a.now = func() time.Time { return now }
	key := func(model string) string { return "test-ns|" + model + "|v|H200|1" }
	a.mu.Lock()
	a.installLocked(LearnedState{
		StartSeconds:  map[string]float64{key("test-model"): 70, key("other"): 70},
		VariantSeenAt: map[string]time.Time{key("test-model"): now, key("other"): now},
	}, "", true)
	a.mu.Unlock()
	ctx, logs := observedCtx(t)
	_, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{fullRow("d0", "v", now.Add(-time.Hour))}, decodeVariants("v")))
	require.NoError(t, err)
	e := logs.FilterMessage("learner-start").All()
	require.Len(t, e, 1)
	assert.EqualValues(t, 1, e[0].ContextMap()["startKeys"])
}
