package main

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// A variant over rows the line predicts, with its line times factor in place
// of the one the rows were made from.
func checkedState(factor float64) *learnreplay.State {
	line := itl.Model{A: 0.01, B: 0.005}
	fills := []float64{0.4, 0.45, 0.5, 0.55, 0.6}
	rows := make([]whatif.LineRow, len(fills))
	for c, k := range fills {
		rows[c] = whatif.LineRow{Cycle: c + 1, K: k, KVPerRequest: 1500,
			TokenRate: whatif.ReplicaTokenRate(line, k, 100000, 1500, 1000)}
	}
	return &learnreplay.State{Cycle: 3, Variants: []learnreplay.VariantState{{
		Variant: "d", Role: "decode", Replicas: 1, Ranks: 1, BatchCap: 1000, CapacityTokens: 100000, Tier: whatif.TierOLS,
		Line: itl.Model{A: line.A * factor, B: line.B * factor}, LineRows: rows,
		Shape: learnreplay.MeasuredShape{InputTokens: 1000, OutputTokens: 1000, FromCycle: 1, ToCycle: 3, Cycles: 3},
	}}}
}

// A line that disagrees with what the recording's rows read prints the
// mismatch, flags every answer read from it and stays an answer. This is the
// page's "a bad ITL line prints a confident TPOT".
func TestLineCheck_ABadLineIsPrintedAndFlagged(t *testing.T) {
	pv := func(t *testing.T, st *learnreplay.State) pricedVariant {
		t.Helper()
		f := answerFlags{set: map[string]bool{}}
		vs, err := f.decodeVariants(st)
		require.NoError(t, err)
		require.Len(t, vs, 1)
		return vs[0]
	}

	good := pv(t, checkedState(1))
	assert.False(t, good.check.Mismatch())
	assert.Equal(t, 5, good.check.Compared)
	assert.NotContains(t, good.flags, lineMismatch)
	assert.Contains(t, good.checkText(), "ok: median 0.0%")

	// A line twice as fast as the replicas: it over-promises.
	bad := pv(t, checkedState(0.5))
	assert.True(t, bad.check.Mismatch())
	assert.Contains(t, bad.flags, lineMismatch)
	text := bad.checkText()
	assert.Contains(t, text, "MISMATCH: median 100.0% off the rate read, line high")
	assert.Contains(t, text, "5 of 5 rows past 15%")
	a, ok := bad.qualify(whatif.Answer{Value: 1}).(whatif.Answer)
	require.True(t, ok)
	assert.Contains(t, a.Flags, lineMismatch, "every answer read from the line carries it")
}

// A check that compared nothing says so: an empty result is not a pass.
func TestLineCheck_NothingComparedIsNotAPass(t *testing.T) {
	st := checkedState(1)
	st.Variants[0].LineRows = nil
	f := answerFlags{set: map[string]bool{}}
	vs, err := f.decodeVariants(st)
	require.NoError(t, err)
	assert.Contains(t, vs[0].checkText(), "not run: 0 rows to compare at fill 0.30 to 0.80, want 5")
	assert.Equal(t, checkNotRun, wireCheck(vs[0]).Status)
	assert.NotContains(t, vs[0].flags, lineMismatch)

	// Rows above the band are named: "no row" read wrongly of a fleet there.
	st.Variants[0].LineRows = []whatif.LineRow{{Cycle: 1, K: 0.9, KVPerRequest: 1500, TokenRate: 100, AboveBand: true}}
	vs, err = f.decodeVariants(st)
	require.NoError(t, err)
	assert.Contains(t, vs[0].checkText(), "(0 on a ramp, 1 above the band, 0 older than 30m0s)")

	st.Variants[0].Line = itl.Model{}
	vs, err = f.decodeVariants(st)
	require.NoError(t, err)
	assert.Empty(t, vs[0].checkText(), "no line, nothing to check")
	assert.Nil(t, wireCheck(vs[0]))
}

