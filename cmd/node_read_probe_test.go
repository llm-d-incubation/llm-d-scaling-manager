package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// listReader is a client.Reader whose List answers with whatever err holds.
type listReader struct {
	err   error
	calls int
}

func (r *listReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("not used")
}

func (r *listReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	r.calls++
	return r.err
}

// resetNodeReadProbe forgets the cached verdict, so each case asks the reader.
func resetNodeReadProbe() {
	nodeReadProbeMu.Lock()
	defer nodeReadProbeMu.Unlock()
	nodeReadProbeOnce = false
	nodeReadProbeErr = nil
}

func nodesForbidden() error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("no rbac"))
}

// The gate exists to catch missing RBAC. A transient API error is not that, and
// failing readiness on one removes the only external-scaler endpoint KEDA can
// reach -- an actuation outage caused by the check meant to prevent one.
func TestNodeReadProbeIgnoresTransientErrors(t *testing.T) {
	for _, transient := range []error{
		apierrors.NewTimeoutError("node list timed out", 1),
		apierrors.NewServiceUnavailable("apiserver restarting"),
	} {
		resetNodeReadProbe()
		assert.NoError(t, nodeReadProbe(context.Background(), &listReader{}), "setup: a first answer, Ready")
		nodeReadProbeMu.Lock()
		nodeReadProbeAt = nodeReadProbeAt.Add(-2 * nodeReadProbeTTL)
		nodeReadProbeMu.Unlock()
		assert.NoError(t, nodeReadProbe(context.Background(), &listReader{err: transient}),
			"%v says nothing about RBAC, so it must not take the pod out of service", transient)
	}
	resetNodeReadProbe()
}

func TestNodeReadProbeFailsOnForbidden(t *testing.T) {
	resetNodeReadProbe()
	t.Cleanup(resetNodeReadProbe)

	forbidden := &listReader{err: nodesForbidden()}
	assert.Error(t, nodeReadProbe(context.Background(), forbidden),
		"Forbidden is the condition the gate exists for")

	resetNodeReadProbe()
	unauthorized := &listReader{err: apierrors.NewUnauthorized("token expired")}
	assert.Error(t, nodeReadProbe(context.Background(), unauthorized))
}

// A transient error keeps the LAST verdict rather than resetting it: a pod that
// was NotReady for missing RBAC stays NotReady through a blip, and one that was
// Ready stays Ready.
func TestNodeReadProbeTransientErrorKeepsLastVerdict(t *testing.T) {
	resetNodeReadProbe()
	t.Cleanup(resetNodeReadProbe)

	assert.Error(t, nodeReadProbe(context.Background(), &listReader{err: nodesForbidden()}))
	// Expire the cache so the next call asks again.
	nodeReadProbeMu.Lock()
	nodeReadProbeAt = nodeReadProbeAt.Add(-2 * nodeReadProbeTTL)
	nodeReadProbeMu.Unlock()

	assert.Error(t, nodeReadProbe(context.Background(),
		&listReader{err: apierrors.NewTimeoutError("blip", 1)}),
		"a blip must not clear a standing Forbidden")

	nodeReadProbeMu.Lock()
	nodeReadProbeAt = nodeReadProbeAt.Add(-2 * nodeReadProbeTTL)
	nodeReadProbeMu.Unlock()
	assert.NoError(t, nodeReadProbe(context.Background(), &listReader{}),
		"granting the RBAC recovers readiness on its own")
}

func TestNodeReadProbeIsRateLimited(t *testing.T) {
	resetNodeReadProbe()
	t.Cleanup(resetNodeReadProbe)

	r := &listReader{}
	assert.NoError(t, nodeReadProbe(context.Background(), r))
	assert.NoError(t, nodeReadProbe(context.Background(), r))
	assert.Equal(t, 1, r.calls, "a second probe inside the TTL must not list nodes again")
}
