package floor

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Figures from the shape-swap P/D run (biran-20260915-102548-571), H200,
// Qwen3-0.6B, 6 req/s throughout. In its first phase (6000 in / 1000 out) one
// decode replica saturated at ~5.4 completions/s with 1.15M tokens resident;
// at six replicas the same load occupied ~100k tokens in total and the
// controller sized the fleet to one, which then re-saturated within a minute.
const (
	runLambda = 6.0
	runMu     = 5.4
	runK1     = int64(929_792) // 1,162,240 x 0.8
)

var _ = Describe("Estimate", func() {
	variants := func(role string, ready int) []domain.VariantCapacity {
		return []domain.VariantCapacity{{
			VariantName: "v", Role: role, ReplicaCount: ready, PerReplicaCapacity: float64(runK1),
		}}
	}
	// Readings from the variant's own bucket with enough samples to order;
	// the specs on holding build their own.
	replicas := func(n int) []capacity.ReplicaCapacity {
		out := make([]capacity.ReplicaCapacity, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, capacity.ReplicaCapacity{VariantName: "v", SaturatedThroughput: runMu,
				SaturatedThroughputSamples: MinThroughputSamplesToOrder})
		}
		return out
	}

	It("sizes the fleet to lambda over mu, in the variant's own tokens", func() {
		// 6 / 5.4 = 1.11 replicas' worth of demand. Through the engine's
		// RC = D / 0.85 - supply that is 1.31 replicas, so a two-replica fleet
		// holds and a one-replica fleet is (correctly) short.
		f := Estimate(runLambda, replicas(6), variants(domain.RoleDecode, 6), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.ByRole).To(HaveKey(domain.RoleDecode))
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6))
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", 1.111, 1e-3))
		Expect(f.Terms[domain.RoleDecode].Backlog).To(BeZero())
	})

	It("holds a role whose shape has changed, however many readings it has", func() {
		// The gate this covers is the one a shape change adds, and it only
		// shows on readings that would otherwise ORDER: the role's own bucket,
		// with enough samples. Without staleShape a fleet of one orders its second replica;
		// with it the same readings may hold what it has and no
		// more, because every reading on record was taken under a shape the
		// fleet has left.
		ordering := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(ordering.Terms[domain.RoleDecode].Held).To(BeFalse(),
			"the same readings order when the shape is steady")

		held := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, true, 0, nil)
		Expect(held.Terms[domain.RoleDecode].Held).To(BeTrue())
		Expect(held.Terms[domain.RoleDecode].HeldWhy).To(Equal("shape-change"),
			"named for the reason, not for the sample count it would otherwise report")
		Expect(held.ByRole[domain.RoleDecode]).To(BeNumerically("<=", ordering.ByRole[domain.RoleDecode]),
			"a hold never asks for more than the order it replaces")
		Expect(held.Terms[domain.RoleDecode].Replicas).To(Equal(ordering.Terms[domain.RoleDecode].Replicas),
			"the uncapped figure is still reported, as it is for the other holds")
	})

	It("does not label a term shape-change when nothing was capped", func() {
		// A fleet whose floor is already under the hold cap is not held at
		// all, and must not be labelled as though it were.
		f := Estimate(runLambda, replicas(60), variants(domain.RoleDecode, 60), nil, BacklogDrainSeconds, 0.85, true, 0, nil)
		Expect(f.Terms[domain.RoleDecode].Held).To(BeFalse())
		Expect(f.Terms[domain.RoleDecode].HeldWhy).To(BeEmpty())
	})

	It("does not move when replicas are added or removed", func() {
		// The property the arrival floor was supposed to have and did not:
		// mu is a per-replica constant, so the floor is the same at one
		// replica as at six.
		one := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		six := Estimate(runLambda, replicas(6), variants(domain.RoleDecode, 6), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(six.ByRole[domain.RoleDecode]).To(BeNumerically("~", one.ByRole[domain.RoleDecode], 1e-6))
	})

	It("asks for the replicas the load needs, whether the fleet has them or not", func() {
		// One replica, lambda / mu = 1.11: through the engine's RC = D / 0.85
		// - supply that orders the second replica in the first cycle lambda
		// is measured. The first version capped this at the fleet's size and
		// the order came 50 s later, from occupancy, after the replica had
		// tipped into preemption (file header).
		f := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6))
		Expect(f.ByRole[domain.RoleDecode]/0.85).To(BeNumerically(">", float64(runK1)),
			"RC = D / scaleUp - one replica's supply is positive: the second replica is ordered")

		By("and not one more as replicas arrive: the figure is the load's, not the fleet's")
		g := Estimate(runLambda, replicas(2), variants(domain.RoleDecode, 2), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("~", f.ByRole[domain.RoleDecode], 1e-6))
		Expect(g.ByRole[domain.RoleDecode]/0.85).To(BeNumerically("<", 2*float64(runK1)),
			"at two replicas RC is negative: nothing more is ordered")
	})

	It("holds but does not order on a borrowed reading", func() {
		// A mu borrowed from a neighbouring bucket is wrong in a known
		// direction; from a longer shape it is too low and would over-order.
		// The shape-swap benchmark's phase 2 starts exactly so: the 4000-token
		// shape reads the 1000-token shape's mu until it has its own. Borrowed
		// readings hold the fleet at its size and no more.
		borrowed := capacity.ReplicaCapacity{VariantName: "v", SaturatedThroughput: 2.67, SaturatedThroughputSamples: 10, SaturatedThroughputBorrowed: true}
		f := Estimate(runLambda, []capacity.ReplicaCapacity{borrowed}, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", runLambda/2.67, 1e-6), "the uncapped figure is reported")
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*float64(runK1), 1e-6), "capped at scaleUp x one replica")
		Expect(f.Terms[domain.RoleDecode].Held).To(BeTrue())
		Expect(f.Terms[domain.RoleDecode].HeldWhy).To(Equal("borrowed"))

		By("ordering once one replica has a reading of its own")
		own := capacity.ReplicaCapacity{VariantName: "v", SaturatedThroughput: 2.67, SaturatedThroughputSamples: MinThroughputSamplesToOrder}
		g := Estimate(runLambda, []capacity.ReplicaCapacity{borrowed, own}, variants(domain.RoleDecode, 2), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(g.Terms[domain.RoleDecode].Held).To(BeFalse())
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/2.67*float64(runK1), 1e-6))
	})

	It("never publishes a negative floor, whatever the anticipated supply says", func() {
		// The hold cap is scaleUp x the role's anticipated supply, and that
		// is (ReplicaCount + PendingReplicas) x P. Unguarded, a negative
		// product caps a real floor BELOW zero -- every floor is above a
		// negative hold, so the branch takes it -- and the package's one
		// promise, that it only ever raises demand, inverts. Producers clamp
		// PendingReplicas today; this package cannot see that they do.
		one := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu, SaturatedThroughputSamples: 1}}
		bad := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode,
			ReplicaCount: 1, PendingReplicas: -3, PerReplicaCapacity: float64(runK1)}}
		f := Estimate(runLambda, one, bad, nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically(">=", 0),
			"a negative anticipated supply must hold the floor at zero, not below it")
		Expect(f.ByRole[domain.RoleDecode]).To(BeZero(),
			"held at zero: the cap is what the engine RC turns into nothing")
		Expect(f.Terms[domain.RoleDecode].Held).To(BeTrue())
	})

	It("never prices a non-finite reading, and never publishes one", func() {
		// The filters were `SaturatedThroughput <= 0` and `p <= 0`. Every
		// comparison against NaN is false, so both ADMITTED a NaN -- and a NaN
		// floor then defeats BOTH caps, because `floor > step` and
		// `floor > hold` are false too. The role published NaN with Held
		// false: the inverse of this package's only promise. Measured before
		// the fix by calling Estimate directly, and asserted here per input.
		v := variants(domain.RoleDecode, 1)

		By("a NaN rate is not a reading, so the role has no floor at all")
		nanMu := []capacity.ReplicaCapacity{
			{VariantName: "v", SaturatedThroughput: math.NaN(), SaturatedThroughputSamples: 3}}
		f := Estimate(runLambda, nanMu, v, nil, BacklogDrainSeconds, 0.85, false, runLambda*10, nil)
		_, present := f.ByRole[domain.RoleDecode]
		Expect(present).To(BeFalse(), "the same as no reading, not a NaN floor")

		By("a NaN rate among finite ones is dropped, not averaged in")
		// slices.Sort puts a NaN wherever it likes, so whether a NaN survived
		// the median used to depend on how many readings there were. It must
		// not be a question of parity.
		// Two DIFFERENT finite readings, so the assertion separates "dropped"
		// from "clamped to zero and averaged in". Filtered, mu is
		// median(5.4, 2.7) = 4.05; clamped-and-included it would be
		// median(0, 2.7, 5.4) = 2.7. With two equal readings both give 5.4 and
		// the spec could not tell them apart.
		const slowMu = runMu / 2
		mixed := []capacity.ReplicaCapacity{
			{VariantName: "v", SaturatedThroughput: runMu, SaturatedThroughputSamples: 3},
			{VariantName: "v", SaturatedThroughput: math.NaN(), SaturatedThroughputSamples: 3},
			{VariantName: "v", SaturatedThroughput: slowMu, SaturatedThroughputSamples: 3},
		}
		g := Estimate(runLambda, mixed, v, nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(math.IsNaN(g.ByRole[domain.RoleDecode])).To(BeFalse())
		Expect(g.Terms[domain.RoleDecode].Mu).To(BeNumerically("~", (runMu+slowMu)/2, 1e-9),
			"the two finite readings decide it between them, and a NaN is not one")
		wantCost := (float64(runK1)/runMu + float64(runK1)/slowMu) / 2
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda*wantCost, 1e-6))

		By("an infinite reading among finite ones is dropped the same way")
		withInf := []capacity.ReplicaCapacity{
			{VariantName: "v", SaturatedThroughput: runMu, SaturatedThroughputSamples: 3},
			{VariantName: "v", SaturatedThroughput: math.Inf(1), SaturatedThroughputSamples: 3},
			{VariantName: "v", SaturatedThroughput: slowMu, SaturatedThroughputSamples: 3},
		}
		gi := Estimate(runLambda, withInf, v, nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(gi.Terms[domain.RoleDecode].Mu).To(BeNumerically("~", (runMu+slowMu)/2, 1e-9),
			"+Inf passes `x > 0`, so only the explicit test keeps it out of the median")

		By("a non-finite BORROWED reading is filtered before it is borrowed")
		// The borrowed branch is reached after the same filter, so a NaN cannot
		// become a borrowed cost either -- and a role whose only reading was a
		// non-finite borrowed one is not "borrowed only", it has nothing.
		borrowedNaN := []capacity.ReplicaCapacity{{VariantName: "v",
			SaturatedThroughput: math.NaN(), SaturatedThroughputSamples: 10,
			SaturatedThroughputBorrowed: true}}
		gb := Estimate(runLambda, borrowedNaN, v, nil, BacklogDrainSeconds, 0.85, false, runLambda*10, nil)
		_, borrowedPresent := gb.ByRole[domain.RoleDecode]
		Expect(borrowedPresent).To(BeFalse(), "nothing to borrow from a NaN")

		By("a NaN per-replica capacity is not a capacity")
		nanP := []capacity.ReplicaCapacity{
			{VariantName: "v", SaturatedThroughput: runMu, SaturatedThroughputSamples: 3}}
		badP := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode,
			ReplicaCount: 1, PerReplicaCapacity: math.NaN()}}
		h := Estimate(runLambda, nanP, badP, nil, BacklogDrainSeconds, 0.85, false, runLambda*10, nil)
		_, present = h.ByRole[domain.RoleDecode]
		Expect(present).To(BeFalse())

		By("an infinite rate is not a reading either")
		// +Inf passes `x > 0`, so positivity alone does not exclude it. It
		// prices a replica at zero cost, and it makes the published
		// PerReplica (cost x mu) a NaN through 0 x +Inf.
		infMu := []capacity.ReplicaCapacity{
			{VariantName: "v", SaturatedThroughput: math.Inf(1), SaturatedThroughputSamples: 3}}
		i := Estimate(runLambda, infMu, v, nil, BacklogDrainSeconds, 0.85, false, runLambda*10, nil)
		_, present = i.ByRole[domain.RoleDecode]
		Expect(present).To(BeFalse(), "no floor, rather than a zero-cost one")
		Expect(math.IsNaN(i.Terms[domain.RoleDecode].PerReplica)).To(BeFalse(),
			"and nothing NaN reaches the Term this package logs")
	})

	It("lets one variant's unusable capacity cost the role nothing", func() {
		// A NaN or +Inf PerReplicaCapacity on ONE variant used to poison the
		// whole role's anticipated supply, because aggregation summed it
		// unguarded. The supply then capped nothing -- `floor > NaN` and
		// `floor > +Inf` are both false -- so the hold silently did not bind.
		//
		// aggregation.perReplica now counts an unusable capacity as none, so the
		// role's supply is whatever its usable variants are worth and the cap
		// binds normally. The variant contributes nothing instead of destroying
		// the figure.
		thin := []capacity.ReplicaCapacity{
			{VariantName: "good", SaturatedThroughput: runMu, SaturatedThroughputSamples: 1}}

		for _, tc := range []struct {
			name string
			bad  float64
		}{
			{"a NaN capacity", math.NaN()},
			{"an infinite capacity", math.Inf(1)},
		} {
			By(tc.name + " on a second variant")
			vs := []domain.VariantCapacity{
				{VariantName: "good", Role: domain.RoleDecode,
					ReplicaCount: 1, PerReplicaCapacity: float64(runK1)},
				{VariantName: "bad", Role: domain.RoleDecode,
					ReplicaCount: 1, PerReplicaCapacity: tc.bad},
			}
			f := Estimate(runLambda, thin, vs, nil, BacklogDrainSeconds, 0.85, false, 0, nil)

			Expect(math.IsNaN(f.ByRole[domain.RoleDecode])).To(BeFalse(),
				"never a NaN floor")
			Expect(f.Terms[domain.RoleDecode].Held).To(BeTrue(),
				"the cap binds, which is what the unguarded sum prevented")
			// The good variant alone is the anticipated supply, so the hold is
			// exactly scaleUp x one replica of it -- not zero, which is what a
			// clamp at the read site produced, and not uncapped.
			Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*float64(runK1), 1e-6),
				"held at the usable variant's worth")
		}
	})

	It("clamps an anticipated supply it did not aggregate itself", func() {
		// nonNegativeSupply is unreachable through Estimate now that the sums
		// are guarded at their source, so it is asserted directly rather than
		// left as untested defence. It is kept because the cap it feeds is this
		// package's only guarantee and a caller may supply its own aggregate.
		Expect(nonNegativeSupply(math.NaN())).To(BeZero(), "max(NaN, 0) is NaN")
		Expect(nonNegativeSupply(math.Inf(1))).To(BeZero(), "max(+Inf, 0) is +Inf")
		Expect(nonNegativeSupply(math.Inf(-1))).To(BeZero())
		Expect(nonNegativeSupply(-1)).To(BeZero())
		Expect(nonNegativeSupply(0)).To(BeZero())
		Expect(nonNegativeSupply(float64(runK1))).To(Equal(float64(runK1)),
			"and a real supply passes through untouched")
	})

	It("prices only the usable variants of a role", func() {
		// The reading filter and the capacity sanitizer are different layers:
		// a variant with an unusable capacity must not contribute a cost even
		// though its replica DOES have a finite reading.
		reps := []capacity.ReplicaCapacity{
			{VariantName: "good", SaturatedThroughput: runMu, SaturatedThroughputSamples: 3},
			{VariantName: "bad", SaturatedThroughput: runMu, SaturatedThroughputSamples: 3},
		}
		vs := []domain.VariantCapacity{
			{VariantName: "good", Role: domain.RoleDecode,
				ReplicaCount: 1, PerReplicaCapacity: float64(runK1)},
			{VariantName: "bad", Role: domain.RoleDecode,
				ReplicaCount: 1, PerReplicaCapacity: math.NaN()},
		}
		f := Estimate(runLambda, reps, vs, nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(math.IsNaN(f.ByRole[domain.RoleDecode])).To(BeFalse())
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda*float64(runK1)/runMu, 1e-6),
			"the good variant's cost alone, as though the bad one were not there")
	})

	It("lets a single reading order while the scheduler holds a queue", func() {
		// The hold exists because an order on a first under-read
		// over-provisions AND the over-provisioned fleet then never saturates
		// again to record the correcting reading. The second half fails while
		// the scheduler is holding work: the queue keeps the fleet saturated
		// until it drains, so the second reading arrives either way.
		one := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu, SaturatedThroughputSamples: 1}}
		v := variants(domain.RoleDecode, 1)

		By("holding when the scheduler queue is only a transient")
		f := Estimate(runLambda, one, v, nil, BacklogDrainSeconds, 0.85, false, runLambda, nil)
		Expect(f.Terms[domain.RoleDecode].Held).To(BeTrue(), "one second of arrivals is not a standing queue")
		Expect(f.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse())

		By("ordering once it holds more than a second of arrivals")
		g := Estimate(runLambda, one, v, nil, BacklogDrainSeconds, 0.85, false, runLambda*10, nil)
		Expect(g.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
		Expect(g.Terms[domain.RoleDecode].Held).To(BeFalse(), "released, so the floor is its own figure")
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically(">", f.ByRole[domain.RoleDecode]),
			"the whole point is that it may ask for more than the fleet it has")

		By("asking for one replica beyond the fleet, and no more")
		// The release skips the scaleUp cap, so it carries its own bound: a
		// thin window is exactly the reading that under-reads mu, and the
		// floor is rate x P/mu, so an unbounded release doubles the order
		// when mu is halved. One replica beyond anticipated is what a queue
		// justifies.
		//
		// scaleUp x (anticipated + P) leaves the engine RC = D/scaleUp -
		// anticipated at one replica, so assert the tokens directly.
		// The bound only BINDS when the floor exceeds it; with an accurate
		// reading the floor sits under it and is left alone.
		// The literal, not Term.PerReplica: that field is cost*mu from the
		// code under test, so comparing against it checks step == step and a
		// corrupted cost*mu would move both sides together.
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("<=", 0.85*2*float64(runK1)+1e-6),
			"never more than one replica beyond the one already there")

		By("and an under-read mu cannot inflate that")
		// Half the reading: the unbounded floor would double. The bound holds.
		thinHalf := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu / 2, SaturatedThroughputSamples: 1}}
		half := Estimate(runLambda, thinHalf, v, nil, BacklogDrainSeconds, 0.85, false, runLambda*10, nil)
		Expect(half.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
		Expect(half.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*2*float64(runK1), 1e-6),
			"still one replica, though the reading is half what it should be")

		By("still holding when the fleet's shape has changed under it")
		st := Estimate(runLambda, one, v, nil, BacklogDrainSeconds, 0.85, true, runLambda*10, nil)
		Expect(st.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse(),
			"every reading on record was taken under a shape the fleet has left; a queue does not make it right")
		Expect(st.Terms[domain.RoleDecode].Held).To(BeTrue())

		By("still holding when the only reading is borrowed from another bucket")
		bor := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu,
			SaturatedThroughputSamples: 10, SaturatedThroughputBorrowed: true}}
		bq := Estimate(runLambda, bor, v, nil, BacklogDrainSeconds, 0.85, false, runLambda*10, nil)
		Expect(bq.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse(),
			"a borrowed reading is wrong in a known direction, queue or no queue")
		Expect(bq.Terms[domain.RoleDecode].Held).To(BeTrue())
		Expect(bq.Terms[domain.RoleDecode].HeldWhy).To(Equal("borrowed"))

		By("not releasing on jitter when the load is light")
		// One stray request is ten seconds of arrivals at 0.1 req/s, so a test
		// against lambda alone would call it a standing queue. Against mu it
		// is a fraction of one replica-second of work, which is what it is.
		light := Estimate(0.1, one, v, nil, BacklogDrainSeconds, 0.85, false, 1, nil)
		Expect(light.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse(),
			"a single queued request on a lightly loaded fleet is jitter, not a queue")

		By("ignoring the engines' own queues, which may be waiting on a KV transfer")
		busy := Estimate(runLambda, one, v, map[string]float64{domain.RoleDecode: 500},
			BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(busy.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse(),
			"a request awaiting a remote KV transfer is not work another replica drains")
		Expect(busy.Terms[domain.RoleDecode].Held).To(BeTrue())
	})

	It("climbs to a figure that does not move, one start at a time", func() {
		// The pending test paces the climb; it does not prevent it. What makes
		// the climb safe is that the figure does not move: (lambda + backlog /
		// drain) / mu is fixed by the load and the queue, not by the fleet. So
		// a thin reading behind a standing queue converges on it instead of
		// ratcheting past it -- which is the property the rule rests on, and
		// the one a reader will want pinned.
		thin := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu, SaturatedThroughputSamples: 1}}
		fleet := func(ready, pending int) []domain.VariantCapacity {
			return []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode,
				PerReplicaCapacity: float64(runK1), ReplicaCount: ready, PendingReplicas: pending}}
		}
		const queue = 60.0
		at := func(ready, pending int) Floor {
			return Estimate(runLambda, thin, fleet(ready, pending), nil, BacklogDrainSeconds, 0.85, false, queue, nil)
		}

		first := at(2, 0)
		Expect(first.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())

		By("the figure is offered while the start is outstanding too")
		Expect(at(2, 1).Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
		Expect(at(2, 1).ByRole[domain.RoleDecode]).To(BeNumerically("~", first.ByRole[domain.RoleDecode], 1e-6),
			"and it is the same figure, so the engine's own anticipated-supply subtraction does the pacing")

		By("and the figure is the same once it lands, and after the next, and the next")
		for _, ready := range []int{3, 4, 5} {
			f := at(ready, 0)
			Expect(f.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
			Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", first.ByRole[domain.RoleDecode], 1e-6),
				"the floor must not grow as the fleet does, or this would ratchet")
		}

		By("and it stops asking once the queue is no longer a queue")
		Expect(at(5, 0).Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
		quiet := Estimate(runLambda, thin, fleet(5, 0), nil, BacklogDrainSeconds, 0.85, false, runMu, nil)
		Expect(quiet.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse())
	})

	It("bounds the release by the role's smallest replica, not a blend", func() {
		// cost is median(P/mu) and mu is median(mu), taken independently, so on
		// a role of unequal variants they multiply to no real replica. With
		// P 930,000 at mu 5.4 and P 600,000 at mu 2.0 the product is 873,610
		// tokens -- 1.46 of the smaller replica. The bound is the smallest P.
		const fastP, slowP = 930_000.0, 600_000.0
		thin := []capacity.ReplicaCapacity{
			{VariantName: "fast", SaturatedThroughput: 5.4, SaturatedThroughputSamples: 1},
			{VariantName: "fast2", SaturatedThroughput: 5.4, SaturatedThroughputSamples: 1},
			{VariantName: "slow", SaturatedThroughput: 2.0, SaturatedThroughputSamples: 1},
			{VariantName: "slow2", SaturatedThroughput: 2.0, SaturatedThroughputSamples: 1},
		}
		mixed := []domain.VariantCapacity{
			{VariantName: "fast", Role: domain.RoleDecode, PerReplicaCapacity: fastP, ReplicaCount: 1},
			{VariantName: "fast2", Role: domain.RoleDecode, PerReplicaCapacity: fastP},
			{VariantName: "slow", Role: domain.RoleDecode, PerReplicaCapacity: slowP},
			{VariantName: "slow2", Role: domain.RoleDecode, PerReplicaCapacity: slowP},
		}
		f := Estimate(runLambda, thin, mixed, nil, BacklogDrainSeconds, 0.85, false, runLambda*10, nil)
		Expect(f.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
		// Exact, not an upper bound. The floor here is 6 x median(P/mu) =
		// 1,416,666.67 against a step of 1,300,500, so the cap BINDS and the
		// figure is known. A bound of `<=` would also admit a step that drops
		// the anticipated term altogether (0.85 x 600,000 = 510,000), which
		// caps the floor at a third of what it should be.
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*(fastP+slowP), 1e-6),
			"one replica of the SMALLEST variant beyond the fleet, never the blend")
	})

	It("releases every role behind one model-wide queue, bounded per role", func() {
		// The scheduler's queue is model-wide and applied to each role, as
		// lambda is: a request waiting there has been dispatched to no pod,
		// and when it is it passes through every role. So one queue releases
		// both roles' thin windows -- deliberately. What keeps that safe is
		// that each release is bounded to one replica OF THAT ROLE, so a
		// two-role fleet cannot be made to order two large replicas by one
		// queue when one of the roles is small.
		//
		// mu is halved for both roles so that both caps BIND. At the honest
		// reading neither does -- the floor sits under the step -- and then
		// bounding prefill by a decode replica leaves the figure untouched,
		// so an assertion on this fixture could not see the very bug the
		// paragraph above describes. With the cap binding the figure is
		// exact, and a swap is visible in either direction: prefill bounded
		// by decode gives 1.111 x k1 rather than 0.85, and decode bounded by
		// prefill gives 1.275 rather than 1.7.
		const thinMu = runMu / 2
		thin := []capacity.ReplicaCapacity{
			{VariantName: "d", SaturatedThroughput: thinMu, SaturatedThroughputSamples: 1},
			{VariantName: "p", SaturatedThroughput: thinMu, SaturatedThroughputSamples: 1},
		}
		pd := []domain.VariantCapacity{
			{VariantName: "d", Role: domain.RoleDecode, PerReplicaCapacity: float64(runK1), ReplicaCount: 1},
			{VariantName: "p", Role: domain.RolePrefill, PerReplicaCapacity: float64(runK1) / 2, ReplicaCount: 1},
		}
		f := Estimate(runLambda, thin, pd, nil, BacklogDrainSeconds, 0.85, false, runLambda*10, nil)

		Expect(f.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue(), "decode released")
		Expect(f.Terms[domain.RolePrefill].OrderedBehindQueue).To(BeTrue(), "prefill released by the same queue")
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*2*float64(runK1), 1e-6),
			"decode bounded by one DECODE replica beyond its fleet")
		Expect(f.ByRole[domain.RolePrefill]).To(BeNumerically("~", 0.85*float64(runK1), 1e-6),
			"prefill bounded by one PREFILL replica, which is half the size")
	})

	It("does not release when there is no scale-up threshold to bound the step", func() {
		// The release skips the scaleUp cap and carries its own bound,
		// scaleUpThreshold x (anticipated + one replica). With no threshold
		// configured that product is zero; the floor is above zero, so the
		// step would clamp it to nothing -- the inverse of a release, applied
		// to every role a standing queue touched. Hence the positive-threshold
		// conjunct, which nothing else in this file exercises: the existing
		// zero-threshold case passes no queue, so it never reaches the branch.
		one := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu, SaturatedThroughputSamples: 1}}
		z := Estimate(runLambda, one, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0, false, runLambda*10, nil)

		Expect(z.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse(),
			"there is nothing to bound the step with, so a queue must not release the hold")
		Expect(z.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda*float64(runK1)/runMu, 1e-6),
			"and the figure is its own -- neither released nor zeroed")
	})

	It("holds but does not order on a single reading", func() {
		// The first reading at a saturation under-reads (3.67 against a true
		// 7.13 on the run); an order on it over-provisions, and the
		// over-provisioned fleet never saturates again to correct it.
		// Letting one reading order one replica was tried and dropped: a
		// ratchet across starts, and one cycle's worth of benefit measured.
		one := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu / 2, SaturatedThroughputSamples: 1}}
		f := Estimate(runLambda, one, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", 2.22, 0.01))
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*float64(runK1), 1e-6),
			"capped at scaleUp x the one replica: RC = 0 exactly")
		Expect(f.Terms[domain.RoleDecode].HeldWhy).To(Equal("single-sample"))

		By("holding at the anticipated size when a replica is already on its way")
		pending := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 2, PendingReplicas: 1, PerReplicaCapacity: 100}}
		pend := Estimate(100, one, pending, nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(pend.Terms[domain.RoleDecode].Held).To(BeTrue())
		Expect(pend.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*300, 1e-6))

		By("ordering from the second reading on")
		two := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu / 2, SaturatedThroughputSamples: 2}}
		g := Estimate(runLambda, two, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(g.Terms[domain.RoleDecode].Held).To(BeFalse())
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("~", 2.22*float64(runK1), 0.01*float64(runK1)))

		By("with no scale-up threshold there is nothing to cap against, and the figure stands")
		z := Estimate(runLambda, one, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0, false, 0, nil)
		Expect(z.Terms[domain.RoleDecode].Held).To(BeFalse())
	})

	It("prices a backlog as arrivals to clear within the drain target", func() {
		// 350 queued requests on the run's first ramp. Charged as residency
		// they were 2.45M tokens -- five replicas' worth on top of the load.
		// As throughput: 350 / 60 s = 5.8 extra req/s, (6 + 5.8) / 5.4 = 2.19
		// replicas in all, the load included.
		backlog := map[string]float64{domain.RoleDecode: 350}
		f := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), backlog, 60, 0.85, false, 0, nil)
		Expect(f.Terms[domain.RoleDecode].Backlog).To(Equal(350.0))
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", (runLambda+350.0/60)/runMu, 1e-6))
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", (runLambda+350.0/60)/runMu*float64(runK1), 1e-6))

		By("a longer drain target asks for less")
		g := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), backlog, 120, 0.85, false, 0, nil)
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("<", f.ByRole[domain.RoleDecode]))

		By("another role's backlog is not this role's")
		h := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1),
			map[string]float64{domain.RolePrefill: 350}, 60, 0.85, false, 0, nil)
		Expect(h.Terms[domain.RoleDecode].Backlog).To(BeZero())

		By("a non-positive drain target disables the backlog term rather than dividing by it")
		z := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), backlog, 0, 0.85, false, 0, nil)
		Expect(z.Terms[domain.RoleDecode].Backlog).To(BeZero())
		Expect(z.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6))
	})

	It("has no opinion for a role that has never been seen saturated", func() {
		rcs := append(replicas(2), capacity.ReplicaCapacity{VariantName: "p", SaturatedThroughput: 0})
		vcs := append(variants(domain.RoleDecode, 2),
			domain.VariantCapacity{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 1, PerReplicaCapacity: 919_449})
		f := Estimate(runLambda, rcs, vcs, nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.ByRole).To(HaveKey(domain.RoleDecode))
		Expect(f.ByRole).NotTo(HaveKey(domain.RolePrefill))
	})

	It("leaves a bridge's throughput out", func() {
		// A warm-pool bridge runs its engine on different terms (lower
		// --gpu-memory-utilization, and it is going home); its rate is not
		// this variant's.
		rcs := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: 1, FromWarmPool: true}}
		f := Estimate(runLambda, rcs, variants(domain.RoleBoth, 0), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.ByRole).To(BeEmpty())
	})

	It("leaves out a replica whose variant has no per-replica capacity", func() {
		// A throughput reading can outlive the capacity it was recorded
		// beside -- the window persists while a variant's P reads zero for a
		// cycle. P / mu is then 0, and a zero cost in the median would drag
		// the role's floor toward nothing for the replicas that are priced.
		vcs := append(variants(domain.RoleDecode, 1),
			domain.VariantCapacity{VariantName: "unpriced", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: 0})
		rcs := append(replicas(1), capacity.ReplicaCapacity{VariantName: "unpriced", SaturatedThroughput: runMu})
		f := Estimate(runLambda, rcs, vcs, nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6),
			"the priced replica alone decides the floor")
	})

	It("says nothing without an arrival rate", func() {
		f := Estimate(0, replicas(2), variants(domain.RoleBoth, 2), nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.ByRole).To(BeEmpty())
	})
})

