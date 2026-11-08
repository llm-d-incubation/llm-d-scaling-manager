package throughput

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/internal/learnertest"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// Property cases for the throughput analyzer's ITL line
// (docs/proposals/analyzer-learner.md, 2.6; PR 4b, "Tests"), on the
// generators and judgement the saturation analyzer's cases run on
// (learnertest). Each case drives the analyzer cycle by cycle on synthetic
// rows and reads the line it prices. A case that fails on today's code by
// design is a row of testdata/properties/known-failures.yaml, with its
// owning PR; each mutant is a switch off (the parent's line), and
// TestThroughputPropertyMutants runs the cases the plan names against it.
//
// The line's declared estimand is the k-weighted ratio (ITL - B) / k, priced
// as ITL at itl.DefaultKSat (computeVariantSupply).

// propLearner is the learner the cases run on.
const propLearner = "throughput-itl"

// The true ITL line every case is generated from: ITL(k) = A k + B, with B
// away from itl.DefaultBaselineSec so a B stuck at the default shows.
const (
	propTrueA = 0.050
	propTrueB = 0.010
)

func propITL(k float64) float64 { return propTrueA*k + propTrueB }

// Declared tolerances (2.6).
const (
	// propBand is the first-sample band: how far a second identifying
	// reading may move the priced line.
	propBand = 0.05
	// propTransientTol is how far a refused or stamped transient may move
	// the priced line against the same run without it.
	propTransientTol = 0.03
	// propTruthTol is known truth's epsilon: priced capacity at most truth
	// x (1 + epsilon), so the priced ITL at least truth / (1 + epsilon).
	propTruthTol = 0.05
	// propStaleTol is how near a converged B must be to the truth. B is the
	// small part of every reading (10 ms of 20 to 48 ms), so propNoise on the
	// ITL is several times that on B: at N = 4 over 20 min the full fit's B
	// lands within 15 % on every seed, and within 10 % on all but about one
	// in a hundred. A B stuck at the default is 40 % off. Measured on the
	// tracker, the converged B's relative error has mean about 0 and SD
	// about 3.6 %, so 15 % is about 4 SD. It is this case's declared
	// tolerance, set with the case, not one widened to pass it.
	propStaleTol = 0.15
	// propNoise and propRho are the ITL gauge's relative noise and its
	// lag-one correlation across cycles.
	propNoise, propRho = 0.02, 0.5
)

// propPriced is the line's ITL at the k it is priced at.
func propPriced(m itl.Model) float64 { return m.ITLAt(itl.DefaultKSat) }

// propRun drives one analyzer cycle by cycle on its own clock.
type propRun struct {
	a     *ThroughputAnalyzer
	clock *clocktesting.FakePassiveClock
	cycle int
	cfg   domain.AnalyzerConfig
}

func newPropRun(a *ThroughputAnalyzer) *propRun {
	c := clocktesting.NewFakePassiveClock(learnertest.Start)
	return &propRun{a: a.WithClock(c), clock: c}
}

// now is the instant of the run's next cycle.
func (r *propRun) now() time.Time {
	return learnertest.Start.Add(time.Duration(r.cycle) * learnertest.Cycle)
}

// step analyzes one cycle of rows and returns the line v1 was priced with,
// if any.
func (r *propRun) step(rows []domain.ReplicaMetrics) (itl.Model, bool, error) {
	r.clock.SetTime(r.now())
	in := domain.AnalyzerInput{ModelID: "m", Namespace: "ns", ReplicaMetrics: rows, ArrivalRate: 4,
		VariantStates: []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: len(rows), GPUsPerReplica: 1}}}
	if r.cfg != nil {
		in.Config = r.cfg
	}
	res, err := r.a.Analyze(context.Background(), in)
	r.cycle++
	if err != nil {
		return itl.Model{}, false, err
	}
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == "v1" {
			st, _ := r.a.VariantState("m", "ns", "v1")
			return st.ITLModel, true, nil
		}
	}
	return itl.Model{}, false, nil
}

// windowLen is how many readings v1's window holds, on either line.
func (r *propRun) windowLen() int {
	st, _ := r.a.VariantState("m", "ns", "v1")
	return st.SampleCount
}

