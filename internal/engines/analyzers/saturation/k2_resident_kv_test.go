package saturation

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// Issue llm-d-incubation/llm-d-scaling-manager#201, the follow-up to PR #205:
// the derived k2 priced each resident request at its context while the
// engine was read to hold about three times that. The fixture: one decode
// pod of variant-d, 1000 in and 100 out at a prefix hit of 0.5 (context
// 1050), a cache of 1,000,000 tokens, --max-num-seqs 256 and a budget of
// 8192, which does not bind (8192 x 100 / 1100 = 745 > 256). At the
// context the derived k2 is 256 x 1050 = 268,800; k1 is 800,000.

const (
	residentCap     = int64(1_000_000)
	residentAtCtx   = 256 * 1050.0
	residentK1      = 800_000.0
	residentRunning = 100
)

// residentRow is the pod holding fill of its cache over residentRunning
// requests: fill x 10,000 tokens a request.
func residentRow(fill float64) domain.ReplicaMetrics {
	return hitRow("pod-1", 1000, 100, residentCap, fill, residentRunning)
}

// residentSwitches is which of the switches a run turns on: #199's and this
// one's.
type residentSwitches struct{ aboveContext, atResident bool }

// residentRun runs three cycles of rows, decode replicas of the variants
// named, edits applied to the analyzer first, and returns each variant's
// capacity, the last P3 k2-decision line (nil with none), and the capacity
// store.
func residentRun(t *testing.T, rows []domain.ReplicaMetrics, variants []string, sw residentSwitches,
	edits ...func(*SaturationAnalyzer)) (map[string]domain.VariantCapacity, map[string]any, *capacity.Store) {
	t.Helper()
	ctx, logs := observedCtx(t)
	store := capacity.NewStore()
	states := make([]domain.VariantReplicaState, 0, len(variants))
	for _, v := range variants {
		store.Update("test-ns", "test-model", v, capacity.Record{
			AcceleratorName: "H200", GpuCount: 1, EngineParams: deploymentParams(),
		}, time.Now())
		states = append(states, domain.VariantReplicaState{VariantName: v, Role: domain.RoleDecode,
			AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1})
	}
	a := NewSaturationAnalyzer(store)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{
		"kv-per-request-above-context": sw.aboveContext,
		"k2-derived-at-resident-kv":    sw.atResident,
	}))
	clock := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	for _, edit := range edits {
		edit(a)
	}
	in := makeAnalyzerInput(rows, states)
	var res *domain.AnalyzerResult
	for range 3 {
		var err error
		res, err = a.Analyze(ctx, in)
		require.NoError(t, err)
		clock = clock.Add(15 * time.Second)
	}
	var p3 map[string]any
	for _, e := range logs.FilterMessage("k2-decision").All() {
		if f := e.ContextMap(); f["priority"] == capacity.K2SrcDerived.String() {
			p3 = f
		}
	}
	out := map[string]domain.VariantCapacity{}
	for _, vc := range res.VariantCapacities {
		out[vc.VariantName] = vc
	}
	return out, p3, store
}

// residentK2 runs one pod of variant-d, and returns its per-replica
// capacity, its reason, and the last P3 k2-decision line.
func residentK2(t *testing.T, rm domain.ReplicaMetrics, sw residentSwitches) (float64, string, map[string]any) {
	t.Helper()
	out, p3, _ := residentRun(t, []domain.ReplicaMetrics{rm}, []string{"variant-d"}, sw)
	require.NotNil(t, p3, "a P3 k2-decision line")
	vc, ok := out["variant-d"]
	require.True(t, ok, "no decode capacity")
	return vc.PerReplicaCapacity, vc.Reason, p3
}

