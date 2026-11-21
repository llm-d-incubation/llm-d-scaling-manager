package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/internal/learnertest"
)

// The closed-loop probe's hand-written cases that are not start probes
// (closed_loop_probe_test.go), at the measured starts
// (learnertest.MeasuredStarts: 15 s, 90 s, 463 s). Every row reports the
// true start as its measured one, as the start probes' rows do, so a 90 s
// run here is not the hand-written case's run, whose rows report none and
// price the default start.
//
// The two restored-first-sight cases are not run here. Measured at every
// start, the fleet with the first sights restored and the fleet that waits
// do not differ: on every cycle through the load step, and in the lowest
// fleet, the final fleet and the time under need through the steady
// restart. Their comparisons hold whatever changes; their own comments say
// they hold the outcome, not the bound.
//
// The decode-step cases run on the shortest-queue router at every start and
// are judged at verdictHorizon, as the start probes are: at 463 s the decode
// step's backlog clears at 44m9s, past its 40 min horizon. At 463 s on this
// router the decode step ends over the bound with or without a restart (23
// replicas for a need of 17, bound 20.5: the
// liveness/floor/closed-loop-*-start-463s-jsq ledger rows,
// ev-shindin/llm-scaling-manager#520), so there a restart case is held to its
// no-restart control rather than to the verdict.

// atStart is pc with its replicas starting in start, judged at
// verdictHorizon for a load step at step (zero for none).
func atStart(pc probeCase, step, start time.Duration) probeCase {
	pc.start = start
	pc.horizon = verdictHorizon(pc, step, start)
	return pc
}

// decodeStepAt is a restartStepCase at start (atStart).
func decodeStepAt(pc probeCase, start time.Duration) probeCase {
	return atStart(pc, restartStepAt, start)
}

// settlesUnlessCold requires r to end within tolerance of need, below
// learnertest.ColdStart; at it, r may end over the bound no further than
// control does.
func settlesUnlessCold(t *testing.T, start time.Duration, r, control probeResult) {
	t.Helper()
	if start < learnertest.ColdStart {
		require.Empty(t, r.verdict())
		return
	}
	require.LessOrEqual(t, r.final, control.final, "at the cold start: ended over the run without the restart")
	require.LessOrEqual(t, r.peak, control.peak, "at the cold start: peaked over the run without the restart")
}

// TestClosedLoopProbeAtStarts_ASteeringRouterKeepsARightMinorityFigure runs
// TestClosedLoopProbe_ASteeringRouterKeepsARightMinorityFigure at each start.
//
// Measured: peak 23, 29 and 26 at 15 s, 90 s and 463 s for a need of 17,
// final 18, 19 and 18; never under need.
func TestClosedLoopProbeAtStarts_ASteeringRouterKeepsARightMinorityFigure(t *testing.T) {
	learnertest.AtMeasuredStarts(t, func(t *testing.T, start time.Duration) {
		r := runProbe(t, atStart(steeredCase(), 0, start))
		t.Logf("need %d, final %d, peak %d, longest under need %s; fleet %s", r.need, r.final, r.peak, r.underFor, traceSummary(r.trace))
		require.Empty(t, r.verdict())
		require.Zero(t, r.underFor, "the fleet never sat under need")
		require.Positive(t, r.muCounts[muReasonSampled], "premise: the steered replicas were read")
	})
}

// TestClosedLoopProbeAtStarts_AControllerRestartBeforeALoadStepSettlesAtNeed
// runs TestClosedLoopProbe_AControllerRestartBeforeALoadStepSettlesAtNeed,
// with the queue priced by drain time (the default), at each start.
//
// The difference, named: at 15 s and 90 s the restart costs the step no time
// under need, and at 463 s it costs one cycle (15 s under need, against none
// without the restart; final 23 and peak 24, as without it). The proposal's
// start-probe table has the same 15 s for restart-before-step at 463 s.
//
// Negative control, in the test: with the queue not priced by drain time
// (SwitchQueuePricedAsDrain off) the restart leaves the step under need for
// 1m15s, 3m30s and 8m0s at 15 s, 90 s and 463 s, against none without it.
func TestClosedLoopProbeAtStarts_AControllerRestartBeforeALoadStepSettlesAtNeed(t *testing.T) {
	learnertest.AtMeasuredStarts(t, func(t *testing.T, start time.Duration) {
		allowance := time.Duration(0)
		if start >= learnertest.ColdStart {
			allowance = propCycle
		}
		for _, on := range []bool{true, false} {
			sw := map[string]bool{SwitchQueuePricedAsDrain: on}
			ctl := decodeStepAt(restartStepCase(0, false, 0), start)
			ctl.switches = sw
			control := runProbe(t, ctl)
			pc := decodeStepAt(restartStepCase(15*time.Minute, false, 0), start)
			pc.switches = sw
			r := runProbe(t, pc)
			t.Logf("queue priced as drain %v: need %d, final %d, peak %d; under need for %s, against %s without the restart (final %d, peak %d)",
				on, r.need, r.final, r.peak, r.underFor, control.underFor, control.final, control.peak)
			if !on {
				require.Greater(t, r.underFor, control.underFor+allowance, "negative control: off, the restart costs the step its floor")
				if start < learnertest.ColdStart {
					require.Empty(t, r.verdict(), "off: inside the bound a start and a settle take")
				}
				continue
			}
			if start < learnertest.ColdStart {
				require.Empty(t, control.verdict(), "premise: without a restart the step settles at need")
			}
			settlesUnlessCold(t, start, r, control)
			require.LessOrEqual(t, r.underFor, control.underFor+allowance, "the first read prices the step the restart left without a mu")
		}
	})
}

