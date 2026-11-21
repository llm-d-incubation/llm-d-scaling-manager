package whatifapi

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// cycles is n cycles every gap from t0, each at rate.
func cycles(t0 time.Time, gap time.Duration, n int, rate float64) []SeriesSample {
	out := make([]SeriesSample, n)
	for i := range out {
		out[i] = SeriesSample{At: t0.Add(time.Duration(i) * gap), Rate: rate, Prefill: 1, Decode: 2}
	}
	return out
}

// A day across a clock change is 24 hours of buckets in UTC: the local
// clock's repeated hour (New York, 2026-11-01, 01:00 to 02:00 twice) is
// neither merged into one bucket nor dropped, and every bucket holds the
// cycles of its own five minutes.
func TestBucketizeAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, ny)
	series := cycles(t0, 30*time.Second, 25*120, 1)
	b := bucketize(series, 5*time.Minute)
	require.Len(t, b, 25*12, "25 hours of cycles is 25 hours of buckets")
	for i, x := range b {
		assert.Len(t, x.samples, 10, "bucket %d (%s)", i, x.start)
		assert.Equal(t, time.UTC, x.start.Location())
		if i > 0 {
			assert.Equal(t, 5*time.Minute, x.start.Sub(b[i-1].start))
		}
	}
	// The hours are UTC too: 25 of them, each once.
	db := make([]DayBucket, len(b))
	for i, x := range b {
		db[i] = DayBucket{Start: x.start}
	}
	hours := hoursOf(series, db)
	require.Len(t, hours, 25)
	for i := 1; i < len(hours); i++ {
		assert.Equal(t, time.Hour, hours[i].Start.Sub(hours[i-1].Start))
	}
}

// Buckets start on the step in UTC whatever the first cycle's time, and a
// gap in the cycles is buckets with no data, kept in place, not closed up.
func TestBucketizeGapsAndAlignment(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 10, 2, 13, 0, time.UTC)
	series := append(cycles(t0, 15*time.Second, 40, 2), cycles(t0.Add(30*time.Minute), 15*time.Second, 40, 3)...)
	b := bucketize(series, 5*time.Minute)
	require.Equal(t, time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), b[0].start)
	var empty int
	for _, x := range b {
		if len(x.samples) == 0 {
			empty++
		}
	}
	assert.Equal(t, 3, empty, "10:15 to 10:30 has no cycles")
	assert.Len(t, b, 9, "10:00 to 10:40")
	// Out of order cycles land in their own buckets.
	rev := append([]SeriesSample{}, series...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	assert.Equal(t, b, bucketize(rev, 5*time.Minute))
}

// A bucket's rate: the mean, the nearest-rank p95 and the most.
func TestRateOf(t *testing.T) {
	var s []SeriesSample
	for i := 1; i <= 20; i++ {
		s = append(s, SeriesSample{Rate: float64(i)})
	}
	assert.Equal(t, DayRate{Mean: 10.5, P95: 19, Max: 20}, rateOf(s))
	assert.Equal(t, DayRate{Mean: 4, P95: 4, Max: 4}, rateOf([]SeriesSample{{Rate: 4}}))
}

// stubDay is a day plan of n cycles every 15 s at rate, a fleet of had
// prefill and decode pods, on the stub's fleets (stub_test.go): prefill knee
// 10 r/s a replica, decode 12, TTFT 0.2 s and TPOT 12 ms targets, headroom
// 0.85.
func stubDay(t *testing.T, r *stubResolver, n int, rate float64, had FleetCounts) *DayPlanResponse {
	t.Helper()
	res, err := r.Resolve(t.Context(), InputsRequest{})
	require.NoError(t, err)
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	for i := range n {
		res.Series = append(res.Series, SeriesSample{At: t0.Add(time.Duration(i) * 15 * time.Second), Rate: rate, RateKnown: true,
			Prefill: had.Prefill, Decode: had.Decode})
	}
	p, err := DayPlan(t.Context(), res, DayPlanRequest{}, "schema-v3")
	require.NoError(t, err)
	return p
}

