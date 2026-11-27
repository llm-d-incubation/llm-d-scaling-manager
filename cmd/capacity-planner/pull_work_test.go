package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
)

// On the public P/D staircase with flow control on, the planner priced the KV
// pull from the decode rows' last cycles, a NIXL transfer of about 60 ms read at
// about 21 r/s a decode pod, as the pull's work, and stretched it again as a
// processor-sharing stage: at 30.8 r/s the pull ran at utilization 0.93 and the
// mean TTFT read 1.02 s, where the router's p50 was 0.16 s and its p90 0.28 s.
// The pull then set the decode count for a TTFT target: 4 decode pods where
// decode itself needs 3. slo.SwitchTTFTPullWork reads the work at each row's
// rate (about 28 ms), and the pull reads what the rows measured (issue
// llm-d-incubation/llm-d-scaling-manager#301).
func TestAtReplicas_PullWorkPricesTheMeasuredPull(t *testing.T) {
	args := atStair("--assume-prompt-cv", "0.06", "--ttft", "600ms@p95", "--ttft-percentile", "mean,95", "--load", "30.8r/s")
	read := func(on bool) (pull, mean float64, text string) {
		if on {
			testReplaySwitches = map[string]map[string]bool{learnreplay.PackageSLO: {slo.SwitchTTFTPullWork: true}}
			defer func() { testReplaySwitches = nil }()
		}
		rep := concReport(t, args...)
		require.Len(t, rep.Shapes, 1)
		s := rep.Shapes[0]
		require.NotNil(t, s.Utilization.Decode.KVPull)
		row := rowOf(t, s, "30.8r/s")
		for _, e := range row.TTFT {
			if e.Statistic == backtestMean {
				require.NotNil(t, e.Value, "%+v", e)
				mean = *e.Value
			}
		}
		text, _ = runCmd(t, args...)
		return s.Utilization.Decode.KVPull.Utilization, mean, text
	}

	pull, mean, text := read(false)
	assert.Greater(t, pull, 1.0, "off: the loaded time as work saturates the pull at the load seen")
	assert.Greater(t, mean, 0.9)
	assert.Contains(t, text, "needs 3 prefill + 4 decode")

	pull, mean, text = read(true)
	assert.InDelta(t, 0.585, pull, 0.02, "on: the pull's work, 28 ms a request")
	assert.Less(t, mean, 0.3, "the pull is the measured transfer, not a queue")
	assert.Contains(t, text, "needs 3 prefill + 3 decode", "decode's own knee sets the count, not the pull")
}
