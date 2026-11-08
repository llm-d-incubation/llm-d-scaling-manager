package itl

import "math"

// The decode arithmetic the ITL model exists to support, in one place.
//
// A replica's KV cache holds k·C tokens at utilization k, and one in-flight
// request occupies KVreq of them, so k·C/KVreq sequences are resident. Each
// advances one token every ITL(k) seconds. The replica's token rate is
// therefore Sequences(k)/ITL(k) -- the same two lines whether the question is
// what a replica is doing now (k = its current utilization) or what it could
// sustain (k = DefaultKSat).
//
// Both take KVreq rather than a shape, and a token count rather than a replica,
// so this package stays free of domain and shape imports; the callers already
// hold shape.Shape.KVreq and domain.ReplicaMetrics.TotalKvCapacityTokens.
//
// Neither applies a KV threshold. k IS the utilization being asked about, so a
// caller passing a capacity that already has a threshold folded into it (the
// saturation analyzer's k1 is C times its KV threshold) would apply it twice.

// Sequences returns the number of in-flight requests resident on a replica
// whose KV cache is kvMaxTokens in total and is k full, when one request
// occupies kvPerRequest tokens. Returns 0 if any input is non-positive.
//
// kvPerRequest is the TIME-AVERAGED footprint, IL + OL/2 in steady state
// (shape.Shape.KVreq), not the peak: sequence ages are spread over [0, OL], so
// the average resident sequence carries half its generation.
func Sequences(k, kvMaxTokens, kvPerRequest float64) float64 {
	if !(k > 0) || !(kvMaxTokens > 0) || !(kvPerRequest > 0) {
		return 0
	}
	return k * kvMaxTokens / kvPerRequest
}

// TokenRate returns the generation tokens per second one replica sustains at
// utilization k: the sequences resident there, each advancing a token every
// ITL(k) seconds. Returns 0 when the model has no reading at k or any input is
// non-positive.
func TokenRate(m Model, k, kvMaxTokens, kvPerRequest float64) float64 {
	seqs := Sequences(k, kvMaxTokens, kvPerRequest)
	if seqs <= 0 {
		return 0
	}
	itlSec := m.ITLAt(k)
	// ITLAt can return NaN for a model fitted on degenerate input; NaN fails
	// every comparison, so this rejects it without a separate math.IsNaN.
	if !(itlSec > 0) {
		return 0
	}
	return seqs / itlSec
}

// The GPS check: does the line describe the replica in front of it?
//
// A fitted ITL(k) predicts a generation-token rate at a replica's own
// utilization, and the replica reports that rate directly
// (vllm:generation_tokens_total). Comparing the two tests the line against a
// signal never fitted into it, at whatever shape and load the replica is under.
//
// It is the ONLY check available on a derived mu. A mu priced for a shape no
// replica has been saturated under has no measured throughput to be compared
// with -- that is what "derived" means -- but the line it came from still has
// to predict what the replica is doing right now, and when it does not, the
// price is not evidence of anything.

const (
	// DefaultGPSMismatchThresholdPct is the largest percentage error between
	// the predicted token rate and the observed one that still counts as the
	// line describing the replica.
	DefaultGPSMismatchThresholdPct = 15.0

	// DefaultGPSMinKForVerification is the utilization below which the
	// comparison is not attempted: at a low k only a handful of sequences are
	// resident, and a percentage error on their token rate is mostly
	// quantisation of the sequence count.
	DefaultGPSMinKForVerification = 0.30
)

// GPSErrorPct returns the percentage error between the token rate m predicts at
// k and the observedGPS a replica reports there.
//
// ok is false when the comparison cannot be made at all: no observed rate, a k
// below DefaultGPSMinKForVerification, or a model that prices nothing at k.
// That is NOT a mismatch. It is the absence of evidence in either direction,
// and a caller that folds the two together either treats an unmeasured fleet
// as a wrong one or a wrong line as a right one.
func GPSErrorPct(m Model, k, kvMaxTokens, kvPerRequest, observedGPS float64) (float64, bool) {
	if !(observedGPS > 0) || k < DefaultGPSMinKForVerification {
		return 0, false
	}
	predicted := TokenRate(m, k, kvMaxTokens, kvPerRequest)
	if !(predicted > 0) {
		return 0, false
	}
	return math.Abs(predicted-observedGPS) / observedGPS * 100, true
}