// The check reads the recording's rows at their own shape, not the shape asked:
// --assume-input-tokens prices another traffic, and the line still has to
// describe the traffic it was read from. So the figures do not move, and the
// verdict says when they are of another shape than the one priced.
func TestLineCheck_ReadsTheRowsNotTheShapeAsked(t *testing.T) {
	recorded := []string{"replicas-to-slo", "--recording", olsSweep, "--model", "model-02", "-o", "json"}
	check := func(args ...string) wireLineCheck {
		t.Helper()
		out, _ := runCmd(t, slices.Concat(recorded, args)...)
		w := decodeOutput(t, out)
		require.NotEmpty(t, w.Roles)
		c := w.Roles[0].Inputs.Line.Check
		require.NotNil(t, c, out)
		return *c
	}
	asked := check(workedShape...)
	plain := check()
	require.Positive(t, plain.Compared, "ols-sweep has rows to compare: an empty check here reads as a pass")
	assert.Equal(t, plain.MedianErrorPct, asked.MedianErrorPct)
	assert.Equal(t, plain.Compared, asked.Compared)
	assert.False(t, plain.OtherShape, "%+v", plain)
	assert.True(t, asked.OtherShape, "%+v", asked)
	assert.Equal(t, checkOtherShape, asked.Status)
}

// An ok on rows of another shape never reads as a plain ok.
func TestLineCheck_AnotherShapeIsSaid(t *testing.T) {
	st := checkedState(1)
	for i := range st.Variants[0].LineRows {
		st.Variants[0].LineRows[i].KVPerRequest = 3000
		k := st.Variants[0].LineRows[i].K
		st.Variants[0].LineRows[i].TokenRate = whatif.ReplicaTokenRate(st.Variants[0].Line, k, 100000, 3000, 1000)
	}
	f := answerFlags{set: map[string]bool{}}
	vs, err := f.decodeVariants(st)
	require.NoError(t, err)
	assert.False(t, vs[0].check.Mismatch())
	assert.Contains(t, vs[0].checkText(), "ok at another shape (rows at KV 3000 a request, priced at 1500): median 0.0%")
	assert.NotContains(t, vs[0].flags, lineMismatch, "an ok at another shape raises no flag")
	assert.Equal(t, checkOtherShape, wireCheck(vs[0]).Status)
}

// A mismatch at another shape says both, and keeps the flag: a line that
// fails the tokens read is suspect at any shape. An ok there raises none.
func TestLineCheck_AMismatchAtAnotherShape(t *testing.T) {
	st := checkedState(1)
	for i := range st.Variants[0].LineRows {
		r := &st.Variants[0].LineRows[i]
		r.KVPerRequest = 3000
		r.TokenRate = whatif.ReplicaTokenRate(st.Variants[0].Line, r.K, 100000, 3000, 1000) / 2
	}
	f := answerFlags{set: map[string]bool{}}
	vs, err := f.decodeVariants(st)
	require.NoError(t, err)
	assert.True(t, vs[0].check.Mismatch())
	assert.True(t, vs[0].check.OtherShape)
	assert.Contains(t, vs[0].flags, lineMismatch)
	assert.Contains(t, vs[0].checkText(), "MISMATCH at another shape (rows at KV 3000 a request, priced at 1500): median 100.0%")
	w := wireCheck(vs[0])
	assert.Equal(t, checkMismatch, w.Status, "the mismatch wins the status")
	assert.True(t, w.OtherShape)
	assert.InDelta(t, 3000, w.RowKVPerRequest, 1e-9)
}