var _ = Describe("Estimate with mixed readings", func() {
	It("lets no borrowed reading outvote a replica's own", func() {
		// Replayed from the 1000/6000 shape-swap trace (2026-09-20, cycle
		// 11:46:37): the replica that had been saturated under the new shape
		// reads its own xxlong bucket at 1.74 req/s, two fresh replicas read
		// an output length of 0 (no completions yet, or the short ones that
		// finish first), land in a bucket with no reading and borrow the
		// previous shape's 4.38. The median of the three was 4.38 -- the
		// backlog of 441 requests read as two replicas' worth instead of five,
		// and the target went from 10 to 4 while the backlog grew.
		variants := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 3, PerReplicaCapacity: 930_000}}
		own := capacity.ReplicaCapacity{VariantName: "v", SaturatedThroughput: 1.74, SaturatedThroughputSamples: MinThroughputSamplesToOrder}
		fresh := capacity.ReplicaCapacity{VariantName: "v", SaturatedThroughput: 4.38, SaturatedThroughputSamples: 10, SaturatedThroughputBorrowed: true}
		backlog := map[string]float64{domain.RoleDecode: 441}
		f := Estimate(1.68, []capacity.ReplicaCapacity{fresh, own, fresh}, variants, backlog, BacklogDrainSeconds, 0.85, false, 0, nil)
		term := f.Terms[domain.RoleDecode]
		Expect(term.Mu).To(Equal(1.74), "the own reading, however many replicas borrow")
		Expect(term.Replicas).To(BeNumerically("~", (1.68+441/BacklogDrainSeconds)/1.74, 1e-6))
		Expect(term.Held).To(BeFalse(), "an own reading with enough samples still orders")
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", (1.68+441/BacklogDrainSeconds)/1.74*930_000, 1e-6))

		By("taking the borrowed readings when no replica reads its own")
		g := Estimate(1.68, []capacity.ReplicaCapacity{fresh, fresh}, variants, backlog, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(g.Terms[domain.RoleDecode].Mu).To(Equal(4.38))
		Expect(g.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", (1.68+441/BacklogDrainSeconds)/4.38, 1e-6))
		// Not held: two replicas' worth is under the fleet's cap (0.85 x 3 x
		// 930k), so the cap has nothing to do -- the borrowed figure
		// under-holds, which is the direction the header accepts.
		Expect(g.Terms[domain.RoleDecode].Held).To(BeFalse())
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("<", 0.85*3*930_000))

		By("keeping the own readings' median when they disagree among themselves")
		own2 := capacity.ReplicaCapacity{VariantName: "v", SaturatedThroughput: 1.26, SaturatedThroughputSamples: 1}
		h := Estimate(1.68, []capacity.ReplicaCapacity{fresh, own, own2, fresh, fresh}, variants, backlog, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(h.Terms[domain.RoleDecode].Mu).To(BeNumerically("~", (1.74+1.26)/2, 1e-9), "the central pair of the two own readings, three borrowed ones ignored")
		Expect(h.Terms[domain.RoleDecode].Held).To(BeFalse(), "one own window has enough samples")
	})

	It("takes the median cost across a role's replicas, not the mean or an extreme", func() {
		// Two variants of one role priced differently: an H200 at 930k tokens
		// completing 5.4/s and a slower card at 600k completing 2.0/s. Costs
		// (P/mu) are 172k and 300k tokens per req/s; with four replicas split
		// two and two the median averages the central pair, 236k. The mean of
		// costs is the same here by symmetry, so the third reading breaks it.
		variants := []domain.VariantCapacity{
			{VariantName: "fast", Role: domain.RoleDecode, ReplicaCount: 3, PerReplicaCapacity: 930_000},
			{VariantName: "slow", Role: domain.RoleDecode, ReplicaCount: 2, PerReplicaCapacity: 600_000},
		}
		replicas := []capacity.ReplicaCapacity{
			{VariantName: "fast", SaturatedThroughput: 5.4},
			{VariantName: "fast", SaturatedThroughput: 5.4},
			{VariantName: "fast", SaturatedThroughput: 5.4},
			{VariantName: "slow", SaturatedThroughput: 2.0},
			{VariantName: "slow", SaturatedThroughput: 2.0},
		}
		f := Estimate(6, replicas, variants, nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		fastCost := 930_000 / 5.4
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 6*fastCost, 1e-6),
			"five readings, three of them the fast card's: the median is the fast card's cost")
		Expect(f.Terms[domain.RoleDecode].Mu).To(Equal(5.4))

		By("averaging the central pair on an even count")
		f = Estimate(6, replicas[1:], variants, nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		slowCost := 600_000 / 2.0
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 6*(fastCost+slowCost)/2, 1e-6))
	})
})

