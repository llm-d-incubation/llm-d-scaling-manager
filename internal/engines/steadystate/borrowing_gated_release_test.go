package steadystate

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// kueueSnapshot is a QuotaSource reporting one fixed Kueue read, replaceable.
type kueueSnapshot struct {
	mu   sync.Mutex
	snap config.ExternalQuotas
}

func (s *kueueSnapshot) Quotas(context.Context) (config.ExternalQuotas, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap, nil
}

func (s *kueueSnapshot) set(q config.ExternalQuotas) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = q
}

// eligibleAs is a borrowing eligibility check that answers reason for every
// namespace ("" is eligible).
type eligibleAs kueue.Reason

func (r eligibleAs) Evaluate(_ context.Context, namespaces []string) map[string]kueue.NamespaceEligibility {
	out := map[string]kueue.NamespaceEligibility{}
	for _, ns := range namespaces {
		out[ns] = kueue.NamespaceEligibility{Reason: kueue.Reason(r)}
	}
	return out
}

// servingQueue is a Kueue read granting namespace serving one ClusterQueue of
// 16 H100 GPUs, in cohort (none when empty), with reserved GPUs reserved in it.
func servingQueue(cohort string, reserved int, now time.Time) config.ExternalQuotas {
	caps := config.ExternalCaps{ByType: map[string]int{"H100": 16}}
	row := config.ExternalBorrow{Queue: "a", Cohort: cohort, Flavor: "h100", Resource: "nvidia.com/gpu", Accelerator: "H100",
		Nominal: 16, Guaranteed: 16, WithinNominal: 16, Reserved: reserved}
	q := config.ExternalQuotas{
		Namespace:              map[string]config.ExternalCaps{"serving": caps},
		NamespaceGuaranteed:    map[string]config.ExternalCaps{"serving": caps},
		NamespaceWithinNominal: map[string]config.ExternalCaps{"serving": caps},
		ObservedAt:             now,
	}
	if cohort == "" {
		row.Reason = string(kueue.ReasonNoCohort)
	} else {
		q.SharedPools = []config.ExternalSharedPool{{Cohort: cohort, Flavor: "h100", Resource: "nvidia.com/gpu", Accelerator: "H100"}}
	}
	q.Borrow = map[string][]config.ExternalBorrow{"serving": {row}}
	return q
}

// gatedFixture is serving's model m, variant v, on a Kueue-backed quota entry
// kq over servingQueue, and an engine reading pods from a fake client.
type gatedFixture struct {
	t       *testing.T
	ctx     context.Context
	t0      time.Time
	src     *kueueSnapshot
	rec     *allocation.BorrowInFlight
	limiter *allocation.DefaultLimiter
	scheme  *runtime.Scheme
}

func newGatedFixture(t *testing.T, cohort string, kq config.KueueQuotaSource, eligibility kueue.Reason) *gatedFixture {
	f := &gatedFixture{t: t, ctx: context.Background(), t0: time.Now(), src: &kueueSnapshot{}, rec: allocation.NewBorrowInFlight()}
	f.src.set(servingQueue(cohort, 16, f.t0))
	entry := config.QuotaLimiterConfig{Name: "kq", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}}, Kueue: &kq}
	inv := allocation.NewQuotaInventoryWithSource(entry, f.src)
	inv.SetGatedSource(f.rec)
	if entry.KueueCountsBorrowable() {
		inv.EnableBorrowing(allocation.BorrowOptions{Physical: true, Eligibility: eligibleAs(eligibility)})
	}
	f.limiter = allocation.NewDefaultLimiter(entry.Name, inv)
	f.scheme = runtime.NewScheme()
	if err := corev1.AddToScheme(f.scheme); err != nil {
		t.Fatal(err)
	}
	return f
}

// compute is the entry's constraints for serving holding 16 GPUs.
func (f *gatedFixture) compute() []*allocation.ResourceConstraints {
	f.t.Helper()
	c, err := f.limiter.ComputeConstraints(f.ctx, nil, map[string]map[string]int{"serving": {"H100": 16}})
	if err != nil {
		f.t.Fatal(err)
	}
	if c.NamespaceBorrow != nil {
		f.t.Fatalf("the entry counts borrowing for %v; the case is one that counts none", c.NamespaceBorrow)
	}
	return []*allocation.ResourceConstraints{c}
}

