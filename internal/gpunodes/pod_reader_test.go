package gpunodes

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

const testGPUProduct = "NVIDIA-H100-80GB-HBM3"

// gpuNode is the fixture GPU node gpu-1, the node gpuPod places Pods on.
func gpuNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-1", Labels: map[string]string{
		"nvidia.com/gpu.product": testGPUProduct}}}
}

// gpuPod is a Pod requesting gpus on the fixture node gpu-1.
func gpuPod(name, ns string, gpus int64) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{NodeName: "gpu-1", Containers: []corev1.Container{{
			Name: "c", Image: "registry.k8s.io/pause:3.9",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(gpus, resource.DecimalSI)},
				Limits:   corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(gpus, resource.DecimalSI)},
			},
		}}},
	}
}

// TestNamespaceScopedUsageCountsOtherTenants reproduces a namespace-scoped
// controller on a real API server: its cache holds only its own namespace, and
// another tenant's Pod holds GPUs on the same node. Through the cache alone the
// usage walk counted 2 of the node's 6 used GPUs; with the uncached pod reader
// it counts all 6.
func TestNamespaceScopedUsageCountsOtherTenants(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set: this test needs envtest (make setup-envtest)")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = env.Stop() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	direct, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"mine", "other-tenant"} {
		if err := direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	node := gpuNode()
	if err := direct.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	node.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}
	node.Status.Capacity = node.Status.Allocatable
	if err := direct.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*corev1.Pod{gpuPod("mine-0", "mine", 2), gpuPod("theirs-0", "other-tenant", 4)} {
		if err := direct.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	// The namespace-scoped manager's cache: DefaultNamespaces = the managed one.
	informers, err := cache.New(cfg, cache.Options{Scheme: scheme.Scheme,
		DefaultNamespaces: map[string]cache.Config{"mine": {}}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = informers.Start(ctx) }()
	if !informers.WaitForCacheSync(ctx) {
		t.Fatal("cache did not start")
	}
	cached, err := client.New(cfg, client.Options{Scheme: scheme.Scheme, Cache: &client.CacheOptions{Reader: informers}})
	if err != nil {
		t.Fatal(err)
	}

	// Fixture guard: through the cache alone the other tenant is invisible.
	cacheOnly, _, err := NewK8sWithGpuOperator(cached).DiscoverUsageByNamespace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cacheOnly[testGPUProduct] != 2 {
		t.Fatalf("fixture: the namespace cache should see only its own 2 GPUs, saw %d", cacheOnly[testGPUProduct])
	}

	byType, byNamespace, err := NewK8sWithGpuOperator(cached).WithPodReader(direct).DiscoverUsageByNamespace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := byType[testGPUProduct]; got != 6 {
		t.Errorf("GPUs in use on the node = %d, want 6 (2 mine + 4 another tenant's)", got)
	}
	if got := byNamespace["other-tenant"][testGPUProduct]; got != 4 {
		t.Errorf("other tenant's GPUs = %d, want 4", got)
	}
}

// TestForbiddenPodListIsReported: a namespace-scoped install may not be allowed
// to list Pods cluster-wide. The walk then counts what its cache holds, and
// must say that it is doing so: llmsm_pod_access_denied reads 1 until the list
// succeeds.
func TestForbiddenPodListIsReported(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	node := gpuNode()
	node.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}
	cached := fake.NewClientBuilder().WithScheme(s).WithObjects(node, gpuPod("mine-0", "mine", 2)).Build()
	forbidden := true
	reader := fake.NewClientBuilder().WithScheme(s).WithObjects(gpuPod("mine-0", "mine", 2)).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if forbidden {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)
			}
			return c.List(ctx, list, opts...)
		}}).Build()

	d := NewK8sWithGpuOperator(cached).WithPodReader(reader)
	byType, _, err := d.DiscoverUsageByNamespace(context.Background())
	if err != nil {
		t.Fatalf("a Forbidden cluster-wide list must fall back to the cache, got %v", err)
	}
	if got := byType[testGPUProduct]; got != 2 {
		t.Errorf("fallback usage = %d, want the cache's 2", got)
	}
	if got := podAccessDenied(t, registry); got != 1 {
		t.Errorf("%s = %v, want 1", constants.LLMSMPodAccessDenied, got)
	}

	forbidden = false
	if _, _, err := d.DiscoverUsageByNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := podAccessDenied(t, registry); got != 0 {
		t.Errorf("%s = %v after access was granted, want 0", constants.LLMSMPodAccessDenied, got)
	}
}

// TestForbiddenPodListWithoutPhysicalLimiterIsNotAFailure: only a physical
// limiter reads the usage walk. On a tenant install bounded by a quota the
// undercount a Forbidden list causes bounds nothing, and reporting it as a
// failure (gauge 1, error log) sends the operator to fix something that does
// not matter.
func TestForbiddenPodListWithoutPhysicalLimiterIsNotAFailure(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	node := gpuNode()
	node.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}
	cached := fake.NewClientBuilder().WithScheme(s).WithObjects(node, gpuPod("mine-0", "mine", 2)).Build()
	reader := fake.NewClientBuilder().WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)
		}}).Build()

	physical := false
	d := NewK8sWithGpuOperator(cached).WithPodReader(reader).
		WithPhysicalLimiterCheck(func() bool { return physical })
	if _, _, err := d.DiscoverUsageByNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := podAccessDenied(t, registry); got != 0 {
		t.Errorf("%s = %v with no physical limiter in force, want 0", constants.LLMSMPodAccessDenied, got)
	}

	physical = true
	if _, _, err := d.DiscoverUsageByNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := podAccessDenied(t, registry); got != 1 {
		t.Errorf("%s = %v once a physical limiter is in force, want 1", constants.LLMSMPodAccessDenied, got)
	}
}

