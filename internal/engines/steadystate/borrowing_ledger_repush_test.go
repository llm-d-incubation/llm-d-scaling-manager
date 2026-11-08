package steadystate

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/controllerstatus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// Review R10: after reading the persisted ledger back, the engine took it as
// already handed to the status publisher and never pushed it; a publisher
// whose carry-over read failed then wrote the ConfigMap without it. The
// loaded ledger is pushed on the next persist.
func TestTheLoadedLedgerIsPushedToThePublisher(t *testing.T) {
	ctx := context.Background()
	g := allocation.NewBorrowGovernor(allocation.DefaultBorrowSettings())
	g.MarkPreemptedIn("b/m", []string{"c"}, time.Now())
	raw, err := g.MarshalLedger()
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: controllerstatus.ConfigMapName, Namespace: config.SystemNamespace()},
		Data:       map[string]string{controllerstatus.KeyBorrowReleaseLedger: raw}}).Build()
	e := &Engine{Config: config.NewTestConfig(), client: c, Status: controllerstatus.New(c, c, config.SystemNamespace())}
	e.borrowing.governor = allocation.NewBorrowGovernor(allocation.DefaultBorrowSettings())
	e.loadBorrowLedger(ctx)
	if !e.borrowing.ledgerLoaded {
		t.Fatal("premise: the ledger is read")
	}
	e.persistBorrowLedger(ctx)
	if got := e.Status.Data()[controllerstatus.KeyBorrowReleaseLedger]; got != raw {
		t.Fatalf("the publisher holds ledger %q after a load, want the loaded %q", got, raw)
	}
}
