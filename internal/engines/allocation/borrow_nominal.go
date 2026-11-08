package allocation

// A queue's nominal figures beside its borrowing (the Kueue borrowing
// proposal, PR #161, B3, B7 and B10), for every namespace a Kueue-reading
// entry reads, counted or not: what of its unused nominal peers borrow now
// that Kueue reclaims for it -- reclaimWithinCohort Any only -- past what the
// cohort's free pool covers (growth into it needs no node hole: Kueue
// preempts the borrowers whose pods fill those nodes; N1), and what it can
// reach without
// borrowing (a queue that cannot reclaim reaches only what it kept plus the
// cohort's free pool).

import (
	"maps"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// NominalFigures is one namespace's nominal figures for one accelerator type,
// summed over the queues it is granted.
type NominalFigures struct {
	// LentOut is the nominal of its reclaiming queues (reclaimWithinCohort Any
	// only: a LowerPriority queue is read as one that cannot reclaim) that
	// peers borrow now and Kueue preempts them for, past PreemptAfter
	// (kueue.QueueFigures.LentOut): a whole replica grown into it places
	// without a node hole, since the borrowers' pods hold its GPUs.
	LentOut int
	// PreemptAfter is the growth Kueue admits for it before it preempts: what
	// its reclaiming queues keep unused plus what the cohort's free pool
	// covers. It needs a node hole.
	PreemptAfter int
	// PreemptKept is the part of PreemptAfter its queues keep unused, which
	// no pool holds.
	PreemptKept int
	// WithinNominal is what it can reach without borrowing (B7).
	WithinNominal int
	// Guaranteed is what it reaches whatever the cohort's free pool holds:
	// nominal for a reclaiming queue, what it kept or uses for one that
	// cannot reclaim.
	Guaranteed int
	// NominalLentOut is the nominal it cannot reach: lent by a queue that cannot
	// reclaim (B10).
	NominalLentOut int
	// Flavors are the flavors its own queues offer the type on, sorted
	// (NamespaceBorrow.Flavors).
	Flavors []string
	// CoHeadroom is, per co-resource its queues' GPU flavor's resource group
	// covers, what their nominal quota of it leaves after everything Kueue
	// reserved (ExternalBorrow.CoUnused), in milli-units: a replica grown
	// within nominal must fit it as well as its GPUs (K2). Nil when no group
	// covers one.
	CoHeadroom map[string]int64
}

// uncountedNominal is, per namespace the read granted borrowing rows for and
// whose borrowing is not counted, and per accelerator type of the entry in
// force, its queues' nominal figures. A namespace withheld as not
// Kueue-managed has none: Kueue reclaims nothing for pods it does not manage,
// so its growth still needs a node hole. Rows the source withheld carry their
// nominal as within nominal and lend nothing reclaimable.
//
// A namespace that is only not selected (kueue.borrowNamespaces) is not
// evaluated for Kueue management; its queues being granted to it is taken as
// Kueue managing its pods, as for any namespace of a Kueue-backed entry.
func (q *QuotaInventory) uncountedNominal(ext config.ExternalQuotas, view *borrowView, entry config.QuotaLimiterConfig) map[string]map[string]NominalFigures {
	out := map[string]map[string]NominalFigures{}
	for ns, rows := range ext.Borrow {
		if !q.governsBorrowRows(ns, rows) || (view != nil && view.byNS[ns] != nil) {
			continue
		}
		if view != nil && view.withheld[ns] == string(kueue.ReasonNotKueueManaged) {
			continue
		}
		for accType := range entry.NamespaceQuotas[ns] {
			nominal, f := 0, NominalFigures{}
			// Per queue; a type several of the namespace's queues grant
			// (multi-queue-namespace) takes the smallest queue's figures,
			// as its pool does (chargeFigure): a replica is admitted
			// through one of them, which is not read (KU2).
			for i, rs := range rowsByQueue(rowsOfType(rows, accType)) {
				qNominal, q := 0, NominalFigures{}
				for _, r := range rs {
					// Charged with the queue's non-managed charge, as the
					// namespace's pool is (KU1).
					qNominal += max(0, r.Nominal-r.Charge)
					q.WithinNominal += max(0, r.WithinNominal-r.Charge)
					q.Guaranteed += max(0, r.Guaranteed-r.Charge)
					if r.Counted() {
						q.LentOut += r.LentOut
						q.PreemptAfter += r.PreemptAfter
						q.PreemptKept += r.PreemptKept
					}
				}
				co := sumCo(rs, func(r config.ExternalBorrow) map[string]int64 { return r.CoUnused }, true)
				if i == 0 {
					q.CoHeadroom = co
					nominal, f = qNominal, q
					continue
				}
				nominal = min(nominal, qNominal)
				f.WithinNominal, f.Guaranteed = min(f.WithinNominal, q.WithinNominal), min(f.Guaranteed, q.Guaranteed)
				f.LentOut, f.PreemptAfter, f.PreemptKept = min(f.LentOut, q.LentOut), min(f.PreemptAfter, q.PreemptAfter),
					min(f.PreemptKept, q.PreemptKept)
				f.CoHeadroom = tighterCo(f.CoHeadroom, co, true)
			}
			f.Flavors = rowFlavors(rowsOfType(rows, accType))
			f.NominalLentOut = max(0, nominal-f.WithinNominal)
			if !q.countsBorrowing() {
				// Not counting borrowing, the entry caps at nominal: only the
				// co-resources are carried (K2 on the nominal path).
				f = NominalFigures{WithinNominal: nominal, Guaranteed: nominal, Flavors: f.Flavors, CoHeadroom: f.CoHeadroom}
			}
			if f.LentOut == 0 && f.PreemptAfter == 0 && f.NominalLentOut == 0 && f.WithinNominal <= f.Guaranteed && len(f.CoHeadroom) == 0 {
				continue // nominal, whatever the pool holds: nothing to carry
			}
			if out[ns] == nil {
				out[ns] = map[string]NominalFigures{}
			}
			out[ns][accType] = f
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// rowsOfType is a namespace's rows of one type: those naming the accelerator,
// or, when none does, the untyped ones (as namespaceBorrow matches them).
func rowsOfType(rows []config.ExternalBorrow, accType string) []config.ExternalBorrow {
	matched := slices.DeleteFunc(slices.Clone(rows), func(r config.ExternalBorrow) bool {
		return r.Accelerator == "" || !accelerator.SameName(r.Accelerator, accType)
	})
	if len(matched) == 0 {
		matched = slices.DeleteFunc(slices.Clone(rows), func(r config.ExternalBorrow) bool { return r.Accelerator != "" })
	}
	return matched
}

// rowsByQueue is rows grouped by ClusterQueue, in the order they name them.
func rowsByQueue(rows []config.ExternalBorrow) [][]config.ExternalBorrow {
	var names []string
	by := map[string][]config.ExternalBorrow{}
	for _, r := range rows {
		if _, seen := by[r.Queue]; !seen {
			names = append(names, r.Queue)
		}
		by[r.Queue] = append(by[r.Queue], r)
	}
	out := make([][]config.ExternalBorrow, 0, len(names))
	for _, n := range names {
		out = append(out, by[n])
	}
	return out
}

// rowFlavors is the flavors rows name with a nominal quota, sorted, each once;
// nil when none. A row of nominal 0 is a flavor the queue lists only to borrow
// on (a-cq offering fb at 0 to reach team b's pool): Kueue admits nothing of
// the queue's own nominal there, so it is not a flavor its guaranteed
// replicas land on (queueFlavor, NEW-3).
func rowFlavors(rows []config.ExternalBorrow) []string {
	var out []string
	for _, r := range rows {
		if r.Flavor != "" && r.Nominal > 0 && !slices.Contains(out, r.Flavor) {
			out = append(out, r.Flavor)
		}
	}
	slices.Sort(out)
	return out
}

// NamespaceNominal returns, for the active namespaces whose borrowing this
// entry reads and does not count, their nominal figures (NominalFigures); nil
// when there are none, or the last read was stale.
func (q *QuotaInventory) NamespaceNominal(activeNamespaces []string) map[string]map[string]NominalFigures {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if len(q.nominal) == 0 {
		return nil
	}
	out := map[string]map[string]NominalFigures{}
	for _, ns := range activeNamespaces {
		if f, ok := q.nominal[ns]; ok {
			out[ns] = maps.Clone(f)
		}
	}
	return out
}

// Reclaim is how one namespace's growth of one type within its reclaiming
// queues' nominal splits (kueue.QueueFigures): After is the growth Kueue
// admits before it preempts for it, which needs a node hole; GPUs is what it
// then reclaims from borrowers, which does not. A replica places without a
// hole only when nothing of After is left and GPUs covers it whole. Kept is
// the part of After the queues keep unused, taken first: no cohort pool holds
// it, so growth into it debits none (M-m1); the rest of After comes out of
// the cohort's free pool.
type Reclaim struct {
	After int
	Kept  int
	GPUs  int
	// needsHole is set while the model's borrowing cooldown is in force
	// (RequireHoles): no replica places without a node hole.
	needsHole bool
}

// RequireHoles is reclaimable with every figure kept and the node-hole
// exemption off (Reclaim.exempt): for a model in its borrowing cooldown
// (BorrowGovernor.CoolingDown). A replica the governor's safety net released
// because it never placed without a hole (M3) must not be ordered without one
// again until the cooldown ends, or Kueue preempts a peer for it every
// placement timeout.
func RequireHoles(reclaimable map[string]Reclaim) map[string]Reclaim {
	if len(reclaimable) == 0 {
		return reclaimable
	}
	out := make(map[string]Reclaim, len(reclaimable))
	for t, r := range reclaimable {
		r.needsHole = true
		out[t] = r
	}
	return out
}

// pool is the part of After the cohort's free pool covers.
func (r Reclaim) pool() int {
	return max(0, r.After-max(0, r.Kept))
}

// sub is r less o, each figure clamped at 0, Kept at most After; r's
// needsHole is kept.
func (r Reclaim) sub(o Reclaim) Reclaim {
	after := max(0, r.After-o.After)
	return Reclaim{After: after, Kept: min(after, max(0, r.Kept-o.Kept)), GPUs: max(0, r.GPUs-o.GPUs), needsHole: r.needsHole}
}

// add is r plus o.
func (r Reclaim) add(o Reclaim) Reclaim {
	return Reclaim{After: r.After + o.After, Kept: r.Kept + o.Kept, GPUs: r.GPUs + o.GPUs}
}

// atMost is r with each figure at most o's.
func (r Reclaim) atMost(o Reclaim) Reclaim {
	return Reclaim{After: min(r.After, o.After), Kept: min(r.Kept, o.Kept), GPUs: min(r.GPUs, o.GPUs)}
}

// exempt reports whether a replica of g GPUs grown next is Kueue's to
// reclaim whole: nothing of After left, and all of it within GPUs. A replica
// that takes part of After first -- a 16-GPU replica with 8 of After left --
// is admitted on that part without a preemption, so the pods holding it need
// a node hole like any growth; only its preempting part would not. The
// replica is then fitted to a node whole, the cautious side (NEW-2, M2):
// exempted whole, half its gang waited Pending for a hole no preemption
// frees.
func (r Reclaim) exempt(g int) bool {
	return !r.needsHole && r.After <= 0 && r.GPUs >= g
}

// take is what a replica of g GPUs grown next consumes: After first -- its
// Kept part first within it -- then GPUs, each clamped at 0.
func (r Reclaim) take(g int) Reclaim {
	after := min(g, max(0, r.After))
	return Reclaim{After: after, Kept: min(after, max(0, r.Kept)), GPUs: min(g-after, max(0, r.GPUs))}
}

// reclaimFor is ns's Reclaim of the type acc, as constraints read it: from
// NamespaceBorrow where the namespace is counted, else from NamespaceNominal.
// Two entries reading one queue give the cautious figure: the smaller GPUs,
// the larger After, the smaller Kept.
func reclaimFor(constraints []*ResourceConstraints, ns, acc string) Reclaim {
	var got Reclaim
	seen := false
	take := func(lentOut, after, kept int) {
		if !seen {
			got, seen = Reclaim{After: after, Kept: min(kept, after), GPUs: lentOut}, true
			return
		}
		got.GPUs, got.After, got.Kept = min(got.GPUs, lentOut), max(got.After, after), min(got.Kept, kept)
	}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		if k, ok := accelerator.FindKey(c.NamespaceBorrow[ns], acc); ok {
			b := c.NamespaceBorrow[ns][k]
			take(b.LentOut, b.PreemptAfter, b.PreemptKept)
		} else if k, ok := accelerator.FindKey(c.NamespaceNominal[ns], acc); ok {
			f := c.NamespaceNominal[ns][k]
			take(f.LentOut, f.PreemptAfter, f.PreemptKept)
		}
	}
	return got
}

// lentLeft is, per type, the nominal ns's reclaiming queues lent out that the
// pass has not grown into yet: from its NamespaceBorrow when counted, else
// from its NamespaceNominal. Nil when none.
func (bs *borrowState) lentLeft(ns string) map[string]int {
	if bs == nil {
		return nil
	}
	left := bs.borrow[ns]
	if left == nil {
		left = bs.lent[ns]
	}
	var out map[string]int
	for t, b := range left {
		if b.lentOut > 0 {
			if out == nil {
				out = map[string]int{}
			}
			out[t] = b.lentOut
		}
	}
	return out
}

// withLent is available with, for namespace ns, what its reclaiming queues
// lent out and it has not grown into yet added to every bounded key covering
// the type: the borrowers hold those GPUs now, and Kueue preempts them to
// admit ns's growth, so the cluster bounds -- the physical limiter's free
// GPUs, a cluster quota -- must not refuse it for their being in use. What
// the model's own variants grew into that way and have not re-placed yet
// (reclaimCredit, NEW-1) is added too: DeductReclaimed took it off LentOut,
// and it is still theirs to re-place. The input when there is nothing to add.
func (bs *borrowState) withLent(ns string, variants []string, available map[string]int) map[string]int {
	lent := bs.lentLeft(ns)
	if bs != nil {
		for _, v := range variants {
			for t, r := range bs.reclaimCredit[ns+"/"+v] {
				n := r.GPUs
				if n <= 0 {
					continue
				}
				if lent == nil {
					lent = map[string]int{}
				}
				if k, ok := accelerator.FindKey(lent, t); ok {
					lent[k] += n
				} else {
					lent[t] = n
				}
			}
		}
	}
	if len(lent) == 0 || available == nil {
		return available
	}
	out := maps.Clone(available)
	for t, n := range lent {
		for k, v := range out {
			if !isUnboundedBudget(v) && accelerator.SameName(k, t) {
				out[k] = v + n
			}
		}
	}
	return out
}

// CreditReclaimable returns copies of constraints whose cluster bounds (every
// provider that is not namespace-scoped: the physical limiter, a cluster
// quota) count reclaimable -- per type, the GPUs a namespace's reclaiming
// queues take back from borrowers (ReclaimableNominal, Reclaim.GPUs) -- as
// free for that namespace's wake: the borrowers hold those GPUs, and Kueue
// preempts them to admit it. Unused nominal nobody borrows is not credited:
// its GPUs are as free, or as used by others, as the bounds say. The input
// when reclaimable is empty.
func CreditReclaimable(constraints []*ResourceConstraints, reclaimable map[string]Reclaim) []*ResourceConstraints {
	if len(reclaimable) == 0 {
		return constraints
	}
	out := make([]*ResourceConstraints, 0, len(constraints))
	for _, c := range constraints {
		if c == nil || c.NamespaceScoped || len(c.Pools) == 0 {
			out = append(out, c)
			continue
		}
		cc := *c
		cc.Pools = maps.Clone(c.Pools)
		for t, p := range cc.Pools {
			for rt, r := range reclaimable {
				if accelerator.SameName(rt, t) {
					p.Used = max(0, p.Used-r.GPUs)
				}
			}
			cc.Pools[t] = p
		}
		cc.TotalLimit, cc.TotalUsed, cc.TotalAvail = poolTotals(cc.Pools)
		out = append(out, &cc)
	}
	return out
}

// NominalLentOutAny reports whether some queue of ns, counted or not, lent
// nominal it cannot reclaim (NamespaceBorrow and NamespaceNominal's
// NominalLentOut): a wake refused within nominal then says why (B10).
func NominalLentOutAny(constraints []*ResourceConstraints, ns string) bool {
	if NominalLentOut(constraints, ns) > 0 {
		return true
	}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for _, f := range c.NamespaceNominal[ns] {
			if f.NominalLentOut > 0 {
				return true
			}
		}
	}
	return false
}

// WithinNominalNow is WithinNominal for a wake (A1, W7): the figures as of
// what the scaling manager asked for that the Kueue read does not show yet
// (u: borrowed draws in flight and lagging, gated and admitted pods) and the
// cohorts closed to new borrowing (blocked). A queue that cannot reclaim
// reaches past what it kept only through the cohort's free pool, so its
// within-nominal figure loses what of the pool those take: the steady-state
// engine's last free 8 GPUs, drawn for a decode a moment ago, are not there
// for a cold wake too. A namespace read and not counted is held at its
// queues' within-nominal figure (NamespaceNominal), its nominal otherwise,
// the same way. A reclaiming queue reaches its whole nominal whatever the
// pool holds, and is unchanged.
func WithinNominalNow(constraints []*ResourceConstraints, u Unreserved, blocked map[string]bool) []*ResourceConstraints {
	after := BlockCohorts(DeductUnreservedAt(constraints, u), blocked)
	adjusted := make([]*ResourceConstraints, 0, len(after))
	for i, c := range after {
		before := constraints[i]
		if c == nil || (c.NamespaceBorrow == nil && len(c.NamespaceNominal) == 0) {
			adjusted = append(adjusted, c)
			continue
		}
		cc := *c
		taken := func(ns, acc string) int {
			return max(0, chunkFreeFor(before, ns, acc)-chunkFreeFor(c, ns, acc))
		}
		if c.NamespaceBorrow != nil {
			cc.NamespaceBorrow = make(map[string]map[string]NamespaceBorrow, len(c.NamespaceBorrow))
			for ns, perType := range c.NamespaceBorrow {
				out := maps.Clone(perType)
				for t, b := range out {
					g := poolLimit(c, ns, t)
					if g < 0 || b.WithinNominal <= g {
						continue
					}
					b.WithinNominal = max(g, b.WithinNominal-taken(ns, t))
					out[t] = b
				}
				cc.NamespaceBorrow[ns] = out
			}
		}
		if len(c.NamespaceNominal) > 0 {
			cc.NamespacePools = maps.Clone(c.NamespacePools)
			for ns, perType := range c.NamespaceNominal {
				pools, ok := cc.NamespacePools[ns]
				if !ok {
					continue
				}
				pools = maps.Clone(pools)
				for t, f := range perType {
					k, found := accelerator.FindKey(pools, t)
					if !found {
						continue
					}
					p := pools[k]
					within := f.WithinNominal
					if within > f.Guaranteed {
						within = max(f.Guaranteed, within-taken(ns, t))
					}
					if p.Limit < 0 || within < p.Limit {
						p.Limit = within
					}
					pools[k] = p
				}
				cc.NamespacePools[ns] = pools
			}
		}
		adjusted = append(adjusted, &cc)
	}
	return WithinNominal(adjusted)
}

// chunkFreeFor is the free GPUs of ns's cohorts' chunks in c serving acc.
func chunkFreeFor(c *ResourceConstraints, ns, acc string) int {
	if c == nil {
		return 0
	}
	n := 0
	for _, p := range c.SharedPools {
		if slices.Contains(c.NamespaceCohorts[ns], p.Cohort) && p.serves(acc) {
			n += p.Free
		}
	}
	return n
}

// poolLimit is ns's pool Limit of type t in c (its guaranteed figure where
// borrowing is counted); config.QuotaUnlimited when it has none.
func poolLimit(c *ResourceConstraints, ns, t string) int {
	if k, ok := accelerator.FindKey(c.NamespacePools[ns], t); ok {
		return c.NamespacePools[ns][k].Limit
	}
	return config.QuotaUnlimited
}

// ReclaimableNominal is, per accelerator type, how ns's growth within its
// reclaiming queues' nominal splits (Reclaim): a wake past After and within
// GPUs -- what peers borrow that Kueue preempts them for -- needs no node
// hole. Types with nothing to reclaim are left out. Read from the constraints
// before WithinNominal drops the borrowing figures.
func ReclaimableNominal(constraints []*ResourceConstraints, ns string) map[string]Reclaim {
	types := map[string]bool{}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for t := range c.NamespaceBorrow[ns] {
			types[t] = true
		}
		for t := range c.NamespaceNominal[ns] {
			types[t] = true
		}
	}
	var out map[string]Reclaim
	for t := range types {
		if r := reclaimFor(constraints, ns, t); r.GPUs > 0 {
			if out == nil {
				out = map[string]Reclaim{}
			}
			out[t] = r
		}
	}
	return out
}
