package learnreplay

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
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

	infinite := parityOutputRows(true, true)
	infinite[1].AvgOutputTokensSquared = math.Inf(1)
	assert.Equal(t, 0.0, spread(infinite), "an infinite bound is no reading of the spread, not an infinite spread")

	// An infinite rate keeps its row out of the weighted E[O^2] (fleet.Mean),
	// so the squared figure is the other row's 1.5e6, where main read NaN. The
	// row's lengths still read NaN, as on main: only the squared figure is pinned.
	fast := parityOutputRows(true, true)
	fast[0].RequestRate = math.Inf(1)
	shapes := map[string][]shapeSample{}
	noteShapes(shapes, fast, 1)
	require.Len(t, shapes["dec"], 1)
	assert.Equal(t, 1.5e6, shapes["dec"][0].squared, "an infinite rate is no weight in E[O^2], not a NaN")
}

// The planner's sequence count reads the draft tokens a step accepts by the
// analyzers' rule (domain.ReplicaMetrics.AcceptedPerStep) with no hold: 3.5
// accepted divides the tokens by 4.5, where the saturation analyzer's step
// budget, at k 3, reads 4 (saturation's
// TestParity_TheAcceptedPerStepIsOneRuleHeldAtK).
func TestParity_TheAcceptedPerStepIsOneRule(t *testing.T) {
	rm := domain.ReplicaMetrics{AvgITL: 0.02, AvgITLKnown: true, GenerationTokenRate: 900,
		SpecAcceptedPerStep: 3.5, SpecAcceptedPerStepKnown: true}
	require.Equal(t, domain.ITLFinite, rm.ITLState())
	accepted, ok := rm.AcceptedPerStep(0)
	require.True(t, ok)
	assert.Equal(t, 900*0.02/(1+accepted), rowSequences(rm), "the shared reading, byte for byte")
	assert.InDelta(t, 4.0, rowSequences(rm), 1e-12, "18 tokens a step-window over 4.5 a step")

	rm.SpecAcceptedPerStepKnown = false
	assert.InDelta(t, 18.0, rowSequences(rm), 1e-12, "no reading: one token a step")

	rm.SpecAcceptedPerStep, rm.SpecAcceptedPerStepKnown = math.Inf(1), true
	assert.InDelta(t, 18.0, rowSequences(rm), 1e-12, "an infinite reading is no reading: one token a step, not zero sequences")
}

// The planner's resident KV reads each row by the saturation analyzer's rule
// (fleet.ResidentRead, saturation's TestParity_TheResidentRowIsOneRule pins
// the same rows): 210,000 tokens over 100 running. What differs is kept on
// each side: the planner sums a variant's rows per cycle and takes the
// median of at least minResidentCycles cycles (residentOf), the analyzer
// sums the fleet's for the one cycle and adds the requests that hold KV
// without running on a split decode.
func TestParity_TheResidentRowIsOneRule(t *testing.T) {
	row := func(kv float64, capTokens int64, running int) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{VariantName: "dec", Ready: true, KvUsageInstant: kv, KvUsageInstantKnown: true,
			RunningRequests: running, RunningRequestsKnown: true, TotalKvCapacityTokens: capTokens}
	}
	notReady, lent := row(0.9, 100_000, 10), row(0.9, 100_000, 10)
	notReady.Ready, lent.FromWarmPool = false, true
	rows := []domain.ReplicaMetrics{row(0.5, 100_000, 50), row(0.3, 200_000, 20), row(1.0, 100_000, 30),
		row(0.1, 100_000, 10), notReady, lent}
	bottom, _ := (&config.ScalingPolicy{}).ITLBand()
	require.Equal(t, 0.15, bottom, "precondition: the band the saturation side reads at")
	var tokens float64
	var running int
	for _, rm := range rows {
		if tk, n, ok := fleet.ResidentRead(rm, bottom); ok {
			tokens, running = tokens+tk, running+n
		}
	}
	in := domain.AnalyzerInput{Config: &config.ScalingPolicy{}, ReplicaMetrics: rows,
		VariantStates: []domain.VariantReplicaState{{VariantName: "dec", Role: domain.RoleDecode}}}
	got := map[string][]residentSample{}
	noteResident(got, in, 3)
	require.Len(t, got["dec"], 1)
	assert.Equal(t, residentSample{cycle: 3, tokens: tokens, running: running}, got["dec"][0], "the shared reading, byte for byte")
	assert.InDelta(t, 210_000, got["dec"][0].tokens, 1e-6)
	assert.Equal(t, 100, got["dec"][0].running)
}
