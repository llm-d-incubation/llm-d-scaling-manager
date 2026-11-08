package allocation

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// maxReplicaCount bounds every replica count converted from a float. A
// float64 at or above 2^63 converts to an int that is negative on amd64, and
// the guards below then read a need that overflowed as no need at all.
const maxReplicaCount = math.MaxInt32

// replicaCount converts a non-negative replica count to an int, at most
// maxReplicaCount. A NaN or a negative count is 0.
func replicaCount(x float64) int {
	if !(x > 0) {
		return 0
	}
	if x >= maxReplicaCount {
		return maxReplicaCount
	}
	return int(x)
}

// replicasForDemand returns ceil(demand/prc) — the single shared definition
// of "how many replicas does this demand need at this per-replica capacity".
// Every partial-replica-rounding site
// in the optimizer goes through this one function, so a change to the
// rounding rule cannot land in one site and drift from another.
//
// Returns 0 when prc <= 0 (no capacity to divide by — the same "no basis"
// reading roleBottleneckReplicas and safeRemovalReplicasForRole already use)
// or when the result would be negative (a negative demand is not a
// meaningful replica count), and at most maxReplicaCount.
func replicasForDemand(demand, prc float64) int {
	if prc <= 0 {
		return 0
	}
	return replicaCount(math.Ceil(demand / prc))
}

// safeReplicasForSpare returns floor(spare/prc) — the single shared
// definition of "how many replicas can safely be removed for this much
// spare capacity at this per-replica capacity", the rounding-down
// counterpart of replicasForDemand (used by safeRemovalReplicasForRole).
//
// Returns 0 when prc <= 0 or the result would be negative, mirroring
// replicasForDemand's guards — rounding DOWN is the conservative direction
// for a removal count exactly as rounding UP is for a demand count, and
// both must refuse rather than guess when there is no capacity to divide by.
func safeReplicasForSpare(spare, prc float64) int {
	if prc <= 0 {
		return 0
	}
	return replicaCount(math.Floor(spare / prc))
}

// demandForRoleOrModel returns nr's demand for role — the single shared
// definition of the role-vs-model demand-read fallback: domain.RoleBoth (or an empty role) reads the model-level
// TotalDemand directly; any other role reads the ENGINE-BUILT
// RoleCapacities[role].TotalDemand, falling back to the model-level
// TotalDemand only on a genuine map miss. Returns 0 when nr has no Result.
//
// Reads RoleCapacities, not aggregation.DemandForRole(nr.Result, role)
// directly — the two are equal in real production data (buildRoleCapacities
// derives RoleCapacities[role].TotalDemand from Result.RoleDemand[role]
// exactly), but this mirrors requiredSpareForRoleOrModel's source of truth
// (also RoleCapacities, engine-built) so both halves of one role-vs-model
// fallback read the SAME map rather than two maps that happen to agree in
// production but can diverge in a hand-built NamedAnalyzerResult (as
// several existing rescale tests construct RoleCapacities directly without
// going through Result.RoleDemand).
func demandForRoleOrModel(nr NamedAnalyzerResult, role string) float64 {
	if nr.Result == nil {
		return 0
	}
	if role == "" {
		role = domain.RoleBoth
	}
	if role != domain.RoleBoth {
		if rc, ok := nr.RoleCapacities[role]; ok {
			return rc.TotalDemand
		}
	}
	return nr.Result.TotalDemand
}

// requiredSpareForRoleOrModel returns nr's RequiredCapacity/SpareCapacity for
// role, falling back to the model-level scalars exactly as
// demandForRoleOrModel does for demand (the same role-vs-model fallback
// shape, applied to the engine-built RC/SC pair rather than analyzer-owned
// demand). RoleCapacities is engine-built
// (buildRoleCapacities), so there is no accessor to share with
// demandForRoleOrModel the way DemandForRole is shared — this is its own
// small helper, but the SAME fallback shape, so a caller cannot special-case
// one without the other drifting.
//
// The RoleCapacities lookup is attempted for EVERY role, RoleBoth included —
// a disaggregated model can still carry a "both" RoleCapacities entry (e.g.
// an empty-Role variant mapped in alongside prefill/decode), and that entry
// must win over the model-level scalars when present. Only a genuine map
// miss falls back to the model-level RequiredCapacity/SpareCapacity.
func requiredSpareForRoleOrModel(nr NamedAnalyzerResult, role string) (requiredCapacity, spareCapacity float64) {
	requiredCapacity, spareCapacity = nr.RequiredCapacity, nr.SpareCapacity
	if role == "" {
		role = domain.RoleBoth
	}
	if rc, ok := nr.RoleCapacities[role]; ok {
		return rc.RequiredCapacity, rc.SpareCapacity
	}
	return requiredCapacity, spareCapacity
}
