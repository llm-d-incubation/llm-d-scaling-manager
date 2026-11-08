package allocation

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// Handing borrowed capacity back to a lender Kueue does not reclaim for
// (PR-C of the Kueue borrowing proposal, kueue.releaseToNonReclaimingLenders).
//
// With the option on, what a lender on reclaimWithinCohort Never -- or on
// LowerPriority below the borrowing model's priority -- lends is counted
// (reclaimablePools). Kueue never preempts for such a lender, so when it waits
// for what it lent, the scaling manager lowers the borrowing models itself:
// it is the lender's reclaim agent. A lender Kueue reclaims for (Any, or
// LowerPriority above the model) is left to Kueue, as without the option.

// ReleaseLenderWaiting is the release cause of borrowed capacity handed back
// to a waiting lender Kueue does not reclaim for (lenderReleases).
const ReleaseLenderWaiting ReleaseCause = "lender-waiting"

// LenderReleasePinReason is the reason a lender release lowers a decision
// with, and the scaling-blocked reason on the model while it is in flight.
const LenderReleasePinReason = "released-to-lender"

// LenderReleaseStepName names the decision step of a lender release.
const LenderReleaseStepName = "kueue-lender-release"

// BorrowLedgerVersion is the ledger format this build writes and reads. A
// ledger that names no version is of version 1. One of a newer version is
// discarded (ErrBorrowLedgerVersion): its fields may not mean what this
// build reads them as.
const BorrowLedgerVersion = 1

// ErrBorrowLedgerVersion is returned by UnmarshalBorrowLedger for a ledger a
// newer build wrote in a format this one does not know.
var ErrBorrowLedgerVersion = errors.New("borrow release ledger: unknown version")

// maxLedgerUnits bounds the variants one persisted lender release may name.
const maxLedgerUnits = 64

// ReleasedUnit is one variant a lender release lowered, and to what.
type ReleasedUnit struct {
	Namespace string `json:"namespace"`
	// Model is the variant's model, whose cooldown in the lender's cohort
	// starts when the release lands.
	Model   string `json:"model,omitempty"`
	Variant string `json:"variant"`
	To      int    `json:"to"`
}

// LenderRelease is a release in flight to one waiting lender: the variants it
// lowered, the GPUs it hands back, and when. No further release goes to that
// lender while it exists. It lands once every variant holds no more than its
// target (LandedAt), which starts each model's cooldown in the lender's
// cohort. It clears once a fresh read shows the lender no longer waiting, or
// Kueue's borrowed figure fallen by at least GPUs (Kueue took the GPUs in),
// or once it outlived the release timeout (lenderTimeout).
type LenderRelease struct {
	LenderKey
	Units []ReleasedUnit `json:"units"`
	GPUs  int            `json:"gpus"`
	// BorrowedBefore is Kueue's borrowed figure of the lender's pool when the
	// release was decided: once a fresh read shows it fallen by GPUs, Kueue
	// has taken the release in.
	BorrowedBefore int        `json:"borrowedBefore,omitempty"`
	ReleasedAt     time.Time  `json:"releasedAt"`
	LandedAt       *time.Time `json:"landedAt,omitempty"`
	// Taken is set when the release landed, yet a read past Kueue's
	// admission lag still shows the lender waiting and Kueue's borrowed
	// figure not fallen by GPUs: another borrower took what was handed back
	// (LenderTaken). The entry then pins nothing and stays, so nothing more
	// is released to the lender, until the lender stops waiting.
	Taken bool `json:"taken,omitempty"`
	// TimedOut is set when the release outlived lenderTimeout with a P/D
	// model only partly landed: that model's pins alone are kept, so its
	// pair is never left half lowered, until it lands or a second timeout.
	TimedOut bool `json:"timedOut,omitempty"`
}

// lenderTimeout is how long a release to a lender of cohort may stay in
// flight before it is dropped and the lender may be released to again: the
// release actuation window, or twice the cohort's expected release latency
// when that is longer (a ScaledObject with a long scale-down window).
func (g *BorrowGovernor) lenderTimeout(cohort string) time.Duration {
	return max(g.settings.ReleaseActuation, 2*g.settings.ReleaseLatencies[cohort])
}

// MaxReleaseLatency caps a cohort's expected release latency
// (BorrowSettings.ReleaseLatencies): a tenant's hour-long termination grace or
// scale-down window must not hold a release in flight, or delay the
// lender-waiting alert, for longer than this.
const MaxReleaseLatency = 30 * time.Minute

