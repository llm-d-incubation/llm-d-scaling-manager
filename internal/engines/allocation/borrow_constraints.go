package allocation

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// NamespaceBorrow is one namespace's borrowing figures for one accelerator type
// (the Kueue borrowing proposal, PR ev-shindin/llm-scaling-manager#161, B3 and B4). The
// namespace's pool Limit is its guaranteed figure; these say what lies past it.
type NamespaceBorrow struct {
	// Cap is how many GPUs past the guaranteed Limit the namespace may reach,
	// drawn from its cohorts' shared pools: the queues' borrowing limits plus,
	// for a queue that cannot reclaim, the nominal it lent out, bounded by the
	// static entry and the namespace ResourceQuota. config.QuotaUnlimited when
	// nothing but the shared pool bounds it.
	Cap int
	// WithinNominal is the namespace's Limit within nominal (B7): what
	// scale-from-zero, the warm pool and rescale may spend. config.QuotaUnlimited
	// when the static entry leaves the type unlimited and Kueue does not bound
	// it either.
	WithinNominal int
	// LentOut is the part of the guaranteed figure that peers borrow now and
	// Kueue preempts them for (kueue.QueueFigures.LentOut): nominal of a
	// reclaiming queue (reclaimWithinCohort Any) past PreemptAfter, capped at
	// what the cohort's other members borrow. Only a whole replica within it,
	// grown past PreemptAfter, may place without a node hole, since the
	// borrowers' pods hold its GPUs; it debits the shared pool in the same
	// cycle, clamped at 0, so no other queue borrows the same GPUs.
	LentOut int
	// PreemptAfter is the guaranteed growth Kueue admits before it preempts
	// for the namespace: what its reclaiming queues keep unused plus the part
	// of their unused nominal the cohort's free pool covers. It needs a node
	// hole like any replica, and debits the shared pool -- all but its
	// PreemptKept part.
	PreemptAfter int
	// PreemptKept is the part of PreemptAfter the namespace's reclaiming
	// queues keep unused: no cohort pool holds it, so growth into it debits
	// none (M-m1). Taken first.
	PreemptKept int
	// NominalLentOut is the part of the namespace's nominal quota it cannot
	// reach: a queue that cannot reclaim lent it to a peer, so its guaranteed
	// figure is below nominal (kueue.ReasonNominalLentOut). Reported, not
	// spent: it is why a capped model of the namespace sits below nominal.
	NominalLentOut int
	// Frozen marks a namespace whose ResourceQuota read failed for a reason
	// that may pass: its figures are the last it read, and nothing new is
	// borrowed for it (its draw allowance is 0) until a read passes. What it
	// holds stays, and its placement-timeout releases still apply.
	Frozen bool
	// Withheld is, on a Frozen namespace whose borrowing is withheld (say
	// not-kueue-managed) while it still holds borrowed GPUs, why: it borrows
	// nothing new, and its cohort's lenders are still read, so their wait still
	// stops new borrowing. Empty otherwise.
	Withheld string
	// PinRequired is set when a variant of the type must pin one of PinLabels
	// -- the nodeLabels of the flavors its queues offer the type on -- to be
	// counted (kueue.ReasonFlavorUnpinned): a queue offers more than one
	// flavor for the GPU. A flavor without nodeLabels cannot be pinned and is
	// not listed.
	PinRequired bool
	PinLabels   []map[string]string
	// CoHeadroom is, per co-resource, how far the namespace's queues may still
	// grow their usage of it, in milli-units: nominal plus borrowingLimit less
	// usage (K2b). A co-resource absent is bounded by the pool alone. CoKept
	// is the part of it they keep unlent and do not use, which no pool holds.
	// Every replica placed while the namespace counts borrowing spends both.
	CoHeadroom, CoKept map[string]int64
	// HighestPriority is the highest priority of the namespace's unfinished
	// GPU Workloads, nil when none was read: the priority of a model whose
	// pod template names no priority class (SharedPoolChunk.drawable).
	HighestPriority *int32
	// Flavors are the flavors the namespace's own queues offer the type on
	// (their ExternalBorrow rows), sorted: the ones Kueue admits its replicas
	// on within nominal, out of every flavor its cohorts' chunks serve
	// (queueFlavor, N2).
	Flavors []string
}

