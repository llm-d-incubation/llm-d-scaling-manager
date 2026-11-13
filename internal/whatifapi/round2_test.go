package whatifapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// errorOf decodes an error body; the test fails on any other shape.
func errorOf(t *testing.T, body []byte) string {
	t.Helper()
	var e ErrorResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&e), "%s", body)
	assert.Equal(t, SchemaVersion, e.SchemaVersion)
	return e.Error
}

// A plan whose request ends while its limits are priced stops there: every
// loop over fleets reads the request's end, caches nothing, and frees its
// slot.
func TestPlanStopsInsideTheLimits(t *testing.T) {
	for name, c := range map[string]struct {
		view   bool
		cancel int64
	}{
		"in the +1 steps":    {false, 1},
		"in the bucket view": {true, 4},
	} {
		t.Run(name, func(t *testing.T) {
			r := &stubResolver{}
			s := New(Config{Resolver: r, WhatifSchema: "schema-v3", MaxPlans: 1})
			id := inputsOf(t, s.Handler())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.onPlainLimits = func(n int64) {
				if n == c.cancel {
					cancel()
				}
			}
			req := PlanRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(4)}}}}
			if c.view {
				req.Views = &ViewRequest{AllTrafficPerBucket: true}
			}
			_, err := s.plan(ctx, req)
			require.ErrorIs(t, err, context.Canceled)
			assert.Zero(t, s.limits.len(), "a stopped limit entry was cached")
			assert.Zero(t, s.plans.len(), "a stopped plan was cached")
			assert.Empty(t, s.planSlots, "a slot was kept")
		})
	}
}

// Identical cold plans asked at once are priced once and share one slot: the
// second is not a spurious 503.
func TestIdenticalPlansShareOnePricing(t *testing.T) {
	r := &stubResolver{}
	s := New(Config{Resolver: r, WhatifSchema: "schema-v3", MaxPlans: 1})
	s.onCurves = func() {}
	curves := 0
	var mu sync.Mutex
	s.onCurves = func() { mu.Lock(); curves++; mu.Unlock() }
	h := s.Handler()
	id := inputsOf(t, h)
	req := PlanRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(4)}}}}
	e, _ := s.inputs.get(id)
	sc, err := scenarioOf(&e.doc, req)
	require.NoError(t, err)
	key := contentID("scenario", sc)

	r.gate = make(chan struct{})
	codes := make([]int, 3)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() { codes[i], _ = post(t, h, "/v1/plan", req) })
	}
	require.Eventually(t, func() bool { return s.planFlight.waiters(key) == len(codes)-1 }, 5*time.Second, time.Millisecond,
		"the other plans never joined the first")
	close(r.gate)
	wg.Wait()
	assert.Equal(t, []int{http.StatusOK, http.StatusOK, http.StatusOK}, codes)
	assert.Equal(t, 1, curves)
}

// The body is one JSON value of at most 1 MiB.
func TestBodyIsOneValueUnderTheLimit(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}}).Handler()
	for name, c := range map[string]struct {
		body string
		want string
	}{
		"two values": {`{} {}`, "more than one JSON value"},
		"over 1 MiB": {`{"model": "` + strings.Repeat("x", 1<<20) + `"}`, "too large"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/inputs", strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
		assert.Contains(t, errorOf(t, rec.Body.Bytes()), c.want, name)
	}
	assert.Equal(t, 1<<20, maxBody, "the doc says 1 MiB")
}

