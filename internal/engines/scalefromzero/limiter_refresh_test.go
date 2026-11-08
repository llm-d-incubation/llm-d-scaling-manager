package scalefromzero

import (
	"context"
	"errors"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// The limiters: list is documented as applied live, and it is the sole switch for
// both engines. This engine used to build its limiter once at startup, so an edit
// changed how the optimizer allocated while the capacity check kept using the
// startup configuration — indefinitely, and with nothing saying so.
func TestLimiterIsRebuiltWhenTheConfigChanges(t *testing.T) {
	ctx := context.Background()
	cfg := config.NewTestConfig()

	withLimiters := func(limiters ...config.QuotaLimiterConfig) {
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			"default": {Limiters: limiters},
		})
	}

	builds := 0
	withLimiters() // none
	e := &Engine{config: cfg}
	e.SetGPULimiter(allocation.NewNoOpLimiter("startup"))
	e.SetLimiterBuilder(func() (allocation.Limiter, error) {
		builds++
		return allocation.NewLimiterFromConfig(cfg, nil)
	})

	// Unchanged config must not rebuild: this runs at 10Hz, and rebuilding would
	// re-create the inventory and its discovery client ten times a second.
	e.refreshLimiter(ctx)
	e.refreshLimiter(ctx)
	if builds != 0 {
		t.Fatalf("rebuilt %d times with an unchanged config, want 0", builds)
	}
	if got := allocation.ConstraintProvidersFrom(e.currentGPULimiter()); len(got) != 0 {
		t.Fatalf("no limiter declared, so nothing should provide constraints; got %d", len(got))
	}

	// Declaring a limiter must reach this engine without a restart.
	withLimiters(config.QuotaLimiterConfig{
		Name: "q", Type: "quota", Scope: config.QuotaScopeCluster,
		ClusterQuotas: map[string]int{"A100": 4},
	})
	e.refreshLimiter(ctx)
	if builds != 1 {
		t.Fatalf("rebuilt %d times after a config change, want 1", builds)
	}
	if got := allocation.ConstraintProvidersFrom(e.currentGPULimiter()); len(got) != 1 {
		t.Fatalf("declared quota must supply constraints to the capacity check; got %d providers", len(got))
	}

	// And removing it must take effect the same way, or the check keeps enforcing
	// a limit the operator has deleted.
	withLimiters()
	e.refreshLimiter(ctx)
	if got := allocation.ConstraintProvidersFrom(e.currentGPULimiter()); len(got) != 0 {
		t.Fatalf("limiter removed from config but still providing %d constraint(s)", len(got))
	}
}

// A build failure must not silently disarm the capacity check.
func TestAFailedRebuildKeepsThePreviousLimiter(t *testing.T) {
	ctx := context.Background()
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		"default": {Limiters: []config.QuotaLimiterConfig{{
			Name: "q", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"A100": 4},
		}}},
	})

	fail := false
	e := &Engine{config: cfg}
	e.SetLimiterBuilder(func() (allocation.Limiter, error) {
		if fail {
			return nil, errors.New("cluster unreachable")
		}
		return allocation.NewLimiterFromConfig(cfg, nil)
	})
	e.refreshLimiter(ctx) // seeded signature differs from a nil limiter, so this builds
	before := e.currentGPULimiter()
	if before == nil {
		t.Fatal("expected a limiter after the first refresh")
	}

	// Change the config so a rebuild is attempted, and make it fail.
	fail = true
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		"default": {Limiters: []config.QuotaLimiterConfig{{
			Name: "q2", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H100": 8},
		}}},
	})
	e.refreshLimiter(ctx)
	if e.currentGPULimiter() != before {
		t.Error("a failed rebuild must keep the previous limiter rather than turning the check off")
	}

	// And it must retry rather than latch the failure: the signature is only
	// advanced on success, so the next cycle tries again.
	fail = false
	e.refreshLimiter(ctx)
	if e.currentGPULimiter() == before {
		t.Error("the rebuild must be retried once the transient failure clears")
	}
}
