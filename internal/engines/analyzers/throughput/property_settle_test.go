package throughput

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/internal/learnertest"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// Property cases for the line on the engine's settle tracker (settle.go,
// learner PR 4b-2): the analyzer as the engine runs it, with a tracker the
// rig observes before each cycle, new with each controller. Registered in
// throughputCases with the line's other cases; the mutant of the restart
// case is SwitchITLWaitsAfterRestart off (TestThroughputPropertySettleMutants).

// engineCycles is how many cycles each engine-rig case runs.
const engineCycles = 32

// engineLineOf is lineOf on the engine's rig, from a controller start at
// cycle from to engineCycles: a new tracker, observed before every cycle.
func engineLineOf(a *ThroughputAnalyzer, from int, rows func(c int) []domain.ReplicaMetrics) (map[int]itl.Model, error) {
	tr := settle.NewTracker()
	tr.SetInterval(learnertest.Cycle)
	r := newPropRun(a.WithSettleView(tr).WithOptimizationInterval(learnertest.Cycle))
	r.cycle = from
	lines := map[int]itl.Model{}
	for c := from; c < engineCycles; c++ {
		rs := rows(c)
		tr.Observe("ns/m", settlerow.Of(rs, nil), r.now())
		m, has, err := r.step(rs)
		if err != nil {
			return nil, err
		}
		if has {
			lines[c] = m
		}
	}
	return lines, nil
}

// propWarmCycles is how many cycles after it turns Ready a joiner reads the
// warm-up ITL (0.45 s at k 0.46, as on the two-namespace run).
const propWarmCycles = 6

// warmJoiner is the joiner's row on cycle c, Ready from cycle joined: the
// warm-up reading for propWarmCycles cycles, then its noisy reading on the
// true line at k 0.46.
func warmJoiner(c, joined int, noise float64) domain.ReplicaMetrics {
	ready := learnertest.Start.Add(time.Duration(joined) * learnertest.Cycle)
	if c-joined < propWarmCycles {
		return lineRow("pod-join", 0.46, 0.45, ready)
	}
	return lineRow("pod-join", 0.46, propITL(0.46)*(1+noise), ready)
}

// Transient: a controller restarts two cycles after a replica joined, while
// its warm-up lasts. The restarted controller's tracker first sees every
// replica on its first cycle, the joiner as any other. Waiting the settle
// after the restart keeps the joiner's warm-up out of the line; taken as
// settled, as before 4b-2, its warm-up readings enter the line on the
// restart's first fit.
func checkRestartWithAJoiner(newA propAnalyzer) func(uint64) error {
	return func(seed uint64) error {
		const before, after = 16, engineCycles - 16
		restart := before
		joined := restart - 2
		f := newPropFleet(learnertest.Rand(seed), before+after, 0.30, 0.40, 0.50)
		jn := learnertest.ARNoise(learnertest.Rand(seed+1), before+after, propNoise, propRho)
		base, err := engineLineOf(newA(), restart, f.rows)
		if err != nil {
			return err
		}
		with, err := engineLineOf(newA(), restart, func(c int) []domain.ReplicaMetrics {
			return append(f.rows(c), warmJoiner(c, joined, jn[c]))
		})
		if err != nil {
			return err
		}
		return movedFrom(base, with, restart, before+after, "with a joiner's warm-up across the restart")
	}
}

// Transient: a replica joins well after the controller started, on the
// engine's tracker: its warm-up is stamped and kept out of the line.
func checkEngineJoiner(newA propAnalyzer) func(uint64) error {
	return func(seed uint64) error {
		const joined, cycles = 16, engineCycles
		f := newPropFleet(learnertest.Rand(seed), cycles, 0.30, 0.40, 0.50)
		jn := learnertest.ARNoise(learnertest.Rand(seed+1), cycles, propNoise, propRho)
		base, err := engineLineOf(newA(), 0, f.rows)
		if err != nil {
			return err
		}
		with, err := engineLineOf(newA(), 0, func(c int) []domain.ReplicaMetrics {
			rows := f.rows(c)
			if c >= joined {
				rows = append(rows, warmJoiner(c, joined, jn[c]))
			}
			return rows
		})
		if err != nil {
			return err
		}
		return movedFrom(base, with, joined, cycles, "with a joiner's warm-up")
	}
}

// Missing: one replica misses one row on the engine's tracker; the line
// does not move past the transient tolerance, nor go silent.
func checkEngineMissedRow(newA propAnalyzer) func(uint64) error {
	return func(seed uint64) error {
		const missed, cycles = 16, engineCycles
		f := newPropFleet(learnertest.Rand(seed), cycles, 0.30, 0.40, 0.50)
		base, err := engineLineOf(newA(), 0, f.rows)
		if err != nil {
			return err
		}
		with, err := engineLineOf(newA(), 0, func(c int) []domain.ReplicaMetrics {
			if c == missed {
				return f.rows(c)[1:]
			}
			return f.rows(c)
		})
		if err != nil {
			return err
		}
		return movedFrom(base, with, missed, cycles, "with a missed row")
	}
}

