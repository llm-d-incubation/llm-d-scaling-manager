package saturation

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/internal/learnertest"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// Issue ev-shindin/llm-scaling-manager#467: the ITL line priced one-sided at the sequence cap while B is
// unlearned (SwitchITLOneSidedAtCap), through Analyze, on the engine's rig.

// oneSidedK is the line's pricing k under makeAnalyzerInput's policy
// (pricingK): kvCacheThreshold 0.8 x scaleUpThreshold 0.85.
var oneSidedK = pricingK(makeAnalyzerInput(nil, nil).Config.(*config.ScalingPolicy))

// capTruth is a true line ITL = a k + b, and the fill k_cap a replica at its
// cap sits at.
type capTruth struct{ a, b, kCap float64 }

func (l capTruth) itl(k float64) float64 { return l.a*k + l.b }

func (l capTruth) String() string {
	return fmt.Sprintf("A %.2f B %.0f ms k_cap %.2f", l.a, 1000*l.b, l.kCap)
}

// capTruths is issue ev-shindin/llm-scaling-manager#467's bias grid, where the default B prices under the
// truth.
var capTruths = []capTruth{{0.05, 0.010, 0.97}, {0.05, 0.030, 0.97}, {0.01, 0.020, 0.97}, {0.01, 0.030, 0.97}}

// oneSidedRig is the engine's rig over a store stating a cap of 64 for
// variant v, with switches set.
func oneSidedRig(t *testing.T, switches map[string]bool) *restartRig {
	t.Helper()
	r := itlRig(t, switches)
	stateCap(r)
	return r
}

// stateCap files v's capacity record, its scale target declaring a cap of
// 64, as the engine does from the scale target each cycle.
func stateCap(r *restartRig) {
	r.a.capacityStore.Update("test-ns", "test-model", "v", capacity.Record{GpuCount: 1, EngineParams: capParams(),
		LearnedFrom: "deployment"}, r.now)
}

// capParams is v's engine: the deployment's, its scale target declaring a
// cap of 64.
func capParams() *capacity.EngineParams {
	p := deploymentParams()
	p.DeclaredBatchCap = 64
	return p
}

// atCapRow is a replica of v at its cap of 64, 5 waiting, no preemption, at
// fill k reading itlSec.
func atCapRow(pod string, k, itlSec float64) domain.ReplicaMetrics {
	rm := itlSettleRow(pod, k)
	rm.AvgITL = itlSec
	rm.RunningRequests, rm.QueueLength, rm.PreemptionRateKnown = 64, 5, true
	return rm
}

// atCapRows is three replicas at their cap on truth l, at k_cap - 0.01, k_cap
// and k_cap + 0.01, each ITL scaled by (1 + noise[i]).
func atCapRows(l capTruth, noise [3]float64) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, 3)
	for i, k := range []float64{l.kCap - 0.01, l.kCap, l.kCap + 0.01} {
		out = append(out, atCapRow(fmt.Sprintf("c%d", i), k, l.itl(k)*(1+noise[i])))
	}
	return out
}

// lineOf is the line the cycle priced variant v on, zero with none, and
// whether its slope was held: the itl-fit line. The model's variants are v
// and two siblings, sib (decode, v's role) and pre (prefill), so a prior's
// scope knows their roles.
func lineOf(t *testing.T, r *restartRig, rows []domain.ReplicaMetrics) (itl.Model, bool) {
	t.Helper()
	ctx, logs := observedCtx(t)
	states := decodeVariants("v", "sib", "pre")
	states[2].Role = domain.RolePrefill
	r.cycleCtx(ctx, rows, states)
	reports := reportsOf(r.a.LearnerReports()["test-ns|test-model"], learn.ITL)
	require.NotEmpty(t, reports)
	fits := logs.FilterMessage("itl-fit").All()
	if len(fits) == 0 {
		return itl.Model{}, false
	}
	f := fits[len(fits)-1].ContextMap()
	held, _ := f["slopeHeld"].(bool)
	return itl.Model{A: f["a"].(float64), B: f["b"].(float64)}, held
}

