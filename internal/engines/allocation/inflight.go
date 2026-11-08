package allocation

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// InFlightTTL is how long a replica admitted onto a node hole holds that hole
// while its pods hold no node. It is about how long placement takes: a pod
// Pending longer than this is not waiting for the hole it was admitted to --
// it is unschedulable for another reason (a taint, an affinity, CPU or memory
// on the node), or the hole went to someone else -- and holding the hole
// longer would only keep it from a replica that could use it. A replica on a
// Kueue flavor is held at least as long as the borrowing governor waits for
// it to place (InFlightAdmission.TTL, the placement timeout).
const InFlightTTL = 5 * time.Minute

// InFlightOwner is the owner the in-flight ledger's holes are charged to on a
// node view (NodeView.DebitsExcept).
const InFlightOwner = "in-flight"

// InFlightLedger remembers the replicas a decision admitted onto node holes
// whose pods hold no node yet, so later decisions do not admit another replica
// into the same hole. The node snapshot counts a pod on its node once it is
// scheduled, or nominated for the node after a preemption; before that, a
// Pending pod holds nothing there, and a wake or a scale-up in that window
// -- scheduling takes seconds, an image pull or a preemption longer -- saw the
// hole free again. Two cold models woke into one free 8-GPU node, one tick
// apart.
//
// One ledger serves both engines (DefaultInFlight): the steady-state engine
// admits the growth it orders, the scale-from-zero engine the wakes it admits,
// and every node view either builds holds what the ledger holds
// (NodeViewFor). Mirrors the utilization share's promised GPUs, which hold a
// freed node for a receiver until its pods land.
//
// An entry is per variant ("namespace/name"): the number of replicas the
// variant is to have holding a node (want), and how to count those it has
// (placed, from the variant's pods: scheduled, or nominated). What it holds is
// want less placed, until placed reaches want or InFlightTTL passes since the
// growth was admitted. A count that rises is applied to a node view only
// once the view's snapshot is at least as recent as when the rise was seen:
// before, the snapshot does not count the pod on its node either.
//
// The steady-state engine persists the live entries in the controller
// status (MarshalInFlight) and restores them after a restart
// (RestoreInFlight), so a hold outlives the process that started it.
//
// Safe for concurrent use.
type InFlightLedger struct {
	mu      sync.Mutex
	entries map[string]*inFlightEntry
	// sighted is, per variant key a committed admission named in this
	// process (InFlightAdmission.ExistingOnly), when it last did. Sightings
	// are not persisted (RestoreInFlight restores entries only), so after a
	// restart no variant is sighted yet. A key
	// not named for sightedFor is dropped (Commit): a deleted variant does
	// not stay, and every cycle names the variants it decides.
	sighted map[string]time.Time
	// restored is the entries RestoreInFlight restored that no committed
	// admission has updated since (DropRestoredExcept).
	restored map[string]bool
	// restorePending is set while the persisted ledger is still to be read
	// back (ExpectRestore, MarkRestored): a wake decided meanwhile would not
	// see the holds it carries. restoreExpected is when ExpectRestore was
	// called.
	restorePending  bool
	restoreExpected time.Time
}

// ScaleTargetRef names a variant's scale target, as the ledger persists it:
// a restored entry counts the target's pods from the moment it is restored
// (RestoreInFlightWith).
type ScaleTargetRef struct {
	// Kind is constants.DeploymentKind or constants.LeaderWorkerSetKind.
	Kind string
	// Name is the scale target's name, in the variant's namespace.
	Name string
}

// ScaleTargetRefOf names acc for the ledger (InFlightAdmission.Target); zero
// for a nil acc.
func ScaleTargetRefOf(acc scaletarget.ScaleTargetAccessor) ScaleTargetRef {
	if acc == nil {
		return ScaleTargetRef{}
	}
	kind := constants.DeploymentKind
	if scaletarget.IsLeaderWorkerSet(acc) {
		kind = constants.LeaderWorkerSetKind
	}
	return ScaleTargetRef{Kind: kind, Name: acc.GetName()}
}

// sightedFor is how long a variant stays sighted with no committed admission
// naming it. Past it, the next admission is a first sighting again, and may
// start a hold bounded by its TTL, as after a restart.
const sightedFor = 2 * InFlightTTL

