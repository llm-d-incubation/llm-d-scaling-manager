package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// stairBOn is the P/D staircase with flow control on: two prefill and two
// decode replicas, and a decode line the planner answers on. P1 learns
// neither V_P nor a on it, so the report's P/D cases assume both.
const stairBOn = "../../internal/engines/analyzers/learnreplay/testdata/goldens/ttft-staircase-b-on/recording.jsonl.gz"

// stairAssumed is the P/D fleet's assumed prefill and the targets it is
// judged by: V_P and a, so that the prefill side prices, and a TTFT and a
// TPOT target the staircase's fleet can meet.
var stairAssumed = []string{"--assume-vp", "200000", "--assume-a", "20ms", "--ttft", "2s@p90", "--tpot", "30ms"}

// atStair is at-replicas on the staircase with the assumed prefill and
// targets, and args.
func atStair(args ...string) []string {
	return slices.Concat([]string{"at-replicas", "--recording", stairBOn}, stairAssumed, args)
}

// reportDeadline bounds one report in a test, so that a slow or looping
// change fails the test that met it rather than go test's whole run.
const reportDeadline = 2 * time.Minute

// The report's golden outputs: a P/D fleet at the window's p95 load
// (under-provisioned and saturated, and exit 0: the report answered; exit 3
// is NOT JUDGED alone), the same as JSON, one over-provisioned by a decode
// replica at a stated load, and a decode-only answer where the prefill side
// does not price. The JSON golden is named
// .json, not -json: TestServeParity covers the plan's JSON goldens, and the
// report's parity with the API is TestAtReplicas_APIIsTheCommand.
func TestAtReplicasGolden(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
	}{
		{"at-replicas-staircase", atStair(), 0},
		{"at-replicas-staircase.json", atStair("-o", "json"), 0},
		{"at-replicas-over", atStair("--decode", "3", "--load-seen", "20"), 0},
		// V_P is refused on the sweep: the report answers for decode alone,
		// which is under-provisioned at the p95: an answer, exit 0.
		{"at-replicas-decode-only", []string{"at-replicas", "--recording", outputSpreadRun, "--tpot", "5ms"}, 0},
		// Three shapes by four loads: the measured one, a short and a long
		// prompt the recording never saw (EXTRAPOLATED), at two shares of each
		// shape's knee, a stated rate and the load seen grown by a fifth. Past
		// the knee the TTFT saturates: an answer, exit 0.
		{"at-replicas-shapes", atStair("--shape", "measured", "--shape", "1k/1k", "--shape", "16k/200/0.3",
			"--load", "50%", "--load", "80%", "--load", "5r/s", "--load", "seen", "--load-seen-x", "1.2"), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { checkGolden(t, c.name, c.code, c.args) })
	}
}

// replayed is a recording replayed to its last cycle once per test binary:
// every test that prices on the state shares the one replay.
type replayed struct {
	once sync.Once
	path string
	st   *learnreplay.State
	err  error
}

func (r *replayed) state(t *testing.T) *learnreplay.State {
	t.Helper()
	r.once.Do(func() {
		f := answerFlags{recording: r.path, cycle: "last", set: map[string]bool{}, warn: io.Discard}
		r.st, r.err = f.readState(context.Background())
	})
	require.NoError(t, r.err)
	return r.st
}

var (
	stairReplay = &replayed{path: stairBOn}
	sweepReplay = &replayed{path: outputSpreadRun}
)

// stairQuery is the report's question on the staircase: the assumed prefill,
// the targets, and np prefill and nd decode replicas at load (0: the
// window's p95).
func stairQuery(np, nd int, load float64) whatifapi.ReportQuery {
	vp, a := 200000.0, 0.020
	return whatifapi.ReportQuery{Prefill: np, Decode: nd, LoadSeen: load, TTFTPercentiles: []float64{0.9}, TPOTPercentile: 0.9,
		TTFT: &whatifapi.ReportTarget{Seconds: 2, Percentile: 0.9}, TPOT: &whatifapi.ReportTarget{Seconds: 0.030},
		Assume: whatifapi.Assume{VP: &vp, A: &a}}
}

// priceReport prices q on st within reportDeadline.
func priceReport(t *testing.T, st *learnreplay.State, q whatifapi.ReportQuery) *whatifapi.ReportResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), reportDeadline)
	defer cancel()
	rep, err := buildReport(ctx, st, &servedState{st: st, decodeVariant: "variant-01"}, q)
	require.NoError(t, err)
	return rep
}

// stairReport prices q on the staircase.
func stairReport(t *testing.T, q whatifapi.ReportQuery) *whatifapi.ReportResponse {
	t.Helper()
	return priceReport(t, stairReplay.state(t), q)
}

// verdictRank orders the verdicts: more replicas never move a fleet down it.
func verdictRank(t *testing.T, v string) int {
	t.Helper()
	switch v {
	case whatifapi.VerdictUnder:
		return 0
	case whatifapi.VerdictRight:
		return 1
	case whatifapi.VerdictOver:
		return 2
	}
	t.Fatalf("verdict %q, want one of the three", v)
	return -1
}

