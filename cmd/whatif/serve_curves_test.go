package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi/apitest"
)

// The processor-sharing fleets the curves are checked on: the worked fleet
// (prompt cv 2, so the larger of processor sharing and first come prices it)
// and the KV-pull fleet (cv 0.06, processor sharing alone).
func curveFleets() map[string]struct {
	recording, model string
	req              whatifapi.PlanRequest
} {
	return map[string]struct {
		recording, model string
		req              whatifapi.PlanRequest
	}{
		"worked": {"ols-sweep.jsonl.gz", "model-02", whatifapi.PlanRequest{
			Fleet:   &whatifapi.FleetKnob{Prefill: ip(1), Decode: ip(1)},
			Traffic: &whatifapi.Traffic{Mix: &whatifapi.Mix{PromptTokens: 1000, OutputTokens: 1000}},
			Assume:  &whatifapi.Assume{VP: fp(30000)},
			SLO: &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p95", Target: fp(0.2)},
				TPOT: &whatifapi.Objective{Statistic: "mean", Target: fp(0.004)}},
			Views: &whatifapi.ViewRequest{AllTrafficPerBucket: true},
		}},
		"pull": {"throughput-sweep.jsonl.gz", "", whatifapi.PlanRequest{
			Fleet:  &whatifapi.FleetKnob{Prefill: ip(2)},
			Assume: &whatifapi.Assume{VP: fp(30000), A: fp(0.009), PromptCV: fp(0.06)},
			SLO: &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p90", Target: fp(0.5)},
				TPOT: &whatifapi.Objective{Statistic: "mean", Target: fp(0.02)}},
		}},
	}
}

// servedFleetOf is the fleet a plan priced, read again from the same inputs.
func servedFleetOf(t *testing.T, src *servedSources, recording, model string, sc whatifapi.Scenario) whatifapi.Fleet {
	t.Helper()
	res, err := src.Resolve(context.Background(), whatifapi.InputsRequest{Model: model,
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: recording}})
	require.NoError(t, err)
	k := whatifapi.Knobs{Prefill: sc.Fleet.Prefill, Decode: sc.Fleet.Decode, Assume: sc.Assume, ArrivalCV2: sc.ArrivalCV2, Headroom: sc.Headroom}
	if !sc.Mix.Observed {
		k.PromptTokens, k.OutputTokens = sc.Mix.PromptTokens, sc.Mix.OutputTokens
	}
	if sc.TTFT.Statistic != "mean" {
		k.TTFTPercentile = percentOf(t, sc.TTFT.Statistic)
	}
	fl, err := res.Pricer.Fleet(k)
	require.NoError(t, err)
	return fl
}

// percentOf is "p95" as 0.95.
func percentOf(t *testing.T, stat string) float64 {
	t.Helper()
	q, err := parsePercent(stat)
	require.NoError(t, err)
	return q
}

