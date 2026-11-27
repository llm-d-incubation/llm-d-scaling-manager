package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// neverQueuedRecording writes the q32 staircase's warm-up (q32Recording) with
// every prefill row's capacity queue rewritten to none and no queue minimum,
// and returns its path: a fleet whose prefill pods never queue for capacity,
// as on a production fleet sized for its load, whose recording is not
// shareable. V_P then has no source but the served peak
// (prefill.SourceServedPeak). The rest is the warm-up's.
func neverQueuedRecording(t *testing.T) string {
	t.Helper()
	in, err := os.Open(q32Recording)
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "never-queued.jsonl.gz")
	out, err := os.Create(path)
	require.NoError(t, err)
	zw := gzip.NewWriter(out)
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	rewritten := 0
	for sc.Scan() {
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		var ev map[string]any
		require.NoError(t, dec.Decode(&ev))
		rewritten += rewriteNoQueue(ev["Data"])
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

// rewriteNoQueue sets no capacity queue and no queue minimum on the prefill
// rows of every analyzer input under v, and returns how many it rewrote.
func rewriteNoQueue(v any) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		rows, okRows := x["ReplicaMetrics"].([]any)
		states, okStates := x["VariantStates"].([]any)
		if okRows && okStates {
			prefill := map[string]bool{}
			for _, s := range states {
				if m, ok := s.(map[string]any); ok && m["Role"] == rolePrefill {
					prefill[m["VariantName"].(string)] = true
				}
			}
			for _, r := range rows {
				m, ok := r.(map[string]any)
				if !ok || !prefill[m["VariantName"].(string)] {
					continue
				}
				m["CapacityQueueKnown"], m["CapacityQueueLength"] = true, json.Number("0")
				m["QueueMinKnown"] = false
				n++
			}
			return n
		}
		for _, c := range x {
			n += rewriteNoQueue(c)
		}
	case []any:
		for _, c := range x {
			n += rewriteNoQueue(c)
		}
	}
	return n
}

// TestNeverQueued_Golden pins the planner on the synthetic never-queued fleet
// (neverQueuedRecording): V_P only the served peak, the prefill knee at
// least its rate, and no TTFT priced from it. go test ./cmd/capacity-planner
// -run TestNeverQueued_Golden -update rewrites it.
func TestNeverQueued_Golden(t *testing.T) {
	rec := neverQueuedRecording(t)
	for _, c := range []struct {
		name string
		args []string
	}{
		{"never-queued-at-replicas", []string{"at-replicas", "--recording", rec}},
		{"never-queued-backtest", []string{"backtest", "--recording", rec, "--split", "50%"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, _ := runCmd(t, c.args...)
			got = strings.ReplaceAll(got, rec, "<synthetic never-queued recording>")
			path := filepath.Join("testdata", c.name+".golden")
			if *update {
				require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
				return
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err, "go test ./cmd/capacity-planner -run TestNeverQueued_Golden -update writes it")
			assert.Equal(t, strings.ReplaceAll(string(want), "\r\n", "\n"), got)
		})
	}
}

// A fleet past its decode operating point whose TTFT no fleet prices (V_P
// only the served peak) is UNDER-PROVISIONED on decode, and its need is
// decode's alone, at least that many: never "no fleet holds", which read the
// TTFT no fleet prices as a target every fleet misses.
// The same where the TTFT is priced only as an upper bound over its target
// (lowerBoundRecording: V_P floored at the served rate, the target under a
// floor that is itself at most its figure): no fleet is shown to meet it or
// to miss it.
func TestNeverQueued_NeedIsDecodesWhereNoTTFTPrices(t *testing.T) {
	for name, rec := range map[string]string{"never queued": neverQueuedRecording(t), "lower-bound knee": lowerBoundRecording(t)} {
		t.Run(name, func(t *testing.T) {
			rep := atReplicasJSON(t, "at-replicas", "--recording", rec)
			require.NotEmpty(t, rep.Shapes)
			v := rep.Shapes[0].Verdict
			require.Equal(t, whatifapi.VerdictUnder, v.Verdict, v.Summary)
			require.NotNil(t, v.Needed, v.Summary)
			assert.Empty(t, v.Needed.Unmet, v.Summary)
			assert.True(t, v.Needed.DecodeOnly, v.Summary)
			assert.Greater(t, v.Needed.Decode, rep.Fleet.Decode, v.Summary)
			assert.NotContains(t, v.Summary, "no fleet up to")
			assert.Contains(t, v.Summary, "needs at least")
		})
	}
}

