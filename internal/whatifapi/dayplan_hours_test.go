package whatifapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shaped is 20 cycles, a bucket, every 15 s from t0 at rate, each of prompt
// tokens.
func shaped(t0 time.Time, rate, prompt float64) []SeriesSample {
	out := make([]SeriesSample, 20)
	for i := range out {
		out[i] = SeriesSample{At: t0.Add(time.Duration(i) * 15 * time.Second), Rate: rate, RateKnown: true, Prefill: 2, Decode: 2,
			PromptTokens: prompt, OutputTokens: 200, HasShape: true}
	}
	return out
}

// An hour's need holds every bucket in it, not only its busiest. On the
// stub (prefill knee 10 r/s a replica at 1000-token prompts, 1.25 at 8000),
// 10:00 runs 2 r/s of 8000-token prompts and needs 4 prefill and 1 decode;
// 10:05 runs 5.5 r/s of 1000-token prompts, the busier bucket, and needs 2
// and 1. The busiest bucket's fleet, 2+1, does not serve 10:00: the hour
// and the peak need 4+1, and say the busiest bucket's 2+1 beside it.
func TestDayPlanHourHoldsEveryBucket(t *testing.T) {
	res, err := (&stubResolver{}).Resolve(t.Context(), InputsRequest{})
	require.NoError(t, err)
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	res.Series = append(shaped(t0, 2, 8000), shaped(t0.Add(5*time.Minute), 5.5, 1000)...)
	p, err := DayPlan(t.Context(), res, DayPlanRequest{}, "schema-v3")
	require.NoError(t, err)
	require.Len(t, p.Buckets, 2)
	for _, b := range p.Buckets {
		require.Equal(t, BucketSized, b.Status, "%s", b.Start)
	}
	assert.Equal(t, DayNeeded{Prefill: ptrInt(4), Decode: 1}, *p.Buckets[0].Needed, "long prompts at 2 r/s")
	assert.Equal(t, DayNeeded{Prefill: ptrInt(2), Decode: 1}, *p.Buckets[1].Needed, "short prompts at 5.5 r/s")

	require.Len(t, p.Hours, 1)
	h := p.Hours[0]
	require.NotNil(t, h.Needed)
	assert.Equal(t, DayNeeded{Prefill: ptrInt(4), Decode: 1}, *h.Needed, "the hour holds 10:00 too")
	require.NotNil(t, h.Busiest)
	assert.Equal(t, DayNeeded{Prefill: ptrInt(2), Decode: 1}, *h.Busiest, "the busiest bucket alone")
	assert.False(t, h.NotConfirmed, "4+1 priced again serves both buckets")

	s := p.Summary
	require.NotNil(t, s.PeakNeeded)
	assert.Equal(t, DayNeeded{Prefill: ptrInt(4), Decode: 1}, *s.PeakNeeded)
	assert.Equal(t, DayNeeded{Prefill: ptrInt(2), Decode: 1}, *s.BusiestNeeded)
	assert.False(t, s.PeakNotConfirmed)
}

// Each role's most, over buckets of which some sized decode alone: the
// prefill the priced ones needed, and the whole a lower bound.
func TestHoldingFleet(t *testing.T) {
	b := func(n DayNeeded) *DayBucket { return &DayBucket{Needed: &n} }
	assert.Nil(t, holdingFleet([]*DayBucket{{}}), "no bucket sized")
	assert.Equal(t, &DayNeeded{Prefill: ptrInt(4), Decode: 3},
		holdingFleet([]*DayBucket{b(DayNeeded{Prefill: ptrInt(4), Decode: 2}), b(DayNeeded{Prefill: ptrInt(1), Decode: 3})}))
	assert.Equal(t, &DayNeeded{Prefill: ptrInt(4), Decode: 4, AtLeast: true},
		holdingFleet([]*DayBucket{b(DayNeeded{Prefill: ptrInt(4), Decode: 2}), b(DayNeeded{Decode: 4, AtLeast: true})}))
	assert.Equal(t, &DayNeeded{Decode: 4, AtLeast: true},
		holdingFleet([]*DayBucket{b(DayNeeded{Decode: 2, AtLeast: true}), b(DayNeeded{Decode: 4, AtLeast: true})}))
}

// The need that holds an hour is priced again at each bucket: where a fleet
// limit falls as replicas are added (the stub's dip, at 4 prefill and 2
// decode), each role's most falls short, and the hour and the peak say so.
// With no TPOT asked, 10:00 (2 r/s of 8000-token prompts) needs 4+1, and
// 10:05 (6.5 r/s of 1000-token prompts; one decode replica's limit is 6 r/s
// there) needs 2+2: each role's most is 4+2, the dip.
func TestDayPlanHourNeedNotConfirmed(t *testing.T) {
	r := &stubResolver{dip: func(p, d int) bool { return p == 4 && d == 2 }}
	res, err := r.Resolve(t.Context(), InputsRequest{})
	require.NoError(t, err)
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	res.Series = append(shaped(t0, 2, 8000), shaped(t0.Add(5*time.Minute), 6.5, 1000)...)
	p, err := DayPlan(t.Context(), res, DayPlanRequest{SLO: &SLORequest{TPOT: &Objective{Statistic: statisticNone}}}, "schema-v3")
	require.NoError(t, err)
	require.Len(t, p.Buckets, 2)
	require.NotNil(t, p.Buckets[0].Needed)
	require.NotNil(t, p.Buckets[1].Needed)
	assert.Equal(t, DayNeeded{Prefill: ptrInt(4), Decode: 1}, *p.Buckets[0].Needed)
	assert.Equal(t, DayNeeded{Prefill: ptrInt(2), Decode: 2}, *p.Buckets[1].Needed)
	require.Len(t, p.Hours, 1)
	h := p.Hours[0]
	require.NotNil(t, h.Needed)
	assert.Equal(t, DayNeeded{Prefill: ptrInt(4), Decode: 2}, *h.Needed)
	assert.True(t, h.NotConfirmed, "4+2 dips: it does not serve 10:00")
	assert.True(t, p.Summary.PeakNotConfirmed)
}

