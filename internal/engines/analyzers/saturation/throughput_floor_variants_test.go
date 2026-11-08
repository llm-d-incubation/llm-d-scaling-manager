package saturation

import (
	"context"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/floor"
)

// Issue #495: the throughput floor raised a role and not its variants, so a
// variant kept its occupancy figure and the warm pool's pressure
// (steadystate.variantPressure, SpareFraction = 1 - Utilization) read it as
// sparer than the role it serves. With SwitchThroughputFloorRaisesVariants on,
// each variant of a raised role reads the role's spare, 1 - role demand / role
// supply; off, it reads its own occupancy, as before.

// floorRaisesVariantsOff turns SwitchThroughputFloorRaisesVariants off.
func floorRaisesVariantsOff(a *SaturationAnalyzer) { a.floorRaisesVariants = false }

// spareOf is what the warm pool reads for vc: steadystate.variantPressure's
// SpareFraction.
func spareOf(vc domain.VariantCapacity) float64 { return 1 - vc.Utilization }

// variantNamed is the variant of vcs named name; the test fails without one.
func variantNamed(t *testing.T, vcs []domain.VariantCapacity, name string) domain.VariantCapacity {
	t.Helper()
	for _, vc := range vcs {
		if vc.VariantName == name {
			return vc
		}
	}
	require.FailNow(t, "no variant "+name)
	return domain.VariantCapacity{}
}

// floorRaisedDecodeCycle is the P/D fleet of "the throughput floor, through
// Analyze" (throughput_floor_test.go): decode seen saturated at runMu over
// MinThroughputSamplesToOrder cycles, then six decode replicas at 3 000
// tokens each and one idle prefill at runLambda, where occupancy reads a
// fraction of one replica and the floor raises decode to lambda / mu. It
// returns the last cycle's result, with edit applied to the analyzer first.
func floorRaisedDecodeCycle(t *testing.T, edit func(*SaturationAnalyzer)) *domain.AnalyzerResult {
	t.Helper()
	a := NewSaturationAnalyzer(capacity.NewStore())
	if edit != nil {
		edit(a)
	}
	clock := time.Date(2026, 9, 17, 10, 48, 9, 0, time.UTC)
	a.now = func() time.Time { return clock }
	ctx := context.Background()

	decode := func(pod string, tokensInUse int64, queue int, rate float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, decodeVariant, tokensInUse, runKvCapacity, queue, 6000, 1000)
		rm.RequestRate = rate
		rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
		rm.Ready = true
		return rm
	}
	prefill := func(pod string, tokensInUse int64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, "prefill-v", tokensInUse, 1_149_312, 0, 6000, 1)
		rm.RequestRate = runLambda
		rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
		rm.Ready = true
		return rm
	}
	states := func(decodeN, prefillN int) []domain.VariantReplicaState {
		return []domain.VariantReplicaState{
			{VariantName: decodeVariant, Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: decodeN, GPUsPerReplica: 1},
			{VariantName: "prefill-v", Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: prefillN, GPUsPerReplica: 1},
		}
	}
	for i := floor.MinThroughputSamplesToOrder - 1; i >= 0; i-- {
		in := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 1_158_912, 10, runMu+0.01*float64(i)), prefill("prefill-0", 66_183)},
			states(1, 1))
		in.ArrivalRate = runLambda
		_, err := a.Analyze(ctx, in)
		require.NoError(t, err)
		clock = clock.Add(ThroughputSampleSpacing + time.Second)
	}

	rm := make([]domain.ReplicaMetrics, 0, 7)
	for i := range 6 {
		rm = append(rm, decode(string(rune('a'+i)), 3_000, 0, 1.0))
	}
	rm = append(rm, prefill("p", 0))
	in := makeAnalyzerInput(rm, states(6, 1))
	in.ArrivalRate = runLambda
	result, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	return result
}