// The fleet a bucket had is judged whole, at the bucket's rate, never role
// by role against the fewest replicas found. Here decode admission cuts the
// prefill knee a hundredfold below 3 prefill and 3 decode replicas, so the
// search, the fewest in all, finds 3 prefill and 1 decode for 6 r/s; the fleet
// had, 2 prefill and 3 decode, serves 6 r/s too (its limit 10 r/s), and
// neither 1+3 (5 r/s) nor 2+2 (cut) does: it matches. Role by role it would
// read under, prefill 2 against 3.
func TestDayPlanJudgesTheFleetItHadWhole(t *testing.T) {
	r := &stubResolver{dip: func(p, d int) bool { return p < 3 && d < 3 }}
	p := stubDay(t, r, 20, 6, FleetCounts{Prefill: 2, Decode: 3})
	require.Len(t, p.Buckets, 1)
	b := p.Buckets[0]
	require.Equal(t, BucketSized, b.Status)
	require.NotNil(t, b.Needed.Prefill)
	assert.Equal(t, DayNeeded{Prefill: ptrInt(3), Decode: 1}, *b.Needed)
	require.NotNil(t, b.HadLimit)
	require.NotNil(t, b.HadLimit.Value)
	assert.GreaterOrEqual(t, *b.HadLimit.Value, 6.0, "the fleet it had serves the rate")
	assert.Equal(t, DayVerdictMatch, b.Verdict, "judged whole, 2+3 serves 6 r/s and one replica fewer does not")
	assert.Empty(t, p.Summary.UnderHours)

	// More than enough: 4+4 serves, and so does 3+4: over.
	over := stubDay(t, r, 20, 6, FleetCounts{Prefill: 4, Decode: 4})
	assert.Equal(t, DayVerdictOver, over.Buckets[0].Verdict)
	// Too few: 1+1 serves 0.1 r/s: under.
	under := stubDay(t, r, 20, 6, FleetCounts{Prefill: 1, Decode: 1})
	assert.Equal(t, DayVerdictUnder, under.Buckets[0].Verdict)
	assert.Len(t, under.Summary.UnderHours, 1)
	// No decode pod serves nothing: under, with no figure to price.
	none := stubDay(t, r, 20, 6, FleetCounts{Prefill: 2, Decode: 0})
	assert.Equal(t, DayVerdictUnder, none.Buckets[0].Verdict)
}

// ptrInt is a pointer to n.
func ptrInt(n int) *int { return &n }

// A source that cannot price prefill sizes decode alone, as a lower bound
// (AtLeast), names the refusal once, gives no GPU-hours of a need, and
// judges decode alone.
func TestDayPlanDecodeAloneIsALowerBound(t *testing.T) {
	p := stubDay(t, &stubResolver{refusePrefill: true}, 20, 6, FleetCounts{Prefill: 1, Decode: 1})
	require.Len(t, p.Refused, 1)
	assert.Equal(t, termTTFT, p.Refused[0].Term)
	assert.Equal(t, string(whatif.NoPrefillCapacity), p.Refused[0].Reason)
	b := p.Buckets[0]
	require.NotNil(t, b.Needed)
	assert.Equal(t, DayNeeded{Decode: 1, AtLeast: true}, *b.Needed)
	assert.Contains(t, b.Flags, FlagPrefillNotPriced)
	assert.Equal(t, DayVerdictMatch, b.Verdict)
	assert.Equal(t, 1, p.Summary.Counts.PrefillNotPriced)
	assert.Nil(t, p.Summary.NeededGPUHours)
	assert.Nil(t, p.Summary.NeededReplicaHours.Prefill)
	assert.True(t, slices.ContainsFunc(p.Notes, func(n string) bool { return strings.Contains(n, "lower bound") }))
}