// figure is serving's H100 figure in cs.
func figure(cs []*allocation.ResourceConstraints) int {
	return cs[0].NamespacePools["serving"]["H100"].Limit
}

var gpus8 = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}

// pod is one of serving's 8-GPU pods: gated at Kueue's admission gate, or on
// node n1.
func pod(ns, name string, gated bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: k8stypes.UID("uid-" + name)},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c",
			Resources: corev1.ResourceRequirements{Requests: gpus8, Limits: gpus8}}}},
	}
	if gated {
		p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: kueue.AdmissionGate}}
	} else {
		p.Spec.NodeName = "n1"
	}
	return p
}

// reservedWorkload is the Workload Kueue's pod integration owns by p, with
// quota reserved for it (QuotaReserved True): p waits on an admission check.
func reservedWorkload(p *corev1.Pod) *unstructured.Unstructured {
	w := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
			"type": kueue.WorkloadQuotaReserved, "status": "True",
		}}},
	}}
	w.SetAPIVersion(kueue.Group + "/v1beta2")
	w.SetKind(kueue.KindWorkload)
	w.SetNamespace(p.Namespace)
	w.SetName("pod-" + p.Name)
	w.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: p.Name, UID: p.UID}})
	return w
}

func (f *gatedFixture) engine(funcs *interceptor.Funcs, objs ...client.Object) (*Engine, *record.FakeRecorder) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: kueue.Group, Version: "v1beta2"}, {Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Group: kueue.Group, Version: "v1beta2", Kind: kueue.KindWorkload}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Event"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	b := fake.NewClientBuilder().WithScheme(f.scheme).WithRESTMapper(mapper).WithObjects(objs...)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	r := record.NewFakeRecorder(10)
	e := &Engine{Config: config.NewTestConfig(), Recorder: r, client: b.Build()}
	e.borrowing.inFlight = f.rec
	return e, r
}

var servingTargets = map[string]scaletarget.ScaleTargetAccessor{"serving/v": scaletarget.NewDeploymentAccessor(
	&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "serving"}})}

// servingState is m at 2 replicas, with v-1 held at Kueue's gate from the
// given time when gated is set.
func servingState(gated bool, since time.Time) []allocation.ModelScalingRequest {
	s := domain.VariantReplicaState{VariantName: "v", CurrentReplicas: 2, DesiredReplicas: 2, GPUsPerReplica: 8,
		AcceleratorName: "H100"}
	if gated {
		s.UnplacedReplicas, s.UnplacedSince, s.UnplacedPods = 1, since, []string{"v-1"}
		s.UnplacedAt = map[string]time.Time{"v-1": since}
		s.GatedAt = map[string]time.Time{"v-1": since}
	}
	return []allocation.ModelScalingRequest{{ModelID: "m", Namespace: "serving", VariantStates: []domain.VariantReplicaState{s}}}
}

func servingDecisions() []domain.VariantDecision {
	return []domain.VariantDecision{{Namespace: "serving", ModelID: "m", VariantName: "v", AcceleratorName: "H100",
		CurrentReplicas: 2, TargetReplicas: 2, GPUsPerReplica: 8}}
}

// cycle runs one cycle's borrowing steps at the given time and returns m's
// target.
func (f *gatedFixture) cycle(e *Engine, reqs []allocation.ModelScalingRequest, at time.Time) int {
	e.prepareBorrowing(f.ctx, f.compute(), reqs, servingTargets, at)
	ds := servingDecisions()
	e.releaseBorrowed(f.ctx, reqs, ds, servingTargets, at)
	return ds[0].TargetReplicas
}