// LenderReleaseScope is, per cohort whose pool opts in to
// releaseToNonReclaimingLenders (every entry reading it), the
// "namespace/variant" keys a release to its lenders may lower: the variants of
// the cohort's namespaces on a type the pool serves. Nil when none opts in.
// A cohort's release latency is measured over these variants alone, and the
// lender-waiting release counter is created for their models alone.
func LenderReleaseScope(constraints []*ResourceConstraints, requests []ModelScalingRequest) map[string][]string {
	chunks, cohorts := mergeSharedPools(constraints)
	var out map[string][]string
	for _, ch := range chunks {
		if !ch.ReleaseToNonReclaiming {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		if _, ok := out[ch.Cohort]; !ok {
			out[ch.Cohort] = []string{}
		}
		for _, req := range requests {
			if !slices.Contains(cohorts[req.Namespace], ch.Cohort) {
				continue
			}
			for _, st := range req.VariantStates {
				k := req.Namespace + "/" + st.VariantName
				if ch.serves(accOf(req, st.VariantName)) && !slices.Contains(out[ch.Cohort], k) {
					out[ch.Cohort] = append(out[ch.Cohort], k)
				}
			}
		}
	}
	for c := range out {
		slices.Sort(out[c])
	}
	return out
}

// lenderPinned reports whether a lender release in flight lowers the
// variant "namespace/variant".
func (g *BorrowGovernor) lenderPinned(key string) bool {
	for _, e := range g.lenderRel {
		for _, u := range e.Units {
			if u.Namespace+"/"+u.Variant == key {
				return true
			}
		}
	}
	return false
}

// modelPinned reports whether a release in flight -- a placement-timeout or
// gate pin, or a lender release -- lowers a variant of req.
func (g *BorrowGovernor) modelPinned(req ModelScalingRequest) bool {
	return slices.ContainsFunc(req.VariantStates, func(st domain.VariantReplicaState) bool {
		k := req.Namespace + "/" + st.VariantName
		_, sticky := g.timeoutHeld[k]
		return sticky || g.lenderPinned(k)
	})
}

// poolRead is what one cycle's read of a pool says about its lender
// releases (observeLenderReleases).
type poolRead struct {
	at       time.Time
	fresh    bool
	borrowed int
	present  map[string]bool
	waiting  map[string]bool
}

// observeLenderReleases advances the lender releases in flight on one
// cycle's states and reads. A stale read changes nothing about a lender; the
// states still land a release.
//
//   - A release lands once every variant holds no more than its target;
//     that starts each released model's cooldown in the lender's cohort.
//   - It clears on a fresh read that shows its lender gone or no longer
//     waiting, or Kueue's borrowed figure fallen by at least the GPUs it
//     handed back (KC2): a landing alone does not clear it, so a read that
//     lags the landing releases nothing a second time.
//   - Landed, with a fresh read past Kueue's admission lag still showing the
//     lender waiting and the borrowed figure not fallen, it is Taken
//     (KC3): another borrower took what it handed back. It pins nothing
//     more, and nothing more goes to that lender while it waits.
//   - Past lenderTimeout it goes, except the pins of a P/D model only partly
//     landed, kept until it lands or a second timeout (WP3).
//   - A restored entry's unit whose variant is gone while its namespace is
//     read goes (WP7); an entry left without units goes.
func (g *BorrowGovernor) observeLenderReleases(chunks []*SharedPoolChunk, requests []ModelScalingRequest,
	states map[string]variantRef, now time.Time) {
	g.halfLowered = nil
	if len(g.lenderRel) == 0 {
		return
	}
	reads := map[string]poolRead{}
	for _, ch := range chunks {
		r := poolRead{at: ch.ReadAt, fresh: !ch.Stale, borrowed: ch.Borrowed, present: map[string]bool{}, waiting: map[string]bool{}}
		for _, l := range ch.Lenders {
			r.present[l.Queue] = true
			r.waiting[l.Queue] = r.waiting[l.Queue] || l.Waiting
		}
		reads[ch.key()] = r
	}
	readNS := map[string]bool{}
	for _, req := range requests {
		readNS[req.Namespace] = true
	}
	for k, e := range g.lenderRel {
		if e.ReleasedAt.After(now) {
			e.ReleasedAt = now // a restored stamp from the future
		}
		if g.lenderRestored[k] {
			e.Units = slices.DeleteFunc(e.Units, func(u ReleasedUnit) bool {
				_, ok := states[u.Namespace+"/"+u.Variant]
				return !ok && readNS[u.Namespace] // deleted: it never lands
			})
			if len(e.Units) == 0 && !e.Taken {
				g.dropLenderRelease(k)
				continue
			}
		}
		if e.LandedAt == nil && len(e.Units) > 0 && unitsLanded(e.Units, states) {
			at := now
			e.LandedAt = &at
			for _, m := range releasedModels(e.Units, requests) {
				g.markBackoff(m, []string{k.Cohort}, at, true)
			}
		}
		if r, ok := reads[k.flavorKey()]; ok && r.fresh {
			switch {
			case !r.present[k.Lender] || !r.waiting[k.Lender]:
				g.dropLenderRelease(k) // it waits for nothing now
				continue
			case !e.Taken && e.GPUs > 0 && r.borrowed <= e.BorrowedBefore-e.GPUs:
				g.dropLenderRelease(k) // Kueue took it in
				continue
			case !e.Taken && e.LandedAt != nil && r.at.After(e.LandedAt.Add(kueueSchedulingInterval)):
				e.Taken, e.Units = true, nil
			}
		}
		age := now.Sub(e.ReleasedAt)
		if e.Taken {
			// It stays while the lender waits, but not for good: a misread
			// (Kueue's figure lagging, say) would starve the lender. Past
			// twice the timeout one new release may be tried.
			if age > 2*g.lenderTimeout(k.Cohort) {
				g.dropLenderRelease(k)
			}
			continue
		}
		switch {
		case age <= g.lenderTimeout(k.Cohort):
		case e.TimedOut && unitsLanded(e.Units, states):
			g.dropLenderRelease(k)
		case e.TimedOut && age > 2*g.lenderTimeout(k.Cohort):
			// Its pins go with it: the pair stays half lowered. Say so.
			g.halfLowered = append(g.halfLowered, releasedModels(e.Units, requests)...)
			g.dropLenderRelease(k)
		case !e.TimedOut:
			if half := halfLanded(e.Units, states); len(half) > 0 {
				e.Units, e.TimedOut = half, true
			} else {
				g.dropLenderRelease(k)
			}
		}
	}
}

// dropLenderRelease forgets a lender release.
func (g *BorrowGovernor) dropLenderRelease(k LenderKey) {
	delete(g.lenderRel, k)
	delete(g.lenderRestored, k)
}

// unitsLanded reports whether every released variant is read and holds no
// more than its target (never read below its minReplicas).
func unitsLanded(units []ReleasedUnit, states map[string]variantRef) bool {
	for _, u := range units {
		s, ok := states[u.Namespace+"/"+u.Variant]
		if !ok || heldOf(s.state) > max(u.To, minOf(s.state)) {
			return false
		}
	}
	return true
}

// halfLanded is the units of every model some of whose released variants
// landed while others did not: a P/D pair half lowered.
func halfLanded(units []ReleasedUnit, states map[string]variantRef) []ReleasedUnit {
	byModel := map[string][]ReleasedUnit{}
	for _, u := range units {
		byModel[u.Namespace+"/"+u.Model] = append(byModel[u.Namespace+"/"+u.Model], u)
	}
	var out []ReleasedUnit
	for _, m := range slices.Sorted(maps.Keys(byModel)) {
		us := byModel[m]
		landed := 0
		for _, u := range us {
			if unitsLanded([]ReleasedUnit{u}, states) {
				landed++
			}
		}
		if landed > 0 && landed < len(us) {
			out = append(out, us...)
		}
	}
	return out
}

// releasedModels is the "namespace/model" keys of a release's units: the
// unit's own, or, for a unit that names none, the model whose request holds
// the variant.
//
// A unit names no model only in a ledger written before units carried one
// (the first cut of version 1). Such an entry is handled per entry, as it
// always was: halfLanded groups its units under one empty model, so a
// timeout keeps all or none of its pins; only the backoff looks the model up
// here.
func releasedModels(units []ReleasedUnit, requests []ModelScalingRequest) []string {
	byVariant := map[string]string{}
	for _, req := range requests {
		for _, st := range req.VariantStates {
			byVariant[req.Namespace+"/"+st.VariantName] = req.ModelID
		}
	}
	set := map[string]bool{}
	for _, u := range units {
		m := u.Model
		if m == "" {
			m = byVariant[u.Namespace+"/"+u.Variant]
		}
		if m != "" {
			set[u.Namespace+"/"+m] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// kueueSchedulingInterval is how long a Workload may wait before its wait
// is more than Kueue's admission lag: Kueue retries a pending Workload on
// every change in its cohort, and within this a Workload a free pool covers
// has been admitted. A lender's wait triggers a release once its Workloads
// waited this long, or on two fresh reads in a row (releaseDue); a landed
// release is judged by a read at least this long after it landed.
const kueueSchedulingInterval = time.Minute

// Reasons a waiting lender Kueue does not reclaim for is handed nothing
// (LenderReleaseBlock.Reason).
const (
	// LenderUnservable: what this controller can give back on the lender's
	// flavor -- whole units, never below minReplicas or a namespace's
	// guaranteed figure, at most one replica past the need -- does not cover
	// what the lender still lacks once the cohort's free pool, its own kept
	// part, what is already being freed and what Kueue preempts for it are
	// counted. Anything handed back would sit idle.
	LenderUnservable = "unservable"
	// LenderTaken: a release to the lender landed, yet a read since shows it
	// still waiting and Kueue's borrowed figure not fallen by what was handed
	// back: another borrower took it. Nothing more is released to it while it
	// waits.
	LenderTaken = "taken-by-another-borrower"
	// LenderPreempting: Workloads Kueue preempted on the lender's flavor in
	// its cohort still hold their quota while their pods stop; nothing is
	// released until they have (Givable is their GPUs), for at most
	// kueue.EvictionBound from each eviction.
	LenderPreempting = "kueue-preempting"
)

// LenderReleaseBlock is a waiting lender Kueue does not reclaim for that a
// cycle handed nothing, and why.
type LenderReleaseBlock struct {
	Lender LenderKey
	Reason string
	// Need is what the lender still lacked, Givable what this controller
	// could have handed back on its flavor (LenderUnservable).
	Need, Givable int
	// Models are the "namespace/model" keys that borrow in its cohort and
	// were candidates for the release.
	Models []string
}

// SharedBorrow is a pool where a release to a lender was weighed and Kueue's
// borrowed figure exceeds what this controller's namespaces hold past their
// guaranteed figures there: another borrower holds the rest, possibly a
// second scaling manager that hands the same lender the same GPUs.
type SharedBorrow struct {
	Cohort, Flavor, Resource string
	Kueue, Ours              int
}

// LenderReport is what the last Releases saw of the waiting lenders it did
// not serve, for the engine to log, count and record.
type LenderReport struct {
	Blocked []LenderReleaseBlock
	Shared  []SharedBorrow
	// HalfLowered is the "namespace/model" keys of P/D models whose release
	// to a lender outlived twice its timeout half landed: its pins dropped
	// with one role lowered and its partner not.
	HalfLowered []string
}

// LenderReport returns what the last Releases reported.
func (g *BorrowGovernor) LenderReport() LenderReport {
	g.mu.Lock()
	defer g.mu.Unlock()
	return LenderReport{Blocked: slices.Clone(g.report.Blocked), Shared: slices.Clone(g.report.Shared),
		HalfLowered: slices.Clone(g.halfLowered)}
}

// lenderReleases hands borrowed capacity back, on every fresh pool whose entry
// opts in (SharedPoolChunk.ReleaseToNonReclaiming), to each lender that waits,
// that Kueue does not reclaim for, and that has no release in flight. A
// release is sized to what unblocks the lender, net of everything that
// already covers it:
//
//	shortfall = PendingGPUs (its smallest Workload Kueue could admit next)
//	          - its kept part, max(0, G - U)
//	          - what Kueue preempts for it from models it outranks
//	          - the pool's share of: Kueue's raw free pool, the GPUs this
//	            cycle already frees on the flavor (current - target, held -
//	            current: terminating pods) and those of landed releases Kueue
//	            has not yet shown free
//
// The pool's share is one budget for its lenders, taken in order, so two
// lenders never count the same free GPU; Kueue's borrowed figure is another,
// decremented by each release, so two lenders are never handed more than is
// borrowed. The models are lowered by whole units until the shortfall is
// covered -- the last unit may pass it by at most one replica -- or, when
// they cannot cover it, not at all: the lender is reported unservable
// (LenderUnservable) and nothing is handed back to sit idle.
//
//   - Models are taken lowest priority first: Kueue's priority of the model
//     (its priority class, as the pool read it; a class not read last), then
//     its scaling priority, then the most expensive.
//   - A unit is one replica of a single-role model on the lender's flavor, or
//     a whole pair at the model's role ratio for a P/D one (yieldUnit, the
//     rule the reclaim yield uses), so a release never leaves a role without
//     its partner. Only replicas a model holds and would keep count as given
//     back: lowering a target above what it holds frees nothing.
//   - No variant goes below minReplicas, and no namespace below its
//     guaranteed figure (borrowedLeft).
//   - A model with a release in flight (any cause) is skipped. Under a
//     LowerPriority lender a model it outranks is left to Kueue, whose
//     preemption counts against the shortfall, and a model whose priority
//     was not read is left alone: Kueue may preempt it too.
//
// A lender's wait triggers a release only once it was read waiting twice in
// a row, or its Workloads waited past Kueue's admission lag (releaseDue).
// Nothing is released before the persisted ledger is read
// (BorrowCycle.LedgerUnread): a release from before a restart may still be
// in flight.
func (g *BorrowGovernor) lenderReleases(c BorrowCycle, states map[string]variantRef, decisions []domain.VariantDecision) []BorrowRelease {
	g.report = LenderReport{}
	if c.LedgerUnread {
		return nil
	}
	chunks, cohorts := mergeSharedPools(c.Constraints)
	var left map[string]map[string]int
	plain := map[string]domain.VariantReplicaState{}
	for k, s := range states {
		plain[k] = s.state
	}
	var out []BorrowRelease
	for _, ch := range chunks {
		if ch.Stale || !ch.ReleaseToNonReclaiming {
			continue
		}
		var b *poolBudget
		for _, l := range ch.Lenders {
			key := LenderKey{Cohort: ch.Cohort, Lender: l.Queue, Flavor: ch.Flavor, Resource: ch.Resource}
			if !l.Waiting || kueue.Reclaims(l.Reclaim, nil, nil) || l.PendingGPUs <= 0 {
				continue
			}
			if e := g.lenderRel[key]; e != nil {
				if e.Taken {
					g.report.Blocked = append(g.report.Blocked, LenderReleaseBlock{Lender: key, Reason: LenderTaken, Need: l.PendingGPUs})
				}
				continue
			}
			if !g.releaseDue(key, l, ch.ReadAt) || kueueReclaimsAll(l, ch) {
				continue
			}
			if kueueFreeing(ch) {
				g.report.Blocked = append(g.report.Blocked, LenderReleaseBlock{Lender: key, Reason: LenderPreempting,
					Need: l.PendingGPUs, Givable: ch.EvictingGPUs})
				continue
			}
			if left == nil {
				left = borrowedLeft(c, decisions)
			}
			if b == nil {
				b = g.budgetOf(c, ch, cohorts, plain, decisions, left)
			}
			if g.kueueMayPreemptOutside(key, l, ch, b) {
				continue
			}
			cands, outranked := g.lenderCandidates(c, key, l, ch, cohorts, plain, left)
			need := l.PendingGPUs - l.Kept - outranked
			covered := min(max(0, need), b.free)
			b.free -= covered
			need -= covered
			if need <= 0 {
				continue
			}
			var rel []BorrowRelease
			gave := min(b.ours, b.borrowed)
			if need <= b.borrowed {
				// Never more than what is still borrowed: two lenders of one
				// pool are not both handed the same borrowed GPUs.
				rel, gave = g.releaseTo(c, key, ch, cands, need, plain, decisions, left)
			}
			if rel == nil {
				block := LenderReleaseBlock{Lender: key, Reason: LenderUnservable, Need: need, Givable: gave}
				for _, cand := range cands {
					block.Models = append(block.Models, modelKey(cand.req))
				}
				g.report.Blocked = append(g.report.Blocked, block)
				continue
			}
			b.borrowed -= gave
			out = append(out, rel...)
		}
		if b != nil && ch.Borrowed > b.ours {
			g.report.Shared = append(g.report.Shared, SharedBorrow{Cohort: ch.Cohort, Flavor: ch.Flavor,
				Resource: ch.Resource, Kueue: ch.Borrowed, Ours: b.ours})
		}
	}
	return out
}

// releaseDue reports whether a lender's wait triggers a release now: it was
// read waiting on two fresh reads in a row (its debounce watch), or the
// longest-waiting of its Workloads has waited past Kueue's admission lag. One
// read alone may catch a Workload between its creation and its admission.
func (g *BorrowGovernor) releaseDue(key LenderKey, l kueue.LenderState, readAt time.Time) bool {
	if w := g.watches[key]; w != nil && w.reads >= 2 {
		return true
	}
	return !l.PendingSince.IsZero() && readAt.Sub(l.PendingSince) > kueueSchedulingInterval
}

// kueueReclaimsAll reports whether Kueue itself reclaims everything borrowed
// on the chunk for the lender: a LowerPriority lender whose head outranks
// the highest priority any borrower holds the cohort's GPUs at.
func kueueReclaimsAll(l kueue.LenderState, ch *SharedPoolChunk) bool {
	return l.Reclaim == kueue.ReclaimLowerPriority && ch.BorrowerPriority != nil &&
		kueue.Reclaims(l.Reclaim, l.PendingPriority, ch.BorrowerPriority)
}

// lenderPreemptWindow is how long a LowerPriority lender's wait is left to
// Kueue when Kueue may preempt a borrower this controller does not govern for
// it and the Workloads could not be read (SharedPoolChunk.EvictionsRead):
// one admission pass, plus twice the default termination grace for the
// victims to go. It is only a fallback: a victim with a longer grace is
// still terminating when it ends, its quota still in Kueue's borrowed
// figure. Where the Workloads are read, a victim is seen directly (Evicted,
// reason Preempted, quota still reserved: EvictingGPUs) and no release goes
// while one is, however long its grace; the wait is then one admission pass,
// for Kueue to choose its victims.
const lenderPreemptWindow = kueueSchedulingInterval + 2*k8sDefaultTerminationGrace

// kueueMayPreemptOutside reports whether a LowerPriority lender's wait is
// still Kueue's to answer: some borrower holds the pool's GPUs at a priority
// below the lender's waiting Workload, or at one not read, and Kueue's
// borrowed figure is more than this controller's namespaces hold, so that
// borrower may be one this controller does not govern, and lenderCandidates
// cannot count what Kueue preempts from it. The lender is left to Kueue
// until it has waited lenderPreemptWindow; then a fresh read decides, Kueue's
// preemption, if any, already in its figures. A borrower of this controller
// that is the lowest only delays the release by the window.
func (g *BorrowGovernor) kueueMayPreemptOutside(key LenderKey, l kueue.LenderState, ch *SharedPoolChunk, b *poolBudget) bool {
	if l.Reclaim != kueue.ReclaimLowerPriority || l.PendingPriority == nil || ch.Borrowed <= b.ours {
		return false
	}
	if ch.BorrowerLowest != nil && *ch.BorrowerLowest >= *l.PendingPriority {
		return false // nobody borrows below its head: Kueue preempts no one
	}
	since := l.PendingSince
	if w := g.watches[key]; w != nil && (since.IsZero() || w.since.Before(since)) {
		since = w.since
	}
	window := lenderPreemptWindow
	if ch.EvictionsRead {
		window = kueueSchedulingInterval
	}
	return since.IsZero() || ch.ReadAt.Sub(since) < window
}

// kueueFreeing reports whether Kueue is freeing GPUs on the chunk's flavor in
// the cohort by preemption: a Workload it preempted (Evicted, reason
// Preempted, not re-admitted since) still holds its quota while its pods
// stop. Its GPUs reach the pool, and its queue's borrowed figure falls, with
// nothing handed back; a release sized before they do would give the same
// GPUs twice. No release goes until they have -- but not for good: a victim
// evicted longer ago than kueue.EvictionBound is stuck (a pod Terminating on
// a NotReady node), not stopping, and is not waited for
// (SharedPoolChunk.StuckEvictingGPUs). The lender is reported
// (LenderPreempting) while it waits.
func kueueFreeing(ch *SharedPoolChunk) bool {
	return ch.EvictingGPUs > 0
}

// poolBudget is what one pool's waiting lenders share in a pass
// (lenderReleases): free, the GPUs Kueue admits a lender from with nothing
// handed back; borrowed, what is left of Kueue's borrowed figure to hand
// back; ours, what this controller's namespaces in the cohort hold past
// their guaranteed figures on the flavor.
type poolBudget struct {
	free, borrowed, ours int
}

// budgetOf is a pool's budget at the start of the pass: Kueue's raw free
// pool, plus what the cycle's decisions already free on the flavor in the
// cohort's namespaces -- current - target, and held - current (terminating
// pods) -- plus the GPUs of releases to lenders that landed and that Kueue
// has not yet shown free; and Kueue's borrowed figure.
func (g *BorrowGovernor) budgetOf(c BorrowCycle, ch *SharedPoolChunk, cohorts map[string][]string,
	states map[string]domain.VariantReplicaState, decisions []domain.VariantDecision, left map[string]map[string]int) *poolBudget {
	b := &poolBudget{free: max(0, ch.RawFree), borrowed: max(0, ch.Borrowed)}
	accs := map[string]string{}
	for _, req := range c.Requests {
		for _, st := range req.VariantStates {
			accs[req.Namespace+"/"+st.VariantName] = accOf(req, st.VariantName)
		}
	}
	// What the cycle frees reaches the cohort's pool only past what the
	// namespace's queue keeps: a GPU freed within it goes back to the queue
	// itself. The namespace's figure past guaranteed (gated pods aside, before
	// the cycle lowers it) is what it borrows from the pool, a lower bound of
	// what it holds past its kept part; freeing is counted up to it.
	past := namespaceBorrowed(c.Constraints, c.Gated)
	passive := passiveModels(c.Requests)
	freed := map[string]map[string]int{}
	for _, d := range decisions {
		acc := accs[d.Namespace+"/"+d.VariantName]
		if !slices.Contains(cohorts[d.Namespace], ch.Cohort) || !ch.serves(acc) {
			continue
		}
		t, ok := accelerator.FindKey(past[d.Namespace], acc)
		if !ok {
			continue
		}
		s := states[d.Namespace+"/"+d.VariantName]
		// This cycle's lower targets count before KEDA acts on them: the
		// other releases' pins (placement timeout, kueue gate) hold them in
		// force until they land, and counting them only once pods go would
		// hand the lender the same GPUs twice in this cycle. A target a wake's
		// reclaim yield lowered frees GPUs for this controller's own reclaim,
		// not for the lender: those do not count. Terminating pods (held past
		// current) are freeing already.
		freeing := max(0, heldOf(s)-d.CurrentReplicas)
		// A passive model's lower target is a would-be figure: nothing
		// lowers it, so it frees nothing. Its terminating pods are real.
		if !yielded(d) && !passive[d.Namespace+"/"+d.ModelID] {
			freeing += max(0, d.CurrentReplicas-d.TargetReplicas)
		}
		if freed[d.Namespace] == nil {
			freed[d.Namespace] = map[string]int{}
		}
		freed[d.Namespace][t] += freeing * max(1, d.GPUsPerReplica)
	}
	for ns, perType := range freed {
		for t, n := range perType {
			b.free += min(n, past[ns][t])
		}
	}
	for k, e := range g.lenderRel {
		if k.flavorKey() == ch.key() && e.LandedAt != nil && !e.Taken {
			b.free += e.GPUs
		}
	}
	for ns, perType := range left {
		if !slices.Contains(cohorts[ns], ch.Cohort) {
			continue
		}
		for t, n := range perType {
			if ch.serves(t) {
				b.ours += n
			}
		}
	}
	return b
}

// yielded reports whether a wake's or a reclaim's yield lowered d this cycle
// (yieldReclaimed's step).
func yielded(d domain.VariantDecision) bool {
	return slices.ContainsFunc(d.DecisionSteps, func(s domain.DecisionStep) bool { return s.Name == ReclaimYieldStepName })
}

// releaseTo lowers, for one waiting lender, the candidate models unit by unit
// until need GPUs on the lender's flavor are handed back (lenderReleases),
// records the release in flight, and returns its records and the GPUs it
// hands back. When the candidates cannot cover need, nothing is lowered: it
// returns nil and what they could have handed back.
func (g *BorrowGovernor) releaseTo(c BorrowCycle, key LenderKey, ch *SharedPoolChunk, cands []lenderCandidate,
	need int, states map[string]domain.VariantReplicaState, decisions []domain.VariantDecision,
	left map[string]map[string]int) ([]BorrowRelease, int) {
	before := slices.Clone(decisions)
	leftBefore := cloneBorrowedLeft(left)
	released := 0
	touched := map[int]bool{}
	for _, cand := range cands {
		if released >= need {
			break
		}
		ns := cand.req.Namespace
		eligible := func(i int) bool { return ch.serves(accOf(cand.req, decisions[i].VariantName)) }
		mine := map[int]bool{}
		nsBefore := maps.Clone(left[ns])
		gave := 0
		for released+gave < need {
			unit := yieldUnitWhere(decisions, ns, cand.req.ModelID, states, cand.req.Disaggregated, eligible)
			freed, largest, ok := unitTakes(unit, decisions, cand.req, ch, left[ns], states)
			if !ok {
				break
			}
			if freed-(need-released-gave) > largest {
				// The model's smallest unit passes what is still lacking by
				// more than one replica (a 2P:3D pair for 8 GPUs): giving it
				// back would drop far more than the lender waits for. The
				// next model, if any.
				break
			}
			if len(touchedWith(touched, mine, unit)) > maxLedgerUnits {
				break // more variants than one ledger entry may name
			}
			for _, i := range slices.Sorted(maps.Keys(unit)) {
				d := &decisions[i]
				gpr := max(1, d.GPUsPerReplica)
				if t, found := accelerator.FindKey(left[ns], accOf(cand.req, d.VariantName)); found {
					left[ns][t] -= heldBelow(*d, states[ns+"/"+d.VariantName], unit[i]) * gpr
				}
				d.TargetReplicas -= unit[i]
				d.GPUsAllocated = d.TargetReplicas * gpr
				mine[i] = true
			}
			gave += freed
		}
		if gave == 0 {
			// Only growth above what it holds was cancelled: nothing of it is
			// handed back, so nothing of it is lowered.
			for i := range mine {
				decisions[i] = before[i]
			}
			left[ns] = nsBefore
			continue
		}
		for i := range mine {
			touched[i] = true
		}
		released += gave
	}
	if released < need {
		copy(decisions, before)
		clear(left)
		for ns, m := range leftBefore {
			left[ns] = m
		}
		return nil, released
	}
	entry := &LenderRelease{LenderKey: key, GPUs: released, BorrowedBefore: ch.Borrowed, ReleasedAt: c.Now}
	out := make([]BorrowRelease, 0, len(touched))
	for _, i := range slices.Sorted(maps.Keys(touched)) {
		d := &decisions[i]
		lenderStep(d, key)
		entry.Units = append(entry.Units, ReleasedUnit{Namespace: d.Namespace, Model: d.ModelID, Variant: d.VariantName, To: d.TargetReplicas})
		out = append(out, BorrowRelease{Namespace: d.Namespace, ModelID: d.ModelID, Variant: d.VariantName,
			From: d.CurrentReplicas, To: d.TargetReplicas, Cause: ReleaseLenderWaiting, Lender: key})
	}
	slices.SortFunc(entry.Units, func(a, b ReleasedUnit) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Variant, b.Variant))
	})
	g.lenderRel[key] = entry
	return out, released
}

// touchedWith is the decisions a release names once unit is added.
func touchedWith(touched, mine map[int]bool, unit map[int]int) map[int]bool {
	out := maps.Clone(touched)
	maps.Copy(out, mine)
	for i := range unit {
		out[i] = true
	}
	return out
}

// cloneBorrowedLeft is a deep copy of borrowedLeft's figures.
func cloneBorrowedLeft(left map[string]map[string]int) map[string]map[string]int {
	out := make(map[string]map[string]int, len(left))
	for ns, m := range left {
		out[ns] = maps.Clone(m)
	}
	return out
}

// heldBelow is how many of the n replicas a unit takes off d's target the
// variant holds and would otherwise keep: those below min(target, held).
// Lowering a target above what the variant holds frees nothing.
func heldBelow(d domain.VariantDecision, s domain.VariantReplicaState, n int) int {
	h := heldOf(s)
	return max(0, min(d.TargetReplicas, h)-min(d.TargetReplicas-n, h))
}

// unitTakes is the GPUs a unit hands back on the chunk's flavor, counting
// only replicas a variant holds and would keep (heldBelow), the largest GPUs
// of one replica in it, and whether it may be released at all: it must take
// something, and every variant's GPUs must lie within what its namespace
// holds past guaranteed of the variant's type (left).
func unitTakes(unit map[int]int, decisions []domain.VariantDecision, req ModelScalingRequest, ch *SharedPoolChunk,
	left map[string]int, states map[string]domain.VariantReplicaState) (freed, largest int, ok bool) {
	if len(unit) == 0 {
		return 0, 0, false
	}
	per := map[string]int{}
	for i, n := range unit {
		d := decisions[i]
		acc := accOf(req, d.VariantName)
		t, found := accelerator.FindKey(left, acc)
		if !found {
			return 0, 0, false
		}
		gpr := max(1, d.GPUsPerReplica)
		largest = max(largest, gpr)
		g := heldBelow(d, states[req.Namespace+"/"+d.VariantName], n) * gpr
		per[t] += g
		if ch.serves(acc) {
			freed += g
		}
	}
	for t, g := range per {
		if g > left[t] {
			return 0, 0, false // never below the guaranteed figure
		}
	}
	return freed, largest, true
}

// borrowedLeft is, per borrowing namespace and type, what it holds past its
// guaranteed figure (gated pods aside), less what the cycle's decisions
// already lower it by: what a lender release may still take.
func borrowedLeft(c BorrowCycle, decisions []domain.VariantDecision) map[string]map[string]int {
	left := namespaceBorrowed(c.Constraints, c.Gated)
	accs := map[string]string{}
	for _, req := range c.Requests {
		for _, st := range req.VariantStates {
			accs[req.Namespace+"/"+st.VariantName] = accOf(req, st.VariantName)
		}
	}
	for _, d := range decisions {
		lowered := d.CurrentReplicas - d.TargetReplicas
		if lowered <= 0 {
			continue
		}
		if t, ok := accelerator.FindKey(left[d.Namespace], accs[d.Namespace+"/"+d.VariantName]); ok {
			left[d.Namespace][t] = max(0, left[d.Namespace][t]-lowered*max(1, d.GPUsPerReplica))
		}
	}
	return left
}

// lenderCandidate is a model a lender release may lower, with its order keys.
type lenderCandidate struct {
	req      ModelScalingRequest
	priority int64 // Kueue's priority of the model; math.MaxInt64 when not read
	cost     float64
}

// lenderCandidates is the models a release to the lender may lower, in
// release order (lenderReleases): in a namespace of the chunk's cohort that
// holds GPUs past its guaranteed figure on a type the chunk serves, with no
// release in flight. Under a LowerPriority lender, a model its head outranks
// is no candidate: Kueue preempts it for the lender, and its GPUs on the
// flavor past guaranteed are returned as outranked; a model whose priority
// was not read is no candidate either, nor counted.
func (g *BorrowGovernor) lenderCandidates(c BorrowCycle, key LenderKey, l kueue.LenderState, ch *SharedPoolChunk,
	cohorts map[string][]string, states map[string]domain.VariantReplicaState,
	left map[string]map[string]int) (out []lenderCandidate, outranked int) {
	lowerPriority := l.Reclaim == kueue.ReclaimLowerPriority
	preempted := map[string]map[string]int{} // per namespace and type, what Kueue preempts
	for _, req := range c.Requests {
		if !slices.Contains(cohorts[req.Namespace], key.Cohort) || len(left[req.Namespace]) == 0 || g.modelPinned(req) {
			continue
		}
		serves, cost := false, 0.0
		onFlavor := map[string]int{}
		for _, st := range req.VariantStates {
			acc := accOf(req, st.VariantName)
			if t, ok := accelerator.FindKey(left[req.Namespace], acc); ok && left[req.Namespace][t] > 0 && ch.serves(acc) {
				serves = true
				onFlavor[t] += heldOf(st) * max(1, st.GPUsPerReplica)
			}
			cost += float64(states[req.Namespace+"/"+st.VariantName].GPUsPerReplica) * variantCost(req, st.VariantName)
		}
		if !serves {
			continue
		}
		prio, known := modelKueuePriority(c.Constraints, ch, req)
		if lowerPriority {
			if !known {
				continue // Kueue may preempt it for the lender: not ours to lower too
			}
			p := prio
			if kueue.Reclaims(l.Reclaim, l.PendingPriority, &p) {
				if preempted[req.Namespace] == nil {
					preempted[req.Namespace] = map[string]int{}
				}
				for t, n := range onFlavor {
					preempted[req.Namespace][t] += n
				}
				continue // Kueue preempts it for the lender
			}
		}
		if req.Passive {
			continue // nothing lowers a passive model: no GPU comes of a release from it
		}
		cand := lenderCandidate{req: req, priority: math.MaxInt64, cost: cost}
		if known {
			cand.priority = int64(prio)
		}
		out = append(out, cand)
	}
	for ns, perType := range preempted {
		for t, n := range perType {
			outranked += min(n, left[ns][t])
		}
	}
	slices.SortStableFunc(out, func(a, b lenderCandidate) int {
		return cmp.Or(cmp.Compare(a.priority, b.priority), cmp.Compare(a.req.Priority, b.req.Priority),
			cmp.Compare(b.cost, a.cost), strings.Compare(modelKey(a.req), modelKey(b.req)))
	})
	return out, outranked
}

// variantCost is a variant's cost as discovery resolved it, 0 when unknown.
func variantCost(req ModelScalingRequest, variant string) float64 {
	for _, v := range req.Variants {
		if v.VariantName == variant {
			return v.Cost
		}
	}
	return 0
}

// modelKueuePriority is the priority Kueue gives the model's Workloads, as
// the chunk's read shows it: the highest over its variants' priority classes
// (SharedPoolChunk.drawable's lookup); a template naming no class takes the
// higher of that class's figure and its namespace's highest priority. Not
// known when some variant's class was not read.
func modelKueuePriority(constraints []*ResourceConstraints, ch *SharedPoolChunk, req ModelScalingRequest) (int32, bool) {
	var fallback *int32
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for _, b := range c.NamespaceBorrow[req.Namespace] {
			if b.HighestPriority != nil && (fallback == nil || *b.HighestPriority > *fallback) {
				p := *b.HighestPriority
				fallback = &p
			}
		}
	}
	var best int32
	for i, st := range req.VariantStates {
		prio, known := ch.ClassPriority[st.PriorityClass]
		if st.PriorityClass == kueue.PriorityClassNone && fallback != nil {
			if !known || *fallback > prio {
				prio = *fallback
			}
			known = true
		}
		if !known {
			return 0, false
		}
		if i == 0 || prio > best {
			best = prio
		}
	}
	return best, len(req.VariantStates) > 0
}

