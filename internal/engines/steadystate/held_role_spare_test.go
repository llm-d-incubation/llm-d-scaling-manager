package steadystate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// heldPrefillEntry is a P/D saturation entry whose prefill runs n replicas of
// prc. Held, its demand is raised the way saturation's holds raise it, to
// scaleDown x supply, and its demand before the hold is a tenth of supply.
// Decode runs two replicas of 300 at half load, which releases nothing.
func heldPrefillEntry(n int, prc, scaleDown, prefill float64, held bool) allocation.NamedAnalyzerResult {
	if held {
		prefill = scaleDown * (float64(n) * prc)
	}
	e := variantsEntry(domain.SaturationAnalyzerName, prefill+300,
		domain.VariantCapacity{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: n, PerReplicaCapacity: prc, TotalDemand: prefill},
		domain.VariantCapacity{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 2, PerReplicaCapacity: 300, TotalDemand: 300})
	e.Result.RoleDemand = map[string]float64{domain.RolePrefill: prefill, domain.RoleDecode: 300}
	if held {
		e.Result.HeldRoleDemand = map[string]float64{domain.RolePrefill: 0.1 * float64(n) * prc}
	}
	return e
}

// costAwareTargetsAt is costAwareTargets at the given thresholds, with
// eligible as the analyzers the policy lists.
func costAwareTargetsAt(named, eligible []allocation.NamedAnalyzerResult, scaleUp, scaleDown float64,
	current map[string]int) (map[string]int, allocation.NamedAnalyzerResult) {
	composite := buildComposite(context.Background(), named, eligible, scaleUp, scaleDown)
	states := make([]domain.VariantReplicaState, 0, len(composite.Result.VariantCapacities))
	metas := make([]domain.VariantMetadata, 0, len(composite.Result.VariantCapacities))
	for _, vc := range composite.Result.VariantCapacities {
		states = append(states, domain.VariantReplicaState{VariantName: vc.VariantName, Role: vc.Role,
			CurrentReplicas: current[vc.VariantName], GPUsPerReplica: 1})
		metas = append(metas, domain.VariantMetadata{VariantName: vc.VariantName, ModelID: "m", Namespace: "ns",
			Role: vc.Role, Cost: 1, AcceleratorName: "H100"})
	}
	req := allocation.ModelScalingRequest{ModelID: "m", Namespace: "ns", CompositeSignal: composite,
		VariantStates: states, Variants: metas, Priority: 1}
	out := map[string]int{}
	for _, d := range allocation.NewCostAwareOptimizer().Optimize(context.Background(), []allocation.ModelScalingRequest{req}, nil) {
		out[d.VariantName] = d.TargetReplicas
	}
	return out, composite
}

// heldCases are the issue's examples. At 0.70 the supply of 21 x 24576
// comes back from scaleDown x supply / scaleDown 5.8e-11 above itself; 20 x
// 165176.33 comes back exact at 0.70 and 4.7e-10 above at 0.69. 21 x 100 is
// the control: exact at 0.70.
var heldCases = []struct {
	name               string
	n                  int
	prc                float64
	scaleUp, scaleDown float64
}{
	{"21x24576", 21, 24576, 0.85, 0.70},
	{"20x165176.33", 20, 165176.33, 0.84, 0.69},
	{"21x100 control", 21, 100, 0.85, 0.70},
}

// TestHeldRoleReleasesNothingOnASpareResidue: a role saturation holds
// releases nothing, alone or listed beside another analyzer whose lower
// reading leaves saturation's held figure in place. Its spare came out a
// rounding residue above zero, which let the role release up to its
// releasable capacity, about 5% of supply: a whole replica at twenty, under
// the hold meant to stop it (issue 194).
func TestHeldRoleReleasesNothingOnASpareResidue(t *testing.T) {
	for _, c := range heldCases {
		t.Run(c.name, func(t *testing.T) {
			sat := heldPrefillEntry(c.n, c.prc, c.scaleDown, 0, true)
			current := map[string]int{"p": c.n, "d": 2}

			alone := []allocation.NamedAnalyzerResult{sat}
			got, composite := costAwareTargetsAt(alone, alone, c.scaleUp, c.scaleDown, current)
			assert.Contains(t, composite.Result.HeldRoleDemand, domain.RolePrefill, "setup: saturation holds prefill")
			assert.Zero(t, composite.RoleCapacities[domain.RolePrefill].SpareCapacity, "a held role has no spare")
			assert.Equal(t, c.n, got["p"], "saturation alone: the held prefill keeps its replicas")
			assert.Equal(t, 2, got["d"], "setup: decode releases nothing")

			other := variantsEntry("throughput", 0.05*float64(c.n)*c.prc+300,
				domain.VariantCapacity{VariantName: "p", Role: domain.RolePrefill, PerReplicaCapacity: c.prc},
				domain.VariantCapacity{VariantName: "d", Role: domain.RoleDecode, PerReplicaCapacity: 300})
			other.Result.RoleDemand = map[string]float64{domain.RolePrefill: 0.05 * float64(c.n) * c.prc, domain.RoleDecode: 300}
			both := []allocation.NamedAnalyzerResult{sat, other}
			got, composite = costAwareTargetsAt(both, both, c.scaleUp, c.scaleDown, current)
			assert.Contains(t, composite.Result.HeldRoleDemand, domain.RolePrefill, "setup: saturation's held figure is kept")
			assert.False(t, composite.Holds[domain.RolePrefill].NoScaleDown, "setup: no composite hold covers it")
			assert.Zero(t, composite.RoleCapacities[domain.RolePrefill].SpareCapacity, "a held role has no spare")
			assert.Equal(t, c.n, got["p"], "with another analyzer: the held prefill keeps its replicas")
		})
	}
}

