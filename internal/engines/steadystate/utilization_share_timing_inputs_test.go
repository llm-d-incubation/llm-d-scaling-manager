package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// The timings a group is planned on take its measured replica start: a group
// whose slowest role starts in 10 minutes moves on 10, and a move latency
// that grows with it.
func TestUtilizationShareTimingInputsCarryTheMeasuredStart(t *testing.T) {
	e := &Engine{}
	none := func(string, string) scaletarget.ScaleTargetAccessor { return nil }
	in := e.shareTimingInputs(allocation.ShareGroup{StartSeconds: map[string]float64{"a": 600, "b": 120}}, none)
	if in.ReplicaStart != 10*time.Minute {
		t.Fatalf("ReplicaStart %v, want the slowest role's 10m", in.ReplicaStart)
	}
	measured, _ := allocation.DeriveShareTimings(in, nil)
	plain, _ := allocation.DeriveShareTimings(e.shareTimingInputs(allocation.ShareGroup{}, none), nil)
	if measured.MoveLatency-plain.MoveLatency != 5*time.Minute {
		t.Fatalf("move latency %v against %v unmeasured; want 5m longer", measured.MoveLatency, plain.MoveLatency)
	}
}
