package steadystate

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// pdEntry is an eligible analyzer entry for a P/D model with prefill variant
// "p" and decode variant "d", two replicas each, priced at prefillPRC and
// 300, with per-role demand roleDemand. A role missing from roleDemand
// has no demand entry, so the analyzer defines no need for it.
func pdEntry(name string, roleDemand map[string]float64, prefillPRC float64) allocation.NamedAnalyzerResult {
	total := 0.0
	for _, d := range roleDemand {
		total += d
	}
	return allocation.NamedAnalyzerResult{
		Name: name,
		Live: true,
		Result: &domain.AnalyzerResult{
			AnalyzerName: name,
			ModelID:      "m",
			Namespace:    "ns",
			TotalDemand:  total,
			RoleDemand:   roleDemand,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 2, PerReplicaCapacity: prefillPRC,
					TotalDemand: roleDemand[domain.RolePrefill], Reason: "measured"},
				{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 2, PerReplicaCapacity: 300,
					TotalDemand: roleDemand[domain.RoleDecode], Reason: "measured"},
			},
		},
	}
}

// twoVariantEntry is an eligible analyzer entry for a non-disaggregated model
// with variants "v1" and "v2" (two replicas each), at PRCs prc1 and prc2, and
// demand split between them as d1 and d2.
func twoVariantEntry(name string, d1, d2, prc1, prc2 float64) allocation.NamedAnalyzerResult {
	return allocation.NamedAnalyzerResult{
		Name: name,
		Live: true,
		Result: &domain.AnalyzerResult{
			AnalyzerName: name,
			ModelID:      "m",
			Namespace:    "ns",
			TotalDemand:  d1 + d2,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "v1", ReplicaCount: 2, PerReplicaCapacity: prc1, TotalDemand: d1, Reason: "measured"},
				{VariantName: "v2", ReplicaCount: 2, PerReplicaCapacity: prc2, TotalDemand: d2, Reason: "measured"},
			},
		},
	}
}

// TestCompositeSizesEachVariantToItsDecidingNeed: two accelerators serving
// one role are each priced so the role's demand needs exactly what the
// deciding analyzer asked of that variant. Converting both needs at
// saturation's PRC instead took the larger, 160, which at A's 10 is 16
// replicas where the analyzer needs 10.
func TestCompositeSizesEachVariantToItsDecidingNeed(t *testing.T) {
	sat := twoVariantEntry(domain.SaturationAnalyzerName, 30, 10, 10, 8) // needs 4 of A, 5 of B
	other := twoVariantEntry("throughput", 50, 50, 10, 5)                // needs 10 of A, 20 of B
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

	vcs := composite.Result.VariantCapacities
	require.Len(t, vcs, 2)
	d := composite.Result.TotalDemand
	assert.InDelta(t, 160, d, 1e-9, "20 of B at saturation's 8")
	assert.InDelta(t, 10, d/vcs[0].PerReplicaCapacity, 1e-9)
	assert.InDelta(t, 20, d/vcs[1].PerReplicaCapacity, 1e-9)
	assert.InDelta(t, 120, vcs[0].TotalDemand, 1e-9, "saturation's 3:1 attribution is kept")
	assert.InDelta(t, vcs[0].TotalDemand/(2*vcs[0].PerReplicaCapacity), vcs[0].Utilization, 1e-9)
}

// TestCompositeKeepsTheDecidersRatio: where the deciding analyzer rates two
// accelerators alike, so does the composite, though saturation rates one
// twice the other.
func TestCompositeKeepsTheDecidersRatio(t *testing.T) {
	sat := twoVariantEntry(domain.SaturationAnalyzerName, 500, 300, 200, 100)
	other := twoVariantEntry("throughput", 1500, 1500, 100, 100) // needs 30 of either
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

	vcs := composite.Result.VariantCapacities
	d := composite.Result.TotalDemand
	assert.InDelta(t, 30, d/vcs[0].PerReplicaCapacity, 1e-9)
	assert.InDelta(t, 30, d/vcs[1].PerReplicaCapacity, 1e-9)
}

