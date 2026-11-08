package steadystate

import (
	"slices"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// A receiver credited with returns still releasing (awaiting-return) says so
// on its own model, as a transfer's receiver does: the group's withheld
// counter names no model, and a LeaderWorkerSet model short for a release
// would otherwise have no reason anywhere.
func TestUtilizationShareAwaitingReturnIsAModelsReason(t *testing.T) {
	g := allocation.ShareGroup{
		Budget: 80,
		Roles: []allocation.ShareRole{
			{Key: "ns/R/both", Weight: 1, Need: 40, Floor: 16, Ceiling: 32, ReplicaGPUs: 16},
			{Key: "ns/D/both", Weight: 1, Need: 2, Floor: 8, Ceiling: 96, ReplicaGPUs: 8},
		},
		Committed:  map[string]int{"ns/R/both": 16, "ns/D/both": 48},
		Thresholds: map[string]float64{"ns/R/both": 0.8, "ns/D/both": 0.8},
		Origins: map[string]allocation.ShareRoleOrigin{
			"ns/R/both": {Namespace: "ns", ModelID: "R"}, "ns/D/both": {Namespace: "ns", ModelID: "D"}},
	}
	ev := allocation.EvaluateShare(g.Roles, g.Committed, g.Thresholds, g.Budget, 0.15)
	reasons := func(unfunded map[string]string) []string {
		return shareBlockedReasons(allocation.NewShareLedger(), g, ev, unfunded, nil, allocation.ShareSkipWaits{},
			time.Unix(0, 0), allocation.ShareTimings{})["ns/R"]
	}
	if got := reasons(map[string]string{"ns/R/both": allocation.ShareUnfundedAwaitingReturn}); !slices.Contains(got,
		constants.ScalingBlockedAwaitingRelease) {
		t.Fatalf("R, waiting on returns, reports %v, want %s", got, constants.ScalingBlockedAwaitingRelease)
	}
	if got := reasons(nil); slices.Contains(got, constants.ScalingBlockedAwaitingRelease) {
		t.Fatalf("control: R, waiting on nothing, reports %v", got)
	}
}
