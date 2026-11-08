package whatif

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// The demand floor's mu is a request rate, and a request rate is not a property
// of a replica -- it is a property of a replica AND a shape. One replica
// generating G tokens a second completes G/O requests a second, so halving the
// generation length doubles mu with nothing about the hardware having changed.
// Measuring that per output-length bucket is a workaround for the units, and it
// cannot be made to work: the reading is only taken while a replica is
// saturated, and a fleet over-provisioned for the shape it has just moved to
// never saturates, so it never prices that shape.
//
// Measured on the 1k/6000 -> 8k/1000 swap of 2026-09-23. Nineteen minutes after
// the switch not one reading had been taken in the bucket the fleet was
// serving, and the floor was still pricing 6 req/s against the 6000-token
// phase's figure. Which figure it inherited was luck: 1.2754 on that run
// against 1.5429 on the one before, implying 4.70 and 3.89 replicas from an
// identical workload. The fleet held 7 while occupancy read a utilization
// of 0.028.
//
// Derived instead, over the model the throughput analyzer already fits and
// signals/itl now carries the arithmetic for:
//
//	muTok = itl.TokenRate(model, kSat, C, KVreq)   tokens/s at saturation
//	muReq = muTok / O                              what the floor prices with
//
// Nothing in it is keyed by a bucket and nothing waits for a saturated cycle:
// (A, B) is fitted from (k, ITL) pairs a replica reports at ANY load, and I
// and O are this cycle's own shape. A shape change reprices the fleet on the
// cycle it is observed rather than whenever the fleet next happens to saturate.
//
// It reproduces a measurement it was never given, which is the reason to
// believe it: on the trace's first shape it derives 1.487 req/s where the fleet
// measured 1.4292 and 1.5429 for itself, and on the second -- which the fleet
// never measured at all -- 4.200 req/s, asking for 1.43 replicas at the 6 req/s
// arriving. Occupancy independently said one.
//
// docs/proposals/shape-shift-treatment.md sets this out in full.

// DerivedMu is one variant's mu, priced from its fitted ITL model rather than
// from a saturated reading. OK is false when the variant has no model yet, in
// which case the caller keeps the measured window.
type DerivedMu struct {
	// Rate is the requests per second one replica completes at the pricing
	// point: TokenSec over the output length.
	Rate float64
	// Seqs is the sequences resident there, after the batch cap.
	Seqs float64
	// TokenSec is the generation tokens per second those sequences produce.
	TokenSec float64
	// OK is true when the three figures above are a price.
	OK bool
	// Why says, when OK is false, which term was missing: no-line (no ITL
	// model), no-shape (no output length or KV footprint), k-price (pricing
	// point outside (0, 1]), no-seqs (no sequence fits), no-itl (the line gives
	// no ITL there) or no-rate. A caller that declines to price for a reason
	// of its own records that reason here too.
	Why string
	// CapAtMeasured says the figure may not price above a measured rate of
	// the variant's own (an aggregated variant's lifted figure, issue #275):
	// where one is on record, the lower of the two prices the replica.
	// DeriveMuAtCap never sets it; the caller that lifts the figure does.
	CapAtMeasured bool
}

// CapacityTokens is the replica's whole KV capacity in tokens: the LIVE
// reading first, the deployment flag only where there is no live reading.
//
// The precedence used to be the other way round, which inverted the capacity
// store's own rule -- LoadFromScaleTarget keeps a live record while the engine
// runs the arguments it was measured on, and folds TotalKvTokensOverride in
// only on the deployment-derived path.
// SGLang's --max-total-tokens is parsed off the Deployment into EngineParams
// for every record, live or not, so as an override it displaced the engine's
// own reported capacity on every cycle of every SGLang variant.
//
// The direction of that error is the one that hurts: the engine clamps
// --max-total-tokens to the memory it actually has, so a flag it could not
// honour over-states C, which over-states the resident sequence count, which
// over-states mu -- and an over-stated mu UNDER-orders replicas. A flag is a
// fallback for a replica reporting nothing at all (a cold variant, or an
// engine that emits no cache_config_info); it is not a measurement.
//
// ranks is how many engine ranks the replica runs (domain.ReplicaMetrics.Ranks).
// The live reading is already the whole replica's; the flag sizes ONE engine,
// so it is multiplied by the ranks.
func CapacityTokens(params *capacity.EngineParams, ranks int, liveTokens int64) float64 {
	// math.MaxInt64 is the collector's overflow SENTINEL, not a capacity
	// (internal/collector/attribute.go). Taken literally it makes the resident
	// sequence count astronomical, which prices mu at an absurd figure and --
	// while the check below was a gate -- rejected the replica's line on every
	// cycle forever.
	if liveTokens > 0 && liveTokens < math.MaxInt64 {
		return float64(liveTokens)
	}
	if params != nil && params.TotalKvTokensOverride > 0 {
		return float64(params.TotalKvTokensOverride) * float64(max(ranks, 1))
	}
	return 0
}

