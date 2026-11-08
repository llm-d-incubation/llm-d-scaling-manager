package allocation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// pairedFleet builds the smallest two-role model that exercises the joint
// commit: one variant per role, equal cost and per-replica capacity, so the
// only thing separating the roles in these tests is headroom and demand.
//
// Decode's required capacity is fixed: every case here wants decode to have
// real demand it cannot serve, and only prefill's figure varies.
func pairedFleet(
	decodeTarget, decodeMax, prefillTarget, prefillMax int,
	prefillRC float64,
) (NamedAnalyzerResult, []variantRecord, map[string]domain.VariantReplicaState, map[string]int) {
	const prc = 100.0
	const decodeRC = 400.0

	variants := []variantRecord{
		rec("decode", "decode", 1.0, prc),
		rec("prefill", "prefill", 1.0, prc),
	}

	dMax, pMax := decodeMax, prefillMax
	stateMap := map[string]domain.VariantReplicaState{
		"decode":  {VariantName: "decode", CurrentReplicas: decodeTarget, MaxReplicas: &dMax},
		"prefill": {VariantName: "prefill", CurrentReplicas: prefillTarget, MaxReplicas: &pMax},
	}

	e := NamedAnalyzerResult{
		Name: domain.SaturationAnalyzerName,
		Result: &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "decode", Role: "decode", PerReplicaCapacity: prc},
				{VariantName: "prefill", Role: "prefill", PerReplicaCapacity: prc},
			},
		},
		RoleCapacities: map[string]domain.RoleCapacity{
			"decode": {
				Role:                   "decode",
				RequiredCapacity:       decodeRC,
				TotalSupply:            float64(decodeTarget) * prc,
				TotalAnticipatedSupply: float64(decodeTarget) * prc,
			},
			"prefill": {
				Role:                   "prefill",
				RequiredCapacity:       prefillRC,
				TotalSupply:            float64(prefillTarget) * prc,
				TotalAnticipatedSupply: float64(prefillTarget) * prc,
			},
		},
		Live: true,
	}

	targets := map[string]int{"decode": decodeTarget, "prefill": prefillTarget}
	return e, variants, stateMap, targets
}

var _ = Describe("allocateForModelPaired with a role at its ceiling", func() {
	// The joint commit exists so a P/D fleet keeps its ratio. A role that has
	// reached its own maxReplicas is finished, not blocking -- it must not
	// prevent its partner from being ordered. Measured on a cluster: decode
	// pinned at 9/9 of max 9 left prefill at 1 replica through a 1119-deep
	// queue with nine replicas of its own headroom unused, in every cycle of
	// the run. See docs/proposals/prefill-starved-by-exhausted-role.md.
	It("still orders prefill when decode is at maxReplicas", func() {
		e, variants, stateMap, targets := pairedFleet(9, 9, 1, 10, 400)

		roles, ps := initRoleState(&e)
		// Roles are the sorted keys, so decode -- the exhausted one -- is
		// picked first. That ordering is what made the old code abandon the
		// pass before prefill was ever committed.
		Expect(roles).To(Equal([]string{"decode", "prefill"}))

		allocateForModelPaired(context.Background(), &e, variants, stateMap,
			nil, targets, costGreedyRolePick, ps, roles)

		Expect(targets["decode"]).To(Equal(9), "decode is at its ceiling and must not exceed it")
		Expect(targets["prefill"]).To(BeNumerically(">", 1), "prefill has headroom and demand and must grow")
	})

	It("does not exceed prefill's own ceiling", func() {
		e, variants, stateMap, targets := pairedFleet(9, 9, 1, 3, 10000)

		roles, ps := initRoleState(&e)
		allocateForModelPaired(context.Background(), &e, variants, stateMap,
			nil, targets, costGreedyRolePick, ps, roles)

		Expect(targets["prefill"]).To(BeNumerically("<=", 3))
		Expect(targets["prefill"]).To(BeNumerically(">", 1))
	})

	It("terminates when every role is exhausted", func() {
		e, variants, stateMap, targets := pairedFleet(9, 9, 10, 10, 400)

		roles, ps := initRoleState(&e)
		allocateForModelPaired(context.Background(), &e, variants, stateMap,
			nil, targets, costGreedyRolePick, ps, roles)

		// Unservable demand must be dropped, or anyRoleNeedsScaleUp stays true
		// on it and the loop spins forever.
		Expect(targets["decode"]).To(Equal(9))
		Expect(targets["prefill"]).To(Equal(10))
	})

	// The control: with headroom on both sides the joint commit is unchanged,
	// so the fix cannot be mistaken for "scale every role independently".
	It("still commits both roles together when both have headroom", func() {
		e, variants, stateMap, targets := pairedFleet(1, 10, 1, 10, 400)

		roles, ps := initRoleState(&e)
		allocateForModelPaired(context.Background(), &e, variants, stateMap,
			nil, targets, costGreedyRolePick, ps, roles)

		Expect(targets["decode"]).To(BeNumerically(">", 1))
		Expect(targets["prefill"]).To(BeNumerically(">", 1))
	})
})

