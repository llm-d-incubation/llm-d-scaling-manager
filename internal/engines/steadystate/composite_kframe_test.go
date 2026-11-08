package steadystate

import (
	"context"
	"math"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
)

// TestCompositeReleasesOnlyWhatEveryFrameCanSpare: a scale-down sized in the
// chosen frame is capped at what every other frame can spare. Saturation rates
// B eight times A and throughput rates them alike; priced in throughput's
// frame, the spare was 194 and shedding both B replicas left saturation's
// demand of 80 on 40 of supply. Capped at saturation's own spare, 85.7, one B
// goes and saturation runs at 0.67.
func TestCompositeReleasesOnlyWhatEveryFrameCanSpare(t *testing.T) {
	sat := variantsEntry(domain.SaturationAnalyzerName, 80,
		domain.VariantCapacity{VariantName: "a", ReplicaCount: 4, PerReplicaCapacity: 10, TotalDemand: 40},
		domain.VariantCapacity{VariantName: "b", ReplicaCount: 2, PerReplicaCapacity: 80, TotalDemand: 40})
	other := variantsEntry("throughput", 25,
		domain.VariantCapacity{VariantName: "a", PerReplicaCapacity: 10},
		domain.VariantCapacity{VariantName: "b", PerReplicaCapacity: 10})
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

	vcs := composite.Result.VariantCapacities
	require.InDelta(t, 80, vcs[1].PerReplicaCapacity, 1e-9, "throughput's frame: 2.5 of either")
	assert.InDelta(t, 200-80/0.7, composite.SpareCapacity, 1e-9, "saturation's own spare, at a ratio of 1 on b")

	// Saturation alone: its spare stands.
	alone := buildComposite(context.Background(), named[:1], named[:1], 0.85, 0.7)
	assert.InDelta(t, 200-80/0.7, alone.SpareCapacity, 1e-9)
}

// TestCompositeBoundsSaturationsSpareByAnotherFrame: priced in saturation's
// frame, the spare is still capped by an analyzer that rates a variant higher:
// shedding b costs it four times what it costs saturation, so saturation's
// spare of 57.1 is capped at that analyzer's 214.3 / 4.
func TestCompositeBoundsSaturationsSpareByAnotherFrame(t *testing.T) {
	sat := variantsEntry(domain.SaturationAnalyzerName, 100,
		domain.VariantCapacity{VariantName: "a", ReplicaCount: 10, PerReplicaCapacity: 10, TotalDemand: 50},
		domain.VariantCapacity{VariantName: "b", ReplicaCount: 10, PerReplicaCapacity: 10, TotalDemand: 50})
	other := variantsEntry("throughput", 200,
		domain.VariantCapacity{VariantName: "a", PerReplicaCapacity: 10},
		domain.VariantCapacity{VariantName: "b", PerReplicaCapacity: 40})
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

	require.Equal(t, 100.0, composite.Result.TotalDemand, "saturation's frame: coverage 2 against 2.5")
	assert.InDelta(t, (500-200/0.7)/4, composite.SpareCapacity, 1e-9)
}

// TestCompositeBoundsOnTheRunningVariantsOnly: a variant with no replicas
// sheds nothing, so its PRC ratio does not tighten the bound. Counted, b's
// ratio of 4 cut the bound to 85.7 / 4 = 21.4, below saturation's own spare
// of 57.1.
func TestCompositeBoundsOnTheRunningVariantsOnly(t *testing.T) {
	sat := variantsEntry(domain.SaturationAnalyzerName, 100,
		domain.VariantCapacity{VariantName: "a", ReplicaCount: 20, PerReplicaCapacity: 10, TotalDemand: 100},
		domain.VariantCapacity{VariantName: "b", ReplicaCount: 0, PerReplicaCapacity: 10})
	other := variantsEntry("throughput", 80,
		domain.VariantCapacity{VariantName: "a", PerReplicaCapacity: 10},
		domain.VariantCapacity{VariantName: "b", PerReplicaCapacity: 40})
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

	require.Equal(t, 100.0, composite.Result.TotalDemand, "saturation's frame: coverage 20/10 against 20/8")
	assert.InDelta(t, 200-100/0.7, composite.SpareCapacity, 1e-9,
		"throughput's own spare is 200-80/0.7 at a ratio of 1 on the running a; saturation's is the smaller")
}

