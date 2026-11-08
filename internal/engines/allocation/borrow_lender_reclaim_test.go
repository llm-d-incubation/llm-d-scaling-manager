package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// noEligibilityLimits answers every namespace as managed, with readable
// ResourceQuotas.
type noEligibilityLimits struct{}

func (noEligibilityLimits) Evaluate(_ context.Context, namespaces []string) map[string]kueue.NamespaceEligibility {
	out := map[string]kueue.NamespaceEligibility{}
	for _, ns := range namespaces {
		out[ns] = kueue.NamespaceEligibility{}
	}
	return out
}

// lenderSnapshot is one Kueue read of cohort "c": namespace "serve" borrows
// through serve-cq (nominal 0, so all it reaches is borrowed), beside the given
// lenders; Kueue's free pool is what they lend unused.
func lenderSnapshot(at time.Time, lenders ...config.ExternalLender) config.ExternalQuotas {
	free := 0
	for _, l := range lenders {
		free += l.Unused
	}
	lenders = append(lenders, config.ExternalLender{Queue: "serve-cq", Reclaim: kueue.ReclaimNever})
	return config.ExternalQuotas{
		Namespace:              map[string]config.ExternalCaps{"serve": {ByType: map[string]int{"H100": 8}}},
		NamespaceGuaranteed:    map[string]config.ExternalCaps{"serve": {ByType: map[string]int{"H100": 0}}},
		NamespaceWithinNominal: map[string]config.ExternalCaps{"serve": {ByType: map[string]int{"H100": 0}}},
		Borrow: map[string][]config.ExternalBorrow{"serve": {{
			Queue: "serve-cq", Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu", Accelerator: "H100",
			Nominal: 8, Borrowable: free, BorrowCap: config.QuotaUnlimited, Reason: config.BorrowReasonNominalLentOut,
		}}},
		SharedPools: []config.ExternalSharedPool{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
			Accelerator: "H100", Free: free, Lenders: lenders}},
		ObservedAt: at,
	}
}

// borrowingOf is what a namespace-scope Kueue entry counts of snap for
// "serve": its shared pool's free GPUs, whether it borrows, and why not.
func borrowingOf(t *testing.T, snap config.ExternalQuotas) (free int, borrows bool, withheld string) {
	t.Helper()
	c := constraintsOf(t, snap)
	for _, p := range c.SharedPools {
		free += p.Free
	}
	_, borrows = c.NamespaceBorrow["serve"]
	return free, borrows, c.BorrowWithheld["serve"]
}

// chunkOf is the one shared pool chunk a namespace-scope Kueue entry counts
// of snap.
func chunkOf(t *testing.T, snap config.ExternalQuotas) *SharedPoolChunk {
	t.Helper()
	c := constraintsOf(t, snap)
	if len(c.SharedPools) != 1 {
		t.Fatalf("pools = %+v, want one", c.SharedPools)
	}
	return &c.SharedPools[0]
}

// constraintsOf is what a namespace-scope Kueue entry counting borrowing
// gives the optimizer for snap.
func constraintsOf(t *testing.T, snap config.ExternalQuotas) *ResourceConstraints {
	t.Helper()
	return constraintsOfEntry(t, snap, config.KueueQuotaSource{Enabled: true})
}

// constraintsOfEntry is constraintsOf for an entry with the given kueue block.
func constraintsOfEntry(t *testing.T, snap config.ExternalQuotas, k config.KueueQuotaSource) *ResourceConstraints {
	t.Helper()
	e := config.QuotaLimiterConfig{
		Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
		Kueue:           &k,
	}
	src := &fakeQuotaSource{}
	src.push(snap, nil)
	inv := NewQuotaInventoryWithSource(e, src)
	inv.now = func() time.Time { return snap.ObservedAt.Add(10 * time.Second) }
	inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: noEligibilityLimits{}})
	active := map[string]map[string]int{}
	for ns := range snap.Borrow {
		active[ns] = map[string]int{}
	}
	c, err := NewDefaultLimiter(e.Name, inv).ComputeConstraints(context.Background(), nil, active)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A lender's unused nominal is borrowable only where Kueue would take it back
