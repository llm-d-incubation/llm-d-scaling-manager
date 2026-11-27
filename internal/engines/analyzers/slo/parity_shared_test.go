package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The planner's model rules that live in shared packages, so the controller
// can call them (issue llm-d-incubation/llm-d-scaling-manager#169). Each test
// calls the shared rule from this package, and holds the analyzer off it as
// the constructor builds it: no rule is used here yet.

// The floor below the band kept by sequence count (itl.BelowBandByCount,
// #457/#495) builds from this package, and the analyzer's decode leaves
// whatif.Decode.BelowBand unset, so no fill below the band is priced by it.
func TestParity_TheFloorByCountIsSharedAndUnused(t *testing.T) {
	obs := make([]itl.Observation, 0, 44)
	seqs := make([]float64, 0, 44)
	for range 22 {
		obs = append(obs, itl.Observation{K: 0.05, ITLSec: 0.010}, itl.Observation{K: 0.05, ITLSec: 0.020})
		seqs = append(seqs, 4, 40)
	}
	least, byCount := itl.BelowBandByCount(obs, seqs)
	assert.Equal(t, 4.0, least)
	assert.True(t, itl.ValidModel(byCount.A, byCount.B), "a line through the two floors: %+v", byCount)

	a := New(nil, nil)
	fleet := fleetReading{ready: map[string]int{"dec": 2}, rows: decodeRows("dec", 2)}
	fleet.shape.KVreq, fleet.shape.AvgOutputTokens = 1500, 1000
	d := pricingDetail{source: constants.SLOSourceOwn, band: whatif.DefaultBand()}
	d.line.Model, d.line.Tier = itl.Model{A: 0.02, B: 0.004}, itl.TierOLS
	got := a.ttftDecode(domain.AnalyzerInput{ModelID: "m", Namespace: "ns"}, fleet, []ttftDecodeLine{{state: decState, detail: d}})
	if assert.NotNil(t, got) {
		assert.Equal(t, whatif.BelowBand{}, got.BelowBand, "the controller builds no floor by count")
	}
}
