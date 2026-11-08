/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aggregation_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/aggregation"
)

func TestAggregation(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Aggregation Suite")
}

var _ = Describe("aggregation helpers", func() {

	Describe("SumTotalSupply", func() {
		It("returns 0 for empty input", func() {
			Expect(aggregation.SumTotalSupply(nil)).To(BeZero())
			Expect(aggregation.SumTotalSupply([]domain.VariantCapacity{})).To(BeZero())
		})

		It("sums ReplicaCount × PerReplicaCapacity across variants", func() {
			vcs := []domain.VariantCapacity{
				{VariantName: "v1", ReplicaCount: 2, PendingReplicas: 1, PerReplicaCapacity: 5000},
				{VariantName: "v2", ReplicaCount: 3, PendingReplicas: 0, PerReplicaCapacity: 8000},
			}
			// 2×5000 + 3×8000 = 10000 + 24000 = 34000 (pending NOT included)
			Expect(aggregation.SumTotalSupply(vcs)).To(Equal(34000.0))
		})

		It("ignores pending replicas", func() {
			vcs := []domain.VariantCapacity{
				{ReplicaCount: 1, PendingReplicas: 99, PerReplicaCapacity: 1000},
			}
			Expect(aggregation.SumTotalSupply(vcs)).To(Equal(1000.0))
		})

		It("handles zero PRC", func() {
			vcs := []domain.VariantCapacity{
				{ReplicaCount: 5, PerReplicaCapacity: 0},
			}
			Expect(aggregation.SumTotalSupply(vcs)).To(BeZero())
		})
	})

	Describe("SumTotalAnticipatedSupply", func() {
		It("returns 0 for empty input", func() {
			Expect(aggregation.SumTotalAnticipatedSupply(nil)).To(BeZero())
		})

		It("sums (ReplicaCount + PendingReplicas) × PRC across variants", func() {
			vcs := []domain.VariantCapacity{
				{ReplicaCount: 2, PendingReplicas: 1, PerReplicaCapacity: 5000},
				{ReplicaCount: 3, PendingReplicas: 0, PerReplicaCapacity: 8000},
			}
			// (2+1)×5000 + (3+0)×8000 = 15000 + 24000 = 39000
			Expect(aggregation.SumTotalAnticipatedSupply(vcs)).To(Equal(39000.0))
		})

		It("equals SumTotalSupply when no variants have pending replicas", func() {
			vcs := []domain.VariantCapacity{
				{ReplicaCount: 3, PendingReplicas: 0, PerReplicaCapacity: 10000},
			}
			Expect(aggregation.SumTotalAnticipatedSupply(vcs)).To(Equal(aggregation.SumTotalSupply(vcs)))
		})
	})

	Describe("SumTotalDemand", func() {
		It("returns 0 for empty input", func() {
			Expect(aggregation.SumTotalDemand(nil)).To(BeZero())
		})

		It("sums TotalDemand across variants", func() {
			vcs := []domain.VariantCapacity{
				{TotalDemand: 3000},
				{TotalDemand: 7000},
			}
			Expect(aggregation.SumTotalDemand(vcs)).To(Equal(10000.0))
		})
	})

	Describe("AggregateByRole", func() {
		It("returns empty map for empty input", func() {
			Expect(aggregation.AggregateByRole(nil)).To(BeEmpty())
		})

		It("canonicalizes empty role to RoleBoth", func() {
			vcs := []domain.VariantCapacity{
				{Role: "", ReplicaCount: 1, PerReplicaCapacity: 1000, TotalDemand: 500},
			}
			result := aggregation.AggregateByRole(vcs)
			Expect(result).To(HaveKey(domain.RoleBoth))
			Expect(result).NotTo(HaveKey(""))
		})

		It("groups variants by role and computes ScopeTotals for each", func() {
			vcs := []domain.VariantCapacity{
				{Role: "prefill", ReplicaCount: 2, PendingReplicas: 1, PerReplicaCapacity: 5000, TotalDemand: 8000},
				{Role: "decode", ReplicaCount: 3, PendingReplicas: 0, PerReplicaCapacity: 8000, TotalDemand: 9000},
				{Role: "decode", ReplicaCount: 1, PendingReplicas: 0, PerReplicaCapacity: 4000, TotalDemand: 2000},
			}
			result := aggregation.AggregateByRole(vcs)

			Expect(result).To(HaveLen(2))

			prefill := result["prefill"]
			Expect(prefill.TotalSupply).To(Equal(10000.0))            // 2×5000
			Expect(prefill.TotalAnticipatedSupply).To(Equal(15000.0)) // (2+1)×5000
			Expect(prefill.TotalDemand).To(Equal(8000.0))

			decode := result["decode"]
			Expect(decode.TotalSupply).To(Equal(28000.0))            // 3×8000 + 1×4000
			Expect(decode.TotalAnticipatedSupply).To(Equal(28000.0)) // no pending
			Expect(decode.TotalDemand).To(Equal(11000.0))            // 9000 + 2000
		})

		It("handles a single non-disaggregated variant (empty role → RoleBoth)", func() {
			vcs := []domain.VariantCapacity{
				{Role: "", ReplicaCount: 2, PendingReplicas: 0, PerReplicaCapacity: 10000, TotalDemand: 15000},
			}
			result := aggregation.AggregateByRole(vcs)
			both := result[domain.RoleBoth]
			Expect(both.TotalSupply).To(Equal(20000.0))
			Expect(both.TotalAnticipatedSupply).To(Equal(20000.0))
			Expect(both.TotalDemand).To(Equal(15000.0))
		})

		It("handles zero ReplicaCount", func() {
			vcs := []domain.VariantCapacity{
				{Role: "prefill", ReplicaCount: 0, PendingReplicas: 0, PerReplicaCapacity: 5000, TotalDemand: 0},
			}
			result := aggregation.AggregateByRole(vcs)
			prefill := result["prefill"]
			Expect(prefill.TotalSupply).To(BeZero())
			Expect(prefill.TotalAnticipatedSupply).To(BeZero())
			Expect(prefill.TotalDemand).To(BeZero())
		})
	})

	Describe("DemandByRole", func() {
		It("returns empty map for empty input", func() {
			Expect(aggregation.DemandByRole(nil)).To(BeEmpty())
		})

		It("canonicalizes empty role to RoleBoth, matching AggregateByRole", func() {
			vcs := []domain.VariantCapacity{
				{Role: "", ReplicaCount: 1, PerReplicaCapacity: 1000, TotalDemand: 500},
			}
			result := aggregation.DemandByRole(vcs)
			Expect(result).To(HaveKey(domain.RoleBoth))
			Expect(result).NotTo(HaveKey(""))
			Expect(result[domain.RoleBoth]).To(Equal(500.0))
		})

		It("sums TotalDemand within each role group", func() {
			vcs := []domain.VariantCapacity{
				{Role: "prefill", ReplicaCount: 2, PendingReplicas: 1, PerReplicaCapacity: 5000, TotalDemand: 8000},
				{Role: "decode", ReplicaCount: 3, PerReplicaCapacity: 8000, TotalDemand: 9000},
				{Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 4000, TotalDemand: 2000},
			}
			result := aggregation.DemandByRole(vcs)
			Expect(result).To(HaveLen(2))
			Expect(result["prefill"]).To(Equal(8000.0))
			Expect(result["decode"]).To(Equal(11000.0)) // 9000 + 2000
		})

		It("keeps a 'both' bucket alongside P/D roles in a mixed fleet", func() {
			// The optimizer looks up RoleCapacities["both"] for a variant whose role
			// is "both" in an otherwise disaggregated model, so the bucket must survive.
			vcs := []domain.VariantCapacity{
				{Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 5000, TotalDemand: 4000},
				{Role: domain.RoleBoth, ReplicaCount: 1, PerReplicaCapacity: 9000, TotalDemand: 3000},
				{Role: "", ReplicaCount: 1, PerReplicaCapacity: 1000, TotalDemand: 250},
			}
			result := aggregation.DemandByRole(vcs)
			Expect(result).To(HaveLen(2))
			Expect(result["prefill"]).To(Equal(4000.0))
			// The explicit "both" and the empty-role variant land in the same bucket.
			Expect(result[domain.RoleBoth]).To(Equal(3250.0))
		})

		It("agrees with AggregateByRole's TotalDemand for the same input by construction", func() {
			vcs := []domain.VariantCapacity{
				{Role: "prefill", ReplicaCount: 2, PendingReplicas: 1, PerReplicaCapacity: 5000, TotalDemand: 8000},
				{Role: "decode", ReplicaCount: 3, PerReplicaCapacity: 8000, TotalDemand: 9000},
				{Role: "", ReplicaCount: 1, PerReplicaCapacity: 4000, TotalDemand: 2000},
			}
			byRole := aggregation.AggregateByRole(vcs)
			demand := aggregation.DemandByRole(vcs)
			Expect(demand).To(HaveLen(len(byRole)))
			for role, totals := range byRole {
				Expect(demand[role]).To(Equal(totals.TotalDemand), "role %s demand mismatch", role)
			}
		})
	})

	Describe("IsDisaggregated", func() {
		It("is false for empty input", func() {
			Expect(aggregation.IsDisaggregated(nil)).To(BeFalse())
		})

		It("is false when every variant is explicitly 'both'", func() {
			vcs := []domain.VariantCapacity{
				{Role: domain.RoleBoth}, {Role: domain.RoleBoth},
			}
			Expect(aggregation.IsDisaggregated(vcs)).To(BeFalse())
		})

		It("is false when every variant has an empty role", func() {
			// Empty canonicalizes to "both", so this fleet is not disaggregated.
			Expect(aggregation.IsDisaggregated([]domain.VariantCapacity{{Role: ""}})).To(BeFalse())
		})

		It("is false for a mix of empty and explicit 'both' roles", func() {
			vcs := []domain.VariantCapacity{{Role: ""}, {Role: domain.RoleBoth}}
			Expect(aggregation.IsDisaggregated(vcs)).To(BeFalse())
		})

		It("is true when any variant serves a P/D role", func() {
			vcs := []domain.VariantCapacity{{Role: domain.RoleBoth}, {Role: "prefill"}}
			Expect(aggregation.IsDisaggregated(vcs)).To(BeTrue())
		})

		It("is true for a fully disaggregated fleet", func() {
			vcs := []domain.VariantCapacity{{Role: "prefill"}, {Role: "decode"}}
			Expect(aggregation.IsDisaggregated(vcs)).To(BeTrue())
		})

		It("matches the DemandByRole key-set test both analyzers rely on", func() {
			// The analyzers gate on IsDisaggregated and then key off DemandByRole,
			// so the two must agree about what counts as a per-role breakdown:
			// disaggregated iff more than just the "both" bucket is present.
			cases := [][]domain.VariantCapacity{
				nil,
				{{Role: ""}},
				{{Role: domain.RoleBoth}, {Role: ""}},
				{{Role: "prefill"}},
				{{Role: "prefill"}, {Role: domain.RoleBoth}},
				{{Role: "prefill"}, {Role: "decode"}},
			}
			for _, vcs := range cases {
				byRole := aggregation.DemandByRole(vcs)
				_, hasBoth := byRole[domain.RoleBoth]
				moreThanBoth := len(byRole) > 1 || (len(byRole) == 1 && !hasBoth)
				Expect(aggregation.IsDisaggregated(vcs)).To(Equal(moreThanBoth),
					"disagreement for %+v", vcs)
			}
		})
	})
})

