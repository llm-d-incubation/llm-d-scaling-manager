package learnreplay

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// engineRow is one decode row: count requests a second under each of two
// edges, the last the +Inf one, and an ITL.
func engineRow(pod string, under, count, itl float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{PodName: pod, VariantName: "d", AvgITL: itl, AvgITLKnown: true, Ready: true,
		TPOTBuckets: []domain.LatencyBucket{{UpperBound: 0.01, Rate: under}, {UpperBound: math.Inf(1), Rate: count}}}
}

// Rows pool by bucket, and the window's mean is the rows' ITLs by requests: a
// busy pod counts for more than an idle one.
func TestEngineTPOTWindow_PoolsTheRows(t *testing.T) {
	w, ok := engineTPOTWindow([]domain.ReplicaMetrics{engineRow("a", 1, 2, 0.01), engineRow("b", 3, 6, 0.02)}, map[string]string{"d": "decode"})
	require.True(t, ok)
	assert.Equal(t, []whatif.HistogramBucket{{UpperEdge: 0.01, Count: 4}, {UpperEdge: math.Inf(1), Count: 8}}, w.Buckets)
	assert.InDelta(t, 8.0, w.Count, 1e-12)
	assert.InDelta(t, 2*0.01+6*0.02, w.Sum, 1e-12)
}

func TestEngineTPOTWindow_LeavesOut(t *testing.T) {
	roles := map[string]string{"d": "decode", "p": "prefill"}

	prefillRow := engineRow("p", 1, 2, 0.01)
	prefillRow.VariantName = "p"
	_, ok := engineTPOTWindow([]domain.ReplicaMetrics{prefillRow}, roles)
	assert.False(t, ok, "prefill makes one token a request")

	noITL := engineRow("a", 1, 2, 0)
	noITL.AvgITLKnown = false
	_, ok = engineTPOTWindow([]domain.ReplicaMetrics{noITL}, roles)
	assert.False(t, ok, "no mean to read a ratio over")

	_, ok = engineTPOTWindow([]domain.ReplicaMetrics{{PodName: "a", VariantName: "d"}}, roles)
	assert.False(t, ok, "no histogram")

	// Another grid does not pool: its counts would be read at the wrong edges.
	other := engineRow("b", 1, 2, 0.01)
	other.TPOTBuckets[0].UpperBound = 0.02
	w, ok := engineTPOTWindow([]domain.ReplicaMetrics{engineRow("a", 1, 2, 0.01), other}, roles)
	require.True(t, ok)
	assert.InDelta(t, 2.0, w.Count, 1e-12)
}

// The replay carries the windows: the engine's on nearly every cycle of this
// run, the router's on none.
func TestStateAt_CarriesTheEngineTPOTWindows(t *testing.T) {
	gz, err := os.ReadFile(filepath.Join("testdata", "goldens", "throughput-sweep-restart", RecordingName))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	st, err := StateAt(context.Background(), raw, Options{}, StateQuery{})
	require.NoError(t, err)
	assert.Empty(t, st.TPOTWindows)
	assert.Len(t, st.EngineTPOTWindows, 314, "two of the 316 cycles give no window")
}
