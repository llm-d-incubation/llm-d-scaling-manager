package steadystate

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/controllerstatus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// statusWith is a fake client holding the controller-status ConfigMap with
// the in-flight ledger raw, failing reads while fail says so.
func statusWith(t *testing.T, raw string, fail *bool) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: controllerstatus.ConfigMapName, Namespace: config.SystemNamespace()},
			Data: map[string]string{controllerstatus.KeyInFlightLedger: raw}}).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if fail != nil && *fail {
				return errors.New("etcdserver: request timed out")
			}
			return cl.Get(ctx, key, obj, opts...)
		}}).Build()
}

// The engine reads the persisted in-flight ledger back from the controller
// status before its first node view, retries a failed read, and persists
// nothing until it has read it: the ledger in the ConfigMap is not
// overwritten by the restarted process's empty one.
//
// DefaultInFlight is process-wide: no t.Parallel.
func TestTheEngineRestoresThePersistedInFlightLedger(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	ctx := context.Background()
	now := time.Now()
	before := allocation.NewInFlightLedger()
	before.Admit("ns/m-v", allocation.ReplicaShape{Variant: "ns/m-v", Accelerator: "A100", GPUs: 8}, 2, 1, nil, now)
	raw, err := before.MarshalInFlight(now)
	if err != nil {
		t.Fatal(err)
	}

	fail := true
	c := statusWith(t, raw, &fail)
	e := &Engine{Config: config.NewTestConfig(), client: c, Status: controllerstatus.New(c, c, config.SystemNamespace())}
	e.restoreInFlight(ctx, now)
	e.persistInFlight(ctx, now)
	if _, set := e.Status.Data()[controllerstatus.KeyInFlightLedger]; set {
		t.Fatal("persisted before the persisted ledger was read: the restart's empty ledger would overwrite it")
	}
	if allocation.DefaultInFlight.Holds("ns/m-v", now) {
		t.Fatal("setup: held before the ledger was read")
	}

	fail = false
	e.restoreInFlight(ctx, now.Add(time.Second))
	if !allocation.DefaultInFlight.Holds("ns/m-v", now.Add(time.Second)) {
		t.Fatal("the persisted hold was not restored once the read succeeded")
	}
	e.persistInFlight(ctx, now.Add(time.Second))
	p, err := allocation.UnmarshalInFlight(e.Status.Data()[controllerstatus.KeyInFlightLedger])
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Entries["ns/m-v"]; !ok {
		t.Fatalf("the restored hold was not persisted again: %+v", p)
	}
}

// The first fitted cycle after a restart ends the restored holds it does not
// admit (allocation.InFlightLedger.DropRestoredExcept): a variant deleted
// while the controller was down keeps no node held.
//
// DefaultInFlight is process-wide: no t.Parallel.
func TestTheFirstCycleEndsARestoredHoldOfAVariantItDoesNotDecide(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	now := time.Now()
	before := allocation.NewInFlightLedger()
	for _, k := range []string{"ns/m-v", "ns/gone-v"} {
		before.Admit(k, allocation.ReplicaShape{Variant: k, Accelerator: "A100", GPUs: 8}, 3, 2, nil, now)
	}
	raw, err := before.MarshalInFlight(now)
	if err != nil {
		t.Fatal(err)
	}
	p, err := allocation.UnmarshalInFlight(raw)
	if err != nil {
		t.Fatal(err)
	}
	allocation.DefaultInFlight.RestoreInFlight(p, now, allocation.InFlightTTL)

	m := growingA100("ns", "m", 8)
	m.VariantStates[0].CurrentReplicas, m.VariantStates[0].FilledReplicas, m.VariantStates[0].HeldKnown = 3, 2, true
	m.VariantStates[0].UnplacedReplicas = 1
	at3 := []domain.VariantDecision{{VariantName: "m-v", Namespace: "ns", AcceleratorName: "A100", TargetReplicas: 3}}
	(&Engine{}).admitInFlight([]allocation.ModelScalingRequest{m}, at3, nil, nil, now.Add(time.Second))
	if allocation.DefaultInFlight.Holds("ns/gone-v", now.Add(time.Second)) {
		t.Fatal("a restored hold of a variant the first cycle does not decide still holds its node")
	}
	if !allocation.DefaultInFlight.Holds("ns/m-v", now.Add(time.Second)) {
		t.Fatal("control: the restored hold of an Unschedulable replica the cycle decides was ended")
	}
}

// A corrupt persisted ledger is ignored: nothing is restored, and the next
// persist replaces it with this process's ledger.
func TestTheEngineIgnoresACorruptInFlightLedger(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	ctx := context.Background()
	now := time.Now()
	c := statusWith(t, `{"version":1,"entries":{"ns/m-v":`, nil)
	e := &Engine{Config: config.NewTestConfig(), client: c, Status: controllerstatus.New(c, c, config.SystemNamespace())}
	e.restoreInFlight(ctx, now)
	if allocation.DefaultInFlight.Holds("ns/m-v", now) {
		t.Fatal("a corrupt ledger restored a hold")
	}
	e.persistInFlight(ctx, now)
	if _, err := allocation.UnmarshalInFlight(e.Status.Data()[controllerstatus.KeyInFlightLedger]); err != nil {
		t.Fatalf("the corrupt ledger was not replaced: %v", err)
	}
}
