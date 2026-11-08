package locator_test

import (
	"context"
	"errors"
	"testing"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/locator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		kedav1alpha1.AddToScheme,
		lwsv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("scheme add: %v", err)
		}
	}
	return s
}

func newClients(t *testing.T, objs ...runtime.Object) (apiReader client.Client) {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithRuntimeObjects(objs...).
		Build()
}

// registered stands in for KEDA having called the scaling manager's external scaler about a
// workload, with its scale target already resolved. That registration is the ONLY
// thing that makes a pod attributable now: the locator reads the registry, not a
// field index, so there is no cluster-wide ScaledObject watch behind it.
func registered(ns, targetKind, targetName string) *registry.Registry {
	// The scaler name is fixed: every caller resolves BY TARGET, so varying it
	// proved nothing. The tests that passed a different one looked it up by name,
	// and went when the by-variant entry point did.
	const scalerName = "h"
	reg := registry.New(0)
	reg.Observe(ns, scalerName, map[string]string{registry.ModelIDKey: "m"})
	reg.SetTarget(ns, scalerName, registry.Target{
		APIVersion: "apps/v1", Kind: targetKind, Name: targetName,
	})
	return reg
}

// variantsOf adapts a registry to the provider locator.New expects.
func variantsOf(reg *registry.Registry) func() *registry.Registry {
	return func() *registry.Registry { return reg }
}

const testNamespace = "default"

func TestLocate_UnmanagedReturnsNil(t *testing.T) {
	ns := testNamespace
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: ns, UID: "uid-rs",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true)}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true)}}},
	}
	apiReader := newClients(t, deploy, rs, pod)
	loc, _ := locator.New(apiReader, nil)
	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
}

func TestLocate_PodNotFound(t *testing.T) {
	apiReader := newClients(t)
	loc, _ := locator.New(apiReader, nil)
	got, err := loc.Locate(context.Background(), testNamespace, "missing")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
}

func TestLocate_CacheHitOnSecondCall(t *testing.T) {
	ns := testNamespace
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: ns, UID: "uid-rs",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true)}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true)}}}}
	apiReader := newClients(t, deploy, rs, pod)
	loc, _ := locator.New(apiReader, variantsOf(registered(ns, "Deployment", "d")))

	// Warm the cache.
	if _, err := loc.Locate(context.Background(), ns, "p"); err != nil {
		t.Fatalf("first Locate: %v", err)
	}

	// Delete the pod from apiReader; if the cache works, the second Locate
	// must still resolve to the same ScaledObject because the pod → Deployment
	// step is cached.
	if err := apiReader.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("second Locate: %v", err)
	}
	if got == nil || got.Name != "h" {
		t.Errorf("cache miss on second call: got=%v", got)
	}
}

func TestLocate_LWSChain(t *testing.T) {
	ns := testNamespace
	lws := &lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "lws", Namespace: ns, UID: "uid-lws"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "leaderworkerset.x-k8s.io/v1", Kind: "LeaderWorkerSet",
				Name: "lws", UID: "uid-lws", Controller: ptr.To(true),
			}}},
	}
	apiReader := newClients(t, lws, pod)
	loc, _ := locator.New(apiReader, variantsOf(registered(ns, "LeaderWorkerSet", "lws")))
	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil || got == nil || got.Name != "h" {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

// TestLocate_KEDADisabledSkipsScaledObject verifies that when KEDA is disabled the
// locator does not touch the (unregistered) ScaledObject field index, so Locate
// returns the managed HPA without erroring on the missing index.
// TestLocate_NothingRegisteredFindsNothing: attribution is only possible for a
// workload KEDA has called the scaling manager about. With an empty registry the answer is "not
// mine" — reported quietly, not as an error, because that is the normal state for
// every unmanaged pod in the cluster.
func TestLocate_NothingRegisteredFindsNothing(t *testing.T) {
	ns := testNamespace
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: ns, UID: "uid-rs",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true)}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true)}}},
	}
	apiReader := newClients(t, deploy, rs, pod)
	loc, _ := locator.New(apiReader, variantsOf(registry.New(0)))

	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("Locate must not error on an unmanaged workload: %v", err)
	}
	if got != nil {
		t.Fatalf("got=%v, want nil", got)
	}
}

// TestLocate_RegisteredForADifferentTargetFindsNothing guards the match itself: a
// registered workload must not attribute a pod belonging to some other Deployment.
func TestLocate_RegisteredForADifferentTargetFindsNothing(t *testing.T) {
	ns := testNamespace
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: ns, UID: "uid-rs",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true)}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true)}}},
	}
	apiReader := newClients(t, deploy, rs, pod)
	loc, _ := locator.New(apiReader, variantsOf(registered(ns, "Deployment", "some-other-deployment")))

	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != nil {
		t.Fatalf("got=%v, want nil: the registered entry scales a different workload", got)
	}
}

