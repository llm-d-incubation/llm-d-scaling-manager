package saturation

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// Issue llm-d-incubation/llm-d-scaling-manager#188: an engine can hold more KV
// per running request than its context (a sparse-attention indexer's keys, a
// speculative draft model's KV). With SwitchKVPerRequestAboveContext on, the
// reading is priced as read above the context, by the capacity planner's rule
// (shape.PriceResident). The tests set the switch by its name, so they run on
// a commit that predates it, and fail there.

// aboveOn turns the switch on, as a replay does.
func aboveOn(t *testing.T) func(*SaturationAnalyzer) {
	return func(a *SaturationAnalyzer) {
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{"kv-per-request-above-context": true}))
	}
}

// aboveOff turns the switch off, as a replay does: the switch is on from the
// constructor.
func aboveOff(t *testing.T) func(*SaturationAnalyzer) {
	return func(a *SaturationAnalyzer) {
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{"kv-per-request-above-context": false}))
	}
}

// aboveOnSGLang is aboveOn on an SGLang engine.
func aboveOnSGLang(t *testing.T) func(*SaturationAnalyzer) {
	return func(a *SaturationAnalyzer) {
		aboveOn(t)(a)
		p := declaredParams(1024)
		p.Engine = inferenceengine.EngineSGLang
		a.capacityStore.Update("test-ns", "test-model", "variant-d", capacity.Record{
			AcceleratorName: "H200", GpuCount: 1, EngineParams: p,
		}, time.Now())
	}
}

// Thirty running requests hold 90,000 tokens, 3000 each, against a context of
// 1050 (cachingRow): an engine holding about 2.9 capacity tokens a context
// token. On, the derived mu is priced at 3000 a request, 0.68 x 100,000 /
// 3000 = 22.7 sequences, where the bound priced 64.8: a KV-bound mu 2.9x
// high. The log marks it above the context. Off, the bound holds, as before.
func TestKVAboveContext_AReadingAboveTheContextIsPricedAsRead(t *testing.T) {
	on := hitMu(t, cachingRow(0.9, 30), aboveOn(t))
	assert.InDelta(t, 3000, on["kvReqPerSeq"], 1e-9)
	assert.InDelta(t, 3000, on["kvReqFleet"], 1e-9, "the GPS check prices the same request")
	assert.InDelta(t, 68_000.0/3000, on["seqs"], 1e-9)
	assert.Equal(t, "above-context", on["residentKVBound"])
	assert.Equal(t, "above-context", on["residentKVBoundFleet"])

	off := hitMu(t, cachingRow(0.9, 30), aboveOff(t))
	assert.InDelta(t, 1050, off["kvReqPerSeq"], 1e-9)
	assert.Equal(t, "upper", off["residentKVBound"])
	assert.Less(t, on["rate"], off["rate"], "more KV a request, fewer sequences, a lower mu")
}

// Ten running requests hold 30,000 tokens at a fill of 0.3, 3000 each,
// against a discount of 550 and a context of 1050. The count gate at the
// discount asks for 0.15 x 100,000 / 550 = 28 requests, more than a cache
// filled to 0.3 holds at 3000 each, so it would read nothing at any fill
// below 0.82. On, the gate is taken at the reading, 0.15 x 100,000 / 3000 =
// 5 requests: the reading is taken and marked above the context. Off, the
// gate stays at the discount and there is no reading, as before.
func TestKVAboveContext_TheCountGateTakesTheReading(t *testing.T) {
	r := residentReading{tokens: 30_000, running: 10, capTokens: 100_000, minK: 0.15}
	s := shape.New(1000, 100, 0.5)
	got, bound := withResidentKV(s, r, residentRule{aboveContext: true})
	assert.InDelta(t, 3000, got.KVreq, 1e-9)
	assert.Equal(t, residentBoundAboveContext, bound)
	got, bound = withResidentKV(s, r, residentRule{})
	assert.InDelta(t, 550, got.KVreq, 1e-9)
	assert.Equal(t, residentBoundNoReading, bound)

	on := hitMu(t, cachingRow(0.3, 10), aboveOn(t))
	assert.InDelta(t, 3000, on["kvReqPerSeq"], 1e-9)
	assert.Equal(t, "above-context", on["residentKVBound"])
	assert.InDelta(t, 68_000.0/3000, on["seqs"], 1e-9)

	off := hitMu(t, cachingRow(0.3, 10), aboveOff(t))
	assert.InDelta(t, 550, off["kvReqPerSeq"], 1e-9)
	assert.Equal(t, "no-reading", off["residentKVBound"])
}

