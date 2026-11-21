package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lightQueueTokens is the computed-token rate under which the synthetic
// recording (lowerBoundRecording) keeps a prefill pod's capacity queue: its
// light minutes queue, its loaded ones do not.
const lightQueueTokens = 3000

// lowerBoundRecording writes the q32 staircase's warm-up (q32Recording) with
// its prefill pods' capacity queue rewritten, and returns its path: a fleet
// whose pods queue for capacity only in light minutes, as on the fleet of
// issue llm-d-incubation/llm-d-scaling-manager#280, whose recording is not
// shareable. Every prefill row reads a capacity queue of one where it
// computed under lightQueueTokens, and none elsewhere (its whole waiting
// count is left as recorded, so the low-load minutes a is fitted on stay
// low-load), and no queue minimum,
// so V_P has no busy minutes: the peak under a queue is a light minute's,
// a lower bound far under the 8k phases' rate. The rest is the warm-up's.
// The fleet's prefix hit is the warm-up's (none): only the queue is that
// fleet's shape.
func lowerBoundRecording(t *testing.T) string {
	t.Helper()
	in, err := os.Open(q32Recording)
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "lower-bound.jsonl.gz")
	out, err := os.Create(path)
	require.NoError(t, err)
	zw := gzip.NewWriter(out)
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	rewritten := 0
	for sc.Scan() {
		// Numbers stay as written: a float would turn an int64 field's
		// 1000000 into 1e+06, which the replay cannot read back.
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		var ev map[string]any
		require.NoError(t, dec.Decode(&ev))
		rewritten += rewriteQueues(ev["Data"])
		b, err := json.Marshal(ev)
		require.NoError(t, err)
		_, err = zw.Write(append(b, '\n'))
		require.NoError(t, err)
	}
	require.NoError(t, sc.Err())
	require.NoError(t, zw.Close())
	require.NoError(t, out.Close())
	require.Positive(t, rewritten, "the warm-up has prefill rows to rewrite")
	return path
}

// rewriteQueues rewrites the prefill rows of every analyzer input under v
// (lowerBoundRecording) and returns how many it rewrote.
func rewriteQueues(v any) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		rows, okRows := x["ReplicaMetrics"].([]any)
		states, okStates := x["VariantStates"].([]any)
		if okRows && okStates {
			prefill := map[string]bool{}
			for _, s := range states {
				if m, ok := s.(map[string]any); ok && m["Role"] == "prefill" {
					prefill[m["VariantName"].(string)] = true
				}
			}
			for _, r := range rows {
				m, ok := r.(map[string]any)
				if !ok || !prefill[m["VariantName"].(string)] {
					continue
				}
				q := json.Number("0")
				if tok := asFloat(m["PrefillComputedTokenRate"]); tok > 0 && tok < lightQueueTokens {
					q = "1"
				}
				m["CapacityQueueKnown"], m["CapacityQueueLength"] = true, q
				m["QueueMinKnown"] = false
				n++
			}
			return n
		}
		for _, c := range x {
			n += rewriteQueues(c)
		}
	case []any:
		for _, c := range x {
			n += rewriteQueues(c)
		}
	}
	return n
}

// asFloat is a JSON number as a float, 0 for anything else.
func asFloat(v any) float64 {
	n, _ := v.(json.Number)
	f, _ := n.Float64()
	return f
}

// TestLowerBoundKnee_Golden pins the planner on the synthetic fleet
// (lowerBoundRecording): V_P floored at the rate the pods were seen to
// compute (prefill-vp-floor-served), and the steps past a knee priced from
// it not priced, not called overloaded (whatif.PastLowerBoundKnee). go test
// ./cmd/capacity-planner -run TestLowerBoundKnee_Golden -update rewrites it.
func TestLowerBoundKnee_Golden(t *testing.T) {
	rec := lowerBoundRecording(t)
	for _, c := range []struct {
		name string
		args []string
	}{
		{"lower-bound-knee-at-replicas", []string{"at-replicas", "--recording", rec}},
		{"lower-bound-knee-backtest", []string{"backtest", "--recording", rec, "--split", "50%"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, _ := runCmd(t, c.args...)
			got = strings.ReplaceAll(got, rec, "<synthetic lower-bound recording>")
			path := filepath.Join("testdata", c.name+".golden")
			if *update {
				require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
				return
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err, "go test ./cmd/capacity-planner -run TestLowerBoundKnee_Golden -update writes it")
			assert.Equal(t, strings.ReplaceAll(string(want), "\r\n", "\n"), got)
		})
	}
}

// On the synthetic fleet the knee is priced from a floored lower bound, and
// no step of the backtest is called overloaded on TTFT.
func TestLowerBoundKnee_NoStepIsCalledOverloaded(t *testing.T) {
	rec := lowerBoundRecording(t)
	got, _ := runCmd(t, "slo-to-rate", "--recording", rec, "--ttft", "20s", "-o", "json")
	assert.Contains(t, got, `"VPLowerBound"`)
	assert.Contains(t, got, `"vpOrigin": "peak-under-queue"`)
	bt, _ := runCmd(t, "backtest", "--recording", rec, "--split", "50%")
	ttft := 0
	for _, line := range strings.Split(bt, "\n") {
		if strings.HasPrefix(line, "TTFT ") {
			ttft++
			assert.NotContains(t, line, "refused Saturated", line)
		}
	}
	require.Positive(t, ttft, "no TTFT line in the backtest:\n%s", bt)
	assert.Contains(t, bt, "refused PastLowerBoundKnee")
}
