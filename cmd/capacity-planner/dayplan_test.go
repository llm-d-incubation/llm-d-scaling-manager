package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The day plan's tests replay each recording once per test binary: every
// day-plan command line on a recording shares one resolved source, as the
// CLI would resolve it (resolveDay), and every plan of one source and one
// request is priced once (planDay). A replay of a staircase takes seconds;
// the commands below ask about the same few dozen times.
var (
	resolved   sync.Map // key: recording, model, variants -> func() (*whatifapi.Resolved, error)
	planned    sync.Map // key: *Resolved and the request -> func() (*whatifapi.DayPlanResponse, error)
	realResolv = resolveDay
	realPlan   = planDay
)

func init() {
	resolveDay = func(ctx context.Context, f *dayPlanFlags, warn io.Writer) (*whatifapi.Resolved, error) {
		if f.recording == "" {
			return realResolv(ctx, f, warn)
		}
		key := strings.Join([]string{f.recording, f.model, f.prefillVariant, f.decodeVariant}, "\x00")
		once, _ := resolved.LoadOrStore(key, sync.OnceValues(func() (*whatifapi.Resolved, error) {
			return realResolv(context.Background(), f, io.Discard)
		}))
		return once.(func() (*whatifapi.Resolved, error))()
	}
	planDay = func(ctx context.Context, res *whatifapi.Resolved, req whatifapi.DayPlanRequest, schema string) (*whatifapi.DayPlanResponse, error) {
		b, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		key := struct {
			res *whatifapi.Resolved
			req string
		}{res, string(b) + schema}
		once, _ := planned.LoadOrStore(key, sync.OnceValues(func() (*whatifapi.DayPlanResponse, error) {
			return realPlan(context.Background(), res, req, schema)
		}))
		return once.(func() (*whatifapi.DayPlanResponse, error))()
	}
}

// dayArgs are the day plan's golden question on the guide's sample
// recording: V_P assumed, as the guide's sample assumes it.
var dayArgs = []string{"day-plan", "--recording", olsSweep, "--model", "model-02", "--assume-vp", "30000",
	"--ttft", "500ms@p95", "--tpot", "4ms"}

// stairDir is the staircase recordings' directory: real P/D runs, one
// prefill and one decode variant, two pods each.
const stairDir = "../../internal/engines/analyzers/learnreplay/testdata/goldens/"

// stair is a staircase recording's path.
func stair(name string) string { return stairDir + name + "/recording.jsonl.gz" }

// dayStairAssumed are the terms the b-on staircase did not learn, assumed, and
// targets it can meet.
var dayStairAssumed = []string{"--assume-vp", "200000", "--assume-a", "20ms", "--ttft", "2s@p90", "--tpot", "30ms"}

// The day plan's goldens, text and JSON. On the sample recording its
// 4k-token buckets put the 500 ms TTFT under the zero-load floor: prefill
// refused there, exit 3. On real P/D staircases: b-on with V_P and a assumed
// sizes every bucket; b-on as recorded learned no V_P, so decode is sized
// alone, a lower bound, and the plan exits 3 naming the refusal; b-off as
// recorded refuses every term that sizes a role, and says so, exit 3, with
// no figure of a need. go test ./cmd/capacity-planner -run TestDayPlanGolden
// -update rewrites them.
func TestDayPlanGolden(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		code int
	}{
		{"day-plan-ols-sweep", dayArgs, 3},
		{"day-plan-ols-sweep-json", append(slices.Clone(dayArgs), "-o", "json"), 3},
		{"day-plan-staircase-b-on-assumed", slices.Concat([]string{"day-plan", "--recording", stair("ttft-staircase-b-on")}, dayStairAssumed), 0},
		{"day-plan-staircase-b-on", []string{"day-plan", "--recording", stair("ttft-staircase-b-on")}, 3},
		{"day-plan-staircase-b-on-json", []string{"day-plan", "-o", "json", "--recording", stair("ttft-staircase-b-on")}, 3},
		{"day-plan-staircase-b-off", []string{"day-plan", "--recording", stair("ttft-staircase-b-off")}, 3},
	} {
		t.Run(c.name, func(t *testing.T) { checkGolden(t, c.name, c.code, c.args) })
	}
}