// The itl-fit line says when the line was priced one-sided
// (pinnedAtBPrior), and not once B is learned or with the switch off, so
// an operator can tell it from the default pin, which shows the same B.
func TestITLOneSided_TheFitLineSaysSo(t *testing.T) {
	l := capTruth{0.01, 0.030, 0.97}
	said := func(switches map[string]bool) bool {
		r := oneSidedRig(t, switches)
		lineAfter(t, r, 11, func(int) []domain.ReplicaMetrics { return atCapRows(l, [3]float64{}) })
		ctx, logs := observedCtx(t)
		r.cycleCtx(ctx, atCapRows(l, [3]float64{}), decodeVariants("v"))
		fits := logs.FilterMessage("itl-fit").All()
		require.NotEmpty(t, fits)
		v, ok := fits[len(fits)-1].ContextMap()["pinnedAtBPrior"].(bool)
		require.True(t, ok, "itl-fit carries no pinnedAtBPrior")
		return v
	}
	require.True(t, said(nil), "on: the line priced one-sided and the log says not")
	require.False(t, said(map[string]bool{SwitchITLOneSidedAtCap: false}), "off: the log says one-sided")
}

// lineAfter runs n cycles of rows and returns the last cycle's line.
func lineAfter(t *testing.T, r *restartRig, n int, rows func(c int) []domain.ReplicaMetrics) itl.Model {
	t.Helper()
	var m itl.Model
	for c := range n {
		m, _ = lineOf(t, r, rows(c))
	}
	return m
}

// oneSidedErr is the error, in percent, of the line priced on truth l at
// the pricing k after 24 cycles, the readings scaled by noise per cycle;
// prime, when set, edits the rig before its first cycle.
func oneSidedErr(t *testing.T, switches map[string]bool, prime func(*restartRig), l capTruth,
	noise func(c int) [3]float64) float64 {
	t.Helper()
	e, _ := oneSidedRun(t, switches, prime, l, noise)
	return e
}

// oneSidedRun is oneSidedErr, and the mean fill kbar of the readings the
// pinned fit read at or past kSat (what the prior's line passes through).
func oneSidedRun(t *testing.T, switches map[string]bool, prime func(*restartRig), l capTruth,
	noise func(c int) [3]float64) (errPct, kbar float64) {
	t.Helper()
	r := oneSidedRig(t, switches)
	if prime != nil {
		prime(r)
	}
	m := lineAfter(t, r, 24, func(c int) []domain.ReplicaMetrics { return atCapRows(l, noise(c)) })
	require.False(t, m.IsZero(), "%s: no line", l)
	truth := l.itl(oneSidedK)
	rs := r.a.itlTracker.Readings(r.a.itlWindowKey("test-ns", "test-model", "v", "H200", 1))
	src := rs.Steady
	if len(src) < itl.DefaultMinSamples {
		src = rs.All
	}
	var sum float64
	n := 0
	for _, o := range src[max(0, len(src)-itl.DefaultMinSamples):] {
		if o.K >= itl.DefaultKSat {
			sum, n = sum+o.K, n+1
		}
	}
	if n > 0 {
		kbar = sum / float64(n)
	}
	return 100 * (m.ITLAt(oneSidedK) - truth) / truth, kbar
}

// priorBound is the most the prior's line can price at the pricing k, in
// percent over the truth, on readings without noise: through the mean at-cap
// point (kbar, the truth's ITL there), with intercept bPrior, its slope
// floored at 0. The window's cap (the least ITL) can only lower the
// intercept toward B and tighten it. kbar is the mean fill the line read
// (oneSidedRun); k_cap when it is not known.
func priorBound(l capTruth, bPrior, kbar float64) float64 {
	if !(kbar > 0) {
		kbar = l.kCap
	}
	p := bPrior + max(0, l.itl(kbar)-bPrior)*oneSidedK/kbar
	return 100 * (p - l.itl(oneSidedK)) / l.itl(oneSidedK)
}

// siblingKey is the window key of variant sib, in v's physics: the same
// model, accelerator (H200) and GPU count, under the same engine arguments
// once stateArgs files them, in the same role (decode, lineOf).
const siblingKey = "test-ns|test-model|sib|H200|1"

// learnedPrior primes a rig with a learned B b on the sibling's key, in v's
// physics: itl.BPrior raises the 30 ms floor to it, never lowers it.
func learnedPrior(b float64) func(*restartRig) {
	return func(r *restartRig) {
		r.a.itlTracker.SetBaseline(siblingKey, b)
		r.a.itlUnderArgs(siblingKey, windowArgs(capParams()))
	}
}