// for the lender: under Never it would not, and borrowing it would hold the
// lender's nominal until the borrower shrinks on its own.
func TestBorrowCountsOnlyWhatLendersCanReclaim(t *testing.T) {
	at := time.Unix(1000, 0)
	lender := func(name, reclaim string, unused int) config.ExternalLender {
		return config.ExternalLender{Queue: name, Reclaim: reclaim, Unused: unused}
	}

	// A2: lender-cannot-reclaim is counted-but-0, like lender-waiting: the
	// namespace stays counted, at its guaranteed figure (0: its nominal 8 is
	// lent to a peer it cannot reclaim from), borrowing nothing new. Dropped
	// from the counted set, it capped at nominal 8 and ordered growth into
	// GPUs a peer holds.
	t.Run("a lender on Never counts nothing; the namespace stays counted at guaranteed, as lender-cannot-reclaim", func(t *testing.T) {
		snap := lenderSnapshot(at, lender("batch-cq", kueue.ReclaimNever, 8))
		free, borrows, why := borrowingOf(t, snap)
		if free != 0 || !borrows {
			t.Errorf("counted %d GPUs of a lender that cannot reclaim (borrows %t), want 0 and still counted", free, borrows)
		}
		if why != string(kueue.ReasonLenderCannotReclaim) {
			t.Errorf("withheld as %q, want %q", why, kueue.ReasonLenderCannotReclaim)
		}
		c := constraintsOf(t, snap)
		if limit := c.NamespacePools["serve"]["H100"].Limit; limit != 0 {
			t.Errorf("serve's cap is %d, want its guaranteed 0, not its nominal 8", limit)
		}
		if b := c.NamespaceBorrow["serve"]["H100"]; !b.Frozen || b.Withheld != string(kueue.ReasonLenderCannotReclaim) {
			t.Errorf("serve's borrowing = %+v, want frozen at nothing new, as lender-cannot-reclaim", b)
		}
	})

	t.Run("a lender on Any counts what it lends", func(t *testing.T) {
		free, borrows, why := borrowingOf(t, lenderSnapshot(at, lender("batch-cq", kueue.ReclaimAny, 8)))
		if free != 8 || !borrows || why != "" {
			t.Errorf("counted %d (borrows %t, withheld %q), want 8, borrowing, not withheld", free, borrows, why)
		}
	})

	// K1: Kueue preempts, for a LowerPriority lender's incoming Workload, only
	// a borrowed Workload of lower priority. What such a lender lends is
	// counted, restricted to the models it outranks: a batch lender at 500 is
	// reclaimable from a canary at 0, never from serving at 1000, whichever
	// other Workloads the namespace runs.
	lowerPriority := func(lenderPriority *int32) config.ExternalQuotas {
		l := lender("batch-cq", kueue.ReclaimLowerPriority, 8)
		l.Priority = lenderPriority
		snap := lenderSnapshot(at, l)
		snap.ClassPriority = map[string]int32{"kueue.x-k8s.io/serving": 1000, "kueue.x-k8s.io/canary": 0}
		return snap
	}
	p := func(n int32) *int32 { return &n }
	t.Run("a LowerPriority lender's GPUs are drawable only by a model it outranks", func(t *testing.T) {
		free, borrows, why := borrowingOf(t, lowerPriority(p(500)))
		if free != 8 || !borrows || why != "" {
			t.Fatalf("counted %d (borrows %t, withheld %q), want 8 in the pool, restricted", free, borrows, why)
		}
		chunk := chunkOf(t, lowerPriority(p(500)))
		if got := chunk.drawable("kueue.x-k8s.io/canary", nil); got != 8 {
			t.Errorf("a canary at 0 may draw %d, want 8: Kueue reclaims from it for the lender at 500", got)
		}
		if got := chunk.drawable("kueue.x-k8s.io/serving", nil); got != 0 {
			t.Errorf("serving at 1000 may draw %d, want 0: Kueue never reclaims from it for the lender at 500", got)
		}
		if got := chunk.drawable("kueue.x-k8s.io/unseen", nil); got != 0 {
			t.Errorf("a class no Workload was read at may draw %d, want 0: its priority is unknown", got)
		}
		if got := chunk.drawable(kueue.PriorityClassNone, p(1000)); got != 0 {
			t.Errorf("a template naming no class, in a namespace running at 1000, may draw %d, want 0", got)
		}
		if got := chunk.drawable(kueue.PriorityClassNone, p(10)); got != 8 {
			t.Errorf("a template naming no class, in a namespace running at most 10, may draw %d, want 8", got)
		}
	})
	t.Run("a LowerPriority lender with no priority read counts nothing", func(t *testing.T) {
		free, borrows, why := borrowingOf(t, lowerPriority(nil))
		if free != 0 || !borrows || why != string(kueue.ReasonLenderCannotReclaim) {
			t.Errorf("counted %d (borrows %t, withheld %q), want 0, counted at guaranteed, withheld as %s",
				free, borrows, why, kueue.ReasonLenderCannotReclaim)
		}
	})

	t.Run("beside each other only the one on Any counts", func(t *testing.T) {
		free, borrows, why := borrowingOf(t, lenderSnapshot(at,
			lender("any-cq", kueue.ReclaimAny, 8), lender("never-cq", kueue.ReclaimNever, 8)))
		if free != 8 || !borrows || why != "" {
			t.Errorf("counted %d of 16 (borrows %t, withheld %q), want the 8 of the lender on Any", free, borrows, why)
		}
	})
}
