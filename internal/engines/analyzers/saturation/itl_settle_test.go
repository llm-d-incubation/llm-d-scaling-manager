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
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// The ITL line on the settle tracker (itl_settle.go, analyzer-learner.md PR
// 4a): each of the four switches, through Analyze, on the engine's rig
// (restartRig: the engine's tracker observed before each cycle). Each test
// runs the switch on and off; off is the line before 4a, the negative
// control the switch must fail.

// itlSettleT0 is the rig's controller start.
var itlSettleT0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// itlSettleCycles is how many cycles the default steadyGate.steadyFor (2
// min) spans at the rig's 15 s.
var itlSettleCycles = int(itl.DefaultSteadyFor / restartCycle)

// itlSettleRow is a decode replica serving at fill k, every gauge read: a
// reading for the line, Ready an hour before the rig starts.
func itlSettleRow(pod string, k float64) domain.ReplicaMetrics {
	rm := makeReplicaMetrics(pod, "v", int64(k*376_480), 376_480, 0, 1000, 500)
	rm.KvCacheUsageKnown = true
	rm.KvUsageInstant, rm.KvUsageInstantKnown = k, true
	rm.AvgITL, rm.AvgITLKnown = 0.008+0.04*k, true
	rm.GenerationTokenRate, rm.GenerationTokenRateKnown = 1200, true
	rm.RequestRate, rm.RequestRateKnown = 2.4, true
	rm.ReadySince = itlSettleT0.Add(-time.Hour)
	return rm
}

// itlSettleFleet is three replicas at distinct, steady fills.
func itlSettleFleet() []domain.ReplicaMetrics {
	return []domain.ReplicaMetrics{itlSettleRow("p0", 0.30), itlSettleRow("p1", 0.45), itlSettleRow("p2", 0.60)}
}

// itlRig is a restartRig with switches set before its first cycle.
func itlRig(t *testing.T, switches map[string]bool) *restartRig {
	t.Helper()
	r := newRestartRig(t, itlSettleT0)
	require.NoError(t, r.a.SetSwitchesForReplay(switches))
	return r
}

// itlCycle runs one cycle and returns the ITL line's report of variant v:
// its counts by reason.
func itlCycle(t *testing.T, r *restartRig, rows []domain.ReplicaMetrics) map[learn.Reason]int64 {
	t.Helper()
	r.cycleCtx(context.Background(), rows, decodeVariants("v"))
	reports := reportsOf(r.a.LearnerReports()["test-ns|test-model"], learn.ITL)
	require.Len(t, reports, 1, "one ITL report for the one decode variant")
	return reports[0].Counts
}

// settlingRun is the cycles, of n, on which some reading was kept out as its
// replica's settle.
func settlingRun(t *testing.T, r *restartRig, n int, rows func(i int) []domain.ReplicaMetrics) []int {
	t.Helper()
	var out []int
	for i := range n {
		if itlCycle(t, r, rows(i))["settling"] > 0 {
			out = append(out, i)
		}
	}
	return out
}

// cyclesFrom is the n cycles from i.
func cyclesFrom(i, n int) []int {
	out := make([]int, n)
	for j := range out {
		out[j] = i + j
	}
	return out
}

// A controller restart is a settle: every replica already serving on the
// line's first cycle waits steadyFor, from the tracker's first sight of it,
// so the line does not refit from a fleet the controller has not watched.
// Off, as before 4a, they are taken as settled from the first cycle.
func TestITLSettle_AControllerRestartWaitsTheSettle(t *testing.T) {
	fleet := func(int) []domain.ReplicaMetrics { return itlSettleFleet() }
	on := settlingRun(t, itlRig(t, nil), 2*itlSettleCycles, fleet)
	assert.Equal(t, cyclesFrom(0, itlSettleCycles), on, "each cycle under steadyFor from the restart waits, and none after")

	off := settlingRun(t, itlRig(t, map[string]bool{SwitchITLWaitsAfterRestart: false}), 2*itlSettleCycles, fleet)
	assert.Empty(t, off, "switch off: the first cycle's replicas are taken as settled")
}

// The analyzer alone stands for a controller that has watched the fleet:
// with no engine tracker, nothing waits on its first cycle, whatever the
// switch says.
func TestITLSettle_TheAnalyzerAloneDoesNotWait(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	now := itlSettleT0
	a.now = func() time.Time { return now }
	for range 3 {
		_, err := a.Analyze(context.Background(), makeAnalyzerInput(itlSettleFleet(), decodeVariants("v")))
		require.NoError(t, err)
		reports := reportsOf(a.LearnerReports()["test-ns|test-model"], learn.ITL)
		require.Len(t, reports, 1)
		assert.Zero(t, reports[0].Counts["settling"])
		assert.Positive(t, reports[0].Counts["offered"], "premise: the rows are readings")
		now = now.Add(restartCycle)
	}
}

