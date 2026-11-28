package whatif

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// A pinned-B line's resamples run through their own intercept (issue
// llm-d-incubation/llm-d-scaling-manager#307): B varies, never under
// minDrawnB, with median the pinned B.
func TestPinnedResamplesDrawB(t *testing.T) {
	rs := readingsOn(worked, 40, 0.40, 0.65, 0.05)
	got := BootstrapLines(rs, Refit{Tier: TierPinnedB, Line: worked, Baseline: worked.B, DrawB: true}, DefaultResampling())
	bs := map[float64]bool{}
	all := make([]float64, 0, len(got.Lines))
	for _, s := range got.Lines {
		if s.Tier != TierPinnedB || s.TierChanged {
			t.Fatalf("resample %+v: want pinned-B, no change of tier", s)
		}
		if s.Line.B < minDrawnB {
			t.Fatalf("resample B %.6f under %.6f", s.Line.B, minDrawnB)
		}
		bs[s.Line.B] = true
		all = append(all, s.Line.B)
	}
	if len(bs) < 20 {
		t.Errorf("%d distinct B in %d resamples, want the intercept drawn", len(bs), len(got.Lines))
	}
	if lo, hi := splitAt(all, worked.B); 2*lo < len(all) || 2*hi < len(all) {
		t.Errorf("%d drawn B at or under the pinned %.9f and %d at or over it, of %d: want it their median", lo, worked.B, hi, len(all))
	}
}

// With the draw off (Refit.DrawB unset, saturation.SwitchITLBandIntercept
// off) the band is the one before issue
// llm-d-incubation/llm-d-scaling-manager#307: every pinned resample keeps
// the baseline, no blocks are counted, and the band is the 10th to the
// 90th percentile of the figures, neither stretched to its point nor
// widened.
func TestBandInterceptOffIsTheBandBefore(t *testing.T) {
	rs := readingsOn(worked, 40, 0.40, 0.65, 0.05)
	got := BootstrapLines(rs, Refit{Tier: TierPinnedB, Line: worked, Baseline: worked.B, BelowBandITL: worked.B * 0.8}, DefaultResampling())
	if got.Blocks != 0 {
		t.Errorf("blocks %d with the draw off, want 0", got.Blocks)
	}
	vals := make([]float64, 0, len(got.Lines))
	for _, s := range got.Lines {
		if s.Line.B != worked.B {
			t.Fatalf("resample B %.9f with the draw off, want the baseline %.9f", s.Line.B, worked.B)
		}
		vals = append(vals, s.Line.ITLAt(0.85))
	}
	slices.Sort(vals)
	at := func(p float64) float64 { return vals[max(0, int(math.Ceil(p*float64(len(vals))))-1)] }
	// A point far above every resample: the band before stays where the
	// resamples put it.
	point := 2 * vals[len(vals)-1]
	a := BandAnswer(Answer{Value: point}, got, func(m itl.Model) (float64, bool) { return m.ITLAt(0.85), true })
	if a.Band == nil || a.Band.Lo != at(bandLo) || a.Band.Hi != at(bandHi) || a.Band.Blocks != 0 {
		t.Errorf("band %+v, want the percentiles [%v, %v] untouched", a.Band, at(bandLo), at(bandHi))
	}
}

// A stated B is an assumption: its resamples keep it.
func TestStatedBIsHeld(t *testing.T) {
	rs := readingsOn(worked, 40, 0.40, 0.65, 0.05)
	got := BootstrapLines(rs, Refit{Tier: TierPinnedB, Line: worked, Baseline: worked.B, HoldB: true, DrawB: true}, DefaultResampling())
	for _, s := range got.Lines {
		if s.Line.B != worked.B {
			t.Fatalf("resample B %.6f, want the stated %.6f", s.Line.B, worked.B)
		}
	}
}

