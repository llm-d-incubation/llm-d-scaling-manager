package throughput

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// The ITL line on the engine's settle tracker (settle.go, learner PR 4b-2):
// each of the six switches on and off, through Analyze, on the engine's rig:
// the engine's tracker at a 15 s interval, observed before every cycle, as
// the engine observes it at the top of runAnalyzers. Off is the line 4b-1
// built, on the key's own trace: the negative control each switch must fail.

// settleSwitches is the six switches of learner PR 4b-2.
var settleSwitches = []string{
	SwitchITLReadsSettle, SwitchITLWaitsAfterRestart, SwitchITLKeepsSettleThroughMissingRow,
	SwitchITLRestartUnderName, SwitchITLIdleOnEvidence, SwitchITLReadsFinitePodMean,
}

const settleCycle = 15 * time.Second

// settleCycles is how many cycles the default steadyGate.steadyFor spans.
var settleCycles = int(itl.DefaultSteadyFor / settleCycle)

// settleRig is the throughput analyzer as the engine runs it, from a
// controller start at lineStart.
type settleRig struct {
	t     *testing.T
	a     *ThroughputAnalyzer
	tr    *settle.Tracker
	clock *clocktesting.FakePassiveClock
	now   time.Time
}

func newSettleRig(t *testing.T, switches map[string]bool) *settleRig {
	t.Helper()
	tr := settle.NewTracker()
	tr.SetInterval(settleCycle)
	c := clocktesting.NewFakePassiveClock(lineStart)
	r := &settleRig{t: t, tr: tr, clock: c, now: lineStart}
	r.a = NewThroughputAnalyzer().WithClock(c).WithSettleView(tr).WithOptimizationInterval(settleCycle)
	require.NoError(t, r.a.SetSwitchesForReplay(switches))
	return r
}

// cycle observes rows into the engine's tracker, analyzes them, moves the
// clock a cycle on, and returns what v1's line read.
func (r *settleRig) cycle(rows []domain.ReplicaMetrics) itl.Reading {
	r.t.Helper()
	r.tr.Observe("ns/m", settlerow.Of(rows, nil), r.now)
	r.clock.SetTime(r.now)
	_, err := r.a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns",
		ReplicaMetrics: rows, ArrivalRate: 4,
		VariantStates: []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: len(rows), GPUsPerReplica: 1}}})
	require.NoError(r.t, err)
	r.now = r.now.Add(settleCycle)
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	st := r.a.variantStates[variantKey("ns", "m", "v1")]
	require.NotNil(r.t, st)
	return st.reading
}

// settleFleet is three replicas of v1 on the line, at distinct steady fills,
// Ready since an hour before the run.
func settleFleet() []domain.ReplicaMetrics {
	early := lineStart.Add(-time.Hour)
	return []domain.ReplicaMetrics{
		lineRow("pod-a", 0.30, lineAt(0.30), early),
		lineRow("pod-b", 0.45, lineAt(0.45), early),
		lineRow("pod-c", 0.60, lineAt(0.60), early),
	}
}

// settlingCycles is the cycles, of n, on which a reading was kept out as
// its replica's settle.
func settlingCycles(r *settleRig, n int, rows func(i int) []domain.ReplicaMetrics) []int {
	var out []int
	for i := range n {
		if r.cycle(rows(i)).Settling > 0 {
			out = append(out, i)
		}
	}
	return out
}

// cycleSpan is n cycles from from.
func cycleSpan(from, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = from + i
	}
	return out
}

func steadySettleFleet(int) []domain.ReplicaMetrics { return settleFleet() }

// A controller restart is a settle: the engine's tracker is new, and sees
// every replica first on the first cycle (settle.Joined). The line does not
// refit for steadyFor from that first sight. Off, as 4b-1, the first
// cycle's replicas are taken as settled.
func TestITLSettle_AControllerRestartWaitsTheSettle(t *testing.T) {
	on := newSettleRig(t, nil)
	assert.Equal(t, cycleSpan(0, settleCycles), settlingCycles(on, 2*settleCycles, steadySettleFleet))
	r, ok := on.tr.Replica("ns/m", settle.Key{Pod: "pod-a", Variant: "v1"})
	require.True(t, ok)
	assert.Equal(t, settle.Joined, r.Reason, "the engine's tracker reads every replica on its first cycle as joined")
	assert.Empty(t, settlingCycles(newSettleRig(t, map[string]bool{SwitchITLWaitsAfterRestart: false}), 2*settleCycles, steadySettleFleet),
		"switch off: taken as settled")
}

