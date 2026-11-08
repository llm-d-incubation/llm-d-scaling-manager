package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// Review W1: a namespace read and not counted -- here outside the
// borrowNamespaces pilot -- carries the nominal its reclaiming queue lent out,
// so its growth into it places without a node hole. One withheld as not
// Kueue-managed carries none: Kueue reclaims nothing for its pods.
func TestUncountedNamespaceCarriesItsLentOutNominal(t *testing.T) {
	observed := time.Unix(1000, 0)
	caps := func(n int) config.ExternalCaps { return config.ExternalCaps{ByType: map[string]int{"H100": n}} }
	row := func(queue string, lentOut int) config.ExternalBorrow {
		return config.ExternalBorrow{Queue: queue, Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
			Accelerator: "H100", Nominal: 16, Guaranteed: 16, BorrowCap: 8, WithinNominal: 16, LentOut: lentOut}
	}
	for _, tc := range []struct {
		name   string
		reason kueue.Reason
		want   int
	}{
		{"not selected: its lent-out nominal is carried", "", 16},
		{"control: not Kueue-managed carries nothing", kueue.ReasonNotKueueManaged, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeQuotaSource{}
			src.push(config.ExternalQuotas{
				Namespace:              map[string]config.ExternalCaps{"lender": caps(16), "borrower": caps(16)},
				NamespaceGuaranteed:    map[string]config.ExternalCaps{"lender": caps(16), "borrower": caps(16)},
				NamespaceWithinNominal: map[string]config.ExternalCaps{"lender": caps(16), "borrower": caps(16)},
				Borrow: map[string][]config.ExternalBorrow{
					"lender": {row("lender-cq", 16)}, "borrower": {row("borrower-cq", 0)}},
				SharedPools: []config.ExternalSharedPool{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
					Accelerator: "H100", Free: 0}},
				ObservedAt: observed,
			}, nil)
			e := config.QuotaLimiterConfig{Name: "nq", Type: "quota", Scope: config.QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
				Kueue:           &config.KueueQuotaSource{Enabled: true, BorrowNamespaces: []string{"borrower"}}}
			if tc.reason != "" {
				e.Kueue.BorrowNamespaces = nil
			}
			inv := NewQuotaInventoryWithSource(e, src)
			inv.now = func() time.Time { return observed.Add(time.Second) }
			inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: &fakeEligibility{
				byNS: map[string]kueue.NamespaceEligibility{"lender": {Reason: tc.reason}}}})
			c, err := NewDefaultLimiter(e.Name, inv).ComputeConstraints(context.Background(), nil,
				map[string]map[string]int{"lender": {}, "borrower": {}})
			if err != nil {
				t.Fatal(err)
			}
			if _, counted := c.NamespaceBorrow["lender"]; counted {
				t.Fatal("premise: the lender must not be counted")
			}
			if got := c.NamespaceNominal["lender"]["H100"].LentOut; got != tc.want {
				t.Fatalf("lender's lent-out nominal %d, want %d", got, tc.want)
			}
		})
	}
}
