package steadystate

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// restoreInFlight ends the wakes' wait (allocation.InFlightLedger.MarkRestored)
// once its read is done, whatever it found -- a ledger, no ConfigMap, a ledger
// it cannot parse -- and not while the read fails: a Forbidden read keeps
// wakes waiting until inFlightRestoreWait ends it.
//
// DefaultInFlight is process-wide: no t.Parallel.
func TestRestoreInFlightEndsTheWakeWaitOnceItHasRead(t *testing.T) {
	before := allocation.NewInFlightLedger()
	now := time.Now()
	before.Admit("ns/m-v", allocation.ReplicaShape{Variant: "ns/m-v", Accelerator: "A100", GPUs: 8}, 2, 1, nil, now)
	raw, err := before.MarshalInFlight(now)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	forbidden := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "scaling-manager-controller-status", errors.New("rbac"))
		}}).Build()
	for _, tc := range []struct {
		name    string
		c       client.Client
		pending bool
	}{
		{"a ledger", statusWith(t, raw, nil), false},
		{"no ConfigMap", fake.NewClientBuilder().WithScheme(scheme).Build(), false},
		{"an unparseable ledger", statusWith(t, `{"version":1,"entries":{`, nil), false},
		{"a Forbidden read", forbidden, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allocation.DefaultInFlight.Reset()
			t.Cleanup(allocation.DefaultInFlight.Reset)
			allocation.DefaultInFlight.ExpectRestore(now)
			e := &Engine{Config: config.NewTestConfig(), client: tc.c}
			e.restoreInFlight(context.Background(), now)
			if got := allocation.DefaultInFlight.RestorePending(); got != tc.pending {
				t.Fatalf("after reading %s the restore is pending = %v, want %v", tc.name, got, tc.pending)
			}
		})
	}
}

// Review of ev-shindin/llm-scaling-manager#215: the skipped-variant test set cycleVariants by hand, so a
// key the cycle records that differs from the one the ledger holds (the
// Variant's name against a decision's VariantName) would pass it.
// Here a real optimizeV2 cycle skips the model -- nothing collected, or
// collection failed -- and the fitted tail that follows keeps its restored
// hold. The hold is keyed as a decision of that variant would key it.
//
// DefaultInFlight is process-wide: no t.Parallel.
func TestASkippedModelKeepsItsRestoredHoldThroughACycle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		collect func() *collector.ReplicaMetricsCollector
	}{
		{"nothing collected", func() *collector.ReplicaMetricsCollector {
			return collector.NewReplicaMetricsCollector(emptyMetricsSource{}, nil, nil, record.NewFakeRecorder(10), nil)
		}},
		{"collection failed", func() *collector.ReplicaMetricsCollector {
			return collector.NewReplicaMetricsCollector(failingMetricsSource{}, nil, nil, record.NewFakeRecorder(10), nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allocation.DefaultInFlight.Reset()
			t.Cleanup(allocation.DefaultInFlight.Reset)
			va := parkedVA()
			// The key inFlightAdmissions gives a decision of this variant:
			// discovery names a variant after its Variant.
			key := utils.GetNamespacedKey(va.Namespace, va.Name)
			now := time.Now()
			restoreHolds(t, now, 3, 2, key)

			e, _ := engineConfigEngine(t)
			e.Config = config.NewTestConfig()
			e.Config.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{config.GlobalDefaultsKey: {}})
			e.optimizer = allocation.NewCostAwareOptimizer()
			e.client = fake.NewClientBuilder().WithObjects(parkedTarget()).Build()
			e.Recorder = record.NewFakeRecorder(10)
			e.ReplicaMetricsCollector = tc.collect()
			e.optimizeV2(context.Background(), map[string][]llmdvariant.Variant{
				"ns/m": {va},
			}, nil)
			if !e.cycleVariants[key] {
				t.Fatalf("the cycle did not record the variant it skipped under %q: %v", key, e.cycleVariants)
			}

			// Another model's fitted cycle tail.
			o := growingA100("ns", "o", 8)
			atOne := []domain.VariantDecision{{VariantName: "o-v", Namespace: "ns", AcceleratorName: "A100", TargetReplicas: 1}}
			at := now.Add(time.Second)
			e.admitInFlight([]allocation.ModelScalingRequest{o}, atOne, nil, nil, at)
			if !allocation.DefaultInFlight.Holds(key, at) {
				t.Fatal("a real cycle that skipped the model ended its restored hold")
			}
		})
	}
}

