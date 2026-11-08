package allocation

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// ReclaimYieldStepName names the decision step that lowers a borrowing
// model's target for a reclaim this controller ordered (yieldReclaimed).
const ReclaimYieldStepName = "kueue-reclaim"

// ReclaimYieldLogMessage is logged when a pass lowers borrowing models'
// targets for a reclaim it ordered; operators search for it.
const ReclaimYieldLogMessage = "Kueue reclaim: a model of this controller grows into nominal its queue lent out; " +
	"the borrowing models here give those replicas back in the same pass"

// yieldReclaimed lowers, in the pass that ordered it, the targets of the
// managed models whose borrowed GPUs a reclaim takes (KU3). A guaranteed
// replica grown into nominal its queue lent out (a reclaim draw's GPUs) is
// admitted by Kueue preempting what peers borrow. When a peer is a namespace
// this controller manages, holding GPUs past its guaranteed figure in the
// same cohort, its replica is the one preempted, the cautious reading: Kueue
// does not say whose borrowing it takes. Left at its target, the preempted
// replica is recreated and waits gated for capacity that is gone, and the
// pass has ordered more than Kueue can admit.
//
// Only a draw's new GPUs count: what it re-places of its own reclaim still in
// flight (BorrowDraw.CreditGPUs) was yielded for in the pass that first
// ordered it, and yielding again would lower the borrower a second time for
// the same replica.
//
// Per cohort and accelerator, the GPUs reclaimed are taken from the own
// borrowers in namespace order, each up to what it holds past guaranteed,
// counting first what the pass already scales it down by; the rest lowers
// its models, in model order, one unit at a time (yieldUnit): a replica of a
// single-role model, a whole pair at the model's role ratio of a P/D one. A
// unit that would give back more than one replica past what is still
// reclaimed is not given: that model is skipped. No variant goes below its
// minReplicas, so a serving model reaches 0 only when its minReplicas is 0. A reclaim from borrowers this controller does not
// manage changes nothing. It returns, per "namespace/variant", the replicas
// it took off; nil when none.
//
// A scale-from-zero wake's reclaim (wakeReclaims) is yielded for the same
// way, in the first pass after the wake recorded it: the wake decides alone
// and lowers no other model's target.
func (bs *borrowState) yieldReclaimed(decisions []domain.VariantDecision, requests []ModelScalingRequest) map[string]int {
	if bs == nil || (len(bs.reclaims) == 0 && len(bs.wakeReclaims) == 0) {
		return nil
	}
	var lowered map[string]int
	type key struct{ cohort, acc string }
	need := map[key]int{}
	reclaimant := map[key]map[string]bool{}
	for _, d := range slices.Concat(bs.reclaims, bs.wakeReclaims) {
		fresh := d.GPUs - d.CreditGPUs
		if fresh <= 0 || d.Cohort == "" {
			continue
		}
		k := key{d.Cohort, d.Accelerator}
		need[k] += fresh
		if reclaimant[k] == nil {
			reclaimant[k] = map[string]bool{}
		}
		reclaimant[k][d.Namespace] = true
	}
	states := map[string]domain.VariantReplicaState{}
	paired := map[string]bool{}
	// A passive model gives nothing back: its target is a would-be figure,
	// and lowering it frees no GPU for the reclaim.
	passive := passiveModels(requests)
	for _, req := range requests {
		paired[req.Namespace+"/"+req.ModelID] = req.Disaggregated
		for _, st := range req.VariantStates {
			states[req.Namespace+"/"+st.VariantName] = st
		}
	}
	keys := slices.SortedFunc(maps.Keys(need), func(a, b key) int {
		return cmp.Or(strings.Compare(a.cohort, b.cohort), strings.Compare(a.acc, b.acc))
	})
	for _, k := range keys {
		left := need[k]
		for _, ns := range slices.Sorted(maps.Keys(bs.heldPast)) {
			if left <= 0 {
				break
			}
			if reclaimant[k][ns] || !slices.Contains(bs.cohorts[ns], k.cohort) {
				continue
			}
			t, ok := accelerator.FindKey(bs.heldPast[ns], k.acc)
			if !ok || bs.heldPast[ns][t] <= 0 {
				continue
			}
			take := min(left, bs.heldPast[ns][t])
			left -= take
			freed := 0
			var models []string
			for i := range decisions {
				d := &decisions[i]
				if d.Namespace != ns || !accelerator.SameName(d.AcceleratorName, k.acc) || passive[ns+"/"+d.ModelID] {
					continue
				}
				if !slices.Contains(models, d.ModelID) {
					models = append(models, d.ModelID)
				}
				freed += max(0, d.CurrentReplicas-d.TargetReplicas) * max(1, d.GPUsPerReplica)
			}
			slices.Sort(models)
			for _, m := range models {
				for freed < take {
					unit := yieldUnit(decisions, ns, m, states, paired[ns+"/"+m])
					gpus, largest := 0, 0
					for i, n := range unit {
						g := max(1, decisions[i].GPUsPerReplica)
						largest = max(largest, g)
						if accelerator.SameName(decisions[i].AcceleratorName, k.acc) {
							gpus += n * g
						}
					}
					if gpus == 0 {
						break // nothing of this model left to give back on k.acc
					}
					if gpus-(take-freed) > largest {
						// The model's smallest unit overshoots what is still
						// reclaimed by more than one replica (a 3P:4D pair
						// for 8 GPUs): giving it back would drop far more
						// than Kueue preempts. The next model, if any.
						break
					}
					for _, i := range slices.Sorted(maps.Keys(unit)) {
						d := &decisions[i]
						d.TargetReplicas -= unit[i]
						d.GPUsAllocated = d.TargetReplicas * max(1, d.GPUsPerReplica)
						yieldStep(d, k.cohort)
						if lowered == nil {
							lowered = map[string]int{}
						}
						lowered[d.Namespace+"/"+d.VariantName] += unit[i]
					}
					freed += gpus
				}
			}
		}
	}
	return lowered
}