// SharedPoolChunk is one cohort's free GPUs of one flavor and GPU resource. A
// replica uses one flavor, so a borrowed replica is placed whole into one chunk,
// and on that flavor's nodes.
type SharedPoolChunk struct {
	Cohort   string
	Flavor   string
	Resource string
	kueue.FlavorPlacement
	// Free is what the chunk still holds.
	Free int
	// Lenders are the cohort's members declaring this flavor and resource, as
	// the read saw them; the governor debounces their waiting.
	Lenders []kueue.LenderState
	// ReadAt is when the Kueue snapshot behind the chunk was read.
	ReadAt time.Time
	// Stale marks a chunk frozen over a stale read: Free is 0, and nothing
	// about its lenders may change what has been decided.
	Stale bool
	// PendingGPUs is what every member queue's pending Workloads ask of this
	// chunk, whichever namespace or controller they belong to (B8): taken out
	// of Free before the optimizer runs (DeductUnreserved).
	PendingGPUs int
	// CoFree is, per other resource the flavor's resource group covers, the
	// cohort's free pool of it in milli-units: a borrowed replica must fit it
	// too, and is debited its per-replica request (B1).
	CoFree map[string]int64
	// CoUnbound are co-resources charged on a flavor not known here (their
	// resource group offers several): a variant that could borrow from the
	// chunk is withheld as co-resource-bound (K2a).
	CoUnbound []string
	// Restricted are the GPUs of Free that lenders on reclaimWithinCohort
	// LowerPriority lend: Kueue takes them back only from a Workload of lower
	// priority than the lender's, so only a model of lower priority may draw
	// on them (drawable).
	Restricted []PriorityLend
	// ClassPriority is, per priority class (kueue.TemplatePriorityClass), the
	// priority Kueue gives a Workload of that class, as read from the
	// Workloads. A model's priority is looked up here.
	ClassPriority map[string]int32
	// Borrowed is what the cohort's members borrow of the chunk's key in the
	// read (Kueue's status borrowed), and BorrowerPriority the highest
	// priority it is held at -- nil, while Borrowed is not 0, when a
	// borrower's priority was not read (kueue.SharedPool). A Restricted
	// lender that does not outrank it stays closed (drawable, M1).
	Borrowed         int
	BorrowerPriority *int32
	// BorrowerLowest is the lowest priority any borrower holds the cohort's
	// GPUs at (kueue.SharedPool.BorrowerLowest); nil when not known. A
	// LowerPriority lender above it may have Kueue preempt for it.
	BorrowerLowest *int32
	// EvictingGPUs is what Workloads Kueue preempted in the cohort still hold
	// reserved while their pods stop; EvictionsRead whether that is known
	// (kueue.SharedPool).
	EvictingGPUs  int
	EvictionsRead bool
	// StuckEvictingGPUs is what victims evicted longer ago than
	// kueue.EvictionBound still hold: no longer waited for.
	StuckEvictingGPUs int
	// ReleaseToNonReclaiming is set when the entry that read the chunk opts
	// in to kueue.releaseToNonReclaimingLenders (PR-C): Free then counts what
	// lenders Kueue does not reclaim for lend, and the governor hands it back
	// when such a lender waits. Off, it is not counted and a waiting lender
	// only closes the cohort to new borrowing.
	ReleaseToNonReclaiming bool
	// RawFree is the cohort's free pool as Kueue's read gives it
	// (kueue.SharedPool.Free), before anything here takes from it: what Kueue
	// admits a waiting lender from with nothing handed back. A release to a
	// lender is sized net of it.
	RawFree int
	// restrictedClosed is set, for the rest of the pass, once a model that
	// some Restricted lender does not outrank drew on the chunk (K2-2), and
	// before the pass when such a model's draw is still in flight
	// (BorrowDraw.ClosesRestricted, DeductUnreservedAt; M1): Kueue
	// does not say whose lending a borrowed GPU is, and preempts the lowest
	// priority first when any lender reclaims. Were a model it outranks to
	// draw the restricted part after, an Any lender's reclaim would take that
	// model, and the LowerPriority lender could never take back what the
	// outranking model holds. Closed, the restricted part is drawable by no
	// model, whatever the placement order.
	restrictedClosed bool
}

