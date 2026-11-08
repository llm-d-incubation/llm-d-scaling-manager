package allocation

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// makeNamed builds a NamedAnalyzerResult with the given RC, SC, and per-variant
// (variantName, perReplicaCapacity) pairs. Live defaults to true — tests
// exercising the liveness gate (needsScaleDownForRole, safeRemovalReplicasForRole)
// override it explicitly on the entries they want treated as non-live.
func makeNamed(rc, sc float64, vcs ...any) NamedAnalyzerResult {
	var caps []domain.VariantCapacity
	for i := 0; i+1 < len(vcs); i += 2 {
		vName := vcs[i].(string)
		prc := vcs[i+1].(float64)
		caps = append(caps, domain.VariantCapacity{
			VariantName:        vName,
			PerReplicaCapacity: prc,
		})
	}
	return NamedAnalyzerResult{
		Name: domain.SaturationAnalyzerName,
		Result: &domain.AnalyzerResult{
			VariantCapacities: caps,
		},
		RequiredCapacity: rc,
		SpareCapacity:    sc,
		Remaining:        rc,
		Spare:            sc,
		Live:             true,
	}
}

var _ = Describe("analyzer helpers", func() {

	Describe("applyAllocation", func() {
		It("subtracts n×PRC from Remaining", func() {
			e := makeNamed(500, 0, "v", 100.0)
			applyAllocation(&e, "v", 2)
			Expect(e.Remaining).To(BeNumerically("~", 300.0, 1e-9))
			// RequiredCapacity is the engine-built copy and must not be mutated.
			Expect(e.RequiredCapacity).To(Equal(500.0))
		})

		It("clamps Remaining to 0", func() {
			e := makeNamed(50, 0, "v", 100.0)
			applyAllocation(&e, "v", 2) // would subtract 200 from 50
			Expect(e.Remaining).To(Equal(0.0))
		})

		It("is a no-op for variants not in the result", func() {
			e := makeNamed(200, 0, "other", 100.0)
			applyAllocation(&e, "v", 3)
			Expect(e.Remaining).To(Equal(200.0))
		})
	})

	Describe("ResultIsInformative", func() {
		It("returns false for a nil Result", func() {
			Expect(ResultIsInformative(NamedAnalyzerResult{Result: nil})).To(BeFalse())
		})

		It("returns false when every VariantCapacity is no-data or error", func() {
			nr := NamedAnalyzerResult{Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "a", Reason: "no-data"},
					{VariantName: "b", Reason: "error"},
				},
			}}
			Expect(ResultIsInformative(nr)).To(BeFalse())
		})

		It("returns false for an empty VariantCapacities slice (e.g. throughput with no resolvable ITL model)", func() {
			nr := NamedAnalyzerResult{Result: &domain.AnalyzerResult{}}
			Expect(ResultIsInformative(nr)).To(BeFalse())
		})

		It("returns true when at least one VariantCapacity carries a usable reason", func() {
			nr := NamedAnalyzerResult{Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "a", Reason: "no-data"},
					{VariantName: "b", Reason: "T1-ols"},
				},
			}}
			Expect(ResultIsInformative(nr)).To(BeTrue())
		})
	})
})

// makeNamedPD builds a NamedAnalyzerResult with RoleCapacities for P/D tests.
// RoleSpare is initialized from pSC/dSC (as initDisaggregatedRemaining would do).
// Live defaults to true; override explicitly for non-live-analyzer scenarios.
func makeNamedPD(pRC, dRC, pSC, dSC float64, pDemand, dDemand float64, vPPRC float64, vDPRC float64) NamedAnalyzerResult {
	return NamedAnalyzerResult{
		Name: domain.SaturationAnalyzerName,
		Result: &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "pf", Role: "prefill", PerReplicaCapacity: vPPRC},
				{VariantName: "dc", Role: "decode", PerReplicaCapacity: vDPRC},
			},
		},
		RoleCapacities: map[string]domain.RoleCapacity{
			"prefill": {Role: "prefill", RequiredCapacity: pRC, SpareCapacity: pSC, TotalDemand: pDemand},
			"decode":  {Role: "decode", RequiredCapacity: dRC, SpareCapacity: dSC, TotalDemand: dDemand},
		},
		Remaining: pRC, // P-scope after initDisaggregatedRemaining
		RoleSpare: map[string]float64{"prefill": pSC, "decode": dSC},
		Live:      true,
	}
}