// propFleet is a fleet of replicas at fixed k on the true line, each ITL
// carrying its own autocorrelated noise; every replica Ready an hour before
// the run.
type propFleet struct {
	ks    []float64
	noise [][]float64
}

func newPropFleet(r *rand.Rand, cycles int, ks ...float64) propFleet {
	f := propFleet{ks: ks}
	for range ks {
		f.noise = append(f.noise, learnertest.ARNoise(r, cycles, propNoise, propRho))
	}
	return f
}

// rows is the fleet's rows on cycle c.
func (f propFleet) rows(c int) []domain.ReplicaMetrics {
	early := learnertest.Start.Add(-time.Hour)
	out := make([]domain.ReplicaMetrics, 0, len(f.ks))
	for i, k := range f.ks {
		out = append(out, lineRow(fmt.Sprintf("pod-%c", 'a'+i), k, propITL(k)*(1+f.noise[i][c]), early))
	}
	return out
}

// lineOf runs cycles of rows on a and returns the last line priced, the
// cycle it was priced on (-1 for none), and every priced line by cycle.
func lineOf(a *ThroughputAnalyzer, cycles int, rows func(c int) []domain.ReplicaMetrics) (last itl.Model, at int, lines map[int]itl.Model, err error) {
	r := newPropRun(a)
	at, lines = -1, map[int]itl.Model{}
	for c := range cycles {
		m, has, err := r.step(rows(c))
		if err != nil {
			return itl.Model{}, -1, nil, err
		}
		if has {
			last, at, lines[c] = m, c, m
		}
	}
	return last, at, lines, nil
}

// propAnalyzer is a case's analyzer: the one production builds, or a
// mutant's.
type propAnalyzer func() *ThroughputAnalyzer

// throughputCases is every property case, on the analyzer newA builds.
func throughputCases(newA propAnalyzer) []learnertest.Case {
	c := func(class learnertest.Class, name string, family learnertest.Family, seeds int, check func(uint64) error) learnertest.Case {
		return learnertest.Case{Class: class, Learner: propLearner, Name: name, Family: family, Tuning: "default", Seeds: seeds, Check: check}
	}
	direct, timeBase := learnertest.FamilyDirect, learnertest.FamilyTimeBase
	cheap := learnertest.CheapSeeds
	// The engine-rig cases of learner PR 4b-2 (property_settle_test.go)
	// first, then the line's own.
	cases := append(settleCases(newA), []learnertest.Case{
		c(learnertest.ClassTransient, "joiner-warm-up", direct, cheap, checkJoinerWarmUp(newA)),
		c(learnertest.ClassTransient, "load-step", direct, cheap, checkLoadStep(newA)),
		c(learnertest.ClassTransient, "controller-restart", direct, cheap, checkRestart(newA)),
		c(learnertest.ClassFirstSample, "one-reading", direct, cheap, checkOneReading(newA)),
		c(learnertest.ClassStale, "b-converges-n2", direct, cheap, checkBConverges(newA, 0.25, 0.70)),
		c(learnertest.ClassStale, "b-converges-n4", direct, cheap, checkBConverges(newA, 0.20, 0.40, 0.55, 0.75)),
		c(learnertest.ClassMissing, "infinite-itl", direct, cheap, checkMissing(newA, func(m *domain.ReplicaMetrics) { m.AvgITL = math.Inf(1) })),
		c(learnertest.ClassMissing, "zero-k", direct, cheap, checkMissing(newA, func(m *domain.ReplicaMetrics) {
			m.KvCacheUsage, m.KvUsageInstant = 0, 0
		})),
		c(learnertest.ClassMissing, "missed-row", direct, cheap, checkMissing(newA, nil)),
		c(learnertest.ClassHeterogeneity, "slow-replica", direct, cheap, checkSlowReplica(newA)),
		c(learnertest.ClassKnownTruth, "line-at-kprice", direct, cheap, checkKnownTruth(newA, false)),
		c(learnertest.ClassKnownTruth, "line-at-kprice-timebase", timeBase, cheap, checkKnownTruth(newA, true)),
		c(learnertest.ClassKnownTruth, "above-band-fallback", direct, cheap, checkAboveBandFallback(newA)),
		c(learnertest.ClassEdges, "ten-readings", direct, 1, checkTenReadings(newA)),
		c(learnertest.ClassEdges, "spread-0.30", direct, 1, checkSpreadEdge(newA)),
		c(learnertest.ClassEdges, "band-edges-default", direct, 1, checkBandEdges(newA, nil, itl.DefaultMinObservableK, itl.DefaultMaxObservableK)),
		c(learnertest.ClassEdges, "band-edges-non-default", direct, 1, checkBandEdges(newA, nonDefaultBand, 0.05, 0.90)),
		c(learnertest.ClassEdges, "steady-for-default", direct, 1, checkSteadyFor(newA, nil, itl.DefaultSteadyFor)),
		c(learnertest.ClassEdges, "steady-for-10m", direct, 1, checkSteadyFor(newA, nonDefaultSteady, 10*time.Minute)),
	}...)
	// Then the cap admission's, learner PR 4b-3 (property_cap_test.go).
	return append(cases, capCases(newA)...)
}