// A decode line pinned on the default intercept prices neither TPOT nor
// decode's knee: dropping TPOT leaves no rule that bounds decode, so nothing
// is sized and the hour is refused, naming both terms, as at-replicas
// refuses the fleet. Sized on prefill's limit alone it read 1 prefill and 1
// decode for 6 r/s, whatever decode could carry. Asked with no TPOT, the
// same.
func TestDayPlanPinnedDecodeLineSizesNothing(t *testing.T) {
	for name, tpot := range map[string]*Objective{"tpot asked": nil, "no tpot": {Statistic: statisticNone}} {
		t.Run(name, func(t *testing.T) {
			r := &stubResolver{pinnedLine: true}
			res, err := r.Resolve(t.Context(), InputsRequest{})
			require.NoError(t, err)
			t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
			for i := range 20 {
				res.Series = append(res.Series, SeriesSample{At: t0.Add(time.Duration(i) * 15 * time.Second), Rate: 6, RateKnown: true,
					Prefill: 2, Decode: 2})
			}
			p, err := DayPlan(t.Context(), res, DayPlanRequest{SLO: &SLORequest{TPOT: tpot}}, "schema-v3")
			require.NoError(t, err)
			b := p.Buckets[0]
			assert.Nil(t, b.Needed, "a decode count no rule bounds")
			assert.Equal(t, BucketRefused, b.Status)
			assert.Equal(t, DayVerdictRefused, b.Verdict)
			require.Len(t, p.Hours, 1)
			assert.Nil(t, p.Hours[0].Needed)
			assert.Equal(t, DayVerdictRefused, p.Hours[0].Verdict)
			assert.Zero(t, p.Summary.Counts.Sized)
			assert.Equal(t, 1, p.Summary.Counts.Refused)
			assert.Nil(t, p.Summary.PeakNeeded)
			terms := make([]string, 0, len(p.Refused))
			for _, x := range p.Refused {
				terms = append(terms, x.Term+":"+x.Reason)
				assert.Equal(t, 1, x.Buckets, x.Term)
				assert.NotContains(t, x.Hint, "--tpot none", "asking no TPOT does not price the knee")
			}
			want := []string{"decodeKnee:PinnedDefaultB"}
			if tpot == nil {
				want = []string{"tpot:PinnedDefaultB", "decodeKnee:PinnedDefaultB"}
			}
			assert.Equal(t, want, terms)
			require.NotEmpty(t, p.Refused)
			knee := p.Refused[len(p.Refused)-1]
			assert.Contains(t, knee.Effect, "the decode knee is not priced")
		})
	}
}

// A decode knee refused for any reason names that reason and never sends the
// operator to --tpot none, which prices no knee; only a missing or pinned
// line is called a line. TPOT refused for a reason of its own still may.
func TestDecodeKneeHintIsReasonAgnostic(t *testing.T) {
	for _, r := range []whatif.Reason{whatif.NoLine, whatif.PinnedDefaultB, whatif.NoCapacity, whatif.NoBatchCap,
		whatif.NoTraffic, whatif.InvalidInput, whatif.NoDecodeReplicas} {
		h := refusalHint(termDecodeKnee, r)
		assert.Contains(t, h, string(r))
		assert.NotContains(t, h, "--tpot none", r)
		line := r == whatif.NoLine || r == whatif.PinnedDefaultB
		assert.Equal(t, line, strings.Contains(h, "ITL line"), r)
	}
	assert.Contains(t, refusalHint(termTPOT, whatif.NoTPOTHistogram), "--tpot none")
}

// A refusal that holds at more replicas than one, which the probe at one a
// role does not see, is still the source's: the buckets are refused and it
// lands in Refused, not as a bucket beyond the bound. The stub refuses the
// prefill limit with NoImbalance from 2 prefill replicas on, as a pricer that
// left the imbalance f unlearned would; 6 r/s needs 2.
func TestDayPlanRefusalAtMoreReplicas(t *testing.T) {
	p := stubDay(t, &stubResolver{refuseFrom2: whatif.NoImbalance}, 20, 6, FleetCounts{Prefill: 2, Decode: 2})
	assert.Zero(t, p.Summary.Counts.Unmet, "no rate is beyond the bound")
	assert.Equal(t, 1, p.Summary.Counts.Refused)
	require.Len(t, p.Refused, 1)
	assert.Equal(t, termFleet, p.Refused[0].Term)
	assert.Equal(t, string(whatif.NoImbalance), p.Refused[0].Reason)
	assert.Equal(t, 1, p.Refused[0].Buckets)
	assert.Equal(t, BucketRefused, p.Buckets[0].Status)

	// The same at 4 r/s, which one prefill replica serves: sized.
	low := stubDay(t, &stubResolver{refuseFrom2: whatif.NoImbalance}, 20, 4, FleetCounts{Prefill: 2, Decode: 2})
	assert.Equal(t, 1, low.Summary.Counts.Sized)
	assert.Empty(t, low.Refused)
}

