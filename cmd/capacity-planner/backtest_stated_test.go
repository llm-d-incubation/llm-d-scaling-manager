package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// backtest and predict take the answers' --assume-prompt-cv,
// --assume-router-hop and --assume-itl-b, so that an answer can be checked
// with the inputs it stated (issue
// llm-d-incubation/llm-d-scaling-manager#277), and check them as an answer
// does, before any recording is read.
func TestBacktestStatedInputFlags(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"cv above 3":    {[]string{"--assume-prompt-cv", "5"}, "--assume-prompt-cv 5, want a cv from 0 to 3"},
		"negative cv":   {[]string{"--assume-prompt-cv", "-1"}, "--assume-prompt-cv -1, want a cv from 0 to 3"},
		"negative hop":  {[]string{"--assume-router-hop", "-1ms"}, "--assume-router-hop -1ms, want a duration of 0 or more"},
		"B not a value": {[]string{"--assume-itl-b", "low"}, `--assume-itl-b "low", want floor`},
		"B too large":   {[]string{"--assume-itl-b", "2s"}, `--assume-itl-b "2s", want floor`},
	} {
		for _, cmd := range []string{"backtest", "predict"} {
			t.Run(cmd+" "+name, func(t *testing.T) {
				args := append([]string{cmd, "--recording", "a"}, c.args...)
				if cmd == "predict" {
					args = append([]string{cmd, "--fit", "a", "--window", "b", "-o", "c"}, c.args...)
				}
				var out strings.Builder
				err := run(t.Context(), args, &out, &out)
				require.Error(t, err)
				assert.Equal(t, 2, exitCode(err), out.String())
				assert.Contains(t, err.Error(), c.want, out.String())
			})
		}
	}

	// Given, they reach the request as the API's assume; V_P and a as before.
	f := backtestFlags{assumePromptCV: 0, assumeRouterHop: 0, assumeITLB: "floor", assumeVP: 9000,
		set: map[string]bool{"assume-prompt-cv": true, "assume-router-hop": true, "assume-itl-b": true}}
	req, err := f.request()
	require.NoError(t, err)
	require.NotNil(t, req.Assume)
	a := req.Assume
	require.NotNil(t, a.PromptCV)
	assert.Zero(t, *a.PromptCV, "a cv of 0 is a stated cv")
	require.NotNil(t, a.RouterHop)
	assert.Zero(t, *a.RouterHop)
	assert.True(t, a.ITLBFromFloor)
	assert.Nil(t, a.ITLB)
	require.NotNil(t, a.VP)
	assert.Nil(t, a.A)

	f = backtestFlags{assumeITLB: "12ms", set: map[string]bool{"assume-itl-b": true}}
	req, err = f.request()
	require.NoError(t, err)
	require.NotNil(t, req.Assume)
	require.NotNil(t, req.Assume.ITLB)
	assert.InDelta(t, 0.012, *req.Assume.ITLB, 1e-12)
	assert.False(t, req.Assume.ITLBFromFloor)

	req, err = (&backtestFlags{routerWaitInPrefill: plannerRouterWaitInPrefill}).request()
	require.NoError(t, err)
	assert.Nil(t, req.Assume, "nothing stated, nothing assumed")
}