// One missing row is a missed scrape, not a replica that left: its settle
// is kept. Off, as before 4a, the replica starts its settle over when it
// comes back and its readings stay out for steadyFor.
func TestITLSettle_OneMissingRowKeepsTheSettle(t *testing.T) {
	missAt := itlSettleCycles + 2
	rows := func(i int) []domain.ReplicaMetrics {
		f := itlSettleFleet()
		if i == missAt {
			return f[:2]
		}
		return f
	}
	n := missAt + itlSettleCycles + 3
	on := settlingRun(t, itlRig(t, nil), n, rows)
	assert.Equal(t, cyclesFrom(0, itlSettleCycles), on, "only the restart's settle: the missed row keeps p2's")

	off := settlingRun(t, itlRig(t, map[string]bool{SwitchITLKeepsSettleThroughMissingRow: false}), n, rows)
	assert.Equal(t, append(cyclesFrom(0, itlSettleCycles), cyclesFrom(missAt+1, itlSettleCycles)...), off,
		"switch off: p2 back after one missed row waits steadyFor again")
}

// An absence past the tracker's tolerance (1 min at a 15 s interval) is a
// replica that left and came back (settle.Rejoined): its settle starts
// over, switch on or off.
func TestITLSettle_AnAbsencePastTheToleranceStartsTheSettleAgain(t *testing.T) {
	missFrom, missFor := itlSettleCycles+2, 5 // 75 s without a row
	rows := func(i int) []domain.ReplicaMetrics {
		f := itlSettleFleet()
		if i >= missFrom && i < missFrom+missFor {
			return f[:2]
		}
		return f
	}
	back := missFrom + missFor
	n := back + itlSettleCycles + 3
	want := append(cyclesFrom(0, itlSettleCycles), cyclesFrom(back, itlSettleCycles)...)
	assert.Equal(t, want, settlingRun(t, itlRig(t, nil), n, rows))
	assert.Equal(t, want, settlingRun(t, itlRig(t, map[string]bool{SwitchITLKeepsSettleThroughMissingRow: false}), n, rows))
}

// A new Ready time under the same Pod name is an engine restart
// (settle.Restarted), whether or not a row that was not Ready was read in
// between: the replica's readings wait steadyFor. Off
// (SwitchITLRestartUnderName), as on the line's own trace, only a missing or
// not-Ready row shows a restart, and the replica is taken as settled.
func TestITLSettle_ANewReadyTimeUnderTheSameNameStartsTheSettle(t *testing.T) {
	restartAt := itlSettleCycles + 2
	rows := func(i int) []domain.ReplicaMetrics {
		f := itlSettleFleet()
		if i >= restartAt {
			f[2].ReadySince = itlSettleT0.Add(time.Duration(restartAt)*restartCycle - 5*time.Second)
		}
		return f
	}
	n := restartAt + itlSettleCycles + 3
	on := settlingRun(t, itlRig(t, nil), n, rows)
	assert.Equal(t, append(cyclesFrom(0, itlSettleCycles), cyclesFrom(restartAt, itlSettleCycles)...), on)

	off := settlingRun(t, itlRig(t, map[string]bool{SwitchITLRestartUnderName: false}), n, rows)
	assert.Equal(t, cyclesFrom(0, itlSettleCycles), off, "switch off: the line's own trace misses the restart")
}

// A scrape that read nothing -- every gauge unknown, the Pod still listed
// and Ready -- is not idleness: two of them do not make a busy replica
// idle, and the next good row is not load returning after idle. Off
// (SwitchITLIdleOnEvidence), as on the line's own trace, a row with neither
// an ITL nor a k is idle, and the replica waits steadyFor after two failed
// scrapes.
func TestITLSettle_AScrapeThatReadNothingIsNotIdle(t *testing.T) {
	failFrom := itlSettleCycles + 2
	rows := func(i int) []domain.ReplicaMetrics {
		f := itlSettleFleet()
		if i == failFrom || i == failFrom+1 {
			f[2] = domain.ReplicaMetrics{PodName: "p2", VariantName: "v", ModelID: "test-model", Namespace: "test-ns",
				Ready: true, ReadySince: f[2].ReadySince}
		}
		return f
	}
	n := failFrom + 2 + itlSettleCycles + 3
	on := settlingRun(t, itlRig(t, nil), n, rows)
	assert.Equal(t, cyclesFrom(0, itlSettleCycles), on, "the failed scrapes keep p2's settle")

	off := settlingRun(t, itlRig(t, map[string]bool{SwitchITLIdleOnEvidence: false}), n, rows)
	assert.Equal(t, append(cyclesFrom(0, itlSettleCycles), cyclesFrom(failFrom+2, itlSettleCycles)...), off,
		"switch off: two rows with neither an ITL nor a k idled p2")
}

