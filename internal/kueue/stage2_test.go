package kueue

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The B1 table order (proposal rev 8): withheld, then frozen, then counted
// but 0, each in its row order. One function decides it.
func TestPrecedenceFollowsTheB1Table(t *testing.T) {
	for _, tc := range []struct {
		in   []Reason
		want Reason
	}{
		{nil, ""},
		{[]Reason{"", ""}, ""},
		{[]Reason{ReasonNoCohort, ReasonNotKueueManaged}, ReasonNotKueueManaged},
		{[]Reason{ReasonStale, ReasonHierarchyUnknown}, ReasonHierarchyUnknown},
		{[]Reason{ReasonKueueControllerDown, ReasonStale}, ReasonStale},
		{[]Reason{ReasonKueueControllerDown, ReasonKueueVersionUnknown}, ReasonKueueVersionUnknown},
		{[]Reason{ReasonPreemptionsUnreadable, ReasonLenderWaiting, ReasonCohortPoolEmpty}, ReasonLenderWaiting},
		{[]Reason{ReasonCoResourceBound, ReasonFlavorUnpinned, ReasonBorrowWithinCohort}, ReasonBorrowWithinCohort},
		{[]Reason{ReasonNominalLentOut, ReasonNoBorrowCap}, ReasonNoBorrowCap},
		{[]Reason{ReasonNominalLentOut}, ReasonNominalLentOut},
		{[]Reason{ReasonLendersUnreadable, ReasonNotSelected}, ReasonNotSelected},
	} {
		if got := Precedence(tc.in...); got != tc.want {
			t.Errorf("Precedence(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	order := ReasonOrder()
	if len(order) != 28 || order[0] != ReasonNotSelected || order[len(order)-1] != ReasonPreemptionsUnreadable {
		t.Errorf("table order = %v", order)
	}
	for i := 1; i < len(order); i++ {
		if got := Precedence(order[i], order[i-1]); got != order[i-1] {
			t.Errorf("%q must win over %q, got %q", order[i-1], order[i], got)
		}
	}
}

// B5: a queue in the cohort with borrowWithinCohort other than Never withholds
// the cohort; read from the ClusterQueue.
func TestBorrowWithinCohortWithholds(t *testing.T) {
	a := member("a", QueueResource{Nominal: 8})
	b := member("b", QueueResource{Nominal: 8})
	if r := figures2(t, ComputeBorrowing(topology(a, b), false), "a").Reason; r != "" {
		t.Fatalf("control: counted, got %q", r)
	}
	b.BorrowWithinCohort = true
	if r := figures2(t, ComputeBorrowing(topology(a, b), false), "a").Reason; r != ReasonBorrowWithinCohort {
		t.Errorf("reason = %q, want borrow-within-cohort", r)
	}
	r := &Reader{gpuNames: map[string]bool{"nvidia.com/gpu": true}}
	cq := clusterQueueObj(map[string]interface{}{"preemption": map[string]interface{}{"borrowWithinCohort": map[string]interface{}{"policy": "LowerPriority"}}})
	if q := r.queueState(cq, map[string]string{"h100": ""}); !q.BorrowWithinCohort {
		t.Error("borrowWithinCohort LowerPriority not read")
	}
	cq = clusterQueueObj(map[string]interface{}{"preemption": map[string]interface{}{"borrowWithinCohort": map[string]interface{}{"policy": "Never"}}})
	if q := r.queueState(cq, map[string]string{"h100": ""}); q.BorrowWithinCohort {
		t.Error("Never read as borrowing within the cohort")
	}
}

// clusterQueueObj is a ClusterQueue with one h100 GPU flavor of 8 GPUs, 64
// CPUs and 512Gi of memory in one resource group, and extra spec fields.
func clusterQueueObj(extra map[string]interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"cohortName": "c",
		"resourceGroups": []interface{}{map[string]interface{}{
			"coveredResources": []interface{}{"nvidia.com/gpu", "cpu", "memory"},
			"flavors": []interface{}{map[string]interface{}{
				"name": "h100",
				"resources": []interface{}{
					map[string]interface{}{"name": "nvidia.com/gpu", "nominalQuota": "8"},
					map[string]interface{}{"name": "cpu", "nominalQuota": "64"},
					map[string]interface{}{"name": "memory", "nominalQuota": "512Gi"},
				},
			}},
		}},
	}
	for k, v := range extra {
		spec[k] = v
	}
	u := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	u.SetName("cq")
	return u
}

func figures2(t *testing.T, b Borrowing, name string) QueueFigures {
	t.Helper()
	if len(b.Queues[name]) != 1 {
		t.Fatalf("figures of %s: %+v", name, b.Queues[name])
	}
	return b.Queues[name][0]
}

// B5: a variant must pin its flavor when the queue offers several for the GPU.
// K3: whenCanBorrow does not decide it. Kueue v0.20's CRD defaults it to
// MayStopSearch on every ClusterQueue, so reading it withheld every
// single-flavor queue whose flavor has no nodeLabels to pin, and with one
// flavor there is no other one to choose anyway.
func TestPinRequiredIsRead(t *testing.T) {
	r := &Reader{gpuNames: map[string]bool{"nvidia.com/gpu": true}}
	flavors := map[string]string{"h100": "", "h100-b": ""}
	if q := r.queueState(clusterQueueObj(nil), flavors); q.PinRequired {
		t.Error("control: one flavor, no fungibility set: no pin required")
	}
	for _, when := range []string{"MayStopSearch", "Borrow", "TryNextFlavor"} {
		cq := clusterQueueObj(map[string]interface{}{"flavorFungibility": map[string]interface{}{
			"whenCanBorrow": when, "whenCanPreempt": "TryNextFlavor"}})
		if q := r.queueState(cq, flavors); q.PinRequired {
			t.Errorf("one flavor, whenCanBorrow %s (MayStopSearch is the CRD default): no pin required", when)
		}
	}
	two := clusterQueueObj(nil)
	groups, _, _ := unstructured.NestedSlice(two.Object, "spec", "resourceGroups")
	g := groups[0].(map[string]interface{})
	g["flavors"] = append(g["flavors"].([]interface{}), map[string]interface{}{"name": "h100-b",
		"resources": []interface{}{map[string]interface{}{"name": "nvidia.com/gpu", "nominalQuota": "8"}}})
	if err := unstructured.SetNestedSlice(two.Object, groups, "spec", "resourceGroups"); err != nil {
		t.Fatal(err)
	}
	if q := r.queueState(two, flavors); !q.PinRequired {
		t.Error("two flavors of one GPU resource: pin required")
	}
	b := ComputeBorrowing(topology(withPin(member("a", QueueResource{Nominal: 8})), member("b", QueueResource{Nominal: 8})), false)
	if !figures2(t, b, "a").PinRequired {
		t.Error("PinRequired not carried onto the figures")
	}
}

func withPin(q QueueState) QueueState { q.PinRequired = true; return q }

// B1: the GPU flavor's resource group covers CPU and memory; they are read in
// milli-units, and the cohort's free pool of each is computed as for GPUs.
func TestCoResourcesAreReadAndPooled(t *testing.T) {
	r := &Reader{gpuNames: map[string]bool{"nvidia.com/gpu": true}}
	q := r.queueState(clusterQueueObj(nil), map[string]string{"h100": ""})
	cpu := q.CoResources[ResourceKey{Flavor: "h100", Resource: "cpu"}]
	if cpu.Nominal != 64000 {
		t.Fatalf("cpu nominal = %d milli, want 64000", cpu.Nominal)
	}
	if _, ok := q.CoResources[ResourceKey{Flavor: "h100", Resource: "memory"}]; !ok {
		t.Fatal("memory not read as a co-resource")
	}
	if !q.CoWithGPU["cpu"] || len(q.CoFlavors["cpu"]) != 1 {
		t.Fatalf("cpu shares the GPU's group: charged on the GPU's flavor; got %v %v", q.CoWithGPU, q.CoFlavors)
	}
	a := withCPU(member("a", QueueResource{Nominal: 8}), "h100", CoQuota{Nominal: 64000, Reserved: 70000}, true)
	b := withCPU(member("b", QueueResource{Nominal: 8}), "h100", CoQuota{Nominal: 64000, Reserved: 0}, true)
	pools := ComputeBorrowing(topology(a, b), false).Pools
	if len(pools) != 1 || pools[0].CoFree["cpu"] != 128000-70000 {
		t.Errorf("cpu free = %+v, want 58000 milli (128 CPUs lent, 70 used past what a keeps)", pools)
	}
}

// B1 and B6: a lender's pending Workload waits only when it fits its unused
// nominal of every covered co-resource, and only on a flavor its node
// selection allows.
func TestLenderWaitsOnlyOnAllowedFlavorAndCoResources(t *testing.T) {
	q := withCPU(member("lender", QueueResource{Nominal: 8}), "h100", CoQuota{Nominal: 16000}, true)
	w := pendingGPU(8, 0)
	w.Requests = map[string]int64{"cpu": 8000}
	q.PendingWorkloads = []PendingWorkload{w}
	h100 := map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"}
	if !LenderWaitingOn(q, gpuKey, h100) {
		t.Fatal("control: fits GPUs and CPU, no selector: waits")
	}
	w.Requests = map[string]int64{"cpu": 32000}
	q.PendingWorkloads = []PendingWorkload{w}
	if LenderWaitingOn(q, gpuKey, h100) {
		t.Error("asks more CPU than the lender's unused nominal: GPUs handed back would not admit it")
	}
	w.Requests = nil
	w.Placements = []WorkloadPlacement{{NodeSelector: map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB"}}}
	q.PendingWorkloads = []PendingWorkload{w}
	if LenderWaitingOn(q, gpuKey, h100) {
		t.Error("pinned to A100 nodes: it does not wait for the H100 flavor")
	}
	if !LenderWaitingOn(q, gpuKey, nil) {
		t.Error("flavor labels unknown: counts")
	}
	w.Placements = []WorkloadPlacement{{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}},
	}}}}}
	q.PendingWorkloads = []PendingWorkload{w}
	if !LenderWaitingOn(q, gpuKey, h100) {
		t.Error("a rule on a key the flavor does not name cannot rule it out: counts on it")
	}
}

