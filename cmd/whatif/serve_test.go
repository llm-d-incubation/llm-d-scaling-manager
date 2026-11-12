package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi/apitest"
)

// servedRecordings are the goldens' recordings, by the name a recording
// source gives them.
var servedRecordings = map[string]string{
	"ols-sweep.jsonl.gz":        olsSweep,
	"throughput-sweep.jsonl.gz": outputSpreadRun,
	"short.jsonl.gz":            shortRun,
	"saturated.jsonl.gz":        saturatedRun,
}

// servedAPI is the API on a directory holding the goldens' recordings, and
// the directory.
func servedAPI(t testing.TB) (http.Handler, *servedSources) {
	t.Helper()
	dir := t.TempDir()
	for name, from := range servedRecordings {
		b, err := os.ReadFile(from)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), b, 0o644))
	}
	src := &servedSources{recordings: dir, warn: io.Discard}
	return whatifapi.New(whatifapi.Config{Resolver: src, WhatifSchema: "schema-v3"}).Handler(), src
}

// apiPost posts body and returns the status and body.
func apiPost(t testing.TB, h http.Handler, path string, body any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)))
	return rec.Code, rec.Body.Bytes()
}

// apiInputs resolves a recording for a model and returns the inputs.
func apiInputs(t testing.TB, h http.Handler, name, model string) (whatifapi.InputsResponse, []byte) {
	t.Helper()
	code, body := apiPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Model: model,
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: name}})
	require.Equal(t, http.StatusOK, code, "%s", body)
	var in whatifapi.InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	return in, body
}

// apiPlan prices a plan and decodes it strictly.
func apiPlan(t testing.TB, h http.Handler, req whatifapi.PlanRequest) (whatifapi.PlanResponse, []byte) {
	t.Helper()
	code, body := apiPost(t, h, "/v1/plan", req)
	require.Equal(t, http.StatusOK, code, "%s", body)
	var out whatifapi.PlanResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&out))
	return out, body
}

// The words a plan request and a golden's roles use.
const (
	statMean   = "mean"
	roleDecode = "decode"
)

func fp(x float64) *float64 { return &x }
func ip(n int) *int         { return &n }

// readGolden decodes a JSON golden.
func readGolden(t *testing.T, name string) wireOutput {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".golden"))
	require.NoError(t, err)
	return decodeOutput(t, strings.ReplaceAll(string(b), "\r\n", "\n"))
}

// role is the golden's role of that name.
func role(t *testing.T, w wireOutput, name string) wireRole {
	t.Helper()
	i := slices.IndexFunc(w.Roles, func(r wireRole) bool { return r.Role == name })
	require.GreaterOrEqual(t, i, 0, "no role %s", name)
	return w.Roles[i]
}

// wantOf is a CLI result as the API writes the same figure.
func wantOf(r *wireResult) whatifapi.Figure {
	if r.Refused != nil {
		return whatifapi.Figure{Refused: refusalWant(r.Refused)}
	}
	a := r.Answer
	f := whatifapi.Figure{Value: fp(a.Value), BoundBy: a.Basis.BoundBy, Flags: a.Flags, Sensitivity: spanWant(a.Sensitivity, a.Unit == unitSeconds)}
	if a.Band != nil {
		f.Band = &whatifapi.Band{Lo: a.Band.Lo, Hi: a.Band.Hi}
	}
	if a.Basis.Uncapped > 0 {
		f.Uncapped = fp(a.Basis.Uncapped)
	}
	if a.Basis.Floor > 0 {
		f.Floor = fp(a.Basis.Floor)
	}
	if a.Basis.PrefillOnlyRate > 0 {
		f.PrefillOnlyRate = fp(a.Basis.PrefillOnlyRate)
	}
	return f
}

// wantTTFT is a CLI TTFT as the API writes it.
func wantTTFT(r *wireTTFTResult) whatifapi.Figure {
	if r.Refused != nil {
		return whatifapi.Figure{Refused: refusalWant(r.Refused)}
	}
	a := r.Answer
	f := whatifapi.Figure{Value: fp(a.Value), Flags: a.Flags, Sensitivity: spanWant(a.Sensitivity, true)}
	if a.Basis.Floor > 0 {
		f.Floor = fp(a.Basis.Floor)
	}
	return f
}

func refusalWant(r *wireRefusal) *whatifapi.Refusal {
	return &whatifapi.Refusal{Reason: r.Reason, Detail: r.Detail, Flags: r.Flags, AssumedTermDecides: r.AssumedTermDecides}
}

