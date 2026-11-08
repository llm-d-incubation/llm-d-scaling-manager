package learnreplay

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The TTFT validation of P2 (docs/proposals/slo-capacity-planner-ttft.md,
// "As built (P2)"): P3's TTFT model, whatif.TTFTAt, priced on what P1 learned
// from the prefill-stress goldens, replayed, against the TTFT the router
// measured at each load level of the runs. The accuracy mark is the S1
// decisions' (slo-tiers.md, "The P2 recordings, and what they must show",
// item 4): a prediction more than 10% below the observed TTFT fails, one more
// than 25% above fails.
//
// These tests record the comparison; they do not tune P3. A level that
// misses the mark is a finding, frozen in the table below with its verdict,
// and the tests fail when a figure moves from it, so a change to P1, P3 or
// the replay that moves one says so.
//
// GREEN IS NOT A PASS. These tests pass while P3 under-predicts exactly as
// recorded: most frozen verdicts are UNDER. A fix to P3 makes them fail, and
// must rewrite ttftFrozen and the verdict counts in
// TestTTFTValidation_AgainstTheRouter, and say in its PR which rows moved.

// The four prefill-stress goldens.
const (
	ttftThreePodsFixture = "ttft-three-pods"
	ttftSaturatedFixture = "ttft-saturated"
	ttftOpenLongFixture  = "ttft-open-loop-long"
	ttftOpenShortFixture = "ttft-open-loop-short"
)

// pr475A is a, the fixed latency, from P1's curved fit (#475) of runs 3 and 4
// pooled (TestQuadratic_Runs3And4PooledGiveA, internal/signals/prefill): the
// intercept 7.95 ms plus run 4's least overhead 2.91 ms. No golden replays
// the two runs pooled, and P1 refuses a on run 3 alone (NoShortPrompts: its
// shortest prompts are 8k) and on run 2 (TooFewLowLoadMinutes), so those
// runs' levels are priced at this a. TestTTFTValidation_PooledAIsP1s holds
// it to P1's own fit of the two goldens' rows pooled, so it cannot drift
// from what P1 computes.
const pr475A = 0.00795 + 0.00291

// pr475B and pr475C are the same curve's b and c, in seconds a token and a
// token squared (TestQuadratic_Runs3And4PooledGiveA): the per-token cost
// b + c*I that P3 rescales V_P along where a fixture fits no curve of its
// own (runs 3 and 2). TestTTFTValidation_PooledAIsP1s holds them to P1 too.
const (
	pr475B = 7.1815e-07
	pr475C = 2.0280e-10
)

// knownPromptCV is the spread the load generator sent: n words drawn
// uniformly from 0.9n to 1.1n, about one token a word, so cv 0.2/sqrt(12).
var knownPromptCV = 0.2 / math.Sqrt(12)

// s1PromptCV is the spread S1 assumes with no histogram (AssumedPromptSpread).
const s1PromptCV = 2.0

// ttftLevel is one load level of a run: its window, from the load
// generator's LEVEL lines (the P2 recordings' sweep logs).
type ttftLevel struct {
	name     string
	from, to time.Time
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// ttftLevels is each compared run's levels. Runs 3 and 4 are open loop
// (Poisson) at light load. Of run 2 (closed loop) only the three levels long
// enough to hold the router's five-minute window are taken; its first three,
// two in flight, are not a Poisson queue (the pod runs near utilization 1
// with nothing waiting), and levels 4 and 5 are shorter than the window.
var ttftLevels = map[string][]ttftLevel{
	ttftOpenShortFixture: {
		{"1k", mustTime("2026-11-05T08:25:42Z"), mustTime("2026-11-05T08:31:42Z")},
		{"2k", mustTime("2026-11-05T08:31:42Z"), mustTime("2026-11-05T08:37:42Z")},
		{"4k", mustTime("2026-11-05T08:37:42Z"), mustTime("2026-11-05T08:43:42Z")},
		{"16k", mustTime("2026-11-05T08:43:42Z"), mustTime("2026-11-05T08:49:42Z")},
	},
	ttftOpenLongFixture: {
		{"8k", mustTime("2026-11-04T22:08:02Z"), mustTime("2026-11-04T22:15:02Z")},
		{"30k", mustTime("2026-11-04T22:15:02Z"), mustTime("2026-11-04T22:22:02Z")},
		{"37k", mustTime("2026-11-04T22:22:02Z"), mustTime("2026-11-04T22:29:02Z")},
	},
	ttftSaturatedFixture: {
		{"30k-x32", mustTime("2026-11-04T20:19:38Z"), mustTime("2026-11-04T20:27:38Z")},
		{"30k-x96", mustTime("2026-11-04T20:27:38Z"), mustTime("2026-11-04T20:35:38Z")},
		{"37k-x64", mustTime("2026-11-04T20:35:38Z"), mustTime("2026-11-04T20:43:38Z")},
	},
}

// p1Line is the fields of P1's prefill-capacity-observed log line the tests
// read: the estimate the saturation analyzer learned, as it logged it.
type p1Line struct {
	VP           float64 `json:"vp"`
	VPSource     string  `json:"vpSource"`
	VPLowerBound bool    `json:"vpLowerBound"`
	VPRefused    string  `json:"vpRefused"`
	VPSamples    int     `json:"vpSamples"`
	BusyMinutes  int     `json:"busyMinutes"`
	SlopeBound   float64 `json:"slopeBound"`
	A            float64 `json:"a"`
	AIntercept   float64 `json:"aIntercept"`
	AOverhead    float64 `json:"aOverhead"`
	AModel       string  `json:"aModel"`
	ASlope       float64 `json:"aSlope"`
	ACurve       float64 `json:"aCurve"`
	VPPrompt     float64 `json:"vpPromptTokens"`
	ARefused     string  `json:"aRefused"`
	ASamples     int     `json:"aSamples"`
	F            float64 `json:"f"`
	FRefused     string  `json:"fRefused"`
	FSamples     int     `json:"fSamples"`
}

// estimate is the line as the prefill.Estimate whatif.PrefillFromEstimate
// maps.
func (l p1Line) estimate() prefill.Estimate {
	return prefill.Estimate{
		VP: l.VP, VPSource: prefill.Source(l.VPSource), VPLowerBound: l.VPLowerBound,
		VPRefused: prefill.Refusal(l.VPRefused), VPSamples: l.VPSamples, SlopeBound: l.SlopeBound,
		A: l.A, ARefused: prefill.Refusal(l.ARefused), ASamples: l.ASamples,
		F: l.F, FRefused: prefill.Refusal(l.FRefused), FSamples: l.FSamples,
		VPPromptTokens: l.VPPrompt, AModel: prefill.AModel(l.AModel), ASlope: l.ASlope, ACurve: l.ACurve,
	}
}

// replayP1 replays a golden as the golden check does (Normal) and returns
// P1's last prefill-capacity-observed line, read from the saturation
// analyzer's own log, and the recording.
func replayP1(t *testing.T, fixture string) (p1Line, []byte) {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join(goldenDir, fixture, RecordingName))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	var last string
	n := 0
	logger := funcr.NewJSON(func(obj string) {
		if strings.Contains(obj, `"msg":"prefill-capacity-observed"`) {
			last = obj
			n++
		}
	}, funcr.Options{Verbosity: 10})
	_, err = Replay(logr.NewContext(context.Background(), logger), raw, Options{Mode: Normal})
	require.NoError(t, err)
	require.Positive(t, n, "%s: no prefill-capacity-observed line", fixture)
	var l p1Line
	require.NoError(t, json.Unmarshal([]byte(last), &l))
	return l, raw
}

// levelObservation is what a recording says of one level.
type levelObservation struct {
	// rate is the prefill pods' request rate, prompt the effective prompt
	// (computed tokens over requests) and tokenRate the computed-token rate,
	// each the mean over the level's cycles; transfer is decode's mean KV
	// transfer time.
	rate, prompt, tokenRate, transfer float64
	// prefillTTFT, prefillQueue and prefillService are the prefill engine's
	// means (AvgTTFT, AvgQueueTime, AvgServiceTime), decodeTTFT the decode
	// engine's AvgTTFT (on P/D, the first token end to end).
	prefillTTFT, prefillQueue, prefillService, decodeTTFT float64
	// router is the router's TTFT histogram over its window ending at the
	// level's last cycle; window its length in seconds.
	router []domain.LatencyBucket
	window float64
	// queuedOverFirst is the share of the prefill engine's requests whose
	// queue time was above its histogram's first bound, firstQueueBound, at
	// the level's last cycle.
	queuedOverFirst, firstQueueBound float64
	cycles                           int
}

// observeLevel reads a level's cycles from the recording. Rows are taken
// from a minute after the level starts, since the one-minute rates cover the
// level before until then.
func observeLevel(t *testing.T, raw []byte, lv ttftLevel) levelObservation {
	t.Helper()
	rec, err := learnrec.Read(raw)
	require.NoError(t, err)
	var o levelObservation
	var rates, prompts, tokens, xfer, pttft, pq, psvc, dttft []float64
	var lastPrefill []domain.ReplicaMetrics
	for _, ev := range rec.Events {
		if ev.Kind != saturation.KindAnalyze || ev.Now.Before(lv.from.Add(time.Minute)) || ev.Now.After(lv.to) {
			continue
		}
		var d saturation.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&d))
		in, err := decodeInput(d.Input)
		require.NoError(t, err)
		roles := map[string]string{}
		for _, vs := range in.VariantStates {
			roles[vs.VariantName] = vs.Role
		}
		rate, tok := 0.0, 0.0
		var pre []domain.ReplicaMetrics
		for _, rm := range in.ReplicaMetrics {
			switch domain.CanonicalRole(roles[rm.VariantName]) {
			case domain.RolePrefill:
				pre = append(pre, rm)
				if rm.RequestRateKnown && rm.RequestRate > 0 && rm.PrefillComputedTokenRateKnown {
					rate += rm.RequestRate
					tok += rm.PrefillComputedTokenRate
					prompts = append(prompts, rm.PrefillComputedTokenRate/rm.RequestRate)
				}
				if rm.AvgTTFTKnown && rm.AvgServiceTimeKnown && rm.AvgQueueTimeKnown {
					pttft, psvc, pq = append(pttft, rm.AvgTTFT), append(psvc, rm.AvgServiceTime), append(pq, rm.AvgQueueTime)
				}
			case domain.RoleDecode:
				if rm.AvgKVTransferTimeKnown {
					xfer = append(xfer, rm.AvgKVTransferTime)
				}
				if rm.AvgTTFTKnown {
					dttft = append(dttft, rm.AvgTTFT)
				}
			}
		}
		rates, tokens = append(rates, rate), append(tokens, tok)
		if in.RouterLatency != nil && len(in.RouterLatency.TTFTBuckets) > 0 {
			o.router, o.window = in.RouterLatency.TTFTBuckets, in.RouterLatency.WindowSeconds
		}
		lastPrefill = pre
		o.cycles++
	}
	require.Positive(t, o.cycles, "%s: no cycle", lv.name)
	require.NotEmpty(t, o.router, "%s: no router TTFT", lv.name)
	o.rate, o.prompt, o.tokenRate, o.transfer = mean(rates), mean(prompts), mean(tokens), mean(xfer)
	o.prefillTTFT, o.prefillQueue, o.prefillService, o.decodeTTFT = mean(pttft), mean(pq), mean(psvc), mean(dttft)
	o.queuedOverFirst = math.NaN()
	for _, rm := range lastPrefill {
		if bs := rm.QueueTimeBuckets; len(bs) > 1 && bs[len(bs)-1].Rate > 0 {
			o.firstQueueBound = bs[0].UpperBound
			o.queuedOverFirst = 1 - bs[0].Rate/bs[len(bs)-1].Rate
		}
	}
	return o
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// bucketQuantile is the q-quantile of a cumulative histogram of per-second
// rates, interpolated inside its bucket as histogram_quantile does, and that
// bucket's bounds: the resolution of the reading.
func bucketQuantile(bs []domain.LatencyBucket, q float64) (v, lo, hi float64) {
	total := bs[len(bs)-1].Rate
	if !(total > 0) {
		return math.NaN(), math.NaN(), math.NaN()
	}
	rank := q * total
	lower, below := 0.0, 0.0
	for _, b := range bs {
		if b.Rate >= rank {
			if math.IsInf(b.UpperBound, 1) {
				return math.Inf(1), lower, math.Inf(1)
			}
			if b.Rate == below {
				return b.UpperBound, lower, b.UpperBound
			}
			return lower + (b.UpperBound-lower)*(rank-below)/(b.Rate-below), lower, b.UpperBound
		}
		lower, below = b.UpperBound, b.Rate
	}
	return math.Inf(1), lower, math.Inf(1)
}