// One cycle where the floor raises decode: the decode variant reads the
// role's spare, which is what the warm pool's pressure reads; prefill, which
// the floor does not raise, is the same either way.
func TestFloorRaisesVariants_TheDecodeVariantReadsTheRolesSpare(t *testing.T) {
	on := floorRaisedDecodeCycle(t, nil)
	off := floorRaisedDecodeCycle(t, floorRaisesVariantsOff)

	d := variantNamed(t, on.VariantCapacities, decodeVariant)
	supply := float64(d.ReplicaCount) * d.PerReplicaCapacity
	require.Positive(t, supply)
	roleDemand := on.RoleDemand[domain.RoleDecode]
	// The premise: the floor raised decode to lambda / mu replicas' worth.
	require.InDelta(t, runLambda/runMu*d.PerReplicaCapacity, roleDemand, 1)

	assert.InDelta(t, roleDemand, d.TotalDemand, 1e-6, "the variant carries the role's floored demand")
	assert.InDelta(t, 1-roleDemand/supply, spareOf(d), 1e-12,
		"the warm pool reads the variant as spare as its role, 1 - role demand / role supply")

	// The role-level figures are the switch's to leave alone.
	assert.Equal(t, off.RoleDemand, on.RoleDemand, "the role demand does not move with the switch")
	assert.Equal(t, off.TotalDemand, on.TotalDemand, "nor does the model total")

	// A role the floor does not raise is unchanged.
	assert.Equal(t, variantNamed(t, off.VariantCapacities, "prefill-v"),
		variantNamed(t, on.VariantCapacities, "prefill-v"), "prefill is not raised, and reads the same")

	// Off, the variant keeps its occupancy figure, and reads sparer than its
	// role.
	dOff := variantNamed(t, off.VariantCapacities, decodeVariant)
	// Measured: 0.9968 off against 0.8148 on -- the 3 000 tokens six
	// replicas hold, against the 1.11 replicas lambda / mu needs.
	assert.Greater(t, spareOf(dOff), spareOf(d)+0.15, "off, the warm pool reads the variant far sparer")
	assert.InDelta(t, 6*3_000/supply, dOff.Utilization, 0.01, "off, the variant reads its occupancy")
	t.Logf("decode role demand %.0f on supply %.0f: spare %.4f on, %.4f off", roleDemand, supply, spareOf(d), spareOf(dOff))
}

// fvK1 is the per-replica capacity of the direct-call fixtures below.
const fvK1 = 930_508.0

// twoDecodeVariants is a P/D fleet with two decode variants of different
// supply -- d4 four replicas of fvK1, d2 two of half that, so 4 and 1 of the
// role's 5 replicas' worth: a split by replica count (4/6, 2/6) is not a
// split by supply (4/5, 1/5) -- idling at 0.1 x fvK1 each, and one prefill
// variant of two replicas holding three replicas' worth, above its floor, so
// the floor does not raise it. Each pod is a row (caps) carrying a saturated
// throughput of runMu on enough samples to order, and its share of its
// variant's demand.
func twoDecodeVariants() ([]domain.VariantCapacity, []capacity.ReplicaCapacity, map[string]float64) {
	variants := []domain.VariantCapacity{
		{VariantName: "p", Role: domain.RolePrefill, PerReplicaCapacity: fvK1, ReplicaCount: 2,
			TotalDemand: 3 * fvK1, Utilization: 1.5},
		{VariantName: "d4", Role: domain.RoleDecode, PerReplicaCapacity: fvK1, ReplicaCount: 4,
			TotalDemand: 0.1 * fvK1, Utilization: 0.1 / 4},
		{VariantName: "d2", Role: domain.RoleDecode, PerReplicaCapacity: fvK1 / 2, ReplicaCount: 2,
			TotalDemand: 0.1 * fvK1, Utilization: 0.1},
	}
	caps := make([]capacity.ReplicaCapacity, 0, 8)
	for _, vc := range variants {
		for i := range vc.ReplicaCount {
			caps = append(caps, capacity.ReplicaCapacity{
				VariantName: vc.VariantName, PodName: fmt.Sprintf("%s-%d", vc.VariantName, i),
				SaturatedThroughput: runMu, SaturatedThroughputSamples: 2,
				ReplicaDemand: int64(vc.TotalDemand / float64(vc.ReplicaCount)),
			})
		}
	}
	roleDemand := map[string]float64{
		domain.RolePrefill: 3 * fvK1,
		domain.RoleDecode:  0.2 * fvK1,
	}
	return variants, caps, roleDemand
}

