package saturation

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// Issue #276: the KV a resident request holds is ILeff + D*OL/2, D =
// E[O^2]/E[O]^2 the spread of output lengths, not ILeff + OL/2. Each reader
// is driven through Analyze.

// withSpread returns rm carrying a bound on E[O^2] of d times its mean
// squared: a spread of output lengths D = d.
func withSpread(rm domain.ReplicaMetrics, d float64) domain.ReplicaMetrics {
	rm.AvgOutputTokensSquared = d * rm.AvgOutputTokens * rm.AvgOutputTokens
	rm.AvgOutputTokensSquaredKnown = true
	return rm
}

// derivedEstimate is the source a zero-replica-capacity-estimate line names
// for an estimate derived from the Deployment's engine params.
const derivedEstimate = "deployment-derived"

// zeroVariant is the variant at zero replicas the estimate tests price.
const zeroVariant = "variant-z"

// switchOff turns SwitchKVPerRequestOutputDispersion off.
func switchOff(a *SaturationAnalyzer) { a.kvOutputDispersion = false }

// derivedK2 runs one cycle of an idle decode replica of 1000 in / 500 out
// with spread d, under the parser's default engine (256 sequences, 8192
// batched tokens), and returns the k2-decision line: the chain reaches P3.
func derivedK2(t *testing.T, d float64, edit func(*SaturationAnalyzer)) map[string]any {
	t.Helper()
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		AcceleratorName: "H100", GpuCount: 1, EngineParams: deploymentParams(),
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	if edit != nil {
		edit(a)
	}
	rm := makeReplicaMetrics("pod-1", "variant-d", 50_000, 1_000_000, 0, 1000, 500)
	if d > 0 {
		rm = withSpread(rm, d)
	}
	in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H100",
			CurrentReplicas: 1, GPUsPerReplica: 1},
	})
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	for _, e := range logs.FilterMessage("k2-decision").All() {
		if f := e.ContextMap(); f["priority"] == capacity.K2SrcDerived.String() {
			return f
		}
	}
	require.FailNow(t, "no derived k2-decision line")
	return nil
}

// The derived k2 prices the sequences the batch cap admits at I + D*O/2
// each. 256 sequences bind (8192 x 500/1500 = 2731 do not): 256 x (1000 +
// 2 x 250) = 384,000 at D = 2, where I + O/2 priced 320,000 -- a replica
// that holds more KV before its batch binds than k2 said, so fewer replicas
// are ordered.
func TestKVOutputDispersion_TheDerivedK2HoldsTheSpread(t *testing.T) {
	f := derivedK2(t, 2, nil)
	assert.EqualValues(t, 384_000, f["k2"])
	assert.InDelta(t, 2, f["outputDispersion"], 1e-12)

	f = derivedK2(t, 1, nil)
	assert.EqualValues(t, 320_000, f["k2"], "equal lengths price as before")
}

// spreadMu runs one cycle of a decode replica of 100 in / 50 out with spread
// d, its cache 100,000 tokens and its declared cap 1024 a rank, so the KV and
// not the cap binds the derived mu, and returns the derived-mu line.
func spreadMu(t *testing.T, d float64, edit func(*SaturationAnalyzer)) map[string]any {
	t.Helper()
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		AcceleratorName: "H200", GpuCount: 1, EngineParams: declaredParams(1024),
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	a.publishLine(a.itlPhysicsKey("test-ns", "test-model", "variant-d", "H200", 1, ""),
		"a fitted line", itl.Model{A: 0.08, B: 0.01}, time.Now())
	if edit != nil {
		edit(a)
	}
	rm := makeReplicaMetrics("pod-1", "variant-d", 5000, 100_000, 0, 100, 50)
	if d > 0 {
		rm = withSpread(rm, d)
	}
	in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H200",
			CurrentReplicas: 1, GPUsPerReplica: 1},
	})
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	fields := requireLogged(t, logs, "derived-mu")
	require.Equal(t, true, fields["ok"], "the line prices mu: %v", fields)
	return fields
}

// The derived mu fits fewer sequences at the pricing point: 0.68 x 100,000 /
// (100 + 3 x 25) = 388.6 at D = 3, where 100 + 25 fitted 544 -- each
// resident request holds more KV, so a replica serves fewer requests a
// second, and more replicas are ordered.
func TestKVOutputDispersion_TheDerivedMuFitsFewerSequences(t *testing.T) {
	on := spreadMu(t, 3, nil)
	assert.InDelta(t, 68_000.0/175, on["seqs"], 1e-6)
	assert.InDelta(t, 175, on["kvReqPerSeq"], 1e-9)
	assert.InDelta(t, 3, on["outputDispersion"], 1e-12)

	flat := spreadMu(t, 1, nil)
	assert.InDelta(t, 544, flat["seqs"], 1e-6, "equal lengths price as before")
	assert.Less(t, on["rate"], flat["rate"])
}