// A bucket is sized for its p95 from 20 cycles on; of 19 it is sized for its
// mean, flagged, since a nearest-rank p95 of 19 is the highest cycle.
func TestDayPlanP95NeedsTwentyCycles(t *testing.T) {
	q := &dayQuestion{sizeFor: SizeForP95}
	r := DayRate{Mean: 5, P95: 9, Max: 10}
	stat, rate := q.sizedRate(r, MinPercentileCycles)
	assert.Equal(t, SizeForP95, stat)
	assert.Equal(t, 9.0, rate)
	stat, rate = q.sizedRate(r, MinPercentileCycles-1)
	assert.Equal(t, SizeForMean, stat)
	assert.Equal(t, 5.0, rate)
	assert.Equal(t, 20, MinPercentileCycles, "the docs, the CLI help and the page say 20")
	// Through a plan: 19 cycles of one bucket.
	p := stubDay(t, &stubResolver{}, 19, 3, FleetCounts{Prefill: 1, Decode: 1})
	assert.Equal(t, SizeForMean, p.Buckets[0].SizedFor)
	assert.Contains(t, p.Buckets[0].Flags, FlagFewCyclesForP95)
	p = stubDay(t, &stubResolver{}, 20, 3, FleetCounts{Prefill: 1, Decode: 1})
	assert.Equal(t, SizeForP95, p.Buckets[0].SizedFor)
	assert.NotContains(t, p.Buckets[0].Flags, FlagFewCyclesForP95)
}

// An hour is under when any bucket is under or beyond the bound, refused
// when every bucket with data is refused, and over only when every sized
// bucket is over and none is unmet.
func TestHourVerdicts(t *testing.T) {
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	a := &FleetCounts{2, 2}
	b := func(i int, status, verdict string) DayBucket {
		x := DayBucket{Start: t0.Add(time.Duration(i) * 5 * time.Minute), Status: status, Verdict: verdict, Actual: a}
		if status == BucketSized {
			x.Needed = &DayNeeded{Decode: 1}
		}
		return x
	}
	for name, c := range map[string]struct {
		buckets []DayBucket
		want    string
	}{
		"over":           {[]DayBucket{b(0, BucketSized, DayVerdictOver), b(1, BucketSized, DayVerdictOver)}, DayVerdictOver},
		"over and unmet": {[]DayBucket{b(0, BucketSized, DayVerdictOver), b(1, BucketUnmet, "")}, DayVerdictUnder},
		"all refused":    {[]DayBucket{b(0, BucketRefused, DayVerdictRefused), b(1, BucketRefused, DayVerdictRefused)}, DayVerdictRefused},
		"over and match": {[]DayBucket{b(0, BucketSized, DayVerdictOver), b(1, BucketSized, DayVerdictMatch)}, DayVerdictMatch},
		"too little":     {[]DayBucket{b(0, BucketTooLittleData, "")}, ""},
	} {
		h := hoursOf(nil, c.buckets)
		require.Len(t, h, 1, name)
		assert.Equal(t, c.want, h[0].Verdict, name)
	}
}

