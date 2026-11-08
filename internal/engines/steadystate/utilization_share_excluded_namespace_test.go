package steadystate

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// namespaceQuotaDoc is one namespace-scoped quota and no cluster-scoped one:
// team-a may hold 8 A100s, team-b 6, and ns-x bypasses it.
const namespaceQuotaDoc = `
limiters:
  - type: quota
    name: tenants
    scope: namespace
    exclude: [ns-x]
    namespaceQuotas:
      team-a:
        A100: 8
      team-b:
        A100: 6
`

func namespaceQuotaConfig(t *testing.T) *config.Config {
	t.Helper()
	var p config.ScalingPolicy
	if err := yaml.Unmarshal([]byte(namespaceQuotaDoc+selectedShadow), &p); err != nil {
		t.Fatal(err)
	}
	c := config.NewTestConfig()
	c.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{config.GlobalDefaultsKey: p})
	return c
}

// The engine told BuildShareGroups the cluster group is a quota whenever any
// limiter entry was a quota. With a namespace-scoped entry alone it is not:
// the same cluster-wide pool must form a group under a cluster quota and none
// under a namespace quota, whose sum bounds no namespace it excludes.
func TestUtilizationShareClusterGroupNeedsAClusterQuota(t *testing.T) {
	reqs := []allocation.ModelScalingRequest{
		shadowRequest("A", "", 4000, 9),
		shadowRequest("B", "", 6000, 5),
		shadowRequest("C", "", 1000, 2),
	}
	for _, c := range []struct {
		name   string
		config func(*testing.T) *config.Config
		groups int
	}{
		{"cluster quota", func(t *testing.T) *config.Config { return shadowConfig(t, selectedShadow) }, 1},
		{"namespace quota alone", namespaceQuotaConfig, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			reg := freshMetrics(t)
			ctx, _ := observe(t)
			e := &Engine{Config: c.config(t)}
			e.evaluateUtilizationShare(ctx, reqs, fullQuota(), nil)
			if got := len(family(t, reg, constants.LLMSMUtilizationShareReplicasToMove)); got != c.groups {
				t.Errorf("published %d groups, want %d", got, c.groups)
			}
		})
	}
}

// A model in a namespace the namespace quota excludes, under the constraints
// that quota really produces: no group is formed for it, and the engine says
// once that today's optimizer keeps it, and why.
func TestUtilizationShareReportsAnExcludedNamespaceUnshared(t *testing.T) {
	reg := freshMetrics(t)
	ctx, logs := observe(t)
	e := &Engine{Config: namespaceQuotaConfig(t)}
	idle := shadowRequest("X", "", 100, 14)
	idle.Namespace = excludedNamespace
	reqs := []allocation.ModelScalingRequest{idle}
	q := e.Config.EffectiveQuotaEntries()
	if len(q) != 1 {
		t.Fatalf("want one quota entry, got %d", len(q))
	}
	rc, err := allocation.NewDefaultLimiter(q[0].Name, allocation.NewQuotaInventory(q[0])).ComputeConstraints(
		context.Background(), map[string]int{"A100": 18},
		map[string]map[string]int{"team-a": {"A100": 2}, "team-b": {"A100": 2}, excludedNamespace: {"A100": 14}})
	if err != nil {
		t.Fatal(err)
	}
	cons := []*allocation.ResourceConstraints{rc}

	e.evaluateUtilizationShare(ctx, reqs, cons, nil)
	e.evaluateUtilizationShare(ctx, reqs, cons, nil)

	if got := family(t, reg, constants.LLMSMUtilizationShareReplicasToMove); len(got) != 0 {
		t.Errorf("published %d groups for a namespace no quota bounds, want none", len(got))
	}
	lines := logs.FilterMessage(unsharedMessage).All()
	if len(lines) != 1 {
		t.Fatalf("want the unshared model reported once over two cycles, got %d lines", len(lines))
	}
	models, ok := lines[0].ContextMap()["models"].([]any)
	if want := "ns-x/X: " + allocation.UnsharedExcludedNamespace("A100"); !ok || len(models) != 1 || models[0] != want {
		t.Errorf("models = %v, want [%s]", lines[0].ContextMap()["models"], want)
	}

	// The model goes away: nothing new to say, and no second line.
	e.evaluateUtilizationShare(ctx, nil, cons, nil)
	if got := len(logs.FilterMessage(unsharedMessage).All()); got != 1 {
		t.Fatalf("after the model left: %d lines, want still 1", got)
	}
	// It comes back: that is a change, and it is reported again.
	e.evaluateUtilizationShare(ctx, reqs, cons, nil)
	if got := len(logs.FilterMessage(unsharedMessage).All()); got != 2 {
		t.Fatalf("after the model returned: %d lines, want 2", got)
	}
}

// excludedNamespace is the namespace namespaceQuotaDoc excludes.
const excludedNamespace = "ns-x"

const unsharedMessage = "Utilization share: not sharing models no quota bounds; the default optimizer keeps them"

// Two unshared models reported in either order are the same set: one line,
// the models sorted.
func TestUtilizationShareReportsUnsharedModelsOnceInAnyOrder(t *testing.T) {
	freshMetrics(t)
	ctx, logs := observe(t)
	e := &Engine{Config: namespaceQuotaConfig(t)}
	x := shadowRequest("X", "", 100, 2)
	x.Namespace = excludedNamespace
	y := shadowRequest("Y", "", 100, 2)
	y.Namespace = excludedNamespace
	q := e.Config.EffectiveQuotaEntries()
	if len(q) != 1 {
		t.Fatalf("want one quota entry, got %d", len(q))
	}
	rc, err := allocation.NewDefaultLimiter(q[0].Name, allocation.NewQuotaInventory(q[0])).ComputeConstraints(
		context.Background(), map[string]int{"A100": 4}, map[string]map[string]int{excludedNamespace: {"A100": 4}})
	if err != nil {
		t.Fatal(err)
	}
	cons := []*allocation.ResourceConstraints{rc}

	e.evaluateUtilizationShare(ctx, []allocation.ModelScalingRequest{y, x}, cons, nil)
	e.evaluateUtilizationShare(ctx, []allocation.ModelScalingRequest{x, y}, cons, nil)

	lines := logs.FilterMessage(unsharedMessage).All()
	if len(lines) != 1 {
		t.Fatalf("want one line for the same two models in either order, got %d", len(lines))
	}
	why := allocation.UnsharedExcludedNamespace("A100")
	models, ok := lines[0].ContextMap()["models"].([]any)
	if !ok || len(models) != 2 || models[0] != "ns-x/X: "+why || models[1] != "ns-x/Y: "+why {
		t.Errorf("models = %v, want ns-x/X then ns-x/Y", lines[0].ContextMap()["models"])
	}
}