// TestCompositePricesAVariantSaturationCannot: in a role another analyzer
// sizes, a variant saturation has no PRC for is priced from that analyzer's
// need, and is not reported as dropped.
func TestCompositePricesAVariantSaturationCannot(t *testing.T) {
	const msg = "a listed analyzer's need is dropped: saturation has no per-replica capacity for the variant"
	sat := twoVariantEntry(domain.SaturationAnalyzerName, 300, 0, 200, 0)
	other := twoVariantEntry("throughput", 30, 30, 10, 10) // needs 6 of either
	named := []allocation.NamedAnalyzerResult{sat, other}
	ctx, logs := zapObserverCtx(t)

	composite := buildComposite(ctx, named, named, 0.85, 0.7)

	d := composite.Result.TotalDemand
	assert.InDelta(t, 1200, d, 1e-9)
	assert.InDelta(t, 6, d/composite.Result.VariantCapacities[1].PerReplicaCapacity, 1e-9)
	assert.Empty(t, logs.FilterMessage(msg).All())
}

// TestCompositeSizesPrefillSaturationSeesAsIdle: on P/D, another analyzer's
// prefill need grows prefill though saturation sees no prefill demand, and
// decode, which it does not size, keeps saturation's figure.
func TestCompositeSizesPrefillSaturationSeesAsIdle(t *testing.T) {
	sat := pdEntry(domain.SaturationAnalyzerName,
		map[string]float64{domain.RolePrefill: 0, domain.RoleDecode: 900}, 100)
	other := pdEntry("slo", map[string]float64{domain.RolePrefill: 300}, 50) // prefill needs 6
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)
	satOnly := buildComposite(context.Background(), []allocation.NamedAnalyzerResult{sat},
		[]allocation.NamedAnalyzerResult{sat}, 0.85, 0.7)

	got := composite.Result
	assert.InDelta(t, 600, got.RoleDemand[domain.RolePrefill], 1e-9, "6 replicas x saturation's 100")
	assert.Equal(t, 900.0, got.RoleDemand[domain.RoleDecode])
	assert.InDelta(t, 1500, got.TotalDemand, 1e-9)
	assert.Equal(t, 100.0, got.VariantCapacities[0].PerReplicaCapacity)
	assert.InDelta(t, 600, got.VariantCapacities[0].TotalDemand, 1e-9)
	assert.Positive(t, composite.RoleCapacities[domain.RolePrefill].RequiredCapacity, "6 wanted, 2 running")
	assert.Zero(t, satOnly.RoleCapacities[domain.RolePrefill].RequiredCapacity)
	assert.Equal(t, satOnly.RoleCapacities[domain.RoleDecode], composite.RoleCapacities[domain.RoleDecode])
}

// TestCompositeSizesOnTheListedAnalyzersAlone: with saturation unlisted its
// demand is only the unit, so a smaller need from the listed analyzer lowers
// the demand, and a scale-down can follow it.
func TestCompositeSizesOnTheListedAnalyzersAlone(t *testing.T) {
	sat := liveEntry(domain.SaturationAnalyzerName, 800, 200) // needs 4
	other := liveEntry("throughput", 20, 10)                  // needs 2
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, []allocation.NamedAnalyzerResult{other}, 0.85, 0.7)

	assert.InDelta(t, 400, composite.Result.TotalDemand, 1e-9)
	assert.Equal(t, 200.0, composite.Result.VariantCapacities[0].PerReplicaCapacity)
	assert.Empty(t, composite.Holds)
	assert.Positive(t, composite.SpareCapacity, "3 running, 2 wanted")
}

