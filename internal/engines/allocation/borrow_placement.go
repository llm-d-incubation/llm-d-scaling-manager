package allocation

import (
	"maps"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// BorrowDraw is borrowed capacity the greedy optimizer ordered for one variant
// in one pass: GPUs drawn from one shared-pool chunk, past the namespace's
// guaranteed figure. The engine keeps it as in flight until the pods exist
// (BorrowInFlight), since Kueue reserves nothing for them before.
type BorrowDraw struct {
	Namespace string
	ModelID   string
	Variant   string
	// Cohort, Flavor and Resource name the chunk drawn from.
	Cohort   string
	Flavor   string
	Resource string
	// GPUs is the borrowed part of the replicas ordered, Replicas how many
	// replicas drew on it.
	GPUs     int
	Replicas int
	// Target is the variant's target after the pass.
	Target int
	// Placement is the flavor's node placement the replicas were fitted to:
	// the in-flight ledger holds their holes on those nodes until the pods
	// hold them (ShapeOfDraws).
	Placement *kueue.FlavorPlacement
	// OrderedAt is when the record first took it (BorrowInFlight.RecordAt);
	// ResolvedAt when its pods were seen to exist, zero while in flight.
	OrderedAt, ResolvedAt time.Time
	// CreditGPUs and CreditReplicas are the part of a pass's draw re-placed
	// on the variant's own draw still in flight (GreedyByScoreOptimizer.
	// BorrowCredit): borrowed this pass, already in the record.
	CreditGPUs, CreditReplicas int
	// Reclaim marks growth within a reclaiming queue's nominal (Reclaim): GPUs
	// is what Kueue admits by preempting borrowers (Reclaim.GPUs), After what
	// it admits before (Reclaim.After), Kept the part of After the queues keep
	// unused. Held in flight so that no later decision -- the next cycle, a
	// wake -- counts the same borrowed GPUs as reclaimable again, nor the same
	// PreemptAfter (DeductReclaimed), and the pool part of After, After less
	// Kept, comes off the pool (DeductUnreservedAt). Cohort, Flavor and
	// Resource name the chunk whose read shows it once it lands, and its pool
	// part comes off; empty when the namespace's cohorts have none serving
	// the accelerator.
	Reclaim     bool
	After, Kept int
	// CreditAfter and CreditKept are, on a Reclaim entry, the part of After
	// and Kept re-placed on the variant's own entry in flight, as CreditGPUs
	// is of GPUs.
	CreditAfter, CreditKept int
	// Accelerator is the type a Reclaim entry was placed for.
	Accelerator string
	// NoHole marks a Reclaim entry whose replica was ordered without a node
	// hole, Kueue's preemption of borrowers to free one: the governor keeps
	// it under the placement timeout (BorrowCycle.NoHole, M3).
	NoHole bool
	// ClosesRestricted marks a draw by a model that some LowerPriority lender
	// of its chunk does not outrank (K2-2): while it is in flight, or lags a
	// read, the chunk's restricted part stays closed to every model, as the
	// pass that drew it closed it (DeductUnreservedAt, M1).
	ClosesRestricted bool
	// Wake marks an entry a scale-from-zero wake recorded (WakeDraws): its
	// variant may not be among the steady-state engine's yet, and the target
	// it decides is not the wake's, so only its replicas existing, or
	// InFlightTTL, ends it.
	Wake bool
}

// reclaimOf is a Reclaim entry's growth as a Reclaim.
func (d BorrowDraw) reclaimOf() Reclaim {
	return Reclaim{After: d.After, Kept: d.Kept, GPUs: d.GPUs}
}

// nsBorrowLeft is what one namespace may still borrow of one accelerator type
// in this pass.
type nsBorrowLeft struct {
	// cap is the GPUs it may still draw past its guaranteed figure; negative
	// is no cap but the pool's.
	cap int
	// after and lentOut are the namespace's Reclaim still to grow into:
	// guaranteed growth Kueue admits before it preempts for it, then what it
	// reclaims from borrowers (NamespaceBorrow.PreemptAfter and LentOut).
	// Growth into either debits the pool, all but after's kept part (kept,
	// NamespaceBorrow.PreemptKept, M-m1).
	after, kept, lentOut int
	// priority is NamespaceBorrow.HighestPriority, the higher of two entries'.
	priority *int32
	// coHeadroom and coKept are NamespaceBorrow.CoHeadroom and CoKept still
	// to spend (K2).
	coHeadroom, coKept map[string]int64
}

// borrowState is one optimizer pass's borrowing: what each namespace may still
// draw, the shared pools, and the node view. Draws mutate it, so a pool is
// spent once across every namespace in its cohort and a node hole once across
// every model. Nil when no constraint counts borrowing and no Kueue-backed
// namespace carries nominal co-resource headroom (hasNominalCo).
//
// The methods the pass calls whether or not it borrows (reachFor,
// newModelPlacer, drawsByVariant) accept a nil receiver and then answer "no
// borrowing"; the others are reached only through a non-nil state.
type borrowState struct {
	// counts is whether some constraint counts borrowing (HasBorrowing).
	// Without it the state exists only to fit growth within nominal to the
	// co-resources of the namespaces whose constraints carry them (K2): only
	// their models are placed by a placer, and every other path of the pass
	// is as without borrowing.
	counts bool
	borrow map[string]map[string]*nsBorrowLeft
	// ownFlavors is, per namespace and type, the flavors its own queues offer
	// the type on (NamespaceBorrow and NominalFigures.Flavors): what
	// queueFlavor picks among its cohorts' chunks.
	ownFlavors map[string]map[string][]string
	// lent is, for a namespace read and not counted, the nominal of its
	// reclaiming queues a peer holds still to grow into (its lentOut; cap
	// unused): such growth, like a counted namespace's, needs no node hole.
	lent    map[string]map[string]*nsBorrowLeft
	reach   map[string]map[string]int
	chunks  []*SharedPoolChunk
	cohorts map[string][]string
	nodes   *NodeView
	draws   []BorrowDraw
	// credit is, per "namespace/variant" and chunk key, the GPUs of the
	// variant's own draws still in flight (BorrowCredit): the pools were
	// debited them before the pass, and the targets restart from the current
	// replicas, so the pass re-places those very replicas. They are the
	// variant's to re-place, not another draw on the pool (A4).
	credit map[string]map[string]int
	// reclaimCredit is, per "namespace/variant" and accelerator, the GPUs of
	// the variant's own growth by Kueue's reclaim still in flight
	// (ReclaimCredit): DeductReclaimed took them off its namespace's LentOut,
	// and the pass re-places those very replicas, so they are the variant's
	// to re-place, not a second reclaim of the same borrowed GPUs (NEW-1).
	reclaimCredit map[string]map[string]Reclaim
	// flavors is every replica the pass placed on a Kueue flavor's nodes, in
	// order (PlacedFlavors); rolled back with the placement.
	flavors []flavorUse
	// reclaims is every guaranteed replica the pass grew into nominal Kueue
	// reclaims from borrowers (ReclaimDraws); rolled back with the placement.
	reclaims []BorrowDraw
	// wakeReclaims is the wakes' reclaims this pass yields for
	// (GreedyByScoreOptimizer.WakeReclaims); never rolled back.
	wakeReclaims []BorrowDraw
	// blocked reports a variant that may not borrow this pass; nil blocks none.
	blocked func(namespace, modelID, variant string) bool
	// blockedIn reports a model that may not draw on one cohort's pools this
	// pass (its backed-off cooldown there); nil blocks none.
	blockedIn func(namespace, modelID, cohort string) bool
	// reclaimBlocked reports a model whose growth may not skip the node hole
	// this pass (ReclaimBlocked); nil blocks none.
	reclaimBlocked func(namespace, modelID string) bool
	// heldPast is, per namespace and type, the GPUs it holds past its
	// guaranteed figure by the read (namespaceHeldPast): borrowed replicas
	// already landed, whose they are the read does not say.
	heldPast map[string]map[string]int
}

// newBorrowState builds the pass's borrowing state, or nil when no constraint
// counts borrowing and none carries nominal co-resource headroom. nodes is
// the cycle's node view, nil without a physical limiter or a fresh node
// snapshot.
func newBorrowState(constraints []*ResourceConstraints, nodes *NodeView) *borrowState {
	counts := HasBorrowing(constraints)
	if !counts {
		if !hasNominalCo(constraints) {
			return nil
		}
		return newNominalCoState(constraints, nodes)
	}
	bs := &borrowState{counts: true, borrow: map[string]map[string]*nsBorrowLeft{}, nodes: nodes}
	bs.chunks, bs.cohorts = mergeSharedPools(constraints)
	bs.ownFlavors = ownFlavorsOf(constraints)
	bs.heldPast = namespaceHeldPast(constraints)
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns, perType := range c.NamespaceBorrow {
			if bs.borrow[ns] == nil {
				bs.borrow[ns] = map[string]*nsBorrowLeft{}
			}
			for accType, b := range perType {
				drawable := b.Cap
				if b.Frozen {
					drawable = 0 // its ResourceQuota read failed: nothing new
				}
				prev, seen := bs.borrow[ns][accType]
				if !seen {
					bs.borrow[ns][accType] = &nsBorrowLeft{cap: drawable, after: b.PreemptAfter, kept: min(b.PreemptKept, b.PreemptAfter),
						lentOut: b.LentOut, priority: b.HighestPriority,
						coHeadroom: maps.Clone(b.CoHeadroom), coKept: maps.Clone(b.CoKept)}
					continue
				}
				prev.coHeadroom = tighterCo(prev.coHeadroom, b.CoHeadroom, true)
				prev.coKept = tighterCo(prev.coKept, b.CoKept, false)
				// Two Kueue-backed entries hold the namespace: the tighter cap.
				prev.cap = tighterBudget(prev.cap, drawable)
				// Two entries reading one queue: the cautious Reclaim, as
				// reclaimFor and bs.lent take it -- fewer GPUs reclaimed, and
				// later (N-m1).
				prev.lentOut = min(prev.lentOut, b.LentOut)
				prev.after = max(prev.after, b.PreemptAfter)
				prev.kept = min(prev.kept, b.PreemptKept)
				if b.HighestPriority != nil && (prev.priority == nil || *b.HighestPriority > *prev.priority) {
					prev.priority = b.HighestPriority
				}
			}
		}
	}
	bs.lent = map[string]map[string]*nsBorrowLeft{}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns, perType := range c.NamespaceNominal {
			if _, counted := bs.borrow[ns]; counted {
				continue
			}
			if bs.lent[ns] == nil {
				bs.lent[ns] = map[string]*nsBorrowLeft{}
			}
			for accType, f := range perType {
				if prev, seen := bs.lent[ns][accType]; seen {
					prev.lentOut = min(prev.lentOut, f.LentOut)
					prev.after = max(prev.after, f.PreemptAfter)
					prev.kept = min(prev.kept, f.PreemptKept)
					prev.coHeadroom = tighterCo(prev.coHeadroom, f.CoHeadroom, true) // K2, nominal path
					continue
				}
				bs.lent[ns][accType] = &nsBorrowLeft{after: f.PreemptAfter, kept: min(f.PreemptKept, f.PreemptAfter), lentOut: f.LentOut,
					coHeadroom: maps.Clone(f.CoHeadroom)} // K2, nominal path: coGuaranteed
			}
		}
	}
	reach := mergeNamespaceConstraints(reachConstraints(constraints))
	bs.reach = map[string]map[string]int{}
	for ns := range bs.borrow {
		if r, ok := reach[ns]; ok {
			bs.reach[ns] = r
		}
	}
	return bs
}

