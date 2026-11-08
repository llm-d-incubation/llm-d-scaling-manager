package allocation

import (
	"testing"
	"time"
)

// Review minor (architect): only the steady-state engine settles the shared
// borrow record (ObserveAt), and only for the models in its cycle. A wake's
// draw whose model the cycles did not observe -- an idle fleet runs no
// optimizing cycle -- stayed in the record, and counted against every later
// wake and borrow, until some cycle observed it. A draw ordered InFlightTTL
// ago no longer counts, observed or not.
func TestAWakeDrawExpiresWithoutAnObservation(t *testing.T) {
	b := NewBorrowInFlight()
	t0 := time.Unix(10_000, 0)
	b.RecordWake([]BorrowDraw{{Namespace: "b", ModelID: "m", Variant: "m-d", Cohort: "co", Flavor: "f",
		Resource: "nvidia.com/gpu", GPUs: 8, Replicas: 1, Target: 1}}, t0)
	if got := b.Unreserved(t0.Add(time.Minute)).InFlight; len(got) != 1 {
		t.Fatalf("setup: the wake's draw is not in flight a minute after it: %+v", got)
	}
	if got := b.Unreserved(t0.Add(InFlightTTL + time.Second)).InFlight; len(got) != 0 {
		t.Fatalf("the wake's draw still counts past InFlightTTL with no observation: %+v", got)
	}
}
