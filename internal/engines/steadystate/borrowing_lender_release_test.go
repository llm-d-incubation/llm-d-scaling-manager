package steadystate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// lenderReleaseWorld is namespace "b" at guaranteed 16 holding 48 GPUs on
// H100 (six 8-GPU replicas of model "x"), in cohort "c", whose pool opts in
// to releaseToNonReclaimingLenders and reads a Never lender waiting for 16.
func lenderReleaseWorld(readAt time.Time) ([]*allocation.ResourceConstraints, []allocation.ModelScalingRequest) {
	c := &allocation.ResourceConstraints{
		ProviderName: "namespace-quota", NamespaceScoped: true, KueueBacked: true, KueueCountsBorrowing: true,
		NamespacePools:   map[string]map[string]allocation.ResourcePool{"b": {"H100": {Limit: 16, Used: 48}}},
		NamespaceBorrow:  map[string]map[string]allocation.NamespaceBorrow{"b": {"H100": {Cap: config.QuotaUnlimited}}},
		NamespaceCohorts: map[string][]string{"b": {"c"}},
		SharedPools: []allocation.SharedPoolChunk{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
			FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: "NVIDIA-H100-80GB-HBM3"},
			Lenders: []kueue.LenderState{{Queue: "lender", Waiting: true, Reclaim: kueue.ReclaimNever, Unused: 32, PendingGPUs: 16,
				PendingSince: readAt.Add(-time.Hour)}},
			ReadAt: readAt, Borrowed: 32, ReleaseToNonReclaiming: true}},
	}
	reqs := []allocation.ModelScalingRequest{{ModelID: "x", Namespace: "b", Priority: 1,
		Variants: []domain.VariantMetadata{{VariantName: "x-v", AcceleratorName: "H100"}},
		VariantStates: []domain.VariantReplicaState{{VariantName: "x-v", CurrentReplicas: 6, GPUsPerReplica: 8,
			AcceleratorName: "H100"}}}}
	return []*allocation.ResourceConstraints{c}, reqs
}

func lenderDecisions(reqs []allocation.ModelScalingRequest) []domain.VariantDecision {
	out := make([]domain.VariantDecision, 0, len(reqs))
	for _, r := range reqs {
		for _, s := range r.VariantStates {
			out = append(out, domain.VariantDecision{Namespace: r.Namespace, ModelID: r.ModelID, VariantName: s.VariantName,
				AcceleratorName: s.AcceleratorName, CurrentReplicas: s.CurrentReplicas, TargetReplicas: s.CurrentReplicas,
				GPUsPerReplica: s.GPUsPerReplica})
		}
	}
	return out
}

// A release to a lender is persisted in the controller status, and a new
// leader term that reads it back holds the pin and releases nothing again.
func TestALenderReleaseSurvivesANewLeaderTerm(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(50000, 0)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cs, reqs := lenderReleaseWorld(now)

	first := fake.NewClientBuilder().WithScheme(scheme).Build()
	e := &Engine{Config: config.NewTestConfig(), Status: controllerstatus.New(first, first, config.SystemNamespace())}
	e.borrowing.governor = allocation.NewBorrowGovernor(allocation.DefaultBorrowSettings())
	e.borrowing.ledgerLoaded = true
	e.borrowing.cycle = allocation.BorrowCycle{Now: now, Constraints: cs, Requests: reqs}
	e.borrowing.governor.Observe(e.borrowing.cycle)
	ds := lenderDecisions(reqs)
	e.releaseBorrowed(ctx, reqs, ds, nil, now)
	if ds[0].TargetReplicas != 4 {
		t.Fatalf("x's target is %d, want 4: two 8-GPU replicas for the lender's 16", ds[0].TargetReplicas)
	}
	raw := e.Status.Data()[controllerstatus.KeyBorrowReleaseLedger]
	if !strings.Contains(raw, `"lenderReleases"`) || !strings.Contains(raw, `"version":1`) {
		t.Fatalf("persisted ledger %q carries no versioned lender release", raw)
	}

	// The next term reads the ConfigMap the last one wrote.
	next := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: controllerstatus.ConfigMapName, Namespace: config.SystemNamespace()},
		Data:       map[string]string{controllerstatus.KeyBorrowReleaseLedger: raw}}).Build()
	e2 := &Engine{Config: config.NewTestConfig(), client: next, Status: controllerstatus.New(next, next, config.SystemNamespace())}
	if !e2.restoreHeldReleases(ctx) {
		t.Fatal("a cycle that counts no borrowing dropped the persisted lender release")
	}
	later := now.Add(time.Minute)
	ds2 := lenderDecisions(reqs)
	e2.releaseBorrowed(ctx, reqs, ds2, nil, later)
	if ds2[0].TargetReplicas != 4 {
		t.Errorf("after the new term x's target is %d, want the pin at 4", ds2[0].TargetReplicas)
	}
	e2.borrowing.cycle = allocation.BorrowCycle{Now: later, Constraints: cs, Requests: reqs}
	e2.borrowing.governor.Observe(e2.borrowing.cycle)
	ds3 := lenderDecisions(reqs)
	if rel := e2.borrowing.governor.Releases(e2.borrowing.cycle, ds3); len(rel) != 0 || ds3[0].TargetReplicas != 4 {
		t.Errorf("the new term released %+v (target %d); want nothing new and the pin at 4", rel, ds3[0].TargetReplicas)
	}
}

