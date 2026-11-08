package saturation

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
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
//     a request where the shape says 10,500 -- a KV-bound mu 5x low.
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

// Which bound, if any, a reading was held to (withResidentKV), as derived-mu
// logs it in residentKVBound.
const (
	// residentBoundNone is a reading inside the bounds, priced as read.
	residentBoundNone = "none"
	// residentBoundLower is a reading below the shape's KVreq, priced at it.
	residentBoundLower = "lower"
	// residentBoundUpper is a reading above the shape with no hit, priced at
	// it.
	residentBoundUpper = "upper"
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

// residentReading is the KV the fleet's running requests hold, summed, how
// many they are, the summed capacity of the replicas they run on, and the
// band's lower edge they were read at.
type residentReading struct {
	tokens    float64
	running   int
	capTokens float64
	minK      float64
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
		r.capTokens += float64(rm.TotalKvCapacityTokens)
	}
	return r
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
func withResidentKV(s shape.Shape, r residentReading) (shape.Shape, string) {
	if !(s.KVreq > 0) {
		return s, residentBoundNoReading
	}
	if !(s.PrefixHitRate > 0) {
		return s, residentBoundNoHit
	}
	noHit := noHitShape(s)
	need := float64(minResidentOf(noHit))
	if byBand := math.Ceil(r.minK * r.capTokens / s.KVreq); byBand > need {
		need = byBand
	}
	if !(float64(r.running) >= need) {
		return s, residentBoundNoReading
	}
	measured := r.perSeq()
	if !(measured > 0) || math.IsInf(measured, 0) {
		return s, residentBoundNoReading
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
