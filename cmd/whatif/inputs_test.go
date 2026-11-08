package main

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The inputs the fleet can be told: --arrival-cv2, --assume-prompt-cv, and the
// spread of output lengths the shape reads.

// ttftAt prices the worked example's fleet, its decode line included, under
// flags, at p90 at 8 r/s.
func ttftAt(t *testing.T, f answerFlags, set ...string) (*pricedFleet, whatif.TTFT) {
	t.Helper()
	st := workedState(t)
	f.assumeVP, f.prefill, f.decode, f.inputTokens, f.outputTokens = 30000, 1, 1, 1000, 1000
	f.set = map[string]bool{"assume-vp": true, "prefill": true, "decode": true, "assume-input-tokens": true, "assume-output-tokens": true}
	for _, name := range set {
		f.set[name] = true
	}
	variants, err := f.decodeVariants(st)
	require.NoError(t, err)
	fleets, err := f.ttftFleets(st, ttftQuestion{q: 0.9}, variants)
	require.NoError(t, err)
	require.Nil(t, fleets[0].refused)
	got := fleets[0].priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, 8, 0.9) })
	require.IsType(t, whatif.TTFT{}, got)
	return fleets[0], got.(whatif.TTFT)
}

// Ca^2 reaches the prefill queue: burstier arrivals never wait less, at the
// default cv too, and 1 is Poisson to the last bit, the flag's stated figure
// included. It is flagged and named, never silently a Poisson answer.
func TestArrivalCV2_ReachesThePrefillQueue(t *testing.T) {
	_, poisson := ttftAt(t, answerFlags{})
	_, one := ttftAt(t, answerFlags{arrivalCV2: 1}, "arrival-cv2")
	assert.Equal(t, poisson.Seconds, one.Seconds, "Ca^2 1 is Poisson")
	assert.Contains(t, one.Flags, assumedArrivalCV2)
	assert.NotContains(t, poisson.Flags, assumedArrivalCV2)

	pf, bursty := ttftAt(t, answerFlags{arrivalCV2: 8}, "arrival-cv2")
	_, smooth := ttftAt(t, answerFlags{arrivalCV2: 0.25}, "arrival-cv2")
	assert.Greater(t, bursty.Seconds, poisson.Seconds)
	assert.Less(t, smooth.Seconds, bursty.Seconds)
	assert.Equal(t, 8.0, pf.in.ArrivalCV2)
	assert.Equal(t, shapeAssumed, pf.in.ArrivalsSource)
	assert.Contains(t, pf.in.Assumed, "arrivals Ca^2 8 (--arrival-cv2)")
	assert.NotContains(t, pf.in.Assumed, "Poisson arrivals")
}

// The fleet that showed a burstier arrival priced better than Poisson at the
// default cv: 300 ms at p95 read 5.26 r/s at Poisson and 5.53 at Ca^2 1.0001, and
// p95 at 10 r/s read 396.0 ms and 361.5 ms. Kingman's decay outran Poisson's root.
// The figures are the first-come model's, where that was; the property holds
// on processor sharing too.
func TestArrivalCV2_TheWorkedFleetPricesNoBetterThanPoisson(t *testing.T) {
	rate := func(flags ...string) float64 {
		out, code := runCmd(t, slices.Concat(workedTTFT("slo-to-rate", "--ttft", "300ms@p95", "-o", "json"), flags)...)
		require.Equal(t, 0, code, out)
		for _, r := range decodeOutput(t, out).Roles {
			if r.Role == "fleet" {
				return r.Results.Rate.Answer.Value
			}
		}
		t.Fatal(out)
		return 0
	}
	p95 := func(flags ...string) float64 {
		out, code := runCmd(t, slices.Concat(workedTTFT("replicas-to-slo", "--ttft-percentile", "95", "--rate", "10", "-o", "json"), flags)...)
		require.Equal(t, 0, code, out)
		for _, r := range decodeOutput(t, out).Roles {
			if r.Role == rolePrefill {
				return r.Results.TTFT.Answer.Value
			}
		}
		t.Fatal(out)
		return 0
	}
	monotone := func(model string) {
		prevRate, prevWait := rate(), p95()
		for _, ca2 := range []string{"1.0001", "2.5", "3", "4", "8"} {
			gotRate, gotWait := rate("--arrival-cv2", ca2), p95("--arrival-cv2", ca2)
			assert.LessOrEqual(t, gotRate, prevRate*(1+1e-6), "%s, Ca^2 %s", model, ca2)
			assert.GreaterOrEqual(t, gotWait, prevWait*(1-1e-9), "%s, Ca^2 %s", model, ca2)
			prevRate, prevWait = gotRate, gotWait
		}
	}
	monotone("processor sharing")
	firstCome(t)
	near(t, "the Poisson rate", rate(), 5.26, 0.005)
	near(t, "the Poisson p95", p95(), 0.3960, 0.00005)
	monotone("first come")
}