// PR ev-shindin/llm-scaling-manager#203 review SF2: a release's latency is measured per cohort whose pool
// opts in, over the models a release there may lower, and capped; a tenant
// elsewhere, or one an Any lender reclaims from, does not lengthen it.
func TestReleaseLatencyIsPerCohortAndCapped(t *testing.T) {
	now := time.Unix(50000, 0)
	grace := func(name string, s int64) *appsv1.Deployment {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "b"}}
		d.Spec.Template.Spec.TerminationGracePeriodSeconds = &s
		return d
	}
	cs, reqs := lenderReleaseWorld(now)
	// Cohort "d", not opted in, with a tenant whose pods take an hour to stop.
	other := *cs[0]
	other.NamespacePools = map[string]map[string]allocation.ResourcePool{"e": {"H100": {Limit: 0, Used: 8}}}
	other.NamespaceBorrow = map[string]map[string]allocation.NamespaceBorrow{"e": {"H100": {Cap: config.QuotaUnlimited}}}
	other.NamespaceCohorts = map[string][]string{"e": {"d"}}
	other.SharedPools = []allocation.SharedPoolChunk{cs[0].SharedPools[0]}
	other.SharedPools[0].Cohort, other.SharedPools[0].ReleaseToNonReclaiming = "d", false
	cs = append(cs, &other)
	reqs = append(reqs, allocation.ModelScalingRequest{ModelID: "y", Namespace: "e",
		VariantStates: []domain.VariantReplicaState{{VariantName: "y-v", CurrentReplicas: 1, GPUsPerReplica: 8, AcceleratorName: "H100"}}})
	// In cohort c, a variant on a type the pool does not serve: no release
	// there lowers it.
	reqs[0].VariantStates = append(reqs[0].VariantStates, domain.VariantReplicaState{VariantName: "z-v", CurrentReplicas: 1,
		GPUsPerReplica: 8, AcceleratorName: "A100"})
	targets := map[string]scaletarget.ScaleTargetAccessor{
		"b/x-v": scaletarget.NewDeploymentAccessor(grace("x-v", 60)),
		"b/z-v": scaletarget.NewDeploymentAccessor(grace("z-v", 3600)),
		"e/y-v": scaletarget.NewDeploymentAccessor(grace("y-v", 3600)),
	}
	e := &Engine{Config: config.NewTestConfig()}
	got := releaseLatencies(e, cs, reqs, targets)
	// 300s window + 15s HPA sync + 30s poll + x's 60s grace.
	if len(got) != 1 || got["c"] != 405*time.Second {
		t.Errorf("latencies %v, want c alone at 405s: neither cohort d's tenant nor c's A100 variant counts", got)
	}
	targets["b/x-v"] = scaletarget.NewDeploymentAccessor(grace("x-v", 7200))
	if got := releaseLatencies(e, cs, reqs, targets); got["c"] != allocation.MaxReleaseLatency {
		t.Errorf("latency %v with a two-hour grace, want the cap %v", got["c"], allocation.MaxReleaseLatency)
	}
}

// PR ev-shindin/llm-scaling-manager#203 review (GT minor): a ledger a newer build wrote is held unread --
// no release to a lender starts -- and is not persisted over.
func TestALedgerOfANewerVersionIsHeldUnread(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(50000, 0)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	newer := `{"version":2,"lenderReleases":[{"cohort":"c","lender":"lender","flavor":"h100","resource":"nvidia.com/gpu"}]}`
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: controllerstatus.ConfigMapName, Namespace: config.SystemNamespace()},
		Data:       map[string]string{controllerstatus.KeyBorrowReleaseLedger: newer}}).Build()
	cs, reqs := lenderReleaseWorld(now)
	e := &Engine{Config: config.NewTestConfig(), client: c, Status: controllerstatus.New(c, c, config.SystemNamespace())}
	e.borrowing.inFlight = allocation.NewBorrowInFlight()
	e.governBorrowing(ctx, cs, reqs, nil, now)
	if !e.borrowing.cycle.LedgerUnread || e.borrowing.ledgerUnread != ledgerNewerReason {
		t.Fatalf("unread %t reason %q, want the newer ledger held unread", e.borrowing.cycle.LedgerUnread, e.borrowing.ledgerUnread)
	}
	ds := lenderDecisions(reqs)
	e.releaseBorrowed(ctx, reqs, ds, nil, now)
	if ds[0].TargetReplicas != 6 {
		t.Errorf("x's target is %d, want 6: nothing is released while a newer build's ledger is unread", ds[0].TargetReplicas)
	}
	if raw := e.Status.Data()[controllerstatus.KeyBorrowReleaseLedger]; raw != "" {
		t.Errorf("persisted %q over the newer build's ledger", raw)
	}
}

// Until the persisted ledger is read, no release to a lender is decided.
func TestNoLenderReleaseWhileTheLedgerIsUnread(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(50000, 0)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	failing := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok {
				return errors.New("etcd is down")
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	cs, reqs := lenderReleaseWorld(now)
	e := &Engine{Config: config.NewTestConfig(), client: failing}
	e.borrowing.inFlight = allocation.NewBorrowInFlight()
	e.governBorrowing(ctx, cs, reqs, nil, now)
	if !e.borrowing.cycle.LedgerUnread {
		t.Fatal("the cycle does not say the ledger is unread after a failed read")
	}
	ds := lenderDecisions(reqs)
	if rel := e.borrowing.governor.Releases(e.borrowing.cycle, ds); len(rel) != 0 {
		t.Errorf("released %+v before the persisted ledger was read", rel)
	}
}
