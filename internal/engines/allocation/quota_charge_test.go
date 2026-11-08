package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// chargeRow is queue a's H100 row of figures f as the reader reports it, with
// reserved what Kueue has charged the queue.
func chargeRow(cohort string, f kueue.QueueFigures, reserved int) config.ExternalBorrow {
	reason := f.Reason
	if reason == "" {
		reason = f.GuaranteedReason
	}
	return config.ExternalBorrow{Queue: "a", Cohort: cohort, Flavor: "h100", Resource: "nvidia.com/gpu", Accelerator: "H100",
		Nominal: f.Nominal, Guaranteed: f.Guaranteed, Borrowable: f.Borrowable, BorrowCap: f.BorrowCap,
		WithinNominal: f.WithinNominal, LentOut: f.LentOut, PreemptAfter: f.PreemptAfter, PreemptKept: f.PreemptKept,
		SourceBorrowed: f.KueueBorrowed, Reason: string(reason), Reserved: reserved}
}

// chargeSnapshot is a read granting row's queue to every namespace of nss.
func chargeSnapshot(row config.ExternalBorrow, free int, nss ...string) config.ExternalQuotas {
	caps := func(n int) config.ExternalCaps { return config.ExternalCaps{ByType: map[string]int{"H100": n}} }
	s := config.ExternalQuotas{
		Namespace: map[string]config.ExternalCaps{}, NamespaceGuaranteed: map[string]config.ExternalCaps{},
		NamespaceWithinNominal: map[string]config.ExternalCaps{}, Borrow: map[string][]config.ExternalBorrow{},
		ObservedAt: time.Now(),
	}
	for _, ns := range nss {
		s.Namespace[ns], s.NamespaceGuaranteed[ns], s.NamespaceWithinNominal[ns] = caps(row.Nominal), caps(row.Guaranteed), caps(row.WithinNominal)
		s.Borrow[ns] = []config.ExternalBorrow{row}
	}
	if row.Cohort != "" {
		s.SharedPools = []config.ExternalSharedPool{{Cohort: row.Cohort, Flavor: "h100", Resource: "nvidia.com/gpu",
			Accelerator: "H100", Free: free}}
	}
	return s
}

// KU1: a Kueue figure is charged with what Kueue reserved for workloads the
// scaling manager does not manage. Queue a (nominal 16) is serving's; an
// unmanaged training job holds 8 of it. Serving holds what managed says and
// asks for two more 8-GPU replicas. Kueue admits only what the queue has left
// after everything it reserved, so the quota may order no more. Before the
// fix the figure was charged with the managed GPUs alone: the training job's
// 8 read as free, and the replicas ordered there waited gated for good.
func TestKueueFigureChargedWithNonManagedReservation(t *testing.T) {
	never := figuresOf(t, h100Queue("a", kueue.ReclaimNever, kueue.QueueResource{Nominal: 16, Reserved: 8}),
		h100Queue("b", kueue.ReclaimNever, kueue.QueueResource{Nominal: 0, Reserved: 8, Borrowed: 8}))
	anyQ := figuresOf(t, h100Queue("a", kueue.ReclaimAny, kueue.QueueResource{Nominal: 16, Reserved: 8}),
		h100Queue("b", kueue.ReclaimNever, kueue.QueueResource{Nominal: 0, Reserved: 8, Borrowed: 8}))
	noCohort := kueue.QueueFigures{Nominal: 16, Guaranteed: 16, WithinNominal: 16, BorrowCap: config.QuotaUnlimited,
		Reason: kueue.ReasonNoCohort}
	for _, tc := range []struct {
		name     string
		snap     config.ExternalQuotas
		held     int // GPUs serving's variants hold
		want     int
		reserved int
	}{
		{"Never, training 8, peers borrow 8, serving 0: no replica", chargeSnapshot(chargeRow("c", never, 8), never.CohortFree, "serving"), 0, 0, 8},
		{"Any, training 8, peers borrow 8, serving 0: one replica, Kueue reclaims 8", chargeSnapshot(chargeRow("c", anyQ, 8), anyQ.CohortFree, "serving"), 0, 8, 8},
		{"no cohort, training 8, serving 0: one replica", chargeSnapshot(chargeRow("", noCohort, 8), 0, "serving"), 0, 8, 8},
		{"queue shared with an unmanaged namespace holding 8: one replica", chargeSnapshot(chargeRow("", noCohort, 8), 0, "serving", "training"), 0, 8, 8},
		{"no cohort, training 8, serving holds 8 of its own: nothing left", chargeSnapshot(chargeRow("", noCohort, 16), 0, "serving"), 8, 0, 16},
		{"control: serving holds the queue's only 8: one replica", chargeSnapshot(chargeRow("", noCohort, 8), 0, "serving"), 8, 8, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := config.QuotaLimiterConfig{Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
				Kueue:           &config.KueueQuotaSource{Enabled: true}}
			src := &fakeQuotaSource{}
			src.push(tc.snap, nil)
			inv := NewQuotaInventoryWithSource(e, src)
			inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: &fakeEligibility{}})
			c, err := NewDefaultLimiter(e.Name, inv).ComputeConstraints(context.Background(), nil,
				map[string]map[string]int{"serving": {"H100": tc.held}})
			if err != nil {
				t.Fatal(err)
			}
			ds := NewGreedyByScoreOptimizer().Optimize(context.Background(),
				[]ModelScalingRequest{borrowModel("serving", "m", 8, tc.held/8, 2, nil)}, []*ResourceConstraints{c})
			if got := addedGPUs(ds)["m"]; got != tc.want {
				t.Fatalf("ordered %d GPUs, want %d (Kueue reserved %d, serving holds %d; pool %+v)",
					got, tc.want, tc.reserved, tc.held, c.NamespacePools["serving"])
			}
		})
	}
}

