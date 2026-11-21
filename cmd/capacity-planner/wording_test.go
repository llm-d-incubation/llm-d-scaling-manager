package main

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// help lists every command with what it does, and a command's own -h starts
// with it: replicas-to-slo says it does not search for a replica count, and
// at-replicas that it does (#317).
func TestHelp_SaysWhatEachCommandDoes(t *testing.T) {
	out, err := runCLI("--help")
	require.NoError(t, err)
	require.Len(t, commandHelp, len(commands), "every command is listed once")
	for name := range commands {
		what := commandWhat(name)
		require.NotEmpty(t, what, name)
		assert.Contains(t, out, "  "+name, name)
		assert.Contains(t, out, what, name)
	}
	assert.Contains(t, commandWhat(replicasQuestion), "does not search for a replica count")
	assert.Contains(t, commandWhat(reportQuestion), "how many replicas")

	h, err := runCLI(replicasQuestion, "-h")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(h, progName+" "+replicasQuestion+": "+commandWhat(replicasQuestion)), h)
	assert.Contains(t, h, "-cycle")
}

// at-replicas judges one decode variant and refuses a model with several:
// its --variant help says so, not that an empty value prices them all.
func TestAtReplicas_VariantHelpSaysOne(t *testing.T) {
	h, err := runCLI(reportQuestion, "-h")
	require.NoError(t, err)
	assert.Contains(t, h, "required when the model has several")
	assert.NotContains(t, h, "prices every decode variant")
}

// A NoLine refusal says what to do: a later cycle, or a busier window.
func TestNoLine_SaysWhatToDo(t *testing.T) {
	out, code := runCmd(t, "replicas-to-slo", "--recording", olsSweep, "--cycle", "1")
	require.Equal(t, 3, code, out)
	assert.Contains(t, out, "no ITL line learned by cycle 1; the line needs a window in which decode's KV-cache use spans 0.30 or more: "+
		"answer at a later cycle where one is learned (--cycle; source.cycle on the API), or record a busier window")
}

// The hint is the same on the CLI and the API (TestServeParity holds their
// refusals equal), and names both ways to pick a cycle.
func TestNoLine_HintIsTheSameOnTheAPI(t *testing.T) {
	h := pricedVariant{api: true}.noLineHint()
	assert.Equal(t, pricedVariant{}.noLineHint(), h)
	assert.Contains(t, h, "--cycle; source.cycle on the API")
}

// longFloat is a number printed to more digits than anyone reads.
var longFloat = regexp.MustCompile(`\d\.\d{5,}`)

// A Saturated refusal prints its rates to four figures, not to the float's
// last digit ("at or above the knee 2.0111384812509345 r/s").
func TestSaturated_RatesAreRounded(t *testing.T) {
	out, code := runCmd(t, slices.Concat([]string{"replicas-to-slo", "--recording", olsSweep}, workedShape, []string{"--rate", "20"})...)
	require.Equal(t, 3, code, out)
	require.Contains(t, out, "at or above the knee")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Saturated") {
			assert.NotRegexp(t, longFloat, line)
		}
	}
}