// The verdict is monotone in N: adding a replica of either role never moves a
// fleet from over- to right-sized, nor from right- to under-provisioned. The
// sweep must see all three verdicts, so that it is not met by one verdict
// everywhere. Every fleet is priced from one replay.
func TestAtReplicas_VerdictMonotoneInN(t *testing.T) {
	const maxP, maxD = 3, 5
	rank := map[[2]int]int{}
	seen := map[string]bool{}
	for np := 1; np <= maxP; np++ {
		for nd := 1; nd <= maxD; nd++ {
			rep := stairReport(t, stairQuery(np, nd, 20))
			rank[[2]int{np, nd}] = verdictRank(t, rep.Shapes[0].Verdict.Verdict)
			seen[rep.Shapes[0].Verdict.Verdict] = true
		}
	}
	assert.Len(t, seen, 3, "the sweep saw %v", seen)
	for np := 1; np <= maxP; np++ {
		for nd := 1; nd <= maxD; nd++ {
			r := rank[[2]int{np, nd}]
			if nd < maxD {
				assert.LessOrEqual(t, r, rank[[2]int{np, nd + 1}], "%d prefill: %d decode then %d", np, nd, nd+1)
			}
			if np < maxP {
				assert.LessOrEqual(t, r, rank[[2]int{np + 1, nd}], "%d decode: %d prefill then %d", nd, np, np+1)
			}
		}
	}
}

// The verdict names the role that can shrink, and is judged at the load, not
// at the operating point: at 20 r/s, two prefill and three decode replicas
// hold with one decode replica fewer, not with one prefill replica fewer.
// An over-provisioned decode carries the decode knee's caveat in its one
// sentence; an under-provisioned fleet names the replica that makes it hold.
func TestAtReplicas_OverProvisionedNamesTheRole(t *testing.T) {
	rep := stairReport(t, stairQuery(2, 3, 20))
	v := rep.Shapes[0].Verdict
	assert.Equal(t, whatifapi.VerdictOver, v.Verdict, v.Reason)
	assert.Equal(t, []string{roleDecode}, v.Shrink)
	assert.Equal(t, whatifapi.ScopeFleet, v.Scope)
	// The staircase-b decode knee is outside its validated region: the
	// caveat says so, and the needed fleet gives decode at the 0.6B goldens'
	// 103% (#297): the replica cut is not safe at that bound.
	assert.Contains(t, v.Summary, "decode knee outside its validated region")
	assert.Contains(t, v.Summary, "needs 2 prefill + 2 decode (has 2 + 3), 3 decode if the decode knee reads 103% high")
	require.Len(t, v.Checks, 3)
	for _, c := range v.Checks {
		assert.True(t, strings.HasPrefix(c.Reason, "20.00 r/s "), "each fleet is checked at the load: %s", c.Reason)
	}
	byFleet := func(np, nd int) whatifapi.ReportCheck {
		i := slices.IndexFunc(v.Checks, func(c whatifapi.ReportCheck) bool { return c.Prefill == np && c.Decode == nd })
		require.GreaterOrEqual(t, i, 0)
		return v.Checks[i]
	}
	assert.True(t, byFleet(2, 3).Holds)
	assert.True(t, byFleet(2, 2).Holds, byFleet(2, 2).Reason)
	assert.False(t, byFleet(1, 3).Holds, byFleet(1, 3).Reason)

	// Two decode replicas are right-sized at that load: one fewer of either
	// role misses.
	rep = stairReport(t, stairQuery(2, 2, 20))
	assert.Equal(t, whatifapi.VerdictRight, rep.Shapes[0].Verdict.Verdict, rep.Shapes[0].Verdict.Reason)
	assert.Empty(t, rep.Shapes[0].Verdict.Shrink)
	// One decode replica is under-provisioned: 20 r/s is past 0.85 of its
	// knee, and a second decode replica is what makes it hold.
	rep = stairReport(t, stairQuery(2, 1, 20))
	v = rep.Shapes[0].Verdict
	assert.Equal(t, whatifapi.VerdictUnder, v.Verdict, v.Reason)
	assert.Equal(t, []string{roleDecode}, v.Grow, v.Summary)
	require.NotNil(t, v.Needed)
	assert.Equal(t, whatifapi.ReportNeeded{Prefill: 2, Decode: 2, MaxReplicas: whatifapi.DefaultSizeBound,
		DecodeAtErrorBound: 3, DecodeKneeError: decodeKneeGoldensHigh}, *v.Needed)
	assert.Contains(t, v.Summary, "needs 2 prefill + 2 decode (has 2 + 1)")
}