// kneeRow is the rate of a capacity row ("prefill knee", "fleet knee"), and
// whether it reads as a lower bound (">=").
func kneeRow(t *testing.T, out, row string) (float64, bool) {
	t.Helper()
	m := regexp.MustCompile(`(?m)^  ` + row + `\s+(>=)?([\d.]+)\s`).FindStringSubmatch(out)
	require.NotNil(t, m, "no %q row with a rate:\n%s", row, out)
	v, err := strconv.ParseFloat(m[2], 64)
	require.NoError(t, err)
	return v, m[1] == ">="
}

// On the never-queued fleet the prefill knee is priced, and as a lower
// bound: V_P is the served peak (refused before the switch). No TTFT is
// priced from it, so no TTFT figure can read under what was measured: every
// TTFT the backtest gives is refused, NoPrefillCapacity below the knee and
// PastLowerBoundKnee past it, never Saturated. The window does not
// contradict a lower-bound knee (#297).
func TestNeverQueued_KneeIsALowerBoundAndNoTTFTIsPriced(t *testing.T) {
	rec := neverQueuedRecording(t)
	atr, _ := runCmd(t, "at-replicas", "--recording", rec)
	pk, lb := kneeRow(t, atr, "prefill knee")
	assert.True(t, lb, "the prefill knee is at least its rate:\n%s", atr)
	assert.Positive(t, pk)
	assert.Contains(t, atr, "is only the rate a prefill pod was seen to compute")
	assert.NotContains(t, atr, contradictsWords)

	bt, _ := runCmd(t, "backtest", "--recording", rec, "--split", "50%", "-o", "json")
	var resp struct {
		Steps []struct {
			Excluded string `json:"excluded"`
			TTFT     []struct {
				Predicted struct {
					Value   *float64 `json:"value"`
					Refused *struct {
						Reason string `json:"reason"`
					} `json:"refused"`
				} `json:"predicted"`
			} `json:"ttft"`
		} `json:"steps"`
	}
	require.NoError(t, json.Unmarshal([]byte(bt), &resp))
	judged := 0
	for _, st := range resp.Steps {
		if st.Excluded != "" {
			continue
		}
		judged++
		for _, p := range st.TTFT {
			assert.Nil(t, p.Predicted.Value, "a TTFT priced from a served-only V_P")
			require.NotNil(t, p.Predicted.Refused)
			assert.Contains(t, []string{string(whatif.NoPrefillCapacity), string(whatif.PastLowerBoundKnee)}, p.Predicted.Refused.Reason)
		}
	}
	require.Positive(t, judged)
}

