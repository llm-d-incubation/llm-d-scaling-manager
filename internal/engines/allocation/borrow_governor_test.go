package allocation

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// govWorld is a cohort "c" with one h100 chunk read at readAt, the given
// lenders, and borrowing namespace "b" guaranteed g and using used.
type govWorld struct {
	readAt  time.Time
	stale   bool
	lenders []kueue.LenderState
	g, used int
	flavor  string
}

func (w govWorld) constraints() []*ResourceConstraints {
	c := borrowQuota(0, nsBorrow{ns: "b", guaranteed: w.g, used: w.used, cap: config.QuotaUnlimited})
	flavor := w.flavor
	if flavor == "" {
		flavor = "h100"
	}
	c.SharedPools[0].Flavor = flavor
	if flavor == "a100" {
		c.SharedPools[0].AcceleratorType = "NVIDIA-A100-SXM4-80GB"
	}
	c.SharedPools[0].Lenders = w.lenders
	c.SharedPools[0].ReadAt = w.readAt
	c.SharedPools[0].Stale = w.stale
	return []*ResourceConstraints{c}
}

// never is a waiting lender that cannot reclaim.
func never() kueue.LenderState {
	return kueue.LenderState{Queue: "lender", Waiting: true, Reclaim: kueue.ReclaimNever}
}

// seenPast runs c once at an earlier time, so the namespaces c sees past
// guaranteed are known past it since then: a governor never told counts the
// placement timeout from its first cycle (observePast).
func seenPast(g *BorrowGovernor, c BorrowCycle, at time.Time) {
	c.Now = at
	g.Observe(c)
}

// decisionsOf are no-change decisions for every variant of the requests.
func decisionsOf(reqs []ModelScalingRequest) []domain.VariantDecision {
	out := make([]domain.VariantDecision, 0, len(reqs))
	for _, r := range reqs {
		for _, s := range r.VariantStates {
			out = append(out, domain.VariantDecision{Namespace: r.Namespace, ModelID: r.ModelID, VariantName: s.VariantName,
				CurrentReplicas: s.CurrentReplicas, TargetReplicas: s.CurrentReplicas, GPUsPerReplica: s.GPUsPerReplica})
		}
	}
	return out
}

func targets(ds []domain.VariantDecision) map[string]int {
	out := map[string]int{}
	for _, d := range ds {
		out[d.VariantName] = d.TargetReplicas
	}
	return out
}

// govModel is a single-variant H100 model in ns "b" at replicas of gpr GPUs.
func govModel(id string, replicas, gpr int) ModelScalingRequest {
	r := borrowModel("b", id, gpr, replicas, 0, nil)
	r.VariantStates[0].AcceleratorName = "H100"
	return r
}

// pdModel is a P/D model in ns "b": one prefill and one decode replica of
// gpr GPUs each.
func pdModel(gpr int) ModelScalingRequest {
	return withSatEntry(&satEntryFixture{
		VariantCapacities: []vcFixture{
			{VariantName: "p", AcceleratorName: "H100", Cost: 5, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
			{VariantName: "d", AcceleratorName: "H100", Cost: 5, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 10000},
		},
	}, ModelScalingRequest{ModelID: "pd", Namespace: "b", Disaggregated: true, Priority: 1,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "p", CurrentReplicas: 1, GPUsPerReplica: gpr, Role: "prefill", AcceleratorName: "H100"},
			{VariantName: "d", CurrentReplicas: 1, GPUsPerReplica: gpr, Role: "decode", AcceleratorName: "H100"},
		}})
}