// outOfScope is, per dimension, a key that differs from v's physics in that
// one dimension alone, and the engine arguments it was fitted under:
// another accelerator, another GPU count, another namespace, other engine
// arguments, and another role (the prefill sibling pre).
var outOfScope = []struct{ name, key, args string }{
	{"accelerator", "test-ns|test-model|sib|H100|1", ""},
	{"GPU count", "test-ns|test-model|sib|H200|8", ""},
	{"namespace", "other-ns|test-model|sib|H200|1", ""},
	{"engine args", siblingKey, "other-args"},
	{"role", "test-ns|test-model|pre|H200|1", ""},
}

// outOfScopePrior primes a rig with a learned B b on one out-of-scope key,
// fitted under args (v's arguments when "").
func outOfScopePrior(key, args string, b float64) func(*restartRig) {
	return func(r *restartRig) {
		if args == "" {
			args = windowArgs(capParams())
		}
		r.a.itlTracker.SetBaseline(key, b)
		r.a.itlUnderArgs(key, args)
	}
}

// biasRow measures one truth three ways, prints its row, and returns on
// without noise, and the mean and the least of the noisy seeds. label names
// the prior's case.
func biasRow(t *testing.T, label string, l capTruth, prime func(*restartRig), bPrior float64) (on, mean, least float64) {
	t.Helper()
	const seeds = 20
	clean := func(int) [3]float64 { return [3]float64{} }
	off := oneSidedErr(t, map[string]bool{SwitchITLOneSidedAtCap: false}, prime, l, clean)
	on, kbar := oneSidedRun(t, nil, prime, l, clean)
	bound := priorBound(l, bPrior, kbar)
	want := 100 * (itl.DefaultBaselineSec - l.b) * (1 - oneSidedK/l.kCap) / l.itl(oneSidedK)
	var sum, lo, hi = 0.0, math.Inf(1), math.Inf(-1)
	for seed := uint64(1); seed <= seeds; seed++ {
		rnd := learnertest.Rand(seed)
		n := [3][]float64{learnertest.ARNoise(rnd, 24, 0.02, 0.5), learnertest.ARNoise(rnd, 24, 0.02, 0.5),
			learnertest.ARNoise(rnd, 24, 0.02, 0.5)}
		e := oneSidedErr(t, nil, prime, l, func(c int) [3]float64 { return [3]float64{n[0][c], n[1][c], n[2][c]} })
		sum += e
		lo, hi = min(lo, e), max(hi, e)
	}
	t.Logf("| %s | %.2f | %.0f ms | %.2f | %+.2f %% | %+.2f %% | %+.2f %% | %+.2f %% | %+.2f %% [%+.2f, %+.2f] |",
		label, l.a, 1000*l.b, l.kCap, off, want, on, bound, sum/seeds, lo, hi)
	require.InDelta(t, want, off, 0.1, "negative control, %s: off is not the derived term", l)
	require.Negative(t, off, "negative control, %s: off did not price under the truth", l)
	// At most the prior's line: the window's cap only tightens it. To 0.05
	// points: kbar here is the mean of the last readings, the fit's own mix
	// of the three fills is near it, not equal.
	require.LessOrEqual(t, on, bound+0.05, "%s: on over the prior's bound", l)
	return on, sum / seeds, lo
}

// The noise gates at B equal to the prior, where the margin is zero: the
// mean of 20 seeds of 2 % autocorrelated noise at most 0.5 % under the truth,
// the worst seed at most 1.5 % under.
const (
	noiseMeanGate  = -0.5
	noiseWorstGate = -1.5
)