// After a controller restart the line is silent for steadyFor and then
// until its window holds itl.DefaultMinSamples readings past their settle
// (steadyGate.minSettledSamples): no throughput capacity, so the analyzer
// neither orders nor vetoes a scale-down. Off, it prices once the window
// holds ten readings.
func TestITLSettle_AfterAControllerRestartTheLineIsSilentForTheSettle(t *testing.T) {
	firstOf := func(switches map[string]bool, replicas int) int {
		r := newSettleRig(t, switches)
		for i := range 4 * settleCycles {
			if r.cycle(settleFleet()[:replicas]).Tier != "" {
				return i
			}
		}
		return -1
	}
	first := func(switches map[string]bool) int { return firstOf(switches, 3) }
	on, off := first(nil), first(map[string]bool{SwitchITLWaitsAfterRestart: false})
	one := firstOf(nil, 1)
	t.Logf("first line after a controller restart: cycle %d on, cycle %d off; one replica, cycle %d on", on, off, one)
	// One replica gives one reading a cycle: the window holds ten on cycle
	// 9, and the line needs three of them past the settle.
	assert.Equal(t, settleCycles+2, one, "one replica: the first line after the restart")
	// Pinned: three replicas, 15 s cycles, steadyFor 2 min. On, the first
	// line is the first cycle past the settle (cycle 8, 2 min silent: the
	// window already holds ten readings by then); off, the fourth cycle,
	// once the window holds ten. The closed-loop probe's four replicas under
	// a growing load take 10 cycles (TestThroughputProbe_AControllerRestart
	// OnTheEngineTrackerWaitsItsSettle).
	assert.Equal(t, settleCycles, on, "the line priced inside the restart's settle, or later than its end")
	assert.Equal(t, 3, off, "switch off: the line priced other than once the window held ten readings")
}

// The analyzer alone, with no engine tracker, has watched the fleet:
// nothing waits on its first cycles, and the rows are readings.
func TestITLSettle_TheAnalyzerAloneDoesNotWait(t *testing.T) {
	r := newLineRun(t, NewThroughputAnalyzer())
	for range 3 {
		r.step(settleFleet())
		r.a.mu.Lock()
		got := r.a.variantStates[variantKey("ns", "m", "v1")].reading
		r.a.mu.Unlock()
		assert.Zero(t, got.Settling)
		assert.Equal(t, 3, got.Offered, "premise: the rows are readings")
	}
}

// The analyzer alone, after a gap longer than it keeps a model it has not
// analyzed (ownSettleTimeout): two identical models settle alike, whichever
// is analyzed first. Its own tracker is evicted inside observe, after the
// first model's rows were observed, so before the fix the first model read
// the gap as an absence (settle.Rejoined) and settled again on cycles 8 to
// 15, while the second, evicted meanwhile, was first-seen and backdated, and
// never settled. What else each keeps across the gap (the variant state, the
// line's window and held slope) is the eviction's, as before 4b-2, and is
// not compared.
func TestITLSettle_AfterALongGapTheAnalyzerAloneSettlesEveryModelAlike(t *testing.T) {
	r := newOwnRig(t, nil)
	settling, tiers := r.run(2*settleCycles+4, settleCycles, func(int) []string { return []string{"m1", "m2"} }, ownFleet)
	t.Logf("settling cycles: %v; tiers: %v", settling, tiers)
	require.NotEmpty(t, tiers["m1/v1"][len(tiers["m1/v1"])-1], "premise: the line priced after the gap")
	assert.Empty(t, settling["m1/v1"], "the model analyzed first after the gap settled: it was not first-seen")
	assert.Equal(t, settling["m2/v1"], settling["m1/v1"], "the model analyzed first after the gap settled differently")
}