// TODO(va-removal): the TestResolveScaleTarget_* tests below cover the method
// added for the CRD-based dual-mode fallback. Remove them when the
// old CRD (and ResolveScaleTarget) are removed.

// TestResolveScaleTarget_UnmanagedDeployment is the locator-side counterpart to
// the KServe regression: the Deployment is fronted by no managed scaler (the
// same shape as TestLocate_UnmanagedReturnsNil), yet ResolveScaleTarget still
// returns the Deployment ref so the collector can fall back to a VA lookup.
func TestResolveScaleTarget_UnmanagedDeployment(t *testing.T) {
	ns := testNamespace
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: ns, UID: "uid-rs",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true)}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true)}}}}

	apiReader := newClients(t, deploy, rs, pod)
	loc, _ := locator.New(apiReader, nil)

	ref, ok, err := loc.ResolveScaleTarget(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("ResolveScaleTarget: %v", err)
	}
	if !ok {
		t.Fatalf("ok=false, want true (chain reaches Deployment d)")
	}
	if ref.Kind != "Deployment" || ref.Name != "d" || ref.APIVersion != "apps/v1" {
		t.Errorf("ref=%+v, want apps/v1 Deployment d", ref)
	}
}

func TestResolveScaleTarget_NoScalerEligibleAncestor(t *testing.T) {
	ns := testNamespace
	// Pod with no controller owner → no Deployment/LWS ancestor.
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns}}
	apiReader := newClients(t, pod)
	loc, _ := locator.New(apiReader, nil)

	ref, ok, err := loc.ResolveScaleTarget(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("ResolveScaleTarget: %v", err)
	}
	if ok {
		t.Errorf("ok=true, want false; ref=%+v", ref)
	}
}

func TestResolveScaleTarget_PodNotFound(t *testing.T) {
	apiReader := newClients(t)
	loc, _ := locator.New(apiReader, nil)

	_, ok, err := loc.ResolveScaleTarget(context.Background(), testNamespace, "missing")
	if err != nil {
		t.Fatalf("ResolveScaleTarget: %v", err)
	}
	if ok {
		t.Errorf("ok=true, want false for a missing pod")
	}
}

// TestResolveScaleTarget_ReusesLocateCache verifies the pod→target step is
// memoized across Locate and ResolveScaleTarget: after Locate warms the cache,
// deleting the pod from apiReader does not prevent ResolveScaleTarget from
// resolving the same Deployment.
func TestResolveScaleTarget_ReusesLocateCache(t *testing.T) {
	ns := testNamespace
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: ns, UID: "uid-rs",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true)}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true)}}}}

	apiReader := newClients(t, deploy, rs, pod)
	loc, _ := locator.New(apiReader, nil)

	// Warm the pod→target cache via Locate (no managed scaler, returns nil).
	if _, err := loc.Locate(context.Background(), ns, "p"); err != nil {
		t.Fatalf("Locate: %v", err)
	}
	// Delete the pod; a cache hit means ResolveScaleTarget never reads it.
	if err := apiReader.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	ref, ok, err := loc.ResolveScaleTarget(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("ResolveScaleTarget: %v", err)
	}
	if !ok || ref.Name != "d" {
		t.Errorf("cache miss: ok=%v ref=%+v, want ok=true Deployment d", ok, ref)
	}
}

func TestGetPodLabels(t *testing.T) {
	const (
		ns      = "test-ns"
		podName = "test-pod"
	)

	tests := []struct {
		name      string
		pod       *corev1.Pod
		namespace string
		podName   string
		want      map[string]string
		wantNil   bool
	}{
		{
			name:      "pod with labels",
			namespace: ns,
			podName:   podName,
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: ns,
					Labels: map[string]string{
						"app":                           "test",
						lwsv1.WorkerIndexLabelKey:       "0",
						"leaderworkerset.x-k8s.io/name": "lws-test",
					},
				},
			},
			want: map[string]string{
				"app":                           "test",
				lwsv1.WorkerIndexLabelKey:       "0",
				"leaderworkerset.x-k8s.io/name": "lws-test",
			},
		},
		{
			name:      "pod without labels",
			namespace: ns,
			podName:   "pod-no-labels",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod-no-labels",
					Namespace: ns,
				},
			},
			want: map[string]string{},
		},
		{
			name:      "pod does not exist",
			namespace: ns,
			podName:   "nonexistent",
			wantNil:   true,
		},
		{
			name:      "empty pod name",
			namespace: ns,
			podName:   "",
			wantNil:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []runtime.Object
			if tt.pod != nil {
				objs = append(objs, tt.pod)
			}
			apiReader := newClients(t, objs...)

			loc, err := locator.New(apiReader, nil)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			got := loc.GetPodLabels(context.Background(), tt.namespace, tt.podName)

			if tt.wantNil {
				if got != nil {
					t.Errorf("GetPodLabels() = %v, want nil", got)
				}
				return
			}

			if len(got) != len(tt.want) {
				t.Errorf("GetPodLabels() label count = %v, want %v", len(got), len(tt.want))
				return
			}

			for k, v := range tt.want {
				if gotV, ok := got[k]; !ok || gotV != v {
					t.Errorf("GetPodLabels()[%q] = %v, want %v", k, gotV, v)
				}
			}
		})
	}
}

