package kueue

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// fixedWorkloads is a WorkloadSource listing fixed Workloads, or failing.
type fixedWorkloads struct {
	items []unstructured.Unstructured
	err   error
}

func (f fixedWorkloads) Workloads(_ context.Context, _ string, owner types.UID) ([]unstructured.Unstructured, error) {
	return ownedBy(f.items, owner), f.err
}

func (fixedWorkloads) Indexed() bool { return false }

// A gated pod is reserved when Kueue reserved quota for the Workload owned by
// the pod, its controller, or the LeaderWorkerSet it names; a Workload
// pending admission reserves nothing. A read that fails for a reason that may
// pass is returned.
func TestReservedPods(t *testing.T) {
	workload := func(name string, reserved bool, owner metav1.OwnerReference) unstructured.Unstructured {
		w := unstructured.Unstructured{Object: map[string]interface{}{}}
		w.SetName(name)
		w.SetOwnerReferences([]metav1.OwnerReference{owner})
		if reserved {
			w.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
				"type": WorkloadQuotaReserved, "status": "True"}}}
		}
		return w
	}
	yes := true
	pods := []*corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "own", UID: "p-own"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "rs", UID: "p-rs", OwnerReferences: []metav1.OwnerReference{
			{Kind: "ReplicaSet", Name: "rs", UID: "rs-uid", Controller: &yes}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "lws-0", UID: "p-lws", Labels: map[string]string{lwsv1.SetNameLabelKey: "set"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pending", UID: "p-pending"}},
	}
	src := fixedWorkloads{items: []unstructured.Unstructured{
		workload("w-own", true, metav1.OwnerReference{Kind: "Pod", Name: "own", UID: "p-own"}),
		workload("w-rs", true, metav1.OwnerReference{Kind: "ReplicaSet", Name: "rs", UID: "rs-uid"}),
		workload("w-lws", true, metav1.OwnerReference{Kind: "LeaderWorkerSet", Name: "set", UID: "lws-uid"}),
		workload("w-pending", false, metav1.OwnerReference{Kind: "Pod", Name: "pending", UID: "p-pending"}),
	}}
	got, err := ReservedPods(context.Background(), src, "ns", pods)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got["p-own"] || !got["p-rs"] || !got["p-lws"] || got["p-pending"] {
		t.Fatalf("reserved = %v, want own, rs and lws-0, not pending", got)
	}
	if _, err := ReservedPods(context.Background(), fixedWorkloads{err: errors.New("cache not synced")}, "ns", pods); err == nil {
		t.Fatal("a failed read reads as none reserved; want the error")
	}
}
