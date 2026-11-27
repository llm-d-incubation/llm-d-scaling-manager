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
	// MaxRatio is the highest bucket bound that holds requests over the mean:
	// no effective prompt in the window is longer (Prefill.PromptMaxRatio).
	// Zero where the histogram bounds none: Assumed, or Clamped by mass past
	// its last bound.
	MaxRatio float64
	// Assumed is no histogram read: CV is AssumedPromptCV. Clamped is a
	// reading past what the histogram can say: CV is PromptCVStop.
	Assumed, Clamped bool
	// ClosedAt is the context window, tokens, the mass past the
	// histogram's last finite bound was held under
	// (ReadPromptSpreadWithin), 0 where none was past it or no window
	// bounded it.
	ClosedAt float64
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
	// pods is the serving rows counted (PromptWindow.MaxPods).
	pods int
	// wholeComputed and whole are the computed and the whole prompt tokens
	// a second of the counted rows that also read a whole prompt over the
	// computed rate's minute (domain.ReplicaMetrics.AvgInputTokensRecent):
	// one reading of the same requests, for ComputedHit.
	wholeComputed, whole float64
	buckets              map[float64]float64
	// tiers is the cached tokens by tier and the KV transfers of the
	// counted rows that read them, for TierLoad (tier_load.go).
	tiers tierSample
	// reuseGap and idle are the counted rows' KV residency histograms
	// summed, upper bound to rate, nil with none read (HitModel).
	reuseGap, idle map[float64]float64
	// routerPredicted and routerBest are the router prefix producer's
	// predicted and best-available cached tokens a second this cycle
	// (NoteRouterPrefix), 0 with none read (HitModel's K).
	routerPredicted, routerBest float64
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
			if il := rm.AvgInputTokensRecent; Finite(il) && il > 0 {
				s.wholeComputed += rm.PrefillComputedTokenRate
				s.whole += r * il
			}
			s.tiers.noteTiers(rm, r)
			s.reuseGap = addLatencyBuckets(s.reuseGap, rm.KVReuseGapBuckets)
			s.idle = addLatencyBuckets(s.idle, rm.KVIdleBeforeEvictBuckets)
		}
	}
	if !(s.computed > 0) {
		return
	}
	s.pods = len(counted)
	if pooled := PoolComputedBuckets(nil, counted); len(pooled) > 0 {
		s.buckets = pooled
	}
	w.samples = append(w.samples, s)
	if len(w.samples) > PromptWindowCycles {
		w.samples = w.samples[len(w.samples)-PromptWindowCycles:]
	}
}

// NoteRouterPrefix adds the router prefix producer's predicted and
// best-available cached tokens a second, read at cycle, to that cycle's
// sample (HitModel's duplication K). A cycle Note did not keep, nil router
// figures or a pair that is not one reading (best under predicted, or none
// predicted) leave the window as it is.
func (w *PromptWindow) NoteRouterPrefix(cycle int, r *domain.RouterLatency) {
	if w == nil || len(w.samples) == 0 || r == nil {
		return
	}
	pr, ba := r.PrefixPredictedTokenRate, r.PrefixBestAvailableTokenRate
	s := &w.samples[len(w.samples)-1]
	if s.cycle != cycle || !Finite(pr) || !Finite(ba) || !(pr > 0) || !(ba >= pr) {
		return
	}
	s.routerPredicted, s.routerBest = pr, ba
}

// AffinityLoss is the share of the cached tokens the router could have hit
// on the best endpoint that it sent elsewhere, 1 - predicted / best
// available, summed over the window's cycles that read both
// (NoteRouterPrefix); false with none.
func (w *PromptWindow) AffinityLoss() (float64, bool) {
	if w == nil {
		return 0, false
	}
	var pr, ba float64
	for _, s := range w.samples {
		pr += s.routerPredicted
		ba += s.routerBest
	}
	if !(ba > 0) {
		return 0, false
	}
	return max(1-pr/ba, 0), true
}

