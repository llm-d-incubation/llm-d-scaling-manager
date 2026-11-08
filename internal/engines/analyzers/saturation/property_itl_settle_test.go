package saturation

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// Property cases for the ITL line on the settle tracker (learner PR 4a,
// analyzer-learner.md 4.3 and 4.4), for the saturation analyzer's line
// (itl.Tracker as the engine drives it, newTrackerITL) and the slo
// analyzer's (slo.Analyzer, newSLOITL). Each event that starts a settle
// carries a warm-up -- readings 5x the line for 90 s, as a replica just
// started reads -- and the line, against the same run without the event,
// moves by at most propTransientTol. Each event that does not start one
// leaves no reading settling.

// settleCases is the cases for the line named learner, from newL.
func settleCases(learner string, newL func() itlLearner) []propCase {
	return []propCase{
		{class: classTransient, learner: learner, name: "controller-restart-warmup", family: familyDirect, tuning: tunedDefault,
			check: checkITLRestartWarmup(newL)},
		{class: classTransient, learner: learner, name: "restart-same-name", family: familyDirect, tuning: tunedDefault,
			check: checkITLSettleEvent(newL, settleRestart)},
		{class: classTransient, learner: learner, name: "idle-rows-positive-evidence", family: familyDirect, tuning: tunedDefault,
			check: checkITLSettleEvent(newL, settleIdle)},
		{class: classMissing, learner: learner, name: "absence-past-tolerance-restarts", family: familyDirect, tuning: tunedDefault,
			check: checkITLSettleEvent(newL, settleAbsence)},
		{class: classMissing, learner: learner, name: "missed-row-keeps-settle", family: familyDirect, tuning: tunedDefault,
			check: checkITLNoSettle(newL, noSettleMissedRow)},
		{class: classMissing, learner: learner, name: "failed-itl-query-not-idle", family: familyDirect, tuning: tunedDefault,
			check: checkITLNoSettle(newL, noSettleFailedQuery)},
	}
}

// itlSettleCases is the saturation analyzer's line's cases, the slo
// analyzer's, and the tracker's own.
func itlSettleCases() []propCase {
	def := func() itlLearner { return newTrackerITL(steadinessOf(tuningDefault)) }
	sloLine := func() itlLearner { return newSLOITL(nil) }
	return append(append(settleCases("itl", def), settleCases("slo-itl", sloLine)...),
		propCase{class: classTransient, learner: "settle", name: "lent-again", family: familyDirect, tuning: tunedDefault,
			check: checkSettleLentAgain(nil)})
}

const (
	// settleWarmup is how many cycles a replica's readings carry its
	// warm-up after a settle starts: 90 s.
	settleWarmup = 6
	// settleRuns is each run's length: 20 min.
	settleRuns = 80
	// settleAt is the cycle the event comes at, past the controller
	// start's own settle.
	settleAt = 20
)

// readyFleet is steadyFleet with every replica Ready an hour before the run.
func readyFleet(base func(i int) []domain.ReplicaMetrics) func(i int) []domain.ReplicaMetrics {
	return func(i int) []domain.ReplicaMetrics {
		rows := base(i)
		for j := range rows {
			rows[j].ReadySince = propStart.Add(-time.Hour)
		}
		return rows
	}
}

// warmup multiplies row's ITL by 5, as a replica in its first 90 s reads.
func warmup(rm *domain.ReplicaMetrics) { rm.AvgITL *= 5 }

// settleEvent edits cycle i's rows of the run in which r2 meets the event.
type settleEvent func(i int, rows []domain.ReplicaMetrics) []domain.ReplicaMetrics

// settleRestart: r2's engine restarts under its own Pod name at settleAt (a
// new Ready time), with no row that is not Ready in between, and warms up.
func settleRestart(i int, rows []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	if i >= settleAt {
		rows[2].ReadySince = propStart.Add(time.Duration(settleAt)*propCycle - 5*time.Second)
	}
	if i >= settleAt && i < settleAt+settleWarmup {
		warmup(&rows[2])
	}
	return rows
}

// settleIdle: r2 is idle on positive evidence (a known KV of 0 and a known
// empty ITL window) for steadyGate.idleRows rows, then load comes back to
// it, and it warms up.
func settleIdle(i int, rows []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	idle := steadinessOf(tuningDefault).IdleRows
	switch {
	case i >= settleAt && i < settleAt+idle:
		rows[2] = idleRow()
		rows[2].PodName = "r2"
	case i >= settleAt+idle && i < settleAt+idle+settleWarmup:
		warmup(&rows[2])
	}
	return rows
}

