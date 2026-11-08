package steadystate

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// A scale-from-zero wake claim wins back what the planner would give the
// model, so it must weigh the model under the same tier the planner does. With
// variants on two tiers of different weights, the wake's weight must equal the
// planner's, not the highest of the two.
func TestWakeClaimWeighsTheModelUnderThePlannersTier(t *testing.T) {
	const ns, model = "ns", "m"
	doc := "limiters:\n  - type: quota\n    name: q\n    scope: cluster\n    quotas:\n      A100: 16\n" +
		"optimizer:\n  type: utilizationShare\n"
	var def config.ScalingPolicy
	if err := yaml.Unmarshal([]byte(doc), &def); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		config.GlobalDefaultsKey: def,
		"bulk":                   {WeightClass: "best-effort"},
		"hot":                    {WeightClass: "critical"},
	})
	us, selected, err := cfg.UtilizationShare()
	if err != nil || !selected {
		t.Fatalf("setup: utilization share not selected (%v)", err)
	}

	for _, tiers := range [][]string{{"hot"}, {"hot", "bulk"}, {"bulk", "hot", ""}} {
		vas := make([]llmdvariant.Variant, len(tiers))
		for i, tier := range tiers {
			vas[i].Spec.ScalingPolicy = tier
		}
		// The planner: the model's resolved policy carries the weight into its
		// scaling request (collectV2ModelRequest).
		resolved := (&Engine{Config: cfg}).resolveModelPolicy(context.Background(),
			cfg.ScalingPolicyConfigForNamespace(ns), model, ns, vas)
		planner, _ := us.Weight(resolved.WeightClass, resolved.Weight)

		// The wake: scale-from-zero passes the tiers its variants name.
		zNs, _, ok := cfg.UtilizationShareWakeScores(ns, model, config.ModelTiers(tiers))
		if !ok {
			t.Fatal("wake scores not available")
		}
		if -zNs != planner {
			t.Errorf("tiers %v: wake weight %v, planner weight %v; they must agree", tiers, -zNs, planner)
		}
	}
}