// The non-default policies 2.6 asks the edges to run at.
var (
	nonDefaultBand = &config.ScalingPolicy{AnalyzerTuning: config.AnalyzerTuning{
		ITLBand: config.ITLBandTuning{MinObservableK: 0.05, MaxObservableK: 0.90}}}
	nonDefaultSteady = &config.ScalingPolicy{AnalyzerTuning: config.AnalyzerTuning{SteadyGate: config.SteadyGateTuning{
		SteadyFor: config.PolicyDuration(10 * time.Minute), Carry: config.PolicyDuration(15 * time.Minute), IdleRows: 5}}}
)

// Transient: a joiner's first four readings carry its warm-up, 0.45 s at k
// 0.46 against a line near 0.03. The line priced on every cycle stays within
// propTransientTol of the same run without the joiner.
func checkJoinerWarmUp(newA propAnalyzer) func(uint64) error {
	return func(seed uint64) error {
		const before, during = 12, 4
		f := newPropFleet(learnertest.Rand(seed), before+during, 0.40, 0.40, 0.40)
		_, _, base, err := lineOf(newA(), before+during, f.rows)
		if err != nil {
			return err
		}
		joined := learnertest.Start.Add(before * learnertest.Cycle)
		_, _, with, err := lineOf(newA(), before+during, func(c int) []domain.ReplicaMetrics {
			rows := f.rows(c)
			if c >= before {
				rows = append(rows, lineRow("pod-join", 0.46, 0.45, joined))
			}
			return rows
		})
		if err != nil {
			return err
		}
		return movedFrom(base, with, before, before+during, "with a joiner's warm-up")
	}
}

// movedFrom is a violation of kind KindTransient when, on a cycle from first
// to the run's end, base priced a line and got priced none, or one more than
// propTransientTol off it.
func movedFrom(base, got map[int]itl.Model, first, cycles int, what string) error {
	if len(base) == 0 {
		return learnertest.Violate(learnertest.KindNotLearned, "the run without the transient priced no line")
	}
	for c := first; c < cycles; c++ {
		b, okB := base[c]
		if !okB {
			continue
		}
		g, okG := got[c]
		if !okG {
			return learnertest.Violate(learnertest.KindTransient, "cycle %d: no line %s, against %.4f s without", c, what, propPriced(b))
		}
		if e := learnertest.RelErr(propPriced(g), propPriced(b)); e > propTransientTol {
			return learnertest.Violate(learnertest.KindTransient, "cycle %d: priced %.4f s %s, against %.4f s without (%.1f %%)",
				c, propPriced(g), what, propPriced(b), 100*e)
		}
	}
	return nil
}

// Transient: a load step from k 0.30 to 0.55 on four spread replicas whose
// ITL overshoots the line by half for its first two minutes (the prefill
// burst of the step). The line priced from the step on stays within
// propTransientTol of the same step without the overshoot.
func checkLoadStep(newA propAnalyzer) func(uint64) error {
	return func(seed uint64) error {
		const steady, after = 24, 24
		f := newPropFleet(learnertest.Rand(seed), steady+after, 0.20, 0.30, 0.40, 0.55)
		rows := func(overshoot bool) func(int) []domain.ReplicaMetrics {
			return func(c int) []domain.ReplicaMetrics {
				out := f.rows(c)
				if c < steady {
					return out
				}
				for i := range out {
					k := out[i].KvCacheUsage + 0.25
					out[i].KvCacheUsage, out[i].KvUsageInstant = k, k
					out[i].AvgITL = propITL(k) * (1 + f.noise[i][c])
					if overshoot && c < steady+8 {
						out[i].AvgITL *= 1.5
					}
				}
				return out
			}
		}
		_, _, base, err := lineOf(newA(), steady+after, rows(false))
		if err != nil {
			return err
		}
		_, _, with, err := lineOf(newA(), steady+after, rows(true))
		if err != nil {
			return err
		}
		return movedFrom(base, with, steady, steady+after, "through the step's overshoot")
	}
}

