package throughput

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The line's learner-write carries its pods and its admission margins
// (analyzer-learner.md 6.1), as the saturation analyzer's ITL line does:
// the replicas whose readings the line was fitted from, lowest mean ITL
// first, at most LearnerWriteMaxPods of them, and the full fit's gate -- the
// steady readings' spread against 0.30 and their count against 10.

// learnerWriteRun runs n cycles of rows at V(3), each line also written in
// the console encoding the controller ships, and returns the learner-write
// lines and that console output.
func learnerWriteRun(t *testing.T, n int, rows []domain.ReplicaMetrics) ([]observer.LoggedEntry, string) {
	t.Helper()
	var buf bytes.Buffer
	enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	console := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.Level(int8(-logging.VERBOSE)))
	observed, logs := observer.New(zapcore.Level(int8(-logging.VERBOSE)))
	r := newLineRun(t, NewThroughputAnalyzer())
	r.ctx = logr.NewContext(context.Background(), zapr.NewLogger(zap.New(zapcore.NewTee(console, observed))))
	for range n {
		r.step(rows)
	}
	return logs.FilterMessage("learner-write").All(), buf.String()
}

// On the three-replica fleet the first line is the full fit over the four
// cycles' twelve steady readings: every pod lists its four readings at its
// own fill and ITL, and the gate is passed, by two readings and with the
// spread exactly at its threshold.
func TestLearnerWrite_CarriesPodsAndMargins(t *testing.T) {
	writes, _ := learnerWriteRun(t, 4, settleFleet())
	require.Len(t, writes, 1, "premise: the fourth cycle's twelve readings fit the first line")
	f := writes[0].ContextMap()
	require.Equal(t, itlReasonT1OLS, f["source"], "premise: twelve steady readings spread 0.30, the full fit")

	pods, ok := f["pods"].([]map[string]any)
	require.True(t, ok, "pods is a list, got %T", f["pods"])
	assert.EqualValues(t, 0, f["podsOmitted"])
	require.Len(t, pods, 3)
	for i, k := range []float64{0.30, 0.45, 0.60} {
		assert.Equal(t, "pod-"+string(rune('a'+i)), pods[i]["pod"], "lowest ITL first")
		assert.InDelta(t, lineAt(k), pods[i]["reading"], 1e-12)
		assert.InDelta(t, k, pods[i]["k"], 1e-12)
		assert.EqualValues(t, 4, pods[i]["rows"])
		assert.Equal(t, itl.WriteTimeBase, pods[i]["timeBase"])
	}
	assert.EqualValues(t, 12, f["fitRows"])
	assert.EqualValues(t, 12, f["steadyHeld"])
	assert.EqualValues(t, itl.DefaultMinSamples, f["steadyHeldThreshold"])
	assert.EqualValues(t, 2, f["steadyHeldOverThreshold"])
	assert.InDelta(t, 0.30, f["kSpread"], 1e-12)
	assert.InDelta(t, itl.DefaultMinKSpread, f["kSpreadThreshold"], 1e-12)
	assert.InDelta(t, 0, f["kSpreadOverThreshold"], 1e-12)
}

