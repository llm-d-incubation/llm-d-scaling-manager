package saturation

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Issue #55: one replica's re-reads within a sample (SwitchMuRereadTakesLatest).
// The rate[1m] is read afresh every 15 s cycle, so a replica saturated for a
// minute reads about four times inside the one-minute sample spacing, each
// read with the gauges' noise. Folded by their max, the sample picks the
// noise; the latest reading does not.

// rereadsPerMinute is how many cycles read one replica within a sample: four
// 15 s cycles in the one-minute spacing.
const rereadsPerMinute = 4

// rereadFigure runs minutes of n pods through l, each pod read
// rereadsPerMinute times a minute, cycle by cycle: pod p's c-th reading of
// minute m is 1 + paths[p][rereadsPerMinute*m+c]. It returns the figure.
func rereadFigure(l muLearner, paths [][]float64, minutes int) float64 {
	for m := range minutes {
		for c := range rereadsPerMinute {
			for p, path := range paths {
				l.read(fmt.Sprintf("p%d", p), 1+path[rereadsPerMinute*m+c])
			}
		}
		l.next()
	}
	return l.figure()
}

// rereadExpectation is E[figure] over trials of minutes at n pods, each pod's
// rate 1 + AR(1) noise ns at the 15 s cycle (phi per cycle, as measured), on
// seed's stream.
func rereadExpectation(newL func() muLearner, seed uint64, n, minutes, trials int, ns arNoiseSpec) float64 {
	r := propRand(seed)
	sum := 0.0
	for range trials {
		paths := make([][]float64, n)
		for p := range paths {
			paths[p] = arNoise(r, rereadsPerMinute*minutes, ns.sigma, ns.phi)
		}
		sum += rereadFigure(newL(), paths, minutes)
	}
	return sum / float64(trials)
}

// rereadTrials is how many trials a case's expectation is over. Mu prices
// the window's lower median, whose expectation sits under the truth by
// itself (-1.8 % at 15 % noise and ten samples, the cost side, and no part
// of the fold); 400 trials keep the spread of the estimate (about 0.3 % at
// one pod) four of its widths inside propNDepBias.
const rereadTrials = 400

// checkRereadFold: n pods in {1, 4}, each read four times a minute with AR(1)
// noise ns per 15 s cycle, for ten minutes; the figure's expectation is
// within propNDepBias of the true rate 1 at each n. The property rig's
// other n-dependence cases read each pod once a minute, so they never reach
// the within-minute fold (issue #55).
func checkRereadFold(newL func() muLearner, ns arNoiseSpec) func(uint64) error {
	return func(seed uint64) error {
		for _, n := range []int{1, 4} {
			mean := rereadExpectation(newL, seed, n, 10, rereadTrials, ns)
			if !(mean > 0) {
				return violation(kindNotLearned, "N=%d: no figure", n)
			}
			if e := mean - 1; !(math.Abs(e) <= propNDepBias) {
				return violation(kindNDependent, "%s, %d reads a minute: E at N=%d is %.4f against a true 1: %+.1f%%",
					ns, rereadsPerMinute, n, mean, 100*e)
			}
		}
		return nil
	}
}

// rereadCases is the within-minute fold's property cases, for mu and for
// the prefill bound, which both write through it.
func rereadCases() []propCase {
	mu := func() muLearner { return newAnalyzerMu() }
	bound := func() muLearner { return newBoundMu(nil) }
	c := func(learner, name string, l func() muLearner, ns arNoiseSpec) propCase {
		return propCase{class: classNDependence, learner: learner, name: name, family: familyDirect,
			tuning: tunedDefault, seeds: 10, check: checkRereadFold(l, ns)}
	}
	return []propCase{
		c("mu", "ar-reread", mu, rateNoise10),
		c("mu", "ar-reread-sigma15", mu, rateNoise15),
		c("prefill-bound", "ar-reread", bound, rateNoise10),
		c("prefill-bound", "ar-reread-sigma15", bound, rateNoise15),
	}
}

// rereadMu is mu's write path with SwitchMuRereadTakesLatest set to on.
func rereadMu(on bool) func() muLearner {
	return func() muLearner {
		m := newAnalyzerMu()
		m.a.muRereadLatest = on
		return m
	}
}

// rereadBound is the prefill bound, its held confidence on, with
// SwitchMuRereadTakesLatest set to on.
func rereadBound(on bool) func() muLearner {
	return func() muLearner {
		b := newBoundMu(nil)
		b.a.muRereadLatest = on
		return b
	}
}