// Decode's admission is a birth-death chain with no Ca^2: on a decode line
// Poisson stands there, flagged and named, whatever the flag says. With no
// line priced nothing rests on Poisson any more, and the flag goes.
func TestArrivalCV2_DecodeAdmissionStaysPoisson(t *testing.T) {
	pf, got := ttftAt(t, answerFlags{arrivalCV2: 4}, "arrival-cv2")
	require.NotNil(t, pf.fleet.Decode)
	assert.Contains(t, got.Flags, assumedPoisson)
	assert.Contains(t, got.Flags, assumedArrivalCV2)
	assert.Contains(t, pf.in.Assumed, "Poisson arrivals at decode admission (--arrival-cv2 does not reach it)")

	alone := fleetOf(t, learnedState(), answerFlags{arrivalCV2: 4}, "arrival-cv2")
	require.Nil(t, alone.fleet.Decode)
	assert.NotContains(t, alone.flags, assumedPoisson)
	assert.Contains(t, alone.flags, assumedArrivalCV2)
	assert.Equal(t, 4.0, alone.fleet.Prefill.ArrivalCV2)
	for _, a := range alone.in.Assumed {
		assert.NotContains(t, a, "Poisson")
	}
}

// --assume-prompt-cv overrides whatever the histogram says, flagged, with the
// read figure beside it, as --assume-vp does; with no histogram it stands in
// place of the default 2, and 2 is the default's answer.
func TestPromptCV_TheFlagOverrides(t *testing.T) {
	t.Run("no histogram", func(t *testing.T) {
		pf := fleetOf(t, learnedState(), answerFlags{assumePromptCV: 0.5}, "assume-prompt-cv")
		assert.Equal(t, 0.5, pf.fleet.Prefill.PromptCV)
		assert.True(t, pf.fleet.Prefill.PromptCVAssumed)
		assert.Equal(t, shapeAssumed, pf.in.PromptCVSource)
		assert.Nil(t, pf.in.PromptCVRead)
		assert.Contains(t, pf.in.Assumed, "prompt cv 0.5 (--assume-prompt-cv)")
		assert.NotContains(t, strings.Join(pf.in.Assumed, " "), "no computed-prompt histogram")
	})
	t.Run("a measured spread", func(t *testing.T) {
		st := learnedState()
		st.Prefill[0].PromptWindow = measuredWindow()
		read := fleetOf(t, st, answerFlags{})
		require.False(t, read.fleet.Prefill.PromptCVAssumed, "the fixture must read a spread")
		pf := fleetOf(t, st, answerFlags{assumePromptCV: 1.5}, "assume-prompt-cv")
		assert.Equal(t, 1.5, pf.fleet.Prefill.PromptCV)
		assert.True(t, pf.fleet.Prefill.PromptCVAssumed)
		require.NotNil(t, pf.in.PromptCVRead)
		assert.Equal(t, read.fleet.Prefill.PromptCV, *pf.in.PromptCVRead)
		assert.Contains(t, strings.Join(pf.in.Assumed, " "), "(--assume-prompt-cv, read ")
	})
	t.Run("0 is a constant prompt, and is asked", func(t *testing.T) {
		pf := fleetOf(t, learnedState(), answerFlags{}, "assume-prompt-cv")
		assert.Zero(t, pf.fleet.Prefill.PromptCV)
		assert.True(t, pf.fleet.Prefill.PromptCVAssumed)
	})
	t.Run("the price moves, and 2 is the default", func(t *testing.T) {
		_, def := ttftAt(t, answerFlags{})
		pf, two := ttftAt(t, answerFlags{assumePromptCV: 2}, "assume-prompt-cv")
		assert.Equal(t, def.Seconds, two.Seconds)
		assert.Contains(t, two.Flags, whatif.AssumedPromptSpread)
		assert.Contains(t, pf.in.Assumed, "prompt cv 2 (--assume-prompt-cv)")
		_, one := ttftAt(t, answerFlags{assumePromptCV: 1}, "assume-prompt-cv")
		assert.Less(t, one.Seconds, def.Seconds)
		assert.Contains(t, one.Flags, whatif.AssumedPromptSpread)
	})
	t.Run("a cv above the peak is flagged", func(t *testing.T) {
		_, wide := ttftAt(t, answerFlags{assumePromptCV: 3}, "assume-prompt-cv")
		assert.Contains(t, wide.Flags, promptSpreadAbovePeak)
	})
}

