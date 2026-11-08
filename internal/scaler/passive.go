package scaler

import (
	"context"
	"sync"

	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/passive"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// PassiveResolver reports whether a model is passive (passive.Resolver.Model).
type PassiveResolver interface {
	Model(namespace, modelID string) (bool, string)
}

// WithPassive makes the handler answer passive models without moving them
// (docs/reference/scaling-policy.md, "passive"). Nil leaves every model
// active. Returns h for chaining.
func (h *Handler) WithPassive(p PassiveResolver) *Handler {
	h.passive = p
	return h
}

// model reports whether the model ref names is passive: the one place the
// handler resolves it, once per question.
func (h *Handler) model(ref *pb.ScaledObjectRef) bool {
	if h.passive == nil {
		return false
	}
	p, _ := h.passive.Model(ref.GetNamespace(), ref.GetScalerMetadata()[registry.ModelIDKey])
	return p
}

// passiveMode is the mode of the variant ref names, with scale target t.
//
// The scaler reads passive live, at every KEDA call; the steady-state engine
// once a cycle. A switch shorter than a cycle moves the count (or stops it)
// with no line from the engine, so the scaler logs every change it answers
// from, once.
func (h *Handler) passiveMode(ctx context.Context, ref *pb.ScaledObjectRef, t registry.Target) passive.Mode {
	if h.passive == nil {
		return passive.Active
	}
	modelID := ref.GetScalerMetadata()[registry.ModelIDKey]
	p, source := h.passive.Model(ref.GetNamespace(), modelID)
	mode := passive.ModeOf(p, t)
	key := types.NamespacedName{Namespace: ref.GetNamespace(), Name: ref.GetName()}
	if prev, changed := h.modes.swap(key, mode); changed {
		kv := []any{"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(), "model", modelID,
			"mode", string(mode), "previousMode", string(prev), "source", source}
		logger := log.FromContext(ctx)
		switch {
		case mode.Passive() && !prev.Passive():
			logger.Info("Passive mode entered: the external scaler answers KEDA for this variant without moving it", kv...)
		case !mode.Passive() && prev.Passive():
			logger.Info("Passive mode left: the external scaler answers KEDA with the controller's decision again", kv...)
		case mode.Passive():
			logger.Info("Passive mode changed: the external scaler answers KEDA in the new mode", kv...)
		}
	}
	return mode
}

// passiveModes is each ScaledObject's passive mode as a handler last
// answered it. The zero value is ready to use.
type passiveModes struct {
	mu   sync.Mutex
	last map[types.NamespacedName]passive.Mode
}

// swap records mode for key and returns the mode it replaces, and whether
// it differs. A key not seen before replaces "", which is not passive, so
// the first answer of an active variant is no change worth a line and the
// first of a passive one is "entered".
func (m *passiveModes) swap(key types.NamespacedName, mode passive.Mode) (passive.Mode, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		m.last = map[types.NamespacedName]passive.Mode{}
	}
	prev := m.last[key]
	m.last[key] = mode
	return prev, prev != mode
}

// passiveAnswer is what GetMetrics answers for a passive variant, whatever the
// decision store holds.
//
//   - Observe (other triggers beside ours): 0. Under AverageValue with target 1
//     the HPA proposes ceil(0/1) = 0 for our metric and takes the highest
//     proposal over its metrics, so ours never wins. Never an error: the HPA
//     refuses every scale-down while any metric fails
//     (computeReplicasForMetrics), and KEDA counts an error toward its
//     fallback.
//   - Hold (our trigger alone): the target's live spec.replicas -- what the HPA
//     itself takes as the current count -- clamped to the ScaledObject's
//     bounds. A desired count equal to the current one returns the current
//     one through any stabilization window, so the HPA holds, and a count an
//     operator sets by hand is the count held.
func (h *Handler) passiveAnswer(ctx context.Context, ref *pb.ScaledObjectRef, t registry.Target, mode passive.Mode) (int64, error) {
	if mode.Answers0() {
		return 0, nil
	}
	n, err := h.currentReplicas(ctx, ref, t)
	if err != nil {
		return 0, err
	}
	return passive.Clamp(n, t), nil
}

// passiveActive is the activation a passive variant reports: never in
// observe, so KEDA's any-trigger activation rests on the other triggers; in
// hold, whether the held count is above zero, so a target at zero stays there
// and a running one is never deactivated. An unreadable count is an error, not
// active: active would wake a target parked at zero on the strength of a
// failed read, and inactive would park a running one. KEDA counts the error
// as a failed trigger read and changes nothing (see errNotLeader).
func (h *Handler) passiveActive(ctx context.Context, ref *pb.ScaledObjectRef, t registry.Target, mode passive.Mode) (bool, error) {
	if mode.Answers0() {
		return false, nil
	}
	n, err := h.passiveAnswer(ctx, ref, t, mode)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// standbyObserve reports whether a replica that does not hold the lease can
// answer ref anyway: an observe answer needs no decision and no replica
// count, so a standby gives the same one the leader would, and the lease
// hand-off stops failing a metric the HPA would otherwise hold every
// scale-down for. Only a passive model costs the ScaledObject read.
func (h *Handler) standbyObserve(ctx context.Context, ref *pb.ScaledObjectRef) bool {
	// The model first: an active model costs the standby no ScaledObject read.
	p := h.model(ref)
	if !p {
		return false
	}
	t, err := h.resolveTarget(ctx, ref)
	if err != nil {
		return false
	}
	return passive.ModeOf(p, t).Answers0()
}