// The floor below the band caps a drawn B.
func TestDrawnBUnderTheFloor(t *testing.T) {
	rs := readingsOn(worked, 40, 0.40, 0.65, 0.05)
	floor := worked.B * 1.05
	got := BootstrapLines(rs, Refit{Tier: TierPinnedB, Line: worked, Baseline: worked.B, BelowBandITL: floor, DrawB: true}, DefaultResampling())
	at := 0
	for _, s := range got.Lines {
		if s.Line.B > floor {
			t.Fatalf("resample B %.6f above the floor %.6f", s.Line.B, floor)
		}
		if s.Line.B == floor {
			at++
		}
	}
	if at == 0 {
		t.Errorf("no resample reached the floor; the case tests nothing")
	}
}

// A floor read under the pinned B does not pull the draw under the line:
// the ceiling is never under the pinned B, so the draw keeps its median
// there and spreads below it only.
func TestFloorUnderThePinnedBKeepsTheDrawCentred(t *testing.T) {
	rs := readingsOn(worked, 40, 0.40, 0.65, 0.05)
	floor := worked.B * 0.8
	got := BootstrapLines(rs, Refit{Tier: TierPinnedB, Line: worked, Baseline: worked.B, BelowBandITL: floor, DrawB: true}, DefaultResampling())
	all := make([]float64, 0, len(got.Lines))
	under := 0
	for _, s := range got.Lines {
		if s.Line.B > worked.B {
			t.Fatalf("resample B %.6f above the pinned %.6f, the ceiling", s.Line.B, worked.B)
		}
		if s.Line.B < worked.B {
			under++
		}
		all = append(all, s.Line.B)
	}
	if under == 0 {
		t.Errorf("no resample drew B under the pinned one; the case tests nothing")
	}
	if lo, hi := splitAt(all, worked.B); 2*lo < len(all) || 2*hi < len(all) {
		t.Errorf("%d drawn B at or under the pinned %.9f and %d at or over it, of %d: want it their median", lo, worked.B, hi, len(all))
	}
}

// Where a line has no intercept to draw (newInterceptDraw) the switch draws
// nothing: an above-band line, a Baseline that is no time, readings with no
// spread in k. With DrawB set, each case has no draw, its resamples are the
// ones with DrawB unset, and a pinned resample keeps Baseline. A pinned line
// on readings with spread is the case the draw moves. The line priced
// one-sided at a prior is in TestBootstrapPricesTheOneSidedLineTheControllerDid.
//
// Seen through BootstrapLines alone these cases would keep Baseline even
// without their guard (an above-band refit takes no intercept, a Baseline
// that is no time fits no pinned line, and readings with no spread read no
// intercept), so the draw itself is asserted absent.
func TestInterceptDrawExclusions(t *testing.T) {
	spread := readingsOn(worked, 40, 0.40, 0.65, 0.05)
	flat := readingsOn(worked, 40, 0.50, 0.50, 0.05)
	above := itl.KeyReadings{Above: spread.All, AboveSteady: spread.Steady}
	pinnedAt := func(b float64) Refit { return Refit{Tier: TierPinnedB, Line: worked, Baseline: b} }
	for _, c := range []struct {
		name  string
		rs    itl.KeyReadings
		fit   Refit
		drawn bool
	}{
		{"pinned on readings with spread", spread, pinnedAt(worked.B), true},
		{"above the band", above, Refit{Tier: TierAboveBand, Line: worked, Baseline: worked.B}, false},
		{"baseline zero", spread, pinnedAt(0), false},
		{"baseline NaN", spread, pinnedAt(math.NaN()), false},
		{"baseline infinite", spread, pinnedAt(math.Inf(1)), false},
		{"no spread in k", flat, pinnedAt(worked.B), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			on := c.fit
			on.DrawB = true
			all := c.rs.All
			if c.fit.Tier == TierAboveBand {
				all = c.rs.Above
			}
			if ic := newInterceptDraw(all, on); (ic != nil) != c.drawn {
				t.Fatalf("draw %+v, want one %v", ic, c.drawn)
			}
			got := BootstrapLines(c.rs, on, DefaultResampling())
			off := BootstrapLines(c.rs, c.fit, DefaultResampling())
			// A Baseline that is no time fits no pinned line (itl.Refit), so
			// those cases have no resamples on either side.
			validBaseline := c.fit.Baseline > 0 && !math.IsInf(c.fit.Baseline, 0)
			if len(got.Lines) != len(off.Lines) || (len(got.Lines) > 0) != validBaseline {
				t.Fatalf("%d resamples with the draw, %d without: want the same, some only with a baseline that is a time", len(got.Lines), len(off.Lines))
			}
			pinned, moved := 0, 0
			for i, s := range got.Lines {
				if s.Line != off.Lines[i].Line {
					moved++
				}
				if s.Tier == TierPinnedB && c.fit.Tier == TierPinnedB {
					pinned++
					if !c.drawn && math.Float64bits(s.Line.B) != math.Float64bits(c.fit.Baseline) {
						t.Fatalf("resample %d B %v, want the baseline %v", i, s.Line.B, c.fit.Baseline)
					}
				}
			}
			t.Logf("%d of %d resamples moved by the draw, %d pinned", moved, len(got.Lines), pinned)
			if c.drawn && moved < len(got.Lines)/2 {
				t.Errorf("the draw moved %d of %d resamples, want most", moved, len(got.Lines))
			}
			if !c.drawn && moved != 0 {
				t.Errorf("the draw moved %d of %d resamples, want none", moved, len(got.Lines))
			}
		})
	}
}