// Transient: a controller restart. The restarted analyzer starts with
// nothing, and every line it prices is within propBand of the truth or on
// its safe side (a priced ITL above the truth orders more): one cycle's rows
// of a replica already serving set no line under the truth.
func checkRestart(newA propAnalyzer) func(uint64) error {
	return func(seed uint64) error {
		const before, after = 16, 16
		f := newPropFleet(learnertest.Rand(seed), before+after, 0.20, 0.30, 0.40)
		r := newPropRun(newA())
		for c := range before {
			if _, _, err := r.step(f.rows(c)); err != nil {
				return err
			}
		}
		restarted := newPropRun(newA())
		restarted.cycle = before
		truth := propITL(itl.DefaultKSat)
		priced := false
		for c := before; c < before+after; c++ {
			m, has, err := restarted.step(f.rows(c))
			if err != nil {
				return err
			}
			if !has {
				continue
			}
			priced = true
			if p := propPriced(m); p < truth/(1+propTruthTol) {
				return learnertest.Violate(learnertest.KindOverPriced, "%s after the restart: priced %.4f s, under the truth %.4f s by %.1f %%",
					time.Duration(c-before)*learnertest.Cycle, p, truth, 100*(1-p/truth))
			}
		}
		return learnertest.Holds(priced, learnertest.KindNotLearned, "no line in %d cycles after the restart", after)
	}
}

// First sample: one reading of one replica sets no line that a second,
// identifying reading would move by more than propBand. The tracker sets
// none from fewer than itl.DefaultMinSamples readings.
func checkOneReading(newA propAnalyzer) func(uint64) error {
	return func(seed uint64) error {
		r := learnertest.Rand(seed)
		early := learnertest.Start.Add(-time.Hour)
		k := 0.20 + 0.10*r.Float64()
		first := lineRow("pod-a", k, propITL(k)*(1+propNoise*r.NormFloat64()), early)
		run := newPropRun(newA())
		m1, has, err := run.step([]domain.ReplicaMetrics{first})
		if err != nil || !has {
			return err
		}
		k2 := 0.60 + 0.10*r.Float64()
		second := lineRow("pod-a", k2, propITL(k2)*(1+propNoise*r.NormFloat64()), early)
		m2, has2, err := run.step([]domain.ReplicaMetrics{second})
		if err != nil {
			return err
		}
		if !has2 {
			return learnertest.Violate(learnertest.KindAdoptedRaw, "a line from one reading (%.4f s), none after the second", propPriced(m1))
		}
		return learnertest.Holds(learnertest.RelErr(propPriced(m1), propPriced(m2)) <= propBand, learnertest.KindAdoptedRaw,
			"one reading at k %.2f priced %.4f s; a second at k %.2f moved it to %.4f s (%.1f %%)",
			k, propPriced(m1), k2, propPriced(m2), 100*learnertest.RelErr(propPriced(m1), propPriced(m2)))
	}
}

// Stale value: from identifying data -- replicas spread over the band, at N
// of 2 and 4 -- the line's remembered B converges to the truth within 20 min
// (catches A3: B stuck at 6 ms).
func checkBConverges(newA propAnalyzer, ks ...float64) func(uint64) error {
	return func(seed uint64) error {
		const cycles = 80
		f := newPropFleet(learnertest.Rand(seed), cycles, ks...)
		a := newA()
		if _, _, _, err := lineOf(a, cycles, f.rows); err != nil {
			return err
		}
		st, _ := a.VariantState("m", "ns", "v1")
		if !st.HasFittedB {
			return learnertest.Violate(learnertest.KindStale, "no B remembered after %s at N=%d", cycles*learnertest.Cycle, len(ks))
		}
		return learnertest.Holds(learnertest.RelErr(st.LastFittedB, propTrueB) <= propStaleTol, learnertest.KindStale,
			"B %.4f s after %s at N=%d, against the true %.4f s", st.LastFittedB, cycles*learnertest.Cycle, len(ks), propTrueB)
	}
}

