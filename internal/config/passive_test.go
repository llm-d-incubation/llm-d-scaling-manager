package config

import "testing"

// TestResolvePassivePrecedence pins the order a model's passive mode resolves
// in: per-model entry, then the tier, then the default entry (the namespace's
// own map replacing the controller's), then LLMSM_PASSIVE.
func TestResolvePassivePrecedence(t *testing.T) {
	const ns, model = "prod", "acme/chat"
	perModel := func(v bool) ScalingPolicy {
		return ScalingPolicy{ModelID: model, Namespace: ns, Passive: boolPtr(v)}
	}
	cases := []struct {
		name       string
		flag       bool
		global     map[string]ScalingPolicy
		local      map[string]ScalingPolicy
		tiers      []string
		want       bool
		wantSource string
	}{
		{name: "nothing set is active", want: false, wantSource: PassiveSourceFlag},
		{name: "the flag alone", flag: true, want: true, wantSource: PassiveSourceFlag},
		{name: "default over the flag", flag: true,
			global: map[string]ScalingPolicy{GlobalDefaultsKey: {Passive: boolPtr(false)}},
			want:   false, wantSource: PassiveSourceDefault},
		{name: "a tier over the default",
			global: map[string]ScalingPolicy{GlobalDefaultsKey: {Passive: boolPtr(true)}, "batch": {Passive: boolPtr(false)}},
			tiers:  []string{"batch"}, want: false, wantSource: PassiveSourceTierPrefix + "batch"},
		{name: "defaultPolicy names the tier for a variant that names none",
			global: map[string]ScalingPolicy{GlobalDefaultsKey: {DefaultPolicy: "lab"}, "lab": {Passive: boolPtr(true)}},
			tiers:  []string{""}, want: true, wantSource: PassiveSourceTierPrefix + "lab"},
		{name: "passive globally, active for one model", flag: true,
			global: map[string]ScalingPolicy{GlobalDefaultsKey: {Passive: boolPtr(true)}, "m": perModel(false)},
			want:   false, wantSource: PassiveSourcePerModel},
		{name: "active globally, passive for one model",
			global: map[string]ScalingPolicy{"m": perModel(true)},
			want:   true, wantSource: PassiveSourcePerModel},
		{name: "any tier passive makes the model passive",
			global: map[string]ScalingPolicy{"a": {Passive: boolPtr(false)}, "b": {Passive: boolPtr(true)}},
			tiers:  []string{"a", "b"}, want: true, wantSource: PassiveSourceTierPrefix + "b"},
		{name: "a variant that names no tier counts too",
			global: map[string]ScalingPolicy{GlobalDefaultsKey: {Passive: boolPtr(true)}, "gold": {Passive: boolPtr(false)}},
			tiers:  []string{"gold", ""}, want: true, wantSource: PassiveSourceDefault},
		{name: "a per-model false wins over every tier",
			global: map[string]ScalingPolicy{"a": {Passive: boolPtr(true)}, "m": perModel(false)},
			tiers:  []string{"a"}, want: false, wantSource: PassiveSourcePerModel},
		{name: "the namespace's own default replaces the controller's",
			global: map[string]ScalingPolicy{GlobalDefaultsKey: {Passive: boolPtr(false)}},
			local:  map[string]ScalingPolicy{GlobalDefaultsKey: {Passive: boolPtr(true)}},
			want:   true, wantSource: PassiveSourceNamespaceDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := NewTestConfig()
			SetPassiveDefaultForTest(cfg, tc.flag)
			if tc.global != nil {
				cfg.UpdateScalingPolicyConfig(tc.global)
			}
			if tc.local != nil {
				cfg.UpdateScalingPolicyConfigForNamespace(ns, tc.local)
			}
			got, source := ResolvePassive(cfg, ns, model, tc.tiers)
			if got != tc.want || source != tc.wantSource {
				t.Errorf("ResolvePassive = (%v, %q), want (%v, %q)", got, source, tc.want, tc.wantSource)
			}
		})
	}
}

// TestMergeCarriesPassiveBothWays: a pointer, so a per-model false turns a
// passive default back to active -- which a plain bool under Merge's
// one-way overlay could never do.
func TestMergeCarriesPassiveBothWays(t *testing.T) {
	base := ScalingPolicy{Passive: boolPtr(true)}
	base.Merge(ScalingPolicy{Passive: boolPtr(false)})
	if base.Passive == nil || *base.Passive {
		t.Fatalf("a set false must win over a true below it, got %v", base.Passive)
	}
	base.Merge(ScalingPolicy{})
	if base.Passive == nil || *base.Passive {
		t.Fatalf("an entry that does not set passive must inherit, got %v", base.Passive)
	}
}

// TestPassiveIsAKnownKey: an unknown key is ignored with a warning, so a
// schema that forgot `passive` would silently leave the model active.
func TestPassiveIsAKnownKey(t *testing.T) {
	if unknown := UnknownScalingPolicyKeys("passive: true\n"); len(unknown) != 0 {
		t.Fatalf("passive reported unknown: %v", unknown)
	}
}
