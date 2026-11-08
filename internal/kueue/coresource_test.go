package kueue

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// withCPU gives q the co-resource cpu, of quota c on flavor, in a resource group
// that covers a GPU (withGPU) or of its own with that one flavor.
func withCPU(q QueueState, flavor string, c CoQuota, withGPU bool) QueueState {
	const res = "cpu"
	if q.CoResources == nil {
		q.CoResources = map[ResourceKey]CoQuota{}
		q.CoFlavors, q.CoWithGPU = map[string][]string{}, map[string]bool{}
	}
	q.CoResources[ResourceKey{Flavor: flavor, Resource: res}] = c
	q.CoFlavors[res] = append(q.CoFlavors[res], flavor)
	q.CoWithGPU[res] = withGPU
	return q
}

// splitGroups is a ClusterQueue whose GPUs sit in one resource group (flavor
// h100) and whose CPU in another, offered on cpuFlavors -- the common shape
// of a queue sharing a CPU pool across GPU flavors.
func splitGroups(cpuFlavors ...string) *unstructured.Unstructured {
	flavors := make([]interface{}, 0, len(cpuFlavors))
	for _, f := range cpuFlavors {
		flavors = append(flavors, map[string]interface{}{"name": f, "resources": []interface{}{
			map[string]interface{}{"name": "cpu", "nominalQuota": "64", "borrowingLimit": "16"},
		}})
	}
	u := clusterQueueObj(map[string]interface{}{"resourceGroups": []interface{}{
		map[string]interface{}{
			"coveredResources": []interface{}{"nvidia.com/gpu"},
			"flavors": []interface{}{map[string]interface{}{"name": "h100", "resources": []interface{}{
				map[string]interface{}{"name": "nvidia.com/gpu", "nominalQuota": "8"},
			}}},
		},
		map[string]interface{}{"coveredResources": []interface{}{"cpu"}, "flavors": flavors},
	}})
	return u
}

// K2a: the CPU a GPU replica requests is admitted by whichever resource group
// covers CPU. A queue whose CPU sits in a group of its own was read as having
// no co-resource at all, so the CPU pool was never checked; and when that
// group offers several flavors, which one Kueue charges is not known.
func TestCoResourcesOfEveryGroupAreRead(t *testing.T) {
	r := &Reader{gpuNames: map[string]bool{"nvidia.com/gpu": true}}
	types := map[string]string{"h100": "", "cpu-a": "", "cpu-b": ""}

	one := r.queueState(splitGroups("cpu-a"), types)
	if one.Malformed || one.Inactive {
		t.Fatalf("queue read malformed %t inactive %t", one.Malformed, one.Inactive)
	}
	if got := one.coFlavor("cpu", "h100"); got != "cpu-a" {
		t.Fatalf("cpu of a replica on h100 is charged on %q, want cpu-a, its group's one flavor", got)
	}
	a := one
	a.Name, a.HasStatus = "a", true
	b := withCPU(member("b", QueueResource{Nominal: 8}), "cpu-a", CoQuota{Nominal: 64000, Reserved: 40000}, false)
	pools := ComputeBorrowing(topology(a, b), false).Pools
	if len(pools) != 1 || pools[0].CoFree["cpu"] != 128000-40000 || len(pools[0].CoUnbound) != 0 {
		t.Errorf("pool = %+v, want cpu free 88000 milli from the cpu-a flavor, nothing unbound", pools)
	}

	two := r.queueState(splitGroups("cpu-a", "cpu-b"), types)
	two.Name, two.HasStatus = "a", true
	pools = ComputeBorrowing(topology(two, member("b", QueueResource{Nominal: 8})), false).Pools
	if len(pools) != 1 || !slices.Equal(pools[0].CoUnbound, []string{"cpu"}) {
		t.Errorf("pool = %+v, want cpu unbound: its group offers two flavors", pools)
	}
	if _, ok := pools[0].CoFree["cpu"]; ok {
		t.Error("an unbound co-resource has no pool figure")
	}
}

// 186-4: the co-resource headroom of growth within nominal (CoUnused). A queue
// in a cohort may borrow its CPU, so its headroom is its reach: a queue of CPU
// nominal 0 holding 32 CPUs, no borrowingLimit, is not bounded at 0; with a
// borrowingLimit of 16 and nominal 64, 70 used leaves 10. A queue in no cohort
// cannot borrow: nominal less reserved. Before, every queue read N - U, so the
// CPU-nominal-0 queue that borrows its CPU was refused every replica.
func TestCoUnusedReachesWhatTheCohortLends(t *testing.T) {
	bl := int64(16000)
	borrows := withCPU(member("a", QueueResource{Nominal: 8}), "h100", CoQuota{Nominal: 0, Reserved: 32000}, true)
	if got, bounded := coUnused(borrows, "h100")["cpu"]; bounded {
		t.Errorf("cpu nominal 0, borrowing CPU without a limit: headroom %d, want no bound", got)
	}
	limited := withCPU(member("a", QueueResource{Nominal: 8}), "h100",
		CoQuota{Nominal: 64000, BorrowingLimit: &bl, Reserved: 70000}, true)
	if got := coUnused(limited, "h100")["cpu"]; got != 10000 {
		t.Errorf("cpu headroom %d, want 10000 milli (64 + 16 - 70)", got)
	}
	alone := limited
	alone.Cohort = ""
	if got := coUnused(alone, "h100")["cpu"]; got != 0 {
		t.Errorf("no cohort: cpu headroom %d, want 0 (64 - 70, clamped)", got)
	}
}

// K2b: a queue's reach of a co-resource stops at nominal plus borrowingLimit,
// as Kueue's does: 64 CPUs nominal, 16 borrowable, 70 used leaves 10.
func TestCoResourceReachStopsAtBorrowingLimit(t *testing.T) {
	r := &Reader{gpuNames: map[string]bool{"nvidia.com/gpu": true}}
	q := r.queueState(splitGroups("cpu-a"), map[string]string{"h100": "", "cpu-a": ""})
	c := q.CoResources[ResourceKey{Flavor: "cpu-a", Resource: "cpu"}]
	if c.BorrowingLimit == nil || *c.BorrowingLimit != 16000 {
		t.Fatalf("cpu borrowingLimit = %v, want 16000 milli", c.BorrowingLimit)
	}
	c.Reserved = 70000
	q.CoResources[ResourceKey{Flavor: "cpu-a", Resource: "cpu"}] = c
	q.Name, q.HasStatus = "x", true
	f := figures2(t, ComputeBorrowing(topology(q, member("b", QueueResource{Nominal: 8})), false), "x")
	if f.CoHeadroom["cpu"] != 10000 {
		t.Errorf("cpu headroom = %v, want 10000 milli (64 + 16 - 70)", f.CoHeadroom)
	}
}
