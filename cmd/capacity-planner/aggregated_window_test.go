package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// aggregatedReporter is contradictsReporter as one aggregated pool: no
// prefill variant, the decode variant serving both roles on dec replicas.
func aggregatedReporter(hot []int, rate float64, dec int) *reporter {
	r := contradictsReporter(hot, rate, 0, dec)
	r.st.Variants = r.st.Variants[1:]
	r.st.Variants[0].Role = ""
	delete(r.st.ServingReplicas, "p")
	r.pfVariant = ""
	return r
}

// withLengths sets the decode variant's lengths in the hot cycles.
func withLengths(r *reporter, il, ol float64) *reporter {
	s := r.st.Variants[len(r.st.Variants)-1].ShapeSeries
	for _, i := range cycles(10, 22) {
		s[i].InputTokens, s[i].OutputTokens = il, ol
	}
	return r
}

// An aggregated fleet (no prefill pods) is judged on its own knee in the
// window check (#357): the decode knee its line prices, which was fitted on
// pods that prefill too. NOT JUDGED where the pool served past it for 3
// minutes, judged otherwise.
//
// A lighter cycle counts its rate at the shape priced (1000/1000 here,
// KVreq 1500). The decode knee goes as 1/(OL*KVreq), and the prompt's KV is
// resident on the decode pods, P/D or not: at 820/1000 the scale is
// 1320/1500 = 0.88. One pool also prefills in its own steps, so it takes
// the prompt ratio, 0.82, where that is lower. 5.9 r/s there is 5.19 r/s at
// the shape priced on P/D, past 1.25x a knee of 4, and 4.84 on one pool,
// under it. At 850/850 the scale is 850*1275/(1000*1500) = 0.72 on both:
// 6.5 r/s is 4.7, under it, where the lower length ratio alone (0.85, 5.5)
// read it past.
func TestReport_AnAggregatedPoolIsJudgedOnItsOwnKnee(t *testing.T) {
	base := fleetEval{kneeOK: true, knee: 4, binds: roleDecodeWord, decodeKnee: whatif.Answer{Value: 4}, under: true, pastOperating: true}

	got := aggregatedReporter(cycles(10, 22), 12, 2).contradicted(0, 2, base)
	assert.Contains(t, got, "2 decode or fewer served above 1.25x the fleet knee of 4.0 r/s at the shape priced for 3 min")
	var v whatifapi.ReportVerdict
	require.True(t, aggregatedReporter(cycles(10, 22), 12, 2).notJudgedUnder(0, 2, base, &v), "served past its knee")
	assert.Equal(t, whatifapi.VerdictNotJudged, v.Verdict)
	assert.Contains(t, v.Summary, contradictsWords)

	assert.False(t, aggregatedReporter(cycles(10, 22), 4.5, 2).notJudgedUnder(0, 2, base, &whatifapi.ReportVerdict{}), "served under 1.25x its knee")
	assert.False(t, aggregatedReporter(cycles(10, 22), 12, 3).notJudgedUnder(0, 2, base, &whatifapi.ReportVerdict{}), "a larger pool served it")

	assert.Empty(t, withLengths(aggregatedReporter(cycles(10, 22), 5.9, 2), 820, 1000).contradicted(0, 2, base), "lighter prompt, one pool")
	assert.False(t, withLengths(aggregatedReporter(cycles(10, 22), 5.9, 2), 820, 1000).notJudgedUnder(0, 2, base, &whatifapi.ReportVerdict{}),
		"judged: the pool did not serve past its own knee")
	assert.NotEmpty(t, withLengths(contradictsReporter(cycles(10, 22), 5.9, 2, 2), 820, 1000).contradicted(2, 2, base),
		"lighter prompt, P/D: past decode's knee at 0.88")
	assert.Empty(t, withLengths(contradictsReporter(cycles(10, 22), 5.2, 2, 2), 820, 1000).contradicted(2, 2, base),
		"lighter prompt, P/D: the prompt's KV is on the decode pods")

	assert.Empty(t, withLengths(contradictsReporter(cycles(10, 22), 6.5, 2, 2), 850, 850).contradicted(2, 2, base), "lighter in both, P/D")
	assert.Empty(t, withLengths(aggregatedReporter(cycles(10, 22), 6.5, 2), 850, 850).contradicted(0, 2, base), "lighter in both, one pool")
	assert.NotEmpty(t, withLengths(aggregatedReporter(cycles(10, 22), 7.5, 2), 850, 850).contradicted(0, 2, base), "past it at 7.5 r/s (5.4)")

	// A longer prompt never scales a rate up: at 1150/850 the KVreq ratio,
	// 1575/1500, is not taken, as the batch cap can hold the knee where the
	// output ratio puts it. 5.8 r/s counts as 4.9, not 5.2.
	assert.Empty(t, withLengths(contradictsReporter(cycles(10, 22), 5.8, 2, 2), 1150, 850).contradicted(2, 2, base), "longer prompt, P/D")
}