// B8: the pool carries what every member's pending Workloads ask of it,
// whichever namespace they are in, on the flavors they allow.
func TestPoolCarriesEveryMembersPendingGPUs(t *testing.T) {
	a := member("a", QueueResource{Nominal: 8})
	b := member("b", QueueResource{Nominal: 8, Reserved: 8})
	big := pendingGPU(16, 0) // too large to make b wait: it would have to borrow
	b.PendingWorkloads = []PendingWorkload{big, pendingGPU(4, 0)}
	pools := ComputeBorrowing(topology(a, b), false).Pools
	if len(pools) != 1 || pools[0].PendingGPUs != 20 {
		t.Errorf("pending = %+v, want 20 GPUs of the pool's demand", pools)
	}
}

// D1: a namespace-scoped reader reads the Workloads, and the LocalQueues they
// queue through, cluster-wide, as the proposal's prerequisites grant: a
// lender's wait in another namespace is seen. Before, it returned unreadable
// without reading, so a namespace-scoped install could never borrow.
func TestReadPendingNamespaceScopedReadsClusterWide(t *testing.T) {
	const v = "v1beta2"
	w := obj(v, KindWorkload, "team-b", "train", map[string]interface{}{})
	spec := podSetsRequesting(map[string]interface{}{"nvidia.com/gpu": "4"})
	spec["queueName"] = "lq"
	w.Object["spec"] = spec
	c := newCohortClient(v, append(docsExample(v, "cohortName"), w)...)
	r := NewReader(c, Options{Namespace: "team-a"})
	got, unreadable := r.readPending(context.Background(), nil)
	if unreadable {
		t.Fatal("a namespace-scoped read reported the lenders unreadable without a failed read")
	}
	if n := len(got.byQueue["team-b-cq"]); n != 1 {
		t.Errorf("team-b-cq has %d pending Workloads read, want the one in team-b, another namespace", n)
	}
}
