package whatifapi

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// shapeOwn is the shape source of a bucket priced at its own shape.
const shapeOwn = "bucket"

// heavierHist is h, a cumulative histogram of total requests, with the
// share under each bound raised to the power f: the same bounds, as a
// recording's histograms keep, and for f > 1 longer requests.
func heavierHist(h map[float64]float64, f float64) map[float64]float64 {
	total := h[math.Inf(1)]
	out := make(map[float64]float64, len(h))
	for le, c := range h {
		out[le] = total * math.Pow(c/total, f)
	}
	return out
}

// shapeEntry is mainEntry's 13 h source with the experimental TTFT model,
// whose cycles i with long(i) serve longer prompts and computed tokens than
// the rest (heavierHist at f). With refit, the model is fitted on the cycles as they
// now are, so its fit steps served both shapes; else it is fitted before
// they changed, and never saw the long ones.
func shapeEntry(t *testing.T, long func(i int) bool, f float64, refit bool) (*Server, *inputsEntry) {
	t.Helper()
	r := &stubResolver{refusePrefill: true}
	e := rmFit(t, r, 3120)
	e.engine.PrefillSlots = 4
	lengthen := func() {
		for i := range e.mixture {
			if long(i) {
				e.mixture[i].Computed = heavierHist(e.mixture[i].Computed, f)
				e.mixture[i].Prompt = heavierHist(e.mixture[i].Prompt, f)
			}
		}
	}
	if refit {
		lengthen()
	}
	e.ttftModel = fitMainMixture(e, rankMixtureSwitches{stratified: whatif.DefaultRankMixtureStratified})
	require.Empty(t, e.ttftModel.info.Abstained)
	if !refit {
		lengthen()
	}
	info := e.ttftModel.info
	e.doc.TTFTModel = &info
	e.id = "sha256:shape-v2"
	s := New(Config{Resolver: r, WhatifSchema: "schema-v3"})
	s.inputs.add(e.id, e)
	return s, e
}

