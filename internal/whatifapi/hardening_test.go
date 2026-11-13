package whatifapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A statistic is "mean" or a percentile spelled pNN or pNN.N, from p1 to
// p99.9: no other spelling is read, so none rounds to p0 or p100 and is then
// priced as the mean.
func TestStatisticForms(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	for _, stat := range []string{"p99.99999999", "p0.0000001", "p1e1", "p0x1p-2", "p100", "p0", "p0.5", "p 90", "p+90", "p90.", "p.5", "90", "P90", "p090"} {
		for _, side := range []string{"ttft", "tpot"} {
			slo := &SLORequest{TTFT: &Objective{Statistic: stat}}
			if side == "tpot" {
				slo = &SLORequest{TPOT: &Objective{Statistic: stat}}
			}
			code, body := post(t, h, "/v1/plan", PlanRequest{InputsID: id, SLO: slo})
			assert.Equal(t, http.StatusBadRequest, code, "%s %q: %s", side, stat, body)
			assert.Contains(t, string(body), "slo."+side+".statistic", "%s %q", side, stat)
		}
	}
	for stat, want := range map[string]string{"p99.9": "p99.9", "p50": "p50", "p9": "p9", "p1": "p1", "p95.0": "p95", "mean": "mean"} {
		out, _ := plan(t, h, PlanRequest{InputsID: id, SLO: &SLORequest{TTFT: &Objective{Statistic: stat}, TPOT: &Objective{Statistic: stat}}})
		assert.Equal(t, want, out.Scenario.TTFT.Statistic, "%q", stat)
		assert.Equal(t, want, out.Scenario.TPOT.Statistic, "%q", stat)
	}
}

// A request past the bound on plans or inputs in flight is 503 with
// Retry-After, before any pricing.
func TestBusyIsServiceUnavailable(t *testing.T) {
	r := &stubResolver{}
	s := New(Config{Resolver: r, WhatifSchema: "schema-v3", MaxPlans: 1, MaxInputs: 1})
	h := s.Handler()
	id := inputsOf(t, h)

	s.planSlots <- struct{}{}
	rec := httptest.NewRecorder()
	b, _ := json.Marshal(PlanRequest{InputsID: id})
	req := httptest.NewRequest(http.MethodPost, "/v1/plan", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	before := r.fleets.Load()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	assert.Equal(t, before, r.fleets.Load(), "a busy server priced a fleet")
	select {
	case <-s.planSlots:
	default:
		t.Fatal("the slot the test took is gone: a plan priced without one released it")
	}
	code, _ := post(t, h, "/v1/plan", PlanRequest{InputsID: id})
	assert.Equal(t, http.StatusOK, code, "a free slot prices again")

	s.inputSlots <- struct{}{}
	code, body := post(t, h, "/v1/inputs", InputsRequest{})
	assert.Equal(t, http.StatusServiceUnavailable, code, "%s", body)
	<-s.inputSlots
}

// A plan whose request has ended stops before its curves, caches nothing,
// and frees its slot.
func TestPlanStopsWhenTheRequestEnds(t *testing.T) {
	curves := 0
	s := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3", MaxPlans: 1})
	s.onCurves = func() { curves++ }
	id := inputsOf(t, s.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.plan(ctx, PlanRequest{InputsID: id})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, curves, "the curves were started")
	_, err = s.plan(context.Background(), PlanRequest{InputsID: id})
	require.NoError(t, err)
	assert.Equal(t, 2, curves, "a stopped curve set was cached")
	assert.Empty(t, s.planSlots, "a slot was kept")
}

// With no authentication the server answers loopback names alone, so that a
// page rebinding its own name to 127.0.0.1 cannot read it.
func TestLoopbackOnly(t *testing.T) {
	h := LoopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	for host, want := range map[string]int{
		"localhost": http.StatusTeapot, "localhost:8080": http.StatusTeapot, "127.0.0.1:8080": http.StatusTeapot,
		"[::1]:8080": http.StatusTeapot, "LOCALHOST:1": http.StatusTeapot,
		"evil.example:8080": http.StatusForbidden, "127.0.0.1.evil.example": http.StatusForbidden, "127.0.0.2:8080": http.StatusTeapot,
		"10.0.0.1:8080": http.StatusForbidden, "": http.StatusForbidden,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/plan", nil)
		req.Host = host
		h.ServeHTTP(rec, req)
		assert.Equal(t, want, rec.Code, "host %q", host)
	}
}

// A body is read only as application/json: no content type, a form, or a
// type that merely starts with it is 415, so no page's cross-site form post
// reaches the API.
func TestContentTypeIsJSON(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}}).Handler()
	for ct, want := range map[string]int{
		"": http.StatusUnsupportedMediaType, "text/plain": http.StatusUnsupportedMediaType,
		"application/x-www-form-urlencoded": http.StatusUnsupportedMediaType, "application/jsonfoo": http.StatusUnsupportedMediaType,
		"application/json": http.StatusOK, "application/json; charset=utf-8": http.StatusOK, "Application/JSON": http.StatusOK,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/inputs", bytes.NewReader([]byte(`{}`)))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		h.ServeHTTP(rec, req)
		assert.Equal(t, want, rec.Code, "content type %q: %s", ct, rec.Body.String())
	}
}

// Of several fields asked wrongly, the error names the same one every time.
func TestValidationOrderIsFixed(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}}).Handler()
	id := inputsOf(t, h)
	for name, c := range map[string]struct {
		req  PlanRequest
		want string
	}{
		"classes":                 {PlanRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Regular: &ClassRate{Rate: f64(1)}, Batch: &ClassRate{Rate: f64(1)}}}}, "classes.regular"},
		"mix":                     {PlanRequest{InputsID: id, Traffic: &Traffic{Mix: &Mix{PromptTokens: -1, OutputTokens: -1}}}, "mix.promptTokens"},
		"assume":                  {PlanRequest{InputsID: id, Assume: &Assume{VP: f64(-1), A: f64(-1), TPOTRatio: f64(-1), PromptCV: f64(-1), RouterHop: f64(-1)}}, "assume.vp"},
		"assume, the second loop": {PlanRequest{InputsID: id, Assume: &Assume{PromptCV: f64(-1), RouterHop: f64(-1)}}, "assume.promptCV"},
		"buckets": {PlanRequest{InputsID: id, Views: &ViewRequest{AllTrafficPerBucket: true},
			SLO: &SLORequest{TTFT: &Objective{BucketTargets: map[string]float64{"zz": 1, "aa": 1, "mm": 1}}}}, "bucketTargets.aa"},
	} {
		for range 30 {
			code, body := post(t, h, "/v1/plan", c.req)
			require.Equal(t, http.StatusBadRequest, code, name)
			require.Contains(t, string(body), c.want, name)
		}
	}
}

// ForbiddenError is a 403.
func TestForbiddenIs403(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, ForbiddenError{Err: errors.New("no")})
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
