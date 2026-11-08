package slo

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// Issue #459: the analyzer alone, with no engine tracker, across a gap
// between two of a model's cycles longer than the state timeout. What the
// line reads after it depends only on the model's own last sight, not on
// whether EvictStale ran, or where in the cycle.

// gapEviction is where in each cycle the rig calls EvictStale.
type gapEviction string

const (
	// evictFirst calls it at the top of the cycle, as the engine and the
	// replay do.
	evictFirst gapEviction = "first"
	// evictBetween calls it between the two models' Analyze calls.
	evictBetween gapEviction = "between"
	// evictNever never calls it, as a caller that drives Analyze alone.
	evictNever gapEviction = "never"
)

const (
	gapStateTimeout = time.Hour
	gapLength       = 2 * time.Hour
)

// gapModels are two models with identical fleets, analyzed in this order.
var gapModels = []string{"m1", "m2"}

// runGap drives the analyzer alone, with switches, over gapAt cycles, a
// jump of gapLength, and settleCycles+4 cycles more, calling EvictStale as
// evict says. It returns, by model, the cycles on which a reading was kept
// out as its replica's settle, as Analyze's slo-variant log states it.
func runGap(t *testing.T, switches map[string]bool, evict gapEviction, gapAt int) map[string][]int {
	t.Helper()
	return runGapOf(t, switches, evict, gapAt, gapLength, func(int) []domain.ReplicaMetrics { return settleFleet() })
}

// runGapOf is runGap with the clock jumping gap before cycle gapAt, on top
// of the cycle's own settleCycle, and cycle i's rows from rows.
func runGapOf(t *testing.T, switches map[string]bool, evict gapEviction, gapAt int, gap time.Duration,
	rows func(i int) []domain.ReplicaMetrics) map[string][]int {
	t.Helper()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	a := newGapAnalyzer(t, switches, &now)
	out := map[string][]int{}
	for i := range gapAt + settleCycles + 4 {
		analyze := func(model string) int64 { return analyzeSettling(t, a, model, rows(i)) }
		if i == gapAt {
			now = now.Add(gap)
		}
		if evict == evictFirst {
			a.EvictStale(now)
		}
		for j, model := range gapModels {
			if j == 1 && evict == evictBetween {
				a.EvictStale(now)
			}
			if analyze(model) > 0 {
				out[model] = append(out[model], i)
			}
		}
		now = now.Add(settleCycle)
	}
	return out
}

// newGapAnalyzer is the analyzer alone, with no engine view, a state
// timeout of gapStateTimeout, switches, and its clock at *now.
func newGapAnalyzer(t *testing.T, switches map[string]bool, now *time.Time) *Analyzer {
	t.Helper()
	a := New(nil, nil).WithStateTimeout(gapStateTimeout)
	a.now = func() time.Time { return *now }
	require.NoError(t, a.SetSwitchesForReplay(switches))
	return a
}

// analyzeSettling is one Analyze of model in namespace ns with rows, and
// how many of agg's readings were kept out as their replica's settle, as
// the slo-variant log line states it.
func analyzeSettling(t *testing.T, a *Analyzer, model string, rows []domain.ReplicaMetrics) int64 {
	t.Helper()
	in := domain.AnalyzerInput{Namespace: "ns", ModelID: model, Config: policy(44*time.Millisecond, 0),
		ArrivalRate: 1, VariantStates: []domain.VariantReplicaState{settleAgg}, ReplicaMetrics: rows}
	core, logs := observer.New(zapcore.Level(-logging.VERBOSE))
	_, err := a.Analyze(logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core))), in)
	require.NoError(t, err)
	lines := logs.FilterMessage("slo-variant").FilterField(zap.String("variant", "agg")).All()
	require.Len(t, lines, 1, "premise: Analyze logs the variant's line")
	settling, ok := lines[0].ContextMap()["settling"].(int64)
	require.True(t, ok, "slo-variant carries no settling")
	return settling
}

