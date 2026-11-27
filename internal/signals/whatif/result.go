package whatif

import (
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// Result is what every what-if question returns: an Answer, an Unanswerable,
// or (from DecodeAdmission) an Admission. It is sealed, so a caller reaches a
// number only through a type switch or assertion, and a refusal can never be
// read as a value of 0. An unchecked assertion on a refusal panics rather than
// yielding a zero.
//
// docs/proposals/slo-capacity-planner.md, "Results and exit codes", sets out
// the contract.
type Result interface {
	result()
}

// Reason says why a question was refused. Each one is a refusal with a cause
// the operator can act on, never a number.
type Reason string

// The reasons this package refuses with. The CLI reference's table lists the
// planner's full set; each later PR adds the reasons its own terms can raise
// (NoRoleMap), so every reason defined here has a case that produces it. The
// TTFT model's are in ttft_result.go.
const (
	// NoLine is no ITL line, or one the planner cannot price from: a slope
	// or intercept that is not positive and finite, or a line that predicts no
	// positive ITL at the bottom of the band.
	NoLine Reason = "NoLine"
	// PinnedDefaultB is a line whose only fit is pinned-B (or held, or
	// borrowed from a donor that is) on the default intercept
	// (itl.DefaultBaselineSec): the slope was fitted against a constant
	// guessed for no card in particular, so the line describes nothing
	// measured about this one.
	PinnedDefaultB Reason = "PinnedDefaultB"
	// NoCapacity is no positive, finite KV capacity reading, or one whose
	// band top holds no whole request (floor(band.Hi*C/KVreq) < 1).
	NoCapacity Reason = "NoCapacity"
	// NoBatchCap is no batch cap known for the replica.
	NoBatchCap Reason = "NoBatchCap"
	// NoTraffic is no traffic shape: an output length or a KV footprint per
	// request that is not positive and finite.
	NoTraffic Reason = "NoTraffic"
	// TokenGapPercentile is a percentile over token gaps (ITL), which is not
	// supported; a percentile over requests of their own TPOT is.
	TokenGapPercentile Reason = "TokenGapPercentile"
	// NoTPOTHistogram is a per-request TPOT percentile with no ratio R_q:
	// no TPOT histogram to learn it from, and none assumed.
	NoTPOTHistogram Reason = "NoTPOTHistogram"
	// TPOTQuantileUnbounded is a TPOT quantile that falls in a histogram's
	// +Inf bucket, which has no upper edge to read.
	TPOTQuantileUnbounded Reason = "TPOTQuantileUnbounded"
	// TPOTBucketTooWide is a per-request TPOT percentile the engine's histogram
	// was asked for, the router's holding no observation, with a q bucket past
	// MaxEngineBucketWidth: the bucket's edge, whatever q was asked. The CLI
	// gives it too for a percentile priced on a host-bound decode variant
	// (intercept and floor under 5 ms) whose R_q was read off coarse buckets:
	// the tail is host jitter the line does not model.
	TPOTBucketTooWide Reason = "TPOTBucketTooWide"
	// TargetBelowFloor is a target below what any number of replicas
	// reaches: the line's ITL at the bottom of the band, times R_q, already
	// meets or exceeds it.
	TargetBelowFloor Reason = "TargetBelowFloor"
	// Saturated is a rate at or above the knee, where the decode replicas
	// cannot drain their arrivals: question 2's operating rate (FillAtRate)
	// and the admission chain (DecodeAdmission) both refuse it, at the same
	// point.
	Saturated Reason = "Saturated"
	// NoDecodeReplicas is a fleet read with no decode replica to price.
	NoDecodeReplicas Reason = "NoDecodeReplicas"
	// MalformedHistogram is a TPOT histogram whose data cannot be read: an
	// edge that is NaN, not positive or not above the one before it, a
	// negative or non-finite count, or observations with no positive sum.
	// (Cumulative counts that dip, as rate() over a scrape leaves them, are
	// not malformed: they are made monotone, as histogram_quantile does.)
	MalformedHistogram Reason = "MalformedHistogram"
	// TooManySlots is a replica whose slots exceed maxSlots: a batch cap and
	// a KV capacity that large are a reading error, and the admission chain
	// holds one state per slot.
	TooManySlots Reason = "TooManySlots"
	// InvalidInput is a question the caller asked wrongly, a usage or
	// configuration error rather than data: a NaN or infinite target, rate or
	// assumed ratio, a percentile outside (0, 1), an assumed ratio below 1, a
	// headroom outside (0, 1] (outside (0, 1) for question 2), or a band that
	// breaks FillBand's precondition. The CLI maps it to exit 2; every other
	// reason is a refusal, exit 3.
	InvalidInput Reason = "InvalidInput"
)

// Reasons lists every Reason this package can return, in a fixed order.
func Reasons() []Reason {
	r := []Reason{NoLine, PinnedDefaultB, NoCapacity, NoBatchCap, NoTraffic, TokenGapPercentile,
		NoTPOTHistogram, TPOTQuantileUnbounded, TPOTBucketTooWide, TargetBelowFloor, Saturated,
		NoDecodeReplicas, MalformedHistogram, TooManySlots, InvalidInput}
	return append(r, ttftReasons()...)
}

// Unanswerable is a refused question: the Reason, and a Detail naming the
// input that caused it. Flags are what the refusal rests on, where that is more
// than its input (a TTFT floor's decode term). AssumedTermDecides says an
// assumed part of that term alone refused it: the target stands above the floor
// with it taken out, so the refusal says nothing of the fleet.
type Unanswerable struct {
	Reason             Reason
	Detail             string
	Flags              []Flag
	AssumedTermDecides bool
}

func (Unanswerable) result() {}

// Error lets an Unanswerable travel as an error where a caller wants one.
func (u Unanswerable) Error() string {
	if u.Detail == "" {
		return string(u.Reason)
	}
	return string(u.Reason) + ": " + u.Detail
}

func refuse(r Reason, detail string) Unanswerable {
	return Unanswerable{Reason: r, Detail: detail}
}

// Flag qualifies an answer without refusing it: the answer stands, and the
// flag says what it rests on that the operator should know.
type Flag string

// The flags this package raises.
const (
	// AssumedTPOTRatio is a percentile TPOT priced with a ratio R_q the
	// caller assumed rather than learned from a histogram.
	AssumedTPOTRatio Flag = "AssumedTPOTRatio"
	// CoarseTPOTBucket is an R_q read off a histogram bucket wider than a
	// quarter of the window's mean: the ratio is set by the bucket grid, not
	// by the traffic.
	CoarseTPOTBucket Flag = "CoarseTPOTBucket"
	// EngineTPOTHistogram is an R_q read from the engine's TPOT histogram in
	// place of the router's, or a refusal of it.
	EngineTPOTHistogram Flag = "EngineTPOTHistogram"
	// MeanBoundedTPOTPercentile is an R_q read below its coarse bucket's upper
	// edge, at the most the window's mean lets the percentile be: never below
	// the true percentile, and different for percentiles that share the
	// bucket where the mean allows it. It comes with CoarseTPOTBucket.
	MeanBoundedTPOTPercentile Flag = "MeanBoundedTPOTPercentile"
	// GlobalTPOTRatio is a percentile TPOT priced at a fill whose bucket of
	// R_q by fill held fewer than MinFillRatioWindows windows: the global
	// R_q stood in for the bucket's own.
	GlobalTPOTRatio Flag = "GlobalTPOTRatio"
	// BelowFittedBand is a fill below the band the line was fitted over: the
	// TPOT is read at the band's bottom, the higher, safer reading, so the
	// answer's TPOT is a floor rather than the line's extrapolation.
	BelowFittedBand Flag = "BelowFittedBand"
	// KVBoundSlots is an admission chain whose slots are set by the KV the
	// band's top holds, floor(band.Hi*C/KVreq), rather than the batch cap: an
	// approximation, since KV-bound admission is not a fixed number of slots.
	KVBoundSlots Flag = "KVBoundSlots"
	// BorrowedLine is an answer priced on a line another variant measured
	// for the same engine configuration: evidence about the configuration,
	// not about this variant's own load. It has no readings to band.
	BorrowedLine Flag = "BorrowedLine"
	// HeldLine is an answer priced on a line the tracker held: a slope from an
	// earlier fit, the window having too few readings to fit a new one, or a
	// line the tier dwell kept where this cycle's readings chose another
	// (itl.Reading.TierHeld).
	HeldLine Flag = "HeldLine"
	// TierUnstable is a band in which more than a tenth of the resamples
	// fitted the line at another tier than the answer's, and those that did
	// price the figure more than 5% away from it: which tier the tracker
	// lands at moves the answer.
	TierUnstable Flag = "TierUnstable"
	// LineNearGate is informational: more than a tenth of the resamples
	// fitted the line at another tier, but their figures stay within 5% of
	// the answer. The line sits near the tracker's gate (the steady
	// readings' count or spread), and the answer does not care which side.
	LineNearGate Flag = "LineNearGate"
	// ExtrapolatedFill is an answer whose fill lies above the highest fill
	// the line's readings were taken at; the basis gives the distance.
	ExtrapolatedFill Flag = "ExtrapolatedFill"
	// ExtrapolatedBelowFloor is a figure priced at fewer sequences a replica
	// than any reading of the floor below the band ran
	// (BelowBand.LeastSequences): a TPOT, the rate at a TPOT target, the
	// knee, the admission chain at its mean sequences, or the TTFT's decode
	// step. It is held to the floor kept by count (BelowBand.ByCount) read at
	// its own count, below every count the floor saw, or to the lowest-count
	// floor where that line was not read. Set only where the lowest-count
	// floor would have held the figure up: one the ITL line reads above it is
	// not flagged.
	ExtrapolatedBelowFloor Flag = "ExtrapolatedBelowFloor"
	// AssumedShape is an answer priced at a prompt or output length the
	// caller assumed rather than the one the fleet measured.
	AssumedShape Flag = "AssumedShape"
)

// The line tiers a Basis names (docs/proposals/slo-capacity-planner.md,
// "Terms"): where the ITL line came from. The first two are the itl
// tracker's own; held and borrowed are what the analyzer does when the
// tracker fits nothing.
const (
	// TierOLS is a full least-squares fit.
	TierOLS = itl.TierOLS
	// TierPinnedB is a fit of the slope alone with the intercept held from
	// the last full fit, or the default.
	TierPinnedB = itl.TierPinnedB
	// TierHeld is a line the tracker held: the last slope kept, the window too
	// thin to refit it, or the tier dwell refusing a change of tier.
	TierHeld = "held"
	// TierBorrowed is another variant's line for the same engine
	// configuration.
	TierBorrowed = "borrowed"
	// TierAboveBand is the tracker's above-band fallback: a pinned-B line
	// fitted from readings above the band, at a fill of at least
	// itl.DefaultKSat, for a key that never fitted in the band.
	TierAboveBand = "above-band"
)

// Interval is an uncertainty band around a figure, Lo to Hi in the figure's
// unit: the 10th to the 90th percentile of a block bootstrap (band.go), so it
// need not be symmetric about the figure.
type Interval struct {
	Lo, Hi float64
}

// Bound names what set a rate. On a tie every bound is named, the
// operating-point cap first.
type Bound string

// The bounds a decode rate can be set by.
const (
	// BoundOperatingPoint is the headroom cap: the operating point, a
	// fraction of the knee, past which decode admission waits are no longer
	// small.
	BoundOperatingPoint Bound = "operating-point cap"
	// BoundTPOT is the TPOT target itself.
	BoundTPOT Bound = "TPOT"
	// BoundBatchCap is the knee set by the batch cap's fill.
	BoundBatchCap Bound = "batch cap"
	// BoundBandTop is the knee set by the band's top, the highest fill the
	// line is fitted over.
	BoundBandTop Bound = "band top"
)

// Basis is what an answer rests on, for the operator to check it against.
// A field that does not apply to the question is 0.
type Basis struct {
	// Fill is the KV fill one decode replica runs at for the answer.
	Fill float64
	// KneeRate is the knee: the rate, in requests per second over the fleet,
	// at which the decode replicas saturate (KneeRate).
	KneeRate float64
	// Floor is the lowest TPOT any number of replicas reaches: the line's
	// ITL at the bottom of the band, times R_q for a percentile.
	Floor float64
	// Uncapped is, for a rate, the rate the target alone allows before the
	// operating-point cap (the "TPOT alone" figure).
	Uncapped float64
	// BoundBy names what set a rate, the operating-point cap first on a tie.
	BoundBy []Bound
	// Tier is the ITL line's tier (TierOLS, TierPinnedB, TierHeld,
	// TierBorrowed), as Decode.Tier gave it; "" when the caller named none.
	Tier string
	// TTFT is the basis of a TTFT answer (RateForTTFT, PrefillKnee); nil on
	// every decode answer. A TTFT answer leaves the TPOT fields above
	// (KneeRate, Floor, Uncapped) at 0, so none of them changes meaning.
	// Basis carries no JSON tags and none is needed: cmd/capacity-planner writes its
	// own wire types (wireBasis), field by field, so this field reaches the
	// wire only when the CLI maps it (TestJSONSchemaIsPinned holds it out).
	TTFT *TTFTBasis
	// Batch is the batch the answer's fill runs against the most the decode
	// line's fit rows held, where it is past them (the planner's
	// BatchBeyondFit flag); nil where it is not, or either is unknown.
	Batch *BatchBeyondFit
}

// BatchBeyondFit is a decode answer's batch past its line's fit, in
// sequences an engine rank: Priced is the batch the answer's fill holds,
// Fitted the most the rows the line was fitted on held. The line is read
// there all the same; ITL also rises with the batch's tokens past the GEMM
// roofline, which a line in fill alone does not carry, so a TPOT reads low
// and a knee high (issues llm-d-incubation/llm-d-scaling-manager#309, #354).
//
// Ranks is the data-parallel engine ranks a pod runs, where more than one,
// and 0 for one: the batches are a rank's, not the pod's.
type BatchBeyondFit struct {
	Priced, Fitted int64
	Ranks          int
}

// BatchUnit is what a BatchBeyondFit batch counts per, in words: "a pod",
// or "an engine rank" on a pod of more than one rank.
func BatchUnit(ranks int) string {
	if ranks > 1 {
		return "an engine rank"
	}
	return "a pod"
}

// Answer is a priced figure: Value in the question's unit (requests per
// second for a rate, seconds for a TPOT), its Basis and Flags, and Band, its
// uncertainty, nil until a caller bands it (BandAnswer).
type Answer struct {
	Value float64
	Basis Basis
	Flags []Flag
	Band  *Interval
	// Sensitivity is the figure's span over V_P and f either way, nil on a
	// figure that does not rest on them.
	Sensitivity *Sensitivity
}

func (Answer) result() {}

// HasFlag reports whether the answer carries f.
func (a Answer) HasFlag(f Flag) bool {
	return slices.Contains(a.Flags, f)
}

// The planner's exit codes (docs/proposals/slo-capacity-planner.md, "Results
// and exit codes").
const (
	// ExitAnswered is every result answered.
	ExitAnswered = 0
	// ExitInternal is an internal error: here, no result to judge.
	ExitInternal = 1
	// ExitUsage is a usage or input error: some result is InvalidInput.
	ExitUsage = 2
	// ExitRefused is a refusal: some result is Unanswerable for a reason the
	// data gave, and none is InvalidInput. The output is still printed.
	ExitRefused = 3
)

// ExitCode is the exit code the planner ends with for these results, so the
// CLI applies the contract rather than re-deriving it: ExitUsage if any is
// InvalidInput (a question asked wrongly outranks what the data says), else
// ExitRefused if any is refused, else ExitAnswered. A nil result is a question
// not asked (DecodeSLO.PercentileTPOT) and is skipped; with nothing left the
// code is ExitInternal.
func ExitCode(results ...Result) int {
	asked, refused := false, false
	for _, r := range results {
		if r == nil {
			continue
		}
		asked = true
		if u, ok := r.(Unanswerable); ok {
			if u.Reason == InvalidInput {
				return ExitUsage
			}
			refused = true
		}
	}
	switch {
	case !asked:
		return ExitInternal
	case refused:
		return ExitRefused
	}
	return ExitAnswered
}
