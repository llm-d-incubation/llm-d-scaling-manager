package main

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// busiestOf is the staircase's busiest stretch, as at-replicas counts it: the
// times of its first and last cycles, and the cycle time.
func busiestOf(t *testing.T) (time.Time, time.Time, time.Duration) {
	t.Helper()
	st := stairReplay.state(t)
	cycle := rollingCycle(st, whatifapi.WindowGapsOf(st.CycleTimes))
	rolling := rollingPeak(st.ArrivalRates, cycle, whatifapi.RollingWindow.Seconds())
	require.NotNil(t, rolling, "premise: the staircase has a busiest stretch")
	end, _ := busiestStretch(st.ArrivalRates, rolling.Cycles)
	require.GreaterOrEqual(t, end, rolling.Cycles-1)
	return st.CycleTimes[end-rolling.Cycles+1], st.CycleTimes[end], time.Duration(cycle * float64(time.Second))
}

// A 15-minute hole beside the staircase's busiest stretch refuses the load
// observed (WindowGapAtPeak): the verdict is not judged, exit 3. A stated
// load is judged as asked. The 15-minute hole of the review's simulation,
// away from the busiest stretch, refuses nothing
// (TestAtReplicas_ReportsWindowGaps).
func TestAtReplicas_RefusesAGapAtThePeak(t *testing.T) {
	from, to, cycle := busiestOf(t)
	st := stairReplay.state(t)
	first := st.CycleTimes[0]
	// The hole ends half a cycle before the stretch's first cycle, or, with
	// less than 20 minutes before it, starts half a cycle after its last.
	drop := func(now time.Time, _ time.Duration) bool {
		return now.Before(from.Add(-cycle/2)) && now.After(from.Add(-15*time.Minute))
	}
	if from.Sub(first) < 20*time.Minute {
		drop = func(now time.Time, _ time.Duration) bool {
			return now.After(to.Add(cycle/2)) && now.Before(to.Add(15*time.Minute))
		}
	}
	holed := holedRecording(t, drop)
	// B from the floor: the hole may cost the decode line its fit, and an
	// unpriced knee is not judged before the load is read.
	args := func(extra ...string) []string {
		return append([]string{"at-replicas", "--recording", holed, "--no-self-check", "--assume-itl-b", "floor"},
			append(slices.Clone(stairAssumed), extra...)...)
	}

	text, code := runCmd(t, args()...)
	assert.Equal(t, 3, code, text)
	assert.Contains(t, text, "a window gap at the peak", text)
	assert.NotContains(t, text, "a window gap at the peak, the p95", "a refused load names no statistic read")
	assert.Contains(t, text, whatifapi.VerdictNotJudged+": a gap in the window borders its busiest stretch", text)
	assert.Contains(t, text, "--load-seen", text)

	out, code := runCmd(t, args("-o", "json")...)
	assert.Equal(t, 3, code)
	var rep whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(out), &rep), out)
	require.NotNil(t, rep.Load.Rate.Refused, "the load is refused")
	assert.Equal(t, whatifapi.ReasonWindowGapAtPeak, rep.Load.Rate.Refused.Reason)
	assert.Contains(t, rep.Load.Rate.Refused.Detail, "a gap in the window")
	assert.Nil(t, rep.Load.Seen)
	assert.NotNil(t, rep.Load.P95, "the load seen is still reported")
	require.NotNil(t, rep.Source.Gaps)
	assert.GreaterOrEqual(t, rep.Source.Gaps.LargestGapSeconds, 14*60.0)
	require.NotEmpty(t, rep.Shapes)
	for _, s := range rep.Shapes {
		assert.Equal(t, whatifapi.VerdictNotJudged, s.Verdict.Verdict)
	}

	// A stated load is never refused.
	out, _ = runCmd(t, args("-o", "json", "--load-seen", "20")...)
	rep = whatifapi.ReportResponse{}
	require.NoError(t, json.Unmarshal([]byte(out), &rep), out)
	require.NotNil(t, rep.Load.Rate.Value, "a stated load is judged")
	assert.InDelta(t, 20, *rep.Load.Rate.Value, 1e-9)
	for _, s := range rep.Shapes {
		assert.NotEqual(t, whatifapi.VerdictNotJudged, s.Verdict.Verdict, s.Verdict.Summary)
	}
}

