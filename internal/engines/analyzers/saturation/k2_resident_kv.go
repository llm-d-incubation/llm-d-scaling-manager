package saturation

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// SwitchK2DerivedAtResidentKV is the replay switch for the KV a resident
// request holds in the derived k2 (P3; issue
// llm-d-incubation/llm-d-scaling-manager#201, the follow-up to PR #205).
//
// The derived k2 is the requests one engine rank holds in steady state times
// the KV each one holds, and it prices each at the context, I + D x O/2
// (estimateCapacityAtCapBudget). Demand is the KV the replica's requests
// actually hold. An engine that holds more KV per request than its context
// (a sparse-attention indexer's keys, a speculative draft model's KV; issue
// #188) fills its cache at the same request count with that many more
// tokens, so its k2 is that many times too low against demand, and the
// fleet is ordered that many times the replicas. On a day-long P/D
// recording the resident KV per running request was 2.7-3.8x the context
// (p5-p95). With SwitchKVPerRequestAboveContext the reading is per request
// holding KV, the running ones and those pulling or holding landed KV, a
// little lower; the derived batch counts running requests only, so k2 errs
// low by that share.
//
// On, the derived k2 prices each request at the fleet's resident reading
// (fleetResidentKVPerRequest), as SwitchKVPerRequestAboveContext prices it
// for mu, where all of these hold (k2ResidentKV):
//
//   - SwitchKVPerRequestHitsResident and SwitchKVPerRequestAboveContext are
//     on. The reading is trusted above the context only behind the gates
//     that switch adds: the count gate taken at the reading, at least
//     minAboveContextSequences requests, and the requests that hold KV
//     without running counted among them. Without it the reading is held at
//     the context, which is what the derived k2 prices already.
//   - The reading was taken and priced as read: residentBoundNone or
//     residentBoundAboveContext, not held at either bound and not refused.
//   - It is above the fleet's whole context. A reading below it is prefix
//     sharing, which the derived k2 has never priced; this switch leaves it.
//   - No shape change is outstanding: the requests resident then belong to
//     two shapes, and the derived k2 prices no spread either.
//   - Every replica read is of the variant being priced. The reading pools
//     the model's generating replicas, and a variant whose engine holds only
//     its context would otherwise be priced at a sibling's extra KV, and
//     ordered too few replicas. (The derived mu pools them still.)
//
// It only raises k2, and never raises it above k1: the figure is the larger
// of the k2 priced at the context and the k2 priced at the reading, the
// latter held at k1 (k2AtResidentKV). The raised figure prices the cycle
// only: the capacity store keeps the figure at the context, so the
// zero-replica estimate, a sibling's lookup and the fallback path, which
// read the store with no reading behind them, do not move. The lower bound
// (P2-borrow, P2-prior) is judged against the figure at the context, and
// where it applies the reading prices nothing. Anywhere else it is priced
// at the context, as before. The prefill role has no derived k2 and does
// not move.
//
// The gate compares the reading with the fleet's context, at the fleet's
// spread of output lengths; the derived k2 prices the replica's own. A
// reading between the two passes the gate and then prices nothing, since
// the figure is never lowered.
//
// The reading reaches the derived k2 as a line in the resident requests
// (residentKVLine), held once plus per request, with nothing held once: the
// fleet's ratio cannot separate a shared prefix from each request's own KV.
// Issue #52's fit of the KV in use against the running count would fill
// both parts, and price the same way.
//
// Not yet safe beside SwitchK2DecodeBudgetAtComputedPrompt. On the day-long
// recording, with both on, k2 rose 2.8x at the median and the decode role
// was asked 7-10 replicas in 10 of 25 hours on a fleet of 11 that served
// the traffic. The reading is not what over-prices there: the steady batch
// at the budget is 16-19 requests a rank, where the ranks ran 2-6. Priced
// at the context, the low KV a request hid that batch.
//
// Off from the constructor: on, a decode is priced at more capacity and
// fewer replicas are ordered, and the default changes only after a GPU
// check, with SwitchKVPerRequestAboveContext's.
const SwitchK2DerivedAtResidentKV = "k2-derived-at-resident-kv"

