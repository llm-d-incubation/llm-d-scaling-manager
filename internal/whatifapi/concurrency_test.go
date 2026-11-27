package whatifapi

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi/apitest"
)

// queueW is a mean time in system that rises to the knee without bound, as
// a queue's does: 0.5 s at no load, 0.1 s / (1 - rate/knee) on top.
func queueW(knee float64) func(float64) (float64, bool) {
	return func(rate float64) (float64, bool) {
		if rate >= knee {
			return 0, false
		}
		return 0.5 + 0.1/(1-rate/knee), true
	}
}

// A concurrency read off a rate translates back into that rate.
func TestRateAtConcurrency_RoundTrip(t *testing.T) {
	const knee = 12.0
	w := queueW(knee)
	for _, rate := range []float64{0.01, 0.5, 3, 6, 9, 11, 11.9, 11.999} {
		v, ok := w(rate)
		require.True(t, ok)
		got, err := RateAtConcurrency(rate*v, knee, w)
		require.NoError(t, err)
		assert.False(t, got.AtKnee, "rate %g", rate)
		assert.InDelta(t, rate, got.Rate, 1e-5*knee, "rate %g -> L %g -> rate", rate, rate*v)
		assert.InDelta(t, rate*v, got.Concurrency, 0)
	}
}

// More requests in the system is more rate, never less.
func TestRateAtConcurrency_Monotone(t *testing.T) {
	const knee = 12.0
	w := queueW(knee)
	last := 0.0
	for _, l := range []float64{0.1, 1, 2, 4, 8, 16, 64, 512, 4096} {
		got, err := RateAtConcurrency(l, knee, w)
		require.NoError(t, err)
		assert.Greater(t, got.Rate, last, "L %g", l)
		assert.Less(t, got.Rate, knee, "L %g stays under the knee", l)
		last = got.Rate
	}
}

// A fleet whose time in system stays bounded to the knee holds at most
// knee x W requests: past that, the answer is the knee, flagged.
func TestRateAtConcurrency_PastWhatTheFleetHolds(t *testing.T) {
	const knee = 10.0
	flat := func(float64) (float64, bool) { return 2, true }
	got, err := RateAtConcurrency(50, knee, flat)
	require.NoError(t, err)
	assert.True(t, got.AtKnee)
	assert.InDelta(t, knee, got.Rate, 0, "the knee's rate")
	assert.InDelta(t, 50, got.Concurrency, 0, "the concurrency asked is kept")

	under, err := RateAtConcurrency(10, knee, flat)
	require.NoError(t, err)
	assert.False(t, under.AtKnee)
	assert.InDelta(t, 5, under.Rate, 1e-5)

	// A time in system refused short of the knee (a queue that saturates
	// first) ends the rates that price: the answer is that edge, flagged.
	edge := func(rate float64) (float64, bool) { return 2, rate < 8 }
	got, err = RateAtConcurrency(50, knee, edge)
	require.NoError(t, err)
	assert.True(t, got.AtKnee)
	assert.InDelta(t, 8, got.Rate, 1e-5)
}

// A time in system refused at every rate leaves nothing to translate with,
// and a concurrency or knee that is not a number above 0 is refused.
func TestRateAtConcurrency_Refused(t *testing.T) {
	never := func(float64) (float64, bool) { return 0, false }
	_, err := RateAtConcurrency(8, 10, never)
	require.Error(t, err)
	assert.True(t, ConcurrencyUnpriced(err))
	for _, c := range []struct{ l, knee float64 }{{0, 10}, {-1, 10}, {math.NaN(), 10}, {math.Inf(1), 10}, {8, 0}, {8, math.NaN()}, {8, math.Inf(1)}} {
		_, err := RateAtConcurrency(c.l, c.knee, queueW(10))
		assert.Error(t, err, "L %g knee %g", c.l, c.knee)
		assert.False(t, ConcurrencyUnpriced(err))
	}
}

