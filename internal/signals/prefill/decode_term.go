package prefill

import (
	"maps"
	"math"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The decode side of a TTFT on a P/D fleet
// (docs/proposals/slo-capacity-planner-ttft.md, "As built: the decode term"):
//
//	router TTFT = prefill engine's + D + R
//	D(I, ITL)   = base + perToken*I + perStep*ITL
//
// D is the decode engine's mean TTFT: the KV pull and its first steps. It
// grows with the prompt, and with the step time, because a request that
// arrives on a decode pod waits on the steps that pod is running. R is the hop
// between the router and the engines, the router's mean less the two engines'.
//
// Neither is a constant of the model. Both are learned here per variant from
// what the collector reads, by the learner rules of this package: readings are
// admitted by state, and a figure the readings cannot support is refused with a
// reason, for the caller to flag (whatif.TermFromEstimate).
//
// D reads a row's AvgTTFT, AvgITL, input tokens, request rate and capacity
// queue, all on the scaling queries of every cycle. R reads the router's TTFT
// _sum and _count, over the router's window. The router's histogram cannot
// stand in for them: its buckets are 25 to 100 ms wide around a figure of 5 to
// 15 ms, and on the warm-up of the staircase run a residual read from them fell
// with the prompt where the true one rises.

const (
	// MinDecodeRequestRate is the least request rate, per second, a decode row
	// must carry to be admitted: below it a one-minute mean holds a few
	// requests.
	MinDecodeRequestRate = 0.05
	// MaxDecodePromptDrift is how far, relatively, a row's one-minute prompt
	// may be from its longer-window prompt: further, the mean TTFT of the
	// minute was not read at the prompt the row states.
	MaxDecodePromptDrift = 0.15
	// MaxDecodeQueue is the most requests a decode row may hold waiting. A few
	// is the KV pull in transit, which D is. Past that the pod is behind on
	// pulls, and its TTFT is a queue no step time explains: on the staircase
	// run's 3,300 rows, 3% of those at 6 to 9 waiting read over 30 ms above
	// the fit, against 44% at 10 to 14 and 81% at 15 to 24. Stated from
	// that one model, not a property of the engine.
	MaxDecodeQueue = 9
	// MaxDecodeQueueWait is the longest the waiting count, over the row's
	// request rate, may read in seconds (Little's law). MaxDecodeQueue alone
	// let a saturating warm-up's rows through, 6 waiting at 8 r/s with a TTFT
	// of 373 ms and an ITL of 2 ms, and that row was held as D. Of the
	// staircase run's rows, none under 0.4 s read over 30 ms above the fit,
	// against 49% at 0.4 to 0.8 s. Stated from that one model.
	MaxDecodeQueueWait = 0.4
	// MinDecodeMinutes is the fewest admitted pod-minutes D is fitted from.
	MinDecodeMinutes = 10
	// MinStepSpread is the least ratio of the longest to the shortest step
	// time the admitted minutes must span for perStep to be read, between the
	// second shortest and the second longest so that each end holds two
	// minutes. At 1.4 the staircase's warm-up read a slope of -1.0; at 3.9
	// its 6.8 was the final figure's.
	MinStepSpread = 1.5
	// MinHopWindows is the fewest admitted router windows R is read from.
	MinHopWindows = 5
	// MinHopCycles is the fewest cycles a router window must hold for its
	// engines' means to be read over it.
	MinHopCycles = 4
	// MinHopRequestRate is the least request rate, per second over the
	// model, of a cycle in a router window: below it the means are a few
	// requests.
	MinHopRequestRate = 0.1
	// MaxHopRateRatio is the most the request rate may move across a router
	// window, largest over least. The router's mean is weighted by the
	// window's requests and the engines' by their cycles' rates; a ramp
	// inside the window parts the two.
	MaxHopRateRatio = 1.3
	// FitSetCycles is how many consecutive cycles a fit must stand before it
	// is priced, and so the dwell on a fall-back: one refused cycle restarts
	// the count. FitClearCycles is how many consecutive refused cycles end a
	// fit that is priced; until then the last fit stands. The fall-back is the
	// safe side, the higher-priced source on every case read, so the way to it
	// is the shorter. Tuned on AR(1) noise at phi 0.9, a correlation of about
	// 10 cycles, around a crossing 80 cycles long at 0.3 of the excursion:
	// one reversal per excursion over 400 draws, against about 9 flips with
	// no latch. At 0.5 of it the mean is 2.0 flips, the worst 4. Shorter
	// settings, 20 and 10, read 4 at 0.3 too. At 15 s a cycle: 7.5 minutes
	// and 3.75.
	FitSetCycles   = 30
	FitClearCycles = 15
	// HeldDecodeAge is how recent a quiet row must be for its TTFT to be held
	// where no fit stands: the five cycles' window of the decode engine's
	// measured TTFT, in minutes, since a row is sampled once a minute.
	HeldDecodeAge = 5 * time.Minute
	// minHopWindowShare is how much of the router's window the cycles must
	// span.
	minHopWindowShare = 0.8
	// engineHorizon is how long a cycle's engine means are kept with no
	// router mean to set them against: twice the router's window.
	engineHorizon = 10 * time.Minute
	// maxDecodeSamples bounds a window's pod-minutes, and maxHopSamples its
	// router windows, whatever the cycle interval: six hours of a dozen pods.
	maxDecodeSamples = 12 * maxSamples
	maxHopSamples    = maxSamples
)

// Why a term of D or R was not learned.
const (
	// TooFewDecodeMinutes is D with fewer than MinDecodeMinutes admitted
	// pod-minutes.
	TooFewDecodeMinutes Refusal = "TooFewDecodeMinutes"
	// NoStepSpread is D whose minutes do not span MinStepSpread in step time:
	// load has not moved enough to tell perStep from base. A flat fleet lands
	// here for as long as it stays flat.
	NoStepSpread Refusal = "NoStepSpread"
	// NonPositiveStepSlope is a fitted perStep not above zero.
	NonPositiveStepSlope Refusal = "NonPositiveStepSlope"
	// NegativePromptSlope is a fitted perToken below zero.
	NegativePromptSlope Refusal = "NegativePromptSlope"
	// NonPositiveDecodePlane is a plane not above zero at the shortest prompt
	// and the shortest step it read: it prices no time where it was measured.
	// Its intercept may be under zero: nothing reads it there, and no figure
	// prices under the old sum wherever it is read (whatif.DecodeTerm). On the
	// staircase run's warm-up it is -2.1 ms and the plane is sound.
	NonPositiveDecodePlane Refusal = "NonPositiveDecodePlane"
	// NonPositiveDecodeBase is D fitted at one prompt with a base not above
	// zero: there the base stands for the whole prompt's part.
	NonPositiveDecodeBase Refusal = "NonPositiveDecodeBase"
	// TooFewHopWindows is R with fewer than MinHopWindows admitted windows.
	TooFewHopWindows Refusal = "TooFewHopWindows"
	// NonPositiveHop is an R not above zero, at one prompt length or at any
	// of a line's: the engines' means already hold the router's.
	NonPositiveHop Refusal = "NonPositiveHop"
	// NoRouterMean is R with no router TTFT _sum and _count read at all.
	NoRouterMean Refusal = "NoRouterMean"
	// FitNotSettled is a fit that stands but has not for FitSetCycles: the
	// fall-back is still priced.
	FitNotSettled Refusal = "FitNotSettled"
)

// DecodeFit is D as learned: base + perToken*I + perStep*ITL, seconds.
type DecodeFit struct {
	// Base is the intercept, seconds. With HeldPromptTokens it is the prompt's
	// part too.
	Base float64
	// PerToken is seconds per prompt token; 0 with HeldPromptTokens.
	PerToken float64
	// PerStep is TTFT seconds per second of ITL.
	PerStep float64
	// HeldPromptTokens is the shortest prompt the minutes read, set when they
	// did not span MinPromptSpread: no perToken can be told from Base, and the
	// caller scales Base by the prompt from there (whatif.PromptLine). 0 with
	// a PerToken fitted.
	HeldPromptTokens float64
	// Minutes is the admitted pod-minutes.
	Minutes int
	// Refused says why no fit stands; "" with one.
	Refused Refusal
}

// HopFit is R as learned: Base + PerToken*I, seconds.
type HopFit struct {
	// Base is the intercept, seconds, and with HeldPromptTokens the median R.
	Base float64
	// PerToken is seconds per prompt token; 0 with HeldPromptTokens.
	PerToken float64
	// HeldPromptTokens is the shortest prompt read, set with too few prompt
	// lengths for a line. 0 with PerToken fitted.
	HeldPromptTokens float64
	// Windows is the admitted router windows.
	Windows int
	// Refused says why no R stands; "" with one.
	Refused Refusal
}

// HeldDecode is the decode engine's TTFT on the window's quiet rows: the
// largest of the last HeldDecodeAge, as a pair with its prompt and step time.
// Quiet is what admits a row to the fit, so it holds no slot wait, which the
// model prices apart, and no speculating row, whose AvgITL is a step and not a
// token. Zero with no such row.
type HeldDecode struct {
	Seconds, PromptTokens, ITLSeconds float64
}

// DecodeTermEstimate is what a decode variant's window has learned.
type DecodeTermEstimate struct {
	Decode DecodeFit
	Hop    HopFit
	// Held is what stands in for D with no fit.
	Held HeldDecode
	// WholePromptTokens is the decode rows' mean whole prompt on the last
	// cycle that read one, request-weighted; 0 with none.
	WholePromptTokens float64
}

// DecodeTermCycle is what one cycle gives a window.
type DecodeTermCycle struct {
	Now time.Time
	// Prefill and Decode are the rows of the model's prefill variants and of
	// the one decode variant.
	Prefill, Decode []domain.ReplicaMetrics
	// PromptTokens is the model's E[I_eff] of the cycle, the prompt of the
	// router's window; D reads each row's own prompt of the minute. 0 admits
	// nothing.
	PromptTokens float64
	// Router is the router's latency of the cycle; nil with none read.
	Router *domain.RouterLatency
	// Settled is nil, or the settle tracker's reading of a row: a replica
	// still warming up is not admitted.
	Settled *Settle
	// RouterHeld says the router held requests this cycle: a router window
	// over it holds a flow-control wait, which is not the hop.
	RouterHeld bool
}

// decodeSample is one pod-minute of a decode row.
type decodeSample struct {
	at                time.Time
	ttft, itl, prompt float64
}

// engineCycle is one cycle's engine means, for the router's window.
type engineCycle struct {
	at                    time.Time
	prefill, decode, rate float64
	prompt                float64
	held                  bool
}

// hopSample is one router window's R.
type hopSample struct {
	at        time.Time
	r, prompt float64
}

// DecodeTermWindow is one decode variant's readings of D and R. It holds no
// persisted state, as the rest of the package does not, and takes its lock from
// its owner.
type DecodeTermWindow struct {
	lastAt  map[string]time.Time
	decode  []decodeSample
	engines []engineCycle
	hops    []hopSample
	lastHop time.Time
	// now is the last cycle seen, whole the decode rows' whole prompt there
	// and routerSeen whether a router mean was ever read.
	now        time.Time
	whole      float64
	routerSeen bool
	// accelerator and args are what the readings were taken under.
	accelerator, args string
	// decodeLatch and hopLatch say whether a fit of D and of R is priced, and
	// decodeFit and hopFit are what the last cycle priced. stoodDecode and
	// stoodHop are the last fit that stood, which a priced fit holds to while
	// refused readings count up to FitClearCycles.
	decodeLatch, hopLatch  fitLatch
	decodeFit, stoodDecode DecodeFit
	hopFit, stoodHop       HopFit
}

// fitLatch is the choice between a fit and its fall-back. It is not persisted: a
// restart comes up on the fall-back, the safe side, and needs FitSetCycles of a
// fit again, which the empty window would need more than anyway.
type fitLatch struct {
	fit bool
	// run counts the consecutive cycles against the choice.
	run int
}

// step takes one cycle's verdict and returns the choice.
func (l *fitLatch) step(admissible bool) bool {
	if admissible == l.fit {
		l.run = 0
		return l.fit
	}
	l.run++
	need := FitClearCycles
	if admissible {
		need = FitSetCycles
	}
	if l.run >= need {
		l.fit, l.run = admissible, 0
	}
	return l.fit
}

// settle fits this cycle's readings and latches each fit against its fall-back.
func (w *DecodeTermWindow) settle() {
	d, h := fitDecode(w.decode), fitHop(w.hops)
	if !w.routerSeen {
		h.Refused = NoRouterMean
	}
	dOK, hOK := d.Refused == "", h.Refused == ""
	if dOK {
		w.stoodDecode = d
	}
	if hOK {
		w.stoodHop = h
	}
	switch priced := w.decodeLatch.step(dOK); {
	case priced:
		w.decodeFit = w.stoodDecode
	case dOK:
		w.decodeFit = DecodeFit{Minutes: d.Minutes, Refused: FitNotSettled}
	default:
		w.decodeFit = d
	}
	switch priced := w.hopLatch.step(hOK); {
	case priced:
		w.hopFit = w.stoodHop
	case hOK:
		w.hopFit = HopFit{Windows: h.Windows, Refused: FitNotSettled}
	default:
		w.hopFit = h
	}
}

// NewDecodeTermWindow returns an empty window.
func NewDecodeTermWindow() *DecodeTermWindow {
	w := &DecodeTermWindow{lastAt: map[string]time.Time{}}
	w.settle()
	return w
}

// Under says what the next readings are taken under. D and R are properties of
// the engine, so a change of accelerator or of engine arguments sweeps the
// window, as the line's does (slo.linesUnderArgs): the old engine's minutes are
// neither refitted nor held as the new one's. An unknown value changes nothing.
func (w *DecodeTermWindow) Under(accelerator, args string) {
	moved := accelerator != "" && w.accelerator != "" && accelerator != w.accelerator ||
		args != "" && w.args != "" && args != w.args
	if accelerator != "" {
		w.accelerator = accelerator
	}
	if args != "" {
		w.args = args
	}
	if moved {
		*w = DecodeTermWindow{lastAt: map[string]time.Time{}, now: w.now, routerSeen: w.routerSeen,
			accelerator: w.accelerator, args: w.args}
	}
}

// Observe takes one cycle.
func (w *DecodeTermWindow) Observe(c DecodeTermCycle) {
	defer w.settle()
	w.now = c.Now
	w.prune(c.Now)
	if whole, ok := wholePrompt(c.Decode); ok {
		w.whole = whole
	}
	if !finitePositive(c.PromptTokens) {
		return
	}
	for _, rm := range c.Decode {
		itl, ttft, prompt, ok := admittedDecodeRow(rm)
		if !ok || !c.Settled.Settled(rm, c.Now) {
			continue
		}
		if at, seen := w.lastAt[rm.PodName]; seen && c.Now.Sub(at) < SampleSpacing {
			continue
		}
		w.lastAt[rm.PodName] = c.Now
		w.decode = append(w.decode, decodeSample{at: c.Now, ttft: ttft, itl: itl, prompt: prompt})
	}
	if len(w.decode) > maxDecodeSamples {
		w.decode = slices.Clone(w.decode[len(w.decode)-maxDecodeSamples:])
	}
	w.observeRouter(c)
}

// admittedDecodeRow is a row's step time and TTFT, in seconds, and its prompt of
// the same minute, when the row is admitted: serving, both means known, carrying
// requests at the prompt it states, with its slot queue read and empty (that
// wait is W_decode, priced apart; an unknown queue is not an empty one) and no
// more than MaxDecodeQueue waiting at all, and with no speculation, where AvgITL
// is a step and the line a token.
func admittedDecodeRow(rm domain.ReplicaMetrics) (itl, ttft, prompt float64, ok bool) {
	switch {
	case !Serving(rm), !rm.AvgTTFTKnown, !finitePositive(rm.AvgTTFT),
		rm.ITLState() != domain.ITLFinite, !finitePositive(rm.AvgITL),
		rm.SpecAcceptedPerStep > 0, !rm.CapacityQueueKnown, rm.CapacityQueueLength > 0, rm.QueueLength > MaxDecodeQueue:
		return 0, 0, 0, false
	}
	rate, known := requestRate(rm)
	if !known || !(rate >= MinDecodeRequestRate) || float64(rm.QueueLength)/rate > MaxDecodeQueueWait {
		return 0, 0, 0, false
	}
	recent, long := rm.AvgInputTokensRecent, rm.AvgInputTokens
	if !finitePositive(recent) || !finitePositive(long) || !(math.Abs(recent/long-1) <= MaxDecodePromptDrift) {
		return 0, 0, 0, false
	}
	return rm.AvgITL, rm.AvgTTFT, recent, true
}

// observeRouter folds the cycle's engines into the router's window and, with
// the router's mean to set them against, takes R once per SampleSpacing.
func (w *DecodeTermWindow) observeRouter(c DecodeTermCycle) {
	p, pRate, pOK := engineMean(c.Prefill)
	d, _, dOK := engineMean(c.Decode)
	if pOK && dOK {
		w.engines = append(w.engines, engineCycle{at: c.Now, prefill: p, decode: d, rate: pRate, prompt: c.PromptTokens, held: c.RouterHeld})
	}
	r := c.Router
	if r == nil || !(r.WindowSeconds > 0) || !finitePositive(r.TTFTSumRate) || !finitePositive(r.TTFTCountRate) {
		return
	}
	w.routerSeen = true
	span := time.Duration(r.WindowSeconds * float64(time.Second))
	cut := slices.IndexFunc(w.engines, func(e engineCycle) bool { return c.Now.Sub(e.at) < span })
	if cut < 0 {
		w.engines = nil
		return
	}
	w.engines = slices.Clone(w.engines[cut:])
	if !w.lastHop.IsZero() && c.Now.Sub(w.lastHop) < SampleSpacing {
		return
	}
	hop, prompt, ok := hopOver(w.engines, r.TTFTSumRate/r.TTFTCountRate, span)
	if !ok {
		return
	}
	w.lastHop = c.Now
	w.hops = append(w.hops, hopSample{at: c.Now, r: hop, prompt: prompt})
	if len(w.hops) > maxHopSamples {
		w.hops = slices.Clone(w.hops[len(w.hops)-maxHopSamples:])
	}
}

// hopOver is R over one router window: the router's mean less the engines'
// means over its cycles, each weighted by its cycle's rate. False unless the
// window is steady: cycles enough, spanning it, the prompt held to
// MaxPromptDrift, the rate to MaxHopRateRatio, and no cycle with the router
// holding requests: the router's mean starts before flow control, so such a
// window would count the queue in R, and the model prices it apart.
func hopOver(cycles []engineCycle, router float64, span time.Duration) (hop, prompt float64, ok bool) {
	if len(cycles) < MinHopCycles {
		return 0, 0, false
	}
	first, last := cycles[0], cycles[len(cycles)-1]
	if float64(last.at.Sub(first.at)) < minHopWindowShare*float64(span) {
		return 0, 0, false
	}
	loRate, hiRate, loPrompt, hiPrompt := first.rate, first.rate, first.prompt, first.prompt
	var p, d, weight, tokens float64
	for _, e := range cycles {
		if e.held {
			return 0, 0, false
		}
		loRate, hiRate = min(loRate, e.rate), max(hiRate, e.rate)
		loPrompt, hiPrompt = min(loPrompt, e.prompt), max(hiPrompt, e.prompt)
		p += e.prefill * e.rate
		d += e.decode * e.rate
		tokens += e.prompt * e.rate
		weight += e.rate
	}
	if !(loRate >= MinHopRequestRate) || !(hiRate <= MaxHopRateRatio*loRate) || !(hiPrompt <= (1+MaxPromptDrift)*loPrompt) {
		return 0, 0, false
	}
	hop = router - p/weight - d/weight
	return hop, tokens / weight, isFinite(hop)
}

// wholePrompt is the serving rows' mean one-minute prompt, weighted by request
// rate. False with no such row.
func wholePrompt(rows []domain.ReplicaMetrics) (float64, bool) {
	var weighted, weight float64
	for _, rm := range rows {
		rr, known := requestRate(rm)
		if !Serving(rm) || !known || !finitePositive(rr) || !finitePositive(rm.AvgInputTokensRecent) {
			continue
		}
		weighted += rr * rm.AvgInputTokensRecent
		weight += rr
	}
	if !(weight > 0) {
		return 0, false
	}
	return weighted / weight, true
}

// engineMean is the serving rows' mean AvgTTFT, weighted by request rate, and
// that rate's sum. False with no such row.
func engineMean(rows []domain.ReplicaMetrics) (mean, rate float64, ok bool) {
	var weighted float64
	for _, rm := range rows {
		rr, known := requestRate(rm)
		if !Serving(rm) || !rm.AvgTTFTKnown || !finitePositive(rm.AvgTTFT) || !known || !finitePositive(rr) {
			continue
		}
		weighted += rr * rm.AvgTTFT
		rate += rr
	}
	if !(rate > 0) {
		return 0, 0, false
	}
	return weighted / rate, rate, true
}

// prune drops what is older than Retention.
func (w *DecodeTermWindow) prune(now time.Time) {
	w.decode = slices.DeleteFunc(w.decode, func(s decodeSample) bool { return now.Sub(s.at) > Retention })
	w.hops = slices.DeleteFunc(w.hops, func(s hopSample) bool { return now.Sub(s.at) > Retention })
	w.engines = slices.DeleteFunc(w.engines, func(e engineCycle) bool { return now.Sub(e.at) > engineHorizon })
	maps.DeleteFunc(w.lastAt, func(_ string, at time.Time) bool { return now.Sub(at) > Retention })
}

// Estimate is what the window supports now, as of its last cycle: each fit past
// its latch, or its fall-back with the reason.
func (w *DecodeTermWindow) Estimate() DecodeTermEstimate {
	return DecodeTermEstimate{Decode: w.decodeFit, Hop: w.hopFit, Held: w.held(), WholePromptTokens: w.whole}
}

// held is the largest TTFT of the last HeldDecodeAge's quiet rows, a pair from
// one row: the largest, as S1 reads the measured figure, the side that asks for
// more.
func (w *DecodeTermWindow) held() HeldDecode {
	var top HeldDecode
	for _, s := range w.decode {
		if w.now.Sub(s.at) <= HeldDecodeAge && s.ttft > top.Seconds {
			top = HeldDecode{Seconds: s.ttft, PromptTokens: s.prompt, ITLSeconds: s.itl}
		}
	}
	return top
}

// fitDecode fits D over the admitted minutes, untrimmed. With the prompts
// spanning MinPromptSpread it is the three-term least squares; otherwise TTFT
// against ITL alone, whose intercept is then the prompt's part as well, held at
// the shortest prompt read, so that the caller's scaling reads high. A plane must
// stand above zero at the shortest prompt and step it read.
func fitDecode(samples []decodeSample) DecodeFit {
	out := DecodeFit{Minutes: len(samples)}
	if len(samples) < MinDecodeMinutes {
		out.Refused = TooFewDecodeMinutes
		return out
	}
	itl := make([]float64, len(samples))
	prompt := make([]float64, len(samples))
	ttft := make([]float64, len(samples))
	for i, s := range samples {
		itl[i], prompt[i], ttft[i] = s.itl, s.prompt, s.ttft
	}
	if spreadOf(itl) < MinStepSpread {
		out.Refused = NoStepSpread
		return out
	}
	if spreadOf(prompt) < MinPromptSpread {
		base, slope := ols(itl, ttft)
		switch {
		case !(slope > 0) || !isFinite(slope):
			out.Refused = NonPositiveStepSlope
		case !(base > 0) || !isFinite(base):
			out.Refused = NonPositiveDecodeBase
		default:
			out.Base, out.PerStep, out.HeldPromptTokens = base, slope, slices.Min(prompt)
		}
		return out
	}
	base, perToken, perStep, ok := olsTwo(prompt, itl, ttft)
	switch {
	case !ok || !(perStep > 0):
		out.Refused = NonPositiveStepSlope
	case !(perToken >= 0):
		out.Refused = NegativePromptSlope
	case !(base+perToken*slices.Min(prompt)+perStep*slices.Min(itl) > 0):
		out.Refused = NonPositiveDecodePlane
	default:
		out.Base, out.PerToken, out.PerStep = base, perToken, perStep
	}
	return out
}

// spreadOf is the second largest of xs over the second smallest: each end holds
// two minutes. 0 with fewer than two.
func spreadOf(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	s := slices.Sorted(slices.Values(xs))
	return s[len(s)-2] / s[1]
}

// olsTwo is the least-squares plane y = a + b*u + c*v. ok is false when the
// solve is singular: u and v move together, or one of them not at all.
func olsTwo(us, vs, ys []float64) (a, b, c float64, ok bool) {
	n := float64(len(us))
	var mu, mv, my float64
	for i := range us {
		mu += us[i]
		mv += vs[i]
		my += ys[i]
	}
	mu, mv, my = mu/n, mv/n, my/n
	var suu, svv, suv, suy, svy float64
	for i := range us {
		du, dv, dy := us[i]-mu, vs[i]-mv, ys[i]-my
		suu += du * du
		svv += dv * dv
		suv += du * dv
		suy += du * dy
		svy += dv * dy
	}
	det := suu*svv - suv*suv
	if !(det > minSolveDet*suu*svv) {
		return 0, 0, 0, false
	}
	b = (suy*svv - svy*suv) / det
	c = (svy*suu - suy*suv) / det
	a = my - b*mu - c*mv
	return a, b, c, isFinite(a) && isFinite(b) && isFinite(c)
}

// fitHop reads R from the admitted windows. A window that held a queue or a ramp
// its gates missed reads far off (one phase of the staircase run read a spread of
// 28 ms around a median of 12.5 ms), so each prompt length gives its median, as
// a's clusters do: two lengths or more, spanning
// MinPromptSpread, give a line through the medians; otherwise R is the median
// of all, held at the shortest prompt. A slope below zero is held too, at the
// largest median, the side that asks for more.
func fitHop(samples []hopSample) HopFit {
	out := HopFit{Windows: len(samples)}
	if len(samples) < MinHopWindows {
		out.Refused = TooFewHopWindows
		return out
	}
	prompt := make([]float64, len(samples))
	r := make([]float64, len(samples))
	for i, s := range samples {
		prompt[i], r[i] = s.prompt, s.r
	}
	var xs, ys []float64
	for _, c := range promptClusters(prompt, r) {
		if c.n >= MinQuadraticClusterMinutes {
			xs, ys = append(xs, c.x), append(ys, c.y)
		}
	}
	held := median(slices.Clone(r))
	shortest := slices.Min(prompt)
	if len(xs) >= 2 && xs[len(xs)-1] >= MinPromptSpread*xs[0] {
		if !(slices.Min(ys) > 0) {
			out.Refused = NonPositiveHop
			return out
		}
		base, slope := ols(xs, ys)
		if slope >= 0 && isFinite(base) {
			out.Base, out.PerToken = base, slope
			return out
		}
		held = slices.Max(ys)
	}
	if !(held > 0) {
		out.Refused = NonPositiveHop
		return out
	}
	out.Base, out.HeldPromptTokens = held, shortest
	return out
}
