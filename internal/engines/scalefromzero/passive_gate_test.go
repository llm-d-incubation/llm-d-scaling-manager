package scalefromzero

import (
	"testing"

	"github.com/go-logr/logr/funcr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/passive"
)

// The scale-from-zero engine takes no action for a passive model with pending
// requests: processInactiveModel returns at passiveGate, before wakeVerdict
// holds a node hole or draws borrowed capacity and before publishActivation
// writes the decision. The gate is tested directly because driving
// processInactiveModel to it takes a live EPP scrape (its queue metric); the
// call site is the line before wakeVerdict.
func TestPassiveGateStopsTheWake(t *testing.T) {
	cfg := config.NewTestConfig()
	yes := true
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		"m": {ModelID: "acme/chat", Namespace: "ns", Passive: &yes}})
	lines := 0
	logger := funcr.New(func(_, _ string) { lines++ }, funcr.Options{})
	e := &Engine{Passive: &passive.Resolver{Config: cfg}}

	if !e.passiveGate(logger, modelGroup{namespace: "ns", modelID: "acme/chat"}) {
		t.Fatal("a passive model passed the gate")
	}
	e.passiveGate(logger, modelGroup{namespace: "ns", modelID: "acme/chat"})
	if lines != 1 {
		t.Fatalf("logged %d lines over two ticks, want 1", lines)
	}
	if e.passiveGate(logger, modelGroup{namespace: "ns", modelID: "acme/other"}) {
		t.Fatal("an active model was stopped at the gate")
	}
	if (&Engine{}).passiveGate(logger, modelGroup{namespace: "ns", modelID: "acme/chat"}) {
		t.Fatal("an engine without a resolver stopped a model")
	}
}