// The forget after a long gap reaches only the stale model's line keys. A
// model of the same namespace analyzed every cycle, whose name has the
// stale one's as a prefix (m10 against m), is midway through a replica's
// settle (a new Ready time under its name) when m comes back from its gap:
// the settle runs its full steadyFor, and its replica stays on the own
// tracker as it was. Forgetting its line keys' traces would end the settle
// on the next cycle: a trace made anew takes the settle as begun before the
// key's first sight, which does not wait with no engine view. Kills a
// prefix without the trailing separator ("ns|m" matches "ns|m10|") and one
// of the whole namespace ("ns|").
func TestSettle_AfterALongGapTheForgetSparesARecentModel(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	a := newGapAnalyzer(t, nil, &now)
	analyzeSettling(t, a, "m", settleFleet())
	now = now.Add(gapLength)
	restartAt := settleCycles + 2
	staleAt := restartAt + 2
	restartedAt := now.Add(time.Duration(restartAt)*settleCycle - 5*time.Second)
	servingSince := now.Add(-time.Hour)
	rows := func(i int) []domain.ReplicaMetrics {
		f := settleFleet()
		for j := range f {
			f[j].ReadySince = servingSince
		}
		if i >= restartAt {
			f[2].ReadySince = restartedAt
		}
		return f
	}
	key := settle.Key{Pod: "agg-c", Variant: "agg"}
	var settling []int
	for i := range restartAt + settleCycles + 3 {
		if analyzeSettling(t, a, "m10", rows(i)) > 0 {
			settling = append(settling, i)
		}
		if i == staleAt {
			before, held := a.ownSettle.Replica("ns/m10", key)
			require.True(t, held, "premise: the own tracker holds m10's restarted replica")
			assert.Zero(t, analyzeSettling(t, a, "m", settleFleet()), "premise: m, back from its gap, is first-seen")
			after, held := a.ownSettle.Replica("ns/m10", key)
			require.True(t, held, "m's gap forgot m10's replica on the own tracker")
			assert.Equal(t, before.Since, after.Since, "m's gap moved m10's settle on the own tracker")
		}
		now = now.Add(settleCycle)
	}
	assert.Equal(t, span(restartAt, settleCycles), settling, "m's gap ended m10's settle: its line keys' traces were forgotten")
}

// After a gap past the state timeout every model is first-seen, wherever
// EvictStale ran and whether it ran at all: nothing settles, and the two
// models read alike. Before the fix only a model EvictStale had evicted was
// first-seen; one it had not read the gap as an absence (settle.Rejoined)
// and settled for steadyFor.
func TestSettle_AfterALongGapTheAnalyzerAloneReadsEveryModelAlike(t *testing.T) {
	gapAt := settleCycles + 2
	for _, evict := range []gapEviction{evictFirst, evictBetween, evictNever} {
		t.Run(string(evict), func(t *testing.T) {
			settling := runGap(t, nil, evict, gapAt)
			t.Logf("settling cycles: %v", settling)
			assert.Empty(t, settling["m1"], "the model analyzed first settled after the gap")
			assert.Empty(t, settling["m2"], "the model analyzed second settled after the gap")
		})
	}
}

// With SwitchITLLongGapFirstSeen off, as before: a model EvictStale did not
// evict reads the gap as an absence and settles for steadyFor, so the
// outcome follows where EvictStale ran.
func TestSettle_AfterALongGapWithTheSwitchOffEvictionOrderStillMatters(t *testing.T) {
	gapAt := settleCycles + 2
	off := map[string]bool{SwitchITLLongGapFirstSeen: false}
	settled := span(gapAt, settleCycles)
	first := runGap(t, off, evictFirst, gapAt)
	assert.Empty(t, first["m1"], "evicted first: m1 settled")
	assert.Empty(t, first["m2"], "evicted first: m2 settled")
	between := runGap(t, off, evictBetween, gapAt)
	assert.Equal(t, settled, between["m1"], "evicted between: m1, analyzed before the eviction, did not settle")
	assert.Empty(t, between["m2"], "evicted between: m2, evicted before its Analyze, settled")
	never := runGap(t, off, evictNever, gapAt)
	assert.Equal(t, settled, never["m1"], "never evicted: m1 did not settle")
	assert.Equal(t, settled, never["m2"], "never evicted: m2 did not settle")
}

