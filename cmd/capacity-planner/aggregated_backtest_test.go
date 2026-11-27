package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// aggregatedRecording writes the recording at src as an
// aggregated fleet and returns its path: every analyzer input loses its
// prefill variant's rows and states, and the decode variant serves both
// roles. scale below 1 multiplies every KV fill by it and divides the KV
// capacity by it, so a request holds the tokens it did and only the fill
// moves: a pod with a larger cache, which serves the recording under the ITL
// band's bottom (issue llm-d-incubation/llm-d-scaling-manager#512).
func aggregatedRecording(t *testing.T, src string, scale float64) string {
	t.Helper()
	in, err := os.Open(src)
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "aggregated.jsonl.gz")
	out, err := os.Create(path)
	require.NoError(t, err)
	zw := gzip.NewWriter(out)
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	rows := 0
	for sc.Scan() {
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		var ev map[string]any
		require.NoError(t, dec.Decode(&ev))
		rows += rewriteAggregated(ev["Data"], scale)
		b, err := json.Marshal(ev)
		require.NoError(t, err)
		_, err = zw.Write(append(b, '\n'))
		require.NoError(t, err)
	}
	require.NoError(t, sc.Err())
	require.NoError(t, zw.Close())
	require.NoError(t, out.Close())
	require.Positive(t, rows, "the warm-up has decode rows to rewrite")
	return path
}

// rewriteAggregated makes every analyzer input under v aggregated, as
// aggregatedRecording says, and returns how many rows it kept.
func rewriteAggregated(v any, scale float64) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		rows, okRows := x["ReplicaMetrics"].([]any)
		states, okStates := x["VariantStates"].([]any)
		if !okRows || !okStates {
			for _, c := range x {
				n += rewriteAggregated(c, scale)
			}
			return n
		}
		prefill := map[string]bool{}
		var kept []any
		for _, s := range states {
			m := s.(map[string]any)
			if m["Role"] == rolePrefill {
				prefill[m["VariantName"].(string)] = true
				continue
			}
			m["Role"] = "both"
			kept = append(kept, m)
		}
		x["VariantStates"] = kept
		var keptRows []any
		for _, r := range rows {
			m := r.(map[string]any)
			if prefill[m["VariantName"].(string)] {
				continue
			}
			for _, f := range []string{"KvCacheUsage", "KvUsageMean", "KvUsageInstant", "MaxRankKvCacheUsage"} {
				if v, ok := m[f].(json.Number); ok {
					fv, _ := v.Float64()
					m[f] = fv * scale
				}
			}
			for _, f := range []string{"TotalKvCapacityTokens", "NumGpuBlocks"} {
				if v, ok := m[f].(json.Number); ok {
					if iv, err := v.Int64(); err == nil && iv > 0 && iv < 1<<62 {
						m[f] = int64(float64(iv)/scale + 0.5)
					}
				}
			}
			keptRows = append(keptRows, m)
			n++
		}
		x["ReplicaMetrics"] = keptRows
	case []any:
		for _, c := range x {
			n += rewriteAggregated(c, scale)
		}
	}
	return n
}

// backtestOf runs backtest with args and decodes its answer.
func backtestOf(t *testing.T, args ...string) whatifapi.BacktestResponse {
	t.Helper()
	out, code := runCmd(t, append([]string{"backtest"}, append(args, "-o", "json")...)...)
	require.NotEqual(t, 2, code, out)
	var r whatifapi.BacktestResponse
	require.NoError(t, json.Unmarshal([]byte(out), &r), out)
	return r
}

// lastError is the error the command line returns, in words.
func lastError(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := run(t.Context(), args, &out, &out); err != nil {
		return err.Error()
	}
	return ""
}

// A backtest of an aggregated recording runs (issue #512): each step's TTFT
// is refused as aggregated, and its TPOT is the decode line's. The warm-up
// served under the band's bottom learns no line: every judged step's TPOT is
// refused NoLine naming the highest fill read, and the verdict says so in the
// same terms, not "fit on more".
func TestAggregatedBacktest_RefusesTPOTNamingTheFill(t *testing.T) {
	r := backtestOf(t, "--recording", aggregatedRecording(t, q32Recording, 0.1), "--split", "50%")
	judged := 0
	for _, st := range r.Steps {
		if st.Excluded != "" {
			continue
		}
		judged++
		require.NotEmpty(t, st.TTFT)
		require.NotNil(t, st.TTFT[0].Predicted.Refused)
		assert.Equal(t, string(whatif.NoPrefillReplicas), st.TTFT[0].Predicted.Refused.Reason)
		ref := st.TPOT[0].Predicted.Refused
		require.NotNil(t, ref, "no line by fill: TPOT refused")
		assert.Equal(t, string(whatif.NoLine), ref.Reason)
		assert.Contains(t, ref.Detail, "KV fill never reached the band's bottom: the highest steady fill read was 0.0")
	}
	require.Positive(t, judged, "steps judged: %s", r.Excluded)
	assert.Contains(t, r.Verdict, "the aggregated pods learned no ITL line by KV fill")
	assert.NotContains(t, r.Verdict, "too few busy minutes")
}

// Where the aggregated pods' line prices, the backtest prices TPOT from it:
// the 0.6B staircase (B on) at its own fill, its pinned-B line read through
// the floor's B as --assume-itl-b floor states it, prices the mean TPOT of
// every judged step.
func TestAggregatedBacktest_PricesTPOTFromTheDecodeLine(t *testing.T) {
	r := backtestOf(t, "--recording", aggregatedRecording(t, stairBOn, 1), "--split", "50%", "--assume-itl-b", "floor")
	judged := 0
	for _, st := range r.Steps {
		if st.Excluded != "" {
			continue
		}
		judged++
		require.NotNil(t, st.TTFT[0].Predicted.Refused)
		assert.NotNil(t, st.TPOT[0].Predicted.Value, "the mean TPOT is priced: %+v", st.TPOT[0].Predicted.Refused)
	}
	require.Positive(t, judged, "steps judged: %s", r.Excluded)
}

// The fit and the test must be the same kind of fleet: an aggregated fit
// tested on the P/D recording is refused, saying so.
func TestAggregatedBacktest_RefusesAFitAndATestOfAnotherKind(t *testing.T) {
	assert.Contains(t, lastError(t, "backtest", "--fit", aggregatedRecording(t, q32Recording, 1), "--test", q32Recording), "not the same kind of fleet")
}
