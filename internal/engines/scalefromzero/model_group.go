/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scalefromzero

import (
	"sort"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// modelGroup is one model's inactive variants. Waking is decided per model
// rather than per variant: a P/D model's decode and prefill have to be chosen
// together against a single GPU budget, and all of a model's variants share one
// EPP queue, so the pending-request check is one query for the group.
type modelGroup struct {
	namespace string
	modelID   string
	variants  []llmdvariant.Variant
	// modelTiers are the tiers the WHOLE model resolves under, running variants
	// included (config.ModelTiers). nil means only variants is known.
	modelTiers []string
}

// tiers returns the policy tiers the model is resolved under: the ones its
// variants name, or "" (the default entry) when none names one
// (config.ModelTiers).
//
// The tiers come from all of the model's variants, running ones included. (The
// steady-state planner sees only running variants, so until a parked variant is
// up it may weigh the model under fewer tiers.) The inactive variants alone are not
// enough: a P/D model whose decode is tagged and running and whose prefill is
// untagged and parked would otherwise wake its prefill under the default entry.
func (g modelGroup) tiers() []string {
	if g.modelTiers != nil {
		return g.modelTiers
	}
	named := make([]string, 0, len(g.variants))
	for i := range g.variants {
		named = append(named, g.variants[i].Spec.ScalingPolicy)
	}
	return config.ModelTiers(named)
}

// key identifies the group for coverage lookups.
func (g modelGroup) key() string {
	return modelKey(g.namespace, g.modelID)
}

func modelKey(namespace, modelID string) string {
	return namespace + "|" + modelID
}

// groupInactiveByModel buckets inactive variants by (namespace, model). Groups
// are returned in a stable order so repeated ticks process models the same way.
//
// active are the variants that are running. They form no group, but they name
// tiers too: each group's tiers come from all of its model's variants, so a
// tagged variant that is running still counts when its untagged sibling wakes.
func groupInactiveByModel(inactive, active []llmdvariant.Variant) []modelGroup {
	byKey := make(map[string]*modelGroup)
	named := make(map[string][]string)
	for _, va := range inactive {
		key := modelKey(va.Namespace, va.Spec.ModelID)
		g, ok := byKey[key]
		if !ok {
			g = &modelGroup{namespace: va.Namespace, modelID: va.Spec.ModelID}
			byKey[key] = g
		}
		g.variants = append(g.variants, va)
		named[key] = append(named[key], va.Spec.ScalingPolicy)
	}
	for _, va := range active {
		key := modelKey(va.Namespace, va.Spec.ModelID)
		if _, ok := byKey[key]; ok {
			named[key] = append(named[key], va.Spec.ScalingPolicy)
		}
	}

	groups := make([]modelGroup, 0, len(byKey))
	for key, g := range byKey {
		g.modelTiers = config.ModelTiers(named[key])
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].namespace != groups[j].namespace {
			return groups[i].namespace < groups[j].namespace
		}
		return groups[i].modelID < groups[j].modelID
	})
	return groups
}

// refusalChanged reports whether outcome differs from the last refusal recorded
// for this model, recording it either way.
//
// The optimize loop runs every 100ms, so a model that cannot be woken produces a
// verdict ten times a second for as long as its requests are queued. Logging
// only transitions keeps the operator-facing signal — "this model has demand and
// was not woken, because X" — without burying the log.
func (e *Engine) refusalChanged(key string, outcome SelectionOutcome) bool {
	e.refusalMu.Lock()
	defer e.refusalMu.Unlock()
	if e.lastRefusal == nil {
		e.lastRefusal = make(map[string]SelectionOutcome)
	}
	if prev, ok := e.lastRefusal[key]; ok && prev == outcome {
		return false
	}
	e.lastRefusal[key] = outcome
	return true
}

// clearRefusal forgets a model's last refusal, so that if it is refused again
// later the reason is reported afresh rather than suppressed as unchanged.
func (e *Engine) clearRefusal(key string) {
	e.refusalMu.Lock()
	defer e.refusalMu.Unlock()
	delete(e.lastRefusal, key)
}

// pruneRefusals drops remembered refusals, refused-wake reasons and extra
// InferencePools for models that are no longer in the inactive population.
//
// Without this the maps are append-only in a process that runs for the life
// of the cluster: every model that is ever refused and then deleted — or
// simply scaled up and left there — keeps its entry forever. The bookkeeping
// is only meaningful for models this cycle is actually considering.
//
// A refused-wake reason is also a series on llmsm_model_scaling_blocked, and
// only processInactiveModel cleared it, for a model still in the groups. A
// model that woke by another path (KEDA, a manual scale, the steady-state
// engine) and left the groups, or was deleted, kept wake-no-capacity for the
// life of the process. Its series is cleared here, as is the series of a
// model whose decode serves (covered): such a model is never refused a wake
// (selectServingSet), so nothing sets the reason again.
func (e *Engine) pruneRefusals(groups []modelGroup, covered map[string]coverage) {
	live := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		live[g.key()] = struct{}{}
	}
	e.wakeBlocked.Range(func(k, v any) bool {
		key, _ := k.(string)
		if _, ok := live[key]; ok && !covered[key].decode {
			return true
		}
		if g, ok := v.(modelGroup); ok {
			e.reportWakeBlocked(g, "")
		}
		return true
	})
	e.extraPools.Range(func(k, _ any) bool {
		key, _ := k.(string)
		if _, ok := live[key]; !ok {
			e.extraPools.Delete(k)
		}
		return true
	})
	e.refusalMu.Lock()
	defer e.refusalMu.Unlock()
	for key := range e.lastRefusal {
		if _, ok := live[key]; !ok {
			delete(e.lastRefusal, key)
		}
	}
}

// coverage records which P/D roles a model is already running.
type coverage struct {
	decode  bool
	prefill bool
}

// activeRoleCoverage maps (namespace, model) to the roles its running variants
// already provide. A "both" variant counts as decode: it serves complete
// requests on its own.
//
// This is what keeps scale-from-zero out of the saturation engine's way. A model
// with a running decode can already serve, so a queue building up on it is a
// saturation problem, not an activation one.
func activeRoleCoverage(
	activeVAs []llmdvariant.Variant,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
) map[string]coverage {
	covered := make(map[string]coverage)
	for _, va := range activeVAs {
		key := modelKey(va.Namespace, va.Spec.ModelID)
		c := covered[key]
		target := scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]
		// A nil target resolves to RoleBoth, which counts as decode — the safe
		// direction, since over-reporting coverage only declines a wake for a
		// model something else is already running.
		switch variantmeta.RoleFromScaleTarget(target) {
		case domain.RolePrefill:
			c.prefill = true
		default:
			c.decode = true
		}
		covered[key] = c
	}
	return covered
}
