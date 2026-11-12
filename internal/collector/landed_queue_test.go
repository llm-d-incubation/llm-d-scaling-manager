package collector

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
)

// The landed KV fetches (vLLM's
// num_requests_kv_fetch_by_stage{stage="completed_waiting"}, v0.32.0 and
// later) ride beside the queue as its capacity part does: read where the
// series exists, unknown where it does not.
func TestCollectReplicaMetrics_LandedQueue(t *testing.T) {
	glm := map[string]string{seriesModelLabel: "glm", "pod": dp8Pod, "instance": "10.0.0.8:8000"}

	t.Run("published", func(t *testing.T) {
		results := singleFrontendResults()
		addValue(results, registration.QueryLandedQueueLength, glm, 3)
		addValue(results, registration.QueryMaxRankLandedQueueLength, glm, 2)
		rows := collectRows(t, results, dp8Pod)
		require.Len(t, rows, 1)
		assert.True(t, rows[0].LandedQueueKnown)
		assert.Equal(t, 3, rows[0].LandedQueueLength)
		assert.Equal(t, 2, rows[0].BusiestRankLandedQueue())
		assert.Equal(t, 8, rows[0].QueueLength, "the full queue is unchanged")
		assert.False(t, rows[0].CapacityQueueKnown, "the two series are read apart")
	})

	t.Run("not published", func(t *testing.T) {
		rows := collectRows(t, singleFrontendResults(), dp8Pod)
		require.Len(t, rows, 1)
		assert.False(t, rows[0].LandedQueueKnown, "an older vLLM publishes no fetch stage")
		assert.Zero(t, rows[0].LandedQueueLength)
		assert.Zero(t, rows[0].BusiestRankLandedQueue())
	})

	t.Run("another model's series", func(t *testing.T) {
		results := singleFrontendResults()
		other := map[string]string{seriesModelLabel: "other", "pod": dp8Pod, "instance": "10.0.0.8:8000"}
		addValue(results, registration.QueryLandedQueueLength, other, 3)
		rows := collectRows(t, results, dp8Pod)
		require.Len(t, rows, 1)
		assert.False(t, rows[0].LandedQueueKnown, "partitioned by model_name like the queue itself")
	})
}

// Ranks with their own API servers merge the landed fetches as the queue
// merges: summed, the busiest rank's max, and known only if every rank says.
func TestCollectReplicaMetrics_LandedQueueMergesPerPort(t *testing.T) {
	build := func(withStage func(i int) bool) map[string]*source.MetricResult {
		results := perPortResults()
		for i, r := range dp8Ranks {
			if !withStage(i) {
				continue
			}
			labels := map[string]string{seriesModelLabel: "glm", "pod": dp8Pod, "instance": fmt.Sprintf("10.0.0.8:%d", 8000+i)}
			// Half of each rank's queue has its KV.
			addValue(results, registration.QueryLandedQueueLength, labels, float64(r.wait/2))
			addValue(results, registration.QueryMaxRankLandedQueueLength, labels, float64(r.wait/2))
		}
		return results
	}

	rows := collectRows(t, build(func(int) bool { return true }), dp8Pod)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].LandedQueueKnown)
	assert.Equal(t, 4, rows[0].LandedQueueLength, "rank 6's 1 and rank 7's 3")
	assert.Equal(t, 3, rows[0].BusiestRankLandedQueue(), "rank 7's")

	rows = collectRows(t, build(func(i int) bool { return i != 7 }), dp8Pod)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].LandedQueueKnown, "rank 7 did not say which of its requests have their KV")
	assert.Zero(t, rows[0].LandedQueueLength)
	assert.Zero(t, rows[0].BusiestRankLandedQueue())
}