// Missing and duplicated: one replica's row on one cycle is edited (an
// infinite ITL, a k of 0) or, with edit nil, missing. The line is priced on
// that cycle, within propTransientTol of the same run without the defect.
func checkMissing(newA propAnalyzer, edit func(*domain.ReplicaMetrics)) func(uint64) error {
	return func(seed uint64) error {
		const before, after = 16, 4
		f := newPropFleet(learnertest.Rand(seed), before+after, 0.40, 0.40, 0.40)
		_, _, base, err := lineOf(newA(), before+after, f.rows)
		if err != nil {
			return err
		}
		_, _, with, err := lineOf(newA(), before+after, func(c int) []domain.ReplicaMetrics {
			rows := f.rows(c)
			if c != before {
				return rows
			}
			if edit == nil {
				return rows[1:]
			}
			edit(&rows[0])
			return rows
		})
		if err != nil {
			return err
		}
		return movedFrom(base, with, before, before+after, "with the defect")
	}
}

// Contamination paired with heterogeneity: one of four replicas at the
// same k is genuinely slower, its slope 1.3 times the others', with no state
// flag. The line moves by its weight under the declared estimand, the
// k-weighted mean: A is (3 + 1.3) / 4 of the truth's.
func checkSlowReplica(newA propAnalyzer) func(uint64) error {
	return func(seed uint64) error {
		const cycles = 16
		f := newPropFleet(learnertest.Rand(seed), cycles, 0.40, 0.40, 0.40, 0.40)
		last, at, _, err := lineOf(newA(), cycles, func(c int) []domain.ReplicaMetrics {
			rows := f.rows(c)
			rows[3].AvgITL = (1.3*propTrueA*0.40 + propTrueB) * (1 + f.noise[3][c])
			return rows
		})
		if err != nil {
			return err
		}
		if at < 0 {
			return learnertest.Violate(learnertest.KindNotLearned, "no line in %d cycles", cycles)
		}
		// At one k the k-weighted ratio is the replicas' mean ITL less B,
		// over k; the pinned fit is through the line's own B.
		slow := 1.3*propTrueA*0.40 + propTrueB
		want := ((3*propITL(0.40)+slow)/4 - last.B) / 0.40
		return learnertest.Holds(learnertest.RelErr(last.A, want) <= propTransientTol, learnertest.KindBiased,
			"A %.4f, against %.4f for the replicas' k-weighted mean through B %.4f", last.A, want, last.B)
	}
}

// Known truth: with the true line under noise, the priced ITL at
// itl.DefaultKSat is at least the truth / (1 + propTruthTol) on every cycle
// a line is priced after the first five minutes, so priced capacity is at
// most truth x (1 + epsilon). On the time-base family each replica's k is a
// per-second path around its level, read as the minute's mean.
func checkKnownTruth(newA propAnalyzer, timeBase bool) func(uint64) error {
	return func(seed uint64) error {
		const cycles = 80
		r := learnertest.Rand(seed)
		f := newPropFleet(r, cycles, 0.20, 0.35, 0.50, 0.70)
		rows := f.rows
		if timeBase {
			paths := make([]learnertest.TimeBase, len(f.ks))
			for i, k := range f.ks {
				path := make([]float64, 60+cycles*int(learnertest.Cycle/time.Second))
				for s, n := range learnertest.ARNoise(r, len(path), 0.05, 0.98) {
					path[s] = k * (1 + n)
				}
				paths[i] = learnertest.SampleTimeBase(path)
			}
			rows = func(c int) []domain.ReplicaMetrics {
				out := f.rows(c)
				for i := range out {
					k := paths[i].Avg1m[c]
					out[i].KvCacheUsage, out[i].KvUsageInstant = paths[i].Instant[c], paths[i].Instant[c]
					out[i].KvUsageMean, out[i].KvUsageMeanKnown = k, true
					out[i].AvgITL = propITL(k) * (1 + f.noise[i][c])
				}
				return out
			}
		}
		_, _, lines, err := lineOf(newA(), cycles, rows)
		if err != nil {
			return err
		}
		truth := propITL(itl.DefaultKSat)
		priced := 0
		for c := 20; c < cycles; c++ {
			m, ok := lines[c]
			if !ok {
				continue
			}
			priced++
			if p := propPriced(m); p < truth/(1+propTruthTol) {
				return learnertest.Violate(learnertest.KindOverPriced, "cycle %d: priced %.4f s, under the truth %.4f s by %.1f %%",
					c, p, truth, 100*(1-p/truth))
			}
		}
		return learnertest.Holds(priced > 0, learnertest.KindNotLearned, "no line priced after five minutes")
	}
}

