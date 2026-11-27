package whatifapi

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stair is a staircase of levels steps at rates 1, 1.5, 2.25, ... each held for
// per steps at one fleet, every step within but those bad says are not.
func stair(levels, per int, bad func(level, step int) bool) []scPoint {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	out := make([]scPoint, 0, levels*per)
	for l := range levels {
		for s := range per {
			within := bad == nil || !bad(l, s)
			ratio := 1.0
			if !within {
				ratio = 1.5
			}
			out = append(out, scPoint{at: t0.Add(time.Duration(l*per+s) * 5 * time.Minute), rate: math.Pow(1.5, float64(l)) * (1 + 0.01*float64(s)),
				np: 2, nd: 2, band: LoadBelow80, ratio: ratio, within: within, measured: 0.4})
		}
	}
	return out
}

// Steps are read level by level: a run of steps at one fleet and about one
// rate is one level, a new rate or fleet starts the next, and the within
// share's bound is drawn over levels, not steps: 9 of 12 steps within reads
// far lower over 4 levels of 3 than the same share would over 12 levels.
func TestSelfCheck_StepsAreReadByLevel(t *testing.T) {
	pts := stair(4, 3, nil)
	require.Len(t, levelsOf(pts), 4)
	pts[4].nd = 3
	assert.Len(t, levelsOf(pts), 4, "one step off a level the run returns to is noise, not a level")
	pts[5].nd = 3
	assert.Len(t, levelsOf(pts), 5, "a run at another fleet is a level of its own")

	// 9 of 12 within: one level of 4 entirely off.
	grouped := levelsOf(stair(4, 3, func(l, _ int) bool { return l == 2 }))
	spread := levelsOf(stair(12, 1, func(l, _ int) bool { return l%4 == 2 }))
	require.Len(t, grouped, 4)
	require.Len(t, spread, 12)
	g, s := withinBound(grouped), withinBound(spread)
	assert.Less(t, g, s, "correlated steps bound the share lower")
	assert.Less(t, g, 0.75)
	assert.InDelta(t, withinBound(grouped), g, 0, "a fixed seed reads the same each time")
	assert.InDelta(t, 1, withinBound(levelsOf(stair(4, 3, nil))), 0)
}

// The worst tenth near the knee is what the near-knee rule reads, not its
// median; steps called overloaded that measured near the unloaded figure
// count as served.
func TestSelfCheck_NearKneeAndServedPastKnee(t *testing.T) {
	pts := stair(2, 3, nil)
	assert.Nil(t, nearKneeOf(pts))
	for i, r := range []float64{1, 1.1, 5, 17, 59} {
		pts = append(pts, scPoint{band: LoadNearKnee, ratio: r, at: time.Unix(int64(i), 0)})
	}
	k := nearKneeOf(pts)
	require.NotNil(t, k)
	assert.Equal(t, 5, k.Steps)
	assert.InDelta(t, 59, k.Worst, 0)

	// The steps below 0.8 measured 0.4 s: 0.5 s is served, 0.7 s is not.
	assert.Equal(t, 1, servedPastKnee(pts, []float64{0.5, 0.7, 3}))
	assert.Equal(t, 0, servedPastKnee(nil, []float64{0.5}), "no step below 0.8: nothing to compare with")
	ttft, tpot := &SelfCheckFigure{}, &SelfCheckFigure{}
	ttft.scoreSteps(pts, []float64{0.5}, true)
	tpot.scoreSteps(pts, []float64{0.5}, false)
	assert.Equal(t, 1, ttft.ServedPastKnee)
	assert.Equal(t, 0, tpot.ServedPastKnee, "a saturated decode queues in TTFT, not TPOT: TPOT does not score the knee")
}

