package allocation

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
)

// BorrowInFlight is borrowed growth the greedy optimizer ordered that Kueue
// has not reserved yet, kept by the steady-state engine across cycles
// (the Kueue borrowing proposal, PR ev-shindin/llm-scaling-manager#161, B4). The Kueue
// reader cannot see it: until the pods exist and are admitted, the cohort's
// free pool still counts those GPUs, and a second cycle -- or another
// namespace -- would borrow them again.
//
// An entry is in flight until the variant's pods exist (its replica count
// reaches the target it was ordered at), its target goes back below that
// target -- the engine's own decision (Trim), or a desired count read back
// (Observe) -- or InFlightTTL passes since it was ordered. Pods that exist are
// still not in a Kueue read taken before they did: a resolved draw lags, held
// against its chunk until the chunk's snapshot was read after it resolved
// (W3), and so is a pod seen at Kueue's admission gate and admitted since
// (SetCycle's admitted). The release steps of the proposal (B5, B6) extend
// this record; it is keyed by namespace, variant and chunk so a release can
// find what a variant borrowed and where.
//
// One record serves both engines (DefaultBorrowInFlight): the steady-state
// engine records its draws and, each cycle, the gated and admitted pods and
// the cohorts closed to new borrowing; a wake deducts them all (A1).
//
// It is safe for concurrent use. Every method accepts a nil receiver, which
// records nothing and holds nothing.
type BorrowInFlight struct {
	mu      sync.Mutex
	entries map[inFlightKey]BorrowDraw
	// lagging is resolved draws, held until a read after they resolved.
	lagging []BorrowDraw
	// The last borrowing cycle's figures (SetCycle), as of cycleAt.
	gated    map[string]map[string]int
	admitted []AdmittedGPUs
	blocked  map[string]bool
	cycleAt  time.Time
	// wakeReclaims is the wakes' reclaim entries (RecordWake) no steady-state
	// pass has yielded for yet (PendingWakeReclaims, YieldedWakeReclaims).
	wakeReclaims []BorrowDraw
}

// DefaultBorrowInFlight is the process-wide record both engines share.
var DefaultBorrowInFlight = NewBorrowInFlight()

// AdmittedGPUs is GPUs of pods seen at Kueue's admission gate and admitted
// since: Kueue reserves them, but a snapshot read before At does not show it.
type AdmittedGPUs struct {
	Namespace string
	// Accelerator is the type the pods' node selector names, "" when none.
	Accelerator string
	GPUs        int
	// At is when the admission was first seen.
	At time.Time
}

// Unreserved is what the scaling manager asked for and a Kueue read may not
// show yet (DeductUnreservedAt).
type Unreserved struct {
	// InFlight is the draws in flight and the lagging ones (ResolvedAt set).
	InFlight []BorrowDraw
	// Gated is GPUs of managed pods at Kueue's admission gate, per namespace
	// and accelerator ("" when the pod names none).
	Gated map[string]map[string]int
	// Admitted is gated pods since admitted.
	Admitted []AdmittedGPUs
}

type inFlightKey struct {
	namespace, variant, cohort, flavor, resource string
	// reclaim keeps a variant's growth by Kueue's reclaim apart from its draw
	// on the same chunk.
	reclaim bool
}

func keyOfDraw(d BorrowDraw) inFlightKey {
	return inFlightKey{d.Namespace, d.Variant, d.Cohort, d.Flavor, d.Resource, d.Reclaim}
}

// VariantObservation is one variant's replica counts as a cycle sees them.
type VariantObservation struct {
	// Current is the replicas that exist (pods created, gated ones included).
	Current int
	// Desired is the target the variant is held at now, as read back; 0 when
	// unknown -- a desired count of 0 the engine decided reaches the record
	// through Trim instead.
	Desired int
}

// NewBorrowInFlight returns an empty record.
func NewBorrowInFlight() *BorrowInFlight {
	return &BorrowInFlight{entries: map[inFlightKey]BorrowDraw{}}
}

// Reset empties the record. For tests.
func (b *BorrowInFlight) Reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(b.entries)
	b.lagging, b.gated, b.admitted, b.blocked, b.cycleAt = nil, nil, nil, nil, time.Time{}
	b.wakeReclaims = nil
}

// Record adds a pass's draws, ordered now. A variant that borrows again from
// the same chunk before its earlier draw resolved holds both, at the newer
// target, from the earlier order: the borrowing placer re-places a draw still
// in flight on its own GPUs (BorrowCredit) and records only what is new.
func (b *BorrowInFlight) Record(draws []BorrowDraw) { b.RecordAt(draws, time.Now()) }

