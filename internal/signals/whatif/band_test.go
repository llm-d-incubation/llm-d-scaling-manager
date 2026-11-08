package whatif

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// readingsOn is n cycles of two replicas' readings on the line, k cycling
// over [lo, hi], with noise that alternates in sign so each resample moves
// the fit. Every reading is steady: the main and the steady window hold the
// same ones.
func readingsOn(line itl.Model, n int, lo, hi, noise float64) itl.KeyReadings {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	out := make([]itl.Observation, 0, 2*n)
	for c := range n {
		at := t0.Add(time.Duration(c) * 15 * time.Second)
		for r, f := range []float64{float64(c%7) / 6, float64((c+3)%7) / 6} {
			k := lo + (hi-lo)*f
			sign := 1.0
			if (c+r)%2 == 1 {
				sign = -1
			}
			out = append(out, itl.Observation{K: k, ITLSec: line.ITLAt(k) * (1 + sign*noise), Timestamp: at})
		}
	}
	return itl.KeyReadings{All: out, Steady: out}
}

var worked = itl.Model{A: 0.004438, B: 0.001557}

func TestDefaultResamplingIsThePages(t *testing.T) {
	if r := DefaultResampling(); r != (Resampling{Block: 4, Resamples: 200, Seed: 1}) {
		t.Errorf("%+v, want blocks of 4 cycles, 200 resamples, seed 1", r)
	}
}

func TestBootstrapLinesIsReproducibleAndMoves(t *testing.T) {
	rs := readingsOn(worked, 40, 0.2, 0.7, 0.05)
	fit := Refit{Tier: TierOLS, Line: worked, Baseline: worked.B}
	a := BootstrapLines(rs, fit, DefaultResampling())
	b := BootstrapLines(rs, fit, DefaultResampling())
	if a.Drawn != 200 || len(a.Lines) != 200 {
		t.Fatalf("drew %d, fitted %d; want 200 of each", a.Drawn, len(a.Lines))
	}
	distinct := map[itl.Model]bool{}
	for i := range a.Lines {
		if a.Lines[i] != b.Lines[i] {
			t.Fatalf("resample %d: %+v then %+v from the same seed", i, a.Lines[i], b.Lines[i])
		}
		distinct[a.Lines[i].Line] = true
	}
	if len(distinct) < 50 {
		t.Errorf("only %d distinct lines in 200 resamples", len(distinct))
	}
	other := BootstrapLines(rs, fit, Resampling{Block: 4, Resamples: 200, Seed: 2})
	if other.Lines[0] == a.Lines[0] && other.Lines[1] == a.Lines[1] {
		t.Errorf("seed 2 drew what seed 1 did")
	}
}

// A resample keeps whole cycles: a cycle's main and steady readings, of
// every replica, come together.
func TestBootstrapKeepsCyclesWhole(t *testing.T) {
	rs := readingsOn(worked, 9, 0.2, 0.7, 0)
	cycles := cyclesOf(rs.All, rs.Steady)
	if len(cycles) != 9 {
		t.Fatalf("%d cycles, want 9", len(cycles))
	}
	for i, c := range cycles {
		if len(c.all) != 2 || len(c.steady) != 2 {
			t.Errorf("cycle %d holds %d and %d readings, want both replicas' in both windows", i, len(c.all), len(c.steady))
		}
	}
}

func TestBootstrapHasNothingToResample(t *testing.T) {
	rs := readingsOn(worked, 40, 0.2, 0.7, 0.05)
	cases := map[string]LineResamples{
		"one cycle":      BootstrapLines(readingsOn(worked, 1, 0.2, 0.7, 0.05), Refit{Tier: TierOLS, Line: worked, Baseline: worked.B}, DefaultResampling()),
		"a held line":    BootstrapLines(rs, Refit{Tier: TierHeld, Line: worked, Baseline: worked.B}, DefaultResampling()),
		"a borrowed one": BootstrapLines(rs, Refit{Tier: TierBorrowed, Line: worked}, DefaultResampling()),
		"no resamples":   BootstrapLines(rs, Refit{Tier: TierOLS, Line: worked}, Resampling{Block: 4}),
	}
	for name, got := range cases {
		if got.Drawn != 0 || len(got.Lines) != 0 {
			t.Errorf("%s: drew %d, fitted %d; want nothing", name, got.Drawn, len(got.Lines))
		}
	}
	if got := BandAnswer(Answer{Value: 1}, LineResamples{}, func(itl.Model) (float64, bool) { return 1, true }); got.Band != nil {
		t.Errorf("banded from no resamples: %+v", got.Band)
	}
}