// dayJSON is a day-plan command line's JSON plan and exit code.
func dayJSON(t *testing.T, args ...string) (*whatifapi.DayPlanResponse, int) {
	t.Helper()
	out, code := runCmd(t, slices.Concat([]string{"day-plan", "-o", "json"}, args)...)
	var p whatifapi.DayPlanResponse
	require.NoError(t, jsonStrict([]byte(out), &p), out)
	return &p, code
}

// On real P/D staircases a refusal is the source's, not a bucket's: no
// bucket is unmet, a refused prefill leaves decode sized alone, a lower
// bound, with no prefill figure, and a refused decode knee sizes nothing,
// names why and exits 3.
func TestDayPlanStaircaseRefusals(t *testing.T) {
	onlyDecode, code := dayJSON(t, "--recording", stair("ttft-staircase-b-on"))
	assert.Equal(t, 3, code)
	require.Len(t, onlyDecode.Refused, 1)
	assert.Equal(t, "NoPrefillCapacity", onlyDecode.Refused[0].Reason)
	assert.Contains(t, onlyDecode.Refused[0].Hint, "--assume-vp")
	assert.Zero(t, onlyDecode.Summary.Counts.Unmet, "a refusal of the source is no bucket's")
	assert.Positive(t, onlyDecode.Summary.Counts.Sized)
	for _, b := range onlyDecode.Buckets {
		if b.Needed != nil {
			assert.Nil(t, b.Needed.Prefill, "%s: prefill sized without V_P", b.Start)
			assert.True(t, b.Needed.AtLeast, "%s: decode alone is a lower bound", b.Start)
			assert.Contains(t, b.Flags, whatifapi.FlagPrefillNotPriced)
		}
	}
	assert.Nil(t, onlyDecode.Summary.NeededGPUHours, "half a fleet's GPUs are not set beside a whole fleet's")
	assert.Nil(t, onlyDecode.Summary.NeededReplicaHours.Prefill)

	none, code := dayJSON(t, "--recording", stair("ttft-staircase-b-off"))
	assert.Equal(t, 3, code)
	assert.Zero(t, none.Summary.Counts.Sized)
	assert.Zero(t, none.Summary.Counts.Unmet)
	assert.Positive(t, none.Summary.Counts.Refused)
	assert.Nil(t, none.Summary.PeakNeeded)
	assert.Nil(t, none.Summary.NeededReplicaHours, "no need is printed as 0")
	assert.Empty(t, none.Summary.UnderHours)
	assert.NotEmpty(t, none.Summary.RefusedHours)
	reasons := make([]string, 0, len(none.Refused))
	for _, r := range none.Refused {
		reasons = append(reasons, r.Term+":"+r.Reason)
		assert.NotEmpty(t, r.Hint)
		assert.Positive(t, r.Buckets, r.Term)
	}
	assert.Equal(t, []string{"ttft:NoFixedLatency", "decodeKnee:PinnedDefaultB"}, reasons)

	full, code := dayJSON(t, slices.Concat([]string{"--recording", stair("ttft-staircase-b-on")}, dayStairAssumed)...)
	assert.Equal(t, 0, code)
	assert.Empty(t, full.Refused)
	assert.Equal(t, len(full.Buckets), full.Summary.Counts.Sized)
	for _, b := range full.Buckets {
		require.NotNil(t, b.Needed.Prefill, "%s", b.Start)
		assert.GreaterOrEqual(t, *b.Limit.Value, *b.SizedRate*(1-1e-9), "%s", b.Start)
	}
}