// fvFloor runs the floor at 12 req/s on an analyzer with
// SwitchThroughputFloorRaisesVariants on, edit applied first, and returns the
// model total it leaves.
func fvFloor(edit func(*SaturationAnalyzer), variants []domain.VariantCapacity, caps []capacity.ReplicaCapacity,
	roleDemand, eppByRole map[string]float64, eppQueued float64,
) float64 {
	a := &SaturationAnalyzer{floorRaisesVariants: true}
	if edit != nil {
		edit(a)
	}
	var total float64
	for _, v := range roleDemand {
		total += v
	}
	return a.applyThroughputFloor(domain.AnalyzerInput{ModelID: "m", Namespace: "n", ArrivalRate: 12},
		&config.ScalingPolicy{}, caps, variants, total, roleDemand, eppByRole, eppQueued, false, logr.Discard())
}

// floorTwoDecodeVariants runs fvFloor on twoDecodeVariants with no scheduler
// queue and returns the variants and role demand it leaves.
func floorTwoDecodeVariants(edit func(*SaturationAnalyzer)) ([]domain.VariantCapacity, map[string]float64) {
	variants, caps, roleDemand := twoDecodeVariants()
	fvFloor(edit, variants, caps, roleDemand, nil, 0)
	return variants, roleDemand
}

// P/D with two decode variants of different supply: the floor's raise is
// split by supply, not by replica count, so both read the role's spare, and
// prefill, which the floor does not raise, keeps its own figures.
func TestFloorRaisesVariants_TwoDecodeVariantsSplitBySupply(t *testing.T) {
	variants, roleDemand := floorTwoDecodeVariants(nil)
	before, _, beforeRoles := twoDecodeVariants()

	want := roleDemand[domain.RoleDecode]
	require.Greater(t, want, beforeRoles[domain.RoleDecode], "the premise: the floor raised decode")
	roleSupply := 5 * fvK1

	d4, d2 := variantNamed(t, variants, "d4"), variantNamed(t, variants, "d2")
	assert.InDelta(t, want*4/5, d4.TotalDemand, 1e-6, "four of the role's five replicas' worth take four fifths")
	assert.InDelta(t, want*1/5, d2.TotalDemand, 1e-6, "two half-size replicas, one fifth")
	assert.InDelta(t, want, d4.TotalDemand+d2.TotalDemand, 1e-6, "the shares are the role's demand")
	for _, vc := range []domain.VariantCapacity{d4, d2} {
		assert.InDelta(t, 1-want/roleSupply, spareOf(vc), 1e-12,
			"%s reads 1 - role demand / role supply", vc.VariantName)
		assert.InDelta(t, vc.TotalDemand/(float64(vc.ReplicaCount)*vc.PerReplicaCapacity), vc.Utilization, 1e-12,
			"%s: Utilization moves with TotalDemand", vc.VariantName)
	}

	// Prefill holds more than its floor: neither it nor its variant moves.
	assert.Equal(t, beforeRoles[domain.RolePrefill], roleDemand[domain.RolePrefill])
	assert.Equal(t, before[0], variantNamed(t, variants, "p"))
}

// The negative control: off, the floor raises the role by the same amount
// and the variants keep their occupancy figures, reading sparer than the
// role.
func TestFloorRaisesVariants_TheSwitchOffLeavesTheVariants(t *testing.T) {
	on, onRoles := floorTwoDecodeVariants(nil)
	off, offRoles := floorTwoDecodeVariants(floorRaisesVariantsOff)
	before, _, _ := twoDecodeVariants()

	assert.Equal(t, onRoles, offRoles, "the floor's role demand is the same either way")
	assert.Equal(t, before, off, "off, no variant moves")
	for _, name := range []string{"d4", "d2"} {
		vOn, vOff := variantNamed(t, on, name), variantNamed(t, off, name)
		assert.Greater(t, spareOf(vOff), spareOf(vOn), "%s: off, the warm pool reads it sparer", name)
		t.Logf("%s spare: %.4f on, %.4f off", name, spareOf(vOn), spareOf(vOff))
	}
}