// newNominalCoState is the pass's state when nothing counts borrowing but some
// namespace's constraints carry nominal co-resource headroom (K2 on the
// nominal path): that headroom alone, per namespace and type, nothing to
// reclaim, no flavor to apply. Only those namespaces' models get a placer
// (newModelPlacer), which checks a replica's co-resources beside its GPUs.
func newNominalCoState(constraints []*ResourceConstraints, nodes *NodeView) *borrowState {
	bs := &borrowState{nodes: nodes, lent: map[string]map[string]*nsBorrowLeft{}}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns, perType := range c.NamespaceNominal {
			for accType, f := range perType {
				if len(f.CoHeadroom) == 0 {
					continue
				}
				if bs.lent[ns] == nil {
					bs.lent[ns] = map[string]*nsBorrowLeft{}
				}
				if prev, seen := bs.lent[ns][accType]; seen {
					prev.coHeadroom = tighterCo(prev.coHeadroom, f.CoHeadroom, true)
					continue
				}
				bs.lent[ns][accType] = &nsBorrowLeft{coHeadroom: maps.Clone(f.CoHeadroom)}
			}
		}
	}
	return bs
}

// reachFor is the namespace's budget at its reach (guaranteed plus borrow
// cap), the upper bound a borrowing namespace's model is sized against; nil
// for a namespace that borrows nothing.
func (bs *borrowState) reachFor(ns string) map[string]int {
	if bs == nil {
		return nil
	}
	return bs.reach[ns]
}

