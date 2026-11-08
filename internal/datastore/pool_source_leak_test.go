package datastore

import (
	"context"
	"runtime"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	poolutil "github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/pool"
)

// TestPoolSetDoesNotLeakScrapeSources rebuilds one pool's scrape source many
// times, as InferencePool reconciles do, then deletes it, and requires the
// goroutine count to come back down.
//
// Each source owns a cache whose cleanup goroutine ran until the context it was
// built with was done. That context is the reconcile's, which controller-runtime
// never cancels, so every PoolSet left one goroutine ticking for the life of the
// process.
func TestPoolSetDoesNotLeakScrapeSources(t *testing.T) {
	ds := NewDatastore(nil)
	c := fake.NewClientBuilder().Build()
	ctx := context.Background() // never cancelled, like a reconcile context
	pool := &poolutil.EndpointPool{
		Name:      "pool",
		Namespace: "ns",
		Selector:  map[string]string{"app": "x"},
		EndpointPicker: &poolutil.EndpointPicker{
			Namespace:         "ns",
			ServiceName:       "epp",
			MetricsPortNumber: 9090,
		},
	}

	before := runtime.NumGoroutine()
	const rebuilds = 50
	for range rebuilds {
		if err := ds.PoolSet(ctx, c, pool); err != nil {
			t.Fatalf("PoolSet: %v", err)
		}
	}
	ds.PoolDelete("ns/pool")

	// The cleanup goroutines exit asynchronously once cancelled.
	deadline := time.Now().Add(5 * time.Second)
	after := runtime.NumGoroutine()
	for after-before > 5 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		after = runtime.NumGoroutine()
	}
	if leaked := after - before; leaked > 5 {
		t.Fatalf("%d goroutines left behind after %d PoolSet calls and a PoolDelete (want ~0)", leaked, rebuilds)
	}
}