// zeroReplicaEstimate runs one cycle of a live variant of 1000 in / 500 out
// with spread d and a variant at zero replicas whose record is derived from
// its Deployment, and returns the zero-replica-capacity-estimate line.
func zeroReplicaEstimate(t *testing.T, d float64, edit func(*SaturationAnalyzer)) map[string]any {
	t.Helper()
	return zeroReplicaEstimateOf(t, deploymentParams(), d, edit)
}

// zeroReplicaEstimateOf is zeroReplicaEstimate with the zero-replica
// variant's record derived from params.
func zeroReplicaEstimateOf(t *testing.T, params *capacity.EngineParams, d float64,
	edit func(*SaturationAnalyzer)) map[string]any {
	t.Helper()
	return zeroReplicaEstimateAs(t, params, domain.RoleDecode, 1, d, edit)
}

// zeroReplicaEstimateAs is zeroReplicaEstimateOf with the zero-replica
// variant in role, on gpus GPUs per replica.
func zeroReplicaEstimateAs(t *testing.T, params *capacity.EngineParams, role string, gpus int, d float64,
	edit func(*SaturationAnalyzer)) map[string]any {
	t.Helper()
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-z", capacity.Record{
		AcceleratorName: "A100", GpuCount: gpus, EngineParams: params,
		EffectiveCapacity: 1, LearnedFrom: "deployment",
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	if edit != nil {
		edit(a)
	}
	rm := withSpread(makeReplicaMetrics("pod-1", "variant-a", 50_000, 1_000_000, 0, 1000, 500), d)
	in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
		{VariantName: "variant-a", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "variant-z", Role: role, AcceleratorName: "A100", CurrentReplicas: 0, GPUsPerReplica: gpus},
	})
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	for _, e := range logs.FilterMessage("zero-replica-capacity-estimate").All() {
		if f := e.ContextMap(); f["variant"] == zeroVariant && f["source"] == derivedEstimate {
			return f
		}
	}
	require.FailNow(t, "no deployment-derived estimate for variant-z")
	return nil
}

// A variant at zero replicas is priced from the model's lengths and their
// spread: 256 x (1000 + 2 x 250) = 384,000 at D = 2.
func TestKVOutputDispersion_AZeroReplicaVariantIsPricedWithTheSpread(t *testing.T) {
	f := zeroReplicaEstimate(t, 2, nil)
	assert.EqualValues(t, 384_000, f["derivedCapacity"])
	assert.InDelta(t, 2, f["modelOutputFactor"], 1e-12)
}

// Off, every reader prices ILeff + OL/2 as before #276, whatever spread was
// read.
func TestKVOutputDispersion_TheSwitchPicksTheKVPerRequest(t *testing.T) {
	assert.EqualValues(t, 320_000, derivedK2(t, 2, switchOff)["k2"])
	assert.InDelta(t, 1, derivedK2(t, 2, switchOff)["outputDispersion"], 1e-12)

	off := spreadMu(t, 3, switchOff)
	assert.InDelta(t, 544, off["seqs"], 1e-6)
	assert.InDelta(t, 125, off["kvReqPerSeq"], 1e-9)

	assert.EqualValues(t, 320_000, zeroReplicaEstimate(t, 2, switchOff)["derivedCapacity"])
}

// No bound read is no spread: a replica without the histogram (SGLang, a
// relabelling that drops it) prices as before, and so does a fleet where
// any generating replica lacks it -- its lengths would otherwise count as
// all one length beside the others' spread.
func TestKVOutputDispersion_NoHistogramPricesAsBefore(t *testing.T) {
	f := derivedK2(t, 0, nil)
	assert.EqualValues(t, 320_000, f["k2"])
	assert.InDelta(t, 1, spreadMu(t, 0, nil)["outputDispersion"], 1e-12)

	known := withSpread(makeReplicaMetrics("pod-1", "variant-a", 5000, 100_000, 0, 100, 50), 3)
	unknown := makeReplicaMetrics("pod-2", "variant-a", 5000, 100_000, 0, 100, 50)
	assert.InDelta(t, 3, fleetOutputDispersion([]domain.ReplicaMetrics{known, known}, nil), 1e-12)
	assert.InDelta(t, 1, fleetOutputDispersion([]domain.ReplicaMetrics{known, unknown}, nil), 1e-12)
	assert.InDelta(t, 1, fleetOutputDispersion(nil, nil), 1e-12)

	// A prefill replica's lengths (about one token) are not the workload's,
	// and do not need a histogram.
	prefill := makeReplicaMetrics("pod-3", "variant-p", 5000, 100_000, 0, 100, 1)
	roles := map[string]string{"variant-p": domain.RolePrefill}
	assert.InDelta(t, 3, fleetOutputDispersion([]domain.ReplicaMetrics{known, prefill}, roles), 1e-12)
}