// Curves agree with points: at each grid rate the curve's figure is TTFTAt's
// and SLOAtReplicas' there; point B lies on the curve, the TTFT at the limit
// at the target within the bisection's tolerance; past the lower knee every
// point is refused Saturated; and point A is priced at the rate, not read off
// the curve.
func TestServeCurvesAgreeWithPoints(t *testing.T) {
	h, src := servedAPI(t)
	for name, c := range curveFleets() {
		t.Run(name, func(t *testing.T) {
			in, _ := apiInputs(t, h, c.recording, c.model)
			req := c.req
			req.InputsID = in.InputsID
			out, body := apiPlan(t, h, req)
			assert.Empty(t, apitest.CheckFigures(body))
			fl := servedFleetOf(t, src, c.recording, c.model, out.Scenario)
			require.Len(t, out.TTFT.Line.Curve, 65)
			knee := math.Inf(1)
			for _, m := range []*whatifapi.Figure{out.TTFT.Markers.PrefillKnee, out.TTFT.Markers.DecodeKnee} {
				if m != nil && m.Value != nil {
					knee = min(knee, *m.Value)
				}
			}
			answered := 0
			for i, p := range out.TTFT.Line.Curve {
				if p.Rate >= knee {
					require.NotNil(t, p.Figure.Refused, "point %d at %g, past the knee %g", i, p.Rate, knee)
					assert.Equal(t, string(whatif.Saturated), p.Figure.Refused.Reason, "point %d", i)
					continue
				}
				want, ok := fl.TTFTAt(p.Rate).(whatif.TTFT)
				require.True(t, ok, "point %d at %g", i, p.Rate)
				require.NotNil(t, p.Figure.Value, "point %d at %g refused %+v", i, p.Rate, p.Figure.Refused)
				assert.Equal(t, want.Seconds, *p.Figure.Value, "point %d", i)
				answered++
				mean, _ := fl.TPOTAt(p.Rate)
				if m, ok := mean.(whatif.Answer); ok {
					assert.Equal(t, m.Value, *out.TPOT.Curve[i].Figure.Value, "TPOT point %d", i)
				}
			}
			assert.Greater(t, answered, 50, "too few points answered")
			// Point B on the curve.
			b := out.TTFT.Line.AtTarget
			require.NotNil(t, b.Rate.Value, "%+v", b.Rate.Refused)
			at, ok := fl.TTFTAt(*b.Rate.Value).(whatif.TTFT)
			require.True(t, ok)
			assert.InDelta(t, b.Seconds, at.Seconds, 1e-3*b.Seconds, "the TTFT at point B's rate")
			// Point A priced at the rate.
			a := out.TTFT.Line.AtRate
			want, ok := fl.TTFTAt(a.Rate).(whatif.TTFT)
			require.True(t, ok)
			assert.Equal(t, want.Seconds, *a.Figure.Value)
			for _, l := range out.TTFT.PerBucket {
				assert.Equal(t, whatifapi.SemanticsAllTraffic, l.Semantics)
				assert.NotEmpty(t, l.Curve)
			}
		})
	}
}

// The fleet's limit is capped at the operating point: with targets loose
// enough that neither binds, the cap does, the limit sits at headroom times
// the lower knee, and the figure without headroom is above it. No golden has
// the cap bind the fleet, so parity alone cannot see a lost cap.
func TestServeFleetLimitIsCapped(t *testing.T) {
	h, _ := servedAPI(t)
	c := curveFleets()["worked"]
	in, _ := apiInputs(t, h, c.recording, c.model)
	req := c.req
	req.InputsID, req.Views = in.InputsID, nil
	req.SLO = &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p95", Target: fp(10)},
		TPOT: &whatifapi.Objective{Statistic: "mean", Target: fp(1)}}
	out, body := apiPlan(t, h, req)
	assert.Empty(t, apitest.CheckFigures(body))
	capRate := out.TTFT.Markers.OperatingCap
	require.NotNil(t, capRate.Value)
	fl := out.Limits.Fleet
	require.NotNil(t, fl.Value, "%+v", fl.Refused)
	assert.True(t, fl.CapApplied, "bound by %v", fl.BoundBy)
	assert.InDelta(t, *capRate.Value, *fl.Value, 1e-9*(*capRate.Value))
	require.NotNil(t, fl.WithoutHeadroom.Value)
	assert.Greater(t, *fl.WithoutHeadroom.Value, *fl.Value)
}

// A recording is named, never pathed, and an error names no server path.
func TestServeInputsRefuseAPath(t *testing.T) {
	h, src := servedAPI(t)
	for _, name := range []string{"../ols-sweep.jsonl.gz", "/etc/passwd", "sub/run.jsonl.gz", `..\run`, ".hidden", "none.jsonl.gz", ""} {
		code, body := apiPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: name}})
		assert.Equal(t, http.StatusBadRequest, code, "%q: %s", name, body)
		assert.NotContains(t, string(body), src.recordings, "%q", name)
	}
	code, body := apiPost(t, h, "/v1/inputs", whatifapi.InputsRequest{})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, string(body), "reads no Prometheus")
	code, body = apiPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Model: "model-09",
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: "ols-sweep.jsonl.gz"}})
	assert.Equal(t, http.StatusBadRequest, code, "%s", body)
	assert.NotContains(t, string(body), src.recordings)
}

