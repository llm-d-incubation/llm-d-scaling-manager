package throughput

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// Analyzer-level tests of what the line on the tracker claims
// (SwitchITLSteadyGate), each written to fail on the one-line change that
// would break its claim.

// idleFleet is steadyFleet below the band: k 0.05, on the line.
func idleFleet() []domain.ReplicaMetrics {
	rows := steadyFleet()
	for i := range rows {
		rows[i].KvCacheUsage, rows[i].KvUsageInstant, rows[i].AvgITL = 0.05, 0.05, lineAt(0.05)
	}
	return rows
}

// A held slope is bounded by the engine arguments it was fitted under
// (itl.Admission.Args, which lineAdmission reads from the capacity store):
// a line fitted at load and held through light load is not held once the
// variant's engine arguments change. Fails if lineAdmission stops setting
// Args.
func TestITLLine_AHeldSlopeEndsWhenTheEngineArgumentsChange(t *testing.T) {
	before := &capacity.EngineParams{Engine: "vllm", GpuMemoryUtilization: 0.90}
	after := &capacity.EngineParams{Engine: "vllm", GpuMemoryUtilization: 0.80}
	require.NotEqual(t, before.ArgsTag(), after.ArgsTag(), "premise: the two configurations have different arguments")
	store := capacity.NewStore()
	store.Update("ns", "m", "v1", capacity.Record{EngineParams: before}, lineStart)
	r := newLineRun(t, NewThroughputAnalyzer().WithSlopeHoldFor(time.Hour).WithCapacityStore(store))
	for range 12 {
		r.step(steadyFleet())
	}
	// Below the band until the band's readings age out (30 min): the line
	// then holds its A.
	var vc domain.VariantCapacity
	var has bool
	for r.now().Before(lineStart.Add(35 * time.Minute)) {
		vc, has = r.step(idleFleet())
	}
	require.True(t, has, "premise: the line holds through the light load")
	require.Equal(t, itlReasonT2Held, vc.Reason, "premise: the line holds through the light load")

	store.Update("ns", "m", "v1", capacity.Record{EngineParams: after}, r.now())
	vc, has = r.step(idleFleet())
	require.False(t, has, "the slope fitted under the old arguments was still held (%s)", vc.Reason)
}

// A shape change empties the line's window and drops its held slope: on
// the cycles after it the line is silent until the new shape's readings
// fit, and then prices the new shape's A. Shorter requests put more of them
// in the same cache, so their A is higher, and the old A held into them
// priced a lower ITL -- fewer replicas. Fails if the shape change stops
// clearing the window (the old shape's readings fit on), or stops dropping
// the slope (T2-held at the old A).
func TestITLLine_AShapeChangeDoesNotPriceTheOldShapesA(t *testing.T) {
	early := lineStart.Add(-time.Hour)
	const oldA, newA = 0.05, 0.10
	fleet := func(il, ol, a float64) []domain.ReplicaMetrics {
		rows := make([]domain.ReplicaMetrics, 0, 3)
		for _, pod := range []string{"pod-a", "pod-b", "pod-c"} {
			row := lineRow(pod, 0.40, 0.006+a*0.40, early)
			row.AvgInputTokens, row.AvgOutputTokens = il, ol
			rows = append(rows, row)
		}
		return rows
	}
	ctx, buf := logContext()
	r := newLineRun(t, NewThroughputAnalyzer().WithSlopeHoldFor(time.Hour))
	r.ctx = ctx
	for range 12 {
		r.step(fleet(1024, 256, oldA))
	}
	require.InDelta(t, oldA, r.slope(), 0.002, "premise: the old shape's line")
	require.Len(t, lineLogs(buf.String()), 2, "premise: silent on the first cycles, then live: %s", buf)

	reasons := []string{}
	for range 6 {
		vc, has := r.step(fleet(256, 64, newA))
		if !has {
			reasons = append(reasons, itlReasonT2Failed)
			continue
		}
		reasons = append(reasons, vc.Reason)
		require.InDelta(t, newA, r.slope(), 0.005, "cycle %d after the shape change priced A %g (reasons %v)",
			len(reasons), r.slope(), reasons)
	}
	// Three replicas a cycle: the tenth reading of the new shape is on the
	// fourth cycle.
	require.Equal(t, []string{itlReasonT2Failed, itlReasonT2Failed, itlReasonT2Failed,
		itlReasonT2Default, itlReasonT2Default, itlReasonT2Default}, reasons)
	// The log says so: one line when the shape change silenced the line,
	// naming it, and one when the new shape's fit brought it back.
	logs := lineLogs(buf.String())
	require.Len(t, logs, 4, "%s", buf)
	require.Contains(t, logs[2], `"silent": true, "cause": "shape-change"`)
	require.Contains(t, logs[3], `"silent": false, "cause": "", "source": "T2-default"`)
	require.Contains(t, logs[3], `"held": 12`, "the new shape's four cycles of three readings")
}