// The frozen table's assumed row was read at cv 2, so the note may quote it at
// cv 2 alone: any other assumed figure has only the sent spread's row, and a
// note that said "at the assumed prompt cv 2" beside cv 1 would name a figure
// the answer was not priced at.
func TestPromptCV_TheNoteQuotesTheTableAtItsOwnCV(t *testing.T) {
	assumedAt := func(cv float64) *pricedFleet {
		pf := &pricedFleet{statedCV: true}
		pf.fleet.Prefill.PromptCV, pf.fleet.Prefill.PromptCVAssumed, pf.in.PromptCV = cv, true, cv
		return pf
	}
	assert.Contains(t, footerNote([]*pricedFleet{assumedAt(whatif.AssumedPromptCV)}), "at the assumed prompt cv 2")
	got := footerNote([]*pricedFleet{assumedAt(1)})
	assert.NotContains(t, got, "4.1x")
	assert.Contains(t, got, "at the sent spread (cv 0.06) it read up to 33%")

	// Both rows were read at Poisson arrivals: a stated Ca^2 quotes neither.
	bursty := assumedAt(whatif.AssumedPromptCV)
	bursty.in.ArrivalCV2 = 4
	note := footerNote([]*pricedFleet{bursty})
	assert.NotContains(t, note, "4.1x")
	assert.NotContains(t, note, "33%")
	assert.Contains(t, note, "no recording has checked it at a stated Ca^2")
}

// A fleet refused before pricing still says what the flags stated, in its
// assumed row, its flags and its note: none of it may read as dropped, nor quote
// the row of the cv the flag replaced. Run 4 refuses V_P with no flag for it.
func TestInputs_ARefusedFleetKeepsTheStatedInputs(t *testing.T) {
	args := []string{"replicas-to-slo", "--recording", shortRun, "--assume-prompt-cv", "1", "-o", "json"}
	out, code := runCmd(t, args...)
	require.Equal(t, 3, code, out)
	w := decodeOutput(t, out)
	assert.NotContains(t, w.Note, "at the assumed prompt cv 2")
	assert.Contains(t, out, "prompt cv 1 (--assume-prompt-cv)")
	assert.Contains(t, out, string(whatif.AssumedPromptSpread))

	out, code = runCmd(t, "replicas-to-slo", "--recording", shortRun, "--arrival-cv2", "4", "-o", "json")
	require.Equal(t, 3, code, out)
	w = decodeOutput(t, out)
	assert.Contains(t, w.Note, "no recording has checked it at a stated Ca^2")
	assert.Contains(t, out, "arrivals Ca^2 4 (--arrival-cv2)")
	assert.Contains(t, out, string(assumedArrivalCV2))
}

// A floor refusal keeps what the floor rests on: the stated cv sets I_q, and
// above the peak a larger cv prices a shorter floor. Ca^2 sets no floor.
func TestInputs_AFloorRefusalKeepsItsFlags(t *testing.T) {
	asked := workedTTFT("slo-to-rate", "--ttft", "1ms@p90", "--assume-prompt-cv", "3", "--arrival-cv2", "2", "-o", "json")
	out, code := runCmd(t, asked...)
	require.Equal(t, 3, code, out)
	assert.Contains(t, out, "TargetBelowFloor")
	var refused *wireRefusal
	for _, r := range decodeOutput(t, out).Roles {
		if r.Results.Rate != nil && r.Results.Rate.Refused != nil {
			refused = r.Results.Rate.Refused
			break
		}
	}
	require.NotNil(t, refused, out)
	assert.Contains(t, refused.Flags, string(whatif.AssumedPromptSpread))
	assert.Contains(t, refused.Flags, string(promptSpreadAbovePeak))
	assert.Contains(t, refused.Flags, string(whatif.AssumedVP))
	assert.NotContains(t, refused.Flags, string(assumedArrivalCV2))
}

