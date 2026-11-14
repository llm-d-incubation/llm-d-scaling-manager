package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// GET /v1/sources lists, sorted, every name a recording source accepts: the
// regular files and the links to one, and not hidden files, directories,
// dangling links or a name recordingPath refuses. Every name listed is one a
// recording source then resolves. It says whether a Prometheus is read.
func TestServeSources(t *testing.T) {
	h, src := servedAPI(t)
	dir := src.recordings
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden.jsonl.gz"), []byte("x"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	want := []string{"ols-sweep.jsonl.gz", "saturated.jsonl.gz", "short.jsonl.gz", "throughput-sweep.jsonl.gz"}
	if runtime.GOOS != "windows" {
		// A link to a recording elsewhere is listed and resolves; a link to
		// nothing or to a directory is not, nor a name with a colon, which
		// recordingPath refuses as a path.
		elsewhere := filepath.Join(t.TempDir(), "elsewhere.jsonl.gz")
		b, err := os.ReadFile(filepath.Join(dir, "short.jsonl.gz"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(elsewhere, b, 0o644))
		require.NoError(t, os.Symlink(elsewhere, filepath.Join(dir, "linked.jsonl.gz")))
		require.NoError(t, os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "dangling.jsonl.gz")))
		require.NoError(t, os.Symlink(filepath.Join(dir, "sub"), filepath.Join(dir, "dirlink")))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "c:colon.jsonl.gz"), []byte("x"), 0o644))
		want = []string{"linked.jsonl.gz", "ols-sweep.jsonl.gz", "saturated.jsonl.gz", "short.jsonl.gz", "throughput-sweep.jsonl.gz"}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sources", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got whatifapi.SourcesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, whatifapi.SourcesResponse{SchemaVersion: whatifapi.SchemaVersion, Namespaces: []string{}, Recordings: want}, got)
	for _, name := range got.Recordings {
		_, err := src.recordingPath(whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: name})
		assert.NoError(t, err, "listed %s, then refused it", name)
	}

	prom := &servedSources{window: captureFlags{prometheus: "http://prom.example:9090", namespace: "llm"}, allowed: []string{"llm", "other"}, warn: io.Discard}
	s, err := prom.Sources(t.Context())
	require.NoError(t, err)
	assert.Equal(t, whatifapi.SourcesResponse{Prometheus: true, Namespace: "llm", Namespaces: []string{"llm", "other"}, Recordings: []string{}}, s)

	many := &servedSources{recordings: t.TempDir(), warn: io.Discard}
	for i := range maxListedRecordings + 1 {
		require.NoError(t, os.WriteFile(filepath.Join(many.recordings, fmt.Sprintf("r%04d.jsonl.gz", i)), nil, 0o644))
	}
	s, err = many.Sources(t.Context())
	require.NoError(t, err)
	assert.Len(t, s.Recordings, maxListedRecordings)
	assert.True(t, s.RecordingsTruncated, "a listing cut at the cap says so")

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