// The verdict's sentence is one plain sentence: it starts with the verdict,
// ends with one full stop, and carries no reason code.
func TestAtReplicas_VerdictIsOnePlainSentence(t *testing.T) {
	sweep := sweepReplay.state(t)
	cases := map[string]whatifapi.ReportVerdict{}
	for _, q := range []whatifapi.ReportQuery{stairQuery(2, 1, 20), stairQuery(2, 2, 20), stairQuery(2, 3, 20), stairQuery(3, 4, 0), stairQuery(2, 2, 0)} {
		v := stairReport(t, q).Shapes[0].Verdict
		cases[fmt.Sprintf("staircase %d+%d at %g", q.Prefill, q.Decode, q.LoadSeen)] = v
	}
	require.Len(t, cases, 5)
	nj := sweepQuery()
	nj.TTFT = &whatifapi.ReportTarget{Seconds: 2, Percentile: 0.9}
	cases["sweep TTFT"] = priceReport(t, sweep, nj).Shapes[0].Verdict
	cases["sweep decode only"] = priceReport(t, sweep, sweepQuery()).Shapes[0].Verdict
	for name, v := range cases {
		assert.True(t, strings.HasPrefix(v.Summary, v.Verdict+": "), "%s: %s", name, v.Summary)
		assert.True(t, strings.HasSuffix(v.Summary, "."), "%s: %s", name, v.Summary)
		assert.Equal(t, 1, strings.Count(v.Summary, ". ")+1, "%s: %s", name, v.Summary)
		for _, code := range []string{"NoPrefillCapacity", "Saturated", "AboveDecodeOnset", "TPOTBucketTooWide", "NoTraffic"} {
			assert.NotContains(t, v.Summary, code, name)
		}
	}
}

// sweepQuery is the decode-only question on the throughput sweep: TPOT mean
// at 5 ms, judged at the window's mean, where decode alone holds.
func sweepQuery() whatifapi.ReportQuery {
	return whatifapi.ReportQuery{TTFTPercentiles: []float64{0.5, 0.9}, TPOTPercentile: 0.9, LoadStatistic: whatifapi.LoadMean,
		TPOT: &whatifapi.ReportTarget{Seconds: 0.005}}
}

// A fleet whose own check is not judged is NOT JUDGED, never RIGHT-SIZED: on
// the sweep, the TTFT target cannot be priced (no prefill speed), and at the
// window's mean decode holds. With no TTFT target, decode alone is judged and
// the scope says so: NOT JUDGED, never RIGHT-SIZED or OVER-PROVISIONED, and no
// fleet rate at the SLO, as slo-to-rate gives none.
func TestAtReplicas_NotJudged(t *testing.T) {
	sweep := sweepReplay.state(t)
	q := sweepQuery()
	q.TTFT = &whatifapi.ReportTarget{Seconds: 2, Percentile: 0.9}
	v := priceReport(t, sweep, q).Shapes[0].Verdict
	assert.Equal(t, whatifapi.VerdictNotJudged, v.Verdict, v.Reason)
	require.NotEmpty(t, v.Checks)
	assert.False(t, v.Checks[0].Known, v.Checks[0].Reason)
	assert.Contains(t, v.Summary, "TTFT p90 cannot be priced")

	for _, nd := range []int{0, 3} {
		q := sweepQuery()
		q.Decode = nd
		rep := priceReport(t, sweep, q)
		s := rep.Shapes[0]
		assert.Equal(t, whatifapi.VerdictNotJudged, s.Verdict.Verdict, "decode %d: %s", nd, s.Verdict.Reason)
		assert.Equal(t, whatifapi.ScopeDecodeOnly, s.Verdict.Scope)
		assert.Empty(t, s.Verdict.Shrink)
		assert.Nil(t, s.Capacity.MaxAtSLO.Value, "decode %d: no fleet rate with prefill not priced", nd)
		assert.Contains(t, s.Flags, string(decodeOnly))
	}
}

