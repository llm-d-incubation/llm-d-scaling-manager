package saturation

import (
	"math"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/aggregation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// hotRankReleaseMargin is how far below k1, as a fraction of the cache, a
// held replica's busiest rank must fall before the hold lets it go. A rank
// that hovers at k1 -- 0.81 one cycle, 0.79 the next -- otherwise turns the
// hold on and off every cycle, and the spare the engine reads with it.
const hotRankReleaseMargin = 0.05

// hotPod names one replica of one variant.
type hotPod struct{ variant, pod string }

// hotRankMemo is the replicas a model's hot-rank hold kept last cycle, and
// when (SaturationAnalyzer.hotRankHeld).
type hotRankMemo struct {
	pods map[hotPod]bool
	seen time.Time
}

// hotRankPods is the replicas whose busiest engine rank is full this cycle,
// and remembers them for the next.
//
// A replica is hot when its busiest rank is at k1 (kvFull). One that was hot
// last cycle stays hot until its busiest rank falls below k1 less
// hotRankReleaseMargin: the hold keeps a replica, and a replica kept and
// given back on alternate cycles is not kept.
//
// Only replicas of more than one rank are judged. With one rank the pod's
// demand is the rank's own, a full engine reads as a full replica, and
// nothing is hidden by a mean. A Pod that is not Ready is not routed to, and
// a warm-pool bridge is not the variant's to release.
//
// Prefill is judged only while decode is not saturated. While it is,
// holdPrefillDemand keeps prefill whole -- the KV a prefill rank holds is then
// decode's backlog, not prefill's load -- and the two never meet. Otherwise a
// full prefill rank is a prefill engine that cannot take the next prompt, and
// its replica is kept as decode's is.
func (a *SaturationAnalyzer) hotRankPods(namespace, modelID string, metrics []domain.ReplicaMetrics,
	rolesByVariant map[string]string, kvCacheThreshold float64, decodeSaturated bool,
) map[hotPod]bool {
	key := namespace + "|" + modelID
	a.mu.Lock()
	defer a.mu.Unlock()
	last := a.hotRankHeld[key].pods
	hot := map[hotPod]bool{}
	for _, rm := range metrics {
		role, known := rolesByVariant[rm.VariantName]
		if !known || !rm.Ready || rm.FromWarmPool || rm.Ranks() <= 1 {
			continue
		}
		if decodeSaturated && domain.CanonicalRole(role) == domain.RolePrefill {
			continue
		}
		p := hotPod{rm.VariantName, rm.PodName}
		if kvFull(rm, kvCacheThreshold) || (last[p] && kvFull(rm, kvCacheThreshold-hotRankReleaseMargin)) {
			hot[p] = true
		}
	}
	if len(hot) == 0 {
		delete(a.hotRankHeld, key)
		return nil
	}
	a.hotRankHeld[key] = hotRankMemo{pods: hot, seen: a.now()}
	return hot
}

// holdHotRanks keeps the capacity of every hot replica (hotRankPods) from
// being released, and returns the model's demand after the hold and what it
// added per role.
//
// A replica running several data-parallel ranks is priced on what the pod
// holds -- its ranks' tokens summed, their utilization averaged -- so one rank
// at its ceiling among idle ones reads as a pod with room to spare. As
// demand that is right: the idle ranks can take the next requests. As grounds
// for a release it is not. The full rank is an engine that cannot allocate,
// and a scale-down may remove any pod, this one included; the requests it
// holds then land on what is left. So a hot replica counts, for the release,
// as at the scale-down boundary: the variant's demand is raised to what the
// other replicas hold plus scaleDown times its per-replica capacity for each
// hot replica. The spare that is left is the other replicas' spare, which is
// what the fleet can give back without counting on the hot replica.
//
// This is not a hold of the role. The role's demand carries the raise and the
// engine's spare formula releases the rest; the caller reports the raise
// apart from domain.AnalyzerResult.HeldRoleDemand, which zeroes a role's
// spare outright.
//
// Only ever a raise, and never an order: the role's demand, and the model's,
// stay at or below scaleUp times the anticipated supply the ENGINE will read
// -- each variant's replicas capped at its scale target's count, as
// steadystate.clampReplicaCountToScaleTarget caps them, so a pod still
// reporting while a scale-down removes it is not counted as room -- and the
// last step lands on that ceiling in the engine's own arithmetic, so no
// rounding orders a replica. A demand already at or above the raise is left
// alone, so a hold that ran before this one is not added to.
//
// variants and roleDemand (nil on an aggregated model) are updated in place;
// total is the model's demand before the hold.
func holdHotRanks(hot map[hotPod]bool, caps []capacity.ReplicaCapacity, variants []domain.VariantCapacity,
	states []domain.VariantReplicaState, roleDemand map[string]float64,
	total, scaleUp, scaleDown float64,
) (float64, map[string]float64) {
	if len(hot) == 0 || !(scaleDown > 0) || !(scaleUp > 0) {
		return total, nil
	}
	hotCount := map[string]int{}
	restDemand := map[string]float64{}
	for _, rc := range caps {
		if rc.FromWarmPool {
			continue
		}
		if hot[hotPod{rc.VariantName, rc.PodName}] {
			hotCount[rc.VariantName]++
			continue
		}
		restDemand[rc.VariantName] += float64(rc.ReplicaDemand)
	}

	// The supply the engine will read: replicas capped at the scale target.
	target := map[string]int{}
	for _, s := range states {
		if s.CurrentReplicas > 0 {
			target[s.VariantName] = s.CurrentReplicas
		}
	}
	engineView := slices.Clone(variants)
	for i := range engineView {
		if n, ok := target[engineView[i].VariantName]; ok {
			engineView[i].ReplicaCount = min(engineView[i].ReplicaCount, n)
		}
	}

	// The room under the scale-up threshold, per role and for the model.
	ceiling := map[string]float64{}
	room := map[string]float64{}
	for role, t := range aggregation.AggregateByRole(engineView) {
		demand := t.TotalDemand
		if roleDemand != nil {
			demand = roleDemand[role]
		}
		ceiling[role] = demandCeiling(t.TotalAnticipatedSupply, scaleUp)
		room[role] = max(0, ceiling[role]-demand)
	}
	modelCeiling := demandCeiling(aggregation.SumTotalAnticipatedSupply(engineView), scaleUp)
	modelRoom := max(0, modelCeiling-total)

	byRole := map[string]float64{}
	var moved float64
	for i := range variants {
		vc := &variants[i]
		n := hotCount[vc.VariantName]
		if n == 0 || !(vc.PerReplicaCapacity > 0) {
			continue
		}
		role := domain.RoleOfVC(*vc)
		want := restDemand[vc.VariantName] + scaleDown*vc.PerReplicaCapacity*float64(n)
		add := min(want-vc.TotalDemand, room[role], modelRoom)
		if !(add > 0) {
			continue
		}
		vc.TotalDemand += add
		if s := float64(vc.ReplicaCount) * vc.PerReplicaCapacity; s > 0 {
			vc.Utilization = vc.TotalDemand / s
		}
		room[role] -= add
		modelRoom -= add
		byRole[role] += add
		moved += add
	}
	if moved == 0 {
		return total, nil
	}
	// Land each sum at or under its ceiling as the engine will compute it.
	if roleDemand != nil {
		for role, add := range byRole {
			before := roleDemand[role]
			after := max(before, min(before+add, ceiling[role]))
			roleDemand[role] = after
			if after > before {
				byRole[role] = after - before
			} else {
				delete(byRole, role)
			}
		}
	}
	return max(total, min(total+moved, modelCeiling)), byRole
}

// demandCeiling is the largest demand d for which d/scaleUp does not exceed
// anticipated in floating point: the engine's required capacity,
// max(0, d/scaleUp - anticipated), is then exactly zero. scaleUp x
// anticipated alone can land an ulp above it, and a positive required
// capacity, however small, is rounded up to a replica.
func demandCeiling(anticipated, scaleUp float64) float64 {
	d := scaleUp * anticipated
	for d > 0 && d/scaleUp > anticipated {
		d = math.Nextafter(d, 0)
	}
	return max(d, 0)
}