// The S1 mark's verdicts. A verdict "beyond the bucket" holds wherever in its
// histogram bucket the observed quantile lies: the prediction is more than
// 10% below the bucket's lower bound, or more than 25% above its upper one.
const (
	verdictPass       = "pass"
	verdictUnder      = "UNDER"
	verdictOver       = "over"
	verdictUnderSolid = "UNDER, beyond the bucket"
	verdictOverSolid  = "over, beyond the bucket"
	verdictRefused    = "refused"
)

func ttftVerdict(pred, obs, lo, hi float64) string {
	switch {
	case pred < 0.9*lo:
		return verdictUnderSolid
	case pred < 0.9*obs:
		return verdictUnder
	case pred > 1.25*hi:
		return verdictOverSolid
	case pred > 1.25*obs:
		return verdictOver
	default:
		return verdictPass
	}
}

// ttftFleetAt is the fleet one level is priced on: one prefill pod at V_P
// and a, the level's measured effective prompt at the given spread, its
// measured KV transfer per token, Poisson arrivals, flow control off. No
// decode line: no committed run fitted a decode ITL line from its loaded
// levels (decode KV stayed near 0.01). Decode's part of the TTFT is the
// decode engine's mean TTFT over the level, at the level's prompt
// (whatif.DecodeTTFT, flagged DecodeEngineMeasured), as S1 prices it.
func ttftFleetAt(p whatif.Prefill, prompt, transfer, decodeTTFT, cv float64) whatif.TTFTFleet {
	p.Replicas, p.PromptTokens, p.PromptCV = 1, prompt, cv
	p.TransferSecondsPerToken = transfer / prompt
	return whatif.TTFTFleet{Prefill: p, DecodeTTFT: whatif.DecodeTTFT{Seconds: decodeTTFT, PromptTokens: prompt},
		FlowControl: whatif.FlowControl{State: whatif.FlowControlOff}}
}

