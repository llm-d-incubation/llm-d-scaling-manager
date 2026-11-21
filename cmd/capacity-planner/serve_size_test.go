package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi/apitest"
)

// apiSize sizes a fleet and decodes the response strictly.
func apiSize(t *testing.T, h http.Handler, req whatifapi.SizeRequest) whatifapi.SizeResponse {
	t.Helper()
	code, body := apiPost(t, h, "/v1/size", req)
	require.Equal(t, http.StatusOK, code, "%s", body)
	assert.Empty(t, apitest.CheckFigures(body), "%s", body)
	var out whatifapi.SizeResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&out))
	return out
}

// sizeOfPlan is a plan request's question as a size at rate, bounded at
// bound replicas a role.
func sizeOfPlan(req whatifapi.PlanRequest, rate float64, bound int) whatifapi.SizeRequest {
	traffic := whatifapi.Traffic{Classes: &whatifapi.Classes{Critical: &whatifapi.ClassRate{Rate: fp(rate)}}}
	if req.Traffic != nil {
		traffic.Mix, traffic.ArrivalCV2 = req.Traffic.Mix, req.Traffic.ArrivalCV2
	}
	return whatifapi.SizeRequest{InputsID: req.InputsID, Traffic: &traffic, SLO: req.SLO, Assume: req.Assume,
		Headroom: req.Headroom, MaxReplicas: &whatifapi.FleetKnob{Prefill: ip(bound), Decode: ip(bound)}}
}

// planOfSize is the plan a size asks of one fleet.
func planOfSize(req whatifapi.SizeRequest, n whatifapi.FleetCounts) whatifapi.PlanRequest {
	return whatifapi.PlanRequest{InputsID: req.InputsID, Fleet: &whatifapi.FleetKnob{Prefill: ip(n.Prefill), Decode: ip(n.Decode)},
		Traffic: req.Traffic, SLO: req.SLO, Assume: req.Assume, Headroom: req.Headroom}
}

// On the engine itself, at the worked fleet's question and two and a half
// times the rate one replica of each role carries: the size's answer is what
// POST /v1/plan says at its fleet, and one replica fewer of either role
// misses the rate in the plan too.
func TestServeSizeIsThePlan(t *testing.T) {
	h, _ := servedAPI(t)
	in, _ := apiInputs(t, h, "ols-sweep.jsonl.gz", "model-02")
	base := workedRate()
	base.InputsID = in.InputsID
	one, _ := apiPlan(t, h, base)
	require.NotNil(t, one.Limits.Fleet.Value, "the worked fleet's limit: %+v", one.Limits.Fleet)
	rate := 2.5 * *one.Limits.Fleet.Value

	req := sizeOfPlan(base, rate, 8)
	out := apiSize(t, h, req)
	require.Nil(t, out.Unmet, "%+v", out.Unmet)
	require.NotNil(t, out.Answer)
	a := out.Answer
	t.Logf("%.4g r/s: %d prefill, %d decode, limit %.4g bound by %v; %d fleets searched",
		rate, a.Fleet.Prefill, a.Fleet.Decode, *a.Limit.Value, a.Limit.BoundBy, out.Searched)
	assert.Greater(t, a.Fleet.Prefill+a.Fleet.Decode, 2, "a rate past one of each needs more")

	p, body := apiPlan(t, h, planOfSize(req, a.Fleet))
	want, err := json.MarshalIndent(a.Plan, "", "  ")
	require.NoError(t, err)
	assert.JSONEq(t, string(body), string(want), "the answer's plan is not POST /v1/plan's at its fleet")
	assert.Equal(t, p.Limits.Fleet.Figure, a.Limit)
	assert.Equal(t, p.Limits.Fleet.Binding, a.Binding)
	require.NotNil(t, a.Limit.Value)
	assert.GreaterOrEqual(t, *a.Limit.Value, rate)

	for _, s := range []struct {
		role string
		step *whatifapi.SizeStep
	}{{"prefill", a.FewerPrefill}, {"decode", a.FewerDecode}} {
		if s.step == nil {
			continue
		}
		require.NotNil(t, s.step.Limit.Value, "%s: %+v", s.role, s.step.Limit)
		assert.Less(t, *s.step.Limit.Value, rate, "%s: one fewer reaches the rate", s.role)
		less, _ := apiPlan(t, h, planOfSize(req, s.step.Fleet))
		require.NotNil(t, less.Limits.Fleet.Value)
		assert.InDelta(t, *less.Limits.Fleet.Value, *s.step.Limit.Value, 1e-9*rate, "%s: the step is not the plan's figure", s.role)
		assert.Equal(t, less.Limits.Fleet.BoundBy, s.step.Limit.BoundBy, s.role)
	}
	assert.True(t, a.FewerPrefill != nil || a.FewerDecode != nil, "no step at all: the test reads nothing")
}

// sizeBenchRequest is the size the proposal's cost figures are measured on:
// the worked recording (model-02), V_P assumed 30000 tokens/s, a stated
// 1,024-token prompt and 1,000-token output, TTFT p95 200 ms, TPOT mean 4 ms,
// at 0.6 r/s, bounded at 32 a role.
func sizeBenchRequest(id string) whatifapi.SizeRequest {
	return whatifapi.SizeRequest{InputsID: id,
		Traffic: &whatifapi.Traffic{Classes: &whatifapi.Classes{Critical: &whatifapi.ClassRate{Rate: fp(0.6)}},
			Mix: &whatifapi.Mix{PromptTokens: 1024, OutputTokens: 1000}},
		SLO:    &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p95", Target: fp(0.2)}, TPOT: &whatifapi.Objective{Statistic: statMean, Target: fp(0.004)}},
		Assume: &whatifapi.Assume{VP: fp(30000)}}
}

