package aggregation_test

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/aggregation"
)

// These sums are multiplied by a replica count and then compared with `>` or `<`
// by every consumer -- the engine's RequiredCapacity and SpareCapacity clamps,
// and the throughput floor's two caps. A NaN makes every such comparison false,
// so one variant with an unusable PerReplicaCapacity did not merely corrupt its
// own term: it disabled the guard the consumer applied to the whole sum, and the
// clamp meant to bound the result silently did not fire.
//
// So the property under test is not "the arithmetic is right" but "no single
// variant can make a total non-finite".
var _ = Describe("non-finite per-replica capacity", func() {
	const good = 930_000.0

	for _, tc := range []struct {
		name string
		p    float64
	}{
		{"a NaN capacity", math.NaN()},
		{"an infinite capacity", math.Inf(1)},
		{"a negatively infinite capacity", math.Inf(-1)},
		{"a negative capacity", -1000},
		{"a zero capacity", 0},
	} {
		It(tc.name+" counts as no capacity at all", func() {
			vcs := []domain.VariantCapacity{
				{VariantName: "good", Role: domain.RoleDecode, ReplicaCount: 2,
					PendingReplicas: 1, PerReplicaCapacity: good},
				{VariantName: "bad", Role: domain.RoleDecode, ReplicaCount: 2,
					PendingReplicas: 1, PerReplicaCapacity: tc.p},
			}
			byRole := aggregation.AggregateByRole(vcs)[domain.RoleDecode]
			totals := map[string]float64{
				"SumTotalSupply":              aggregation.SumTotalSupply(vcs),
				"SumTotalAnticipatedSupply":   aggregation.SumTotalAnticipatedSupply(vcs),
				"AggregateByRole.Supply":      byRole.TotalSupply,
				"AggregateByRole.Anticipated": byRole.TotalAnticipatedSupply,
			}
			for label, v := range totals {
				Expect(math.IsNaN(v)).To(BeFalse(), "%s must never be NaN", label)
				Expect(math.IsInf(v, 0)).To(BeFalse(), "%s must never be infinite", label)
			}

			// The good variant's contribution is exactly what it was worth, so
			// the sanitizer drops the bad term rather than the whole sum.
			Expect(totals["SumTotalSupply"]).To(BeNumerically("~", 2*good, 1e-6))
			Expect(totals["SumTotalAnticipatedSupply"]).To(BeNumerically("~", 3*good, 1e-6))
			Expect(byRole.TotalSupply).To(BeNumerically("~", 2*good, 1e-6))
			Expect(byRole.TotalAnticipatedSupply).To(BeNumerically("~", 3*good, 1e-6))
		})
	}

	It("never lets one variant's stuck count cancel a sibling's supply", func() {
		// nonNegativeSupply clamps the SUMMED role figure, so a negative term
		// from one variant is consumed inside the sum and is unrecoverable.
		// v2's row is stale -- its crashed Pod still reported, so pending
		// clamped to 0 while the Pod listing saw it stuck -- and before the
		// clamp its term went negative and ate a replica of v1's real supply.
		vcs := []domain.VariantCapacity{
			{VariantName: "v1", Role: domain.RoleDecode, ReplicaCount: 6,
				PendingReplicas: 2, PerReplicaCapacity: good},
			{VariantName: "v2", Role: domain.RoleDecode, ReplicaCount: 4,
				PendingReplicas: 0, StuckReplicas: 3, PerReplicaCapacity: good},
		}
		Expect(aggregation.SumTotalAnticipatedSupply(vcs)).
			To(BeNumerically("~", 12*good, 1e-6),
				"8 from v1 and 4 from v2: a stuck Pod may say at worst "+
					"'nothing is arriving', never 'supply is negative'")
		Expect(aggregation.AggregateByRole(vcs)[domain.RoleDecode].TotalAnticipatedSupply).
			To(BeNumerically("~", 12*good, 1e-6), "same rule per role")
	})

	It("keeps anticipated supply at or above supply when a Pod is stuck", func() {
		// The stale-row case on one variant: ReplicaCount still counts the
		// crashed Pod, so charging it again as a negative arrival put
		// anticipated BELOW supply and inverted the band holdPrefillDemand
		// and the engine's RC both read.
		vcs := []domain.VariantCapacity{
			{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 4,
				PendingReplicas: 0, StuckReplicas: 1, PerReplicaCapacity: good},
		}
		Expect(aggregation.SumTotalAnticipatedSupply(vcs)).
			To(BeNumerically(">=", aggregation.SumTotalSupply(vcs)))
	})

	It("still reports a negative supply from a negative pending count", func() {
		// Only the CAPACITY is sanitized. A negative PendingReplicas is a real
		// case with a real guard downstream -- the floor holds the figure at
		// zero rather than publishing a negative -- and reinterpreting it here
		// would hide it from the consumer that handles it.
		vcs := []domain.VariantCapacity{
			{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 1,
				PendingReplicas: -3, PerReplicaCapacity: good},
		}
		Expect(aggregation.SumTotalAnticipatedSupply(vcs)).
			To(BeNumerically("~", -2*good, 1e-6),
				"the negative survives, for the downstream clamp to see")
	})

	It("leaves a large finite capacity alone", func() {
		// The sanitizer excludes non-finite values, not big ones.
		big := math.MaxFloat64 / 4
		vcs := []domain.VariantCapacity{
			{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 1,
				PerReplicaCapacity: big},
		}
		Expect(aggregation.SumTotalSupply(vcs)).To(Equal(big))
	})
})