// cohortChunks is the chunks a namespace may draw a replica of acc from, in a
// stable order.
func (bs *borrowState) cohortChunks(ns, acc string) []*SharedPoolChunk {
	var out []*SharedPoolChunk
	for _, c := range bs.chunks {
		if slices.Contains(bs.cohorts[ns], c.Cohort) && c.serves(acc) {
			out = append(out, c)
		}
	}
	return out
}

// debitPool takes n GPUs of acc out of the namespace's cohort chunks, first
// chunk first, each clamped at 0. Used for growth that needs no gate -- a
// reclaiming queue growing into nominal it lent out -- so that no other queue
// borrows the same GPUs in the same cycle.
func (bs *borrowState) debitPool(ns, acc string, n int) {
	for _, c := range bs.cohortChunks(ns, acc) {
		if n <= 0 {
			return
		}
		take := min(c.Free, n)
		c.Free -= take
		n -= take
	}
}

// reclaimDraw is the in-flight entry of one guaranteed replica grown into its
// namespace's Reclaim (BorrowDraw.Reclaim): took.GPUs Kueue admits by
// preempting borrowers, took.After it admits before -- its Kept part from what
// the queues keep, the rest from the cohort's free pool. Named after the
// first of the namespace's cohort chunks serving acc, whose read shows the
// replica once it lands, and the chunk its pool part comes off.
func (bs *borrowState) reclaimDraw(ns, modelID, variant, acc string, took Reclaim, target int) BorrowDraw {
	d := BorrowDraw{Namespace: ns, ModelID: modelID, Variant: variant, GPUs: took.GPUs, After: took.After, Kept: took.Kept,
		Replicas: 1, Target: target, Reclaim: true, Accelerator: acc}
	if cs := bs.cohortChunks(ns, acc); len(cs) > 0 {
		d.Cohort, d.Flavor, d.Resource = cs[0].Cohort, cs[0].Flavor, cs[0].Resource
	}
	return d
}

// saveLeft copies a per-namespace allowance map by value, co-resource
// allowances included.
func saveLeft(m map[string]map[string]*nsBorrowLeft) map[string]map[string]nsBorrowLeft {
	out := make(map[string]map[string]nsBorrowLeft, len(m))
	for ns, perType := range m {
		out[ns] = make(map[string]nsBorrowLeft, len(perType))
		for t, b := range perType {
			c := *b
			c.coHeadroom, c.coKept = maps.Clone(b.coHeadroom), maps.Clone(b.coKept)
			out[ns][t] = c
		}
	}
	return out
}

// restoreLeft puts saveLeft's values back.
func restoreLeft(m map[string]map[string]*nsBorrowLeft, s map[string]map[string]nsBorrowLeft) {
	for ns, perType := range s {
		for t, b := range perType {
			*m[ns][t] = b
		}
	}
}

// borrowSave is a borrowState taken before a tentative placement.
type borrowSave struct {
	borrow     map[string]map[string]nsBorrowLeft
	lent       map[string]map[string]nsBorrowLeft
	free       []int
	closed     []bool
	coFree     []map[string]int64
	nodes      viewDebits
	draws      int
	guaranteed map[string]int
	credit     map[string]map[string]int
	reclaimCr  map[string]map[string]Reclaim
	flavors    int
	reclaims   int
}

// flavorUse is one replica placed on a Kueue flavor's nodes.
type flavorUse struct {
	variant   string // "namespace/variant"
	placement *kueue.FlavorPlacement
	gpus      int
}