// ownRig is the throughput analyzer alone, with no engine view: the line
// reads the tracker it observes itself. Cycles are settleCycle apart from
// lineStart.
type ownRig struct {
	t     *testing.T
	a     *ThroughputAnalyzer
	clock *clocktesting.FakePassiveClock
	now   time.Time
}

func newOwnRig(t *testing.T, switches map[string]bool) *ownRig {
	t.Helper()
	c := clocktesting.NewFakePassiveClock(lineStart)
	r := &ownRig{t: t, a: NewThroughputAnalyzer().WithClock(c).WithOptimizationInterval(settleCycle), clock: c, now: lineStart}
	require.NoError(t, r.a.SetSwitchesForReplay(switches))
	return r
}

// analyze is one Analyze of model at the rig's instant, with rows; it
// returns each variant's reading that cycle.
func (r *ownRig) analyze(model string, rows []domain.ReplicaMetrics) map[string]itl.Reading {
	r.t.Helper()
	r.clock.SetTime(r.now)
	var states []domain.VariantReplicaState
	seen := map[string]bool{}
	for _, rm := range rows {
		if !seen[rm.VariantName] {
			seen[rm.VariantName] = true
			states = append(states, domain.VariantReplicaState{VariantName: rm.VariantName, CurrentReplicas: 3, GPUsPerReplica: 1})
		}
	}
	_, err := r.a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: model, Namespace: "ns",
		ReplicaMetrics: rows, ArrivalRate: 4, VariantStates: states})
	require.NoError(r.t, err)
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	out := map[string]itl.Reading{}
	for v := range seen {
		st := r.a.variantStates[variantKey("ns", model, v)]
		require.NotNil(r.t, st)
		out[v] = st.reading
	}
	return out
}

// run drives n cycles, the clock jumping 2 h before cycle gapAt; each
// cycle analyzes the models order gives, in that order, with rows(model, i).
// It returns, by "model/variant", the cycles on which a reading was kept out
// as its replica's settle, and each cycle's tier.
func (r *ownRig) run(n, gapAt int, order func(i int) []string,
	rows func(model string, i int) []domain.ReplicaMetrics) (map[string][]int, map[string][]string) {
	settling, tiers := map[string][]int{}, map[string][]string{}
	for i := range n {
		if i == gapAt {
			r.now = r.now.Add(2 * time.Hour)
		}
		for _, model := range order(i) {
			for v, rd := range r.analyze(model, rows(model, i)) {
				key := model + "/" + v
				if rd.Settling > 0 {
					settling[key] = append(settling[key], i)
				}
				tiers[key] = append(tiers[key], rd.Tier)
			}
		}
		r.now = r.now.Add(settleCycle)
	}
	return settling, tiers
}

func ownFleet(string, int) []domain.ReplicaMetrics { return settleFleet() }