// coWithin reports whether one replica's requests fit a namespace's
// co-resource headroom (NamespaceBorrow.CoHeadroom, K2b).
func coWithin(headroom, requests map[string]int64) bool {
	for res, h := range headroom {
		if requests[res] > h {
			return false
		}
	}
	return true
}

// coFitsKept reports whether one replica's requests fit, per co-resource the
// chunk's pool holds, that pool plus what the namespace keeps unlent. A
// co-resource the replica does not request is taken as 0 here: a variant
// whose request of one is unknown is withheld before the pass
// (WithholdByVariants).
func coFitsKept(free, kept, requests map[string]int64) bool {
	for res, f := range free {
		if requests[res] > f+kept[res] {
			return false
		}
	}
	return true
}

// spendCo debits one replica's co-resource requests: the namespace's
// headroom in full, and of each request what the namespace keeps first, the
// rest from chunk c's pool (none when c is nil).
func spendCo(b *nsBorrowLeft, c *SharedPoolChunk, requests map[string]int64) {
	for res, n := range requests {
		if h, ok := b.coHeadroom[res]; ok {
			b.coHeadroom[res] = h - n
		}
		k := min(b.coKept[res], n)
		if k > 0 {
			b.coKept[res] -= k
		}
		if c == nil {
			continue
		}
		if f, ok := c.CoFree[res]; ok {
			c.CoFree[res] = f - (n - k)
		}
	}
}

// coGuaranteed fits one replica placed within the guaranteed figure to the
// co-resources (K2c): Kueue admits it only when every resource it requests
// fits, its CPU and memory as much as its GPUs. It must fit the namespace's
// headroom and, unless the queue reclaims what it lent (reclaim: Kueue
// preempts borrowers of it), one of its cohorts' chunks serving acc --
// pool plus what the namespace keeps -- which it is debited. b nil (the
// namespace counts no borrowing) checks its queues' nominal co-resource
// headroom only (NominalFigures.CoHeadroom), when the constraints carry one.
func (bs *borrowState) coGuaranteed(ns, acc string, b *nsBorrowLeft, requests map[string]int64, reclaim bool) bool {
	if b == nil {
		k, ok := accelerator.FindKey(bs.lent[ns], acc)
		if !ok || len(bs.lent[ns][k].coHeadroom) == 0 {
			return true
		}
		nominal := bs.lent[ns][k]
		if !coWithin(nominal.coHeadroom, requests) {
			return false
		}
		spendCo(nominal, nil, requests)
		return true
	}
	if !coWithin(b.coHeadroom, requests) {
		return false
	}
	var withCo []*SharedPoolChunk
	if !reclaim {
		for _, c := range bs.cohortChunks(ns, acc) {
			if len(c.CoFree) > 0 {
				withCo = append(withCo, c)
			}
		}
	}
	if len(withCo) == 0 {
		spendCo(b, nil, requests)
		return true
	}
	for _, c := range withCo {
		if coFitsKept(c.CoFree, b.coKept, requests) {
			spendCo(b, c, requests)
			return true
		}
	}
	return false
}

// PriorityLend is what one LowerPriority lender lends into a chunk: GPUs a
// model may borrow only when its priority is below Below.
type PriorityLend struct {
	Queue string
	Below int32
	GPUs  int
}

