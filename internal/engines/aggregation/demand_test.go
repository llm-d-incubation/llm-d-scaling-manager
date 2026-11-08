package aggregation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

var _ = Describe("DemandForRole", func() {

	Describe("non-disaggregated layout (RoleDemand nil)", func() {
		It("reads TotalDemand for domain.RoleBoth", func() {
			result := &domain.AnalyzerResult{TotalDemand: 42.0, RoleDemand: nil}
			value, present := DemandForRole(result, domain.RoleBoth)
			Expect(present).To(BeTrue())
			Expect(value).To(Equal(42.0))
		})

		It("canonicalizes an empty role string to domain.RoleBoth", func() {
			result := &domain.AnalyzerResult{TotalDemand: 42.0, RoleDemand: nil}
			value, present := DemandForRole(result, "")
			Expect(present).To(BeTrue())
			Expect(value).To(Equal(42.0))
		})

		It("reads a real zero TotalDemand as present, not absent", func() {
			result := &domain.AnalyzerResult{TotalDemand: 0, RoleDemand: nil}
			value, present := DemandForRole(result, domain.RoleBoth)
			Expect(present).To(BeTrue())
			Expect(value).To(BeZero())
		})

		It("reports not-present for a specific role when the layout is non-disaggregated", func() {
			result := &domain.AnalyzerResult{TotalDemand: 42.0, RoleDemand: nil}
			_, present := DemandForRole(result, "prefill")
			Expect(present).To(BeFalse())

			_, present = DemandForRole(result, "decode")
			Expect(present).To(BeFalse())
		})
	})

	Describe("disaggregated layout (RoleDemand non-nil)", func() {
		It("reads a present role's value", func() {
			result := &domain.AnalyzerResult{
				RoleDemand: map[string]float64{"prefill": 10.0, "decode": 20.0},
			}
			value, present := DemandForRole(result, "prefill")
			Expect(present).To(BeTrue())
			Expect(value).To(Equal(10.0))

			value, present = DemandForRole(result, "decode")
			Expect(present).To(BeTrue())
			Expect(value).To(Equal(20.0))
		})

		It("distinguishes present-and-zero from absent", func() {
			result := &domain.AnalyzerResult{
				RoleDemand: map[string]float64{"prefill": 0.0},
			}
			value, present := DemandForRole(result, "prefill")
			Expect(present).To(BeTrue())
			Expect(value).To(BeZero())

			_, present = DemandForRole(result, "decode")
			Expect(present).To(BeFalse())
		})

		It("canonicalizes an empty role to domain.RoleBoth when looking up a disaggregated map", func() {
			result := &domain.AnalyzerResult{
				RoleDemand: map[string]float64{domain.RoleBoth: 5.0, "prefill": 10.0},
			}
			value, present := DemandForRole(result, "")
			Expect(present).To(BeTrue())
			Expect(value).To(Equal(5.0))
		})

		It("reports not-present for a role entirely absent from the map", func() {
			result := &domain.AnalyzerResult{
				RoleDemand: map[string]float64{"prefill": 10.0},
			}
			_, present := DemandForRole(result, "decode")
			Expect(present).To(BeFalse())
		})
	})

	// Demand is per (model, role) and does not depend on which SOs exist. Adding or
	// removing a VariantCapacities entry (an SO) for a role must never
	// change that role's DemandForRole reading, because the accessor reads
	// only RoleDemand/TotalDemand -- never VariantCapacities -- by
	// construction.
	It("is independent of which SOs (VariantCapacities entries) exist for the role", func() {
		before := &domain.AnalyzerResult{
			RoleDemand:        map[string]float64{"prefill": 400},
			VariantCapacities: []domain.VariantCapacity{{VariantName: "p1", Role: "prefill", PerReplicaCapacity: 100}},
		}
		demandBefore, presentBefore := DemandForRole(before, "prefill")
		Expect(presentBefore).To(BeTrue())
		Expect(demandBefore).To(Equal(400.0))

		// Add a second SO for the same role.
		added := &domain.AnalyzerResult{
			RoleDemand: map[string]float64{"prefill": 400}, // unchanged
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "p1", Role: "prefill", PerReplicaCapacity: 100},
				{VariantName: "p2", Role: "prefill", PerReplicaCapacity: 150}, // new SO
			},
		}
		demandAfterAdd, presentAfterAdd := DemandForRole(added, "prefill")
		Expect(presentAfterAdd).To(BeTrue())
		Expect(demandAfterAdd).To(Equal(demandBefore), "adding an SO must not change the role's demand")

		// Remove every SO for the role entirely.
		removed := &domain.AnalyzerResult{
			RoleDemand:        map[string]float64{"prefill": 400}, // still unchanged -- demand is analyzer-owned, not derived from SOs
			VariantCapacities: nil,
		}
		demandAfterRemove, presentAfterRemove := DemandForRole(removed, "prefill")
		Expect(presentAfterRemove).To(BeTrue())
		Expect(demandAfterRemove).To(Equal(demandBefore), "removing every SO must not change the role's demand")
	})
})
