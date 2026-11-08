package prefill

import (
	"bytes"
	"compress/gzip"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// olsSweepRecording is the ols-sweep learner golden's recording: one decode
// and one prefill pod, P/D on vLLM, swept from 1 to 30 requests/s and from
// 1000- to 6000-token prompts. The prefill pod never queued.
var olsSweepRecording = filepath.Join("..", "..", "engines", "analyzers", "learnreplay",
	"testdata", "goldens", "ols-sweep", "recording.jsonl.gz")

// replayOLSSweep feeds every Analyze input of the recording to an observer,
// with the settle tracker observed from the same rows as the engine observes
// it, and returns the prefill variant's last estimate, how many cycles it
// saw, and every prefill row it was fed.
func replayOLSSweep(t *testing.T) (Estimate, int, []domain.ReplicaMetrics) {
	t.Helper()
	gz, err := os.ReadFile(olsSweepRecording)
	require.NoError(t, err)
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	require.NoError(t, err)
	raw, err := io.ReadAll(zr)
	require.NoError(t, err)
	rec, err := learnrec.Read(raw)
	require.NoError(t, err)

	tr := settle.NewTracker()
	o := NewObserver()
	var est Estimate
	var all []domain.ReplicaMetrics
	cycles := 0
	for _, ev := range rec.Events {
		switch ev.Kind {
		case "settle.Observe":
			var d struct {
				Namespace, ModelID string
				Interval           time.Duration
			}
			require.NoError(t, ev.DecodeData(&d))
			tr.SetInterval(d.Interval)
			continue
		case "saturation.Analyze":
		default:
			continue
		}
		// The Analyze event as saturation records it (AnalyzeEvent); only
		// its input is read.
		var d struct {
			Input           learnrec.Input
			Records, Result learnrec.Raw
			Error           string
		}
		require.NoError(t, ev.DecodeData(&d))
		roles := map[string]string{}
		for _, vs := range d.Input.VariantStates {
			roles[vs.VariantName] = vs.Role
		}
		model := d.Input.Namespace + "/" + d.Input.ModelID
		tr.Observe(model, settlerow.Of(d.Input.ReplicaMetrics, roles), ev.Now)
		var rows []domain.ReplicaMetrics
		for _, rm := range d.Input.ReplicaMetrics {
			if domain.CanonicalRole(roles[rm.VariantName]) == domain.RolePrefill {
				rows = append(rows, rm)
			}
		}
		if len(rows) == 0 {
			continue
		}
		est = o.Observe(Cycle{
			Key: "prefill", Engine: inferenceengine.EngineVLLM, Disaggregated: true, Rows: rows,
			Settle: &Settle{View: tr, Model: model}, Now: ev.Now,
		})
		all = append(all, rows...)
		cycles++
	}
	return est, cycles, all
}

// lowLoadLine is the fixed cost of the recording's prefill pod, worked out
// apart from the observer: every prefill row that is Ready, has no queue,
// reads a computed-token rate, a request rate and a service time, and is
// occupied (request rate times service time) at most 0.1, fitted by the
// closed-form least squares of the service time against the effective prompt
// (computed tokens over requests); and the least AvgTTFT - AvgServiceTime of
// those rows. It has no settle, no steady gate and no one-sample-a-minute
// fold: what those add must not move a figure by more than the tolerance.
func lowLoadLine(rows []domain.ReplicaMetrics) (intercept, slope, overhead float64, n int) {
	var sx, sy, sxx, sxy float64
	overhead = math.Inf(1)
	for _, rm := range rows {
		if !rm.Ready || rm.QueueLength != 0 || !rm.PrefillComputedTokenRateKnown || !rm.RequestRateKnown ||
			!(rm.RequestRate > 0) || !(rm.PrefillComputedTokenRate > 0) || !(rm.AvgServiceTime > 0) ||
			rm.RequestRate*rm.AvgServiceTime > 0.1 {
			continue
		}
		x, y := rm.PrefillComputedTokenRate/rm.RequestRate, rm.AvgServiceTime
		sx, sy, sxx, sxy = sx+x, sy+y, sxx+x*x, sxy+x*y
		n++
		if rm.AvgTTFT >= rm.AvgServiceTime {
			overhead = math.Min(overhead, rm.AvgTTFT-rm.AvgServiceTime)
		}
	}
	k := float64(n)
	slope = (k*sxy - sx*sy) / (k*sxx - sx*sx)
	return (sy - slope*sx) / k, slope, overhead, n
}

// On ols-sweep, P1 fits a and refuses V_P and f
// (docs/proposals/slo-capacity-planner.md, the PR plan; the TTFT page's
// "What is derivable today": the one prefill pod never queued, so it
// supports a and the low-load slope, not V_P or f).
func TestOLSSweep_FitsAAndRefusesVPAndF(t *testing.T) {
	est, cycles, rows := replayOLSSweep(t)
	require.Equal(t, 226, cycles, "the recording's Analyze cycles")

	// a: the intercept and the overhead each against lowLoadLine's, within
	// 0.25 ms, and a as their sum: 8.36 ms plus 2.68 ms, 11.04 ms, against
	// the independent 8.46 and 2.54. (The
	// page once said 10.1 ms: that intercept was fitted against
	// AvgInputTokens, the long-window prompt, not the minute the service time
	// covers, and with no overhead.)
	intercept, slope, overhead, n := lowLoadLine(rows)
	require.GreaterOrEqual(t, n, 100, "the independent fit's rows")
	require.InDelta(t, 0.0085, intercept, 0.0005, "the independent intercept")
	require.InDelta(t, 0.0025, overhead, 0.0005, "the independent overhead")
	require.Empty(t, est.ARefused)
	assert.GreaterOrEqual(t, est.ASamples, MinFixedSamples)
	assert.True(t, est.AOverheadKnown)
	assert.InDelta(t, intercept, est.AIntercept, 0.00025, "a's intercept")
	assert.InDelta(t, overhead, est.AOverhead, 0.00025, "a's overhead")
	assert.InDelta(t, intercept+overhead, est.A, 0.00025, "a: the intercept plus the least overhead")
	assert.GreaterOrEqual(t, est.APromptMax, MinPromptSpread*est.APromptMin)
	// Two prompt levels, 1k and 6k: the curve needs three, and a is the
	// line's (fixed.go).
	assert.Equal(t, AModelLinear, est.AModel)
	assert.Equal(t, TooFewPromptClusters, est.ALineReason)
	assert.Equal(t, 2, est.AClusters)
	// The slope bound is a validation signal, far above the 30k tok/s the
	// pod was offered at its peak: about 520k tok/s.
	assert.InEpsilon(t, 1/slope, est.SlopeBound, 0.1, "the slope bound")
	assert.Greater(t, est.SlopeBound, 30000.0)

	// V_P: no busy minute (none can be recognised without the queue's
	// minimum), no queue ever, no back-pressure bound.
	assert.Equal(t, NoPrefillCapacity, est.VPRefused)
	assert.Zero(t, est.VP)
	assert.Empty(t, est.VPSource)

	// f: one prefill pod.
	assert.Equal(t, OnePod, est.FRefused)
	assert.Zero(t, est.F)
	t.Logf("ols-sweep: a %.5f s (intercept %.5f, overhead %.5f) over %d minutes, prompts %.0f..%.0f; slope bound %.0f tok/s; V_P %s; f %s",
		est.A, est.AIntercept, est.AOverhead, est.ASamples, est.APromptMin, est.APromptMax, est.SlopeBound, est.VPRefused, est.FRefused)
	t.Logf("independent: intercept %.5f, overhead %.5f, slope bound %.0f tok/s, over %d rows", intercept, overhead, 1/slope, n)
}