// The count gate's own case (TestKVHitsResident_FewLongRequestsSharingAPrefixAreNoReading)
// with the switch on: a 500,000-token cache, 100,000-token prompts 90% shared,
// 1000 out. The discount is 10,500 and the context 100,500. Two requests at a
// fill of 0.222 read 55,500 each, below the context: the shared prefix spread
// over two requests, not KV the engine holds beyond the prompt. The gate stays
// at the discount, asks for 8, and there is no reading. Eight requests reading
// 220,000 each in a 2,000,000-token cache, over twice the context, are taken
// on and priced as read; off, the gate at the discount asks for 29 and reads
// nothing.
func TestKVAboveContext_FewLongRequestsBelowTheContextAreNoReading(t *testing.T) {
	row := func(capTokens int64, running int, perSeq float64) domain.ReplicaMetrics {
		rm := hitRow("pod-1", 100_000, 1000, capTokens, float64(running)*perSeq/float64(capTokens), running)
		rm.PrefixCacheHitRate = 0.9
		return rm
	}
	two := hitMu(t, row(500_000, 2, 55_500), aboveOn(t))
	assert.Equal(t, residentBoundNoReading, two["residentKVBound"])
	assert.InDelta(t, 10_500, two["kvReqPerSeq"], 1e-6, "the discount, not 55,500")
	eight := hitMu(t, row(500_000, 8, 21_750), aboveOn(t))
	assert.Equal(t, residentBoundNone, eight["residentKVBound"])
	assert.InDelta(t, 21_750, eight["kvReqPerSeq"], 1e-6)

	above := hitMu(t, row(2_000_000, 8, 220_000), aboveOn(t))
	assert.Equal(t, residentBoundAboveContext, above["residentKVBound"])
	assert.InDelta(t, 220_000, above["kvReqPerSeq"], 1e-6)
	off := hitMu(t, row(2_000_000, 8, 220_000), aboveOff(t))
	assert.Equal(t, residentBoundNoReading, off["residentKVBound"])
	assert.InDelta(t, 10_500, off["kvReqPerSeq"], 1e-6)
}

// The review's three readings above the context that are not KV held beyond
// prompt and output. The shape is 10,000 in, 1000 out, no hit: a context of
// 10,500. Off, a shape with no hit takes no reading at all.

// noHitRow is a replica of that shape: running requests, the tokens in use
// over capTokens.
func noHitRow(capTokens int64, running int, tokens float64) domain.ReplicaMetrics {
	rm := hitRow("pod-1", 10_000, 1000, capTokens, tokens/float64(capTokens), running)
	rm.PrefixCacheHitRate = 0
	return rm
}

// prefillVariant is a prefill variant beside variant-d: with it, variant-d is
// the decode of a prefill/decode split (hitsMuOf).
var prefillVariant = domain.VariantReplicaState{
	VariantName: "variant-p", Role: domain.RolePrefill, AcceleratorName: "H200", GPUsPerReplica: 1,
}

// pdMu is hitMu on the decode of a prefill/decode split.
func pdMu(t *testing.T, rm domain.ReplicaMetrics, edit func(*SaturationAnalyzer)) map[string]any {
	t.Helper()
	return hitsMuOf(t, []domain.ReplicaMetrics{rm}, edit, prefillVariant)
}

// A P/D decode pulling 30 prompts from prefill beside 4 running: vLLM
// allocates the pulled prompts' blocks first and counts the 30 as waiting.
// 342,000 tokens over the 4 read 85,500 a request, 8.1 times the context;
// over the 34 that hold them, 10,059, under the context and priced there.
func TestKVAboveContext_RequestsPullingTheirKVHoldIt(t *testing.T) {
	rm := noHitRow(1_000_000, 4, 342_000)
	rm.QueueLength, rm.CapacityQueueLength, rm.CapacityQueueKnown = 30, 0, true
	on := pdMu(t, rm, aboveOn(t))
	assert.Equal(t, residentBoundLower, on["residentKVBound"])
	assert.InDelta(t, 10_500, on["kvReqPerSeq"], 1e-6, "not 85,500")
	off := pdMu(t, rm, aboveOff(t))
	assert.Equal(t, residentBoundNoHit, off["residentKVBound"])

	// Requests waiting for capacity hold no KV: with all 30 of them waiting
	// for capacity the 4 running are the ones read, at 85,500 each, and 4 are
	// fewer than minAboveContextSequences: no reading.
	rm.CapacityQueueLength = 30
	capWait := pdMu(t, rm, aboveOn(t))
	assert.Equal(t, residentBoundNoReading, capWait["residentKVBound"])
}