// Issue #511: the analyzer alone takes a model's replicas on its first
// Analyze, and on its first after a gap past ownSettleTimeout, as long
// joined: its own tracker holds each settle.Joined itl.DefaultSteadyFor
// before the cycle, its Ready time first seen then too. Observed at the
// backdate and again at the cycle, the 2 min gap was past the tracker's
// absence tolerance at a 15 s interval (1 min), and every replica read
// settle.Rejoined at the cycle. The line keeps no reading out as a settle
// either way: its keys' first sight is that cycle too.
func TestITLSettle_TheOwnTrackerBackdatesAFirstSighting(t *testing.T) {
	r := newOwnRig(t, nil)
	require.Less(t, settle.Tolerance(settleCycle), ownSettleBackdate, "premise: the backdate is past the tolerance")
	held := func(at time.Time) {
		t.Helper()
		since := at.Add(-itl.DefaultSteadyFor)
		for _, rm := range settleFleet() {
			got, ok := r.a.ownSettle.Replica("ns/m", settlerow.Key(rm))
			require.True(t, ok, "%s is not held", rm.PodName)
			assert.Equal(t, settle.Joined, got.Reason, "%s at %v: the first sighting's settle", rm.PodName, at)
			assert.Equal(t, since, got.Since, "%s at %v: the first sighting's settle began at the cycle, not backdated", rm.PodName, at)
			assert.Equal(t, since, got.FirstSeen, "%s at %v: first seen", rm.PodName, at)
			assert.Equal(t, since, got.ReadySeen, "%s at %v: its Ready time first seen", rm.PodName, at)
			assert.Equal(t, at, got.LastSeen, "%s at %v: last seen", rm.PodName, at)
		}
	}
	var settling []int
	for i := range 2*settleCycles + 4 {
		if i == settleCycles {
			r.now = r.now.Add(2 * time.Hour)
		}
		if r.analyze("m", settleFleet())["v1"].Settling > 0 {
			settling = append(settling, i)
		}
		if i == 0 || i == settleCycles {
			held(r.now)
		}
		r.now = r.now.Add(settleCycle)
	}
	assert.Empty(t, settling, "the line kept a first sighting's readings out as a settle")
	assert.Equal(t, int64(0), r.a.ownSettle.Verdicts()[settle.Rejoined], "a replica of a first sighting rejoined")

	// Off, as before: two observations, the gap read as an absence; the
	// line reads the same.
	off := newOwnRig(t, map[string]bool{SwitchITLOwnSettleBackdates: false})
	assert.Zero(t, off.analyze("m", settleFleet())["v1"].Settling, "switch off: the line kept a first sighting's readings out")
	for _, rm := range settleFleet() {
		got, ok := off.a.ownSettle.Replica("ns/m", settlerow.Key(rm))
		require.True(t, ok, "switch off: %s is not held", rm.PodName)
		assert.Equal(t, settle.Rejoined, got.Reason, "switch off: %s", rm.PodName)
		assert.Equal(t, off.now, got.Since, "switch off: %s", rm.PodName)
	}
}

// Three identical models across a gap past ownSettleTimeout, analyzed in
// two orders: every model reads alike in both, and none settles. A guard:
// before the review's round 2 only the first model analyzed after the gap
// took the stale path, and the others, evicted inside its observe, were
// first-seen without forgetting their line keys' traces. That read alike
// too, since the same observe drops their variant states at the same
// timeout and the line's keys with them (Sweep); the stale path now covers
// both, so it does not rest on the two timeouts staying equal.
func TestITLSettle_AfterALongGapTheOrderOfTheModelsDoesNotMatter(t *testing.T) {
	models := []string{"m1", "m2", "m3"}
	forward := func(int) []string { return models }
	backward := func(int) []string { return []string{"m3", "m2", "m1"} }
	n := 2*settleCycles + 4
	fwd, _ := newOwnRig(t, nil).run(n, settleCycles, forward, ownFleet)
	bwd, _ := newOwnRig(t, nil).run(n, settleCycles, backward, ownFleet)
	t.Logf("settling cycles, forward: %v; backward: %v", fwd, bwd)
	assert.Empty(t, fwd, "forward order: a model settled after the gap")
	assert.Empty(t, bwd, "backward order: a model settled after the gap")
}

// A variant with no row on the stale model's first cycle after the gap
// forgets its line key's traces too: when it comes back a cycle later it
// is first-seen, as a variant the model shows for the first time, and does
// not settle. A guard, as the order case above: forgetting only the
// variants with rows also passes today, because the stale cycle's observe
// sweeps the absent variant's key at the same timeout.
func TestITLSettle_AfterALongGapAnAbsentVariantForgetsItsTracesToo(t *testing.T) {
	rows := func(_ string, i int) []domain.ReplicaMetrics {
		f := settleFleet()
		if i == settleCycles {
			return f
		}
		for _, rm := range settleFleet() {
			rm.PodName, rm.VariantName = rm.PodName+"-2", "v2"
			f = append(f, rm)
		}
		return f
	}
	settling, tiers := newOwnRig(t, nil).run(2*settleCycles+4, settleCycles, func(int) []string { return []string{"m"} }, rows)
	t.Logf("settling cycles: %v; tiers: %v", settling, tiers)
	require.NotEmpty(t, tiers["m/v2"], "premise: v2 was analyzed")
	assert.Empty(t, settling["m/v2"], "the variant absent on the stale cycle settled when it came back")
	assert.Empty(t, settling["m/v1"])
}