// The load is the window's per-cycle arrival rate at a statistic: by
// definition its p95 is a rate the window read, at or above 95% of the
// cycles with traffic and above fewer than 95% of them; the mean is below
// it and the peak at or above it. The verdict is judged at the statistic
// asked, p95 by default.
func TestAtReplicas_LoadStatistics(t *testing.T) {
	st := stairReplay.state(t)
	var rates []float64
	for _, x := range st.ArrivalRates {
		if x > 0 {
			rates = append(rates, x)
		}
	}
	require.Len(t, st.ArrivalRates, st.Cycles, "one rate per cycle")
	require.NotEmpty(t, st.ServingReplicas)
	for v, row := range st.ServingReplicas {
		require.Len(t, row, st.Cycles, "%s: one serving count per cycle", v)
	}
	rep := stairReport(t, stairQuery(0, 0, 0))
	l := rep.Load
	require.NotNil(t, l.P95)
	assert.Equal(t, whatifapi.LoadP95, l.Statistic)
	assert.Equal(t, "observed", l.Source)
	assert.Equal(t, len(rates), l.CyclesWithTraffic)
	assert.Equal(t, *l.P95, *l.Rate.Value)
	assert.Contains(t, rates, *l.P95)
	atOrBelow, below := 0, 0
	for _, x := range rates {
		if x <= *l.P95 {
			atOrBelow++
		}
		if x < *l.P95 {
			below++
		}
	}
	assert.GreaterOrEqual(t, float64(atOrBelow), 0.95*float64(len(rates)))
	assert.Less(t, float64(below), 0.95*float64(len(rates)))
	assert.Less(t, *l.Mean, *l.P95)
	assert.LessOrEqual(t, *l.P95, *l.Peak)
	// The staircase is read every 15 s: two minutes are eight cycles, and
	// their busiest mean lies between the window's mean and its peak.
	require.NotNil(t, l.Rolling)
	assert.Equal(t, 8, l.Rolling.Cycles)
	assert.InDelta(t, 120, l.Rolling.WindowSeconds, 5)
	assert.Greater(t, l.Rolling.Peak, *l.Mean)
	assert.LessOrEqual(t, l.Rolling.Peak, *l.Peak)

	for _, stat := range []string{whatifapi.LoadMean, whatifapi.LoadPeak} {
		q := stairQuery(0, 0, 0)
		q.LoadStatistic = stat
		got := stairReport(t, q).Load
		assert.Equal(t, stat, got.Statistic)
		want := map[string]*float64{whatifapi.LoadMean: l.Mean, whatifapi.LoadPeak: l.Peak}[stat]
		assert.Equal(t, *want, *got.Rate.Value, stat)
	}
	q := stairQuery(0, 0, 0)
	q.LoadStatistic, q.LoadSeenX = whatifapi.LoadPeak, 1.5
	grown := stairReport(t, q)
	assert.Equal(t, *l.Peak, *grown.Load.Rate.Value, "growth never moves the load judged at")
	require.Len(t, grown.Shapes[0].Growth, 1)
	assert.InDelta(t, 1.5**l.Peak, grown.Shapes[0].Growth[0].Rate, 1e-9)
}

// Growth is a what-if beside the verdict, never in its place: at 20 r/s two
// prefill and three decode replicas are over-provisioned, and asking what
// twice the traffic does (--load-seen-x 2) leaves that verdict, the load
// judged and every other row as they were, and adds its own column and its
// own verdict, which differs. "seen x2" in the loads asks the same.
func TestAtReplicas_GrowthIsAWhatIf(t *testing.T) {
	base := stairReport(t, stairQuery(2, 3, 20))
	q := stairQuery(2, 3, 20)
	q.LoadSeenX = 2
	grown := stairReport(t, q)

	assert.Equal(t, base.Load, grown.Load)
	assert.Equal(t, base.Shapes[0].Verdict, grown.Shapes[0].Verdict, "the verdict at the load seen")
	assert.Equal(t, whatifapi.VerdictOver, grown.Shapes[0].Verdict.Verdict)
	assert.Empty(t, base.Shapes[0].Growth)
	assert.Equal(t, append(slices.Clone(base.Loads), "seen x2"), grown.Loads)
	require.Len(t, grown.Shapes[0].Table, len(base.Shapes[0].Table)+1)
	assert.Equal(t, base.Shapes[0].Table, grown.Shapes[0].Table[:len(base.Shapes[0].Table)], "the other columns")
	last := grown.Shapes[0].Table[len(grown.Shapes[0].Table)-1]
	assert.Equal(t, "seen x2", last.Load)
	assert.InDelta(t, 40, last.Rate, 1e-9)

	require.Len(t, grown.Shapes[0].Growth, 1)
	g := grown.Shapes[0].Growth[0]
	assert.Equal(t, "seen x2", g.Load)
	assert.InDelta(t, 2, g.X, 0)
	assert.InDelta(t, 40, g.Rate, 1e-9)
	assert.NotEqual(t, whatifapi.VerdictOver, g.Verdict.Verdict, g.Verdict.Summary)
	assert.NotContains(t, g.Verdict.Summary, "load", "a growth line is short: the column names its load")
	require.NotNil(t, g.Verdict.Needed, g.Verdict.Summary)
	assert.Contains(t, g.Verdict.Summary, "needs ")

	sh, err := whatifapi.ParseReportLoad("seen\u00d72")
	require.NoError(t, err)
	q = stairQuery(2, 3, 20)
	q.Loads = []whatifapi.ReportLoadAsk{sh}
	byLoad := stairReport(t, q)
	require.Len(t, byLoad.Shapes[0].Growth, 1)
	assert.Equal(t, g, byLoad.Shapes[0].Growth[0])
}

