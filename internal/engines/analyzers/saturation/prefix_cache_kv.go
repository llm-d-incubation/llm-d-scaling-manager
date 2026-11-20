package saturation

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// The KV a resident request holds, when some of its prompt was a prefix-cache
// hit (issue ev-shindin/llm-scaling-manager#277).
//
// shape.New discounts the prompt by the hit rate, ILeff = IL*(1 - hit), as if
// every hit saved KV. A hit saves compute, but it saves KV only when the block
// it hits is also held by another running request: a hit on a block a
// finished request left behind makes that block the new request's own, so it
// is resident as if it had been computed. With multi-turn reuse most hits are
// of that kind, and pricing the resident request at ILeff + D*OL/2 then
// over-counts the sequences a cache holds, up to about 4x. The derived mu
// (whatif.DeriveMuAtCap) sizes its sequence count by that figure, so an over-counted
// count over-states mu and under-orders replicas.
//
// The engine says how many requests it holds (vllm:num_requests_running,
// sglang:num_running_reqs; domain.ReplicaMetrics.RunningRequests), and its
// cache says how many tokens they hold (vLLM counts a block once however many
// running requests share it, and a cached block no running request holds as
// free), so the KV per resident request can be measured: the tokens in use
// over the running requests, over the fleet's Ready generating replicas
// (fleetResidentKVPerRequest). Where there is a reading it replaces the
// assumed discount; where there is none the request is priced at
// ILeff + D*OL/2, as before.
//
// The reading is bounded below by the shape's own KVreq, ILeff + D*OL/2, and
// above by the same shape with no hit, IL + D*OL/2. So it moves the price
// only towards the measured residency, and never below the discount: the
// derived mu is never above the one before, and without evidence it is the
// one before. Outside the bounds:
//
//   - below: requests part way through their prefill (resident, their prompt
//     not yet all written), or a spread of output lengths D under-states. Not
//     taken as more sharing than the hit rate allows.
//   - above: a prompt length that lags the workload, which the shape tracker
//     answers; or, on the decode side of a prefill/decode split, KV that
//     vLLM allocates for a request as it starts pulling the prompt's KV from
//     prefill, while the request waits (WAITING_FOR_REMOTE_KVS) and is not
//     counted as running. Under transfer load that numerator holds KV no
//     running request does, and the reading moves towards no discount. No
//     gauge counts those blocks, so it is not subtracted; it errs towards
//     more replicas.
//
// A reading is taken only where the requests read are enough of what the
// pricing point holds for the reading to describe it. A prefix several
// running requests share is held once and spread over the requests running
// now, so few requests read each one's share of it high: 500 shared and 500
// unique prompt tokens with 100 out, at hit 0.5, read 800 a request at 2
// running and 554 at the 123 the pricing point holds, where the shape says
// 550. Two gates, both the ITL band's lower edge
// (analyzerTuning.itlBand.minObservableK, minK below):
//
//   - by fill: only replicas whose KV fill is at least minK are read. The band
//     is where the line mu is priced from is fitted.
//   - by count: the requests read must be at least minK x C / KVreq, C the
//     summed capacity of the replicas read and KVreq the shape's own, with
//     the hit rate's discount (withResidentKV). That is the band's edge in
//     requests: the count a cache filled to minK holds if the discount is
//     right. The fill gate alone does not bound it where prompts are long
//     against the cache: a 500,000-token cache with 100,000-token prompts,
//     90% of them shared, holds 2 requests at a fill of 0.22 and reads 55,500
//     a request where the shape says 10,500 -- a KV-bound mu 5x low. With
//     SwitchKVPerRequestAboveContext on, KVreq here is the reading where it
//     is above the whole context (countGateKV); this case reads below it and
//     stays no reading.
//
// The count gate costs the issue's own case some of its reach. Where the hits
// are on blocks no running request shares, a request holds IL + D*OL/2 and a
// cache at fill k holds k x C / (IL + D*OL/2) of them, so the count gate
// passes only from a fill of minK x (IL + D*OL/2) / (ILeff + D*OL/2): 0.30 at
// a discount that halves the request, 0.60 at one that quarters it, and
// above the pricing point (0.68 of the cache on the defaults) at one that
// cuts it more than about 4.5x. There the price stays the
// discount, as before; a line in the running count that separates the shared
// KV from each request's own would read both (a follow-up).
//
// The noise a reading keeps is one-sided: a reading below the shape's KVreq
// is clamped to it and changes nothing, while one above moves the price. The
// spread of prompt lengths is not modelled (minResidentSequences models the
// age of the generated part only), so on long prompts read over few requests
// the reading errs towards more replicas, at most to the price with no
// discount.
//
// The reading is taken while a shape change is outstanding too. The requests
// resident then belong to both shapes, but each shape that is priced bounds
// the reading by its own ILeff and IL (withResidentKV), and the short-window
// shape is the new one, so a mixed population moves the price no further
// than between the new shape's discount and none.
//
// SGLang publishes both readings as the busiest rank's (max by pod), against
// the pod's whole KV capacity, so on a pod running data-parallel attention
// ranks the reading would over-read by about the rank count and land on the
// upper bound. That is not verified on a cluster; it errs towards more
// replicas, at most to the price with no discount.
//
// It moves the derived mu and the GPS check that predicts a token rate from
// the same KV per request (noteLineMismatch). It does not move the prompt
// length prefill's throughput key is discounted by (throughputKeyInput): a hit
// does save prefill compute. Nor the derived k2 or the zero-replica estimate,
// which price the full prompt already (estimateCapacityAtCapSpread).

