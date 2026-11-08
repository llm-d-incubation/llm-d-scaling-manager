package allocation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

var _ = Describe("replicasForDemand", func() {
	It("returns ceil(demand/prc)", func() {
		Expect(replicasForDemand(1000, 300)).To(Equal(4)) // ceil(3.33) = 4
	})

	It("returns 0 when prc is 0", func() {
		Expect(replicasForDemand(1000, 0)).To(Equal(0))
	})

	It("returns 0 when prc is negative", func() {
		Expect(replicasForDemand(1000, -5)).To(Equal(0))
	})

	It("returns 0 for a negative demand rather than a negative replica count", func() {
		Expect(replicasForDemand(-1000, 300)).To(Equal(0))
	})

	It("returns 0 for zero demand", func() {
		Expect(replicasForDemand(0, 300)).To(Equal(0))
	})
})

var _ = Describe("safeReplicasForSpare", func() {
	It("returns floor(spare/prc)", func() {
		Expect(safeReplicasForSpare(1000, 300)).To(Equal(3)) // floor(3.33) = 3
	})

	It("returns 0 when prc is 0", func() {
		Expect(safeReplicasForSpare(1000, 0)).To(Equal(0))
	})

	It("returns 0 when prc is negative", func() {
		Expect(safeReplicasForSpare(1000, -5)).To(Equal(0))
	})

	It("returns 0 for negative spare rather than a negative replica count", func() {
		Expect(safeReplicasForSpare(-1000, 300)).To(Equal(0))
	})
})

var _ = Describe("demandForRoleOrModel", func() {
	It("returns 0 for a nil Result", func() {
		Expect(demandForRoleOrModel(NamedAnalyzerResult{Result: nil}, "prefill")).To(BeZero())
	})

	It("returns model-level TotalDemand for domain.RoleBoth", func() {
		nr := NamedAnalyzerResult{Result: &domain.AnalyzerResult{TotalDemand: 500}}
		Expect(demandForRoleOrModel(nr, domain.RoleBoth)).To(Equal(500.0))
	})

	It("canonicalizes an empty role to domain.RoleBoth", func() {
		nr := NamedAnalyzerResult{Result: &domain.AnalyzerResult{TotalDemand: 500}}
		Expect(demandForRoleOrModel(nr, "")).To(Equal(500.0))
	})

	It("reads RoleCapacities[role].TotalDemand for a non-both role when present", func() {
		nr := NamedAnalyzerResult{
			Result:         &domain.AnalyzerResult{TotalDemand: 9999}, // decoy
			RoleCapacities: map[string]domain.RoleCapacity{"prefill": {TotalDemand: 400}},
		}
		Expect(demandForRoleOrModel(nr, "prefill")).To(Equal(400.0))
	})

	It("falls back to model-level TotalDemand when the role is absent from RoleCapacities", func() {
		nr := NamedAnalyzerResult{
			Result:         &domain.AnalyzerResult{TotalDemand: 700},
			RoleCapacities: map[string]domain.RoleCapacity{"decode": {TotalDemand: 400}}, // no "prefill"
		}
		Expect(demandForRoleOrModel(nr, "prefill")).To(Equal(700.0))
	})

	It("never consults RoleCapacities for domain.RoleBoth even if a \"both\" entry exists", func() {
		// As roleDemandGPUs needs: the "both" role always reads the
		// model-level scalar, unconditionally.
		nr := NamedAnalyzerResult{
			Result:         &domain.AnalyzerResult{TotalDemand: 500},
			RoleCapacities: map[string]domain.RoleCapacity{domain.RoleBoth: {TotalDemand: 999}},
		}
		Expect(demandForRoleOrModel(nr, domain.RoleBoth)).To(Equal(500.0))
	})
})