// A decode line the planner refuses prices neither TPOT nor the decode knee.
// With a assumed, prefill prices on these recordings, but no rule bounds
// decode: no bucket is sized and no hour is judged, and the plan names the
// decode knee, as at-replicas refuses the same fleet ("the decode knee is
// not priced"). Sized on prefill's limit alone, decode read 1 replica in
// every bucket (three-pods: 17 prefill + 1 decode at the busiest). Asked
// with no TPOT, the same.
func TestDayPlanUnpricedDecodeSizesNothing(t *testing.T) {
	for name, c := range map[string]struct {
		recording, tpot string
		want            []string
	}{
		"pinned line":         {stair("ttft-three-pods"), "30ms", []string{"tpot:PinnedDefaultB", "decodeKnee:PinnedDefaultB"}},
		"pinned line no tpot": {stair("ttft-three-pods"), "none", []string{"decodeKnee:PinnedDefaultB"}},
		"no line":             {stair("ttft-saturated"), "30ms", []string{"tpot:NoLine", "decodeKnee:NoLine"}},
	} {
		t.Run(name, func(t *testing.T) {
			p, code := dayJSON(t, "--recording", c.recording, "--assume-a", "20ms", "--ttft", "2s@p90", "--tpot", c.tpot)
			assert.Equal(t, 3, code)
			assert.Zero(t, p.Summary.Counts.Sized)
			assert.Positive(t, p.Summary.Counts.Refused)
			assert.Nil(t, p.Summary.PeakNeeded)
			require.NotEmpty(t, p.Buckets)
			for _, b := range p.Buckets {
				assert.Nil(t, b.Needed, "%s: a decode count no rule bounds", b.Start)
			}
			assert.NotEmpty(t, p.Summary.RefusedHours)
			for _, h := range p.Hours {
				assert.Nil(t, h.Needed, "%s", h.Start)
				assert.Contains(t, []string{whatifapi.DayVerdictRefused, ""}, h.Verdict, "%s: judged with decode unpriced", h.Start)
			}
			reasons := make([]string, 0, len(p.Refused))
			for _, r := range p.Refused {
				reasons = append(reasons, r.Term+":"+r.Reason)
				assert.Positive(t, r.Buckets, r.Term)
				assert.NotContains(t, r.Hint, "--tpot none", "asking no TPOT does not price the knee")
			}
			assert.Equal(t, c.want, reasons)
			require.NotEmpty(t, p.Refused)
			assert.Contains(t, p.Refused[len(p.Refused)-1].Effect, "the decode knee is not priced")

			// at-replicas says the same of the fleet the recording had.
			out, _ := runCmd(t, "at-replicas", "--recording", c.recording, "--assume-a", "20ms", "--ttft", "2s@p90", "--tpot", "30ms")
			assert.Contains(t, out, "the decode knee is not priced")
		})
	}
}

// A plan that sizes no bucket exits 3, even with nothing refused and no rate
// beyond the bound: every bucket has too little data.
func TestDayPlanNothingSizedExits3(t *testing.T) {
	p, code := dayJSON(t, slices.Concat([]string{"--recording", stair("ttft-staircase-b-on"), "--min-requests", "1e12"}, dayStairAssumed)...)
	assert.Empty(t, p.Refused)
	assert.Zero(t, p.Summary.Counts.Unmet)
	assert.Zero(t, p.Summary.Counts.Sized)
	assert.Equal(t, len(p.Buckets), p.Summary.Counts.TooLittleData)
	assert.Equal(t, 3, code)
}

// olsRes is the sample recording, resolved as the CLI resolves it, once per
// test binary.
func olsRes() (*whatifapi.Resolved, error) {
	return resolveDay(context.Background(), &dayPlanFlags{recording: olsSweep, model: "model-02"}, io.Discard)
}

// stubResolver resolves every request to one Resolved.
type stubResolver struct{ res *whatifapi.Resolved }

func (s stubResolver) Resolve(context.Context, whatifapi.InputsRequest) (*whatifapi.Resolved, error) {
	return s.res, nil
}

