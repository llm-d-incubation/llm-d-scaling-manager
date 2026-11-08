package throughput

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// pricedKVreq is the KV per request v1's supply was priced at this cycle: one
// replica's supply is k_sat x KV_max / KVreq sequences over ITL(k_sat)
// (computeVariantSupply), and lineRow's KV_max is 65536.
func pricedKVreq(t *testing.T, a *ThroughputAnalyzer, vc domain.VariantCapacity) float64 {
	t.Helper()
	st, ok := a.VariantState("m", "ns", "v1")
	require.True(t, ok)
	itlSat := st.ITLModel.ITLAt(itl.DefaultKSat)
	require.Greater(t, itlSat, 0.0)
	require.Greater(t, vc.PerReplicaCapacity, 0.0)
	return itl.DefaultKSat * 65536 / (vc.PerReplicaCapacity * itlSat)
}

// dispersionKVreq runs an analyzer through Analyze on the steady fleet (1024
// tokens in, 256 out, no prefix hits), each row read through spread, and
// returns the KV per request its supply was priced at.
func dispersionKVreq(t *testing.T, on bool, spread func(*domain.ReplicaMetrics)) float64 {
	t.Helper()
	a := NewThroughputAnalyzer()
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchKVPerRequestOutputDispersion: on}))
	r := newLineRun(t, a)
	var (
		vc  domain.VariantCapacity
		has bool
	)
	for range 12 {
		rows := steadyFleet()
		for i := range rows {
			spread(&rows[i])
		}
		vc, has = r.step(rows)
	}
	require.True(t, has, "no capacity for v1: the case tests nothing")
	return pricedKVreq(t, a, vc)
}

// spreadOf gives a row outputs of dispersion d about its mean.
func spreadOf(d float64) func(*domain.ReplicaMetrics) {
	return func(rm *domain.ReplicaMetrics) {
		rm.AvgOutputTokensSquared = d * rm.AvgOutputTokens * rm.AvgOutputTokens
		rm.AvgOutputTokensSquaredKnown = true
	}
}

// saturationKVreq is the KV per request the saturation analyzer prices the
// same fleet at: the fleet's shape with its request-weighted spread
// (saturation's fleetShape: shape.New(...).WithOutputDispersion(D), D =
// shape.Dispersion of the fleet's E[O] and its bound on E[O^2]).
func saturationKVreq(d float64) float64 {
	const il, ol = 1024.0, 256.0
	return shape.New(il, ol, 0).WithOutputDispersion(shape.Dispersion(ol, d*ol*ol)).KVreq
}

// Outputs of D 2 about a mean of 256 hold 1024 + 2 x 256/2 = 1280 tokens a
// resident request, the KV per request saturation prices the same fleet at;
// with the switch off, 1024 + 256/2 = 1152, as before.
func TestKVOutputDispersion_TheSupplyHoldsTheSpread(t *testing.T) {
	require.InDelta(t, 1280, saturationKVreq(2), 1e-9)
	require.InDelta(t, saturationKVreq(2), dispersionKVreq(t, true, spreadOf(2)), 1e-6)
	require.InDelta(t, 1152, dispersionKVreq(t, false, spreadOf(2)), 1e-6, "off: D 1, as before")
	require.True(t, NewThroughputAnalyzer().kvOutputDispersion, "on from the constructor")
}

// No reading of the spread is no spread. Every case is one row of three that
// read no bound beside two that read D 4, so a guard that lets the bad row
// through prices a spread from mismatched rows (fleet.Mean drops a NaN or
// +Inf bound from E[O^2] but not the row's length from E[O]): no histogram,
// the presence bit unset, a bound of 0, NaN, or +Inf. Each prices D 1. A
// fleet with no histogram at all prices D 1 too.
func TestKVOutputDispersion_NoReadingIsNoSpread(t *testing.T) {
	podABound := func(v float64, known bool) func(*domain.ReplicaMetrics) {
		return func(rm *domain.ReplicaMetrics) {
			spreadOf(4)(rm)
			if rm.PodName == "pod-a" {
				rm.AvgOutputTokensSquared, rm.AvgOutputTokensSquaredKnown = v, known
			}
		}
	}
	require.InDelta(t, 1024+4*128.0, dispersionKVreq(t, true, spreadOf(4)), 1e-6, "premise: the good rows read D 4")
	for name, spread := range map[string]func(*domain.ReplicaMetrics){
		"absent":  podABound(0, false),
		"unknown": podABound(4*256*256, false),
		"zero":    podABound(0, true),
		"NaN":     podABound(math.NaN(), true),
		"+Inf":    podABound(math.Inf(1), true),
		"no row":  func(*domain.ReplicaMetrics) {},
	} {
		require.InDelta(t, 1152, dispersionKVreq(t, true, spread), 1e-6, name)
	}
}

