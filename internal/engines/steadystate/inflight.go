package steadystate

import (
	"context"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// createdReplicas is each variant's replicas whose pods exist, keyed
// "namespace/variant" (allocation.InFlightLedger.ReleaseUncreated).
func createdReplicas(requests []allocation.ModelScalingRequest) map[string]int {
	out := map[string]int{}
	for _, req := range requests {
		for _, st := range req.VariantStates {
			out[utils.GetNamespacedKey(req.Namespace, st.VariantName)] = st.CurrentReplicas
		}
	}
	return out
}

// admitInFlight records this cycle's targets in the in-flight ledger
// (allocation.DefaultInFlight), when the cycle was fitted to nodes: each
// variant is to have its target's replicas holding a node, and until its pods
// do, the holes they were ordered into are held from every later decision of
// either engine. A variant at or below what it has placed ends its entry.
//
// The models the utilization share plans are left out: a fill's hole is held
// as a promise (allocation.ReservePromised) until its receiver's pods land.
//
// A variant the pass placed on a Kueue flavor -- its queue's, within
// guaranteed, or a borrowed chunk's -- or with borrowed growth in flight
// (allocation.BorrowInFlight) has its holes held on that flavor's nodes, where
// the placer fitted them (borrowedFlavors). Every hole is held with its pod
// templates' nodeSelector and required node affinity. The ledger holds the node;
// the shared pool's GPUs are held apart, by the borrowed draw until the pods
// exist and by the gated-pod deduction while Kueue's admission gate holds them
// (prepareBorrowing): one is a hole on a node, the other GPUs of a cohort's
// pool, so neither is counted twice.
//
// The pass's placements were admitted provisionally right after the
// optimizer (admitPassInFlight); this replaces that admission with the
// cycle's final one, as one step (allocation.InFlightLedger.Commit).
func (e *Engine) admitInFlight(requests []allocation.ModelScalingRequest, decisions []domain.VariantDecision,
	planned map[string]bool, scaleTargets map[string]scaletarget.ScaleTargetAccessor, now time.Time) {
	adms := e.inFlightAdmissions(requests, decisions, planned, scaleTargets, now)
	allocation.DefaultInFlight.Commit(e.inFlightMark, adms, now)
	e.inFlightMark = nil
	// A restored hold this cycle does not admit ends when its variant is gone
	// or the cycle decided it without admitting it (not asking, passive,
	// planned by the share). One of a variant the cycle enumerated and did
	// not decide -- a model skipped, its metrics missing -- keeps its hold
	// until a cycle admits it or it expires: one skipped cycle does not cost
	// an Unschedulable replica its hole.
	keep := make(map[string]bool, len(adms)+len(e.cycleVariants))
	for k := range e.cycleVariants {
		keep[k] = true
	}
	// Decided means a resolved accelerator and a state the cycle read: a
	// decision without either says nothing about the variant's replicas.
	stated := createdReplicas(requests)
	for i := range decisions {
		key := utils.GetNamespacedKey(decisions[i].Namespace, decisions[i].VariantName)
		if _, ok := stated[key]; ok && constants.IsAcceleratorResolved(decisions[i].AcceleratorName) {
			delete(keep, key)
		}
	}
	for _, a := range adms {
		keep[a.Key] = true
	}
	allocation.DefaultInFlight.DropRestoredExcept(keep)
}

// admitPassInFlight admits the pass's placements to the in-flight ledger
// right after the optimizer, provisionally: until admitInFlight commits the
// cycle's final targets, the utilization share, scale-to-zero and the
// borrowing releases run, and a wake in that window saw the nodes the pass
// chose free and took one (WN5). A provisional admission an earlier cycle
// left uncommitted is set back first.
func (e *Engine) admitPassInFlight(requests []allocation.ModelScalingRequest, decisions []domain.VariantDecision,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor, now time.Time) {
	if e.inFlightMark != nil {
		allocation.DefaultInFlight.Commit(e.inFlightMark, nil, now)
	}
	e.inFlightMark = allocation.DefaultInFlight.Provisional(e.inFlightAdmissions(requests, decisions, nil, scaleTargets, now), now)
}

// inFlightAdmissions is what admitInFlight admits: per decided variant of a
// model planned does not name, its target's replicas of its shape. A variant
// whose target does not grow it past its current replicas is admitted only
// to an entry it already has -- growth ordered earlier whose pods hold no
// node yet -- so a pod of it at Kueue's gate after a preemption holds no
// hole a growing peer could use. The first cycle after a restart, a variant
// none of whose replicas is unplaced starts its entry all the same: the
// persisted ledger may not carry it, and its Pending pods' nodes stay held
// (allocation.InFlightAdmission.ExistingOnly). Each admission names the
// variant's scale target, persisted so a restart counts its pods.
func (e *Engine) inFlightAdmissions(requests []allocation.ModelScalingRequest, decisions []domain.VariantDecision,
	planned map[string]bool, scaleTargets map[string]scaletarget.ScaleTargetAccessor, now time.Time) []allocation.InFlightAdmission {
	var out []allocation.InFlightAdmission
	states := map[string]domain.VariantReplicaState{}
	flavors := e.borrowedFlavors(now)
	placementTimeout := e.borrowSettings().PlacementTimeout
	for _, req := range requests {
		// A passive model orders nothing: a hole held for its would-be growth
		// would be held from every model that can grow.
		if planned[utils.GetNamespacedKey(req.Namespace, req.ModelID)] || e.modelPassive(req.Namespace, req.ModelID) {
			continue
		}
		for _, st := range req.VariantStates {
			states[utils.GetNamespacedKey(req.Namespace, st.VariantName)] = st
		}
	}
	for i := range decisions {
		d := &decisions[i]
		key := utils.GetNamespacedKey(d.Namespace, d.VariantName)
		st, ok := states[key]
		if !ok || !constants.IsAcceleratorResolved(d.AcceleratorName) {
			continue
		}
		// What holds a node now: the filled replicas when the pods were
		// listed, else every current replica (the growth alone is then held).
		placedNow := st.CurrentReplicas
		if st.HeldKnown {
			placedNow = st.FilledReplicas
		}
		var placed func() (allocation.Placed, bool)
		if acc, ns := scaleTargets[key], d.Namespace; acc != nil && e.client != nil {
			c := e.client
			placed = func() (allocation.Placed, bool) {
				whole, partial, ok := variantmeta.PlacedGroups(context.Background(), c, ns, acc)
				return allocation.Placed{Whole: whole, Partial: partial}, ok
			}
		}
		// Every hole keeps its pod templates' placement, a guaranteed or
		// unbounded one too: the pass fitted it with them (WN3).
		shape := allocation.ReplicaShape{Variant: key, Accelerator: d.AcceleratorName,
			PodGPUs: st.PodGPUs, GPUs: max(st.GPUsPerReplica, 1), Tolerations: st.Tolerations, DomainKey: st.TopologyKey,
			Placements: st.Placements}
		var ttl time.Duration
		if f, ok := flavors[key]; ok {
			// On a Kueue flavor the governor waits the placement timeout
			// before it releases an unplaced replica -- three times it while
			// a cluster-autoscaler scale-up is pending for the variant
			// (allocation.BorrowCycle.ScaleUpPending): its hole is held as
			// long, when that is longer than the ledger's own TTL.
			shape.Flavor, ttl = f, placementTimeout
			if e.borrowing.cycle.ScaleUpPending[key] {
				ttl *= 3
			}
		}
		out = append(out, allocation.InFlightAdmission{Key: key, Shape: shape, Want: d.TargetReplicas,
			PlacedNow: placedNow, Placed: placed, Target: allocation.ScaleTargetRefOf(scaleTargets[key]), TTL: ttl,
			ExistingOnly: d.TargetReplicas <= st.CurrentReplicas, Unplaced: st.UnplacedReplicas > 0})
	}
	return out
}
