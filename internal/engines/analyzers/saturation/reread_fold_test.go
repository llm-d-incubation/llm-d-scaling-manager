package saturation

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
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

// readWith is one reading of pod through the rig's write path with the row's
// evidence of the moment it was read, and the clock moved on a second.
func (m *analyzerMu) readWith(pod string, rate float64, ev readEvidence) muWrite {
	w := m.a.recordSaturatedReading(propMuKey, pod, rate, ev)
	m.clock = m.clock.Add(time.Second)
	return w
}

// running is a row whose running batch was read: n requests.
func running(n int) readEvidence { return readEvidence{running: n, runningKnown: true} }

// queueMin is a row whose least queue over the minute was read: q requests.
func queueMin(q float64) readEvidence { return readEvidence{queueMin: q, queueMinKnown: true} }

// The way OUT of saturation, which the latest reading alone took and the max
// never did: the queue drains, the 1m-max queue gate still admits the
// replica, and its rate[1m] falls as it covers time served under capacity.
// On pd-restart 12:28:02 a decode replica read [6.23, 6.235, 5.99, 4.91]
// as its batch fell from 16 running to 9 and 5, and the latest held mu at
// 5.06 against a settled 6.2 for 17 min. A re-read counts only while the row
// says the replica is still saturated, so the sample is the last such
// reading. With no evidence on the row the latest is taken: pinned, since
// the collector reads the running batch for every vLLM and SGLang replica.
func TestRereadFold_AnExitKeepsTheLastSaturatedReading(t *testing.T) {
	reads := []float64{1.0, 1.0, 0.8, 0.5}
	for _, c := range []struct {
		name string
		ev   []readEvidence
		want float64
	}{
		{"running batch falls 16 -> 9 -> 5", []readEvidence{running(16), running(16), running(9), running(5)}, 1.0},
		{"queue emptied (QueueMin 0)", []readEvidence{queueMin(40), queueMin(30), queueMin(0), queueMin(0)}, 1.0},
		{"QueueMin wins over a full batch", []readEvidence{
			{queueMin: 40, queueMinKnown: true, running: 16, runningKnown: true},
			{queueMin: 30, queueMinKnown: true, running: 16, runningKnown: true},
			{queueMin: 0, queueMinKnown: true, running: 16, runningKnown: true},
			{queueMin: 0, queueMinKnown: true, running: 16, runningKnown: true}}, 1.0},
		{"no evidence: the latest", make([]readEvidence, 4), 0.5},
	} {
		m := rereadMu(true)().(*analyzerMu)
		for i, v := range reads {
			m.readWith("p0", v, c.ev[i])
		}
		require.InDelta(t, c.want, m.sampleOf(), 1e-12, c.name)
	}
	// The recorded trace itself.
	m := rereadMu(true)().(*analyzerMu)
	for i, v := range []float64{6.23, 6.235, 5.99, 4.91} {
		m.readWith("p0", v, running([]int{16, 16, 9, 5}[i]))
	}
	require.InDelta(t, 6.235, m.sampleOf(), 1e-12, "pd-restart 12:28:02: the last reading with a full batch")
}

// A low latest reading after a full one is dropped only by the replica's
// state, never by its value: with the batch still full it is a real
// reading of the rate and counts; with the batch emptied it does not.
func TestRereadFold_APartialLowLatestAfterAFullRead(t *testing.T) {
	m := rereadMu(true)().(*analyzerMu)
	m.readWith("p0", 1.0, running(16))
	m.readWith("p0", 0.6, running(4))
	require.InDelta(t, 1.0, m.sampleOf(), 1e-12, "the batch emptied: the full read stands")
	m = rereadMu(true)().(*analyzerMu)
	m.readWith("p0", 1.0, running(16))
	m.readWith("p0", 0.6, running(16))
	require.InDelta(t, 0.6, m.sampleOf(), 1e-12, "the batch still full: the low read counts")
	m = rereadMu(true)().(*analyzerMu)
	m.readWith("p0", 1.0, running(16))
	m.readWith("p0", 0.6, running(4))
	m.readWith("p0", 0.95, running(16))
	require.InDelta(t, 0.95, m.sampleOf(), 1e-12, "full again: the latest counted reading")
}