// drawable is what a model of the given priority class may draw on the chunk: Free
// less the restricted GPUs whose lender does not outrank it. A class whose
// priority is not known may draw on none of them.
//
// A template that names no class (kueue.PriorityClassNone) may still run at a
// class set on the scale target object itself, which the template does not
// show, so its priority is the higher of the no-class Workloads' and
// fallback, the highest priority of its namespace's Workloads (nil when none
// was read): the side that draws less.
func (c *SharedPoolChunk) drawable(class string, fallback *int32) int {
	prio, known := c.ClassPriority[class]
	if class == kueue.PriorityClassNone && fallback != nil {
		if !known || *fallback > prio {
			prio = *fallback
		}
		known = true
	}
	free := c.Free
	for _, r := range c.Restricted {
		if c.restrictedClosed || c.heldAbove(r) || !known || !kueue.Reclaims(kueue.ReclaimLowerPriority, &r.Below, &prio) {
			free -= r.GPUs
		}
	}
	return max(0, free)
}

// heldAbove reports whether the read shows the chunk's key borrowed at a
// priority the LowerPriority lender r does not outrank, or at one not read
// (M1, NEW-4). The K2-2 closure then holds across cycles, not for one pass:
// a model drawing r's lending now would be the one an Any lender's reclaim
// preempts first, and r could never take back what the higher priority
// holds.
func (c *SharedPoolChunk) heldAbove(r PriorityLend) bool {
	if c.Borrowed <= 0 {
		return false
	}
	return c.BorrowerPriority == nil || !kueue.Reclaims(kueue.ReclaimLowerPriority, &r.Below, c.BorrowerPriority)
}

// key identifies a chunk across providers that read the same cohort.
func (c SharedPoolChunk) key() string {
	return c.Cohort + "/" + c.Flavor + "/" + c.Resource
}

// serves reports whether a replica on accelerator acc may be placed into the
// chunk: the flavor names that accelerator by identity, or names none (an
// untyped flavor, which Kueue's quickstart uses, admits any).
func (c SharedPoolChunk) serves(acc string) bool {
	return c.AcceleratorType == "" || accelerator.SameName(c.AcceleratorType, acc)
}

// HasBorrowing reports whether any constraint counts cohort borrowing. Every
// borrowing path is skipped when it does not, so a cycle without it is exactly
// the cycle before borrowing was counted.
func HasBorrowing(constraints []*ResourceConstraints) bool {
	return slices.ContainsFunc(constraints, func(c *ResourceConstraints) bool {
		return c != nil && c.NamespaceBorrow != nil
	})
}

// hasNominalCo reports whether some constraint carries a namespace's nominal
// co-resource headroom (NominalFigures.CoHeadroom): the pass then fits growth
// within nominal to it, borrowing counted or not (K2).
func hasNominalCo(constraints []*ResourceConstraints) bool {
	return slices.ContainsFunc(constraints, func(c *ResourceConstraints) bool {
		if c == nil {
			return false
		}
		for _, perType := range c.NamespaceNominal {
			for _, f := range perType {
				if len(f.CoHeadroom) > 0 {
					return true
				}
			}
		}
		return false
	})
}

// NominalLentOut is the nominal of ns's queues that a peer holds and they
// cannot reclaim (NamespaceBorrow.NominalLentOut), summed over constraints and
// types.
func NominalLentOut(constraints []*ResourceConstraints, ns string) int {
	n := 0
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for _, b := range c.NamespaceBorrow[ns] {
			n += b.NominalLentOut
		}
	}
	return n
}

// WithinNominal returns copies of constraints for the consumers that must never
// spend borrowed capacity: scale-from-zero, the warm pool and rescale (B7). A
// model woken onto borrowed GPUs can be preempted before it serves.
//
// Each borrowing namespace's Limit becomes its within-nominal figure and the
// borrowing figures are dropped; the reported aggregate is rebuilt from the new
// limits, as DefaultLimiter builds it. Within nominal is not the guaranteed
// figure: under Kueue's default (reclaimWithinCohort: Never) an idle queue's
// guaranteed figure is 0, and that would stop every idle namespace on a default
// setup from waking, which it can today. Constraints without borrowing are
// returned as they are, so without it this is the identity.
func WithinNominal(constraints []*ResourceConstraints) []*ResourceConstraints {
	return withoutBorrowing(constraints, true)
}

