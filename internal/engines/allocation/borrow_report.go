package allocation

// What on-call sees of cohort borrowing
// (the Kueue borrowing proposal, PR #161, Part C): the
// figures one cycle used, and what the governor holds against each variant.
// Everything here reads; nothing decides.

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// Reasons a counted namespace's borrowable figure is 0 this cycle, beside the
// kueue.Reason values a withheld namespace carries. They are the kueue
// package's, where their order (kueue.Precedence) lives.
const (
	// BorrowReasonLenderWaiting: a lender of the namespace's cohort waits for
	// capacity it lent, so the cohort is closed to new borrowing.
	BorrowReasonLenderWaiting = string(kueue.ReasonLenderWaiting)
	// BorrowReasonPoolEmpty: the cohort's shared pools serving the type hold
	// no free GPUs (after what is in flight and gated is taken out).
	BorrowReasonPoolEmpty = string(kueue.ReasonCohortPoolEmpty)
	// BorrowReasonNoBorrowCap: the namespace may not go past its guaranteed
	// figure: its queues' borrowingLimit, the static entry or its
	// ResourceQuota stops it there.
	BorrowReasonNoBorrowCap = string(kueue.ReasonNoBorrowCap)
	// BorrowReasonPreemptionsUnreadable: Kueue's preemptions in the namespace
	// could not be read this cycle, so none of its models grows on borrowed
	// capacity (a preemption may have gone unseen).
	BorrowReasonPreemptionsUnreadable = string(kueue.ReasonPreemptionsUnreadable)
)

// MarkPreemptionsUnreadable reports, on every budget of a namespace whose
// Kueue preemptions could not be read this cycle, a borrowable figure of 0:
// the governor blocks every borrowed growth there (BorrowGovernor.BorrowBlocked).
// The reason shown is preemptions-unreadable unless a reason earlier in the
// B1 table order holds (kueue.Precedence).
func MarkPreemptionsUnreadable(budgets []LimiterBudget, unknown map[string]bool) {
	for i := range budgets {
		b := &budgets[i]
		if !unknown[b.Namespace] || b.Guaranteed < 0 {
			continue
		}
		b.Borrowable, b.Reason = 0, kueue.PrecedenceOf(b.Reason, BorrowReasonPreemptionsUnreadable)
	}
}

// LimiterBudget is one namespace-scope limiter entry's figures for one
// namespace and accelerator type, as a cycle used them.
type LimiterBudget struct {
	Limiter     string
	Namespace   string
	Accelerator string
	// Guaranteed is the namespace's cap without borrowing (its pool Limit);
	// config.QuotaUnlimited when nothing bounds it.
	Guaranteed int
	// Borrowable is what the namespace may draw past Guaranteed now, were no
	// other namespace to draw first: min(its borrow cap, the free GPUs of its
	// cohorts' shared pools serving the type). 0 where borrowing is not
	// counted.
	Borrowable int
	// Used is what the namespace's managed variants hold of the type.
	Used int
	// Reason is why Borrowable is 0 for an entry that reads borrowing; empty
	// when it is not 0, or when the entry does not read borrowing at all.
	Reason string
}

// LimiterBudgets is the budget of every namespace-scope provider in
// constraints, per namespace and type, in a stable order. blocked is the
// cohorts closed to new borrowing (BorrowGovernor.BlockedCohorts); the pools
// in constraints are already closed for them, and it only names the reason.
func LimiterBudgets(constraints []*ResourceConstraints, blocked map[string]bool) []LimiterBudget {
	var out []LimiterBudget
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns, perType := range c.NamespacePools {
			for t, p := range perType {
				b := LimiterBudget{Limiter: c.ProviderName, Namespace: ns, Accelerator: t, Guaranteed: p.Limit, Used: p.Used}
				switch nb, counted := c.NamespaceBorrow[ns][t]; {
				case counted && p.Limit >= 0:
					b.Borrowable, b.Reason = borrowableNow(c, ns, t, nb, blocked)
					if w := c.BorrowWithheld[ns]; w != "" {
						// Counted only to keep what it holds, or frozen: no
						// new borrowing, and the entry's one reason, which it
						// picked by kueue.Precedence (withheldReasons) -- the
						// order's one source. Every counted-but-0 reason the
						// pools could add ranks after it; the stale pools a
						// freeze leaves behind are its symptom, not a second
						// cause, so a controller that is down shows as such.
						b.Borrowable, b.Reason = 0, w
					}
				case c.BorrowWithheld[ns] != "":
					b.Reason = c.BorrowWithheld[ns]
				}
				out = append(out, b)
			}
		}
	}
	slices.SortFunc(out, func(a, b LimiterBudget) int {
		return cmp.Or(strings.Compare(a.Limiter, b.Limiter), strings.Compare(a.Namespace, b.Namespace),
			strings.Compare(a.Accelerator, b.Accelerator))
	})
	return out
}