// The edges of the scenario's ranges.
func TestScenarioBoundaries(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}}).Handler()
	id := inputsOf(t, h)
	for name, c := range map[string]struct {
		req  PlanRequest
		code int
	}{
		"headroom 1":  {PlanRequest{InputsID: id, Headroom: f64(1)}, http.StatusOK},
		"headroom 0":  {PlanRequest{InputsID: id, Headroom: f64(0)}, http.StatusBadRequest},
		"fleet 64":    {PlanRequest{InputsID: id, Fleet: &FleetKnob{Prefill: intp(64), Decode: intp(64)}}, http.StatusOK},
		"fleet 0":     {PlanRequest{InputsID: id, Fleet: &FleetKnob{Decode: intp(0)}}, http.StatusBadRequest},
		"prefill 65":  {PlanRequest{InputsID: id, Fleet: &FleetKnob{Prefill: intp(65)}}, http.StatusBadRequest},
		"prefill -1":  {PlanRequest{InputsID: id, Fleet: &FleetKnob{Prefill: intp(-1)}}, http.StatusBadRequest},
		"headroom -1": {PlanRequest{InputsID: id, Headroom: f64(-1)}, http.StatusBadRequest},
	} {
		code, body := post(t, h, "/v1/plan", c.req)
		assert.Equal(t, c.code, code, "%s: %s", name, body)
	}
}

// No answer, and no error, is cached by a client or a proxy.
func TestNoStore(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}}).Handler()
	id := inputsOf(t, h)
	for name, body := range map[string]any{"answer": PlanRequest{InputsID: id}, "error": PlanRequest{InputsID: "sha256:none"}} {
		rec := httptest.NewRecorder()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/plan", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"), name)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"), name)
	}
}

// Every refusal before pricing is an ErrorResponse saying why: 401, 403, 404,
// 415, a generic 500, and a 503 with Retry-After on either endpoint.
func TestErrorBodies(t *testing.T) {
	c := newCluster("/whatif/v1/plan", "")
	s := New(Config{Resolver: &stubResolver{}, MaxInputs: 1})
	authed := NewAuthenticator(c, nil, 0, nil).Wrap(s.Handler())
	for name, tc := range map[string]struct {
		h      http.Handler
		path   string
		auth   string
		ct     string
		code   int
		reason string
	}{
		"401": {authed, "/v1/plan", "", "application/json", http.StatusUnauthorized, "bearer token is required"},
		"403": {authed, "/v1/inputs", "Bearer good", "application/json", http.StatusForbidden, "may not get /whatif/v1/inputs"},
		"415": {s.Handler(), "/v1/plan", "", "text/plain", http.StatusUnsupportedMediaType, "want application/json"},
		"404": {s.Handler(), "/v2/plan", "", "application/json", http.StatusNotFound, "no such endpoint"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", tc.ct)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		tc.h.ServeHTTP(rec, req)
		assert.Equal(t, tc.code, rec.Code, name)
		assert.Contains(t, errorOf(t, rec.Body.Bytes()), tc.reason, name)
	}

	code, body := post(t, s.Handler(), "/v1/inputs", InputsRequest{Model: "boom"})
	assert.Equal(t, http.StatusInternalServerError, code)
	assert.Contains(t, errorOf(t, body), "the source broke")

	s.inputSlots <- struct{}{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/inputs", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	<-s.inputSlots
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, retryAfterSeconds, rec.Header().Get("Retry-After"))
	assert.Contains(t, errorOf(t, rec.Body.Bytes()), "retry shortly")
}

// An inputs entry the cache dropped is a 404, not another source's plan.
func TestEvictedInputsAre404(t *testing.T) {
	s := New(Config{Resolver: &stubResolver{}})
	s.inputs = newLRU[*inputsEntry](1)
	h := s.Handler()
	resolve := func(model string) string {
		code, body := post(t, h, "/v1/inputs", InputsRequest{Model: model})
		require.Equal(t, http.StatusOK, code, "%s", body)
		var in InputsResponse
		require.NoError(t, json.Unmarshal(body, &in))
		return in.InputsID
	}
	a, b := resolve("a"), resolve("b")
	require.NotEqual(t, a, b)
	code, body := post(t, h, "/v1/plan", PlanRequest{InputsID: a})
	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, errorOf(t, body), "unknown inputsId")
	code, _ = post(t, h, "/v1/plan", PlanRequest{InputsID: b})
	assert.Equal(t, http.StatusOK, code)
}

