package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource"
)

// runCLIWarn runs whatif with args under ctx and returns its warnings and
// its error; its output is dropped.
func runCLIWarn(ctx context.Context, args ...string) (string, error) {
	var warn bytes.Buffer
	err := run(ctx, args, io.Discard, &warn)
	return warn.String(), err
}

// authSeen is an https server that records the Authorization header of each
// request it answers.
type authSeen struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newAuthSeen(t *testing.T, next http.HandlerFunc) *authSeen {
	t.Helper()
	a := &authSeen{}
	a.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.seen = append(a.seen, r.Header.Get("Authorization"))
		a.mu.Unlock()
		next(w, r)
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *authSeen) headers() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

// caFile writes srv's certificate as a PEM file.
func caFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))
	return path
}

// The bearer token goes to Prometheus' own host, and not to the host a
// redirect points at.
func TestClient_TheBearerTokenStaysOnItsHost(t *testing.T) {
	other := newAuthSeen(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	})
	prom := newAuthSeen(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	})
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("s3cret\n"), 0o600))
	f := captureFlags{prometheus: prom.URL, tokenFile: token, caFile: caFile(t, prom.Server), timeout: 10 * time.Second}
	api, err := f.client(&bytes.Buffer{})
	require.NoError(t, err)
	_, _, err = api.QueryRange(context.Background(), "up", promv1.Range{Start: cliFrom, End: cliFrom.Add(time.Minute), Step: 15 * time.Second})
	require.NoError(t, err)
	require.NotEmpty(t, prom.headers())
	for _, h := range prom.headers() {
		assert.Equal(t, "Bearer s3cret", h, "Prometheus' own host gets the token")
	}
	require.NotEmpty(t, other.headers(), "the redirect was not followed")
	for _, h := range other.headers() {
		assert.Empty(t, h, "the token went to the host a redirect named")
	}
}

// A bearer token for a plain-http Prometheus is refused, not dropped.
func TestCapture_ATokenForPlainHTTPIsRefused(t *testing.T) {
	srv := fleetServer(t)
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("s3cret"), 0o600))
	_, err := runCLI(captureArgs(srv.URL, filepath.Join(t.TempDir(), "c.gz"), "--bearer-token-file", token)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https Prometheus only")
	assert.Equal(t, 2, exitCode(err))
	assert.Empty(t, srv.Requests(), "refused before any query")
}

// --insecure-skip-verify says so on stderr.
func TestClient_InsecureSkipVerifyWarns(t *testing.T) {
	var warn bytes.Buffer
	_, err := captureFlags{prometheus: "https://prom.example:9090", insecure: true, timeout: time.Second}.client(&warn)
	require.NoError(t, err)
	assert.Contains(t, warn.String(), "--insecure-skip-verify")
	assert.Contains(t, warn.String(), "prom.example:9090")
	warn.Reset()
	_, err = captureFlags{prometheus: "https://prom.example:9090", timeout: time.Second}.client(&warn)
	require.NoError(t, err)
	assert.Empty(t, warn.String())
}

// Every request has a timeout, which --timeout sets.
func TestClient_ARequestTimesOut(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	api, err := captureFlags{prometheus: slow.URL, timeout: 200 * time.Millisecond}.client(&bytes.Buffer{})
	require.NoError(t, err)
	began := time.Now()
	_, _, err = api.QueryRange(context.Background(), "up", promv1.Range{Start: cliFrom, End: cliFrom, Step: time.Second})
	require.Error(t, err)
	assert.Less(t, time.Since(began), 3*time.Second)
	_, err = runCLI(captureArgs(slow.URL, filepath.Join(t.TempDir(), "c.gz"), "--timeout", "0s")...)
	require.Error(t, err)
	assert.Equal(t, 2, exitCode(err))
}

// --batch-cap beside the engine's own batch flag in --engine-args is warned
// about.
func TestCapture_BatchCapTwiceWarns(t *testing.T) {
	srv := fleetServer(t)
	warn, err := runCLIWarn(context.Background(), captureArgs(srv.URL, filepath.Join(t.TempDir(), "c.gz"),
		"--batch-cap", "32", "--engine-args", "--max-num-seqs=64 --block-size=16")...)
	require.NoError(t, err)
	assert.Contains(t, warn, "--batch-cap 32 and --max-num-seqs in --engine-args")
	warn, err = runCLIWarn(context.Background(), captureArgs(srv.URL, filepath.Join(t.TempDir(), "c.gz"),
		"--batch-cap", "32", "--engine-args", "--block-size=16")...)
	require.NoError(t, err)
	assert.Empty(t, warn)
}