// Anticipated supply is what the engine subtracts from demand to get RC, so a
// replica counted there is one the optimizer will not order. A Pod stuck on an
// image pull or unschedulable on GPU quota is not Ready and never will be, and
// counting it withheld the scale-up that would have cleared the queue -- for as
// long as the Pod existed.
//
// SUBTRACTED from PendingReplicas rather than replacing it. PendingReplicas is
// everything the scale target owns that did not report this cycle, which
// includes replicas that turned Ready between the scrape and now; counting the
// starting Pods directly dropped that term and re-ordered a replica that
// already existed.
var _ = Describe("anticipated supply and the replicas that are stuck", func() {
	const prc = 1000.0
	vc := func(pending, stuck int) domain.VariantCapacity {
		return domain.VariantCapacity{
			VariantName: "v", Role: domain.RoleDecode,
			ReplicaCount: 1, PendingReplicas: pending, StuckReplicas: stuck,
			PerReplicaCapacity: prc,
		}
	}

	It("excludes the pods that will never serve", func() {
		Expect(aggregation.SumTotalAnticipatedSupply(
			[]domain.VariantCapacity{vc(5, 3)})).To(Equal(3 * prc))
	})

	It("keeps the ready-but-unscraped replicas it cannot see", func() {
		// The measured incident: 4 replicas, 2 Ready, 1 row scraped. Nothing is
		// proven stuck, so nothing is subtracted, and the role is not re-ordered
		// a replica it already has.
		Expect(aggregation.SumTotalAnticipatedSupply(
			[]domain.VariantCapacity{vc(3, 0)})).To(Equal(4 * prc))
	})

	It("falls back exactly when the listing proved nothing", func() {
		Expect(aggregation.SumTotalAnticipatedSupply(
			[]domain.VariantCapacity{vc(4, 0)})).To(Equal(5 * prc))
	})

	It("will not let the stuck count manufacture a negative", func() {
		// More stuck than pending is NOT the same case as a negative pending
		// count, which is why this no longer passes through. A negative
		// PendingReplicas is a raw input with a documented clamp downstream
		// (nonNegativeSupply); the stuck gap is DERIVED from two caches with
		// independent lag and goes negative in ordinary operation -- a crashed
		// replica whose metrics row is still cached clamps pending to 0 while
		// the Pod listing already counts it stuck.
		//
		// nonNegativeSupply clamps the SUMMED role figure, so a negative term
		// here is consumed inside the sum and cancels a sibling variant's real
		// supply before any clamp can see it. Held at the ready fleet instead:
		// the most a stuck Pod can honestly say is "nothing is arriving".
		Expect(aggregation.SumTotalAnticipatedSupply(
			[]domain.VariantCapacity{vc(2, 9)})).To(Equal(1 * prc))
	})

	It("applies the same rule per role", func() {
		byRole := aggregation.AggregateByRole([]domain.VariantCapacity{vc(5, 3)})
		Expect(byRole[domain.RoleDecode].TotalAnticipatedSupply).To(Equal(3 * prc))
		Expect(byRole[domain.RoleDecode].TotalSupply).To(Equal(1*prc),
			"ready supply is unchanged by any of this")
	})
})
