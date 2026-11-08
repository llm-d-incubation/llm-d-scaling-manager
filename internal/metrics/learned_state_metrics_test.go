package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// seriesValue returns the value of the one series of name whose labels include
// want, and whether there is one.
func seriesValue(t *testing.T, registry *prometheus.Registry, name string, want map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	series:
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			for k, v := range want {
				if got[k] != v {
					continue series
				}
			}
			return metricValue(m), true
		}
	}
	return 0, false
}

func metricValue(m *dto.Metric) float64 {
	if m.GetGauge() != nil {
		return m.GetGauge().GetValue()
	}
	return m.GetCounter().GetValue()
}

func TestLearnedStateBorrowsPublishWhatAVariantBorrowedAndFromWhom(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	emitter.RecordLearnedStateBorrows("ns", "m", "canary", []domain.LearnedStateBorrow{
		{Quantity: domain.BorrowedK2, Donor: "main", Relaxed: 0.25},
		{Quantity: domain.BorrowedITLLine, Donor: "other"},
	})

	if v, ok := seriesValue(t, registry, constants.LLMSMLearnedStateBorrowed, map[string]string{
		constants.LabelVariantName: "canary", constants.LabelQuantity: "k2", constants.LabelDonor: "main",
	}); !ok || v != 1 {
		t.Fatalf("k2 borrow series = %v (present %v), want 1", v, ok)
	}
	if _, ok := seriesValue(t, registry, constants.LLMSMLearnedStateBorrowed, map[string]string{
		constants.LabelVariantName: "canary", constants.LabelQuantity: "itl-line", constants.LabelDonor: "other",
	}); !ok {
		t.Fatal("no series for the borrowed ITL line")
	}
	if v, ok := seriesValue(t, registry, constants.LLMSMLearnedStateBorrowRelaxed, map[string]string{
		constants.LabelVariantName: "canary",
	}); !ok || v != 0.25 {
		t.Fatalf("relaxed = %v (present %v), want 0.25", v, ok)
	}
}

// A borrow that ends, or moves to another donor, must not leave its old series
// behind: both would read as current.
func TestLearnedStateBorrowsReplaceWhatWasPublishedBefore(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	emitter.RecordLearnedStateBorrows("ns", "m", "canary", []domain.LearnedStateBorrow{
		{Quantity: domain.BorrowedK2, Donor: "main"},
	})
	emitter.RecordLearnedStateBorrows("ns", "m", "canary", []domain.LearnedStateBorrow{
		{Quantity: domain.BorrowedK2, Donor: "other"},
	})
	if got := countSeries(t, registry, constants.LLMSMLearnedStateBorrowed); got != 1 {
		t.Fatalf("%d borrow series after the donor changed, want 1", got)
	}

	emitter.RecordLearnedStateBorrows("ns", "m", "canary", nil)
	if got := countSeries(t, registry, constants.LLMSMLearnedStateBorrowed); got != 0 {
		t.Fatalf("%d borrow series after the borrow ended, want 0", got)
	}
	if got := countSeries(t, registry, constants.LLMSMLearnedStateBorrowRelaxed); got != 0 {
		t.Fatalf("%d relaxed series after the borrow ended, want 0", got)
	}
}

// A shared ITL line may come from a variant of the same name in another
// namespace. The donor's namespace tells them apart -- and tells a borrow
// from ns-a apart from the variant borrowing from itself.
func TestLearnedStateBorrowsNameTheDonorsNamespace(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	emitter.RecordLearnedStateBorrows("ns-b", "m", "decode", []domain.LearnedStateBorrow{
		{Quantity: domain.BorrowedITLLine, Donor: "decode", DonorNamespace: "ns-a"},
	})
	if _, ok := seriesValue(t, registry, constants.LLMSMLearnedStateBorrowed, map[string]string{
		constants.LabelNamespace: "ns-b", constants.LabelDonor: "decode", constants.LabelDonorNamespace: "ns-a",
	}); !ok {
		t.Fatal("the donor's namespace is not on the series")
	}
}

