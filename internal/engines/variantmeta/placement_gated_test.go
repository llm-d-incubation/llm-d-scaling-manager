package variantmeta

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// GatedAt is the part of UnplacedAt Kueue holds at its admission gate: a pod
// the scheduler cannot place was admitted by Kueue, and a pod gated for less
// than GateGrace is not unplaced yet. On a LeaderWorkerSet a group is gated
// when one of its unplaced pods is.
func TestCountGatedIsThePartOfUnplacedAtKueuesGate(t *testing.T) {
	now := time.Unix(10000, 0)
	pod := func(name, group string, gatedFor time.Duration, unschedulable bool) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))},
			Status: corev1.PodStatus{Phase: corev1.PodPending}}
		if group != "" {
			p.Labels = map[string]string{lwsv1.GroupIndexLabelKey: group}
		}
		if gatedFor > 0 {
			p.CreationTimestamp = metav1.NewTime(now.Add(-gatedFor))
			p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: kueue.AdmissionGate}}
		}
		if unschedulable {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: corev1.PodReasonUnschedulable, LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Minute))}}
		}
		return p
	}

	// d is gated as long as b, but Kueue reserved its quota: it waits on an
	// admission check (a ProvisioningRequest), not on quota.
	list := []corev1.Pod{pod("a", "", 0, true), pod("b", "", 5*time.Minute, false), pod("c", "", 5*time.Second, false),
		pod("d", "", 5*time.Minute, false)}
	list[3].UID = "d-uid"
	_, _, _, at := countUnplaced(list, false, now)
	gated := countGated(list, false, at, map[types.UID]bool{"d-uid": true})
	if len(gated) != 1 || !gated["b"].Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("gated = %v, want only b at its creation: a is admitted, c within the grace, d reserved", gated)
	}

	groups := []corev1.Pod{pod("g0-a", "0", 0, true), pod("g1-a", "1", 0, true), pod("g1-b", "1", 5*time.Minute, false)}
	_, _, _, at = countUnplaced(groups, true, now)
	gated = countGated(groups, true, at, nil)
	if len(gated) != 1 || !gated["1"].Equal(at["1"]) {
		t.Fatalf("gated = %v, want group 1 alone, at its unplaced time %v", gated, at["1"])
	}
}