// The tracker's readiness gates the full fit on a resample, as it gated the
// line: steady readings spread less than itl.DefaultMinKSpread fit pinned-B,
// though itl.Fit alone would take them. An ols line on such readings (one
// fitted when the window was wider) bands as TierUnstable.
func TestBootstrapRerunsTheReadinessGate(t *testing.T) {
	rs := readingsOn(worked, 40, 0.40, 0.65, 0.05)
	if _, ok := itl.Fit(rs.Steady); !ok {
		t.Fatal("the full fit refuses the readings; the case tests nothing")
	}
	got := BootstrapLines(rs, Refit{Tier: TierOLS, Line: worked, Baseline: worked.B}, DefaultResampling())
	if len(got.Lines) != 200 {
		t.Fatalf("fitted %d of %d", len(got.Lines), got.Drawn)
	}
	for _, s := range got.Lines {
		if s.Tier != TierPinnedB || !s.TierChanged || s.Line.B != worked.B {
			t.Fatalf("resample %+v: want a pinned-B fit at the baseline, a change of tier", s)
		}
	}
	a := BandAnswer(Answer{Value: 1}, got, func(m itl.Model) (float64, bool) { return m.ITLAt(0.5), true })
	if !a.HasFlag(TierUnstable) {
		t.Errorf("flags %v, want TierUnstable", a.Flags)
	}
}

// A change of tier counts both ways: a pinned-B line whose readings are
// ready refits ols on the resamples, and that is a change too.
func TestBootstrapCountsAChangeToOLS(t *testing.T) {
	rs := readingsOn(worked, 40, 0.2, 0.7, 0.05)
	got := BootstrapLines(rs, Refit{Tier: TierPinnedB, Line: worked, Baseline: worked.B}, DefaultResampling())
	changed := 0
	for _, s := range got.Lines {
		if s.TierChanged {
			if s.Tier != TierOLS {
				t.Fatalf("resample %+v: a change that is not to ols", s)
			}
			changed++
		}
	}
	if changed < 180 {
		t.Errorf("%d of %d resamples changed to ols, want nearly all", changed, len(got.Lines))
	}
}

// Readings too few to fit, on every resample, are where the tracker holds
// the key's slope: the resample is priced at the held A, at the baseline,
// and has changed tier.
func TestBootstrapHoldsWhereTheTrackerHolds(t *testing.T) {
	rs := readingsOn(worked, 4, 0.2, 0.7, 0.05) // 8 readings, under DefaultMinSamples
	got := BootstrapLines(rs, Refit{Tier: TierPinnedB, Line: worked, Baseline: worked.B}, DefaultResampling())
	if len(got.Lines) != 200 {
		t.Fatalf("fitted %d of %d", len(got.Lines), got.Drawn)
	}
	for _, s := range got.Lines {
		if s.Tier != TierHeld || !s.TierChanged || s.Line != worked {
			t.Fatalf("resample %+v: want the held line, a change of tier", s)
		}
	}
}

// An above-band line resamples the above-band windows, by the fallback's
// own fit, and stays above-band.
func TestBootstrapAboveBandOnItsOwnReadings(t *testing.T) {
	line := itl.Model{A: 0.05, B: 0.01}
	above := readingsOn(line, 20, 0.86, 0.95, 0.02)
	rs := itl.KeyReadings{Above: above.All, AboveSteady: above.Steady}
	got := BootstrapLines(rs, Refit{Tier: TierAboveBand, Line: line}, DefaultResampling())
	if len(got.Lines) != 200 {
		t.Fatalf("fitted %d of %d", len(got.Lines), got.Drawn)
	}
	for _, s := range got.Lines {
		if s.Tier != TierAboveBand || s.TierChanged {
			t.Fatalf("resample %+v: want an above-band fit", s)
		}
	}
	if none := BootstrapLines(itl.KeyReadings{All: above.All, Steady: above.Steady}, Refit{Tier: TierAboveBand, Line: line}, DefaultResampling()); none.Drawn != 0 {
		t.Errorf("an above-band line resampled the band's windows: drew %d", none.Drawn)
	}
}