// The loopback rule is one: an address CheckListen binds with no
// authentication is a Host LoopbackOnly answers, and the other way round.
func TestOneLoopbackRule(t *testing.T) {
	h := LoopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	for _, host := range []string{"localhost", "127.0.0.1", "127.0.0.2", "127.255.255.254", "::1", "10.0.0.1", "0.0.0.0", "evil.example", "::"} {
		hostport := net.JoinHostPort(host, "8080")
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Host = hostport
		h.ServeHTTP(rec, req)
		assert.Equal(t, CheckListen(hostport, AuthNone) == nil, rec.Code == http.StatusTeapot, "%s", host)
	}
}

// A failed review is not kept: the next request asks the API again.
func TestFailedReviewIsNotCached(t *testing.T) {
	c := newCluster("/whatif/v1/plan", "")
	c.fail = errors.New("unreachable")
	h := NewAuthenticator(c, nil, 0, nil).Wrap(New(Config{Resolver: &stubResolver{}}).Handler())
	assert.Equal(t, http.StatusInternalServerError, call(h, "/v1/plan", "Bearer good").Code)
	c.fail = nil
	assert.Equal(t, http.StatusBadRequest, call(h, "/v1/plan", "Bearer good").Code)
	assert.Equal(t, 2, c.tokens)
}

// The review cache is bounded, and holds no token or user name in the clear.
func TestReviewCacheIsBoundedAndHashed(t *testing.T) {
	c := newCluster("/whatif/v1/plan", "")
	a := NewAuthenticator(c, nil, 0, nil)
	a.reviews = newLRU[cachedReview](3)
	h := a.Wrap(New(Config{Resolver: &stubResolver{}}).Handler())
	for _, tok := range []string{"r1", "r2", "r3", "r4", "good"} {
		call(h, "/v1/plan", "Bearer "+tok)
	}
	assert.Equal(t, 3, a.reviews.len())
	a.reviews.mu.Lock()
	defer a.reviews.mu.Unlock()
	for key := range a.reviews.items {
		for _, clear := range []string{"good", "r1", "alice", "planners"} {
			assert.NotContains(t, key, clear)
		}
	}
}

// Callers asking for one token's review at once share one TokenReview.
func TestConcurrentReviewsShareOne(t *testing.T) {
	c := newCluster("/whatif/v1/plan", "")
	gate := make(chan struct{})
	c.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		<-gate
		return false, nil, nil
	})
	a := NewAuthenticator(c, nil, 0, nil)
	h := a.Wrap(New(Config{Resolver: &stubResolver{}}).Handler())
	codes := make([]int, 4)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() { codes[i] = call(h, "/v1/plan", "Bearer good").Code })
	}
	key := hashKey("token", []byte("good"))
	require.Eventually(t, func() bool { return a.inFlight.waiters(key) == len(codes)-1 }, 5*time.Second, time.Millisecond)
	close(gate)
	wg.Wait()
	assert.Equal(t, []int{400, 400, 400, 400}, codes)
	assert.Equal(t, 1, c.tokens)
}

// A review runs apart from the request that started it: its client gone, the
// review still answers those waiting on it.
func TestReviewOutlivesItsCaller(t *testing.T) {
	a := NewAuthenticator(newCluster("", ""), nil, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := a.cached(ctx, "k", func(rctx context.Context) (cachedReview, error) {
		if err := rctx.Err(); err != nil {
			return cachedReview{}, err
		}
		_, hasDeadline := rctx.Deadline()
		return cachedReview{ok: hasDeadline, user: authnv1.UserInfo{Username: "alice"}}, nil
	})
	require.NoError(t, err)
	assert.True(t, r.ok, "the review had no timeout of its own")
}

// ForbiddenError and InputError say something with no error inside.
func TestErrorTypesWithNoError(t *testing.T) {
	assert.Equal(t, "forbidden", ForbiddenError{}.Error())
	assert.Equal(t, "invalid request", InputError{}.Error())
}
