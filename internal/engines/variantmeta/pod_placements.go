package variantmeta

import (
	"reflect"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// podPlacements is the node placement of one replica's pods
// (domain.VariantMetadata.Placements): one entry when every pod is placed
// alike, or two, the leader's then the workers', for a LeaderWorkerSet whose
// leader template places differently from its worker template -- an
// unconstrained one then as a zero PodPlacement, so each entry stays its
// template's. Nil when no template constrains its nodes.
func podPlacements(acc scaletarget.ScaleTargetAccessor) []domain.PodPlacement {
	if acc == nil {
		return nil
	}
	of := func(t *corev1.PodTemplateSpec) *domain.PodPlacement {
		if t == nil {
			return nil
		}
		return domain.PlacementOf(&t.Spec)
	}
	leader, worker := acc.GetLeaderPodTemplateSpec(), acc.GetWorkerPodTemplateSpec()
	lp, wp := of(leader), of(worker)
	switch {
	case leader == nil || worker == nil || worker == leader:
		// One template places every pod.
		if lp == nil {
			lp = wp
		}
		if lp == nil {
			return nil
		}
		return []domain.PodPlacement{*lp}
	case reflect.DeepEqual(lp, wp):
		if lp == nil {
			return nil
		}
		return []domain.PodPlacement{*lp}
	}
	out := make([]domain.PodPlacement, 2)
	if lp != nil {
		out[0] = *lp
	}
	if wp != nil {
		out[1] = *wp
	}
	return out
}

// PodPlacements is the node placement of one replica's pods of acc, as
// podPlacements has it; nil when no template constrains its nodes.
// The scale-from-zero engine fits a wake with it (WN3).
func PodPlacements(acc scaletarget.ScaleTargetAccessor) []domain.PodPlacement {
	return podPlacements(acc)
}

// replicaRequests is what one replica's pods request in all of each resource,
// in milli-units (domain.VariantMetadata.ReplicaRequests): the leader pod once
// and, for a LeaderWorkerSet, each worker of the group. A resource no
// container names is absent. Nil when there is no template.
func replicaRequests(acc scaletarget.ScaleTargetAccessor) map[string]int64 {
	if acc == nil {
		return nil
	}
	leader, worker := acc.GetLeaderPodTemplateSpec(), acc.GetWorkerPodTemplateSpec()
	if leader == nil && worker == nil {
		return nil
	}
	out := map[string]int64{}
	add := func(t *corev1.PodTemplateSpec, n int64) {
		if t == nil {
			return
		}
		for name, milli := range kueue.PodRequestsMilli(&t.Spec) {
			out[name] += n * milli
		}
	}
	size := max(int64(acc.GetGroupSize()), 1)
	if leader != nil {
		add(leader, 1)
		add(worker, size-1)
	} else {
		add(worker, size)
	}
	return out
}

// priorityClass is the Kueue priority class of one replica's Workload, read
// from its leader template, or its worker template when it has no leader
// (domain.VariantMetadata.PriorityClass). Empty when there is no template.
func priorityClass(acc scaletarget.ScaleTargetAccessor) string {
	if acc == nil {
		return ""
	}
	if t := acc.GetLeaderPodTemplateSpec(); t != nil {
		return kueue.TemplatePriorityClass(t)
	}
	return kueue.TemplatePriorityClass(acc.GetWorkerPodTemplateSpec())
}