// A replica Kueue keeps at its admission gate, in a Kueue-backed namespace
// that counts no borrowing: an entry with countBorrowable: false, and a
// queue in no cohort. In 8-GPU units: serving's queue holds 16; an unmanaged
// training job holds 8 of it, serving's variant 8 admitted and 8 more gated.
// Kueue reserved 16 and will never admit the gated replica.
//
//   - The charge. The engine lists the gated pod, so the next read of the
//     quota does not take its GPUs off Kueue's reservation: the figure is the
//     training job's 8 less, 8, not 16. Before, the gated pods were listed only
//     where borrowing was counted: the gated 8 cancelled the training job's
//     charge, the figure read 16, and nothing was past it.
//   - The release. Past the placement timeout the gated replica is released,
//     2 -> 1, with the BorrowedCapacityReleased Event of cause kueue-gate.
//     Before, the placement timeout ran only where borrowing was counted, and
//     the replica waited gated for good.
//   - Control: a replica Kueue admits within the timeout -- the training job
//     ended -- is not released.
func TestGatedReplicaOutsideBorrowingIsChargedAndReleased(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name   string
		cohort string
		entry  config.KueueQuotaSource
	}{
		{"countBorrowable false", "c", config.KueueQuotaSource{Enabled: true, CountBorrowable: &no}},
		{"queue in no cohort", "", config.KueueQuotaSource{Enabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGatedFixture(t, tc.cohort, tc.entry, "")
			t0, timeout := f.t0, config.DefaultPlacementTimeout
			gatedSince := t0.Add(-time.Minute)

			// The charge.
			e, events := f.engine(nil, pod("serving", "v-0", false), pod("serving", "v-1", true))
			if got := figure(f.compute()); got != 16 {
				t.Fatalf("before any pod list serving's figure is %d, want 16: the gated 8 still read as reserved", got)
			}
			e.prepareBorrowing(f.ctx, f.compute(), servingState(true, gatedSince), servingTargets, t0)
			if got := figure(f.compute()); got != 8 {
				t.Fatalf("serving's figure is %d, want 8: the training job's 8 stay charged; the gated replica is not Kueue's reservation", got)
			}

			// The release.
			if got := f.cycle(e, servingState(true, gatedSince), t0); got != 2 {
				t.Fatalf("released at once to %d; the placement timeout has not passed", got)
			}
			t1 := t0.Add(timeout)
			if got := f.cycle(e, servingState(true, gatedSince), t1); got != 1 {
				t.Fatalf("serving's target is %d after the placement timeout, want 1: the gated replica is released", got)
			}
			select {
			case ev := <-events.Events:
				if !strings.Contains(ev, constants.K8SEventBorrowedCapacityReleased) || !strings.Contains(ev, "admission gate") ||
					!strings.Contains(ev, string(allocation.ReleaseKueueGate)) {
					t.Errorf("event %q, want %s of cause %s", ev, constants.K8SEventBorrowedCapacityReleased, allocation.ReleaseKueueGate)
				}
			default:
				t.Error("no Event for the release")
			}
			if e.borrowing.governor.CoolingDown("serving", "m", t1) {
				t.Error("the release started a borrowing cooldown: nothing was borrowed")
			}

			// Control: Kueue admits the replica within the timeout.
			f.rec.Reset()
			f.src.set(servingQueue(tc.cohort, 16, t0))
			e, _ = f.engine(nil, pod("serving", "v-0", false), pod("serving", "v-1", true))
			f.cycle(e, servingState(true, gatedSince), t0)
			f.cycle(e, servingState(true, gatedSince), t0)
			admitted := t0.Add(timeout - time.Minute)
			f.src.set(servingQueue(tc.cohort, 16, admitted)) // the training job ended; serving's 16 are reserved
			placed, _ := f.engine(nil, pod("serving", "v-0", false), pod("serving", "v-1", false))
			e.client = placed.client
			for _, at := range []time.Time{admitted, t1, t1.Add(timeout)} {
				if got := f.cycle(e, servingState(false, time.Time{}), at); got != 2 {
					t.Fatalf("at +%s serving's target is %d, want 2: Kueue admitted the replica within the timeout", at.Sub(t0), got)
				}
			}
		})
	}
}