// SwitchKVPerRequestHitsResident is the replay switch for prefix-cache hits
// that stay resident (issue ev-shindin/llm-scaling-manager#277). On, the derived mu and the GPS check price
// a resident request at the KV the fleet's running requests are measured to
// hold, between ILeff + D*OL/2 and IL + D*OL/2, and at ILeff + D*OL/2 with no
// reading. Off, at ILeff + D*OL/2 throughout.
const SwitchKVPerRequestHitsResident = "kv-per-request-hits-resident"

// SwitchKVPerRequestAboveContext is the replay switch for a reading above the
// whole context (issue llm-d-incubation/llm-d-scaling-manager#188). An engine
// can hold more KV per request than its prompt and output in the same pool (a
// sparse-attention indexer's keys, a speculative draft model's KV), and the
// upper bound above then under-prices a request by that factor and over-states
// a KV-bound mu by the same. On, a reading is priced by the capacity planner's
// rule (shape.PriceResident): held at the discount from below, priced as read
// above the context, except on SGLang, held at the context; a shape with no
// hit takes a reading too. Off, as SwitchKVPerRequestHitsResident prices it.
// It needs SwitchKVPerRequestHitsResident on, and moves nothing without it.
//
// Off from the constructor: the controller keeps the bound until a GPU check
// on a prefix-heavy P/D workload shows decode is not under-sized with it gone.
const SwitchKVPerRequestAboveContext = "kv-per-request-above-context"

// Which bound, if any, a reading was held to (withResidentKV), as derived-mu
// logs it in residentKVBound.
const (
	// residentBoundNone is a reading inside the bounds, priced as read.
	residentBoundNone = "none"
	// residentBoundLower is a reading below the shape's KVreq, priced at it.
	residentBoundLower = "lower"
	// residentBoundUpper is a reading above the shape with no hit, priced at
	// it: with SwitchKVPerRequestAboveContext off, or on an engine that
	// over-reads there (residentRule.heldAtContext).
	residentBoundUpper = "upper"
	// residentBoundAboveContext is a reading above shape.AboveContextFactor
	// times the shape with no hit, priced as read
	// (SwitchKVPerRequestAboveContext).
	residentBoundAboveContext = "above-context"
	// residentBoundNoReading is no reading, or too few requests in it
	// (minResidentSequences, or the band's count gate): priced at the
	// shape's KVreq.
	residentBoundNoReading = "no-reading"
	// residentBoundNoHit is a shape with no prefix-cache hit: its two bounds
	// are one figure, and the reading cannot move it.
	residentBoundNoHit = "no-hit"
	// residentBoundOff is SwitchKVPerRequestHitsResident off.
	residentBoundOff = "off"
)