// A bound of +Inf or NaN is no reading (fleet.SquaredBoundRead): a fleet
// with one is read as no spread. Admitted, +Inf passed the bound check while
// the mean of E[O^2] dropped the row and the mean of E[O] kept it, so D read
// from mismatched rows: a 5-token row beside a 50-token one at D 3 read E[O]
// 27.5 against the other row's E[O^2] 7500, D 9.9.
func TestKVOutputDispersion_ABoundThatIsNoNumberIsNoReading(t *testing.T) {
	known := withSpread(makeReplicaMetrics("pod-1", "variant-a", 5000, 100_000, 0, 100, 50), 3)
	for _, v := range []float64{math.Inf(1), math.NaN()} {
		bad := known
		bad.PodName, bad.AvgOutputTokens, bad.AvgOutputTokensSquared = "pod-2", 5, v
		assert.InDelta(t, 1, fleetOutputDispersion([]domain.ReplicaMetrics{known, bad}, nil), 1e-12, "bound %v", v)
		_, _, ok := fleetOutputMoments([]domain.ReplicaMetrics{known, bad}, nil)
		assert.False(t, ok, "bound %v", v)
	}
}

// The fleet's spread is weighted by request rate, as its mean is, so the two
// divide into one fleet's D: a replica at 100 tokens with D 1 serving three
// times the requests of one at 300 with D 2 is E[O] = 150, E[O^2] =
// (3 x 10,000 + 180,000)/4 = 52,500, D = 2.33.
func TestKVOutputDispersion_TheFleetSpreadIsRequestWeighted(t *testing.T) {
	a := withSpread(makeReplicaMetrics("pod-1", "variant-a", 0, 100_000, 0, 100, 100), 1)
	a.RequestRate = 3
	b := withSpread(makeReplicaMetrics("pod-2", "variant-a", 0, 100_000, 0, 100, 300), 2)
	b.RequestRate = 1
	assert.InDelta(t, 52_500.0/(150*150), fleetOutputDispersion([]domain.ReplicaMetrics{a, b}, nil), 1e-12)
}

// estimateCapacityAtCapSpread reads a D below 1 or not finite as 1, and the
// one-figure form is the spread form at 1.
func TestKVOutputDispersion_EstimateReadsABadSpreadAsNone(t *testing.T) {
	p := deploymentParams()
	base := estimateCapacityAtCapSpread(p, 256, 1000, 500, 1)
	for _, d := range []float64{0, 0.5, -1, math.NaN(), math.Inf(1)} {
		assert.Equal(t, base, estimateCapacityAtCapSpread(p, 256, 1000, 500, d), "D %v", d)
	}
	assert.Equal(t, int64(384_000), estimateCapacityAtCapSpread(p, 256, 1000, 500, 2))
}

// The zero-replica estimate prices the fleet's own requests, request-weighted
// on both moments (modelOutputFactor). One replica serves 9 requests a second
// of 100 tokens and one serves 1 of 1000, both at 1000 in: E[O] 190, E[O^2]
// 109,000, so a resident request holds 1000 + 109,000/380 = 1286.8 and 256 of
// them 329,431. The plain mean the estimate also reads is 550, and the
// request-weighted D (3.02) times it priced 1000 + 830: 468,557, 42% over.
func TestKVOutputDispersion_AZeroReplicaEstimateIsRequestWeighted(t *testing.T) {
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-z", capacity.Record{
		AcceleratorName: "A100", GpuCount: 1, EngineParams: deploymentParams(),
		EffectiveCapacity: 1, LearnedFrom: "deployment",
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	short := withSpread(makeReplicaMetrics("pod-1", "variant-a", 50_000, 1_000_000, 0, 1000, 100), 1)
	short.RequestRate = 9
	long := withSpread(makeReplicaMetrics("pod-2", "variant-a", 50_000, 1_000_000, 0, 1000, 1000), 1)
	long.RequestRate = 1
	in := makeAnalyzerInput([]domain.ReplicaMetrics{short, long}, []domain.VariantReplicaState{
		{VariantName: "variant-a", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 2, GPUsPerReplica: 1},
		{VariantName: "variant-z", Role: domain.RoleDecode, AcceleratorName: "A100", CurrentReplicas: 0, GPUsPerReplica: 1},
	})
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	var got any
	for _, e := range logs.FilterMessage("zero-replica-capacity-estimate").All() {
		if f := e.ContextMap(); f["variant"] == zeroVariant && f["source"] == derivedEstimate {
			got = f["derivedCapacity"]
		}
	}
	require.NotNil(t, got, "no deployment-derived estimate for variant-z")
	assert.InDelta(t, 329_431, got, 1, "256 sequences of 1000 + E[O^2]/(2 E[O]), both request-weighted")
}