// TestResetPodAccessDeniedLogsTheNextDenialAgain: turning the physical limiter
// off stops the walk and clears the gauge. Turning it on again must report a
// still-Forbidden list as a failure again, gauge and error log both. The level
// last logged used to survive the reset, so the error was not logged again.
func TestResetPodAccessDeniedLogsTheNextDenialAgain(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	node := gpuNode()
	node.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}
	cached := fake.NewClientBuilder().WithScheme(s).WithObjects(node, gpuPod("mine-0", "mine", 2)).Build()
	reader := fake.NewClientBuilder().WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)
		}}).Build()
	errorLogs := 0
	ctx := log.IntoContext(context.Background(), funcr.New(func(_, args string) {
		if strings.Contains(args, "the physical limiter can admit scale-ups") {
			errorLogs++
		}
	}, funcr.Options{}))

	d := NewK8sWithGpuOperator(cached).WithPodReader(reader)
	for range 3 {
		if _, _, err := d.DiscoverUsageByNamespace(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if errorLogs != 1 {
		t.Fatalf("fixture: a denial that persists is logged once, got %d", errorLogs)
	}

	d.ResetPodAccessDenied()
	if got := podAccessDenied(t, registry); got != 0 {
		t.Errorf("%s = %v after the reset, want 0", constants.LLMSMPodAccessDenied, got)
	}

	if _, _, err := d.DiscoverUsageByNamespace(ctx); err != nil {
		t.Fatal(err)
	}
	if errorLogs != 2 {
		t.Errorf("error logs after the reset and another denied list = %d, want 2", errorLogs)
	}
	if got := podAccessDenied(t, registry); got != 1 {
		t.Errorf("%s = %v after another denied list, want 1", constants.LLMSMPodAccessDenied, got)
	}
}

// TestPodAccessDeniedReachesTheHook: the controller publishes a denied
// cluster-wide Pod list in its status ConfigMap, beside the gauge. The hook is
// told every value the gauge takes: 1 while a physical limiter reads the
// undercount, 0 once the list succeeds or the reset clears it.
func TestPodAccessDeniedReachesTheHook(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	node := gpuNode()
	node.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}
	cached := fake.NewClientBuilder().WithScheme(s).WithObjects(node).Build()
	forbidden := true
	reader := fake.NewClientBuilder().WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if forbidden {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)
			}
			return c.List(ctx, list, opts...)
		}}).Build()
	var told []bool
	d := NewK8sWithGpuOperator(cached).WithPodReader(reader).
		WithPodAccessDeniedHook(func(denied bool) { told = append(told, denied) })

	if _, _, err := d.DiscoverUsageByNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	forbidden = false
	if _, _, err := d.DiscoverUsageByNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.ResetPodAccessDenied()
	if want := []bool{true, false, false}; !slices.Equal(told, want) {
		t.Errorf("hook told %v, want %v (denied, listed, reset)", told, want)
	}
}

// TestPodListIsServedFromTheWatchCache: the uncached list runs every refresh
// and lists every Pod in the cluster. It must ask for resourceVersion "0", so
// the API server answers from its watch cache instead of etcd, and it must not
// page: a continue token that expires between pages (410 Gone) has no fallback.
func TestPodListIsServedFromTheWatchCache(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	node := gpuNode()
	node.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}
	cached := fake.NewClientBuilder().WithScheme(s).WithObjects(node).Build()
	var sent []metav1.ListOptions
	reader := fake.NewClientBuilder().WithScheme(s).WithObjects(gpuPod("theirs-0", "other-tenant", 4)).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			sent = append(sent, *(&client.ListOptions{}).ApplyOptions(opts).AsListOptions())
			return c.List(ctx, list, opts...)
		}}).Build()

	byType, _, err := NewK8sWithGpuOperator(cached).WithPodReader(reader).DiscoverUsageByNamespace(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := byType[testGPUProduct]; got != 4 {
		t.Errorf("usage = %d, want the 4 GPUs the reader's Pod holds", got)
	}
	if len(sent) != 1 {
		t.Fatalf("pod lists sent = %d, want exactly 1", len(sent))
	}
	if got := sent[0]; got.ResourceVersion != "0" || got.Limit != 0 || got.Continue != "" {
		t.Errorf("list options = resourceVersion %q, limit %d, continue %q; want \"0\", 0, \"\"",
			got.ResourceVersion, got.Limit, got.Continue)
	}
}

// podAccessDenied reads llmsm_pod_access_denied from registry, or -1 when absent.
func podAccessDenied(t *testing.T, registry *prometheus.Registry) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == constants.LLMSMPodAccessDenied {
			v := 0.0
			for _, m := range f.GetMetric() {
				v += m.GetGauge().GetValue()
			}
			return v
		}
	}
	return -1
}