// inFlightEntry is one variant's replicas in flight.
type inFlightEntry struct {
	shape ReplicaShape
	want  int
	// base is how many replicas held a node when the entry was admitted,
	// assumed already in any snapshot; seen is each higher count since, with
	// when it was first observed.
	base int
	seen []placedAt
	// expires is when the entry stops holding anything; it is kept, holding
	// nothing, until the variant reaches want or wants more, so an
	// unschedulable replica is not admitted again every cycle.
	expires time.Time
	// ttl is how long a rise of want holds (InFlightAdmission.TTL).
	ttl time.Duration
	// placed counts the variant's replicas holding a node, ok false when it
	// cannot tell.
	placed func() (Placed, bool)
	// target is the variant's scale target, persisted so a restored entry
	// can count its pods; zero when no admission named one.
	target ScaleTargetRef
	// partial is the replicas part of whose pods hold a node, as last
	// counted, each placed pod with when it was first seen.
	partial []partialSeen
}

// partialSeen is one partial replica as last counted, and per placed pod
// (PlacedGPUs) when it was first seen placed.
type partialSeen struct {
	domain.PartialReplica
	at []time.Time
}

// mergePartial is cur, the partial replicas counted now, each placed pod
// keeping when it was first seen in old -- matched by node and GPUs -- and a
// pod not in old seen now. A replica one more of whose pods placed thus has
// only that pod newer than an earlier snapshot.
func mergePartial(old []partialSeen, cur []domain.PartialReplica, now time.Time) []partialSeen {
	if len(cur) == 0 {
		return nil
	}
	used := make([]bool, len(old))
	out := make([]partialSeen, 0, len(cur))
	for _, c := range cur {
		s := partialSeen{PartialReplica: c, at: make([]time.Time, len(c.PlacedGPUs))}
		var gpus []int
		var at []time.Time
		for i := range old {
			if !used[i] && old[i].Node == c.Node {
				used[i] = true
				gpus, at = slices.Clone(old[i].PlacedGPUs), slices.Clone(old[i].at)
				break
			}
		}
		for j, g := range c.PlacedGPUs {
			s.at[j] = now
			if k := slices.Index(gpus, g); k >= 0 {
				s.at[j] = at[k]
				gpus, at = slices.Delete(gpus, k, k+1), slices.Delete(at, k, k+1)
			}
		}
		out = append(out, s)
	}
	return out
}

// Placed is what a variant's pods hold, as an in-flight entry counts them
// (InFlightLedger.Admit): Whole, the replicas every pod of which holds a node
// (scheduled to it, or nominated for it); Partial, each replica -- a
// LeaderWorkerSet group -- some but not all of whose pods do. The ledger
// holds a partial replica's other pods in the topology domain its placed
// pods are in (WN2).
type Placed struct {
	Whole   int
	Partial []domain.PartialReplica
}

// placedAt is a placed count and when it was first observed.
type placedAt struct {
	n  int
	at time.Time
}

// NewInFlightLedger returns an empty ledger.
func NewInFlightLedger() *InFlightLedger {
	return &InFlightLedger{entries: map[string]*inFlightEntry{}, sighted: map[string]time.Time{}, restored: map[string]bool{}}
}

// DefaultInFlight is the process-wide ledger both engines share.
var DefaultInFlight = NewInFlightLedger()

// Admit records that variant key ("namespace/name") is to have want replicas
// of shape holding a node, placedNow of which do. placed counts them later
// (nil: never known, so the entry holds until it expires). A want no higher
// than placedNow ends the entry. A want higher than the entry's admits more
// replicas and starts InFlightTTL afresh; an equal or lower want keeps the
// clock, so a replica Pending for good is not held again every cycle.
func (l *InFlightLedger) Admit(key string, shape ReplicaShape, want, placedNow int, placed func() (Placed, bool), now time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.admitLocked(InFlightAdmission{Key: key, Shape: shape, Want: want, PlacedNow: placedNow, Placed: placed}, now, true)
}

// InFlightAdmission is one variant's admission to the ledger, as Admit takes
// it.
type InFlightAdmission struct {
	Key       string
	Shape     ReplicaShape
	Want      int
	PlacedNow int
	Placed    func() (Placed, bool)
	// Target is the variant's scale target, whose pods Placed counts. It is
	// persisted with the entry, so the entry counts them again from the
	// moment a restart restores it (RestoreInFlightWith).
	Target ScaleTargetRef
	// TTL is how long the replicas a rise of Want admits are held, at least
	// InFlightTTL: the steady-state engine holds a replica on a Kueue flavor
	// for the borrowing placement timeout when that is longer, since the
	// governor waits that long before it releases the replica.
	TTL time.Duration
	// ExistingOnly marks a target that does not grow the variant: it updates
	// the growth still in flight and starts no hold of its own -- when the
	// variant has a replica that cannot place (Unplaced), or once this
	// process has sighted the variant. On the first sighting, with every
	// replica placeable, it starts a hold as growth does: after a restart
	// the replica a Pending pod was ordered for before it is growth no entry
	// remembers when the persisted ledger did not carry it (it was written
	// before the order, or could not be read). Without that hold a wake took
	// the node the Pending pod was about to land on. An entry the persisted
	// ledger restored (RestoreInFlight) is updated like any other, so a
	// replica that cannot place keeps the hold it had before the restart.
	ExistingOnly bool
	// Unplaced is set when the variant has a replica that cannot place: a
	// pod Unschedulable, or held at Kueue's admission gate past a cycle --
	// a preempted borrower's replacement. A hole would only keep that
	// replica's node from a peer that can use it.
	Unplaced bool
}

