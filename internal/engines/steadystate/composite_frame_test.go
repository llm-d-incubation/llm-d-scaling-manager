package steadystate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// reasonMeasured is a capacity reason that is neither no-data nor an error.
const reasonMeasured = "measured"

// variantsEntry is an eligible analyzer entry for a non-disaggregated model
// with demand and the given variants, each measured.
func variantsEntry(name string, demand float64, vcs ...domain.VariantCapacity) allocation.NamedAnalyzerResult {
	for i := range vcs {
		vcs[i].Reason = reasonMeasured
	}
	return allocation.NamedAnalyzerResult{
		Name: name,
		Live: true,
		Result: &domain.AnalyzerResult{
			AnalyzerName: name, ModelID: "m", Namespace: "ns", TotalDemand: demand, VariantCapacities: vcs,
		},
	}
}

// TestCompositePricesASplitRoleInOneFrame: with saturation deciding one
// accelerator and another analyzer the other, the role is priced in the frame
// that needs more for the fleet it runs, not a mix of the two. Mixed, the
// demand was the others' 6000 with the A100 at saturation's need of 50, 120
// each, and the role was 14 A100 short where saturation wanted 10 more and
// the other analyzer none.
func TestCompositePricesASplitRoleInOneFrame(t *testing.T) {
	sat := variantsEntry(domain.SaturationAnalyzerName, 5000,
		domain.VariantCapacity{VariantName: "h100", ReplicaCount: 10, PerReplicaCapacity: 200, TotalDemand: 2000}, // needs 25
		domain.VariantCapacity{VariantName: "a100", ReplicaCount: 20, PerReplicaCapacity: 100, TotalDemand: 3000}) // needs 50
	other := variantsEntry("throughput", 3000,
		domain.VariantCapacity{VariantName: "h100", PerReplicaCapacity: 100},
		domain.VariantCapacity{VariantName: "a100", PerReplicaCapacity: 100}) // needs 30 of either
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

	vcs := composite.Result.VariantCapacities
	assert.Equal(t, 5000.0, composite.Result.TotalDemand, "saturation's frame needs more: 10/25 + 20/50 < 10/30 + 20/30")
	assert.Equal(t, 200.0, vcs[0].PerReplicaCapacity)
	assert.Equal(t, 100.0, vcs[1].PerReplicaCapacity)
	assert.InDelta(t, 5000/0.85-4000, composite.RequiredCapacity, 1e-9, "saturation's own shortfall")

	// The other analyzer needing 60 of either is the larger need: its frame.
	other = variantsEntry("throughput", 6000,
		domain.VariantCapacity{VariantName: "h100", PerReplicaCapacity: 100},
		domain.VariantCapacity{VariantName: "a100", PerReplicaCapacity: 100})
	named = []allocation.NamedAnalyzerResult{sat, other}
	composite = buildComposite(context.Background(), named, named, 0.85, 0.7)
	vcs = composite.Result.VariantCapacities
	d := composite.Result.TotalDemand
	assert.InDelta(t, 60*200, d, 1e-9)
	assert.InDelta(t, 60, d/vcs[0].PerReplicaCapacity, 1e-9)
	assert.InDelta(t, 60, d/vcs[1].PerReplicaCapacity, 1e-9)
}

// TestCompositeReadsAVariantTheOthersDoNotSizeAtSaturationsRatio: an
// analyzer that sizes one of a role's variants is compared on the whole
// fleet, its need for the others read at saturation's ratio. Compared on its
// one variant alone, a smaller need than saturation's looked larger and
// shrank the role.
func TestCompositeReadsAVariantTheOthersDoNotSizeAtSaturationsRatio(t *testing.T) {
	sat := variantsEntry(domain.SaturationAnalyzerName, 400,
		domain.VariantCapacity{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 100, TotalDemand: 40},
		domain.VariantCapacity{VariantName: "v2", ReplicaCount: 10, PerReplicaCapacity: 100, TotalDemand: 360}) // needs 4 of either
	other := variantsEntry("throughput", 350, domain.VariantCapacity{VariantName: "v1", PerReplicaCapacity: 100}) // needs 3.5
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

	assert.Equal(t, 400.0, composite.Result.TotalDemand)
	assert.Equal(t, 100.0, composite.Result.VariantCapacities[0].PerReplicaCapacity)
}

// TestCompositeDropsANeedSaturationCannotCarry: an others' need on a variant
// saturation has no PRC for, in a role priced in saturation's frame, cannot
// be carried: the variant keeps no capacity, saturation's hold is not lowered
// by it, and the drop is logged. With saturation's demand zero the role used
// to be sized at zero with the need lost silently.
func TestCompositeDropsANeedSaturationCannotCarry(t *testing.T) {
	const msg = "a listed analyzer's need is dropped: saturation has no per-replica capacity for the variant"
	for _, satDemand := range []float64{100, 0} {
		sat := variantsEntry(domain.SaturationAnalyzerName, satDemand,
			domain.VariantCapacity{VariantName: "a", ReplicaCount: 2, PerReplicaCapacity: 10, TotalDemand: satDemand},
			domain.VariantCapacity{VariantName: "b", PerReplicaCapacity: 0})
		sat.Result.HeldRoleDemand = map[string]float64{domain.RoleBoth: 40}
		other := variantsEntry("throughput", 50, domain.VariantCapacity{VariantName: "b", PerReplicaCapacity: 10}) // needs 5 of b
		named := []allocation.NamedAnalyzerResult{sat, other}
		ctx, logs := zapObserverCtx(t)

		got := buildComposite(ctx, named, named, 0.85, 0.7).Result

		require.Len(t, logs.FilterMessage(msg).All(), 1, "saturation demand %v", satDemand)
		assert.Equal(t, satDemand, got.TotalDemand)
		assert.Zero(t, got.VariantCapacities[1].PerReplicaCapacity)
		assert.Equal(t, 40.0, got.HeldRoleDemand[domain.RoleBoth])
	}
}