// shapePlan is the day plan of e on s with the experimental models at a p90
// TTFT of target; with windowShape, its cycles read no lengths, so every
// bucket is priced at the window's shape, as before buckets were priced at
// their own.
func shapePlan(t *testing.T, s *Server, e *inputsEntry, target float64, windowShape bool) DayPlanResponse {
	t.Helper()
	if windowShape {
		w := *e
		w.series = slices.Clone(e.series)
		for i := range w.series {
			w.series[i].HasShape = false
		}
		s = New(Config{Resolver: &stubResolver{refusePrefill: true}, WhatifSchema: "schema-v3"})
		s.inputs.add(w.id, &w)
		e = &w
	}
	body, err := s.dayPlan(context.Background(), DayPlanRequest{InputsID: e.id, ExperimentalModels: true,
		SLO: &SLORequest{TTFT: &Objective{Statistic: "p90", Target: f64(target)}}})
	require.NoError(t, err)
	var out DayPlanResponse
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// shapeTarget is the p90 TTFT target e's shape plans are asked at: x times
// the model's floor at the window's shape (its p90 at no load, one
// recorded prefill replica's worth of ranks), so the model meets it at both
// shapes.
func shapeTarget(t *testing.T, e *inputsEntry, x float64) float64 {
	t.Helper()
	mm := e.ttftModel
	v, _, u := mm.rq.m.Price(1e-6, mm.pooled.Computed, mm.pooled.Prompt, 0.9)
	require.Nil(t, u)
	return x * v
}

// A bucket whose requests are longer than the window's, in a window whose
// fit served both lengths, is priced at its own shape: it carries no
// WindowShape, and the TTFT model prices it. Drawing from its own longer
// histograms, the model sets one prefill replica a lower limit than at the
// window's shape (the bucket needs more prefill); a bucket of the shorter
// requests, no lower one.
func TestDayPlan_ExperimentalModelsPriceABucketAtItsOwnShape(t *testing.T) {
	const first = 2340 // the last quarter of the cycles serves the long requests
	s, e := shapeEntry(t, func(i int) bool { return i >= first }, 1.5, true)
	// The target is the model's p90 at the long requests' shape at 6 r/s on
	// one prefill replica: it prices them, and its curve's end bounds
	// neither shape's limit there.
	mm := e.ttftModel
	idx := make([]int, len(e.series))
	for i := range idx {
		idx[i] = i
	}
	longStep := whatif.MixtureStepOf(mixtureWindow(e, idx, e.series[first-1].At, e.series[len(e.series)-1].At), e.series[len(e.series)-1].At)
	target, _, u := mm.rq.m.Price(6*float64(mm.prefill), longStep.Computed, longStep.Prompt, 0.9)
	require.Nil(t, u)
	own := shapePlan(t, s, e, target, false)
	assert.Contains(t, strings.Join(own.Notes, " "), "a bucket is priced at its own rate and its own request shape")
	// A bucket is long (short) where its whole shape window, at most an hour
	// each side, is.
	edge := e.series[first].At
	long, short := -1, -1
	for i, b := range own.Buckets {
		if b.Needed == nil {
			continue
		}
		assert.NotContains(t, b.Flags, FlagWindowShape, "bucket %s", b.Start)
		assert.Equal(t, shapeOwn, b.Shape.Source, "bucket %s", b.Start)
		assert.Contains(t, b.Limit.Flags, string(whatif.RankMixtureTTFT), "bucket %s", b.Start)
		switch {
		case !b.Start.Before(edge.Add(time.Hour)) && long < 0:
			long = i
		case b.Start.Add(time.Hour).Before(edge) && short < 0:
			short = i
		}
	}
	require.GreaterOrEqual(t, long, 0, "a long bucket is sized")
	require.GreaterOrEqual(t, short, 0, "a short bucket is sized")

	// One replica a role's prefill limit, at the bucket's own shape and at
	// the window's.
	ctx := context.Background()
	q, err := dayQuestionOf(e, DayPlanRequest{InputsID: e.id, ExperimentalModels: true,
		SLO: &SLORequest{TTFT: &Objective{Statistic: "p90", Target: f64(target)}}})
	require.NoError(t, err)
	buckets := bucketize(e.series, q.step)
	cycle := sampleStep(e.series, q.step).Seconds()
	req := make([]float64, len(buckets))
	for i, b := range buckets {
		if len(b.samples) > 0 {
			req[i] = rateOf(b.samples).Mean * cycle * float64(len(b.samples))
		}
	}
	prefill := func(models *experimentalModels, k Knobs) float64 {
		t.Helper()
		var refused []DayRefusal
		g, err := q.group(ctx, k, models, &refused)
		require.NoError(t, err)
		v, err := g.z.at(ctx, FleetCounts{Prefill: 1, Decode: 1})
		require.NoError(t, err)
		x, ok := valueOf(v.prefill)
		require.True(t, ok, "%v", v.prefill)
		return x
	}
	window := prefill(q.models, q.k)
	at := func(i int) float64 {
		t.Helper()
		hs, flag := q.modelShapeOf(buckets, req, i)
		require.Empty(t, flag)
		p, o, _, ok := bucketShape(buckets[i].samples)
		require.True(t, ok)
		k := q.k
		k.PromptTokens, k.OutputTokens = shapeKnob(p), shapeKnob(o)
		return prefill(q.models.atBucket(hs), k)
	}
	l, sh := at(long), at(short)
	t.Logf("one prefill replica's limit: %.3g r/s at the window's shape, %.3g at a long bucket's own, %.3g at a short one's", window, l, sh)
	assert.Less(t, l, window, "the long bucket needs more prefill than at the window's shape")
	assert.GreaterOrEqual(t, sh, window, "the short bucket needs no more")
}

// A bucket whose shape the model's fit never served is priced at the
// window's shape, as without its own: flagged WindowShape and
// ShapeOutsideFit, saying what lies outside and the range, and needing
// what the window's shape needs. The note counts it.
func TestDayPlan_ExperimentalModelsFallBackOutsideTheFit(t *testing.T) {
	const from, to = 1200, 1440 // an hour of requests longer than the rest's
	s, e := shapeEntry(t, func(i int) bool { return i >= from && i < to }, 3, false)
	target := shapeTarget(t, e, 2.5)
	own := shapePlan(t, s, e, target, false)
	window := shapePlan(t, s, e, target, true)
	require.Len(t, window.Buckets, len(own.Buckets))
	lo, hi := e.series[from].At, e.series[to-1].At
	outside, inside := 0, 0
	for i, b := range own.Buckets {
		if b.Needed == nil {
			continue
		}
		m := b.Shape.Model
		require.NotNil(t, m, "bucket %s", b.Start)
		if b.Start.Before(lo) || b.Start.After(hi) {
			continue
		}
		if !slices.Contains(b.Flags, FlagShapeOutsideFit) {
			inside++
			continue
		}
		outside++
		assert.Contains(t, b.Flags, FlagWindowShape, "bucket %s", b.Start)
		assert.Equal(t, "window", b.Shape.Source, "bucket %s", b.Start)
		require.NotEmpty(t, m.OutsideFit, "bucket %s", b.Start)
		assert.Contains(t, strings.Join(m.OutsideFit, "; "), "computed", "bucket %s", b.Start)
		assert.Equal(t, window.Buckets[i].Needed, b.Needed, "bucket %s is priced as at the window's shape", b.Start)
		assert.Equal(t, window.Buckets[i].Limit, b.Limit, "bucket %s", b.Start)
	}
	assert.Positive(t, outside, "the buckets of the long hour fall back")
	t.Logf("in the long hour: %d sized buckets fell back, %d (their window mostly short) priced at their own", outside, inside)
	assert.Regexp(t, `[1-9][0-9]* at the window's \(WindowShape, ShapeOutsideFit\)`, strings.Join(own.Notes, "\n"))
}

// A bucket's shape window grows a bucket each side at a time until it holds
// MinShapeRequests and spans MinShapeWindow, clipped at the window's ends;
// one that reaches MaxShapeWindow first is short.
func TestBucketShapeWindow(t *testing.T) {
	step := 5 * time.Minute
	full := slices.Repeat([]float64{2000}, 48)
	from, to, ok := bucketShapeWindow(full, 20, step)
	assert.True(t, ok)
	assert.Equal(t, [2]int{14, 26}, [2]int{from, to}, "an hour is thirteen five-minute buckets centred")
	from, to, ok = bucketShapeWindow(full, 0, step)
	assert.True(t, ok)
	assert.Equal(t, [2]int{0, 11}, [2]int{from, to}, "clipped at the start: twelve buckets, an hour")
	thin := slices.Repeat([]float64{800}, 48)
	from, to, ok = bucketShapeWindow(thin, 20, step)
	assert.True(t, ok)
	assert.Equal(t, [2]int{10, 30}, [2]int{from, to}, "16000 requests at 800 a bucket")
	none := slices.Repeat([]float64{10}, 48)
	from, to, ok = bucketShapeWindow(none, 20, step)
	assert.False(t, ok)
	assert.LessOrEqual(t, time.Duration(to-from+1)*step, MaxShapeWindow)
	_, _, ok = bucketShapeWindow([]float64{5000}, 0, step)
	assert.False(t, ok, "a source shorter than MinShapeWindow")
}

// A fit range is the rate-weighted 5th to 95th percentile: one odd step,
// light, does not widen it.
func TestFitSpanOf(t *testing.T) {
	vals := []float64{100, 110, 120, 130, 140, 150, 160, 170, 180, 190, 1000}
	w := slices.Repeat([]float64{1}, 10)
	w = append(w, 0.1)
	s := fitSpanOf(vals, w)
	assert.Equal(t, 11, s.steps)
	assert.InDelta(t, 100, s.lo, 1e-9)
	assert.InDelta(t, 190, s.hi, 1e-9, "the light odd step is past the 95th percentile")
	s = fitSpanOf([]float64{5, math.NaN()}, []float64{1, 1})
	assert.Equal(t, 1, s.steps)
}

// The command's day plan (DayPlan, on a resolved source) reads each
// bucket's own shape from the source's per-rank series, as the server's
// does: a bucket inside the fit is priced at its own shape, not every one
// at the window's for want of the series.
func TestDayPlan_CommandReadsEachBucketsOwnShape(t *testing.T) {
	_, e := shapeEntry(t, func(int) bool { return false }, 1, true)
	res := &Resolved{Inputs: e.doc, Pricer: e.pricer, Series: e.series, Latency: e.latency, Mixture: e.mixture, Engine: e.engine}
	res.Inputs.TTFTModel = nil
	out, err := DayPlan(context.Background(), res, DayPlanRequest{ExperimentalModels: true,
		SLO: &SLORequest{TTFT: &Objective{Statistic: "p90", Target: f64(shapeTarget(t, e, 2.5))}}}, "schema-v3")
	require.NoError(t, err)
	require.Positive(t, out.Summary.Counts.Sized)
	own := 0
	for _, b := range out.Buckets {
		if b.Needed == nil {
			continue
		}
		require.NotNil(t, b.Shape.Model, "bucket %s", b.Start)
		assert.Empty(t, b.Shape.Model.OutsideFit, "bucket %s", b.Start)
		if b.Shape.Source == shapeOwn {
			own++
		}
	}
	assert.Equal(t, out.Summary.Counts.Sized, own, "every sized bucket at its own shape")
}

// The range check: a bucket's mean prompt and computed tokens inside the
// fit steps' 5th to 95th percentile, its
// computed-token p90 at most the largest step's, and enough fit steps.
func TestOutsideFit(t *testing.T) {
	span := fitSpan{lo: 1000, hi: 2000, steps: 40}
	m := &experimentalModels{ttft: &mainMixture{fit: mixtureRange{prompt: span, computed: span, computedP90: 5000}}}
	in := &bucketHists{mixtureOK: true, prompt: 1500, computed: 1500, computedP90: 4000}
	assert.Empty(t, m.outsideFit(in))
	edge := *in
	edge.prompt, edge.computed = 1000, 2000
	assert.Empty(t, m.outsideFit(&edge), "at the edges")
	out := *in
	out.computed = 2001
	assert.Len(t, m.outsideFit(&out), 1)
	assert.Contains(t, m.outsideFit(&out)[0], "computed tokens")
	tail := *in
	tail.computedP90 = 5001
	assert.Contains(t, m.outsideFit(&tail)[0], "computed-token p90")
	none := *in
	none.mixtureOK = false
	assert.Contains(t, m.outsideFit(&none)[0], "no prompt or computed-token histogram")
	m.ttft.fit.prompt.steps = MinShapeFitSteps - 1
	assert.Contains(t, m.outsideFit(in)[0], "no range")
}

// A bucket model's prefill limit, read off one search a prefill count
// (rateAtTargetShared), is the per-fleet search's at every fleet, within
// a thousandth; the prices it keeps are the model's.
func TestDayPlan_SharedRateAtTargetIsThePerFleetSearch(t *testing.T) {
	const first = 2340
	_, e := shapeEntry(t, func(i int) bool { return i >= first }, 1.5, true)
	mm := e.ttftModel
	q, err := dayQuestionOf(e, DayPlanRequest{InputsID: e.id, ExperimentalModels: true,
		SLO: &SLORequest{TTFT: &Objective{Statistic: "p90", Target: f64(1)}}})
	require.NoError(t, err)
	buckets := bucketize(e.series, q.step)
	cycle := sampleStep(e.series, q.step).Seconds()
	req := make([]float64, len(buckets))
	for i, b := range buckets {
		if len(b.samples) > 0 {
			req[i] = rateOf(b.samples).Mean * cycle * float64(len(b.samples))
		}
	}
	hs, flag := q.modelShapeOf(buckets, req, len(buckets)-6)
	require.Empty(t, flag)
	floor, _, u := mm.rq.m.Price(1e-6, hs.mixture.Computed, hs.mixture.Prompt, 0.9)
	require.Nil(t, u)
	compared := 0
	// With PrefillHitAtCount the histogram, and the search, move with the
	// prefill count.
	for _, hit := range []bool{false, true} {
		shared := q.models.atBucket(hs).ttft
		shared.hitAtCount = hit
		plain := *shared
		plain.prices, plain.atTarget = nil, nil
		for _, x := range []float64{1.2, 1.5, 2, 3} {
			for _, n := range []FleetCounts{{Prefill: 1, Decode: 1}, {Prefill: 1, Decode: 3}, {Prefill: 2, Decode: 2}, {Prefill: 3, Decode: 8}} {
				k := q.k
				k.Prefill, k.Decode = n.Prefill, n.Decode
				inner, err := e.pricer.Fleet(k)
				require.NoError(t, err)
				a, okA := shared.fleet(inner, k).(*mixtureFleet)
				b, okB := plain.fleet(inner, k).(*mixtureFleet)
				require.True(t, okA && okB)
				ra, endA, okRA := a.rateAtTarget(x * floor)
				rb, endB, okRB := b.rateAtTarget(x * floor)
				require.Equal(t, okRB, okRA, "hit %v x %g fleet %+v", hit, x, n)
				assert.Equal(t, endB, endA, "hit %v x %g fleet %+v", hit, x, n)
				if okRA {
					// The drawn curve wiggles at a few parts in a hundred thousand where it
					// is flat: two bisections over different spans land that far apart.
					assert.InEpsilon(t, rb, ra, 1e-3, "hit %v x %g fleet %+v", hit, x, n)
					compared++
				}
			}
		}
		assert.NotEmpty(t, shared.prices, "the bucket model kept its prices")
	}
	assert.Positive(t, compared)
}
