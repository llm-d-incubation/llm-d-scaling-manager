package steadystate

// Cohort borrowing on the engine side
// (the Kueue borrowing proposal, PR #161, B4 and B5): what
// the Kueue reader cannot see -- growth ordered on borrowed capacity that Kueue
// has not reserved yet, and managed pods held at its admission gate -- is taken
// out of the shared pools before the optimizer runs. Borrowed replicas are
// fitted into the cycle's node view (the one the physical limiter's node fit
// builds, allocation.NodeViewFor), charged to their models like any other
// placement, so the utilization share's idle fill after the pass and the
// in-flight ledger see them (allocation.DefaultInFlight).

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/controllerstatus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/resources"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// borrowingState is the steady-state engine's cohort-borrowing state.
type borrowingState struct {
	// inFlight is borrowed growth ordered and not yet visible as pods, and
	// what a Kueue read may not show yet: the process-wide record
	// (allocation.DefaultBorrowInFlight), which the wake reads too. The
	// release steps of the proposal extend it.
	inFlight *allocation.BorrowInFlight
	// credit is this cycle's draws in flight on open cohorts, each its
	// variant's to re-place (GreedyByScoreOptimizer.BorrowCredit).
	credit []allocation.BorrowDraw
	// reclaimCredit is this cycle's growth by Kueue's reclaim in flight, the
	// wakes' included, each its variant's to re-place
	// (GreedyByScoreOptimizer.ReclaimCredit, NEW-1).
	reclaimCredit []allocation.BorrowDraw
	// wakeYield is the wakes' reclaims this cycle's pass yields for
	// (GreedyByScoreOptimizer.WakeReclaims), reported yielded after the pass
	// (recordBorrowDraws).
	wakeYield []allocation.BorrowDraw
	// passFlavors is the Kueue flavor this cycle's pass placed each variant's
	// replicas on (GreedyByScoreOptimizer.PlacedFlavors).
	passFlavors map[string]*kueue.FlavorPlacement
	// gatedSeen is the managed pods seen at Kueue's admission gate, kept
	// past the gate until a read shows them (gatedGPUs); gatedErrLogged the
	// namespaces whose pod list failed and was logged, until one succeeds.
	gatedSeen      map[types.UID]gatedPod
	gatedErrLogged map[string]bool
	// reservedErrLogged is the namespaces whose Workload read for the gated
	// pods failed and was logged, until one succeeds (reservedGated).
	reservedErrLogged map[string]bool
	// physicalSkipped is set while a declared physical limiter is missing from
	// the cycle's constraints (notePhysicalSkipped): no new borrowing starts.
	physicalSkipped bool

	// governor decides the placement hold, the cooldowns and the releases of
	// borrowed capacity (borrowing_release.go); nil until a cycle borrows.
	governor *allocation.BorrowGovernor
	// ledgerLoaded is set once the persisted release ledger was read back;
	// ledgerErrLogged once a failed read was logged.
	ledgerLoaded    bool
	ledgerErrLogged bool
	// ledgerUnread is why the persisted ledger could not be read
	// (forbidden, read-error, newer-version); empty once read. A ledger of a
	// newer version is held unread: no release to a lender starts, and the
	// ledger is not persisted over it (persistBorrowLedger).
	ledgerUnread string
	// holdChecked is set once a cycle that counts no borrowing, before any
	// that does, read the persisted pins (restoreHeldReleases).
	holdChecked bool
	// lastLedger is the ledger last handed to the status publisher.
	lastLedger string
	// preemptSeen is the preemptions already counted -- the preempted pods'
	// UIDs, else the Workload's -- and their namespace; kept while listed.
	preemptSeen map[types.UID]string
	// watchSeen is the preempted pods the pod informer reported
	// (Engine.Preemptions), kept for the cooldown: they are gone by the next
	// list, so preemptSeen's keep-while-listed rule would forget them.
	watchSeen map[types.UID]time.Time
	// watchPending is the watched preemptions not matched to a model yet,
	// oldest first: the model may be missing from a cycle's requests, or the
	// cycle has no governor. Kept until matched or older than the cooldown,
	// bounded like the watch.
	watchPending []observedPreemption
	// preemptDroppedAt is when the watch or watchPending last dropped a
	// preemption past its bound, zero once a cycle counting borrowing acted on
	// it.
	preemptDroppedAt time.Time
	// preemptCounted is the preemptions already counted in
	// llmsm_kueue_preempted_pods_total, by preemptionKey, kept for the cooldown.
	preemptCounted map[string]time.Time
	// preemptEventAt is, per "namespace/model", when the last preemption
	// event counted on llmsm_kueue_preempted_pods_total happened; kept for the
	// cooldown (recordReclaim).
	preemptEventAt map[string]time.Time
	// preemptErrLogged and eventsErrLogged are the namespaces whose
	// preemption or cluster-autoscaler Event read failed and was logged, until
	// a read succeeds.
	preemptErrLogged map[string]bool
	eventsErrLogged  map[string]bool
	// cycle is this cycle's input to the governor, kept for the releases
	// after the optimizer; zero when the cycle borrows nothing.
	cycle allocation.BorrowCycle
	// gatedCycle is the governor's input in a cycle that counts no borrowing
	// and has Kueue-backed namespaces (governGated): only their replicas held
	// at Kueue's admission gate are released. Zero otherwise.
	gatedCycle allocation.BorrowCycle
	// gated is this cycle's GPUs of managed pods at Kueue's admission gate,
	// per namespace and accelerator (gatedGPUs), for what on-call sees.
	gated map[string]map[string]int
	// reclaimedUntil is, per "namespace/model", until when the
	// lender-reclaimed reason holds: a recent Kueue preemption plus the
	// cooldown.
	reclaimedUntil map[string]time.Time
	// budgets is the limiterBudgets value last handed to the status
	// publisher, whose asOf stamps the next one keeps.
	budgets controllerstatus.LimiterBudgets
	// cohortsByNS is this cycle's cohorts per borrowing namespace, so a
	// preemption backs its model off in those cohorts (B12); nil outside a
	// cycle that counts borrowing, when a preemption cools the model down in
	// every cohort.
	cohortsByNS map[string][]string
	// splitWarned is the borrow-namespaces-split warning last logged, so it
	// is logged once per change (B13).
	splitWarned string
	// releaseLatencies is, per cohort whose pool opts in to
	// releaseToNonReclaimingLenders, how long a release to a waiting lender
	// there takes to land (releaseLatencies), as last measured: kept through
	// cycles that count no borrowing.
	releaseLatencies map[string]time.Duration
	// lenderBlocked is, per lender reported as handed nothing, the report
	// last logged and recorded, so each is logged and recorded once per
	// change; sharedWarned the same for a pool where Kueue's borrowed
	// figure exceeds this controller's (SF4).
	lenderBlocked map[string]string
	sharedWarned  map[string]string
}