// ttftCase is one frozen row: a level, a spread and a percentile, the
// observed router quantile and P3's prediction, and the verdict.
type ttftCase struct {
	fixture, level string
	cv, q          float64
	observed       float64
	predicted      float64 // 0 when refused
	refused        whatif.Reason
	verdict        string
}

// ttftFrozen is the frozen table. Green is not a pass: the rows record P3
// under-predicting, and a fix to P3 must rewrite them and the counts below.
// It is P3 (main afdac8282, with #484) against the router at
// p90 and p95, at the spread the load generator sent (cv 0.058) and at S1's
// assumed 2. V_P is run 2's busy minutes on main, 134,775 tok/s, for every
// level; a is P1's own on run 4 (the curve's, 11.58 ms) and the runs 3 and 4
// pooled curve's on runs 3 and 2 (pr475A, 10.86 ms).
var ttftFrozen = []ttftCase{
	{"ttft-open-loop-short", "1k", knownPromptCV, 0.9, 0.046964964693101574, 0.03205189263487375, "", "UNDER"},
	{"ttft-open-loop-short", "1k", knownPromptCV, 0.95, 0.048502851711026626, 0.03639224916826264, "", "UNDER"},
	{"ttft-open-loop-short", "1k", s1PromptCV, 0.9, 0.046964964693101574, 0.05155667352013814, "", "pass"},
	{"ttft-open-loop-short", "1k", s1PromptCV, 0.95, 0.048502851711026626, 0.06804197839674676, "", "over, beyond the bucket"},
	{"ttft-open-loop-short", "2k", knownPromptCV, 0.9, 0.04788160469667319, 0.04477440513217962, "", "pass"},
	{"ttft-open-loop-short", "2k", knownPromptCV, 0.95, 0.04915280495759948, 0.05451087902054527, "", "pass"},
	{"ttft-open-loop-short", "2k", s1PromptCV, 0.9, 0.04788160469667319, 0.0877606470113836, "", "over, beyond the bucket"},
	{"ttft-open-loop-short", "2k", s1PromptCV, 0.95, 0.04915280495759948, 0.12248856740118555, "", "over, beyond the bucket"},
	{"ttft-open-loop-short", "4k", knownPromptCV, 0.9, 0.09208928571428572, 0.07042363483243055, "", "UNDER"},
	{"ttft-open-loop-short", "4k", knownPromptCV, 0.95, 0.09604464285714287, 0.09030200062819052, "", "pass"},
	{"ttft-open-loop-short", "4k", s1PromptCV, 0.9, 0.09208928571428572, 0.16003100022811548, "", "over, beyond the bucket"},
	{"ttft-open-loop-short", "4k", s1PromptCV, 0.95, 0.09604464285714287, 0.23089808926129796, "", "over, beyond the bucket"},
	{"ttft-open-loop-short", "16k", knownPromptCV, 0.9, 0.31625000000000003, 0.212818229405279, "", "UNDER"},
	{"ttft-open-loop-short", "16k", knownPromptCV, 0.95, 0.358125, 0.2816659040997275, "", "UNDER"},
	{"ttft-open-loop-short", "16k", s1PromptCV, 0.9, 0.31625000000000003, 0.5226584800980371, "", "over, beyond the bucket"},
	{"ttft-open-loop-short", "16k", s1PromptCV, 0.95, 0.358125, 0.7846906014684796, "", "over, beyond the bucket"},
	{"ttft-open-loop-long", "8k", knownPromptCV, 0.9, 0.09763598326359833, 0.1463867158051446, "", "over, beyond the bucket"},
	{"ttft-open-loop-long", "8k", knownPromptCV, 0.95, 0.10964285714285714, 0.16057175696996404, "", "over"},
	{"ttft-open-loop-long", "8k", s1PromptCV, 0.9, 0.09763598326359833, 0.3077818743796636, "", "over, beyond the bucket"},
	{"ttft-open-loop-long", "8k", s1PromptCV, 0.95, 0.10964285714285714, 0.4531461601144341, "", "over, beyond the bucket"},
	{"ttft-open-loop-long", "30k", knownPromptCV, 0.9, 0.5697435897435897, 0.37991619603970506, "", "UNDER"},
	{"ttft-open-loop-long", "30k", knownPromptCV, 0.95, 0.5848717948717949, 0.39253387349028235, "", "UNDER"},
	{"ttft-open-loop-long", "30k", s1PromptCV, 0.9, 0.5697435897435897, 0.9230159414573099, "", "over, beyond the bucket"},
	{"ttft-open-loop-long", "30k", s1PromptCV, 0.95, 0.5848717948717949, 1.3957045336664022, "", "over, beyond the bucket"},
	{"ttft-open-loop-long", "37k", knownPromptCV, 0.9, 0.7946666666666666, 0.5224629865548522, "", "UNDER, beyond the bucket"},
	{"ttft-open-loop-long", "37k", knownPromptCV, 0.95, 0.9149999999999995, 0.7137288996469937, "", "UNDER, beyond the bucket"},
	{"ttft-open-loop-long", "37k", s1PromptCV, 0.9, 0.7946666666666666, 1.3729570090221657, "", "over, beyond the bucket"},
	{"ttft-open-loop-long", "37k", s1PromptCV, 0.95, 0.9149999999999995, 2.0876265672745546, "", "over, beyond the bucket"},
	{"ttft-saturated", "30k-x32", knownPromptCV, 0.9, 7.8, 0, "Saturated", "refused"},
	{"ttft-saturated", "30k-x32", knownPromptCV, 0.95, 7.8999999999999995, 0, "Saturated", "refused"},
	{"ttft-saturated", "30k-x32", s1PromptCV, 0.9, 7.8, 0, "Saturated", "refused"},
	{"ttft-saturated", "30k-x32", s1PromptCV, 0.95, 7.8999999999999995, 0, "Saturated", "refused"},
	{"ttft-saturated", "30k-x96", knownPromptCV, 0.9, 29, 0, "Saturated", "refused"},
	{"ttft-saturated", "30k-x96", knownPromptCV, 0.95, 29.5, 0, "Saturated", "refused"},
	{"ttft-saturated", "30k-x96", s1PromptCV, 0.9, 29, 0, "Saturated", "refused"},
	{"ttft-saturated", "30k-x96", s1PromptCV, 0.95, 29.5, 0, "Saturated", "refused"},
	{"ttft-saturated", "37k-x64", knownPromptCV, 0.9, 28.929411764705883, 0, "Saturated", "refused"},
	{"ttft-saturated", "37k-x64", knownPromptCV, 0.95, 29.46470588235294, 0, "Saturated", "refused"},
	{"ttft-saturated", "37k-x64", s1PromptCV, 0.9, 28.929411764705883, 0, "Saturated", "refused"},
	{"ttft-saturated", "37k-x64", s1PromptCV, 0.95, 29.46470588235294, 0, "Saturated", "refused"},
}

