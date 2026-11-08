package allocation

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
)

// Review A4: the warm pool's headroom was published from the constraints before
// the pass, per model, and in GPUs only. It is published once the pass is over,
// from what the pass left: the GPUs it ordered and the holes it filled. 28 free
// as 8+8+4+4+4, a model taking two 8-GPU replicas: 12 GPUs are left, in holes
// no 8-GPU Pod fits.
func TestWarmPoolHeadroomIsWhatThePassLeft(t *testing.T) {
	decision.DefaultHeadroom.Reset()
	t.Cleanup(decision.DefaultHeadroom.Reset)
	got, _ := optimizeOnNodes(h100Nodes(8, 8, 4, 4, 4), famWanting("x", "m", famHBM3, 2, 8))
	if got["m-v"] != 2 {
		t.Fatalf("ordered %d, want 2", got["m-v"])
	}
	now := time.Now()
	if free, state := decision.GPUHeadroomReading("pool-ns", famHBM3, 0, time.Minute, now); state != decision.HeadroomBounded || free != 12 {
		t.Errorf("headroom after the pass: %d (%v), want 12 bounded (28 free less the 16 ordered)", free, state)
	}
	if units, known := decision.GPUHeadroomUnits(famHBM3, 8, 0, time.Minute, now); !known || units != 0 {
		t.Errorf("8-GPU Pods the nodes still hold: %d (known %v), want 0: the holes left are 4+4+4", units, known)
	}
}
