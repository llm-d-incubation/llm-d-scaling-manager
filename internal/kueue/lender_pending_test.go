package kueue

import (
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Review finding (security, Kueue maintainer): a lender counted as waiting
// whenever Kueue reported any pending Workload, so a CPU job, or one too
// large to admit, made the borrowers give back capacity for nothing; and the
// release was bounded by everything lent out, not by what the lender asked
// for.
var _ = Describe("Lender waiting from its pending Workloads", func() {
	cpu := PendingWorkload{GPUs: nil}
	amd := PendingWorkload{GPUs: map[string]int64{"amd.com/gpu": 2}}
	lender := func(pending ...PendingWorkload) QueueState {
		q := member("lender", QueueResource{Nominal: 8, Reserved: 3}) // 5 unused
		q.PendingWorkloads = pending
		return q
	}

	DescribeTable("what it waits for",
		func(q QueueState, want int) {
			Expect(lenderPending(q, gpuKey)).To(Equal(want))
			Expect(LenderWaiting(q, gpuKey)).To(Equal(want > 0))
		},
		Entry("a CPU-only Workload is not waiting for GPUs", lender(cpu), 0),
		Entry("another resource's Workload is not waiting for this one", lender(amd), 0),
		Entry("a Workload larger than the unused nominal would have to borrow itself", lender(pendingGPU(6, 0)), 0),
		Entry("the largest Workload that fits is what it waits for", lender(pendingGPU(2, 0), pendingGPU(5, 0), pendingGPU(9, 0)), 5),
	)

	// PR-C review (KC1, KC4, KC5): a release to a waiting lender is sized by
	// the least that unblocks it -- the smallest Workload Kueue could admit
	// next -- at the priority Kueue would preempt for it with, leaving out
	// what handing capacity back would not admit.
	now := time.Unix(90000, 0)
	at := func(w PendingWorkload, since time.Time) PendingWorkload {
		w.Since = since
		return w
	}
	requeued := func(w PendingWorkload, until time.Time) PendingWorkload {
		w.RequeueAt = &until
		return w
	}
	strict := func(q QueueState) QueueState {
		q.QueueingStrategy = QueueingStrictFIFO
		return q
	}
	p := func(n int32) *int32 { return &n }
	preempting := func(q QueueState, policy string, lowest int32) QueueState {
		q.WithinClusterQueue, q.LowestReserved = policy, p(lowest)
		return q
	}
	DescribeTable("what a release to it is sized by",
		func(q QueueState, gpus int, prio *int32) {
			d := lenderNext(q, gpuKey, nil, now)
			Expect(d.gpus).To(Equal(gpus))
			Expect(d.priority).To(Equal(prio))
		},
		Entry("CPU and other resources ask nothing", lender(cpu, amd), 0, nil),
		Entry("the smallest Workload that fits, at the highest priority of those that fit",
			lender(pendingGPU(4, 5), pendingGPU(2, 1), pendingGPU(9, 99)), 2, p(5)),
		Entry("one in requeue backoff is not in line yet",
			lender(requeued(pendingGPU(2, 0), now.Add(time.Minute)), pendingGPU(4, 0)), 4, p(0)),
		Entry("one whose backoff is over is in line again",
			lender(requeued(pendingGPU(2, 0), now.Add(-time.Minute)), pendingGPU(4, 0)), 2, p(0)),
		Entry("StrictFIFO: a head that does not fit blocks those behind it",
			strict(lender(at(pendingGPU(9, 0), now.Add(-time.Hour)), at(pendingGPU(2, 0), now))), 0, nil),
		Entry("StrictFIFO: the head alone counts",
			strict(lender(at(pendingGPU(4, 0), now.Add(-time.Hour)), at(pendingGPU(2, 0), now))), 4, p(0)),
		Entry("StrictFIFO: the head is the highest priority, whatever its age",
			strict(lender(at(pendingGPU(4, 0), now.Add(-time.Hour)), at(pendingGPU(2, 9), now))), 2, p(9)),
		Entry("a Workload it admits by preempting its own lower priority ones is not the cohort's to serve",
			preempting(lender(pendingGPU(2, 50), pendingGPU(4, 10)), WithinLowerPriority, 10), 4, p(10)),
		Entry("LowerOrNewerEqualPriority preempts at equal priority too",
			preempting(lender(pendingGPU(4, 10)), WithinLowerOrNewerEqualPriority, 10), 0, nil),
	)

	It("fits a Workload by the same rule as the waiting signal: unused plus kept", func() {
		r := QueueResource{Nominal: 32, LendingLimit: ptr(16), Reserved: 4}
		Expect(r.unused()).To(Equal(16))
		Expect(r.keptFree()).To(Equal(12))
		Expect(r.admits(28)).To(BeTrue(), "N - U = 28")
		Expect(r.admits(29)).To(BeFalse())
	})

	// PR #203 re-check (KC4): Kueue reclaims for a LowerPriority lender from
	// any borrower below its head, this controller's or not.
	It("carries the lowest priority a borrower holds the pool at, unknown when one is not read", func() {
		a := member("a", QueueResource{Nominal: 0, Reserved: 4, Borrowed: 4})
		a.LowestReserved = p(10)
		b := member("b", QueueResource{Nominal: 0, Reserved: 4, Borrowed: 4})
		b.LowestReserved = p(200)
		pool := ComputeBorrowing(topology(member("lender", QueueResource{Nominal: 8}), a, b), false).Pools[0]
		Expect(pool.BorrowerLowest).To(HaveValue(Equal(int32(10))))
		b.LowestReserved = nil
		pool = ComputeBorrowing(topology(member("lender", QueueResource{Nominal: 8}), a, b), false).Pools[0]
		Expect(pool.BorrowerLowest).To(BeNil())
	})

	// PR #203 final check: what preemption victims hold is waited for on the
	// lender's flavor alone, and for EvictionBound at most.
	It("counts a victim's quota on its own flavor, and not once it is stuck past the bound", func() {
		victimOf := func(flavor string, age time.Duration) QueueState {
			q := member("a", QueueResource{Nominal: 0, Reserved: 8, Borrowed: 8})
			q.Evicting = []EvictingWorkload{{Since: now.Add(-age),
				GPUs: map[ResourceKey]int64{{Flavor: flavor, Resource: gpuKey.Resource}: 8}}}
			return q
		}
		pool := func(q QueueState) SharedPool {
			t := topology(member("lender", QueueResource{Nominal: 8}), q)
			t.Now = now
			return ComputeBorrowing(t, false).Pools[0]
		}
		Expect(pool(victimOf("h100", 5*time.Minute)).EvictingGPUs).To(Equal(8), "a victim stopping on this flavor")
		Expect(pool(victimOf("", 5*time.Minute)).EvictingGPUs).To(Equal(8), "no flavor named: counted, the cautious side")
		Expect(pool(victimOf("a100", 5*time.Minute)).EvictingGPUs).To(BeZero(), "a victim on another flavor frees nothing here")
		stuck := pool(victimOf("h100", EvictionBound+time.Minute))
		Expect(stuck.EvictingGPUs).To(BeZero(), "stuck past the bound: no longer waited for")
		Expect(stuck.StuckEvictingGPUs).To(Equal(8))
	})

	It("carries the part it keeps unused: Kueue admits that much without a release", func() {
		l := member("lender", QueueResource{Nominal: 32, LendingLimit: ptr(16)})
		l.PendingWorkloads = []PendingWorkload{pendingGPU(16, 0)}
		b := ComputeBorrowing(topology(l, member("a", QueueResource{Nominal: 0, Reserved: 16, Borrowed: 16})), false)
		Expect(b.Pools).To(HaveLen(1))
		got := b.Pools[0].Lenders[slices.IndexFunc(b.Pools[0].Lenders, func(s LenderState) bool { return s.Queue == "lender" })]
		Expect(got.Kept).To(Equal(16))
		Expect(got.PendingGPUs).To(Equal(16))
	})

	lenderOf := func(qs ...QueueState) LenderState {
		b := ComputeBorrowing(topology(qs...), false)
		Expect(b.Pools).To(HaveLen(1))
		for _, ls := range b.Pools[0].Lenders {
			if ls.Queue == "lender" {
				return ls
			}
		}
		Fail("no lender")
		return LenderState{}
	}
	borrower := member("a", QueueResource{Nominal: 0, Reserved: 4, Borrowed: 4})

	It("carries the lender's priority as read, for the reclaim judgement", func() {
		l := withReclaim(lender(pendingGPU(2, 1000), pendingGPU(9, 50)), ReclaimLowerPriority)
		l.LenderPriority = p(50)
		got := lenderOf(l, borrower)
		Expect(got).To(Equal(LenderState{Queue: "lender", Waiting: true, Reclaim: ReclaimLowerPriority,
			Unused: 5, Priority: p(50), PendingGPUs: 2, PendingPriority: p(1000)}), "the 9-GPU Workload does not fit the 5 unused")
	})

	It("carries no priority when none of its Workloads was read", func() {
		Expect(lenderOf(withReclaim(lender(), ReclaimLowerPriority), borrower).Priority).To(BeNil())
	})
})