// residentSpreadTolerance is the relative spread, one standard deviation of
// the mean over the requests read, that a reading may carry.
const residentSpreadTolerance = 0.05

// minResidentFloor is the fewest running requests a reading is taken from,
// whatever the shape: one request is one age, not a fleet's.
const minResidentFloor = 2

// minAboveContextSequences is the fewest requests a reading above the whole
// context is taken from (SwitchKVPerRequestAboveContext). The shape is the
// mean of the requests that completed, not of the ones running now, and a
// few running requests can be the long tail of a mixed workload: three
// 40,000-token prompts in a shape of 10,000 read 40,500 a request, four
// times the context, with no KV held beyond prompt and output. Below this
// count one or two long prompts are a quarter of the sample. Above it the
// spread is not bounded either: k prompts L times the shape's among n lift
// the reading by about k x (L - 1) / n, and nothing in the scrape tells them
// apart. A running count that lags the KV it is read against (6 of 20 at
// the scrape) is caught the same way only when it is small. On a day-long
// P/D recording every reading came from 12 or more requests.
const minAboveContextSequences = 8

// residentReading is the KV the fleet's running requests hold, summed, how
// many they are, the summed capacity of the replicas they run on, and the
// band's lower edge they were read at. holding is the requests that hold KV
// without running, where the replica says (holdingNotRunning). variant is
// the variant of the replicas read, and mixed says they were of more than
// one (k2ResidentKV reads it).
type residentReading struct {
	tokens    float64
	running   int
	holding   int
	capTokens float64
	minK      float64
	variant   string
	mixed     bool
}

// holders is the requests the tokens in use are spread over under rule:
// the running ones, and with rule.aboveContext also those that hold KV
// without running (residentReading.holding). Off, the running ones, as
// before.
func (r residentReading) holders(rule residentRule) int {
	if rule.aboveContext {
		return r.running + r.holding
	}
	return r.running
}

// perHolder is the KV a request holds under rule (holders), or 0 with none.
func (r residentReading) perHolder(rule residentRule) float64 {
	n := r.holders(rule)
	if n <= 0 {
		return 0
	}
	return r.tokens / float64(n)
}

// holdingNotRunning is the requests on rm that hold KV and are not running:
// on the decode side of a prefill/decode split, those waiting for their KV
// from prefill (WAITING_FOR_REMOTE_KVS) and those whose KV has landed and wait
// for the next step. vLLM allocates their blocks before the pull, and counts
// them in num_requests_waiting, not running (the file comment). They are the
// waiting queue less the requests waiting for capacity, which hold none; 0
// where the replica does not split its queue by reason.
func holdingNotRunning(rm domain.ReplicaMetrics) int {
	if !rm.CapacityQueueKnown {
		return 0
	}
	return max(0, rm.QueueLength-rm.CapacityQueueLength)
}

// perSeq is the KV a running request holds, or 0 with none running.
func (r residentReading) perSeq() float64 {
	if r.running <= 0 {
		return 0
	}
	return r.tokens / float64(r.running)
}

// noHitShape is s with no prefix-cache hit, its spread kept: the KV a
// resident request holds when no hit saves any. A literal's
// OutputDispersion of 0 reads as 1 (WithOutputDispersion).
func noHitShape(s shape.Shape) shape.Shape {
	return shape.New(s.AvgInputTokens, s.AvgOutputTokens, 0).WithOutputDispersion(s.OutputDispersion)
}

// minResidentSequences is the fewest running requests a reading is taken from
// for shape s, for the spread of their ages (minResidentOf).
func minResidentSequences(s shape.Shape) int {
	return minResidentOf(noHitShape(s))
}

// MinResidentSequences is the fewest running requests a resident reading is
// taken from for shape s (minResidentSequences): the capacity planner's own
// reading (learnreplay.ResidentKV) takes the same gate.
func MinResidentSequences(s shape.Shape) int {
	return minResidentSequences(s)
}