// The GPS diagnostic judges each replica at the k paired with its ITL
// (KvUsageForITL): a replica whose minute's mean fill is 0.80 and whose
// instant fill is 0.35, generating at the rate the line predicts at 0.80,
// is no mismatch. Fails if the diagnostic reads KvUsageInstant.
func TestITLLine_TheGPSDiagnosticReadsTheKPairedWithTheITL(t *testing.T) {
	a := NewThroughputAnalyzer()
	model := itl.Model{A: 0.01, B: 0.006}
	s := shape.Shape{KVreq: 1280}
	row := lineRow("pod-a", 0.35, model.ITLAt(0.80), lineStart)
	row.KvUsageMean, row.KvUsageMeanKnown = 0.80, true
	row.GenerationTokenRate = itl.TokenRate(model, 0.80, float64(row.TotalKvCapacityTokens), s.KVreq)
	instantErr, ok := itl.GPSErrorPct(model, 0.35, float64(row.TotalKvCapacityTokens), s.KVreq, row.GenerationTokenRate)
	require.True(t, ok)
	require.Greater(t, instantErr, DefaultGPSMismatchThresholdPct, "premise: at the instant k the rate is a mismatch")
	state := &variantState{}
	require.False(t, a.noteGPSMismatch(context.Background(), state, []domain.ReplicaMetrics{row}, s, model, "ns", "m", "v1"),
		"a replica on the line at the k paired with its ITL was judged a mismatch")
}

// An infinite observed rate makes the GPS error NaN: no evidence either
// way, so no mismatch.
func TestITLLine_ANaNGPSErrorIsNoMismatch(t *testing.T) {
	a := NewThroughputAnalyzer()
	model := itl.Model{A: 0.05, B: 0.006}
	s := shape.Shape{KVreq: 1280}
	row := lineRow("pod-a", 0.50, lineAt(0.50), lineStart)
	row.GenerationTokenRate = math.Inf(1)
	errPct, ok := itl.GPSErrorPct(model, 0.50, float64(row.TotalKvCapacityTokens), s.KVreq, row.GenerationTokenRate)
	require.True(t, ok && math.IsNaN(errPct), "premise: an infinite rate gives a NaN error (%g, %v)", errPct, ok)
	state := &variantState{}
	require.False(t, a.noteGPSMismatch(context.Background(), state, []domain.ReplicaMetrics{row}, s, model, "ns", "m", "v1"))
	require.False(t, state.gpsMismatched)
}

// A mismatch the log opened is closed when there is nothing to compare: no
// shape (KVreq 0), or, through Analyze, no line.
func TestITLLine_AGPSMismatchEndsWhenThereIsNothingToCompare(t *testing.T) {
	ctx, buf := logContext()
	a := NewThroughputAnalyzer()
	state := &variantState{gpsMismatched: true}
	require.False(t, a.noteGPSMismatch(ctx, state, nil, shape.Shape{}, itl.Model{}, "ns", "m", "v1"))
	require.False(t, state.gpsMismatched)
	require.Equal(t, 1, strings.Count(buf.String(), `"mismatched": false`), "the end was not logged: %s", buf)

	// Through Analyze: a mismatched line that goes silent (one replica, too
	// few readings after a shape change) logs the end.
	ctx, buf = logContext()
	r := newLineRun(t, NewThroughputAnalyzer())
	r.ctx = ctx
	mismatched := func(il float64) []domain.ReplicaMetrics {
		rows := steadyFleet()
		for i := range rows {
			rows[i].GenerationTokenRate = 1e6
			rows[i].AvgInputTokens = il
		}
		return rows
	}
	for range 6 {
		r.step(mismatched(1024))
	}
	require.Equal(t, 1, strings.Count(buf.String(), `"mismatched": true`), "premise: the mismatch started: %s", buf)
	_, has := r.step(mismatched(256))
	require.False(t, has, "premise: the shape change left no line")
	require.Equal(t, 1, strings.Count(buf.String(), `"mismatched": false`), "the end was not logged: %s", buf)
}

// A stale row is no reading of the replica now: with one replica's rows
// stale and reading a warm-up ITL, the line is the fresh replicas'. Fails if
// freshRows admits stale rows.
func TestITLLine_AStaleRowIsNoReading(t *testing.T) {
	r := newLineRun(t, NewThroughputAnalyzer())
	for range 12 {
		stale := lineRow("pod-stale", 0.40, 0.45, lineStart.Add(-time.Hour))
		stale.Metadata = &domain.ReplicaMetricsMetadata{FreshnessStatus: "stale"}
		r.step(append(steadyFleet(), stale))
	}
	require.InDelta(t, 0.05, r.slope(), 0.002, "a stale row moved A")
}

