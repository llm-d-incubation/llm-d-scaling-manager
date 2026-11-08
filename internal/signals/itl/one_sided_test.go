package itl

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// cappedRows is three replicas at their sequence cap (64 running on one
// rank, requests waiting, no preemption) at fill k, on the line ITL = a k +
// b, Ready an hour before start.
func cappedRows(k, a, b float64, start time.Time) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, 3)
	for i := range 3 {
		out = append(out, domain.ReplicaMetrics{PodName: fmt.Sprintf("pod-%d", i), VariantName: "v", Ready: true,
			ReadySince: start.Add(-time.Hour), KvCacheUsage: k, KvUsageInstant: k, AvgITL: a*k + b,
			RunningRequests: 64, QueueLength: 5, PreemptionRateKnown: true})
	}
	return out
}

// With OneSidedAboveKSat a key with no learned B, taught only by replicas
// at their cap above DefaultKSat, prices at DefaultKSat at or above the true
// line: the line through the least ITL of those readings wins over the fit
// through DefaultBaselineSec, which prices under a true B above it. Without
// it, as before, the fit through the default prices under the truth.
func TestTracker_OneSidedAboveKSatPricesAtOrAboveTheTruth(t *testing.T) {
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	const a, b = 0.01, 0.030
	truth := a*DefaultKSat + b
	price := func(oneSided bool) (Reading, float64) {
		tr := NewTracker()
		adm := Admission{MaxNumSeqs: 64, OneSidedAboveKSat: oneSided}
		var r Reading
		for c := range 6 {
			k := []float64{0.91, 0.92, 0.93}[c%3]
			r = tr.Observe("k", "v", cappedRows(k, a, b, start), adm, start.Add(time.Duration(c)*15*time.Second))
		}
		if r.Tier == "" {
			t.Fatalf("one-sided %v: no line", oneSided)
		}
		return r, r.Model.ITLAt(DefaultKSat)
	}
	r, on := price(true)
	if !r.PinnedAtLeastITL || on < truth {
		t.Errorf("one-sided: priced %.5f s against the truth %.5f s (pinned at the least ITL %v)", on, truth, r.PinnedAtLeastITL)
	}
	r, off := price(false)
	if r.PinnedAtLeastITL || !(off < truth) {
		t.Errorf("negative control, without it: priced %.5f s against the truth %.5f s; it should price under", off, truth)
	}
	t.Logf("truth %.5f s: one-sided %.5f s, through the default B %.5f s", truth, on, off)
}

// The line through the least ITL reads only the readings at or past
// DefaultKSat: in a window of capped readings at k 0.82 and 0.93, on a
// shallow line, its pin is the least ITL at 0.93 and its slope is fitted
// over those alone (flat, on readings without noise), not the 0.82 ones,
// which lie under the k the line is priced at and would turn its margin
// against the truth.
func TestTracker_OneSidedReadsOnlyTheReadingsPastKSat(t *testing.T) {
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	const a, b = 0.01, 0.030
	tr := NewTracker()
	adm := Admission{MaxNumSeqs: 64, OneSidedAboveKSat: true}
	var r Reading
	for c := range 6 {
		rows := cappedRows(0.82, a, b, start)
		rows[2].KvCacheUsage, rows[2].KvUsageInstant, rows[2].AvgITL = 0.93, 0.93, a*0.93+b
		r = tr.Observe("k", "v", rows, adm, start.Add(time.Duration(c)*15*time.Second))
	}
	if r.Capped != 3 || !r.PinnedAtLeastITL {
		t.Fatalf("premise: three capped readings a cycle, priced one-sided; got capped %d, one-sided %v", r.Capped, r.PinnedAtLeastITL)
	}
	want := Model{A: 0, B: a*0.93 + b}
	if math.Abs(r.Model.A-want.A) > 1e-12 || math.Abs(r.Model.B-want.B) > 1e-12 {
		t.Errorf("the line %+v, want %+v: the pin or the slope read readings under kSat", r.Model, want)
	}
	if truth := a*DefaultKSat + b; r.Model.ITLAt(DefaultKSat) < truth {
		t.Errorf("priced %.5f s, under the truth %.5f s", r.Model.ITLAt(DefaultKSat), truth)
	}
}