// The size's costs, on sizeBenchRequest's question:
//
//	go test ./cmd/capacity-planner -run '^$' -bench Size -benchtime 3x
//
// SizeFleetLimits is one fleet's limits as the search prices them, at n
// prefill replicas and one decode; SizeCold a size end to end on a new
// server, the answer's cold plan included; SizeAnswerPlan that plan alone.
func BenchmarkSizeFleetLimits(b *testing.B) {
	_, src := servedAPI(b)
	res, err := src.Resolve(context.Background(), whatifapi.InputsRequest{Model: "model-02",
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: "ols-sweep.jsonl.gz"}})
	require.NoError(b, err)
	for _, n := range []int{1, 32, 64} {
		b.Run(fmt.Sprintf("prefill=%d", n), func(b *testing.B) {
			for b.Loop() {
				fl, err := res.Pricer.Fleet(whatifapi.Knobs{Prefill: n, Decode: 1, PromptTokens: 1024, OutputTokens: 1000,
					TTFTPercentile: 0.95, Assume: whatifapi.Assume{VP: fp(30000)}, Headroom: 0.85})
				require.NoError(b, err)
				fl.PlainLimits(0.2, 0.004, 0.85)
			}
		})
	}
}

func BenchmarkSizeCold(b *testing.B) {
	for b.Loop() {
		b.StopTimer()
		h, _ := servedAPI(b)
		in, _ := apiInputs(b, h, "ols-sweep.jsonl.gz", "model-02")
		b.StartTimer()
		code, body := apiPost(b, h, "/v1/size", sizeBenchRequest(in.InputsID))
		require.Equal(b, http.StatusOK, code, "%s", body)
		var out whatifapi.SizeResponse
		require.NoError(b, json.Unmarshal(body, &out))
		require.NotNil(b, out.Answer)
		b.ReportMetric(float64(out.Searched), "fleets")
		b.ReportMetric(float64(out.Answer.Fleet.Prefill), "prefill")
		b.ReportMetric(float64(out.Answer.Fleet.Decode), "decode")
	}
}

func BenchmarkSizeAnswerPlan(b *testing.B) {
	h, _ := servedAPI(b)
	in, _ := apiInputs(b, h, "ols-sweep.jsonl.gz", "model-02")
	req := sizeBenchRequest(in.InputsID)
	code, body := apiPost(b, h, "/v1/size", req)
	require.Equal(b, http.StatusOK, code, "%s", body)
	var out whatifapi.SizeResponse
	require.NoError(b, json.Unmarshal(body, &out))
	require.NotNil(b, out.Answer)
	n := out.Answer.Fleet
	for b.Loop() {
		b.StopTimer()
		h, _ := servedAPI(b)
		in, _ := apiInputs(b, h, "ols-sweep.jsonl.gz", "model-02")
		b.StartTimer()
		apiPlan(b, h, planOfSize(sizeBenchRequest(in.InputsID), n))
	}
	b.ReportMetric(float64(n.Prefill), "prefill")
}

// Sized on the worst corner (SizeOnLow), the benchmark's question has a TTFT floor above
// its 200 ms target: the size is that refusal, at any count, never the bound
// reached with a shortfall that points the wrong way. Since the planner's a reads the
// median overhead (issue llm-d-incubation/llm-d-scaling-manager#306), the
// floor at the learned figures is above it too, 200.7 ms, so the refusal is
// the base figure's, not only the corner's.
func TestServeSizeLowUnderTheFloor(t *testing.T) {
	h, _ := servedAPI(t)
	in, _ := apiInputs(t, h, "ols-sweep.jsonl.gz", "model-02")
	req := sizeBenchRequest(in.InputsID)
	req.SizeOn = whatifapi.SizeOnLow
	req.MaxReplicas = &whatifapi.FleetKnob{Prefill: ip(4), Decode: ip(4)}
	out := apiSize(t, h, req)
	require.Nil(t, out.Answer)
	require.NotNil(t, out.Unmet)
	t.Logf("%s: %s", out.Unmet.Refused.Reason, out.Unmet.Refused.Detail)
	assert.Equal(t, "TargetBelowFloor", out.Unmet.Refused.Reason)
	assert.Contains(t, out.Unmet.Refused.Detail, "at or below the TTFT floor")
	assert.NotContains(t, out.Unmet.Refused.Detail, "short of")
}

// A recording that learned no V_P and no decode line sizes nothing: the
// refusal is the engine's. With no TPOT asked, the decode knee is what bounds
// decode, so it is named first, as at-replicas names it.
func TestServeSizeRefusesATermRefused(t *testing.T) {
	h, _ := servedAPI(t)
	in, _ := apiInputs(t, h, "short.jsonl.gz", "")
	out := apiSize(t, h, whatifapi.SizeRequest{InputsID: in.InputsID,
		Traffic:     &whatifapi.Traffic{Classes: &whatifapi.Classes{Critical: &whatifapi.ClassRate{Rate: fp(1)}}},
		SLO:         &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: statMean, Target: fp(1)}, TPOT: &whatifapi.Objective{Statistic: "none"}},
		MaxReplicas: &whatifapi.FleetKnob{Prefill: ip(4), Decode: ip(4)}})
	assert.Nil(t, out.Answer)
	require.NotNil(t, out.Unmet)
	assert.Equal(t, "decode", out.Unmet.Role)
	assert.Equal(t, "NoLine", out.Unmet.Refused.Reason)
	assert.NotEmpty(t, out.Unmet.Refused.Detail)
	assert.NotNil(t, out.Unmet.AtBound.Limit.Refused)
	t.Logf("refused: %s: %s", out.Unmet.Refused.Reason, out.Unmet.Refused.Detail)
}