// RecordAt is Record at a given time.
func (b *BorrowInFlight) RecordAt(draws []BorrowDraw, now time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, d := range draws {
		k := keyOfDraw(d)
		d.OrderedAt, d.ResolvedAt = now, time.Time{}
		// What was re-placed on the variant's own draw is already here.
		d.GPUs, d.Replicas = d.GPUs-d.CreditGPUs, d.Replicas-d.CreditReplicas
		d.After, d.Kept = d.After-d.CreditAfter, max(0, d.Kept-d.CreditKept)
		d.CreditGPUs, d.CreditReplicas, d.CreditAfter, d.CreditKept = 0, 0, 0, 0
		prev, ok := b.entries[k]
		if ok {
			d.GPUs += prev.GPUs
			d.After += prev.After
			d.Kept += prev.Kept
			d.Replicas += prev.Replicas
			d.Target = max(d.Target, prev.Target)
			d.OrderedAt = prev.OrderedAt
			d.ClosesRestricted = d.ClosesRestricted || prev.ClosesRestricted
			d.NoHole = d.NoHole || prev.NoHole
		}
		if (d.GPUs <= 0 && d.After <= 0) || d.Replicas <= 0 {
			continue
		}
		b.entries[k] = d
	}
}

// Observe settles the record against a cycle's replica counts, now.
func (b *BorrowInFlight) Observe(observed map[string]VariantObservation) {
	b.ObserveAt(observed, time.Now())
}

// ObserveAt settles the record against a cycle's replica counts, observed
// keyed by "namespace/variant". A draw whose variant is no longer observed,
// whose desired count went back below its target, or that was ordered
// InFlightTTL ago is dropped (its pods were never created, or the target
// that ordered them is gone). The replicas whose pods now exist resolve: they
// leave the draw and lag (W3), until a read after now shows them, or
// InFlightTTL passes.
func (b *BorrowInFlight) ObserveAt(observed map[string]VariantObservation, now time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lagging = slices.DeleteFunc(b.lagging, func(d BorrowDraw) bool {
		_, ok := observed[d.Namespace+"/"+d.Variant]
		return !ok || now.Sub(d.ResolvedAt) > InFlightTTL
	})
	for k, d := range b.entries {
		o, ok := observed[k.namespace+"/"+k.variant]
		if now.Sub(d.OrderedAt) > InFlightTTL {
			delete(b.entries, k)
			continue
		}
		if d.Wake {
			// A wake's variant may not be observed yet, and its desired
			// count is the wake's own: only its pods existing end it.
			if !ok {
				continue
			}
		} else if !ok || (o.Desired > 0 && o.Desired < d.Target) {
			delete(b.entries, k)
			continue
		}
		missing := max(0, d.Target-o.Current)
		if missing >= d.Replicas {
			continue
		}
		// Replicas that now exist are no longer in flight; they lag.
		still := d.replicasOf(missing)
		resolved := d
		resolved.Replicas = d.Replicas - missing
		resolved.GPUs, resolved.After = d.GPUs-still.GPUs, d.After-still.After
		resolved.Kept = min(resolved.After, max(0, d.Kept-still.Kept))
		resolved.ResolvedAt = now
		if resolved.GPUs > 0 || resolved.After > 0 {
			b.lagging = append(b.lagging, resolved)
		}
		if missing == 0 {
			delete(b.entries, k)
			continue
		}
		b.entries[k] = still
	}
}

// replicasOf is d cut to n of its replicas, each figure shared per replica,
// rounded up, its credits clamped to what is left.
func (d BorrowDraw) replicasOf(n int) BorrowDraw {
	part := func(v int) int { return min(v, n*ceilDiv(v, d.Replicas)) }
	out := d
	out.Replicas = n
	out.GPUs, out.After = part(d.GPUs), part(d.After)
	out.Kept = min(out.After, part(d.Kept))
	out.CreditReplicas = min(d.CreditReplicas, n)
	out.CreditGPUs, out.CreditAfter, out.CreditKept = min(d.CreditGPUs, out.GPUs), min(d.CreditAfter, out.After), min(d.CreditKept, out.Kept)
	return out
}