// GuaranteedOnly returns copies of constraints without the borrowing figures,
// each namespace at its guaranteed Limit: the view of the utilization-share
// optimizer, which budgets the guaranteed figure plus the borrowed GPUs a group
// holds now (its free figure clamps at 0 and adds back what the group holds), so
// held borrowed GPUs are never debt, and starts no new borrowing (B4). Its
// ClusterFree reads only the cluster bounds -- a namespace-scope provider, the
// only one that borrows, is never one -- and so never includes a shared pool.
//
// A borrowing namespace's Used is charged only what it holds past its borrow
// cap beyond the guaranteed figure (heldPastBorrowCap): the share takes a
// namespace holding more than its quota allows back within it (overQuota),
// and borrowed GPUs within the cap are not that.
func GuaranteedOnly(constraints []*ResourceConstraints) []*ResourceConstraints {
	return withoutBorrowing(constraints, false)
}

// heldPastBorrowCap is p's Used with the GPUs held past its guaranteed Limit
// and within the borrow cap b taken out: unchanged within the Limit, the Limit
// within the cap, and Used less the cap past it. An unlimited Limit or cap
// leaves nothing past them.
func heldPastBorrowCap(p ResourcePool, b NamespaceBorrow) int {
	switch {
	case p.Limit < 0:
		return p.Used
	case b.Cap < 0:
		return min(p.Used, p.Limit)
	}
	return max(min(p.Used, p.Limit), p.Used-b.Cap)
}

// withoutBorrowing drops the borrowing figures from every constraint carrying
// them, moving each borrowing namespace's Limit to its within-nominal figure
// when within is set, and its Used to heldPastBorrowCap, recorded in
// CountedBorrow, when it is not.
func withoutBorrowing(constraints []*ResourceConstraints, within bool) []*ResourceConstraints {
	if !HasBorrowing(constraints) {
		return constraints
	}
	out := make([]*ResourceConstraints, 0, len(constraints))
	for _, c := range constraints {
		if c == nil || c.NamespaceBorrow == nil {
			out = append(out, c)
			continue
		}
		cc := *c
		cc.NamespacePools = make(map[string]map[string]ResourcePool, len(c.NamespacePools))
		if !within {
			cc.CountedBorrow = map[string]map[string]bool{}
		}
		for ns, perType := range c.NamespacePools {
			pools := maps.Clone(perType)
			for accType, b := range c.NamespaceBorrow[ns] {
				p, ok := pools[accType]
				if !ok {
					continue
				}
				if within {
					p.Limit = b.WithinNominal
				} else {
					p.Used = heldPastBorrowCap(p, b)
					if cc.CountedBorrow[ns] == nil {
						cc.CountedBorrow[ns] = map[string]bool{}
					}
					cc.CountedBorrow[ns][accType] = true
				}
				pools[accType] = p
			}
			cc.NamespacePools[ns] = pools
		}
		cc.NamespaceBorrow, cc.SharedPools, cc.NamespaceCohorts = nil, nil, nil
		cc.Pools = aggregateNamespacePools(cc.NamespacePools)
		cc.TotalLimit, cc.TotalUsed, cc.TotalAvail = poolTotals(cc.Pools)
		out = append(out, &cc)
	}
	return out
}

// reachPools is nsPools with each borrowing type's Limit raised by its cap: the
// most the namespace may reach. An unlimited cap is the unlimited sentinel.
// Used for the reported aggregate of a borrowing provider and for the greedy
// optimizer's upper bound per model.
func reachPools(nsPools map[string]map[string]ResourcePool, borrow map[string]map[string]NamespaceBorrow) map[string]map[string]ResourcePool {
	out := make(map[string]map[string]ResourcePool, len(nsPools))
	for ns, perType := range nsPools {
		pools := maps.Clone(perType)
		for accType, b := range borrow[ns] {
			p, ok := pools[accType]
			if !ok || p.Limit < 0 {
				continue
			}
			if b.Cap < 0 {
				p.Limit = config.QuotaUnlimited
			} else {
				p.Limit += b.Cap
			}
			pools[accType] = p
		}
		out[ns] = pools
	}
	return out
}

