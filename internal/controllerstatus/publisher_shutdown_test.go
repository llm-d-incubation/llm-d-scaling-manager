package controllerstatus

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Review of #200: the debounce returned on shutdown without writing, so the
// last change of a term -- the in-flight ledger's last cycle -- was lost to
// the next leader. Start writes it once more, bounded, when its context ends.
func TestStartWritesTheLastChangeOnShutdown(t *testing.T) {
	w := &writes{}
	p, c := newTestPublisher(t, w)
	p.debounce = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var cm corev1.ConfigMap
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: ConfigMapName}, &cm); err == nil && cm.Data[KeyFormatVersion] != "" {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("timed out waiting for the first write")
		}
		time.Sleep(5 * time.Millisecond)
	}
	p.SetInFlightLedger(`{"version":1}`)
	cancel() // inside the debounce: the change is not written yet
	if err := <-done; err != nil {
		t.Fatalf("Start returned %v", err)
	}
	if got := stored(t, c).Data[KeyInFlightLedger]; got != `{"version":1}` {
		t.Fatalf("the change made just before shutdown was not written: inFlightLedger %q", got)
	}
}

// The in-flight ledger is state: a new term carries it over rather than
// overwriting it with its own empty start.
func TestTheInFlightLedgerIsCarriedOverToANewTerm(t *testing.T) {
	if !slices.Contains(persistedKeys, KeyInFlightLedger) {
		t.Fatalf("persistedKeys %v does not carry %s", persistedKeys, KeyInFlightLedger)
	}
	w := &writes{}
	existing := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: testNamespace},
		Data: map[string]string{KeyInFlightLedger: `{"version":1,"entries":{"ns/m":{}}}`}}
	p, _ := newTestPublisher(t, w, existing)
	if err := p.carryOver(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.Data()[KeyInFlightLedger]; got != existing.Data[KeyInFlightLedger] {
		t.Fatalf("the new term holds inFlightLedger %q, want it carried over", got)
	}
}
