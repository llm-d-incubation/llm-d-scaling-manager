package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// lowerBoundReport is at-replicas on the synthetic lower-bound fleet
// (lowerBoundRecording) at a stated load, as JSON, and its one shape.
func lowerBoundReport(t *testing.T, rec, load string) whatifapi.ReportShape {
	t.Helper()
	got, _ := runCmd(t, "at-replicas", "--recording", rec, "--load-seen", load, "-o", "json")
	var r whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(got), &r), got)
	require.Len(t, r.Shapes, 1)
	return r.Shapes[0]
}

// marks are a shape's summary marks by load.
func marks(s whatifapi.ReportShape) map[string]string {
	out := map[string]string{}
	for _, row := range s.Table {
		out[row.Load] = row.Cell.Mark
	}
	return out
}

// A knee priced from a lower bound of V_P never shows the fleet saturated
// (#280): past it, short of decode's knee, the verdict is NOT JUDGED with
// the knee given as at least its rate, the cells at and past it are marked
// past a lower-bound knee, and the knees carry lowerBound.
func TestLowerBoundKnee_VerdictIsNotSaturated(t *testing.T) {
	rec := lowerBoundRecording(t)
	s := lowerBoundReport(t, rec, "8")
	v := s.Verdict
	assert.Equal(t, whatifapi.VerdictNotJudged, v.Verdict, v.Summary)
	// 4.15, not the 4.04 the floored V_P gives: the knee is raised to the
	// work the prefill pods sustained with a steady backlog (KneeAtServedWork).
	assert.Contains(t, v.Summary, "1.9x a prefill knee of at least 4.1 r/s")
	assert.Contains(t, v.Summary, "the prefill knee rests on a lower bound of V_P")
	assert.NotContains(t, v.Summary, "saturates;")
	require.Len(t, v.Checks, 1)
	assert.False(t, v.Checks[0].Known, v.Checks[0].Reason)
	assert.Contains(t, v.Checks[0].Reason, "a prefill knee of at least 4.15 r/s (V_P is a lower bound)")

	m := marks(s)
	assert.Equal(t, whatifapi.CellPastLowerBound, m["100%"], "the cell at the knee")
	assert.Equal(t, whatifapi.CellPastLowerBound, m["seen"], "the cell at the load")
	for load, mark := range m {
		assert.NotEqual(t, whatifapi.CellSaturated, mark, load)
	}

	c := s.Capacity
	assert.True(t, c.PrefillKnee.LowerBound, "the prefill knee is at least its rate")
	assert.True(t, c.FleetKnee.LowerBound, "the fleet knee prefill sets is at least its rate")
	assert.True(t, c.Operating.LowerBound, "so is the operating point on it")
	assert.False(t, c.DecodeKnee.LowerBound, "decode's knee does not rest on V_P")

	txt, _ := runCmd(t, "at-replicas", "--recording", rec, "--load-seen", "8")
	for _, line := range strings.Split(txt, "\n") {
		if strings.HasPrefix(line, "  measured  ") && strings.Contains(line, "@") {
			assert.NotContains(t, line, " SAT", "the summary row: %s", line)
			assert.Contains(t, line, "@4.15 LB", line)
		}
	}
	assert.Contains(t, txt, "prefill knee      >=4.15")
}

// Past decode's own knee too, the fleet saturates whatever bound V_P is:
// the verdict is UNDER-PROVISIONED on decode's knee, never the lower
// bound's.
func TestLowerBoundKnee_PastDecodeKneeSaturates(t *testing.T) {
	rec := lowerBoundRecording(t)
	s := lowerBoundReport(t, rec, "200")
	v := s.Verdict
	assert.Equal(t, whatifapi.VerdictUnder, v.Verdict, v.Summary)
	assert.Contains(t, v.Summary, "1.5x the decode knee of 134.2 r/s, the fleet saturates")
	assert.NotContains(t, v.Summary, "fleet knee of 4.0")
	assert.Equal(t, whatifapi.CellSaturated, marks(s)["seen"])
	assert.Equal(t, whatifapi.CellPastLowerBound, marks(s)["100%"], "the prefill knee's own rate is still short of decode's")
}