// settleAbsence: r2 has no row for 75 s, past the tracker's 1 min
// tolerance, and comes back warming up.
func settleAbsence(i int, rows []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	const gone = 5
	switch {
	case i >= settleAt && i < settleAt+gone:
		return rows[:2]
	case i >= settleAt+gone && i < settleAt+gone+settleWarmup:
		warmup(&rows[2])
	}
	return rows
}

// checkITLSettleEvent: the event starts r2's settle, so its warm-up stays
// out of the line: from the event on, the line moves by at most
// propTransientTol against the run without it.
func checkITLSettleEvent(newL func() itlLearner, event settleEvent) func(uint64) error {
	return func(seed uint64) error {
		base := readyFleet(steadyFleet(propRand(seed), 3, settleRuns))
		control := itlRun(newL(), settleRuns, base)
		got := itlRun(newL(), settleRuns, func(i int) []domain.ReplicaMetrics { return event(i, base(i)) })
		return compareFrom(control, got, settleAt, "the event")
	}
}

// compareFrom holds got to control from cycle from on.
func compareFrom(control, got []itl.Reading, from int, what string) error {
	compared := 0
	for i := from; i < len(control); i++ {
		c, g := itlAt(control[i]), itlAt(got[i])
		if math.IsNaN(c) || math.IsNaN(g) {
			continue
		}
		compared++
		if e := relErr(g, c); e > propTransientTol {
			return violation(kindTransient, "cycle %d (%s after %s): ITL(%.2f) %.5f against %.5f without it, %.1f%% apart",
				i, time.Duration(i-from)*propCycle, what, propKPrice, g, c, 100*e)
		}
	}
	return holds(compared > 0, kindNotLearned, "no cycle after %s had a line in both runs", what)
}

// checkITLRestartWarmup: the controller restarts as the fleet starts
// (replicas Ready before the controller's first cycle, so nothing tells
// them apart from an established fleet), and every replica's first 90 s
// carry its warm-up. The line waits the settle and does not learn the
// warm-up: against the same restart with no warm-up, it moves by at most
// propTransientTol. Before learner PR 4a the line took the first cycle's
// replicas as settled and fitted the warm-up (analyzer-learner.md 4.4).
func checkITLRestartWarmup(newL func() itlLearner) func(uint64) error {
	return func(seed uint64) error {
		base := readyFleet(steadyFleet(propRand(seed), 3, settleRuns))
		control := itlRun(newL(), settleRuns, base)
		got := itlRun(newL(), settleRuns, func(i int) []domain.ReplicaMetrics {
			rows := base(i)
			if i < settleWarmup {
				for j := range rows {
					warmup(&rows[j])
				}
			}
			return rows
		})
		return compareFrom(control, got, 0, "the restart")
	}
}

// checkSettleLentAgain: r2 is a warm-pool Pod, handed back and lent again to
// the same variant between two cycles, so its key does not change and it is
// never absent. Only the new lend time says so, and the tracker starts a
// settle at it. No learner reads the event yet (a bridge is no reading of the
// ITL line or of k2); the balance tracker does, so the case holds the tracker.
func checkSettleLentAgain(wrap func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		fleet := readyFleet(steadyFleet(r, 3, settleRuns))
		lentAt := propStart.Add(time.Duration(settleAt+r.IntN(20)) * propCycle)
		st := settle.NewTracker()
		st.SetInterval(propCycle)
		var key settle.Key
		for i := range settleRuns {
			now := propStart.Add(time.Duration(i) * propCycle)
			rows := fleet(i)
			rows[2].FromWarmPool, rows[2].LentSince = true, propStart.Add(-time.Hour)
			if !now.Before(lentAt) {
				rows[2].LentSince = lentAt
			}
			if wrap != nil {
				wrap(&rows[2])
			}
			key = settlerow.Key(rows[2])
			st.Observe(propSettleModel, settlerow.Of(rows, nil), now)
		}
		rep, ok := st.Replica(propSettleModel, key)
		if !ok {
			return violation(kindNotLearned, "the tracker does not know r2")
		}
		return holds(rep.Reason == settle.Lent && rep.Since.Equal(lentAt), kindTransient,
			"lent again at %s, and its settle is %s at %s", lentAt.Format(time.TimeOnly), rep.Reason, rep.Since.Format(time.TimeOnly))
	}
}

