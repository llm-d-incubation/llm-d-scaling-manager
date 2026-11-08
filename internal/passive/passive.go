// Package passive resolves passive mode: a model the controller observes and
// never scales (docs/reference/scaling-policy.md, "passive"). Whether a model is
// passive is configuration (config.ResolvePassive); how its trigger answers KEDA
// depends on what else its ScaledObject carries (Mode). The external scaler,
// the engines and the warm pool all ask the same Resolver, so they cannot
// disagree about a model.
package passive

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// Mode is how one variant's trigger behaves. The values are the mode label of
// llmsm_passive_mode.
type Mode string

const (
	// Active is ordinary scaling: the trigger answers the controller's decision.
	Active Mode = constants.PassiveModeActive
	// Hold is passive with our trigger alone on the ScaledObject: it answers the
	// target's live spec.replicas, clamped to the ScaledObject's bounds, so the
	// HPA keeps the count it has and an operator's kubectl scale sticks.
	Hold Mode = constants.PassiveModeHold
	// Observe is passive with other triggers beside ours: it answers 0 and
	// inactive, so it never wins the HPA's highest-metric choice nor KEDA's
	// any-trigger activation, and KEDA scales on the other triggers.
	Observe Mode = constants.PassiveModeObserve
	// ObserveUnsafe is Observe on a ScaledObject whose scalingModifiers formula
	// names our trigger. It answers as Observe, but the formula takes our 0
	// into account, so it is not inert. Unsupported; warned about.
	ObserveUnsafe Mode = constants.PassiveModeObserveUnsafe
)

// Modes lists every Mode, in the order the gauge publishes them.
var Modes = []Mode{Active, Hold, Observe, ObserveUnsafe}

// Passive reports whether m changes no replica count of the controller's.
func (m Mode) Passive() bool { return m != Active && m != "" }

// Answers0 reports whether the trigger answers 0 and inactive (Observe and
// ObserveUnsafe): an answer that needs no decision and no replica count.
func (m Mode) Answers0() bool { return m == Observe || m == ObserveUnsafe }

// ModeOf is the mode of a passive model's variant, given its scale target as
// the ScaledObject read resolved it. A target never read (the zero Target)
// counts no foreign trigger and holds: answering the live count is the answer
// that changes nothing on either shape until the read lands.
func ModeOf(passive bool, t registry.Target) Mode {
	switch {
	case !passive:
		return Active
	case t.ForeignTriggers == 0:
		return Hold
	case t.ModifiersUseOwnTrigger:
		return ObserveUnsafe
	default:
		return Observe
	}
}

// Resolver answers passive mode from the live configuration and the registry
// of workloads KEDA has called about. A nil Resolver, or one without a Config,
// answers Active for everything: the behaviour before passive mode existed.
type Resolver struct {
	Config   *config.Config
	Registry *registry.Registry
}

// Model reports whether a model is passive and the layer that decided it
// (config.ResolvePassive), over the tiers its live variants name.
func (r *Resolver) Model(namespace, modelID string) (bool, string) {
	if r == nil || r.Config == nil {
		return false, config.PassiveSourceFlag
	}
	var tiers []string
	if r.Registry != nil && modelID != "" {
		tiers = r.Registry.TiersOf(namespace, modelID)
	}
	return config.ResolvePassive(r.Config, namespace, modelID, tiers)
}

// ForMetadata is the mode of the variant whose trigger carries metadata, with
// scale target t, and the layer that decided whether it is passive. A warm
// pool's trigger names no model and resolves on the default entry and the
// flag alone.
func (r *Resolver) ForMetadata(namespace string, metadata map[string]string, t registry.Target) (Mode, string) {
	passive, source := r.Model(namespace, metadata[registry.ModelIDKey])
	return ModeOf(passive, t), source
}

// Resolution is one variant's passive mode, the layer that decided whether
// it is passive, and the registry entry it was read from.
type Resolution struct {
	Mode   Mode
	Source string
	// Entry is the variant's registry entry; zero when Found is false.
	Entry registry.Entry
	// Found reports a live registry entry for the variant.
	Found bool
}

// Resolve is the passive mode of a variant of model modelID in namespace,
// looked up in the registry by each of names in turn -- as a ScaledObject's
// name, then as a scale target's -- so a caller passes what it knows. With no
// live entry the model alone decides, with no ScaledObject read (hold). A nil
// Resolver, or one without a Config, resolves everything active.
func (r *Resolver) Resolve(namespace, modelID string, names ...string) Resolution {
	if r == nil || r.Config == nil {
		return Resolution{Mode: Active, Source: config.PassiveSourceFlag}
	}
	if r.Registry != nil {
		for _, name := range names {
			e, ok := r.Registry.Get(namespace, name)
			if !ok {
				e, ok = r.Registry.ByTargetName(namespace, name)
			}
			if ok {
				m, src := r.ForMetadata(namespace, e.Metadata, e.Target)
				return Resolution{Mode: m, Source: src, Entry: e, Found: true}
			}
		}
	}
	m, src := r.ForMetadata(namespace, map[string]string{registry.ModelIDKey: modelID}, registry.Target{})
	return Resolution{Mode: m, Source: src}
}

// Variant is the mode of the registered variant named name in namespace: by
// its ScaledObject's name, else by its scale target's. ok is false when no
// live entry matches; the caller then treats the variant as Active.
func (r *Resolver) Variant(namespace, name string) (Mode, bool) {
	res := r.Resolve(namespace, "", name)
	if !res.Found {
		return Active, false
	}
	return res.Mode, true
}

// Clamp bounds a held count to the ScaledObject's envelope, as KEDA and the
// HPA bound it: minReplicaCount (default 0) and maxReplicaCount when known.
func Clamp(n int64, t registry.Target) int64 {
	if t.MinReplicas != nil && n < int64(*t.MinReplicas) {
		n = int64(*t.MinReplicas)
	}
	if t.MaxReplicas != nil && *t.MaxReplicas > 0 && n > int64(*t.MaxReplicas) {
		n = int64(*t.MaxReplicas)
	}
	return max(n, 0)
}
