package steadystate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

const orphanRoleDemandMsg = "analyzer attributed demand to a role with no variants"

// buildRoleCapacities warns when an analyzer keys demand by a role that no
// variant serves. That pairing depends on the analyzer's role view matching
// discovery's — an invariant the types cannot express — and a divergence would
// otherwise silently mint a zero-supply bucket that the threshold post-step
// reads as an unservable shortfall and turns into a spurious scale-up. These
// tests pin both that the warning fires when it should and that it stays quiet
// on the normal paths, so it can't decay into per-cycle log noise.
func TestBuildRoleCapacities_LogsDemandForRoleWithNoVariants(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	r := &allocation.NamedAnalyzerResult{
		Result: &domain.AnalyzerResult{
			AnalyzerName: "saturation",
			ModelID:      "m1",
			Namespace:    "ns1",
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "d1", Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 4000},
			},
			RoleDemand: map[string]float64{"decode": 2000, "prefill": 1500},
		},
	}
	buildCapacities(ctx, r, nil, 1.0, 1.0)

	require.Equal(t, 1, logs.Len(), "expected exactly one warning for the orphaned role")
	entry := logs.All()[0]
	assert.Equal(t, orphanRoleDemandMsg, entry.Message)

	fields := entry.ContextMap()
	assert.Equal(t, "prefill", fields["role"])
	assert.Equal(t, "saturation", fields["analyzer"])
	// modelID/namespace make the line actionable: the engine reconciles many
	// models per cycle into a single controller log.
	assert.Equal(t, "m1", fields["modelID"])
	assert.Equal(t, "ns1", fields["namespace"])
	assert.EqualValues(t, 1500, fields["demand"])
	// ContextMap widens the []string field to []any.
	assert.Equal(t, []any{"decode"}, fields["variantRoles"])

	// The bucket is still built, so the shortfall stays visible downstream.
	assert.Zero(t, r.RoleCapacities["prefill"].TotalSupply)
	assert.EqualValues(t, 1500, r.RoleCapacities["prefill"].TotalDemand)
}

func TestBuildRoleCapacities_QuietWhenEveryRoleHasVariants(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	r := &allocation.NamedAnalyzerResult{
		Result: &domain.AnalyzerResult{
			AnalyzerName: "saturation",
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "p1", Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 5000},
				{VariantName: "d1", Role: "decode", ReplicaCount: 2, PerReplicaCapacity: 4000},
			},
			RoleDemand: map[string]float64{"prefill": 4000, "decode": 8000},
		},
	}
	buildCapacities(ctx, r, nil, 1.0, 1.0)

	assert.Zero(t, logs.FilterMessage(orphanRoleDemandMsg).Len())
}

func TestBuildRoleCapacities_QuietWhenNonDisaggregated(t *testing.T) {
	// No RoleDemand at all: the builder returns nil RoleCapacities without
	// inspecting roles, so there is nothing to warn about.
	ctx, logs := zapObserverCtx(t)

	r := &allocation.NamedAnalyzerResult{
		Result: &domain.AnalyzerResult{
			AnalyzerName: "saturation",
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", Role: domain.RoleBoth, ReplicaCount: 1, PerReplicaCapacity: 5000},
			},
		},
	}
	buildCapacities(ctx, r, nil, 1.0, 1.0)

	assert.Nil(t, r.RoleCapacities)
	assert.Zero(t, logs.FilterMessage(orphanRoleDemandMsg).Len())
}

func TestBuildRoleCapacities_QuietWhenOrphanRoleHasZeroDemand(t *testing.T) {
	// A role with no variants AND no demand is not a scale-up risk, so it must
	// not warn — otherwise a fleet that legitimately drains a role to zero would
	// log every reconcile cycle.
	ctx, logs := zapObserverCtx(t)

	r := &allocation.NamedAnalyzerResult{
		Result: &domain.AnalyzerResult{
			AnalyzerName: "saturation",
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "d1", Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 4000},
			},
			RoleDemand: map[string]float64{"decode": 2000, "prefill": 0},
		},
	}
	buildCapacities(ctx, r, nil, 1.0, 1.0)

	assert.Zero(t, logs.FilterMessage(orphanRoleDemandMsg).Len())
}