// Known truth, the above-band fallback after a controller restart: the
// fleet of checkAboveBandFallback, every replica serving above the band
// since before the restart, on the engine's rig from a restart at cycle 0.
// The band's line waits steadyFor after the restart
// (SwitchITLWaitsAfterRestart); the fallback's window does not (itl's
// aboveBandSettling), so the fallback prices inside that wait, and still
// never under the true line. The first two cycles read 30 % over the line,
// a transient the restart caught: above the line, they can only raise the
// price. Negative control (a mutant run): with the restart wait stamped on
// the fallback's readings too, the fallback first prices on cycle 8, after
// the wait, and the case fails on all 100 seeds.
func checkAboveBandAfterRestart(newA propAnalyzer) func(uint64) error {
	return aboveBandFallbackCheck(newA, engineCycles, 2, settleCycles-1,
		func(a *ThroughputAnalyzer, rows func(int) []domain.ReplicaMetrics) (map[int]itl.Model, error) {
			return engineLineOf(a, 0, rows)
		})
}

// settleCases is the engine-rig cases, on the analyzer newA builds.
func settleCases(newA propAnalyzer) []learnertest.Case {
	c := func(class learnertest.Class, name string, check func(uint64) error) learnertest.Case {
		return learnertest.Case{Class: class, Learner: propLearner, Name: name, Family: learnertest.FamilyDirect,
			Tuning: "default", Seeds: learnertest.CheapSeeds, Check: check}
	}
	return []learnertest.Case{
		c(learnertest.ClassTransient, "controller-restart-with-a-joiner", checkRestartWithAJoiner(newA)),
		c(learnertest.ClassTransient, "joiner-warm-up-on-the-engine-tracker", checkEngineJoiner(newA)),
		c(learnertest.ClassMissing, "missed-row-on-the-engine-tracker", checkEngineMissedRow(newA)),
		c(learnertest.ClassKnownTruth, "above-band-fallback-after-a-restart", checkAboveBandAfterRestart(newA)),
	}
}

// settleMutants is each settle case with a negative control, and the
// switch whose off path reintroduces the defect.
var settleMutants = map[string]string{
	"transient/throughput-itl/controller-restart-with-a-joiner": SwitchITLWaitsAfterRestart,
}

// TestThroughputPropertySettleMutants runs each settle case that has a
// negative control with its switch off: every one must fail there, and its
// first failure is logged as the case's negative control. The other settle
// cases' results with every 4b-2 switch off, the line 4b-1 built, are
// logged too: they hold on either path.
func TestThroughputPropertySettleMutants(t *testing.T) {
	ids := map[string]bool{}
	for _, c := range settleCases(NewThroughputAnalyzer) {
		ids[c.ID()] = true
	}
	for id, sw := range settleMutants {
		if !ids[id] {
			t.Errorf("%s is no registered settle case", id)
			continue
		}
		for _, c := range settleCases(withSwitchOff(sw)) {
			if c.ID() != id {
				continue
			}
			run := runSeeds(t, c)
			if len(run.Failures) == 0 {
				t.Errorf("%s passes with %s off: the case no longer catches the defect the switch fixes", id, sw)
				continue
			}
			t.Logf("caught with %s off: %s %s", sw, id, run)
		}
	}
	for _, c := range settleCases(withSwitchesOff(settleSwitches...)) {
		t.Logf("on the line 4b-1 built, %s %s", c.ID(), runSeeds(t, c))
	}
}

// runSeeds runs c on its seeds.
func runSeeds(t *testing.T, c learnertest.Case) learnertest.CaseRun {
	t.Helper()
	seeds, _, err := learnertest.SeedsFor(c.Seeds)
	if err != nil {
		t.Fatal(err)
	}
	run, err := learnertest.RunCase(c.Check, seeds)
	if err != nil {
		t.Fatalf("%s: %v", c.ID(), err)
	}
	return run
}

// withSwitchOff is an analyzer with one replay switch off.
func withSwitchOff(name string) propAnalyzer { return withSwitchesOff(name) }

// withSwitchesOff is an analyzer with the replay switches named off.
func withSwitchesOff(names ...string) propAnalyzer {
	return func() *ThroughputAnalyzer {
		a := NewThroughputAnalyzer()
		off := map[string]bool{}
		for _, n := range names {
			off[n] = false
		}
		if err := a.SetSwitchesForReplay(off); err != nil {
			panic(err)
		}
		return a
	}
}
