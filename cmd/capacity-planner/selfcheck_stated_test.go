package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statedCheck is the self-check of an -o json answer, read by key, so that
// the test reads the parent's output too.
type statedCheck struct {
	Status string   `json:"status"`
	Line   string   `json:"line"`
	Stated []string `json:"stated"`
	TTFT   *struct {
		Bias *float64 `json:"bias"`
	} `json:"ttft"`
	TPOT *struct {
		Steps   int            `json:"steps"`
		Refused map[string]int `json:"refused"`
	} `json:"tpot"`
}

// selfCheckOf runs args with -o json and returns the answer's self-check.
func selfCheckOf(t *testing.T, args ...string) statedCheck {
	t.Helper()
	out, code := runCmd(t, append(args, "-o", "json")...)
	require.NotEqual(t, 2, code, out)
	var doc struct {
		SelfCheck *statedCheck `json:"selfCheck"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc), out)
	require.NotNil(t, doc.SelfCheck, "no self-check")
	return *doc.SelfCheck
}

// The self-check replays the recording with the prompt spread the answer
// states, and its line names the input. Before issue
// llm-d-incubation/llm-d-scaling-manager#277 the line was the same with and
// without it.
//
// The self-check scores the mean (#291). On the q32 warm-up each step reads
// its own prompt cv, 0.3 to 0.45, from the prefill pods' histogram (the
// answer's default cv 2 is not the steps'). At a cv past
// whatif.MaxPSPromptCV the larger of processor sharing's and first come's
// mean is priced; first come's lies above processor sharing's only past
// cv 1, and processor sharing's mean does not depend on the spread. So
// --assume-prompt-cv 0.06 leaves the mean bias where it was (both read
// 1.6890; it moves the percentiles), and the stated cv is shown to reach
// the replay's pricing by a cv past 1: --assume-prompt-cv 3 prices first
// come's larger mean on every step, and the bias rises.
func TestSelfCheckReplaysTheStatedPromptCV(t *testing.T) {
	withSelfCheck(t)
	plain := selfCheckOf(t, selfCheckQ32...)
	narrow := selfCheckOf(t, append(selfCheckQ32, "--assume-prompt-cv", "0.06")...)
	wide := selfCheckOf(t, append(selfCheckQ32, "--assume-prompt-cv", "3")...)
	assert.Empty(t, plain.Stated)
	assert.True(t, strings.HasPrefix(plain.Line, "Self-check: tested on the last "), plain.Line)
	assert.Equal(t, []string{"--assume-prompt-cv 0.06"}, narrow.Stated)
	assert.True(t, strings.HasPrefix(narrow.Line, "Self-check, replayed with the stated --assume-prompt-cv 0.06 (flagged as stated): tested on the last "), narrow.Line)
	assert.Equal(t, []string{"--assume-prompt-cv 3"}, wide.Stated)
	for _, sc := range []statedCheck{plain, narrow, wide} {
		require.NotNil(t, sc.TTFT, sc.Line)
		require.NotNil(t, sc.TTFT.Bias, sc.Line)
	}
	assert.InDelta(t, *plain.TTFT.Bias, *narrow.TTFT.Bias, 1e-9, "below cv 1 the priced mean is processor sharing's, which the spread does not move")
	assert.Greater(t, *wide.TTFT.Bias, *plain.TTFT.Bias, "the stated cv 3 reaches the replay's pricing: first come's mean, above processor sharing's")
}

// The self-check replays the recording with the ITL B the answer states: on
// a staircase phase whose decode line rests on the default intercept, the
// default self-check refuses every p90 TPOT step PinnedDefaultB, and with
// --assume-itl-b floor it scores them, flagged as stated.
func TestSelfCheckReplaysTheStatedITLB(t *testing.T) {
	withSelfCheck(t)
	sc := selfCheckOf(t, "at-replicas", "--recording", stair("ttft-staircase-b-on"), "--assume-itl-b", "floor")
	assert.Equal(t, []string{"--assume-itl-b floor"}, sc.Stated)
	assert.True(t, strings.HasPrefix(sc.Line, "Self-check, replayed with the stated --assume-itl-b floor (flagged as stated, outside the validated region)"), sc.Line)
	require.NotNil(t, sc.TPOT, sc.Line)
	assert.Zero(t, sc.TPOT.Refused["PinnedDefaultB"], "the fits priced through the stated B: %s", sc.Line)
	assert.Positive(t, sc.TPOT.Steps, sc.Line)
}