// TestTTFTValidation_PooledAIsP1s holds pr475A to P1: the prefill rows of
// runs 3 and 4, from their goldens, fed to one observer through one settle
// tracker, run 4 moved to start five minutes after run 3 ends so both stay
// inside Retention (TestQuadratic_Runs3And4PooledGiveA's replayRuns does the
// same from a CSV of these rows). P1's a there is the curve's intercept plus
// the least overhead.
func TestTTFTValidation_PooledAIsP1s(t *testing.T) {
	tr := settle.NewTracker()
	tr.SetInterval(15 * time.Second)
	o := prefill.NewObserver()
	var est prefill.Estimate
	var shift time.Duration
	var last time.Time
	for _, f := range []string{ttftOpenLongFixture, ttftOpenShortFixture} {
		gz, err := os.ReadFile(filepath.Join(goldenDir, f, RecordingName))
		require.NoError(t, err)
		raw, err := gunzip(gz)
		require.NoError(t, err)
		rec, err := learnrec.Read(raw)
		require.NoError(t, err)
		first := true
		for _, ev := range rec.Events {
			if ev.Kind != saturation.KindAnalyze {
				continue
			}
			var d saturation.AnalyzeEvent
			require.NoError(t, ev.DecodeData(&d))
			in, err := decodeInput(d.Input)
			require.NoError(t, err)
			roles := map[string]string{}
			for _, vs := range in.VariantStates {
				roles[vs.VariantName] = vs.Role
			}
			var rows []domain.ReplicaMetrics
			for _, rm := range in.ReplicaMetrics {
				if domain.CanonicalRole(roles[rm.VariantName]) == domain.RolePrefill {
					rm.PodName, rm.VariantName = "prefill-"+f, "p"
					rows = append(rows, rm)
				}
			}
			if len(rows) == 0 {
				continue
			}
			if first && !last.IsZero() {
				shift = last.Add(5 * time.Minute).Sub(ev.Now)
			}
			first = false
			now := ev.Now.Add(shift)
			for i := range rows {
				rows[i].ReadySince = rows[i].ReadySince.Add(shift)
			}
			tr.Observe("ns/m", settlerow.Of(rows, map[string]string{"p": domain.RolePrefill}), now)
			est = o.Observe(prefill.Cycle{
				Key: "k", Engine: inferenceengine.EngineVLLM, Disaggregated: true, Rows: rows,
				Settle: &prefill.Settle{View: tr, Model: "ns/m"}, Now: now,
			})
			last = now
		}
	}
	require.Empty(t, est.ARefused)
	require.Equal(t, prefill.AModelQuadratic, est.AModel)
	t.Logf("runs 3+4 pooled: a %.5f = intercept %.5f + overhead %.5f, %d minutes in %d clusters",
		est.A, est.AIntercept, est.AOverhead, est.ASamples, est.AClusters)
	assert.InDelta(t, pr475A, est.A, 0.00001, "pr475A against P1's pooled fit")
	assert.InEpsilon(t, pr475B, est.ASlope, 1e-4, "pr475B against P1's pooled fit")
	assert.InEpsilon(t, pr475C, est.ACurve, 1e-4, "pr475C against P1's pooled fit")
}