// splitAt is how many of xs are at or under x, and how many at or over it.
func splitAt(xs []float64, x float64) (under, over int) {
	for _, v := range xs {
		if v <= x {
			under++
		}
		if v >= x {
			over++
		}
	}
	return under, over
}

func TestBootstrapCountsBlocks(t *testing.T) {
	for _, c := range []struct{ cycles, want int }{{5, 2}, {8, 2}, {9, 3}, {40, 10}, {3, 1}} {
		got := BootstrapLines(readingsOn(worked, c.cycles, 0.2, 0.7, 0.05), Refit{Tier: TierOLS, Line: worked, Baseline: worked.B, DrawB: true}, DefaultResampling())
		if got.Blocks != c.want {
			t.Errorf("%d cycles: %d blocks, want %d", c.cycles, got.Blocks, c.want)
		}
	}
}

func TestBandWidening(t *testing.T) {
	for _, c := range []struct {
		g    int
		want float64
	}{{0, 1}, {1, 1}, {2, 3.40}, {3, 1.80}, {4, 1.48}, {5, 1.34}, {10, 1.14}, {20, 1.06}, {100, 1.01}} {
		if got := bandWidening(c.g); math.Abs(got-c.want) > 0.01 {
			t.Errorf("g %d: %.3f, want %.2f", c.g, got, c.want)
		}
	}
	for g := 2; g < 200; g++ {
		if bandWidening(g+1) > bandWidening(g) {
			t.Fatalf("widening rises from %d to %d blocks", g, g+1)
		}
	}
}

// A band of few blocks is widened about the answer, which does not move,
// and carries its blocks.
func TestBandAnswerWidensFewBlocks(t *testing.T) {
	lines := make([]Resample, 0, 10)
	for i := range 10 {
		lines = append(lines, Resample{Line: itl.Model{A: 1, B: float64(i)}, Tier: TierPinnedB})
	}
	value := func(m itl.Model) (float64, bool) { return 10 + m.B, true }
	wide := BandAnswer(Answer{Value: 14}, LineResamples{Lines: lines, Drawn: 10, Blocks: 2}, value)
	many := BandAnswer(Answer{Value: 14}, LineResamples{Lines: lines, Drawn: 10, Blocks: 1000}, value)
	if wide.Value != 14 || many.Value != 14 {
		t.Fatalf("values %v %v, want 14", wide.Value, many.Value)
	}
	f := bandWidening(2)
	if math.Abs(wide.Band.Lo-(14-f*(14-10))) > 1e-9 || math.Abs(wide.Band.Hi-(14+f*(18-14))) > 1e-9 {
		t.Errorf("band %+v, want 10 and 18 moved from 14 by %.2f", *wide.Band, f)
	}
	if wide.Band.Blocks != 2 {
		t.Errorf("blocks %d, want 2", wide.Band.Blocks)
	}
	if many.Band.Hi-many.Band.Lo >= wide.Band.Hi-wide.Band.Lo {
		t.Errorf("a band of 1000 blocks %+v not narrower than one of 2 %+v", *many.Band, *wide.Band)
	}
}

