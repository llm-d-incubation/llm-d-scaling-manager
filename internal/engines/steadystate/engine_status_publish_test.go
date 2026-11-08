package steadystate

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/prometheus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/controllerstatus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// TestOptimizeCyclePublishesUtilizationShareMode runs whole optimize cycles --
// not observeUtilizationShareMode alone -- and reads the controller-status
// publisher after each: the utilizationShare key must follow the applied
// policy from one cycle to the next, including on a cycle with no model to
// plan, which is the cycle a parked fleet runs forever.
func TestOptimizeCyclePublishesUtilizationShareMode(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()
	sources := source.NewSourceRegistry()
	if err := sources.Register("prometheus", prometheus.NewPrometheusSource(ctx, nil, prometheus.DefaultPrometheusSourceConfig())); err != nil {
		t.Fatal(err)
	}
	cfg := shadowConfig(t, selectedShadow)
	e := NewEngine(c, c, c.Scheme(), nil, sources, cfg, allocation.NewNoOpLimiter("test"))
	e.Variants = registry.New(0)
	e.Status = controllerstatus.New(c, c, "scaling-manager-system")
	if got := e.Status.Data()[controllerstatus.KeyUtilizationShare]; got != constants.UtilizationShareModeOff {
		t.Fatalf("before any cycle utilizationShare = %q, want the publisher's default %q", got, constants.UtilizationShareModeOff)
	}

	for _, step := range []struct {
		name, optimizer, want string
		wantErr               bool
	}{
		{"shadow", selectedShadow, constants.UtilizationShareModeShadow, false},
		{"active", "optimizer:\n  type: utilizationShare\n", constants.UtilizationShareModeActive, false},
		{"invalid", "optimizer:\n  type: bogus\n", constants.UtilizationShareModeInvalid, true},
		{"off", "", constants.UtilizationShareModeOff, false},
	} {
		setShadowPolicy(t, cfg, step.optimizer)
		if err := e.optimize(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: optimize: %v", step.name, err)
		}
		d := e.Status.Data()
		if got := d[controllerstatus.KeyUtilizationShare]; got != step.want {
			t.Errorf("%s: after a cycle utilizationShare = %q, want %q", step.name, got, step.want)
		}
		if gotErr := d[controllerstatus.KeyUtilizationShareError] != ""; gotErr != step.wantErr {
			t.Errorf("%s: utilizationShareError = %q, want set=%v", step.name, d[controllerstatus.KeyUtilizationShareError], step.wantErr)
		}
	}
}