// TestTTFTValidation_P1OnTheGoldens holds what main's P1 learns from each
// golden: the inputs the validation prices on, and the refusals that make it
// borrow V_P across runs and a from #475.
func TestTTFTValidation_P1OnTheGoldens(t *testing.T) {
	sat, _ := replayP1(t, ttftSaturatedFixture)
	assert.Equal(t, string(prefill.SourceBusyMinutes), sat.VPSource)
	assert.InDelta(t, 134775.41, sat.VP, 0.01)
	assert.Equal(t, 28, sat.VPSamples)
	assert.False(t, sat.VPLowerBound)
	assert.Equal(t, string(prefill.TooFewLowLoadMinutes), sat.ARefused)
	assert.Equal(t, string(prefill.OnePod), sat.FRefused)

	long, _ := replayP1(t, ttftOpenLongFixture)
	assert.Equal(t, string(prefill.NoPrefillCapacity), long.VPRefused)
	assert.Equal(t, string(prefill.NoShortPrompts), long.ARefused)
	assert.Equal(t, 15, long.ASamples)

	short, _ := replayP1(t, ttftOpenShortFixture)
	assert.Equal(t, string(prefill.NoPrefillCapacity), short.VPRefused)
	assert.Empty(t, short.ARefused)
	assert.Equal(t, string(prefill.AModelQuadratic), short.AModel)
	assert.InDelta(t, 0.01158, short.A, 0.00001, "the curve's intercept plus the least overhead")
	assert.InDelta(t, short.AIntercept+short.AOverhead, short.A, 1e-12)
	assert.Equal(t, 16, short.ASamples)

	three, _ := replayP1(t, ttftThreePodsFixture)
	assert.Equal(t, string(prefill.SourcePeakUnderQueue), three.VPSource)
	assert.True(t, three.VPLowerBound)
	assert.InDelta(t, 115621.26, three.VP, 0.01)
	assert.Equal(t, string(prefill.NoPromptSpread), three.ARefused)
	assert.Empty(t, three.FRefused)
	assert.InDelta(t, 1, three.F, 1e-12, "f at three pods")
	assert.Equal(t, 45, three.FSamples)
	t.Logf("P1: run 2 %+v; run 3 %+v; run 4 %+v; run 1 %+v", sat, long, short, three)

	// PrefillFromEstimate carries main's refusals: no run alone gives P3 a
	// fleet it can price.
	for name, l := range map[string]p1Line{"run 2": sat, "run 3": long, "run 4": short, "run 1": three} {
		_, u := whatif.PrefillFromEstimate(l.estimate())
		require.NotNil(t, u, "%s", name)
		t.Logf("%s: PrefillFromEstimate refuses %s (%s)", name, u.Reason, u.Detail)
	}
}