// shapeSwap runs two cycles of one idle decode replica at 2000 in under the
// parser's default engine: 1000 out, every request alike, then a cycle half
// way through a switch to 100 out. The [5m] figures hold both shapes (a mean
// of 550 and E[O^2] 505,000, D 1.67); the [1m] ones the new shape. It returns
// the second cycle's derived-mu and k2-decision lines.
func shapeSwap(t *testing.T, edit func(*SaturationAnalyzer)) (mu, k2 map[string]any) {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		AcceleratorName: "H100", GpuCount: 1, EngineParams: deploymentParams(),
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	a.publishLine(a.itlPhysicsKey("test-ns", "test-model", "variant-d", "H100", 1, ""),
		"a fitted line", itl.Model{A: 0.08, B: 0.01}, time.Now())
	if edit != nil {
		edit(a)
	}
	states := []domain.VariantReplicaState{{VariantName: "variant-d", Role: domain.RoleDecode,
		AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1}}
	row := func(out5m, squared, out1m float64) []domain.ReplicaMetrics {
		rm := makeReplicaMetrics("pod-1", "variant-d", 50_000, 1_000_000, 0, 2000, out5m)
		rm.AvgOutputTokensSquared, rm.AvgOutputTokensSquaredKnown = squared, true
		rm.AvgOutputTokensRecent, rm.AvgInputTokensRecent = out1m, 2000
		return []domain.ReplicaMetrics{rm}
	}
	ctx1, _ := observedCtx(t)
	_, err := a.Analyze(ctx1, makeAnalyzerInput(row(1000, 1_000_000, 1000), states))
	require.NoError(t, err)
	ctx2, logs := observedCtx(t)
	_, err = a.Analyze(ctx2, makeAnalyzerInput(row(550, 505_000, 100), states))
	require.NoError(t, err)
	mu = requireLogged(t, logs, "derived-mu")
	for _, e := range logs.FilterMessage("k2-decision").All() {
		if f := e.ContextMap(); f["priority"] == capacity.K2SrcDerived.String() {
			k2 = f
		}
	}
	require.NotNil(t, k2, "no derived k2-decision line")
	return mu, k2
}

// The short-window shape takes no spread: mid-swap the mu is priced at 2000
// + 100/2, the shape being served, not at the [5m] D of the two shapes mixed.
func TestKVOutputDispersion_TheShortWindowTakesNoSpread(t *testing.T) {
	mu, _ := shapeSwap(t, nil)
	require.Equal(t, true, mu["muShortWindow"], "the swap opened the short window")
	assert.InDelta(t, 1, mu["outputDispersion"], 1e-12)
	assert.InDelta(t, 2050, mu["kvReqPerSeq"], 1e-9, "2000 + 100/2")
	assert.InDelta(t, 2275, mu["kvReqFleet"], 1e-9, "the fleet's [5m] shape takes no spread either: 2000 + 550/2")
}

// Neither does the k2 path while the change is outstanding. Priced with the
// mixed window's D 1.67 a resident request held 2000 + 1.67 x 275 = 2459
// against the 2050 the arriving requests hold, k2 20% high; without the
// spread it is 2275, as before #276.
func TestKVOutputDispersion_AShapeChangeHoldsTheK2Spread(t *testing.T) {
	_, k2 := shapeSwap(t, nil)
	assert.InDelta(t, 1, k2["outputDispersion"], 1e-12)
	assert.EqualValues(t, 256*2275, k2["k2"])
}