// A day plan refuses what it cannot answer before pricing anything.
func TestDayQuestionRefusals(t *testing.T) {
	doc := InputsResponse{Fleet: FleetInfo{Prefill: &RoleInfo{Variant: "p", Replicas: 1}, Decode: RoleInfo{Variant: "d", Replicas: 1}},
		Policy: PolicyInfo{Headroom: 0.85, TTFT: &PolicyObjective{Statistic: "p90", Seconds: 1}}}
	series := cycles(time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), 15*time.Second, 100, 1)
	e := &inputsEntry{id: "x", doc: doc, series: series}
	// 12.5 hours at a 15 s step is 3000 steps, and two more buckets: over
	// the cap; at 16 s, under it.
	t0 := series[0].At
	long := &inputsEntry{id: "y", doc: doc, series: []SeriesSample{{At: t0}, {At: t0.Add(750 * time.Minute)}}}
	for name, c := range map[string]struct {
		e    *inputsEntry
		req  DayPlanRequest
		want string
	}{
		"no series":   {&inputsEntry{doc: doc}, DayPlanRequest{}, "no cycles"},
		"no prefill":  {&inputsEntry{series: series}, DayPlanRequest{}, "no prefill variant"},
		"a rate":      {e, DayPlanRequest{Traffic: &Traffic{Classes: &Classes{}}}, "traffic.classes"},
		"a bad step":  {e, DayPlanRequest{Step: "1s"}, "step"},
		"a bad stat":  {e, DayPlanRequest{SizeFor: "p50"}, "sizeFor"},
		"15s":         {e, DayPlanRequest{Step: "15s"}, ""},
		"too many":    {long, DayPlanRequest{Step: "15s"}, "3000 a day plan sizes"},
		"just enough": {long, DayPlanRequest{Step: "16s"}, ""},
		"a bad floor": {e, DayPlanRequest{MinRequests: ptr(-1)}, "minRequests"},
	} {
		_, err := dayQuestionOf(c.e, c.req)
		if c.want == "" {
			assert.NoError(t, err, name)
			continue
		}
		require.Error(t, err, name)
		assert.True(t, IsInputError(err), name)
		assert.Contains(t, err.Error(), c.want, name)
	}
}

// The busiest sized bucket is refused while the window's largest gap borders
// it, and the next busiest takes its place; a gap away from it refuses
// nothing.
func TestRefuseGapAtPeak(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC)
	step := 5 * time.Minute
	sized := func(i int, rate float64) DayBucket {
		return DayBucket{Start: t0.Add(time.Duration(i) * step), SizedRate: &rate, Status: BucketSized, Verdict: DayVerdictMatch,
			Needed: &DayNeeded{Decode: int(rate)}}
	}
	// Cycles every 15 s over 12 buckets, none in buckets 4-5 (16:20-16:30):
	// the gap runs from bucket 3's last cycle to bucket 6's first.
	times := cyclesAt(t0, 12*20, func(i int) bool { return i >= 80 && i < 120 })
	plan := func(rates []float64) *DayPlanResponse {
		// A decode knee refused elsewhere counts none of the window's buckets.
		out := &DayPlanResponse{Refused: []DayRefusal{{Role: roleDecode, Term: termDecodeKnee, Reason: "NoLine"}}, Gaps: WindowGapsOf(times)}
		for i, r := range rates {
			if i == 4 || i == 5 {
				out.Buckets = append(out.Buckets, DayBucket{Start: t0.Add(time.Duration(i) * step), Status: BucketNoData})
				continue
			}
			out.Buckets = append(out.Buckets, sized(i, r))
		}
		q := &dayQuestion{step: step}
		q.refuseGapAtPeak(out)
		q.countRefused(out)
		return out
	}

	// Bucket 6 is the busiest and bucket 3 the next: both border the gap and
	// are refused; bucket 7 is then the busiest and stands.
	p := plan([]float64{1, 2, 3, 8, 0, 0, 9, 7, 5, 4, 3, 2})
	for _, i := range []int{3, 6} {
		b := p.Buckets[i]
		assert.Equal(t, BucketRefused, b.Status, "bucket %d", i)
		assert.Equal(t, DayVerdictRefused, b.Verdict)
		assert.Nil(t, b.Needed)
		assert.Contains(t, b.Flags, FlagWindowGapAtPeak)
	}
	assert.Equal(t, BucketSized, p.Buckets[7].Status)
	require.Len(t, p.Refused, 2)
	assert.Zero(t, p.Refused[0].Buckets, "a bucket refused for a gap is not the decode knee's")
	r := p.Refused[1]
	assert.Equal(t, ReasonWindowGapAtPeak, r.Reason)
	assert.Equal(t, termWindow, r.Term)
	assert.Equal(t, 2, r.Buckets)
	assert.Contains(t, r.Detail, "10m0s from 16:19 to 16:30Z")
	assert.NotEmpty(t, r.Hint)

	// The busiest bucket is away from the gap: nothing is refused.
	p = plan([]float64{1, 2, 3, 4, 0, 0, 5, 7, 9, 4, 3, 2})
	assert.Len(t, p.Refused, 1, "only the decode knee's")
	for _, b := range p.Buckets {
		assert.NotEqual(t, BucketRefused, b.Status)
	}
}