// A point that is no number gets the resamples' own band, unwidened: no
// edge is moved from it, so none becomes NaN or Inf.
func TestBandAnswerOfANonFinitePointIsUnwidened(t *testing.T) {
	lines := make([]Resample, 0, 10)
	for i := range 10 {
		lines = append(lines, Resample{Line: itl.Model{A: 1, B: float64(i)}, Tier: TierPinnedB})
	}
	value := func(m itl.Model) (float64, bool) { return 10 + m.B, true }
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		got := BandAnswer(Answer{Value: v}, LineResamples{Lines: lines, Drawn: 10, Blocks: 2}, value)
		if got.Band == nil {
			t.Fatalf("point %v: no band", v)
		}
		if got.Band.Lo != 10 || got.Band.Hi != 18 || got.Band.Blocks != 2 {
			t.Errorf("point %v: band %+v, want the 10th to 90th percentile [10, 18] unwidened, 2 blocks", v, *got.Band)
		}
	}
}

// calibrationReadings is cycles of two replicas' readings on line, k drawn
// uniformly over [lo, hi], with multiplicative normal noise.
func calibrationReadings(rng *rand.Rand, line itl.Model, cycles int, lo, hi, noise float64) []itl.Observation {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	out := make([]itl.Observation, 0, 2*cycles)
	for c := range cycles {
		at := t0.Add(time.Duration(c) * 15 * time.Second)
		for range 2 {
			k := lo + (hi-lo)*rng.Float64()
			out = append(out, itl.Observation{K: k, ITLSec: line.ITLAt(k) * (1 + noise*rng.NormFloat64()), Timestamp: at})
		}
	}
	return out
}

// The band covers the true ITL at fill 0.85 nearer its nominal 80% with B
// drawn than with B held, on a pinned line whose B was learned from another
// window of the same design (its error that window's sampling error): the
// case the issue names, readings spread too little for the full fit.
func TestDrawnBCoversTheTruthNearerNominal(t *testing.T) {
	truth := itl.Model{A: 0.02, B: 0.008}
	const seeds, kSat = 60, 0.85
	covered := map[bool]int{}
	for seed := range uint64(seeds) {
		rng := rand.New(rand.NewPCG(seed, 307))
		learned, ok := olsIntercept(calibrationReadings(rng, truth, 12, 0.20, 0.45, 0.04))
		if !ok || !(learned > 0) {
			t.Fatalf("seed %d: no learned B", seed)
		}
		obs := calibrationReadings(rng, truth, 12, 0.20, 0.45, 0.04)
		line, ok := itl.FitPinnedB(obs, learned)
		if !ok {
			t.Fatalf("seed %d: no pinned line through %.5f", seed, learned)
		}
		rs := itl.KeyReadings{All: obs, Steady: obs}
		for _, hold := range []bool{true, false} {
			lines := BootstrapLines(rs, Refit{Tier: TierPinnedB, Line: line, Baseline: learned, HoldB: hold, DrawB: true}, DefaultResampling())
			a := BandAnswer(Answer{Value: line.ITLAt(kSat)}, lines, func(m itl.Model) (float64, bool) { return m.ITLAt(kSat), true })
			if a.Band == nil {
				t.Fatalf("seed %d: no band", seed)
			}
			if v := truth.ITLAt(kSat); a.Band.Lo <= v && v <= a.Band.Hi {
				covered[hold]++
			}
		}
	}
	held, drawn := float64(covered[true])/seeds, float64(covered[false])/seeds
	t.Logf("coverage of the true ITL at %.2f: B held %.2f, B drawn %.2f (nominal 0.80)", kSat, held, drawn)
	if !(drawn > held) || !(math.Abs(drawn-0.8) < math.Abs(held-0.8)) {
		t.Errorf("B drawn covers %.2f, B held %.2f: want drawn higher and nearer 0.80", drawn, held)
	}
}