// The bias table at the saturation line's pricing k (0.68): a variant whose
// three replicas sit at their stated cap, no B learned on its key. Off (the
// switch off, the negative control) the line through the default B is
// under the truth by the derived term (B0 - B)(1 - k/kbar). On, at most the
// prior's line (priorBound), and not under the truth while B is at most the
// prior, without noise. Cases, one subtest each:
//
//   - the floor, 30 ms, with nothing learned, and its noise gates;
//   - a learned B of 45 ms on a sibling in the variant's physics, which
//     raises the prior;
//   - a learned 45 ms on a key that differs in one dimension alone
//     (accelerator, GPU count, namespace, engine arguments, role), which
//     must leave the floor's price: the bug the fleet-wide prior had;
//   - an outlier fit of 80 ms in the physics, which the window's least ITL
//     caps;
//   - a true B above the prior (40 ms), under by the bound.
func TestITLOneSided_TheBiasTableAtThePricingK(t *testing.T) {
	t.Log("| prior | A | B | k_cap | off | derived | on | bound | on, 2 % noise: mean [least, most] |")
	floor := map[capTruth]float64{}
	for _, l := range capTruths {
		t.Run("floor/"+l.String(), func(t *testing.T) {
			on, mean, lo := biasRow(t, "30 ms", l, nil, itl.DefaultBPrior)
			floor[l] = on
			require.GreaterOrEqual(t, on, -1e-9, "%s: on priced under the truth without noise", l)
			if l.b == itl.DefaultBPrior {
				require.GreaterOrEqual(t, mean, noiseMeanGate, "%s: the noisy mean past its gate", l)
				require.GreaterOrEqual(t, lo, noiseWorstGate, "%s: the worst noisy seed past its gate", l)
			}
		})
	}
	for _, l := range capTruths {
		t.Run("scoped 45 ms/"+l.String(), func(t *testing.T) {
			on, _, _ := biasRow(t, "45 ms scoped", l, learnedPrior(0.045), 0.045)
			require.GreaterOrEqual(t, on, -1e-9, "scoped learned prior, %s: on priced under the truth", l)
		})
	}
	for _, d := range outOfScope {
		for _, l := range capTruths {
			t.Run("out of scope by "+d.name+"/"+l.String(), func(t *testing.T) {
				on, _, _ := biasRow(t, "45 ms, other "+d.name, l, outOfScopePrior(d.key, d.args, 0.045), itl.DefaultBPrior)
				require.InDelta(t, floor[l], on, 1e-9, "%s: a 45 ms B on another %s moved the prior", l, d.name)
			})
		}
	}
	for _, l := range capTruths {
		t.Run("outlier 80 ms/"+l.String(), func(t *testing.T) {
			on, _, _ := biasRow(t, "80 ms outlier", l, learnedPrior(0.080), 0.080)
			require.GreaterOrEqual(t, on, -1e-9, "outlier prior, %s: on priced under the truth", l)
			// Capped at the least ITL: at most the line with that intercept.
			capped := priorBound(l, l.itl(l.kCap-0.01), 0)
			require.LessOrEqual(t, on, capped+0.5, "outlier prior, %s: not capped by the window", l)
			require.Less(t, on, priorBound(l, 0.080, 0)-1, "outlier prior, %s: priced at the uncapped prior", l)
		})
	}
	t.Run("B above the prior", func(t *testing.T) {
		above := capTruth{0.01, 0.040, 0.97}
		on, _, _ := biasRow(t, "30 ms", above, nil, itl.DefaultBPrior)
		require.Negative(t, on, "B above the prior: not under the truth")
		_, kbar := oneSidedRun(t, nil, nil, above, func(int) [3]float64 { return [3]float64{} })
		require.InDelta(t, priorBound(above, itl.DefaultBPrior, kbar), on, 0.05, "B above the prior: not the bound")
	})
}