// The band is the 10th to the 90th percentile by nearest rank, skips a
// line that refuses or prices a non-finite figure, and leaves the answer's
// own flags alone.
func TestBandAnswerPercentiles(t *testing.T) {
	var rs LineResamples
	for i := 1; i <= 100; i++ {
		rs.Lines = append(rs.Lines, Resample{Line: itl.Model{A: float64(i)}})
	}
	rs.Lines = append(rs.Lines, Resample{Line: itl.Model{A: -1}}, Resample{Line: itl.Model{A: -2}})
	rs.Drawn = len(rs.Lines)
	a := Answer{Value: 50, Flags: []Flag{AssumedShape}}
	got := BandAnswer(a, rs, func(m itl.Model) (float64, bool) {
		switch m.A {
		case -1:
			return 0, false
		case -2:
			return math.NaN(), true
		}
		return m.A, true
	})
	if got.Band == nil || got.Band.Lo != 10 || got.Band.Hi != 90 {
		t.Errorf("band %+v, want [10, 90]", got.Band)
	}
	if got.HasFlag(TierUnstable) || len(got.Flags) != 1 || len(a.Flags) != 1 {
		t.Errorf("flags %v (the answer's own: %v), want only AssumedShape", got.Flags, a.Flags)
	}
}

// tierCase is 200 resamples, changed of them at another tier, each pricing
// figure 1 when unchanged and moved when changed.
func tierCase(changed int, moved float64) Answer {
	rs := LineResamples{Drawn: 200}
	for i := range 200 {
		rs.Lines = append(rs.Lines, Resample{Line: itl.Model{A: 1}, TierChanged: i < changed})
		if i < changed {
			rs.Lines[i].Line.A = moved
		}
	}
	return BandAnswer(Answer{Value: 1}, rs, func(m itl.Model) (float64, bool) { return m.A, true })
}

// Above a tenth of the resamples changing tier, and only above it, the
// answer is flagged; whether TierUnstable or LineNearGate turns on whether
// the changed resamples' median figure lies more than 5% from the answer.
func TestTierChangeFlags(t *testing.T) {
	for _, c := range []struct {
		changed  int
		moved    float64
		unstable bool
		nearGate bool
	}{
		{20, 2, false, false},     // a tenth exactly: no flag, whatever the move
		{21, 2, true, false},      // the change moves the figure 100%
		{21, 1.06, true, false},   // 6%: moves it
		{21, 1.04, false, true},   // 4%: near the gate only
		{21, 0.94, true, false},   // 6% the other way
		{200, 1.04, false, true},  // every resample changed, and the figure did not move
		{200, 1.051, true, false}, // just past 5%
	} {
		got := tierCase(c.changed, c.moved)
		if got.HasFlag(TierUnstable) != c.unstable || got.HasFlag(LineNearGate) != c.nearGate {
			t.Errorf("%d changed, figure %g: flags %v, want TierUnstable %v, LineNearGate %v",
				c.changed, c.moved, got.Flags, c.unstable, c.nearGate)
		}
	}
}

// A resample that fitted no line has changed tier: it counts.
func TestTierChangesCountsWhatFittedNothing(t *testing.T) {
	rs := LineResamples{Drawn: 10}
	for range 5 {
		rs.Lines = append(rs.Lines, Resample{Line: itl.Model{A: 1}})
	}
	if got := rs.TierChanges(); got != 5 {
		t.Errorf("10 drawn, 5 fitted unchanged: %d changes, want the 5 that fitted nothing", got)
	}
	rs.Lines[0].TierChanged = true
	if got := rs.TierChanges(); got != 6 {
		t.Errorf("one fitted at another tier too: %d, want 6", got)
	}
}

