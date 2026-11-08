package slo

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// Issue #420: the analyzer reads the cap the llm-d.ai/max-num-seqs
// annotation declares where the engine's flags state none, for the line's
// admission at the cap and for the cap a replica is priced at.

var batchCapAgg = domain.VariantReplicaState{VariantName: "agg", Role: domain.RoleBoth,
	AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: 2}

// declared is an engine whose flags state no cap (max_num_seqs the parser's
// default of 256), and whose scale target declares 64.
func declared() capacity.EngineParams {
	return capacity.EngineParams{MaxNumSeqs: 256, DeclaredBatchCap: 64}
}

// batchCapAnalyzer is an analyzer over a store holding params for agg, on
// a clock the caller advances, with edit applied.
func batchCapAnalyzer(params capacity.EngineParams, clock *time.Time, edit func(*Analyzer)) *Analyzer {
	s := capacity.NewStore()
	s.Update("ns", "m", "agg", capacity.Record{EngineParams: &params}, time.Now())
	a := New(nil, s)
	a.now = func() time.Time { return *clock }
	if edit != nil {
		edit(a)
	}
	return a
}

// cappedLine prices agg with one replica at 64 running, 768 waiting, at a
// fill above the band, and returns what its line read.
func cappedLine(t *testing.T, params capacity.EngineParams, edit func(*Analyzer)) itl.Reading {
	t.Helper()
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	a := batchCapAnalyzer(params, &clock, edit)
	r := row("agg", "agg-hot", 0.87)
	r.RunningRequests, r.QueueLength, r.PreemptionRateKnown = 64, 768, true
	in := domain.AnalyzerInput{Namespace: "ns", ModelID: "m", Config: policy(44*time.Millisecond, 0),
		VariantStates: []domain.VariantReplicaState{batchCapAgg}, ReplicaMetrics: []domain.ReplicaMetrics{r}}
	_, _, d := a.priceVariant(in, batchCapAgg, windowKey("ns", "m", batchCapAgg), readFleet(in), 0.044, 0.85, clock)
	return d.line
}

// A replica at the cap its scale target declares teaches the line from
// above the band, as one at the cap its flags state does (#287). Before
// #420 the reading counted as above the band.
func TestBatchCap_TheLineTakesAReplicaAtItsDeclaredCap(t *testing.T) {
	got := cappedLine(t, declared(), nil)
	assert.Equal(t, 1, got.Capped)
	assert.Zero(t, got.AboveBand)
}

// The switch off reads the flags' cap alone, as before #420.
func TestBatchCap_TheAdmissionSwitchPicksTheSource(t *testing.T) {
	got := cappedLine(t, declared(), func(a *Analyzer) { a.batchCapAdmission = false })
	assert.Zero(t, got.Capped)
	assert.Equal(t, 1, got.AboveBand)
}

// pricedAt warms agg's line over five cycles and returns one replica's
// capacity.
func pricedAt(t *testing.T, params capacity.EngineParams, edit func(*Analyzer)) float64 {
	t.Helper()
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	a := batchCapAnalyzer(params, &clock, edit)
	var res *domain.AnalyzerResult
	for i := range 5 {
		var err error
		res, err = a.Analyze(context.Background(), domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns", Config: policy(44*time.Millisecond, 0), ArrivalRate: 2,
			VariantStates: []domain.VariantReplicaState{batchCapAgg}, ReplicaMetrics: cycleRows("agg", i),
		})
		require.NoError(t, err)
		clock = clock.Add(15 * time.Second)
	}
	require.Len(t, res.VariantCapacities, 1)
	return res.VariantCapacities[0].PerReplicaCapacity
}

// A replica is priced at the cap its scale target declares. k = 0.6 implies
// 120 sequences; the engine admits 64, so the cache never fills past
// 64 * 500 / 100000 = 0.32, and 64 sequences advance at ITL(0.32). Before
// #420 the default of 256 bounded it, and the target alone priced it at
// 120 sequences: 1.6x what the engine runs.
func TestBatchCap_AReplicaIsPricedAtItsDeclaredCap(t *testing.T) {
	assert.InDelta(t, 64/testLine.ITLAt(0.32), pricedAt(t, declared(), nil), 1e-6)
}