// With SwitchITLReadsSettle off the line is 4b-1's own trace, and a long gap
// forgets nothing: a replica idle on its last two cycles before the gap
// whose load comes back after it waits steadyFor, as 4b-1's idle rule says.
// On, the model is first-seen after the gap and nothing settles. Kills the
// stale path run with the switch off (its traces forgotten, the idle run
// with them).
func TestITLSettle_AfterALongGapWithTheMoveOffTheOwnTraceIsKept(t *testing.T) {
	rows := func(_ string, i int) []domain.ReplicaMetrics {
		f := settleFleet()
		if i == settleCycles-2 || i == settleCycles-1 {
			settleIdle(&f[2])
		}
		return f
	}
	one := func(int) []string { return []string{"m"} }
	n := 2*settleCycles + 4
	off, _ := newOwnRig(t, map[string]bool{SwitchITLReadsSettle: false}).run(n, settleCycles, one, rows)
	on, _ := newOwnRig(t, nil).run(n, settleCycles, one, rows)
	assert.Equal(t, cycleSpan(settleCycles, settleCycles), off["m/v1"], "switch off: the idle run did not survive the gap")
	assert.Empty(t, on["m/v1"], "switch on: the model was not first-seen after the gap")
}

// The eviction forgets the model on the analyzer's own tracker and drops
// its stamp: a model not analyzed for ownSettleTimeout leaves nothing
// behind, though no Analyze of it comes again.
func TestITLSettle_TheEvictionForgetsAModelOnTheOwnTracker(t *testing.T) {
	r := newOwnRig(t, nil)
	r.analyze("m1", settleFleet())
	r.now = r.now.Add(2 * time.Hour)
	r.analyze("m2", settleFleet())
	r.a.mu.Lock()
	_, stamped := r.a.ownSettled["ns/m1"]
	r.a.mu.Unlock()
	assert.False(t, stamped, "the evicted model kept its stamp")
	_, held := r.a.ownSettle.Replica("ns/m1", settle.Key{Pod: "pod-a", Variant: "v1"})
	assert.False(t, held, "the evicted model's replicas stayed on the own tracker")
	_, held = r.a.ownSettle.Replica("ns/m2", settle.Key{Pod: "pod-a", Variant: "v1"})
	require.True(t, held, "premise: the tracker keys replicas so")
}

// One missing row keeps the replica's settle (the 1 min absence rule). Off,
// as 4b-1, the replica starts over when it comes back, and its readings are
// kept out of the pinned fit for steadyFor.
func TestITLSettle_OneMissingRowKeepsTheSettle(t *testing.T) {
	missAt := settleCycles + 2
	rows := func(i int) []domain.ReplicaMetrics {
		f := settleFleet()
		if i == missAt {
			return f[:2]
		}
		return f
	}
	n := missAt + settleCycles + 3
	assert.Equal(t, cycleSpan(0, settleCycles), settlingCycles(newSettleRig(t, nil), n, rows))
	assert.Equal(t, append(cycleSpan(0, settleCycles), cycleSpan(missAt+1, settleCycles)...),
		settlingCycles(newSettleRig(t, map[string]bool{SwitchITLKeepsSettleThroughMissingRow: false}), n, rows),
		"switch off: back after one missed row, it waits again")
}