// ttftPrefill is the prefill side P3 prices a fixture's levels with: V_P
// from run 2's busy minutes, and a from the fixture's own P1 where main fits
// it, else #475's. f is left unknown: one pod prices as 1.
func ttftPrefill(t *testing.T, vp, own p1Line) (whatif.Prefill, string) {
	t.Helper()
	est := vp.estimate()
	est.A, est.ARefused, est.F, est.FRefused = own.A, prefill.Refusal(own.ARefused), 0, prefill.OnePod
	src := "main"
	if own.ARefused != "" {
		est.A, est.ARefused, src = pr475A, "", "runs 3+4 pooled"
	}
	if own.AModel == string(prefill.AModelQuadratic) {
		est.AModel, est.ASlope, est.ACurve = prefill.AModelQuadratic, own.ASlope, own.ACurve
	} else {
		est.AModel, est.ASlope, est.ACurve = prefill.AModelQuadratic, pr475B, pr475C
	}
	p, u := whatif.PrefillFromEstimate(est)
	require.Nil(t, u)
	return p, src
}

// TestTTFTValidation_AgainstTheRouter prices each level and compares it with
// the router's TTFT, at p90 and p95, against the frozen table.
func TestTTFTValidation_AgainstTheRouter(t *testing.T) {
	vp, _ := replayP1(t, ttftSaturatedFixture)
	got := make([]ttftCase, 0, 40)
	for _, f := range []string{ttftOpenShortFixture, ttftOpenLongFixture, ttftSaturatedFixture} {
		own, raw := replayP1(t, f)
		p, aSrc := ttftPrefill(t, vp, own)
		for _, lv := range ttftLevels[f] {
			o := observeLevel(t, raw, lv)
			require.InDelta(t, 300, o.window, 0, "the router's window")
			for _, cv := range []float64{knownPromptCV, s1PromptCV} {
				fl := ttftFleetAt(p, o.prompt, o.transfer, o.decodeTTFT, cv)
				for _, q := range []float64{0.9, 0.95} {
					obs, lo, hi := bucketQuantile(o.router, q)
					c := ttftCase{fixture: f, level: lv.name, cv: cv, q: q, observed: obs}
					switch r := whatif.TTFTAt(fl, o.rate, q).(type) {
					case whatif.TTFT:
						// No decode line, the decode engine's TTFT measured.
						require.True(t, r.HasFlag(whatif.DecodeEngineMeasured), "%v", r.Flags)
						require.False(t, r.HasFlag(whatif.DecodeSideOmitted), "%v", r.Flags)
						c.predicted, c.verdict = r.Seconds, ttftVerdict(r.Seconds, obs, lo, hi)
					case whatif.Unanswerable:
						c.refused, c.verdict = r.Reason, verdictRefused
					}
					got = append(got, c)
					t.Logf("%-20s %-8s cv %.3f p%2.0f  rate %.3f  prompt %6.0f  observed %7.4f in [%g, %g]  predicted %7.4f  ratio %.3f  %s%s  (a %s)",
						f, lv.name, cv, q*100, o.rate, o.prompt, obs, lo, hi, c.predicted, c.predicted/obs, c.verdict, refusedNote(c), aSrc)
				}
			}
		}
	}
	if len(ttftFrozen) == 0 {
		for _, c := range got {
			fmt.Printf("\t{%q, %q, %s, %g, %v, %v, %q, %q},\n", c.fixture, c.level, cvName(c.cv), c.q, c.observed, c.predicted, c.refused, c.verdict)
		}
		t.Fatal("no frozen table")
	}
	require.Len(t, got, len(ttftFrozen), "rows")
	for i, w := range ttftFrozen {
		g := got[i]
		require.Equal(t, []any{w.fixture, w.level, w.q, w.refused, w.verdict}, []any{g.fixture, g.level, g.q, g.refused, g.verdict}, "row %d", i)
		require.InDelta(t, w.cv, g.cv, 1e-12, "row %d", i)
		assert.InEpsilon(t, w.observed, g.observed, 1e-9, "row %d: observed", i)
		if w.refused == "" {
			assert.InEpsilon(t, w.predicted, g.predicted, 1e-6, "row %d: predicted", i)
		}
	}
	// The finding, counted. At the spread the runs sent, P3 (with #484's
	// decode-engine term, V_P rescaled to the prompt and the margin near the
	// knee) is under the mark at 9 of 14 light-load rows (2k at both
	// percentiles and 4k at p95 pass, 8k is over), 2 of them below even the
	// bucket's lower bound (37k). At S1's assumed cv 2 it is over at all but
	// one. Every row of run 2 is refused (Saturated), so none is under.
	count := func(cv float64, fixtures []string, prefix string) int {
		n := 0
		for _, c := range got {
			if c.cv == cv && slices.Contains(fixtures, c.fixture) && strings.HasPrefix(c.verdict, prefix) {
				n++
			}
		}
		return n
	}
	lightRuns := []string{ttftOpenShortFixture, ttftOpenLongFixture}
	assert.Equal(t, 9, count(knownPromptCV, lightRuns, verdictUnder), "light load, sent spread, under")
	assert.Equal(t, 2, count(knownPromptCV, lightRuns, verdictUnderSolid), "light load, sent spread, under beyond the bucket")
	assert.Equal(t, 13, count(s1PromptCV, lightRuns, verdictOver), "light load, cv 2, over")
	assert.Equal(t, 0, count(s1PromptCV, []string{ttftSaturatedFixture}, verdictUnderSolid)+count(knownPromptCV, []string{ttftSaturatedFixture}, verdictUnderSolid), "run 2, under")
	assert.Equal(t, 12, count(knownPromptCV, []string{ttftSaturatedFixture}, verdictRefused)+count(s1PromptCV, []string{ttftSaturatedFixture}, verdictRefused), "run 2, every row refused")
}