// yieldUnit is the next unit model m of namespace ns gives back, as replicas
// per index into decisions; nil when none is left above minReplicas. A
// single-role model gives one replica of its first variant (by name) above
// minReplicas. A P/D model gives a whole pair at its role ratio -- each role's
// total target over their greatest common divisor, 1P:2D giving one prefill
// and two decodes -- each role's part from its variants with the most above
// minReplicas first, so the ratio is kept, as the share transfer keeps it.
func yieldUnit(decisions []domain.VariantDecision, ns, m string, states map[string]domain.VariantReplicaState,
	paired bool) map[int]int {
	return yieldUnitWhere(decisions, ns, m, states, paired, nil)
}

// yieldUnitWhere is yieldUnit with a single-role model's replica taken only
// from a variant eligible accepts (nil accepts every one): a release to a
// lender gives back a replica on the lender's flavor, not the first variant
// by name. A P/D pair is whole whatever eligible says.
func yieldUnitWhere(decisions []domain.VariantDecision, ns, m string, states map[string]domain.VariantReplicaState,
	paired bool, eligible func(i int) bool) map[int]int {
	minOfVariant := func(d domain.VariantDecision) int { return minOf(states[ns+"/"+d.VariantName]) }
	byRole := map[string][]int{}
	for i, d := range decisions {
		if d.Namespace != ns || d.ModelID != m {
			continue
		}
		role := d.Role
		if !paired {
			role = ""
		}
		byRole[role] = append(byRole[role], i)
	}
	spare := func(i int) int { return decisions[i].TargetReplicas - minOfVariant(decisions[i]) }
	order := func(idx []int) []int {
		out := slices.Clone(idx)
		slices.SortStableFunc(out, func(a, b int) int {
			if paired {
				if c := cmp.Compare(spare(b), spare(a)); c != 0 {
					return c
				}
			}
			return strings.Compare(decisions[a].VariantName, decisions[b].VariantName)
		})
		return out
	}
	if !paired {
		for _, i := range order(byRole[""]) {
			if spare(i) > 0 && (eligible == nil || eligible(i)) {
				return map[int]int{i: 1}
			}
		}
		return nil
	}
	totals := map[string]int{}
	g := 0
	for role, idx := range byRole {
		for _, i := range idx {
			totals[role] += decisions[i].TargetReplicas
		}
		if totals[role] > 0 {
			g = gcd(g, totals[role])
		}
	}
	if g == 0 {
		return nil
	}
	unit := map[int]int{}
	for _, role := range slices.Sorted(maps.Keys(byRole)) {
		n := totals[role] / g
		for _, i := range order(byRole[role]) {
			take := min(n, max(0, spare(i)))
			if take > 0 {
				unit[i] = take
				n -= take
			}
		}
		if n > 0 {
			return nil // the role cannot give its part without going below minReplicas
		}
	}
	return unit
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// yieldStep records on d that a reclaim in cohort lowered its target.
func yieldStep(d *domain.VariantDecision, cohort string) {
	action := domain.ActionNoChange
	switch {
	case d.TargetReplicas < d.CurrentReplicas:
		action = domain.ActionScaleDown
	case d.TargetReplicas > d.CurrentReplicas:
		action = domain.ActionScaleUp
	}
	d.Action = action
	d.DecisionSteps = append(d.DecisionSteps, domain.DecisionStep{
		Name: ReclaimYieldStepName, Action: action, TargetReplicas: d.TargetReplicas, WasConstrained: true,
		Reason: fmt.Sprintf("a model of this controller reclaims nominal this namespace borrows in cohort %s; "+
			"Kueue preempts a borrowed replica to admit it", cohort),
	})
}
