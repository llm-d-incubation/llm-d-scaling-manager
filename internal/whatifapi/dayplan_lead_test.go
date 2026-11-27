package whatifapi

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A replica must be Ready when its bucket starts, so with a scale-up lead
// the plan holds, in a bucket, the replicas the buckets within the lead
// after it need. On the stub, 10:00 runs 2 r/s of 1000-token prompts and
// needs 1 prefill + 1 decode, 10:05 runs 2 r/s of 8000-token prompts and
// needs 4 + 1, 10:10 runs 2 r/s of 1000-token prompts again. With a 90 s
// prefill lead and none for decode, 10:00 holds the 4 prefill 10:05 needs;
// 10:05 and 10:10 hold their own. The need and the verdicts do not move.
func TestDayPlanLeadHoldsAhead(t *testing.T) {
	res, err := (&stubResolver{}).Resolve(t.Context(), InputsRequest{})
	require.NoError(t, err)
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	res.Series = append(append(shaped(t0, 2, 1000), shaped(t0.Add(5*time.Minute), 2, 8000)...), shaped(t0.Add(10*time.Minute), 2, 1000)...)
	plain, err := DayPlan(t.Context(), res, DayPlanRequest{}, "schema-v3")
	require.NoError(t, err)
	assert.Nil(t, plain.Lead, "the stub learned no start time and none was asked")
	assert.Nil(t, plain.Buckets[0].Held)
	assert.Nil(t, plain.Summary.HeldReplicaHours)

	p, err := DayPlan(t.Context(), res, DayPlanRequest{Lead: &DayLead{Prefill: ptr(90.0)}}, "schema-v3")
	require.NoError(t, err)
	require.NotNil(t, p.Lead)
	assert.Equal(t, RoleLead{Seconds: 90, Source: LeadFromRequest}, p.Lead.Prefill)
	assert.Equal(t, RoleLead{Source: LeadNone}, p.Lead.Decode)
	require.Len(t, p.Buckets, 3)
	for i, b := range p.Buckets {
		require.NotNil(t, b.Needed, "bucket %d", i)
		assert.Equal(t, *plain.Buckets[i].Needed, *b.Needed, "the need does not move")
		assert.Equal(t, plain.Buckets[i].Verdict, b.Verdict, "nor the verdict")
	}
	assert.Equal(t, DayNeeded{Prefill: ptrInt(1), Decode: 1}, *p.Buckets[0].Needed)
	assert.Equal(t, DayNeeded{Prefill: ptrInt(4), Decode: 1}, *p.Buckets[1].Needed)
	assert.Equal(t, DayNeeded{Prefill: ptrInt(4), Decode: 1}, *p.Buckets[0].Held, "10:00 starts 10:05's prefill")
	assert.Equal(t, DayNeeded{Prefill: ptrInt(4), Decode: 1}, *p.Buckets[1].Held)
	assert.Equal(t, DayNeeded{Prefill: ptrInt(1), Decode: 1}, *p.Buckets[2].Held, "nothing after 10:10")
	require.NotNil(t, p.Hours[0].Held)
	assert.Equal(t, DayNeeded{Prefill: ptrInt(4), Decode: 1}, *p.Hours[0].Held)

	// The held replica-hours: the need's, and the 3 prefill replicas 10:05
	// adds held 90 s longer.
	need, held := p.Summary.NeededReplicaHours, p.Summary.HeldReplicaHours
	require.NotNil(t, held)
	require.NotNil(t, held.Prefill)
	assert.InDelta(t, *need.Prefill+3*90.0/3600, *held.Prefill, 1e-9)
	assert.InDelta(t, need.Decode, held.Decode, 1e-9, "no decode lead")
	assert.Contains(t, p.Notes[0], "prefill 90 s asked, decode none known")
}

// noTPOT asks no TPOT target: one stub decode replica's limit is then 6 r/s
// at 1000-token prompts.
var noTPOT = &SLORequest{TPOT: &Objective{Statistic: statisticNone}}