// TestTTFTValidation_TheResidual is S1's item 5 at light load, as far as the
// recordings carry it: the router's TTFT less the two engines' mean TTFTs,
// against S1's budget of 10 ms. On P/D a request's time is the prefill
// engine's TTFT, then the decode engine's (decode's own clock: the KV pull
// and its first step; it reads below prefill's at 30k, so it is not end to
// end). The router records no TTFT sum, so its median stands in for its
// mean, read from 25-50 ms to 200 ms wide buckets: the residual is coarse,
// and on prompts this even the median and the mean are close.
func TestTTFTValidation_TheResidual(t *testing.T) {
	residuals := make([]float64, 0, 7)
	for _, f := range []string{ttftOpenShortFixture, ttftOpenLongFixture} {
		gz, err := os.ReadFile(filepath.Join(goldenDir, f, RecordingName))
		require.NoError(t, err)
		raw, err := gunzip(gz)
		require.NoError(t, err)
		for _, lv := range ttftLevels[f] {
			o := observeLevel(t, raw, lv)
			p50, _, _ := bucketQuantile(o.router, 0.5)
			r := p50 - o.prefillTTFT - o.decodeTTFT
			residuals = append(residuals, r)
			t.Logf("residual: %s router p50 %.4f, prefill engine %.4f + decode engine %.4f: %+.4f s",
				lv.name, p50, o.prefillTTFT, o.decodeTTFT, r)
		}
	}
	slices.Sort(residuals)
	t.Logf("residual (router p50 less both engines' mean TTFT): least %+.4f, median %+.4f, largest %+.4f",
		residuals[0], residuals[len(residuals)/2], residuals[len(residuals)-1])
	assert.InDelta(t, 0.005, residuals[len(residuals)/2], 0.005, "the median residual, within S1's 10 ms budget")
	assert.Less(t, residuals[len(residuals)-1], 0.07, "the largest, at 30k")
}

func refusedNote(c ttftCase) string {
	if c.refused == "" {
		return ""
	}
	return " " + string(c.refused)
}

func cvName(cv float64) string {
	if cv == knownPromptCV {
		return "knownPromptCV"
	}
	return "s1PromptCV"
}

