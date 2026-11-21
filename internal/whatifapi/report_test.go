package whatifapi

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rqInt(n int) *int { return &n }

// ReportQueryOf refuses a report asked wrongly with an InputError naming the
// field, before anything is priced.
func TestReportQueryOf_RefusesABadQuestion(t *testing.T) {
	shapes := func(n int) []string { return strings.Split(strings.Repeat("measured,", n-1)+"measured", ",") }
	loads := func(n int) []string { return strings.Split(strings.Repeat("seen,", n-1)+"seen", ",") }
	for _, c := range []struct {
		name string
		req  ReportRequest
		want string
	}{
		{"7 shapes", ReportRequest{Shapes: shapes(7)}, "shapes"},
		{"9 loads", ReportRequest{Loads: loads(9)}, "loads"},
		{"a bad shape", ReportRequest{Shapes: []string{"8k"}}, "shapes"},
		{"a bad load", ReportRequest{Loads: []string{"0r/s"}}, "loads"},
		{"a growth of 0", ReportRequest{Loads: []string{"seen x0"}}, "loads"},
		{"a growth of nothing", ReportRequest{Loads: []string{"seen x"}}, "loads"},
		{"fleet.prefill 0", ReportRequest{Fleet: &FleetKnob{Prefill: rqInt(0)}}, "fleet.prefill"},
		{"loadSeen 0", ReportRequest{LoadSeen: f64(0)}, "loadSeen"},
		{"loadSeen NaN", ReportRequest{LoadSeen: f64(math.NaN())}, "loadSeen"},
		{"loadSeen +Inf", ReportRequest{LoadSeen: f64(math.Inf(1))}, "loadSeen"},
		{"loadSeenX -1", ReportRequest{LoadSeenX: f64(-1)}, "loadSeenX"},
		{"loadStatistic p99", ReportRequest{LoadStatistic: "p99"}, "loadStatistic"},
		{"loadStatistic and loadSeen", ReportRequest{LoadStatistic: LoadPeak, LoadSeen: f64(20)}, "loadStatistic"},
		{"arrivalCV2 0", ReportRequest{ArrivalCV2: f64(0)}, "arrivalCV2"},
		{"arrivalCV2 NaN", ReportRequest{ArrivalCV2: f64(math.NaN())}, "arrivalCV2"},
		{"headroom 1", ReportRequest{Headroom: f64(1)}, "headroom"},
		{"bucketTargets", ReportRequest{SLO: &SLORequest{TTFT: &Objective{Target: f64(2), BucketTargets: map[string]float64{"1k": 1}}}}, "bucketTargets"},
		{"a target of 0", ReportRequest{SLO: &SLORequest{TPOT: &Objective{Target: f64(0)}}}, "slo.tpot.target"},
		{"5 ttftStatistics", ReportRequest{TTFTStatistics: []string{"p50", "p90", "p95", "p99", "mean"}}, "ttftStatistics"},
		{"a bad tpotStatistic", ReportRequest{TPOTStatistic: "p100"}, "tpotStatistic"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ReportQueryOf(c.req)
			require.Error(t, err)
			assert.True(t, IsInputError(err), "%v is not an InputError", err)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

// ReportQueryOf reads what it accepts into numbers: the limits themselves
// pass, and an absent field takes its default.
func TestReportQueryOf_ReadsAGoodQuestion(t *testing.T) {
	q, err := ReportQueryOf(ReportRequest{})
	require.NoError(t, err)
	assert.Equal(t, []float64{0.5, 0.9}, q.TTFTPercentiles)
	assert.InDelta(t, 0.9, q.TPOTPercentile, 0)
	assert.Empty(t, q.LoadStatistic, "the default statistic is the reporter's")
	assert.Nil(t, q.ArrivalCV2)

	q, err = ReportQueryOf(ReportRequest{
		Fleet:  &FleetKnob{Prefill: rqInt(3), Decode: rqInt(4)},
		Shapes: []string{"measured", "8k/200", "1k/4k/0.3", "2k/2k", "4k/1k", "16k/200"},
		Loads:  []string{"50%", "80%", "100%", "5r/s", "seen", "10r/s", "20%", "30%"}, LoadStatistic: LoadPeak, LoadSeenX: f64(1.5),
		ArrivalCV2: f64(2), Headroom: f64(0.8), TTFTStatistics: []string{"mean", "p50", "p90", "p95"}, TPOTStatistic: "mean",
		SLO: &SLORequest{TTFT: &Objective{Statistic: "p90", Target: f64(2)}, TPOT: &Objective{Target: f64(0.03)}},
	})
	require.NoError(t, err)
	assert.Equal(t, 3, q.Prefill)
	assert.Equal(t, 4, q.Decode)
	assert.Len(t, q.Shapes, 6)
	require.NotNil(t, q.Shapes[2].Hit)
	assert.InDelta(t, 0.3, *q.Shapes[2].Hit, 0)
	assert.InDelta(t, 4096, q.Shapes[2].OL, 0)
	assert.Len(t, q.Loads, 8)
	assert.Equal(t, LoadPeak, q.LoadStatistic)
	assert.InDelta(t, 1.5, q.LoadSeenX, 0)
	require.NotNil(t, q.ArrivalCV2)
	assert.InDelta(t, 2, *q.ArrivalCV2, 0)
	assert.InDelta(t, 0.8, q.Headroom, 0)
	assert.Equal(t, []float64{0, 0.5, 0.9, 0.95}, q.TTFTPercentiles)
	assert.InDelta(t, 0, q.TPOTPercentile, 0)
	assert.Equal(t, &ReportTarget{Seconds: 2, Percentile: 0.9}, q.TTFT)
	assert.Equal(t, &ReportTarget{Seconds: 0.03}, q.TPOT)

	// A growth load is written three ways and labelled one.
	q, err = ReportQueryOf(ReportRequest{Loads: []string{"seen x1.5", "seen\u00d71.5", "seen*2", "seen"}})
	require.NoError(t, err)
	require.Len(t, q.Loads, 4)
	for i, want := range []float64{1.5, 1.5, 2} {
		assert.True(t, q.Loads[i].Grown())
		assert.InDelta(t, want, q.Loads[i].X, 0)
	}
	assert.Equal(t, "seen x1.5", q.Loads[1].Label)
	assert.Equal(t, "seen x2", q.Loads[2].Label)
	assert.False(t, q.Loads[3].Grown())
}

// SmallestFleet finds the fewest replicas in all, not decode's floor and the
// prefill that joins it: where decode admission sets most of a TTFT, the
// floor (5 decode) needs 18 prefill, while 7 decode need 6. Of the fleets of
// 13, the one with the fewest decode replicas is the answer. Below the
// floor nothing holds, and with no fleet in the bound the fleet is unmet.
func TestSmallestFleet_FewestInAll(t *testing.T) {
	decodeHolds := func(n FleetCounts) (bool, error) { return n.Decode >= 5, nil }
	holds := func(n FleetCounts) (bool, error) { return n.Decode >= 5 && n.Prefill*(n.Decode-4) >= 18, nil }
	n, unmet, err := SmallestFleet(FleetCounts{Prefill: 32, Decode: 32}, FleetCounts{}, decodeHolds, holds)
	require.NoError(t, err)
	assert.Empty(t, unmet)
	assert.Equal(t, FleetCounts{Prefill: 6, Decode: 7}, n)

	_, unmet, err = SmallestFleet(FleetCounts{Prefill: 3, Decode: 6}, FleetCounts{}, decodeHolds, holds)
	require.NoError(t, err)
	assert.Equal(t, roleFleet, unmet)
	_, unmet, err = SmallestFleet(FleetCounts{Prefill: 32, Decode: 4}, FleetCounts{}, decodeHolds, holds)
	require.NoError(t, err)
	assert.Equal(t, roleDecode, unmet)
}
