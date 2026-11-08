package whatif

import (
	"fmt"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// Issue #467: a band on a line the controller priced one-sided at the
// sequence cap. The tracker, with no learned B, prices the variant from the
// line through the least ITL of its readings at the cap with intercept the
// prior on B (itl.Admission.BPrior; returned through the default B, at its
// pricing k: itl.Admission.OneSidedAt); a refit of the
// same readings without the admission pins the default B alone and prices
// under it. Given the admission (Refit.OneSided), every resample prices the
// controller's line, to the spread a resample of three fills gives
// (0.01 %); without it, the band lies under the truth.
func TestBootstrapPricesTheOneSidedLineTheControllerDid(t *testing.T) {
	const kPrice = 0.68
	truth := itl.Model{A: 0.05, B: 0.010}
	adm := itl.Admission{MaxNumSeqs: 64, OneSidedAboveKSat: true, OneSidedAt: kPrice, BPrior: itl.DefaultBPrior}
	tr := itl.NewTracker()
	start := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	var line itl.Reading
	for c := range 24 {
		rows := make([]domain.ReplicaMetrics, 0, 3)
		for i, k := range []float64{0.96, 0.97, 0.98} {
			rows = append(rows, domain.ReplicaMetrics{PodName: fmt.Sprintf("p%d", i), VariantName: "v", Ready: true,
				ReadySince: start.Add(-time.Hour), KvCacheUsage: k, KvUsageInstant: k, AvgITL: truth.ITLAt(k),
				RunningRequests: 64, QueueLength: 5, PreemptionRateKnown: true})
		}
		line = tr.Observe("k", "v", rows, adm, start.Add(time.Duration(c)*15*time.Second))
	}
	if !line.PinnedAtBPrior || line.Tier != itl.TierPinnedB {
		t.Fatalf("premise: the controller's line %+v is not one-sided", line)
	}
	rs := tr.Readings("k")
	controller := line.Model.ITLAt(kPrice)

	on := BootstrapLines(rs, Refit{Tier: TierPinnedB, Line: line.Model, Baseline: itl.DefaultBaselineSec, OneSided: adm}, DefaultResampling())
	off := BootstrapLines(rs, Refit{Tier: TierPinnedB, Line: line.Model, Baseline: itl.DefaultBaselineSec}, DefaultResampling())
	if len(on.Lines) != on.Drawn || len(off.Lines) != off.Drawn || on.Drawn == 0 {
		t.Fatalf("drew %d/%d and %d/%d", len(on.Lines), on.Drawn, len(off.Lines), off.Drawn)
	}
	value := func(m itl.Model) (float64, bool) { return m.ITLAt(kPrice), true }
	band := BandAnswer(Answer{Value: controller}, on, value)
	offBand := BandAnswer(Answer{Value: controller}, off, value)
	t.Logf("controller %.5f s at k %.2f, truth %.5f s; band given the admission [%.5f, %.5f], without it [%.5f, %.5f]",
		controller, kPrice, truth.ITLAt(kPrice), band.Band.Lo, band.Band.Hi, offBand.Band.Lo, offBand.Band.Hi)
	if !(band.Band.Lo <= controller && controller <= band.Band.Hi) || band.Band.Hi-band.Band.Lo > 5e-4*controller || band.Band.Lo < truth.ITLAt(kPrice) {
		t.Errorf("the band [%.6f, %.6f] does not price the controller's line %.6f, at or above the truth", band.Band.Lo, band.Band.Hi, controller)
	}
	for _, s := range on.Lines {
		if s.TierChanged || s.Line.B != itl.DefaultBaselineSec {
			t.Fatalf("resample %+v: want the pinned-B line through the default B", s)
		}
	}
	// The negative control: without the admission the band prices the fit
	// through the default B alone, under the truth.
	if !(offBand.Band.Hi < truth.ITLAt(kPrice)) {
		t.Errorf("negative control: without the admission the band [%.5f, %.5f] is not under the truth %.5f",
			offBand.Band.Lo, offBand.Band.Hi, truth.ITLAt(kPrice))
	}
}
