package saturation

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// A start key that keeps pricing and does not move logs learner-write once
// per LearnerHeartbeat (analyzer-learner.md 6.1, heartbeat): not every
// cycle, and not never. The cycle is 30 s, so the heartbeat falls on the
// 20th cycle after the line before.
func TestStartLearner_AQuietKeyLogsOncePerHeartbeat(t *testing.T) {
	const cycleEvery = 30 * time.Second
	var writes []map[string]any
	logger := funcr.NewJSON(func(obj string) {
		var m map[string]any
		if json.Unmarshal([]byte(obj), &m) == nil && m["msg"] == "learner-write" {
			writes = append(writes, m)
		}
	}, funcr.Options{Verbosity: 10})
	a := blendAnalyzer(t, true, true)
	note := startNote{key: confirmKey, namespace: "ns", modelID: "model", variant: confirmVariant, role: domain.RoleDecode}
	// cycle runs one start-learner cycle with p0 still reporting the start it
	// was admitted with, and returns how many learner-write lines it logged.
	cycle := func() int {
		at := a.now().Add(cycleEvery)
		a.now = func() time.Time { return at }
		before := len(writes)
		a.beginStartCycle("ns", "model")
		a.noteReplicaStartFor(note, confirmRows(confirmReplica("p0", 460)), logger)
		a.closeStartCycle("ns", "model", map[string]string{confirmVariant: domain.RoleDecode})
		reps := a.TakeLearnerReports()["ns|model"]
		require.Len(t, reps, 1, "premise: the key prices every cycle")
		require.Equal(t, learn.Source(startSourceMeasured), reps[0].Source, "premise: a measured figure, which is the only one that logs")
		return len(writes) - before
	}

	require.Equal(t, 1, cycle(), "the first write logs")
	require.Equal(t, true, writes[0]["firstWrite"])
	first := writes[0]["value"]

	perHeartbeat := int(LearnerHeartbeat / cycleEvery)
	require.Greater(t, perHeartbeat, 1, "premise: the heartbeat spans several cycles")
	for beat := range 2 {
		for i := 1; i < perHeartbeat; i++ {
			require.Zero(t, cycle(), "beat %d, cycle %d of %d: a quiet key does not log before the heartbeat", beat, i, perHeartbeat)
		}
		require.Equal(t, 1, cycle(), "beat %d: a quiet key that has not logged for LearnerHeartbeat logs once", beat)
		line := writes[len(writes)-1]
		assert.Equal(t, false, line["firstWrite"], "beat %d", beat)
		assert.Equal(t, first, line["value"], "beat %d: the figure did not move", beat)
		assert.Equal(t, first, line["previousValue"], "beat %d: against the last logged value", beat)
		assert.Equal(t, line["source"], line["previousSource"], "beat %d: the source did not change", beat)
	}
	assert.Len(t, writes, 3, "the first write and one per heartbeat")
}