// The concurrency at a rate is Little's law on the mean time in system, by
// role: prefill is the time in the prefill engine alone, the router's gate a
// term of its own; refused where a mean it rests on is, and unbounded past
// the knee or where a mean is refused as saturated.
func TestConcurrencyAt(t *testing.T) {
	// OwnPrefill 0.3 holds a, the transfer and the prompt's own work 0.15:
	// only the work and the engine's wait are on the prefill pod.
	ttft := whatif.TTFT{Seconds: 0.5, Parts: whatif.TTFTParts{EngineWait: 0.1, OwnPrefill: 0.3, PrefillWork: 0.15, DecodeWait: 0.1}}
	tpot := whatif.Answer{Value: 0.01}
	c := ConcurrencyAt(2, ttft, tpot, 101, 2, false)
	require.NotNil(t, c.Fleet.Value)
	assert.Equal(t, unitRequests, c.Fleet.Unit)
	assert.InDelta(t, 2*(0.5+100*0.01), *c.Fleet.Value, 1e-12, "rate x (TTFT + (OL-1) x TPOT)")
	require.NotNil(t, c.Prefill)
	assert.InDelta(t, 2*(0.1+0.15), *c.Prefill.Value, 1e-12, "rate x (prefill wait + the prompt's own work), not a or the transfer")
	assert.Nil(t, c.RouterQueue, "no gate priced")
	require.NotNil(t, c.Decode)
	assert.InDelta(t, 2*101*0.01, *c.Decode.Value, 1e-12, "rate x OL x TPOT")
	require.NotNil(t, c.DecodePerPod)
	assert.InDelta(t, 2*101*0.01/2, *c.DecodePerPod.Value, 1e-12)
	assert.False(t, c.Unbounded)

	// The router's gate holds requests outside every engine: a term of its
	// own, in the fleet and not in prefill. A tier load is the prefill
	// engine's.
	gated := ttft
	gated.Parts.RouterWait, gated.Parts.BurstWait, gated.Parts.TierLoad = 0.2, 0.05, 0.02
	c = ConcurrencyAt(2, gated, tpot, 101, 2, false)
	require.NotNil(t, c.RouterQueue)
	assert.InDelta(t, 2*0.25, *c.RouterQueue.Value, 1e-12)
	assert.InDelta(t, 2*(0.1+0.02+0.15), *c.Prefill.Value, 1e-12)

	// No prefill work: no prefill side to count.
	decodeOnly := whatif.TTFT{Seconds: 0.05, Parts: whatif.TTFTParts{DecodeEngine: 0.05}}
	c = ConcurrencyAt(2, decodeOnly, tpot, 101, 2, false)
	assert.Nil(t, c.Prefill)
	assert.NotNil(t, c.Fleet.Value)

	refusedTTFT := whatif.Unanswerable{Reason: whatif.NoPrefillCapacity, Detail: "no V_P"}
	c = ConcurrencyAt(2, refusedTTFT, tpot, 101, 2, false)
	assert.Nil(t, c.Fleet.Value)
	require.NotNil(t, c.Fleet.Refused)
	assert.Equal(t, string(whatif.NoPrefillCapacity), c.Fleet.Refused.Reason)
	assert.False(t, c.Unbounded)
	assert.Nil(t, c.Prefill, "no TTFT parts to read")
	assert.NotNil(t, c.Decode, "decode's own figure stands on TPOT alone")

	refusedTPOT := whatif.Unanswerable{Reason: whatif.NoLine, Detail: "no line"}
	c = ConcurrencyAt(2, ttft, refusedTPOT, 101, 2, false)
	require.NotNil(t, c.Fleet.Refused)
	assert.Equal(t, string(whatif.NoLine), c.Fleet.Refused.Reason)
	assert.Nil(t, c.Decode)
	assert.NotNil(t, c.Prefill)

	c = ConcurrencyAt(20, ttft, tpot, 101, 2, true)
	assert.True(t, c.Unbounded)
	assert.Nil(t, c.Fleet.Value, "no number past the knee")
	require.NotNil(t, c.Fleet.Refused)
	assert.Equal(t, string(whatif.Saturated), c.Fleet.Refused.Reason)

	// A queue that saturates short of the knee (the KV pull) refuses the
	// mean TTFT as saturated: requests queue without bound there too.
	for _, sat := range []struct{ ttft, tpot whatif.Result }{
		{whatif.Unanswerable{Reason: whatif.Saturated, Detail: "no finite mean TTFT"}, tpot},
		{ttft, whatif.Unanswerable{Reason: whatif.Saturated, Detail: "no decode fill"}},
	} {
		c = ConcurrencyAt(10, sat.ttft, sat.tpot, 101, 2, false)
		assert.True(t, c.Unbounded)
		require.NotNil(t, c.Fleet.Refused)
		assert.Equal(t, string(whatif.Saturated), c.Fleet.Refused.Reason)
		assert.Contains(t, c.Fleet.Refused.Detail, "without bound")
	}

	c = ConcurrencyAt(2, ttft, tpot, 0, 2, false)
	assert.Nil(t, c.Fleet.Value, "no output length, no time in system")
}

// A knee known only as a lower bound is not known to be where requests
// queue without bound: its concurrency is refused, not unbounded.
func TestLowerBoundKneeConcurrency(t *testing.T) {
	c := LowerBoundKneeConcurrency()
	assert.False(t, c.Unbounded)
	assert.Nil(t, c.Fleet.Value)
	require.NotNil(t, c.Fleet.Refused)
	assert.Equal(t, string(whatif.PastLowerBoundKnee), c.Fleet.Refused.Reason)
	assert.Contains(t, c.Fleet.Refused.Detail, "at least the knee")
}

