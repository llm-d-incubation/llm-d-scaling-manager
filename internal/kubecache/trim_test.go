package kubecache

import (
	"context"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestStripUnused(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:          "p",
		Annotations:   map[string]string{LastAppliedAnnotation: "{...}", "keep": "me"},
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
	}}
	if _, err := StripUnused(pod); err != nil {
		t.Fatal(err)
	}
	if pod.ManagedFields != nil || pod.Annotations[LastAppliedAnnotation] != "" || pod.Annotations["keep"] != "me" {
		t.Errorf("after StripUnused: %+v", pod.ObjectMeta)
	}
	// A tombstone, or anything else without object metadata, passes through.
	if out, err := StripUnused("not an object"); err != nil || out != "not an object" {
		t.Errorf("non-object: %v %v", out, err)
	}
}

func TestStripNodeKeepsWhatReadersUse(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: map[string]string{"nvidia.com/gpu.product": "H100"}},
		Spec:       corev1.NodeSpec{Unschedulable: true, Taints: []corev1.Taint{{Key: "k", Effect: corev1.TaintEffectNoSchedule}}},
		Status: corev1.NodeStatus{
			Images:      []corev1.ContainerImage{{Names: []string{"img"}, SizeBytes: 1}},
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	if _, err := StripNode(node); err != nil {
		t.Fatal(err)
	}
	if node.Status.Images != nil {
		t.Error("images kept")
	}
	if node.Labels["nvidia.com/gpu.product"] != "H100" || !node.Spec.Unschedulable || len(node.Spec.Taints) != 1 ||
		len(node.Status.Conditions) != 1 || node.Status.Allocatable.Name("nvidia.com/gpu", resource.DecimalSI).Value() != 8 {
		t.Errorf("StripNode removed something a reader uses: %+v", node)
	}
}

func TestTrimKeepsAnExistingNodeEntry(t *testing.T) {
	sel := cache.ByObject{Label: nil, Transform: nil, UnsafeDisableDeepCopy: new(bool)}
	opts := cache.Options{ByObject: map[client.Object]cache.ByObject{&corev1.Node{}: sel}}
	Trim(&opts)
	if len(opts.ByObject) != 1 {
		t.Fatalf("Trim added a second Node entry: %d entries", len(opts.ByObject))
	}
	for _, by := range opts.ByObject {
		if by.Transform == nil || by.UnsafeDisableDeepCopy == nil {
			t.Errorf("existing Node entry lost settings or got no transform: %+v", by)
		}
	}
	if opts.DefaultTransform == nil {
		t.Error("no default transform")
	}
}

// TestCachedObjectsAreTrimmed runs a real informer cache against an API server
// in the namespace-scoped shape (DefaultNamespaces set) and reads a Node and a
// Pod back through it, with and without Trim.
func TestCachedObjectsAreTrimmed(t *testing.T) {
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

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	const ns = "trim-ns"
	must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "gpu-node",
		Labels:      map[string]string{"nvidia.com/gpu.product": "H100"},
		Annotations: map[string]string{LastAppliedAnnotation: "{}"},
	}}
	must(t, c.Create(ctx, node))
	node.Status.Images = []corev1.ContainerImage{{Names: []string{"registry.k8s.io/pause:3.9"}, SizeBytes: 1}}
	must(t, c.Status().Update(ctx, node))
	must(t, c.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns, Annotations: map[string]string{LastAppliedAnnotation: "{}"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.k8s.io/pause:3.9"}}},
	}))

	read := func(trim bool) (*corev1.Node, *corev1.Pod) {
		opts := cache.Options{Scheme: scheme.Scheme, DefaultNamespaces: map[string]cache.Config{ns: {}}}
		if trim {
			Trim(&opts)
		}
		informers, err := cache.New(cfg, opts)
		if err != nil {
			t.Fatalf("cache.New (trim=%v): %v", trim, err)
		}
		cctx, stop := context.WithCancel(ctx)
		defer stop()
		go func() { _ = informers.Start(cctx) }()
		if !informers.WaitForCacheSync(cctx) {
			t.Fatal("cache did not start")
		}
		var n corev1.Node
		var p corev1.Pod
		must(t, informers.Get(ctx, client.ObjectKey{Name: "gpu-node"}, &n))
		must(t, informers.Get(ctx, client.ObjectKey{Namespace: ns, Name: "p"}, &p))
		return &n, &p
	}

	// The fixture must carry what Trim removes, or the trimmed read proves nothing.
	n, p := read(false)
	if len(n.Status.Images) == 0 || len(n.ManagedFields) == 0 || p.Annotations[LastAppliedAnnotation] == "" {
		t.Fatalf("untrimmed fixture lacks the fields under test: images=%d managedFields=%d podAnnotations=%v",
			len(n.Status.Images), len(n.ManagedFields), p.Annotations)
	}

	n, p = read(true)
	if len(n.Status.Images) != 0 || n.ManagedFields != nil || n.Annotations[LastAppliedAnnotation] != "" {
		t.Errorf("cached Node not trimmed: images=%d managedFields=%d annotations=%v",
			len(n.Status.Images), len(n.ManagedFields), n.Annotations)
	}
	if n.Labels["nvidia.com/gpu.product"] != "H100" {
		t.Errorf("cached Node lost its labels: %v", n.Labels)
	}
	if p.ManagedFields != nil || p.Annotations[LastAppliedAnnotation] != "" {
		t.Errorf("cached Pod not trimmed: managedFields=%d annotations=%v", len(p.ManagedFields), p.Annotations)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