// reachConstraints returns copies of the borrowing constraints with each
// namespace at its reach, for the greedy optimizer's per-model upper bound.
func reachConstraints(constraints []*ResourceConstraints) []*ResourceConstraints {
	out := make([]*ResourceConstraints, 0, len(constraints))
	for _, c := range constraints {
		if c == nil || c.NamespaceBorrow == nil {
			out = append(out, c)
			continue
		}
		cc := *c
		cc.NamespacePools = reachPools(c.NamespacePools, c.NamespaceBorrow)
		out = append(out, &cc)
	}
	return out
}

// mergeSharedPools collects every provider's chunks, one per cohort, flavor and
// resource at the smallest figure any provider gives, in a stable order, and the
// namespaces' cohorts. Two Kueue-backed entries read the same cohorts; their
// chunks are one pool, not two. The merge takes the cautious side of every
// field: stale when either read is, the older read time (a lender's read is
// new only once both entries read past it), the union of the lenders -- a
// lender seen by both waits when either saw it wait -- and of the restricted
// GPUs, each at the larger figure and the lower lender priority, and per
// priority class the higher priority.
func mergeSharedPools(constraints []*ResourceConstraints) ([]*SharedPoolChunk, map[string][]string) {
	byKey := map[string]*SharedPoolChunk{}
	cohorts := map[string][]string{}
	for _, c := range constraints {
		if c == nil || c.NamespaceBorrow == nil {
			continue
		}
		for _, p := range c.SharedPools {
			if prev, ok := byKey[p.key()]; ok {
				mergeChunk(prev, p)
				continue
			}
			chunk := p
			chunk.Lenders = slices.Clone(p.Lenders)
			chunk.CoFree = maps.Clone(p.CoFree)
			chunk.CoUnbound = slices.Clone(p.CoUnbound)
			chunk.Restricted = slices.Clone(p.Restricted)
			chunk.ClassPriority = maps.Clone(p.ClassPriority)
			byKey[p.key()] = &chunk
		}
		for ns, cs := range c.NamespaceCohorts {
			for _, co := range cs {
				if !slices.Contains(cohorts[ns], co) {
					cohorts[ns] = append(cohorts[ns], co)
				}
			}
		}
	}
	chunks := slices.Collect(maps.Values(byKey))
	slices.SortFunc(chunks, func(a, b *SharedPoolChunk) int {
		return cmp.Or(strings.Compare(a.Cohort, b.Cohort), strings.Compare(a.Flavor, b.Flavor), strings.Compare(a.Resource, b.Resource))
	})
	return chunks, cohorts
}