// queueFlavor is the flavor Kueue admits ns's replicas of acc on within its
// guaranteed figure (W5): the one flavor its OWN queues offer acc on with a
// nominal quota (N2; a flavor listed at nominal 0 only to borrow on is not
// one, NEW-3) --
// every chunk of its cohorts serving acc on one of those flavors naming the
// same node labels and tolerations. A cohort's chunks are its members'
// flavors together: per-team pools, a's flavor pinned to pool=a and b's to
// pool=b, would otherwise disagree and leave a's replica fitted to b's nodes.
// Without its own flavors known (none read), every chunk of its cohorts
// serving acc. Nil when the namespace is not known to be Kueue-managed
// (neither counted nor carrying its queues' nominal figures), or its queues
// offer acc on more than one placement: Kueue then picks, and the replica is
// fitted by its templates alone.
func (bs *borrowState) queueFlavor(ns, acc string) *kueue.FlavorPlacement {
	_, counted := bs.borrow[ns]
	_, read := bs.lent[ns]
	if !counted && !read {
		return nil
	}
	var own []string
	if k, ok := accelerator.FindKey(bs.ownFlavors[ns], acc); ok {
		own = bs.ownFlavors[ns][k]
	}
	var got *kueue.FlavorPlacement
	for _, c := range bs.cohortChunks(ns, acc) {
		switch {
		case len(own) > 0 && !slices.Contains(own, c.Flavor):
			continue // a peer's flavor, not one ns's queues offer
		case got == nil:
			got = &c.FlavorPlacement
		case !maps.Equal(got.NodeLabels, c.NodeLabels) || !slices.Equal(got.Tolerations, c.Tolerations):
			return nil
		}
	}
	return got
}

// QueueFlavorOf is queueFlavor for the scale-from-zero wake: per accelerator,
// the flavor Kueue admits ns's replicas on within its queues, which the wake's
// node fit applies as the steady placer does to a guaranteed replica (W5).
// Nil when no constraint reads ns's queues (neither counted nor carrying
// their nominal figures).
func QueueFlavorOf(constraints []*ResourceConstraints, ns string) func(acc string) *kueue.FlavorPlacement {
	read := slices.ContainsFunc(constraints, func(c *ResourceConstraints) bool {
		if c == nil {
			return false
		}
		_, counted := c.NamespaceBorrow[ns]
		_, nominal := c.NamespaceNominal[ns]
		return counted || nominal
	})
	if !read {
		return nil
	}
	bs := &borrowState{borrow: map[string]map[string]*nsBorrowLeft{ns: nil}, ownFlavors: ownFlavorsOf(constraints)}
	bs.chunks, bs.cohorts = mergeSharedPools(constraints)
	return func(acc string) *kueue.FlavorPlacement { return bs.queueFlavor(ns, acc) }
}

// ownFlavorsOf is, per namespace and type, the flavors its own queues offer
// the type on, from every constraint's NamespaceBorrow and NamespaceNominal,
// together.
func ownFlavorsOf(constraints []*ResourceConstraints) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	add := func(ns, t string, flavors []string) {
		if len(flavors) == 0 {
			return
		}
		if out[ns] == nil {
			out[ns] = map[string][]string{}
		}
		for _, f := range flavors {
			if !slices.Contains(out[ns][t], f) {
				out[ns][t] = append(out[ns][t], f)
			}
		}
	}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns, perType := range c.NamespaceBorrow {
			for t, b := range perType {
				add(ns, t, b.Flavors)
			}
		}
		for ns, perType := range c.NamespaceNominal {
			for t, f := range perType {
				add(ns, t, f.Flavors)
			}
		}
	}
	return out
}

// PlacedFlavors is, per "namespace/variant", the Kueue flavor the last pass
// placed the most of its replicas' GPUs on (guaranteed or borrowed), the
// first on a tie; nil when it placed none on a flavor. The in-flight ledger
// holds the variant's holes on that flavor's nodes; it keeps one flavor per
// variant, so a variant split across two flavors in one pass is held on the
// larger.
func (o *GreedyByScoreOptimizer) PlacedFlavors() map[string]*kueue.FlavorPlacement {
	if len(o.flavors) == 0 {
		return nil
	}
	gpus := map[string]map[*kueue.FlavorPlacement]int{}
	var order []flavorUse
	for _, f := range o.flavors {
		if gpus[f.variant] == nil {
			gpus[f.variant] = map[*kueue.FlavorPlacement]int{}
		}
		if _, seen := gpus[f.variant][f.placement]; !seen {
			order = append(order, f)
		}
		gpus[f.variant][f.placement] += f.gpus
	}
	out := map[string]*kueue.FlavorPlacement{}
	for _, f := range order {
		if best, ok := out[f.variant]; !ok || gpus[f.variant][f.placement] > gpus[f.variant][best] {
			out[f.variant] = f.placement
		}
	}
	return out
}

// setCredit takes the variants' own draws in flight as credit (A4).
func (bs *borrowState) setCredit(draws []BorrowDraw) {
	if bs == nil || len(draws) == 0 {
		return
	}
	bs.credit = map[string]map[string]int{}
	for _, d := range draws {
		k := d.Namespace + "/" + d.Variant
		if bs.credit[k] == nil {
			bs.credit[k] = map[string]int{}
		}
		bs.credit[k][d.Cohort+"/"+d.Flavor+"/"+d.Resource] += d.GPUs
	}
}

// setReclaimCredit takes the variants' own growth by Kueue's reclaim in
// flight as credit (NEW-1), per variant and the accelerator it was placed
// for.
func (bs *borrowState) setReclaimCredit(draws []BorrowDraw) {
	if bs == nil || len(draws) == 0 {
		return
	}
	bs.reclaimCredit = map[string]map[string]Reclaim{}
	for _, d := range draws {
		if !d.Reclaim || (d.GPUs <= 0 && d.After <= 0) {
			continue
		}
		k := d.Namespace + "/" + d.Variant
		if bs.reclaimCredit[k] == nil {
			bs.reclaimCredit[k] = map[string]Reclaim{}
		}
		bs.reclaimCredit[k][d.Accelerator] = bs.reclaimCredit[k][d.Accelerator].add(d.reclaimOf())
	}
}

