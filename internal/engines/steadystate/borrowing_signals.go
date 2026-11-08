package steadystate

// What on-call sees of the GPU budget and of cohort borrowing
// (the Kueue borrowing proposal, PR #161, Part C): the
// figures the cycle used (metrics and the controller-status limiterBudgets
// key), the borrowing reasons on llmsm_model_scaling_blocked, unplaced pods,
// waiting lenders, and an Event per release. Everything here reports what the
// cycle decided; nothing decides.

import (
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/controllerstatus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// publishBorrowSignals publishes the cycle's budgets and borrowing signals.
// Called every cycle that reaches the optimizer, after the releases; with no
// limiter, or no borrowing counted, it clears what an earlier cycle set.
// constraints are the ones the optimizer ran on.
func (e *Engine) publishBorrowSignals(constraints []*allocation.ResourceConstraints,
	requests []allocation.ModelScalingRequest, decisions []domain.VariantDecision, now time.Time) {
	st := &e.borrowing
	borrowing := st.governor != nil && !st.cycle.Now.IsZero()
	var blocked map[string]bool
	if borrowing {
		blocked = st.governor.BlockedCohorts(now)
	}

	budgets := allocation.LimiterBudgets(constraints, blocked)
	if borrowing {
		allocation.MarkPreemptionsUnreadable(budgets, st.cycle.PreemptionUnknown)
	}
	qb := make([]metrics.QuotaBudget, 0, len(budgets))
	sb := make([]controllerstatus.LimiterBudget, 0, len(budgets))
	for _, b := range budgets {
		qb = append(qb, metrics.QuotaBudget{Limiter: b.Limiter, Namespace: b.Namespace, Accelerator: b.Accelerator,
			Guaranteed: b.Guaranteed, Borrowable: b.Borrowable, Reason: b.Reason})
		sb = append(sb, controllerstatus.LimiterBudget{Limiter: b.Limiter, Namespace: b.Namespace,
			Accelerator: b.Accelerator, Guaranteed: b.Guaranteed, Borrowable: b.Borrowable, Used: b.Used, Reason: b.Reason})
	}
	metrics.PublishQuotaBudgets(qb)
	st.budgets = controllerstatus.StampLimiterBudgets(st.budgets, sb, now)
	e.Status.SetLimiterBudgets(st.budgets)

	for _, req := range requests {
		var active []string
		if borrowing {
			active = e.borrowReasons(req, constraints, decisions, now)
		}
		metrics.SetModelScalingBlockedReasons(req.Namespace, req.ModelID, constants.ScalingBlockedReasonsBorrowing, active)
	}

	var unplaced []metrics.UnplacedVariant
	var lenders []metrics.WaitingLender
	var held []metrics.BorrowedGPUs
	if borrowing {
		unplaced = unplacedPastTimeout(constraints, requests, e.borrowSettings().PlacementTimeout, now)
		lenders = waitingLenders(st.governor.WaitingLenders(now),
			allocation.BorrowingCohorts(constraints, st.gated), now)
		for _, b := range allocation.BorrowedByPool(constraints, st.gated) {
			held = append(held, metrics.BorrowedGPUs{Cohort: b.Cohort, Flavor: b.Flavor, GPUs: b.GPUs})
		}
	}
	metrics.PublishKueueUnplacedPods(unplaced)
	metrics.PublishKueueLenderWaiting(lenders)
	metrics.PublishKueueBorrowedGPUs(held)
	// The threshold of LLMSMKueueLenderWaitingPastCooldown: the cooldown in
	// force, published while some entry counts borrowing.
	metrics.PublishKueueBorrowCooldown(e.borrowSettings().Cooldown.Seconds(),
		e.Config != nil && e.Config.KueueCountsBorrowable())
	// What the same alert adds to the cooldown, per cohort whose pool opts
	// in to releaseToNonReclaimingLenders: how long a release there takes to
	// land. The last figures measured are kept through cycles that count no
	// borrowing; every other cohort's threshold stays the cooldown alone.
	on := e.Config != nil && e.Config.KueueReleasesToNonReclaiming()
	latencies := map[string]float64{}
	for cohort, d := range st.releaseLatencies {
		if on && d > 0 {
			latencies[cohort] = d.Seconds()
		}
	}
	metrics.PublishKueueReleaseLatency(latencies)
	// No release to a lender is decided while the persisted ledger is
	// unread: say why, while the option is on.
	unread := ""
	if on {
		unread = st.ledgerUnread
	}
	metrics.PublishBorrowLedgerUnread(unread)
	metrics.PublishKueueNeverRead(kueueNeverRead(constraints))
}

// kueueNeverRead is the providers whose Kueue source has never been read
// while their static caps leave a type unlimited, sorted, once each.
func kueueNeverRead(constraints []*allocation.ResourceConstraints) []string {
	var out []string
	for _, c := range constraints {
		if c != nil && c.KueueNeverRead && !slices.Contains(out, c.ProviderName) {
			out = append(out, c.ProviderName)
		}
	}
	slices.Sort(out)
	return out
}

// borrowReasons is the borrowing reasons that hold for one model this cycle.
func (e *Engine) borrowReasons(req allocation.ModelScalingRequest, constraints []*allocation.ResourceConstraints,
	decisions []domain.VariantDecision, now time.Time) []string {
	st := &e.borrowing
	var unplaced, reclaimed, lentOut, released bool
	counted := countsBorrowing(constraints, req.Namespace)
	for _, v := range req.VariantStates {
		s := st.governor.VariantStatus(req.Namespace, v.VariantName)
		unplaced = unplaced || s.TimeoutRelease || (s.Unplaced && counted)
		released = released || s.LenderRelease
	}
	if until, ok := st.reclaimedUntil[req.Namespace+"/"+req.ModelID]; ok && now.Before(until) {
		reclaimed = true
	}
	for i := range decisions {
		d := &decisions[i]
		if d.Namespace == req.Namespace && d.ModelID == req.ModelID && d.WasLimited &&
			nominalLentOut(constraints, req.Namespace, d.AcceleratorName) > 0 {
			lentOut = true
		}
	}
	// A slice, not a map: the reasons come out in this fixed order.
	var out []string
	for _, r := range []struct {
		reason string
		on     bool
	}{
		{constants.ScalingBlockedBorrowUnplaced, unplaced},
		{constants.ScalingBlockedLenderReclaimed, reclaimed},
		{constants.ScalingBlockedNominalLentOut, lentOut},
		{constants.ScalingBlockedReleasedToLender, released},
	} {
		if r.on {
			out = append(out, r.reason)
		}
	}
	return out
}

// countsBorrowing reports whether some constraint counts ns's borrowing.
func countsBorrowing(constraints []*allocation.ResourceConstraints, ns string) bool {
	for _, c := range constraints {
		if c != nil && c.NamespaceBorrow[ns] != nil {
			return true
		}
	}
	return false
}

// nominalLentOut is the nominal of ns's queues serving acc that a peer holds
// and they cannot reclaim, summed over the constraints; acc "" sums every type.
func nominalLentOut(constraints []*allocation.ResourceConstraints, ns, acc string) int {
	n := 0
	for _, c := range constraints {
		if c == nil {
			continue
		}
		perType := c.NamespaceBorrow[ns]
		if acc == "" {
			for _, b := range perType {
				n += b.NominalLentOut
			}
			continue
		}
		if key, ok := accelerator.FindKey(perType, acc); ok {
			n += perType[key].NominalLentOut
		}
	}
	return n
}

// unplacedPastTimeout is, per variant in a namespace that counts borrowing,
// its pods unplaced for longer than the placement timeout.
func unplacedPastTimeout(constraints []*allocation.ResourceConstraints, requests []allocation.ModelScalingRequest,
	timeout time.Duration, now time.Time) []metrics.UnplacedVariant {
	var out []metrics.UnplacedVariant
	for _, req := range requests {
		if !countsBorrowing(constraints, req.Namespace) {
			continue
		}
		for _, s := range req.VariantStates {
			if s.UnplacedReplicas <= 0 || s.UnplacedSince.IsZero() || now.Sub(s.UnplacedSince) <= timeout {
				continue
			}
			pods := len(s.UnplacedPods)
			if pods == 0 {
				pods = s.UnplacedReplicas
			}
			out = append(out, metrics.UnplacedVariant{Namespace: req.Namespace, Variant: s.VariantName, Pods: pods})
		}
	}
	return out
}

// waitingLenders is each waiting lender in a cohort some governed namespace
// borrows in, once per ClusterQueue, with its longest wait.
func waitingLenders(waits []allocation.LenderWait, borrowing map[string]bool, now time.Time) []metrics.WaitingLender {
	type key struct{ cohort, queue string }
	since := map[key]time.Time{}
	var order []key
	for _, w := range waits {
		if !borrowing[w.Cohort] {
			continue
		}
		k := key{w.Cohort, w.Lender}
		prev, seen := since[k]
		if !seen {
			order = append(order, k)
		}
		if !seen || w.Since.Before(prev) {
			since[k] = w.Since
		}
	}
	out := make([]metrics.WaitingLender, 0, len(order))
	for _, k := range order {
		out = append(out, metrics.WaitingLender{Cohort: k.cohort, ClusterQueue: k.queue, Seconds: now.Sub(since[k]).Seconds()})
	}
	return out
}

// preemptionEventWindow is how close two preemptions of one model must be to
// count as one event on llmsm_kueue_preempted_pods_total, as the governor's
// backoff judges one event (allocation sameEvent).
const preemptionEventWindow = time.Minute

// recordReclaim counts a Kueue preemption of a model's pods -- once per
// preempted Workload, by key (preemptionKey), so a LeaderWorkerSet or pod
// group whose pods arrive one by one counts once, and once per model within
// preemptionEventWindow, so a P/D pair counts once too -- and holds the
// lender-reclaimed reason for the cooldown after it. A preemption older than
// the cooldown, one a restart finds again, counts nothing.
func (e *Engine) recordReclaim(modelKey string, w kueue.PreemptedWorkload, now time.Time, key string) {
	cooldown := e.borrowSettings().Cooldown
	at := w.At
	if at.IsZero() || at.After(now) {
		at = now
	}
	if now.Sub(at) > cooldown {
		return
	}
	st := &e.borrowing
	if st.reclaimedUntil == nil {
		st.reclaimedUntil = map[string]time.Time{}
	}
	if until := at.Add(cooldown); until.After(st.reclaimedUntil[modelKey]) {
		st.reclaimedUntil[modelKey] = until
	}
	for k, until := range st.reclaimedUntil {
		if !now.Before(until) {
			delete(st.reclaimedUntil, k)
		}
	}
	if st.preemptCounted == nil {
		st.preemptCounted = map[string]time.Time{}
	}
	for k, seen := range st.preemptCounted {
		if now.Sub(seen) > cooldown {
			delete(st.preemptCounted, k)
		}
	}
	if _, counted := st.preemptCounted[key]; counted && key != "" {
		return
	}
	st.preemptCounted[key] = now
	// One event per model per preemption: the two Workloads of a P/D pair,
	// preempted together, arrive one by one and are one loss (R7).
	if st.preemptEventAt == nil {
		st.preemptEventAt = map[string]time.Time{}
	}
	for k, last := range st.preemptEventAt {
		if now.Sub(last) > cooldown {
			delete(st.preemptEventAt, k)
		}
	}
	if last, ok := st.preemptEventAt[modelKey]; ok && at.Sub(last).Abs() < preemptionEventWindow {
		return
	}
	st.preemptEventAt[modelKey] = at
	ns, model, _ := strings.Cut(modelKey, "/")
	metrics.CountKueuePreemptedPods(ns, model, 1)
}

// borrowReleasedEvent records, on the released variant's scale target, that
// the scaling manager lowered it to give borrowed capacity back, and why. The
// recorder aggregates repeats.
func (e *Engine) borrowReleasedEvent(r allocation.BorrowRelease, acc scaletarget.ScaleTargetAccessor) {
	if r.Cause == allocation.ReleaseLenderWaiting {
		e.shareEvent(acc, corev1.EventTypeNormal, constants.K8SEventBorrowedCapacityReleased,
			fmt.Sprintf("Released borrowed Kueue capacity to ClusterQueue %s: replicas lowered from %d to %d (cause %s). "+
				"It waits for capacity it lent in cohort %s, and Kueue does not reclaim it "+
				"(releaseToNonReclaimingLenders is on); this model borrows nothing new there for the cooldown",
				r.Lender.Lender, r.From, r.To, r.Cause, r.Lender.Cohort))
		return
	}
	if r.AtGate {
		e.shareEvent(acc, corev1.EventTypeWarning, constants.K8SEventBorrowedCapacityReleased,
			fmt.Sprintf("Released replicas held at Kueue's admission gate: replicas lowered from %d to %d (cause %s). "+
				"Kueue did not admit them within the placement timeout: their ClusterQueue has no room for them, "+
				"and their namespace borrows nothing from its cohort", r.From, r.To, r.Cause))
		return
	}
	e.shareEvent(acc, corev1.EventTypeWarning, constants.K8SEventBorrowedCapacityReleased,
		fmt.Sprintf("Released borrowed Kueue capacity: replicas lowered from %d to %d (cause %s). "+
			"Replicas on borrowed GPUs did not place within the placement timeout; this model borrows "+
			"nothing new for the cooldown", r.From, r.To, r.Cause))
}