// A k2 borrow that ends while a line borrow continues takes its relaxation
// with it, and leaves the line's series.
func TestLearnedStateBorrowsDropTheRelaxationWithTheK2Borrow(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	line := domain.LearnedStateBorrow{Quantity: domain.BorrowedITLLine, Donor: "a", DonorNamespace: "ns"}
	emitter.RecordLearnedStateBorrows("ns", "m", "canary", []domain.LearnedStateBorrow{
		{Quantity: domain.BorrowedK2, Donor: "main", DonorNamespace: "ns", Relaxed: 0.3}, line,
	})
	emitter.RecordLearnedStateBorrows("ns", "m", "canary", []domain.LearnedStateBorrow{line})
	if got := countSeries(t, registry, constants.LLMSMLearnedStateBorrowRelaxed); got != 0 {
		t.Fatalf("%d relaxed series after the k2 borrow ended, want 0", got)
	}
	if _, ok := seriesValue(t, registry, constants.LLMSMLearnedStateBorrowed, map[string]string{
		constants.LabelQuantity: "itl-line",
	}); !ok {
		t.Fatal("the line borrow, still in effect, was removed with the k2 borrow")
	}
	if got := countSeries(t, registry, constants.LLMSMLearnedStateBorrowed); got != 1 {
		t.Fatalf("%d borrow series, want the line's alone", got)
	}
}

// Deleting one model's variant must not touch another model's variant of the
// same name in the same namespace.
func TestDeletingLearnedStateBorrowsIsScopedToTheModel(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	b := []domain.LearnedStateBorrow{{Quantity: domain.BorrowedK2, Donor: "main"}}
	emitter.RecordLearnedStateBorrows("ns", "m1", "canary", b)
	emitter.RecordLearnedStateBorrows("ns", "m2", "canary", b)

	emitter.DeleteLearnedStateBorrows("ns", "m1", "canary")
	if _, ok := seriesValue(t, registry, constants.LLMSMLearnedStateBorrowed, map[string]string{
		constants.LabelModelName: "m2",
	}); !ok {
		t.Fatal("deleting m1's variant removed m2's variant of the same name")
	}
	emitter.DeleteLearnedStateBorrowsForModel("ns", "m2")
	if got := countSeries(t, registry, constants.LLMSMLearnedStateBorrowed); got != 0 {
		t.Fatalf("%d borrow series after both models were deleted, want 0", got)
	}
}

// The counters are published at zero from registration, so rate() on a fresh
// controller reads 0 rather than nothing.
func TestLearnedStateCountersStartAtZeroAndCount(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	for _, o := range LearnedStateRestoreOutcomes {
		if v, ok := seriesValue(t, registry, constants.LLMSMLearnedStateRestoresTotal,
			map[string]string{constants.LabelOutcome: o}); !ok || v != 0 {
			t.Fatalf("%s restores = %v (present %v) before any restore, want 0", o, v, ok)
		}
	}
	emitter.AddLearnedStateRestores(map[string]int{"whole": 3, "prior": 1})
	emitter.IncLearnedStateSnapshotError(LearnedStateOpRead)
	for o, want := range map[string]float64{"whole": 3, "prior": 1, "args": 0} {
		if v, _ := seriesValue(t, registry, constants.LLMSMLearnedStateRestoresTotal,
			map[string]string{constants.LabelOutcome: o}); v != want {
			t.Fatalf("%s restores = %v, want %v", o, v, want)
		}
	}
	if v, _ := seriesValue(t, registry, constants.LLMSMLearnedStateSnapshotErrorsTotal,
		map[string]string{constants.LabelOperation: LearnedStateOpRead}); v != 1 {
		t.Fatalf("read errors = %v, want 1", v)
	}
	if v, ok := seriesValue(t, registry, constants.LLMSMLearnedStateSnapshotErrorsTotal,
		map[string]string{constants.LabelOperation: LearnedStateOpWrite}); !ok || v != 0 {
		t.Fatalf("write errors = %v (present %v), want 0", v, ok)
	}
}