// 100 requests hold 300,000 tokens, 3000 each: 2.9x the context, priced as
// read by #199's switch. With both switches on the derived k2 prices each at
// 3000, 256 x 3000 = 768,000, under k1. With this switch alone it is the
// context's 268,800: the reading is held at the context without #199's, and
// trusted above it only behind #199's gates. With #199's alone, mu moves and
// k2 does not.
func TestK2DerivedAtResidentKV_PricesTheReadingAboveTheContext(t *testing.T) {
	rm := residentRow(0.3)
	got, why, line := residentK2(t, rm, residentSwitches{aboveContext: true, atResident: true})
	require.True(t, strings.HasPrefix(why, "P3"), "the derived k2 prices it: %s", why)
	assert.Equal(t, 768_000.0, got)
	assert.InDelta(t, 3000, line["residentKVPerRequest"], 1e-9)
	assert.EqualValues(t, int64(residentAtCtx), line["k2AtContext"])

	for _, sw := range []residentSwitches{{}, {aboveContext: true}, {atResident: true}} {
		got, why, line := residentK2(t, rm, sw)
		require.True(t, strings.HasPrefix(why, "P3"), "%+v: %s", sw, why)
		assert.Equal(t, residentAtCtx, got, "%+v: priced at the context", sw)
		assert.NotContains(t, line, "residentKVPerRequest", "%+v: the line as before", sw)
		assert.NotContains(t, line, "k2AtContext", "%+v", sw)
	}
}

// The raised figure prices the cycle only: the capacity store keeps the
// derived k2 at the context, which the zero-replica estimate, a sibling's
// lookup and the fallback path read with no reading behind them.
func TestK2DerivedAtResidentKV_TheStoreKeepsTheContext(t *testing.T) {
	out, _, store := residentRun(t, []domain.ReplicaMetrics{residentRow(0.3)}, []string{"variant-d"},
		residentSwitches{aboveContext: true, atResident: true})
	require.Equal(t, 768_000.0, out["variant-d"].PerReplicaCapacity, "premise: the reading prices the cycle")
	rec := store.Get("test-ns", "test-model", "variant-d")
	require.NotNil(t, rec)
	assert.Equal(t, int64(residentAtCtx), rec.EffectiveCapacity)
}

// A sibling's k2 window at 500,000 sits between the figure at the context
// (268,800) and the one at the reading (768,000). The lower bound is judged
// against the context, as with the switch off: the sibling is above it, no
// bound arms, and the reading prices the replica. Judged against the raised
// figure, the sibling would have armed a bound the context never would.
func TestK2DerivedAtResidentKV_TheLowerBoundIsJudgedAtTheContext(t *testing.T) {
	sw := residentSwitches{aboveContext: true, atResident: true}
	_, p3, _ := residentRun(t, []domain.ReplicaMetrics{residentRow(0.3)}, []string{"variant-d"}, sw)
	own, ok := p3["historyKey"].(string)
	require.True(t, ok && strings.Contains(own, "variant-d"), "the replica's k2 key: %v", p3["historyKey"])
	donor := strings.Replace(own, "variant-d", "variant-s", 1)
	sibling := func(a *SaturationAnalyzer) {
		a.capacityStore.Update("test-ns", "test-model", "variant-s", capacity.Record{
			AcceleratorName: "H200", GpuCount: 1, EngineParams: deploymentParams(), TotalKvCapacityTokens: residentCap,
		}, time.Now())
		setDonor(a, donor, 500_000)
	}
	on, _, _ := residentRun(t, []domain.ReplicaMetrics{residentRow(0.3)}, []string{"variant-d"}, sw, sibling)
	assert.Equal(t, 768_000.0, on["variant-d"].PerReplicaCapacity)
	assert.True(t, strings.HasPrefix(on["variant-d"].Reason, "P3"), on["variant-d"].Reason)

	// Control: a sibling under the context's figure arms the bound on or
	// off, and the reading prices nothing.
	under := func(a *SaturationAnalyzer) { sibling(a); setDonor(a, donor, 200_000) }
	got, _, _ := residentRun(t, []domain.ReplicaMetrics{residentRow(0.3)}, []string{"variant-d"}, sw, under)
	off, _, _ := residentRun(t, []domain.ReplicaMetrics{residentRow(0.3)}, []string{"variant-d"},
		residentSwitches{aboveContext: true}, under)
	assert.Equal(t, off["variant-d"].PerReplicaCapacity, got["variant-d"].PerReplicaCapacity)
	assert.Less(t, got["variant-d"].PerReplicaCapacity, residentAtCtx)
}

