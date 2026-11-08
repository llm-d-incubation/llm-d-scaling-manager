package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// The gpu-policy readiness gate fails the pod only for a physical limiter
// declared ALONE. Beside quota entries the physical provider is skipped and the
// quotas still bound: such a policy ran quota-only before the limiters
// composed, and an upgrade that armed the gate for it took a controller without
// node read NotReady, removed the external-scaler endpoints and froze scaling
// for every model. The probe still runs for a composed set, so the skip is
// published (composedNodeRead).
func TestGPUPolicyReadyFailsOnlyAPhysicalOnlyPolicy(t *testing.T) {
	forbidden := errors.New("nodes is forbidden")
	probed := 0
	probe := func(context.Context) error { probed++; return forbidden }

	quota := config.QuotaLimiterConfig{Type: "quota", Name: "q", Scope: config.QuotaScopeCluster,
		ClusterQuotas: map[string]int{"H100": 8}}
	for _, tc := range []struct {
		name     string
		limiters []config.QuotaLimiterConfig
		armed    bool
		composed bool
	}{
		{"none", nil, false, false},
		{"quota only", []config.QuotaLimiterConfig{quota}, false, false},
		{"physical only", []config.QuotaLimiterConfig{{Type: "gpu-inventory"}}, true, false},
		{"physical and quota", []config.QuotaLimiterConfig{{Type: "gpu-inventory"}, quota}, false, true},
		{"quota and the inventory alias", []config.QuotaLimiterConfig{quota, {Type: "inventory"}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewTestConfig()
			cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{"default": {Limiters: tc.limiters}})
			probed = 0
			err := gpuPolicyReady(context.Background(), cfg, probe)
			switch {
			case tc.armed:
				assert.ErrorIs(t, err, forbidden, "a physical limiter alone without node read must take the controller out of service")
				assert.Equal(t, 1, probed)
			case tc.composed:
				assert.NoError(t, err, "beside a quota the physical limiter is skipped; the pod must stay Ready")
				assert.Equal(t, 1, probed, "the node read is still probed, so the skip can be published")
			default:
				assert.NoError(t, err)
				assert.Equal(t, 0, probed, "no physical limiter: the gate must not list nodes")
			}
			composedNodeRead.mu.Lock()
			denied := composedNodeRead.denied
			composedNodeRead.mu.Unlock()
			assert.Equal(t, tc.composed, denied, "the skip is recorded for a composed policy, and only for one")
		})
	}
}

// A composed policy's node-read skip clears once the read is granted, without a
// restart: the probe asks the API server each time.
func TestGPUPolicyReadyComposedSkipClearsWhenGranted(t *testing.T) {
	quota := config.QuotaLimiterConfig{Type: "quota", Name: "q", Scope: config.QuotaScopeCluster,
		ClusterQuotas: map[string]int{"H100": 8}}
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{"default": {
		Limiters: []config.QuotaLimiterConfig{{Type: "gpu-inventory"}, quota}}})
	probeErr := errors.New("nodes is forbidden")
	probe := func(context.Context) error { return probeErr }

	assert.NoError(t, gpuPolicyReady(context.Background(), cfg, probe))
	composedNodeRead.mu.Lock()
	assert.True(t, composedNodeRead.denied)
	composedNodeRead.mu.Unlock()

	probeErr = nil
	assert.NoError(t, gpuPolicyReady(context.Background(), cfg, probe))
	composedNodeRead.mu.Lock()
	assert.False(t, composedNodeRead.denied, "a granted read must clear the skip")
	composedNodeRead.mu.Unlock()
}

// The gate through the real nodeReadProbe: only Forbidden or Unauthorized is a
// verdict, and before the first one a physical limiter alone is not ready.
// Beside a quota the pod stays Ready throughout, and a List that gets no answer
// -- before the first verdict or after one -- is never published as
// node_read_denied, nor clears one that was.
func TestGPUPolicyReadyThroughTheNodeReadProbe(t *testing.T) {
	quota := config.QuotaLimiterConfig{Type: "quota", Name: "q", Scope: config.QuotaScopeCluster,
		ClusterQuotas: map[string]int{"H100": 8}}
	timeout := apierrors.NewTimeoutError("node list timed out", 1)
	denied := func() bool {
		composedNodeRead.mu.Lock()
		defer composedNodeRead.mu.Unlock()
		return composedNodeRead.denied
	}
	expireVerdict := func() {
		nodeReadProbeMu.Lock()
		nodeReadProbeAt = nodeReadProbeAt.Add(-2 * nodeReadProbeTTL)
		nodeReadProbeMu.Unlock()
	}
	reset := func() {
		resetNodeReadProbe()
		composedNodeRead.report(context.Background(), nil)
	}
	t.Cleanup(reset)
	gate := func(cfg *config.Config, r *listReader) error {
		return gpuPolicyReady(context.Background(), cfg, func(ctx context.Context) error { return nodeReadProbe(ctx, r) })
	}
	policy := func(limiters ...config.QuotaLimiterConfig) *config.Config {
		cfg := config.NewTestConfig()
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{"default": {Limiters: limiters}})
		return cfg
	}
	physical := policy(config.QuotaLimiterConfig{Type: "gpu-inventory"})
	composed := policy(config.QuotaLimiterConfig{Type: "gpu-inventory"}, quota)

	t.Run("physical alone, no answer yet", func(t *testing.T) {
		reset()
		err := gate(physical, &listReader{err: timeout})
		assert.ErrorIs(t, err, errNodeReadUnanswered, "no verdict yet: not ready")
	})
	t.Run("physical alone, a blip after Forbidden keeps it NotReady", func(t *testing.T) {
		reset()
		r := &listReader{err: nodesForbidden()}
		assert.Error(t, gate(physical, r))
		expireVerdict()
		r.err = timeout
		err := gate(physical, r)
		assert.True(t, apierrors.IsForbidden(err), "a blip keeps the Forbidden verdict, got %v", err)
	})
	t.Run("composed, no answer yet", func(t *testing.T) {
		reset()
		assert.NoError(t, gate(composed, &listReader{err: timeout}), "beside a quota the pod stays Ready")
		assert.False(t, denied(), "no answer is not a denial")
	})
	t.Run("composed, Forbidden then a blip", func(t *testing.T) {
		reset()
		r := &listReader{err: nodesForbidden()}
		assert.NoError(t, gate(composed, r))
		assert.True(t, denied(), "a definite Forbidden is published")
		expireVerdict()
		r.err = timeout
		assert.NoError(t, gate(composed, r))
		assert.True(t, denied(), "a blip keeps the denial")
	})
	t.Run("composed, granted then a blip", func(t *testing.T) {
		reset()
		r := &listReader{}
		assert.NoError(t, gate(composed, r))
		assert.False(t, denied())
		expireVerdict()
		r.err = timeout
		assert.NoError(t, gate(composed, r))
		assert.False(t, denied(), "a blip after a grant is not a denial")
	})
}
