package whatif

import (
	"math"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// PromptWindowCycles is how many of a prefill variant's latest cycles with
// traffic its prompt spread is read over: five minutes at the default 15 s
// cycle. One cycle's one-minute histogram is too few requests: at 40 requests
// a minute, one prompt in 50 being long is missed by 45% of minutes.
const PromptWindowCycles = 20

// The evidence a window must hold before its spread is read: until then it is
// Assumed, as a window with no histogram is, so a restart cannot read low with
// no flag. Both count only the cycles that carried a histogram.
const (
	// PromptWindowMinRequests is the requests the histogram must have counted.
	// A spread is set by its tail, and a tail of share p is missed by n requests
	// with chance (1-p)^n: at 200, a 2% tail is missed 1.8% of the time, and a
	// 5% one 0.004%. The window's 20 cycles span 345 s at the default 15 s
	// cycle, so a variant under 35 requests a minute never reaches it there
	// and stays Assumed; at a 60 s cycle that floor is 10.
	PromptWindowMinRequests = 200
	// PromptWindowMinCycles is the cycles it must have counted them over. It is
	// for fast traffic, where one minute passes the count: one minute is one
	// burst, not the traffic. Five is whatif.TTFTWindowCycles, two minutes at
	// the default 15 s cycle. At slower traffic the count binds first.
	PromptWindowMinCycles = 5
	// promptHistogramWindow is the window of the histogram's rate
	// (registration.TTFTInputWindow), what a cycle's rate is a count over.
	promptHistogramWindow = time.Minute
)

// PromptSpread is the coefficient of variation of the effective prompt, and
// how it was had: what the slo analyzer and the what-if CLI pass as
// Prefill.PromptCV.
type PromptSpread struct {
	CV float64
	// Assumed is no histogram read: CV is AssumedPromptCV. Clamped is a
	// reading past what the histogram can say: CV is PromptCVStop.
	Assumed, Clamped bool
}

// PoolComputedBuckets adds the computed-token histograms of rows' serving rows
// to pooled, each bound's rate summed, and returns it (a new map from nil). A
// bucket that is no reading adds nothing, so a row carrying no histogram
// leaves the pool as it was: absent, not empty mass.
func PoolComputedBuckets(pooled map[float64]float64, rows []domain.ReplicaMetrics) map[float64]float64 {
	if pooled == nil {
		pooled = map[float64]float64{}
	}
	for _, rm := range rows {
		if !prefill.Serving(rm) {
			continue
		}
		for _, b := range rm.ComputedTokensBuckets {
			if !math.IsNaN(b.UpperBound) && Finite(b.Rate) && b.Rate >= 0 {
				pooled[b.UpperBound] += b.Rate
			}
		}
	}
	return pooled
}

// promptSample is one cycle of a PromptWindow: its computed tokens and requests
// a second, and its pooled computed-token histogram, nil with none.
type promptSample struct {
	cycle             int
	at                time.Time
	computed, request float64
	buckets           map[float64]float64
}

// PromptWindow is a prefill variant's latest PromptWindowCycles cycles with
// traffic: the prompt and its spread over them. The slo analyzer and the
// what-if CLI each keep one per variant, so they read one figure from one
// traffic. A nil window has read nothing.
type PromptWindow struct {
	samples []promptSample
}

// Note folds one cycle's rows, read at at, into the window. A cycle with no
// traffic leaves it as it is, so an idle tail is read at the traffic it last served. Only the
// serving rows that read both rates count, and the histogram is pooled over
// those same rows.
func (w *PromptWindow) Note(rows []domain.ReplicaMetrics, cycle int, at time.Time) {
	s := promptSample{cycle: cycle, at: at}
	counted := make([]domain.ReplicaMetrics, 0, len(rows))
	for _, rm := range rows {
		if !prefill.Serving(rm) || !rm.PrefillComputedTokenRateKnown || !rm.RequestRateKnown {
			continue
		}
		if r := rm.RequestRate; Finite(rm.PrefillComputedTokenRate) && rm.PrefillComputedTokenRate > 0 && Finite(r) && r > 0 {
			s.computed += rm.PrefillComputedTokenRate
			s.request += r
			counted = append(counted, rm)
		}
	}
	if !(s.computed > 0) {
		return
	}
	if pooled := PoolComputedBuckets(nil, counted); len(pooled) > 0 {
		s.buckets = pooled
	}
	w.samples = append(w.samples, s)
	if len(w.samples) > PromptWindowCycles {
		w.samples = w.samples[len(w.samples)-PromptWindowCycles:]
	}
}

// Clone is a copy that a later Note does not reach. Nil stays nil.
func (w *PromptWindow) Clone() *PromptWindow {
	if w == nil {
		return nil
	}
	return &PromptWindow{samples: slices.Clone(w.samples)}
}

// Prompt is the mean effective prompt over the window, computed tokens over
// requests, so each cycle weighs by its requests, and the cycles it spans.
func (w *PromptWindow) Prompt() (tokens float64, from, to, cycles int) {
	if w == nil || len(w.samples) == 0 {
		return 0, 0, 0, 0
	}
	var computed, requests float64
	for _, s := range w.samples {
		computed += s.computed
		requests += s.request
	}
	return computed / requests, w.samples[0].cycle, w.samples[len(w.samples)-1].cycle, len(w.samples)
}

// Spread is the prompt spread over the window: the histograms summed, as the
// rates are, and read at the mean of the cycles that carried one, so a window
// reaching back before the histogram does not mix two sets of requests. Below
// PromptWindowMinCycles or PromptWindowMinRequests it is Assumed, whatever the
// histogram says.
func (w *PromptWindow) Spread() PromptSpread {
	pooled := map[float64]float64{}
	var computed, requests float64
	cycles, counted := 0, 0.0
	var last time.Time
	if w != nil {
		for _, s := range w.samples {
			if s.buckets == nil {
				continue
			}
			// Each rate is over the last minute, so at a short cycle the
			// samples overlap: only the time since the last one is new.
			covered := promptHistogramWindow
			if cycles > 0 {
				covered = min(max(s.at.Sub(last), 0), promptHistogramWindow)
			}
			counted += s.request * covered.Seconds()
			last = s.at
			cycles++
			computed += s.computed
			requests += s.request
			for bound, rate := range s.buckets {
				pooled[bound] += rate
			}
		}
	}
	if cycles < PromptWindowMinCycles || counted < PromptWindowMinRequests {
		return ReadPromptSpread(nil, 0)
	}
	return ReadPromptSpread(pooled, computed/requests)
}

// promptMeanTolerance is the rounding allowed when a mean sits on the buckets'
// upper-bound mean, as constant prompts at a bucket's bound do.
const promptMeanTolerance = 1e-9

// ReadPromptSpread reads the spread from pooled bucket rates, computed once
// over the pool. mean is E[I_eff], computed tokens over completed requests. It
// is not the histogram's own mean: nothing queries the histogram's _sum or
// _count, and the two rates can cover different requests.
//
// The second moment uses each bucket's upper bound, so when mean and histogram
// count the same requests cv reads at or above the truth: that costs replicas,
// not safety. They do not always: a pod in a rollout may carry no histogram, and
// the requests finishing in a window are not the ones that prefilled in it. So
// a mean above the buckets' upper-bound mean proves two populations, and the
// reading is refused as Assumed, not read as cv 0 or close to it. That bound
// also covers cv just above 0: past the check, cv is at least the buckets' own
// spread about their upper-bound mean. A mean inside the bound can still be
// another population's, which no histogram can show.
//
// Mass past the last finite bound has no upper bound, so cv is the top and
// Clamped, as is a cv above it. With no histogram, no request in it, or no mean
// it is AssumedPromptCV, Assumed.
func ReadPromptSpread(pooled map[float64]float64, mean float64) PromptSpread {
	assumed := PromptSpread{CV: AssumedPromptCV, Assumed: true}
	total, ok := pooled[math.Inf(1)]
	if !ok || !(total > 0) || !(mean > 0) {
		return assumed
	}
	bounds := make([]float64, 0, len(pooled))
	for b := range pooled {
		if !math.IsInf(b, 1) {
			bounds = append(bounds, b)
		}
	}
	slices.Sort(bounds)
	var first, second, below float64
	for _, u := range bounds {
		// Rates over a window can dip between bounds; a dip is no mass.
		n := max(pooled[u]-below, 0)
		first += n * u
		second += n * u * u
		below = max(below, pooled[u])
	}
	if total > below {
		return PromptSpread{CV: PromptCVStop, Clamped: true}
	}
	if mean > first/total*(1+promptMeanTolerance) {
		return assumed
	}
	cv := math.Sqrt(max(second/total/(mean*mean)-1, 0))
	if !Finite(cv) || cv > PromptCVStop {
		return PromptSpread{CV: PromptCVStop, Clamped: true}
	}
	return PromptSpread{CV: cv}
}