// TestHeldRoleSpareIsZeroOnTheAnalyzersOwnEntry: the spare saturation's own
// entry reports for a held role is zero too, so what is published for it
// matches what the optimizer acts on.
func TestHeldRoleSpareIsZeroOnTheAnalyzersOwnEntry(t *testing.T) {
	for _, c := range heldCases {
		sat := heldPrefillEntry(c.n, c.prc, c.scaleDown, 0, true)
		nr := buildNamedResult(context.Background(), domain.SaturationAnalyzerName, sat.Result,
			config.ScalingPolicy{}, nil, c.scaleUp, c.scaleDown)
		assert.Zero(t, nr.RoleCapacities[domain.RolePrefill].SpareCapacity, c.name)
		assert.Positive(t, nr.RoleCapacities[domain.RoleDecode].SpareCapacity, "%s: decode is not held", c.name)
		assert.Positive(t, nr.SpareCapacity, "%s: the model-level spare stands while a role is not held", c.name)
	}
}

// TestHeldSpareIsZeroAtModelLevelWhenEveryRoleIsHeld: with every role held,
// or the one role of an aggregated model, the model-level spare is zero too.
func TestHeldSpareIsZeroAtModelLevelWhenEveryRoleIsHeld(t *testing.T) {
	pd := heldPrefillEntry(21, 24576, 0.70, 0, true)
	pd.Result.HeldRoleDemand[domain.RoleDecode] = 300
	pd.Result.RoleDemand[domain.RoleDecode] = 0.70 * 600
	pd.Result.TotalDemand = pd.Result.RoleDemand[domain.RolePrefill] + 0.70*600
	nr := buildNamedResult(context.Background(), domain.SaturationAnalyzerName, pd.Result,
		config.ScalingPolicy{}, nil, 0.85, 0.70)
	assert.Zero(t, nr.SpareCapacity, "P/D, both roles held")

	supply := 21 * 24576.0
	agg := variantsEntry(domain.SaturationAnalyzerName, 0.70*supply,
		domain.VariantCapacity{VariantName: "v", ReplicaCount: 21, PerReplicaCapacity: 24576, TotalDemand: 0.70 * supply})
	agg.Result.HeldRoleDemand = map[string]float64{domain.RoleBoth: 0.1 * supply}
	nr = buildNamedResult(context.Background(), domain.SaturationAnalyzerName, agg.Result,
		config.ScalingPolicy{}, nil, 0.85, 0.70)
	assert.Nil(t, nr.RoleCapacities, "setup: aggregated")
	assert.Zero(t, nr.SpareCapacity, "aggregated, held")
}

// TestCappedHeldRoleOrdersNothingOnARequiredResidue: the same residue on the
// way up. While decode is saturated, saturation clamps prefill's demand down
// to scaleUp x anticipated supply, where RequiredCapacity is zero. 0.80 x
// (21 x 24576) / 0.80 comes back 5.8e-11 above 21 x 24576, and a scale-up
// rounds up, so the held role ordered a whole replica.
func TestCappedHeldRoleOrdersNothingOnARequiredResidue(t *testing.T) {
	const n, prc, scaleUp, scaleDown = 21, 24576.0, 0.80, 0.65
	sat := heldPrefillEntry(n, prc, scaleUp, 0, true)
	sat.Result.HeldRoleDemand[domain.RolePrefill] = 2 * n * prc
	alone := []allocation.NamedAnalyzerResult{sat}
	got, composite := costAwareTargetsAt(alone, alone, scaleUp, scaleDown, map[string]int{"p": n, "d": 2})
	assert.Zero(t, composite.RoleCapacities[domain.RolePrefill].RequiredCapacity, "a capped held role has no required capacity")
	assert.Equal(t, n, got["p"], "the held prefill orders nothing")
	assert.Equal(t, 2, got["d"], "setup: decode orders nothing")
}

// TestHeldRoleStillOrdersARealShortfall: a hold is a floor on the way up,
// so a held role whose demand is genuinely above the scale-up threshold
// still grows. Prefill 21 x 100 is held at 1% above scaleUp x supply.
func TestHeldRoleStillOrdersARealShortfall(t *testing.T) {
	sat := heldPrefillEntry(21, 100, 0.85*1.01, 0, true)
	alone := []allocation.NamedAnalyzerResult{sat}
	got, composite := costAwareTargetsAt(alone, alone, 0.85, 0.70, map[string]int{"p": 21, "d": 2})
	assert.InDelta(t, 21, composite.RoleCapacities[domain.RolePrefill].RequiredCapacity, 1e-9)
	assert.Equal(t, 22, got["p"])
}

// TestUnheldRoleStillReleasesOnASmallSpare: a role nothing holds, with a real
// spare well below one replica, still releases what its releasable capacity
// allows. Prefill runs 21 x 100 at demand 1469: a spare of 1.4, and 108 of
// releasable capacity at the release target, so one replica goes.
func TestUnheldRoleStillReleasesOnASmallSpare(t *testing.T) {
	sat := heldPrefillEntry(21, 100, 0.70, 0.70*2100-1, false)
	alone := []allocation.NamedAnalyzerResult{sat}
	got, composite := costAwareTargetsAt(alone, alone, 0.85, 0.70, map[string]int{"p": 21, "d": 2})
	assert.InDelta(t, 2100-1469/0.7, composite.RoleCapacities[domain.RolePrefill].SpareCapacity, 1e-9)
	assert.Equal(t, 20, got["p"])
}
