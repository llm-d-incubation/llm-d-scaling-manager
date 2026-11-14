package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// GET /v1/sources lists the recordings by file name, sorted, leaving out
// hidden files and directories, and says whether a Prometheus is read.
func TestServeSources(t *testing.T) {
	h, src := servedAPI(t)
	require.NoError(t, os.WriteFile(filepath.Join(src.recordings, ".hidden.jsonl.gz"), []byte("x"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(src.recordings, "sub"), 0o755))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sources", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got whatifapi.SourcesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, whatifapi.SourcesResponse{SchemaVersion: whatifapi.SchemaVersion, Namespaces: []string{},
		Recordings: []string{"ols-sweep.jsonl.gz", "saturated.jsonl.gz", "short.jsonl.gz", "throughput-sweep.jsonl.gz"}}, got)

	prom := &servedSources{window: captureFlags{prometheus: "http://prom.example:9090", namespace: "llm"}, allowed: []string{"llm", "other"}, warn: io.Discard}
	s, err := prom.Sources(t.Context())
	require.NoError(t, err)
	assert.Equal(t, whatifapi.SourcesResponse{Prometheus: true, Namespace: "llm", Namespaces: []string{"llm", "other"}, Recordings: []string{}}, s)

	gone := &servedSources{recordings: filepath.Join(t.TempDir(), "gone"), warn: io.Discard}
	_, err = gone.Sources(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "gone", "an error names no server path")
}

// serve, as started, serves the page at / on a loopback name, behind the
// same check as the API.
func TestServeServesThePage(t *testing.T) {
	f := serveFlags{recordings: t.TempDir(), listen: "127.0.0.1:0", auth: whatifapi.AuthNone}
	h, err := f.handler(newFlags("serve", io.Discard), io.Discard)
	require.NoError(t, err)
	for host, want := range map[string]int{"127.0.0.1:8080": http.StatusOK, "rebound.example:8080": http.StatusForbidden} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil))
		assert.Equal(t, want, rec.Code, host)
		if want == http.StatusOK {
			assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
			assert.Contains(t, rec.Body.String(), "Capacity planner")
		}
	}
}
