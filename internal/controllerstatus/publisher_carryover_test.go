package controllerstatus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Review R10: a new term's carry-over read failed silently, and the first
// write then erased the persisted borrowing release ledger. The read is
// retried with the write's backoff, and nothing is written until it passes.
func TestStartWritesNothingUntilThePersistedKeysAreRead(t *testing.T) {
	w := &writes{}
	existing := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: testNamespace},
		Data: map[string]string{KeyBorrowReleaseLedger: `{"timeoutHeld":{"b/v":2}}`}}
	p, base := newTestPublisher(t, w, existing)
	p.debounce, p.retryMin, p.retryMax = 10*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond
	var failing atomic.Int32
	failing.Store(3)
	p.reader = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if failing.Load() > 0 {
				failing.Add(-1)
				return errors.New("etcdserver: request timed out")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	written := func() bool {
		var cm corev1.ConfigMap
		err := base.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: ConfigMapName}, &cm)
		return err == nil && cm.Data[KeyFormatVersion] != ""
	}
	for !written() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("timed out waiting for the first write")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start returned %v", err)
	}
	if failing.Load() != 0 {
		t.Fatalf("a write happened with %d carry-over reads still failing", failing.Load())
	}
	if got := stored(t, base).Data[KeyBorrowReleaseLedger]; got != existing.Data[KeyBorrowReleaseLedger] {
		t.Fatalf("the first write left the ledger %q, want it carried over", got)
	}
}
