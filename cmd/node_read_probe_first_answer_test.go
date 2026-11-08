package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// Before the first definite answer there is no verdict to keep: a probe whose
// first List times out must report not ready, and ask again on the next probe
// rather than wait out the TTL. It reported Ready, which put a controller that
// may not list nodes into service until the next answer.
func TestNodeReadProbeIsNotReadyBeforeTheFirstAnswer(t *testing.T) {
	resetNodeReadProbe()
	t.Cleanup(resetNodeReadProbe)

	r := &listReader{err: apierrors.NewTimeoutError("node list timed out", 1)}
	assert.Error(t, nodeReadProbe(context.Background(), r),
		"no answer yet: not ready")
	assert.Error(t, nodeReadProbe(context.Background(), r),
		"still no answer: still not ready")
	assert.Equal(t, 2, r.calls, "with no answer yet, every probe must ask again")

	r.err = nil
	assert.NoError(t, nodeReadProbe(context.Background(), r), "the first answer decides")
	r.err = apierrors.NewTimeoutError("blip", 1)
	nodeReadProbeMu.Lock()
	nodeReadProbeAt = nodeReadProbeAt.Add(-2 * nodeReadProbeTTL)
	nodeReadProbeMu.Unlock()
	assert.NoError(t, nodeReadProbe(context.Background(), r), "after an answer, a blip keeps it")
}

// blockingReader's List blocks until its context ends, like a List against an
// API server that never answers.
type blockingReader struct{ listReader }

func (r *blockingReader) List(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	r.calls++
	<-ctx.Done()
	return ctx.Err()
}

// The List is bounded by nodeReadProbeTimeout: an API server that never answers
// yields a verdict here, the previous one, rather than a probe the kubelet
// abandons. Run with a short timeout so the margin does not depend on the
// runner's speed; the production value is pinned against the probe below.
func TestNodeReadProbeBoundsAHangingList(t *testing.T) {
	resetNodeReadProbe()
	t.Cleanup(resetNodeReadProbe)
	saved := nodeReadProbeTimeout
	nodeReadProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { nodeReadProbeTimeout = saved })

	assert.Error(t, nodeReadProbe(context.Background(), &listReader{err: nodesForbidden()}), "setup: Forbidden")
	nodeReadProbeMu.Lock()
	nodeReadProbeAt = nodeReadProbeAt.Add(-2 * nodeReadProbeTTL)
	nodeReadProbeMu.Unlock()

	start := time.Now()
	err := nodeReadProbe(context.Background(), &blockingReader{})
	took := time.Since(start)
	assert.True(t, apierrors.IsForbidden(err), "a hanging List must keep the previous verdict, got %v", err)
	assert.Less(t, took, time.Second, "the List must be cut off at nodeReadProbeTimeout (100ms here)")
}

// The production timeout must end the List before the kubelet gives up on the
// readiness probe, or a hanging API server fails the probe instead of keeping
// the previous verdict.
func TestNodeReadProbeTimeoutIsUnderTheReadinessProbeTimeout(t *testing.T) {
	raw, err := os.ReadFile("../config/base/manager/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var d appsv1.Deployment
	if err := yaml.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	var probe *corev1.Probe
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.ReadinessProbe != nil {
			probe = c.ReadinessProbe
		}
	}
	if probe == nil || probe.TimeoutSeconds == 0 {
		t.Fatal("no readinessProbe with timeoutSeconds in config/base/manager/deployment.yaml")
	}
	limit := time.Duration(probe.TimeoutSeconds) * time.Second
	assert.Less(t, nodeReadProbeTimeout, limit, "the List timeout must be under the probe's timeoutSeconds")
}

// A blip after the first answer keeps the verdict AND restarts the TTL: the
// next probe reads the kept verdict instead of sending another List. Without
// the restart every probe during an outage would hit the API server.
func TestNodeReadProbeBlipRestartsTheTTL(t *testing.T) {
	resetNodeReadProbe()
	t.Cleanup(resetNodeReadProbe)

	assert.NoError(t, nodeReadProbe(context.Background(), &listReader{}), "setup: first answer")
	nodeReadProbeMu.Lock()
	nodeReadProbeAt = nodeReadProbeAt.Add(-2 * nodeReadProbeTTL)
	nodeReadProbeMu.Unlock()

	blip := &listReader{err: apierrors.NewTimeoutError("blip", 1)}
	assert.NoError(t, nodeReadProbe(context.Background(), blip), "a blip keeps the verdict")
	assert.Equal(t, 1, blip.calls, "the expired TTL must send one List")
	assert.NoError(t, nodeReadProbe(context.Background(), blip), "still the kept verdict")
	assert.Equal(t, 1, blip.calls, "a probe right after a blip must not send another List")
}