// 186-1: the charge is per namespace and queue. One ClusterQueue of 16 is
// shared by ns1, holding nothing, and ns2, holding all 16. Kueue admits
// nothing more through it, for either. Before, the managed GPUs of both
// namespaces were pooled per queue: the queue's charge read 0, and ns1 was
// given 16 GPUs that ns2 holds.
func TestKueueChargePerNamespaceInSharedQueue(t *testing.T) {
	noCohort := kueue.QueueFigures{Nominal: 16, Guaranteed: 16, WithinNominal: 16, BorrowCap: config.QuotaUnlimited,
		Reason: kueue.ReasonNoCohort}
	e := config.QuotaLimiterConfig{Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
		Kueue:           &config.KueueQuotaSource{Enabled: true}}
	src := &fakeQuotaSource{}
	src.push(chargeSnapshot(chargeRow("", noCohort, 16), 0, "ns1", "ns2"), nil)
	inv := NewQuotaInventoryWithSource(e, src)
	inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: &fakeEligibility{}})
	c, err := NewDefaultLimiter(e.Name, inv).ComputeConstraints(context.Background(), nil,
		map[string]map[string]int{"ns1": {}, "ns2": {"H100": 16}})
	if err != nil {
		t.Fatal(err)
	}
	ds := NewGreedyByScoreOptimizer().Optimize(context.Background(), []ModelScalingRequest{
		borrowModel("ns1", "m1", 8, 0, 2, nil), borrowModel("ns2", "m2", 8, 2, 1, nil)}, []*ResourceConstraints{c})
	if got := addedGPUs(ds); got["m1"] != 0 || got["m2"] != 0 {
		t.Fatalf("ordered ns1 %d and ns2 %d GPUs, want 0 and 0: ns2 holds the queue's 16 (pools %+v)",
			got["m1"], got["m2"], c.NamespacePools)
	}
}

// multiQueueSnapshot is a read granting ns every row, one per queue, with the
// namespace figures each type's queues give: one queue's figure, or the
// smallest of several (the reader's minCaps).
func multiQueueSnapshot(ns string, rows ...config.ExternalBorrow) config.ExternalQuotas {
	caps := config.ExternalCaps{ByType: map[string]int{}}
	for _, r := range rows {
		if n, seen := caps.ByType[r.Accelerator]; seen {
			caps.ByType[r.Accelerator] = min(n, r.Nominal)
			continue
		}
		caps.ByType[r.Accelerator] = r.Nominal
	}
	return config.ExternalQuotas{
		Namespace: map[string]config.ExternalCaps{ns: caps}, NamespaceGuaranteed: map[string]config.ExternalCaps{ns: caps},
		NamespaceWithinNominal: map[string]config.ExternalCaps{ns: caps},
		Borrow:                 map[string][]config.ExternalBorrow{ns: rows}, ObservedAt: time.Now(),
	}
}

// queueRow is one queue's row of nominal 16 GPUs of acc, reserved r, in no
// cohort; withheld as multi-queue-namespace when ambiguous.
func queueRow(queue, acc string, r int, ambiguous bool) config.ExternalBorrow {
	const n = 16
	row := config.ExternalBorrow{Queue: queue, Flavor: queue + "-flavor", Resource: "nvidia.com/gpu", Accelerator: acc,
		Nominal: n, Guaranteed: n, WithinNominal: n, BorrowCap: config.QuotaUnlimited, Reason: string(kueue.ReasonNoCohort),
		Reserved: r}
	if ambiguous {
		row = row.Withheld(string(kueue.ReasonMultiQueueNamespace))
	}
	return row
}

