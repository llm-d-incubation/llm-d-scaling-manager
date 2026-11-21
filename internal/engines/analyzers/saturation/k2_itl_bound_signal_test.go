package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// What the bound made of k2 is published for exactly the replicas the
// k2-itl-bound line is logged for: none with the switch off, no TPOT
// objective, or a replica that is not decode. A bound that held k2 names its
// target; one that applies and held nothing is k2-under-bound, which is not
// inert; one that does not apply names why.
func TestDecodePackingOf(t *testing.T) {
	cases := []struct {
		name           string
		b              itlBatchBound
		held           bool
		bound, reason  string
		ok, inertAsDom bool
	}{
		{name: "off", b: itlBatchBound{}},
		{name: "no target", b: itlBatchBound{on: true, why: itlBoundNoTarget}},
		{name: "not decode", b: itlBatchBound{on: true, why: itlBoundNotDecode}},
		{name: "held", b: at(0.2), held: true, bound: k2BoundITL, reason: itlTargetSLO, ok: true},
		{name: "applies, held nothing", b: at(0.5), bound: k2BoundNone, reason: itlBoundNotBinding, ok: true},
		{name: "borrowed", b: itlBatchBound{on: true, why: itlBoundBorrowed}, bound: k2BoundNone, reason: itlBoundBorrowed,
			ok: true, inertAsDom: true},
		{name: "unreachable", b: itlBatchBound{on: true, target: 0.002, source: itlTargetSLO, why: itlBoundUnreach},
			bound: k2BoundNone, reason: itlBoundUnreach, ok: true, inertAsDom: true},
		{name: "no reason, out of range", b: itlBatchBound{on: true, fill: 1.5}, bound: k2BoundNone, reason: itlBoundBadBand,
			ok: true, inertAsDom: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bound, reason, ok := decodePackingOf(c.b, c.held)
			assert.Equal(t, c.ok, ok)
			assert.Equal(t, c.bound, bound)
			assert.Equal(t, c.reason, reason)
			if ok {
				p := &domain.DecodePacking{Bound: bound, Reason: reason}
				assert.Equal(t, c.inertAsDom, p.Inert(), "Inert")
			}
		})
	}
}

// The replica's capacity row carries it, with the k2 before the bound: held
// at a fill of 0.2 the row reads itl, 268,800 before and 200,000 after; at
// 0.5 the bound held nothing; on a default intercept it did not apply; off,
// the row carries nothing.
func TestK2BatchAtITLTarget_TheRowCarriesIt(t *testing.T) {
	rc, _, _ := priceBound(t, boundRow(), deploymentParams(), false, at(0.2))
	assert.Equal(t, k2BoundITL, rc.PackingBound)
	assert.Equal(t, itlTargetSLO, rc.PackingReason)
	assert.Equal(t, int64(boundAtSteady), rc.K2Unbounded)
	assert.Equal(t, int64(200_000), rc.ComputeBoundCapacity)
	p := decodePackingRow(rc)
	require.NotNil(t, p)
	assert.Equal(t, domain.DecodePacking{Bound: k2BoundITL, Reason: itlTargetSLO, K2Unbounded: boundAtSteady, K2: 200_000}, *p)

	rc, _, _ = priceBound(t, boundRow(), deploymentParams(), false, at(0.5))
	assert.Equal(t, k2BoundNone, rc.PackingBound)
	assert.Equal(t, itlBoundNotBinding, rc.PackingReason)
	assert.Equal(t, rc.ComputeBoundCapacity, rc.K2Unbounded)

	rc, _, _ = priceBound(t, boundRow(), deploymentParams(), false, itlBatchBound{on: true, why: itlBoundDefaultB})
	assert.Equal(t, k2BoundNone, rc.PackingBound)
	assert.Equal(t, itlBoundDefaultB, rc.PackingReason)

	rc, _, _ = priceBound(t, boundRow(), deploymentParams(), false, itlBatchBound{})
	assert.Empty(t, rc.PackingBound, "off")
	assert.Zero(t, rc.K2Unbounded)
	assert.Nil(t, decodePackingRow(rc))
}

// packingRun runs 40 cycles of itlBoundRun's fleet and returns what the
// last cycle made of v's packing (TakeDecodePacking), nil with none.
func packingRun(t *testing.T, on bool, tpot time.Duration, role string) *domain.DecodePacking {
	t.Helper()
	r := itlRig(t, map[string]bool{SwitchK2BatchAtITLTarget: on})
	r.a.capacityStore.Update("test-ns", "test-model", "v", capacity.Record{GpuCount: 1, EngineParams: deploymentParams(),
		LearnedFrom: "deployment"}, r.now)
	for range 40 {
		rows := itlSettleFleet()
		states := decodeVariants("v")
		states[0].Role = role
		r.tr.Observe(settleModel("test-ns", "test-model"), settlerow.Of(rows, map[string]string{"v": role}), r.now)
		in := makeAnalyzerInput(rows, states)
		if tpot > 0 {
			tpotPolicy(in.Config.(*config.ScalingPolicy), tpot)
		}
		_, err := r.a.Analyze(context.Background(), in)
		require.NoError(t, err)
		r.now = r.now.Add(restartCycle)
	}
	packing, ok := r.a.TakeDecodePacking("test-ns", "test-model")
	require.True(t, ok, "the analysis leaves its packing")
	_, again := r.a.TakeDecodePacking("test-ns", "test-model")
	require.False(t, again, "taken once")
	p, found := packing["v"]
	if !found {
		return nil
	}
	return &p
}

// Through Analyze, the model's packing carries it: with a 16 ms objective
// on v's own line, bound itl, its k2 under the steady batch's figure. With
// no objective, the switch off, or an aggregated variant, nothing.
func TestK2BatchAtITLTarget_TheVariantResultCarriesIt(t *testing.T) {
	on := packingRun(t, true, 16*time.Millisecond, domain.RoleDecode)
	require.NotNil(t, on)
	assert.Equal(t, k2BoundITL, on.Bound)
	assert.Equal(t, itlTargetSLO, on.Reason)
	assert.Less(t, on.K2, on.K2Unbounded)
	assert.False(t, on.Inert())

	assert.Nil(t, packingRun(t, true, 0, domain.RoleDecode), "no objective")
	assert.Nil(t, packingRun(t, false, 16*time.Millisecond, domain.RoleDecode), "off")
	assert.Nil(t, packingRun(t, true, 16*time.Millisecond, domain.RoleBoth), "aggregated")
}

// A borrowed line leaves the bound inert, and the packing says why.
func TestK2BatchAtITLTarget_ABorrowedLineIsInert(t *testing.T) {
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		AcceleratorName: "H200", GpuCount: 1, EngineParams: deploymentParams(),
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchK2BatchAtITLTarget: true}))
	a.publishLine(a.itlPhysicsKey("test-ns", "test-model", "variant-d", "H200", 1, ""),
		"a fitted line", itl.Model{A: 0.05, B: 0.012}, time.Now())
	in := makeAnalyzerInput([]domain.ReplicaMetrics{boundRow()}, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
	})
	tpotPolicy(in.Config.(*config.ScalingPolicy), 22*time.Millisecond)
	_, err := a.Analyze(context.Background(), in)
	require.NoError(t, err)
	packing, ok := a.TakeDecodePacking("test-ns", "test-model")
	require.True(t, ok)
	p, found := packing["variant-d"]
	require.True(t, found)
	assert.Equal(t, domain.DecodePacking{Bound: k2BoundNone, Reason: itlBoundBorrowed,
		K2Unbounded: boundAtSteady, K2: boundAtSteady}, p)
	assert.True(t, p.Inert())
}