// A gap is past the state timeout only when longer than it, as EvictStale
// reads one: a gap of exactly the timeout is read as before, an absence
// past the tracker's tolerance (settle.Rejoined), and the replicas settle;
// a nanosecond more and the model is first-seen. The gap between the two
// Analyze calls is the jump and the cycle's own settleCycle. Never
// evicted, so only observeOwnSettle judges the gap.
func TestSettle_AGapOfExactlyTheStateTimeoutIsNoLongGap(t *testing.T) {
	gapAt := settleCycles + 2
	fleet := func(int) []domain.ReplicaMetrics { return settleFleet() }
	exact := runGapOf(t, nil, evictNever, gapAt, gapStateTimeout-settleCycle, fleet)
	past := runGapOf(t, nil, evictNever, gapAt, gapStateTimeout-settleCycle+time.Nanosecond, fleet)
	assert.Equal(t, span(gapAt, settleCycles), exact["m1"], "a gap of exactly the timeout was read as a long gap")
	assert.Empty(t, past["m1"], "a gap a nanosecond past the timeout was not first-seen")
}

// With the move off (SwitchITLReadsSettle) the line reads its own trace of
// each replica, and the long-gap switch does nothing: the trace is kept
// across the gap. A replica idle on the two cycles before the gap, whose
// load comes back after it, waits steadyFor (the own trace's idle rule)
// with the long-gap switch on or off. Forgetting the traces on the long gap
// would lose the idle run, and nothing would settle.
func TestSettle_AfterALongGapWithTheMoveOffTheOwnTraceIsKept(t *testing.T) {
	gapAt := settleCycles + 2
	rows := func(i int) []domain.ReplicaMetrics {
		f := settleFleet()
		if i == gapAt-2 || i == gapAt-1 {
			settleIdle(&f[2])
		}
		return f
	}
	moveOff := runGapOf(t, map[string]bool{SwitchITLReadsSettle: false}, evictNever, gapAt, gapLength, rows)
	bothOff := runGapOf(t, map[string]bool{SwitchITLReadsSettle: false, SwitchITLLongGapFirstSeen: false},
		evictNever, gapAt, gapLength, rows)
	t.Logf("settling cycles, move off: %v; both off: %v", moveOff, bothOff)
	assert.Equal(t, span(gapAt, settleCycles), bothOff["m1"], "premise: the idle run survives the gap on the own trace")
	assert.Equal(t, bothOff, moveOff, "with the move off, the long-gap switch changed the line")
}

// After a long gap the own tracker holds of the model what it holds when
// EvictStale forgot the model first, replica by replica, and none that left
// during the gap. Two timeouts: the default's hour, and one shorter than the
// tracker's own forget horizon (settle.TraceHorizon), under which a gap
// past the timeout is not long enough for the tracker to drop a replica that
// left by itself. Holds the forget (without it, under the short timeout, a
// replica that left stays held) and the backdate: either way each replica
// reads settle.Joined ownSettleBackdate before the cycle, as on a first
// sighting (issue #511: two observations that far apart read the gap as an
// absence, settle.Rejoined at the cycle).
func TestSettle_AfterALongGapTheOwnTrackerHoldsWhatAnEvictionLeaves(t *testing.T) {
	model := utils.GetNamespacedKey("ns", "m")
	gone := settle.Key{Pod: "agg-c", Variant: "agg"}
	run := func(timeout, gap time.Duration, evict bool) map[string]settle.Replica {
		now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
		a := newGapAnalyzer(t, nil, &now).WithStateTimeout(timeout)
		for range 3 {
			analyzeSettling(t, a, "m", settleFleet())
			now = now.Add(settleCycle)
		}
		now = now.Add(gap)
		if evict {
			a.EvictStale(now)
		}
		analyzeSettling(t, a, "m", settleFleet()[:2])
		_, held := a.ownSettle.Replica(model, gone)
		assert.False(t, held, "timeout %v, evicted first %v: a replica that left during the gap is still held", timeout, evict)
		out := map[string]settle.Replica{}
		for _, pod := range []string{"agg-a", "agg-b"} {
			r, ok := a.ownSettle.Replica(model, settle.Key{Pod: pod, Variant: "agg"})
			require.True(t, ok, "timeout %v, evicted first %v: %s is not held", timeout, evict, pod)
			assertBackdatedJoin(t, r, now)
			out[pod] = r
		}
		return out
	}
	for _, c := range []struct{ timeout, gap time.Duration }{
		{gapStateTimeout, gapLength},
		{2 * time.Minute, 3 * time.Minute},
	} {
		if c.timeout < settle.TraceHorizon {
			require.Less(t, c.gap, settle.TraceHorizon, "premise: the tracker alone keeps a replica that left")
		}
		assert.Equal(t, run(c.timeout, c.gap, true), run(c.timeout, c.gap, false),
			"timeout %v: the stale model's own tracker differs from an evicted one's", c.timeout)
	}
}

