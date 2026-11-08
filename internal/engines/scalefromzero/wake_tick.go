package scalefromzero

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// A wake is judged against fresh constraints: the usage the cluster reports
// now. A wake admitted a moment ago is not in that usage yet -- its pods do not
// exist, or are Pending and hold no node -- so within one tick each wake's
// demand is carried forward by hand: debited from the constraints of every
// later wake in the tick (tickWakes), and from the tick's node view
// (tickNodes). Models are decided concurrently, so the check and the debit are
// one step under wakeMu (decideWake).
//
// Across ticks, the in-flight ledger (allocation.DefaultInFlight) carries a
// wake that was fitted to nodes: its node hole stays held, from both engines,
// until its pods hold a node (scheduled, or nominated for one after a
// preemption) or allocation.InFlightTTL passes. What a wake takes of a Kueue
// cohort's free pool, and what Kueue reclaims for it from borrowers, is held
// in the shared borrow record (allocation.DefaultBorrowInFlight, N3) until
// its pods exist, for both engines. Its other pool demand is not carried
// across ticks: a quota counts it once the steady-state engine next
// publishes what the scaling manager's variants hold, and the physical pool once its pods
// land. Under a physical limiter the held hole refuses the next wake anyway;
// under a quota alone that window is the limit of the check.

// tickNodeView is a tick's node view, built on first use.
type tickNodeView struct {
	built bool
	view  *allocation.NodeView
	// skipped is the reason last reported for not fitting wakes to nodes
	// ("" when they were fitted); it outlives the tick so a standing reason
	// is reported once.
	skipped string
}

// beginTick forgets the previous tick's wakes and node view.
func (e *Engine) beginTick() {
	e.wakeMu.Lock()
	defer e.wakeMu.Unlock()
	e.tickWakes, e.tickReclaimed = nil, nil
	e.tickNodes.built, e.tickNodes.view = false, nil
}

// debitReclaimed records what a wake took of its namespace's reclaimable
// nominal. Caller holds wakeMu.
func (e *Engine) debitReclaimed(namespace string, took map[string]allocation.Reclaim) {
	if len(took) == 0 {
		return
	}
	if e.tickReclaimed == nil {
		e.tickReclaimed = map[string]map[string]allocation.Reclaim{}
	}
	if e.tickReclaimed[namespace] == nil {
		e.tickReclaimed[namespace] = map[string]allocation.Reclaim{}
	}
	for t, n := range took {
		prev := e.tickReclaimed[namespace][t]
		e.tickReclaimed[namespace][t] = allocation.Reclaim{After: prev.After + n.After, Kept: prev.Kept + n.Kept, GPUs: prev.GPUs + n.GPUs}
	}
}

// reclaimableLeft is reclaimable less what this tick's earlier wakes of
// namespace took of it. Caller holds wakeMu.
func (e *Engine) reclaimableLeft(namespace string, reclaimable map[string]allocation.Reclaim) map[string]allocation.Reclaim {
	took := e.tickReclaimed[namespace]
	if len(took) == 0 || len(reclaimable) == 0 {
		return reclaimable
	}
	out := make(map[string]allocation.Reclaim, len(reclaimable))
	for t, r := range reclaimable {
		after := max(0, r.After-took[t].After)
		out[t] = allocation.Reclaim{After: after, Kept: min(after, max(0, r.Kept-took[t].Kept)), GPUs: max(0, r.GPUs-took[t].GPUs)}
	}
	return out
}

// decideWake judges a model's wake and, when it selects a set, records what
// the set takes, as one step: see the comment at the top of this file.
func (e *Engine) decideWake(ctx context.Context, group modelGroup, candidates []Candidate, covered coverage) (placement, []Candidate, SelectionOutcome) {
	own := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		own[c.VariantName] = true
	}
	place := e.placementForModel(ctx, group.namespace, own)
	e.wakeMu.Lock()
	defer e.wakeMu.Unlock()
	if place.snapshotTakenAt.IsZero() && place.constraints != nil {
		place.constraints = allocation.WithholdPromised(place.constraints, e.tickWakes)
		if e.awaitInFlightRestore(ctx, place.constraints, time.Now()) {
			return place, nil, OutcomeAwaitingInFlight
		}
		place.nodes = e.tickNodeViewFor(ctx, place.constraints)
	}
	place.reclaimable = e.reclaimableLeft(group.namespace, place.reclaimable)
	if allocation.DefaultBorrowCooldowns.CoolingDown(group.namespace, group.modelID, time.Now()) {
		// The governor's safety net released a replica of this model that
		// was ordered without a node hole and never placed, and its cooldown
		// runs: the wake needs a real hole until it ends, as the steady-state
		// pass's growth does (GreedyByScoreOptimizer.ReclaimBlocked). Without
		// it the released wake was woken again at once, and Kueue preempted a
		// peer for it every placement timeout (AR1).
		place.reclaimable = allocation.RequireHoles(place.reclaimable)
	}
	selected, outcome := e.selectOrClaim(ctx, group, candidates, covered, place)
	if len(selected) > 0 {
		e.recordWake(ctx, group, place, selected)
	}
	return place, selected, outcome
}