// A cell prints each latency at the statistic its target is judged at, not
// p90 whatever the SLO says (#299): with TTFT judged at p95 and TPOT at p99
// while the tables print p50/p90 and p90, every cell is at p95 and p99, to
// the bit what a table asked for those statistics prints, and the legend
// names them; a TPOT target on the mean puts the mean in the cell.
func TestAtReplicas_CellAtTheSLOStatistics(t *testing.T) {
	q := stairQuery(2, 3, 20)
	q.TTFTPercentiles = []float64{0.5, 0.9}
	q.TTFT = &whatifapi.ReportTarget{Seconds: 2, Percentile: 0.95}
	q.TPOT = &whatifapi.ReportTarget{Seconds: 0.030, Percentile: 0.99}
	rep := stairReport(t, q)
	at := stairQuery(2, 3, 20)
	at.TTFTPercentiles, at.TPOTPercentile = []float64{0.95}, 0.99
	at.TTFT, at.TPOT = q.TTFT, q.TPOT
	want := stairReport(t, at)
	rows, wantRows := rep.Shapes[0].Table, want.Shapes[0].Table
	require.Len(t, wantRows, len(rows))
	valued := 0
	for i, row := range rows {
		c := row.Cell
		assert.Equal(t, "p95", c.TTFT.Statistic, row.Label)
		assert.Equal(t, wantRows[i].TTFT[0].Figure, c.TTFT.Figure, row.Label)
		if c.TPOT.Statistic == whatifapi.ReportStatisticName(0) {
			assert.Nil(t, wantRows[i].TPOTPercentile.Value, "%s: the mean stands in only where the p99 is refused", row.Label)
			continue
		}
		assert.Equal(t, "p99", c.TPOT.Statistic, row.Label)
		assert.Equal(t, wantRows[i].TPOTPercentile.Figure, c.TPOT.Figure, row.Label)
		if c.TTFT.Value != nil && c.TPOT.Value != nil {
			valued++
		}
	}
	assert.Positive(t, valued, "a cell with both figures")
	assert.Contains(t, summaryText(rep), "cell = TTFT p95 ms / TPOT p99 ms")

	q.TPOT = &whatifapi.ReportTarget{Seconds: 0.030}
	for _, row := range stairReport(t, q).Shapes[0].Table {
		assert.Equal(t, whatifapi.ReportStatisticName(0), row.Cell.TPOT.Statistic, row.Label)
		assert.Equal(t, row.TPOTMean, row.Cell.TPOT.Figure, row.Label)
	}
}

// at-replicas prices on the paths the other questions take: on the same
// recording and flags, its knees are replicas-to-slo's, its TTFT at a stated
// load is replicas-to-slo's at that rate, and its highest rate at the SLO is
// slo-to-rate's fleet rate, each to the bit.
func TestAtReplicas_AgreesWithTheOtherQuestions(t *testing.T) {
	const load = 20.0
	base := []string{"--recording", stairBOn, "--assume-vp", "200000", "--assume-a", "20ms"}
	r2s, code := runCmd(t, slices.Concat([]string{"replicas-to-slo"}, base, []string{"--ttft-percentile", "90", "--rate", "20", "-o", "json"})...)
	require.Equal(t, 0, code, r2s)
	s2r, code := runCmd(t, slices.Concat([]string{"slo-to-rate"}, base, []string{"--ttft", "2s@p90", "--tpot", "30ms", "-o", "json"})...)
	require.Equal(t, 0, code, s2r)
	role := func(out wireOutput, name string) wireResults {
		i := slices.IndexFunc(out.Roles, func(r wireRole) bool { return r.Role == name })
		require.GreaterOrEqual(t, i, 0, "no %s role", name)
		return out.Roles[i].Results
	}
	answer := func(r *wireResult) float64 {
		require.NotNil(t, r)
		require.NotNil(t, r.Answer, "%+v", r.Refused)
		return r.Answer.Value
	}
	r2sOut, s2rOut := decodeOutput(t, r2s), decodeOutput(t, s2r)

	rep := stairReport(t, stairQuery(0, 0, load))
	s := rep.Shapes[0]
	require.NotNil(t, s.Capacity.DecodeKnee.Value)
	require.NotNil(t, s.Capacity.PrefillKnee.Value)
	require.NotNil(t, s.Capacity.MaxAtSLO.Value)
	assert.Equal(t, answer(role(r2sOut, roleDecode).Knee), *s.Capacity.DecodeKnee.Value, "decode knee")
	assert.Equal(t, answer(role(r2sOut, rolePrefill).Knee), *s.Capacity.PrefillKnee.Value, "prefill knee")
	assert.Equal(t, answer(role(s2rOut, "fleet").Rate), *s.Capacity.MaxAtSLO.Value, "max at SLO")

	ttft := role(r2sOut, rolePrefill).TTFT
	require.NotNil(t, ttft)
	require.NotNil(t, ttft.Answer, "%+v", ttft.Refused)
	i := slices.IndexFunc(s.Table, func(r whatifapi.ReportRow) bool { return r.Load == "seen" })
	require.GreaterOrEqual(t, i, 0)
	require.Equal(t, load, s.Table[i].Rate)
	require.NotNil(t, s.Table[i].TTFT[0].Value)
	assert.Equal(t, ttft.Answer.Value, *s.Table[i].TTFT[0].Value, "TTFT p90 at %g r/s", load)
}

