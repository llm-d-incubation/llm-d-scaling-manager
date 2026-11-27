package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// lowerBoundVP is the V_P the synthetic lower-bound fleet
// (lowerBoundRecording) learns: the peak under a queue, a lower bound.
const lowerBoundVP = "17046.48"

// seenLoad is the table row and summary column of the load judged.
const seenLoad = "seen"

// seenTTFT is the shape's TTFT at the load judged, at the cell's statistic.
func seenTTFT(t *testing.T, s whatifapi.ReportShape) whatifapi.Figure {
	t.Helper()
	for _, row := range s.Table {
		if row.Load == seenLoad {
			require.NotNil(t, row.Cell.TTFT.Value, "the load's TTFT is priced")
			return row.Cell.TTFT.Figure
		}
	}
	t.Fatal("no row at the load judged")
	return whatifapi.Figure{}
}

// A TTFT priced below a lower-bound prefill knee is priced from a lower
// bound of V_P: it is at most its figure, so a figure over the target is no
// known miss (#369). At a stated load under the knee's operating point the
// verdict is NOT JUDGED, never UNDER-PROVISIONED, and says the TTFT is at
// most its figure; the cells under the knee are marked as upper bounds.
func TestUpperBoundTTFT_UnderALowerBoundKneeIsNoKnownMiss(t *testing.T) {
	rec := lowerBoundRecording(t)
	s := lowerBoundReport(t, rec, "3")
	v := s.Verdict
	assert.Equal(t, whatifapi.VerdictNotJudged, v.Verdict, v.Summary)
	assert.Contains(t, v.Summary, "TTFT p95 is at most 14.5 s against a 200 ms target")
	assert.Contains(t, v.Summary, "the true TTFT may be lower, so the miss is not known; --assume-vp states a V_P to price it")
	require.NotEmpty(t, v.Checks)
	assert.False(t, v.Checks[0].Known, v.Checks[0].Reason)
	assert.False(t, v.Checks[0].Holds, v.Checks[0].Reason)

	ttft := seenTTFT(t, s)
	assert.True(t, ttft.UpperBound, "the TTFT at the load is at most its figure")
	m := marks(s)
	for _, load := range []string{"50%", "80%", seenLoad} {
		assert.Equal(t, whatifapi.CellUpperBound, m[load], load)
	}
	assert.Equal(t, whatifapi.CellPastLowerBound, m["100%"], "the knee itself is still past a lower-bound knee")
	for load, mark := range m {
		assert.NotEqual(t, whatifapi.CellMissed, mark, load)
	}
	for _, row := range s.Table {
		if row.Load == seenLoad {
			assert.True(t, row.E2EMeanSum.UpperBound, "a sum of an upper bound is one")
		}
	}
	assert.True(t, s.Capacity.MaxAtSLO.Refused != nil && strings.Contains(s.Capacity.MaxAtSLO.Refused.Detail, "the floor is priced from a lower bound of V_P"),
		"%+v", s.Capacity.MaxAtSLO)

	txt, _ := runCmd(t, "at-replicas", "--recording", rec, "--load-seen", "3")
	assert.Contains(t, txt, "<=14545/18.4 @3.00 UB")
	assert.Contains(t, txt, "UB a TTFT over its target that is only an upper bound")
	assert.NotContains(t, txt, "UNDER-PROVISIONED")
}

// The issue's own load: 3.6 r/s, just past the operating point of the
// lower-bound knee, gave UNDER-PROVISIONED on "TTFT p95 is 33.9 s against a
// 200 ms target". Past the operating point the verdict is #280's: not
// judged, and the TTFT is no known miss.
func TestUpperBoundTTFT_TheIssuesLoadIsNotUnder(t *testing.T) {
	rec := lowerBoundRecording(t)
	s := lowerBoundReport(t, rec, "3.6")
	v := s.Verdict
	assert.Equal(t, whatifapi.VerdictNotJudged, v.Verdict, v.Summary)
	assert.NotContains(t, v.Summary, "33.9 s against")
	// 4.1, not the 4.0 V_P gives: the knee is floored at the work the
	// prefill pods served (prefill-vp-served-unqueued, servedKneeFloor).
	assert.Contains(t, v.Summary, "a prefill knee of at least 4.1 r/s")
	assert.True(t, seenTTFT(t, s).UpperBound)
	assert.Equal(t, whatifapi.CellUpperBound, marks(s)[seenLoad])
}

// The upper bound holds in the model: a stated V_P at or above the learned
// lower bound prices the TTFT at the load at or under the lower bound's
// figure, lower the higher it is, and a stated V_P is judged as it always
// was: no bound, and the miss known.
func TestUpperBoundTTFT_AStatedVPPricesAtOrUnderIt(t *testing.T) {
	rec := lowerBoundRecording(t)
	bound := seenTTFT(t, lowerBoundReport(t, rec, "3"))
	last := *bound.Value
	for _, vp := range []string{lowerBoundVP, "34093", "100000"} {
		got, _ := runCmd(t, "at-replicas", "--recording", rec, "--load-seen", "3", "--assume-vp", vp, "-o", "json")
		var r whatifapi.ReportResponse
		require.NoError(t, json.Unmarshal([]byte(got), &r), got)
		require.Len(t, r.Shapes, 1)
		s := r.Shapes[0]
		f := seenTTFT(t, s)
		assert.False(t, f.UpperBound, "V_P %s is stated: no bound", vp)
		assert.LessOrEqual(t, *f.Value, last, "V_P %s: the TTFT falls as V_P rises", vp)
		last = *f.Value
		assert.Equal(t, whatifapi.VerdictUnder, s.Verdict.Verdict, "V_P %s: the miss is known: %s", vp, s.Verdict.Summary)
		assert.Equal(t, whatifapi.CellMissed, marks(s)[seenLoad], vp)
	}
}