// The bias table at the measured noise (rateNoise10, rateNoise15: phi 0.3 per
// 15 s cycle), four reads a minute, one pod and four, after ten minutes and
// (the bound, whose held figure accumulates) an hour. On, every figure is
// within propNDepBias of the truth; off, the max over re-reads reads high at
// every point. Both directions are asserted: off is the parent's fold.
func TestRereadFold_TheBiasAtTheMeasuredNoise(t *testing.T) {
	var table strings.Builder
	table.WriteString("\n| learner | noise | pods | minutes | on (latest) | off (max) |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, l := range []struct {
		name    string
		learner func(bool) func() muLearner
		minutes []int
	}{
		{"mu", rereadMu, []int{10}},
		{"prefill bound", rereadBound, []int{10, 60}},
	} {
		for _, ns := range []arNoiseSpec{rateNoise10, rateNoise15} {
			for _, n := range []int{1, 4} {
				for _, minutes := range l.minutes {
					trials := rereadTrials
					if minutes > 10 {
						trials = rereadTrials / 4
					}
					on := rereadExpectation(l.learner(true), 1, n, minutes, trials, ns)
					off := rereadExpectation(l.learner(false), 1, n, minutes, trials, ns)
					fmt.Fprintf(&table, "| %s | %.0f %% | %d | %d | %+.1f %% | %+.1f %% |\n",
						l.name, 100*ns.sigma, n, minutes, 100*(on-1), 100*(off-1))
					require.LessOrEqual(t, math.Abs(on-1), propNDepBias, "%s, %s, N=%d, %d min: on", l.name, ns, n, minutes)
					require.Greater(t, off-1, 0.05, "%s, %s, N=%d, %d min: off, the max over re-reads reads high", l.name, ns, n, minutes)
				}
			}
		}
	}
	t.Log(table.String())
}

// The four re-read cases pass on every seed with the switch on, and fail on
// every seed with it off: off is the parent's max over re-reads.
func TestRereadFold_TheCasesFailWithTheSwitchOff(t *testing.T) {
	for _, c := range []struct {
		name  string
		check func(bool) func(uint64) error
	}{
		{"mu", func(on bool) func(uint64) error { return checkRereadFold(rereadMu(on), rateNoise10) }},
		{"mu-sigma15", func(on bool) func(uint64) error { return checkRereadFold(rereadMu(on), rateNoise15) }},
		{"prefill-bound", func(on bool) func(uint64) error { return checkRereadFold(rereadBound(on), rateNoise10) }},
		{"prefill-bound-sigma15", func(on bool) func(uint64) error { return checkRereadFold(rereadBound(on), rateNoise15) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			failed := 0
			var first error
			for seed := uint64(1); seed <= 10; seed++ {
				require.NoError(t, c.check(true)(seed), "on, seed %d", seed)
				if err := c.check(false)(seed); err != nil {
					failed++
					if first == nil {
						first = err
					}
				}
			}
			t.Logf("off: %d/10 seeds fail; first: %v", failed, first)
			require.Equal(t, 10, failed, "off, the max over re-reads must fail every seed")
		})
	}
}

// sampleOf is the newest sample of the analyzerMu rig's window.
func (m *analyzerMu) sampleOf() float64 {
	m.a.mu.Lock()
	defer m.a.mu.Unlock()
	return m.a.saturatedThroughput[propMuKey].Last()
}

// What the max was kept for: a replica that fills during the minute reads
// low first, while its rate window still holds unsaturated time, and higher
// as the minute goes on. The latest reading is the fullest window, so the
// sample is the max either way; a mean of the re-reads (0.7875 here) would
// count the partial windows. Where a later reading is lower, the sample
// follows it with the switch on, and keeps the earlier noise off.
func TestRereadFold_AFillKeepsTheFullestWindow(t *testing.T) {
	for _, c := range []struct {
		name  string
		reads []float64
		on    float64
		off   float64
	}{
		{"filling", []float64{0.5, 0.75, 0.9, 1.0}, 1.0, 1.0},
		{"noise above, then the rate", []float64{1.0, 1.2, 0.95, 1.0}, 1.0, 1.2},
	} {
		for _, on := range []bool{true, false} {
			m := rereadMu(on)().(*analyzerMu)
			for _, v := range c.reads {
				m.read("p0", v)
			}
			want := c.off
			if on {
				want = c.on
			}
			require.InDelta(t, want, m.sampleOf(), 1e-12, "%s, switch on %v", c.name, on)
		}
	}
	// Across replicas the fold is still the mean of each one's latest.
	m := rereadMu(true)().(*analyzerMu)
	for _, r := range [][2]float64{{0.5, 2.0}, {0.9, 1.6}, {1.0, 1.0}} {
		m.read("p0", r[0])
		m.read("p1", r[1])
	}
	require.InDelta(t, 1.0, m.sampleOf(), 1e-12, "the mean of p0's 1.0 and p1's 1.0")
}

// A sample restored with no per-pod readings counts its value as the reading
// of the pod that reads next: on, that pod's reading replaces it; off, it
// raised it.
func TestRereadFold_ARestoredSampleIsReplacedByTheNextReading(t *testing.T) {
	for _, on := range []bool{true, false} {
		m := rereadMu(on)().(*analyzerMu)
		m.read("p0", 1.3)
		m.a.mu.Lock()
		delete(m.a.throughputPodReads, propMuKey) // a restore carries the sample, not its readings
		m.a.mu.Unlock()
		m.read("p0", 1.0)
		want := 1.3
		if on {
			want = 1.0
		}
		require.InDelta(t, want, m.sampleOf(), 1e-12, "switch on %v", on)
	}
}
