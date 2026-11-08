package saturation

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// a's curved fit through Analyze (SwitchPrefillAQuadratic): a prefill pod at
// low load swept over 1k, 8k, 30k and 37k prompts, its service time convex in
// the prompt as run 3 of the TTFT recordings measured it.

// lowLoadPrefillCycle runs one cycle of the harness's P/D model with the
// prefill pod at low load on prompts of ieff tokens, and returns its
// observed line.
func (h *observeHarness) lowLoadPrefillCycle(ieff float64) string {
	h.t.Helper()
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	dec := makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500)
	pre := makeReplicaMetrics("prefill-0", "prefill", 0, 16000, 0, ieff, 1)
	svc := 0.0028 + 1.734e-6*ieff + 1.727e-10*ieff*ieff
	pre.RequestRate, pre.RequestRateKnown = 0.02, true
	pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = 0.02*ieff, true
	pre.AvgServiceTime, pre.AvgTTFT = svc, svc+0.0025
	h.lines = nil
	_, err := h.a.Analyze(h.ctx, makeAnalyzerInput([]domain.ReplicaMetrics{dec, pre}, states))
	require.NoError(h.t, err)
	h.clock = h.clock.Add(time.Minute)
	obs := observedLines(h.lines)
	require.Len(h.t, obs, 1)
	return obs[0]
}

// aQuadRun drives a harness through five minutes at each of 1k and run 3's
// levels with SwitchPrefillAQuadratic set to on, and returns the last line.
func aQuadRun(t *testing.T, on bool) string {
	t.Helper()
	h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
	require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillAQuadratic: on}))
	var last string
	for _, l := range []float64{1000, 8000, 30000, 37000} {
		for range 5 {
			last = h.lowLoadPrefillCycle(l)
		}
	}
	return last
}

// On, a is the curve's intercept, 2.8 ms, plus the 2.5 ms overhead, and the
// line says so; off, a is the line's, which meets the axis below zero, and
// is refused as P1 shipped.
func TestPrefillAQuad_TheSwitchFitsTheCurve(t *testing.T) {
	on := aQuadRun(t, true)
	assert.Contains(t, on, `"aModel"="quadratic"`)
	assert.Contains(t, on, `"aLineReason"=""`)
	assert.Contains(t, on, `"aClusters"=4`)
	assert.Contains(t, on, `"aRefused"=""`)
	m := regexp.MustCompile(`"aIntercept"=([0-9.e+-]+)`).FindStringSubmatch(on)
	require.Len(t, m, 2, on)
	intercept, err := strconv.ParseFloat(m[1], 64)
	require.NoError(t, err)
	assert.InDelta(t, 0.0028, intercept, 1e-9)

	off := aQuadRun(t, false)
	assert.Contains(t, off, `"aModel"="linear"`)
	assert.Contains(t, off, `"aLineReason"="LineOnly"`)
	assert.Contains(t, off, `"aRefused"="NonPositiveIntercept"`)
	assert.Contains(t, off, `"a"=0 "aIntercept"=0 `)
}