// Two decode variants read together: variant-d's engine holds 3000 a
// request, variant-e's about its context (1067). The pooled reading, 1840 a
// request, is not either's, so neither is priced at it.
func TestK2DerivedAtResidentKV_APooledReadingOfTwoVariantsIsNotTaken(t *testing.T) {
	e := hitRow("pod-2", 1000, 100, residentCap, 0.16, 150)
	e.VariantName = "variant-e"
	rows := []domain.ReplicaMetrics{residentRow(0.3), e}
	on, _, _ := residentRun(t, rows, []string{"variant-d", "variant-e"}, residentSwitches{aboveContext: true, atResident: true})
	assert.Equal(t, residentAtCtx, on["variant-d"].PerReplicaCapacity)
	assert.Equal(t, residentAtCtx, on["variant-e"].PerReplicaCapacity)
}

// 100 requests hold 400,000 tokens, 4000 each: priced at the reading the
// derived k2 would be 1,024,000, over k1. It is held at k1, 800,000.
func TestK2DerivedAtResidentKV_HeldAtK1(t *testing.T) {
	got, why, line := residentK2(t, residentRow(0.4), residentSwitches{aboveContext: true, atResident: true})
	require.True(t, strings.HasPrefix(why, "P3"), why)
	assert.Equal(t, residentK1, got)
	assert.EqualValues(t, int64(residentK1), line["k2"])
	off, _, _ := residentK2(t, residentRow(0.4), residentSwitches{aboveContext: true})
	assert.Equal(t, residentAtCtx, off)
}

// 100 requests at a fill of 0.09 hold 900 each: a reading between the
// discount (550) and the context (1050), prefix sharing the derived k2 has
// never priced. It stays at the context with both switches on.
func TestK2DerivedAtResidentKV_ABelowContextReadingIsNotTaken(t *testing.T) {
	rm := hitRow("pod-1", 1000, 100, 100_000, 0.9, residentRunning)
	on, _, line := residentK2(t, rm, residentSwitches{aboveContext: true, atResident: true})
	off, _, _ := residentK2(t, rm, residentSwitches{aboveContext: true})
	assert.Equal(t, off, on)
	assert.NotContains(t, line, "residentKVPerRequest")
}

// k2ResidentKV's gate, each condition alone. The shape's context is 1050;
// the reading, 3000, is what withResidentKV priced.
func TestK2DerivedAtResidentKV_TheGate(t *testing.T) {
	read := shape.New(1000, 100, 0.5)
	read.KVreq = 3000
	all := func() *SaturationAnalyzer {
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.kvHitsResident, a.kvAboveContext, a.k2AtResidentKV = true, true, true
		return a
	}
	assert.Equal(t, residentKVLine{perRequest: 3000}, all().k2ResidentKV(read, residentBoundAboveContext, false, residentReading{variant: "variant-d"}, "variant-d"),
		"a ratio: all of it per request, none held once")
	assert.Equal(t, residentKVLine{perRequest: 3000}, all().k2ResidentKV(read, residentBoundNone, false, residentReading{variant: "variant-d"}, "variant-d"))

	for name, a := range map[string]*SaturationAnalyzer{
		"switch off":        func() *SaturationAnalyzer { a := all(); a.k2AtResidentKV = false; return a }(),
		"#199's off":        func() *SaturationAnalyzer { a := all(); a.kvAboveContext = false; return a }(),
		"hits-resident off": func() *SaturationAnalyzer { a := all(); a.kvHitsResident = false; return a }(),
	} {
		assert.Zero(t, a.k2ResidentKV(read, residentBoundAboveContext, false, residentReading{variant: "variant-d"}, "variant-d"), name)
	}
	assert.Zero(t, all().k2ResidentKV(read, residentBoundAboveContext, true, residentReading{variant: "variant-d"}, "variant-d"), "a shape change outstanding")
	for _, b := range []string{residentBoundUpper, residentBoundLower, residentBoundNoReading, residentBoundNoHit, residentBoundOff} {
		assert.Zero(t, all().k2ResidentKV(read, b, false, residentReading{variant: "variant-d"}, "variant-d"), b)
	}
	below := read
	below.KVreq = 900
	assert.Zero(t, all().k2ResidentKV(below, residentBoundNone, false, residentReading{variant: "variant-d"}, "variant-d"), "below the context")
	atCtx := read
	atCtx.KVreq = read.Context()
	assert.Zero(t, all().k2ResidentKV(atCtx, residentBoundNone, false, residentReading{variant: "variant-d"}, "variant-d"), "at the context")
	inf := read
	inf.KVreq = math.Inf(1)
	assert.Zero(t, all().k2ResidentKV(inf, residentBoundAboveContext, false, residentReading{variant: "variant-d"}, "variant-d"), "not finite")
	assert.Zero(t, all().k2ResidentKV(read, residentBoundAboveContext, false,
		residentReading{variant: "variant-d", mixed: true}, "variant-d"), "a reading of two variants")
	assert.Zero(t, all().k2ResidentKV(read, residentBoundAboveContext, false,
		residentReading{variant: "variant-e"}, "variant-d"), "another variant's reading")
}