// Idle on positive evidence: a known KV of 0 and a known empty ITL window,
// for steadyGate.idleRows rows, then load comes back. The replica's first
// readings carry its warm-up, so it waits steadyFor, on or off.
func TestITLSettle_LoadBackAfterAnIdleRunStartsTheSettle(t *testing.T) {
	idleFrom := itlSettleCycles + 2
	rows := func(i int) []domain.ReplicaMetrics {
		f := itlSettleFleet()
		if i == idleFrom || i == idleFrom+1 {
			f[2].KvCacheUsage, f[2].KvUsageInstant, f[2].TokensInUse = 0, 0, 0
			f[2].AvgITL, f[2].AvgITLKnown, f[2].AvgITLEmpty = 0, false, true
			f[2].GenerationTokenRate, f[2].RequestRate = 0, 0
		}
		return f
	}
	n := idleFrom + 2 + itlSettleCycles + 3
	want := append(cyclesFrom(0, itlSettleCycles), cyclesFrom(idleFrom+2, itlSettleCycles)...)
	assert.Equal(t, want, settlingRun(t, itlRig(t, nil), n, rows))
	assert.Equal(t, want, settlingRun(t, itlRig(t, map[string]bool{SwitchITLReadsSettle: false}), n, rows))
}

// A Pod with an Absent rank carries its Finite ranks' mean in AvgITL with
// AvgITLKnown false: no reading. Off, as before the presence bits, any
// finite AvgITL above zero was one.
func TestITLSettle_APodWithAnAbsentRankIsNoReading(t *testing.T) {
	rows := func() []domain.ReplicaMetrics {
		f := itlSettleFleet()
		f[2].AvgITLKnown = false // one rank's ITL query failed: the pod is Absent
		return f
	}
	for _, tc := range []struct {
		on             bool
		offered, noITL int64
		description    string
	}{
		{true, 2, 1, "on: the Absent pod is no reading"},
		{false, 3, 0, "off: its Finite ranks' mean was a reading"},
	} {
		r := itlRig(t, map[string]bool{SwitchITLReadsFinitePodMean: tc.on})
		got := itlCycle(t, r, rows())
		assert.Equal(t, tc.offered, got["offered"], tc.description)
		assert.Equal(t, tc.noITL, got["no-itl"], tc.description)
	}
}

// The six switches are each other's: each is read on its own. A replay
// that turns one off leaves the others on.
func TestITLSettle_EachSwitchIsItsOwnField(t *testing.T) {
	names := []string{SwitchITLReadsSettle, SwitchITLWaitsAfterRestart, SwitchITLKeepsSettleThroughMissingRow,
		SwitchITLReadsFinitePodMean, SwitchITLRestartUnderName, SwitchITLIdleOnEvidence}
	for _, off := range names {
		a := NewSaturationAnalyzer(capacity.NewStore())
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{off: false}))
		got := map[string]bool{}
		for _, name := range names {
			got[name] = *a.switchFields()[name]
		}
		for _, name := range names {
			assert.Equal(t, name != off, got[name], fmt.Sprintf("%s off: %s", off, name))
		}
	}
}

// Review r1 (B1): a settle start only moves forward. The tracker keeps a
// replica's rows for settle.TraceHorizon (5 min); once the row that was not
// Ready ages out of it, the start the tracker reads falls back to the
// replica's earlier settle. That is no event, and the replica does not
// settle again. Before the fix it did, 5 min after the blip.
func TestITLSettle_ANotReadyRowAgeingOutStartsNoSettle(t *testing.T) {
	blipAt := itlSettleCycles + 2
	rows := func(i int) []domain.ReplicaMetrics {
		f := itlSettleFleet()
		if i == blipAt {
			f[2].Ready = false
		}
		return f
	}
	n := blipAt + int(10*time.Minute/restartCycle)
	got := settlingRun(t, itlRig(t, nil), n, rows)
	assert.Equal(t, append(cyclesFrom(0, itlSettleCycles), cyclesFrom(blipAt+1, itlSettleCycles)...), got,
		"the blip's settle, and none when its row leaves the tracker's trace")
}

// Review r1 (B1): the tracker keeps a replica's last eight returns of load;
// eight short idle spells after the one return that met idleRows evict it,
// and the start the tracker reads falls back. The replica does not settle
// again.
func TestITLSettle_AReturnEvictedFromTheTrackerStartsNoSettle(t *testing.T) {
	idleAt := itlSettleCycles + 2
	idle := func(rm *domain.ReplicaMetrics) {
		rm.KvCacheUsage, rm.KvUsageInstant, rm.TokensInUse = 0, 0, 0
		rm.AvgITL, rm.AvgITLKnown, rm.AvgITLEmpty = 0, false, true
		rm.GenerationTokenRate, rm.RequestRate = 0, 0
	}
	// Two idle rows, load back at idleAt+2 (a return that meets idleRows),
	// then eight single idle rows, each followed by load.
	short := idleAt + 2 + itlSettleCycles + 2
	rows := func(i int) []domain.ReplicaMetrics {
		f := itlSettleFleet()
		switch {
		case i == idleAt || i == idleAt+1:
			idle(&f[2])
		case i >= short && i < short+16 && (i-short)%2 == 0:
			idle(&f[2])
		}
		return f
	}
	n := short + 16 + itlSettleCycles + 3
	got := settlingRun(t, itlRig(t, nil), n, rows)
	assert.Equal(t, append(cyclesFrom(0, itlSettleCycles), cyclesFrom(idleAt+2, itlSettleCycles)...), got,
		"the return's settle, and none when the tracker evicts it")
}