// Known truth, the above-band fallback: a key that never fitted, its
// replicas steady above the band at k 0.86 to 0.94 (at or above
// itl.DefaultKSat, the only readings the fallback keeps), prices from them
// (T2-above-band), and never under the true line: on every cycle it prices,
// its ITL at itl.DefaultKSat is at least the truth's, for a true B under the
// default the band pins (4 ms) and over it (propTrueB, 10 ms). One-sided by
// construction (itl's fitAboveBand): B is pinned at the least ITL among the
// readings, which is at or above the true B, and every reading's k is at or
// above the price's.
//
// Margins, measured over the 100 seeds: at 4 ms the fallback prices at
// least 2.5 % over the truth on every cycle (one seed's worst is 2.97 %);
// at 10 ms at least 2.0 % (four seeds' worst under 2.5 %, the least 2.42 %).
// The margin is the pinned B's: the least ITL, about 48 ms, against a true B
// of 4 or 10, times 1 - kSat/kbar. Negative control (a mutant run, and
// itl's TestFitAboveBand_NeverPricesUnderTheTrueLine): with B pinned at
// itl.DefaultBaselineSec, as the band's fit pins a key that never fitted,
// the 10 ms case prices under the truth on all 100 seeds (by over 1 % on
// 85, over 2 % on 11), and the 4 ms case too: there the B term is small
// and the gauge's noise outweighs it.
func checkAboveBandFallback(newA propAnalyzer) func(uint64) error {
	return aboveBandFallbackCheck(newA, 24, 0, 0, func(a *ThroughputAnalyzer, rows func(int) []domain.ReplicaMetrics) (map[int]itl.Model, error) {
		_, _, lines, err := lineOf(a, 24, rows)
		return lines, err
	})
}

// aboveBandFallbackCheck is checkAboveBandFallback's judgement over the
// given number of cycles of the analyzer newA builds, run by run. The first
// warm cycles read 30 % over the true line: a transient the run starts in,
// which sits above the line and can only raise the price. With firstBy
// above zero the fallback must have priced by that cycle.
func aboveBandFallbackCheck(newA propAnalyzer, cycles, warm, firstBy int,
	run func(*ThroughputAnalyzer, func(int) []domain.ReplicaMetrics) (map[int]itl.Model, error)) func(uint64) error {
	return func(seed uint64) error {
		f := newPropFleet(learnertest.Rand(seed), cycles, 0.86, 0.90, 0.94)
		for _, b := range []float64{0.004, propTrueB} {
			a := newA()
			lines, err := run(a, func(c int) []domain.ReplicaMetrics {
				out := f.rows(c)
				for i := range out {
					out[i].AvgITL = (propTrueA*f.ks[i] + b) * (1 + f.noise[i][c])
					if c < warm {
						out[i].AvgITL *= 1.3
					}
				}
				return out
			})
			if err != nil {
				return err
			}
			if len(lines) == 0 {
				return learnertest.Violate(learnertest.KindNotLearned, "true B %.3f s: no line in %d cycles above the band", b, cycles)
			}
			if n := a.PathCounts()[PathITLAboveBand]; n == 0 {
				return learnertest.Violate(learnertest.KindNotLearned, "true B %.3f s: a line above the band, but not the fallback's", b)
			}
			if firstBy > 0 {
				first := cycles
				for c := range lines {
					first = min(first, c)
				}
				if first > firstBy {
					return learnertest.Violate(learnertest.KindNotLearned, "true B %.3f s: the fallback first priced on cycle %d, after cycle %d", b, first, firstBy)
				}
			}
			truth := propTrueA*itl.DefaultKSat + b
			for c := range cycles {
				m, ok := lines[c]
				if !ok {
					continue
				}
				if p := propPriced(m); p < truth {
					return learnertest.Violate(learnertest.KindOverPriced, "true B %.3f s, cycle %d: the fallback priced %.4f s, under the truth %.4f s by %.2f %%",
						b, c, p, truth, 100*(1-p/truth))
				}
			}
		}
		return nil
	}
}