// dayPlanParity is each JSON golden of the day plan, with the recording it
// was planned from and the request its command line asks.
func dayPlanParity() map[string]struct {
	flags dayPlanFlags
	req   whatifapi.DayPlanRequest
} {
	minReq := fp(whatifapi.DefaultMinRequests)
	return map[string]struct {
		flags dayPlanFlags
		req   whatifapi.DayPlanRequest
	}{
		"day-plan-ols-sweep-json": {dayPlanFlags{recording: olsSweep, model: "model-02"}, whatifapi.DayPlanRequest{
			SLO: &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p95", Target: fp(0.5)},
				TPOT: &whatifapi.Objective{Statistic: statMean, Target: fp(0.004)}},
			Assume: &whatifapi.Assume{VP: fp(30000)}, MinRequests: minReq, Step: "5m0s"}},
		"day-plan-staircase-b-on-json": {dayPlanFlags{recording: stair("ttft-staircase-b-on")}, whatifapi.DayPlanRequest{
			MinRequests: minReq, Step: "5m0s"}},
	}
}

// POST /v1/dayplan's body is the CLI's JSON golden, byte for byte but for
// the ids: one code path, for every JSON golden of the day plan.
func TestDayPlanAPIParity(t *testing.T) {
	for golden, c := range dayPlanParity() {
		t.Run(golden, func(t *testing.T) {
			res, err := resolveDay(context.Background(), &c.flags, io.Discard)
			require.NoError(t, err)
			h := whatifapi.New(whatifapi.Config{Resolver: stubResolver{res}, WhatifSchema: "schema-v" + strconv.Itoa(schemaVersion)}).Handler()
			code, body := apiPost(t, h, "/v1/inputs", whatifapi.InputsRequest{})
			require.Equal(t, 200, code, "%s", body)
			var in whatifapi.InputsResponse
			require.NoError(t, json.Unmarshal(body, &in))
			req := c.req
			req.InputsID = in.InputsID
			code, body = apiPost(t, h, "/v1/dayplan", req)
			require.Equal(t, 200, code, "%s", body)
			var api whatifapi.DayPlanResponse
			require.NoError(t, jsonStrict(body, &api))
			b, err := os.ReadFile(filepath.Join("testdata", golden+".golden"))
			require.NoError(t, err)
			var cli whatifapi.DayPlanResponse
			require.NoError(t, jsonStrict(b, &cli))
			assert.NotEmpty(t, cli.WhatifSchema, "the CLI names the schema its figures follow")
			api.DayPlanID, cli.DayPlanID, api.InputsID, cli.InputsID = "", "", "", ""
			assert.Equal(t, cli, api)
			assert.NotEmpty(t, api.Buckets)
		})
	}
}

// day24h is a day of synthesized cycles every 15 s from 2026-11-01 00:00
// UTC: a diurnal rate from about 2 r/s at night to about 16 r/s at 16:00,
// with a jitter within each five minutes, every cycle at 1,000 prompt and
// 1,000 output tokens, and a fleet of 2 prefill and 3 decode replicas. A
// half hour from 03:00 has no cycles: a scrape gap.
func day24h() []whatifapi.SeriesSample {
	var out []whatifapi.SeriesSample
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 24*240; i++ {
		at := t0.Add(time.Duration(i) * 15 * time.Second)
		if at.Hour() == 3 && at.Minute() < 30 {
			continue
		}
		h := at.Sub(t0).Hours()
		rate := 2 + 14*(0.5-0.5*math.Cos(2*math.Pi*(h-4)/24))
		rate *= 1 + 0.15*math.Sin(float64(i)*1.7)
		out = append(out, whatifapi.SeriesSample{At: at, Rate: rate, RateKnown: true, Prefill: 2, Decode: 3, PrefillGPUs: 1, DecodeGPUs: 1,
			PromptTokens: 1000, OutputTokens: 1000, HasShape: true})
	}
	return out
}

// dayRequest is the 24-hour plan's question.
func dayRequest() whatifapi.DayPlanRequest {
	return whatifapi.DayPlanRequest{SLO: &whatifapi.SLORequest{TTFT: &whatifapi.Objective{Statistic: "p95", Target: fp(0.5)},
		TPOT: &whatifapi.Objective{Statistic: statMean, Target: fp(0.004)}}, Assume: &whatifapi.Assume{VP: fp(30000)}}
}

