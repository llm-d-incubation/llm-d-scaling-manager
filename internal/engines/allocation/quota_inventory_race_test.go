package allocation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// G6: EnableBorrowing writes the borrowing options under the lock while
// Refresh read them, and the clock, without it. Run under -race, a Refresh
// beside an EnableBorrowing reported a data race.
func TestEnableBorrowingBesideRefreshDoesNotRace(t *testing.T) {
	at := time.Unix(1000, 0)
	e := config.QuotaLimiterConfig{
		Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
		Kueue:           &config.KueueQuotaSource{Enabled: true},
	}
	src := &fakeQuotaSource{}
	src.push(lenderSnapshot(at), nil)
	inv := NewQuotaInventoryWithSource(e, src)
	inv.now = func() time.Time { return at.Add(10 * time.Second) }
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			inv.EnableBorrowing(BorrowOptions{Physical: i%2 == 0, Eligibility: noEligibilityLimits{}})
		}
	}()
	for i := 0; i < 50; i++ {
		if err := inv.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