// The inputs name every term with its source, and the observed mix's
// refusal; resolving them again is the same id.
func TestServeInputs(t *testing.T) {
	h, _ := servedAPI(t)
	in, body := apiInputs(t, h, "throughput-sweep.jsonl.gz", "")
	again, _ := apiInputs(t, h, "throughput-sweep.jsonl.gz", "")
	assert.Equal(t, in.InputsID, again.InputsID)
	assert.Empty(t, apitest.CheckFigures(body))
	names := map[string]bool{}
	for _, term := range in.Terms {
		names[term.Name] = true
	}
	for _, n := range []string{"itlA", "itlB", "capacityTokens", "kvPerRequest", "batchCap", "tpotRatio"} {
		assert.True(t, names[n], "term %s: %s", n, body)
	}
	require.NotNil(t, in.Observed.Mix.Refused)
	assert.Equal(t, reasonNoPerRequestLengths, in.Observed.Mix.Refused.Reason)
	assert.Len(t, in.Observed.Buckets, 5)
	assert.Equal(t, whatifapi.BucketOf(*in.Observed.PromptTokens.Value), in.Observed.PromptBucket)
}

// serve refuses to start where it would serve without authentication off
// loopback, or a token in the clear.
func TestServeRefusesUnsafeBinds(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"serve", "--recordings", dir, "--listen", "0.0.0.0:0"}, "--auth=kubernetes"},
		{[]string{"serve", "--recordings", dir, "--listen", ":0"}, "not a loopback address"},
		{[]string{"serve", "--recordings", dir, "--listen", "0.0.0.0:0", "--auth", "kubernetes"}, "--tls-cert-file"},
		{[]string{"serve", "--listen", "127.0.0.1:0"}, "--prometheus, --recordings"},
		{[]string{"serve", "--recordings", dir, "--from", "2026-11-09T10:00:00Z"}, "each request names"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := run(ctx, c.args, io.Discard, io.Discard)
		cancel()
		require.Error(t, err, "%v", c.args)
		var u usageError
		assert.True(t, errors.As(err, &u), "%v: %v", c.args, err)
		assert.True(t, strings.Contains(err.Error(), c.want), "%v: %v", c.args, err)
	}
}

// Plans priced at once on one inputs entry share its replayed state: under
// -race, nothing they price writes it, and each gets its own scenario's
// answer.
func TestServeConcurrentPlans(t *testing.T) {
	h, _ := servedAPI(t)
	in, _ := apiInputs(t, h, "short.jsonl.gz", "")
	plan := func(rate float64) whatifapi.PlanRequest {
		return whatifapi.PlanRequest{InputsID: in.InputsID, Assume: &whatifapi.Assume{VP: fp(130000)},
			Traffic: &whatifapi.Traffic{Classes: &whatifapi.Classes{Critical: &whatifapi.ClassRate{Rate: fp(rate)}}},
			SLO:     &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p90", Target: fp(1)}}}
	}
	rates := []float64{1, 2, 3, 4}
	got := make([][]byte, len(rates))
	var wg sync.WaitGroup
	for i, r := range rates {
		wg.Go(func() {
			code, body := apiPost(t, h, "/v1/plan", plan(r))
			if code == http.StatusOK {
				got[i] = body
			}
		})
	}
	wg.Wait()
	// The same plans, one at a time on a server of their own.
	alone, _ := servedAPI(t)
	again, _ := apiInputs(t, alone, "short.jsonl.gz", "")
	require.Equal(t, in.InputsID, again.InputsID)
	for i, r := range rates {
		require.NotNil(t, got[i], "rate %g", r)
		_, body := apiPlan(t, alone, plan(r))
		assert.Equal(t, string(body), string(got[i]), "rate %g", r)
	}
}

// safeBuffer is a buffer one goroutine writes and another reads.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// serve answers on loopback with no token, and stops when its context ends.
func TestServeServesLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var warn safeBuffer
	go func() {
		done <- run(ctx, []string{"serve", "--recordings", t.TempDir(), "--listen", "127.0.0.1:0"}, io.Discard, &warn)
	}()
	var addr string
	require.Eventually(t, func() bool {
		s := warn.String()
		_, rest, ok := strings.Cut(s, "API on ")
		addr, _, _ = strings.Cut(rest, " ")
		return ok && addr != ""
	}, 5*time.Second, 10*time.Millisecond)
	resp, err := http.Post("http://"+addr+"/v1/plan", "application/json", strings.NewReader(`{"inputsId": "sha256:none"}`))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}