// prepareBorrowing readies one cycle's borrowing: it drops the in-flight draws
// that resolved, takes what is in flight and what managed pods hold at Kueue's
// admission gate out of the shared pools. Without borrowing in the
// constraints it returns them as they are, exactly as before borrowing was
// counted; the pods at Kueue's gate are still listed and recorded, for the
// quota charge.
func (e *Engine) prepareBorrowing(ctx context.Context, constraints []*allocation.ResourceConstraints,
	requests []allocation.ModelScalingRequest, scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	now time.Time) []*allocation.ResourceConstraints {
	st := &e.borrowing
	st.cycle, st.gated, st.cohortsByNS, st.credit, st.reclaimCredit = allocation.BorrowCycle{}, nil, nil, nil, nil
	st.gatedCycle, st.wakeYield = allocation.BorrowCycle{}, nil
	if st.inFlight == nil {
		st.inFlight = allocation.DefaultBorrowInFlight
	}
	st.inFlight.ObserveAt(observeVariants(requests), now)
	if !allocation.HasBorrowing(constraints) {
		// The pods at Kueue's admission gate are listed in every Kueue-backed
		// namespace all the same: the quota charge reads them from the record
		// (KU1), so a replica Kueue keeps gated does not cancel what Kueue
		// reserved for others.
		var gated map[string]map[string]int
		var admitted []allocation.AdmittedGPUs
		kueueBacked := len(allocation.KueueNamespaces(constraints)) > 0
		if kueueBacked {
			gated, admitted, _ = e.gatedGPUs(ctx, constraints, now)
		}
		st.inFlight.SetCycle(gated, admitted, nil, now)
		e.idlePreemptions(ctx, requests, scaleTargets, now)
		if kueueBacked {
			e.governGated(ctx, constraints, requests, now)
		}
		return constraints
	}
	physical := e.Config != nil && e.Config.HasPhysicalLimiter()
	e.notePhysicalSkipped(ctx, physical && !slices.ContainsFunc(constraints, func(c *allocation.ResourceConstraints) bool {
		return c != nil && c.ProviderName == allocation.PhysicalLimiterName
	}))
	gated, admitted, unreadable := e.gatedGPUs(ctx, constraints, now)
	st.gated = gated
	u := st.inFlight.Unreserved(now)
	u.Gated, u.Admitted = gated, admitted
	constraints = allocation.DeductUnreservedAt(constraints, u)
	// What earlier cycles and wakes grew into by Kueue's reclaim is not
	// reclaimable again until a read shows it (N1) -- except to the variant
	// that grew: the pass re-places its replicas whose pods are not created
	// yet on its own entry (reclaimCredit, NEW-1), a woken variant's too.
	constraints = allocation.DeductReclaimed(constraints, u.InFlight)
	st.reclaimCredit = st.inFlight.Reclaims()
	// A wake's reclaim takes borrowed GPUs from this controller's own
	// borrowers as a pass's does: this cycle's greedy pass yields for it,
	// and recordBorrowDraws reports it yielded once the pass has run.
	st.wakeYield = st.inFlight.PendingWakeReclaims(now)
	// A namespace whose gated pods could not be listed may hold any number of
	// them: its cohorts borrow nothing new this cycle (G3).
	unreadableCohorts := cohortsOf(constraints, unreadable)
	constraints = allocation.BlockCohorts(constraints, unreadableCohorts)
	// What only the variants' pod templates show: a flavor not pinned where
	// Kueue could pick another, a co-resource request not stated (B1).
	constraints = allocation.WithholdByVariants(constraints, requests)
	e.warnBorrowNamespacesSplit(ctx, constraints)
	constraints = e.governBorrowing(ctx, constraints, requests, scaleTargets, now)
	blocked := st.governor.BlockedCohorts(now)
	for c := range unreadableCohorts {
		blocked[c] = true
	}
	// The wake deducts what this cycle deducted, and closes what it closed.
	st.inFlight.SetCycle(gated, admitted, blocked, now)
	// A variant's own draw in flight is its to re-place (A4), on a cohort
	// still open to borrowing.
	for _, d := range st.inFlight.Entries() {
		if !blocked[d.Cohort] {
			st.credit = append(st.credit, d)
		}
	}
	return constraints
}