// TestCompositeBoundsARoleSpareOnPD: on P/D the bound applies to the role's
// spare, and the model's spare cannot exceed the roles'.
func TestCompositeBoundsARoleSpareOnPD(t *testing.T) {
	pd := func(name string, prefill float64, vcs ...domain.VariantCapacity) allocation.NamedAnalyzerResult {
		e := variantsEntry(name, prefill+300, vcs...)
		e.Result.RoleDemand = map[string]float64{domain.RolePrefill: prefill, domain.RoleDecode: 300}
		return e
	}
	sat := pd(domain.SaturationAnalyzerName, 80,
		domain.VariantCapacity{VariantName: "pa", Role: domain.RolePrefill, ReplicaCount: 4, PerReplicaCapacity: 10, TotalDemand: 40},
		domain.VariantCapacity{VariantName: "pb", Role: domain.RolePrefill, ReplicaCount: 2, PerReplicaCapacity: 80, TotalDemand: 40},
		domain.VariantCapacity{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 2, PerReplicaCapacity: 300, TotalDemand: 300})
	other := pd("throughput", 25,
		domain.VariantCapacity{VariantName: "pa", Role: domain.RolePrefill, PerReplicaCapacity: 10},
		domain.VariantCapacity{VariantName: "pb", Role: domain.RolePrefill, PerReplicaCapacity: 10})
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

	prefill := composite.RoleCapacities[domain.RolePrefill]
	assert.InDelta(t, 200-80/0.7, prefill.SpareCapacity, 1e-9)
	var sum float64
	for _, rc := range composite.RoleCapacities {
		sum += rc.SpareCapacity
	}
	assert.LessOrEqual(t, composite.SpareCapacity, sum+1e-9)
}

// TestCompositeGivesEachAnalyzerItsOwnFrame: each analyzer is priced in its
// own frame. Taking the largest need among the others per variant mixed them,
// and an external analyzer's zero need on b made the others' coverage
// infinite: throughput's need of 10 a was lost and the role shrank.
func TestCompositeGivesEachAnalyzerItsOwnFrame(t *testing.T) {
	sat := variantsEntry(domain.SaturationAnalyzerName, 20,
		domain.VariantCapacity{VariantName: "a", ReplicaCount: 2, PerReplicaCapacity: 10, TotalDemand: 10},
		domain.VariantCapacity{VariantName: "b", ReplicaCount: 2, PerReplicaCapacity: 10, TotalDemand: 10})
	throughput := variantsEntry("throughput", 100, domain.VariantCapacity{VariantName: "a", PerReplicaCapacity: 10})
	external := variantsEntry("external", 0,
		domain.VariantCapacity{VariantName: "a", PerReplicaCapacity: 10},
		domain.VariantCapacity{VariantName: "b", PerReplicaCapacity: 10})

	for _, named := range [][]allocation.NamedAnalyzerResult{
		{sat, throughput},
		{sat, throughput, external},
		{sat, external, throughput},
	} {
		composite := buildComposite(context.Background(), named, named, 0.85, 0.7)
		assert.InDelta(t, 100, composite.Result.TotalDemand, 1e-9, "throughput's frame, %d analyzers", len(named))
		assert.Positive(t, composite.RequiredCapacity)
		assert.False(t, math.IsNaN(composite.SpareCapacity), "a zero need prices no variant at D/0")
	}

	// Releasing: the external analyzer's zero need on b is a frame with no
	// capacity of its own (D = 0), which caps nothing and must not turn the
	// spare into NaN.
	idle := variantsEntry(domain.SaturationAnalyzerName, 20,
		domain.VariantCapacity{VariantName: "a", ReplicaCount: 4, PerReplicaCapacity: 10, TotalDemand: 10},
		domain.VariantCapacity{VariantName: "b", ReplicaCount: 4, PerReplicaCapacity: 10, TotalDemand: 10})
	named := []allocation.NamedAnalyzerResult{idle, external}
	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)
	assert.InDelta(t, 80-20/0.7, composite.SpareCapacity, 1e-9)
}

// TestCompositeLogsTheFrameEachRoleIsPricedIn: at VERBOSE, each role's frame
// and every frame's coverage are logged.
func TestCompositeLogsTheFrameEachRoleIsPricedIn(t *testing.T) {
	core, logs := observer.New(zapcore.Level(-logging.VERBOSE))
	ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)))
	named := []allocation.NamedAnalyzerResult{
		liveEntry(domain.SaturationAnalyzerName, 800, 200), // needs 4
		liveEntry("throughput", 60, 10),                    // needs 6
	}

	buildComposite(ctx, named, named, 0.85, 0.7)

	lines := logs.FilterMessage("composite-pricing").All()
	require.Len(t, lines, 1)
	assert.Equal(t, "throughput", lines[0].ContextMap()["frame"])
	assert.Equal(t, domain.RoleBoth, lines[0].ContextMap()["role"])
	assert.Len(t, lines[0].ContextMap()["frames"], 2)
}
