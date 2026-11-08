package pool

import (
	"context"
	"errors"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// ErrPassive is what a PassiveGuard returns for a passive variant: the pool
// does not warm, wake, return or evict a model the controller only observes.
var ErrPassive = errors.New("the model is passive: the warm pool does not act for it")

// PassiveGuard is a Pool that adds nothing for a passive variant
// (docs/reference/scaling-policy.md, "passive"): it does not load a copy for
// it (Warm) or wake one into a serving replica (Activate). Returning a copy
// (Deactivate) and unloading one (Evict) pass through: they give capacity
// back to the pool and are the one change the pool may still make for a
// passive model, as the share's mark cleanup is for its pods. They, and
// ListWarm, are the embedded Pool's own: the guard does not override them.
//
// The refusal is logged once per variant while it lasts, and again after the
// variant has been active.
type PassiveGuard struct {
	Pool
	// Passive reports whether the variant is passive. Nil passes everything.
	Passive func(namespace, variant string) bool

	mu      sync.Mutex
	refused map[string]bool
}

// refuse reports whether the guard refuses an action for model, and logs the
// first refusal.
func (g *PassiveGuard) refuse(action string, model ModelRef) bool {
	key := model.Namespace + "/" + model.Variant
	passive := g.Passive != nil && g.Passive(model.Namespace, model.Variant)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !passive {
		delete(g.refused, key)
		return false
	}
	if !g.refused[key] {
		if g.refused == nil {
			g.refused = map[string]bool{}
		}
		g.refused[key] = true
		ctrl.Log.WithName("warmpool").Info("Warm pool: the model is passive; the pool takes no action for it",
			"namespace", model.Namespace, "variant", model.Variant, "action", action)
	}
	return true
}

// Warm loads a copy, unless the variant is passive.
func (g *PassiveGuard) Warm(ctx context.Context, pod types.NamespacedName, model ModelRef, tier Tier) error {
	if g.refuse("warm", model) {
		return ErrPassive
	}
	return g.Pool.Warm(ctx, pod, model, tier)
}

// Activate wakes a copy into a serving replica, unless the variant is passive.
func (g *PassiveGuard) Activate(ctx context.Context, pod types.NamespacedName, model ModelRef) (Endpoint, error) {
	if g.refuse("activate", model) {
		return Endpoint{}, ErrPassive
	}
	return g.Pool.Activate(ctx, pod, model)
}