// The age limit on a real replay: read at the last cycle, every row of
// throughput-sweep-restart is older than the tracker's window, and read near them
// none is. Both clocks come from the replay.
func TestLineCheck_TheAgeLimitOnARealReplay(t *testing.T) {
	check := func(args ...string) wireLineCheck {
		t.Helper()
		q := slices.Concat([]string{"replicas-to-slo", "--recording", goldenDir + "throughput-sweep-restart/recording.jsonl.gz", "-o", "json"}, args)
		out, _ := runCmd(t, q...)
		w := decodeOutput(t, out)
		require.NotEmpty(t, w.Roles)
		c := w.Roles[0].Inputs.Line.Check
		require.NotNil(t, c, out)
		return *c
	}
	last := check()
	assert.Equal(t, 53, last.TooOld)
	assert.Equal(t, checkNotRun, last.Status)
	assert.Equal(t, 0, last.Compared)

	near := check("--cycle", "60")
	assert.Equal(t, 0, near.TooOld, "%+v", near)
	assert.Positive(t, near.Compared, "%+v", near)
}

// A row older than the tracker's window is no row of the check.
func TestLineCheck_AnOldRowIsNotCompared(t *testing.T) {
	st := checkedState(1)
	st.At = st.Variants[0].LineRows[0].At.Add(whatif.MaxLineRowAge + 1)
	f := answerFlags{set: map[string]bool{}}
	vs, err := f.decodeVariants(st)
	require.NoError(t, err)
	assert.Equal(t, 5, vs[0].check.TooOld)
	assert.Contains(t, vs[0].checkText(), "not run: 0 rows")
	assert.Contains(t, vs[0].checkText(), "5 older than 30m0s")
}

const goldenDir = "../../internal/engines/analyzers/learnreplay/testdata/goldens/"

// The check on committed recordings, at their last cycle, frozen: the figures
// the page quotes, not a pass mark. ols-sweep, the worked example, holds its
// line. The staircase's B off reads the opposite: its line predicts half the
// tokens read. It is the line the planner refuses, PinnedDefaultB, so no
// answer rests on it, and the check prints all the same.
func TestLineCheck_OnTheGoldens(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		compared int
		median   float64
	}{
		{"ols-sweep", checkOK, 10, 3.0},
		{"ttft-three-pods", checkOK, 57, 2.0},
		{"ttft-staircase-b-off", checkMismatch, 36, 49.8},
		{"ttft-staircase-c-on", checkMismatch, 26, 61.7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _ := runCmd(t, "replicas-to-slo", "--recording", goldenDir+c.name+"/recording.jsonl.gz", "-o", "json")
			w := decodeOutput(t, out)
			require.NotEmpty(t, w.Roles)
			got := w.Roles[0].Inputs.Line.Check
			require.NotNil(t, got, out)
			assert.Equal(t, c.status, got.Status)
			assert.Equal(t, c.compared, got.Compared)
			assert.InDelta(t, c.median, got.MedianErrorPct, 0.05)
		})
	}
}

// Through the command: the synthetic mu-paths run prices a line nine times the
// tokens its rows read, and every answer on it says so.
func TestLineCheck_AMismatchReachesEveryAnswer(t *testing.T) {
	q := []string{"replicas-to-slo", "--recording", goldenDir + "mu-paths/recording.jsonl.gz", "--variant", "variant-03"}
	out, _ := runCmd(t, slices.Concat(q, []string{"-o", "json"})...)
	w := decodeOutput(t, out)
	require.Len(t, w.Roles, 1)
	r := w.Roles[0]
	assert.Equal(t, checkMismatch, r.Inputs.Line.Check.Status)
	for name, res := range map[string]*wireResult{"knee": r.Results.Knee, "rate": r.Results.OperatingRate, "tpot": r.Results.MeanTPOT} {
		require.NotNil(t, res.Answer, name)
		assert.Contains(t, res.Answer.Flags, string(lineMismatch), name)
	}
	text, code := runCmd(t, q...)
	assert.Equal(t, 0, code, "a flag, not a refusal")
	assert.Contains(t, text, "line check      MISMATCH: median 974.3% off the rate read, line high")
}