// Every resample holds exactly the steady readings the steady window does,
// and the cycles without steady readings come back in as many readings, so
// a main window larger than the steady one stays larger by the same count.
func TestBootstrapKeepsTheSteadyCount(t *testing.T) {
	steady := readingsOn(worked, 15, 0.2, 0.52, 0.05)
	// Unsteady readings in cycles of their own, after the steady ones: the
	// main window holds 16 more readings than the steady window.
	extra := readingsOn(worked, 8, 0.3, 0.4, 0.08)
	for i := range extra.All {
		extra.All[i].Timestamp = extra.All[i].Timestamp.Add(time.Hour)
	}
	rs := itl.KeyReadings{All: append(append([]itl.Observation{}, steady.All...), extra.All...), Steady: steady.Steady}
	if len(rs.All) <= len(rs.Steady) {
		t.Fatalf("main %d, steady %d: the case tests nothing", len(rs.All), len(rs.Steady))
	}
	r := newResampler(rs.All, rs.Steady, 4)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 200 {
		all, st := r.draw(rng)
		if len(st) != len(rs.Steady) || len(all) != len(rs.All) {
			t.Fatalf("draw %d: main %d, steady %d; want %d and %d", i, len(all), len(st), len(rs.All), len(rs.Steady))
		}
		for j := 1; j < len(all); j++ {
			if all[j].Timestamp.Before(all[j-1].Timestamp) {
				t.Fatalf("draw %d: the main window is out of time order at %d", i, j)
			}
		}
		// Stamped again in order, the cycles must be in the order they were
		// read in too, or the stamps order a shuffle.
		for j := 1; j < len(r.picked); j++ {
			if r.picked[j].at.Before(r.picked[j-1].at) {
				t.Fatalf("draw %d: cycle %d was read before cycle %d", i, j, j-1)
			}
		}
	}
	got := BootstrapLines(rs, Refit{Tier: TierOLS, Line: worked, Baseline: worked.B}, DefaultResampling())
	if got.Drawn != 200 || len(got.Lines) != 200 {
		t.Errorf("drew %d, fitted %d", got.Drawn, len(got.Lines))
	}
}

// The tier reaches every Basis, and the admission chain's too.
func TestTierReachesTheBasis(t *testing.T) {
	d := olsSweep()
	d.Tier = TierOLS
	slo := SLOAtReplicas(d, ReplicasQuery{Headroom: 0.85})
	for name, r := range map[string]Result{"knee": slo.KneeRate, "rate": slo.Rate, "mean": slo.MeanTPOT,
		"question 1": rateFor(d, TPOTTarget{Seconds: 0.004}, 0.85), "fill": FillAtRate(d, 5)} {
		if got := mustAnswer(t, r).Basis.Tier; got != TierOLS {
			t.Errorf("%s: tier %q", name, got)
		}
	}
	adm, ok := DecodeAdmission(d, 5).(Admission)
	if !ok || adm.Basis.Tier != TierOLS {
		t.Errorf("admission %+v: want tier ols", adm)
	}
}

// PinnedDefaultB refuses a line whose slope was fitted against the default
// intercept -- pinned-B, held, or borrowed from a donor that was -- within a
// tolerance, and nothing else: an ols line that lands on it is a fit, and an
// above-band line pins its own least ITL.
func TestPinnedDefaultB(t *testing.T) {
	def := itl.DefaultBaselineSec
	for _, c := range []struct {
		tier   string
		b      float64
		refuse bool
	}{
		{TierPinnedB, def, true},
		{TierHeld, def, true},
		{TierBorrowed, def, true},
		{TierPinnedB, def * (1 + 1e-12), true},
		{TierPinnedB, def * (1 + 1e-6), false},
		{TierPinnedB, 0.0015, false},
		{TierOLS, def, false},
		{TierAboveBand, def, false},
	} {
		d := olsSweep()
		d.Tier, d.Line.B = c.tier, c.b
		u, refused := KneeRate(d).(Unanswerable)
		if refused != c.refuse || refused && u.Reason != PinnedDefaultB {
			t.Errorf("%s at B %.15g: %#v, want refused %v", c.tier, c.b, KneeRate(d), c.refuse)
		}
	}
}
