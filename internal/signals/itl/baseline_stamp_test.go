package itl

import (
	"fmt"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// An intercept with no reading and no slope is as old as its stamp: the one a
// snapshot carried (RestoreBaselineAt), or unknown, and the stamp goes with
// the intercept on every path that drops it.
func TestBaselineStamp_ReportAndLifecycle(t *testing.T) {
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	report := func(tr *Tracker, key string) time.Time {
		_, stored := tr.Baseline(key)
		return tr.Report(learn.Report{}, key, Reading{BaselineStored: stored}).LastAdmitted
	}

	tr := NewTracker()
	tr.RestoreBaselineAt("k", 0.01, at)
	if got := report(tr, "k"); !got.Equal(at) {
		t.Fatalf("a restored intercept reports %v, want its stamp %v", got, at)
	}
	if got := tr.BaselinesAdmittedAt(); len(got) != 1 || !got["k"].Equal(at) {
		t.Fatalf("BaselinesAdmittedAt = %v", got)
	}
	tr.RestoreBaselineAt("k", 0.02, at.Add(time.Hour))
	if b, _ := tr.Baseline("k"); b != 0.01 || !report(tr, "k").Equal(at) {
		t.Fatalf("a second restore replaced the held intercept or its stamp: %v %v", b, report(tr, "k"))
	}

	tr.RestoreBaseline("unknown", 0.01)
	if got := report(tr, "unknown"); !got.IsZero() {
		t.Fatalf("an intercept restored with no stamp reports %v, want zero", got)
	}
	if _, ok := tr.BaselinesAdmittedAt()["unknown"]; ok {
		t.Fatal("an unknown age was exported as a stamp")
	}

	tr.SetBaseline("k", 0.03)
	if got := report(tr, "k"); !got.IsZero() {
		t.Fatalf("an intercept put back from a stash kept the old stamp: %v", got)
	}

	for name, drop := range map[string]func(*Tracker){
		"sweep":  func(tr *Tracker) { tr.Sweep(at, func(k string) bool { return k == "k" }) },
		"forget": func(tr *Tracker) { tr.ForgetKeys(at, learn.ByOperator, func(k string) bool { return k == "k" }) },
		"all":    func(tr *Tracker) { tr.ForgetBaselines() },
	} {
		tr := NewTracker()
		tr.RestoreBaselineAt("k", 0.01, at)
		drop(tr)
		// The map itself, not BaselineAdmittedAt: that reads through the
		// intercept, so a stamp left behind would not show there.
		if len(tr.baselineAt) != 0 {
			t.Errorf("%s: the stamp outlived its intercept: %v", name, tr.baselineAt)
		}
		if got := tr.BaselineAdmittedAt("k"); !got.IsZero() {
			t.Errorf("%s: the stamp outlived its intercept: %v", name, got)
		}
		tr.RestoreBaseline("k", 0.01)
		if got := report(tr, "k"); !got.IsZero() {
			t.Errorf("%s: a stamp came back with a new intercept of unknown age: %v", name, got)
		}
		if len(tr.baselineAt) != 0 {
			t.Errorf("%s: stamps left: %v", name, tr.baselineAt)
		}
	}
}

// A priced line with no reading in its window and no held slope reads the
// intercept the key holds when it reports, not the one Observe read before
// its fit: the reading's BaselineStored is false on the cycle whose full fit
// first stores B, and that cycle reports B's stamp too. Observe cannot reach
// such a reading today -- both windows age out on DefaultObservationMaxAge, so
// an empty window leaves no steady reading to fit -- so it is held here, at
// the Report.
func TestBaselineStamp_TheFirstFitThatStoresBIsStamped(t *testing.T) {
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for name, stored := range map[string]bool{"B stored before the fit": true, "B stored by this fit": false} {
		tr := NewTracker()
		tr.RestoreBaselineAt("k", 0.01, at)
		got := tr.Report(learn.Report{}, "k", Reading{Tier: TierOLS, BaselineStored: stored, SteadyHeld: 10})
		if got.PricingKey != "k" || !got.LastAdmitted.Equal(at) {
			t.Errorf("%s: %+v, want key k at %v", name, got, at)
		}
	}
}

// Observe end to end: a full fit stamps B with the newest reading it was
// fitted from, and a later cycle that refits the same readings, with nothing
// new admitted, keeps that stamp rather than taking the cycle's clock.
func TestBaselineStamp_ObserveStampsTheNewestFittedReading(t *testing.T) {
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	online := func(n int, lo, hi float64) []domain.ReplicaMetrics {
		rows := make([]domain.ReplicaMetrics, n)
		for i := range n {
			k := lo + (hi-lo)*float64(i)/float64(n-1)
			rows[i] = domain.ReplicaMetrics{PodName: fmt.Sprintf("p%d", i), VariantName: "v", Ready: true,
				KvUsageInstant: k, AvgITL: 0.02*k + 0.01}
		}
		return rows
	}
	tr := NewTracker()
	first := tr.Observe("k", "v", online(10, 0.2, 0.7), Admission{}, at)
	if first.Tier != TierOLS {
		t.Fatalf("premise: a full fit, got %q", first.Tier)
	}
	if got := tr.BaselineAdmittedAt("k"); !got.Equal(at) {
		t.Fatalf("the first full fit stamped B %v, want its readings' %v", got, at)
	}

	later := at.Add(time.Minute)
	light := []domain.ReplicaMetrics{{PodName: "p0", VariantName: "v", Ready: true, KvUsageInstant: 0.06, AvgITL: 0.0112}}
	again := tr.Observe("k", "v", light, Admission{}, later)
	if again.Tier != TierOLS || again.Offered != 0 {
		t.Fatalf("premise: a refit with nothing admitted, got tier %q, %d offered", again.Tier, again.Offered)
	}
	if got := tr.BaselineAdmittedAt("k"); !got.Equal(at) {
		t.Errorf("a refit of the same readings stamped B %v, want the newest fitted reading %v, not the cycle %v",
			got, at, later)
	}
}

// A full fit stamps the intercept with the newest reading it was fitted from,
// not the cycle's clock: a refit from readings the steady window carries does
// not make it younger.
func TestBaselineStamp_NewestOf(t *testing.T) {
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	obs := []Observation{{Timestamp: at}, {Timestamp: at.Add(2 * time.Minute)}, {Timestamp: at.Add(time.Minute)}}
	if got := newestOf(obs, at.Add(time.Hour)); !got.Equal(at.Add(2 * time.Minute)) {
		t.Errorf("newestOf = %v", got)
	}
	if got := newestOf(nil, at); !got.Equal(at) {
		t.Errorf("newestOf(nil) = %v", got)
	}
}
