package whatif

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// engineTPOTInput is a P/D model's cycle: two decode rows of variant "d"
// (one not serving), an unsplit row of variant "b", a decode row of
// variant "d2", and a prefill row, each with an engine TPOT histogram.
func engineTPOTInput() domain.AnalyzerInput {
	h := func(r float64) []domain.LatencyBucket {
		return []domain.LatencyBucket{{UpperBound: 0.05, Rate: r}, {UpperBound: math.Inf(1), Rate: 2 * r}}
	}
	return domain.AnalyzerInput{
		VariantStates: []domain.VariantReplicaState{{VariantName: "p", Role: "prefill"}, {VariantName: "d", Role: "decode"},
			{VariantName: "d2", Role: "decode"}, {VariantName: "b", Role: "both"}},
		ReplicaMetrics: []domain.ReplicaMetrics{
			{PodName: "p-0", VariantName: "p", Ready: true, TPOTBuckets: h(100)},
			{PodName: "d-0", VariantName: "d", Ready: true, TPOTBuckets: h(1)},
			{PodName: "d-1", VariantName: "d", Ready: false, TPOTBuckets: h(2)},
			{PodName: "d2-0", VariantName: "d2", Ready: true, TPOTBuckets: h(10)},
			{PodName: "b-0", VariantName: "b", Ready: true, TPOTBuckets: []domain.LatencyBucket{{UpperBound: 0.025, Rate: 5},
				{UpperBound: math.Inf(1), Rate: 6}}},
		},
	}
}

// The engines' TPOT histogram of a decode cycle is summed as a learner
// recording's latency sums it (learnreplay's latencyOf): every decode and
// unsplit row of the variant, serving or not, never a prefill row, each
// bound's rate added and the bounds sorted. The planner reads it from the
// recording's latency and the controller from this cycle, so the two must
// sum the same rows, or the TPOT fit's fold scores differ between them.
func TestDecodeCycleOf_EngineTPOTSumsTheRecordingsRows(t *testing.T) {
	at := time.Unix(1000, 0)
	in := engineTPOTInput()

	got := DecodeCycleOf(in, at, "d").EngineTPOT
	assert.Equal(t, []domain.LatencyBucket{{UpperBound: 0.05, Rate: 3}, {UpperBound: math.Inf(1), Rate: 6}}, got,
		"variant d: both its rows, the one not serving too")

	got = DecodeCycleOf(in, at, "").EngineTPOT
	assert.Equal(t, []domain.LatencyBucket{{UpperBound: 0.025, Rate: 5}, {UpperBound: 0.05, Rate: 13},
		{UpperBound: math.Inf(1), Rate: 32}}, got, "every decode and unsplit row, sorted by bound, no prefill row")

	in.ReplicaMetrics = in.ReplicaMetrics[:1]
	assert.Nil(t, DecodeCycleOf(in, at, "").EngineTPOT, "no decode row reads one")
}

// The step anchored at a cycle carries that cycle's engine histogram, so
// the joint bracket reads it: without it the step has the router's alone.
func TestRouterTPOTStepAt_CarriesTheAnchorsEngineTPOT(t *testing.T) {
	at := time.Unix(3600, 0)
	router := &domain.RouterLatency{WindowSeconds: 60, TPOTSumRate: 0.5, TPOTCountRate: 20,
		TPOTBuckets: []domain.LatencyBucket{{UpperBound: 0.01, Rate: 2}, {UpperBound: 0.04, Rate: 15}, {UpperBound: math.Inf(1), Rate: 20}}}
	decode := DecodeCycle{At: at, Output: map[float64]float64{16: 5, 128: 18, math.Inf(1): 20},
		EngineTPOT: []domain.LatencyBucket{{UpperBound: 0.025, Rate: 8}, {UpperBound: math.Inf(1), Rate: 20}}}
	c := FitCycle{At: at, Latency: true, Router: router, Decode: decode}

	st, ok := RouterTPOTStepAt([]FitCycle{c}, &c)
	require.True(t, ok)
	assert.Equal(t, []float64{0.025, math.Inf(1)}, st.EngineEdges)
	assert.Equal(t, []float64{8 * DefaultMeasureWindow, 20 * DefaultMeasureWindow}, st.EngineCum,
		"counted over the engines' own minute, not the router's window")

	c.Decode.EngineTPOT = nil
	st, ok = RouterTPOTStepAt([]FitCycle{c}, &c)
	require.True(t, ok)
	assert.Nil(t, st.EngineEdges)
}
