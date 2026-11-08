package scalefromzero

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// Review WN3: the wake fitted a replica by accelerator and taints only
// (shapeOf), never by its pod templates' nodeSelector or required node
// affinity. A P/D model pins its prefill to role=prefill nodes and its
// decode to role=decode nodes; with two prefill nodes free and the decode
// node full, the pair woke, its decode Pending for good. The wake fits the
// decode alone where it can land -- here nowhere: nothing wakes.
//
// The node store, the usage store and the in-flight ledger are process-wide:
// no t.Parallel.
func TestAPinnedPairWakesOnlyWhereEachRoleLands(t *testing.T) {
	publishUsage(t)
	pinned := func(name, role string) Candidate {
		c := cand(name, role, wakeHBM3, 8, 1)
		c.Placements = []domain.PodPlacement{{NodeSelector: map[string]string{"role": role}}}
		return c
	}
	for _, tc := range []struct {
		name     string
		decodeOK bool
		want     int
	}{
		{"only prefill nodes free: nothing wakes", false, 0},
		{"control: the decode node free too: the pair wakes", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decodeUsed := 8
			if tc.decodeOK {
				decodeUsed = 0
			}
			allocation.DefaultInFlight.Reset()
			decision.DefaultNodeGPUs.Publish(map[string]decision.NodeGPU{
				"p1": {Accelerator: wakeHBM3, Capacity: 8, Labels: map[string]string{"role": "prefill"}},
				"p2": {Accelerator: wakeHBM3, Capacity: 8, Labels: map[string]string{"role": "prefill"}},
				"d1": {Accelerator: wakeHBM3, Capacity: 8, Used: decodeUsed, Labels: map[string]string{"role": "decode"}},
			}, time.Now())
			t.Cleanup(func() {
				decision.DefaultNodeGPUs.Publish(nil, time.Time{})
				allocation.DefaultInFlight.Reset()
			})
			e := physicalEngine(24)
			e.beginTick()
			_, selected, outcome := e.decideWake(context.Background(), modelGroup{namespace: "a", modelID: "pd"},
				[]Candidate{pinned("pd-decode", "decode"), pinned("pd-prefill", "prefill")}, coverage{})
			if len(selected) != tc.want {
				t.Fatalf("woke %v (%s), want %d variants", names(selected), outcome, tc.want)
			}
		})
	}
}