func TestGetPodLabels_CacheReuse(t *testing.T) {
	const (
		ns      = "test-ns"
		podName = "test-pod"
	)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			Labels: map[string]string{
				"app":                     "test",
				lwsv1.WorkerIndexLabelKey: "0",
			},
		},
	}

	apiReader := newClients(t, pod)

	loc, err := locator.New(apiReader, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	labels1 := loc.GetPodLabels(context.Background(), ns, podName)
	if labels1 == nil {
		t.Fatal("GetPodLabels() first call returned nil")
	}

	// Delete the pod; a cache hit means the second call never reads it.
	if err := apiReader.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}

	labels2 := loc.GetPodLabels(context.Background(), ns, podName)
	if labels2 == nil {
		t.Fatal("GetPodLabels() second call returned nil — cache miss after pod deleted")
	}

	for k, v := range labels1 {
		if labels2[k] != v {
			t.Errorf("cached label %q = %q, want %q", k, labels2[k], v)
		}
	}
}

func TestGetPodLabels_TransientGetError(t *testing.T) {
	scheme := newScheme(t)
	errTransient := errors.New("connection refused")

	apiReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return errTransient
			},
		}).
		Build()

	loc, err := locator.New(apiReader, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	got := loc.GetPodLabels(context.Background(), "ns", "pod-a")
	if got != nil {
		t.Errorf("expected nil on transient Get error, got %v", got)
	}
}

func TestGetPodLabels_ResolveScaleTargetErrorSkipsCache(t *testing.T) {
	const ns = "test-ns"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: ns,
			Labels: map[string]string{"app": "test"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "rs",
				UID:        "uid-rs",
				Controller: ptr.To(true),
			}},
		},
	}

	scheme := newScheme(t)
	errTransient := errors.New("etcd timeout")
	callCount := 0

	apiReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.ReplicaSet); ok {
					callCount++
					return errTransient
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	loc, err := locator.New(apiReader, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// First call: pod Get succeeds, ReplicaSet Get fails → labels returned, cache skipped.
	got := loc.GetPodLabels(context.Background(), ns, "p")
	if got == nil {
		t.Fatal("expected labels on resolveScaleTarget error, got nil")
	}
	if got["app"] != "test" {
		t.Errorf("expected label app=test, got %v", got)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 ReplicaSet Get call, got %d", callCount)
	}

	// Second call: should retry the owner walk (not serve from cache).
	_ = loc.GetPodLabels(context.Background(), ns, "p")
	if callCount != 2 {
		t.Errorf("expected 2 ReplicaSet Get calls (cache was skipped), got %d", callCount)
	}
}

func TestGetPodLabels_WalkErrorDoesNotPoisonLocate(t *testing.T) {
	const ns = "test-ns"

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rs", Namespace: ns, UID: "uid-rs",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true),
			}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: ns,
			Labels: map[string]string{"app": "test"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true),
			}},
		},
	}

	scheme := newScheme(t)
	failOnce := true

	apiReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(deploy, rs, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.ReplicaSet); ok && failOnce {
					failOnce = false
					return errors.New("etcd timeout")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	loc, err := locator.New(apiReader, variantsOf(registered(ns, "Deployment", "d")))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// GetPodLabels triggers a transient walk error → labels returned, cache skipped.
	labels := loc.GetPodLabels(context.Background(), ns, "p")
	if labels == nil || labels["app"] != "test" {
		t.Fatalf("GetPodLabels: expected labels with app=test, got %v", labels)
	}

	// Locate must still resolve correctly — the cache must not be poisoned.
	ms, err := loc.Locate(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if ms == nil {
		t.Fatalf("Locate: expected the registered scaler, got nil")
	}
	if ms.Name != "h" {
		t.Errorf("Locate: scaler name = %q, want %q", ms.Name, "h")
	}
}