// gapAtPeak refuses on a gap at least RollingWindow long whose bordering
// cycle lies in the busiest stretch or is the peak cycle, and nothing else.
func TestGapAtPeak(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 2, 18, 0, 0, time.UTC)
	// 100 cycles every 15 s, cycles 50-69 (5 minutes) missing.
	var times []time.Time
	var rates []float64
	for i := range 100 {
		if i >= 50 && i < 70 {
			continue
		}
		times = append(times, t0.Add(time.Duration(i)*15*time.Second))
		rates = append(rates, 10)
	}
	g := whatifapi.WindowGapsOf(times)
	require.NotNil(t, g)
	rolling := func(rs []float64) *whatifapi.ReportRolling {
		return rollingPeak(rs, g.CycleSeconds, whatifapi.RollingWindow.Seconds())
	}
	with := func(at []int, x float64) []float64 {
		rs := slices.Clone(rates)
		for _, i := range at {
			rs[i] = x
		}
		return rs
	}
	// Index 49 is the cycle before the gap, 50 the one after it.
	before := with([]int{42, 43, 44, 45, 46, 47, 48, 49}, 30)
	u := gapAtPeak(before, times, g, rolling(before))
	require.NotNil(t, u, "the busiest stretch ends on the cycle before the gap")
	assert.Equal(t, whatif.Reason(whatifapi.ReasonWindowGapAtPeak), u.Reason)
	assert.Contains(t, u.Detail, "5m0s from 02:30 to 02:35Z")
	assert.Contains(t, u.Detail, "--load-seen")

	after := with([]int{50, 51, 52, 53, 54, 55, 56, 57}, 30)
	assert.NotNil(t, gapAtPeak(after, times, g, rolling(after)), "the busiest stretch starts on the cycle after the gap")

	// No stretch to read: the peak cycle alone decides.
	spike := with([]int{50}, 30)
	assert.NotNil(t, gapAtPeak(spike, times, g, nil), "the peak cycle borders the gap")

	away := with([]int{10, 11, 12, 13, 14, 15, 16, 17}, 30)
	assert.Nil(t, gapAtPeak(away, times, g, rolling(away)), "the busiest stretch is away from the gap")

	// A gap shorter than RollingWindow (one cycle) refuses nothing.
	short := slices.Concat(times[:30], times[31:50])
	sg := whatifapi.WindowGapsOf(short)
	require.NotNil(t, sg)
	sr := slices.Concat(before[:30], before[31:50])
	assert.Nil(t, gapAtPeak(sr, short, sg, rollingPeak(sr, sg.CycleSeconds, whatifapi.RollingWindow.Seconds())))

	assert.Nil(t, gapAtPeak(before, times, nil, rolling(before)), "no gaps read")
}

// busiestStretch is the first stretch of the highest mean; rollingPeak reads
// its mean.
func TestBusiestStretch(t *testing.T) {
	end, peak := busiestStretch([]float64{1, 5, 5, 1, 5, 5, math.NaN(), 0}, 2)
	assert.Equal(t, 2, end)
	assert.InDelta(t, 5, peak, 1e-9)
	end, _ = busiestStretch([]float64{1, 2}, 3)
	assert.Equal(t, -1, end)
	r := rollingPeak([]float64{1, 5, 5, 1}, 60, 120)
	require.NotNil(t, r)
	assert.Equal(t, 2, r.Cycles)
	assert.InDelta(t, 5, r.Peak, 1e-9)
}

// A day whose busiest minutes fall in a scrape gap: the buckets beside the
// gap that are the busiest are refused (WindowGapAtPeak), the plan exits 3,
// and the busiest need is read from the buckets left.
func TestDayPlan_RefusesAGapAtThePeak(t *testing.T) {
	res, err := olsRes()
	require.NoError(t, err)
	r := *res
	var series []whatifapi.SeriesSample
	for _, x := range day24h() {
		// The diurnal peak is at 16:00: drop 15:50 to 16:20.
		if h, m := x.At.Hour(), x.At.Minute(); (h == 15 && m >= 50) || (h == 16 && m < 20) {
			continue
		}
		series = append(series, x)
	}
	r.Series = series
	p, err := whatifapi.DayPlan(t.Context(), &r, dayRequest(), "schema-v3")
	require.NoError(t, err)
	require.NotNil(t, p.Gaps)
	exit := dayPlanExit(p)
	require.Error(t, exit, "the plan exits 3")
	assert.Equal(t, 3, exitCode(exit))
	var refusal *whatifapi.DayRefusal
	for i := range p.Refused {
		if p.Refused[i].Reason == whatifapi.ReasonWindowGapAtPeak {
			refusal = &p.Refused[i]
		}
	}
	require.NotNil(t, refusal, "the plan refuses the busiest bucket")
	assert.Equal(t, "window", refusal.Term)
	assert.Positive(t, refusal.Buckets)
	refused := 0
	for _, b := range p.Buckets {
		if !slices.Contains(b.Flags, whatifapi.FlagWindowGapAtPeak) {
			continue
		}
		refused++
		assert.Equal(t, whatifapi.BucketRefused, b.Status)
		assert.Nil(t, b.Needed)
		assert.True(t, b.Start.Equal(time.Date(2026, 11, 1, 15, 45, 0, 0, time.UTC)) || b.Start.Equal(time.Date(2026, 11, 1, 16, 20, 0, 0, time.UTC)),
			"a refused bucket borders the gap: %s", b.Start)
	}
	assert.Equal(t, refusal.Buckets, refused)
	var b bytes.Buffer
	require.NoError(t, writeDayPlanText(&b, whatifapi.InputsResponse{Namespace: "ns", Model: "model-02"}, p))
	assert.Contains(t, b.String(), "REFUSED the busiest bucket (WindowGapAtPeak)", b.String())

	// The day as recorded, its gap at 03:00, refuses nothing for it.
	for _, x := range dayPlan24h(t).Refused {
		assert.NotEqual(t, whatifapi.ReasonWindowGapAtPeak, x.Reason)
	}
}