// ownReclaim is what of variant v's own growth by reclaim in flight the pass
// has not re-placed yet, on accelerator acc, and the key it is kept under.
func (bs *borrowState) ownReclaim(v, acc string) (Reclaim, string) {
	if k, ok := accelerator.FindKey(bs.reclaimCredit[v], acc); ok {
		return bs.reclaimCredit[v][k], k
	}
	return Reclaim{}, ""
}

// cloneCredit copies a credit map.
func cloneCredit[V any](m map[string]map[string]V) map[string]map[string]V {
	if m == nil {
		return nil
	}
	out := make(map[string]map[string]V, len(m))
	for k, v := range m {
		out[k] = maps.Clone(v)
	}
	return out
}

func (bs *borrowState) save(guaranteed map[string]int) borrowSave {
	s := borrowSave{borrow: saveLeft(bs.borrow), lent: saveLeft(bs.lent), nodes: bs.nodes.saveDebits(), draws: len(bs.draws), guaranteed: maps.Clone(guaranteed), credit: cloneCredit(bs.credit), reclaimCr: cloneCredit(bs.reclaimCredit), flavors: len(bs.flavors), reclaims: len(bs.reclaims)}
	for _, c := range bs.chunks {
		s.free = append(s.free, c.Free)
		s.closed = append(s.closed, c.restrictedClosed)
		s.coFree = append(s.coFree, maps.Clone(c.CoFree))
	}
	return s
}

// restore puts the state back as save took it; the node view's debits and the
// owners they were charged to with it.
func (bs *borrowState) restore(s borrowSave, guaranteed map[string]int) {
	restoreLeft(bs.borrow, s.borrow)
	restoreLeft(bs.lent, s.lent)
	for i, c := range bs.chunks {
		c.Free = s.free[i]
		c.restrictedClosed = s.closed[i]
		c.CoFree = s.coFree[i]
	}
	bs.nodes.restoreDebits(s.nodes)
	bs.draws = bs.draws[:s.draws]
	bs.credit = s.credit
	bs.reclaimCredit = s.reclaimCr
	bs.flavors = bs.flavors[:s.flavors]
	bs.reclaims = bs.reclaims[:s.reclaims]
	clear(guaranteed)
	maps.Copy(guaranteed, s.guaranteed)
}

// modelPlacer places one model's replicas, one at a time, against its
// namespace's guaranteed budget first and the shared pools after (B4, B5).
type modelPlacer struct {
	bs        *borrowState
	namespace string
	modelID   string
	// guaranteed is this model's working copy of its namespace's guaranteed
	// budget; nil for an open namespace, where everything is guaranteed.
	guaranteed map[string]int
	// borrows is whether the namespace may borrow at all.
	borrows bool
	targets map[string]int
	// limited is the model's limit tracker: a replica the placer refuses is
	// marked there, as node fit's when the pools held its GPUs and no node
	// did, else as the pools'.
	limited gpuLimitTracker
	// cut is the variant of the last replica refused, cutByNodes whether
	// node fit refused it, and cutOutranked whether only LowerPriority
	// lending that does not outrank the model held the GPUs it needed.
	cut          string
	cutByNodes   bool
	cutOutranked bool
	// class is the priority class of the variant being placed
	// (domain.VariantReplicaState.PriorityClass).
	class string
	// straddled is set when the last unit refused would have straddled
	// guaranteed and borrowed capacity.
	straddled bool
}

// newModelPlacer returns the placer for one model, or nil when the pass counts
// no borrowing, which leaves allocation exactly as it was -- except, then, for
// a model whose namespace carries nominal co-resource headroom, whose
// replicas the placer fits to it (newNominalCoState).
func (bs *borrowState) newModelPlacer(namespace, modelID string, nsBudget map[string]int, targets map[string]int,
	limited gpuLimitTracker) *modelPlacer {
	if bs == nil || (!bs.counts && len(bs.lent[namespace]) == 0) {
		return nil
	}
	_, borrows := bs.borrow[namespace]
	return &modelPlacer{bs: bs, namespace: namespace, modelID: modelID, guaranteed: maps.Clone(nsBudget), borrows: borrows,
		targets: targets, limited: limited}
}

// placeUnit places one iteration's commit -- kByRole replicas of each picked
// role's variant -- as one unit: all of it, or a smaller unit at the same
// shape, never one role without its partner. It trims kByRole as
// fitPairToBudget does (the role holding the most GPUs gives one replica at a
// time, none below one) until the unit places, and reports false when not
// even one replica of each role places, in which case nothing is committed.
// Every trim marks the variant whose replica was refused: by node fit
// (NodeFitLimiterName) when a pool held its GPUs and no node held it, else
// by the pool. A unit refused only for straddling guaranteed and borrowed
// capacity first fills the guaranteed remainder with one role alone
// (fillGuaranteed), so the next pair borrows whole; when no role can, the
// refusal is the rule's (PairBorrowsWholeName), not scarcity.
func (p *modelPlacer) placeUnit(picked []string, kByRole map[string]int, variantByRole map[string]string,
	variants []variantRecord, stateMap map[string]domain.VariantReplicaState) bool {
	gpus := func(role string) int { return gpusPerReplicaFromState(stateMap, variantByRole[role]) }
	for {
		saved := p.bs.save(p.guaranteed)
		p.cut, p.cutByNodes, p.cutOutranked, p.straddled = "", false, false, false
		if p.tryUnit(picked, kByRole, variantByRole, variants, stateMap) {
			return true
		}
		p.bs.restore(saved, p.guaranteed)
		if p.straddled {
			// An odd guaranteed remainder (W2): fill it with one role's
			// replicas alone -- a unit of one role straddles nothing -- and
			// the next pair, all past guaranteed, borrows whole.
			if p.fillGuaranteed(picked, kByRole, variantByRole, variants, stateMap) {
				return true
			}
			// What is left is the rule's, not scarcity.
			p.limited.markPairWhole(p.cut)
			p.cut = ""
		}
		p.markCut()
		largest := ""
		for _, role := range picked {
			if kByRole[role] > 1 && (largest == "" || kByRole[role]*gpus(role) > kByRole[largest]*gpus(largest)) {
				largest = role
			}
		}
		if largest == "" {
			return false
		}
		kByRole[largest]--
	}
}