// lenderStep records on d that a release to a waiting lender lowered it.
func lenderStep(d *domain.VariantDecision, key LenderKey) {
	action := domain.ActionNoChange
	if d.TargetReplicas < d.CurrentReplicas {
		action = domain.ActionScaleDown
	}
	d.SetDecisionReason(action, domain.DecisionReasonV2, string(domain.DecisionReasonV2)+" ("+LenderReleasePinReason+")")
	d.DecisionSteps = append(d.DecisionSteps, domain.DecisionStep{
		Name: LenderReleaseStepName, Action: action, TargetReplicas: d.TargetReplicas, WasConstrained: true,
		Reason: fmt.Sprintf("ClusterQueue %s waits for capacity it lent in cohort %s, and Kueue does not reclaim it; "+
			"handed back (releaseToNonReclaimingLenders)", key.Lender, key.Cohort),
	})
}

// maxLedgerGPUs bounds the GPUs, and Kueue's borrowed figure, one persisted
// lender release may name: no cluster this controller runs on holds a
// million GPUs, so a larger figure is not one it wrote.
const maxLedgerGPUs = 1_000_000

// restoreLenderReleases adds the persisted lender releases the governor does
// not hold. An entry naming no complete lender, no unit (unless Taken, which
// pins nothing), more units than maxLedgerUnits, a unit that names no variant
// or a target out of bounds, no release time, GPUs out of bounds, or the same
// lender as an earlier entry is dropped and counted. What it adds is marked
// restored: a unit whose variant is gone clears (observeLenderReleases).
func (g *BorrowGovernor) restoreLenderReleases(entries []LenderRelease, dropped *int) {
	if len(entries) > maxLedgerItems {
		*dropped += len(entries) - maxLedgerItems
		entries = entries[:maxLedgerItems]
	}
	seen := map[LenderKey]bool{}
	for _, e := range entries {
		k := e.LenderKey
		if k.Cohort == "" || k.Lender == "" || k.Flavor == "" || k.Resource == "" || e.ReleasedAt.IsZero() ||
			(len(e.Units) == 0 && !e.Taken) || len(e.Units) > maxLedgerUnits ||
			e.GPUs < 0 || e.GPUs > maxLedgerGPUs || e.BorrowedBefore < 0 || e.BorrowedBefore > maxLedgerGPUs ||
			slices.ContainsFunc(e.Units, func(u ReleasedUnit) bool {
				return u.Namespace == "" || u.Variant == "" || u.To < 0 || u.To > maxLedgerReplicas
			}) || seen[k] {
			*dropped++
			continue
		}
		seen[k] = true
		if _, held := g.lenderRel[k]; held {
			continue
		}
		cp := e
		cp.Units = slices.Clone(e.Units)
		if e.LandedAt != nil {
			at := *e.LandedAt
			cp.LandedAt = &at
		}
		g.lenderRel[k] = &cp
		g.lenderRestored[k] = true
	}
}