// addLatencyBuckets adds b's rates to m by bound, m made on the first
// bucket read; a bucket that is no reading adds nothing.
func addLatencyBuckets(m map[float64]float64, b []domain.LatencyBucket) map[float64]float64 {
	for _, x := range b {
		if math.IsNaN(x.UpperBound) || !Finite(x.Rate) || x.Rate < 0 {
			continue
		}
		if m == nil {
			m = map[float64]float64{}
		}
		m[x.UpperBound] += x.Rate
	}
	return m
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

// MaxPods is the most serving pods one cycle of the window counted, 0 for
// a nil or empty window. ComputedHit is read over the whole window, so a
// window that spans a change of prefill count reads a hit from up to that
// many pods, not only from the count of its last cycle.
func (w *PromptWindow) MaxPods() int {
	if w == nil {
		return 0
	}
	n := 0
	for _, s := range w.samples {
		n = max(n, s.pods)
	}
	return n
}

// ComputedHit is the share of the whole prompt the prefill engines did not
// compute over the window, 1 - computed tokens / whole prompt tokens, read
// from the rows that carry both over the same minute: the hit from every
// tier (the GPU prefix cache, an offload tier, a KV connector), where
// vllm:prefix_cache_hits counts the GPU's alone. Held to [0, 1); false with
// no whole prompt read (a recording made before the collector read the
// one-minute prompt).
func (w *PromptWindow) ComputedHit() (float64, bool) {
	if w == nil {
		return 0, false
	}
	var computed, whole float64
	for _, s := range w.samples {
		computed += s.wholeComputed
		whole += s.whole
	}
	if !(whole > 0) || !(computed > 0) {
		return 0, false
	}
	return min(max(1-computed/whole, 0), math.Nextafter(1, 0)), true
}

// Spread is the prompt spread over the window: the histograms summed, as the
// rates are, and read at the mean of the cycles that carried one, so a window
// reaching back before the histogram does not mix two sets of requests. Below
// PromptWindowMinCycles or PromptWindowMinRequests it is Assumed, whatever the
// histogram says.
func (w *PromptWindow) Spread() PromptSpread { return w.SpreadWithin(0) }

// SpreadWithin is Spread with the histogram's mass past its last finite
// bound held under contextTokens, the engines' context window
// (ReadPromptSpreadWithin); 0 is no window known, Spread.
func (w *PromptWindow) SpreadWithin(contextTokens float64) PromptSpread {
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
	return ReadPromptSpreadWithin(pooled, computed/requests, contextTokens)
}

// promptMeanTolerance is the rounding allowed when a mean sits on the buckets'
// upper-bound mean, as constant prompts at a bucket's bound do.
const promptMeanTolerance = 1e-9

// ReadPromptSpread reads the spread from pooled bucket rates, computed once
// over the pool. mean is E[I_eff], computed tokens over completed requests. It
// is not the histogram's own mean: nothing queries the histogram's _sum or
// _count, and the two rates can cover different requests.
//
// The second moment is the largest any prompts in the buckets can have at
// mean, so when mean and histogram count the same requests cv reads at or
// above the truth: that costs replicas, not safety. Within a bucket (l, u] the
// second moment at a given bucket mean is largest with the bucket's mass on
// its two edges, and each token of mean moved into the bucket adds l + u to
// it. So the mass starts on the lower edges and the mean is raised to mean in
// the top buckets first (promptSecondMoment). Every 8k prompt in a (5000,
// 10000] bucket at a mean of 8000 read cv 0.31; the bucket's upper bound
// alone read 0.75, and the truth on the staircase was about 0.06. The bound
// is no tighter than that: those buckets also hold prompts split between
// 5000 and 10000 at that mean.
//
// The mean and the histogram do not always count the same requests: a pod in
// a rollout may carry no histogram, and the requests finishing in a window are
// not the ones that prefilled in it. So a mean above the buckets' upper-bound
// mean proves two populations, and the reading is refused as Assumed, not
// read as cv 0 or close to it. A mean below their lower-bound mean proves the
// same, and reads every request at its bucket's upper bound, high, the safe
// side. One population's mean is never on the lower edges, but a mean read a
// little low can fall under them: constant 5050-token prompts read cv 0.10 at
// their mean and 1.73 at a mean 1% low; every request at its upper bound read
// them at 1.71 either way. A mean inside the bounds can still be another
// population's, which no histogram can show.
//
// Mass past the last finite bound has no upper bound, so cv is the top and
// Clamped, as is a cv above it (ReadPromptSpreadWithin bounds it by the
// engines' context window). With no histogram, no request in it, or no mean
// it is AssumedPromptCV, Assumed.
//
// MaxRatio is the upper bound of the highest bucket holding requests, over
// the mean: the buckets prove no prompt is longer, though a gamma of the cv
// read puts mass there (the (5000, 10000] bucket at a mean of 8000 and cv
// 0.31 puts 19.8% of it above 10000). A cv above the top keeps it: the bound
// holds whatever the spread inside it. A mean below the lower edges reads
// every request at its upper bound, and the ratio over it is the same bound.
func ReadPromptSpread(pooled map[float64]float64, mean float64) PromptSpread {
	return ReadPromptSpreadWithin(pooled, mean, 0)
}

// ReadPromptSpreadWithin is ReadPromptSpread where no request's prompt is
// longer than contextTokens, the engines' context window (--max-model-len):
// no prompt computes more tokens than it holds, so the mass past the last
// finite bound lies in (last bound, contextTokens], read at the largest
// second moment as every bucket is, and the spread is a bound again, not
// the top: ClosedAt says so. vLLM's histogram edges stop under its
// context window (1-2-5 up to it, so 20000 under 40960), and on a lab P/D
// run of 6k-28k-token prompts 12-20% of the computed prompts sat past the
// last edge: read at the top, cv 3, against 1.4-1.6 bounded by the window.
// A window at or under the last bound, or 0, bounds nothing:
// ReadPromptSpread.
func ReadPromptSpreadWithin(pooled map[float64]float64, mean, contextTokens float64) PromptSpread {
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
	buckets := make([]promptBucket, 0, len(bounds))
	var below, lower float64
	for _, u := range bounds {
		// Rates over a window can dip between bounds; a dip is no mass.
		buckets = append(buckets, promptBucket{lo: lower, hi: u, n: max(pooled[u]-below, 0)})
		below = max(below, pooled[u])
		lower = u
	}
	closedAt := 0.0
	if total > below {
		if !(contextTokens > lower) || !Finite(contextTokens) {
			return PromptSpread{CV: PromptCVStop, Clamped: true}
		}
		buckets = append(buckets, promptBucket{lo: lower, hi: contextTokens, n: total - below})
		below, closedAt = total, contextTokens
	}
	// The buckets count below requests: the +Inf rate can read under the
	// last bound's, each series' rate extrapolated on its own. The mean is
	// spread over the requests the buckets hold, or the mass left on the
	// lower edges would pull the reading under what they allow.
	second, ok := promptSecondMoment(buckets, mean*below)
	if !ok {
		return assumed
	}
	top := 0.0
	for _, b := range buckets {
		if b.n > 0 {
			top = b.hi
		}
	}
	cv := math.Sqrt(max(second/below/(mean*mean)-1, 0))
	// At least 1: the mean may sit a rounding past the top bound
	// (promptSecondMoment's tolerance), and no bound is under the mean.
	ratio := max(top/mean, 1)
	if !Finite(cv) || cv > PromptCVStop {
		return PromptSpread{CV: PromptCVStop, MaxRatio: ratio, Clamped: true, ClosedAt: closedAt}
	}
	return PromptSpread{CV: cv, MaxRatio: ratio, ClosedAt: closedAt}
}

// promptBucket is one histogram bucket, (lo, hi], and the requests in it.
type promptBucket struct{ lo, hi, n float64 }

// promptSecondMoment is the largest sum of squared prompts the buckets can
// hold at a sum of prompts tokens: the mass on the lower edges, then moved up
// to the upper edges top bucket first, until the sum is tokens. False is a sum
// above the upper edges' (two populations). A sum below the lower edges' is
// read at the upper edges, the largest second moment the buckets hold at all.
func promptSecondMoment(buckets []promptBucket, tokens float64) (float64, bool) {
	var lower, upper, second, upperSecond float64
	for _, b := range buckets {
		lower += b.n * b.lo
		upper += b.n * b.hi
		second += b.n * b.lo * b.lo
		upperSecond += b.n * b.hi * b.hi
	}
	if tokens > upper*(1+promptMeanTolerance) {
		return 0, false
	}
	if tokens < lower {
		return upperSecond, true
	}
	need := tokens - lower
	for i := len(buckets) - 1; i >= 0 && need > 0; i-- {
		b := buckets[i]
		moved := min(need, b.n*(b.hi-b.lo))
		second += moved * (b.lo + b.hi)
		need -= moved
	}
	return second, true
}
