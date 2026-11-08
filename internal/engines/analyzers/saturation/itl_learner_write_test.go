package saturation

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// The ITL line's learner-write carries its pods and its admission margins
// (analyzer-learner.md 6.1): the replicas whose readings the line was fitted
// from, lowest mean ITL first, and the full fit's gate -- the steady
// readings' spread against 0.30 and their count against 10 -- with how far
// over each it is.

// On the three-replica fleet the first line is the full fit over the four
// cycles' twelve steady readings (the rig's replicas have been Ready an hour,
// and nothing waits): every pod lists its four readings at its own fill and
// ITL, and the gate is passed, by two readings and with the spread exactly
// at its threshold.
func TestITLLearner_LearnerWriteCarriesPodsAndMargins(t *testing.T) {
	r := newITLLearnerRig()
	ctx, logs := observedCtx(t)
	for range 4 {
		r.cycle(t, ctx, itlSettleFleet())
	}
	writes := learnerWrites(logs, learn.ITL)
	require.Len(t, writes, 1, "premise: the fourth cycle's twelve readings fit the first line")
	f := writes[0].ContextMap()
	require.Equal(t, itlSourceOLS, f["source"], "premise: twelve steady readings spread 0.30, the full fit")

	pods, ok := f["pods"].([]map[string]any)
	require.True(t, ok, "pods is a list, got %T", f["pods"])
	assert.EqualValues(t, 0, f["podsOmitted"])
	require.Len(t, pods, 3)
	for i, k := range []float64{0.30, 0.45, 0.60} {
		assert.Equal(t, fmt.Sprintf("p%d", i), pods[i]["pod"], "lowest ITL first")
		assert.InDelta(t, 0.008+0.04*k, pods[i]["reading"], 1e-12)
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

// A fleet whose fills span 0.10 misses the full fit's gate: the line is the
// pinned fit, and the spread's margin says by how much it missed.
func TestITLLearner_LearnerWriteMarginsOfAPinnedFit(t *testing.T) {
	narrow := []domain.ReplicaMetrics{itlSettleRow("p0", 0.40), itlSettleRow("p1", 0.45), itlSettleRow("p2", 0.50)}
	r := newITLLearnerRig()
	ctx, logs := observedCtx(t)
	for range 4 {
		r.cycle(t, ctx, narrow)
	}
	writes := learnerWrites(logs, learn.ITL)
	require.Len(t, writes, 1, "premise: twelve readings fit the first line")
	f := writes[0].ContextMap()
	require.Equal(t, itlSourcePinned, f["source"], "premise: a spread of 0.10 is short of the full fit")
	assert.InDelta(t, 0.10, f["kSpread"], 1e-9)
	assert.InDelta(t, -0.20, f["kSpreadOverThreshold"], 1e-9, "missed by 0.20")
	assert.EqualValues(t, 12, f["steadyHeld"])
	assert.EqualValues(t, 2, f["steadyHeldOverThreshold"])
	assert.EqualValues(t, 12, f["fitRows"], "the pinned fit reads the steady readings the main window spans")
	pods, ok := f["pods"].([]map[string]any)
	require.True(t, ok, "pods is a list, got %T", f["pods"])
	require.Len(t, pods, 3)
	var rows int64
	for _, p := range pods {
		rows += int64(p["rows"].(int))
	}
	assert.EqualValues(t, 12, rows, "every reading the fit read is some pod's")
}

// A fleet of twenty lists sixteen pods, lowest ITL first, counts four in
// podsOmitted, and the line stays under the CRI line limit.
func TestITLLearner_AWriteLineListsSixteenPodsLowestFirst(t *testing.T) {
	const n = 20
	fleet := make([]domain.ReplicaMetrics, 0, n)
	for i := range n {
		// Highest fill first, so the order the line lists is its own.
		fleet = append(fleet, itlSettleRow(fmt.Sprintf("pod-with-a-long-generated-name-%02d", i), 0.75-0.025*float64(i)))
	}
	r := newITLLearnerRig()
	var buf bytes.Buffer
	enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	console := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.Level(int8(-perReplicaLogLevel)))
	observed, logs := observer.New(zapcore.Level(-perReplicaLogLevel))
	ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(zapcore.NewTee(console, observed))))
	r.cycle(t, ctx, fleet)

	writes := learnerWrites(logs, learn.ITL)
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
	assert.Equal(t, fmt.Sprintf("pod-with-a-long-generated-name-%02d", n-1), pods[0]["pod"], "the lowest fill reads the lowest ITL")
	assert.EqualValues(t, n, f["fitRows"])

	for _, line := range strings.Split(buf.String(), "\n") {
		assert.Less(t, len(line), criMaxLogLineBytes, "a line over the CRI limit is split by the runtime")
		if strings.Contains(line, "learner-write") {
			t.Logf("the learner-write line is %d bytes of the CRI limit's %d", len(line), criMaxLogLineBytes)
		}
	}
	assert.Contains(t, buf.String(), "learner-write")
}