var _ = Describe("BorrowGovernor", func() {
	t0 := time.Unix(10000, 0)
	settings := DefaultBorrowSettings()

	// run feeds the governor one cycle and returns the releases.
	run := func(g *BorrowGovernor, w govWorld, now time.Time, reqs []ModelScalingRequest) []BorrowRelease {
		c := BorrowCycle{Now: now, Constraints: w.constraints(), Requests: reqs}
		g.Observe(c)
		return g.Releases(c, decisionsOf(reqs))
	}
	// warm takes a fresh governor past the restart entry and the debounce:
	// reads at t0, t0+30s, t0+61s, the lender waiting throughout.
	warm := func(w govWorld, reqs []ModelScalingRequest) (*BorrowGovernor, []BorrowRelease, time.Time) {
		g := NewBorrowGovernor(settings)
		var rel []BorrowRelease
		for _, d := range []time.Duration{0, 30 * time.Second, 61 * time.Second} {
			w.readAt = t0.Add(d)
			rel = run(g, w, t0.Add(d), reqs)
		}
		return g, rel, t0.Add(61 * time.Second)
	}

	Describe("the debounced lender rule", func() {
		// B2: the first sighting of a waiting lender stops new borrowing in
		// its cohort; the debounce applies only to the alert (WaitingLenders)
		// and the cohort cooldown.
		It("blocks new borrowing at the first sighting, and counts the wait only after two fresh reads 60s apart", func() {
			g := NewBorrowGovernor(settings)
			reqs := []ModelScalingRequest{govModel("m", 4, 8)}
			w := govWorld{lenders: []kueue.LenderState{never()}, used: 32}
			for _, d := range []time.Duration{0, 30 * time.Second} {
				w.readAt = t0.Add(d)
				run(g, w, t0.Add(d), reqs)
				Expect(g.WaitingLenders(t0.Add(d))).To(BeEmpty(), d.String())
				Expect(g.BlockedCohorts(t0.Add(d))).To(HaveKey("c"), "raw signal, "+d.String())
			}
			w.readAt = t0.Add(61 * time.Second)
			run(g, w, t0.Add(61*time.Second), reqs)
			Expect(g.WaitingLenders(t0.Add(61 * time.Second))).To(HaveLen(1))
			Expect(g.BlockedCohorts(t0.Add(61 * time.Second))).To(HaveKey("c"))

			// The lender stops waiting: the raw block lifts at once, the
			// cooldown its debounced wait started stays.
			w.lenders = []kueue.LenderState{{Queue: "lender", Reclaim: kueue.ReclaimNever}}
			w.readAt = t0.Add(90 * time.Second)
			run(g, w, t0.Add(90*time.Second), reqs)
			Expect(g.BlockedCohorts(t0.Add(90*time.Second))).To(HaveKey("c"), "the cohort cooldown after the wait is kept")
			Expect(g.BlockedCohorts(t0.Add(61*time.Second + settings.Cooldown))).To(BeEmpty())

			fresh := NewBorrowGovernor(settings)
			run(fresh, govWorld{lenders: []kueue.LenderState{{Queue: "lender"}}, used: 32, readAt: t0}, t0, reqs)
			Expect(fresh.BlockedCohorts(t0)).To(BeEmpty(), "control: no lender waits, nothing blocked")
		})

		It("hands nothing back to a waiting lender that cannot reclaim, and closes the cohort to new borrowing", func() {
			g, rel, now := warm(govWorld{lenders: []kueue.LenderState{never()}, used: 32},
				[]ModelScalingRequest{govModel("m", 4, 8)})
			Expect(rel).To(BeEmpty(), "what it lent was never counted as borrowable")
			Expect(g.BlockedCohorts(now)).To(HaveKey("c"), "no new borrowing while the lender waits")
			Expect(g.WaitingLenders(now)).To(HaveLen(1), "the wait is reported")
			reqs := []ModelScalingRequest{govModel("m", 4, 8)}
			c := BorrowCycle{Now: now, Constraints: govWorld{lenders: []kueue.LenderState{never()}, used: 32, readAt: now}.constraints(),
				Requests: reqs}
			g.Observe(c)
			ds := decisionsOf(reqs)
			g.Releases(c, ds)
			Expect(targets(ds)["m-v"]).To(Equal(4), "and nothing pins a variant")
		})

		It("releases nothing for a lender that can reclaim, and closes the cohort to new borrowing", func() {
			l := never()
			l.Reclaim = kueue.ReclaimAny
			g, rel, now := warm(govWorld{lenders: []kueue.LenderState{l}, used: 32}, []ModelScalingRequest{govModel("m", 4, 8)})
			Expect(rel).To(BeEmpty(), "Kueue preempts for it")
			Expect(g.BlockedCohorts(now)).To(HaveKey("c"))
		})
	})

	It("freezes the lender's wait on a stale read", func() {
		reqs := []ModelScalingRequest{govModel("m", 4, 8)}
		w := govWorld{lenders: []kueue.LenderState{never()}, used: 32}
		g, _, now := warm(w, reqs)
		w.stale = true
		w.lenders = []kueue.LenderState{{Queue: "lender"}} // the frozen read
		w.readAt = now.Add(time.Hour)
		for i := 0; i < 3; i++ {
			now = now.Add(30 * time.Second)
			run(g, w, now, reqs)
		}
		Expect(g.WaitingLenders(now)).To(HaveLen(1), "neither reset nor cleared while stale")
	})

	Describe("cooldowns", func() {
		It("keeps the cohort closed for the cooldown after its lender stopped waiting", func() {
			w := govWorld{lenders: []kueue.LenderState{never()}, used: 32}
			g, _, now := warm(w, []ModelScalingRequest{govModel("m", 4, 8)})
			w.lenders = []kueue.LenderState{{Queue: "lender"}}
			w.readAt = now.Add(30 * time.Second)
			run(g, w, now.Add(30*time.Second), []ModelScalingRequest{govModel("m", 3, 8)})
			Expect(g.WaitingLenders(now.Add(30*time.Second))).To(BeEmpty(), "reset when the lender stops waiting")
			Expect(g.BlockedCohorts(now.Add(9 * time.Minute))).To(HaveKey("c"))
			Expect(g.BlockedCohorts(now.Add(11 * time.Minute))).To(BeEmpty())
		})

		It("blocks a model's new borrowing for the cooldown after a preemption", func() {
			g := NewBorrowGovernor(settings)
			g.MarkPreempted("b/m", t0)
			Expect(g.BorrowBlocked("b", "m", "m-v", t0.Add(9*time.Minute))).To(BeTrue())
			Expect(g.BorrowBlocked("b", "m", "m-v", t0.Add(11*time.Minute))).To(BeFalse())
		})
	})

	Describe("placement", func() {
		unplaced := func(r ModelScalingRequest, variant int, since time.Time) ModelScalingRequest {
			r.VariantStates[variant].UnplacedReplicas = 1
			r.VariantStates[variant].UnplacedSince = since
			return r
		}

		It("holds new borrowed growth of a variant with unplaced replicas", func() {
			g := NewBorrowGovernor(settings)
			g.Observe(BorrowCycle{Now: t0, Requests: []ModelScalingRequest{unplaced(govModel("m", 2, 8), 0, t0)}})
			Expect(g.BorrowBlocked("b", "m", "m-v", t0)).To(BeTrue())
			Expect(g.BorrowBlocked("b", "other", "other-v", t0)).To(BeFalse())
		})

		It("releases a borrowed unit unplaced past the timeout with the partner that placed, and starts the cooldown", func() {
			g := NewBorrowGovernor(settings)
			pd := unplaced(pdModel(8), 1, t0) // decode unplaced, prefill placed
			c := BorrowCycle{Now: t0.Add(6 * time.Minute), Constraints: govWorld{used: 16}.constraints(), Requests: []ModelScalingRequest{pd}}
			seenPast(g, c, t0)
			g.Observe(c)
			ds := decisionsOf(c.Requests)
			rel := g.Releases(c, ds)
			Expect(rel).To(HaveLen(2))
			Expect(targets(ds)).To(Equal(map[string]int{"p": 0, "d": 0}))
			Expect(g.BorrowBlockedIn("b", "pd", "c", t0.Add(7*time.Minute))).To(BeTrue(), "the model cools down in its cohort")
		})

		It("waits while the cluster autoscaler is scaling up, at most three times the timeout", func() {
			g := NewBorrowGovernor(settings)
			r := unplaced(govModel("m", 2, 8), 0, t0)
			pending := map[string]bool{"b/m-v": true}
			cycle := func(at time.Duration) []BorrowRelease {
				c := BorrowCycle{Now: t0.Add(at), Constraints: govWorld{used: 16}.constraints(), Requests: []ModelScalingRequest{r}, ScaleUpPending: pending}
				g.Observe(c)
				return g.Releases(c, decisionsOf(c.Requests))
			}
			Expect(cycle(0)).To(BeEmpty())
			Expect(cycle(6*time.Minute)).To(BeEmpty(), "TriggeredScaleUp: the timeout waits")
			Expect(cycle(16*time.Minute)).To(HaveLen(1), "capped at three times the timeout")

			g = NewBorrowGovernor(settings)
			pending = nil // a later NotTriggerScaleUp ends the wait
			Expect(cycle(0)).To(BeEmpty())
			Expect(cycle(6 * time.Minute)).To(HaveLen(1))
		})

		It("exempts growth within guaranteed, a reclaiming queue's growth below nominal included", func() {
			g := NewBorrowGovernor(settings)
			c := BorrowCycle{Now: t0.Add(time.Hour), Constraints: govWorld{used: 16, g: 16}.constraints(),
				Requests: []ModelScalingRequest{unplaced(govModel("m", 2, 8), 0, t0)}}
			g.Observe(c)
			Expect(g.Releases(c, decisionsOf(c.Requests))).To(BeEmpty(), "Kueue reclaims for it")
		})

		// The live-Kueue e2e (kueue_borrow_test.go, spec 4): nominal 2, queue
		// a reclaims (Any) and already reserves its whole nominal, and Kueue
		// preempted a borrowed pod -- the ReplicaSet's replacement is gated.
		// The namespace holds 3 replicas of 1 GPU, 1 of them gated: the gated
		// one is past guaranteed, Kueue has nothing to reclaim for it, and it
		// must go after the timeout.
		gatedReplacement := func(lentOut, used int) BorrowCycle {
			r := govModel("m", used, 1)
			r = unplaced(r, 0, t0)
			c := borrowQuota(0, nsBorrow{ns: "b", guaranteed: 2, used: used, cap: config.QuotaUnlimited, lentOut: lentOut})
			return BorrowCycle{Now: t0.Add(settings.PlacementTimeout + time.Second),
				Constraints: []*ResourceConstraints{c}, Requests: []ModelScalingRequest{r}}
		}

		It("releases a gated replica past guaranteed when its queue reserves its full nominal", func() {
			g := NewBorrowGovernor(settings)
			c := gatedReplacement(0, 3)
			seenPast(g, c, t0)
			g.Observe(c)
			ds := decisionsOf(c.Requests)
			rel := g.Releases(c, ds)
			Expect(rel).To(HaveLen(1), "the gated replacement of a preempted borrowed pod is borrowed")
			Expect(rel[0].Cause).To(Equal(ReleasePlacementTimeout))
			Expect(rel[0].From).To(Equal(3))
			Expect(rel[0].To).To(Equal(2))
			Expect(targets(ds)).To(Equal(map[string]int{"m-v": 2}))
			Expect(g.BorrowBlocked("b", "m", "m-v", c.Now.Add(time.Minute))).To(BeTrue(), "the model's cooldown starts")
		})

		It("exempts a gated replica within guaranteed while its reclaiming queue has nominal lent out", func() {
			g := NewBorrowGovernor(settings)
			c := gatedReplacement(1, 2) // holds 2 of 2, 1 gated: a peer holds 1 of a's nominal
			g.Observe(c)
			Expect(g.Releases(c, decisionsOf(c.Requests))).To(BeEmpty(), "Kueue reclaims the lent-out GPU for it")
		})
	})

	It("stops the greedy optimizer from borrowing for a blocked variant, not from growing within guaranteed", func() {
		o := NewGreedyByScoreOptimizer()
		o.BorrowBlocked = func(_, model, _ string) bool { return model == "mb" }
		got := addedGPUs(o.Optimize(context.Background(), []ModelScalingRequest{
			borrowModel("g", "mg", 8, 0, 1, nil),
			borrowModel("b", "mb", 8, 0, 1, nil),
		}, []*ResourceConstraints{physicalH100(64), borrowQuota(16,
			nsBorrow{ns: "g", guaranteed: 8, cap: config.QuotaUnlimited},
			nsBorrow{ns: "b", cap: config.QuotaUnlimited})}))
		Expect(got).To(Equal(map[string]int{"mg": 8}))
	})
})