// Review ev-shindin/llm-scaling-manager#199 (1): a gated pod whose Workload Kueue already reserved quota
// for -- waiting on an admission check, a ProvisioningRequest asking for a
// node -- is in Kueue's reservation. Queue of 16 in no cohort, serving holds
// 8 admitted and 8 reserved waiting for a node: Kueue reserved serving's 16
// and nothing else. Its figure stays 16, so nothing is past it. Before, the
// gated pod was taken off the managed usage and charged as someone else's:
// the figure read 8, 8 past it, and the replica waiting for its node was
// released at the placement timeout, its ProvisioningRequest cancelled.
func TestReservedGatedReplicaIsNotChargedAsAnothers(t *testing.T) {
	f := newGatedFixture(t, "", config.KueueQuotaSource{Enabled: true}, "")
	waiting := pod("serving", "v-1", true)
	e, _ := f.engine(nil, pod("serving", "v-0", false), waiting, reservedWorkload(waiting))
	f.cycle(e, servingState(false, time.Time{}), f.t0)
	if got := figure(f.compute()); got != 16 {
		t.Fatalf("serving's figure is %d, want 16: Kueue reserved the gated replica; it is serving's own, not another workload's", got)
	}
}

// Review ev-shindin/llm-scaling-manager#199 (2): an entry that counts borrowing, with the namespace
// withheld for a while (here its ResourceQuotas cannot be read), is not
// governed as one that counts none: Kueue may still admit the gated replica
// by borrowing, and the borrowing release state (HoldReleases) is kept.
// Before, the cycle observed the governor and released the gated replica
// against the namespace's nominal at the placement timeout. A queue in no
// cohort under the same entry is released, as above.
func TestWithheldNamespaceOfACountingEntryIsNotReleasedAtTheGate(t *testing.T) {
	f := newGatedFixture(t, "c", config.KueueQuotaSource{Enabled: true}, kueue.ReasonResourceQuotaUnreadable)
	e, _ := f.engine(nil, pod("serving", "v-0", false), pod("serving", "v-1", true))
	gatedSince := f.t0.Add(-time.Minute)
	f.cycle(e, servingState(true, gatedSince), f.t0)
	f.cycle(e, servingState(true, gatedSince), f.t0)
	if cs := f.compute(); cs[0].BorrowWithheld["serving"] != string(kueue.ReasonResourceQuotaUnreadable) {
		t.Fatalf("serving is withheld for %q, want %s", cs[0].BorrowWithheld["serving"], kueue.ReasonResourceQuotaUnreadable)
	}
	if got := f.cycle(e, servingState(true, gatedSince), f.t0.Add(2*config.DefaultPlacementTimeout)); got != 2 {
		t.Fatalf("serving's target is %d, want 2: a namespace an entry counting borrowing withholds is not released at the gate", got)
	}
}

// Review ev-shindin/llm-scaling-manager#199 (3): a cycle that counts no borrowing reads no Events for the
// gated release. The cluster autoscaler does not act on a gated pod, so no
// scale-up stretches its timeout; before, the cycle listed the
// cluster-autoscaler Events of every unplaced pod past the timeout, in every
// namespace, Kueue's or not -- two uncached LISTs per namespace per cycle.
func TestGatedReleaseReadsNoEvents(t *testing.T) {
	no := false
	f := newGatedFixture(t, "c", config.KueueQuotaSource{Enabled: true, CountBorrowable: &no}, "")
	var lists atomic.Int32
	funcs := &interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*corev1.EventList); ok {
			lists.Add(1)
		}
		return cl.List(ctx, list, opts...)
	}}
	e, _ := f.engine(funcs, pod("serving", "v-0", false), pod("serving", "v-1", true), pod("other", "o-1", true))
	since := f.t0.Add(-time.Minute)
	other := allocation.ModelScalingRequest{ModelID: "o", Namespace: "other", VariantStates: []domain.VariantReplicaState{{
		VariantName: "o", CurrentReplicas: 1, GPUsPerReplica: 8, AcceleratorName: "H100",
		UnplacedReplicas: 1, UnplacedSince: since, UnplacedPods: []string{"o-1"}, UnplacedAt: map[string]time.Time{"o-1": since}}}}
	for _, at := range []time.Time{f.t0, f.t0.Add(config.DefaultPlacementTimeout)} {
		f.cycle(e, append(servingState(true, since), other), at)
	}
	if n := lists.Load(); n != 0 {
		t.Fatalf("%d Event LISTs in cycles that count no borrowing, want 0", n)
	}
}