// 186-2: a namespace on several GPU queues is decided per accelerator type.
//   - cq-h grants 16 H100 and cq-a 16 A100; the namespace holds 8 H100. Each
//     type has one queue, so its 8 H100 count in cq-h: Kueue has 8 left there.
//     Before, a namespace on two queues counted its GPUs in none, so cq-h's
//     charge read 8 and its headroom 16 - 8 - 8 = 0.
//   - cq-1 and cq-2 both grant 16 H100; the namespace holds 8, in cq-2. Which
//     queue a replica goes through is not read: min_q(N_q - R_q) + M =
//     min(16, 8) + 8 = 16, headroom 8. Before, the larger charge (8) came off
//     the smaller figure and the headroom read 0.
func TestMultiQueueNamespaceChargedPerType(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap config.ExternalQuotas
	}{
		{"one queue per type", multiQueueSnapshot("serving",
			queueRow("cq-h", "H100", 8, false), queueRow("cq-a", "A100", 0, false))},
		{"two queues grant H100", multiQueueSnapshot("serving",
			queueRow("cq-1", "H100", 0, true), queueRow("cq-2", "H100", 8, true))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := config.QuotaLimiterConfig{Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1, "A100": -1}},
				Kueue:           &config.KueueQuotaSource{Enabled: true}}
			src := &fakeQuotaSource{}
			src.push(tc.snap, nil)
			inv := NewQuotaInventoryWithSource(e, src)
			inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: &fakeEligibility{}})
			c, err := NewDefaultLimiter(e.Name, inv).ComputeConstraints(context.Background(), nil,
				map[string]map[string]int{"serving": {"H100": 8}})
			if err != nil {
				t.Fatal(err)
			}
			ds := NewGreedyByScoreOptimizer().Optimize(context.Background(),
				[]ModelScalingRequest{borrowModel("serving", "m", 8, 1, 2, nil)}, []*ResourceConstraints{c})
			if got := addedGPUs(ds)["m"]; got != 8 {
				t.Fatalf("ordered %d GPUs, want 8: Kueue has 8 H100 left for the namespace (pool %+v)",
					got, c.NamespacePools["serving"])
			}
		})
	}
}

// gatedAt is a GatedSource reporting fixed gated GPUs.
type gatedAt map[string]map[string]int

func (g gatedAt) GatedGPUs(time.Time) map[string]map[string]int { return g }

// KU1, release half: a replica already gated does not cancel the charge.
// Queue a (16, in cohort c): an unmanaged training job holds 8, serving 8
// admitted and 8 more at Kueue's gate, so serving's variants hold 16 and
// Kueue reserved 16. Kueue will never admit the gated replica. Its GPUs are
// not Kueue's reservation, so the charge stays at the training job's 8:
// serving's figure is 8, and its 16 are 8 past it -- held past guaranteed,
// which the placement timeout releases (borrowedRelease). Before, the
// managed 16 cancelled the whole charge: the figure read 16, nothing was
// past it, and the gated replica waited for good.
func TestGatedReplicaDoesNotCancelTheCharge(t *testing.T) {
	f := kueue.QueueFigures{Nominal: 16, Guaranteed: 16, WithinNominal: 16, BorrowCap: 0}
	e := config.QuotaLimiterConfig{Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
		Kueue:           &config.KueueQuotaSource{Enabled: true}}
	src := &fakeQuotaSource{}
	src.push(chargeSnapshot(chargeRow("c", f, 16), 0, "serving"), nil)
	inv := NewQuotaInventoryWithSource(e, src)
	inv.EnableBorrowing(BorrowOptions{Physical: true, Eligibility: &fakeEligibility{}})
	inv.SetGatedSource(gatedAt{"serving": {borrowH100: 8}})
	c, err := NewDefaultLimiter(e.Name, inv).ComputeConstraints(context.Background(), nil,
		map[string]map[string]int{"serving": {"H100": 16}})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.NamespacePools["serving"]["H100"].Limit; got != 8 {
		t.Errorf("serving's figure is %d, want 8: 16 less the training job's 8 (pool %+v)", got, c.NamespacePools["serving"])
	}
	if got := namespaceHeldPast([]*ResourceConstraints{c})["serving"]["H100"]; got != 8 {
		t.Errorf("serving holds %d GPUs past its figure, want 8: the gated replica, for the placement timeout to release", got)
	}
}

// KU1: the charge goes in before the static entry bounds the figure, so a
// static cap tighter than what Kueue leaves is not charged a second time.
func TestKueueChargeBeforeStaticBound(t *testing.T) {
	noCohort := kueue.QueueFigures{Nominal: 16, Guaranteed: 16, WithinNominal: 16, BorrowCap: config.QuotaUnlimited,
		Reason: kueue.ReasonNoCohort}
	e := config.QuotaLimiterConfig{Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"default": {"H100": 4}},
		Kueue:           &config.KueueQuotaSource{Enabled: true, CountBorrowable: new(bool)}}
	src := &fakeQuotaSource{}
	src.push(chargeSnapshot(chargeRow("", noCohort, 8), 0, "serving"), nil)
	inv := NewQuotaInventoryWithSource(e, src)
	inv.SetManagedUsage(map[string]map[string]int{"serving": {}})
	if err := inv.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := inv.EffectiveConfig().NamespaceQuotas["serving"]["H100"]; got != 4 {
		t.Fatalf("cap %d, want 4: min(static 4, Kueue 16 less the 8 it reserved for others)", got)
	}
}
