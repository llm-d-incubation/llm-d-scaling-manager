package saturation

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// Issue #277: a prefix-cache hit saves KV only when another running request
// holds the block too, so where the running requests are read, the KV a
// resident request holds is measured, between ILeff + D*OL/2 and
// IL + D*OL/2; where they are not, it is ILeff + D*OL/2 as before.

// hitsOff turns SwitchKVPerRequestHitsResident off.
func hitsOff(a *SaturationAnalyzer) { a.kvHitsResident = false }

// hitRow is a Ready decode replica pod of variant-d, il in / ol out at a hit
// rate of 0.5, its cache capTokens tokens, holding kv of it now across
// running requests.
func hitRow(pod string, il, ol float64, capTokens int64, kv float64, running int) domain.ReplicaMetrics {
	rm := makeReplicaMetrics(pod, "variant-d", 5000, capTokens, 0, il, ol)
	rm.PrefixCacheHitRate = 0.5
	rm.KvUsageInstant, rm.KvUsageInstantKnown = kv, true
	rm.RunningRequests, rm.RunningRequestsKnown = running, true
	return rm
}

// cachingRow is hitRow at 1000 in / 100 out and a cache of 100,000 tokens.
// The shape prices a resident request at 500 + 50 = 550, and with no hit at
// 1000 + 50 = 1050. A reading needs 2 requests (minResidentSequences).
func cachingRow(kv float64, running int) domain.ReplicaMetrics {
	return hitRow("pod-1", 1000, 100, 100_000, kv, running)
}

// hitsMu runs one cycle of rows, decode replicas of variant-d under a fitted
// line and a declared cap of 1024 a rank (the KV, not the cap, binds), and
// returns the first derived-mu line.
func hitsMu(t *testing.T, rows []domain.ReplicaMetrics, edit func(*SaturationAnalyzer)) map[string]any {
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
	in := makeAnalyzerInput(rows, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H200",
			CurrentReplicas: len(rows), GPUsPerReplica: 1},
	})
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)
	fields := requireLogged(t, logs, "derived-mu")
	require.Equal(t, true, fields["ok"], "the line prices mu: %v", fields)
	return fields
}

// hitMu is hitsMu of one row.
func hitMu(t *testing.T, rm domain.ReplicaMetrics, edit func(*SaturationAnalyzer)) map[string]any {
	t.Helper()
	return hitsMu(t, []domain.ReplicaMetrics{rm}, edit)
}

// Fifty running requests hold 50,000 tokens, 1000 each: the hits were mostly
// on blocks no other running request holds. The derived mu fits 0.68 x
// 100,000 / 1000 = 68 sequences, where the hit rate's discount fitted 123.6
// -- a mu 1.8x too high, and that many too few replicas. Off, as before.
func TestKVHitsResident_TheMeasuredKVPricesTheDerivedMu(t *testing.T) {
	on := hitMu(t, cachingRow(0.5, 50), nil)
	assert.InDelta(t, 1000, on["kvReqPerSeq"], 1e-9)
	assert.InDelta(t, 1000, on["kvReqFleet"], 1e-9, "the GPS check prices the same request")
	assert.InDelta(t, 68, on["seqs"], 1e-9)
	assert.InDelta(t, 1000, on["residentKVPerSeq"], 1e-9)
	assert.EqualValues(t, 50, on["residentRunning"])
	assert.Equal(t, residentBoundNone, on["residentKVBound"])

	off := hitMu(t, cachingRow(0.5, 50), hitsOff)
	assert.InDelta(t, 550, off["kvReqPerSeq"], 1e-9)
	assert.InDelta(t, 550, off["kvReqFleet"], 1e-9)
	assert.InDelta(t, 68_000.0/550, off["seqs"], 1e-9)
	assert.Equal(t, residentBoundOff, off["residentKVBound"])
	assert.Less(t, on["rate"], off["rate"])
}

