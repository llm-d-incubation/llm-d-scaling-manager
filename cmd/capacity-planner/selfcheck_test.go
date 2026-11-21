package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// withSelfCheck turns the self-check on, as it is outside the tests, with a
// fixed release.
func withSelfCheck(t *testing.T) {
	t.Helper()
	defaultSelfCheck, selfCheckVersion = true, func() string { return "vtest" }
	t.Cleanup(func() { defaultSelfCheck, selfCheckVersion = false, plannerVersion })
}

// selfCheckQ32 is at-replicas on the q32 staircase's warm-up recording.
var selfCheckQ32 = []string{"at-replicas", "--recording", q32Recording, "--ttft", "2s@p90"}

// The answer carries its self-check: the line, why, what to trust, and the
// issue to review, ahead of the answer. The JSON answer carries it too,
// and its issue holds no name of the recording: model, namespace,
// variants, file. (Each run replays the q32 warm-up: two runs, not more.)
func TestSelfCheckGolden(t *testing.T) {
	withSelfCheck(t)
	// The verdict rests on the figures the self-check finds inconsistent:
	// it is printed, says not trusted, and exits ExitUntrusted (#298).
	checkGolden(t, "at-replicas-q32-self-check", whatifapi.ExitUntrusted, selfCheckQ32)
	out, code := runCmd(t, append(selfCheckQ32, "-o", "json")...)
	require.Equal(t, whatifapi.ExitUntrusted, code, out)
	var rep whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	assert.Equal(t, whatifapi.ExitUntrusted, rep.ExitCode)
	sc := rep.SelfCheck
	require.NotNil(t, sc)
	assert.Equal(t, whatifapi.SelfCheckInconsistent, sc.Status, sc.Line)
	assert.Contains(t, sc.Untrusted, "ttft")
	require.NotEmpty(t, rep.Shapes)
	assert.True(t, strings.HasPrefix(rep.Shapes[0].Verdict.Trust, whatifapi.TrustNot+": "), rep.Shapes[0].Verdict.Trust)
	require.NotEmpty(t, sc.IssueURL)
	u, err := url.Parse(sc.IssueURL)
	require.NoError(t, err)
	body := u.Query().Get("diagnostics")
	for _, name := range []string{rep.Source.Model, rep.Source.Namespace, rep.Fleet.PrefillVariant, rep.Fleet.DecodeVariant, "warmup", "staircase-q32"} {
		require.NotEmpty(t, name)
		assert.NotContains(t, sc.IssueURL, name)
		assert.NotContains(t, body, name)
	}
	assert.Contains(t, body, "planner: vtest")

	// --no-self-check: no self-check, in either output.
	out, code = runCmd(t, append(selfCheckQ32, "-o", "json", "--no-self-check")...)
	require.Equal(t, 0, code, out)
	var off whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(out), &off))
	assert.Nil(t, off.SelfCheck)
}
