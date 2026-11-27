package whatifapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowResolver is the stub, each resolve taking delay, or until the
// request's context ends, as a replay of a long recording does.
type slowResolver struct {
	stubResolver
	delay time.Duration
	done  chan error
}

func (r *slowResolver) Resolve(ctx context.Context, req InputsRequest) (*Resolved, error) {
	select {
	case <-ctx.Done():
		r.done <- context.Cause(ctx)
		return nil, ctx.Err()
	case <-time.After(r.delay):
		r.done <- nil
	}
	return r.stubResolver.Resolve(ctx, req)
}

// POST /v1/inputs on a long source runs past the server's read and write
// timeouts and is answered: the handler moves the connection's write
// deadline to the long-request deadline once the body is read. Before, the
// write timeout cut the connection before the answer (EOF at the client).
// The read timeout bounds only the body: the resolve's context runs on.
func TestLongRequestOutlivesServerTimeouts(t *testing.T) {
	r := &slowResolver{delay: time.Second, done: make(chan error, 1)}
	s := New(Config{Resolver: r, WhatifSchema: "schema-v3"})
	s.longDeadline = 10 * time.Second
	srv := httptest.NewUnstartedServer(s.Handler())
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = 200*time.Millisecond, 300*time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	body := []byte(`{"source":{"kind":"recording","name":"run.jsonl.gz"}}`)
	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/inputs", "application/json", bytes.NewReader(body))
	require.NoError(t, err, "the connection was cut before the answer")
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, <-r.done, "the resolve ran to its end, its context not ended by the read timeout")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "%s", out)
	assert.Contains(t, string(out), `"inputsId"`)
	t.Logf("answered after %s, past the server's %s read and %s write timeouts", time.Since(start), srv.Config.ReadTimeout, srv.Config.WriteTimeout)
}

// deadlineRecorder is a recorder that keeps the deadlines a handler sets
// through http.ResponseController.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	read, write time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error  { d.read = t; return nil }
func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error { d.write = t; return nil }

// Every route that computes moves its write deadline once its body is read,
// and leaves the read deadline to the server; the listing and a body refused
// as unreadable keep the server's own.
func TestLongRequestDeadlineRoutes(t *testing.T) {
	s := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"})
	h := s.Handler()
	serve := func(method, path, body string) *deadlineRecorder {
		rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/v1/inputs", "/v1/plan", "/v1/size", "/v1/report", "/v1/dayplan", "/v1/backtest", "/v1/predict", "/v1/compare"} {
		before := time.Now()
		rec := serve(http.MethodPost, path, `{}`)
		assert.False(t, rec.write.Before(before.Add(s.longDeadline)), "%s: write deadline %s, want %s from the request", path, rec.write, s.longDeadline)
		assert.True(t, rec.read.IsZero(), "%s: the read deadline is the server's", path)

		rec = serve(http.MethodPost, path, `{`)
		assert.Equal(t, http.StatusBadRequest, rec.Code, path)
		assert.True(t, rec.write.IsZero(), "%s: a body not read keeps the server's deadlines", path)
	}
	rec := serve(http.MethodGet, "/v1/sources", "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, rec.write.IsZero(), "GET /v1/sources keeps the server's deadline")
	assert.Equal(t, LongRequestDeadline, New(Config{Resolver: &stubResolver{}}).longDeadline, "the default")
}