// A decision that names a variant with an unresolved accelerator, or one the
// cycle read no state for, says nothing about its replicas: the variant's
// restored hold is kept, as for a variant the cycle skipped.
//
// DefaultInFlight is process-wide: no t.Parallel.
func TestAnUnresolvedDecisionKeepsTheRestoredHold(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	now := time.Now()
	restoreHolds(t, now, 3, 2, "ns/u-v", "ns/s-v")
	u := growingA100("ns", "u", 8)
	decisions := []domain.VariantDecision{
		{VariantName: "u-v", Namespace: "ns", AcceleratorName: constants.UnresolvedAcceleratorType, TargetReplicas: 3},
		{VariantName: "s-v", Namespace: "ns", AcceleratorName: "A100", TargetReplicas: 3},
	}
	e := &Engine{cycleVariants: map[string]bool{"ns/u-v": true, "ns/s-v": true}}
	at := now.Add(time.Second)
	e.admitInFlight([]allocation.ModelScalingRequest{u}, decisions, nil, nil, at)
	if !allocation.DefaultInFlight.Holds("ns/u-v", at) {
		t.Fatal("a decision with an unresolved accelerator ended the variant's restored hold")
	}
	if !allocation.DefaultInFlight.Holds("ns/s-v", at) {
		t.Fatal("a decision with no state ended the variant's restored hold")
	}
}

// restoredLWSPod is pod worker (0 the leader) of group g of LeaderWorkerSet m-lws,
// bound to a node.
func restoredLWSPod(g, worker int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "m-lws-" + string(rune('0'+g)) + "-" + string(rune('0'+worker)), Namespace: "ns",
			Labels: map[string]string{lwsv1.SetNameLabelKey: "m-lws", lwsv1.GroupIndexLabelKey: string(rune('0' + g))}},
		Spec:   corev1.PodSpec{NodeName: "node-" + string(rune('0'+g))},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// A restored hold of a LeaderWorkerSet counts its groups through
// restoredPlaced: a group only some of whose pods hold a node is partial, and
// the hold ends once every group is whole. A count that cannot read its
// target counts nothing.
func TestARestoredLeaderWorkerSetCountsItsGroups(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, lwsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	lws := &lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "m-lws", Namespace: "ns"},
		Spec: lwsv1.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2)),
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{Size: ptr.To(int32(2))}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, restoredLWSPod(0, 0), restoredLWSPod(0, 1), restoredLWSPod(1, 0)).Build()
	e := &Engine{client: c}
	count := e.restoredPlaced("ns/m-v", allocation.ScaleTargetRef{Kind: constants.LeaderWorkerSetKind, Name: "m-lws"})
	if count == nil {
		t.Fatal("no count for a restored LeaderWorkerSet")
	}
	p, ok := count()
	if !ok || p.Whole != 1 || len(p.Partial) != 1 || p.Partial[0].Node != "node-1" {
		t.Fatalf("counted %+v (ok %v), want one whole group and group 1 partial on node-1", p, ok)
	}
	if err := c.Create(context.Background(), restoredLWSPod(1, 1)); err != nil {
		t.Fatal(err)
	}
	if p, ok := count(); !ok || p.Whole != 2 || len(p.Partial) != 0 {
		t.Fatalf("counted %+v (ok %v) once group 1 is whole, want two whole groups", p, ok)
	}
	missing := e.restoredPlaced("ns/x-v", allocation.ScaleTargetRef{Kind: constants.LeaderWorkerSetKind, Name: "gone"})
	if _, ok := missing(); ok {
		t.Fatal("a count of a scale target that cannot be read claimed to know")
	}
}