// A variant above its share keeps its own figure, and its sibling takes what
// is left of the role, not its own supply share of the whole: the variants sum
// to the role, so the optimizer's split by variant demand
// (allocation.variantDemandShare) prices the hot one at its own figure.
func TestFloorRaisesVariants_AVariantAboveItsShareKeepsItAndTheRestFillsTheRole(t *testing.T) {
	variants, caps, roleDemand := twoDecodeVariants()
	// d2 holds 0.95 of its one replica's worth: above a fifth of any floor
	// under 4.75 replicas' worth.
	variants[2].TotalDemand = 0.95 * fvK1
	variants[2].Utilization = 0.95
	roleDemand[domain.RoleDecode] = variants[1].TotalDemand + variants[2].TotalDemand
	measured := roleDemand[domain.RoleDecode]
	hot := variants[2]
	fvFloor(nil, variants, caps, roleDemand, nil, 0)

	want := roleDemand[domain.RoleDecode]
	require.Greater(t, want, measured, "the premise: the floor raised decode")
	require.Greater(t, hot.TotalDemand, want/5, "the premise: d2 is above its supply share")
	assert.Equal(t, hot, variants[2], "the hot variant is above its share and keeps its figures")
	assert.InDelta(t, want-hot.TotalDemand, variants[1].TotalDemand, 1e-6, "its sibling takes the rest of the role")
	assert.InDelta(t, want, variants[1].TotalDemand+variants[2].TotalDemand, 1e-6, "the variants sum to the role")
	assert.InDelta(t, variants[1].TotalDemand/(4*fvK1), variants[1].Utilization, 1e-12)
}

// The water-fill itself: a variant pushed above the level by the first pass
// is taken out on the next, the variants sum to max(want, their own sum), a
// variant with no supply or a non-finite capacity keeps its figure, and a role
// not raised is not touched.
func TestFloorRaisesVariants_TheWaterFill(t *testing.T) {
	vc := func(name string, replicas int, perReplica, demand float64) domain.VariantCapacity {
		v := domain.VariantCapacity{VariantName: name, Role: domain.RoleDecode, ReplicaCount: replicas,
			PerReplicaCapacity: perReplica, TotalDemand: demand}
		if s := float64(replicas) * perReplica; s > 0 && !math.IsInf(s, 1) {
			v.Utilization = demand / s
		}
		return v
	}
	sum := func(vs []domain.VariantCapacity) float64 {
		var s float64
		for _, v := range vs {
			s += v.TotalDemand
		}
		return s
	}

	// Supplies 1, 1, 2 (4 in all) and a want of 4. The first level is 1 a
	// unit, so a (1.5) is above it; without a, 2.5 over a supply of 3 is
	// 0.83, so b (0.9) is above that too; c then takes the 1.6 left, a level
	// of 0.8.
	vs := []domain.VariantCapacity{vc("a", 1, 1, 1.5), vc("b", 1, 1, 0.9), vc("c", 2, 1, 0.1)}
	raiseVariantsToRoles(vs, map[string]float64{domain.RoleDecode: 4})
	assert.InDelta(t, 1.5, vs[0].TotalDemand, 1e-12)
	assert.InDelta(t, 0.9, vs[1].TotalDemand, 1e-12, "above the second level, so it keeps its own")
	assert.InDelta(t, 1.6, vs[2].TotalDemand, 1e-12)
	assert.InDelta(t, 0.8, vs[2].Utilization, 1e-12)
	assert.InDelta(t, 4, sum(vs), 1e-12, "the variants sum to the role")

	// A want under the variants' own sum moves none of them.
	vs = []domain.VariantCapacity{vc("a", 1, 1, 1.5), vc("b", 1, 1, 0.1)}
	unmoved := slices.Clone(vs)
	raiseVariantsToRoles(vs, map[string]float64{domain.RoleDecode: 1.2})
	assert.Equal(t, unmoved, vs)

	// No supply, or a capacity that is not a number of tokens, keeps its
	// figure and counts toward the role; the role not raised is not touched.
	vs = []domain.VariantCapacity{vc("zero", 0, 1, 0.5), vc("inf", 1, math.Inf(1), 0.5), vc("ok", 1, 1, 0),
		{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 1, PerReplicaCapacity: 1, TotalDemand: 0.1, Utilization: 0.1}}
	raiseVariantsToRoles(vs, map[string]float64{domain.RoleDecode: 2})
	assert.InDelta(t, 0.5, vs[0].TotalDemand, 0)
	assert.InDelta(t, 0.5, vs[1].TotalDemand, 0, "a +Inf capacity is no supply: no +Inf demand")
	assert.False(t, math.IsNaN(vs[1].Utilization), "and no NaN utilization")
	assert.InDelta(t, 1, vs[2].TotalDemand, 1e-12, "the one variant with supply takes the rest")
	assert.InDelta(t, 0.1, vs[3].TotalDemand, 0, "prefill was not raised")
}