// The page's figures for the worked question at the inputs it can now state.
func TestInputs_TheWorkedExampleAtTheStatedInputs(t *testing.T) {
	asked := workedTTFT("slo-to-rate", "--tpot", "4ms", "--ttft", "200ms@p95")
	for _, c := range []struct {
		flags []string
		want  string
	}{
		{nil, "fleet    0.25 r/s"},
		{[]string{"--assume-prompt-cv", "1"}, "fleet    9.64 r/s"},
		{[]string{"--assume-prompt-cv", "1", "--arrival-cv2", "4"}, "fleet    4.21 r/s"},
	} {
		text, code := runCmd(t, slices.Concat(asked, c.flags)...)
		assert.Equal(t, 0, code, text)
		assert.Contains(t, text, c.want, "%v", c.flags)
	}
}

// The two flags, refused before any replay, on both questions and both exit
// paths. Ca^2 0 is refused and not read: P3 reads it as Poisson, and a flag
// asking for evenly spaced arrivals would be priced as Poisson with no word.
func TestInputs_UsageErrors(t *testing.T) {
	q2 := []string{"replicas-to-slo", "--recording", shortRun}
	q1 := []string{"slo-to-rate", "--recording", shortRun, "--ttft", "1s"}
	for _, c := range []struct {
		flag, value, want string
	}{
		{"--arrival-cv2", "0", "not as evenly spaced"},
		{"--arrival-cv2", "-1", "--arrival-cv2"},
		{"--arrival-cv2", "NaN", "--arrival-cv2"},
		{"--arrival-cv2", "Inf", "--arrival-cv2"},
		{"--assume-prompt-cv", "-0.1", "--assume-prompt-cv"},
		{"--assume-prompt-cv", "3.1", "from 0 to 3"},
		{"--assume-prompt-cv", "NaN", "--assume-prompt-cv"},
		{"--assume-prompt-cv", "Inf", "--assume-prompt-cv"},
	} {
		for _, q := range [][]string{q2, q1} {
			t.Run(q[0]+" "+c.flag+" "+c.value, func(t *testing.T) {
				args := slices.Concat(q, []string{c.flag, c.value})
				err := run(context.Background(), args, io.Discard, io.Discard)
				require.Error(t, err)
				assert.Equal(t, 2, exitCode(err), "%v", err)
				assert.Contains(t, err.Error(), c.want)
			})
		}
	}
	for _, flag := range [][]string{{"--arrival-cv2", "2"}, {"--assume-prompt-cv", "1"}} {
		args := slices.Concat([]string{"slo-to-rate", "--recording", shortRun, "--tpot", "4ms"}, flag)
		err := run(context.Background(), args, io.Discard, io.Discard)
		require.Error(t, err, "%v", flag)
		assert.Contains(t, err.Error(), "need a --ttft target", "%v", flag)
	}
}

// outputSpreadRun is a committed recording whose decode rows read the
// output-length histogram, with a line to price it on.
const outputSpreadRun = "../../internal/engines/analyzers/learnreplay/testdata/goldens/throughput-sweep/recording.jsonl.gz"

// A wider spread only ever lowers a rate and raises a TPOT, at every D and
// wherever the batch cap or the band binds: the planner errs toward more
// replicas, never fewer. D 1 is the shape as it was priced before ev-shindin/llm-scaling-manager#276. A
// refusal promises nothing: a rate of 0, a TPOT with no bound.
func TestOutputSpread_OnlyAsksForMoreReplicas(t *testing.T) {
	price := func(d, outputTokens float64, cap int64) (knee, tpotRate, tpot float64) {
		st := workedState(t)
		st.Variants[0].Shape.OutputDispersion = d
		f := answerFlags{decode: 1, inputTokens: 1000, outputTokens: outputTokens, batchCap: cap, set: map[string]bool{
			"decode": true, "assume-input-tokens": true, "assume-output-tokens": true, "batch-cap": true}}
		variants, err := f.decodeVariants(st)
		require.NoError(t, err)
		dec := variants[0].decode
		if k, ok := whatif.KneeRate(dec).(whatif.Answer); ok {
			knee = k.Value
		}
		if r, ok := whatif.RateForTPOT(dec, whatif.RateQuery{Target: whatif.TPOTTarget{Seconds: 0.006}, Headroom: 0.85}).(whatif.Answer); ok {
			tpotRate = r.Value
		}
		tpot = math.Inf(1)
		if at, ok := whatif.SLOAtReplicas(dec, whatif.ReplicasQuery{Rate: 1}).MeanTPOT.(whatif.Answer); ok {
			tpot = at.Value
		}
		return knee, tpotRate, tpot
	}
	for _, shape := range []struct {
		ol  float64
		cap int64
	}{{1000, 0}, {1000, 64}, {4000, 0}, {250, 0}} {
		kneePrev, ratePrev, tpotPrev := price(1, shape.ol, shape.cap)
		first := kneePrev
		require.Positive(t, first, "OL %g cap %d: D 1 must answer", shape.ol, shape.cap)
		for _, d := range []float64{1.07, 1.2, 1.44, 2, 4} {
			knee, rate, tpot := price(d, shape.ol, shape.cap)
			assert.LessOrEqual(t, knee, kneePrev, "OL %g cap %d D %g", shape.ol, shape.cap, d)
			assert.LessOrEqual(t, rate, ratePrev, "OL %g cap %d D %g", shape.ol, shape.cap, d)
			assert.GreaterOrEqual(t, tpot, tpotPrev, "OL %g cap %d D %g", shape.ol, shape.cap, d)
			kneePrev, ratePrev, tpotPrev = knee, rate, tpot
		}
		assert.Less(t, kneePrev, first, "OL %g cap %d: D must move the knee", shape.ol, shape.cap)
	}
}