// The roles' leads swapped: a decode lead and none for prefill. With no TPOT
// asked, 10:05 runs 6.5 r/s of 1000-token prompts, where both roles need
// more than at 10:00's 2 r/s; 10:00 holds 10:05's decode, and only its own
// prefill.
func TestDayPlanLeadHoldsDecodeAlone(t *testing.T) {
	res, err := (&stubResolver{}).Resolve(t.Context(), InputsRequest{})
	require.NoError(t, err)
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	res.Series = append(append(shaped(t0, 2, 1000), shaped(t0.Add(5*time.Minute), 6.5, 1000)...), shaped(t0.Add(10*time.Minute), 2, 1000)...)
	p, err := DayPlan(t.Context(), res, DayPlanRequest{SLO: noTPOT, Lead: &DayLead{Prefill: ptr(0.0), Decode: ptr(90.0)}}, "schema-v3")
	require.NoError(t, err)
	require.Len(t, p.Buckets, 3)
	n0, n1 := p.Buckets[0].Needed, p.Buckets[1].Needed
	require.NotNil(t, n0)
	require.NotNil(t, n1)
	require.NotNil(t, n0.Prefill)
	require.NotNil(t, n1.Prefill)
	require.Greater(t, n1.Decode, n0.Decode, "the premise: decode rises at 10:05")
	require.Greater(t, *n1.Prefill, *n0.Prefill, "the premise: prefill rises at 10:05 too")
	require.NotNil(t, p.Buckets[0].Held)
	assert.Equal(t, DayNeeded{Prefill: n0.Prefill, Decode: n1.Decode}, *p.Buckets[0].Held,
		"10:00 starts 10:05's decode, not its prefill")
	assert.Equal(t, *n1, *p.Buckets[1].Held)

	need, held := p.Summary.NeededReplicaHours, p.Summary.HeldReplicaHours
	require.NotNil(t, held)
	require.NotNil(t, held.Prefill)
	assert.InDelta(t, *need.Prefill, *held.Prefill, 1e-9, "no prefill lead")
	assert.InDelta(t, need.Decode+float64(n1.Decode-n0.Decode)*90.0/3600, held.Decode, 1e-9)
	assert.Contains(t, p.Notes[0], "decode 90 s asked")
}

// A lead of exactly one 300 s bucket holds a rise one bucket early; 400 s,
// past one bucket, holds it two buckets early. With no TPOT asked, 10:10
// runs 6.5 r/s of 1000-token prompts and needs more decode than the 2 r/s
// around it.
func TestDayPlanLeadAtAndPastOneBucket(t *testing.T) {
	res, err := (&stubResolver{}).Resolve(t.Context(), InputsRequest{})
	require.NoError(t, err)
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	rates := []float64{2, 2, 6.5, 2}
	s := make([]SeriesSample, 0, 20*len(rates))
	for i, rate := range rates {
		s = append(s, shaped(t0.Add(time.Duration(i)*5*time.Minute), rate, 1000)...)
	}
	res.Series = s
	held := func(lead float64) []int {
		p, err := DayPlan(t.Context(), res, DayPlanRequest{SLO: noTPOT, Lead: &DayLead{Decode: ptr(lead)}}, "schema-v3")
		require.NoError(t, err)
		require.Len(t, p.Buckets, 4)
		out := make([]int, 4)
		for k, b := range p.Buckets {
			require.NotNil(t, b.Held, "bucket %d", k)
			out[k] = b.Held.Decode
		}
		return out
	}
	p, err := DayPlan(t.Context(), res, DayPlanRequest{SLO: noTPOT}, "schema-v3")
	require.NoError(t, err)
	require.NotNil(t, p.Buckets[0].Needed)
	require.NotNil(t, p.Buckets[2].Needed)
	lo, hi := p.Buckets[0].Needed.Decode, p.Buckets[2].Needed.Decode
	require.Greater(t, hi, lo, "the premise: decode rises at 10:10")
	assert.Equal(t, []int{lo, hi, hi, lo}, held(300), "300 s: started at 10:05")
	assert.Equal(t, []int{hi, hi, hi, lo}, held(400), "400 s: started at 10:03:20, in 10:00")
	assert.Equal(t, []int{lo, hi, hi, lo}, held(299), "299 s: within 10:05")
}

// A role's lead is the one asked, else the start time the source learned,
// else none; a seed says so.
func TestDayPlanLeadOf(t *testing.T) {
	learned := &RoleInfo{StartSeconds: ptr(67.5)}
	seeded := &RoleInfo{StartSeconds: ptr(120.0), StartSeeded: true}
	asked := RoleLead{Seconds: 30, Source: LeadFromRequest}
	assert.Equal(t, asked, leadOf(asked, learned))
	assert.Equal(t, RoleLead{Seconds: 67.5, Source: LeadLearned}, leadOf(RoleLead{}, learned))
	assert.Equal(t, RoleLead{Seconds: 120, Source: LeadSeeded}, leadOf(RoleLead{}, seeded))
	assert.Equal(t, RoleLead{Source: LeadNone}, leadOf(RoleLead{}, &RoleInfo{}))
	assert.Equal(t, RoleLead{Source: LeadNone}, leadOf(RoleLead{}, nil))
	// Through a plan: the inputs' learned decode start.
	res, err := (&stubResolver{}).Resolve(t.Context(), InputsRequest{})
	require.NoError(t, err)
	res.Inputs.Fleet.Decode.StartSeconds = ptr(67.5)
	res.Series = shaped(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), 2, 1000)
	p, err := DayPlan(t.Context(), res, DayPlanRequest{}, "schema-v3")
	require.NoError(t, err)
	require.NotNil(t, p.Lead)
	assert.Equal(t, RoleLead{Seconds: 67.5, Source: LeadLearned}, p.Lead.Decode)
	assert.NotNil(t, p.Buckets[0].Held)
	// A lead of 0 asked overrides the learned one: nothing is held, and the
	// plan says it was asked.
	p, err = DayPlan(t.Context(), res, DayPlanRequest{Lead: &DayLead{Prefill: ptr(0.0), Decode: ptr(0.0)}}, "schema-v3")
	require.NoError(t, err)
	require.NotNil(t, p.Lead)
	assert.Equal(t, RoleLead{Source: LeadFromRequest}, p.Lead.Decode)
	assert.Nil(t, p.Buckets[0].Held)
	assert.Contains(t, p.Notes[0], "The lead asked is 0")
}