// The noise gates hold for the analyzer's line (through the mean at-cap
// point), and fail for the line through the least at-cap reading, which
// 4b-3's rule and the first prior used: its selection bias (the least of the
// noisy readings sits under the true line by about the noise) puts it under
// the gates. The least-reading line is worked by hand from the readings the
// analyzer kept, at B equal to the prior.
func TestITLOneSided_TheLeastReadingLineFailsTheNoiseGates(t *testing.T) {
	const seeds = 20
	for _, l := range []capTruth{{0.05, 0.030, 0.97}, {0.01, 0.030, 0.97}} {
		t.Run(l.String(), func(t *testing.T) {
			truth := l.itl(oneSidedK)
			var sumMean, sumLeast float64
			worstMean, worstLeast := math.Inf(1), math.Inf(1)
			for seed := uint64(1); seed <= seeds; seed++ {
				rnd := learnertest.Rand(seed)
				n := [3][]float64{learnertest.ARNoise(rnd, 24, 0.02, 0.5), learnertest.ARNoise(rnd, 24, 0.02, 0.5),
					learnertest.ARNoise(rnd, 24, 0.02, 0.5)}
				r := oneSidedRig(t, nil)
				on := lineAfter(t, r, 24, func(c int) []domain.ReplicaMetrics { return atCapRows(l, [3]float64{n[0][c], n[1][c], n[2][c]}) })
				rs := r.a.itlTracker.Readings(r.a.itlWindowKey("test-ns", "test-model", "v", "H200", 1))
				src := rs.Steady
				if len(src) < itl.DefaultMinSamples {
					src = rs.All
				}
				src = src[max(0, len(src)-itl.DefaultMinSamples):]
				least := itl.Observation{ITLSec: math.Inf(1)}
				for _, o := range src {
					if o.ITLSec < least.ITLSec {
						least = o
					}
				}
				lst := itl.Model{A: max(0, (least.ITLSec-itl.DefaultBPrior)/least.K), B: itl.DefaultBPrior}
				em := 100 * (on.ITLAt(oneSidedK) - truth) / truth
				el := 100 * (lst.ITLAt(oneSidedK) - truth) / truth
				sumMean, sumLeast = sumMean+em, sumLeast+el
				worstMean, worstLeast = min(worstMean, em), min(worstLeast, el)
			}
			t.Logf("%s: mean point mean %+.2f %% worst %+.2f %%; least reading mean %+.2f %% worst %+.2f %%",
				l, sumMean/seeds, worstMean, sumLeast/seeds, worstLeast)
			require.GreaterOrEqual(t, worstMean, noiseWorstGate, "the analyzer's line past the worst-seed gate")
			require.GreaterOrEqual(t, sumMean/seeds, noiseMeanGate, "the analyzer's line past the mean gate")
			require.False(t, sumLeast/seeds >= noiseMeanGate && worstLeast >= noiseWorstGate,
				"the least-reading line passes the gates (mean %+.2f %%, worst %+.2f %%): they test nothing", sumLeast/seeds, worstLeast)
		})
	}
}

// The prior reads the line published for the variant's own physics key
// (itlPhysicsKey: the model, accelerator, GPUs and engine fingerprint, no
// namespace; with enableLearnedStateReuse, which shares lines), as well as
// the tracker's keys in scope. A variant of another
// namespace on the same engine is out of the tracker scope (its role is not
// known here), but publishes its learned line under the same physics key:
// that 45 ms line raises the prior. Published each cycle, as that variant's
// own cycle would, since v republishes its own line on every fit. One
// published through the default B is no learned floor and leaves the floor.
// The truth's least at-cap ITL (78 ms) is above 45 ms, so the window's cap
// does not hide the raise.
func TestITLOneSided_APublishedLineOnThePhysicsKeyRaisesThePrior(t *testing.T) {
	l := capTruth{0.05, 0.030, 0.97}
	truth := l.itl(oneSidedK)
	run := func(b float64, accel string) float64 {
		r := oneSidedRig(t, nil)
		key := r.a.itlPhysicsKey("test-ns", "test-model", "v", accel, 1,
			r.a.engineFingerprint("test-ns", "test-model", "v"))
		require.NotEqual(t, r.a.itlWindowKey("test-ns", "test-model", "v", "H200", 1), key,
			"premise: the physics key is a shared one")
		var m itl.Model
		for range 24 {
			if b > 0 {
				r.a.publishLineFor(key, "other-ns|test-model|v|H200|1", itl.Model{A: 0.01, B: b}, r.now, false)
			}
			ctx, logs := observedCtx(t)
			in := makeAnalyzerInput(atCapRows(l, [3]float64{}), decodeVariants("v"))
			in.Config.(*config.ScalingPolicy).EnableLearnedStateReuse = true
			r.tr.Observe(settleModel("test-ns", "test-model"), settlerow.Of(in.ReplicaMetrics, nil), r.now)
			_, err := r.a.Analyze(ctx, in)
			require.NoError(t, err)
			r.now = r.now.Add(restartCycle)
			if fits := logs.FilterMessage("itl-fit").All(); len(fits) > 0 {
				f := fits[len(fits)-1].ContextMap()
				m = itl.Model{A: f["a"].(float64), B: f["b"].(float64)}
			}
		}
		return 100 * (m.ITLAt(oneSidedK) - truth) / truth
	}
	floor, raised, def := run(0, "H200"), run(0.045, "H200"), run(itl.DefaultBaselineSec, "H200")
	// A line published for another physics key, here another accelerator,
	// is out of the prior's scope.
	other := run(0.045, "H100")
	t.Logf("floor %+.2f %%, published 45 ms %+.2f %% (bound %+.2f %%), at the default B %+.2f %%, on another physics key %+.2f %%",
		floor, raised, priorBound(l, 0.045, 0), def, other)
	require.InDelta(t, floor, other, 1e-9, "a line published on another physics key moved the prior")
	require.Greater(t, raised, floor+1, "a published 45 ms line did not raise the prior")
	require.LessOrEqual(t, raised, priorBound(l, 0.045, 0)+0.05, "the raised prior over its bound")
	require.InDelta(t, floor, def, 1e-9, "a line through the default B moved the prior")
}