// k2AtResidentKV prices the batch the budget switch prices, at the reading:
// on the #205 fixture (four ranks, B 256, S 64, MTP) with its budget on, the
// ratio to the context's figure is the reading over I + O/2. It is never
// below the context's figure and never above k1 unless that figure is.
func TestK2DerivedAtResidentKV_TheFigure(t *testing.T) {
	params := budgetParams()
	rm := budgetRow()
	bc := pricedBatchCap{perRank: 64, budget: decodeStepBudget(rm, domain.RoleDecode, params)}
	atCtx := estimateCapacityAtCapBudget(params, bc.perRank, budgetIn, budgetOut, 1, bc.budget) * budgetRanks
	require.Positive(t, atCtx)
	ctx := budgetIn + budgetOut/2
	k1 := int64(math.MaxInt64 / 2)

	bc.resident = residentKVLine{perRequest: 3 * ctx}
	got := k2AtResidentKV(params, bc, budgetIn, budgetOut, budgetRanks, k1, atCtx)
	assert.InDelta(t, 3, float64(got)/float64(atCtx), 1e-6, "three times the context, the same batch")

	assert.Equal(t, atCtx+1, k2AtResidentKV(params, bc, budgetIn, budgetOut, budgetRanks, atCtx+1, atCtx), "held at k1")
	assert.Equal(t, atCtx, k2AtResidentKV(params, bc, budgetIn, budgetOut, budgetRanks, atCtx/2, atCtx),
		"k1 under the context's figure: unchanged, min(k1, k2) takes it")
	bc.resident = residentKVLine{perRequest: ctx / 2}
	assert.Equal(t, atCtx, k2AtResidentKV(params, bc, budgetIn, budgetOut, budgetRanks, k1, atCtx), "never lower")
	bc.resident = residentKVLine{}
	assert.Equal(t, atCtx, k2AtResidentKV(params, bc, budgetIn, budgetOut, budgetRanks, k1, atCtx), "no reading")
	bc.resident = residentKVLine{held: -1, perRequest: 3 * ctx}
	assert.Equal(t, atCtx, k2AtResidentKV(params, bc, budgetIn, budgetOut, budgetRanks, k1, atCtx), "a negative held part")

	// A line with KV held once (issue #52's form): each rank adds it once,
	// whatever its batch.
	held := 50_000.0
	bc.resident = residentKVLine{held: held, perRequest: ctx}
	got = k2AtResidentKV(params, bc, budgetIn, budgetOut, budgetRanks, k1, atCtx)
	assert.InDelta(t, float64(atCtx)+budgetRanks*held, float64(got), 2*budgetRanks, "the context's figure plus the held part a rank")
}

// The switch is off from the constructor, and a replay names it.
func TestK2DerivedAtResidentKV_OffFromTheConstructor(t *testing.T) {
	fields := NewSaturationAnalyzer(capacity.NewStore()).switchFields()
	require.Contains(t, fields, "k2-derived-at-resident-kv")
	assert.False(t, *fields["k2-derived-at-resident-kv"])
	assert.Contains(t, ReplaySwitchNames(), SwitchK2DerivedAtResidentKV)
}