// The requests held without running are counted on the decode of a
// prefill/decode split only. Ten running requests and ten deferred hold
// 420,000 tokens against a context of 10,500: over the 20 that hold them on
// a P/D decode, 21,000 a request; over the 10 running on an aggregated
// replica, whose deferred requests hold no pulled KV, 42,000. A landed
// series of 0 does not drop the ten (the ones still pulling hold their
// blocks too), and a replica that publishes only the landed series counts
// those.
func TestKVAboveContext_HoldersAreCountedOnADisaggregatedDecode(t *testing.T) {
	rm := noHitRow(1_000_000, 10, 420_000)
	rm.QueueLength, rm.CapacityQueueLength, rm.CapacityQueueKnown = 10, 0, true
	pd := pdMu(t, rm, aboveOn(t))
	assert.InDelta(t, 21_000, pd["kvReqPerSeq"], 1e-6)
	agg := hitMu(t, rm, aboveOn(t))
	assert.InDelta(t, 42_000, agg["kvReqPerSeq"], 1e-6, "an aggregated replica counts its running requests only")

	rm.LandedQueueLength, rm.LandedQueueKnown = 0, true
	landed := pdMu(t, rm, aboveOn(t))
	assert.InDelta(t, 21_000, landed["kvReqPerSeq"], 1e-6, "the pulling requests still count")
	assert.EqualValues(t, 10, landed["residentHolding"])

	rm.CapacityQueueKnown, rm.LandedQueueLength = false, 10
	landed = pdMu(t, rm, aboveOn(t))
	assert.InDelta(t, 21_000, landed["kvReqPerSeq"], 1e-6, "the landed series where the queue is not split by reason")
}

// Three running requests with 40,000-token prompts in a shape whose mean is
// 10,000: 121,500 tokens, 40,500 a request, four times the context. They are
// the long tail of the workload, not KV held beyond it, and three are fewer
// than minAboveContextSequences: no reading. Five more requests held
// without running on a P/D decode spread it to 15,188 over eight, still
// above the context, and three are still the requests running: no reading.
func TestKVAboveContext_AFewLongPromptsAreNoReading(t *testing.T) {
	on := hitMu(t, noHitRow(500_000, 3, 121_500), aboveOn(t))
	assert.Equal(t, residentBoundNoReading, on["residentKVBound"])
	assert.InDelta(t, 10_500, on["kvReqPerSeq"], 1e-6, "not 40,500")
	off := hitMu(t, noHitRow(500_000, 3, 121_500), aboveOff(t))
	assert.Equal(t, residentBoundNoHit, off["residentKVBound"])

	queued := noHitRow(500_000, 3, 121_500)
	queued.QueueLength, queued.CapacityQueueLength, queued.CapacityQueueKnown = 5, 0, true
	held := pdMu(t, queued, aboveOn(t))
	assert.Equal(t, residentBoundNoReading, held["residentKVBound"], "the guard counts the running requests")
	assert.InDelta(t, 10_500, held["kvReqPerSeq"], 1e-6, "not 15,188")

	// Eight such requests are read as they are: the scrape cannot tell a
	// long tail from KV held beyond the prompt (minAboveContextSequences).
	eight := hitMu(t, noHitRow(500_000, 8, 324_000), aboveOn(t))
	assert.Equal(t, residentBoundAboveContext, eight["residentKVBound"])
}

