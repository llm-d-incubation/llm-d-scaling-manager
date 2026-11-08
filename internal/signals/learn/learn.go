// Package learn is the learners' shared vocabulary (docs/proposals/
// analyzer-learner.md, 6.3): the Report a learner fills each cycle, the
// sources and reasons it fills it with, and the plain list of learners the
// forget-learned-state annotation reaches. It holds types only: a learner
// fills the Report, the analyzer logs it, and nothing here logs, registers
// a metric, or drives a learner.
//
// It imports only the standard library, so domain and internal/metrics can
// use its types without a cycle.
package learn

import (
	"maps"
	"slices"
	"time"
)

// K2 is the k2 learner: KV at saturation, a variant's own window (the
// saturation analyzer's row 3) and the lower bound it borrows from a
// sibling (row 4).
const K2 = "k2"

// Mu is the measured service rate (the saturation analyzer's row 5) and
// its choice against the rate derived from the ITL line (row 6).
const Mu = "mu"

// ITL is the saturation analyzer's ITL line, ITL(k) = A*k + B (its row 1):
// the window of (k, ITL) readings per key, its intercept and its held
// slope.
const ITL = "itl"

// Start is the saturation analyzer's replica start time (its row 12): how
// long a replica of a variant takes to become Ready, per key, and the
// distinct Pods it is from.
const Start = "start"

// SLO is the slo analyzer's ITL line, and Throughput the throughput analyzer's:
// each an instance of ITL's rules, an itl.Tracker of the analyzer's own. Each
// has a Report of its own, so its series is not the saturation line's, and is
// not on Learners: the annotation forgets both under ITL.
const (
	SLO        = "slo"
	Throughput = "throughput"
)

// Reporters is every name a Report may carry: Learners, and the two lines
// forgotten under ITL. It bounds the learner label of the series the Reports
// set.
func Reporters() []string {
	return append(Learners(), SLO, Throughput)
}

// Learners is every learner the forget-learned-state annotation reaches, by
// name, in the order Forget reaches them. A plain list, not a registry: each
// analyzer forgets a name on it with code of its own, and a test holds every
// name to that code. A learner joins the list in the PR that moves it onto
// the shared rules.
func Learners() []string {
	return []string{K2, Mu, ITL, Start}
}

// By is what forgot a key.
type By string

const (
	// ByOperator is the forget-learned-state annotation.
	ByOperator By = "operator"
	// ByEvicted is the learner's own sweep: the key went unused past its
	// horizon.
	ByEvicted By = "evicted"
	// ByArguments is a change of the variant's engine arguments: the key's
	// line was the old engine's.
	ByArguments By = "arguments"
)

// Forgotten is one key a learner forgot, and what forgot it.
type Forgotten struct {
	Key string
	By  By
}

// Keys is the key of each of forgotten, in order.
func Keys(forgotten []Forgotten) []string {
	keys := make([]string, 0, len(forgotten))
	for _, f := range forgotten {
		keys = append(keys, f.Key)
	}
	return keys
}

// Source is where a learner's figure came from, in the learner's own
// vocabulary (k2's P1-obs, P2-hist and the rest). A restore is not a source.
type Source string

// Reason is an admission outcome: why a reading was admitted, or refused.
// Each learner names its own.
type Reason string

// Report is one learner's account of one variant for one cycle.
type Report struct {
	Learner   string
	Namespace string
	ModelID   string
	Variant   string
	Role      string
	// PricingKey is the key that priced the variant this cycle, "" when no
	// key did.
	PricingKey string
	Source     Source
	// Counts is the cycle's admission outcomes by reason. The reasons are
	// each learner's own and need not partition its rows: a sum over them
	// may count a row twice (the ITL line's "settling" readings are counted
	// under "unsteady" too).
	Counts map[Reason]int64
	// Samples is how many readings the pricing key's window holds, and
	// LastAdmitted when the newest of them was admitted, zero when none.
	Samples      int
	LastAdmitted time.Time
	// Writes is how many readings the cycle admitted.
	Writes int64
	// Forgotten is the keys of the variant the learner forgot since its last
	// report, and what forgot each.
	Forgotten []Forgotten
}

// Clone is a deep copy of r.
func (r Report) Clone() Report {
	r.Counts = maps.Clone(r.Counts)
	r.Forgotten = slices.Clone(r.Forgotten)
	return r
}