// InFlightMark is the entries a provisional admission (Provisional)
// replaced, per variant key; nil for a variant that had none.
type InFlightMark map[string]*inFlightEntry

// Provisional admits adms at once and returns what they replaced, for
// Commit. The steady-state engine admits its pass's placements so right
// after the optimizer, before the rest of its cycle -- the utilization share,
// scale-to-zero, the borrowing releases -- may lower them: a wake in that
// window saw the nodes the pass chose free and took one (WN5).
func (l *InFlightLedger) Provisional(adms []InFlightAdmission, now time.Time) InFlightMark {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	mark := InFlightMark{}
	for _, a := range adms {
		if _, seen := mark[a.Key]; seen {
			continue
		}
		var prev *inFlightEntry
		if e, ok := l.entries[a.Key]; ok {
			c := *e
			c.seen = slices.Clone(e.seen)
			prev = &c
		}
		mark[a.Key] = prev
	}
	for _, a := range adms {
		// Not a sighting: Commit sets the entry back and admits again.
		l.admitLocked(a, now, false)
	}
	return mark
}

// Commit replaces a provisional admission with the cycle's final one, as one
// step: every variant mark covers is set back to its entry before, then adms
// are admitted. A target the cycle lowered after the pass thus holds what
// the lower target asks, on the clock it had before -- a replica the pass
// asked again and a release took back does not restart its hold. A nil mark
// only admits.
//
// A wake that admits a variant mark covers between Provisional and Commit
// is set back with the rest: its admission is discarded, and the cycle's
// final admission for the variant, made from the states read before the
// wake, stands. For a variant the cycle kept at zero that ends the wake's
// hold: the woken replica's node is free to other decisions until its pod
// lands, and no later cycle holds it again (ExistingOnly). The end-of-cycle
// admission always replaced an earlier one this way for the variants the
// cycle decided; it is left so because a wake and a pass rarely decide one
// variant within one cycle.
func (l *InFlightLedger) Commit(mark InFlightMark, adms []InFlightAdmission, now time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, prev := range mark {
		if prev == nil {
			delete(l.entries, k)
			continue
		}
		l.entries[k] = prev
	}
	maps.DeleteFunc(l.sighted, func(_ string, at time.Time) bool { return now.Sub(at) > sightedFor })
	for _, a := range adms {
		l.admitLocked(a, now, true)
	}
}

// admitLocked is Admit under the lock. sight records the variant as sighted
// (InFlightAdmission.ExistingOnly); a provisional admission does not.
func (l *InFlightLedger) admitLocked(a InFlightAdmission, now time.Time, sight bool) {
	key, shape, want, placedNow, placed := a.Key, a.Shape, a.Want, a.PlacedNow, a.Placed
	_, sighted := l.sighted[key]
	if sight {
		l.sighted[key] = now
		// Admitted again: the entry has its own count now.
		delete(l.restored, key)
	}
	if want <= placedNow {
		delete(l.entries, key)
		return
	}
	ttl := max(a.TTL, InFlightTTL)
	e, ok := l.entries[key]
	switch {
	case !ok && a.ExistingOnly && (a.Unplaced || sighted):
		return
	case !ok:
		l.entries[key] = &inFlightEntry{shape: shape, want: want, base: placedNow,
			expires: now.Add(ttl), ttl: ttl, placed: placed, target: a.Target}
		return
	}
	if a.Target.Name != "" {
		e.target = a.Target
	}
	if shape.Flavor == nil && e.shape.Flavor != nil && want <= e.want {
		// The replicas admitted on a borrowed flavor's nodes are the same
		// ones once the borrowed draw resolves (their pods exist, gated by
		// Kueue and still holding no node): their holes stay on that flavor,
		// for as long as it holds them.
		shape.Flavor, shape.Placements = e.shape.Flavor, e.shape.Placements
		ttl = max(ttl, e.ttl)
	}
	switch {
	case want > e.want:
		e.expires = now.Add(ttl)
	case ttl != e.ttl:
		// Same replicas, another TTL: the hold ends ttl after it started. An
		// entry that left its flavor holds for InFlightTTL, not the flavor's
		// placement timeout.
		e.expires = e.expires.Add(ttl - e.ttl)
	}
	e.ttl = ttl
	e.shape, e.want, e.placed = shape, want, placed
	e.base = max(e.base, placedNow)
}