func spanWant(s *wireSensitivity, worseIsHigh bool) *whatifapi.Span {
	if s == nil {
		return nil
	}
	out := &whatifapi.Span{Lo: fp(s.Lo), Hi: fp(s.Hi)}
	switch {
	case s.Refused == "":
	case worseIsHigh:
		out.Hi, out.HiRefused = nil, s.Refused
	default:
		out.Lo, out.LoRefused = nil, s.Refused
	}
	return out
}

// sameFigure holds got to want, but for the unit, which the CLI writes on the
// role: every number, bound, flag and refusal.
func sameFigure(t *testing.T, what string, want whatifapi.Figure, got whatifapi.Figure) {
	t.Helper()
	got.Unit = ""
	if len(want.Flags) == 0 {
		want.Flags = nil
	}
	if len(want.BoundBy) == 0 {
		want.BoundBy = nil
	}
	if want.Refused != nil && len(want.Refused.Flags) == 0 {
		want.Refused.Flags = nil
	}
	assert.Equal(t, want, got, "%s", what)
}

// sameAtHeadroomRate holds a decode figure replicas-to-slo read at the rate
// its headroom set to the API's at that rate given: the same value and
// refusal. The band is not compared: the CLI resamples the rate with the line
// (the operating rate moves with each resampled knee), the API prices a rate
// given, as replicas-to-slo --rate does. Nor are the flags the prefill knee
// lends a figure read at its rate (operatingTTFT.onDecode): with a rate
// given, no knee set it.
func sameAtHeadroomRate(t *testing.T, what string, want whatifapi.Figure, got whatifapi.Figure, lent []string) {
	t.Helper()
	if want.Refused != nil {
		sameFigure(t, what, want, got)
		return
	}
	require.NotNil(t, got.Value, "%s: refused %+v", what, got.Refused)
	assert.Equal(t, *want.Value, *got.Value, "%s", what)
	for _, f := range want.Flags {
		if !slices.Contains(got.Flags, f) {
			assert.Contains(t, lent, f, "%s: flag %s neither priced nor the prefill knee's", what, f)
		}
	}
	for _, f := range got.Flags {
		assert.Contains(t, want.Flags, f, "%s: flag %s the CLI does not print", what, f)
	}
}

// parityCase is one CLI JSON golden and the plan that asks its question.
type parityCase struct {
	golden, recording, model string
	// plan is the request, but for inputsId; ratePrefill takes the rate from
	// the prefill role's operating point, else from decode's.
	plan func(rate float64) whatifapi.PlanRequest
	// question is slo-to-rate (the limits) or replicas-to-slo (points at the
	// operating rate).
	question string
}