// The rule lasts while the key has no learned B, both ways, in one run.
// Unlearned, the capped replicas alone price one-sided; once replicas
// stepping through the band teach B by a full fit, the pinned fit through it
// prices the same capped readings; once the variant is forgotten
// (ForgetVariant), B is unlearned again and the one-sided price comes back.
//
// The truth's B (20 ms) is under the prior, a sibling's learned 30 ms, so
// the prior's line is above the truth and a raise after B is learned shows.
func TestITLOneSided_ALearnedBEndsItAndAForgetBringsItBack(t *testing.T) {
	l := capTruth{0.05, 0.020, 0.97}
	r := oneSidedRig(t, nil)
	learnedPrior(0.030)(r)
	key := r.a.itlWindowKey("test-ns", "test-model", "v", "H200", 1)
	capOnly := func(int) []domain.ReplicaMetrics { return atCapRows(l, [3]float64{}) }
	oneSided := func(m itl.Model) bool { return m.B == itl.DefaultBaselineSec && m.ITLAt(oneSidedK) >= l.itl(oneSidedK) }

	m := lineAfter(t, r, 12, capOnly)
	_, learned := r.a.itlTracker.Baseline(key)
	require.False(t, learned, "premise: no B yet")
	require.True(t, oneSided(m), "unlearned: %+v did not price one-sided", m)

	// Two replicas stepping through the band, beside the capped ones.
	lineAfter(t, r, 64, func(c int) []domain.ReplicaMetrics {
		k := 0.25
		if (c/16)%2 == 1 {
			k = 0.65
		}
		return append(capOnly(c), itlRowOn("b0", k, l), itlRowOn("b1", 0.45, l))
	})
	b, learned := r.a.itlTracker.Baseline(key)
	require.True(t, learned && b > 0, "premise: the band's full fit learned B")
	// The band's readings age out; the capped replicas alone price again.
	m = lineAfter(t, r, 140, capOnly)
	require.NotEqual(t, itl.DefaultBaselineSec, m.B, "learned: still pinned at the default (%+v)", m)
	require.InDelta(t, b, m.B, 1e-12, "learned: the pinned fit pins the learned B")
	// Through the true B, on readings without noise, the pinned fit is the
	// true line: no one-sided raise above it.
	require.InDelta(t, l.itl(oneSidedK), m.ITLAt(oneSidedK), 1e-9, "learned: still priced one-sided")

	r.a.ForgetVariant("test-ns", "test-model", "v", "tok")
	_, learned = r.a.itlTracker.Baseline(key)
	require.False(t, learned, "premise: the forget dropped B")
	// The forget drops the capacity record, which the engine reloads from
	// the scale target on the same cycle, and the window, which fills again.
	stateCap(r)
	m = lineAfter(t, r, 24, capOnly)
	require.True(t, oneSided(m), "forgotten: %+v, the one-sided price did not come back", m)
}

// itlRowOn is a replica of v in the band at fill k on truth l.
func itlRowOn(pod string, k float64, l capTruth) domain.ReplicaMetrics {
	rm := itlSettleRow(pod, k)
	rm.AvgITL = l.itl(k)
	return rm
}