// Holds reports whether the ledger holds anything for variant key now.
func (l *InFlightLedger) Holds(key string, now time.Time) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	return ok && now.Before(e.expires)
}

// Forget drops variant key's entry.
func (l *InFlightLedger) Forget(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// Reset empties the ledger. For tests.
func (l *InFlightLedger) Reset() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	clear(l.entries)
	clear(l.sighted)
	clear(l.restored)
	l.restorePending, l.restoreExpected = false, time.Time{}
}

// ExpectRestore marks the persisted ledger as still to be read back: until
// MarkRestored, RestorePending reports true, and a decision that builds a
// node view from the ledger would miss the holds a restart carries. Called
// once at start, before either engine runs, with the time of the start:
// how long a wake has waited is measured from it (RestorePendingSince), or
// from a later RestartRestoreClock (when the replica becomes leader).
func (l *InFlightLedger) ExpectRestore(now time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.restorePending, l.restoreExpected = true, now
}

// RestartRestoreClock moves the time a pending restore is measured from to
// now: the engines run only while this replica leads, so a standby that
// waited for the lease measures from when it won it, not from its start.
// No-op once nothing is pending.
func (l *InFlightLedger) RestartRestoreClock(now time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.restorePending {
		l.restoreExpected = now
	}
}

// MarkRestored records that the persisted ledger was read back, or found
// absent or unusable: nothing more is to be restored.
func (l *InFlightLedger) MarkRestored() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.restorePending, l.restoreExpected = false, time.Time{}
}

// RestorePending reports whether the persisted ledger is still to be read
// back (ExpectRestore).
func (l *InFlightLedger) RestorePending() bool {
	_, pending := l.RestorePendingSince()
	return pending
}