// TrimmedTo is d cut to target: the replicas it ordered above target are not
// in flight, each figure and credit shared per replica (replicasOf). Reports
// false when nothing is left.
func (d BorrowDraw) TrimmedTo(target int) (BorrowDraw, bool) {
	cut := d.Target - target
	if cut <= 0 {
		return d, true
	}
	if cut >= d.Replicas {
		return d, false
	}
	d = d.replicasOf(d.Replicas - cut)
	d.Target = target
	return d, true
}

// Trim lowers the draws to the targets the engine decided this cycle, keyed
// "namespace/variant": a draw ordered past a variant's target is cut by the
// replicas above it, and dropped when none is left. A decided target is known
// even at 0, unlike a desired count read back (W6). A variant without a
// target here is left as it is.
func (b *BorrowInFlight) Trim(targets map[string]int) {
	if b == nil || len(targets) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, d := range b.entries {
		t, ok := targets[k.namespace+"/"+k.variant]
		if !ok || d.Wake {
			continue // a wake's target is its own, not this engine's
		}
		if d, keep := d.TrimmedTo(t); keep {
			b.entries[k] = d
		} else {
			delete(b.entries, k)
		}
	}
}

// Entries returns the draws still in flight -- ordered, their pods not
// created -- in a stable order. The lagging ones are not among them, nor is
// growth by Kueue's reclaim (BorrowDraw.Reclaim), which draws on no pool.
func (b *BorrowInFlight) Entries() []BorrowDraw {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.DeleteFunc(b.entriesLocked(), func(d BorrowDraw) bool { return d.Reclaim })
}

// Reclaims returns the growth by Kueue's reclaim still in flight
// (BorrowDraw.Reclaim) -- ordered, its pods not created -- in a stable order,
// a wake's among them: each its variant's to re-place, as Entries' draws are
// (GreedyByScoreOptimizer.ReclaimCredit, NEW-1). The lagging ones are not
// among them: their pods exist, and no pass re-places them.
func (b *BorrowInFlight) Reclaims() []BorrowDraw {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.DeleteFunc(b.entriesLocked(), func(d BorrowDraw) bool { return !d.Reclaim })
}

func (b *BorrowInFlight) entriesLocked() []BorrowDraw {
	keys := slices.SortedFunc(maps.Keys(b.entries), compareInFlightKeys)
	out := make([]BorrowDraw, 0, len(keys))
	for _, k := range keys {
		out = append(out, b.entries[k])
	}
	return out
}

// SetCycle records a cycle's gated and admitted pods and the cohorts closed
// to new borrowing, for the wake (Unreserved, Blocked) and the quota charge
// (GatedGPUs). A cycle that counts no borrowing records the gated and
// admitted pods of its Kueue-backed namespaces alone, or nothing.
func (b *BorrowInFlight) SetCycle(gated map[string]map[string]int, admitted []AdmittedGPUs, blocked map[string]bool, now time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gated, b.admitted, b.blocked, b.cycleAt = gated, slices.Clone(admitted), maps.Clone(blocked), now
}