// admitInFlight holds each woken replica's node hole in the in-flight ledger
// until its pods hold a node, on its queue's flavor when flavorOf names one --
// then for the borrowing placement timeout when that is longer than the
// ledger's TTL, as the steady-state engine holds a replica on a flavor.
// Caller holds wakeMu.
func (e *Engine) admitInFlight(namespace string, selected []Candidate, flavorOf func(string) *kueue.FlavorPlacement) {
	now := time.Now()
	var placementTimeout time.Duration
	if e.config != nil {
		placementTimeout = e.config.KueueBorrowTimings().PlacementTimeout
	}
	for _, c := range selected {
		if !constants.IsAcceleratorResolved(c.Accelerator) {
			continue
		}
		var placed func() (allocation.Placed, bool)
		if cl, acc := e.client, c.target; cl != nil && acc != nil {
			placed = func() (allocation.Placed, bool) {
				whole, partial, ok := variantmeta.PlacedGroups(context.Background(), cl, namespace, acc)
				return allocation.Placed{Whole: whole, Partial: partial}, ok
			}
		}
		key := namespace + "/" + c.VariantName
		shape := shapeOf(namespace, c, flavorOf)
		var ttl time.Duration
		if shape.Flavor != nil {
			ttl = placementTimeout
		}
		allocation.DefaultInFlight.Commit(nil, []allocation.InFlightAdmission{{Key: key, Shape: shape, Want: 1,
			Placed: placed, Target: allocation.ScaleTargetRefOf(c.target), TTL: ttl}}, now)
	}
}

// wakeReplicas is one replica of each selected candidate whose accelerator is
// resolved, in order, as allocation.WakeDraws reads them.
func wakeReplicas(selected []Candidate) []allocation.WakeReplica {
	out := make([]allocation.WakeReplica, 0, len(selected))
	for _, c := range selected {
		if !constants.IsAcceleratorResolved(c.Accelerator) {
			continue
		}
		out = append(out, allocation.WakeReplica{Variant: c.VariantName, Accelerator: c.Accelerator, GPUs: c.GPUsPerReplica})
	}
	return out
}

// debitTick records a wake's demand against this tick's later wakes. Caller
// holds wakeMu.
func (e *Engine) debitTick(namespace string, demand map[string]int) {
	if len(demand) == 0 {
		return
	}
	if e.tickWakes == nil {
		e.tickWakes = map[string]map[string]int{}
	}
	if e.tickWakes[namespace] == nil {
		e.tickWakes[namespace] = map[string]int{}
	}
	for acc, gpus := range demand {
		e.tickWakes[namespace][acc] += gpus
	}
}

// inFlightRestoreWait bounds how long wakes wait for the in-flight holds to be
// read back after this replica becomes leader (awaitInFlightRestore). The
// steady-state engine reads them before its first cycle, within milliseconds
// of this engine's first tick; a read that fails is tried again each cycle
// (every 15 seconds by default). A ledger that cannot be read for this long is given up on
// for wakes: a parked model kept from waking costs more than the holes the
// holds would keep, which expire within minutes anyway.
const inFlightRestoreWait = time.Minute

// awaitInFlightRestore reports whether a wake fitted to nodes under
// constraints must wait for the in-flight holds a restart carries
// (allocation.InFlightLedger.RestorePending): the node view built now would
// not hold them, and the wake could take the hole of a replica ordered
// before the restart -- Unschedulable, or a LeaderWorkerSet group waiting
// for its workers. A wake the physical limiter does not bound reads no hold
// and does not wait; one funded by a share claim under it waits too, since
// the check comes before the claim. Past inFlightRestoreWait from when this
// replica became leader (StartOptimizeLoop restarts the clock), not from the
// first wake, wakes go without the holds, logged once, for good. Caller
// holds wakeMu.
func (e *Engine) awaitInFlightRestore(ctx context.Context, constraints []*allocation.ResourceConstraints, now time.Time) bool {
	if e.inFlightGaveUp || !allocation.PhysicalBounds(constraints) {
		return false
	}
	since, pending := allocation.DefaultInFlight.RestorePendingSince()
	if !pending {
		return false
	}
	if now.Sub(since) < inFlightRestoreWait {
		if !e.inFlightWaitLogged {
			e.inFlightWaitLogged = true
			log.FromContext(ctx).Info("Scale-from-zero: wakes wait for the in-flight node holds to be read back after the start",
				"maxWait", inFlightRestoreWait.String(), "left", (inFlightRestoreWait - now.Sub(since)).String())
		}
		return true
	}
	e.inFlightGaveUp = true
	log.FromContext(ctx).Info("Warning: the in-flight node holds were not read back in time; wakes go without them",
		"waited", inFlightRestoreWait.String())
	return false
}

// tickNodeViewFor is the tick's node view when the physical limiter bounds a
// wake checked against constraints, built on first use; nil otherwise. A
// physical limiter skipped for this wake (no node read, usage unobserved) is
// not among the constraints, and its node fit is skipped with it. Caller holds
// wakeMu.
func (e *Engine) tickNodeViewFor(ctx context.Context, constraints []*allocation.ResourceConstraints) *allocation.NodeView {
	if !allocation.PhysicalBounds(constraints) {
		return nil
	}
	if !e.tickNodes.built {
		now := time.Now()
		view, reason := allocation.NodeViewFor(constraints, nil, now)
		// A node freed for a share receiver is not free to a wake either (the
		// constraints already withhold the promise from the pools).
		// Each promise is held on the node its hole is on.
		allocation.ReservePromised(view, decision.LatestSharePromisedHoles(now))
		e.tickNodes.built, e.tickNodes.view = true, view.WithLayoutLog(&e.layouts)
		if reason != e.tickNodes.skipped {
			if reason != "" {
				log.FromContext(ctx).Info("Scale-from-zero: wakes are not fitted to nodes", "reason", reason)
			} else {
				log.FromContext(ctx).Info("Scale-from-zero: wakes are fitted to nodes again")
			}
			e.tickNodes.skipped = reason
		}
	}
	return e.tickNodes.view
}
