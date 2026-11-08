package scaletarget

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/resources"
)

// ScaleTargetAccessor provides a uniform interface to extract scaling-relevant
// information from any supported scale target kind (Deployment, LeaderWorkerSet).
type ScaleTargetAccessor interface {
	// GetName returns the name of the scale target.
	GetName() string

	// GetNamespace returns the namespace of the scale target.
	GetNamespace() string

	// GetReplicas returns current spec replicas.
	GetReplicas() *int32
	GetDeletionTimestamp() *v1.Time
	// GetUID identifies this incarnation of the scale target: a target deleted
	// and re-created under the same name is a different fleet.
	GetUID() types.UID

	// GetStatusReplicas returns status replicas (actual running).
	GetStatusReplicas() int32
	GetStatusReadyReplicas() int32

	// GetTotalGPUsPerReplica returns total GPU count across all pods in a replica.
	// For Deployment: GPUs from the single pod template.
	// For LWS: leader_GPUs + (Size - 1) * worker_GPUs.
	GetTotalGPUsPerReplica() int

	// GetLeaderPodTemplateSpec returns the pod template for the leader/primary pod.
	// For Deployment: the single pod template.
	// For LWS: the leader template (falls back to worker template if not set).
	// Use this for: engine args extraction (leader starts the API server),
	// metrics port discovery, pod label matching.
	GetLeaderPodTemplateSpec() *corev1.PodTemplateSpec

	// GetWorkerPodTemplateSpec returns the pod template for worker pods.
	// For Deployment: same as GetLeaderPodTemplateSpec() (single template).
	// For LWS: the worker template.
	// Use this for: GPU resource extraction when workers differ from leader.
	GetWorkerPodTemplateSpec() *corev1.PodTemplateSpec

	// GetGroupSize returns the number of pods per replica.
	// For Deployment: always 1.
	// For LWS: spec.leaderWorkerTemplate.size (1 leader + N-1 workers).
	GetGroupSize() int32
}

// PodGPUs returns the GPUs each pod of one replica requests, leader first: one
// entry for a Deployment, GetGroupSize entries for a LeaderWorkerSet, whose
// leader runs the worker template when it has none of its own. It returns nil
// when no pod requests a GPU explicitly: the replica's default of one GPU says
// nothing about how it is split across pods.
func PodGPUs(acc ScaleTargetAccessor) []int {
	if acc == nil {
		return nil
	}
	gpus := func(t *corev1.PodTemplateSpec) int {
		if t == nil {
			return 0
		}
		return resources.GetContainersGPUs(t.Spec.Containers)
	}
	size := max(int(acc.GetGroupSize()), 1)
	out := make([]int, 0, size)
	out = append(out, gpus(acc.GetLeaderPodTemplateSpec()))
	worker := gpus(acc.GetWorkerPodTemplateSpec())
	for range size - 1 {
		out = append(out, worker)
	}
	for _, g := range out {
		if g > 0 {
			return out
		}
	}
	return nil
}

// PodTolerations returns the tolerations of a replica's pods as node fit judges
// them: the leader's template, or the worker's when it has none of its own. A
// LeaderWorkerSet's workers are judged by its leader's. Nil for a nil acc.
func PodTolerations(acc ScaleTargetAccessor) []corev1.Toleration {
	if acc == nil {
		return nil
	}
	if t := acc.GetLeaderPodTemplateSpec(); t != nil {
		return t.Spec.Tolerations
	}
	if t := acc.GetWorkerPodTemplateSpec(); t != nil {
		return t.Spec.Tolerations
	}
	return nil
}

// IsLeaderWorkerSet reports whether acc is a LeaderWorkerSet. Group size is not
// the test: an LWS of size 1 is still an LWS, whose pods belong to a
// StatefulSet and are named by group index, not to a ReplicaSet.
func IsLeaderWorkerSet(acc ScaleTargetAccessor) bool {
	_, ok := acc.(*lwsAccessor)
	return ok
}

// RollingOut reports whether acc's workload says it is mid-rollout: a
// Deployment some of whose pods are not of its current template -- including a
// new ReplicaSet whose pods do not exist yet -- or whose controller has not yet
// observed its latest spec (a template change not acted on yet; a scale too,
// briefly), or a LeaderWorkerSet whose UpdateInProgress condition is True.
//
// For a LeaderWorkerSet the condition is the whole test, and the status
// counts are not. The LWS controller (v0.8.0, updateConditions) skips a group
// whose worker StatefulSet does not exist yet, while Status.Replicas is the
// leader StatefulSet's and counts a Pending leader -- and the worker
// StatefulSet is created only once the leader is Ready (startup policy
// LeaderReady) or scheduled (exclusive placement). So UpdatedReplicas <
// Replicas on every scale-up of such a set: read as a rollout, it held back
// every reading of the variant for as long as the new group took to start,
// which is when it saturates. UpdateInProgress counts only groups that exist.
// Nor do the conditions carry an ObservedGeneration (makeCondition sets
// none), so a template change the controller has not acted on yet cannot be
// told from the status: the pods' own revisions tell it, once any group has
// moved (the analyzer's revision check, ReplicaMetrics.Revision).
func RollingOut(acc ScaleTargetAccessor) bool {
	switch a := acc.(type) {
	case *deploymentAccessor:
		d := a.deployment
		return d.Status.ObservedGeneration < d.Generation || d.Status.UpdatedReplicas < d.Status.Replicas
	case *lwsAccessor:
		return meta.IsStatusConditionTrue(a.lws.Status.Conditions, string(lwsv1.LeaderWorkerSetUpdateInProgress))
	}
	return false
}

// LWSPartition is a LeaderWorkerSet's rolling-update partition: groups below it
// stay on their revision while those at or above it update. 0 when unset, and
// for any other target.
func LWSPartition(acc ScaleTargetAccessor) int {
	a, ok := acc.(*lwsAccessor)
	if !ok {
		return 0
	}
	if c := a.lws.Spec.RolloutStrategy.RollingUpdateConfiguration; c != nil && c.Partition != nil {
		return int(*c.Partition)
	}
	return 0
}
