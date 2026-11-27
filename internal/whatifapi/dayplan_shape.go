package whatifapi

import (
	"fmt"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// This file prices a day plan's bucket, when the plan is asked for the
// experimental models (DayPlanRequest.ExperimentalModels), at the bucket's
// own measured request shape: the TTFT model draws from the prompt and
// computed-token histograms of the bucket's prefill pods, as a backtest
// prices each test step from that step's own histograms. A bucket's
// computed tokens carry its prefix hit, so a bucket whose hit is lower than
// the window's is priced with the longer prefills it served. The TPOT model
// keeps its ratio at the window's output lengths, placed around the
// planner's mean TPOT at the bucket's own: on held-out steps its ratio at
// each step's own output lengths put the measured p95 in its bucket less
// often than at the window's, so the window's stays.
//
// The histograms are read over a window centred on the bucket that grows,
// a bucket each side at a time, until it holds MinShapeRequests requests and
// spans MinShapeWindow, and no further than MaxShapeWindow: one five-minute
// bucket's computed-token tail is too noisy to price on. A bucket whose
// window cannot reach both is priced at the window's shape, as without this
// (WindowShape, ShapeFewRequests).
//
// The TTFT model was fitted on the window's DefaultBacktestStep steps, so a
// bucket is priced at its own shape only where that shape lies inside what
// those steps served: its mean prompt and computed tokens within the steps'
// rate-weighted 5th to 95th percentiles, and its computed-token p90 at
// most the largest step's. A bucket outside is priced
// at the window's shape, flagged WindowShape and ShapeOutsideFit
// (DayModelShape.OutsideFit says which and the range).

// The window a bucket's shape is read over (bucketShapeWindow).
const (
	// MinShapeRequests is the fewest requests the window must hold.
	MinShapeRequests = 16000.0
	// MinShapeWindow is the shortest span the window must cover.
	MinShapeWindow = time.Hour
	// MaxShapeWindow is the longest span it may cover: past it, the
	// bucket's shape is the hours around it, not its own.
	MaxShapeWindow = 2 * time.Hour
)

// MinShapeFitSteps is the fewest fit steps the TTFT model's range is read
// from; with fewer, no bucket is in range.
const MinShapeFitSteps = 20

// The flags a day plan with the experimental models adds to a bucket priced
// at the window's shape in place of its own.
const (
	// FlagShapeOutsideFit is a bucket whose own shape lies outside what the
	// models' fit window served (DayModelShape.OutsideFit).
	FlagShapeOutsideFit = "ShapeOutsideFit"
	// FlagShapeFewRequests is a bucket whose shape window, at
	// MaxShapeWindow, holds fewer than MinShapeRequests requests.
	FlagShapeFewRequests = "ShapeFewRequests"
)

// DayModelShape is the shape the experimental TTFT model reads for a
// bucket (DayShape.Model): its window, the requests it held, and its mean
// prompt and computed tokens and computed-token p90, absent where its
// prefill pods read none. OutsideFit names what lies outside the model's
// fit, and the range, where the bucket is priced at the window's shape for
// it.
type DayModelShape struct {
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
	Requests       float64   `json:"requests"`
	PromptTokens   *float64  `json:"promptTokens,omitempty"`
	ComputedTokens *float64  `json:"computedTokens,omitempty"`
	ComputedP90    *float64  `json:"computedP90,omitempty"`
	OutsideFit     []string  `json:"outsideFit,omitempty"`
}

// fitSpan is a shape figure's range over a model's fit steps: the
// rate-weighted 5th to 95th percentile, of steps steps.
type fitSpan struct {
	lo, hi float64
	steps  int
}

// fitSpanOf is the rate-weighted 5th to 95th percentile of vals, weighted
// by w; a value whose weight is not positive is left out.
func fitSpanOf(vals, w []float64) fitSpan {
	type vw struct{ v, w float64 }
	var xs []vw
	var total float64
	for i, v := range vals {
		if w[i] > 0 && whatif.Finite(v) {
			xs = append(xs, vw{v, w[i]})
			total += w[i]
		}
	}
	out := fitSpan{steps: len(xs)}
	if len(xs) == 0 {
		return out
	}
	slices.SortFunc(xs, func(a, b vw) int {
		switch {
		case a.v < b.v:
			return -1
		case a.v > b.v:
			return 1
		}
		return 0
	})
	at := func(q float64) float64 {
		cum := 0.0
		for _, x := range xs {
			cum += x.w
			if cum >= q*total*(1-1e-12) {
				return x.v
			}
		}
		return xs[len(xs)-1].v
	}
	out.lo, out.hi = at(0.05), at(0.95)
	return out
}

// mixtureRange is the shape the TTFT model's fit steps served: their mean
// prompt and computed tokens' spans, and the largest computed-token p90.
type mixtureRange struct {
	prompt, computed fitSpan
	computedP90      float64
}

// mixtureRangeOf is the range of steps, each weighted by its rate.
func mixtureRangeOf(steps []whatif.MixtureStep) mixtureRange {
	var p, c, w []float64
	var out mixtureRange
	for _, st := range steps {
		pm, okp := whatif.CumulativeMean(st.Prompt)
		cm, okc := whatif.CumulativeMean(st.Computed)
		if !okp || !okc {
			continue
		}
		p, c, w = append(p, pm), append(c, cm), append(w, st.Rate)
		if q, ok := whatif.CumulativeQuantile(st.Computed, 0.9); ok {
			out.computedP90 = max(out.computedP90, q)
		}
	}
	out.prompt, out.computed = fitSpanOf(p, w), fitSpanOf(c, w)
	return out
}

// bucketHists is a bucket's shape as the models read it, over its window.
type bucketHists struct {
	shape DayModelShape
	// mixture is the prefill pods' histograms pooled over the window
	// (mixtureOK where they hold requests), and prompt, computed and
	// computedP90 its figures.
	mixture                       whatif.MixtureStep
	mixtureOK                     bool
	prompt, computed, computedP90 float64
	// lengths is the window's mean prompt and output length as its cycles
	// read them (bucketShape), lengthsOK where they read any: the
	// planner's own terms at the bucket's shape over the same window.
	lengths   [2]float64
	lengthsOK bool
}

// bucketShapeWindow is the buckets [from, to] around bucket i whose
// requests (req, by bucket) the shape is read over: grown a bucket each
// side at a time, clipped at the window's ends, until it holds
// MinShapeRequests and spans MinShapeWindow; ok false where it reaches
// MaxShapeWindow first, and then [from, to] is the widest tried.
func bucketShapeWindow(req []float64, i int, step time.Duration) (from, to int, ok bool) {
	for j := 0; ; j++ {
		from, to = max(0, i-j), min(len(req)-1, i+j)
		if time.Duration(2*j+1)*step > MaxShapeWindow && j > 0 {
			from, to = max(0, i-j+1), min(len(req)-1, i+j-1)
			return from, to, false
		}
		n := 0.0
		for _, r := range req[from : to+1] {
			n += r
		}
		if n >= MinShapeRequests && time.Duration(to-from+1)*step >= MinShapeWindow {
			return from, to, true
		}
		if from == 0 && to == len(req)-1 {
			return from, to, false
		}
	}
}

// bucketHistsOf is the shape the TTFT model reads over the cycles of
// buckets [from, to] of bs, each a step long.
func (q *dayQuestion) bucketHistsOf(bs []dayBucket, req []float64, from, to int, step time.Duration) *bucketHists {
	e := q.e
	lo, hi := bs[from].start, bs[to].start.Add(step)
	h := &bucketHists{shape: DayModelShape{From: lo, To: hi}}
	var samples []SeriesSample
	for i := from; i <= to; i++ {
		h.shape.Requests += req[i]
		samples = append(samples, bs[i].samples...)
	}
	if p, o, _, ok := bucketShape(samples); ok {
		h.lengths, h.lengthsOK = [2]float64{p, o}, true
	}
	// The cycles are in [lo, hi): mixtureWindow reads (from, to].
	a, b := lo.Add(-time.Nanosecond), hi.Add(-time.Nanosecond)
	idx := make([]int, len(e.series))
	for i := range idx {
		idx[i] = i
	}
	if len(e.mixture) == len(e.series) {
		h.mixture = whatif.MixtureStepOf(mixtureWindow(e, idx, a, b), b)
		pm, okp := whatif.CumulativeMean(h.mixture.Prompt)
		cm, okc := whatif.CumulativeMean(h.mixture.Computed)
		c90, ok90 := whatif.CumulativeQuantile(h.mixture.Computed, 0.9)
		if okp && okc && ok90 {
			h.mixtureOK, h.prompt, h.computed, h.computedP90 = true, pm, cm, c90
			h.shape.PromptTokens, h.shape.ComputedTokens, h.shape.ComputedP90 = ptr(pm), ptr(cm), ptr(c90)
		}
	}
	return h
}

// outsideFit is what of h lies outside what the TTFT model's fit steps
// served, in words; nil where h is inside.
func (m *experimentalModels) outsideFit(h *bucketHists) []string {
	r := m.ttft.fit
	switch n := min(r.prompt.steps, r.computed.steps); {
	case !h.mixtureOK:
		return []string{"the prefill pods read no prompt or computed-token histogram over the bucket's window"}
	case n < MinShapeFitSteps:
		return []string{fmt.Sprintf("the TTFT model's fit has %d steps that read the shape, under %d: no range", n, MinShapeFitSteps)}
	}
	var out []string
	span := func(what string, v float64, s fitSpan) {
		// Within rounding: a uniform window reads its own mean back.
		if v < s.lo*(1-rangeTolerance) || v > s.hi*(1+rangeTolerance) {
			out = append(out, fmt.Sprintf("%s %.0f outside %.0f to %.0f (the fit's rate-weighted 5th to 95th percentile of %d steps)",
				what, v, s.lo, s.hi, s.steps))
		}
	}
	span("prompt tokens", h.prompt, r.prompt)
	span("computed tokens", h.computed, r.computed)
	if h.computedP90 > r.computedP90*(1+rangeTolerance) {
		out = append(out, fmt.Sprintf("computed-token p90 %.0f above the fit's largest step's %.0f", h.computedP90, r.computedP90))
	}
	return out
}

// atBucket is m pricing at a bucket's own shape: the TTFT model drawing
// from h's histograms (h nil where it does not price), the TPOT model's
// ratio the window's, around the planner's mean at the bucket's own output
// length. A model that does not price is m's, as it is.
func (m *experimentalModels) atBucket(h *bucketHists) *experimentalModels {
	out := &experimentalModels{ttft: m.ttft, tpot: m.tpot}
	if m.ttft.priced() && h != nil && h.mixtureOK {
		c := *m.ttft
		c.pooled, c.bucket = h.mixture, true
		c.prices, c.atTarget = map[mixturePriceKey]mixturePrice{}, map[mixturePriceKey]mixturePrice{}
		out.ttft = &c
	}
	if m.tpot.priced() {
		c := *m.tpot
		c.bucket = true
		out.tpot = &c
	}
	return out
}

// pricing says which of the models prices a fleet at k, one replica a
// role, for a target the plan asks: the TTFT model where ttft, the TPOT
// model where tpot (a TPOT model at a mean target prices nothing).
func (m *experimentalModels) pricing(k Knobs, ttft, tpot bool) (byTTFT, byTPOT bool) {
	if m == nil {
		return false, false
	}
	k.Prefill, k.Decode = 1, 1
	return ttft && m.ttft != nil && m.ttft.notPricedAt(k) == "", tpot && m.tpot != nil && m.tpot.notPricedAt(k) == ""
}

// modelShapeOf is bucket i's shape as the TTFT model reads it, over its
// window of bs (req each bucket's requests), and the flag of a bucket priced
// at the window's shape in its place: FlagShapeFewRequests where its window
// cannot hold enough requests, FlagShapeOutsideFit where its shape lies
// outside the model's fit; "" where it is priced at its own.
func (q *dayQuestion) modelShapeOf(bs []dayBucket, req []float64, i int) (*bucketHists, string) {
	from, to, ok := bucketShapeWindow(req, i, q.step)
	h := q.bucketHistsOf(bs, req, from, to, q.step)
	if !ok {
		return h, FlagShapeFewRequests
	}
	if out := q.models.outsideFit(h); len(out) > 0 {
		h.shape.OutsideFit = out
		return h, FlagShapeOutsideFit
	}
	return h, ""
}

// modelShapeNote says, in words, where the shape of each bucket sized with
// the experimental models came from (shapes, by flag; "" its own).
func modelShapeNote(shapes map[string]int) string {
	return fmt.Sprintf("With the experimental models, a bucket is priced at its own rate and its own request shape: the TTFT model draws "+
		"from the prompt and computed-token histograms (the prefix hit) of a window around the bucket of at least %.0f requests and %.0f "+
		"minutes (at most %.0f), as a backtest prices each step, and the planner's terms take the lengths over the same window; buckets whose shape so read agrees on the 5%% grid share the first one's "+
		"priced fleets; the TPOT model keeps its ratio at the window's output lengths. %d buckets were priced at their own shape; %d at the "+
		"window's (WindowShape, %s), their shape outside the range the TTFT model's fit steps served (their mean prompt and computed tokens' "+
		"rate-weighted 5th to 95th percentile, and their largest computed-token p90; shape.model.outsideFit says "+
		"which); %d at the window's (WindowShape, %s), their window holding too few requests.",
		MinShapeRequests, MinShapeWindow.Minutes(), MaxShapeWindow.Minutes(), shapes[""], shapes[FlagShapeOutsideFit], FlagShapeOutsideFit,
		shapes[FlagShapeFewRequests], FlagShapeFewRequests)
}

// key is h on the 5% shape grid (shapeKnob): its mean computed tokens and
// computed-token p90, zero for h nil or not read (its prompt is the
// planner's, on the group's own key). Buckets of one key are priced at the
// first one's histograms.
func (h *bucketHists) key() [2]float64 {
	if h == nil || !h.mixtureOK {
		return [2]float64{}
	}
	return [2]float64{shapeKnob(h.computed), shapeKnob(h.computedP90)}
}

// rangeTolerance is the relative rounding a shape figure may sit outside
// its fit range by and still be in it.
const rangeTolerance = 1e-9
