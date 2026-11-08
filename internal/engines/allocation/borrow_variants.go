package allocation

import (
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// WithholdByVariants returns copies of constraints in which a namespace's
// borrowing of a type is withheld -- Frozen, nothing new borrowed, the reason
// in Withheld -- when one of its variants of that type breaks a B1 condition
// that only the variants' pod templates show:
//
//   - kueue.ReasonFlavorUnpinned: its queue requires a pinned flavor
//     (NamespaceBorrow.PinRequired) and the variant's pod templates pin none
//     of PinLabels;
//   - kueue.ReasonCoResourceBound: a shared pool it could borrow from covers a
//     co-resource (CPU, memory, ...) whose per-replica request the variant's
//     template does not state.
//
// What the namespace holds stays; only new borrowing stops. When a namespace
// already carries a reason, the first in the B1 table order is kept
// (kueue.Precedence). Constraints that change nothing are returned as they
// are; without borrowing, the input.
func WithholdByVariants(constraints []*ResourceConstraints, requests []ModelScalingRequest) []*ResourceConstraints {
	if !HasBorrowing(constraints) {
		return constraints
	}
	chunks, _ := mergeSharedPools(constraints)
	out := make([]*ResourceConstraints, 0, len(constraints))
	for _, c := range constraints {
		if c == nil || c.NamespaceBorrow == nil {
			out = append(out, c)
			continue
		}
		var copied map[string]map[string]NamespaceBorrow
		for _, req := range requests {
			perType, ok := c.NamespaceBorrow[req.Namespace]
			if !ok {
				continue
			}
			for _, st := range req.VariantStates {
				acc := accOf(req, st.VariantName)
				key, ok := accelerator.FindKey(perType, acc)
				if !ok {
					continue
				}
				coRes, unbound := coResources(chunks, c.NamespaceCohorts[req.Namespace], acc)
				reason := variantReason(perType[key], st, coRes, unbound)
				if reason == "" {
					continue
				}
				if copied == nil {
					copied = make(map[string]map[string]NamespaceBorrow, len(c.NamespaceBorrow))
					for ns, pt := range c.NamespaceBorrow {
						copied[ns] = maps.Clone(pt)
					}
				}
				nb := copied[req.Namespace][key]
				prev := nb.Withheld
				if nb.Frozen && prev == "" {
					prev = string(kueue.ReasonStale)
				}
				nb.Frozen, nb.Withheld = true, kueue.PrecedenceOf(prev, string(reason))
				copied[req.Namespace][key] = nb
				perType = copied[req.Namespace]
			}
		}
		if copied == nil {
			out = append(out, c)
			continue
		}
		cc := *c
		cc.NamespaceBorrow = copied
		out = append(out, &cc)
	}
	return out
}

// BorrowNamespacesSplitReason names the warning of two quota entries over one
// cohort of which only one sets kueue.borrowNamespaces (B13). Logged, never a
// withheld reason.
const BorrowNamespacesSplitReason = "borrow-namespaces-split"

// BorrowSplit is one cohort counted by quota entries that disagree on
// borrowNamespaces: WithList set it, WithoutList did not.
type BorrowSplit struct {
	Cohort                string
	WithList, WithoutList []string
}

// BorrowNamespacesSplit is every cohort that two providers counting borrowing
// both read, where one sets borrowNamespaces and another does not, sorted by
// cohort.
func BorrowNamespacesSplit(constraints []*ResourceConstraints) []BorrowSplit {
	by := map[string]*BorrowSplit{}
	for _, c := range constraints {
		if c == nil || c.NamespaceBorrow == nil {
			continue
		}
		var cohorts []string
		for _, cs := range c.NamespaceCohorts {
			for _, co := range cs {
				if !slices.Contains(cohorts, co) {
					cohorts = append(cohorts, co)
				}
			}
		}
		for _, co := range cohorts {
			s := by[co]
			if s == nil {
				s = &BorrowSplit{Cohort: co}
				by[co] = s
			}
			if c.BorrowNamespacesSet {
				s.WithList = append(s.WithList, c.ProviderName)
			} else {
				s.WithoutList = append(s.WithoutList, c.ProviderName)
			}
		}
	}
	var out []BorrowSplit
	for _, co := range slices.Sorted(maps.Keys(by)) {
		s := by[co]
		if len(s.WithList) > 0 && len(s.WithoutList) > 0 {
			slices.Sort(s.WithList)
			slices.Sort(s.WithoutList)
			out = append(out, *s)
		}
	}
	return out
}

// variantReason is the B1 reason one variant breaks, or empty: of the two
// that hold, the first in the table order. unbound is set when a pool it could
// borrow from charges a co-resource on a flavor not known (K2a).
func variantReason(nb NamespaceBorrow, st domain.VariantReplicaState, coRes []string, unbound bool) kueue.Reason {
	var reasons []kueue.Reason
	if nb.PinRequired && !slices.ContainsFunc(nb.PinLabels, func(l map[string]string) bool { return pinsLabels(st.Placements, l) }) {
		reasons = append(reasons, kueue.ReasonFlavorUnpinned)
	}
	for _, res := range coRes {
		if _, known := st.ReplicaRequests[res]; !known {
			reasons = append(reasons, kueue.ReasonCoResourceBound)
			break
		}
	}
	if unbound {
		reasons = append(reasons, kueue.ReasonCoResourceBound)
	}
	return kueue.Precedence(reasons...)
}

// coResources is the co-resources the shared pools of cohorts serving acc
// cover, sorted, and whether one of those pools charges a co-resource on a
// flavor not known (SharedPoolChunk.CoUnbound).
func coResources(chunks []*SharedPoolChunk, cohorts []string, acc string) (out []string, unbound bool) {
	for _, ch := range chunks {
		if !slices.Contains(cohorts, ch.Cohort) || !ch.serves(acc) {
			continue
		}
		unbound = unbound || len(ch.CoUnbound) > 0
		for res := range ch.CoFree {
			if !slices.Contains(out, res) {
				out = append(out, res)
			}
		}
	}
	slices.Sort(out)
	return out, unbound
}

// pinsLabels reports whether every pod template of a replica pins a node
// carrying labels: each label is required by the template's nodeSelector, or
// by every term of its required node affinity (an In of that one value). A
// replica with no placement pins nothing.
func pinsLabels(placements []domain.PodPlacement, labels map[string]string) bool {
	if len(placements) == 0 || len(labels) == 0 {
		return false
	}
	for _, p := range placements {
		for k, v := range labels {
			if p.NodeSelector[k] == v {
				continue
			}
			if !affinityPins(p, k, v) {
				return false
			}
		}
	}
	return true
}

// affinityPins reports whether a template's required node affinity admits
// only nodes with label k=v: every term requires k In [v].
func affinityPins(p domain.PodPlacement, k, v string) bool {
	if p.RequiredNodeAffinity == nil || len(p.RequiredNodeAffinity.NodeSelectorTerms) == 0 {
		return false
	}
	for _, term := range p.RequiredNodeAffinity.NodeSelectorTerms {
		if !slices.ContainsFunc(term.MatchExpressions, func(e corev1.NodeSelectorRequirement) bool {
			return e.Key == k && e.Operator == corev1.NodeSelectorOpIn && len(e.Values) == 1 && e.Values[0] == v
		}) {
			return false
		}
	}
	return true
}
