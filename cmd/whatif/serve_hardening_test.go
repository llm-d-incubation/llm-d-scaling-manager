package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// workedRate is the ttft-rate-worked golden's question as a plan.
func workedRate() whatifapi.PlanRequest {
	i := slices.IndexFunc(parityCases(), func(c parityCase) bool { return c.golden == "ttft-rate-worked-json" })
	return parityCases()[i].plan(0)
}

// cliRole is the role of that name in a CLI command's JSON output.
func cliRole(t *testing.T, name string, args ...string) wireRole {
	t.Helper()
	out, code := runCmd(t, append(args, "-o", "json")...)
	require.Equal(t, 0, code, out)
	return role(t, decodeOutput(t, out), name)
}

// The figures no golden pins, held to the CLI run for them: the fleet's limit
// without headroom is slo-to-rate --headroom 1's fleet rate, and each +1 step
// is slo-to-rate's fleet rate at one more replica of the role (with no span:
// the step is priced without the V_P and f sweep).
func TestServeParityWithoutHeadroomAndMarginal(t *testing.T) {
	h, _ := servedAPI(t)
	in, _ := apiInputs(t, h, "ols-sweep.jsonl.gz", "model-02")
	req := workedRate()
	req.InputsID = in.InputsID
	out, _ := apiPlan(t, h, req)
	question := workedTTFT("slo-to-rate", "--tpot", "4ms", "--ttft", "200ms@p95")

	sameFigure(t, "without headroom", wantOf(cliRole(t, "fleet", append(slices.Clone(question), "--headroom", "1")...).Results.Rate),
		out.Limits.Fleet.WithoutHeadroom)
	for _, c := range []struct {
		what string
		flag string
		step whatifapi.MarginalStep
	}{
		{"+1 prefill", "--prefill", out.Marginal.PrefillPlusOne},
		{"+1 decode", "--decode", out.Marginal.DecodePlusOne},
	} {
		want := wantOf(cliRole(t, "fleet", append(slices.Clone(question), c.flag, "2")...).Results.Rate)
		want.Sensitivity = nil
		sameFigure(t, c.what, want, c.step.Limit)
	}
}

// Two recordings of the same bytes are one inputsId, and each caller is told
// its own recording's name.
func TestServeInputsAnswerTheirOwnSource(t *testing.T) {
	h, src := servedAPI(t)
	b, err := os.ReadFile(filepath.Join(src.recordings, "ols-sweep.jsonl.gz"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(src.recordings, "copy.jsonl.gz"), b, 0o644))
	first, _ := apiInputs(t, h, "ols-sweep.jsonl.gz", "model-02")
	second, _ := apiInputs(t, h, "copy.jsonl.gz", "model-02")
	assert.Equal(t, first.InputsID, second.InputsID)
	assert.Equal(t, "ols-sweep.jsonl.gz", first.Source.Name)
	assert.Equal(t, "copy.jsonl.gz", second.Source.Name)
}

// A Prometheus source reads only a namespace the server allows, spelled as a
// DNS-1123 label, and with Kubernetes authentication only one whose Pods the
// user may get: the server's credentials reach more than the user's may.
// Each is refused before any query.
func TestServeNamespaces(t *testing.T) {
	denied := errors.New("alice may not get pods in llm")
	src := &servedSources{window: captureFlags{prometheus: "http://127.0.0.1:1", namespace: "llm", ksm: true},
		allowed: []string{"llm", "llm-b"}, warn: io.Discard}
	h := whatifapi.New(whatifapi.Config{Resolver: src}).Handler()
	ask := func(ns string) (int, string) {
		code, body := apiPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourcePrometheus, Namespace: ns}})
		return code, string(body)
	}
	code, body := ask("other")
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, "llm, llm-b")
	code, body = ask("Bad_NS")
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, body = ask("../x")
	assert.Equal(t, http.StatusBadRequest, code, body)

	var asked []string
	src.namespaceAllowed = func(_ context.Context, ns string) error {
		asked = append(asked, ns)
		return whatifapi.ErrForbidden{Err: denied}
	}
	code, body = ask("")
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Equal(t, []string{"llm"}, asked, "the server's namespace, checked for the user")
	code, _ = ask("llm-b")
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, []string{"llm", "llm-b"}, asked)
}

// serve refuses a Prometheus with no namespace to confine it to, and a
// namespace that is no DNS-1123 label.
func TestServeNeedsANamespaceForPrometheus(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"serve", "--prometheus", "http://127.0.0.1:1", "--kube-state-metrics"}, "--namespace or --allowed-namespaces"},
		{[]string{"serve", "--prometheus", "http://127.0.0.1:1", "--kube-state-metrics", "--namespace", "Bad_NS"}, "Bad_NS"},
		{[]string{"serve", "--prometheus", "http://127.0.0.1:1", "--kube-state-metrics", "--namespace", "a", "--allowed-namespaces", "b,c"}, "not among"},
	} {
		err := run(context.Background(), append(c.args, "--listen", "127.0.0.1:0"), io.Discard, io.Discard)
		var u usageError
		require.ErrorAs(t, err, &u, "%v", c.args)
		assert.Contains(t, err.Error(), c.want)
	}
}

// The loopback server refuses a request whose Host is not a loopback name.
func TestServeRefusesAForeignHost(t *testing.T) {
	f := serveFlags{recordings: t.TempDir(), listen: "127.0.0.1:0", auth: whatifapi.AuthNone}
	h, err := f.handler(newFlags("serve", io.Discard), io.Discard)
	require.NoError(t, err)
	b, _ := json.Marshal(whatifapi.PlanRequest{InputsID: "sha256:none"})
	for host, want := range map[string]int{"127.0.0.1:8080": http.StatusNotFound, "rebound.example:8080": http.StatusForbidden} {
		req, _ := http.NewRequest(http.MethodPost, "http://"+host+"/v1/plan", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, want, rec.Code, host)
	}
}

// The server bounds how long a client may take to send a request and hold an
// idle connection, and lets a response run longer than the slowest cold plan
// measured (9.9 s) or a window's capture.
func TestServeTimeouts(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	assert.Positive(t, srv.ReadHeaderTimeout)
	assert.Positive(t, srv.ReadTimeout)
	assert.LessOrEqual(t, srv.ReadTimeout, 30*time.Second)
	assert.Positive(t, srv.IdleTimeout)
	assert.GreaterOrEqual(t, srv.WriteTimeout, 5*time.Minute)
}
