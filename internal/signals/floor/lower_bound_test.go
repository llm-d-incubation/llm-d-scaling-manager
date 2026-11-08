package floor

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// A prefill lower bound in the floor (issue #144). The figures: prefill P of
// 919,449 tokens, a bound of 2 req/s per replica, 6 req/s arriving, scale-up
// 0.85. lambda / mu is 3 replicas' worth of demand against a ready fleet of
// one, which the engine turns into RC = 3 / 0.85 - 1 = 2.53 replicas.
var _ = Describe("Estimate with a prefill lower bound", func() {
	const (
		p       = 919_449.0
		bound   = 2.0
		scaleUp = 0.85
	)
	prefill := func(ready int) []domain.VariantCapacity {
		return []domain.VariantCapacity{{VariantName: "pre", Role: domain.RolePrefill,
			ReplicaCount: ready, PerReplicaCapacity: p}}
	}
	boundRow := func(samples int) capacity.ReplicaCapacity {
		return capacity.ReplicaCapacity{VariantName: "pre", SaturatedThroughput: bound,
			SaturatedThroughputSamples: samples, SaturatedThroughputLowerBound: true}
	}
	estimate := func(lambda float64, rows []capacity.ReplicaCapacity, ready int, stale bool) Floor {
		return Estimate(lambda, rows, prefill(ready), nil, BacklogDrainSeconds, scaleUp, stale, 0, nil)
	}

	It("prices prefill on the bound and may grow it with two readings", func() {
		f := estimate(runLambda, []capacity.ReplicaCapacity{boundRow(MinThroughputSamplesToOrder)}, 1, false)
		Expect(f.ByRole).To(HaveKey(domain.RolePrefill), "a role with only a bound has a floor term")
		t := f.Terms[domain.RolePrefill]
		Expect(t.LowerBound).To(BeTrue())
		Expect(t.MayOrder).To(BeTrue())
		Expect(t.Held).To(BeFalse())
		Expect(t.Mu).To(Equal(bound))
		Expect(t.Replicas).To(BeNumerically("~", 3, 1e-9))
		// 3 replicas' worth against a ready fleet of one: the growth cap is
		// 0.85 x 2 x one replica, and binds.
		Expect(t.GrowthCapped).To(BeTrue())
		Expect(f.ByRole[domain.RolePrefill]).To(BeNumerically("~", scaleUp*LowerBoundMaxGrowth*p, 1e-6))
	})

	It("caps a bound's order at twice the READY fleet, whatever it implies", func() {
		// The over-order case: the bound reads ten times low, so lambda / mu
		// asks for thirty replicas. One cycle may take a ready fleet of one
		// to two (RC = 2 x 0.85 / 0.85 - 1 = 1) and no further, and the cap
		// does not move while the ordered replica starts: it is drawn on
		// ready supply, which a pending replica is not.
		low := boundRow(MinThroughputSamplesToOrder)
		low.SaturatedThroughput = bound / 10
		f := estimate(runLambda, []capacity.ReplicaCapacity{low}, 1, false)
		Expect(f.Terms[domain.RolePrefill].Replicas).To(BeNumerically("~", 30, 1e-9))
		Expect(f.Terms[domain.RolePrefill].GrowthCapped).To(BeTrue())
		Expect(f.ByRole[domain.RolePrefill]).To(BeNumerically("~", scaleUp*2*p, 1e-6))

		pending := prefill(1)
		pending[0].PendingReplicas = 1
		next := Estimate(runLambda, []capacity.ReplicaCapacity{low}, pending, nil, BacklogDrainSeconds, scaleUp, false, 0, nil)
		Expect(next.ByRole[domain.RolePrefill]).To(BeNumerically("~", scaleUp*2*p, 1e-6),
			"the replica already ordered is not ordered again: RC = 2 - (1 ready + 1 pending) = 0")
	})

	It("does not cap a bound that asks for less than the growth limit", func() {
		f := estimate(runLambda, []capacity.ReplicaCapacity{boundRow(2), boundRow(2), boundRow(2)}, 3, false)
		Expect(f.Terms[domain.RolePrefill].GrowthCapped).To(BeFalse())
		Expect(f.ByRole[domain.RolePrefill]).To(BeNumerically("~", runLambda/bound*p, 1e-6))
	})

	It("only holds on a single reading", func() {
		f := estimate(runLambda, []capacity.ReplicaCapacity{boundRow(1)}, 1, false)
		t := f.Terms[domain.RolePrefill]
		Expect(t.MayOrder).To(BeFalse())
		Expect(t.Held).To(BeTrue())
		Expect(t.HeldWhy).To(Equal("lower-bound"))
		Expect(f.ByRole[domain.RolePrefill]).To(BeNumerically("~", scaleUp*p, 1e-6), "RC = 0")
	})

	It("only holds while the shape is stale, and says both", func() {
		f := estimate(runLambda, []capacity.ReplicaCapacity{boundRow(5)}, 1, true)
		Expect(f.Terms[domain.RolePrefill].HeldWhy).To(Equal("lower-bound+shape-change"))
		Expect(f.ByRole[domain.RolePrefill]).To(BeNumerically("~", scaleUp*p, 1e-6))
	})

	It("gives way to any replica's own reading, however thin", func() {
		// A measurement, even a single one that may only hold, outranks a
		// bound: the bound is there for the fleet that has no reading at all.
		own := capacity.ReplicaCapacity{VariantName: "pre", SaturatedThroughput: 5, SaturatedThroughputSamples: 1}
		f := estimate(runLambda, []capacity.ReplicaCapacity{own, boundRow(5)}, 1, false)
		t := f.Terms[domain.RolePrefill]
		Expect(t.LowerBound).To(BeFalse())
		Expect(t.Mu).To(Equal(5.0))
		Expect(t.HeldWhy).To(Equal("single-sample"))
	})

	It("outranks a neighbour's figure, which may only hold", func() {
		neighbour := capacity.ReplicaCapacity{VariantName: "pre", SaturatedThroughput: 9,
			SaturatedThroughputSamples: 5, SaturatedThroughputBorrowed: true}
		f := estimate(runLambda, []capacity.ReplicaCapacity{neighbour, boundRow(5)}, 3, false)
		Expect(f.Terms[domain.RolePrefill].LowerBound).To(BeTrue())
		Expect(f.Terms[domain.RolePrefill].BorrowedOnly).To(BeFalse())
	})

	It("is not released by a standing scheduler queue", func() {
		// The queue release trusts a thin own window to be roughly right; a
		// bound is low by an unknown factor, so a thin bound only holds.
		f := Estimate(runLambda, []capacity.ReplicaCapacity{boundRow(1)}, prefill(1), nil,
			BacklogDrainSeconds, scaleUp, false, 500, nil)
		Expect(f.Terms[domain.RolePrefill].OrderedBehindQueue).To(BeFalse())
		Expect(f.Terms[domain.RolePrefill].Held).To(BeTrue())
	})

	It("never puts a role below zero on a fleet with nothing ready", func() {
		f := estimate(runLambda, []capacity.ReplicaCapacity{boundRow(5)}, 0, false)
		Expect(f.ByRole[domain.RolePrefill]).To(BeNumerically(">=", 0))
	})
})