// persistLenderReleases is the lender releases in flight, in a stable order,
// nil when none.
func (g *BorrowGovernor) persistLenderReleases() []LenderRelease {
	if len(g.lenderRel) == 0 {
		return nil
	}
	out := make([]LenderRelease, 0, len(g.lenderRel))
	for _, e := range g.lenderRel {
		cp := *e
		cp.Units = slices.Clone(e.Units)
		cp.ReleasedAt = e.ReleasedAt.UTC()
		if e.LandedAt != nil {
			at := e.LandedAt.UTC()
			cp.LandedAt = &at
		}
		out = append(out, cp)
	}
	slices.SortFunc(out, func(a, b LenderRelease) int { return compareLenderKeys(a.LenderKey, b.LenderKey) })
	return out
}

// applyLenderPins keeps every lender release in flight in force on decisions,
// never below a variant's minReplicas: a restored target below it (a ledger
// from before minReplicas was raised) is clamped.
func (g *BorrowGovernor) applyLenderPins(decisions []domain.VariantDecision, byVariant map[string]int,
	states map[string]variantRef) {
	for _, e := range g.lenderRel {
		for _, u := range e.Units {
			k := u.Namespace + "/" + u.Variant
			to := u.To
			if s, ok := states[k]; ok {
				to = max(to, minOf(s.state))
			}
			lowerTo(decisions, byVariant, k, to, LenderReleasePinReason)
		}
	}
}