// TestTTFTValidation_WhatTheRunsShow holds the P3 checks these runs can
// test beside the router comparison (slo-capacity-planner-ttft.md, "What is
// UNVALIDATED, and what P2 must check"): V_P as a throughput, the wait tail
// at light load, a under a queue, the KV transfer, and f on three pods.
func TestTTFTValidation_WhatTheRunsShow(t *testing.T) {
	vp, satRaw := replayP1(t, ttftSaturatedFixture)

	// Check 1, V_P as a throughput: a pod under a queue serves the
	// busy-minute V_P at the prompt it was learned on, and less at a longer
	// one.
	served := make([]float64, 0, len(ttftLevels[ttftSaturatedFixture]))
	for _, lv := range ttftLevels[ttftSaturatedFixture] {
		o := observeLevel(t, satRaw, lv)
		served = append(served, o.tokenRate)
		t.Logf("V_P: %s serves %.0f tok/s at prompt %.0f (busy-minute V_P %.0f, ratio %.3f)", lv.name, o.tokenRate, o.prompt, vp.VP, o.tokenRate/vp.VP)
	}
	assert.InEpsilon(t, vp.VP, served[0], 0.01, "30k, 32 in flight")
	assert.InEpsilon(t, vp.VP, served[1], 0.01, "30k, 96 in flight")
	assert.InEpsilon(t, 0.86, served[2]/vp.VP, 0.01, "37k against V_P learned at 30k")

	// Check 2, the wait tail at light load: no request queued past the
	// engine histogram's first bound (0.3 s), and P3's tail there is small
	// but not zero, so it sits at or above the measured one. The bound is
	// coarse: this checks the direction only, not how far above.
	// Check 6, a at load: the prefill engine's TTFT beyond its queue and
	// service times, at light load and under a queue.
	light, loaded := make([]float64, 0, 7), make([]float64, 0, 3)
	for _, f := range []string{ttftOpenShortFixture, ttftOpenLongFixture} {
		own, raw := replayP1(t, f)
		p, _ := ttftPrefill(t, vp, own)
		for _, lv := range ttftLevels[f] {
			o := observeLevel(t, raw, lv)
			require.False(t, math.IsNaN(o.queuedOverFirst), "%s: a queue-time histogram", lv.name)
			assert.Zero(t, o.queuedOverFirst, "%s: queued past %g s", lv.name, o.firstQueueBound)
			r, ok := whatif.TTFTAt(ttftFleetAt(p, o.prompt, o.transfer, o.decodeTTFT, knownPromptCV), o.rate, 0.9).(whatif.TTFT)
			require.True(t, ok)
			tail := r.PrefillUtilization * math.Exp(-(o.firstQueueBound-r.WaitOffset)/r.WaitScale)
			assert.Positive(t, tail, "%s: P3's P(W > %g s)", lv.name, o.firstQueueBound)
			assert.Less(t, tail, 0.02, "%s: P3's P(W > %g s)", lv.name, o.firstQueueBound)
			light = append(light, o.prefillTTFT-o.prefillQueue-o.prefillService)
			t.Logf("wait: %s queued past %g s %.3f, P3 %.2e; engine TTFT %.4f = queue %.4f + service %.4f + %.4f",
				lv.name, o.firstQueueBound, o.queuedOverFirst, tail, o.prefillTTFT, o.prefillQueue, o.prefillService, light[len(light)-1])
		}
	}
	for _, lv := range ttftLevels[ttftSaturatedFixture] {
		o := observeLevel(t, satRaw, lv)
		loaded = append(loaded, o.prefillTTFT-o.prefillQueue-o.prefillService)
		t.Logf("a at load: %s engine TTFT %.3f = queue %.3f + service %.3f + %.3f",
			lv.name, o.prefillTTFT, o.prefillQueue, o.prefillService, loaded[len(loaded)-1])
	}
	// What the engine adds beyond queue and service is 3 to 83 ms at light
	// load, growing with the prompt, and 224 to 277 ms under a queue: it
	// grows under load, by more than P3's a (3.7 to 10.9 ms) is.
	t.Logf("a at load: beyond queue and service, light load max %.4f s, under a queue min %.4f s", slices.Max(light), slices.Min(loaded))
	assert.Less(t, slices.Max(light), 0.09)
	assert.Greater(t, slices.Min(loaded), 0.2)

	// Check 9, T_xfer: decode's mean KV transfer per prompt token from 1k
	// to 37k tokens.
	perToken := make([]float64, 0, 7)
	for _, f := range []string{ttftOpenShortFixture, ttftOpenLongFixture} {
		_, raw := replayP1(t, f)
		for _, lv := range ttftLevels[f] {
			o := observeLevel(t, raw, lv)
			perToken = append(perToken, o.transfer/o.prompt)
			t.Logf("T_xfer: %s %.4f s over %.0f tokens, %.3g s a token", lv.name, o.transfer, o.prompt, o.transfer/o.prompt)
		}
	}
	// It falls from 3.9 to 2.4 us a token as the prompt grows, flat from
	// 16k up: a fixed cost per transfer plus a per-token one, so a single
	// per-token figure taken at long prompts reads a short prompt's
	// transfer low.
	slices.Sort(perToken)
	t.Logf("T_xfer: per token, least %.3g, largest %.3g", perToken[0], perToken[len(perToken)-1])
	assert.InDelta(t, 2.4e-6, perToken[0], 0.05e-6)
	assert.InDelta(t, 3.9e-6, perToken[len(perToken)-1], 0.05e-6)

	// Check 4, f on three pods: P1 reads 1 (TestTTFTValidation_P1OnTheGoldens),
	// and the busiest pod's raw share over the mean, per cycle.
	_, raw := replayP1(t, ttftThreePodsFixture)
	shares := rawShares(t, raw)
	require.NotEmpty(t, shares)
	slices.Sort(shares)
	t.Logf("f: %d cycles at three loaded pods: raw share median %.3f, max %.3f; S1's floor 1.5",
		len(shares), shares[len(shares)/2], shares[len(shares)-1])
	assert.Less(t, shares[len(shares)/2], 1.05, "the median raw share")
	assert.Less(t, shares[len(shares)-1], 1.5, "S1's floor holds over every cycle")
}

// rawShares is, per cycle with load on all three prefill pods, the busiest
// pod's computed-token rate over the mean (S1's f_raw, not de-biased).
func rawShares(t *testing.T, raw []byte) []float64 {
	t.Helper()
	rec, err := learnrec.Read(raw)
	require.NoError(t, err)
	var out []float64
	for _, ev := range rec.Events {
		if ev.Kind != saturation.KindAnalyze {
			continue
		}
		var d saturation.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&d))
		in, err := decodeInput(d.Input)
		require.NoError(t, err)
		roles := map[string]string{}
		for _, vs := range in.VariantStates {
			roles[vs.VariantName] = vs.Role
		}
		var rs []float64
		for _, rm := range in.ReplicaMetrics {
			if domain.CanonicalRole(roles[rm.VariantName]) == domain.RolePrefill && rm.Ready && rm.PrefillComputedTokenRateKnown {
				rs = append(rs, rm.PrefillComputedTokenRate)
			}
		}
		if len(rs) != 3 || slices.Min(rs) < 10000 {
			continue
		}
		out = append(out, slices.Max(rs)/mean(rs))
	}
	return out
}
