// Package capacity is what a replica can hold and how that is learned: the
// KV-cache capacity a variant's engines report or its deployment implies
// (k1, memory-bound), the compute-bound capacity observed under saturation
// (k2), the window that smooths those observations, and the store that
// keeps a record per variant so a variant with no live replica can still be
// sized.
package capacity

// LearnedFromLive indicates a capacity record was derived from live metrics.
const LearnedFromLive = "live"

// K2Source identifies which priority level produced the compute-bound capacity
// estimate for a replica.
type K2Source int

// The K2 sources, in priority order: the analyzer takes the first that yields.
const (
	K2SrcObserved   K2Source = iota + 1 // queue saturated: tokensInUse
	K2SrcHistorical                     // rolling average from prior observations
	K2SrcDerived                        // estimated from deployment args
	K2SrcFallback                       // fallback to k1 (memory-bound)
	// K2SrcBorrowed is a compatible sibling variant's measured k2, used by a
	// variant with no history of its own and only where it is LOWER than what
	// the variant would otherwise get from P3 or P4. Not a step in the chain:
	// it is checked after P3/P4 have produced the variant's own figure, and
	// can only lower it. Numbered last so the others keep their values.
	K2SrcBorrowed
	// K2SrcUnadmitted is a saturated reading from a row that may not write
	// the variant's k2 -- a warm-pool bridge, or a Pod that is not Ready --
	// used to price that row for the cycle it was read in, and only where it
	// is LOWER than what the chain gave. Never recorded: not in the window,
	// not in the capacity store, and no saturated throughput is learned from
	// it.
	K2SrcUnadmitted
	// K2SrcPrior is the variant's own k2, measured under engine arguments it
	// no longer runs, used only where it is LOWER than what the variant would
	// otherwise get and relaxing to it on the same clock as a borrowed figure.
	// Not a borrow: it is the variant's own measurement, of an engine it ran
	// before.
	K2SrcPrior
)

// String is the log label of a K2Source; the labels are what the k2 decision
// log line and the analyzer result's Reason carry. Empty for a source that is
// none of the seven.
func (s K2Source) String() string {
	switch s {
	case K2SrcObserved:
		return "P1-obs"
	case K2SrcHistorical:
		return "P2-hist"
	case K2SrcDerived:
		return "P3-k2"
	case K2SrcFallback:
		return "P4-k1"
	case K2SrcBorrowed:
		return "P2-borrow"
	case K2SrcUnadmitted:
		return "P1-obs-unadmitted"
	case K2SrcPrior:
		return "P2-prior"
	}
	return ""
}