// A prefill knee priced from a lower bound of V_P is raised to the rate the
// prefill pods' sustained work serves at the prompt priced, at the pod count
// priced, and flagged; a knee from a V_P that is no bound, or the caller's,
// or at another pod count, or over the floor, is left as priced.
func TestServedKneeFloor(t *testing.T) {
	pf := &pricedFleet{fleet: whatif.TTFTFleet{Prefill: whatif.Prefill{Replicas: 2, PromptTokens: 1000}},
		servedComputed: map[int]float64{2: 30000, 3: 90000}}
	lb := whatif.Answer{Value: 10, Flags: []whatif.Flag{whatif.VPLowerBound},
		Basis: whatif.Basis{TTFT: &whatif.TTFTBasis{KneeRate: 10}}}

	got, ok := pf.servedKneeFloor(lb).(whatif.Answer)
	require.True(t, ok)
	assert.InDelta(t, 30.0, got.Value, 1e-9, "30000 tok/s over 1000-token prompts")
	assert.True(t, got.HasFlag(kneeAtServedWork))
	assert.True(t, got.HasFlag(whatif.VPLowerBound), "still a lower bound")
	assert.InDelta(t, 30.0, got.Basis.TTFT.KneeRate, 1e-9)
	require.NotNil(t, got.Sensitivity, "the operating point reads the knee's span")
	assert.InDelta(t, 30.0, got.Sensitivity.Lo, 1e-9, "no lower than the work measured")
	assert.InDelta(t, 10.0, lb.Basis.TTFT.KneeRate, 1e-9, "the priced answer is not changed in place")
	assert.False(t, lb.HasFlag(kneeAtServedWork))

	plain := whatif.Answer{Value: 10}
	assert.Equal(t, plain, pf.servedKneeFloor(plain), "a knee from a V_P that is no bound: a window past it contradicts the model")
	high := whatif.Answer{Value: 40, Flags: []whatif.Flag{whatif.VPLowerBound}}
	assert.Equal(t, high, pf.servedKneeFloor(high), "never lowered")

	other := *pf
	other.fleet.Prefill.Replicas = 4
	assert.Equal(t, lb, other.servedKneeFloor(lb), "no work read at the pod count priced")
	stated := *pf
	stated.assumedVP = true
	assert.Equal(t, lb, stated.servedKneeFloor(lb), "--assume-vp is priced as given")
	none := *pf
	none.servedComputed = nil
	assert.Equal(t, lb, none.servedKneeFloor(lb), "the switch off reads no work")
	refused := whatif.Unanswerable{Reason: whatif.NoPrefillCapacity}
	assert.Equal(t, whatif.Result(refused), pf.servedKneeFloor(refused))
}

// #477: where the prefill pods served at the pod count priced only with
// their backlog growing, no work floors the knee: it is left at its lower
// bound and flagged ServedWorkUnsteady. A count with steady work is floored
// as before, unflagged.
func TestServedKneeFloor_UnsteadyWorkIsNoFloor(t *testing.T) {
	pf := &pricedFleet{fleet: whatif.TTFTFleet{Prefill: whatif.Prefill{Replicas: 2, PromptTokens: 1000}},
		servedComputed: map[int]float64{3: 90000}, servedUnsteady: map[int]bool{2: true}}
	lb := whatif.Answer{Value: 10, Flags: []whatif.Flag{whatif.VPLowerBound}}
	got, ok := pf.servedKneeFloor(lb).(whatif.Answer)
	require.True(t, ok)
	assert.InDelta(t, 10.0, got.Value, 1e-9, "left at its lower bound")
	assert.True(t, got.HasFlag(servedWorkUnsteady))
	assert.False(t, got.HasFlag(kneeAtServedWork))
	assert.False(t, lb.HasFlag(servedWorkUnsteady), "the priced answer is not changed in place")

	steady := *pf
	steady.fleet.Prefill.Replicas = 3
	got, ok = steady.servedKneeFloor(lb).(whatif.Answer)
	require.True(t, ok)
	assert.InDelta(t, 90.0, got.Value, 1e-9)
	assert.False(t, got.HasFlag(servedWorkUnsteady))
}

// A knee raised to the work served (KneeAtServedWork) is still a lower
// bound: a window that served past it does not contradict it (#297), as
// it does not a lower-bound knee left as priced.
func TestServedKneeFloor_NeverContradictsTheWindow(t *testing.T) {
	floored := fleetEval{kneeOK: true, knee: 4, binds: rolePrefill,
		prefillKnee: whatif.Answer{Value: 4, Flags: []whatif.Flag{whatif.VPLowerBound, kneeAtServedWork}}}
	assert.Empty(t, contradictsReporter(cycles(10, 22), 12, 2, 2).contradicted(2, 2, floored))
}