// cohortsOf is the cohorts constraints map the namespaces to.
func cohortsOf(constraints []*allocation.ResourceConstraints, namespaces []string) map[string]bool {
	if len(namespaces) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for _, ns := range namespaces {
			for _, co := range c.NamespaceCohorts[ns] {
				out[co] = true
			}
		}
	}
	return out
}

// notePhysicalSkipped records whether this cycle's declared physical limiter
// was skipped (its provider could not answer, or node read is denied), logging
// when that changes. A namespace counts borrowing on the physical limiter as
// its GPU bound; with the limiter skipped nothing bounds what it borrows by the
// GPUs that exist, so no new borrowing starts this cycle (borrowBlocked). The
// quota bounds still apply, what is held stays, and releases still run: the
// skipped provider costs the borrowing that relied on it, not the other bounds.
func (e *Engine) notePhysicalSkipped(ctx context.Context, skipped bool) {
	st := &e.borrowing
	if skipped != st.physicalSkipped {
		if skipped {
			ctrl.LoggerFrom(ctx).Info("Kueue borrowing paused: the physical limiter is skipped this cycle, so no GPU bound covers borrowed replicas; guaranteed growth continues")
		} else {
			ctrl.LoggerFrom(ctx).Info("Kueue borrowing resumed: the physical limiter bounds again")
		}
	}
	st.physicalSkipped = skipped
}