// RestorePendingSince is RestorePending with when the restore was expected
// (ExpectRestore's time).
func (l *InFlightLedger) RestorePendingSince() (time.Time, bool) {
	if l == nil {
		return time.Time{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.restoreExpected, l.restorePending
}

// settle counts each entry's placed replicas: an entry whose variant has
// them all ends, a higher count is remembered with when it was seen, and an
// entry expired a TTL ago is dropped (its variant was deleted, or stopped
// asking). The counts are read without the lock: they read the pods.
func (l *InFlightLedger) settle(now time.Time) {
	l.mu.Lock()
	keys := slices.Sorted(maps.Keys(l.entries))
	counts := make(map[string]func() (Placed, bool), len(keys))
	for _, k := range keys {
		if e := l.entries[k]; e.placed != nil {
			counts[k] = e.placed
		}
	}
	l.mu.Unlock()
	observed := map[string]Placed{}
	for _, k := range keys {
		if f := counts[k]; f != nil {
			if n, ok := f(); ok {
				observed[k] = n
			}
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		e, ok := l.entries[k]
		if !ok {
			continue
		}
		if now.Sub(e.expires) > max(e.ttl, InFlightTTL) {
			delete(l.entries, k)
			continue
		}
		p, ok := observed[k]
		if !ok {
			continue
		}
		n := p.Whole
		if n >= e.want {
			delete(l.entries, k)
			continue
		}
		if last := e.base; n > last && (len(e.seen) == 0 || n > e.seen[len(e.seen)-1].n) {
			e.seen = append(e.seen, placedAt{n: n, at: now})
		}
		e.partial = mergePartial(e.partial, p.Partial, now)
	}
}

// holdOn debits from v the replicas the ledger holds as of now, each charged
// to its own owner under InFlightOwner: per live entry, in key order, want less the replicas placed
// by the time v's snapshot was taken, each placed best-fit. One that no
// longer fits is skipped -- it is in flight either way, and the hole it was
// admitted to has gone to someone else.
func (l *InFlightLedger) holdOn(v *NodeView, now time.Time) {
	if l == nil || v == nil {
		return
	}
	l.settle(now)
	l.mu.Lock()
	defer l.mu.Unlock()
	prev := v.setOwner(InFlightOwner)
	defer v.setOwner(prev)
	for _, k := range slices.Sorted(maps.Keys(l.entries)) {
		e := l.entries[k]
		if !now.Before(e.expires) {
			continue
		}
		placed := e.base
		for _, s := range e.seen {
			if !s.at.After(v.takenAt) {
				placed = max(placed, s.n)
			}
		}
		r := v.fitOf(e.shape)
		// Each held replica is charged to its own owner, numbered from the
		// variant's first replica, so ReleaseUncreated can take back the
		// holes of the replicas whose pods do not exist yet. A replica part
		// of whose pods hold a node comes first: its pods exist, and only
		// the rest are held, where its placed pods are (holdPartial).
		for n := placed + 1; n <= e.want; n++ {
			v.setOwner(inFlightReplicaOwner(k, n))
			if i := n - placed - 1; i < len(e.partial) {
				v.holdPartial(r, e.partial[i])
				continue
			}
			v.place(r)
		}
	}
}

// holdPartial holds, for one replica part of whose pods hold a node (p), the
// pods that hold none yet -- r's pods less the placed ones' GPUs -- best-fit
// in the topology domain of the node p's placed pods hold (r.domain; the
// group's own domain, so it is not refused as taken), or anywhere r may land
// when r has no topology key or the node is not in the view. Held whole, the
// replica found no hole once its leader held one node, and the node its
// workers need was free to anyone (WN2). A placed pod first seen after the
// view's snapshot was taken is not in it: its GPUs are held on its node too,
// as far as it has them free. One the snapshot shows is not charged again.
// Nothing is held when the rest fit nowhere.
func (v *NodeView) holdPartial(r replicaFit, p partialSeen) {
	if v == nil || len(r.pods) == 0 {
		return
	}
	rest := slices.Clone(r.pods)
	for _, g := range p.PlacedGPUs {
		if i := slices.Index(rest, g); i >= 0 {
			rest = slices.Delete(rest, i, i+1)
		}
	}
	rest = slices.DeleteFunc(rest, func(g int) bool { return g <= 0 })
	node, known := v.slotOf(p.Node)
	if known {
		for j, g := range p.PlacedGPUs {
			if !p.at[j].After(v.takenAt) {
				continue
			}
			if f := v.free(node); f > 0 && g > 0 {
				v.charge(node, min(g, f))
			}
		}
	}
	if len(rest) == 0 {
		return
	}
	key, d := "", ""
	if known && r.domain != "" {
		if d = v.slots[node].labels[r.domain]; d != "" {
			key = r.domain
		}
	}
	// Which of r's pods the rest are is not known: they are fitted where any
	// pod of r may land (eligibleFit).
	nodes := v.eligibleFit(r)
	pods := make([]podFit, 0, len(rest))
	for _, g := range largestFirst(rest) {
		pods = append(pods, podFit{gpus: g, nodes: nodes})
	}
	taken, ok := v.bestFit(pods, key, d)
	if !ok {
		return
	}
	for _, i := range slices.Sorted(maps.Keys(taken)) {
		v.charge(i, taken[i])
	}
	if d != "" {
		v.takeDomain(domainID(key, d))
	}
}

// inFlightReplicaOwner is the owner of the hole the ledger holds for variant
// key's n-th replica (1-based): InFlightOwner, then the variant and number.
func inFlightReplicaOwner(key string, n int) string {
	return InFlightOwner + "\x00" + key + "\x00" + strconv.Itoa(n)
}

// ReleaseUncreated takes back from v the holes the in-flight ledger holds for
// replicas whose pods do not exist yet: per variant key ("namespace/name") of
// created, every held replica numbered above its created replicas. A pass that
// sizes a variant from its current replicas re-places exactly those replicas;
// holding their holes against it refused the very replica it ordered a cycle
// ago, and its target alternated. The holes of replicas whose pods exist and
// hold no node yet stay held. A variant not in created is left as it is.
func (l *InFlightLedger) ReleaseUncreated(v *NodeView, created map[string]int) {
	if l == nil || v == nil || len(created) == 0 {
		return
	}
	release := map[string]bool{}
	for owner := range v.owned {
		rest, ok := strings.CutPrefix(owner, InFlightOwner+"\x00")
		if !ok {
			continue
		}
		key, num, ok := strings.Cut(rest, "\x00")
		if !ok {
			continue
		}
		have, tracked := created[key]
		n, err := strconv.Atoi(num)
		if tracked && err == nil && n > have {
			release[owner] = true
		}
	}
	for owner := range release {
		for i, g := range v.owned[owner] {
			if v.debit[i] -= g; v.debit[i] <= 0 {
				delete(v.debit, i)
			}
		}
		delete(v.owned, owner)
	}
	maps.DeleteFunc(v.domains, func(_, owner string) bool { return release[owner] })
}