// borrowableNow is min(cap, free GPUs of ns's cohorts' chunks serving acc), and
// why it is 0 when it is: of every reason that holds, the first in the B1
// table order (kueue.Precedence).
func borrowableNow(c *ResourceConstraints, ns, acc string, nb NamespaceBorrow, blocked map[string]bool) (int, string) {
	if nb.Frozen {
		if nb.Withheld != "" {
			return 0, nb.Withheld
		}
		return 0, string(kueue.ReasonStale)
	}
	free, stale, closed := 0, false, false
	for _, ch := range c.SharedPools {
		if !slices.Contains(c.NamespaceCohorts[ns], ch.Cohort) || !ch.serves(acc) {
			continue
		}
		free += ch.Free
		stale = stale || ch.Stale
		closed = closed || blocked[ch.Cohort]
	}
	if nb.Cap != config.QuotaUnlimited {
		free = min(free, nb.Cap)
	}
	if free > 0 {
		return free, ""
	}
	var reasons []kueue.Reason
	if stale {
		reasons = append(reasons, kueue.ReasonStale)
	}
	if closed {
		reasons = append(reasons, kueue.ReasonLenderWaiting)
	}
	if nb.Cap == 0 {
		reasons = append(reasons, kueue.ReasonNoBorrowCap)
	} else if !closed && !stale {
		reasons = append(reasons, kueue.ReasonCohortPoolEmpty)
	}
	return 0, string(kueue.Precedence(reasons...))
}

// VariantBorrowStatus is what the governor holds against one variant now.
type VariantBorrowStatus struct {
	// TimeoutRelease: a placement-timeout release lowers the variant until
	// its unplaced replicas are gone.
	TimeoutRelease bool
	// Unplaced: the variant has unplaced replicas, so it grows on no
	// borrowed capacity (the placement hold).
	Unplaced bool
	// LenderRelease: a release to a waiting lender Kueue does not reclaim
	// for lowers the variant until it lands (releaseToNonReclaimingLenders).
	LenderRelease bool
}

// VariantStatus reports what the governor holds against a variant.
func (g *BorrowGovernor) VariantStatus(namespace, variant string) VariantBorrowStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := namespace + "/" + variant
	var s VariantBorrowStatus
	_, s.TimeoutRelease = g.timeoutHeld[key]
	s.Unplaced = g.held[key]
	s.LenderRelease = g.lenderPinned(key)
	return s
}

// LenderWait is one lender that counts as waiting, and since when.
type LenderWait struct {
	LenderKey
	Since time.Time
}

// WaitingLenders is every lender that counts as waiting now (debounced), in a
// stable order.
func (g *BorrowGovernor) WaitingLenders(now time.Time) []LenderWait {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []LenderWait
	for k, w := range g.watches {
		if g.waiting(k, now) {
			out = append(out, LenderWait{LenderKey: k, Since: w.since})
		}
	}
	slices.SortFunc(out, func(a, b LenderWait) int { return compareLenderKeys(a.LenderKey, b.LenderKey) })
	return out
}

// PoolBorrowed is the GPUs this controller's namespaces hold past their
// guaranteed figures in one cohort and flavor.
type PoolBorrowed struct {
	Cohort, Flavor string
	GPUs           int
}

// BorrowedByPool is, per cohort and flavor, the GPUs the namespaces of
// constraints hold past their guaranteed figures (gated pods, which hold no
// reservation, not counted), sorted. Kueue does not say which flavor a
// borrowed GPU sits on: a namespace's figure for a type goes to the first of
// its cohorts' chunks that serves the type, in chunk order.
func BorrowedByPool(constraints []*ResourceConstraints, gated map[string]map[string]int) []PoolBorrowed {
	chunks, cohorts := mergeSharedPools(constraints)
	by := map[[2]string]int{}
	for ns, perType := range namespaceBorrowed(constraints, gated) {
		for t, n := range perType {
			if n <= 0 {
				continue
			}
			for _, ch := range chunks {
				if slices.Contains(cohorts[ns], ch.Cohort) && ch.serves(t) {
					by[[2]string{ch.Cohort, ch.Flavor}] += n
					break
				}
			}
		}
	}
	out := make([]PoolBorrowed, 0, len(by))
	for k, n := range by {
		out = append(out, PoolBorrowed{Cohort: k[0], Flavor: k[1], GPUs: n})
	}
	slices.SortFunc(out, func(a, b PoolBorrowed) int {
		return cmp.Or(strings.Compare(a.Cohort, b.Cohort), strings.Compare(a.Flavor, b.Flavor))
	})
	return out
}

// BorrowingCohorts is the cohorts some namespace of constraints borrows in
// now: it holds GPUs past its guaranteed figure (gated pods, which hold no
// reservation, not counted).
func BorrowingCohorts(constraints []*ResourceConstraints, gated map[string]map[string]int) map[string]bool {
	out := map[string]bool{}
	for ns, perType := range namespaceBorrowed(constraints, gated) {
		for _, n := range perType {
			if n <= 0 {
				continue
			}
			for _, c := range constraints {
				if c == nil {
					continue
				}
				for _, cohort := range c.NamespaceCohorts[ns] {
					out[cohort] = true
				}
			}
		}
	}
	return out
}
