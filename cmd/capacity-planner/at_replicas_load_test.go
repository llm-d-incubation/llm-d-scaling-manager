package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// atReplicasJSON runs at-replicas with args and -o json, and reads its report:
// it answered, exit 0 or 3.
func atReplicasJSON(t *testing.T, args ...string) whatifapi.ReportResponse {
	t.Helper()
	text, code := runCmd(t, append(slices.Clone(args), "-o", "json")...)
	require.Contains(t, []int{0, 3}, code, text)
	var rep whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(text), &rep))
	require.NotEmpty(t, rep.Shapes, text)
	return rep
}

// A fleet is judged at the busy part of the window, not at its mean, which
// hides the peak. On the staircase the mean is 31.59 r/s, the p95 41.70 and
// the peak 43.24: by default the load judged is the p95, and at the peak
// three prefill and four decode replicas, over-provisioned at the mean, are
// under-provisioned: 43.24 is past 0.85 of their 49.99 r/s knee, priced at
// f 1.5 on the third pod (--imbalance-across-counts=false; read off the
// curve, f is 1 there and the knee 74 r/s, issue
// llm-d-incubation/llm-d-scaling-manager#300). On the
// throughput sweep the mean is 6.9 r/s, the p95 26.9 and the peak 30.04, past
// 0.85 of the 30.88 r/s decode knee: under-provisioned at the default and at
// the peak.
func TestAtReplicas_JudgedAtTheBusyLoad(t *testing.T) {
	t.Run("staircase 3 prefill + 4 decode", func(t *testing.T) {
		args := atStair("--prefill", "3", "--decode", "4", "--imbalance-across-counts=false")
		rep := atReplicasJSON(t, args...)
		require.NotNil(t, rep.Load.Rate.Value)
		assert.InDelta(t, 41.70, *rep.Load.Rate.Value, 0.005, "the default load is the p95, not the mean")
		assert.Equal(t, whatifapi.VerdictOver, rep.Shapes[0].Verdict.Verdict, "decode has one to spare even at the peak: %s",
			rep.Shapes[0].Verdict.Summary)

		peak := atReplicasJSON(t, append(args, "--load-statistic", "peak")...)
		require.NotNil(t, peak.Load.Peak)
		require.NotNil(t, peak.Load.Rate.Value)
		assert.InDelta(t, 43.24, *peak.Load.Peak, 0.005)
		assert.Equal(t, *peak.Load.Peak, *peak.Load.Rate.Value)
		assert.Equal(t, whatifapi.VerdictUnder, peak.Shapes[0].Verdict.Verdict, peak.Shapes[0].Verdict.Reason)
	})
	t.Run("throughput sweep, decode only", func(t *testing.T) {
		args := []string{"at-replicas", "--recording", outputSpreadRun, "--tpot", "5ms"}
		rep := atReplicasJSON(t, args...)
		require.NotNil(t, rep.Load.P95)
		assert.InDelta(t, 26.90, *rep.Load.P95, 0.005, "the sweep's p95")
		assert.Equal(t, whatifapi.VerdictUnder, rep.Shapes[0].Verdict.Verdict, "at the default load: %s", rep.Shapes[0].Verdict.Reason)

		peak := atReplicasJSON(t, append(args, "--load-statistic", "peak")...)
		require.NotNil(t, peak.Load.Peak)
		require.NotNil(t, peak.Load.Rate.Value)
		assert.InDelta(t, 30.04, *peak.Load.Peak, 0.005)
		assert.Equal(t, *peak.Load.Peak, *peak.Load.Rate.Value)
		assert.Equal(t, whatifapi.VerdictUnder, peak.Shapes[0].Verdict.Verdict, peak.Shapes[0].Verdict.Reason)
	})
}