// An absence past the tracker's tolerance (1 min at 15 s) is a replica that
// left and came back (settle.Rejoined): its settle starts again, switch on
// or off.
func TestITLSettle_AnAbsencePastTheToleranceStartsTheSettleAgain(t *testing.T) {
	missFrom, missFor := settleCycles+2, 5
	rows := func(i int) []domain.ReplicaMetrics {
		f := settleFleet()
		if i >= missFrom && i < missFrom+missFor {
			return f[:2]
		}
		return f
	}
	back := missFrom + missFor
	want := append(cycleSpan(0, settleCycles), cycleSpan(back, settleCycles)...)
	n := back + settleCycles + 3
	assert.Equal(t, want, settlingCycles(newSettleRig(t, nil), n, rows))
	assert.Equal(t, want, settlingCycles(newSettleRig(t, map[string]bool{SwitchITLKeepsSettleThroughMissingRow: false}), n, rows))
	// The key's own trace does not wait after the restart
	// (SwitchITLWaitsAfterRestart reads the tracker only).
	assert.Equal(t, cycleSpan(back, settleCycles), settlingCycles(newSettleRig(t, map[string]bool{SwitchITLReadsSettle: false}), n, rows),
		"on the key's own trace too")
}

// A new Ready time under the same Pod name is an engine restart: the
// replica waits steadyFor. Off (SwitchITLRestartUnderName), as the line's
// own trace did, it is missed.
func TestITLSettle_ANewReadyTimeUnderTheSameNameStartsTheSettle(t *testing.T) {
	restartAt := settleCycles + 2
	rows := func(i int) []domain.ReplicaMetrics {
		f := settleFleet()
		if i >= restartAt {
			f[2].ReadySince = lineStart.Add(time.Duration(restartAt)*settleCycle - 5*time.Second)
		}
		return f
	}
	n := restartAt + settleCycles + 3
	assert.Equal(t, append(cycleSpan(0, settleCycles), cycleSpan(restartAt, settleCycles)...),
		settlingCycles(newSettleRig(t, nil), n, rows))
	assert.Equal(t, cycleSpan(0, settleCycles), settlingCycles(newSettleRig(t, map[string]bool{SwitchITLRestartUnderName: false}), n, rows),
		"switch off: the restart is missed")
}

// A rollout that keeps the Pod's name (a new template revision) is a
// settle on the tracker (settle.RolledOut). Off (SwitchITLReadsSettle), on
// the key's own trace, nothing showed it.
func TestITLSettle_ANewRevisionUnderTheSameNameStartsTheSettle(t *testing.T) {
	rollAt := settleCycles + 2
	rows := func(i int) []domain.ReplicaMetrics {
		f := settleFleet()
		for j := range f {
			f[j].Revision = "rev-1"
		}
		if i >= rollAt {
			for j := range f {
				f[j].Revision = "rev-2"
			}
		}
		return f
	}
	n := rollAt + settleCycles + 3
	assert.Equal(t, append(cycleSpan(0, settleCycles), cycleSpan(rollAt, settleCycles)...),
		settlingCycles(newSettleRig(t, nil), n, rows))
	// Off, nor does the key's own trace wait after the restart
	// (SwitchITLWaitsAfterRestart reads the tracker only).
	assert.Empty(t, settlingCycles(newSettleRig(t, map[string]bool{SwitchITLReadsSettle: false}), n, rows),
		"switch off: the rollout is missed")
}

// A gap between two of the model's cycles longer than the tolerance -- the
// controller paused, its clock jumped -- is an absence to the tracker, and
// every replica settles again (settle.Rejoined). Off (SwitchITLReadsSettle),
// the key's own trace counts an absence only by cycles that went without a
// replica, and nothing settles.
func TestITLSettle_AGapBetweenCyclesPastTheToleranceSettlesAgain(t *testing.T) {
	gapAt := settleCycles + 2
	run := func(switches map[string]bool) []int {
		r := newSettleRig(t, switches)
		var out []int
		for i := range gapAt + settleCycles + 3 {
			if i == gapAt {
				r.now = r.now.Add(3 * time.Minute)
			}
			if r.cycle(settleFleet()).Settling > 0 {
				out = append(out, i)
			}
		}
		return out
	}
	assert.Equal(t, append(cycleSpan(0, settleCycles), cycleSpan(gapAt, settleCycles)...), run(nil))
	// Off, nor does the key's own trace wait after the restart
	// (SwitchITLWaitsAfterRestart reads the tracker only).
	assert.Empty(t, run(map[string]bool{SwitchITLReadsSettle: false}),
		"switch off: a gap between cycles is no absence")
}

