package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
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
// bound of V_P. It is flagged so and claims no bound (#515): no "<=", no
// UB, no upper bound on the e2e sum or the concurrency, no "at least" on
// the rate. Over its target its miss is still not known, as its V_P terms
// can read it high: the verdict is NOT JUDGED, never UNDER-PROVISIONED, and
// the cells under the knee are marked LV.
func TestLowerBoundVPTTFT_UnderALowerBoundKneeIsNoKnownMiss(t *testing.T) {
	rec := lowerBoundRecording(t)
	s := lowerBoundReport(t, rec, "3")
	v := s.Verdict
	assert.Equal(t, whatifapi.VerdictNotJudged, v.Verdict, v.Summary)
	assert.Contains(t, v.Summary, "TTFT p95 is 14.5 s against a 200 ms target")
	assert.Contains(t, v.Summary, "it is priced from a lower bound of V_P, which can read it high, so the miss is not known; "+
		"--assume-vp states a V_P to price it")
	assert.NotContains(t, v.Summary, "at most")
	require.NotEmpty(t, v.Checks)
	assert.False(t, v.Checks[0].Known, v.Checks[0].Reason)
	assert.False(t, v.Checks[0].Holds, v.Checks[0].Reason)

	ttft := seenTTFT(t, s)
	noUpperBound(t, ttft, "the TTFT at the load")
	assert.True(t, slices.Contains(ttft.Flags, string(whatif.VPLowerBound)), "%v", ttft.Flags)
	m := marks(s)
	for _, load := range []string{"50%", "80%", seenLoad} {
		assert.Equal(t, whatifapi.CellVPLowerBound, m[load], load)
	}
	assert.Equal(t, whatifapi.CellPastLowerBound, m["100%"], "the knee itself is still past a lower-bound knee")
	for load, mark := range m {
		assert.NotEqual(t, whatifapi.CellMissed, mark, load)
	}
	for _, row := range s.Table {
		noUpperBound(t, row.E2EMeanSum, row.Load)
		if c := row.Concurrency; c != nil {
			noUpperBound(t, c.Fleet, row.Load)
		}
		if fc := row.FromConcurrency; fc != nil {
			assert.False(t, fc.LowerBound, row.Load)
		}
	}
	assert.False(t, s.Capacity.MaxAtSLO.LowerBound)
	assert.True(t, s.Capacity.MaxAtSLO.Refused != nil && strings.Contains(s.Capacity.MaxAtSLO.Refused.Detail,
		"the floor is priced from a lower bound of V_P, which can read it high"), "%+v", s.Capacity.MaxAtSLO)

	txt, _ := runCmd(t, "at-replicas", "--recording", rec, "--load-seen", "3")
	assert.Contains(t, txt, " 14545/18.4 @3.00 LV")
	assert.Contains(t, txt, "LV a TTFT over its target priced from a lower bound of V_P")
	assert.NotContains(t, txt, "UNDER-PROVISIONED")
	assert.NotContains(t, txt, "<=")
	assert.NotContains(t, txt, "upper bound")
	assert.Contains(t, txt, "TTFT p95 is 14545.4 ms, over 200.0 ms, but not judged: it is priced from a lower bound of V_P")
	assert.NotContains(t, txt, "is at most")
}

// The issue's own load (#369): 3.6 r/s, just past the operating point of
// the lower-bound knee, gave UNDER-PROVISIONED on "TTFT p95 is 33.9 s
// against a 200 ms target". Past the operating point the verdict is #280's:
// not judged, and the TTFT is no known miss.
func TestLowerBoundVPTTFT_TheIssuesLoadIsNotUnder(t *testing.T) {
	rec := lowerBoundRecording(t)
	s := lowerBoundReport(t, rec, "3.6")
	v := s.Verdict
	assert.Equal(t, whatifapi.VerdictNotJudged, v.Verdict, v.Summary)
	assert.NotContains(t, v.Summary, "33.9 s against")
	// 4.1, not the 4.0 V_P gives: the knee is floored at the work the
	// prefill pods served (prefill-vp-served-unqueued, servedKneeFloor).
	assert.Contains(t, v.Summary, "a prefill knee of at least 4.1 r/s")
	noUpperBound(t, seenTTFT(t, s), seenLoad)
	assert.Equal(t, whatifapi.CellVPLowerBound, marks(s)[seenLoad])
}

// A stated V_P prices the TTFT as given: not flagged, the miss known. Here
// the stated V_P at or above the learned lower bound prices the TTFT at or
// under the lower bound's figure, as the V_P terms are the only ones that
// move; that is this fleet, not a bound the planner claims.
func TestLowerBoundVPTTFT_AStatedVPIsJudgedAsGiven(t *testing.T) {
	rec := lowerBoundRecording(t)
	last := *seenTTFT(t, lowerBoundReport(t, rec, "3")).Value
	for _, vp := range []string{lowerBoundVP, "34093", "100000"} {
		got, _ := runCmd(t, "at-replicas", "--recording", rec, "--load-seen", "3", "--assume-vp", vp, "-o", "json")
		var r whatifapi.ReportResponse
		require.NoError(t, json.Unmarshal([]byte(got), &r), got)
		require.Len(t, r.Shapes, 1)
		s := r.Shapes[0]
		f := seenTTFT(t, s)
		assert.False(t, whatifapi.TTFTFromLowerBoundVP(wireFlags(f)), "V_P %s is stated", vp)
		assert.LessOrEqual(t, *f.Value, last, "V_P %s: the TTFT falls as V_P rises", vp)
		last = *f.Value
		assert.Equal(t, whatifapi.VerdictUnder, s.Verdict.Verdict, "V_P %s: the miss is known: %s", vp, s.Verdict.Summary)
		assert.Equal(t, whatifapi.CellMissed, marks(s)[seenLoad], vp)
	}
}

// noUpperBound says f, on the wire, carries no upperBound key: the field is
// deprecated and never set (#515).
func noUpperBound(t *testing.T, f whatifapi.Figure, what string) {
	t.Helper()
	b, err := json.Marshal(f)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "upperBound", what)
}

// wireFlags is a wire figure's flags as the model's.
func wireFlags(f whatifapi.Figure) []whatif.Flag {
	out := make([]whatif.Flag, 0, len(f.Flags))
	for _, x := range f.Flags {
		out = append(out, whatif.Flag(x))
	}
	return out
}
