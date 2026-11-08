package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// k2's wait after a rollout (analyzer-learner.md, 4.2 and 4.3;
// SwitchK2WaitsAfterRollout).

// revisedFull is p0 at its queue, Ready since ready, on revision.
func revisedFull(ready time.Time, revision string) []domain.ReplicaMetrics {
	row := fullRow("p0", "v", ready)
	row.Revision = revision
	return []domain.ReplicaMetrics{row}
}

// rolledCycles settles p0 on rev-1, rolls it to rev-2 under its name and Ready
// time, and returns the k2 sources of the cycles after: the wait a new Ready
// time would start, read from the tracker's RolledOut.
func rolledCycles(t *testing.T, switches map[string]bool, from string) []string {
	t.Helper()
	r := newRestartRig(t, restartT0)
	require.NoError(t, r.a.SetSwitchesForReplay(switches))
	ready := restartT0.Add(-time.Hour)
	settled(t, r, func() []domain.ReplicaMetrics { return revisedFull(ready, from) }, decodeVariants("v"))
	got := make([]string, 0, waitWindow+1)
	for range waitWindow + 1 {
		got = append(got, r.cycle(revisedFull(ready, "rev-2"), decodeVariants("v"))...)
	}
	return got
}

// On, a new revision under the same key waits a window, and is admitted after
// it. Off, it is read at once: this is the reading a k2 is written low from,
// and a low k2 is permanent (3.4).
func TestK2Rollout_ANewRevisionWaitsAWindow(t *testing.T) {
	on := rolledCycles(t, nil, "rev-1")
	for i, source := range on[:waitWindow] {
		assert.Equal(t, k2ReasonObsUnsettled, source, "cycle %d after the rollout", i)
	}
	assert.Equal(t, p1Obs, on[waitWindow], "the wait ends: a window, not for good")

	for i, source := range rolledCycles(t, map[string]bool{SwitchK2WaitsAfterRollout: false}, "rev-1") {
		assert.Equal(t, p1Obs, source, "negative control: off, cycle %d is read at once", i)
	}
}

// A revision read for the first time is no rollout: an unknown turned known
// starts nothing (settle.Tracker.note), and a fleet whose revisions start to
// be read does not wait.
func TestK2Rollout_AFirstRevisionIsNotOne(t *testing.T) {
	for i, source := range rolledCycles(t, nil, "") {
		assert.Equal(t, p1Obs, source, "cycle %d", i)
	}
}

// With the memo of Ready times (SwitchK2ReadsSettle off) the switch reads
// nothing: there is no tracker to read.
func TestK2Rollout_TheMemoDoesNotWait(t *testing.T) {
	for i, source := range rolledCycles(t, map[string]bool{SwitchK2ReadsSettle: false}, "rev-1") {
		assert.Equal(t, p1Obs, source, "cycle %d", i)
	}
}

// A wait begun later than the rollout stands: the rollout does not move it
// back. Past the rollout's window, a row that was not Ready starts a wait of
// its own.
func TestK2Rollout_ALaterWaitIsNotMovedBack(t *testing.T) {
	r := newRestartRig(t, restartT0)
	ready := restartT0.Add(-time.Hour)
	settled(t, r, func() []domain.ReplicaMetrics { return revisedFull(ready, "rev-1") }, decodeVariants("v"))
	for range waitWindow + 2 {
		r.cycle(revisedFull(ready, "rev-2"), decodeVariants("v"))
	}
	notReady := revisedFull(ready, "rev-2")
	notReady[0].Ready = false
	r.cycle(notReady, decodeVariants("v"))
	assert.Equal(t, []string{k2ReasonObsUnsettled}, r.cycle(revisedFull(ready, "rev-2"), decodeVariants("v")))
}

// Only a rollout starts this wait: a new lend under a bridge's key moves the
// tracker's Since too, and is no join.
func TestK2Rollout_OnlyARolloutStartsTheWait(t *testing.T) {
	tr := settle.NewTracker()
	a := NewSaturationAnalyzer(nil).WithSettleView(tr)
	early := restartT0.Add(-time.Hour)
	row := domain.ReplicaMetrics{PodName: "p0", VariantName: "v", Ready: true, ReadySince: early, FromWarmPool: true,
		LentSince: early}
	model := settleModel("ns", "m")
	roles := map[string]string{"v": domain.RoleDecode}
	lentAgain := restartT0.Add(JoinedReplicaWindow + time.Minute)
	for at := restartT0; at.Before(lentAgain); at = at.Add(15 * time.Second) {
		tr.Observe(model, settlerow.Of([]domain.ReplicaMetrics{row}, roles), at)
	}
	row.LentSince = lentAgain
	tr.Observe(model, settlerow.Of([]domain.ReplicaMetrics{row}, roles), lentAgain)
	r, ok := tr.Replica(model, settlerow.Key(row))
	require.True(t, ok)
	require.Equal(t, settle.Lent, r.Reason, "premise: the lend moved the tracker's settle")
	require.Equal(t, lentAgain, r.Since)
	in := k2SettleInput{namespace: "ns", modelID: "m", variant: "v", pod: "p0", ready: true, bridge: true, readyAt: early}
	joined, _, _, _ := a.k2Joined(in, restartT0, lentAgain)
	assert.False(t, joined)
}

// A new Ready time waits whichever way the switch is: only the revision that
// arrives with the Ready time unchanged depends on it, so the switch adds a
// wait and shortens none.
func TestK2Rollout_ANewReadyTimeWaitsEitherWay(t *testing.T) {
	for _, on := range []bool{true, false} {
		r := newRestartRig(t, restartT0)
		require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchK2WaitsAfterRollout: on}))
		ready := restartT0.Add(-time.Hour)
		settled(t, r, func() []domain.ReplicaMetrics { return revisedFull(ready, "rev-1") }, decodeVariants("v"))
		got := r.cycle(revisedFull(r.now, "rev-2"), decodeVariants("v"))
		assert.Equal(t, []string{k2ReasonObsUnsettled}, got, "switch %v", on)
	}
}