// A Pod lent by the warm pool is no reading on either path: the analyzer
// drops it before the line (ownReplicasOnly), and the line refuses a
// FromWarmPool row. The tracker's lend settle (settle.Lent) so moves nothing
// on this line.
func TestITLSettle_ALentPodIsNoReadingOnEitherPath(t *testing.T) {
	rows := func(int) []domain.ReplicaMetrics {
		lent := lineRow("pod-lent", 0.50, lineAt(0.50), lineStart.Add(-time.Hour))
		lent.FromWarmPool, lent.LentSince = true, lineStart
		return append(settleFleet(), lent)
	}
	for _, sw := range []map[string]bool{nil, {SwitchITLReadsSettle: false}} {
		r := newSettleRig(t, sw)
		for i := range 2 * settleCycles {
			got := r.cycle(rows(i))
			assert.Equal(t, 3, got.Considered, "the lent pod reached the line (%v)", sw)
		}
	}
}

// A replica that joins after the first cycle settles on either path.
func TestITLSettle_AJoinerSettlesOnEitherPath(t *testing.T) {
	joinAt := settleCycles + 2
	rows := func(i int) []domain.ReplicaMetrics {
		f := settleFleet()
		if i >= joinAt {
			f = append(f, lineRow("pod-join", 0.50, lineAt(0.50), lineStart.Add(time.Duration(joinAt)*settleCycle)))
		}
		return f
	}
	want := append(cycleSpan(0, settleCycles), cycleSpan(joinAt, settleCycles)...)
	n := joinAt + settleCycles + 3
	assert.Equal(t, want, settlingCycles(newSettleRig(t, nil), n, rows))
	// The key's own trace does not wait after the restart
	// (SwitchITLWaitsAfterRestart reads the tracker only).
	assert.Equal(t, cycleSpan(joinAt, settleCycles), settlingCycles(newSettleRig(t, map[string]bool{SwitchITLReadsSettle: false}), n, rows))
}

// SwitchITLWaitsAfterRestart reads the engine's tracker only: with the line
// on its own trace (SwitchITLReadsSettle off) and an engine view, a
// controller restart does not wait, as a key the analyzer made anew must
// not. Before, the own trace waited steadyFor on every key's first cycle.
func TestITLSettle_TheOwnTraceDoesNotWaitAfterARestart(t *testing.T) {
	assert.Empty(t, settlingCycles(newSettleRig(t, map[string]bool{SwitchITLReadsSettle: false}), 2*settleCycles, steadySettleFleet))
	assert.Equal(t, cycleSpan(0, settleCycles), settlingCycles(newSettleRig(t, nil), 2*settleCycles, steadySettleFleet),
		"premise: on the tracker the restart waits")
}

// settleIdle makes rm idle on evidence: a known KV of 0 and a known empty
// ITL window.
func settleIdle(rm *domain.ReplicaMetrics) {
	rm.KvUsageInstant, rm.KvCacheUsage, rm.KvCacheUsageKnown = 0, 0, true
	rm.AvgITL, rm.AvgITLKnown, rm.AvgITLEmpty = 0, false, true
}

// Load back after idleRows rows idle on evidence: the replica's settle
// starts again.
func TestITLSettle_LoadBackAfterAnIdleRunStartsTheSettle(t *testing.T) {
	idleFrom := settleCycles + 2
	rows := func(i int) []domain.ReplicaMetrics {
		f := settleFleet()
		if i == idleFrom || i == idleFrom+1 {
			settleIdle(&f[2])
		}
		return f
	}
	n := idleFrom + 2 + settleCycles + 3
	assert.Equal(t, append(cycleSpan(0, settleCycles), cycleSpan(idleFrom+2, settleCycles)...),
		settlingCycles(newSettleRig(t, nil), n, rows))
}