// POST /v1/report is the command: the same question asked of the API gives
// the command's JSON, but for the recording's path, which the API never
// names. A stated Ca^2 reaches both.
func TestAtReplicas_APIIsTheCommand(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile(stairBOn)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stair.jsonl.gz"), b, 0o644))
	h := whatifapi.New(whatifapi.Config{Resolver: &servedSources{recordings: dir, warn: io.Discard}, WhatifSchema: "schema-v3"}).Handler()
	in, _ := apiInputs(t, h, "stair.jsonl.gz", "")
	code, body := apiPost(t, h, "/v1/report", whatifapi.ReportRequest{InputsID: in.InputsID,
		Fleet: &whatifapi.FleetKnob{Decode: ip(3)}, LoadSeen: fp(20), Shapes: []string{"measured", "1k/1k"}, ArrivalCV2: fp(2),
		SLO:    &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p90", Target: fp(2)}, TPOT: &whatifapi.Objective{Statistic: statMean, Target: fp(0.030)}},
		Assume: &whatifapi.Assume{VP: fp(200000), A: fp(0.020)}})
	require.Equal(t, http.StatusOK, code, "%s", body)
	var got whatifapi.ReportResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&got))

	text, exit := runCmd(t, atStair("--decode", "3", "--load-seen", "20", "--shape", "measured", "--shape", "1k/1k",
		"--arrival-cv2", "2", "-o", "json")...)
	require.Equal(t, 0, exit, text)
	var want whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(text), &want))
	assert.Equal(t, stairBOn, want.Source.Recording)
	want.Source.Recording = ""
	assert.Equal(t, want, got)
	assert.Contains(t, got.Shapes[0].Assumed, "arrivals Ca^2 2 (--arrival-cv2)", "the stated Ca^2 is priced")

	// The load seen, not stated: the API reads the load's shape off the only
	// decode variant, which it does not name, and prices the shape "load" as
	// the command does.
	code, body = apiPost(t, h, "/v1/report", whatifapi.ReportRequest{InputsID: in.InputsID,
		Fleet: &whatifapi.FleetKnob{Decode: ip(3)}, Shapes: []string{"measured", "load"},
		SLO:    &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p90", Target: fp(2)}, TPOT: &whatifapi.Objective{Statistic: statMean, Target: fp(0.030)}},
		Assume: &whatifapi.Assume{VP: fp(200000), A: fp(0.020)}})
	require.Equal(t, http.StatusOK, code, "%s", body)
	var seen whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal(body, &seen))
	text, exit = runCmd(t, atStair("--decode", "3", "--shape", "measured", "--shape", "load", "-o", "json")...)
	require.Equal(t, 0, exit, text)
	var cmd whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(text), &cmd))
	require.Len(t, seen.Shapes, 2)
	require.NotNil(t, seen.Shapes[0].LoadShape, "the served report reads the load's shape")
	assert.Equal(t, cmd.Shapes[0].LoadShape, seen.Shapes[0].LoadShape)
	assert.Equal(t, cmd.Shapes[1].InputTokens, seen.Shapes[1].InputTokens, "the shape load is the load's cycles'")
}