// A self-check says what it tested, and an answer outside that is not
// covered.
func TestSelfCheck_SaysWhatItCovered(t *testing.T) {
	steps := []BacktestStep{{Rate: 2, Prefill: 2, Decode: 2}, {Rate: 9.5, Prefill: 2, Decode: 2}, {Rate: 4, Prefill: 2, Decode: 3},
		{Rate: 40, Prefill: 9, Decode: 9, Excluded: ExcludedWarmUp}}
	c := coverageOf(steps)
	require.NotNil(t, c)
	assert.Equal(t, "2.00 to 9.50 r/s at 2 prefill + 2 decode or 2 prefill + 3 decode", c.Words())
	assert.True(t, c.Covers(5, 2, 2))
	assert.False(t, c.Covers(12, 2, 2), "a rate past the steps'")
	assert.False(t, c.Covers(5, 3, 3), "a fleet no step ran")
	assert.Nil(t, coverageOf(steps[3:]))

	e, folds := scEntry(t, 1, 1)
	sc := selfCheckEntry(context.Background(), e, folds, SelfCheckInfo{PlannerVersion: "vtest"})
	require.NotNil(t, sc.Covered, sc.Line)
	assert.Contains(t, sc.Line, "It tested "+sc.Covered.Words()+": an answer at another rate or fleet is not covered by it.")
	require.NotNil(t, sc.TTFT)
	assert.Positive(t, sc.TTFT.Levels, sc.Line)
	assert.Contains(t, sc.Line, "load levels")
}

// A verdict says what the self-check says of it: not trusted, with exit
// ExitUntrusted, when the figures it rests on are inconsistent; not covered
// at a rate, fleet or shape the self-check did not test; and nothing on a
// verdict not judged, or with no self-check run.
func TestReport_JudgeTrust(t *testing.T) {
	cov := &SelfCheckCoverage{RateLo: 1, RateHi: 10, Fleets: []FleetCounts{{Prefill: 2, Decode: 2}}}
	report := func(untrusted []string, rate float64, scope, source, verdict string, ttftTarget bool) *ReportResponse {
		r := &ReportResponse{Fleet: ReportFleet{Prefill: 2, Decode: 2}, Load: ReportLoad{Rate: Figure{Value: ptr(rate)}},
			Shapes:    []ReportShape{{Source: source, Verdict: ReportVerdict{Verdict: verdict, Scope: scope}}},
			SelfCheck: &SelfCheck{Status: SelfCheckInconsistent, Untrusted: untrusted, Covered: cov}}
		if ttftTarget {
			r.SLO.TTFT = &ReportObjective{Statistic: "p90", Target: 2}
		}
		r.JudgeTrust()
		return r
	}
	for name, c := range map[string]struct {
		r     *ReportResponse
		trust string
		code  int
	}{
		"tpot untrusted":              {report([]string{"tpot"}, 5, ScopeFleet, "measured", VerdictUnder, false), TrustNot + ": the self-check found this recording's TPOT figures", ExitUntrusted},
		"ttft untrusted, fleet":       {report([]string{"ttft"}, 5, ScopeFleet, "measured", VerdictRight, false), TrustNot + ": the self-check found this recording's TTFT figures", ExitUntrusted},
		"ttft untrusted, decode only": {report([]string{"ttft"}, 5, ScopeDecodeOnly, "measured", VerdictUnder, false), "", 0},
		"ttft target, decode only":    {report([]string{"ttft"}, 5, ScopeDecodeOnly, "measured", VerdictUnder, true), TrustNot, ExitUntrusted},
		"not covered: rate":           {report(nil, 12.5, ScopeFleet, "measured", VerdictUnder, false), TrustNotCovered + ": it tested 1.00 to 10.00 r/s at 2 prefill + 2 decode; this verdict is at 12.50 r/s", 0},
		"not covered: shape":          {report(nil, 5, ScopeFleet, "asked", VerdictUnder, false), TrustNotCovered + ": it tested the recorded traffic, not this shape", 0},
		"covered":                     {report(nil, 5, ScopeFleet, "measured", VerdictUnder, false), "", 0},
		"not judged":                  {report([]string{"tpot"}, 5, ScopeFleet, "measured", VerdictNotJudged, false), "", 0},
	} {
		t.Run(name, func(t *testing.T) {
			got := c.r.Shapes[0].Verdict.Trust
			if c.trust == "" {
				assert.Empty(t, got)
			} else {
				assert.Contains(t, got, c.trust)
			}
			assert.Equal(t, c.code, c.r.ExitCode)
		})
	}
	// A growth what-if is judged at its own rate, and never moves the exit
	// code.
	g := &ReportResponse{Fleet: ReportFleet{Prefill: 2, Decode: 2}, Load: ReportLoad{Rate: Figure{Value: ptr(5)}},
		Shapes: []ReportShape{{Source: "measured", Verdict: ReportVerdict{Verdict: VerdictRight},
			Growth: []ReportGrowth{{Rate: 15, Verdict: ReportVerdict{Verdict: VerdictUnder}}}}},
		SelfCheck: &SelfCheck{Status: SelfCheckWeak, Covered: cov}}
	g.JudgeTrust()
	assert.Empty(t, g.Shapes[0].Verdict.Trust)
	assert.Contains(t, g.Shapes[0].Growth[0].Verdict.Trust, TrustNotCovered+": it tested 1.00 to 10.00 r/s")
	assert.Equal(t, 0, g.ExitCode)
	g.Shapes[0].Source, g.Shapes[0].Growth[0].Verdict.Trust = "asked", ""
	g.JudgeTrust()
	assert.Contains(t, g.Shapes[0].Growth[0].Verdict.Trust, "not this shape", "a typed shape's what-if is not covered either")

	r := report([]string{"tpot"}, 5, ScopeFleet, "measured", VerdictUnder, false)
	r.ExitCode, r.Shapes[0].Verdict.Trust = 3, ""
	r.JudgeTrust()
	assert.Equal(t, 3, r.ExitCode, "a refusal's exit code stands")
	r.SelfCheck.Status = SelfCheckNotRun
	r.Shapes[0].Verdict.Trust = ""
	r.JudgeTrust()
	assert.Empty(t, r.Shapes[0].Verdict.Trust)
}