// mergeChunk folds p, another provider's read of the same chunk, into prev
// (mergeSharedPools).
func mergeChunk(prev *SharedPoolChunk, p SharedPoolChunk) {
	prev.Free = min(prev.Free, p.Free)
	prev.RawFree = min(prev.RawFree, p.RawFree)
	prev.PendingGPUs = max(prev.PendingGPUs, p.PendingGPUs)
	for res, n := range p.CoFree {
		if have, ok := prev.CoFree[res]; !ok || n < have {
			if prev.CoFree == nil {
				prev.CoFree = map[string]int64{}
			}
			prev.CoFree[res] = n
		}
	}
	for _, res := range p.CoUnbound {
		if !slices.Contains(prev.CoUnbound, res) {
			prev.CoUnbound = append(prev.CoUnbound, res)
		}
	}
	slices.Sort(prev.CoUnbound)
	prev.Stale = prev.Stale || p.Stale
	// Released only when every entry reading the cohort opts in: one that
	// does not counts none of that lending (its Free is the smaller), and a
	// release it never asked for would lower its models.
	prev.ReleaseToNonReclaiming = prev.ReleaseToNonReclaiming && p.ReleaseToNonReclaiming
	prev.restrictedClosed = prev.restrictedClosed || p.restrictedClosed
	// The borrowed figure: the larger, at the higher priority, unknown when
	// either read holds borrowing at a priority it did not read.
	unknown := (prev.Borrowed > 0 && prev.BorrowerPriority == nil) || (p.Borrowed > 0 && p.BorrowerPriority == nil)
	switch {
	case unknown:
		prev.BorrowerPriority = nil
	case p.BorrowerPriority != nil && (prev.BorrowerPriority == nil || *p.BorrowerPriority > *prev.BorrowerPriority):
		bp := *p.BorrowerPriority
		prev.BorrowerPriority = &bp
	}
	// The lowest borrower priority: the lower, unknown when either read has
	// borrowing without one.
	switch {
	case (prev.Borrowed > 0 && prev.BorrowerLowest == nil) || (p.Borrowed > 0 && p.BorrowerLowest == nil):
		prev.BorrowerLowest = nil
	case p.BorrowerLowest != nil && (prev.BorrowerLowest == nil || *p.BorrowerLowest < *prev.BorrowerLowest):
		lo := *p.BorrowerLowest
		prev.BorrowerLowest = &lo
	}
	prev.Borrowed = max(prev.Borrowed, p.Borrowed)
	prev.EvictingGPUs = max(prev.EvictingGPUs, p.EvictingGPUs)
	prev.StuckEvictingGPUs = max(prev.StuckEvictingGPUs, p.StuckEvictingGPUs)
	prev.EvictionsRead = prev.EvictionsRead && p.EvictionsRead
	if !p.ReadAt.IsZero() && (prev.ReadAt.IsZero() || p.ReadAt.Before(prev.ReadAt)) {
		prev.ReadAt = p.ReadAt
	}
	for _, l := range p.Lenders {
		i := slices.IndexFunc(prev.Lenders, func(o kueue.LenderState) bool { return o.Queue == l.Queue })
		if i < 0 {
			prev.Lenders = append(prev.Lenders, l)
			continue
		}
		o := &prev.Lenders[i]
		o.Waiting = o.Waiting || l.Waiting
		// What a release hands it, the side releasing less: the smaller
		// demand, the larger part it keeps, the higher priority (more is left
		// to Kueue's reclaim), the later start of its wait.
		o.PendingGPUs = min(o.PendingGPUs, l.PendingGPUs)
		o.Kept = max(o.Kept, l.Kept)
		if l.PendingPriority != nil && (o.PendingPriority == nil || *l.PendingPriority > *o.PendingPriority) {
			p := *l.PendingPriority
			o.PendingPriority = &p
		}
		if l.PendingSince.After(o.PendingSince) {
			o.PendingSince = l.PendingSince
		}
	}
	slices.SortFunc(prev.Lenders, func(a, b kueue.LenderState) int { return strings.Compare(a.Queue, b.Queue) })
	// Restricted GPUs: the cautious side is the larger figure and the lower
	// lender priority, so fewer models may draw on them.
	for _, r := range p.Restricted {
		i := slices.IndexFunc(prev.Restricted, func(o PriorityLend) bool { return o.Queue == r.Queue })
		if i < 0 {
			prev.Restricted = append(prev.Restricted, r)
			continue
		}
		o := &prev.Restricted[i]
		o.GPUs, o.Below = max(o.GPUs, r.GPUs), min(o.Below, r.Below)
	}
	for class, n := range p.ClassPriority {
		if have, ok := prev.ClassPriority[class]; !ok || n > have {
			if prev.ClassPriority == nil {
				prev.ClassPriority = map[string]int32{}
			}
			prev.ClassPriority[class] = n
		}
	}
}

// tighterCo merges two entries' co-resource figures for one namespace: per
// co-resource the smaller. For headroom, a co-resource one entry leaves
// unbounded takes the other's bound (union); for what is kept, only one both
// read counts (intersection).
func tighterCo(a, b map[string]int64, union bool) map[string]int64 {
	out := map[string]int64{}
	for res, n := range a {
		if m, ok := b[res]; ok {
			out[res] = min(n, m)
		} else if union {
			out[res] = n
		}
	}
	if union {
		for res, m := range b {
			if _, ok := a[res]; !ok {
				out[res] = m
			}
		}
	}
	return out
}