// The spread is read with the shape's weights: 9 requests a second of 100
// tokens beside 1 of 1000 mean 190, E[O^2] 109000, D 3.02, as saturation's
// fleetOutputDispersion and the slo analyzer read them. A row the shape does
// not count (no prompt length) has no say, reading or not.
func TestKVOutputDispersion_TheShapesWeights(t *testing.T) {
	busy := domain.ReplicaMetrics{RequestRate: 9, AvgInputTokens: 500, AvgOutputTokens: 100,
		AvgOutputTokensSquared: 1e4, AvgOutputTokensSquaredKnown: true}
	long := domain.ReplicaMetrics{RequestRate: 1, AvgInputTokens: 500, AvgOutputTokens: 1000,
		AvgOutputTokensSquared: 1e6, AvgOutputTokensSquaredKnown: true}
	noPrompt := domain.ReplicaMetrics{RequestRate: 50, AvgOutputTokens: 5}
	rows := []domain.ReplicaMetrics{busy, long, noPrompt}
	_, ol, _ := averageShapeMetrics(rows)
	require.InDelta(t, 190, ol, 1e-9)
	require.InDelta(t, 109000.0/(190*190), variantOutputDispersion(rows), 1e-9)
}

// A shape change holds the spread at 1 for the policy's shape-change window
// (DefaultShapeChangeSpreadHold with none): the [5m] histogram then holds
// both shapes. The fleet moves from 256 to 512 tokens out at D 2; every
// cycle priced inside the window reads 1024 + 512/2 = 1280, and the first one
// past it 1024 + 2 x 512/2 = 1536.
func TestKVOutputDispersion_AShapeChangeHoldsTheSpread(t *testing.T) {
	a := NewThroughputAnalyzer()
	r := newLineRun(t, a)
	rowsAt := func(ol float64) []domain.ReplicaMetrics {
		rows := steadyFleet()
		for i := range rows {
			rows[i].AvgOutputTokens = ol
			spreadOf(2)(&rows[i])
		}
		return rows
	}
	for range 12 {
		r.step(rowsAt(256))
	}
	changedAt := r.now()
	held := 0
	for r.now().Sub(changedAt) < DefaultShapeChangeSpreadHold {
		at := r.now()
		vc, has := r.step(rowsAt(512))
		if !has {
			continue
		}
		held++
		require.InDelta(t, 1280, pricedKVreq(t, a, vc), 1e-6, "priced the spread %s after the change", at.Sub(changedAt))
	}
	require.Positive(t, held, "no cycle priced inside the window: the case tests nothing")
	vc, has := r.step(rowsAt(512))
	require.True(t, has)
	require.InDelta(t, 1536, pricedKVreq(t, a, vc), 1e-6, "the spread past the window")
}

// The window is the policy's when it sets one.
func TestKVOutputDispersion_TheHoldIsThePolicysWindow(t *testing.T) {
	require.Equal(t, DefaultShapeChangeSpreadHold, spreadHoldOf(nil))
	require.Equal(t, 90*time.Second, spreadHoldOf(holdPolicy(90)))
	state := &variantState{}
	now := lineStart
	require.False(t, spreadHeld(state, time.Minute, now), "no change yet")
	state.shapeChangedAt = now.Add(-59 * time.Second)
	require.True(t, spreadHeld(state, time.Minute, now))
	state.shapeChangedAt = now.Add(-time.Minute)
	require.False(t, spreadHeld(state, time.Minute, now))
}

// holdPolicy is a config that states a shape-change window of seconds.
type holdPolicy int

func (holdPolicy) GetAnalyzerName() string { return AnalyzerName }

func (h holdPolicy) ShapeChangeWindow(time.Duration) time.Duration {
	return time.Duration(h) * time.Second
}