// A running count that lags the KV it is read against: 20 requests hold
// 210,000 tokens, the scrape reads 6 running, 35,000 a request. Six are
// fewer than minAboveContextSequences: no reading. Two more held without
// running on a P/D decode (one-minute peaks, not the scrape's instant) make
// eight holders at 26,250 each, and six running: still no reading.
func TestKVAboveContext_ALaggingRunningCountIsNoReading(t *testing.T) {
	on := hitMu(t, noHitRow(500_000, 6, 210_000), aboveOn(t))
	assert.Equal(t, residentBoundNoReading, on["residentKVBound"])
	assert.InDelta(t, 10_500, on["kvReqPerSeq"], 1e-6, "not 35,000")
	off := hitMu(t, noHitRow(500_000, 6, 210_000), aboveOff(t))
	assert.Equal(t, residentBoundNoHit, off["residentKVBound"])

	queued := noHitRow(500_000, 6, 210_000)
	queued.QueueLength, queued.CapacityQueueLength, queued.CapacityQueueKnown = 2, 0, true
	held := pdMu(t, queued, aboveOn(t))
	assert.Equal(t, residentBoundNoReading, held["residentKVBound"], "the guard counts the running requests")
	assert.InDelta(t, 10_500, held["kvReqPerSeq"], 1e-6, "not 26,250")
}

// A reading the requests held without running spread under the context is
// not priced under the one read without them: 30 running and 70 held on a
// P/D decode hold 450,000 tokens against a discount of 5500 and a context
// of 10,500 (hit 0.5). Over the 100, 4500 a request, under the discount;
// over the 30 running, as the switch off reads, 15,000, held at the context.
// On prices the context, the off bound, not the discount.
func TestKVAboveContext_NeverUnderTheSwitchOffPrice(t *testing.T) {
	rm := noHitRow(1_000_000, 30, 450_000)
	rm.PrefixCacheHitRate = 0.5
	rm.QueueLength, rm.CapacityQueueLength, rm.CapacityQueueKnown = 70, 0, true
	on := pdMu(t, rm, aboveOn(t))
	off := pdMu(t, rm, aboveOff(t))
	assert.InDelta(t, 10_500, off["kvReqPerSeq"], 1e-6)
	assert.Equal(t, residentBoundUpper, off["residentKVBound"])
	assert.InDelta(t, 10_500, on["kvReqPerSeq"], 1e-6, "not the discount of 5500")
	assert.Equal(t, residentBoundUpper, on["residentKVBound"], "the off bound priced it")
}

// randomResidentCase is a random shape and fleet reading, with requests held
// without running.
func randomResidentCase(rng *rand.Rand) (shape.Shape, residentReading) {
	s := shape.New(1+rng.Float64()*50_000, 1+rng.Float64()*5000, rng.Float64()).WithOutputDispersion(1 + 3*rng.Float64())
	if rng.IntN(4) == 0 {
		s = shape.New(s.AvgInputTokens, s.AvgOutputTokens, 0).WithOutputDispersion(s.OutputDispersion)
	}
	r := residentReading{
		tokens: rng.Float64() * 2e7, running: rng.IntN(60), holding: rng.IntN(200),
		capTokens: rng.Float64() * 2e6, minK: 0.15 * rng.Float64(),
	}
	return s, r
}

// Whatever the shape, the reading and the requests held without running,
// the switch on never prices a request under the switch off: the requests
// held only spread a reading over more requests, and the guard only drops
// one, and neither takes the price under v0.12.2's.
func TestKVAboveContext_OnIsNeverUnderOff(t *testing.T) {
	rng := rand.New(rand.NewPCG(188, 1))
	for i := range 20_000 {
		s, r := randomResidentCase(rng)
		for _, sglang := range []bool{false, true} {
			on, bound := withResidentKV(s, r, residentRule{aboveContext: true, heldAtContext: sglang})
			off, offBound := withResidentKV(s, r, residentRule{heldAtContext: sglang})
			require.GreaterOrEqual(t, on.KVreq, off.KVreq,
				"case %d (sglang %v): on %s %v under off %s %v, reading %+v", i, sglang, bound, on.KVreq, offBound, off.KVreq, r)
		}
	}
}

// Whatever the shape and the reading, with the switch on the price is never
// under the discount (s.KVreq), only KVreq moves, and on SGLang it is never
// over the context. The rule on its own (priceResidentReading, without the floor at
// the switch-off price) holds the discount too: shape.PriceResident's floor.
func TestKVAboveContext_TheDiscountIsTheFloor(t *testing.T) {
	rng := rand.New(rand.NewPCG(188, 2))
	for i := range 20_000 {
		s, r := randomResidentCase(rng)
		for _, sglang := range []bool{false, true} {
			rule := residentRule{aboveContext: true, heldAtContext: sglang}
			raw, rawBound := priceResidentReading(s, r, rule)
			require.GreaterOrEqual(t, raw.KVreq, s.KVreq, "case %d (sglang %v, %s): the rule under the discount", i, sglang, rawBound)
			got, bound := withResidentKV(s, r, rule)
			require.GreaterOrEqual(t, got.KVreq, s.KVreq, "case %d (sglang %v, %s): under the discount", i, sglang, bound)
			if sglang {
				require.LessOrEqual(t, got.KVreq, s.Context()*(1+1e-12), "case %d (%s): SGLang over the context", i, bound)
			}
			got.KVreq = s.KVreq
			require.Equal(t, s, got, "case %d: only KVreq moves", i)
		}
	}
}