// markCut marks the last refused replica's variant in the model's tracker.
func (p *modelPlacer) markCut() {
	if p.cut == "" {
		return
	}
	switch {
	case p.cutOutranked:
		p.limited.markLenderCannotReclaim(p.cut)
	case p.cutByNodes:
		p.limited.markNodeFit(p.cut)
	default:
		p.limited.mark(p.cut)
	}
}

// tryUnit places every replica of the unit, one at a time, and reports
// whether all placed.
//
// A unit of several roles -- a P/D pair -- borrows only whole (B4, B9): either
// no replica of it draws on the shared pools, or every one does. A pair with
// its prefill on guaranteed capacity and its decode borrowed would, once
// Kueue reclaims the decode, leave a prefill serving nothing; with minReplicas
// 1 the placement timeout could not release the prefill either, and with
// minReplicas 0 releasing it would give back guaranteed capacity. Such a unit
// does not place; placeUnit then fills the guaranteed remainder with one role
// alone, or tries a smaller unit.
func (p *modelPlacer) tryUnit(picked []string, kByRole map[string]int, variantByRole map[string]string,
	variants []variantRecord, stateMap map[string]domain.VariantReplicaState) bool {
	var borrowed, guaranteed int
	for _, role := range picked {
		v := variantByRole[role]
		r := p.replica(variants, stateMap, v)
		g := gpusPerReplicaFromState(stateMap, v)
		requests := stateMap[v].ReplicaRequests
		p.class = stateMap[v].PriorityClass // K1: whose LowerPriority lending it may draw on
		for i := 0; i < kByRole[role]; i++ {
			placed, drew := p.placeReplica(v, r, g, requests, p.targets[v]+kByRole[role])
			if !placed {
				return false
			}
			if drew {
				borrowed++
			} else {
				guaranteed++
			}
		}
	}
	if len(picked) > 1 && borrowed > 0 && guaranteed > 0 {
		// A P/D unit straddling guaranteed and borrowed capacity: the
		// guaranteed figure ran out for part of it.
		p.straddled = true
		return p.refuse(variantByRole[picked[len(picked)-1]], false)
	}
	return true
}

// fillGuaranteed places, for the first role of picked that can, as many of
// its kByRole replicas as fit the guaranteed budget alone, at least one, and
// none on borrowed capacity; kByRole is then that role's count and 0 for the
// others. A role is filled only while the other roles have a replica that is
// not borrowed to pair with (guaranteedPartners). It reports false,
// committing nothing, when no role places one replica that way.
func (p *modelPlacer) fillGuaranteed(picked []string, kByRole map[string]int, variantByRole map[string]string,
	variants []variantRecord, stateMap map[string]domain.VariantReplicaState) bool {
	for _, role := range picked {
		v := variantByRole[role]
		if !p.guaranteedPartners(picked, role, variantByRole, variants, stateMap) {
			continue
		}
		r := p.replica(variants, stateMap, v)
		g := gpusPerReplicaFromState(stateMap, v)
		n := 0
		for n < kByRole[role] && p.guaranteed != nil && p.guaranteed[budgetKey(p.guaranteed, r.acc)] >= g {
			saved := p.bs.save(p.guaranteed)
			placed, drew := p.placeReplica(v, r, g, stateMap[v].ReplicaRequests, p.targets[v]+n+1)
			if !placed || drew {
				p.bs.restore(saved, p.guaranteed)
				break
			}
			n++
		}
		if n == 0 {
			continue
		}
		for _, other := range picked {
			kByRole[other] = 0
		}
		kByRole[role] = n
		return true
	}
	return false
}

// guaranteedPartners reports whether every role of picked other than role
// has a replica that is not borrowed -- its replicas so far less those its
// draws in flight hold (BorrowCredit), those this pass drew, and, the
// cautious side, all its namespace holds past its guaranteed figure of the
// type by the read (a borrowed replica already landed: the read does not say
// whose) -- to pair with role's replicas placed alone on guaranteed capacity
// (N-m2). Without one, the role filled alone pairs only with borrowed
// replicas: once Kueue reclaims them it serves nothing, a prefill with no
// decode.
func (p *modelPlacer) guaranteedPartners(picked []string, role string, variantByRole map[string]string,
	variants []variantRecord, stateMap map[string]domain.VariantReplicaState) bool {
	for _, other := range picked {
		if other == role {
			continue
		}
		v := variantByRole[other]
		borrowed := 0
		for _, gpus := range p.bs.credit[p.namespace+"/"+v] {
			borrowed += gpus
		}
		for _, d := range p.bs.draws {
			if d.Namespace == p.namespace && d.Variant == v {
				borrowed += d.GPUs
			}
		}
		if k, ok := accelerator.FindBudgetKey(p.bs.heldPast[p.namespace], accFromVCs(variants, v)); ok {
			borrowed += p.bs.heldPast[p.namespace][k]
		}
		if g := gpusPerReplicaFromState(stateMap, v); g > 0 {
			borrowed = ceilDiv(borrowed, g)
		}
		if p.targets[v]-borrowed < 1 {
			return false
		}
	}
	return true
}