// A load may be a concurrency: "64c".
func TestParseReportLoad_Concurrency(t *testing.T) {
	l, err := ParseReportLoad("64c")
	require.NoError(t, err)
	assert.InDelta(t, 64, l.Concurrency, 0)
	assert.Equal(t, "64c", l.Label)
	assert.Zero(t, l.Rate)
	assert.Zero(t, l.Share)
	l, err = ParseReportLoad("2.5c")
	require.NoError(t, err)
	assert.InDelta(t, 2.5, l.Concurrency, 0)
	for _, s := range []string{"c", "0c", "-4c", "NaNc", "Infc", "2000000c", "x64c"} {
		_, err := ParseReportLoad(s)
		assert.Error(t, err, s)
	}
}

// A plan asked a concurrency prices at the rate that holds it, says so, and
// reads that concurrency back at the rate.
func TestPlanAtConcurrency(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	traffic := func(cr ClassRate) *Traffic {
		return &Traffic{Classes: &Classes{Critical: &cr}, Mix: &Mix{OutputTokens: 100}}
	}
	for _, n := range []float64{0.5, 2, 6} {
		out, body := plan(t, h, PlanRequest{InputsID: id, Traffic: traffic(ClassRate{Concurrency: f64(n)})})
		assert.Empty(t, apitest.CheckFigures(body), "%s", body)
		ch := out.Limits.Chosen
		assert.Equal(t, RateSourceConcurrency, ch.RateSource)
		require.NotNil(t, ch.FromConcurrency)
		assert.False(t, ch.FromConcurrency.AtKnee, "%g requests", n)
		assert.InDelta(t, ch.Rate, ch.FromConcurrency.Rate, 0)
		require.NotNil(t, ch.Concurrency)
		require.NotNil(t, ch.Concurrency.Fleet.Value, "%+v", ch.Concurrency.Fleet.Refused)
		assert.InDelta(t, n, *ch.Concurrency.Fleet.Value, 1e-4*n, "the concurrency read back at the rate it gave")
		require.NotNil(t, out.Scenario.Concurrency)
		assert.Nil(t, out.Scenario.Rate)

		// The rate it gave, asked as a rate, reads the same concurrency.
		again, _ := plan(t, h, PlanRequest{InputsID: id, Traffic: traffic(ClassRate{Rate: f64(ch.Rate)})})
		assert.Equal(t, RateSourceRequest, again.Limits.Chosen.RateSource)
		assert.Nil(t, again.Limits.Chosen.FromConcurrency)
		require.NotNil(t, again.Limits.Chosen.Concurrency.Fleet.Value)
		assert.InDelta(t, n, *again.Limits.Chosen.Concurrency.Fleet.Value, 1e-4*n)
	}

	// The stub's TTFT is a queue's, which grows without bound at the knee:
	// any concurrency is reached just under it, never flagged past it.
	out, _ := plan(t, h, PlanRequest{InputsID: id, Traffic: traffic(ClassRate{Concurrency: f64(1e5)})})
	ch := out.Limits.Chosen
	require.NotNil(t, ch.FromConcurrency)
	assert.False(t, ch.FromConcurrency.AtKnee)
	assert.Less(t, ch.Rate, 10.0, "under the lower knee, 10 r/s")
	assert.Greater(t, ch.Rate, 9.99)
}

// Past the knee the plan names no concurrency, and says why.
func TestPlanConcurrencyPastTheKnee(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	out, body := plan(t, h, PlanRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(11)}}, Mix: &Mix{OutputTokens: 100}}})
	assert.Empty(t, apitest.CheckFigures(body), "%s", body)
	c := out.Limits.Chosen.Concurrency
	require.NotNil(t, c)
	assert.True(t, c.Unbounded)
	assert.Nil(t, c.Fleet.Value)
}

// A concurrency is refused beside a rate, past its bound, on a class v1
// does not price, with no output length to price at, and by a size, whose
// fleet is the answer.
func TestConcurrencyRequestsRefused(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	out := &Mix{OutputTokens: 100}
	for name, tr := range map[string]*Traffic{
		"rate and concurrency":  {Classes: &Classes{Critical: &ClassRate{Rate: f64(2), Concurrency: f64(4)}}, Mix: out},
		"zero":                  {Classes: &Classes{Critical: &ClassRate{Concurrency: f64(0)}}, Mix: out},
		"past the bound":        {Classes: &Classes{Critical: &ClassRate{Concurrency: f64(2e6)}}, Mix: out},
		"a regular concurrency": {Classes: &Classes{Regular: &ClassRate{Concurrency: f64(4)}}, Mix: out},
		"no output length":      {Classes: &Classes{Critical: &ClassRate{Concurrency: f64(4)}}},
	} {
		code, body := post(t, h, "/v1/plan", PlanRequest{InputsID: id, Traffic: tr})
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", name, body)
	}
	code, body := post(t, h, "/v1/size", SizeRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Concurrency: f64(4)}}}})
	assert.Equal(t, http.StatusBadRequest, code, "%s", body)
	var e ErrorResponse
	require.NoError(t, json.Unmarshal(body, &e))
	assert.Contains(t, e.Error, "a size needs a rate")
}