// The switch off prices at the flags' cap or the default, as before #420.
func TestBatchCap_ThePerRankSwitchPicksTheSource(t *testing.T) {
	off := func(a *Analyzer) { a.batchCapPerRank = false }
	assert.InDelta(t, 120/0.044, pricedAt(t, declared(), off), 1e-6)
	// The flags' cap is read either way.
	stated := capacity.EngineParams{MaxNumSeqs: 64, MaxNumSeqsSet: true, DeclaredBatchCap: 128}
	assert.InDelta(t, 64/testLine.ITLAt(0.32), pricedAt(t, stated, off), 1e-6)
	assert.InDelta(t, 64/testLine.ITLAt(0.32), pricedAt(t, stated, nil), 1e-6, "the flags win over the annotation")
}

// pricedDetail warms agg's line as pricedAt does, then prices it once more
// and returns what the replica was priced from.
func pricedDetail(t *testing.T, params capacity.EngineParams, edit func(*Analyzer)) pricingDetail {
	t.Helper()
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	a := batchCapAnalyzer(params, &clock, edit)
	var in domain.AnalyzerInput
	for i := range 5 {
		in = domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns", Config: policy(44*time.Millisecond, 0), ArrivalRate: 2,
			VariantStates: []domain.VariantReplicaState{batchCapAgg}, ReplicaMetrics: cycleRows("agg", i),
		}
		_, err := a.Analyze(context.Background(), in)
		require.NoError(t, err)
		clock = clock.Add(15 * time.Second)
	}
	_, _, d := a.priceVariant(in, batchCapAgg, windowKey("ns", "m", batchCapAgg), readFleet(in), 0.044, 0.85, clock)
	return d
}

// The slo-variant line names the cap per rank a replica was priced at and
// where it came from. maxNumSeqs is max_num_seqs alone, the parser's 256
// where the flags are unread, so it cannot say a declared cap priced it.
func TestBatchCap_TheLogNamesThePricedCap(t *testing.T) {
	d := pricedDetail(t, declared(), nil)
	assert.EqualValues(t, 64, d.batchCap)
	assert.Equal(t, batchCapDeclared, d.batchCapSource)
	assert.EqualValues(t, 256, d.maxSeqs, "max_num_seqs is still the default")

	d = pricedDetail(t, capacity.EngineParams{MaxNumSeqs: 32, MaxNumSeqsSet: true, DeclaredBatchCap: 64}, nil)
	assert.EqualValues(t, 32, d.batchCap)
	assert.Equal(t, batchCapFlags, d.batchCapSource)

	d = pricedDetail(t, declared(), func(a *Analyzer) { a.batchCapPerRank = false })
	assert.EqualValues(t, 256, d.batchCap)
	assert.Equal(t, batchCapDefault, d.batchCapSource)
}

// pricedAtKV is pricedAt with every row's KV capacity set to kvTokens.
func pricedAtKV(t *testing.T, params capacity.EngineParams, kvTokens int64) float64 {
	t.Helper()
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	a := batchCapAnalyzer(params, &clock, nil)
	var res *domain.AnalyzerResult
	for i := range 5 {
		rows := cycleRows("agg", i)
		for j := range rows {
			rows[j].TotalKvCapacityTokens = kvTokens
		}
		var err error
		res, err = a.Analyze(context.Background(), domain.AnalyzerInput{
			ModelID: "m", Namespace: "ns", Config: policy(44*time.Millisecond, 0), ArrivalRate: 2,
			VariantStates: []domain.VariantReplicaState{batchCapAgg}, ReplicaMetrics: rows,
		})
		require.NoError(t, err)
		clock = clock.Add(15 * time.Second)
	}
	require.Len(t, res.VariantCapacities, 1)
	return res.VariantCapacities[0].PerReplicaCapacity
}

// A declared cap above the parser's 256 raises the price: with 300000 KV
// tokens, k = 0.6 implies 360 sequences, which the default of 256 caps and
// a declared 512 does not. So a fleet whose cap is declared above 256 runs
// fewer replicas than one whose flags are unread -- right when the
// declaration is true, and priced exactly as the same cap stated in flags.
func TestBatchCap_ADeclaredCapAbove256RaisesThePrice(t *testing.T) {
	const kv = 300_000
	unread := pricedAtKV(t, capacity.EngineParams{MaxNumSeqs: 256}, kv)
	declared := pricedAtKV(t, capacity.EngineParams{MaxNumSeqs: 256, DeclaredBatchCap: 512}, kv)
	stated := pricedAtKV(t, capacity.EngineParams{MaxNumSeqs: 512, MaxNumSeqsSet: true}, kv)
	assert.Greater(t, declared, unread*1.05, "the default of 256 binds; the declared 512 does not")
	assert.InDelta(t, stated, declared, 1e-6, "declared as if stated in the flags")
}