// D reaches the KV per request as ILeff + D*OL/2, an assumed length included:
// a spread is a figure of the traffic, whatever length is asked. No reading is
// flagged and not called D 1 in the JSON.
func TestOutputSpread_ReachesTheKVPerRequest(t *testing.T) {
	st := workedState(t)
	st.Variants[0].Shape.OutputDispersion = 1.5
	f := answerFlags{inputTokens: 1000, outputTokens: 400, set: map[string]bool{"assume-input-tokens": true, "assume-output-tokens": true}}
	variants, err := f.decodeVariants(st)
	require.NoError(t, err)
	pv := variants[0]
	near(t, "KVreq", pv.decode.KVPerRequest, pv.shape.ILeff+1.5*400/2, 1e-9)
	assert.NotContains(t, pv.flags, whatif.OutputSpreadUnread)
	assert.Contains(t, pv.shapeText(), "output spread D 1.500 (measured)")
	assert.Equal(t, 1.5, inputsOf(&f, pv).Shape.OutputDispersion)

	st.Variants[0].Shape.OutputDispersion = 0
	variants, err = f.decodeVariants(st)
	require.NoError(t, err)
	pv = variants[0]
	near(t, "KVreq", pv.decode.KVPerRequest, pv.shape.ILeff+400.0/2, 1e-9)
	assert.Contains(t, pv.flags, whatif.OutputSpreadUnread)
	assert.NotContains(t, pv.shapeText(), "output spread")
	assert.Zero(t, inputsOf(&f, pv).Shape.OutputDispersion)
}

// The real path: a recording whose rows read the histogram prices the spread
// it read, and the JSON and the text carry it. The figure is read back, and
// the KV per request is held to the formula.
func TestOutputSpread_FromARecording(t *testing.T) {
	out, code := runCmd(t, "replicas-to-slo", "--recording", outputSpreadRun, "-o", "json")
	require.Equal(t, 0, code, out)
	var w wireOutput
	require.NoError(t, json.Unmarshal([]byte(out), &w), out)
	priced := 0
	for _, r := range w.Roles {
		if r.Inputs == nil {
			continue
		}
		s := r.Inputs.Shape
		require.Greater(t, s.OutputDispersion, 1.0, "role %s must read a spread", r.Role)
		near(t, "KVreq", r.Inputs.KVPerRequest, s.InputTokens*(1-s.PrefixHitRate)+s.OutputDispersion*s.OutputTokens/2, 1e-6)
		priced++
	}
	require.Positive(t, priced, out)
	text, _ := runCmd(t, "replicas-to-slo", "--recording", outputSpreadRun)
	assert.Contains(t, text, "output spread D ")
	assert.NotContains(t, text, string(whatif.OutputSpreadUnread))
}

// With no reading the limit is said in words, on the decode line's flags and on
// the assumed row of a fleet priced on the line.
func TestOutputSpread_NoReadingIsNamedInWords(t *testing.T) {
	text, code := runCmd(t, "replicas-to-slo", "--recording", olsSweep)
	require.Equal(t, 0, code, text)
	assert.Contains(t, text, "OutputSpreadUnread (no output-length histogram: KVreq is ILeff + OL/2, which over-promises by 7-44% on a real spread)")
	pf, _ := ttftAt(t, answerFlags{})
	assert.Contains(t, pf.in.Assumed, outputSpreadUnreadWords)
}