var _ = Describe("a derived mu, in the floor", func() {
	// variants() is a closure in the Describe above, so this block carries its
	// own single-decode fleet rather than reaching for it.
	oneDecode := []domain.VariantCapacity{{
		VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: float64(runK1),
	}}

	// Estimate lets a derived reading order with no samples and through a
	// shape change, because it is priced for the shape arriving NOW rather
	// than recorded under the one that left. Nothing asserted that, so
	// reinstating the sample check would have gone unnoticed.
	It("orders on a derived reading with no samples, even under a stale shape", func() {
		d := []capacity.ReplicaCapacity{{
			VariantName: "v", SaturatedThroughput: runMu / 2,
			SaturatedThroughputSamples: 0, SaturatedThroughputDerived: true,
		}}
		f := Estimate(runLambda, d, oneDecode, nil, BacklogDrainSeconds, 0.85, true, 0, nil)
		Expect(f.Terms[domain.RoleDecode].Held).To(BeFalse(),
			"a derived figure waits neither for samples nor for the hold")
		Expect(f.Terms[domain.RoleDecode].HeldWhy).To(BeEmpty())
	})

	It("wins over the borrowed downgrade", func() {
		d := []capacity.ReplicaCapacity{{
			VariantName: "v", SaturatedThroughput: runMu / 2,
			SaturatedThroughputSamples:  0,
			SaturatedThroughputBorrowed: true, SaturatedThroughputDerived: true,
		}}
		f := Estimate(runLambda, d, oneDecode, nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.Terms[domain.RoleDecode].Held).To(BeFalse())
	})

	It("still refuses to order on a single MEASURED reading", func() {
		one := []capacity.ReplicaCapacity{{
			VariantName: "v", SaturatedThroughput: runMu / 2, SaturatedThroughputSamples: 1,
		}}
		f := Estimate(runLambda, one, oneDecode, nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.Terms[domain.RoleDecode].HeldWhy).To(Equal("single-sample"),
			"the derived path must not have loosened the measured one")
	})
})