// TTFT steps near the knee left unscored as unsettled are counted beside
// the near-knee rule: in its words where it runs, and as why it did not
// where too few steps there were scored. A step whose TTFT was refused is
// the refusal's, not counted as unsettled.
func TestSelfCheckNearKneeUnsettled(t *testing.T) {
	m := measured{requests: 1000, ttft: []*float64{ptr(1.0)}, ttftWhy: []string{""}, ttftBucket: []*Band{nil}}
	step := func(load string, unsettled bool, excluded string, pred *float64) BacktestStep {
		st := BacktestStep{Load: load, Unsettled: unsettled, Excluded: excluded,
			TTFT: []BacktestPoint{{Statistic: "p90", Predicted: Figure{Value: pred}}}}
		scoreStep(&st, m, nil)
		return st
	}
	steps := []BacktestStep{
		step(LoadNearKnee, true, "", ptr(1.0)),
		step(LoadNearKnee, true, "", ptr(1.0)),
		step(LoadNearKnee, true, ExcludedBurst, ptr(1.0)),
		step(LoadBelow80, true, "", ptr(1.0)),
		step(LoadNearKnee, false, "", ptr(1.0)),
		step(LoadNearKnee, true, "", nil),
	}
	assert.Equal(t, UnscoredRefused, steps[5].TTFT[0].Unscored, "a refused TTFT at an unsettled step counts by its refusal")
	assert.Equal(t, 2, nearKneeUnsettled(steps), "judged steps near the knee with a priced TTFT only")

	fig := func(scored int) *SelfCheckFigure {
		return &SelfCheckFigure{Bias: ptr(1), Steps: 10, Levels: 10, NearKnee: &SelfCheckNearKnee{Steps: scored, Worst: 1}, NearKneeUnsettled: 2}
	}
	sc := &SelfCheck{}
	rules, _, _ := sc.figureRules("ttft", "TTFT", fig(selfCheckNearKneeMin), 0.9)
	i := slices.IndexFunc(rules, func(r SelfCheckRule) bool { return r.Name == "ttft-near-knee" })
	require.GreaterOrEqual(t, i, 0)
	assert.Contains(t, rules[i].Detail, "; 2 more there did not settle and are not scored")
	assert.Empty(t, sc.tail())

	sc = &SelfCheck{}
	rules, _, _ = sc.figureRules("ttft", "TTFT", fig(1), 0.9)
	assert.False(t, slices.ContainsFunc(rules, func(r SelfCheckRule) bool { return r.Name == "ttft-near-knee" }))
	assert.Equal(t, " The near-knee rule did not run: at 0.8 to 1.0 of the knee 1 steps were scored, fewer than 3; "+
		"2 more there did not settle and are not scored.", sc.tail())
}