// TestCompositeHeldRoleDemand: saturation's demand before its own hold is
// raised to another analyzer's larger demand, which the hold did not set,
// and dropped where saturation did not size the role.
func TestCompositeHeldRoleDemand(t *testing.T) {
	newSat := func() allocation.NamedAnalyzerResult {
		sat := pdEntry(domain.SaturationAnalyzerName,
			map[string]float64{domain.RolePrefill: 500, domain.RoleDecode: 900}, 100)
		sat.Result.HeldRoleDemand = map[string]float64{domain.RolePrefill: 300}
		return sat
	}
	other := pdEntry("slo", map[string]float64{domain.RolePrefill: 400}, 100) // prefill needs 4

	t.Run("both listed", func(t *testing.T) {
		sat := newSat()
		named := []allocation.NamedAnalyzerResult{sat, other}
		got := buildComposite(context.Background(), named, named, 0.85, 0.7).Result
		assert.Equal(t, 500.0, got.RoleDemand[domain.RolePrefill], "saturation's held figure is the larger")
		assert.InDelta(t, 400, got.HeldRoleDemand[domain.RolePrefill], 1e-9)
		assert.Equal(t, 300.0, sat.Result.HeldRoleDemand[domain.RolePrefill], "saturation's own map is not written")
	})
	t.Run("saturation unlisted", func(t *testing.T) {
		sat := newSat()
		named := []allocation.NamedAnalyzerResult{sat, other}
		got := buildComposite(context.Background(), named, []allocation.NamedAnalyzerResult{other}, 0.85, 0.7).Result
		assert.InDelta(t, 400, got.RoleDemand[domain.RolePrefill], 1e-9)
		assert.NotContains(t, got.HeldRoleDemand, domain.RolePrefill)
	})
	t.Run("the other asks for less than the hold kept", func(t *testing.T) {
		sat := newSat()
		low := pdEntry("slo", map[string]float64{domain.RolePrefill: 200}, 100)
		named := []allocation.NamedAnalyzerResult{sat, low}
		got := buildComposite(context.Background(), named, named, 0.85, 0.7).Result
		assert.Equal(t, 500.0, got.RoleDemand[domain.RolePrefill])
		assert.Equal(t, 300.0, got.HeldRoleDemand[domain.RolePrefill])
	})
}

// TestCompositeSplitsNewDemandEvenly: demand saturation attributed to no
// variant is shared evenly among the role's variants.
func TestCompositeSplitsNewDemandEvenly(t *testing.T) {
	sat := twoVariantEntry(domain.SaturationAnalyzerName, 0, 0, 200, 100)
	other := liveEntry("throughput", 60, 10) // needs 6 of v1 only
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

	vcs := composite.Result.VariantCapacities
	assert.InDelta(t, 1200, composite.Result.TotalDemand, 1e-9)
	assert.InDelta(t, 600, vcs[0].TotalDemand, 1e-9)
	assert.InDelta(t, 600, vcs[1].TotalDemand, 1e-9)
	assert.Equal(t, 100.0, vcs[1].PerReplicaCapacity, "no listed analyzer sized v2")
	assert.InDelta(t, 600.0/(2*100), vcs[1].Utilization, 1e-9, "its utilization follows its new demand")
}

// TestCompositeIgnoresANeedItCannotPrice: a need whose demand at
// saturation's PRC overflows, or that is read for a variant saturation has no
// PRC for, sizes nothing. With saturation listed its own demand stands; with
// it unlisted the role holds.
func TestCompositeIgnoresANeedItCannotPrice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		satPRC float64
		other  allocation.NamedAnalyzerResult
	}{
		{"overflows", 200, liveEntry("throughput", math.MaxFloat64/2, 1)},
		{"no saturation PRC", 0, liveEntry("throughput", 60, 10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sat := liveEntry(domain.SaturationAnalyzerName, 800, tc.satPRC)
			named := []allocation.NamedAnalyzerResult{sat, tc.other}

			listed := buildComposite(context.Background(), named, named, 0.85, 0.7)
			assert.Equal(t, 800.0, listed.Result.TotalDemand)
			assert.Equal(t, tc.satPRC, listed.Result.VariantCapacities[0].PerReplicaCapacity, "saturation's PRC is kept")
			assert.False(t, math.IsInf(listed.Result.VariantCapacities[0].Utilization, 0))

			unlisted := buildComposite(context.Background(), named, []allocation.NamedAnalyzerResult{tc.other}, 0.85, 0.7)
			assert.Equal(t, 800.0, unlisted.Result.TotalDemand)
			assert.Equal(t, allocation.RoleHold{NoScaleUp: true, NoScaleDown: true}, unlisted.Holds[domain.RoleBoth])
		})
	}
}