// A report asked wrongly is refused before anything is priced.
func TestAtReplicas_RefusesABadQuestion(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"load 0", atStair("--load", "0r/s"), "--load"},
		{"load-seen 0", atStair("--load-seen", "0"), "--load-seen"},
		{"a shape of one length", atStair("--shape", "8k"), "--shape"},
		{"headroom 1", atStair("--headroom", "1"), "--headroom"},
		{"ttft without a unit", atStair("--ttft", "2"), "--ttft"},
		{"tpot percentile 100", atStair("--tpot-percentile", "100"), "--tpot-percentile"},
		{"a flag it does not read", atStair("--ps-onsets"), "--ps-onsets"},
		{"the router-wait switch it does not read", atStair("--router-wait-in-prefill"), "--router-wait-in-prefill"},
		{"a load statistic it does not know", atStair("--load-statistic", "p99"), "--load-statistic"},
		{"a load statistic and a stated load", atStair("--load-statistic", "peak", "--load-seen", "20"), "--load-statistic"},
		{"a growth of 0", atStair("--load", "seen x0"), "--load"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, code := runCmd(t, c.args...)
			assert.Equal(t, 2, code, out)
			var b bytes.Buffer
			err := run(context.Background(), c.args, &b, io.Discard)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

// shapeKnees is the fleet knee at each shape, every shape priced in one
// report from one replay.
func shapeKnees(t *testing.T, shapes ...string) []float64 {
	t.Helper()
	q := stairQuery(0, 0, 20)
	for _, s := range shapes {
		sh, err := whatifapi.ParseReportShape(s)
		require.NoError(t, err)
		q.Shapes = append(q.Shapes, sh)
	}
	rep := stairReport(t, q)
	require.Len(t, rep.Shapes, len(shapes))
	knees := make([]float64, len(shapes))
	for i, s := range rep.Shapes {
		require.NotNil(t, s.Capacity.FleetKnee.Value, "%s: %+v", s.Label, s.Capacity.FleetKnee)
		knees[i] = *s.Capacity.FleetKnee.Value
	}
	return knees
}

// The knee falls as a request grows: as IL rises at a fixed OL (the prompt
// costs prefill more and holds more KV), and as OL rises at a fixed IL
// (a request holds its slot longer). Each step must fall, not only not
// rise, so a shape that is not priced fails here.
func TestAtReplicas_KneeFallsAsTheShapeGrows(t *testing.T) {
	for _, c := range []struct {
		name   string
		shapes []string
	}{
		{"IL at OL 200", []string{"1k/200", "2k/200", "4k/200", "8k/200", "16k/200"}},
		{"OL at IL 8k", []string{"8k/100", "8k/200", "8k/400", "8k/800"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			knees := shapeKnees(t, c.shapes...)
			for i := 1; i < len(knees); i++ {
				assert.Less(t, knees[i], knees[i-1], "%s then %s", c.shapes[i-1], c.shapes[i])
			}
		})
	}
}

// A shape far from the recording's is flagged, not hidden; the measured one
// never is. A prefix-hit rate the recording never saw is as far as a length.
func TestAtReplicas_FarShapesAreExtrapolated(t *testing.T) {
	q := stairQuery(0, 0, 20)
	for _, s := range []string{"measured", "8000/200", "1k/1k", "8000/200/0", "8000/200/0.3"} {
		sh, err := whatifapi.ParseReportShape(s)
		require.NoError(t, err)
		q.Shapes = append(q.Shapes, sh)
	}
	rep := stairReport(t, q)
	assert.False(t, rep.Shapes[0].Extrapolated)
	assert.False(t, rep.Shapes[1].Extrapolated, rep.Shapes[1].ExtrapolatedWhy)
	assert.True(t, rep.Shapes[2].Extrapolated)
	assert.Contains(t, rep.Shapes[2].Flags, string(extrapolatedShape))
	assert.False(t, rep.Shapes[3].Extrapolated, rep.Shapes[3].ExtrapolatedWhy)
	assert.True(t, rep.Shapes[4].Extrapolated)
	assert.Contains(t, rep.Shapes[4].ExtrapolatedWhy, "prefix hit 0.30")
}

// The fleet a verdict says is needed holds every target at the load with
// headroom, and one replica fewer of either role does not: on an
// under-provisioned staircase (2 + 2 at its p95), one under by a decode
// replica (2 + 1 at 20 r/s), and one over-provisioned (3 + 4 at its p95).
func TestAtReplicas_NeededFleetHolds(t *testing.T) {
	for _, c := range []struct {
		np, nd int
		load   float64
	}{{2, 2, 0}, {2, 1, 20}, {3, 4, 0}} {
		t.Run(fmt.Sprintf("%d+%d", c.np, c.nd), func(t *testing.T) {
			rep := stairReport(t, stairQuery(c.np, c.nd, c.load))
			v := rep.Shapes[0].Verdict
			require.Contains(t, []string{whatifapi.VerdictUnder, whatifapi.VerdictOver}, v.Verdict, v.Summary)
			require.NotNil(t, v.Needed, v.Summary)
			n := *v.Needed
			require.Empty(t, n.Unmet)
			load := *rep.Load.Rate.Value
			holds := func(np, nd int) bool {
				t.Helper()
				return stairReport(t, stairQuery(np, nd, load)).Shapes[0].Verdict.Checks[0].Holds
			}
			assert.True(t, holds(n.Prefill, n.Decode), "%d + %d at %.2f r/s", n.Prefill, n.Decode, load)
			if n.Prefill > 1 {
				assert.False(t, holds(n.Prefill-1, n.Decode), "%d + %d at %.2f r/s", n.Prefill-1, n.Decode, load)
			}
			if n.Decode > 1 {
				assert.False(t, holds(n.Prefill, n.Decode-1), "%d + %d at %.2f r/s", n.Prefill, n.Decode-1, load)
			}
			assert.Contains(t, v.Summary, fmt.Sprintf("needs %d prefill + %d decode (has %d + %d)", n.Prefill, n.Decode, c.np, c.nd))
		})
	}
}

// The verdict judges each role on its own: three prefill and four decode
// replicas at the staircase's p95 have a decode replica to spare even at the
// peak, and no prefill to spare, prefill being 1.9% under its operating
// point; the sentence says both, and the f the third prefill pod is priced
// with, which no run learned.
func TestAtReplicas_ShrinkIsPerRole(t *testing.T) {
	v := stairReport(t, stairQuery(3, 4, 0)).Shapes[0].Verdict
	assert.Equal(t, whatifapi.VerdictOver, v.Verdict, v.Summary)
	assert.Equal(t, []string{roleDecode}, v.Shrink)
	require.Len(t, v.Roles, 2)
	byRole := map[string]whatifapi.ReportRole{}
	for _, r := range v.Roles {
		byRole[r.Role] = r
	}
	assert.True(t, byRole[roleDecode].Spare)
	assert.Contains(t, byRole[roleDecode].Detail, "even at the peak 43.2 r/s")
	assert.False(t, byRole[rolePrefill].Spare)
	assert.Contains(t, byRole[rolePrefill].Detail, "1.9% under its operating point")
	assert.Contains(t, v.Summary, "prefill f 1.5 assumed at 3 pods")

	// Stated at the peak, decode still has one to spare and prefill is past
	// its edge: the fleet is under-provisioned, and the fleet it needs has
	// one decode replica fewer than it runs.
	v = stairReport(t, stairQuery(3, 4, 43.24)).Shapes[0].Verdict
	assert.Equal(t, whatifapi.VerdictUnder, v.Verdict, v.Summary)
	require.NotNil(t, v.Needed)
	assert.Less(t, v.Needed.Decode, 4, v.Summary)
}

// TestNoRoleHint: two variants a capture read as both roles (a P/D fleet with
// no role map) get the role-map hint, not only "name one"; a real decode
// variant among them, or none read as both, gets nothing added.
func TestNoRoleHint(t *testing.T) {
	pv := func(role string) pricedVariant {
		return pricedVariant{state: learnreplay.VariantState{Variant: "v-" + role, Role: role}}
	}
	assert.Contains(t, noRoleHint([]pricedVariant{pv("both"), pv("")}), "--role-of")
	for _, vs := range [][]pricedVariant{{pv("decode"), pv("decode")}, {pv("both"), pv("decode")}} {
		assert.Empty(t, noRoleHint(vs), "roles %s, %s", vs[0].state.Role, vs[1].state.Role)
	}
}

// The fleet needed never cuts a role the verdict says has no spare: four
// prefill and four decode replicas at the staircase's p95 (41.7 r/s) have a
// decode replica to spare even at the peak (43.2 r/s), and no prefill to
// spare, since three prefill replicas hold at the p95 but miss at the peak.
// The fleet needed was once found at the p95 alone, and said "needs 3
// prefill + 3 decode (has 4 + 4)" right after "prefill: no spare".
func TestAtReplicas_NeededKeepsARoleWithNoSpare(t *testing.T) {
	v := stairReport(t, stairQuery(4, 4, 0)).Shapes[0].Verdict
	require.Equal(t, whatifapi.VerdictOver, v.Verdict, v.Summary)
	assert.Equal(t, []string{roleDecode}, v.Shrink)
	require.NotNil(t, v.Needed, v.Summary)
	assert.Equal(t, whatifapi.ReportNeeded{Prefill: 4, Decode: 3, MaxReplicas: whatifapi.DefaultSizeBound},
		whatifapi.ReportNeeded{Prefill: v.Needed.Prefill, Decode: v.Needed.Decode, MaxReplicas: v.Needed.MaxReplicas}, v.Summary)
	assert.Contains(t, v.Summary, "prefill: no spare: one fewer misses at the peak 43.2 r/s")
	assert.Contains(t, v.Summary, "needs 4 prefill + 3 decode (has 4 + 4)")
	for _, alt := range v.Needed.Alternatives {
		assert.GreaterOrEqual(t, alt.Prefill, 4, "alternative %d + %d: %s", alt.Prefill, alt.Decode, v.Summary)
	}
}

// Across fleets over-provisioned at the staircase's p95, the fleet needed
// and the roles agree: a role with no spare is cut only in trade for more
// of the other role, and the fleet needed is smaller than the one judged.
// Spares are judged one role at a time, so with two the fewest in all may
// cut only one of them. The 16k-prompt shape is judged at TTFT p95, where
// decode admission sets most of its TTFT. Every fleet here is
// over-provisioned: one that is not fails rather than skips.
func TestAtReplicas_NeededAgreesWithSpare(t *testing.T) {
	for _, c := range []struct {
		shape  string
		np, nd int
	}{{"measured", 3, 4}, {"measured", 4, 4}, {"measured", 4, 5}, {"measured", 5, 4}, {"measured", 5, 5},
		{"measured", 4, 7}, {"16k/200/0.3", 5, 7}, {"16k/200/0.3", 6, 7}, {"16k/200/0.3", 8, 10}} {
		np, nd := c.np, c.nd
		t.Run(fmt.Sprintf("%s %d+%d", c.shape, np, nd), func(t *testing.T) {
			q := stairQuery(np, nd, 0)
			if c.shape != "measured" {
				sh, err := whatifapi.ParseReportShape(c.shape)
				require.NoError(t, err)
				q.TTFT.Percentile = 0.95
				q.Shapes = []whatifapi.ReportShapeAsk{sh}
			}
			v := stairReport(t, q).Shapes[0].Verdict
			require.Equal(t, whatifapi.VerdictOver, v.Verdict, v.Summary)
			require.NotNil(t, v.Needed, v.Summary)
			has := map[string]int{rolePrefill: np, roleDecode: nd}
			got := map[string]int{rolePrefill: v.Needed.Prefill, roleDecode: v.Needed.Decode}
			assert.Less(t, v.Needed.Prefill+v.Needed.Decode, np+nd, v.Summary)
			for _, r := range v.Roles {
				other := rolePrefill
				if r.Role == rolePrefill {
					other = roleDecode
				}
				if r.Spare || got[r.Role] >= has[r.Role] {
					continue
				}
				// A role with no spare is cut: only in trade for more of
				// the other, and the sentence says so.
				assert.Greater(t, got[other], has[other], "%s has no spare but is cut: %s", r.Role, v.Summary)
				assert.Contains(t, v.Grow, other, v.Summary)
				assert.Contains(t, v.Summary, "trading "+r.Role+" for "+other)
			}
		})
	}
}
