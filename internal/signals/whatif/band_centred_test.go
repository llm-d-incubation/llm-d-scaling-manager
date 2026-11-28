package whatif

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// A pinned line whose B was learned above these readings' own (issue
// llm-d-incubation/llm-d-scaling-manager#307): its least reading sits under
// the pinned B. The band of a figure priced on it holds the figure, and the
// resamples are centred on it: drawing B must widen the band, never move it
// off its point.
func TestPinnedBandHoldsAndCentresItsPoint(t *testing.T) {
	rs := readingsOn(worked, 40, 0.05, 0.30, 0.05)
	pinned := worked.B * 1.3
	line, ok := itl.FitPinnedB(rs.All, pinned)
	if !ok {
		t.Fatalf("no pinned line through %.6f", pinned)
	}
	least := line.B
	for _, o := range rs.All {
		least = min(least, o.ITLSec)
	}
	if !(least < pinned) {
		t.Fatalf("least reading %.6f not under the pinned B %.6f; the case tests nothing", least, pinned)
	}
	lines := BootstrapLines(rs, Refit{Tier: TierPinnedB, Line: line, Baseline: pinned, DrawB: true}, DefaultResampling())
	if len(lines.Lines) == 0 {
		t.Fatal("no resample fitted")
	}
	for _, k := range []float64{0.10, 0.85} {
		value := func(m itl.Model) (float64, bool) { return m.ITLAt(k), true }
		point := line.ITLAt(k)
		a := BandAnswer(Answer{Value: point}, lines, value)
		if a.Band == nil {
			t.Fatalf("k %.2f: no band", k)
		}
		if !(a.Band.Lo <= point && point <= a.Band.Hi) {
			t.Errorf("k %.2f: band [%.6f, %.6f] does not hold its point %.6f", k, a.Band.Lo, a.Band.Hi, point)
		}
		below := 0
		for _, s := range lines.Lines {
			if v, _ := value(s.Line); v <= point {
				below++
			}
		}
		share := float64(below) / float64(len(lines.Lines))
		t.Logf("k %.2f: point %.6f, band [%.6f, %.6f], %.2f of the resamples at or under it", k, point, a.Band.Lo, a.Band.Hi, share)
		if share < 0.25 || share > 0.75 {
			t.Errorf("k %.2f: %.2f of the resample figures at or under the point, want the resamples centred on it (0.25 to 0.75)", k, share)
		}
	}
}