// PricingK is the KV utilization, as a fraction of PHYSICAL capacity, that a
// replica is considered full at -- the point mu is priced for -- given a
// tier's kvCacheThreshold and scaleUpThreshold and the band its line is fitted
// over.
//
// It is the product of the two thresholds because they compound: k1, the
// memory-bound capacity, is kvCacheThreshold of physical KV, and the analyzer
// scales out at scaleUpThreshold of k1. On the defaults (0.80, 0.85) that is
// 0.68 of physical; with scaleUpThreshold at 0.95 it is 0.76. Both sit inside
// the ITL window's band -- the policy's analyzerTuning.itlBand.minObservableK
// and analyzerTuning.itlBand.maxObservableK, 0.15 and 0.80 by default -- so
// the line is evaluated where it was fitted rather than beyond it. A product
// that is not positive (an unset threshold) prices at itl.DefaultKSat.
//
// A configuration whose product falls outside that range is clamped to it: the
// model says nothing useful outside the range it was fitted on, and a clamped
// price is a worse answer than an extrapolated one only in theory.
//
// A band that breaks FillBand's precondition (inverted, NaN at either end, or
// an end outside (0, 1)) is replaced by DefaultBand. Clamped to an inverted
// band, the answer would be whichever edge the last clamp applied; the policy
// never passes one, since its validation resets such a band to the default.
func PricingK(kvCacheThreshold, scaleUpThreshold float64, band FillBand) float64 {
	if !band.valid() {
		band = DefaultBand()
	}
	k := itl.DefaultKSat
	if kvCacheThreshold*scaleUpThreshold > 0 {
		k = kvCacheThreshold * scaleUpThreshold
	}
	// Clamped on every path, including the fallback. DefaultKSat is 0.85 and
	// the default band tops out at 0.80, so returning the fallback unclamped
	// evaluated the line 0.05 outside the range it was fitted over -- the
	// exact thing this function exists to prevent.
	return min(max(k, band.Lo), band.Hi)
}

// DeriveMuAtCap prices one replica of a variant at saturation under the
// shape it is serving now, with its resident sequences capped at batchCap per
// engine rank.
//
// kvPerRequest is the time-averaged footprint ILeff + D*OL/2
// (shape.Shape.KVreq): sequence ages are spread over each request's length,
// so with equal lengths the average resident sequence carries half its
// generation, and with a spread D = E[O^2]/OL^2 of them a long request,
// resident longer, weighs more (shape.WithOutputDispersion, issue #276).
//
// The cap is the one the derived k2 is priced at (the saturation analyzer's
// pricingBatchCap): the engine's max_num_seqs, lowered by
// --max-num-active-seqs when set (issue #335), and where the flags are unread
// the declared or learned cap in place of the parser's 256, which priced a
// sequence-bound engine above what it can run and so ordered too few replicas
// (issue #420). A batchCap of 0 caps nothing. The cap is not optional: short
// requests imply more resident sequences than the engine will admit, and the
// trace's second phase ran at exactly 256, the configured ceiling.
//
// max_num_seqs bounds ONE engine, and a data-parallel replica runs ranks of
// them (domain.ReplicaMetrics.Ranks): the replica admits ranks x max_num_seqs,
// the cap CappedSequences takes.
//
// C is the engine's whole KV capacity, not k1. k1 is already C times the
// analyzer's KV threshold, and itl.Sequences applies k itself, so passing k1
// would apply a threshold twice.
//
// avgOutput is the output length mu is divided by, passed in rather than taken
// from fleet: it must be the SHORT-window figure, while the shape keeps the [5m]
// one every other consumer shares. A count-weighted mean over five minutes is
// dominated by the previous shape's long stragglers for five minutes after they
// stop arriving -- measured decaying 3750 -> 250 across one 6000 -> 250 switch --
// and the divisor is where that error reaches mu undamped. KVreq, by contrast,
// is ILeff + OL/2 and barely moves: at a 20k prompt the same error shifts it
// about 1.5%.
func DeriveMuAtCap(model itl.Model, params *capacity.EngineParams, batchCap int64, ranks int,
	totalKvTokens int64, fleet shape.Shape, kPrice float64, avgOutput float64) DerivedMu {
	if model.IsZero() {
		return DerivedMu{Why: "no-line"}
	}
	if !(avgOutput > 0) || !(fleet.KVreq > 0) {
		return DerivedMu{Why: "no-shape"}
	}
	if !(kPrice > 0) || kPrice > 1 {
		return DerivedMu{Why: "k-price"}
	}
	capacityTokens := CapacityTokens(params, ranks, totalKvTokens)
	replicaCap := 0.0
	if batchCap > 0 {
		replicaCap = float64(batchCap) * float64(max(ranks, 1))
	}
	// The SHAPE's KVreq, not one re-derived here. shape.New computes
	// ILeff = IL*(1 - prefixHitRate) and KVreq = ILeff + D*OL/2, so a fleet
	// whose prompts are largely cache hits occupies far less than IL + OL/2
	// and holds correspondingly more sequences. Re-deriving it dropped the
	// hit-rate term, which on a caching workload under-states the resident
	// count and over-orders replicas by the same factor. (It made no
	// difference to the 2026-09-24 run -- those pods export no prefix-cache
	// series at all, so the hit rate was 0 and the two agreed at 4000 -- but
	// that is the workload being uninteresting, not the arithmetic being
	// right.) The discount holds only for hits on blocks another running
	// request holds too, so the caller bounds KVreq by the KV the running
	// requests are measured to hold (the saturation analyzer's
	// withResidentKV, issue #277).
	seqs := CappedSequences(kPrice, capacityTokens, fleet.KVreq, replicaCap)
	if seqs <= 0 {
		return DerivedMu{Why: "no-seqs"}
	}
	itlSec := model.ITLAt(kPrice)
	if !(itlSec > 0) {
		return DerivedMu{Why: "no-itl"}
	}
	tokenSec := seqs / itlSec
	rate := tokenSec / avgOutput
	if !(rate > 0) {
		return DerivedMu{Why: "no-rate"}
	}
	return DerivedMu{Rate: rate, Seqs: seqs, TokenSec: tokenSec, OK: true}
}