// --role-of selectors whose name prefixes nest exit 2.
func TestCapture_NestedRolePrefixesExitTwo(t *testing.T) {
	srv := fleetServer(t)
	out := filepath.Join(t.TempDir(), "c.gz")
	_, err := runCLI(captureArgs(srv.URL, out, "--role-of", "prefill=app=m-dec-pre")...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlap")
	assert.Equal(t, 2, exitCode(err))
	assert.Empty(t, srv.Requests())
}

// A failed range query exits 1, names the query, and does not sit in the
// collector's backoff.
func TestCapture_AFailedHistogramQueryExitsOneAtOnce(t *testing.T) {
	srv := fleetServer(t)
	reg := source.NewSourceRegistry()
	require.NoError(t, reg.Register("prometheus", source.NewNoOpSource()))
	registration.RegisterSaturationQueries(reg)
	registration.RegisterQueueingModelQueries(reg)
	registration.RegisterSLOQueries(reg)
	ttft, err := reg.Get("prometheus").QueryList().Build(registration.QueryTTFTBuckets, map[string]string{source.ParamNamespace: "llm"})
	require.NoError(t, err)
	srv.Fail(ttft, "query processing would load too many samples into memory in query execution")
	out := filepath.Join(t.TempDir(), "c.gz")
	began := time.Now()
	_, err = runCLI(captureArgs(srv.URL, out)...)
	require.Error(t, err)
	assert.Less(t, time.Since(began), 3*time.Second)
	assert.Contains(t, err.Error(), "too many samples")
	assert.Contains(t, err.Error(), fmt.Sprintf("%q", ttft))
	assert.Equal(t, 1, exitCode(err))
	_, statErr := os.Stat(out)
	assert.True(t, os.IsNotExist(statErr))
}

// The capture's flags survive capture, generate and a Fidelity replay of
// the generated recording: a golden made from it shows them.
func TestCapture_FlagsSurviveGenerateAndReplay(t *testing.T) {
	srv := fleetServer(t)
	dir := t.TempDir()
	capture := filepath.Join(dir, "run.capture.jsonl.gz")
	_, err := runCLI(captureArgs(srv.URL, capture)...)
	require.NoError(t, err)
	raw, err := readMaybeGzipped(capture)
	require.NoError(t, err)
	read, err := learnrec.Read(raw)
	require.NoError(t, err)
	var window learnreplay.CaptureWindow
	require.NoError(t, read.Events[1].DecodeData(&window))
	require.Contains(t, window.Flags, whatifsource.FlagRolesInferred)
	require.Contains(t, window.Flags, whatifsource.FlagNoRollouts)

	recording := filepath.Join(dir, "run.jsonl")
	text, err := runCLI("generate", "-in", capture, "-o", recording)
	require.NoError(t, err)
	assert.Contains(t, text, "caveat: "+whatifsource.FlagNoRollouts)
	b, err := os.ReadFile(recording)
	require.NoError(t, err)
	start, err := startOf(b)
	require.NoError(t, err)
	assert.Equal(t, window.Flags, start.Caveats)

	rep, err := learnreplay.Replay(context.Background(), b, learnreplay.Options{Mode: learnreplay.Fidelity})
	require.NoError(t, err)
	assert.Empty(t, rep.Failed())
	again, err := startOf(rep.Recording)
	require.NoError(t, err)
	assert.Equal(t, window.Flags, again.Caveats)

	exported, _, err := learnreplay.Export(b, learnreplay.ExportOptions{})
	require.NoError(t, err, "the export refuses a caveat's words")
	pseudo, err := startOf(exported)
	require.NoError(t, err)
	assert.Equal(t, window.Flags, pseudo.Caveats)
}

// generate exits 2 for a capture it cannot read, and 1 for a failure that
// is not the capture's.
func TestGenerate_ExitCodes(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage")
	require.NoError(t, os.WriteFile(garbage, []byte("not a capture\n"), 0o600))
	_, err := runCLI("generate", "-in", garbage, "-o", filepath.Join(dir, "out.jsonl"))
	require.Error(t, err)
	assert.Equal(t, 2, exitCode(err))
	assert.Equal(t, 1, strings.Count(err.Error(), "generate:"), "one prefix: %v", err)

	srv := fleetServer(t)
	capture := filepath.Join(dir, "c.gz")
	_, err = runCLI(captureArgs(srv.URL, capture)...)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = runCLIWarn(ctx, "generate", "-in", capture, "-o", filepath.Join(dir, "out.jsonl"))
	require.Error(t, err)
	assert.Equal(t, 1, strings.Count(err.Error(), "generate:"), "one prefix: %v", err)
	assert.Equal(t, 1, exitCode(err), "%v", err)
}

// startOf reads the start line's caveats.
func TestStartOf_ReadsTheCaveats(t *testing.T) {
	var buf bytes.Buffer
	rec := learnrec.New(&buf, 0)
	rec.Record(learnrec.KindStart, cliFrom, learnrec.Start{Commit: "abc", Caveats: []string{"one", "two"}})
	require.NoError(t, rec.Err())
	start, err := startOf(buf.Bytes())
	require.NoError(t, err)
	assert.Equal(t, []string{"one", "two"}, start.Caveats)
	assert.Equal(t, "abc", start.Commit)
	_, err = startOf([]byte{})
	assert.Error(t, err)
}

// A failed capture leaves the capture that was at -o, and no temporary
// file beside it.
func TestCapture_AFailedRunKeepsThePreviousCapture(t *testing.T) {
	srv := fleetServer(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "c.gz")
	_, err := runCLI(captureArgs(srv.URL, out)...)
	require.NoError(t, err)
	before, err := os.ReadFile(out)
	require.NoError(t, err)
	info, err := os.Stat(out)
	require.NoError(t, err)
	if os.PathSeparator == '/' {
		assert.Equal(t, os.FileMode(outputMode), info.Mode().Perm())
	}

	reg := source.NewSourceRegistry()
	require.NoError(t, reg.Register("prometheus", source.NewNoOpSource()))
	registration.RegisterSaturationQueries(reg)
	kv, err := reg.Get("prometheus").QueryList().Build(registration.QueryKvCacheUsage, map[string]string{source.ParamNamespace: "llm"})
	require.NoError(t, err)
	srv.Fail(kv, "query timed out")
	_, err = runCLI(captureArgs(srv.URL, out)...)
	require.Error(t, err)
	assert.Equal(t, 1, exitCode(err))
	after, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the failed run overwrote the capture")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "a temporary file was left: %v", entries)
}

// The client's inputs are checked: an empty token, and TLS flags for an
// http Prometheus.
func TestClient_InputChecks(t *testing.T) {
	srv := fleetServer(t)
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(empty, []byte(" \n\t"), 0o600))
	_, err := captureFlags{prometheus: "https://prom.example", tokenFile: empty, timeout: time.Second}.client(io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "holds no token")
	assert.Equal(t, 2, exitCode(err))
	for _, extra := range [][]string{{"--ca-file", empty}, {"--insecure-skip-verify"}} {
		_, err := runCLI(captureArgs(srv.URL, filepath.Join(dir, "c.gz"), extra...)...)
		require.Error(t, err, "%v", extra)
		assert.Contains(t, err.Error(), "are for an https Prometheus")
		assert.Equal(t, 2, exitCode(err))
	}
	assert.Empty(t, srv.Requests())
}

