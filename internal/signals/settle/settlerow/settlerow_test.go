package settlerow

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
)

// A row keeps the fields the tracker reads, its role from discovery, and its
// ITL as the three states the presence bits give.
func TestOfReadsRolesAndITLStates(t *testing.T) {
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	rows := []domain.ReplicaMetrics{
		{PodName: "p", VariantName: "pre", Ready: true, ReadySince: at, AvgITLEmpty: true,
			RequestRate: 0.5, RequestRateKnown: true},
		{PodName: "d", VariantName: "dec", FromWarmPool: true, LentSince: at, Revision: "r",
			KvCacheUsage: 0.4, KvCacheUsageKnown: true, AvgITL: 0.02, AvgITLKnown: true,
			GenerationTokenRate: 100, GenerationTokenRateKnown: true},
		{PodName: "u", VariantName: "unlisted"},
	}
	got := Of(rows, map[string]string{"pre": domain.RolePrefill, "dec": domain.RoleDecode})
	want := []settle.Row{
		{Key: settle.Key{Pod: "p", Variant: "pre"}, Prefill: true, Ready: true, ReadySince: at, ITL: settle.ITLEmpty,
			RequestRate: 0.5, RequestRateKnown: true},
		{Key: settle.Key{Pod: "d", Variant: "dec", Bridge: true}, LentSince: at, Revision: "r",
			KV: 0.4, KVKnown: true, ITL: settle.ITLFinite, GenerationRate: 100, GenerationRateKnown: true},
		{Key: settle.Key{Pod: "u", Variant: "unlisted"}, ITL: settle.ITLAbsent},
	}
	if len(got) != len(want) {
		t.Fatalf("%d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

// ReadyAgain is the first Ready row after the last row that was not Ready:
// zero with no row that was not Ready, or with none Ready after it.
func TestReadyAgain(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	trace := func(ready ...bool) []settle.Sample {
		out := make([]settle.Sample, len(ready))
		for i, r := range ready {
			out[i] = settle.Sample{At: at.Add(time.Duration(i) * time.Minute), Row: settle.Row{Ready: r}}
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		trace []settle.Sample
		want  time.Time
	}{
		{"no rows", nil, time.Time{}},
		{"every row Ready", trace(true, true, true), time.Time{}},
		{"Ready again after one that was not", trace(true, false, true, true), at.Add(2 * time.Minute)},
		{"the last of two blips", trace(true, false, true, false, true), at.Add(4 * time.Minute)},
		{"not Ready still", trace(true, true, false), time.Time{}},
		{"never Ready yet", trace(false, false), time.Time{}},
	} {
		if got := ReadyAgain(tc.trace); !got.Equal(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ObserveOwnSince backdates a first observation past the tracker's absence
// tolerance (issue ev-shindin/llm-scaling-manager#511): the replica reads Joined at the backdate, and a
// later observation keeps it. ObserveOwn at the same backdate reads the gap
// as an absence.
func TestObserveOwnSinceBackdatesPastTheTolerance(t *testing.T) {
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	const backdate = 2 * time.Minute
	rows := []settle.Row{{Key: settle.Key{Pod: "p", Variant: "v"}, Ready: true}}
	since, own := settle.NewTracker(), settle.NewTracker()
	ObserveOwnSince(since, "m", rows, true, backdate, at)
	ObserveOwnSince(since, "m", rows, false, backdate, at.Add(15*time.Second))
	ObserveOwn(own, "m", rows, true, backdate, at)
	if r, _ := since.Replica("m", rows[0].Key); r.Reason != settle.Joined || !r.Since.Equal(at.Add(-backdate)) {
		t.Errorf("ObserveOwnSince: settle %s at %v, want joined at %v", r.Reason, r.Since, at.Add(-backdate))
	}
	if r, _ := own.Replica("m", rows[0].Key); r.Reason != settle.Rejoined || !r.Since.Equal(at) {
		t.Errorf("ObserveOwn past the tolerance: settle %s at %v, want rejoined at %v", r.Reason, r.Since, at)
	}
}