// A day plan's bucket says what the self-check says of its verdict, as a
// report's verdict does: not trusted when the verdict rests on figures it
// found inconsistent (TTFT where the bucket priced prefill, TPOT always),
// else not covered at a sized rate or a fleet it did not test; nothing on
// a bucket refused or not sized, or with no self-check run. The hour takes
// its worst bucket's opening words, and the summary counts both.
func TestDayPlan_JudgeTrust(t *testing.T) {
	h0 := time.Date(2026, 11, 14, 7, 0, 0, 0, time.UTC)
	cov := &SelfCheckCoverage{RateLo: 1, RateHi: 10, Fleets: []FleetCounts{{Prefill: 2, Decode: 2}}}
	bucket := func(min int, verdict string, rate float64, prefill bool) DayBucket {
		b := DayBucket{Start: h0.Add(time.Duration(min) * time.Minute), Verdict: verdict, SizedRate: ptr(rate), Actual: &FleetCounts{Prefill: 2, Decode: 2}}
		if !prefill {
			b.Flags = []string{FlagPrefillNotPriced}
		}
		return b
	}
	plan := func(untrusted []string) *DayPlanResponse {
		return &DayPlanResponse{
			SelfCheck: &SelfCheck{Status: SelfCheckInconsistent, Untrusted: untrusted, Covered: cov},
			Buckets: []DayBucket{
				bucket(0, DayVerdictUnder, 5, true),    // 07:00 priced prefill
				bucket(5, DayVerdictOver, 5, false),    // 07:05 decode alone
				bucket(60, DayVerdictMatch, 12, false), // 08:00 past the rates tested
				bucket(65, DayVerdictRefused, 5, true), // 08:05 refused
				{Start: h0.Add(70 * time.Minute)},      // 08:10 not sized
				bucket(120, DayVerdictUnder, 5, false), // 09:00 covered
			},
			Hours: []DayHour{{Start: h0}, {Start: h0.Add(time.Hour)}, {Start: h0.Add(2 * time.Hour)}},
		}
	}

	p := plan([]string{"ttft"})
	p.JudgeTrust()
	assert.Equal(t, TrustNot+": the self-check found this recording's TTFT figures inconsistent with it, and this verdict rests on them", p.Buckets[0].Trust)
	assert.Empty(t, p.Buckets[1].Trust, "decode sized alone rests on no TTFT figure")
	assert.Equal(t, TrustNotCovered+": it tested 1.00 to 10.00 r/s at 2 prefill + 2 decode; this verdict is at 12.00 r/s at 2 prefill + 2 decode", p.Buckets[2].Trust)
	assert.Empty(t, p.Buckets[3].Trust, "a refused bucket")
	assert.Empty(t, p.Buckets[4].Trust, "a bucket not sized")
	assert.Empty(t, p.Buckets[5].Trust)
	assert.Equal(t, []string{TrustNot, TrustNotCovered, ""}, []string{p.Hours[0].Trust, p.Hours[1].Trust, p.Hours[2].Trust})
	assert.Equal(t, 1, p.Summary.Counts.Untrusted)
	assert.Equal(t, 1, p.Summary.Counts.NotCovered)

	p = plan([]string{"tpot"})
	p.JudgeTrust()
	for _, i := range []int{0, 1, 2, 5} {
		assert.Contains(t, p.Buckets[i].Trust, TrustNot+": the self-check found this recording's TPOT figures", "bucket %d: TPOT sizes every bucket", i)
	}
	assert.Equal(t, 4, p.Summary.Counts.Untrusted)
	assert.Zero(t, p.Summary.Counts.NotCovered, "not trusted outranks not covered")

	p = plan([]string{"ttft", "tpot"})
	p.JudgeTrust()
	assert.Contains(t, p.Buckets[0].Trust, "TTFT and TPOT figures")
	assert.Contains(t, p.Buckets[1].Trust, "TPOT figures")
	assert.NotContains(t, p.Buckets[1].Trust, "TTFT")

	p = plan([]string{"ttft"})
	p.Buckets[0].Start = h0.Add(10 * time.Minute)
	p.Buckets[1], p.Buckets[2] = bucket(0, DayVerdictMatch, 12, false), p.Buckets[1]
	p.JudgeTrust()
	assert.Equal(t, TrustNot, p.Hours[0].Trust, "not trusted outranks a not covered bucket met before it in the hour")

	// A fleet the check did not test, a stated shape, an unmet bucket (judged
	// by the bound, tested for no coverage) and a fleet with no replica of a
	// role (judged on no figure).
	p = plan(nil)
	p.Buckets[0].Actual = &FleetCounts{Prefill: 3, Decode: 2}
	p.Buckets[1].Shape.Source = dayShapeStated
	p.Buckets[2].Status, p.Buckets[2].Verdict = BucketUnmet, DayVerdictUnder
	p.Buckets[5].Status, p.Buckets[5].Actual = BucketSized, &FleetCounts{Prefill: 2}
	p.JudgeTrust()
	assert.Equal(t, TrustNotCovered+": it tested 1.00 to 10.00 r/s at 2 prefill + 2 decode; this verdict is at 5.00 r/s at 3 prefill + 2 decode", p.Buckets[0].Trust)
	assert.Equal(t, TrustNotCovered+": it tested the recorded traffic, not this shape", p.Buckets[1].Trust)
	assert.Empty(t, p.Buckets[2].Trust, "unmet, past the rates tested")
	assert.Empty(t, p.Buckets[5].Trust, "no hadLimit")
	assert.Equal(t, 2, p.Summary.Counts.NotCovered)
	p.SelfCheck.Untrusted = []string{"tpot"}
	p.JudgeTrust()
	assert.Contains(t, p.Buckets[2].Trust, TrustNot+":", "an unmet bucket's verdict rests on the figures too")
	assert.Empty(t, p.Buckets[5].Trust, "no figure, nothing to distrust")
	assert.Equal(t, 3, p.Summary.Counts.Untrusted, "judged afresh: 0, 1 and 2, not added to the last call's")
	assert.Zero(t, p.Summary.Counts.NotCovered)
	before := p.Summary.Counts.Untrusted
	p.JudgeTrust()
	assert.Equal(t, before, p.Summary.Counts.Untrusted, "a second call counts the same")
	p.SelfCheck.Untrusted = nil
	p.Buckets[2].Shape.Source = dayShapeStated
	p.JudgeTrust()
	assert.Contains(t, p.Buckets[2].Trust, "not this shape", "an unmet bucket priced at a stated shape is not covered either")
	p.SelfCheck.Untrusted = []string{"tpot"}
	p.Buckets[2].Shape.Source = ""
	p.JudgeTrust()
	assert.Equal(t, "TPOT", p.UntrustedFigures(p.Buckets[0]))
	assert.Empty(t, p.UntrustedFigures(p.Buckets[5]))

	p = plan([]string{"ttft", "tpot"})
	p.SelfCheck.Status = SelfCheckNotRun
	p.JudgeTrust()
	for _, b := range p.Buckets {
		assert.Empty(t, b.Trust)
	}
	assert.Zero(t, p.Summary.Counts.Untrusted)
	p.SelfCheck = nil
	p.JudgeTrust()
	assert.Zero(t, p.Summary.Counts.Untrusted)
}