// The line logs one line at the default level when it goes silent and
// one when it comes back, with what its rows were: here a fleet started
// above the band, silent until the fallback has ten readings.
func TestITLLine_TheLineLogsWhenItGoesSilentAndComesBack(t *testing.T) {
	ctx, buf := logContext()
	r := newLineRun(t, NewThroughputAnalyzer())
	r.ctx = ctx
	above := steadyFleet()
	for i := range above {
		above[i].KvCacheUsage, above[i].KvUsageInstant, above[i].AvgITL = 0.90, 0.90, lineAt(0.90)
	}
	for range 8 {
		r.step(above)
	}
	log := buf.String()
	require.Equal(t, 2, logLinesWith(log, "throughput analyzer: ITL line"), "want one line silent, one back: %s", log)
	logs := lineLogs(log)
	require.Contains(t, logs[0], `"silent": true, "cause": "restart"`)
	require.Contains(t, logs[1], `"silent": false, "cause": "", "source": "T2-above-band"`)
	for _, l := range logs {
		require.Contains(t, l, `"aboveBand": 3`)
		require.Contains(t, l, `"inBand": 0`)
	}
	// Silent on the first cycle, three readings above the band; back on the
	// fourth, with twelve. The band's window holds none on either.
	require.Contains(t, logs[0], `"held": 0, "aboveBandHeld": 3`)
	require.Contains(t, logs[1], `"held": 0, "aboveBandHeld": 12`)
}

// logContext is a context whose logger writes, at the shipped verbosity, to
// the buffer returned.
func logContext() (context.Context, *bytes.Buffer) {
	var buf bytes.Buffer
	enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	core := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.InfoLevel)
	return logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core))), &buf
}

// withoutAboveBandFallback turns the line's above-band fallback off, for a
// negative control.
func (a *ThroughputAnalyzer) withoutAboveBandFallback() *ThroughputAnalyzer {
	a.noAboveBandFallback = true
	return a
}

// withAboveBandRestartWait stamps the above-band readings with the
// restart's wait, as learner PR 4b-2 first built it, for a negative control.
func (a *ThroughputAnalyzer) withAboveBandRestartWait() *ThroughputAnalyzer {
	a.aboveBandWaitsAfterRestart = true
	return a
}

// A flat above-band line (A = 0: every reading the same ITL, B pinned at
// it) is a price end to end: the line prices one ITL at every k, the
// variant keeps a finite, positive per-replica capacity on every cycle, the
// k-based local demand (no completion rate) and the GPS diagnostic read it
// without a division by A, and the variant is never silent once priced.
func TestITLLine_AFlatAboveBandLineIsAPrice(t *testing.T) {
	r := newLineRun(t, NewThroughputAnalyzer())
	rows := func() []domain.ReplicaMetrics {
		out := steadyFleet()
		for i := range out {
			out[i].KvCacheUsage, out[i].KvUsageInstant, out[i].AvgITL = 0.90, 0.90, lineAt(0.90)
			out[i].RequestRate = 0 // demand from k (computeLocalDemand)
			out[i].GenerationTokenRate = 1e6
		}
		return out
	}
	priced := 0
	for c := range 20 {
		vc, has := r.step(rows())
		if c < 3 {
			require.False(t, has, "cycle %d: a line from fewer than ten readings", c)
			continue
		}
		require.True(t, has, "cycle %d: the flat line went silent", c)
		require.Equal(t, itlReasonT2AboveBand, vc.Reason)
		st, _ := r.a.VariantState("m", "ns", "v1")
		require.Zero(t, st.ITLModel.A, "premise: the line is flat")
		require.InDelta(t, lineAt(0.90), st.ITLModel.B, 1e-12, "B pinned at the readings' ITL")
		require.Equal(t, st.ITLModel.ITLAt(0.10), st.ITLModel.ITLAt(itl.DefaultKSat), "a flat line prices one ITL at every k")
		for _, v := range []float64{vc.PerReplicaCapacity, st.PerReplicaSupply, st.TotalSupply, st.Demand, vc.TotalDemand} {
			require.False(t, math.IsNaN(v) || math.IsInf(v, 0), "cycle %d: a non-finite figure from a flat line: %+v", c, vc)
		}
		require.Positive(t, vc.PerReplicaCapacity, "cycle %d", c)
		require.Positive(t, st.Demand, "cycle %d: the k-based demand", c)
		priced++
	}
	require.Equal(t, 17, priced)
}