// minResidentOf is the fewest running requests a reading is taken from for a
// shape whose KV per request with no hit is noHit. Each resident request is
// caught at its own age, so its generated part is spread over [0, OL] (D x OL
// with a spread of lengths), a standard deviation of about D x OL / sqrt(12).
// Over n requests the mean's spread falls as 1/sqrt(n), so n is the least
// that brings it within residentSpreadTolerance of noHit's KVreq, and at
// least minResidentFloor. A shape whose figures are not finite asks for
// math.MaxInt32, which no fleet reaches.
//
// The fewest is a property of the shape, not a fixed count: at IL = OL it is
// 15 requests, at IL = 10 OL it is 2. A fixed count large enough for the
// first was never reached by a fleet of one or two whole-node pods serving
// long prompts. Below the fewest the price is the shape's own, as before, so
// the count only decides when the measurement starts.
func minResidentOf(noHit shape.Shape) int {
	if !(noHit.KVreq > 0) {
		return minResidentFloor
	}
	sd := noHit.OutputDispersion * noHit.AvgOutputTokens / math.Sqrt(12)
	ratio := sd / (residentSpreadTolerance * noHit.KVreq)
	n := math.Ceil(ratio * ratio)
	if !(n < math.MaxInt32) {
		return math.MaxInt32
	}
	return max(int(n), minResidentFloor)
}

// fleetResidentKVPerRequest is the KV the fleet's running requests hold: the
// tokens in use, the running requests and the capacity, summed over the
// Ready generating replicas that read both now and whose KV fill is between
// minK, the ITL band's lower edge, and 1 (see the file comment).
//
// Both halves are instant readings of the same scrape (KvUsageInstant,
// RunningRequests), so they describe the same requests. A replica lent by the
// warm pool is not counted, as the ITL line counts none (noteITL), and nor is
// one whose live capacity is unread or the collector's overflow sentinel
// (whatif.CapacityTokens): its tokens in use are unknown.
func fleetResidentKVPerRequest(replicas []domain.ReplicaMetrics, rolesByVariant map[string]string, minK float64) residentReading {
	r := residentReading{minK: minK}
	for _, rm := range replicas {
		if !rm.Ready || rm.FromWarmPool || !generatesOutput(rm, rolesByVariant) {
			continue
		}
		if !rm.RunningRequestsKnown || !rm.KvUsageInstantKnown || rm.RunningRequests <= 0 {
			continue
		}
		if rm.TotalKvCapacityTokens <= 0 || rm.TotalKvCapacityTokens == math.MaxInt64 {
			continue
		}
		if !(rm.KvUsageInstant >= minK && rm.KvUsageInstant <= 1) {
			continue
		}
		r.tokens += rm.KvUsageInstant * float64(rm.TotalKvCapacityTokens)
		r.running += rm.RunningRequests
		r.holding += holdingNotRunning(rm)
		r.capTokens += float64(rm.TotalKvCapacityTokens)
		if r.variant == "" {
			r.variant = rm.VariantName
		} else if r.variant != rm.VariantName {
			r.mixed = true
		}
	}
	return r
}

// residentRule is how withResidentKV prices a reading above the whole
// context.
type residentRule struct {
	// aboveContext is SwitchKVPerRequestAboveContext: a reading above the
	// whole context is priced as read (shape.PriceResident), not at the
	// context, and a shape with no hit takes a reading too.
	aboveContext bool
	// heldAtContext says the engine's reading over-states above the context
	// (SGLang), so it is held there whatever aboveContext says.
	heldAtContext bool
}

// residentRuleFor is the rule a replica on params is priced by.
func (a *SaturationAnalyzer) residentRuleFor(params *capacity.EngineParams) residentRule {
	return residentRule{
		aboveContext:  a.kvAboveContext,
		heldAtContext: params != nil && params.Engine == inferenceengine.EngineSGLang,
	}
}

