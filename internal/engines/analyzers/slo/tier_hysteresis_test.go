package slo

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// tierReadings is one cycle's rows: twelve pods near k 0.55, ITL scale times
// testLine's, and with wide two pods that span the set value's spread.
func tierReadings(wide bool, scale float64) []domain.ReplicaMetrics {
	var rows []domain.ReplicaMetrics
	for i := range 12 {
		r := row("agg", fmt.Sprintf("agg-%d", i), 0.50+0.01*float64(i))
		r.AvgITL *= scale
		rows = append(rows, r)
	}
	if wide {
		rows = append(rows, row("agg", "agg-lo", 0.25), row("agg", "agg-hi", 0.75))
	}
	return rows
}

// tierHeldOnEngine walks the key to the full fit, lets the spread collapse, then
// offers readings 3 % over the line for two cycles, and says whether the dwell
// refused the pinned fit. maxSeqs is the engine's stated cap.
func tierHeldOnEngine(t *testing.T, maxSeqs func(kvPerRequest float64) int64) itl.Reading {
	t.Helper()
	clock := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	const target = 1.0
	in := func(rows []domain.ReplicaMetrics) domain.AnalyzerInput {
		return domain.AnalyzerInput{Namespace: "ns", ModelID: "m", Config: policy(time.Second, 0),
			VariantStates: []domain.VariantReplicaState{batchCapAgg}, ReplicaMetrics: rows}
	}
	kv := readFleet(in(tierReadings(true, 1))).shape.KVreq
	require.Positive(t, kv, "the rows state no KV per request")
	a := batchCapAnalyzer(capacity.EngineParams{MaxNumSeqs: maxSeqs(kv)}, &clock, nil)
	step := func(rows []domain.ReplicaMetrics) itl.Reading {
		i := in(rows)
		_, _, d := a.priceVariant(i, batchCapAgg, windowKey("ns", "m", batchCapAgg), readFleet(i), target, 0.85, clock)
		clock = clock.Add(15 * time.Second)
		return d.line
	}
	var r itl.Reading
	for range 12 {
		r = step(tierReadings(true, 1))
	}
	require.Equal(t, itl.TierOLS, r.Tier, "the key never took the full fit")
	for range 5 {
		r = step(tierReadings(false, 1))
	}
	require.Less(t, r.SteadySpread, itl.DefaultClearKSpread, "the spread never collapsed")
	step(tierReadings(false, 1.03))
	return step(tierReadings(false, 1.03))
}

// The batch cap reaches the latch's price. Pinned on the same B, a line 3 % over
// at k 0.55 is 3.5 % over at the band's top and 2 % over at k 0.25. Its price
// is what slo sells, the ITL at the fill the cap lets the engine reach, so with
// the cap binding at 0.25 the change is under the margin, and with no cap
// binding, at the top, it is over.
func TestTierPrice_TheBatchCapReachesTheLatch(t *testing.T) {
	bound := tierHeldOnEngine(t, func(kv float64) int64 { return int64(0.25 * 100000 / kv) })
	assert.True(t, bound.TierHeld, "cap binding at 0.25: tier %q, want the pinned fit refused", bound.Tier)

	free := tierHeldOnEngine(t, func(float64) int64 { return 1 << 20 })
	assert.False(t, free.TierHeld, "no cap binding: tier %q, want the pinned fit through", free.Tier)
	assert.Equal(t, itl.TierPinnedB, free.Tier)
	assert.False(t, free.SlopeHeld, "no cap binding: the slope was held")
}