// Edges, ten readings: one replica, one reading a cycle. The ninth cycle
// prices no line, and the tenth does (itl.DefaultMinSamples).
func checkTenReadings(newA propAnalyzer) func(uint64) error {
	return func(uint64) error {
		_, _, lines, err := lineOf(newA(), 10, func(int) []domain.ReplicaMetrics {
			return []domain.ReplicaMetrics{lineRow("pod-a", 0.40, propITL(0.40), learnertest.Start.Add(-time.Hour))}
		})
		if err != nil {
			return err
		}
		for c := range 9 {
			if _, ok := lines[c]; ok {
				return learnertest.Violate(learnertest.KindWrongSide, "a line from %d readings, under the %d the window needs", c+1, itl.DefaultMinSamples)
			}
		}
		_, ok := lines[9]
		return learnertest.Holds(ok, learnertest.KindWrongSide, "no line from %d readings", itl.DefaultMinSamples)
	}
}

// Edges, the spread: two replicas, steady, k apart by just under and just
// over itl.DefaultMinKSpread. The full fit is reached over the edge and not
// under it.
func checkSpreadEdge(newA propAnalyzer) func(uint64) error {
	return func(uint64) error {
		ols := func(spread float64) (bool, error) {
			a := newA()
			_, _, _, err := lineOf(a, 40, func(int) []domain.ReplicaMetrics {
				early := learnertest.Start.Add(-time.Hour)
				return []domain.ReplicaMetrics{lineRow("pod-a", 0.30, propITL(0.30), early),
					lineRow("pod-b", 0.30+spread, propITL(0.30+spread), early)}
			})
			st, _ := a.VariantState("m", "ns", "v1")
			return st.ObservationReady, err
		}
		under, err := ols(itl.DefaultMinKSpread - 0.01)
		if err != nil {
			return err
		}
		over, err := ols(itl.DefaultMinKSpread + 0.01)
		if err != nil {
			return err
		}
		return learnertest.Holds(!under && over, learnertest.KindWrongSide,
			"full fit at a spread of %.2f: %v; at %.2f: %v", itl.DefaultMinKSpread-0.01, under, itl.DefaultMinKSpread+0.01, over)
	}
}

// Edges, the band: a replica just inside each edge of the band is a reading,
// and one just outside is not, at the default band and at a non-default one.
func checkBandEdges(newA propAnalyzer, cfg domain.AnalyzerConfig, lo, hi float64) func(uint64) error {
	return func(uint64) error {
		for _, tc := range []struct {
			k    float64
			want bool
		}{{lo - 0.005, false}, {lo + 0.005, true}, {hi - 0.005, true}, {hi + 0.005, false}} {
			r := newPropRun(newA())
			r.cfg = cfg
			if _, _, err := r.step([]domain.ReplicaMetrics{lineRow("pod-a", tc.k, propITL(tc.k), learnertest.Start.Add(-time.Hour))}); err != nil {
				return err
			}
			if got := r.windowLen() == 1; got != tc.want {
				return learnertest.Violate(learnertest.KindWrongSide, "a reading at k %.3f in the band [%.2f, %.2f]: kept %v, want %v",
					tc.k, lo, hi, got, tc.want)
			}
		}
		return nil
	}
}