// countGateKV is the KV a request is taken to hold by the count gate, minK x
// the capacity read / countGateKV (the file comment): s's own KVreq, or with
// rule.aboveContext the reading itself where it is above s's whole context.
//
// The count gate is there for few requests sharing a prefix, which each read
// a large share of it. That share is at most the prompt, so on its own it
// lifts a reading to the context and no further: a reading at or below the
// context keeps the gate at KVreq, on or off, and few long requests sharing a
// prefix stay no reading. A reading above the context is KV the engine holds
// beyond the prompt and output, the case SwitchKVPerRequestAboveContext
// prices. A gate at KVreq asks a cache filled to k for k x C / KVreq
// requests, and one that holds R times that per request has k / R x C /
// KVreq: it passes only from a fill of minK x R, so at R above 1 / minK no
// fill passes. A gate at the context fails the same way, from a fill of minK
// x m / context. Above the context the gate is taken at the reading, so only
// the fill gate applies there. The rule steps at the context: with few
// requests, a reading just above it is taken where one just below is not.
func countGateKV(s shape.Shape, r residentReading, rule residentRule) float64 {
	if rule.aboveContext {
		if m := r.perHolder(rule); m > s.Context() && m > s.KVreq && !math.IsInf(m, 0) {
			return m
		}
	}
	return s.KVreq
}

// withResidentKV is s with its KVreq set to the KV a resident request is
// measured to hold, bounded by s's own KVreq and the KVreq of s with no hit,
// and which bound applied: residentBoundNone (priced as read),
// residentBoundLower (below the discount: s as it was), residentBoundUpper
// (priced with no discount), residentBoundNoHit (s has no hit: the bounds
// are one figure) or residentBoundNoReading (s as it was). It is no reading
// with fewer requests than minResidentOf asks for s, or than the band's
// count gate asks: minK x the capacity read / s.KVreq (the file comment).
// Only KVreq moves; ILeff stays the hit-discounted prompt the shape's other
// readers take.
//
// With rule.aboveContext (SwitchKVPerRequestAboveContext) the upper bound
// goes: the reading is priced by shape.PriceResident, the capacity planner's
// rule. Above the context it is priced as read (residentBoundNone, or
// residentBoundAboveContext past shape.AboveContextFactor times it), except
// on an engine that over-reads there (rule.heldAtContext), held at the
// context (residentBoundUpper). A shape with no hit is read as well: its
// discount is its context, and an engine can hold more than that. The count
// gate is then taken at the KV a request is read to hold (countGateKV). The
// tokens are spread over the requests that hold KV without running as well
// (residentReading.holders): a decode pulling 30 prompts beside 4 running
// would read 8 times the context over the running 4. A reading above the
// context needs minAboveContextSequences of them.
func withResidentKV(s shape.Shape, r residentReading, rule residentRule) (shape.Shape, string) {
	if !(s.KVreq > 0) {
		return s, residentBoundNoReading
	}
	if !rule.aboveContext && !(s.PrefixHitRate > 0) {
		return s, residentBoundNoHit
	}
	noHit := noHitShape(s)
	need := float64(minResidentOf(noHit))
	if byBand := math.Ceil(r.minK * r.capTokens / countGateKV(s, r, rule)); byBand > need {
		need = byBand
	}
	n := r.holders(rule)
	if !(float64(n) >= need) {
		return s, residentBoundNoReading
	}
	measured := r.perHolder(rule)
	if !(measured > 0) || math.IsInf(measured, 0) {
		return s, residentBoundNoReading
	}
	if rule.aboveContext {
		if measured > s.Context() && n < minAboveContextSequences {
			return s, residentBoundNoReading
		}
		kv, how := s.PriceResident(measured, rule.heldAtContext)
		s.KVreq = kv
		switch how {
		case shape.ResidentAtDiscount:
			return s, residentBoundLower
		case shape.ResidentHeldAtContext:
			return s, residentBoundUpper
		case shape.ResidentAboveContext:
			return s, residentBoundAboveContext
		default:
			return s, residentBoundNone
		}
	}
	switch {
	case measured < s.KVreq:
		return s, residentBoundLower
	case measured > noHit.KVreq:
		s.KVreq = noHit.KVreq
		return s, residentBoundUpper
	default:
		s.KVreq = measured
		return s, residentBoundNone
	}
}