// A held slope keeps the one-sided price, and ends with the hold: after the
// capped replicas leave, the held line is the last line fitted, which
// priced one-sided at the pricing k, until the slope is older than the
// bucket retention (1 h here), counted from that fit; after that the line is
// silent. Off, the held line prices under the truth (the negative control).
func TestITLOneSided_TheHeldSlopeKeepsThePriceAndEndsWithTheHold(t *testing.T) {
	l := capTruth{0.01, 0.030, 0.97}
	truth := l.itl(oneSidedK)
	type outcome struct {
		lastFitLine, held              itl.Model
		lastFit, lastHeld, firstSilent time.Duration
	}
	run := func(switches map[string]bool) outcome {
		r := oneSidedRig(t, switches)
		h := capacity.DefaultHorizons()
		h.BucketRetention = time.Hour
		r.a.WithHorizons(h)
		t0 := r.now
		var o outcome
		lineAfter(t, r, 20, func(int) []domain.ReplicaMetrics { return atCapRows(l, [3]float64{}) })
		// Below the band: no new readings; the window ages out, the slope
		// holds, and the hold ends.
		idle := []domain.ReplicaMetrics{itlRowOn("c0", 0.05, l), itlRowOn("c1", 0.05, l), itlRowOn("c2", 0.05, l)}
		for r.now.Before(t0.Add(2 * time.Hour)) {
			at := r.now.Sub(t0)
			m, slopeHeld := lineOf(t, r, idle)
			switch {
			case !m.IsZero() && !slopeHeld:
				o.lastFitLine, o.lastFit = m, at
			case !m.IsZero():
				o.held, o.lastHeld = m, at
			case o.firstSilent == 0 && o.lastHeld > 0:
				o.firstSilent = at
			}
		}
		return o
	}
	o := run(nil)
	t.Logf("last fit %s (%.5f s at k %.2f), held to %s (%.5f s), silent from %s; truth %.5f s",
		o.lastFit, o.lastFitLine.ITLAt(oneSidedK), oneSidedK, o.lastHeld, o.held.ITLAt(oneSidedK), o.firstSilent, truth)
	require.False(t, o.held.IsZero(), "premise: the slope was held")
	require.InDelta(t, o.lastFitLine.ITLAt(oneSidedK), o.held.ITLAt(oneSidedK), 1e-12,
		"the held line does not price the last one-sided line")
	require.GreaterOrEqual(t, o.held.ITLAt(oneSidedK), truth)
	require.NotZero(t, o.firstSilent, "the hold never ended")
	require.Equal(t, restartCycle, o.firstSilent-o.lastHeld, "silent on the cycle after the last held one")
	require.True(t, o.lastHeld-o.lastFit <= time.Hour && o.firstSilent-o.lastFit > time.Hour,
		"the hold ended %s after the last fit, want just past 1h", o.firstSilent-o.lastFit)

	off := run(map[string]bool{SwitchITLOneSidedAtCap: false})
	require.Less(t, off.held.ITLAt(oneSidedK), truth, "negative control: off, the held line was not under the truth")
}

// Off, the switch is the line before ev-shindin/llm-scaling-manager#467: the capped replicas price
// through the default B, under the truth, and the analyzer says it accepts
// the name.
func TestITLOneSided_TheSwitchPicksTheLine(t *testing.T) {
	l := capTruth{0.05, 0.030, 0.97}
	capOnly := func(int) []domain.ReplicaMetrics { return atCapRows(l, [3]float64{}) }
	on := lineAfter(t, oneSidedRig(t, nil), 12, capOnly)
	off := lineAfter(t, oneSidedRig(t, map[string]bool{SwitchITLOneSidedAtCap: false}), 12, capOnly)
	require.GreaterOrEqual(t, on.ITLAt(oneSidedK), l.itl(oneSidedK))
	require.Less(t, off.ITLAt(oneSidedK), l.itl(oneSidedK))
	require.Contains(t, ReplaySwitchNames(), SwitchITLOneSidedAtCap)
	require.NoError(t, NewSaturationAnalyzer(nil).SetSwitchesForReplay(map[string]bool{SwitchITLOneSidedAtCap: true}))
}

