package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// Review W8 / A7: the ledger keeps one flavor per variant, and growth that
// named none dropped the borrowed flavor its earlier replicas were admitted
// on. The flavor stays while the borrowed replicas lag (their pods exist, a
// read has not shown them), and this cycle's pass names its own -- the
// queue's, for guaranteed growth.
func TestTheLedgerKeepsTheFlavorOfALaggingBorrowedReplica(t *testing.T) {
	t0 := time.Unix(95_000, 0)
	flavor := &kueue.FlavorPlacement{AcceleratorType: "H100", NodeLabels: map[string]string{"pool": "a"}}
	e := &Engine{}
	e.borrowing.inFlight = allocation.NewBorrowInFlight()
	e.borrowing.inFlight.RecordAt([]allocation.BorrowDraw{{Namespace: "a", Variant: "v", Cohort: "c", Flavor: "h100",
		Resource: "nvidia.com/gpu", GPUs: 8, Replicas: 1, Target: 2, Placement: flavor}}, t0)
	// The borrowed replica's pod exists now: the draw resolves and lags.
	e.borrowing.inFlight.ObserveAt(map[string]allocation.VariantObservation{"a/v": {Current: 2, Desired: 2}}, t0.Add(30*time.Second))
	if got := e.borrowedFlavors(t0.Add(30 * time.Second))["a/v"]; got != flavor {
		t.Fatalf("the hole of a lagging borrowed replica is held on %+v, want its flavor", got)
	}

	// This cycle's pass placed the variant on its queue's flavor: that wins.
	queue := &kueue.FlavorPlacement{AcceleratorType: "H100", NodeLabels: map[string]string{"pool": "q"}}
	e.borrowing.passFlavors = map[string]*kueue.FlavorPlacement{"a/v": queue}
	if got := e.borrowedFlavors(t0.Add(30 * time.Second))["a/v"]; got != queue {
		t.Fatalf("the pass's flavor %+v is not the one held", got)
	}
}
