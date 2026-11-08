package steadystate

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
)

// The constraints line has to actually reach a log, at the level it claims.
//
// It exists because a quota that fails to bind is indistinguishable from one
// never declared, and the first version of it was written, deployed, and never
// observed -- so it was dropped rather than shipped unverified. An observer at
// V(DEBUG) is the cheap proof that it fires and carries the numbers, and it
// pins the LEVEL: an observer at InfoLevel sees nothing here, which is exactly
// how the line went missing on a cluster running with -v below DEBUG.
func TestGPUConstraintsAreLogged(t *testing.T) {
	const (
		ns    = "llm-d-sim"
		accel = "NVIDIA-A100-PCIE-80GB"
	)

	core, logs := observer.New(zapcore.Level(-logging.DEBUG))
	ctx := logr.NewContext(t.Context(), zapr.NewLogger(zap.New(core)))

	quota := allocation.NewDefaultLimiter("namespace-quota",
		allocation.NewQuotaInventory(config.QuotaLimiterConfig{
			Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
			NamespaceQuotas: map[string]map[string]int{ns: {accel: 2}},
		}))

	e := &Engine{GPULimiter: quota, optimizer: allocation.NewGreedyByScoreOptimizer()}

	_, constraints := e.selectV2Optimizer(ctx, []allocation.ModelScalingRequest{{
		ModelID:   "test-model",
		Namespace: ns,
		CompositeSignal: allocation.NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName, Result: &domain.AnalyzerResult{},
		},
		Variants:      []domain.VariantMetadata{{VariantName: "v", AcceleratorName: accel}},
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 1}},
	}})

	if len(constraints) == 0 {
		t.Fatal("no constraints were computed, so there was nothing to report")
	}

	entries := logs.FilterMessage("GPU constraints computed").All()
	if len(entries) != 1 {
		t.Fatalf("want exactly one constraints line, got %d", len(entries))
	}

	// The fields an operator needs to tell the three failure modes apart.
	fields := entries[0].ContextMap()
	for _, want := range []string{"provider", "basis", "usageByNamespace", "namespacePools", "totalLimit"} {
		if _, ok := fields[want]; !ok {
			t.Errorf("the line omits %q, which is one of the things it exists to show", want)
		}
	}
	if got := fields["provider"]; got != "namespace-quota" {
		t.Errorf("provider = %v, want the limiter's own name", got)
	}
}

// An observer above DEBUG must NOT see it: the line is diagnostic, and a
// cluster running at default verbosity should not pay for it.
func TestGPUConstraintsLogIsDebugOnly(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logr.NewContext(t.Context(), zapr.NewLogger(zap.New(core)))

	quota := allocation.NewDefaultLimiter("q",
		allocation.NewQuotaInventory(config.QuotaLimiterConfig{
			Name: "q", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"A100": 4},
		}))
	e := &Engine{GPULimiter: quota, optimizer: allocation.NewGreedyByScoreOptimizer()}

	_, _ = e.selectV2Optimizer(ctx, []allocation.ModelScalingRequest{{
		ModelID:   "m",
		Namespace: "ns",
		CompositeSignal: allocation.NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName, Result: &domain.AnalyzerResult{},
		},
		Variants:      []domain.VariantMetadata{{VariantName: "v", AcceleratorName: "A100"}},
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 1}},
	}})

	if n := logs.FilterMessage("GPU constraints computed").Len(); n != 0 {
		t.Errorf("the diagnostic leaked into a non-debug logger (%d entries)", n)
	}
}