// accH100 is the accelerator of the role whose pool runs dry.
const accH100 = "H100"

var _ = Describe("allocateForModelPaired with a role that needs nothing", func() {
	// A role whose required capacity is zero commits nothing in the joint
	// commit (its k is 0), so its only possible effect on the pass was a veto.
	// Picking it anyway let an empty GPU pool on ITS accelerator abort the
	// pass for a partner that needed capacity and had GPUs to take it -- and
	// blamed the idle role as GPU-limited.
	It("orders decode on free H200 while prefill needs nothing and its H100 pool is empty", func() {
		e, variants, stateMap, targets := pairedFleet(1, 10, 1, 10, 0)
		variants[0].AcceleratorName = "H200"
		variants[1].AcceleratorName = accH100
		available := map[string]int{"H200": 8, accH100: 0}
		limited := make(gpuLimitTracker)

		roles, ps := initRoleState(&e)
		pick := fairShareRolePick(1e9, roles, limited)
		allocateForModelPaired(context.Background(), &e, variants, stateMap,
			available, targets, pick, ps, roles)

		// decodeRC 400 at 100 per replica: four more replicas.
		Expect(targets["decode"]).To(Equal(5), "decode needs four replicas and has 8 free H200")
		Expect(targets["prefill"]).To(Equal(1), "prefill needs nothing and must not grow")
		Expect(limited).NotTo(HaveKey("prefill"),
			"a role that asked for nothing was not held back by the GPU budget")
		Expect(available["H200"]).To(Equal(4))
	})

	It("orders decode on one accelerator when only prefill's replica is too large to fit", func() {
		e, variants, stateMap, targets := pairedFleet(1, 10, 1, 10, 0)
		variants[0].AcceleratorName = accH100
		variants[1].AcceleratorName = accH100
		decode, prefill := stateMap["decode"], stateMap["prefill"]
		decode.GPUsPerReplica, prefill.GPUsPerReplica = 2, 8
		stateMap["decode"], stateMap["prefill"] = decode, prefill
		available := map[string]int{accH100: 4}
		limited := make(gpuLimitTracker)

		roles, ps := initRoleState(&e)
		pick := fairShareRolePick(1e9, roles, limited)
		allocateForModelPaired(context.Background(), &e, variants, stateMap,
			available, targets, pick, ps, roles)

		Expect(targets["decode"]).To(Equal(3), "4 free GPUs at 2 per decode replica: two more")
		Expect(targets["prefill"]).To(Equal(1))
		Expect(limited).NotTo(HaveKey("prefill"))
		Expect(limited).To(HaveKey("decode"), "decode wanted four and the budget held it to two")
	})

	// The control: a role that DOES need capacity and cannot get GPUs still
	// keeps the pair together, as before. Spending GPUs on one side of a P/D
	// fleet whose other side cannot follow buys no throughput.
	It("still holds decode when prefill needs capacity and has no GPUs", func() {
		e, variants, stateMap, targets := pairedFleet(1, 10, 1, 10, 400)
		variants[0].AcceleratorName = "H200"
		variants[1].AcceleratorName = accH100
		available := map[string]int{"H200": 8, accH100: 0}
		limited := make(gpuLimitTracker)

		roles, ps := initRoleState(&e)
		pick := fairShareRolePick(1e9, roles, limited)
		allocateForModelPaired(context.Background(), &e, variants, stateMap,
			available, targets, pick, ps, roles)

		Expect(targets["decode"]).To(Equal(1))
		Expect(targets["prefill"]).To(Equal(1))
		Expect(limited).To(HaveKey("prefill"), "prefill needed capacity and the budget had none")
	})
})