// day24hPlan is the day plan of day24h on the sample recording's learned
// state, planned once per test binary.
var day24hPlan = sync.OnceValues(func() (*whatifapi.DayPlanResponse, error) {
	res, err := olsRes()
	if err != nil {
		return nil, err
	}
	r := *res
	r.Series = day24h()
	return whatifapi.DayPlan(context.Background(), &r, dayRequest(), "schema-v3")
})

// dayPlan24h is day24hPlan for t.
func dayPlan24h(t *testing.T) *whatifapi.DayPlanResponse {
	t.Helper()
	p, err := day24hPlan()
	require.NoError(t, err)
	return p
}

// A day of 5-minute buckets, held to its golden: the hours, the summary and
// the notes. go test ./cmd/capacity-planner -run TestDayPlan24hGolden -update
// rewrites it.
func TestDayPlan24hGolden(t *testing.T) {
	p := dayPlan24h(t)
	require.Len(t, p.Buckets, 288)
	assert.Equal(t, 6, p.Summary.Counts.NoData, "the half hour with no cycles")
	assert.Equal(t, 282, p.Summary.Counts.Sized)
	assert.Equal(t, 282, p.Summary.Counts.SizedForP95, "20 cycles a bucket: every bucket sized for its p95")
	var b bytes.Buffer
	require.NoError(t, writeDayPlanText(&b, whatifapi.InputsResponse{Namespace: "ns", Model: "model-02"}, p))
	path := filepath.Join("testdata", "day-plan-24h.golden")
	if *update {
		require.NoError(t, os.WriteFile(path, b.Bytes(), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "go test ./cmd/capacity-planner -run TestDayPlan24hGolden -update writes it")
	assert.Equal(t, strings.ReplaceAll(string(want), "\r\n", "\n"), b.String())
}

// neededTotalRises asserts that the replicas a bucket needs, in all, never
// fall as its sized rate rises, among the sized buckets of one shape (the
// search is the fewest in all, SmallestFleet). A role alone may: one more
// decode replica can take a prefill replica's place.
func neededTotalRises(t *testing.T, buckets []whatifapi.DayBucket) int {
	t.Helper()
	byShape := map[[2]float64][]whatifapi.DayBucket{}
	for _, b := range buckets {
		if b.Status != whatifapi.BucketSized || b.Needed.Prefill == nil {
			continue
		}
		var k [2]float64
		if b.Shape.PromptTokens != nil && b.Shape.OutputTokens != nil {
			// The shape grid the plan prices on: 5% steps.
			k = [2]float64{math.Round(math.Log(*b.Shape.PromptTokens) / math.Log(1.05)), math.Round(math.Log(*b.Shape.OutputTokens) / math.Log(1.05))}
		}
		byShape[k] = append(byShape[k], b)
	}
	n := 0
	for _, bs := range byShape {
		sort.SliceStable(bs, func(i, j int) bool { return *bs[i].SizedRate < *bs[j].SizedRate })
		for i := 1; i < len(bs); i++ {
			lo, hi := bs[i-1], bs[i]
			n++
			assert.GreaterOrEqual(t, *hi.Needed.Prefill+hi.Needed.Decode, *lo.Needed.Prefill+lo.Needed.Decode,
				"%d+%d at %.4g r/s (%s), fewer than %d+%d at %.4g r/s (%s)", *hi.Needed.Prefill, hi.Needed.Decode, *hi.SizedRate,
				hi.Start.Format("15:04"), *lo.Needed.Prefill, lo.Needed.Decode, *lo.SizedRate, lo.Start.Format("15:04"))
		}
	}
	return n
}

// The replicas a bucket needs follow its own rate: the fleet's limit at the
// replicas found reaches the bucket's own p95, every bucket sized, and the
// replicas needed in all never fall as the rate rises. Sizing every bucket
// at one rate, the window's mean, fails the first: a bucket over the mean is
// short of it.
func TestDayPlanNeededFollowsRate(t *testing.T) {
	p := dayPlan24h(t)
	assert.Equal(t, whatifapi.SizeForP95, p.SizeFor)
	var sized []whatifapi.DayBucket
	for _, b := range p.Buckets {
		if b.Status != whatifapi.BucketSized {
			continue
		}
		sized = append(sized, b)
		require.NotNil(t, b.Limit.Value, "%s", b.Start)
		assert.GreaterOrEqual(t, *b.Limit.Value, b.Rate.P95*(1-1e-9), "%s: the fleet needed does not serve the bucket's p95", b.Start)
	}
	require.Len(t, sized, 282)
	assert.Equal(t, 281, neededTotalRises(t, p.Buckets), "every neighbouring pair of one shape compared")
	peak, low := p.Summary.PeakNeeded, p.Summary.MinNeeded
	assert.Greater(t, peak.Decode+*peak.Prefill, low.Decode+*low.Prefill, "a day from 2 to 16 r/s needs the same fleet all day")
	assert.NotEmpty(t, p.Summary.UnderHours, "the peak needs more than 2 prefill and 3 decode")
	assert.NotEmpty(t, p.Summary.OverHours, "the night needs fewer")
}

// On a real P/D staircase too, the replicas needed in all never fall as a
// bucket's rate rises (b-on with V_P and a assumed: every bucket sized).
func TestDayPlanStaircaseNeededTotalRises(t *testing.T) {
	p, _ := dayJSON(t, slices.Concat([]string{"--recording", stair("ttft-staircase-b-on")}, dayStairAssumed)...)
	assert.Positive(t, neededTotalRises(t, p.Buckets))
}

// The fleet a bucket had is judged whole at the bucket's rate. At 14:50 of
// the day the fewest replicas in all are 3 prefill and 2 decode for 17.97
// r/s; the fleet that ran, 2 prefill and 3 decode, as many in all, serves it
// too, so the bucket is no SLO risk, though prefill alone (2 against 3) reads
// short. At 10:15 the fewest in all are 1 prefill and 2 decode, where a
// search fixing decode at its own floor first found 3 + 1. Every verdict is
// the had fleet's limit against the rate.
func TestDayPlanVerdictIsTheFleetItHad(t *testing.T) {
	p := dayPlan24h(t)
	at := func(hhmm string) *whatifapi.DayBucket {
		for i := range p.Buckets {
			if p.Buckets[i].Start.Format("15:04") == hhmm {
				return &p.Buckets[i]
			}
		}
		t.Fatalf("no bucket at %s", hhmm)
		return nil
	}
	b := at("14:50")
	require.NotNil(t, b.Needed.Prefill)
	assert.Equal(t, whatifapi.DayNeeded{Prefill: ptrInt(3), Decode: 2}, *b.Needed)
	assert.Greater(t, *b.Needed.Prefill, b.Actual.Prefill, "the case: prefill alone reads short")
	require.NotNil(t, b.HadLimit.Value)
	assert.GreaterOrEqual(t, *b.HadLimit.Value, *b.SizedRate, "the fleet it had serves the rate")
	assert.NotEqual(t, whatifapi.DayVerdictUnder, b.Verdict)
	assert.Equal(t, whatifapi.DayNeeded{Prefill: ptrInt(1), Decode: 2}, *at("10:15").Needed, "the fewest in all, not 3 + 1")
	for _, x := range p.Buckets {
		if x.Status != whatifapi.BucketSized {
			continue
		}
		short := *x.HadLimit.Value < *x.SizedRate*(1-1e-9)
		assert.Equal(t, short, x.Verdict == whatifapi.DayVerdictUnder, "%s: had %g r/s at %g r/s, verdict %s",
			x.Start.Format("15:04"), *x.HadLimit.Value, *x.SizedRate, x.Verdict)
	}
}

// ptrInt is a pointer to n.
func ptrInt(n int) *int { return &n }

// jsonStrict decodes b into v, refusing a field v does not name.
func jsonStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
