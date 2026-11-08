package allocation

import (
	"math"
	"reflect"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// TestShareGroupsHonourCompositeHolds: the share optimizer sizes from demand,
// not from the zeroed RequiredCapacity/SpareCapacity, so it reads the holds
// itself. A role nothing listed can size freezes the model; a role that may
// not shrink keeps a floor at what it has and gives nothing away.
func TestShareGroupsHonourCompositeHolds(t *testing.T) {
	full := []*ResourceConstraints{{Pools: map[string]ResourcePool{"A100": {Limit: 16, Used: 16}}}}
	build := func(hold *RoleHold) ShareGroup {
		t.Helper()
		req := shareReq("A", "ns", 800, 5) // a need of 1 GPU, holding 5
		if hold != nil {
			req.CompositeSignal.Holds = map[string]RoleHold{domain.RoleBoth: *hold}
		}
		groups := BuildShareGroups([]ModelScalingRequest{req}, full, ShareGroupOptions{ClusterIsQuota: true})
		if len(groups) != 1 {
			t.Fatalf("got %d groups, want 1", len(groups))
		}
		return groups[0]
	}

	free := build(nil)
	if len(free.Roles) != 1 || free.Roles[0].Floor != 1 {
		t.Fatalf("setup: unheld role %+v, want one role with floor 1", free.Roles)
	}
	if _, ok := free.Give["ns/A/both"]; !ok {
		t.Fatal("setup: an unheld role with spare can give")
	}

	down := build(&RoleHold{NoScaleDown: true})
	if len(down.Roles) != 1 || down.Roles[0].Floor != 5 {
		t.Fatalf("no scale-down: roles %+v, want one role with floor 5 (what it holds)", down.Roles)
	}
	if _, ok := down.Give["ns/A/both"]; ok {
		t.Fatal("no scale-down: the role must give nothing")
	}

	// Another analyzer reads the role as demand, at the share's threshold,
	// never as a floor: one that agrees (demand 800 of the same 5000)
	// leaves need and floor as saturation alone set them. One reading 2400
	// of 4000 can spare 1000 at the share's 0.8, so the role keeps 4000
	// (need 4 GPUs) -- 4.25 if read at full utilization -- and can still
	// give.
	framed := func(supply, demand float64) ShareRole {
		t.Helper()
		req := shareReq("A", "ns", 800, 5)
		req.CompositeSignal.OtherFrames = map[string]OtherFrames{domain.RoleBoth: {
			Supply: 5000, Frames: []FrameReading{{Supply: supply, Demand: demand, Ratio: 1}}}}
		groups := BuildShareGroups([]ModelScalingRequest{req}, full, ShareGroupOptions{ClusterIsQuota: true})
		if len(groups) != 1 || len(groups[0].Roles) != 1 {
			t.Fatalf("other frame %v: groups %+v, want one role", demand, groups)
		}
		if _, ok := groups[0].Give["ns/A/both"]; !ok {
			t.Fatalf("other frame %v: the role must still be able to give", demand)
		}
		return groups[0].Roles[0]
	}
	if r := framed(5000, 800); !reflect.DeepEqual(r, free.Roles[0]) {
		t.Fatalf("agreeing frame: role %+v, want %+v as saturation alone", r, free.Roles[0])
	}
	if r := framed(4000, 2400); math.Abs(r.Need-4) > 1e-9 || r.Floor != 1 {
		t.Fatalf("tighter frame: role %+v, want need 4 and floor 1", r)
	}

	both := build(&RoleHold{NoScaleUp: true, NoScaleDown: true})
	if len(both.Roles) != 0 || len(both.Frozen) != 1 {
		t.Fatalf("held both ways: roles %+v frozen %v, want the model frozen", both.Roles, both.Frozen)
	}
}