// A lead asked out of range is a usage error, before any source is read.
func TestCheckDayPlanLead(t *testing.T) {
	for _, l := range []DayLead{{Prefill: ptr(-1.0)}, {Decode: ptr(math.NaN())}, {Decode: ptr(maxDayPlanLead + 1)}} {
		err := CheckDayPlan(DayPlanRequest{Lead: &l})
		require.Error(t, err)
		assert.True(t, IsInputError(err))
		assert.Contains(t, err.Error(), "lead.")
	}
	require.NoError(t, CheckDayPlan(DayPlanRequest{Lead: &DayLead{Prefill: ptr(0.0), Decode: ptr(maxDayPlanLead)}}))
}

// heldHours integrates the held fleet: with no lead it is the need's own
// replica-hours; a rise is held one lead early, a fall is not.
func TestHeldHours(t *testing.T) {
	need := []float64{1, 4, 1}
	assert.InDelta(t, 6*300.0/3600, heldHours(need, 300, 0), 1e-12)
	assert.InDelta(t, (6*300.0+3*90)/3600, heldHours(need, 300, 90), 1e-12)
	// A lead past a whole bucket holds the rise from two buckets before.
	assert.InDelta(t, (1*200.0+4*1000)/3600, heldHours([]float64{1, 1, 4, 4}, 300, 400), 1e-12)
	assert.InDelta(t, 0.0, heldHours(nil, 300, 90), 1e-12)
}

// A bucket sized for the p95 less counting noise: a steady Poisson stream
// of 2 r/s, read as 1-minute rates, is sized under its p95; the bucket
// carries the figure and the count says so.
func TestDayPlanSizedForP95Modulation(t *testing.T) {
	res, err := (&stubResolver{}).Resolve(t.Context(), InputsRequest{})
	require.NoError(t, err)
	rng := rand.New(rand.NewPCG(3, 5))
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	s := shaped(t0, 2, 1000)
	for i := range s {
		n := 0
		for x := rng.ExpFloat64() / 2; x < 60; x += rng.ExpFloat64() / 2 {
			n++
		}
		s[i].Rate = float64(n) / 60
	}
	res.Series = s
	p, err := DayPlan(t.Context(), res, DayPlanRequest{SizeFor: SizeForP95Modulation}, "schema-v3")
	require.NoError(t, err)
	b := p.Buckets[0]
	assert.Equal(t, SizeForP95Modulation, b.SizedFor)
	require.NotNil(t, b.Rate.P95Modulation)
	// Twenty readings estimate the variance loosely, so part of the noise
	// stays in; the figure lies between the mean and the p95.
	assert.Less(t, *b.SizedRate, b.Rate.P95, "the counting noise is taken out")
	assert.GreaterOrEqual(t, *b.SizedRate, b.Rate.Mean)
	assert.Equal(t, *b.Rate.P95Modulation, *b.SizedRate)
	rates := make([]float64, len(s))
	for i, x := range s {
		rates[i] = x.Rate
	}
	assert.Equal(t, ModulationP95Every(rates, b.Rate.Mean, b.Rate.P95, 15), *b.SizedRate, "the bucket's own cycles 15 s apart, at its mean and p95")
	assert.Equal(t, 1, p.Summary.Counts.SizedForP95Modulation)
	assert.Zero(t, p.Summary.Counts.SizedForP95)

	// Of 19 cycles it is sized for its mean, flagged as the p95 is.
	res.Series = s[:19]
	p, err = DayPlan(t.Context(), res, DayPlanRequest{SizeFor: SizeForP95Modulation}, "schema-v3")
	require.NoError(t, err)
	assert.Equal(t, SizeForMean, p.Buckets[0].SizedFor)
	assert.Contains(t, p.Buckets[0].Flags, FlagFewCyclesForP95)

	// The p95 asked carries no p95Modulation.
	res.Series = s
	p, err = DayPlan(t.Context(), res, DayPlanRequest{SizeFor: SizeForP95}, "schema-v3")
	require.NoError(t, err)
	assert.Equal(t, SizeForP95, p.Buckets[0].SizedFor)
	assert.Nil(t, p.Buckets[0].Rate.P95Modulation)
}