// warnBorrowNamespacesSplit logs, when it changes, that two quota entries
// count borrowing over one cohort and only one of them names borrowNamespaces
// (B13): the pilot list of one does not hold back the other, so the pilot is
// not one. A warning only; nothing is rejected or withheld.
func (e *Engine) warnBorrowNamespacesSplit(ctx context.Context, constraints []*allocation.ResourceConstraints) {
	split := allocation.BorrowNamespacesSplit(constraints)
	key := fmt.Sprint(split)
	if key == e.borrowing.splitWarned {
		return
	}
	e.borrowing.splitWarned = key
	if len(split) == 0 {
		return
	}
	for _, s := range split {
		ctrl.LoggerFrom(ctx).Info("Warning: two quota entries count Kueue borrowing in one cohort and only one sets kueue.borrowNamespaces; the other borrows for all its namespaces (borrow-namespaces-split)",
			"reason", allocation.BorrowNamespacesSplitReason, "cohort", s.Cohort, "withList", s.WithList, "withoutList", s.WithoutList)
	}
}

// recordBorrowDraws keeps what the optimizer borrowed this cycle as in flight,
// and returns it: the pass's draws less those of the models the utilization
// share planned -- it overrides their targets, so what the pass ordered for
// them is not spent (W9) -- each cut to its variant's decided target. The
// draws already in flight are cut to the decided targets too (Trim): a
// target of 0 the engine decided is known, unlike a desired count read back
// as 0 (W6). The replicas the pass grew into nominal Kueue reclaims from
// borrowers are recorded beside them, the same way, and not returned: they
// borrow nothing (N1).
func (e *Engine) recordBorrowDraws(optimizer allocation.ScalingOptimizer, decisions []domain.VariantDecision, now time.Time) []allocation.BorrowDraw {
	e.borrowing.passFlavors = nil
	g, ok := optimizer.(*allocation.GreedyByScoreOptimizer)
	if !ok || e.borrowing.inFlight == nil {
		return nil
	}
	e.borrowing.passFlavors = g.PlacedFlavors()
	// The wakes' reclaims the pass yielded for are yielded for once.
	e.borrowing.inFlight.YieldedWakeReclaims(g.WakeReclaimsYielded())
	targets := decidedTargets(decisions)
	keep := func(in []allocation.BorrowDraw) []allocation.BorrowDraw {
		var out []allocation.BorrowDraw
		for _, d := range in {
			// A passive model draws nothing: its would-be growth is never ordered.
			if e.cyclePlanned[utils.GetNamespacedKey(d.Namespace, d.ModelID)] || e.modelPassive(d.Namespace, d.ModelID) {
				continue
			}
			if t, ok := targets[d.Namespace+"/"+d.Variant]; ok {
				var left bool
				if d, left = d.TrimmedTo(t); !left {
					continue
				}
			}
			out = append(out, d)
		}
		return out
	}
	draws := keep(g.BorrowDraws())
	e.borrowing.inFlight.Trim(targets)
	e.borrowing.inFlight.RecordAt(slices.Concat(draws, keep(g.ReclaimDraws())), now)
	return draws
}

// decidedTargets is each decision's target, keyed "namespace/variant".
func decidedTargets(decisions []domain.VariantDecision) map[string]int {
	out := make(map[string]int, len(decisions))
	for _, d := range decisions {
		out[d.Namespace+"/"+d.VariantName] = d.TargetReplicas
	}
	return out
}