const unsizableShortfallMsg = "analyzer needs capacity but no variant has a per-replica capacity to scale with"

// A shortfall the optimizer cannot size reads from the outside exactly like a
// stuck autoscaler: the engine reports it needs capacity every cycle and never
// acts, because sizing a scale-up divides by per-replica capacity and every one
// of them is zero. These tests pin that the state is announced rather than left
// to be inferred from a prc:0 buried in the analyzer-result line.
func TestWarnUnsizableShortfall_LogsWhenNoVariantCanBeSized(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	r := &allocation.NamedAnalyzerResult{
		Result: &domain.AnalyzerResult{
			AnalyzerName: "saturation",
			ModelID:      "m1",
			Namespace:    "ns1",
			TotalDemand:  2,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "vB", ReplicaCount: 1, PerReplicaCapacity: 0},
				{VariantName: "vA", ReplicaCount: 1, PerReplicaCapacity: 0},
			},
		},
	}
	// scaleUp=0.85 with zero supply leaves RequiredCapacity > 0.
	buildCapacities(ctx, r, nil, 0.85, 0.70)

	require.Positive(t, r.RequiredCapacity, "setup: expected an unmet shortfall")
	entries := logs.FilterMessage(unsizableShortfallMsg)
	require.Equal(t, 1, entries.Len())

	fields := entries.All()[0].ContextMap()
	assert.Equal(t, "m1", fields["modelID"])
	assert.Equal(t, "ns1", fields["namespace"])
	assert.Equal(t, "saturation", fields["analyzer"])
	// Sorted, so the line is stable across cycles despite map/slice ordering.
	assert.Equal(t, []any{"vA", "vB"}, fields["variants"])
}

func TestWarnUnsizableShortfall_QuietWhenAnyVariantCanAbsorbIt(t *testing.T) {
	// One sizable variant is enough: the optimizer has something to divide by.
	ctx, logs := zapObserverCtx(t)

	r := &allocation.NamedAnalyzerResult{
		Result: &domain.AnalyzerResult{
			AnalyzerName: "saturation",
			TotalDemand:  10000,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "dead", ReplicaCount: 1, PerReplicaCapacity: 0},
				{VariantName: "live", ReplicaCount: 1, PerReplicaCapacity: 100},
			},
		},
	}
	buildCapacities(ctx, r, nil, 0.85, 0.70)

	require.Positive(t, r.RequiredCapacity, "setup: expected an unmet shortfall")
	assert.Zero(t, logs.FilterMessage(unsizableShortfallMsg).Len())
}

func TestWarnUnsizableShortfall_QuietWhenThereIsNoShortfall(t *testing.T) {
	// Zero capacity is only worth reporting when something is actually being
	// asked for; an idle model with no demand must not log every cycle.
	ctx, logs := zapObserverCtx(t)

	r := &allocation.NamedAnalyzerResult{
		Result: &domain.AnalyzerResult{
			AnalyzerName: "saturation",
			TotalDemand:  0,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 0},
			},
		},
	}
	buildCapacities(ctx, r, nil, 0.85, 0.70)

	assert.Zero(t, r.RequiredCapacity)
	assert.Zero(t, logs.FilterMessage(unsizableShortfallMsg).Len())
}

func TestWarnUnsizableShortfall_QuietWhenThereAreNoVariants(t *testing.T) {
	// No variants at all is a different condition (nothing discovered yet), and
	// naming zero variants in the log would say nothing useful.
	ctx, logs := zapObserverCtx(t)

	r := &allocation.NamedAnalyzerResult{
		Result: &domain.AnalyzerResult{AnalyzerName: "saturation", TotalDemand: 5},
	}
	buildCapacities(ctx, r, nil, 0.85, 0.70)

	assert.Zero(t, logs.FilterMessage(unsizableShortfallMsg).Len())
}
