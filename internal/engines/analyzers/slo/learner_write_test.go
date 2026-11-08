package slo

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

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The line's learner-write carries its pods and its admission margins
// (analyzer-learner.md 6.1), as the saturation analyzer's ITL line does:
// the replicas whose readings the line was fitted from, lowest mean ITL
// first, at most LearnerWriteMaxPods of them, and the full fit's gate -- the
// steady readings' spread against 0.30 and their count against 10.

// criLineLimitBytes is the longest line containerd and CRI-O pass whole; a
// longer one is split by the runtime.
const criLineLimitBytes = 16 * 1024

// On the three-replica fleet the first line is the full fit over the four
// cycles' twelve steady readings: every pod lists its four readings at its
// own fill and ITL, and the gate is passed, by two readings and with the
// spread exactly at its threshold.
func TestLearner_WriteCarriesPodsAndMargins(t *testing.T) {
	lines := learnerCycles(t, newRolloutRig(t, nil), 4, steadyRows)
	require.Len(t, lines, 1, "premise: the fourth cycle's twelve readings fit the first line")
	f := lines[0].ContextMap()
	require.Equal(t, constants.SLOSourceOwn, f["source"])
	require.EqualValues(t, 12, f["steadyHeld"], "premise: every reading steady, the full fit")

	pods, ok := f["pods"].([]map[string]any)
	require.True(t, ok, "pods is a list, got %T", f["pods"])
	assert.EqualValues(t, 0, f["podsOmitted"])
	require.Len(t, pods, 3)
	for i, k := range []float64{0.30, 0.45, 0.60} {
		assert.Equal(t, "agg-"+string(rune('a'+i)), pods[i]["pod"], "lowest ITL first")
		assert.InDelta(t, testLine.ITLAt(k), pods[i]["reading"], 1e-12)
		assert.InDelta(t, k, pods[i]["k"], 1e-12)
		assert.EqualValues(t, 4, pods[i]["rows"])
		assert.Equal(t, itl.WriteTimeBase, pods[i]["timeBase"])
	}
	assert.EqualValues(t, 12, f["fitRows"])
	assert.EqualValues(t, itl.DefaultMinSamples, f["steadyHeldThreshold"])
	assert.EqualValues(t, 2, f["steadyHeldOverThreshold"])
	assert.InDelta(t, 0.30, f["kSpread"], 1e-12)
	assert.InDelta(t, itl.DefaultMinKSpread, f["kSpreadThreshold"], 1e-12)
	assert.InDelta(t, 0, f["kSpreadOverThreshold"], 1e-12)
}

// A fleet of twenty lists sixteen pods, lowest ITL first, counts four in
// podsOmitted, and the line stays under the CRI line limit.
func TestLearner_WriteListsSixteenPodsLowestFirst(t *testing.T) {
	const n = 20
	fleet := make([]domain.ReplicaMetrics, 0, n)
	for i := range n {
		// Highest fill first, so the order the line lists is its own.
		fleet = append(fleet, row("agg", fmt.Sprintf("agg-7f9c6d5b8-%05d", i), 0.75-0.025*float64(i)))
	}
	agg := settleAgg
	agg.CurrentReplicas = n
	r := newRolloutRig(t, nil)
	var buf bytes.Buffer
	enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	console := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.Level(int8(-logging.VERBOSE)))
	observed, logs := observer.New(zapcore.Level(int8(-logging.VERBOSE)))
	ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(zapcore.NewTee(console, observed))))
	_, err := r.a.Analyze(ctx, domain.AnalyzerInput{Namespace: "ns", ModelID: "m", Config: policy(44*time.Millisecond, 0),
		VariantStates: []domain.VariantReplicaState{agg}, ReplicaMetrics: fleet})
	require.NoError(t, err)

	lines := logs.FilterMessage("learner-write").All()
	require.Len(t, lines, 1, "premise: twenty readings fit a line on the first cycle")
	f := lines[0].ContextMap()
	assert.EqualValues(t, n-LearnerWriteMaxPods, f["podsOmitted"])
	pods, ok := f["pods"].([]map[string]any)
	require.True(t, ok, "pods is a list, got %T", f["pods"])
	require.Len(t, pods, LearnerWriteMaxPods)
	readings := make([]float64, 0, len(pods))
	for _, p := range pods {
		readings = append(readings, p["reading"].(float64))
	}
	assert.IsIncreasing(t, readings, "lowest ITL first")
	assert.Equal(t, fmt.Sprintf("agg-7f9c6d5b8-%05d", n-1), pods[0]["pod"], "the lowest fill reads the lowest ITL")
	assert.EqualValues(t, n, f["fitRows"])

	for _, line := range strings.Split(buf.String(), "\n") {
		assert.Less(t, len(line), criLineLimitBytes, "a line over the CRI limit is split by the runtime")
	}
	assert.Contains(t, buf.String(), "learner-write")
}