// noSettle edits cycle i's rows of a run in which r2 meets something that
// is not a settle.
type noSettle func(i int, rows []domain.ReplicaMetrics) []domain.ReplicaMetrics

// noSettleMissedRow: r2's row is missing on one cycle: a missed scrape.
func noSettleMissedRow(i int, rows []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	if i == settleAt {
		return rows[:2]
	}
	return rows
}

// noSettleFailedQuery: r2's ITL query fails on one cycle (its KV read), and
// then a scrape reads nothing at all for two (every gauge unknown, the Pod
// listed and Ready): none of it is idleness, and the next good row is not
// load coming back.
func noSettleFailedQuery(i int, rows []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	switch i {
	case settleAt:
		rows[2].AvgITL, rows[2].AvgITLKnown = 0, false
	case settleAt + 1, settleAt + 2:
		rows[2] = domain.ReplicaMetrics{PodName: "r2", VariantName: "v", Ready: true, ReadySince: rows[2].ReadySince}
	}
	return rows
}

// checkITLNoSettle: what the run meets starts no settle: from it on, no
// reading is kept out as its replica's settle.
func checkITLNoSettle(newL func() itlLearner, edit noSettle) func(uint64) error {
	return func(seed uint64) error {
		base := readyFleet(steadyFleet(propRand(seed), 3, settleRuns))
		got := itlRun(newL(), settleRuns, func(i int) []domain.ReplicaMetrics { return edit(i, base(i)) })
		if got[settleAt-1].Settling != 0 {
			return violation(kindNotLearned, "premise: cycle %d is past the controller start's settle, and %d readings settle",
				settleAt-1, got[settleAt-1].Settling)
		}
		for i := settleAt; i < settleRuns; i++ {
			if got[i].Settling > 0 {
				return violation(kindMoved, "cycle %d (%s after it): %d readings settling", i,
					time.Duration(i-settleAt)*propCycle, got[i].Settling)
			}
		}
		return nil
	}
}

// The cases' negative control: the same checks against the line before
// learner PR 4a -- the key's own trace, every settle switch off -- in both
// analyzers. The four cases of a 4a change fail it on every seed run; the
// two whose event the line's own trace already started a settle on hold on
// both, and say so.
func TestITLSettleCases_TheLineBefore4aFailsTheChanges(t *testing.T) {
	itlBefore := func() itlLearner {
		st := settle.NewTracker()
		st.SetInterval(propCycle)
		return &trackerITL{tr: itl.NewTracker(), settle: st, adm: itl.Admission{Steady: steadinessOf(tuningDefault)}}
	}
	sloBefore := func() itlLearner {
		l := newSLOITL(nil)
		require.NoError(t, l.a.SetSwitchesForReplay(map[string]bool{
			slo.SwitchITLReadsSettle: false, slo.SwitchITLWaitsAfterRestart: false,
			slo.SwitchITLKeepsSettleThroughMissingRow: false, slo.SwitchITLReadsFinitePodMean: false,
			slo.SwitchITLRestartUnderName: false, slo.SwitchITLIdleOnEvidence: false,
		}))
		return l
	}
	// fails is each case's verdict on the line before 4a: true where 4a
	// changed what the line does.
	fails := map[string]bool{
		"controller-restart-warmup":       true,
		"restart-same-name":               true,
		"missed-row-keeps-settle":         true,
		"failed-itl-query-not-idle":       true,
		"idle-rows-positive-evidence":     false,
		"absence-past-tolerance-restarts": false,
	}
	for _, before := range []struct {
		learner string
		newL    func() itlLearner
	}{{"itl", itlBefore}, {"slo-itl", sloBefore}} {
		for _, c := range settleCases(before.learner, before.newL) {
			want, ok := fails[c.name]
			require.True(t, ok, "%s: say whether the line before 4a fails it", c.name)
			for seed := uint64(1); seed <= 5; seed++ {
				err := c.check(seed)
				if want {
					var v *propViolation
					require.ErrorAs(t, err, &v, "%s/%s seed %d: the line before 4a passes a case of a 4a change", before.learner, c.name, seed)
					if seed == 1 {
						t.Logf("%s/%s on the line before 4a: %v", before.learner, c.name, err)
					}
				} else {
					require.NoError(t, err, "%s/%s seed %d: the line before 4a also starts a settle here", before.learner, c.name, seed)
				}
			}
		}
	}
}
