package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// gappedRecording is the staircase with every event from 20 to 35 minutes
// after its first one removed, written to a temporary file: a capture with a
// 15-minute hole, as the review simulated (issue #315).
func gappedRecording(t *testing.T) string {
	t.Helper()
	return holedRecording(t, func(_ time.Time, d time.Duration) bool { return d >= 20*time.Minute && d < 35*time.Minute })
}

// holedRecording is the staircase without the events drop picks, by their
// time and their time since the first event, written to a temporary file
// with the rest renumbered.
func holedRecording(t *testing.T, drop func(now time.Time, sinceFirst time.Duration) bool) string {
	t.Helper()
	in, err := os.Open(stairBOn)
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "gapped.jsonl.gz")
	outF, err := os.Create(path)
	require.NoError(t, err)
	zw := gzip.NewWriter(outF)
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<28)
	var first time.Time
	kept, dropped := 0, 0
	for sc.Scan() {
		var ev struct{ Now time.Time }
		require.NoError(t, json.Unmarshal(sc.Bytes(), &ev))
		if first.IsZero() {
			first = ev.Now
		}
		if drop(ev.Now, ev.Now.Sub(first)) {
			dropped++
			continue
		}
		kept++
		// The reader wants consecutive sequence numbers: renumber the line.
		line := sc.Text()
		_, rest, ok := strings.Cut(line, ",")
		require.True(t, ok && strings.HasPrefix(line, `{"Seq":`), "premise: a line starts with its Seq")
		_, err := fmt.Fprintf(zw, "{\"Seq\":%d,%s\n", kept, rest)
		require.NoError(t, err)
	}
	require.NoError(t, sc.Err())
	require.NoError(t, zw.Close())
	require.NoError(t, outF.Close())
	require.Positive(t, dropped, "premise: the hole removed events")
	require.Positive(t, kept)
	return path
}

// A window with a 15-minute hole says so: cycles expected against present,
// the largest gap, and WindowGaps, in the text and the JSON. The staircase
// as recorded has no hole, and its report has no gaps line.
func TestAtReplicas_ReportsWindowGaps(t *testing.T) {
	gapped := gappedRecording(t)
	args := func(rec string, extra ...string) []string {
		return append([]string{"at-replicas", "--recording", rec, "--no-self-check"}, append(stairAssumed, extra...)...)
	}
	var out, warn strings.Builder
	require.NoError(t, run(t.Context(), args(gapped), &out, &warn), warn.String())
	text := out.String()
	assert.Contains(t, text, "window gaps", text)
	assert.Contains(t, text, "largest gap 15m", text)
	assert.Contains(t, text, whatifapi.FlagWindowGaps, text)

	out.Reset()
	require.NoError(t, run(t.Context(), args(gapped, "-o", "json"), &out, &warn), warn.String())
	var rep whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(out.String()), &rep))
	require.NotNil(t, rep.Source.Gaps)
	g := rep.Source.Gaps
	assert.True(t, g.Flagged)
	assert.Greater(t, g.ExpectedCycles, g.PresentCycles)
	assert.InDelta(t, 15*60, g.LargestGapSeconds, 2*g.CycleSeconds)

	out.Reset()
	require.NoError(t, run(t.Context(), args(stairBOn), &out, &warn), warn.String())
	assert.NotContains(t, out.String(), "window gaps")
}