// borrowedFlavors is, per "namespace/variant", the Kueue flavor placement the
// in-flight node ledger holds the variant's holes on (admitInFlight): the one
// this cycle's pass placed its replicas on, guaranteed or borrowed (W5), else
// that of its borrowed growth still in flight or lagging -- so a hole stays
// on its flavor while a read has not shown the replica, also when later
// growth names none (W8). The ledger keeps one flavor per variant: a variant
// drawing on several is held on the one it drew the most GPUs from, the first
// in the record's order on a tie.
func (e *Engine) borrowedFlavors(now time.Time) map[string]*kueue.FlavorPlacement {
	entries := e.borrowing.inFlight.Unreserved(now).InFlight
	if len(entries) == 0 && len(e.borrowing.passFlavors) == 0 {
		return nil
	}
	out := map[string]*kueue.FlavorPlacement{}
	most := map[string]int{}
	for _, d := range entries {
		if d.Placement == nil {
			continue
		}
		key := d.Namespace + "/" + d.Variant
		if _, seen := out[key]; !seen || d.GPUs > most[key] {
			out[key], most[key] = d.Placement, d.GPUs
		}
	}
	maps.Copy(out, e.borrowing.passFlavors)
	return out
}

// observeVariants is each variant's replica counts, keyed "namespace/variant".
func observeVariants(requests []allocation.ModelScalingRequest) map[string]allocation.VariantObservation {
	out := map[string]allocation.VariantObservation{}
	for _, req := range requests {
		for _, s := range req.VariantStates {
			out[req.Namespace+"/"+s.VariantName] = allocation.VariantObservation{Current: s.CurrentReplicas, Desired: s.DesiredReplicas}
		}
	}
	return out
}

// reservedGated is, of the GPU pods at Kueue's admission gate in ns, those
// whose Workload Kueue has already reserved quota for (kueue.ReservedPods).
// A failed Workload read is logged once per namespace until one succeeds, and
// reads every gated pod as unreserved.
func (e *Engine) reservedGated(ctx context.Context, ns string, pods []corev1.Pod) map[types.UID]bool {
	var gated []*corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp == nil && kueue.PodGated(p) && resources.PodGPURequests(p) > 0 {
			gated = append(gated, p)
		}
	}
	if len(gated) == 0 {
		return nil
	}
	st := &e.borrowing
	if st.reservedErrLogged == nil {
		st.reservedErrLogged = map[string]bool{}
	}
	reserved, err := kueue.ReservedPods(ctx, kueue.WorkloadsVia(e.client), ns, gated)
	if err != nil {
		if !st.reservedErrLogged[ns] {
			st.reservedErrLogged[ns] = true
			ctrl.LoggerFrom(ctx).Error(err, "Cannot read Kueue Workloads; every pod at Kueue's admission gate reads as waiting for quota",
				"namespace", ns)
		}
		return nil
	}
	delete(st.reservedErrLogged, ns)
	return reserved
}

// gatedPod is a managed pod seen at Kueue's admission gate.
type gatedPod struct {
	namespace, accelerator string
	gpus                   int
	// admittedAt is when the pod was first seen past the gate; zero while
	// gated.
	admittedAt time.Time
}

