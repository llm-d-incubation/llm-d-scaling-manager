package kueue

import "slices"

// Reasons added for the withheld and counted-but-0 rows of the B1 table (the
// Kueue borrowing proposal, revision 8). The other reasons are declared beside
// the figures they qualify (borrow.go).
const (
	// ReasonLendersUnreadable: the Workloads were not read cluster-wide this
	// read -- a namespace-scoped install sees its own namespace only -- or the
	// read failed, or was Forbidden. A lender's pending Workload would then go
	// unseen and its wait would never stop new borrowing, so nothing is
	// borrowed. Forbidden is not absent.
	ReasonLendersUnreadable Reason = "lenders-unreadable"
	// ReasonKueueVersionUnknown: the Kueue controller's Deployment was read but
	// its image names no version (a digest-only reference, say), so whether
	// the fields borrowing relies on hold is not known. kueue.assumeTestedVersion
	// lifts it. A Deployment that cannot be read at all is
	// ReasonKueueControllerDown, never this.
	ReasonKueueVersionUnknown Reason = "kueue-version-unknown"
	// ReasonBorrowWithinCohort: a queue in the cohort sets
	// spec.preemption.borrowWithinCohort to a policy other than Never: Kueue may
	// then preempt within the cohort to borrow, which the reach here does not
	// model.
	ReasonBorrowWithinCohort Reason = "borrow-within-cohort"
	// ReasonFlavorUnpinned: the namespace's queue offers more than one flavor
	// for the GPU and a variant's pod template does not pin a flavor's
	// nodeLabels: Kueue may then put its replica on a flavor other than the
	// one it was counted on. Decided on the engine side, from the variants'
	// pod templates.
	ReasonFlavorUnpinned Reason = "flavor-unpinned"
	// ReasonCoResourceBound: the GPU flavor's resource group covers another
	// resource (CPU, memory, ...) and a variant's per-replica request of it is
	// not known from its pod template: Kueue admits per resource group, so a
	// replica could be held back by the co-resource while the GPU figure says
	// it fits. Decided on the engine side.
	ReasonCoResourceBound Reason = "co-resource-bound"
	// ReasonMultiQueueNamespace: more than one of the namespace's GPU
	// ClusterQueues grants this accelerator type (or the row's flavor is
	// untyped). Which of them a variant's replica is admitted through is not
	// read, so the namespace is capped at the smallest single queue's figure
	// of the type, and borrows none of it. A type only one of its queues
	// grants is not affected.
	ReasonMultiQueueNamespace Reason = "multi-queue-namespace"
)

// Reasons a counted namespace's borrowable figure is 0 this cycle (the third
// table of B1), decided on the engine side each cycle.
const (
	// ReasonLenderWaiting: a lender of the namespace's cohort waits for
	// capacity it lent; no new borrowing in the cohort.
	ReasonLenderWaiting Reason = "lender-waiting"
	// ReasonCohortPoolEmpty: the free pool holds no GPUs, after what is in
	// flight and gated.
	ReasonCohortPoolEmpty Reason = "cohort-pool-empty"
	// ReasonNoBorrowCap: borrowingLimit, the static entry or the ResourceQuota
	// stops the namespace at guaranteed.
	ReasonNoBorrowCap Reason = "no-borrow-cap"
	// ReasonPreemptionsUnreadable: Kueue's preemptions in the namespace could
	// not be read this cycle.
	ReasonPreemptionsUnreadable Reason = "preemptions-unreadable"
)

// reasonOrder is the order of the B1 tables of the Kueue borrowing proposal
// (revision 8): withheld, then frozen, then counted but 0, each in its table's
// row order. When several reasons hold for one namespace, the first wins.
var reasonOrder = []Reason{
	// Withheld.
	ReasonNotSelected,
	ReasonLendersUnreadable,
	ReasonNotKueueManaged,
	ReasonResourceQuotaUnreadable,
	ReasonNoGPUBound,
	ReasonMultiQueueNamespace,
	ReasonKueueVersionUnknown,
	ReasonKueueVersionUntested,
	ReasonNoCohort,
	ReasonInactive,
	ReasonMalformed,
	ReasonNoStatus,
	ReasonHierarchicalCohort,
	ReasonHierarchyUnknown,
	ReasonEffectiveQuotas,
	ReasonFairSharing,
	ReasonTopologyAware,
	ReasonAdmissionChecks,
	ReasonBorrowWithinCohort,
	ReasonFlavorUnpinned,
	ReasonCoResourceBound,
	// Frozen.
	ReasonStale,
	ReasonKueueControllerDown,
	// Counted, but 0 this cycle.
	ReasonLenderCannotReclaim,
	ReasonLenderWaiting,
	ReasonCohortPoolEmpty,
	ReasonNoBorrowCap,
	ReasonPreemptionsUnreadable,
}

// ReasonOrder returns the B1 table order, first wins; a copy.
func ReasonOrder() []Reason {
	return slices.Clone(reasonOrder)
}

// rank is a reason's place in reasonOrder; a reason not in it -- the empty
// one, or nominal-lent-out, which qualifies a figure and withholds nothing --
// ranks after every reason that is.
func rank(r Reason) int {
	if i := slices.Index(reasonOrder, r); i >= 0 {
		return i
	}
	return len(reasonOrder)
}

// Precedence returns the reason a namespace shows when several hold: the first
// in the B1 table order (withheld, then frozen, then counted but 0). Empty
// reasons are ignored; a reason outside the table loses to every reason in it.
// Empty when every reason is.
//
// It is the one place the order lives: the reader (a cohort's reasons), the
// quota entry (the namespace's own reasons beside its queues') and the cycle's
// budgets (counted-but-0 beside a frozen or withheld reason) all pick with it.
func Precedence(reasons ...Reason) Reason {
	var best Reason
	for _, r := range reasons {
		if r == "" {
			continue
		}
		if best == "" || rank(r) < rank(best) {
			best = r
		}
	}
	return best
}

// PrecedenceOf is Precedence over reasons spelled as strings, as the config
// snapshot and the engine carry them.
func PrecedenceOf(reasons ...string) string {
	rs := make([]Reason, 0, len(reasons))
	for _, r := range reasons {
		rs = append(rs, Reason(r))
	}
	return string(Precedence(rs...))
}