// With no TTFT target, neither asked nor in the policy, a day plan sizes
// decode alone, as at-replicas judges decode alone, and says so: no
// refusal, prefill not sized, decode a lower bound.
func TestDayPlanNoTTFTTargetSizesDecodeAlone(t *testing.T) {
	p := stubDay(t, &stubResolver{noPolicyTTFT: true}, 20, 6, FleetCounts{Prefill: 1, Decode: 1})
	assert.Nil(t, p.TTFT, "no TTFT target to report")
	assert.Empty(t, p.Refused, "nothing asked was refused")
	require.Len(t, p.Buckets, 1)
	b := p.Buckets[0]
	require.Equal(t, BucketSized, b.Status)
	require.NotNil(t, b.Needed)
	assert.Nil(t, b.Needed.Prefill, "prefill is not sized")
	assert.True(t, b.Needed.AtLeast)
	assert.Equal(t, 1, b.Needed.Decode, "decode's TPOT limit, 7 r/s, holds 6 r/s at one replica")
	assert.Contains(t, b.Flags, FlagPrefillNotPriced)
	var said bool
	for _, n := range p.Notes {
		said = said || (len(n) > 15 && n[:15] == "No TTFT target:")
	}
	assert.True(t, said, "a note says why prefill is not sized: %q", p.Notes)
	assert.Contains(t, p.Notes, "Where prefill is not priced, decode is sized on its own limit alone: a lower bound (at least). "+
		"The fleet's limit, which decode admission can hold down, is not priced.")

	// A target asked prices prefill, policy or none.
	asked, err := DayPlanWith(t, &stubResolver{noPolicyTTFT: true}, DayPlanRequest{SLO: &SLORequest{TTFT: &Objective{Statistic: "p90", Target: ptr(0.2)}}})
	require.NoError(t, err)
	require.NotNil(t, asked.TTFT)
	require.NotNil(t, asked.Buckets[0].Needed)
	assert.NotNil(t, asked.Buckets[0].Needed.Prefill)
}

// DayPlanWith is a day plan of one bucket at 6 r/s on r, asked req.
func DayPlanWith(t *testing.T, r *stubResolver, req DayPlanRequest) (*DayPlanResponse, error) {
	t.Helper()
	res, err := r.Resolve(t.Context(), InputsRequest{})
	require.NoError(t, err)
	res.Series = shaped(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), 6, 1000)
	return DayPlan(t.Context(), res, req, "schema-v3")
}

// CheckDayPlan refuses what a day plan asks wrongly with no source at all,
// so that the CLI refuses it before a replay of minutes.
func TestCheckDayPlan(t *testing.T) {
	for name, c := range map[string]struct {
		req  DayPlanRequest
		want string
	}{
		"nothing":         {DayPlanRequest{}, ""},
		"TPOT to policy":  {DayPlanRequest{SLO: &SLORequest{TPOT: &Objective{Statistic: "mean"}}}, ""},
		"no TTFT, a TPOT": {DayPlanRequest{SLO: &SLORequest{TPOT: &Objective{Statistic: "mean", Target: ptr(0.03)}}}, ""},
		"a bad step":      {DayPlanRequest{Step: "1s"}, "step"},
		"a bad sizeFor":   {DayPlanRequest{SizeFor: "p50"}, "sizeFor"},
		"a bad floor":     {DayPlanRequest{MinRequests: ptr(-1)}, "minRequests"},
		"a bad bound":     {DayPlanRequest{MaxReplicas: &FleetKnob{Decode: ptrInt(0)}}, "maxReplicas.decode"},
		"a bad statistic": {DayPlanRequest{SLO: &SLORequest{TTFT: &Objective{Statistic: "p100", Target: ptr(1.0)}}}, "slo.ttft.statistic"},
		"a bad TPOT":      {DayPlanRequest{SLO: &SLORequest{TPOT: &Objective{Statistic: "p90x", Target: ptr(0.03)}}}, "slo.tpot.statistic"},
		"a bad headroom":  {DayPlanRequest{Headroom: ptr(1.5)}, "headroom"},
		"classes":         {DayPlanRequest{Traffic: &Traffic{Classes: &Classes{}}}, "traffic.classes"},
		"a bad mix":       {DayPlanRequest{Traffic: &Traffic{Mix: &Mix{PromptTokens: -1}}}, "traffic.mix.promptTokens"},
		// The terms a plan states in place of its source's (the self-check's
		// Stated) are checked before the replay too.
		"a bad V_P":        {DayPlanRequest{Assume: &Assume{VP: ptr(-1.0)}}, "assume.vp"},
		"a bad a":          {DayPlanRequest{Assume: &Assume{A: ptr(-1.0)}}, "assume.a"},
		"a bad arrivalCV2": {DayPlanRequest{Traffic: &Traffic{ArrivalCV2: ptr(-1.0)}}, "arrivalCV2"},
		"stated terms":     {DayPlanRequest{Assume: &Assume{VP: ptr(30000.0)}, Traffic: &Traffic{ArrivalCV2: ptr(2.0)}}, ""},
	} {
		err := CheckDayPlan(c.req)
		if c.want == "" {
			assert.NoError(t, err, name)
			continue
		}
		require.Error(t, err, name)
		assert.True(t, IsInputError(err), name)
		assert.Contains(t, err.Error(), c.want, name)
	}
}