// The reading only says how much of the discount is real. Below the shape's
// KVreq (20,000 tokens over 50 requests, 400 each: requests part way through
// their prefill) it is the shape's 550, no more sharing than the hit rate
// allows. Above the shape with no hit (90,000 over 50, 1800 each) it is
// 1050. The log names the bound.
func TestKVHitsResident_TheReadingIsBoundedByTheHitRate(t *testing.T) {
	low := hitMu(t, cachingRow(0.2, 50), nil)
	assert.InDelta(t, 550, low["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundLower, low["residentKVBound"])
	high := hitMu(t, cachingRow(0.9, 50), nil)
	assert.InDelta(t, 1050, high["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundUpper, high["residentKVBound"])
	// Between the two it is the reading: 30,000 over 40, 750 each.
	assert.InDelta(t, 750, hitMu(t, cachingRow(0.3, 40), nil)["kvReqPerSeq"], 1e-9)

	// Off, the shape's discount, whatever was read.
	assert.InDelta(t, 550, hitMu(t, cachingRow(0.9, 50), hitsOff)["kvReqPerSeq"], 1e-9)
}

// No reading prices as before, at the shape's 550: a missing running-requests
// series, a missing KV series, and a fill below the ITL band are each no
// reading. The switch only moves the price where it has evidence.
func TestKVHitsResident_NoReadingPricesAsBefore(t *testing.T) {
	unknownRunning := cachingRow(0.5, 50)
	unknownRunning.RunningRequestsKnown = false
	unknownKV := cachingRow(0.5, 50)
	unknownKV.KvUsageInstantKnown = false
	for name, rm := range map[string]domain.ReplicaMetrics{
		"running unknown": unknownRunning, "kv unknown": unknownKV,
		"below the band": cachingRow(0.14, 20), // 700 a request, inside the bounds
	} {
		on := hitMu(t, rm, nil)
		assert.InDelta(t, 550, on["kvReqPerSeq"], 1e-9, name)
		assert.Equal(t, residentBoundNoReading, on["residentKVBound"], name)
	}
	// The band's count gate: a cache filled to 0.15 holds 0.15 x 100,000 /
	// 550 = 27.3 requests if the discount is right, so 27 are no reading and
	// 28 are one (15,000 over 28, 535.7: under the discount, priced at it).
	edge := hitMu(t, cachingRow(0.15, 27), nil)
	assert.Equal(t, residentBoundNoReading, edge["residentKVBound"])
	edge = hitMu(t, cachingRow(0.15, 28), nil)
	assert.Equal(t, residentBoundLower, edge["residentKVBound"])
	assert.InDelta(t, 550, edge["kvReqPerSeq"], 1e-9)
}

// Few long requests sharing a large prefix are no reading. A 500,000-token
// cache serves 100,000-token prompts, 90% of them a shared prefix, with
// 1000 out: the shape prices 10,000 + 500 = 10,500 a request. Two running
// hold 90,000 + 2 x 10,500 = 111,000 tokens, a fill of 0.22, inside the
// band, and read 55,500 a request: priced so, the KV-bound mu would be 5x
// low. The band's count gate asks for 0.15 x 500,000 / 10,500 = 7.1, so 8
// requests; 8 hold 174,000, and read 21,750.
func TestKVHitsResident_FewLongRequestsSharingAPrefixAreNoReading(t *testing.T) {
	row := func(running int) domain.ReplicaMetrics {
		rm := hitRow("pod-1", 100_000, 1000, 500_000, (90_000+float64(running)*10_500)/500_000, running)
		rm.PrefixCacheHitRate = 0.9
		return rm
	}
	two := hitMu(t, row(2), nil)
	assert.Equal(t, residentBoundNoReading, two["residentKVBound"])
	assert.InDelta(t, 10_500, two["kvReqPerSeq"], 1e-6, "the discount, not 55,500")
	eight := hitMu(t, row(8), nil)
	assert.Equal(t, residentBoundNone, eight["residentKVBound"])
	assert.InDelta(t, 21_750, eight["kvReqPerSeq"], 1e-6)
}

// The GPS check receives the KV per request the reading set. The replica
// generates 1000 tokens a second at a fill of 0.5: 50 requests of 1000 each,
// an ITL of 0.08 x 0.5 + 0.01 = 0.05 s. Priced at the measured 1000 the
// line predicts exactly that, and nothing is logged; priced at the
// discount's 550 it predicts 1818, 82% off, and itl-gps-mismatch fires.
func TestKVHitsResident_TheGPSCheckPricesTheMeasuredKV(t *testing.T) {
	mismatch := func(edit func(*SaturationAnalyzer)) int {
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
		rm := cachingRow(0.5, 50)
		rm.GenerationTokenRate, rm.GenerationTokenRateKnown = 1000, true
		_, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
			{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H200",
				CurrentReplicas: 1, GPUsPerReplica: 1},
		}))
		require.NoError(t, err)
		require.NotEmpty(t, logs.FilterMessage("derived-mu").All(), "the cycle priced the replica")
		return logs.FilterMessage("itl-gps-mismatch").Len()
	}
	assert.Zero(t, mismatch(nil), "the line predicts the observed rate at the measured KV")
	assert.Equal(t, 1, mismatch(hitsOff), "off, the discount over-predicts the rate")
}

// On a P/D fleet only decode is read. A prefill replica holds the KV of
// prompts it is handing to decode, 90,000 tokens over 10 running here; read,
// it would pull the decode reading of 1000 a request to 140,000 / 60 = 2333,
// past the upper bound.
func TestKVHitsResident_APrefillReplicaIsNotRead(t *testing.T) {
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		AcceleratorName: "H200", GpuCount: 1, EngineParams: declaredParams(1024),
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	a.publishLine(a.itlPhysicsKey("test-ns", "test-model", "variant-d", "H200", 1, ""),
		"a fitted line", itl.Model{A: 0.08, B: 0.01}, time.Now())
	prefill := hitRow("prefill-1", 1000, 1, 100_000, 0.9, 10)
	prefill.VariantName = "variant-p"
	_, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{cachingRow(0.5, 50), prefill},
		[]domain.VariantReplicaState{
			{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
			{VariantName: "variant-p", Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
		}))
	require.NoError(t, err)
	f := requireLogged(t, logs, "derived-mu")
	assert.EqualValues(t, 50, f["residentRunning"])
	assert.InDelta(t, 1000, f["kvReqPerSeq"], 1e-9)
}

// The reviewer's shared-prefix case. 500 prompt tokens are a system prompt
// every request shares and 500 are its own, with 100 out: the hit rate is
// 0.5 and, at the pricing point, the shape's 550 is right, because the shared
// blocks are held once. A replica running 123 requests holds 500 + 123 x 550
// = 68,150 tokens, 554.1 a request. Four replicas running 2 each hold 500 +
// 2 x 550 = 1600, 800 a request: the shared prefix spread over 2. Read at
// that load the KV-bound mu would be 31% low. Their fill, 0.016, is under
// the ITL band, so they are not read.
func TestKVHitsResident_ASharedPrefixIsReadInsideTheBandOnly(t *testing.T) {
	light := func(n int) []domain.ReplicaMetrics {
		out := make([]domain.ReplicaMetrics, 0, n)
		for i := range n {
			out = append(out, hitRow(fmt.Sprintf("light-%d", i), 1000, 100, 100_000, 0.016, 2))
		}
		return out
	}
	heavy := hitRow("heavy", 1000, 100, 100_000, 0.6815, 123)

	f := hitsMu(t, light(4), nil)
	assert.InDelta(t, 550, f["kvReqPerSeq"], 1e-9, "light load alone is no reading: the shape's 550, not 800")
	assert.Equal(t, residentBoundNoReading, f["residentKVBound"])

	f = hitsMu(t, append(light(4), heavy), nil)
	assert.InDelta(t, 68_150.0/123, f["kvReqPerSeq"], 1e-6, "the replica in the band alone: 554.1")
	assert.EqualValues(t, 123, f["residentRunning"])
}

// The fewest requests a reading is taken from follows the shape: the prompt
// carries no spread, so long prompts need few. At 1000 in / 1000 out (hit
// 0.5: 500 + 500 = 1000, and 1500 with no hit) it is 15. Fourteen requests
// holding 1200 each are no reading; fifteen are.
func TestKVHitsResident_TheFewestRequestsFollowTheShape(t *testing.T) {
	assert.Equal(t, 15, minResidentSequences(shape.New(1000, 1000, 0.5)))
	assert.Equal(t, 2, minResidentSequences(shape.New(1000, 100, 0.5)))
	assert.Equal(t, 2, minResidentSequences(shape.New(8000, 500, 0.9)))
	// A spread of output lengths widens the ages a request is caught at.
	assert.Greater(t, minResidentSequences(shape.New(1000, 1000, 0.5).WithOutputDispersion(2)), 15)
	assert.Equal(t, minResidentFloor, minResidentSequences(shape.Shape{}))

	few := hitMu(t, hitRow("pod-1", 1000, 1000, 20_000, 14*1200.0/20_000, 14), nil)
	assert.InDelta(t, 1000, few["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundNoReading, few["residentKVBound"])
	enough := hitMu(t, hitRow("pod-1", 1000, 1000, 20_000, 15*1200.0/20_000, 15), nil)
	assert.InDelta(t, 1200, enough["kvReqPerSeq"], 1e-6)
}

// A fleet with no hits prices as before, whatever is read: the two bounds
// are one figure.
func TestKVHitsResident_NoHitsPricesAsBefore(t *testing.T) {
	rm := cachingRow(0.9, 50)
	rm.PrefixCacheHitRate = 0
	on := hitMu(t, rm, nil)
	assert.InDelta(t, 1050, on["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundNoHit, on["residentKVBound"], "not upper: there is no discount to take back")
	assert.Equal(t, residentBoundNoHit, on["residentKVBoundFleet"])
	assert.InDelta(t, 1050, hitMu(t, rm, hitsOff)["kvReqPerSeq"], 1e-9)
}

// While a shape change is outstanding the reading is kept, held to the bounds
// of the shape priced: the short-window shape (2000 in, 100 out, hit 0.5)
// runs from 1050 to 2050, and 80,000 tokens over 50 requests read 1600, so
// mu is priced at 1600. Above the new shape's upper bound (95,000 over 40,
// 2375) it is 2050. Off, at the discount's 1050.
func TestKVHitsResident_AShapeChangeKeepsTheReadingInTheNewBounds(t *testing.T) {
	swap := func(kv float64, running int, edit func(*SaturationAnalyzer)) map[string]any {
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
		states := []domain.VariantReplicaState{{VariantName: "variant-d", Role: domain.RoleDecode,
			AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1}}
		row := func(out5m, out1m float64) []domain.ReplicaMetrics {
			rm := hitRow("pod-1", 2000, out5m, 100_000, kv, running)
			rm.AvgOutputTokensRecent, rm.AvgInputTokensRecent = out1m, 2000
			return []domain.ReplicaMetrics{rm}
		}
		ctx1, _ := observedCtx(t)
		_, err := a.Analyze(ctx1, makeAnalyzerInput(row(1000, 1000), states))
		require.NoError(t, err)
		ctx2, logs := observedCtx(t)
		_, err = a.Analyze(ctx2, makeAnalyzerInput(row(550, 100), states))
		require.NoError(t, err)
		return requireLogged(t, logs, "derived-mu")
	}
	on := swap(0.8, 50, nil)
	require.Equal(t, true, on["muShortWindow"], "the swap opened the short window")
	assert.InDelta(t, 1600, on["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundNone, on["residentKVBound"])
	// The fleet's own [5m] shape (2000 in, 550 out: 1275, and 2275 with no
	// hit) bounds the same reading by its own figures.
	assert.InDelta(t, 1600, on["kvReqFleet"], 1e-9)

	high := swap(0.95, 40, nil)
	require.Equal(t, true, high["muShortWindow"])
	assert.InDelta(t, 2050, high["kvReqPerSeq"], 1e-9)
	assert.Equal(t, residentBoundUpper, high["residentKVBound"])
	assert.InDelta(t, 2275, high["kvReqFleet"], 1e-9, "the fleet's upper bound, not the new shape's")
	assert.Equal(t, residentBoundUpper, high["residentKVBoundFleet"])

	// 44,000 over 40, 1100: inside the new shape's bounds, under the
	// fleet's discount.
	low := swap(0.44, 40, nil)
	assert.InDelta(t, 1100, low["kvReqPerSeq"], 1e-9)
	assert.InDelta(t, 1275, low["kvReqFleet"], 1e-9)
	assert.Equal(t, residentBoundLower, low["residentKVBoundFleet"])

	off := swap(0.8, 50, hitsOff)
	require.Equal(t, true, off["muShortWindow"])
	assert.InDelta(t, 1050, off["kvReqPerSeq"], 1e-9)
}

// The fleet reading is the tokens in use and the running requests, summed
// over the Ready generating replicas that read both inside the band. A
// prefill replica, a warm-pool bridge, a replica not Ready, one missing
// either reading, one whose fill is not a fraction, one whose capacity is
// unread or the overflow sentinel, one under the band, and one with no
// running request are each left out.
func TestKVHitsResident_TheFleetReadingCountsReadyGeneratingReplicas(t *testing.T) {
	roles := map[string]string{"variant-d": domain.RoleDecode, "variant-p": domain.RolePrefill}
	row := func(variant string, kv float64, capTokens int64, running int) domain.ReplicaMetrics {
		rm := makeReplicaMetrics("pod", variant, 0, capTokens, 0, 1000, 100)
		rm.KvUsageInstant, rm.KvUsageInstantKnown = kv, true
		rm.RunningRequests, rm.RunningRequestsKnown = running, true
		return rm
	}
	counted := []domain.ReplicaMetrics{
		row("variant-d", 0.5, 100_000, 50), // 50,000 over 50
		row("variant-d", 0.3, 200_000, 20), // 60,000 over 20
		row("variant-d", 1.0, 100_000, 30), // a full cache is a fill
	}
	got := fleetResidentKVPerRequest(counted, roles, 0.15)
	assert.InDelta(t, 210_000, got.tokens, 1e-6)
	assert.Equal(t, 100, got.running)
	assert.InDelta(t, 400_000, got.capTokens, 1e-6)
	assert.InDelta(t, 0.15, got.minK, 1e-12)

	notReady := row("variant-d", 0.9, 100_000, 10)
	notReady.Ready = false
	bridge := row("variant-d", 0.9, 100_000, 10)
	bridge.FromWarmPool = true
	noRunning := row("variant-d", 0.9, 100_000, 10)
	noRunning.RunningRequestsKnown = false
	noKV := row("variant-d", 0.9, 100_000, 10)
	noKV.KvUsageInstantKnown = false
	leftOut := []domain.ReplicaMetrics{
		row("variant-p", 0.9, 100_000, 10),
		notReady, bridge, noRunning, noKV,
		row("variant-d", math.NaN(), 100_000, 10),
		row("variant-d", 0.9, 0, 10),
		row("variant-d", 0.9, math.MaxInt64, 10),
		row("variant-d", 0.1, 100_000, 10),
		row("variant-d", 1.5, 100_000, 10),
		row("variant-d", 0.9, 100_000, 0),
	}
	got = fleetResidentKVPerRequest(append(leftOut, counted...), roles, 0.15)
	assert.InDelta(t, 210_000, got.tokens, 1e-6, "the rows left out move nothing")
	assert.Equal(t, 100, got.running)
	assert.InDelta(t, 400_000, got.capTokens, 1e-6)

	assert.Equal(t, residentReading{minK: 0.15}, fleetResidentKVPerRequest(leftOut, roles, 0.15), "no row counted is no reading")
	assert.Zero(t, residentReading{}.perSeq())
}

// withResidentKV moves KVreq alone, keeps the spread, and leaves a shape with
// no KV per request, or no reading, as it is.
func TestKVHitsResident_ResidentKVMovesOnlyKVreq(t *testing.T) {
	s := shape.New(1000, 100, 0.5).WithOutputDispersion(2) // 600, 1100 with no hit
	reading := func(perSeq float64) residentReading { return residentReading{tokens: perSeq * 10, running: 10} }

	got, bound := withResidentKV(s, residentReading{})
	assert.InDelta(t, 600, got.KVreq, 1e-9, "no reading: the shape's 500 + 2 x 100/2")
	assert.Equal(t, residentBoundNoReading, bound)
	got, bound = withResidentKV(s, reading(700))
	assert.InDelta(t, 700, got.KVreq, 1e-9)
	assert.Equal(t, residentBoundNone, bound)
	assert.InDelta(t, 500, got.ILeff, 1e-9, "ILeff stays the discounted prompt")
	assert.InDelta(t, 2, got.OutputDispersion, 1e-12)
	got, _ = withResidentKV(s, reading(5000))
	assert.InDelta(t, 1100, got.KVreq, 1e-9, "1000 + 2 x 100/2")
	got, _ = withResidentKV(s, reading(100))
	assert.InDelta(t, 600, got.KVreq, 1e-9)

	// A literal's dispersion of 0 reads as 1.
	got, _ = withResidentKV(shape.Shape{AvgInputTokens: 1000, AvgOutputTokens: 100, PrefixHitRate: 0.5, KVreq: 550}, reading(5000))
	assert.InDelta(t, 1050, got.KVreq, 1e-9)
	got, bound = withResidentKV(shape.Shape{}, reading(700))
	assert.Equal(t, shape.Shape{}, got)
	assert.Equal(t, residentBoundNoReading, bound)
}

// The replay hook reaches the switch: off through SetSwitchesForReplay the
// derived mu prices the discount's 550; on again, the measured 1000.
func TestKVHitsResident_TheReplayHookSetsTheSwitch(t *testing.T) {
	set := func(on bool) func(*SaturationAnalyzer) {
		return func(a *SaturationAnalyzer) {
			require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchKVPerRequestHitsResident: on}))
		}
	}
	assert.InDelta(t, 550, hitMu(t, cachingRow(0.5, 50), set(false))["kvReqPerSeq"], 1e-9)
	assert.InDelta(t, 1000, hitMu(t, cachingRow(0.5, 50), set(true))["kvReqPerSeq"], 1e-9)
}

// A shape whose figures are not finite asks for more requests than any fleet
// holds.
func TestKVHitsResident_AShapeThatIsNotFiniteIsNeverRead(t *testing.T) {
	assert.Equal(t, math.MaxInt32, minResidentSequences(shape.Shape{AvgInputTokens: 1000, AvgOutputTokens: math.Inf(1)}))
}

// Whatever the shape and the reading, the price is never under the
// discount nor over no discount, and only KVreq moves.
func TestKVHitsResident_TheReadingStaysInsideTheBounds(t *testing.T) {
	rng := rand.New(rand.NewPCG(277, 3))
	for i := range 2000 {
		s := shape.New(1+rng.Float64()*50_000, 1+rng.Float64()*5000, rng.Float64()).WithOutputDispersion(1 + 3*rng.Float64())
		r := residentReading{
			tokens: rng.Float64() * 2e6, running: rng.IntN(500), capTokens: rng.Float64() * 2e6, minK: 0.15 * rng.Float64(),
		}
		got, bound := withResidentKV(s, r)
		noHit := noHitShape(s)
		require.GreaterOrEqual(t, got.KVreq, s.KVreq, "case %d (%s): under the discount", i, bound)
		require.LessOrEqual(t, got.KVreq, noHit.KVreq*(1+1e-12), "case %d (%s): over no discount", i, bound)
		got.KVreq = s.KVreq
		require.Equal(t, s, got, "case %d: only KVreq moves", i)
	}
}

// The derived mu is never above main's: over random shapes and readings,
// priced through Analyze with the switch on and off.
func TestKVHitsResident_TheDerivedMuIsNeverAboveMain(t *testing.T) {
	rng := rand.New(rand.NewPCG(277, 4))
	for i := range 60 {
		capTokens := int64(50_000 + rng.IntN(950_000))
		rm := hitRow("pod-1", 50+rng.Float64()*20_000, 10+rng.Float64()*4000, capTokens, rng.Float64(), 1+rng.IntN(400))
		rm.PrefixCacheHitRate = rng.Float64()
		on, off := hitMu(t, rm, nil), hitMu(t, rm, hitsOff)
		require.LessOrEqual(t, on["rate"], off["rate"].(float64)*(1+1e-12),
			"case %d: bound %v, kvReqPerSeq %v against %v", i, on["residentKVBound"], on["kvReqPerSeq"], off["kvReqPerSeq"])
	}
}