// zeroReplicaSwap runs shapeSwap's two cycles for a live variant beside a
// variant at zero replicas whose record is derived from its Deployment, and
// returns the second cycle's zero-replica-capacity-estimate line.
func zeroReplicaSwap(t *testing.T) map[string]any {
	t.Helper()
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-z", capacity.Record{
		AcceleratorName: "A100", GpuCount: 1, EngineParams: deploymentParams(),
		EffectiveCapacity: 1, LearnedFrom: "deployment",
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	states := []domain.VariantReplicaState{
		{VariantName: "variant-a", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "variant-z", Role: domain.RoleDecode, AcceleratorName: "A100", CurrentReplicas: 0, GPUsPerReplica: 1},
	}
	row := func(out5m, squared, out1m float64) []domain.ReplicaMetrics {
		rm := makeReplicaMetrics("pod-1", "variant-a", 50_000, 1_000_000, 0, 2000, out5m)
		rm.AvgOutputTokensSquared, rm.AvgOutputTokensSquaredKnown = squared, true
		rm.AvgOutputTokensRecent, rm.AvgInputTokensRecent = out1m, 2000
		return []domain.ReplicaMetrics{rm}
	}
	ctx1, _ := observedCtx(t)
	_, err := a.Analyze(ctx1, makeAnalyzerInput(row(1000, 1_000_000, 1000), states))
	require.NoError(t, err)
	ctx2, logs := observedCtx(t)
	_, err = a.Analyze(ctx2, makeAnalyzerInput(row(550, 505_000, 100), states))
	require.NoError(t, err)
	for _, e := range logs.FilterMessage("zero-replica-capacity-estimate").All() {
		if f := e.ContextMap(); f["variant"] == zeroVariant && f["source"] == derivedEstimate {
			return f
		}
	}
	require.FailNow(t, "no deployment-derived estimate for variant-z")
	return nil
}

// Nor does the zero-replica estimate take the mixed window's spread while
// the change is outstanding: 256 x (2000 + 550/2) = 582,400, where the
// factor 1.67 the window reads priced 629,527.
func TestKVOutputDispersion_AShapeChangeHoldsTheZeroReplicaSpread(t *testing.T) {
	f := zeroReplicaSwap(t)
	assert.InDelta(t, 1, f["modelOutputFactor"], 1e-12)
	assert.EqualValues(t, 256*2275, f["derivedCapacity"])
}

// The replay hook reaches the switch: off through SetSwitchesForReplay, the
// derived k2 at D 2 prices I + O/2 as before (320,000); on again, 384,000.
func TestKVOutputDispersion_TheReplayHookSetsTheSwitch(t *testing.T) {
	set := func(on bool) func(*SaturationAnalyzer) {
		return func(a *SaturationAnalyzer) {
			require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchKVPerRequestOutputDispersion: on}))
		}
	}
	assert.EqualValues(t, 320_000, derivedK2(t, 2, set(false))["k2"])
	assert.EqualValues(t, 384_000, derivedK2(t, 2, set(true))["k2"])
}

// A factor under 1 is priced as 1. One replica serves 99 requests a second
// of 100 tokens and one serves 1 of 1000: E[O] 109, E[O^2] 19,900, a plain
// mean of 550, so the factor is 19,900 / (109 x 550) = 0.33. The estimate
// keeps 256 x (1000 + 550/2) = 326,400, the price before #276, rather than
// moving below it to the 1000 + 91 the requests hold; the log carries the
// factor as computed.
func TestKVOutputDispersion_AZeroReplicaFactorUnderOneIsPricedAsOne(t *testing.T) {
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-z", capacity.Record{
		AcceleratorName: "A100", GpuCount: 1, EngineParams: deploymentParams(),
		EffectiveCapacity: 1, LearnedFrom: "deployment",
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	short := withSpread(makeReplicaMetrics("pod-1", "variant-a", 50_000, 1_000_000, 0, 1000, 100), 1)
	short.RequestRate = 99
	long := withSpread(makeReplicaMetrics("pod-2", "variant-a", 50_000, 1_000_000, 0, 1000, 1000), 1)
	long.RequestRate = 1
	in := makeAnalyzerInput([]domain.ReplicaMetrics{short, long}, []domain.VariantReplicaState{
		{VariantName: "variant-a", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 2, GPUsPerReplica: 1},
		{VariantName: "variant-z", Role: domain.RoleDecode, AcceleratorName: "A100", CurrentReplicas: 0, GPUsPerReplica: 1},
	})
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	var f map[string]any
	for _, e := range logs.FilterMessage("zero-replica-capacity-estimate").All() {
		if m := e.ContextMap(); m["variant"] == zeroVariant && m["source"] == derivedEstimate {
			f = m
		}
	}
	require.NotNil(t, f, "no deployment-derived estimate for variant-z")
	assert.InDelta(t, 19_900.0/(109*550), f["modelOutputFactor"], 1e-9)
	assert.EqualValues(t, 326_400, f["derivedCapacity"])
}