var _ = Describe("paired helpers", func() {

	Describe("initRoleState", func() {
		It("disaggregated: roles from RoleCapacities; picker-state from RC; RoleSpare from SC", func() {
			e := makeNamedPD(15000, 5000, 20000, 10000, 15000, 5000, 10000, 10000)
			roles, ps := initRoleState(&e)
			Expect(roles).To(ConsistOf("prefill", "decode"))
			Expect(ps["prefill"]).To(BeNumerically("~", 15000.0, 1e-9))
			Expect(ps["decode"]).To(BeNumerically("~", 5000.0, 1e-9))
			Expect(e.RoleSpare["prefill"]).To(BeNumerically("~", 20000.0, 1e-9))
			Expect(e.RoleSpare["decode"]).To(BeNumerically("~", 10000.0, 1e-9))
		})

		It("non-disaggregated: synthetic 'both' role using model-level Remaining/Spare", func() {
			e := makeNamed(20000, 5000, "v", 10.0)
			roles, ps := initRoleState(&e)
			Expect(roles).To(ConsistOf(domain.RoleBoth))
			Expect(ps[domain.RoleBoth]).To(BeNumerically("~", 20000.0, 1e-9))
			Expect(e.RoleSpare[domain.RoleBoth]).To(BeNumerically("~", 5000.0, 1e-9))
		})
	})

	Describe("roleBottleneckReplicas", func() {
		It("computes ceil(roleRemaining/PRC)", func() {
			// prefill remaining=10000, PRC=5000 → ceil(10000/5000)=2
			e := makeNamedPD(10000, 20000, 0, 0, 10000, 20000, 5000, 8000)
			_, ps := initRoleState(&e)
			Expect(roleBottleneckReplicas(e, ps, "prefill", "pf")).To(Equal(2))
			// decode: ceil(20000/8000)=3
			Expect(roleBottleneckReplicas(e, ps, "decode", "dc")).To(Equal(3))
		})

		It("returns 0 when PRC=0 (cold-start guard)", func() {
			e := makeNamedPD(10000, 20000, 0, 0, 10000, 20000, 0, 0)
			_, ps := initRoleState(&e)
			Expect(roleBottleneckReplicas(e, ps, "prefill", "pf")).To(Equal(0))
		})
	})

	Describe("safeRemovalReplicasForRole", func() {
		It("computes removable replicas from RoleSpare for a given role", func() {
			// RoleSpare["prefill"]=20000, PRC_P=10000 → floor(20000/10000)=2
			e := makeNamedPD(0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			Expect(safeRemovalReplicasForRole(e, "pf", "prefill")).To(Equal(2))
			// RoleSpare["decode"]=30000, PRC_D=10000 → floor(30000/10000)=3
			Expect(safeRemovalReplicasForRole(e, "dc", "decode")).To(Equal(3))
		})

		It("returns 0 when RoleSpare for role is 0", func() {
			e := makeNamedPD(0, 0, 0, 30000, 10000, 30000, 10000, 10000)
			Expect(safeRemovalReplicasForRole(e, "pf", "prefill")).To(Equal(0))
		})

		It("returns 0 when RoleSpare is nil", func() {
			e := makeNamed(0, 100, "v", 10.0)
			e.RoleSpare = nil
			Expect(safeRemovalReplicasForRole(e, "v", "prefill")).To(Equal(0))
		})

		It("returns 0 for a non-live entry", func() {
			e := makeNamedPD(0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			e.Live = false
			Expect(safeRemovalReplicasForRole(e, "pf", "prefill")).To(Equal(0))
		})
	})

	Describe("applyDeallocationForRole", func() {
		It("decrements RoleSpare[role] by n×PRC", func() {
			// RoleSpare["prefill"]=20000, PRC=10000, n=2 → 20000-20000=0
			e := makeNamedPD(0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			applyDeallocationForRole(&e, "pf", "prefill", 2)
			Expect(e.RoleSpare["prefill"]).To(Equal(0.0))
			// decode spare unchanged
			Expect(e.RoleSpare["decode"]).To(BeNumerically("~", 30000.0, 1e-9))
		})

		It("clamps RoleSpare to 0", func() {
			e := makeNamedPD(0, 0, 5000, 0, 10000, 0, 10000, 10000)
			applyDeallocationForRole(&e, "pf", "prefill", 5) // would subtract 50000
			Expect(e.RoleSpare["prefill"]).To(Equal(0.0))
		})
	})

	Describe("needsScaleDownForRole", func() {
		It("returns true when the live entry has RoleSpare[role] > 0", func() {
			e := makeNamedPD(0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			Expect(needsScaleDownForRole(e, "prefill")).To(BeTrue())
			Expect(needsScaleDownForRole(e, "decode")).To(BeTrue())
		})

		It("returns false when RoleSpare[role] = 0", func() {
			e := makeNamedPD(0, 0, 0, 30000, 10000, 30000, 10000, 10000)
			Expect(needsScaleDownForRole(e, "prefill")).To(BeFalse())
			Expect(needsScaleDownForRole(e, "decode")).To(BeTrue())
		})

		It("returns false for nil RoleSpare", func() {
			e := makeNamed(0, 100, "v", 10.0)
			e.RoleSpare = nil
			Expect(needsScaleDownForRole(e, "prefill")).To(BeFalse())
		})

		It("returns false for a non-live entry (safety floor)", func() {
			e := makeNamedPD(0, 0, 20000, 30000, 10000, 30000, 10000, 10000)
			e.Live = false
			Expect(needsScaleDownForRole(e, "prefill")).To(BeFalse())
			Expect(needsScaleDownForRole(e, "decode")).To(BeFalse())
		})

		It("returns false when the live entry has no spare (real veto preserved)", func() {
			e := makeNamedPD(0, 0, 0, 30000, 10000, 30000, 10000, 10000)
			Expect(e.Live).To(BeTrue())
			Expect(needsScaleDownForRole(e, "prefill")).To(BeFalse())
		})
	})

	Describe("variantsForRole", func() {
		It("filters variants by exact role match", func() {
			vcs := []variantRecord{
				rec("pf", "prefill", 0, 0),
				rec("dc", "decode", 0, 0),
				rec("both", "both", 0, 0),
			}
			Expect(variantsForRole(vcs, "prefill")).To(HaveLen(1))
			Expect(variantsForRole(vcs, "prefill")[0].VariantName).To(Equal("pf"))
			Expect(variantsForRole(vcs, "decode")[0].VariantName).To(Equal("dc"))
		})

		It("matches 'both' query against both explicit 'both' and empty-role variants", func() {
			vcs := []variantRecord{
				rec("pf", "prefill", 0, 0),
				rec("dc", "decode", 0, 0),
				rec("all", "both", 0, 0),
				rec("also-both", "", 0, 0), // empty Role → canonicalized to "both" by variantsForRole
			}
			result := variantsForRole(vcs, "both")
			Expect(result).To(HaveLen(2))
			names := []string{result[0].VariantName, result[1].VariantName}
			Expect(names).To(ConsistOf("all", "also-both"))
			// querying "" matches nothing (vc empty roles are canonicalized to "both", not "")
			Expect(variantsForRole(vcs, "")).To(BeEmpty())
		})
	})

})

// The figures are run T's (2026-09-28), phase-1 steady state, read off the
// controller's analyzer-result and throughput-demand-floor-total lines:
//
//	demand after the floor      5,383,320 tokens
//	decode supply at 9 replicas 8,374,572 tokens  (9 x 930,508)
//	utilisation                 0.643             (below the 0.70 boundary)
//	one replica                   930,508 tokens
//
// The fleet wanted to shrink and could not: RoleSpare, measured against the
// BOTTOM of the hysteresis band, was 684,115 -- less than one replica -- so
// floor(684,115/930,508) released nothing, and the run held nine replicas for
// nineteen minutes at an identical median TTFT to main's seven.
var _ = Describe("releasing against a target inside the band", func() {
	const (
		prc     = 930508.0
		demand  = 5383320.0
		scaleUp = 0.85
		scaleDn = 0.70
		variant = "decode-v"
	)
	supplyFor := func(replicas float64) float64 { return replicas * prc }

	// One decode role, sized as run T's was at the given replica count.
	atReplicas := func(replicas float64) *NamedAnalyzerResult {
		supply := supplyFor(replicas)
		e := &NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				TotalDemand: demand,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: variant, PerReplicaCapacity: prc, Role: domain.RoleDecode},
				},
			},
			ScaleUpThreshold:  scaleUp,
			ScaleDownBoundary: scaleDn,
			TotalSupply:       supply,
			RoleCapacities: map[string]domain.RoleCapacity{
				domain.RoleDecode: {
					Role:        domain.RoleDecode,
					TotalSupply: supply,
					TotalDemand: demand,
					// As the engine computes it: supply - demand/scaleDownBoundary.
					SpareCapacity: math.Max(0, supply-demand/scaleDn),
				},
			},
			Live: true,
		}
		initRoleState(e)
		return e
	}

	It("releases the replica the boundary-measured budget refused", func() {
		e := atReplicas(9)

		By("confirming the old budget could not have released one")
		Expect(e.RoleSpare[domain.RoleDecode]).To(BeNumerically("~", 684115, 1),
			"this is the figure run T logged")
		Expect(e.RoleSpare[domain.RoleDecode]).To(BeNumerically("<", prc),
			"less than one replica, so dividing by prc floors to zero")

		By("and that the role is nonetheless allowed to consider releasing")
		Expect(needsScaleDownForRole(*e, domain.RoleDecode)).To(BeTrue(),
			"utilisation 0.643 is below the 0.70 boundary; the entry hysteresis is unchanged")

		Expect(safeRemovalReplicasForRole(*e, variant, domain.RoleDecode)).To(Equal(1),
			"one replica, measured against the midpoint of the band")
	})

	It("stops at eight rather than chasing main's seven", func() {
		// Eight replicas is utilisation 0.723 -- inside the band. Seven would be
		// 0.826, which is 0.024 from the scale-up threshold, and releasing to
		// there is how a fleet flaps. The gain is deliberately 9 -> 8.
		e := atReplicas(8)
		Expect(safeRemovalReplicasForRole(*e, variant, domain.RoleDecode)).To(Equal(0),
			"the release target is reached; nothing further is owed")
	})

	It("never offers less than the boundary-measured budget did", func() {
		// The change may only ADD permission. Where RoleSpare was derived from
		// something other than this supply figure it stays in force, so no
		// scale-down that works today is withdrawn.
		e := atReplicas(9)
		e.RoleSpare[domain.RoleDecode] = 5 * prc // a budget from elsewhere, larger
		Expect(safeRemovalReplicasForRole(*e, variant, domain.RoleDecode)).To(Equal(5))
	})

	It("charges a release against both budgets", func() {
		// A role with two variants must not release the same capacity twice.
		e := atReplicas(9)
		before := e.RoleReleasable[domain.RoleDecode]
		applyDeallocationForRole(e, variant, domain.RoleDecode, 1)
		Expect(e.RoleReleasable[domain.RoleDecode]).To(BeNumerically("~", before-prc, 1e-6))
		Expect(e.RoleSpare[domain.RoleDecode]).To(BeZero(),
			"684,115 charged one replica floors at zero")
	})

	It("falls back to the boundary budget when no supply figure is known", func() {
		// A fixture or optimizer path that sets Spare without a supply figure
		// keeps its old behaviour rather than losing its release entirely.
		e := &NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: variant, PerReplicaCapacity: prc},
				},
			},
			ScaleUpThreshold:  scaleUp,
			ScaleDownBoundary: scaleDn,
			Spare:             3 * prc,
			Live:              true,
		}
		initRoleState(e)
		Expect(e.RoleReleasable).NotTo(HaveKey(domain.RoleBoth),
			"nothing was computable, so nothing is recorded")
		Expect(safeRemovalReplicasForRole(*e, variant, domain.RoleBoth)).To(Equal(3))
	})

	It("derives the target from the two thresholds, and degrades safely", func() {
		e := atReplicas(9)
		Expect(e.releaseTargetUtilization()).To(BeNumerically("~", 0.7375, 1e-9),
			"a quarter of the way up the band, not the middle of it")

		By("leaving most of the band as anti-flap margin")
		// The distance from the release target to the scale-up threshold IS the
		// margin, and it is the reason this is a quarter rather than a half.
		// sticky.go releases its scale-down hold exactly when demand at that
		// count would reach the scale-up threshold, and KEDA's HPA takes the max
		// over 300 s on the way down -- so one re-ordered replica in twenty
		// cycles throws the release away. The midpoint left +9.7%; this leaves
		// over 15%.
		headroom := scaleUp/e.releaseTargetUtilization() - 1
		Expect(headroom).To(BeNumerically(">", 0.15))

		By("falling back to the boundary when the thresholds are unusable")
		bad := *e
		bad.ScaleUpThreshold = 0.5 // inverted against the 0.70 boundary
		Expect(bad.releaseTargetUtilization()).To(Equal(scaleDn))
	})
})
