package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/balance"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// routerPolicyRecording writes the q32 staircase's warm-up (q32Recording)
// into dir with the router's prefix pair set on every analyzer input, kept
// at kept(arrival rate) of the best available, and returns its path: the
// warm-up's two prefill pods behind a router whose own series say what it
// scores. Nothing else is touched.
func routerPolicyRecording(t *testing.T, dir string, kept func(rate float64) float64) string {
	t.Helper()
	in, err := os.Open(q32Recording)
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	require.NoError(t, err)
	path := filepath.Join(dir, filepath.Base(q32Recording))
	out, err := os.Create(path)
	require.NoError(t, err)
	zw := gzip.NewWriter(out)
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	set := 0
	for sc.Scan() {
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		var ev map[string]any
		require.NoError(t, dec.Decode(&ev))
		set += setPrefixPair(ev["Data"], kept)
		b, err := json.Marshal(ev)
		require.NoError(t, err)
		_, err = zw.Write(append(b, '\n'))
		require.NoError(t, err)
	}
	require.NoError(t, sc.Err())
	require.NoError(t, zw.Close())
	require.NoError(t, out.Close())
	require.Positive(t, set, "the warm-up has analyzer inputs with a router latency")
	return path
}

// setPrefixPair sets the prefix pair on every analyzer input under v that
// carries a router latency and a positive arrival rate, and returns how
// many it set.
func setPrefixPair(v any, kept func(rate float64) float64) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		rl, okRL := x["RouterLatency"].(map[string]any)
		if num, ok := x["ArrivalRate"].(json.Number); okRL && ok {
			if rate, err := num.Float64(); err == nil && rate > 0 {
				best := 1000 * rate
				rl["PrefixBestAvailableTokenRate"] = json.Number(strconv.FormatFloat(best, 'g', -1, 64))
				rl["PrefixPredictedTokenRate"] = json.Number(strconv.FormatFloat(kept(rate)*best, 'g', -1, 64))
				n++
			}
		}
		for _, c := range x {
			n += setPrefixPair(c, kept)
		}
	case []any:
		for _, c := range x {
			n += setPrefixPair(c, kept)
		}
	}
	return n
}

// servedInputsOf is the inputs document POST /v1/inputs gives for the one
// recording in dir.
func servedInputsOf(t *testing.T, dir string) whatifapi.InputsResponse {
	t.Helper()
	src := &servedSources{warn: io.Discard, recordings: dir}
	h := whatifapi.New(whatifapi.Config{Resolver: src, WhatifSchema: "schema-v" + strconv.Itoa(schemaVersion)}).Handler()
	code, body := apiPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording,
		Name: filepath.Base(q32Recording)}})
	require.Equal(t, 200, code, "%s", body)
	var in whatifapi.InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	return in
}

// A state that never read the router, a zero State as a source with no
// router series replays to, gives a policy with no router block, never one
// of empty verdicts, so its inputs document is as before.
func TestRouterPolicy_PolicyOfZeroStateHasNoRouter(t *testing.T) {
	p := policyOf(&answerFlags{}, &learnreplay.State{}, &pricedFleet{})
	assert.Nil(t, p.Router)
	b, err := json.Marshal(p)
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"router"`)

	st := &learnreplay.State{RouterPolicy: balance.DetectRouterPolicy([]balance.RouterCycle{{KeptKnown: true,
		PredictedTokenRate: 1, BestAvailableTokenRate: 1}})}
	p = policyOf(&answerFlags{}, st, &pricedFleet{})
	require.NotNil(t, p.Router, "a source that read the series carries the block, decided or not")
	assert.Equal(t, balance.PolicyNotDetected, p.Router.Policy)
}

// The planner names the router's policy as its series read it
// (balance.DetectRouterPolicy, the controller's reader): in the inputs
// document and in a TTFT answer, text
// and JSON, where the source holds the series. It moves no figure:
// --load-aware-router still prices f, and where the two disagree the
// answer says so.
func TestRouterPolicy_NamedInInputsAndAnswer(t *testing.T) {
	plain := servedInputsOf(t, q32Dir)
	assert.Nil(t, plain.Policy.Router, "the committed warm-up holds no prefix pair: no router block")

	dir := t.TempDir()
	// A router that keeps the whole hit at light load and gives some up as
	// the load rises: cache-aware and load-aware.
	rec := routerPolicyRecording(t, dir, func(rate float64) float64 { return 0.99 - 0.01*rate })
	got := servedInputsOf(t, dir).Policy.Router
	require.NotNil(t, got)
	assert.Equal(t, balance.PolicyCacheAndLoadAware, got.Policy, "%+v", got)
	assert.Equal(t, balance.RouterYes, got.LoadAwareByHit)
	assert.Equal(t, 2, got.Candidates)

	args := func(rec string, extra ...string) []string {
		return append([]string{"replicas-to-slo", "--recording", rec, "--prefill", "2", "--decode", "2",
			"--assume-input-tokens", "8000", "--assume-output-tokens", "200", "--ttft-percentile", "90",
			"--assume-prompt-cv", "0.0577", "--arrival-cv2", "1", "--flow-control", "off"}, extra...)
	}
	base, code := runCmd(t, args(q32Recording)...)
	require.Equal(t, 0, code, base)
	assert.NotContains(t, base, "(from its series)")
	withSeries, code := runCmd(t, args(rec)...)
	require.Equal(t, 0, code, withSeries)
	words := "; router " + balance.PolicyCacheAndLoadAware + " (from its series)"
	assert.Contains(t, withSeries, words)
	note := "f priced without --load-aware-router, but the router's series read it load-aware (" + balance.PolicyCacheAndLoadAware + ")"
	assert.Contains(t, withSeries, note)
	// Every figure as without the series: only the policy's words and the
	// note differ.
	stripped := strings.NewReplacer(words, "", note+"; ", "", "; "+note, "").Replace(withSeries)
	assert.Equal(t, strings.ReplaceAll(base, rec, q32Recording), strings.ReplaceAll(stripped, rec, q32Recording))

	// Stated, the flag agrees with the series: nothing is noted.
	stated, code := runCmd(t, args(rec, "--load-aware-router")...)
	require.Equal(t, 0, code, stated)
	assert.Contains(t, stated, words)
	assert.NotContains(t, stated, "but the router's series read it")

	js, code := runCmd(t, args(rec, "-o", "json")...)
	require.Equal(t, 0, code, js)
	assert.Contains(t, js, `"routerPolicy"`)
	assert.Contains(t, js, `"policy": "`+balance.PolicyCacheAndLoadAware+`"`)
	plainJS, _ := runCmd(t, args(q32Recording, "-o", "json")...)
	assert.NotContains(t, plainJS, `"routerPolicy"`, "absent where the source holds no series")
}