// TestClosedLoopProbeAtStarts_AShortStabilizationThroughARestartSettlesAtNeed
// runs TestClosedLoopProbe_AShortStabilizationThroughARestartSettlesAtNeed at
// each start. At 463 s both windows end at 23 replicas (the ledger rows);
// the short window ends no higher than the default one.
func TestClosedLoopProbeAtStarts_AShortStabilizationThroughARestartSettlesAtNeed(t *testing.T) {
	learnertest.AtMeasuredStarts(t, func(t *testing.T, start time.Duration) {
		long := runProbe(t, decodeStepAt(restartStepCase(15*time.Minute, false, 0), start))
		short := runProbe(t, decodeStepAt(restartStepCase(15*time.Minute, false, 2*time.Minute), start))
		t.Logf("need %d, final %d, peak %d; under need for %s at 120 s, %s at 300 s; final %d at 300 s", short.need, short.final, short.peak,
			short.underFor, long.underFor, long.final)
		settlesUnlessCold(t, start, short, long)
		require.Equal(t, long.underFor, short.underFor)
	})
}

// TestClosedLoopProbeAtStarts_WithoutTheUnreadCreditASlowReplicaTakesTheFleetOverTheBound
// runs TestClosedLoopProbe_ASlowReplicaInsideTheServedBoundStaysUnderTheBound's
// negative control at each start: with the unread replicas not credited
// (SwitchMuUnreadAtTarget off) the slow period takes the fleet over the
// bound at every start.
//
// The difference, named: at 15 s and 90 s the low figure then seals the
// fleet (final 23 against a bound of 21.7), and at 463 s it does not
// (final 20): the order that lands 463 s later meets a replica that has
// recovered, and the figure is rewritten before the fleet settles. The
// assertions with the credit on are the start probes'
// (liveness/mu/closed-loop-slow-pod-055-start-*).
func TestClosedLoopProbeAtStarts_WithoutTheUnreadCreditASlowReplicaTakesTheFleetOverTheBound(t *testing.T) {
	learnertest.AtMeasuredStarts(t, func(t *testing.T, start time.Duration) {
		noCredit := slowReplicaCase(0.55)
		noCredit.switches = map[string]bool{SwitchMuUnreadAtTarget: false}
		c := runProbe(t, atStart(noCredit, 0, start))
		bound := overBound(c.need)
		t.Logf("unread replicas at what they serve: need %d, final %d, peak %d (bound %.1f); %s", c.need, c.final, c.peak, bound, c.verdict())
		require.Greater(t, float64(c.peak), bound, "the credit is what keeps the slow period under the bound")
		if start < learnertest.ColdStart {
			require.Contains(t, c.verdict(), "over-ordered", "the low figure seals the fleet")
			return
		}
		require.Empty(t, c.verdict(), "the named difference: at the cold start the figure is rewritten before the fleet settles")
	})
}

// steeredCase is TestClosedLoopProbe_ASteeringRouterKeepsARightMinorityFigure's
// fleet: eighteen replicas at mu 1 under 12 req/s, four of them sent twice
// the share by a queue-blind router, and a lull at 2 req/s from 10 to 45 min.
func steeredCase() probeCase {
	pods := make([]float64, 18)
	for i := range pods {
		pods[i] = 1
	}
	return probeCase{
		name: "steered", pods: pods, addedMu: 1, meanMu: 1,
		lambda: func(e time.Duration) float64 {
			if e >= 10*time.Minute && e < 45*time.Minute {
				return 2
			}
			return 12
		},
		weight: func(i int, _ time.Duration) float64 {
			if i < 4 {
				return 2
			}
			return 1
		},
		queueBlind: true,
		horizon:    60 * time.Minute,
	}
}
