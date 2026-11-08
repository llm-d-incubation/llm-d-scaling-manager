package scalefromzero

import (
	"context"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// A wake resolves the model under the tiers of ALL its variants, running ones
// included, as the planner does. Grouped from the parked variants alone, a P/D
// model whose decode is tagged and running and whose prefill is parked woke its
// prefill under the wrong tier: no requirePrefill, and the default weight.
func TestWakeResolvesTiersOfRunningVariants(t *testing.T) {
	on := true
	doc := "limiters:\n  - type: quota\n    name: q\n    scope: cluster\n    quotas:\n      A100: 16\n" +
		"optimizer:\n  type: utilizationShare\n"
	var def config.ScalingPolicy
	if err := yaml.Unmarshal([]byte(doc), &def); err != nil {
		t.Fatal(err)
	}
	critical := config.ScalingPolicy{
		WeightClass:   "critical",
		ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on},
	}
	c := config.NewTestConfig()
	c.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		config.GlobalDefaultsKey: def,
		"a-tier":                 critical,
		"b-tier":                 {WeightClass: "standard"},
	})
	tagged := func(name, tier string) llmdvariant.Variant {
		v := va(name, selNS, "m", name+"-deploy")
		v.Spec.ScalingPolicy = tier
		return v
	}

	cases := []struct {
		name      string
		decode    string // tier the running decode names
		prefill   string // tier the parked prefill names
		wantTiers []string
	}{
		{name: "decode tagged, prefill untagged", decode: "a-tier", prefill: "", wantTiers: []string{"a-tier"}},
		{name: "only the larger-named tier is parked", decode: "a-tier", prefill: "b-tier",
			wantTiers: []string{"a-tier", "b-tier"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			active := []llmdvariant.Variant{tagged("dec", tc.decode)}
			inactive := []llmdvariant.Variant{tagged("pre", tc.prefill)}
			groups := groupInactiveByModel(inactive, active)
			if len(groups) != 1 || len(groups[0].variants) != 1 {
				t.Fatalf("got %d groups, want one group holding only the parked prefill", len(groups))
			}
			group := groups[0]
			if got := group.tiers(); !slices.Equal(got, tc.wantTiers) {
				t.Fatalf("tiers = %v, want %v", got, tc.wantTiers)
			}

			e := &Engine{config: c}
			if !e.requirePrefill(group) {
				t.Fatal("requirePrefill = false, want the running decode's tier to require prefill")
			}

			// The decode is serving, so the wake claims for the prefill alone.
			// Weighed under a-tier (critical, z=-4) it beats a receiver at
			// z=-2; under the default or b-tier (standard, z=-1) it does not.
			freshClaims(t, -2)
			candidates := []Candidate{cand("pre", domain.RolePrefill, "A100", 1, 1)}
			set, ok := e.claimShareTransfer(context.Background(), group, candidates, nil, coverage{decode: true})
			if !ok || len(set) != 1 || set[0].VariantName != "pre" {
				t.Fatalf("claimed %+v (%v), want the prefill weighed under a-tier", set, ok)
			}
		})
	}
}
