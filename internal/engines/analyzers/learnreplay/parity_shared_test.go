package learnreplay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/fleet"
)

// The quantities the planner's replay reads that the controller reads too,
// read by one shared rule (issue llm-d-incubation/llm-d-scaling-manager#169,
// the parity audit's section 2.2). Each test pins the planner's figure on a
// fixture that the controller's side pins in its own package
// (saturation/parity_shared_test.go): a change to either side's call moves
// that side's figure alone, and fails its test.

// parityOutputRows is two decode rows of one variant, 9 and 1 requests a
// second, of 100 and 1000 output tokens with E[O^2] bounds of 20000 and
// 1.5e6: D 168000 / 190^2 at those weights. first and second say whether
// each row's rate is marked read.
func parityOutputRows(first, second bool) []domain.ReplicaMetrics {
	row := func(pod string, rate, out, squared float64, known bool) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: pod, VariantName: "dec", RequestRate: rate, RequestRateKnown: known,
			AvgInputTokens: 1000, AvgOutputTokens: out, AvgOutputTokensSquared: squared, AvgOutputTokensSquaredKnown: true}
	}
	return []domain.ReplicaMetrics{row("dec-0", 9, 100, 20000, first), row("dec-1", 1, 1000, 1.5e6, second)}
}

// The planner's spread of output lengths, D, is fleet.OutputMoments' reading
// of the cycle (the call the saturation, slo and throughput analyzers' D
// make), at the planner's weights: a rate counts only where the row marks it
// read. Where every rate is read the planner and the saturation analyzer
// read one D; where one is not, the planner leaves that row out and the
// analyzer counts it, and where none is, the planner weighs the rows alike
// and the analyzer by their rates: each side's rule kept.
func TestParity_TheOutputSpreadIsTheAnalyzersReading(t *testing.T) {
	const both = 168000.0 / (190 * 190)
	spread := func(rows []domain.ReplicaMetrics) float64 {
		shapes := map[string][]shapeSample{}
		noteShapes(shapes, rows, 1)
		require.Len(t, shapes["dec"], 1)
		return shapeOf(shapes["dec"]).OutputDispersion
	}
	rows := parityOutputRows(true, true)
	known := func(rm domain.ReplicaMetrics) float64 {
		if rm.RequestRateKnown {
			return rm.RequestRate
		}
		return 0
	}
	shared, ok := fleet.OutputDispersion(rows, func(rm domain.ReplicaMetrics) bool { return rm.AvgOutputTokens > 0 },
		fleet.WeightedBy(known))
	require.True(t, ok)
	assert.Equal(t, shared, spread(rows), "the shared reading, byte for byte")
	assert.InDelta(t, both, spread(rows), 1e-12, "saturation's fleetOutputDispersion pins the same figure")

	assert.InDelta(t, 2.0, spread(parityOutputRows(true, false)), 1e-12,
		"the planner's weights: the row whose rate is not read is left out (saturation reads 168000/190^2)")
	assert.InDelta(t, 760000.0/(550*550), spread(parityOutputRows(false, false)), 1e-12,
		"no rate read: the rows alike (saturation reads 168000/190^2)")

	unread := parityOutputRows(true, true)
	unread[1].AvgOutputTokensSquaredKnown = false
	assert.Equal(t, 0.0, spread(unread), "a row with no bound: the cycle has no reading of its spread")
}