// gatedGPUs is, per namespace some borrowing constraint maps to a cohort, and
// per namespace a Kueue-backed constraint carries figures for
// (allocation.KueueNamespaces), the GPUs of pods held at Kueue's admission
// gate, per the accelerator their node selector names ("" when none). They
// hold no reservation yet, so the cohort's free pool still counts what Kueue
// is about to admit them onto, and the queue's charge does not count them as
// reserved (KU1). Read from the manager's cached pod informer: one list per
// such namespace, no API call.
//
// admitted is the pods seen gated and past the gate since: Kueue reserved them,
// but a read taken before then does not show it, so they stay deducted from a
// chunk read before their admission was seen (W3), for at most InFlightTTL.
//
// unreadable is the namespaces whose pods could not be listed. Logged once
// per namespace until a list succeeds; the caller closes their cohorts to new
// borrowing this cycle (G3), since any number of gated pods may hide there.
func (e *Engine) gatedGPUs(ctx context.Context, constraints []*allocation.ResourceConstraints,
	now time.Time) (gated map[string]map[string]int, admitted []allocation.AdmittedGPUs, unreadable []string) {
	if e.client == nil {
		return nil, nil, nil
	}
	st := &e.borrowing
	if st.gatedSeen == nil {
		st.gatedSeen = map[types.UID]gatedPod{}
	}
	if st.gatedErrLogged == nil {
		st.gatedErrLogged = map[string]bool{}
	}
	namespaces := map[string]bool{}
	inCohort := map[string]bool{}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns := range c.NamespaceCohorts {
			namespaces[ns] = true
			inCohort[ns] = true
		}
	}
	// Every Kueue-backed namespace, borrowing counted or not: its charge
	// reads its gated pods (KU1), and the placement timeout releases one
	// gated for good (uncountedHeldPast).
	for _, ns := range allocation.KueueNamespaces(constraints) {
		namespaces[ns] = true
	}
	gated = map[string]map[string]int{}
	listed := map[string]bool{}
	present := map[types.UID]bool{}
	for _, ns := range slices.Sorted(maps.Keys(namespaces)) {
		var pods corev1.PodList
		if err := e.client.List(ctx, &pods, client.InNamespace(ns)); err != nil {
			unreadable = append(unreadable, ns)
			if !st.gatedErrLogged[ns] {
				st.gatedErrLogged[ns] = true
				msg := "Cannot list the pods at Kueue's admission gate; nothing new is borrowed in the namespace's cohorts until a list succeeds"
				if !inCohort[ns] {
					msg = "Cannot list the pods at Kueue's admission gate; the namespace's quota charge counts them as admitted until a list succeeds"
				}
				ctrl.LoggerFrom(ctx).Error(err, msg, "namespace", ns)
			}
			continue
		}
		delete(st.gatedErrLogged, ns)
		listed[ns] = true
		reserved := e.reservedGated(ctx, ns, pods.Items)
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.DeletionTimestamp != nil {
				continue
			}
			g := resources.PodGPURequests(p)
			if g <= 0 {
				continue
			}
			present[p.UID] = true
			acc := accelerator.ProductFromNodeSelector(p.Spec.NodeSelector)
			// A gated pod whose Workload Kueue already reserved quota for
			// (waiting on an admission check, a ProvisioningRequest) is in
			// Kueue's reservation: it is counted as admitted, not gated.
			if kueue.PodGated(p) && !reserved[p.UID] {
				if gated[ns] == nil {
					gated[ns] = map[string]int{}
				}
				gated[ns][acc] += g
				st.gatedSeen[p.UID] = gatedPod{namespace: ns, accelerator: acc, gpus: g}
				continue
			}
			if s, seen := st.gatedSeen[p.UID]; seen && s.admittedAt.IsZero() {
				s.admittedAt = now
				st.gatedSeen[p.UID] = s
			}
		}
	}
	for uid, s := range st.gatedSeen {
		switch {
		case !namespaces[s.namespace], listed[s.namespace] && !present[uid]:
			delete(st.gatedSeen, uid) // gone, or no longer read
		case !s.admittedAt.IsZero() && now.Sub(s.admittedAt) > allocation.InFlightTTL:
			delete(st.gatedSeen, uid)
		case !s.admittedAt.IsZero():
			admitted = append(admitted, allocation.AdmittedGPUs{Namespace: s.namespace, Accelerator: s.accelerator,
				GPUs: s.gpus, At: s.admittedAt})
		}
	}
	slices.SortFunc(admitted, func(a, b allocation.AdmittedGPUs) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Accelerator, b.Accelerator),
			a.At.Compare(b.At), cmp.Compare(a.GPUs, b.GPUs))
	})
	return gated, admitted, unreadable
}