// A reading below the discount keeps the gate at the discount, on or off:

// A reading below the discount keeps the gate at the discount, on or off:
// 2000 tokens over 10 requests, 200 each, reads nothing at a gate of 28.
func TestKVAboveContext_ALowReadingKeepsTheGate(t *testing.T) {
	r := residentReading{tokens: 2000, running: 10, capTokens: 100_000, minK: 0.15}
	s := shape.New(1000, 100, 0.5)
	_, bound := withResidentKV(s, r, residentRule{aboveContext: true})
	assert.Equal(t, residentBoundNoReading, bound)
}

// Between the context and twice it the reading is priced as read and the
// bound is none: 60,000 over 40, 1500 each. Below the discount it is held at
// the discount, as before: 20,000 over 50, 400 against 550.
func TestKVAboveContext_TheFloorStaysAndANearContextReadingIsPlain(t *testing.T) {
	near := hitMu(t, cachingRow(0.6, 40), aboveOn(t))
	assert.InDelta(t, 1500, near["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundNone, near["residentKVBound"])

	low := hitMu(t, cachingRow(0.2, 50), aboveOn(t))
	assert.InDelta(t, 550, low["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundLower, low["residentKVBound"])
}

// SGLang reads the busiest rank against the pod's whole capacity, so above
// the context its reading is held at the context, switch on or off.
func TestKVAboveContext_SGLangIsHeldAtTheContext(t *testing.T) {
	sg := hitMu(t, cachingRow(0.9, 30), aboveOnSGLang(t))
	assert.InDelta(t, 1050, sg["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundUpper, sg["residentKVBound"])
}

// A shape with no prefix-cache hit takes a reading too: its discount is its
// context, and the engine's own KV is above it. Off, a no-hit shape is not
// read at all.
func TestKVAboveContext_ANoHitShapeIsReadAboveItsContext(t *testing.T) {
	noHit := cachingRow(0.9, 30)
	noHit.PrefixCacheHitRate = 0
	on := hitMu(t, noHit, aboveOn(t))
	assert.InDelta(t, 3000, on["kvReqPerSeq"], 1e-9)
	assert.Equal(t, "above-context", on["residentKVBound"])

	below := cachingRow(0.5, 50)
	below.PrefixCacheHitRate = 0
	held := hitMu(t, below, aboveOn(t))
	assert.InDelta(t, 1050, held["kvReqPerSeq"], 1e-9, "1000 a request is below the context: held at it")
	assert.Equal(t, residentBoundLower, held["residentKVBound"])

	off := hitMu(t, noHit, aboveOff(t))
	assert.InDelta(t, 1050, off["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundNoHit, off["residentKVBound"])
}

// The switch needs SwitchKVPerRequestHitsResident: with that off, nothing is
// read, and the price is the shape's.
func TestKVAboveContext_NothingWithoutHitsResident(t *testing.T) {
	got := hitMu(t, cachingRow(0.9, 30), func(a *SaturationAnalyzer) {
		aboveOn(t)(a)
		hitsOff(a)
	})
	assert.InDelta(t, 550, got["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundOff, got["residentKVBound"])
}

// The controller prices a reading above the context as read: the switch is
// on in a new analyzer, and an analyzer with no replay switches set prices
// TestKVAboveContext_AReadingAboveTheContextIsPricedAsRead's 3000 tokens a
// request as read, not at the context of 1050.
func TestKVAboveContext_OnFromTheConstructor(t *testing.T) {
	fields := NewSaturationAnalyzer(capacity.NewStore()).switchFields()
	require.Contains(t, fields, "kv-per-request-above-context")
	assert.True(t, *fields["kv-per-request-above-context"])

	got := hitMu(t, cachingRow(0.9, 30), nil)
	assert.InDelta(t, 3000, got["kvReqPerSeq"], 1e-9, "priced as read, not at the context")
	assert.Equal(t, "above-context", got["residentKVBound"])
}
