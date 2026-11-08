package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// Review T5: ReasonNoGPUBound, asserted by name. Borrowing is counted only
// with a GPU bound in force: the physical limiter, or a namespace
// ResourceQuota at most nominal plus borrowingLimit (8 + 8 here).
func TestNoGPUBoundWithholdsBorrowing(t *testing.T) {
	observed := time.Unix(1000, 0)
	caps := func(n int) config.ExternalCaps { return config.ExternalCaps{ByType: map[string]int{"H100": n}} }
	for _, tc := range []struct {
		name     string
		physical bool
		quota    map[string]int
		want     string
	}{
		{"no physical limiter, no ResourceQuota", false, nil, string(kueue.ReasonNoGPUBound)},
		{"a ResourceQuota above nominal plus borrowingLimit", false, map[string]int{"nvidia.com/gpu": 17}, string(kueue.ReasonNoGPUBound)},
		{"control: a ResourceQuota at nominal plus borrowingLimit", false, map[string]int{"nvidia.com/gpu": 16}, ""},
		{"control: the physical limiter bounds it", true, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeQuotaSource{}
			src.push(config.ExternalQuotas{
				Namespace:              map[string]config.ExternalCaps{"a": caps(8)},
				NamespaceGuaranteed:    map[string]config.ExternalCaps{"a": caps(8)},
				NamespaceWithinNominal: map[string]config.ExternalCaps{"a": caps(8)},
				Borrow: map[string][]config.ExternalBorrow{"a": {{Queue: "a-cq", Cohort: "c", Flavor: "h100",
					Resource: "nvidia.com/gpu", Accelerator: "H100", Nominal: 8, Guaranteed: 8, Borrowable: 8,
					BorrowCap: 8, WithinNominal: 8}}},
				SharedPools: []config.ExternalSharedPool{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
					Accelerator: "H100", Free: 8}},
				ObservedAt: observed,
			}, nil)
			e := config.QuotaLimiterConfig{Name: "nq", Type: "quota", Scope: config.QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
				Kueue:           &config.KueueQuotaSource{Enabled: true}}
			inv := NewQuotaInventoryWithSource(e, src)
			inv.now = func() time.Time { return observed.Add(time.Second) }
			inv.EnableBorrowing(BorrowOptions{Physical: tc.physical, Eligibility: &fakeEligibility{
				byNS: map[string]kueue.NamespaceEligibility{"a": {GPUQuota: tc.quota}}}})
			if err := inv.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := inv.BorrowWithheld([]string{"a"})["a"]; got != tc.want {
				t.Fatalf("withheld reason %q, want %q", got, tc.want)
			}
		})
	}
}