// parityCases are every JSON golden in testdata: each, priced through the API,
// is the same figures (TestServeParity).
func parityCases() []parityCase {
	worked := &whatifapi.Traffic{Mix: &whatifapi.Mix{PromptTokens: 1000, OutputTokens: 1000}}
	workedFleet := &whatifapi.FleetKnob{Prefill: ip(1), Decode: ip(1)}
	vp := func(x float64) *whatifapi.Assume { return &whatifapi.Assume{VP: fp(x)} }
	at := func(rate float64, t *whatifapi.Traffic) *whatifapi.Traffic {
		out := whatifapi.Traffic{Classes: &whatifapi.Classes{Critical: &whatifapi.ClassRate{Rate: fp(rate)}}}
		if t != nil {
			out.Mix, out.ArrivalCV2 = t.Mix, t.ArrivalCV2
		}
		return &out
	}
	obj := func(stat string, target float64) *whatifapi.Objective {
		return &whatifapi.Objective{Statistic: stat, Target: fp(target)}
	}
	return []parityCase{
		{"replicas-to-slo-worked-json", "ols-sweep.jsonl.gz", "model-02", func(r float64) whatifapi.PlanRequest {
			return whatifapi.PlanRequest{Traffic: at(r, worked), SLO: &whatifapi.SLORequest{TTFT: obj("p90", 1), TPOT: obj("p90", 0.01)},
				Assume: &whatifapi.Assume{TPOTRatio: fp(1.3)}}
		}, replicasQuestion},
		{"slo-to-rate-worked-json", "ols-sweep.jsonl.gz", "", func(float64) whatifapi.PlanRequest {
			return whatifapi.PlanRequest{Traffic: worked, SLO: &whatifapi.SLORequest{TTFT: obj("p90", 1), TPOT: obj(statMean, 0.004)}}
		}, rateQuestion},
		{"replicas-to-slo-output-spread-json", "throughput-sweep.jsonl.gz", "", func(r float64) whatifapi.PlanRequest {
			return whatifapi.PlanRequest{Traffic: at(r, nil), SLO: &whatifapi.SLORequest{TTFT: obj("p90", 1), TPOT: obj(statMean, 0.01)}}
		}, replicasQuestion},
		{"ttft-replicas-worked-json", "ols-sweep.jsonl.gz", "model-02", func(r float64) whatifapi.PlanRequest {
			return whatifapi.PlanRequest{Fleet: workedFleet, Traffic: at(r, worked), Assume: vp(30000),
				SLO: &whatifapi.SLORequest{TTFT: obj("p90", 1), TPOT: obj(statMean, 0.01)}}
		}, replicasQuestion},
		{"ttft-rate-worked-json", "ols-sweep.jsonl.gz", "model-02", func(float64) whatifapi.PlanRequest {
			return whatifapi.PlanRequest{Fleet: workedFleet, Traffic: worked, Assume: vp(30000),
				SLO: &whatifapi.SLORequest{TTFT: obj("p95", 0.2), TPOT: obj(statMean, 0.004)}}
		}, rateQuestion},
		{"ttft-replicas-stated-inputs-json", "ols-sweep.jsonl.gz", "model-02", func(r float64) whatifapi.PlanRequest {
			stated := &whatifapi.Traffic{Mix: worked.Mix, ArrivalCV2: fp(1.5)}
			return whatifapi.PlanRequest{Fleet: workedFleet, Traffic: at(r, stated), Assume: &whatifapi.Assume{VP: fp(30000), PromptCV: fp(1)},
				SLO: &whatifapi.SLORequest{TTFT: obj("p90", 1), TPOT: obj(statMean, 0.01)}}
		}, replicasQuestion},
		{"ttft-replicas-short-json", "short.jsonl.gz", "", func(r float64) whatifapi.PlanRequest {
			return whatifapi.PlanRequest{Traffic: at(r, nil), Assume: vp(130000), SLO: &whatifapi.SLORequest{TTFT: obj("", 1), TPOT: obj(statMean, 0.01)}}
		}, replicasQuestion},
		{"ttft-no-vp-json", "short.jsonl.gz", "", func(float64) whatifapi.PlanRequest {
			// Neither knee is answered, so the rate is given; the limits do not
			// read it.
			return whatifapi.PlanRequest{Traffic: at(1, nil), SLO: &whatifapi.SLORequest{TTFT: obj(statMean, 1), TPOT: &whatifapi.Objective{Statistic: "none"}}}
		}, rateQuestion},
		{"ttft-replicas-pull-json", "throughput-sweep.jsonl.gz", "", func(r float64) whatifapi.PlanRequest {
			return whatifapi.PlanRequest{Fleet: &whatifapi.FleetKnob{Prefill: ip(2)}, Traffic: at(r, nil),
				Assume: &whatifapi.Assume{VP: fp(30000), A: fp(0.009), PromptCV: fp(0.06)},
				SLO:    &whatifapi.SLORequest{TTFT: obj("p90", 1), TPOT: obj(statMean, 0.01)}}
		}, replicasQuestion},
		{"ttft-assume-a-json", "saturated.jsonl.gz", "", func(r float64) whatifapi.PlanRequest {
			return whatifapi.PlanRequest{Traffic: at(r, nil), Assume: &whatifapi.Assume{A: fp(0.011)},
				SLO: &whatifapi.SLORequest{TTFT: obj("p95", 1), TPOT: obj(statMean, 0.01)}}
		}, replicasQuestion},
	}
}

// Every JSON golden in testdata has its parity case.
func TestServeParityCoversEveryJSONGolden(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*-json.golden"))
	require.NoError(t, err)
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(p), ".golden"))
	}
	cases := parityCases()
	covered := make([]string, 0, len(cases))
	for _, c := range cases {
		covered = append(covered, c.golden)
	}
	slices.Sort(names)
	slices.Sort(covered)
	require.Len(t, names, 10, "the JSON goldens")
	assert.Equal(t, names, covered)
}

