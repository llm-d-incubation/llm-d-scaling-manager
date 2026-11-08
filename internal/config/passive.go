package config

import (
	"fmt"
	"slices"
	"strconv"

	"gopkg.in/yaml.v3"
)

// ManagerConfigKey is the data key of the manager ConfigMap
// (scaling-manager-manager-config) that holds its settings as YAML.
const ManagerConfigKey = "config.yaml"

// ManagerConfigPassive reads LLMSM_PASSIVE from the manager ConfigMap's data, as
// the loader reads it at start: an absent key, or one that is not a boolean,
// is false. set reports whether the key was there and parsed.
func ManagerConfigPassive(data map[string]string) (passive, set bool) {
	var m map[string]any
	if err := yaml.Unmarshal([]byte(data[ManagerConfigKey]), &m); err != nil {
		return false, false
	}
	v, found := m["LLMSM_PASSIVE"]
	if !found {
		return false, false
	}
	b, err := strconv.ParseBool(fmt.Sprint(v))
	if err != nil {
		return false, false
	}
	return b, true
}

// Passive-mode sources: the layer whose value decided a model's passive mode,
// as ResolvePassive reports it and the "Passive mode" log lines print it.
const (
	// PassiveSourcePerModel is a per-model entry (its body names the model).
	PassiveSourcePerModel = "perModel"
	// PassiveSourceTierPrefix prefixes the name of the tier that decided it.
	PassiveSourceTierPrefix = "tier:"
	// PassiveSourceDefault is the controller's own `default` entry.
	PassiveSourceDefault = "default"
	// PassiveSourceNamespaceDefault is the `default` entry of the model
	// namespace's own scaling-policy map, which replaces the controller's.
	PassiveSourceNamespaceDefault = "namespaceDefault"
	// PassiveSourceFlag is the LLMSM_PASSIVE controller flag.
	PassiveSourceFlag = "LLMSM_PASSIVE"
)

// ResolvePassive reports whether a model is passive -- observed, never scaled
// -- and the layer that decided it.
//
// tiers is the tier each of the model's variants names in its trigger
// metadata, "" for none. The layers, most specific first:
//
//  1. a per-model entry that sets `passive` (FindModelOverride);
//  2. the tier the variant names, else `default.defaultPolicy`;
//  3. the `default` entry -- the namespace's own map when it has one, which
//     replaces the controller's whole;
//  4. the LLMSM_PASSIVE flag.
//
// When the variants name different tiers the model is passive if ANY of them
// resolves passive: a model is scaled whole, and passive is the side that
// moves nothing. A variant that names none counts as the default entry (and
// its defaultPolicy) does: unlike the thresholds (ModelTiers), it is not
// folded into the tiers its siblings name, since "any" must see every
// variant. A per-model entry sits above every tier, so its `passive:
// false` makes the whole model active.
//
// modelID "" (a warm pool's trigger, which serves no model) skips layer 1.
// A nil cfg is active, from the flag's default.
func ResolvePassive(cfg *Config, namespace, modelID string, tiers []string) (bool, string) {
	if cfg == nil {
		return false, PassiveSourceFlag
	}
	policies := cfg.ScalingPolicyConfigForNamespace(namespace)
	if modelID != "" {
		if o, ok := FindModelOverride(policies, modelID, namespace); ok && o.Passive != nil {
			return *o.Passive, PassiveSourcePerModel
		}
	}
	base, baseSource := cfg.PassiveDefault(), PassiveSourceFlag
	def, hasDefault := policies[GlobalDefaultsKey]
	if hasDefault && def.Passive != nil {
		base, baseSource = *def.Passive, PassiveSourceDefault
		if cfg.NamespaceHasLocalPolicy(namespace) {
			baseSource = PassiveSourceNamespaceDefault
		}
	}
	first, firstSource := false, ""
	for i, tier := range passiveTiers(tiers) {
		if tier == "" {
			tier = def.DefaultPolicy
		}
		v, src := base, baseSource
		if entry, ok := policies[tier]; ok && tier != "" && PolicyEntryKey(tier, entry) && entry.Passive != nil {
			v, src = *entry.Passive, PassiveSourceTierPrefix+tier
		}
		if v {
			return true, src
		}
		if i == 0 {
			first, firstSource = v, src
		}
	}
	return first, firstSource
}

// passiveTiers is tiers sorted and distinct, "" kept; one "" when empty.
func passiveTiers(tiers []string) []string {
	out := []string{}
	for _, t := range tiers {
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	slices.Sort(out)
	return out
}