// replica is one replica of variant v as the placer fits it: the view's
// shape (pods, tolerations, topology key, as node fit reads them) with the
// pod templates' own placement.
func (p *modelPlacer) replica(variants []variantRecord, stateMap map[string]domain.VariantReplicaState, v string) replicaFit {
	st := stateMap[v]
	return p.bs.nodes.fitOf(ReplicaShape{Variant: p.namespace + "/" + v, Accelerator: accFromVCs(variants, v),
		PodGPUs: st.PodGPUs, GPUs: gpusPerReplicaFromState(stateMap, v), Tolerations: st.Tolerations,
		DomainKey: st.TopologyKey, Placements: st.Placements})
}

// refuse records that variant's replica did not place, and why, and reports
// false. An empty variant records a refusal that marks nothing.
func (p *modelPlacer) refuse(variant string, byNodes bool) bool {
	p.cut, p.cutByNodes, p.cutOutranked = variant, byNodes, false
	return false
}

// placeReplica places one replica r of g GPUs. Within the guaranteed budget
// it must fit a node of the view, as any replica must when a physical limiter
// bounds the cycle (no node holding it is scarcity), with one exception:
// growth into nominal the namespace lent out is Kueue's to reclaim, by
// preempting the borrowers whose pods fill those holes now, so it places
// whether or not a node shows a hole, and debits the one it fits, if any.
// Growth into lent-out nominal also debits the pool, clamped. Past the
// guaranteed budget the borrowed part must fit the namespace's cap, one chunk
// of its cohorts, and -- with a node view -- a node of that chunk's flavor,
// whole replica at once. Every node must also satisfy the replica's pod
// templates, its tolerations and topology key, and a borrowed replica must
// fit the chunk's free pool of every co-resource its flavor's resource group
// covers (requests, per replica in milli-units), which it is debited. It
// reports whether the replica placed, and whether it drew on a shared pool. A
// replica refused for want of a node, when a chunk's pools held it, is
// recorded as node fit's (refuse).
func (p *modelPlacer) placeReplica(variant string, r replicaFit, g int, requests map[string]int64, target int) (placed, drew bool) {
	bs := p.bs
	acc := r.acc
	left := -1 // unbounded: an open namespace, or an unlimited allowance
	key := ""
	if p.guaranteed != nil {
		key = budgetKey(p.guaranteed, acc)
		left = p.guaranteed[key]
	}
	var borrow *nsBorrowLeft
	if p.borrows {
		if k, ok := accelerator.FindKey(bs.borrow[p.namespace], acc); ok {
			borrow = bs.borrow[p.namespace][k]
		}
	}
	if left < 0 || left >= g {
		// Growth into nominal a reclaiming queue lent out: the namespace's
		// own, counted or not (W1). A lender at zero whose nodes borrowers
		// fill must still order the replica Kueue preempts them for -- but
		// only a replica Kueue does preempt for whole: once nothing is left
		// of what it admits without preemption (after; a replica taking part
		// of it needs a hole for that part, so it is fitted whole, NEW-2),
		// and whole within what peers borrow (lentOut, N1). Unused nominal
		// nobody borrows frees no node.
		lent := borrow
		if lent == nil {
			if k, ok := accelerator.FindKey(bs.lent[p.namespace], acc); ok {
				lent = bs.lent[p.namespace][k]
			}
		}
		// The variant's own growth by reclaim still in flight is its to
		// re-place (NEW-1): its pods are not created, so the pass sizes it
		// again from its current replicas.
		own, ownKey := bs.ownReclaim(p.namespace+"/"+variant, acc)
		var share Reclaim
		if lent != nil {
			share = Reclaim{After: lent.after, Kept: lent.kept, GPUs: lent.lentOut}.add(own)
		}
		reclaim := share.exempt(g)
		if reclaim && bs.reclaimBlocked != nil && bs.reclaimBlocked(p.namespace, p.modelID) {
			// Backed off: a replica of the model ordered without a hole
			// never placed (the governor's safety net, M3). Until the
			// cooldown ends its growth needs a real hole.
			reclaim = false
		}
		if !bs.coGuaranteed(p.namespace, acc, borrow, requests, reclaim) { // K2c
			return p.refuse(variant, false), false
		}
		// Kueue applies its queue's flavor -- node labels, tolerations -- to
		// every pod it admits, within guaranteed too (W5).
		qf := bs.queueFlavor(p.namespace, acc)
		if qf != nil {
			r.flavor = qf
		}
		onNode := bs.nodes.place(r)
		if !onNode && !reclaim {
			return p.refuse(variant, true), false
		}
		if qf != nil && onNode {
			bs.flavors = append(bs.flavors, flavorUse{variant: p.namespace + "/" + variant, placement: qf, gpus: g})
		}
		if left >= 0 {
			p.guaranteed[key] = left - g
		}
		if lent != nil {
			// Kueue charges it to the cohort either way: from what the
			// queues keep first, which no pool holds (M-m1), then from the
			// free pool, then from what it preempts.
			took := share.take(g)
			fromOwn := took.atMost(own)
			fresh := took.sub(fromOwn)
			lent.after -= fresh.After
			lent.kept = max(0, lent.kept-fresh.Kept)
			lent.lentOut -= fresh.GPUs
			if fromOwn != (Reclaim{}) {
				bs.reclaimCredit[p.namespace+"/"+variant][ownKey] = own.sub(fromOwn)
			}
			// The pool part of its own entry came off the pool before the
			// pass (DeductUnreservedAt).
			bs.debitPool(p.namespace, acc, took.pool()-fromOwn.pool()+took.GPUs)
			if took.After > 0 || took.GPUs > 0 {
				// Held in the shared record, so the wake and the next cycle
				// see what of the pool and of LentOut it took (DeductReclaimed,
				// DeductUnreservedAt).
				d := bs.reclaimDraw(p.namespace, p.modelID, variant, acc, took, target)
				// What it re-placed of its own entry is in the record already.
				d.CreditGPUs, d.CreditAfter, d.CreditKept = fromOwn.GPUs, fromOwn.After, fromOwn.Kept
				if fromOwn == took {
					d.CreditReplicas = 1
				}
				d.NoHole = !onNode
				bs.reclaims = append(bs.reclaims, d)
			}
		}
		return true, false
	}
	if borrow == nil {
		return p.refuse(variant, false), false
	}
	if bs.blocked != nil && bs.blocked(p.namespace, p.modelID, variant) {
		// A borrowing rule holds it back (a placement hold, a cooldown, the
		// physical limiter skipped), not scarcity: its own reason says so,
		// and the decision is not marked limited.
		return p.refuse("", false), false
	}
	need := g - left
	if borrow.cap >= 0 && borrow.cap < need {
		return p.refuse(variant, false), false
	}
	byNodes, open := false, false
	// outranked: a chunk held the GPUs only as LowerPriority lending that
	// does not outrank the model; scarce: one did not hold them at all.
	outranked, scarce := false, false
	chunks := bs.cohortChunks(p.namespace, acc)
	for _, c := range chunks {
		if bs.blockedIn != nil && bs.blockedIn(p.namespace, p.modelID, c.Cohort) {
			continue // backed off in this cohort (B12)
		}
		open = true
		// The variant's own draw in flight on this chunk is its to re-place.
		own := bs.credit[p.namespace+"/"+variant][c.key()]
		// K1: a LowerPriority lender's GPUs only for a model it outranks.
		if c.drawable(p.class, borrow.priority)+own < need {
			if c.Free+own >= need {
				outranked = true
			} else {
				scarce = true
			}
			continue
		}
		if !coFitsKept(c.CoFree, borrow.coKept, requests) || !coWithin(borrow.coHeadroom, requests) { // K2b
			scarce = true
			continue // a co-resource of the flavor's group (B1) would hold it back
		}
		// place commits only when every pod fits, so one call both probes
		// and debits.
		flavor := r
		flavor.flavor = &c.FlavorPlacement
		if !bs.nodes.place(flavor) {
			byNodes = true // the pools held it; no node of the flavor did
			continue
		}
		fromOwn := min(own, need)
		if fromOwn > 0 {
			bs.credit[p.namespace+"/"+variant][c.key()] -= fromOwn
		}
		closes := c.drawable(p.class, borrow.priority) < c.Free
		if closes {
			// A model some LowerPriority lender does not outrank drew here:
			// its restricted part closes for every model this pass (K2-2),
			// and in later cycles while the draw is in flight (M1).
			c.restrictedClosed = true
		}
		c.Free -= need - fromOwn
		spendCo(borrow, c, requests)
		if borrow.cap >= 0 {
			borrow.cap -= need
		}
		p.guaranteed[key] = 0
		d := BorrowDraw{
			Namespace: p.namespace, ModelID: p.modelID, Variant: variant,
			Cohort: c.Cohort, Flavor: c.Flavor, Resource: c.Resource,
			GPUs: need, Replicas: 1, Target: target, Placement: &c.FlavorPlacement, ClosesRestricted: closes,
		}
		if fromOwn > 0 {
			d.CreditGPUs, d.CreditReplicas = fromOwn, 1
		}
		bs.draws = append(bs.draws, d)
		bs.flavors = append(bs.flavors, flavorUse{variant: p.namespace + "/" + variant, placement: &c.FlavorPlacement, gpus: g})
		return true, true
	}
	if len(chunks) > 0 && !open {
		// Backed off in every cohort it could draw on: a rule, not
		// scarcity, as for a blocked variant above.
		return p.refuse("", false), false
	}
	if outranked && !scarce && !byNodes {
		// The GPUs are there, lent only by LowerPriority lenders that do not
		// outrank the model: Kueue would never reclaim them for their lender.
		// A rule, not scarcity (lender-cannot-reclaim).
		p.refuse(variant, false)
		p.cutOutranked = true
		return false, false
	}
	return p.refuse(variant, byNodes), false
}

// drawsByVariant folds the pass's draws into one per variant and chunk.
func (bs *borrowState) drawsByVariant() []BorrowDraw {
	if bs == nil || len(bs.draws) == 0 {
		return nil
	}
	type key struct{ ns, variant, cohort, flavor, resource string }
	idx := map[key]int{}
	var out []BorrowDraw
	for _, d := range bs.draws {
		k := key{d.Namespace, d.Variant, d.Cohort, d.Flavor, d.Resource}
		i, ok := idx[k]
		if !ok {
			idx[k] = len(out)
			out = append(out, d)
			continue
		}
		out[i].GPUs += d.GPUs
		out[i].Replicas += d.Replicas
		out[i].CreditGPUs += d.CreditGPUs
		out[i].CreditReplicas += d.CreditReplicas
		out[i].Target = max(out[i].Target, d.Target)
		out[i].ClosesRestricted = out[i].ClosesRestricted || d.ClosesRestricted
	}
	return out
}