// recordingRT records each request it is given and answers 200.
type recordingRT struct{ seen []*http.Request }

func (r *recordingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.seen = append(r.seen, req)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

// The token goes over https to Prometheus' host, its case aside, and to
// nothing else: not over http to the same host, not to another host.
func TestBearer_SendsTheTokenOverHTTPSToItsHostOnly(t *testing.T) {
	for _, c := range []struct {
		url  string
		sent bool
	}{
		{"https://prom.example:9090/api/v1/query_range", true},
		{"https://PROM.Example:9090/api/v1/query_range", true},
		{"http://prom.example:9090/api/v1/query_range", false},
		{"https://other.example:9090/api/v1/query_range", false},
		{"https://prom.example:9091/api/v1/query_range", false},
	} {
		base := &recordingRT{}
		b := bearer{base: base, host: "prom.example:9090", token: "s3cret"}
		req, err := http.NewRequest(http.MethodGet, c.url, nil)
		require.NoError(t, err)
		resp, err := b.RoundTrip(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Len(t, base.seen, 1)
		want := ""
		if c.sent {
			want = "Bearer s3cret"
		}
		assert.Equal(t, want, base.seen[0].Header.Get("Authorization"), c.url)
		assert.Empty(t, req.Header.Get("Authorization"), "the caller's request was changed")
	}
}

// A redirect from https to http on the same host and port does not carry
// the token: only the scheme tells the two requests apart.
func TestClient_AnHTTPSRedirectToHTTPDropsTheToken(t *testing.T) {
	var mu sync.Mutex
	var tlsSeen, plainSeen []string
	var plainURL string
	srv := newBothSchemes(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.TLS != nil {
			tlsSeen = append(tlsSeen, r.Header.Get("Authorization"))
			http.Redirect(w, r, plainURL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			return
		}
		plainSeen = append(plainSeen, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	plainURL = "http://" + srv.addr
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("s3cret"), 0o600))
	f := captureFlags{prometheus: "https://" + srv.addr, tokenFile: token, caFile: srv.caFile, timeout: 10 * time.Second}
	api, err := f.client(io.Discard)
	require.NoError(t, err)
	_, _, err = api.QueryRange(context.Background(), "up", promv1.Range{Start: cliFrom, End: cliFrom, Step: time.Second})
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, tlsSeen)
	assert.Equal(t, "Bearer s3cret", tlsSeen[0], "the https request carries the token")
	require.NotEmpty(t, plainSeen, "the redirect was not followed")
	for _, h := range plainSeen {
		assert.Empty(t, h, "the token went over http to the same host and port")
	}
}

// bothSchemes is a server that answers https and plain http on one port.
type bothSchemes struct {
	addr, caFile string
}

// newBothSchemes serves h on one port, over TLS when a connection opens
// with a TLS handshake and in the clear otherwise, with httptest's
// certificate (127.0.0.1).
func newBothSchemes(t *testing.T, h http.Handler) bothSchemes {
	t.Helper()
	cert := httptest.NewTLSServer(http.NotFoundHandler())
	ca := caFile(t, cert)
	cfg := cert.TLS.Clone()
	cert.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.Serve(sniffing{Listener: ln, tls: cfg}) }()
	t.Cleanup(func() { _ = s.Close() })
	return bothSchemes{addr: ln.Addr().String(), caFile: ca}
}

// sniffing hands out each connection as TLS when its first byte opens a
// TLS handshake (0x16), and as it is otherwise.
type sniffing struct {
	net.Listener
	tls *tls.Config
}

func (s sniffing) Accept() (net.Conn, error) {
	c, err := s.Listener.Accept()
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(c)
	first, err := r.Peek(1)
	if err != nil {
		_ = c.Close()
		return s.Accept()
	}
	pc := peeked{Conn: c, r: r}
	if first[0] == 0x16 {
		return tls.Server(pc, s.tls), nil
	}
	return pc, nil
}

// peeked is a connection whose first bytes were read into r.
type peeked struct {
	net.Conn
	r *bufio.Reader
}

func (p peeked) Read(b []byte) (int, error) { return p.r.Read(b) }

// Exit codes by whose fault: nothing the role map attributes is the
// caller's (2), a failed query Prometheus' (1).
func TestCapture_ExitCodesByFault(t *testing.T) {
	srv := fleetServer(t)
	dir := t.TempDir()
	_, err := runCLI("capture", "--prometheus", srv.URL, "--namespace", "llm", "--role-of", "decode=app=nothing",
		"--from", cliFrom.Format(time.RFC3339), "--to", cliFrom.Add(time.Minute).Format(time.RFC3339), "-o", filepath.Join(dir, "a.gz"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, whatifsource.ErrNothingAttributed), "%v", err)
	assert.Equal(t, 2, exitCode(err))

	reg := source.NewSourceRegistry()
	require.NoError(t, reg.Register("prometheus", source.NewNoOpSource()))
	registration.RegisterSaturationQueries(reg)
	kv, err := reg.Get("prometheus").QueryList().Build(registration.QueryKvCacheUsage, map[string]string{source.ParamNamespace: "llm"})
	require.NoError(t, err)
	srv.Fail(kv, "query timed out")
	_, err = runCLI(captureArgs(srv.URL, filepath.Join(dir, "b.gz"))...)
	require.Error(t, err)
	assert.True(t, errors.Is(err, whatifsource.ErrQueryFailed), "%v", err)
	assert.Equal(t, 1, exitCode(err))
	assert.Equal(t, 1, strings.Count(err.Error(), "capture:"), "one prefix: %v", err)
}