var _ = Describe("requiredSpareForRoleOrModel", func() {
	It("returns model-level RequiredCapacity/SpareCapacity when RoleCapacities has no entry for role", func() {
		nr := NamedAnalyzerResult{RequiredCapacity: 100, SpareCapacity: 10}
		rc, sc := requiredSpareForRoleOrModel(nr, "prefill")
		Expect(rc).To(Equal(100.0))
		Expect(sc).To(Equal(10.0))
	})

	It("prefers RoleCapacities[role] when present, for a non-both role", func() {
		nr := NamedAnalyzerResult{
			RequiredCapacity: 9999, SpareCapacity: 8888, // decoys
			RoleCapacities: map[string]domain.RoleCapacity{"prefill": {RequiredCapacity: 100, SpareCapacity: 10}},
		}
		rc, sc := requiredSpareForRoleOrModel(nr, "prefill")
		Expect(rc).To(Equal(100.0))
		Expect(sc).To(Equal(10.0))
	})

	It("canonicalizes an empty role to domain.RoleBoth before the lookup", func() {
		nr := NamedAnalyzerResult{
			RequiredCapacity: 9999, SpareCapacity: 8888,
			RoleCapacities: map[string]domain.RoleCapacity{domain.RoleBoth: {RequiredCapacity: 300, SpareCapacity: 30}},
		}
		rc, sc := requiredSpareForRoleOrModel(nr, "")
		Expect(rc).To(Equal(300.0))
		Expect(sc).To(Equal(30.0))
	})

	// Confirms the deliberate difference from demandForRoleOrModel: THIS
	// helper attempts the RoleCapacities lookup for domain.RoleBoth too (a
	// disaggregated model may still carry a "both" RoleCapacities entry,
	// e.g. an empty-Role variant mapped in alongside prefill/decode), unlike
	// demandForRoleOrModel which never consults RoleCapacities for
	// RoleBoth. Each matches what its own call sites need.
	It("DOES consult RoleCapacities for domain.RoleBoth when a \"both\" entry exists", func() {
		nr := NamedAnalyzerResult{
			RequiredCapacity: 9999, SpareCapacity: 8888,
			RoleCapacities: map[string]domain.RoleCapacity{domain.RoleBoth: {RequiredCapacity: 300, SpareCapacity: 30}},
		}
		rc, sc := requiredSpareForRoleOrModel(nr, domain.RoleBoth)
		Expect(rc).To(Equal(300.0))
		Expect(sc).To(Equal(30.0))
	})
})

// Consistency within each category of helper: rounding, and the
// role-vs-model fallback. replicasForDemand is the ONLY rounding definition
// roleDemandGPUs/roleBottleneckReplicas/safeRemovalReplicasForRole go
// through, so any two of them fed the same (demand, prc) cannot disagree --
// there is no second implementation left to drift.
var _ = Describe("query API consistency", func() {
	It("replicasForDemand and safeReplicasForSpare agree on direction: ceil >= floor for the same inputs", func() {
		demand, prc := 1000.0, 300.0
		Expect(replicasForDemand(demand, prc)).To(BeNumerically(">=", safeReplicasForSpare(demand, prc)))
	})

	It("roleBottleneckReplicas and a direct replicasForDemand call agree for the same (demand, prc)", func() {
		e := NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 300}},
			},
		}
		state := RolePairedState{"prefill": 1000}
		Expect(roleBottleneckReplicas(e, state, "prefill", "v")).To(Equal(replicasForDemand(1000, 300)),
			"roleBottleneckReplicas must not compute a different rounding than the shared definition")
	})

	It("demandForRoleOrModel and requiredSpareForRoleOrModel agree on which source wins for the same role", func() {
		// Both helpers implement the same role-vs-model fallback shape:
		// when a role has an explicit
		// RoleCapacities entry, both read from it rather than the
		// model-level scalars, for the same role, on the same NamedAnalyzerResult.
		nr := NamedAnalyzerResult{
			RequiredCapacity: 9999, SpareCapacity: 8888,
			Result: &domain.AnalyzerResult{TotalDemand: 7777},
			RoleCapacities: map[string]domain.RoleCapacity{
				"prefill": {TotalDemand: 400, RequiredCapacity: 40, SpareCapacity: 4},
			},
		}
		Expect(demandForRoleOrModel(nr, "prefill")).To(Equal(400.0))
		rc, sc := requiredSpareForRoleOrModel(nr, "prefill")
		Expect(rc).To(Equal(40.0))
		Expect(sc).To(Equal(4.0))
	})
})

// Every query-API helper here is a pure function over its arguments -- no
// mutable state, no hidden clock -- so calling it twice on the same inputs
// must give the same answer, unlike the optimizer's mutable
// Remaining/RoleSpare fields.
var _ = Describe("query API repeatability", func() {
	It("replicasForDemand gives the same answer on repeated calls with the same inputs", func() {
		first := replicasForDemand(1000, 333)
		second := replicasForDemand(1000, 333)
		Expect(first).To(Equal(second))
	})

	It("demandForRoleOrModel and requiredSpareForRoleOrModel give the same answer on repeated calls, current/anticipated/partial notwithstanding", func() {
		// The helpers do not know or care which allocation phase they are
		// called in -- they read whatever NamedAnalyzerResult they are
		// given, which is the caller's job to keep current. Calling twice
		// on an UNCHANGED value must be idempotent.
		nr := NamedAnalyzerResult{
			RequiredCapacity: 100, SpareCapacity: 10,
			Result:         &domain.AnalyzerResult{TotalDemand: 500},
			RoleCapacities: map[string]domain.RoleCapacity{"prefill": {TotalDemand: 200, RequiredCapacity: 20, SpareCapacity: 2}},
		}
		d1 := demandForRoleOrModel(nr, "prefill")
		d2 := demandForRoleOrModel(nr, "prefill")
		Expect(d1).To(Equal(d2))

		rc1, sc1 := requiredSpareForRoleOrModel(nr, "prefill")
		rc2, sc2 := requiredSpareForRoleOrModel(nr, "prefill")
		Expect(rc1).To(Equal(rc2))
		Expect(sc1).To(Equal(sc2))
	})
})
