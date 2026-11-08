package variantmeta

import (
	"context"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// GateGrace is how long a pod may sit at Kueue's admission gate before it
// counts as unplaced: one engine cycle. A pod is gated the moment it is
// created, and Kueue admits a fitting one within a cycle; only one it did not
// admit says the capacity is not there.
var GateGrace = 30 * time.Second

// placement counts a variant's unplaced replicas
// (the Kueue borrowing proposal, PR #161, B5): pods the
// scheduler reports Unschedulable, and pods held at Kueue's admission gate for
// longer than GateGrace. A pod pulling an image or loading weights is placed:
// it has a node. On a LeaderWorkerSet a replica is a group, unplaced while any
// of its pods is. since is when the newest unplaced pod became so (the
// Unschedulable condition's transition, or the gated pod's creation), pods
// their names, sorted, and at when each unplaced replica became so: per pod,
// or on a LeaderWorkerSet per group (its group-index label), a group counting
// from when its newest unplaced pod became so. since is the newest, for the
// signals that ask whether anything is newly unplaced; the placement timeout
// reads at (BorrowGovernor.timeoutReleases), so it counts replicas, never the
// pods of one group as several. gated is the part of at held at Kueue's
// admission gate (countGated).
func placement(ctx context.Context, k8sClient client.Client, namespace string,
	scaleTarget scaletarget.ScaleTargetAccessor, now time.Time) (unplaced int, since time.Time, pods []string, at, gated map[string]time.Time) {
	if k8sClient == nil || scaleTarget == nil {
		return 0, time.Time{}, nil, nil, nil
	}
	list, err := ListVariantPods(ctx, k8sClient, namespace, scaleTarget)
	if err != nil {
		return 0, time.Time{}, nil, nil, nil
	}
	group := scaletarget.IsLeaderWorkerSet(scaleTarget)
	unplaced, since, pods, at = countUnplaced(list, group, now)
	if len(at) == 0 {
		return unplaced, since, pods, at, nil
	}
	var gatedPods []*corev1.Pod
	for i := range list {
		if kueue.PodGated(&list[i]) {
			gatedPods = append(gatedPods, &list[i])
		}
	}
	reserved, err := kueue.ReservedPods(ctx, kueue.WorkloadsVia(k8sClient), namespace, gatedPods)
	if err != nil {
		// Whether Kueue reserved them cannot be told: none is reported as
		// waiting for quota, so none is released for it.
		return unplaced, since, pods, at, nil
	}
	return unplaced, since, pods, at, countGated(list, group, at, reserved)
}

// countGated is the replicas of at (countUnplaced's) one of whose unplaced
// pods is held at Kueue's admission gate waiting for quota, each at its time
// in at: per pod, or on a LeaderWorkerSet per group. A gated pod whose
// Workload Kueue already reserved quota for (reserved, kueue.ReservedPods)
// waits on an admission check, not on quota, and is left out. Nil when none.
func countGated(list []corev1.Pod, group bool, at map[string]time.Time, reserved map[types.UID]bool) map[string]time.Time {
	var out map[string]time.Time
	for i := range list {
		p := &list[i]
		if !kueue.PodGated(p) || reserved[p.UID] || p.DeletionTimestamp != nil || p.Spec.NodeName != "" {
			continue
		}
		key := p.Name
		if group {
			key = p.Labels[lwsv1.GroupIndexLabelKey]
		}
		t, unplaced := at[key]
		if !unplaced {
			continue // gated for less than GateGrace: not unplaced yet
		}
		if out == nil {
			out = map[string]time.Time{}
		}
		out[key] = t
	}
	return out
}

// countUnplaced is placement over listed pods.
func countUnplaced(list []corev1.Pod, group bool, now time.Time) (unplaced int, since time.Time, pods []string, at map[string]time.Time) {
	for i := range list {
		p := &list[i]
		t, ok := unplacedSince(p, now)
		if !ok {
			continue
		}
		pods = append(pods, p.Name)
		if at == nil {
			at = map[string]time.Time{}
		}
		key := p.Name
		if group {
			key = p.Labels[lwsv1.GroupIndexLabelKey]
		}
		if prev, seen := at[key]; !seen || t.After(prev) {
			at[key] = t
		}
		if t.After(since) {
			since = t
		}
	}
	slices.Sort(pods)
	return len(at), since, pods, at
}

// unplacedSince reports whether a pod is unplaced, and since when.
func unplacedSince(p *corev1.Pod, now time.Time) (time.Time, bool) {
	if p.DeletionTimestamp != nil || p.Spec.NodeName != "" {
		return time.Time{}, false
	}
	switch p.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return time.Time{}, false
	}
	if kueue.PodGated(p) {
		created := p.CreationTimestamp.Time
		if created.IsZero() || now.Sub(created) <= GateGrace {
			return time.Time{}, false
		}
		return created, true
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			at := c.LastTransitionTime.Time
			if at.IsZero() {
				at = p.CreationTimestamp.Time
			}
			return at, true
		}
	}
	return time.Time{}, false
}
