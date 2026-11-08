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
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
)

// The shipped-behaviour row of analyzer-learner.md 7 (ev-shindin/llm-scaling-manager#303): the default
// verbosity does not grow per replica, and no line passes the CRI limit.
// Every package that adds learner-write carries the saturation package's two
// tests (saturation/replica_log_test.go); these are the throughput
// analyzer's, which learner PR 4b-1 gave learner-write.

// criMaxLogLineBytes is the longest line containerd and CRI-O pass whole: a
// longer one is split, and a grep for the variant misses its second half (as
// saturation's replica_log.go states it).
const criMaxLogLineBytes = 16 * 1024

// perReplicaThroughputMessages are the lines the analyzer writes once per
// replica per cycle.
var perReplicaThroughputMessages = []string{"throughput analyzer: GPS mismatch detected"}

// logRun drives a over warm cycles of one variant of pods replicas, enough
// for the line to fit and its first learner-write to burst, and returns what
// a controller at -v=verbosity writes over the whole run, in the console
// encoding the controller ships (cmd/main.go: Development: true). Every
// replica's generated-token rate is far off the line's, so the GPS check
// finds every one of them mismatched.
func logRun(t *testing.T, a *ThroughputAnalyzer, verbosity, pods int) string {
	t.Helper()
	var buf bytes.Buffer
	enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	core := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.Level(int8(-verbosity)))
	ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)))
	c := clocktesting.NewFakePassiveClock(lineStart)
	a = a.WithClock(c)
	early := lineStart.Add(-time.Hour)
	rows := make([]domain.ReplicaMetrics, 0, pods)
	for i := range pods {
		k := 0.20 + 0.50*float64(i)/float64(pods-1)
		row := lineRow(fmt.Sprintf("decode-7f9c6d5b8-%05d", i), k, lineAt(k), early)
		row.GenerationTokenRate = 1e6
		rows = append(rows, row)
	}
	const warm = 12
	for i := range warm {
		c.SetTime(lineStart.Add(time.Duration(i) * 15 * time.Second))
		_, err := a.Analyze(ctx, domain.AnalyzerInput{ModelID: "m", Namespace: "ns", ReplicaMetrics: rows, ArrivalRate: 4,
			VariantStates: []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: pods, GPUsPerReplica: 1}}})
		require.NoError(t, err)
	}
	return buf.String()
}

func logLines(log string) int { return strings.Count(log, "\n") }

func logLinesWith(log, msg string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "\t"+msg+"\t") {
			n++
		}
	}
	return n
}

// At the shipped verbosity a run writes the same lines with 8 replicas as
// with 64: the per-pod GPS-mismatch line is at the debug level, and the
// variant's summary and its learner-write are one line each, not one per
// replica. Negative control: the parent's line (both switches off) logs the
// mismatch per pod at the default level, and its count grows with the
// fleet.
func TestThroughputLogAtTheShippedVerbosityDoesNotGrowPerReplica(t *testing.T) {
	const small, large = 8, 64
	shippedSmall := logRun(t, NewThroughputAnalyzer(), logging.DEFAULT, small)
	shippedLarge := logRun(t, NewThroughputAnalyzer(), logging.DEFAULT, large)
	verboseLarge := logRun(t, NewThroughputAnalyzer(), logging.VERBOSE, large)
	debugLarge := logRun(t, NewThroughputAnalyzer(), logging.DEBUG, large)
	t.Logf("lines over the run at -v=%d: %d with %d replicas, %d with %d; at -v=%d with %d: %d",
		logging.DEFAULT, logLines(shippedSmall), small, logLines(shippedLarge), large, logging.DEBUG, large, logLines(debugLarge))

	// The measurement has something to measure: the per-pod line exists,
	// at the debug level, and the run reached a line and logged it.
	for _, msg := range perReplicaThroughputMessages {
		assert.GreaterOrEqual(t, logLinesWith(debugLarge, msg), large,
			"%q must still be written once per replica at -v=%d", msg, logging.DEBUG)
		assert.Zero(t, logLinesWith(shippedLarge, msg),
			"%q is per replica per cycle and must not reach the shipped log", msg)
	}
	require.Positive(t, logLinesWith(verboseLarge, "learner-write"), "precondition: the run fitted a line and logged it")
	require.Equal(t, 1, logLinesWith(shippedLarge, "throughput analyzer: GPS mismatch"),
		"the variant's mismatch is one summary line, when it starts")
	// Fewer replicas may log more: the line's silence summary logs when the
	// line goes silent and comes back, and a small fleet needs more cycles
	// for its ten readings, while a large one has them on the first.
	assert.LessOrEqual(t, logLines(shippedLarge), logLines(shippedSmall),
		"lines at the shipped verbosity must not grow with the replica count")
	assert.LessOrEqual(t, logLinesWith(shippedSmall, "throughput analyzer: ITL line"), 2,
		"the line's silence is one summary line when it starts and one when it ends")
	assert.Equal(t, logLinesWith(verboseLarge, "learner-write"),
		logLinesWith(logRun(t, NewThroughputAnalyzer(), logging.VERBOSE, small), "learner-write"),
		"learner-write is per variant, not per replica")

	parentSmall := logRun(t, newParentLineAnalyzer(), logging.DEFAULT, small)
	parentLarge := logRun(t, newParentLineAnalyzer(), logging.DEFAULT, large)
	t.Logf("negative control, the parent's line at -v=%d: %d lines with %d replicas, %d with %d",
		logging.DEFAULT, logLines(parentSmall), small, logLines(parentLarge), large)
	assert.Greater(t, logLines(parentLarge), logLines(parentSmall),
		"negative control: the parent logs the mismatch per pod at the default level")
}

// No line the analyzer writes at the shipped or the verbose level passes the
// CRI limit, at 64 replicas with every one of them mismatched: the summary
// names the worst pod, not every one. Negative control: the same check
// fails on a line one byte over.
func TestThroughputLogStaysWithinTheCRILineLimit(t *testing.T) {
	over := func(log string) []int {
		var long []int
		for i, line := range strings.Split(strings.TrimSuffix(log, "\n"), "\n") {
			if len(line)+1 > criMaxLogLineBytes {
				long = append(long, i)
			}
		}
		return long
	}
	for _, v := range []int{logging.DEFAULT, logging.VERBOSE} {
		log := logRun(t, NewThroughputAnalyzer(), v, 64)
		require.NotEmpty(t, log, "precondition: the run logged at -v=%d", v)
		assert.Empty(t, over(log), "lines over the CRI limit at -v=%d", v)
	}
	require.Len(t, over(strings.Repeat("x", criMaxLogLineBytes)+"\n"), 1, "negative control: a line over the limit is caught")
}