// A replica restarted under the same name within the minute reads its first,
// partial window with an empty batch: it does not count, and the reading
// before the restart stands. With no reading of the sample counted, the
// sample is the replica's highest, the fold as it was before.
func TestRereadFold_ARestartMidMinute(t *testing.T) {
	m := rereadMu(true)().(*analyzerMu)
	m.readWith("p0", 1.0, running(16))
	m.readWith("p0", 1.0, running(16))
	m.readWith("p0", 0.3, running(1))
	require.InDelta(t, 1.0, m.sampleOf(), 1e-12, "the restart's partial window does not count")

	m = rereadMu(true)().(*analyzerMu)
	for _, v := range []float64{0.9, 1.1, 1.0} {
		m.readWith("p0", v, queueMin(0))
	}
	require.InDelta(t, 1.1, m.sampleOf(), 1e-12, "none counted: the max, as before")
}

// A sample restored with no per-pod readings, then read by two replicas
// within the minute, is the mean of their readings: the restored value is no
// replica's, and leaves no residue in the mean or in a replica's highest.
func TestRereadFold_ATwoPodRestoredSample(t *testing.T) {
	for _, ev := range []readEvidence{{}, running(16)} {
		m := rereadMu(true)().(*analyzerMu)
		m.readWith("p0", 1.3, ev)
		m.a.mu.Lock()
		delete(m.a.throughputPodReads, propMuKey) // a restore carries the sample, not its readings
		m.a.mu.Unlock()
		m.readWith("p0", 1.0, ev)
		m.readWith("p1", 0.8, ev)
		require.InDelta(t, 0.9, m.sampleOf(), 1e-12, "mean of p0 and p1 (evidence %+v)", ev)
		m.a.mu.Lock()
		peak := m.a.throughputPodReads[propMuKey].peaks["p0"]
		m.a.mu.Unlock()
		require.InDelta(t, 1.0, peak, 1e-12, "p0's highest is its own reading, not the restored 1.3")
	}
}

// Open: a drain burst at the end of an episode. Where the row says the queue
// emptied over the minute (QueueMin 0) the burst does not count; with only a
// full running batch, or no evidence, it is the latest reading and is taken,
// as the max took it (throughput_floor.go, file header).
func TestRereadFold_ADrainAtTheEndIsStillTaken(t *testing.T) {
	m := rereadMu(true)().(*analyzerMu)
	for i, v := range []float64{1.0, 1.0, 1.4} {
		m.readWith("p0", v, []readEvidence{queueMin(20), queueMin(10), queueMin(0)}[i])
	}
	require.InDelta(t, 1.0, m.sampleOf(), 1e-12, "QueueMin 0: the burst does not count")
	m = rereadMu(true)().(*analyzerMu)
	for _, v := range []float64{1.0, 1.0, 1.4} {
		m.readWith("p0", v, running(16))
	}
	require.InDelta(t, 1.4, m.sampleOf(), 1e-12, "a full batch through the burst: taken (open)")
}

// The fold is in processing order, which is cycle order: one reading per
// replica per cycle. A duplicate read inside the spacing (the same scrape
// pair read again) folds and changes nothing; a reading lower than the one
// before it, read later, is the latest whatever its value.
func TestRereadFold_ADuplicateOrStaleReadInsideTheSpacing(t *testing.T) {
	m := rereadMu(true)().(*analyzerMu)
	require.Equal(t, muSampled, m.readWith("p0", 1.0, running(16)))
	require.Equal(t, muFolded, m.readWith("p0", 1.0, running(16)), "a duplicate folds")
	require.InDelta(t, 1.0, m.sampleOf(), 1e-12)
	m.a.mu.Lock()
	require.Equal(t, 1, m.a.saturatedThroughput[propMuKey].Len(), "and starts no sample")
	m.a.mu.Unlock()
	m.readWith("p0", 1.1, running(16))
	m.readWith("p0", 0.9, running(16))
	require.InDelta(t, 0.9, m.sampleOf(), 1e-12, "the later reading, read later")
}

// The spacing's boundary: a reading at exactly ThroughputSampleSpacing after
// the sample started starts the next one; a nanosecond before, it folds.
func TestRereadFold_TheBoundaryAtExactlyTheSpacing(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	now := propStart
	a.now = func() time.Time { return now }
	require.Equal(t, muSampled, a.recordSaturatedReading(propMuKey, "p0", 1.0, running(16)))
	now = propStart.Add(ThroughputSampleSpacing - time.Nanosecond)
	require.Equal(t, muFolded, a.recordSaturatedReading(propMuKey, "p0", 1.2, running(16)))
	now = propStart.Add(ThroughputSampleSpacing)
	require.Equal(t, muSampled, a.recordSaturatedReading(propMuKey, "p0", 0.9, running(16)))
	a.mu.Lock()
	defer a.mu.Unlock()
	ra := a.saturatedThroughput[propMuKey]
	require.Equal(t, 2, ra.Len())
	require.Equal(t, []float64{1.2, 0.9}, ra.Snapshot().Values)
}