// A role the swap LOWERS -- the queue priced as work to drain, below the
// residency it was measured at -- is not followed, even where the lowered
// figure is still above what its variants hold.
func TestFloorRaisesVariants_ARoleTheSwapLowersLeavesItsVariants(t *testing.T) {
	variants, caps, _ := twoDecodeVariants()
	variants = variants[1:] // decode alone: the queue is decode's to drain
	caps = slices.DeleteFunc(caps, func(rc capacity.ReplicaCapacity) bool { return rc.VariantName == "p" })
	const queueTokens = 3 * fvK1
	own := variants[0].TotalDemand + variants[1].TotalDemand
	roleDemand := map[string]float64{domain.RoleDecode: own + queueTokens}
	before := slices.Clone(variants)
	fvFloor(nil, variants, caps, roleDemand, map[string]float64{domain.RoleDecode: queueTokens}, 20)

	want := roleDemand[domain.RoleDecode]
	require.Less(t, want, own+queueTokens, "the premise: the swap lowered decode")
	require.Greater(t, want, own, "the premise: the lowered figure is above what the variants hold")
	assert.Equal(t, before, variants, "a lowered role's variants keep their figures")
}

// Prefill with no mu: its share of the scheduler queue is dropped from the
// role (it was never in a variant), and the prefill variant is not moved by
// the drop.
func TestFloorRaisesVariants_PrefillsDroppedQueueShareLeavesItsVariant(t *testing.T) {
	variants, caps, roleDemand := twoDecodeVariants()
	caps = slices.DeleteFunc(caps, func(rc capacity.ReplicaCapacity) bool { return rc.VariantName == "p" })
	caps = append(caps, capacity.ReplicaCapacity{VariantName: "p", PodName: "p-0"},
		capacity.ReplicaCapacity{VariantName: "p", PodName: "p-1"}) // no mu
	const share = 2 * fvK1
	variants[0].TotalDemand, variants[0].Utilization = 0.1*fvK1, 0.05
	roleDemand[domain.RolePrefill] = variants[0].TotalDemand + share
	before := variants[0]
	fvFloor(nil, variants, caps, roleDemand, map[string]float64{domain.RolePrefill: share, domain.RoleDecode: 0}, 20)

	require.InDelta(t, before.TotalDemand, roleDemand[domain.RolePrefill], 1e-6, "the premise: the share was dropped")
	assert.Equal(t, before, variantNamed(t, variants, "p"), "the prefill variant is not moved by the drop")
}

// The hot-rank hold runs after the floor and adds what a hot replica's
// variant is short of against the VARIANT's TotalDemand, into the role and the
// model total. On, a floored variant already carries its share of the floor,
// and the hold adds nothing that share covers; off, the variant still read its
// occupancy, and the hold added the hot replica's want on top of a role the
// floor had already raised to carry it -- the load counted twice.
func TestFloorRaisesVariants_TheHotRankHoldDoesNotCountTheFloorTwice(t *testing.T) {
	run := func(edit func(*SaturationAnalyzer)) (role, total float64, hot map[string]float64) {
		variants, caps, roleDemand := twoDecodeVariants()
		total = fvFloor(edit, variants, caps, roleDemand, nil, 0)
		states := make([]domain.VariantReplicaState, 0, len(variants))
		for _, vc := range variants {
			states = append(states, domain.VariantReplicaState{VariantName: vc.VariantName, Role: vc.Role,
				CurrentReplicas: vc.ReplicaCount})
		}
		total, hot = holdHotRanks(map[hotPod]bool{{"d4", "d4-0"}: true}, caps, variants, states,
			roleDemand, total, 0.85, 0.70)
		return roleDemand[domain.RoleDecode], total, hot
	}
	onRole, onTotal, onHot := run(nil)
	offRole, offTotal, offHot := run(floorRaisesVariantsOff)

	// The floor's decode figure, from the same fleet without the hold.
	_, roles := floorTwoDecodeVariants(nil)
	assert.InDelta(t, roles[domain.RoleDecode], onRole, 1e-6, "on, the hold adds nothing to a floored decode")
	assert.Empty(t, onHot)
	assert.Greater(t, offRole, onRole+0.5*fvK1, "off, the hold adds the hot replica's want on top of the floor")
	assert.NotEmpty(t, offHot)
	assert.Greater(t, offTotal, onTotal, "and the model total with it")
	t.Logf("decode role demand %.0f on, %.0f off; model total %.0f on, %.0f off", onRole, offRole, onTotal, offTotal)
}