var _ = Describe("the GPS check does not gate the floor", func() {
	oneDecode := []domain.VariantCapacity{{
		VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: float64(runK1),
	}}

	It("orders on a derived figure whatever the observed rate said", func() {
		// The gate this replaces was measured in run T: 28 withholdings, all
		// of them in the phase-1 ramp, because the k it read carried no window
		// while the rate it compared against was averaged over a minute. This
		// package must therefore have no opinion about the check at all -- the
		// diagnostic lives in the saturation analyzer's log.
		d := []capacity.ReplicaCapacity{{
			VariantName: "v", SaturatedThroughput: runMu / 2,
			SaturatedThroughputSamples: MinThroughputSamplesToOrder,
			SaturatedThroughputDerived: true,
		}}
		f := Estimate(runLambda, d, oneDecode, nil, BacklogDrainSeconds, 0.85, false, 0, nil)
		Expect(f.Terms[domain.RoleDecode].Held).To(BeFalse())
		Expect(f.Terms[domain.RoleDecode].HeldWhy).To(BeEmpty())
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically(">", 0.85*float64(runK1)))
	})
})

// The release step, on run T's figures. Its log carried orderedBehindQueue=true
// for five consecutive cycles with replicasImplied of 4.15, 4.35, 5.41, 5.84 and
// 8.69, and the old cap granted exactly one replica on each of them while the
// scheduler queue climbed to 191.
var _ = Describe("a standing queue orders what it justifies", func() {
	Describe("queueJustifiedReplicas", func() {
		It("is what it takes to clear the queue inside the drain window", func() {
			// 191 requests at 1 req/s per replica over 60 s: 3.18, floored to 3.
			Expect(queueJustifiedReplicas(191, 1.0, 60)).To(Equal(3.0))
			// The deeper queue the same ramp reached a cycle later.
			Expect(queueJustifiedReplicas(267, 1.0, 60)).To(Equal(4.0))
		})

		It("floors at one, never zero", func() {
			// The caller has already decided the queue is standing, so "none"
			// is not an available answer -- and this must never be MORE
			// conservative than the single replica it replaces.
			Expect(queueJustifiedReplicas(1, 1.0, 60)).To(Equal(1.0))
			Expect(queueJustifiedReplicas(59, 1.0, 60)).To(Equal(1.0))
			Expect(queueJustifiedReplicas(0, 1.0, 60)).To(Equal(1.0))
		})

		It("does not round up", func() {
			// A queue worth 1.98 replicas justifies one. The next cycle sees
			// what the first did and asks again; rounding up every cycle of a
			// long ramp is how a fleet overshoots.
			Expect(queueJustifiedReplicas(119, 1.0, 60)).To(Equal(1.0))
			Expect(queueJustifiedReplicas(120, 1.0, 60)).To(Equal(2.0))
		})

		It("answers one when the arithmetic is unavailable", func() {
			// No service rate and no window means no arithmetic; the queue is
			// still standing, so the previous behaviour stands too.
			Expect(queueJustifiedReplicas(191, 0, 60)).To(Equal(1.0))
			Expect(queueJustifiedReplicas(191, 1.0, 0)).To(Equal(1.0))
			Expect(queueJustifiedReplicas(191, -1, 60)).To(Equal(1.0))
		})
	})

	Describe("in Estimate", func() {
		oneDecode := []domain.VariantCapacity{{
			VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 1,
			PerReplicaCapacity: float64(runK1),
		}}
		// A window too thin to order on: one sample, so mayOrder is false and
		// only a standing queue can release it.
		thin := []capacity.ReplicaCapacity{{
			VariantName: "v", SaturatedThroughput: runMu, SaturatedThroughputSamples: 1,
		}}

		It("releases more than one replica behind a deep queue", func() {
			// The engines' own backlog has to be large enough that the UNCAPPED
			// floor exceeds the cap, or the cap never binds and the assertion
			// below measures nothing. schedulerQueued releases the hold; it is
			// the backlog map that feeds rate.
			engineBacklog := map[string]float64{domain.RoleDecode: 100 * runMu * BacklogDrainSeconds}
			deep := 20 * runMu * BacklogDrainSeconds // 20 replicas' worth
			shallow := runLambda + 1

			deepF := Estimate(runLambda, thin, oneDecode, engineBacklog,
				BacklogDrainSeconds, 0.85, false, deep, nil)
			shallowF := Estimate(runLambda, thin, oneDecode, engineBacklog,
				BacklogDrainSeconds, 0.85, false, shallow, nil)

			Expect(deepF.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
			Expect(deepF.Terms[domain.RoleDecode].Held).To(BeFalse(),
				"the release fired, so the hold below it never runs")
			Expect(deepF.Terms[domain.RoleDecode].QueueJustifiedReplicas).To(BeNumerically(">", 1),
				"the queue is worth twenty replicas; one is not what it justifies")

			// Both are capped, so the only difference is how many replicas the
			// queue justified -- which is the whole change.
			Expect(deepF.ByRole[domain.RoleDecode]).To(BeNumerically(">",
				shallowF.ByRole[domain.RoleDecode]),
				"a deeper queue must admit a bigger step")
			Expect(shallowF.Terms[domain.RoleDecode].QueueJustifiedReplicas).To(Equal(1.0))
		})

		It("still grants one behind a shallow queue", func() {
			// Worth less than a replica-second of service: the release fires --
			// the caller's own test is queue > max(lambda, mu) -- but the step
			// is the single replica it always was.
			shallow := runLambda + 1
			f := Estimate(runLambda, thin, oneDecode, nil, BacklogDrainSeconds, 0.85, false, shallow, nil)
			term := f.Terms[domain.RoleDecode]
			Expect(term.OrderedBehindQueue).To(BeTrue())
			Expect(term.QueueJustifiedReplicas).To(Equal(1.0))
		})

		It("reports nothing when the release never fires", func() {
			// A window that may order on its own merits does not reach this
			// path, so the field stays zero and cannot be read as "one".
			own := []capacity.ReplicaCapacity{{
				VariantName: "v", SaturatedThroughput: runMu,
				SaturatedThroughputSamples: MinThroughputSamplesToOrder,
			}}
			f := Estimate(runLambda, own, oneDecode, nil, BacklogDrainSeconds, 0.85, false, 0, nil)
			Expect(f.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse())
			Expect(f.Terms[domain.RoleDecode].QueueJustifiedReplicas).To(BeZero())
		})
	})
})

// The landing projection, and the in-flight credit that keeps it from ratcheting.
var _ = Describe("the backlog a role is priced for", func() {
	const T = 70.0 // run T's measured start, near enough

	one := func(count int, ages ...float64) []startingReplicas {
		return []startingReplicas{{count: count, ages: ages}}
	}

	It("adds what arrives while a replica starts", func() {
		// 6 req/s over a 70 s start is 420 requests, against one replica
		// draining 70. The queue the fleet meets is not the queue it sees.
		Expect(backlogAtLanding(191, 6, 1.0, T, 1, nil)).
			To(BeNumerically("~", 191+420-70, 1e-6))
	})

	It("credits a starting replica with the window it will be Ready for", func() {
		// One 60 s into a 70 s start drains for 60 s of the window; one ordered
		// a second ago drains for 1. The count alone cannot say that.
		Expect(backlogAtLanding(191, 6, 1.0, T, 1, one(2, 60, 1))).
			To(BeNumerically("~", 191+420-(70+61), 1e-6))
	})

	It("falls back to half the window when the ages are unavailable", func() {
		Expect(backlogAtLanding(191, 6, 1.0, T, 1, one(2))).
			To(BeNumerically("~", 191+420-(70+70), 1e-6))
	})

	It("falls back PER VARIANT, not per role", func() {
		// A role whose variants report differently: one Pod listing succeeded,
		// another returned nil. Summed into the role, the successful one's ages
		// suppressed the other's count entirely and credited six starting
		// replicas with nothing.
		mixed := []startingReplicas{
			{count: 1, ages: []float64{10}},
			{count: 6},
		}
		Expect(startingCredit(T, 1, []float64{10}) + startingCredit(T, 6, nil)).
			To(BeNumerically("~", 10+6*35, 1e-6))
		Expect(backlogAtLanding(400, 6, 1.0, T, 1, mixed)).
			To(BeNumerically("~", 400+420-(70+10+210), 1e-6))
	})

	It("refuses a Pod that has been starting far too long", func() {
		// Stuck on an image pull, a crash loop or GPU quota: not Ready for as
		// long as it exists. Clamped to the window it earned exactly as much as
		// a Ready replica, permanently, and the floor stopped asking for the
		// capacity that would clear its own queue.
		Expect(startingCredit(T, 1, []float64{5000})).To(BeZero())
		// Between one and two starts is still plausibly a slow start.
		Expect(startingCredit(T, 1, []float64{1.5 * T})).To(Equal(T))
	})

	It("never credits more replicas than are starting", func() {
		// ReplicaCount comes from the metrics rows and the ages from the Pod
		// informer -- two caches with independent lag, so a replica that is
		// Ready and scraped can still read Ready=false here and be counted in
		// both terms.
		Expect(startingCredit(T, 1, []float64{70, 70, 70})).To(BeNumerically("~", T, 1e-6))
	})

	It("can price BELOW the standing queue", func() {
		// A fleet that will have drained the queue before new capacity lands
		// needs no capacity for it.
		Expect(backlogAtLanding(100, 1, 1.0, T, 5, nil)).To(BeZero())
	})

	It("leaves the observed backlog alone when nothing is known", func() {
		Expect(backlogAtLanding(191, 6, 1.0, 0, 1, nil)).To(Equal(191.0))
		Expect(backlogAtLanding(191, 6, 0, T, 1, nil)).To(Equal(191.0))
	})

	It("ignores an age that is not a number of seconds", func() {
		Expect(startingCredit(T, 2, []float64{-5, 30})).To(Equal(30.0))
	})
})

// The release cap, and the ratchet it used to be.
//
// Run U measured it: 1 -> 2 -> 3 -> 4 -> 6 -> 7 -> 9 atMax in 75 seconds against
// a steady-state need of six. The cap was built on ANTICIPATED supply, so the
// engine's RC = step/scaleUp - anticipated cancelled to exactly k replicas
// however many were already in flight -- and the scheduler queue is not reduced
// by replicas ordered last cycle, because they are not Ready yet. The same
// unserved requests justified k again every cycle while k itself grew.
var _ = Describe("the release cap and replicas already in flight", func() {
	// One ready replica throughout: what varies is how many are in flight.
	oneDecode := func(pending int) []domain.VariantCapacity {
		return []domain.VariantCapacity{{
			VariantName: "v", Role: domain.RoleDecode,
			ReplicaCount: 1, PendingReplicas: pending,
			PerReplicaCapacity: float64(runK1),
		}}
	}
	thin := []capacity.ReplicaCapacity{{
		VariantName: "v", SaturatedThroughput: runMu, SaturatedThroughputSamples: 1,
	}}
	engineBacklog := map[string]float64{domain.RoleDecode: 100 * runMu * BacklogDrainSeconds}
	deep := 20 * runMu * BacklogDrainSeconds

	It("does not grant more because more are already coming", func() {
		none := Estimate(runLambda, thin, oneDecode(0), engineBacklog,
			BacklogDrainSeconds, 0.85, false, deep, nil)
		many := Estimate(runLambda, thin, oneDecode(5), engineBacklog,
			BacklogDrainSeconds, 0.85, false, deep, nil)

		Expect(none.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
		Expect(many.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
		Expect(many.ByRole[domain.RoleDecode]).To(Equal(none.ByRole[domain.RoleDecode]),
			"five replicas in flight must not raise the cap; measured against "+
				"anticipated supply they did, and the fleet ratcheted to its ceiling")
	})

	It("still grows with the queue", func() {
		// The point of the cap is that a deeper queue justifies a bigger step.
		// Fixing the ratchet must not take that away.
		shallow := Estimate(runLambda, thin, oneDecode(0), engineBacklog,
			BacklogDrainSeconds, 0.85, false, runLambda+1, nil)
		Expect(shallow.Terms[domain.RoleDecode].QueueJustifiedReplicas).To(Equal(1.0))
		big := Estimate(runLambda, thin, oneDecode(0), engineBacklog,
			BacklogDrainSeconds, 0.85, false, deep, nil)
		Expect(big.Terms[domain.RoleDecode].QueueJustifiedReplicas).To(BeNumerically(">", 1))
		Expect(big.ByRole[domain.RoleDecode]).To(BeNumerically(">", shallow.ByRole[domain.RoleDecode]))
	})
})
