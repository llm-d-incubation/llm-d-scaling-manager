package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Synthetic: no recording holds a data-parallel engine (issue
// llm-d-incubation/llm-d-scaling-manager#312). A pod of eight ranks at a mean
// fill of 0.4, its busiest rank 1.25 times that.
func TestLineSamples_BusiestRankRowsAndRatio(t *testing.T) {
	in := lineInput(0.4, 1000)
	in.ReplicaMetrics[0].EngineRanks = 8
	in.ReplicaMetrics[0].KvCacheUsage, in.ReplicaMetrics[0].MaxRankKvCacheUsage = 0.4, 0.5
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

	off := noteCycles(t0, repeated(3, in)...)
	require.NotEmpty(t, off.rows("v"))
	assert.InDelta(t, 0.4, off.rows("v")[0].K, 1e-12, "off: the mean over the ranks")
	assert.Zero(t, off.busiestRatio("v"), "off: no ratio kept")

	on := newLineSamples()
	busiest := func(variant string) bool { return variant == "v" }
	for i := range 3 {
		on.note(in, i+1, t0.Add(time.Duration(i)*15*time.Second), busiest)
	}
	require.NotEmpty(t, on.rows("v"))
	assert.InDelta(t, 0.5, on.rows("v")[0].K, 1e-12, "on: the busiest rank's")
	assert.InDelta(t, 1.25, on.busiestRatio("v"), 1e-12)

	// The planner's fill: the busiest rank's, at the mean fill over the ratio.
	k, ok := decodeFill(in.ReplicaMetrics, nil, busiest)
	require.True(t, ok)
	assert.InDelta(t, 0.5, k, 1e-12)
}

// The ratio is the median of the rows kept, and a row whose busiest rank sits
// under the band's bottom is not one: a nearly idle pod's spread is not the
// one it runs at the band's top.
func TestLineSamples_BusiestRatioMedianOfLoadedRows(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	l := newLineSamples()
	busiest := func(string) bool { return true }
	for i, c := range [][2]float64{{0.4, 0.44}, {0.4, 0.5}, {0.4, 0.6}, {0.05, 0.1}} {
		in := lineInput(c[0], 1000)
		in.ReplicaMetrics[0].EngineRanks = 8
		in.ReplicaMetrics[0].KvCacheUsage, in.ReplicaMetrics[0].MaxRankKvCacheUsage = c[0], c[1]
		l.note(in, i+1, t0.Add(time.Duration(i)*15*time.Second), busiest)
	}
	assert.Len(t, l.ratios["v"], 3, "the idle row's ratio of 2 is not kept")
	assert.InDelta(t, 1.25, l.busiestRatio("v"), 1e-12)
}