// Unreserved is everything the record holds that a Kueue read may not show
// yet: the draws in flight and lagging, and, when the last borrowing cycle is
// younger than InFlightTTL, its gated and admitted pods.
//
// A draw ordered, or a lagging one resolved, InFlightTTL ago or more is left
// out even while no ObserveAt has dropped it: only the steady-state engine
// settles the record, and a cycle that observes nothing -- an idle fleet, a
// woken model not in its requests yet -- left a wake's draw counted against
// every later wake and borrow for as long as it ran so.
func (b *BorrowInFlight) Unreserved(now time.Time) Unreserved {
	if b == nil {
		return Unreserved{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	live := slices.DeleteFunc(b.entriesLocked(), func(d BorrowDraw) bool { return now.Sub(d.OrderedAt) > InFlightTTL })
	for _, d := range b.lagging {
		if now.Sub(d.ResolvedAt) <= InFlightTTL {
			live = append(live, d)
		}
	}
	u := Unreserved{InFlight: live}
	if now.Sub(b.cycleAt) <= InFlightTTL {
		u.Gated, u.Admitted = b.gated, slices.Clone(b.admitted)
	}
	return u
}

// GatedGPUs is the GPUs of managed pods the last cycle read at Kueue's
// admission gate, per namespace and accelerator ("" when the pod names none),
// in every Kueue-backed namespace whether or not it counts borrowing, when
// that cycle is younger than InFlightTTL; nil otherwise. It is the
// GatedSource a Kueue-backed quota charges its figures with (KU1).
func (b *BorrowInFlight) GatedGPUs(now time.Time) map[string]map[string]int {
	gated := b.Unreserved(now).Gated
	if gated == nil {
		return nil
	}
	out := make(map[string]map[string]int, len(gated))
	for ns, perAcc := range gated {
		out[ns] = maps.Clone(perAcc)
	}
	return out
}

// Blocked is the cohorts the last borrowing cycle closed to new borrowing,
// when it is younger than InFlightTTL; nil otherwise.
func (b *BorrowInFlight) Blocked(now time.Time) map[string]bool {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Sub(b.cycleAt) > InFlightTTL {
		return nil
	}
	return maps.Clone(b.blocked)
}

// compareInFlightKeys orders keys by namespace, variant, cohort, flavor and
// resource, a draw before a reclaim. Keys are unique in the record, so the
// order is total.
func compareInFlightKeys(a, b inFlightKey) int {
	return cmp.Or(
		cmp.Compare(a.namespace, b.namespace),
		cmp.Compare(a.variant, b.variant),
		cmp.Compare(a.cohort, b.cohort),
		cmp.Compare(a.flavor, b.flavor),
		cmp.Compare(a.resource, b.resource),
		cmp.Compare(boolOrder(a.reclaim), boolOrder(b.reclaim)),
	)
}

// boolOrder orders false before true.
func boolOrder(b bool) int {
	if b {
		return 1
	}
	return 0
}

// DeductUnreserved returns copies of constraints whose shared pools no longer
// count what the scaling manager asked for and Kueue has not reserved: the
// draws in flight, from the chunk each was drawn from, and gated -- GPUs of
// managed pods behind Kueue's admission gate, per namespace and accelerator
// ("" when the pod names none) -- from the chunks of the namespace's cohorts
// that serve the accelerator, first chunk first. Every figure clamps at 0.
// Constraints without borrowing are returned as they are.
//
// Gated demand is read twice: from the governed namespaces' gated pods
// (gated), and from the pending Workloads of every namespace in the cohort's
// ClusterQueues (SharedPoolChunk.PendingGPUs, B8), which a gated pod's own
// Workload is among. Per chunk the larger of the two comes off, so a governed
// pod is not counted twice, and a peer's ask -- another controller's, or a
// batch job's -- is not borrowed from under it.
func DeductUnreserved(constraints []*ResourceConstraints, inFlight []BorrowDraw, gated map[string]map[string]int) []*ResourceConstraints {
	return DeductUnreservedAt(constraints, Unreserved{InFlight: inFlight, Gated: gated})
}

// DeductUnreservedAt is DeductUnreserved over everything u holds. A lagging
// draw (ResolvedAt set) and an admitted pod come off a chunk only while the
// chunk's snapshot was read no later than they resolved -- or its read time
// is unknown -- since a later read already counts them as reserved (W3). The
// admitted pods are charged beside the gated ones, before the larger of those
// and the pending demand is taken: a pod admitted after the read may still be
// among its pending Workloads, the cautious side.
func DeductUnreservedAt(constraints []*ResourceConstraints, u Unreserved) []*ResourceConstraints {
	if !HasBorrowing(constraints) || (len(u.InFlight) == 0 && len(u.Gated) == 0 && len(u.Admitted) == 0 && !anyPending(constraints)) {
		return constraints
	}
	// Growth within a reclaiming queue's nominal draws on the pool only for
	// the part of its PreemptAfter the pool covers -- not what the queues
	// keep, not what Kueue reclaims from borrowers (DeductReclaimed) -- and
	// that part is held like a draw on its chunk, so a wake or a queue that
	// cannot reclaim does not reach the same GPUs through the pool meanwhile.
	draws := make([]BorrowDraw, 0, len(u.InFlight))
	for _, d := range u.InFlight {
		if d.Reclaim {
			pool := d.reclaimOf().pool()
			if pool <= 0 {
				continue
			}
			d.GPUs, d.Reclaim, d.ClosesRestricted = pool, false, false
		}
		draws = append(draws, d)
	}
	inFlight := lagCoveredByGated(draws, u.Gated, u.Admitted)
	out := make([]*ResourceConstraints, 0, len(constraints))
	for _, c := range constraints {
		if c == nil || c.NamespaceBorrow == nil || len(c.SharedPools) == 0 {
			out = append(out, c)
			continue
		}
		cc := *c
		cc.SharedPools = slices.Clone(c.SharedPools)
		for _, d := range inFlight {
			for i := range cc.SharedPools {
				p := &cc.SharedPools[i]
				if p.Cohort == d.Cohort && p.Flavor == d.Flavor && p.Resource == d.Resource && !readAfter(p, d.ResolvedAt) {
					p.Free = max(0, p.Free-d.GPUs)
					// A draw by a model some LowerPriority lender does not
					// outrank keeps the restricted part closed until a
					// read shows it borrowed (SharedPoolChunk.Borrowed, M1).
					p.restrictedClosed = p.restrictedClosed || d.ClosesRestricted
				}
			}
		}
		// What the governed gated pods take of each chunk, first chunk first;
		// then what the admitted ones take of a chunk read before them.
		fromPods := make([]int, len(cc.SharedPools))
		left := make([]int, len(cc.SharedPools))
		for i := range cc.SharedPools {
			left[i] = cc.SharedPools[i].Free
		}
		charge := func(ns, acc string, n int, at time.Time) {
			for i := range cc.SharedPools {
				p := &cc.SharedPools[i]
				if n <= 0 {
					return
				}
				if !slices.Contains(c.NamespaceCohorts[ns], p.Cohort) || (acc != "" && !p.serves(acc)) || readAfter(p, at) {
					continue
				}
				take := min(left[i], n)
				left[i] -= take
				fromPods[i] += take
				n -= take
			}
		}
		for _, ns := range slices.Sorted(maps.Keys(u.Gated)) {
			byAcc := u.Gated[ns]
			for _, acc := range slices.Sorted(maps.Keys(byAcc)) {
				charge(ns, acc, byAcc[acc], time.Time{})
			}
		}
		for _, a := range u.Admitted {
			charge(a.Namespace, a.Accelerator, a.GPUs, a.At)
		}
		for i := range cc.SharedPools {
			p := &cc.SharedPools[i]
			p.Free = max(0, p.Free-max(fromPods[i], p.PendingGPUs))
		}
		out = append(out, &cc)
	}
	return out
}

// lagCoveredByGated is draws with each lagging one's GPUs less what its
// namespace's gated and admitted pods of the same accelerator already charge:
// a draw resolves into pods at Kueue's gate, or admitted past it, which those
// deductions count, and must not be counted twice. Only what is left -- pods
// never seen at the gate, admitted before a read shows them -- lags. A gated
// pod of another accelerator -- another variant's -- covers nothing (N-m3).
// A pod whose node selector names none (""), or a draw whose flavor names
// none, may be either's, and covers as before. In flight draws are as they
// are.
func lagCoveredByGated(draws []BorrowDraw, gated map[string]map[string]int, admitted []AdmittedGPUs) []BorrowDraw {
	if (len(gated) == 0 && len(admitted) == 0) || !slices.ContainsFunc(draws, func(d BorrowDraw) bool { return !d.ResolvedAt.IsZero() }) {
		return draws
	}
	type cover struct {
		ns, acc string
		gpus    int
	}
	var covers []*cover
	for _, ns := range slices.Sorted(maps.Keys(gated)) {
		for _, acc := range slices.Sorted(maps.Keys(gated[ns])) {
			covers = append(covers, &cover{ns, acc, gated[ns][acc]})
		}
	}
	for _, a := range admitted {
		covers = append(covers, &cover{a.Namespace, a.Accelerator, a.GPUs})
	}
	sameAcc := func(a, b string) bool {
		return a == "" || b == "" || accelerator.SameName(a, b)
	}
	out := slices.Clone(draws)
	for i := range out {
		d := &out[i]
		if d.ResolvedAt.IsZero() {
			continue
		}
		acc := drawAccelerator(*d)
		for _, c := range covers {
			if d.GPUs <= 0 {
				break
			}
			if c.ns != d.Namespace || c.gpus <= 0 || !sameAcc(c.acc, acc) {
				continue
			}
			n := min(c.gpus, d.GPUs)
			d.GPUs -= n
			c.gpus -= n
		}
	}
	return out
}

// drawAccelerator is the accelerator a draw's pods run on: the one it was
// placed for, else its flavor's; "" when neither names one.
func drawAccelerator(d BorrowDraw) string {
	if d.Accelerator != "" {
		return d.Accelerator
	}
	if d.Placement != nil {
		return d.Placement.AcceleratorType
	}
	return ""
}

// DeductReclaimed returns copies of constraints whose namespaces' LentOut
// (NamespaceBorrow and NamespaceNominal) no longer count what the scaling
// manager already grew into by Kueue's reclaim and a read may not show yet:
// the Reclaim entries of inFlight, in flight, or lagging until a read of
// their chunk after they resolved (W3). Without one, the GPUs peers borrow
// stay reclaimable to the next cycle and the next wake, which would order
// replicas for the same borrowed GPUs again (N1). PreemptAfter and PreemptKept
// lose the entries' After and Kept the same way: growth Kueue admitted before
// preempting is not there to admit again. Every figure clamps at 0, Kept at
// most After.
func DeductReclaimed(constraints []*ResourceConstraints, inFlight []BorrowDraw) []*ResourceConstraints {
	var reclaims []BorrowDraw
	for _, d := range inFlight {
		if d.Reclaim && (d.GPUs > 0 || d.After > 0) {
			reclaims = append(reclaims, d)
		}
	}
	if len(reclaims) == 0 {
		return constraints
	}
	out := make([]*ResourceConstraints, 0, len(constraints))
	for _, c := range constraints {
		if c == nil || (len(c.NamespaceBorrow) == 0 && len(c.NamespaceNominal) == 0) {
			out = append(out, c)
			continue
		}
		cc := *c
		cc.NamespaceBorrow = maps.Clone(c.NamespaceBorrow)
		cc.NamespaceNominal = maps.Clone(c.NamespaceNominal)
		for _, d := range reclaims {
			if !d.ResolvedAt.IsZero() && slices.ContainsFunc(c.SharedPools, func(p SharedPoolChunk) bool {
				return p.Cohort == d.Cohort && p.Flavor == d.Flavor && p.Resource == d.Resource && readAfter(&p, d.ResolvedAt)
			}) {
				continue // a read after it landed counts it
			}
			if k, ok := accelerator.FindKey(cc.NamespaceBorrow[d.Namespace], d.Accelerator); ok {
				perType := maps.Clone(cc.NamespaceBorrow[d.Namespace])
				b := perType[k]
				left := Reclaim{After: b.PreemptAfter, Kept: b.PreemptKept, GPUs: b.LentOut}.sub(d.reclaimOf())
				b.LentOut, b.PreemptAfter, b.PreemptKept = left.GPUs, left.After, left.Kept
				perType[k] = b
				cc.NamespaceBorrow[d.Namespace] = perType
			}
			if k, ok := accelerator.FindKey(cc.NamespaceNominal[d.Namespace], d.Accelerator); ok {
				perType := maps.Clone(cc.NamespaceNominal[d.Namespace])
				f := perType[k]
				left := Reclaim{After: f.PreemptAfter, Kept: f.PreemptKept, GPUs: f.LentOut}.sub(d.reclaimOf())
				f.LentOut, f.PreemptAfter, f.PreemptKept = left.GPUs, left.After, left.Kept
				perType[k] = f
				cc.NamespaceNominal[d.Namespace] = perType
			}
		}
		out = append(out, &cc)
	}
	return out
}

// readAfter reports whether chunk p's snapshot was read after at: it then
// counts what resolved at at. A zero at (still in flight, still gated) or an
// unknown read time is never read after.
func readAfter(p *SharedPoolChunk, at time.Time) bool {
	return !at.IsZero() && !p.ReadAt.IsZero() && p.ReadAt.After(at)
}

// anyPending reports whether some borrowing constraint's chunk carries pending
// demand.
func anyPending(constraints []*ResourceConstraints) bool {
	for _, c := range constraints {
		if c == nil || c.NamespaceBorrow == nil {
			continue
		}
		for _, p := range c.SharedPools {
			if p.PendingGPUs > 0 {
				return true
			}
		}
	}
	return false
}

// BlockCohorts returns copies of constraints whose shared pools in the given
// cohorts read 0 free: nothing new is borrowed there this cycle (a lender is
// waiting, or waited within the cooldown). Without such a cohort, the input.
func BlockCohorts(constraints []*ResourceConstraints, cohorts map[string]bool) []*ResourceConstraints {
	if len(cohorts) == 0 || !HasBorrowing(constraints) {
		return constraints
	}
	out := make([]*ResourceConstraints, 0, len(constraints))
	for _, c := range constraints {
		if c == nil || len(c.SharedPools) == 0 {
			out = append(out, c)
			continue
		}
		cc := *c
		cc.SharedPools = slices.Clone(c.SharedPools)
		for i := range cc.SharedPools {
			if cohorts[cc.SharedPools[i].Cohort] {
				cc.SharedPools[i].Free = 0
			}
		}
		out = append(out, &cc)
	}
	return out
}

func ceilDiv(a, b int) int {
	if b <= 0 {
		return a
	}
	return (a + b - 1) / b
}