// ITLOneSided reads back the admission the key was last priced with: the
// scoped prior (45 ms, a sibling's learned B in the physics), at the pricing
// k, and whether the line was raised. Off, the zero Admission.
func TestITLOneSided_TheWhatIfReadsTheScopedPrior(t *testing.T) {
	l := capTruth{0.05, 0.030, 0.97}
	key := "test-ns|test-model|v|H200|1"
	r := oneSidedRig(t, nil)
	learnedPrior(0.045)(r)
	lineAfter(t, r, 12, func(int) []domain.ReplicaMetrics { return atCapRows(l, [3]float64{}) })
	adm, raised := r.a.ITLOneSided(key)
	require.True(t, adm.OneSidedAboveKSat, "no admission recorded")
	require.InDelta(t, 0.045, adm.BPrior, 1e-15, "the recorded prior is not the scoped one")
	require.InDelta(t, oneSidedK, adm.OneSidedAt, 1e-15, "the recorded fill is not the pricing k")
	require.True(t, raised, "the line was raised and the record says not")

	off := oneSidedRig(t, map[string]bool{SwitchITLOneSidedAtCap: false})
	lineAfter(t, off, 12, func(int) []domain.ReplicaMetrics { return atCapRows(l, [3]float64{}) })
	adm, raised = off.a.ITLOneSided(key)
	require.Equal(t, itl.Admission{}, adm, "off: an admission recorded")
	require.False(t, raised)
}

// The record of the admission goes with its variant: EvictStaleHistory drops
// it once the variant is unseen past the variant timeout, as it drops the
// variant's itlArgs.
func TestITLOneSided_TheRecordIsEvictedWithItsVariant(t *testing.T) {
	l := capTruth{0.05, 0.030, 0.97}
	key := "test-ns|test-model|v|H200|1"
	r := oneSidedRig(t, nil)
	lineAfter(t, r, 12, func(int) []domain.ReplicaMetrics { return atCapRows(l, [3]float64{}) })
	adm, _ := r.a.ITLOneSided(key)
	require.True(t, adm.OneSidedAboveKSat, "premise: an admission recorded")
	r.a.EvictStaleHistory()
	adm, _ = r.a.ITLOneSided(key)
	require.True(t, adm.OneSidedAboveKSat, "evicted while the variant is fresh")
	r.now = r.now.Add(r.a.Horizons().VariantTimeout + time.Hour)
	r.a.EvictStaleHistory()
	adm, _ = r.a.ITLOneSided(key)
	require.False(t, adm.OneSidedAboveKSat, "kept past the variant timeout")
}

// A line priced at the prior is not published for siblings to borrow (issue
// ev-shindin/llm-scaling-manager#467): it carries the donor's at-cap readings and pricing k. With line
// sharing on, a sibling of the same engine and no readings of its own
// borrows no line while the donor is one-sided, so never the raised A; with
// the switch off, the donor publishes its plain line and the sibling borrows
// it.
func TestITLOneSided_ARaisedLineIsNotLent(t *testing.T) {
	l := capTruth{0.05, 0.030, 0.97}
	run := func(switches map[string]bool) (donor itl.Model, borrowed []map[string]any) {
		r := oneSidedRig(t, switches)
		r.a.capacityStore.Update("test-ns", "test-model", "sib", capacity.Record{GpuCount: 1, EngineParams: capParams(),
			LearnedFrom: "deployment"}, r.now)
		for range 16 {
			ctx, logs := observedCtx(t)
			in := makeAnalyzerInput(atCapRows(l, [3]float64{}), decodeVariants("v", "sib"))
			in.Config.(*config.ScalingPolicy).EnableLearnedStateReuse = true
			r.tr.Observe(settleModel("test-ns", "test-model"), settlerow.Of(in.ReplicaMetrics, nil), r.now)
			_, err := r.a.Analyze(ctx, in)
			require.NoError(t, err)
			r.now = r.now.Add(restartCycle)
			for _, e := range logs.FilterMessage("itl-fit").All() {
				f := e.ContextMap()
				if f["variant"] == "v" {
					donor = itl.Model{A: f["a"].(float64), B: f["b"].(float64)}
				}
			}
			for _, e := range logs.FilterMessage("itl-line-borrowed").All() {
				borrowed = append(borrowed, e.ContextMap())
			}
		}
		return donor, borrowed
	}
	donor, borrowed := run(nil)
	require.False(t, donor.IsZero(), "premise: the donor has a line")
	require.Empty(t, borrowed, "a sibling borrowed while the donor was priced at the prior (donor %+v)", donor)
	plain, offBorrowed := run(map[string]bool{SwitchITLOneSidedAtCap: false})
	require.NotEmpty(t, offBorrowed, "negative control: off, the sibling borrowed nothing")
	require.InDelta(t, plain.A, offBorrowed[len(offBorrowed)-1]["a"].(float64), 1e-12, "off: the sibling did not borrow the donor's plain line")
	require.Greater(t, donor.A, plain.A, "premise: on, the donor's A is raised over its plain fit")
}