// recordedPrefillRows is runs 3 and 4 of the TTFT recordings, as the prefill
// package's fixture holds them (internal/signals/prefill/testdata): one P/D
// prefill pod of Qwen3-0.6B at TP1 on an H200, at low load, run 3 over 8k,
// 30k and 37k prompts and run 4 over 1k, 2k, 4k and 16k, a cycle every 15 s.
// Run 4 is moved to start five minutes after run 3 ends, so both are inside
// prefill.Retention when it ends.
func recordedPrefillRows(t *testing.T) (times []time.Time, rows []domain.ReplicaMetrics) {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "signals", "prefill", "testdata", "run34-prefill-rows.csv"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	recs, err := csv.NewReader(f).ReadAll()
	require.NoError(t, err)
	require.Equal(t, "run", recs[0][0])
	num := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		require.NoError(t, err)
		return v
	}
	var shift time.Duration
	var last time.Time
	prev := ""
	for _, r := range recs[1:] {
		now, err := time.Parse(time.RFC3339Nano, r[1])
		require.NoError(t, err)
		since, err := time.Parse(time.RFC3339Nano, r[3])
		require.NoError(t, err)
		if r[0] != prev {
			if !last.IsZero() {
				shift = last.Add(5 * time.Minute).Sub(now)
			}
			prev = r[0]
		}
		now, since = now.Add(shift), since.Add(shift)
		rm := makeReplicaMetrics("prefill-"+r[0], "prefill", 0, 16000, int(num(r[5])), num(r[8]), 1)
		rm.Ready, rm.ReadySince = r[2] == "true", since
		rm.KvCacheUsage, rm.KvCacheUsageKnown = num(r[4]), true
		rm.RequestRate, rm.RequestRateKnown = num(r[6]), true
		rm.PrefillComputedTokenRate, rm.PrefillComputedTokenRateKnown = num(r[7]), true
		rm.AvgInputTokensRecent = num(r[8])
		rm.AvgServiceTime, rm.AvgTTFT = num(r[9]), num(r[10])
		times, rows = append(times, now), append(rows, rm)
		last = now
	}
	return times, rows
}

// recordedRun replays the recorded prefill rows through Analyze, beside an
// idle decode pod, with SwitchPrefillAQuadratic set to on and the engine's
// settle tracker observed every cycle at the recordings' 15 s, and returns
// the last observed line.
func recordedRun(t *testing.T, on bool) string {
	t.Helper()
	times, rows := recordedPrefillRows(t)
	require.Len(t, rows, 198)
	h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
	tr := settle.NewTracker()
	tr.SetInterval(15 * time.Second)
	h.a.WithSettleView(tr)
	require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillAQuadratic: on}))
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	roles := map[string]string{"decode": domain.RoleDecode, "prefill": domain.RolePrefill}
	var last string
	for i, pre := range rows {
		h.clock = times[i]
		dec := makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500)
		dec.ReadySince = times[0].Add(-time.Hour)
		in := []domain.ReplicaMetrics{dec, pre}
		tr.Observe(settleModel("test-ns", "test-model"), settlerow.Of(in, roles), times[i])
		h.lines = nil
		_, err := h.a.Analyze(h.ctx, makeAnalyzerInput(in, states))
		require.NoError(t, err)
		obs := observedLines(h.lines)
		require.Len(t, obs, 1)
		last = obs[0]
	}
	return last
}

// Runs 3 and 4's real rows through Analyze: on, the observed line reads the
// curve's intercept at 7.95 ms (the prefill package's
// TestQuadratic_Runs3And4PooledGiveA, through the observer alone), its
// standard error and seven clusters; off, the line P1 shipped refuses the
// same minutes.
func TestPrefillAQuad_TheRecordingsThroughAnalyze(t *testing.T) {
	on := recordedRun(t, true)
	assert.Contains(t, on, `"aModel"="quadratic"`)
	assert.Contains(t, on, `"aClusters"=7`)
	assert.Contains(t, on, `"aRefused"=""`)
	assert.Contains(t, on, `"aExtrapolated"=false`)
	assert.Contains(t, on, `"aSamples"=31`)
	field := func(name string) float64 {
		m := regexp.MustCompile(`"` + name + `"=([0-9.e+-]+)`).FindStringSubmatch(on)
		require.Len(t, m, 2, "%s in %s", name, on)
		v, err := strconv.ParseFloat(m[1], 64)
		require.NoError(t, err)
		return v
	}
	assert.InDelta(t, 0.00795, field("aIntercept"), 0.00002)
	assert.InDelta(t, 0.00047, field("aInterceptSE"), 0.00002)

	off := recordedRun(t, false)
	assert.Contains(t, off, `"aModel"="linear"`)
	assert.Contains(t, off, `"aLineReason"="LineOnly"`)
	assert.Contains(t, off, `"aRefused"="NonPositiveIntercept"`)
}
