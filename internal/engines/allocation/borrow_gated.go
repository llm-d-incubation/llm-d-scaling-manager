package allocation

import (
	"maps"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// Replicas held at Kueue's admission gate, waiting for quota, in a namespace
// that counts no borrowing: an entry with countBorrowable false, or a queue in
// no cohort. Kueue gates a pod its queue has no room for. With nothing
// borrowed Kueue reclaims nothing from a cohort for it; it may still preempt
// lower-priority work inside the queue itself (withinClusterQueue), which
// takes as long as the victims' termination grace period. The engine lists
// those pods in every Kueue-backed namespace, so the quota charge does not
// count them as reserved (KU1), and the governor releases one held there past
// the placement timeout (gatedRelease), as it does a borrowed one.

// KueueNamespaces is every namespace a Kueue-backed constraint (KueueBacked)
// carries figures for, sorted: the namespaces whose pods at Kueue's admission
// gate the engine lists, borrowing counted or not.
func KueueNamespaces(constraints []*ResourceConstraints) []string {
	set := map[string]bool{}
	for _, c := range constraints {
		if c == nil || !c.KueueBacked {
			continue
		}
		for ns := range c.NamespacePools {
			set[ns] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// UncountedKueueNamespaces is the namespaces whose gated replicas the
// placement timeout releases against their Kueue figure (uncountedHeldPast),
// sorted: those of a Kueue-backed entry that counts no borrowing, and, of an
// entry that counts it, those whose queue is in no cohort -- nothing to
// borrow from. A namespace a constraint counts borrowing for this cycle is
// not among them; nor is one an entry that counts borrowing withholds for any
// other reason (a stale read, an eligibility check): Kueue may still admit it
// by borrowing, and its release state is the borrowing one's.
func UncountedKueueNamespaces(constraints []*ResourceConstraints) []string {
	counted := map[string]bool{}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns := range c.NamespaceBorrow {
			counted[ns] = true
		}
	}
	set := map[string]bool{}
	excluded := map[string]bool{}
	for _, c := range constraints {
		if c == nil || !c.KueueBacked {
			continue
		}
		for ns := range c.NamespacePools {
			switch {
			case counted[ns]:
			case c.KueueCountsBorrowing && c.BorrowWithheld[ns] != string(kueue.ReasonNoCohort):
				excluded[ns] = true
			default:
				set[ns] = true
			}
		}
	}
	for ns := range excluded {
		delete(set, ns)
	}
	return slices.Sorted(maps.Keys(set))
}

// uncountedHeldPast is, per namespace of UncountedKueueNamespaces and per
// type, the GPUs its variants hold past its Kueue figure (the pool's Limit,
// charged with what Kueue reserved for others), gated replicas included:
// Kueue admits nothing past that figure from the cohort. Within the figure
// Kueue admits the replica, or reclaims lent-out nominal for it. Two entries
// over one namespace count the larger. The counted namespaces are
// namespaceHeldPast's.
func uncountedHeldPast(constraints []*ResourceConstraints) map[string]map[string]int {
	uncounted := map[string]bool{}
	for _, ns := range UncountedKueueNamespaces(constraints) {
		uncounted[ns] = true
	}
	out := map[string]map[string]int{}
	if len(uncounted) == 0 {
		return out
	}
	for _, c := range constraints {
		if c == nil || !c.KueueBacked {
			continue
		}
		for ns, pools := range c.NamespacePools {
			if !uncounted[ns] {
				continue
			}
			for t, p := range pools {
				if p.Limit < 0 || p.Used <= p.Limit {
					continue
				}
				if out[ns] == nil {
					out[ns] = map[string]int{}
				}
				out[ns][t] = max(out[ns][t], p.Used-p.Limit)
			}
		}
	}
	return out
}