// residentKVLine is the KV one engine rank holds as a line in the requests
// resident on it: held once whatever the count (a prefix every request
// shares), plus perRequest for each. The fleet's resident reading is a ratio
// and cannot tell the two apart, so it is all perRequest, held 0; a fit of
// the KV in use against the running count (issue
// llm-d-incubation/llm-d-scaling-manager#52) gives both. Both are per
// engine rank: a fit against a replica's running count gives a replica's
// held part, which is divided by its ranks before it lands here. The zero
// value is no reading: the derived k2 prices the context.
type residentKVLine struct {
	held       float64
	perRequest float64
}

// known reports whether l is a reading the derived k2 may price at.
func (l residentKVLine) known() bool {
	return l.perRequest > 0 && !math.IsInf(l.perRequest, 0) && l.held >= 0 && !math.IsInf(l.held, 0)
}

// kvAt is the KV one engine rank holds with n requests resident on it.
func (l residentKVLine) kvAt(n float64) float64 {
	return l.held + l.perRequest*n
}

// k2ResidentKV is the KV line the derived k2 prices a replica of variant at
// under SwitchK2DerivedAtResidentKV: fleetShape's KVreq as withResidentKV
// priced it from read, with bound the bound it reported, all of it per
// request. It is the zero value, the context, unless every condition the
// switch's comment lists holds.
func (a *SaturationAnalyzer) k2ResidentKV(fleetShape shape.Shape, bound string, shapeChanging bool,
	read residentReading, variant string) residentKVLine {
	if !a.k2AtResidentKV || !a.kvHitsResident || !a.kvAboveContext || shapeChanging {
		return residentKVLine{}
	}
	if read.mixed || read.variant != variant {
		return residentKVLine{}
	}
	if bound != residentBoundNone && bound != residentBoundAboveContext {
		return residentKVLine{}
	}
	kv := fleetShape.KVreq
	if math.IsInf(kv, 0) || !(kv > fleetShape.Context()) {
		return residentKVLine{}
	}
	return residentKVLine{perRequest: kv}
}

// derivedBatch is the requests one engine rank holds in steady state at
// batch cap S under params, its token budget spent as budget says
// (steadyBatch): S where params state no budget. 0 where the derived k2 has
// nothing to price (estimateCapacityAtCapBudget).
func derivedBatch(params *capacity.EngineParams, batchCap int64, avgInput, avgOutput float64, budget stepBudget) float64 {
	if batchCap <= 0 || avgOutput <= 0 || (params != nil && params.EffectiveMaxBatchedTokens <= 0) {
		return 0
	}
	if params == nil {
		return float64(batchCap)
	}
	return steadyBatch(float64(params.EffectiveMaxBatchedTokens), float64(batchCap), avgInput, avgOutput, budget)
}

// k2AtResidentKV is the derived k2 of a replica of ranks engine ranks with
// the KV its resident requests hold read off batchCap.resident, not priced
// at their context (SwitchK2DerivedAtResidentKV): each rank's steady batch
// on the line, times the ranks. Held at k1, and never below atContext, the
// figure priced at the context. It is atContext with no line or no batch.
func k2AtResidentKV(params *capacity.EngineParams, batchCap pricedBatchCap, avgInput, avgOutput float64,
	ranks int, k1, atContext int64) int64 {
	if !batchCap.resident.known() {
		return atContext
	}
	n := derivedBatch(params, batchCap.perRank, avgInput, avgOutput, batchCap.budget)
	if !(n > 0) {
		return atContext
	}
	at := batchCap.resident.kvAt(n) * float64(max(ranks, 1))
	if !(at < float64(k1)) {
		return max(atContext, k1)
	}
	return max(atContext, int64(at))
}
