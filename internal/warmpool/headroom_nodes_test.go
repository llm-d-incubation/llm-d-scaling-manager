package warmpool

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/pool"
)

// Review A4: the warm pool sized itself in GPUs, against an aggregate. Twelve
// free H100s as 4+4+4 read as one more 8-GPU Pod, which then sat Pending: a
// Pod lands on one node. Whole 8-GPU units.
func TestWarmPoolGrowsOnlyByPodsANodeHolds(t *testing.T) {
	for _, c := range []struct {
		name  string
		holes []int
		want  int32
	}{
		{"12 free as 4+4+4 affords no 8-GPU Pod", []int{4, 4, 4}, 1},
		{"12 free as 8+4 affords one", []int{8, 4}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			decision.ResetWarmPoolGPUs()
			t.Cleanup(decision.ResetWarmPoolGPUs)
			r, published := eightGPUPodPool(t)

			store := decision.NewHeadroomStore()
			now := time.Now()
			store.PublishBoundsOnNodes(nil, map[string]int{"A100": 12},
				map[string]decision.NodeHoles{"A100": {Free: c.holes}}, 1<<62, now)
			r.Headroom = func(ns, acc string, v uint64) (int, decision.HeadroomState) {
				return store.Reading(ns, acc, v, time.Minute, now)
			}
			r.NodeUnits = func(acc string, unit int, v uint64) (int, bool) {
				return store.Units(acc, unit, v, time.Minute, now)
			}
			if _, err := r.Once(context.Background()); err != nil {
				t.Fatalf("Once: %v", err)
			}
			if *published != c.want {
				t.Fatalf("published %d, want %d", *published, c.want)
			}
		})
	}
}

// eightGPUPodPool is onePodPool with an 8-GPU Pod: at one Pod, wanting two.
func eightGPUPodPool(t *testing.T) (*Reconciler, *int32) {
	t.Helper()
	held := withAccel(pool.Membership{Pod: podA(), State: pool.Absent, Pool: "p"}, "A100")
	held.Capacity.GPUs = 8
	p := &fakePool{memberships: []pool.Membership{held}}
	cfg := testConfig()
	cfg.SleepMinSize = 1

	var published int32
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{{Name: "p", Config: cfg, Replicas: 1, Deployment: "p"}}
	r.PublishSize = func(_, _ string, replicas int32) { published = replicas }
	return r, &published
}

func TestHeadroomUnitsCountWholeUnitsPerNode(t *testing.T) {
	store := decision.NewHeadroomStore()
	now := time.Now()
	store.PublishBoundsOnNodes(nil, map[string]int{"H100": 12},
		map[string]decision.NodeHoles{"NVIDIA-H100-80GB-HBM3": {Free: []int{4, 4, 4}}}, 1, now)
	for _, c := range []struct {
		unit      int
		want      int
		wantKnown bool
	}{
		{8, 0, true}, // no node holds 8
		{4, 3, true}, // each holds one
		// Larger than any node: a warm-pool Pod is one Pod, and no node holds
		// it. Review round 2 (whole-node owner): this read "unknown", and the
		// pool then grew by GPUs alone into Pods that stay Pending.
		{16, 0, true},
	} {
		got, known := store.Units("H100", c.unit, 1, time.Minute, now)
		if got != c.want || known != c.wantKnown {
			t.Errorf("Units(H100, %d) = (%d, %v), want (%d, %v)", c.unit, got, known, c.want, c.wantKnown)
		}
	}
	if _, known := store.Units("H100", 8, 2, time.Minute, now); known {
		t.Error("a snapshot older than the pool's charge must not answer")
	}
	store.PublishBounds(nil, map[string]int{"H100": 12}, 1, now)
	if _, known := store.Units("H100", 8, 1, time.Minute, now); known {
		t.Error("no node holes published (no physical limiter): units must be unknown")
	}
}