// Edges, steadyFor: three replicas at k 0.30 for 4 min, then two of them
// step to 0.55 and 0.75. The spread the full fit needs comes from the new
// level, whose readings are unsteady until it has held for steadyFor: the
// full fit comes no earlier than steadyFor after the step and within two
// cycles of it, at the default and at 10 min.
func checkSteadyFor(newA propAnalyzer, cfg domain.AnalyzerConfig, steadyFor time.Duration) func(uint64) error {
	return func(uint64) error {
		const step = 16
		r := newPropRun(newA())
		r.cfg = cfg
		early := learnertest.Start.Add(-time.Hour)
		first := -1
		for c := range step + 60 {
			ks := []float64{0.30, 0.30, 0.30}
			if c >= step {
				ks = []float64{0.30, 0.55, 0.75}
			}
			rows := make([]domain.ReplicaMetrics, 0, len(ks))
			for i, k := range ks {
				rows = append(rows, lineRow(fmt.Sprintf("pod-%c", 'a'+i), k, propITL(k), early))
			}
			if _, _, err := r.step(rows); err != nil {
				return err
			}
			st, _ := r.a.VariantState("m", "ns", "v1")
			if st.ObservationReady && first < 0 {
				first = c
			}
		}
		if first < 0 {
			return learnertest.Violate(learnertest.KindNotLearned, "no full fit in 15 min after the step")
		}
		at := time.Duration(first-step) * learnertest.Cycle
		return learnertest.Holds(at >= steadyFor && at <= steadyFor+2*learnertest.Cycle, learnertest.KindWrongSide,
			"first full fit %s after the step, against steadyFor %s", at, steadyFor)
	}
}

// propLedgerPath is the throughput line's known-failures ledger.
const propLedgerPath = "testdata/properties/known-failures.yaml"

// TestThroughputLineProperties runs every property case on the analyzer
// production builds, each judged against its ledger row.
func TestThroughputLineProperties(t *testing.T) {
	learnertest.RunProperties(t, throughputCases(NewThroughputAnalyzer), propLedgerPath)
}

// TestThroughputPropertyLedgerNamesRegisteredCases checks the ledger file
// against the registered cases.
func TestThroughputPropertyLedgerNamesRegisteredCases(t *testing.T) {
	for _, e := range learnertest.CheckLedger(learnertest.ReadLedger(t, propLedgerPath), throughputCases(NewThroughputAnalyzer)) {
		t.Error(e)
	}
}

// TestThroughputPropertyCaseListIsPinned pins the registered case ids.
func TestThroughputPropertyCaseListIsPinned(t *testing.T) {
	learnertest.CheckCaseList(t, throughputCases(NewThroughputAnalyzer), "testdata/properties/cases.txt")
}

// throughputMutants is each case the plan names a negative control for,
// with the switch-off analyzer that reintroduces the defect: the parent's
// line (SwitchITLSteadyGate off) for the line's own fixes.
var throughputMutants = []string{
	"transient/throughput-itl/joiner-warm-up",
	"transient/throughput-itl/load-step",
	"first-sample/throughput-itl/one-reading",
	"missing-duplicated/throughput-itl/infinite-itl",
	"edges/throughput-itl/ten-readings",
	"edges/throughput-itl/steady-for-default",
	"edges/throughput-itl/steady-for-10m",
}

// TestThroughputPropertyMutants runs the named cases on the parent's line,
// each mutant a switch off (analyzer-learner.md, PR 4b, "Tests"): every one
// must fail there, and its first failure is logged as the case's negative
// control. Every other case's result on the parent is logged too.
func TestThroughputPropertyMutants(t *testing.T) {
	mutant := map[string]bool{}
	for _, id := range throughputMutants {
		mutant[id] = true
	}
	found := 0
	for _, c := range throughputCases(newParentLineAnalyzer) {
		seeds, _, err := learnertest.SeedsFor(c.Seeds)
		if err != nil {
			t.Fatal(err)
		}
		run, err := learnertest.RunCase(c.Check, seeds)
		if err != nil {
			t.Fatalf("%s: %v", c.ID(), err)
		}
		if !mutant[c.ID()] {
			t.Logf("on the parent's line, %s %s", c.ID(), run)
			continue
		}
		found++
		if len(run.Failures) == 0 {
			t.Errorf("%s passes on the parent's line: the case no longer catches the defect the steady gate fixes", c.ID())
			continue
		}
		t.Logf("caught on the parent's line: %s %s", c.ID(), run)
	}
	if found != len(throughputMutants) {
		t.Errorf("%d of the %d mutant cases are registered", found, len(throughputMutants))
	}
}
