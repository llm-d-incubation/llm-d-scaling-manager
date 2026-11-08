package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// restoreHolds restores into DefaultInFlight a hold of want replicas, placed
// of them placed, for each key, as a restart reads them back.
func restoreHolds(t *testing.T, now time.Time, want, placed int, keys ...string) {
	t.Helper()
	before := allocation.NewInFlightLedger()
	for _, k := range keys {
		before.Admit(k, allocation.ReplicaShape{Variant: k, Accelerator: "A100", GPUs: 8}, want, placed, nil, now)
	}
	raw, err := before.MarshalInFlight(now)
	if err != nil {
		t.Fatal(err)
	}
	p, err := allocation.UnmarshalInFlight(raw)
	if err != nil {
		t.Fatal(err)
	}
	allocation.DefaultInFlight.RestoreInFlight(p, now, allocation.InFlightTTL)
}

// Issue ev-shindin/llm-scaling-manager#215: the first fitted cycle after a restart ended every restored
// hold it did not admit, so a model that cycle skipped -- its metrics
// missing -- lost its Unschedulable replica's hold. A restored hold of a
// variant the cycle enumerated and did not decide is kept, cycle after
// cycle, until a cycle admits it or it expires by its own clock. One of a
// variant gone, or decided without being admitted, still ends.
//
// DefaultInFlight is process-wide: no t.Parallel.
func TestARestoredHoldSurvivesACycleThatSkipsItsVariant(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	now := time.Now()
	restoreHolds(t, now, 3, 2, "ns/m-v", "ns/gone-v", "ns/o-v")

	// The cycle skips model m (enumerated, no request), decides o and the
	// share plans it (so it is not admitted), and gone-v is not there.
	o := growingA100("ns", "o", 8)
	atOne := []domain.VariantDecision{{VariantName: "o-v", Namespace: "ns", AcceleratorName: "A100", TargetReplicas: 1}}
	e := &Engine{cycleVariants: map[string]bool{"ns/m-v": true, "ns/o-v": true}}
	planned := map[string]bool{"ns/o": true}
	for i := 1; i <= 3; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		e.admitInFlight([]allocation.ModelScalingRequest{o}, atOne, planned, nil, at)
		if !allocation.DefaultInFlight.Holds("ns/m-v", at) {
			t.Fatalf("cycle %d: the restored hold of a variant the cycle skipped was ended", i)
		}
	}
	if allocation.DefaultInFlight.Holds("ns/gone-v", now.Add(time.Second)) {
		t.Fatal("control: the restored hold of a variant that is gone still holds its node")
	}
	if allocation.DefaultInFlight.Holds("ns/o-v", now.Add(time.Second)) {
		t.Fatal("control: the restored hold of a variant the cycle decided and did not admit still holds its node")
	}
	if allocation.DefaultInFlight.Holds("ns/m-v", now.Add(allocation.InFlightTTL+time.Second)) {
		t.Fatal("a skipped variant's restored hold outlived its own clock")
	}

	// Its model comes back: the cycle admits the Unschedulable replica, and
	// the hold goes on as a live one.
	m := growingA100("ns", "m", 8)
	m.VariantStates[0].CurrentReplicas, m.VariantStates[0].FilledReplicas, m.VariantStates[0].HeldKnown = 3, 2, true
	m.VariantStates[0].UnplacedReplicas = 1
	at3 := []domain.VariantDecision{{VariantName: "m-v", Namespace: "ns", AcceleratorName: "A100", TargetReplicas: 3}}
	at := now.Add(4 * time.Second)
	e.admitInFlight([]allocation.ModelScalingRequest{m}, at3, nil, nil, at)
	if !allocation.DefaultInFlight.Holds("ns/m-v", at) {
		t.Fatal("the cycle that admitted the skipped variant again ended its restored hold")
	}
}

// The persisted ledger names each hold's scale target, so a restart can
// count its pods.
func TestTheInFlightLedgerPersistsTheScaleTarget(t *testing.T) {
	now := time.Now()
	l := allocation.NewInFlightLedger()
	l.Commit(nil, []allocation.InFlightAdmission{{Key: "ns/m-v", Shape: allocation.ReplicaShape{Variant: "ns/m-v", Accelerator: "A100", GPUs: 8},
		Want: 3, PlacedNow: 2, Target: allocation.ScaleTargetRef{Kind: "LeaderWorkerSet", Name: "m-lws"}}}, now)
	raw, err := l.MarshalInFlight(now)
	if err != nil {
		t.Fatal(err)
	}
	p, err := allocation.UnmarshalInFlight(raw)
	if err != nil {
		t.Fatal(err)
	}
	if pe := p.Entries["ns/m-v"]; pe.TargetKind != "LeaderWorkerSet" || pe.TargetName != "m-lws" {
		t.Fatalf("the persisted hold names scale target %q/%q, want LeaderWorkerSet/m-lws", pe.TargetKind, pe.TargetName)
	}
}