// A scrape that read nothing is not idleness. Off (SwitchITLIdleOnEvidence),
// as on the line's own trace, two rows with neither an ITL nor a k idle the
// replica, and it waits steadyFor when load comes back.
func TestITLSettle_AScrapeThatReadNothingIsNotIdle(t *testing.T) {
	failFrom := settleCycles + 2
	rows := func(i int) []domain.ReplicaMetrics {
		f := settleFleet()
		if i == failFrom || i == failFrom+1 {
			f[2] = domain.ReplicaMetrics{PodName: "pod-c", VariantName: "v1", Ready: true}
		}
		return f
	}
	n := failFrom + 2 + settleCycles + 3
	assert.Equal(t, cycleSpan(0, settleCycles), settlingCycles(newSettleRig(t, nil), n, rows))
	assert.Equal(t, append(cycleSpan(0, settleCycles), cycleSpan(failFrom+2, settleCycles)...),
		settlingCycles(newSettleRig(t, map[string]bool{SwitchITLIdleOnEvidence: false}), n, rows),
		"switch off: two rows with neither an ITL nor a k idled the replica")
}

// A Pod with an Absent rank is no reading. Off, its Finite ranks' mean was
// one.
func TestITLSettle_APodWithAnAbsentRankIsNoReading(t *testing.T) {
	rows := settleFleet()
	rows[2].AvgITLKnown = false
	on := newSettleRig(t, nil).cycle(rows)
	assert.Equal(t, 2, on.Offered)
	assert.Equal(t, 1, on.NoITL)
	off := newSettleRig(t, map[string]bool{SwitchITLReadsFinitePodMean: false}).cycle(rows)
	assert.Equal(t, 3, off.Offered, "switch off: the Absent pod's mean is a reading")
	assert.Zero(t, off.NoITL)
}

// The settle switches only reach the line on the tracker: with
// SwitchITLSteadyGate off the parent's window reads none of them.
func TestITLSettle_TheParentsWindowReadsNoSettle(t *testing.T) {
	a := newParentLineAnalyzer().WithSettleView(settle.NewTracker())
	adm := a.lineAdmission(a.admission(itlPolicy{}, "ns", "m"), "ns", "m", variantEngine{})
	assert.Nil(t, adm.Settle)
	assert.False(t, adm.WaitsAfterRestart || adm.KeepsThroughMissingRow || adm.FiniteOnly ||
		adm.RestartUnderName || adm.IdleOnEvidence)
}

// At an optimize interval of 48 s or more the own tracker's tolerance covers
// the backdate, so before issue #511 both observations stood: a first
// sighting's trace held an invented row at the backdate as well as the row
// read. With SwitchITLOwnSettleBackdates on it holds only the row read; off,
// as before, both. The settle begins at the backdate either way.
func TestITLSettle_TheOwnTrackerInventsNoRowAtTheBackdate(t *testing.T) {
	const interval = time.Minute
	require.GreaterOrEqual(t, settle.Tolerance(interval), ownSettleBackdate, "premise: the tolerance covers the backdate")
	for _, on := range []bool{true, false} {
		c := clocktesting.NewFakePassiveClock(lineStart)
		r := &ownRig{t: t, a: NewThroughputAnalyzer().WithClock(c).WithOptimizationInterval(interval), clock: c, now: lineStart}
		require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchITLOwnSettleBackdates: on}))
		r.analyze("m", settleFleet())
		want := 1
		if !on {
			want = 2
		}
		for _, rm := range settleFleet() {
			key := settlerow.Key(rm)
			got, ok := r.a.ownSettle.Replica("ns/m", key)
			require.True(t, ok, "switch %v: %s is not held", on, rm.PodName)
			assert.Equal(t, settle.Joined, got.Reason, "switch %v: %s", on, rm.PodName)
			assert.Equal(t, lineStart.Add(-ownSettleBackdate), got.Since, "switch %v: %s", on, rm.PodName)
			trace := r.a.ownSettle.Trace("ns/m", key)
			assert.Len(t, trace, want, "switch %v: %s's trace", on, rm.PodName)
			assert.Equal(t, lineStart, trace[len(trace)-1].At, "switch %v: %s's last row", on, rm.PodName)
		}
	}
}