// assertBackdatedJoin asserts the own tracker holds r as a first sighting
// at the cycle at: settle.Joined, seen since the line's settle earlier
// (itl.DefaultSteadyFor), and last seen at the cycle.
func assertBackdatedJoin(t *testing.T, r settle.Replica, at time.Time) {
	t.Helper()
	since := at.Add(-itl.DefaultSteadyFor)
	assert.Equal(t, settle.Joined, r.Reason, "%s: the first sighting's settle", r.Pod)
	assert.Equal(t, since, r.Since, "%s: the first sighting's settle began at the cycle, not backdated", r.Pod)
	assert.Equal(t, since, r.FirstSeen, "%s: first seen", r.Pod)
	assert.Equal(t, at, r.LastSeen, "%s: last seen", r.Pod)
}

// Issue #511: the analyzer alone takes a model's replicas on its first
// Analyze as long joined: its own tracker holds each settle.Joined
// itl.DefaultSteadyFor before the cycle. Observed at the backdate and again
// at the cycle, the 2 min gap was past the tracker's absence tolerance
// (settle.Period: the analyzer sets no interval on its own tracker), and
// every replica read settle.Rejoined at the cycle. The line still keeps no
// reading out as a settle on that cycle or after, with or without the
// backdate: its keys' first sight is that cycle too.
func TestSettle_TheOwnTrackerBackdatesAFirstSighting(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	a := newGapAnalyzer(t, nil, &now)
	model := utils.GetNamespacedKey("ns", "m")
	at := now
	var settling []int
	for i := range settleCycles + 2 {
		if analyzeSettling(t, a, "m", settleFleet()) > 0 {
			settling = append(settling, i)
		}
		if i == 0 {
			for _, rm := range settleFleet() {
				r, ok := a.ownSettle.Replica(model, settlerow.Key(rm))
				require.True(t, ok, "%s is not held", rm.PodName)
				assertBackdatedJoin(t, r, at)
			}
		}
		now = now.Add(settleCycle)
	}
	assert.Empty(t, settling, "the line kept a first sighting's readings out as a settle")
	assert.Equal(t, int64(0), a.ownSettle.Verdicts()[settle.Rejoined], "a replica of the first sighting rejoined")

	// Off, as before: two observations, the gap read as an absence.
	now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	off := newGapAnalyzer(t, map[string]bool{SwitchITLOwnSettleBackdates: false}, &now)
	assert.Zero(t, analyzeSettling(t, off, "m", settleFleet()), "switch off: the line kept a first sighting's readings out")
	for _, rm := range settleFleet() {
		r, ok := off.ownSettle.Replica(model, settlerow.Key(rm))
		require.True(t, ok, "switch off: %s is not held", rm.PodName)
		assert.Equal(t, settle.Rejoined, r.Reason, "switch off: %s", rm.PodName)
		assert.Equal(t, now, r.Since, "switch off: %s", rm.PodName)
	}
}

// With the engine's settle view the analyzer keeps no tracker of its own,
// and the long-gap switch does nothing: the engine's tracker reads the gap
// as an absence (settle.Rejoined), and the replicas settle, on or off.
func TestSettle_WithAnEngineViewTheLongGapSwitchDoesNothing(t *testing.T) {
	gapAt := settleCycles + 2
	run := func(switches map[string]bool) []int {
		now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
		tr := settle.NewTracker()
		tr.SetInterval(settleCycle)
		a := newGapAnalyzer(t, switches, &now).WithSettleView(tr)
		var out []int
		for i := range gapAt + settleCycles + 4 {
			if i == gapAt {
				now = now.Add(gapLength)
			}
			tr.Observe(utils.GetNamespacedKey("ns", "m"), settlerow.Of(settleFleet(), map[string]string{"agg": domain.RoleBoth}), now)
			if analyzeSettling(t, a, "m", settleFleet()) > 0 {
				out = append(out, i)
			}
			now = now.Add(settleCycle)
		}
		return out
	}
	on := run(nil)
	t.Logf("settling cycles: %v", on)
	require.Contains(t, on, gapAt, "premise: the engine's tracker settles the replicas after the gap")
	assert.Equal(t, on, run(map[string]bool{SwitchITLLongGapFirstSeen: false}), "with an engine view, the switch changed the line")
}