// Parity with the CLI: each JSON golden's question, asked of the API, gives
// the same limits, points, bounds, flags and refusals. A drift between the
// API and the CLI fails here.
func TestServeParity(t *testing.T) {
	h, _ := servedAPI(t)
	for _, c := range parityCases() {
		t.Run(c.golden, func(t *testing.T) {
			w := readGolden(t, c.golden)
			in, inBody := apiInputs(t, h, c.recording, c.model)
			assert.Empty(t, apitest.CheckFigures(inBody))
			if c.question == rateQuestion {
				req := c.plan(0)
				req.InputsID = in.InputsID
				out, body := apiPlan(t, h, req)
				assert.Empty(t, apitest.CheckFigures(body))
				if i := slices.IndexFunc(w.Roles, func(r wireRole) bool { return r.Role == roleDecode }); i >= 0 {
					require.NotNil(t, out.Limits.Decode)
					sameFigure(t, "decode limit", wantOf(w.Roles[i].Results.Rate), *out.Limits.Decode)
					sameFigure(t, "TPOT point B", wantOf(w.Roles[i].Results.Rate), out.TPOT.AtTarget.Rate)
				} else {
					assert.Nil(t, out.Limits.Decode, "no TPOT asked, no decode limit")
				}
				// A golden with no TTFT asked has no prefill or fleet role: the
				// API always asks one, at its own target, which the CLI did not.
				if !slices.ContainsFunc(w.Roles, func(r wireRole) bool { return r.Role == rolePrefill }) {
					return
				}
				pr := role(t, w, rolePrefill)
				sameFigure(t, "prefill limit", wantOf(pr.Results.Rate), out.Limits.Prefill)
				sameFigure(t, "TTFT point B", wantOf(pr.Results.Rate), out.TTFT.Line.AtTarget.Rate)
				sameFigure(t, "fleet limit", wantOf(role(t, w, "fleet").Results.Rate), out.Limits.Fleet.Figure)
				return
			}
			parityAtRate(t, h, in, c, w)
		})
	}
}

// parityAtRate holds a replicas-to-slo golden to the API priced at its
// operating rate: the knees, the TTFT and the TPOTs there.
func parityAtRate(t *testing.T, h http.Handler, in whatifapi.InputsResponse, c parityCase, w wireOutput) {
	t.Helper()
	d := role(t, w, roleDecode)
	var rate float64
	var pr *wireRole
	if i := slices.IndexFunc(w.Roles, func(r wireRole) bool { return r.Role == rolePrefill }); i >= 0 {
		pr = &w.Roles[i]
	}
	switch {
	case pr != nil && pr.TTFT.Rate != nil:
		rate = *pr.TTFT.Rate
	case d.Results.OperatingRate != nil && d.Results.OperatingRate.Answer != nil:
		rate = d.Results.OperatingRate.Answer.Value
	default:
		t.Fatalf("%s: no operating rate to price at", c.golden)
	}
	req := c.plan(rate)
	req.InputsID = in.InputsID
	out, body := apiPlan(t, h, req)
	assert.Empty(t, apitest.CheckFigures(body))
	assert.Equal(t, rate, out.Limits.Chosen.Rate)
	sameFigure(t, "decode knee", wantOf(d.Results.Knee), *out.TTFT.Markers.DecodeKnee)
	var lent []string
	if pr != nil {
		sameFigure(t, "prefill knee", wantOf(pr.Results.Knee), *out.TTFT.Markers.PrefillKnee)
		sameFigure(t, "TTFT point A", wantTTFT(pr.Results.TTFT), out.TTFT.Line.AtRate.Figure)
		if pr.Results.Knee.Answer != nil {
			lent = pr.Results.Knee.Answer.Flags
		}
	}
	tpot := d.Results.MeanTPOT
	if req.SLO.TPOT.Statistic != statMean {
		tpot = d.Results.PercentileTPOT
	}
	sameAtHeadroomRate(t, "TPOT point A", wantOf(tpot), out.TPOT.AtRate.Figure, lent)
	if req.SLO.TPOT.Statistic != statMean {
		// The mean too, asked as its own plan.
		req.SLO.TPOT = &whatifapi.Objective{Statistic: statMean, Target: req.SLO.TPOT.Target}
		req.Assume = nil
		mean, _ := apiPlan(t, h, req)
		sameAtHeadroomRate(t, "mean TPOT point A", wantOf(d.Results.MeanTPOT), mean.TPOT.AtRate.Figure, lent)
	}
}
