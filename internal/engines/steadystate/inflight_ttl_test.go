package steadystate

import (
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// Review minor (whole-node): the ledger held every hole for the fixed
// InFlightTTL (5m), while the borrowing governor waits the configurable
// placement timeout before it releases a replica on a Kueue flavor. With a
// 10m timeout, the hole of a borrowed replica still waiting for its node
// went free at 5m, and another decision took it while the governor still
// counted on it. A replica on a flavor is held for the longer of the two.
func TestAFlavoredHoleIsHeldForThePlacementTimeout(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	e := tenMinuteTimeoutEngine(t)
	flavor := &kueue.FlavorPlacement{NodeLabels: map[string]string{"pool": "borrowed"}}
	e.borrowing.passFlavors = map[string]*kueue.FlavorPlacement{"ns/m-v": flavor}
	now := time.Now()
	reqs := []allocation.ModelScalingRequest{growingA100("ns", "m", 8), growingA100("ns", "n", 16)}
	decisions := []domain.VariantDecision{
		{VariantName: "m-v", Namespace: "ns", AcceleratorName: "A100", TargetReplicas: 2},
		{VariantName: "n-v", Namespace: "ns", AcceleratorName: "A100", TargetReplicas: 2},
	}
	e.admitInFlight(reqs, decisions, nil, nil, now)
	at := now.Add(7 * time.Minute)
	if !allocation.DefaultInFlight.Holds("ns/m-v", at) {
		t.Fatal("the borrowed replica's hole went free at 7m, inside its 10m placement timeout")
	}
	if allocation.DefaultInFlight.Holds("ns/n-v", at) {
		t.Fatal("a replica on no flavor is held past the ledger's 5m TTL")
	}
}

// Review 187 minor: while a cluster-autoscaler scale-up is pending for an
// unplaced replica, the governor waits three placement timeouts before it
// releases it, but the ledger held its hole for one: at 10m the hole went
// free while the governor still counted on the replica for 20m more.
func TestAFlavoredHoleFollowsAPendingScaleUp(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	e := tenMinuteTimeoutEngine(t)
	e.borrowing.passFlavors = map[string]*kueue.FlavorPlacement{
		"ns/m-v": {NodeLabels: map[string]string{"pool": "borrowed"}}}
	now := time.Now()
	reqs := []allocation.ModelScalingRequest{growingA100("ns", "m", 8)}
	grow := []domain.VariantDecision{{VariantName: "m-v", Namespace: "ns", AcceleratorName: "A100", TargetReplicas: 2}}
	e.admitInFlight(reqs, grow, nil, nil, now)

	// At 9m the replica still has not placed, and a scale-up is pending.
	e.borrowing.cycle.ScaleUpPending = map[string]bool{"ns/m-v": true}
	e.admitInFlight(reqs, grow, nil, nil, now.Add(9*time.Minute))
	if !allocation.DefaultInFlight.Holds("ns/m-v", now.Add(20*time.Minute)) {
		t.Fatal("the hole went free inside the governor's tripled placement timeout")
	}
	if allocation.DefaultInFlight.Holds("ns/m-v", now.Add(31*time.Minute)) {
		t.Fatal("the hole outlived the tripled placement timeout from when it started")
	}
}

// tenMinuteTimeoutEngine is an engine whose Kueue borrowing placement
// timeout is 10m.
func tenMinuteTimeoutEngine(t *testing.T) *Engine {
	t.Helper()
	c := config.NewTestConfig()
	var p config.ScalingPolicy
	if err := yaml.Unmarshal([]byte(`
limiters:
  - type: quota
    name: q
    scope: cluster
    quotas:
      A100: 16
    kueue:
      enabled: true
      placementTimeout: 10m
`), &p); err != nil {
		t.Fatal(err)
	}
	c.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{config.GlobalDefaultsKey: p})
	e := &Engine{Config: c}
	if got := e.borrowSettings().PlacementTimeout; got != 10*time.Minute {
		t.Fatalf("setup: placement timeout %v, want 10m", got)
	}
	return e
}
