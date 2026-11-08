package kueue

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// ReservedPods returns, of pods (all in namespace ns, normally at Kueue's
// admission gate), those whose Workload Kueue has already reserved quota for
// (QuotaReserved, or admitted). Such a pod still carries the gate while an
// admission check -- a ProvisioningRequest asking the cluster autoscaler for
// a node -- has not passed, yet its GPUs are in the ClusterQueue's
// reservation: it is not waiting for quota.
//
// A pod's Workload is the one owned by the pod itself (Kueue's pod
// integration), by the pod's controller, or, for a LeaderWorkerSet's pod, by
// the LeaderWorkerSet it names. One read of the namespace's Workloads, from
// the informer when the controller wired one (WorkloadsVia). A Workload kind
// not served, or a Forbidden read, reserves none; any other failed read is
// returned, and the caller reads the pods as unreserved.
//
// A LeaderWorkerSet whose Workloads are owned by the set is matched by the
// set alone: while one of its groups is reserved, a gated pod of another
// group reads as reserved too.
func ReservedPods(ctx context.Context, src WorkloadSource, ns string, pods []*corev1.Pod) (map[types.UID]bool, error) {
	if len(pods) == 0 {
		return nil, nil
	}
	items, err := src.Workloads(ctx, ns, "")
	if err != nil {
		if workloadsAbsent(err) {
			// No Workload kind served, or the read is denied: none reads as
			// reserved, as the managed check reads it (G4).
			return nil, nil
		}
		return nil, err
	}
	owners := map[types.UID]bool{}
	sets := map[string]bool{}
	for i := range items {
		if !workloadReserved(&items[i]) {
			continue
		}
		for _, ref := range items[i].GetOwnerReferences() {
			owners[ref.UID] = true
			if ref.Kind == "LeaderWorkerSet" {
				sets[ref.Name] = true
			}
		}
	}
	var out map[types.UID]bool
	for _, p := range pods {
		reserved := owners[p.UID]
		if ref := metav1.GetControllerOf(p); ref != nil && owners[ref.UID] {
			reserved = true
		}
		if set := p.Labels[lwsv1.SetNameLabelKey]; set != "" && sets[set] {
			reserved = true
		}
		if reserved {
			if out == nil {
				out = map[types.UID]bool{}
			}
			out[p.UID] = true
		}
	}
	return out, nil
}