// A line that keeps fitting and moves less than ITLWriteBand logs
// learner-write once per LearnerHeartbeat (analyzer-learner.md 6.1,
// heartbeat): not every cycle, and not never. The run's cycle is 15 s, so
// the heartbeat falls on the 40th cycle after the line before. The fleet's
// ITL rises 1 % at the start of each heartbeat, under the band, so the line
// the heartbeat logs differs from the one logged before it, and its
// previous figures must be the logged ones, not the last cycle's fit.
func TestLearnerWrite_AQuietLineLogsOncePerHeartbeat(t *testing.T) {
	observed, logs := observer.New(zapcore.Level(int8(-logging.VERBOSE)))
	r := newLineRun(t, NewThroughputAnalyzer())
	r.ctx = logr.NewContext(context.Background(), zapr.NewLogger(zap.New(observed)))
	scale := 1.0
	// cycle runs one cycle of the three-replica fleet, its ITL scaled by
	// scale, and returns how many learner-write lines it logged.
	cycle := func() int {
		before := logs.FilterMessage("learner-write").Len()
		fleet := settleFleet()
		for i := range fleet {
			fleet[i].AvgITL *= scale
		}
		r.step(fleet)
		return logs.FilterMessage("learner-write").Len() - before
	}
	for i := range 3 {
		require.Zero(t, cycle(), "premise: cycle %d holds too few steady readings to fit", i)
	}
	require.Equal(t, 1, cycle(), "the first write logs")
	writes := logs.FilterMessage("learner-write").All()
	first := writes[0].ContextMap()
	require.Equal(t, true, first["firstWrite"])
	require.Equal(t, itlReasonT1OLS, first["source"], "premise: the full fit")

	cycleEvery := r.now().Sub(r.clock.Now()) // the next cycle against the last
	perHeartbeat := int(LearnerHeartbeat / cycleEvery)
	require.Greater(t, perHeartbeat, 1, "premise: the heartbeat spans several cycles")
	prev := first
	for beat := range 2 {
		scale += 0.01
		for i := 1; i < perHeartbeat; i++ {
			require.Zero(t, cycle(), "beat %d, cycle %d of %d: a line moving under the band does not log before the heartbeat", beat, i, perHeartbeat)
		}
		require.Equal(t, 1, cycle(), "beat %d: a line that has not logged for LearnerHeartbeat logs once", beat)
		all := logs.FilterMessage("learner-write").All()
		line := all[len(all)-1].ContextMap()
		assert.Equal(t, false, line["firstWrite"], "beat %d", beat)
		// The window has rolled over to the scaled readings: the line is the
		// first one scaled, and no longer the one logged before it.
		assert.InEpsilon(t, first["a"].(float64)*scale, line["a"], 1e-9, "beat %d: the slope follows the load", beat)
		assert.InEpsilon(t, first["b"].(float64)*scale, line["b"], 1e-9, "beat %d: the intercept follows the load", beat)
		require.Greater(t, line["a"].(float64)-prev["a"].(float64), 0.005*prev["a"].(float64),
			"premise: the line moved since the last logged one")
		assert.Equal(t, prev["a"], line["previousA"], "beat %d: against the last logged slope", beat)
		assert.Equal(t, prev["b"], line["previousB"], "beat %d: against the last logged intercept", beat)
		assert.Equal(t, first["source"], line["source"], "beat %d: the source did not change", beat)
		assert.Equal(t, line["source"], line["previousSource"], "beat %d", beat)
		prev = line
	}
	assert.Equal(t, 3, logs.FilterMessage("learner-write").Len(), "the first write and one per heartbeat")
}

// A fleet of twenty lists sixteen pods, lowest ITL first, counts four in
// podsOmitted, and the line stays under the CRI line limit.
func TestLearnerWrite_ListsSixteenPodsLowestFirst(t *testing.T) {
	const n = 20
	early := lineStart.Add(-time.Hour)
	fleet := make([]domain.ReplicaMetrics, 0, n)
	for i := range n {
		// Highest fill first, so the order the line lists is its own.
		k := 0.75 - 0.025*float64(i)
		fleet = append(fleet, lineRow(fmt.Sprintf("decode-7f9c6d5b8-%05d", i), k, lineAt(k), early))
	}
	writes, console := learnerWriteRun(t, 1, fleet)
	require.Len(t, writes, 1, "premise: twenty readings fit a line on the first cycle")
	f := writes[0].ContextMap()
	assert.EqualValues(t, n-LearnerWriteMaxPods, f["podsOmitted"])
	pods, ok := f["pods"].([]map[string]any)
	require.True(t, ok, "pods is a list, got %T", f["pods"])
	require.Len(t, pods, LearnerWriteMaxPods)
	readings := make([]float64, 0, len(pods))
	for _, p := range pods {
		readings = append(readings, p["reading"].(float64))
	}
	assert.IsIncreasing(t, readings, "lowest ITL first")
	assert.Equal(t, fmt.Sprintf("decode-7f9c6d5b8-%05d", n-1), pods[0]["pod"], "the lowest fill reads the lowest ITL")
	assert.EqualValues(t, n, f["fitRows"])

	for _, line := range strings.Split(console, "\n") {
		assert.Less(t, len(line), criMaxLogLineBytes, "a line over the CRI limit is split by the runtime")
	}
	assert.Contains(t, console, "learner-write")
}
