package pool

import (
	"context"
	"errors"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

// countingPool records every call that reaches it.
type countingPool struct{ calls []string }

func (p *countingPool) ListWarm(context.Context) ([]Membership, error) {
	p.calls = append(p.calls, "list")
	return nil, nil
}
func (p *countingPool) Warm(context.Context, types.NamespacedName, ModelRef, Tier) error {
	p.calls = append(p.calls, "warm")
	return nil
}
func (p *countingPool) Activate(context.Context, types.NamespacedName, ModelRef) (Endpoint, error) {
	p.calls = append(p.calls, "activate")
	return Endpoint{}, nil
}
func (p *countingPool) Deactivate(context.Context, types.NamespacedName, ModelRef) error {
	p.calls = append(p.calls, "deactivate")
	return nil
}
func (p *countingPool) Evict(context.Context, types.NamespacedName, ModelRef) error {
	p.calls = append(p.calls, "evict")
	return nil
}

// TestPassiveGuardSkipsPassiveModels: the pool adds nothing for a passive
// model -- no copy loaded (a supervisor create), none woken into a serving
// replica (a pod patch) -- and still gives capacity back for it: Deactivate and
// Evict pass through. An active model's calls all pass through.
func TestPassiveGuardSkipsPassiveModels(t *testing.T) {
	inner := &countingPool{}
	g := &PassiveGuard{Pool: inner, Passive: func(_, variant string) bool { return variant == "observed" }}
	ctx, pod := context.Background(), types.NamespacedName{Namespace: "pool", Name: "p0"}
	m := ModelRef{Namespace: "ns", Variant: "observed"}
	if err := g.Warm(ctx, pod, m, Ram); !errors.Is(err, ErrPassive) {
		t.Errorf("Warm for a passive model: err = %v, want ErrPassive", err)
	}
	if _, err := g.Activate(ctx, pod, m); !errors.Is(err, ErrPassive) {
		t.Errorf("Activate for a passive model: err = %v, want ErrPassive", err)
	}
	if err := g.Deactivate(ctx, pod, m); err != nil {
		t.Errorf("Deactivate for a passive model: %v, want it passed through", err)
	}
	if err := g.Evict(ctx, pod, m); err != nil {
		t.Errorf("Evict for a passive model: %v, want it passed through", err)
	}
	if want := []string{"deactivate", "evict"}; !slices.Equal(inner.calls, want) {
		t.Fatalf("a passive model's calls reaching the pool: %v, want %v", inner.calls, want)
	}
	inner.calls = nil
	a := ModelRef{Namespace: "ns", Variant: "scaled"}
	_, aerr := g.Activate(ctx, pod, a)
	for i, err := range []error{g.Warm(ctx, pod, a, Ram), aerr, g.Deactivate(ctx, pod, a), g.Evict(ctx, pod, a)} {
		if err != nil {
			t.Errorf("call %d for an active model: %v", i, err)
		}
	}
	if want := []string{"activate", "warm", "deactivate", "evict"}; !slices.Equal(inner.calls, want) {
		t.Fatalf("calls = %v, want %v", inner.calls, want)
	}
}
