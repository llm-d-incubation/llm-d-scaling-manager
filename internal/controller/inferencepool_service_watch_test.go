package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/datastore"
)

// gaieCRD returns the path of the v1 InferencePool CRD in the module cache.
func gaieCRD(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/gateway-api-inference-extension").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("locating the gateway-api-inference-extension module: %v", err)
	}
	path := filepath.Join(strings.TrimSpace(string(out)), "config", "crd", "bases",
		"inference.networking.k8s.io_inferencepools.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("InferencePool CRD: %v", err)
	}
	return path
}

// TestEndpointPickerPortChangeReachesTheReconciler: the EPP metrics port is read
// from the endpoint picker's Service, which bumps no pool generation. With the
// pool's GenerationChangedPredicate and no Service watch, a port change would
// never reach the reconciler, and the pool kept scraping the old port.
func TestEndpointPickerPortChangeReachesTheReconciler(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set: this test needs envtest (make setup-envtest)")
	}
	env := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{Paths: []string{gaieCRD(t)}}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = env.Stop() }()

	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1.Install(s); err != nil {
		t.Fatal(err)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     s,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ds := datastore.NewDatastore(nil)
	r := &InferencePoolReconciler{Client: mgr.GetClient(), Datastore: ds,
		PoolGKNN: common.GKNN{GroupKind: v1.SchemeGroupVersion.WithKind(inferencePoolKind).GroupKind()}}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	go func() { _ = mgr.Start(ctx) }()

	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	const ns = "epp-port"
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "epp", Namespace: ns},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "grpc", Port: 9002},
			{Name: "http-metrics", Port: 9090},
		}},
	}
	must(c.Create(ctx, svc))
	must(c.Create(ctx, &v1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: ns},
		Spec: v1.InferencePoolSpec{
			Selector:          v1.LabelSelector{MatchLabels: map[v1.LabelKey]v1.LabelValue{"app": "model"}},
			TargetPorts:       []v1.Port{{Number: 8000}},
			EndpointPickerRef: v1.EndpointPickerRef{Name: "epp", Port: &v1.Port{Number: 9002}},
		},
	}))

	waitPort := func(want int32) bool {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if p, err := ds.PoolGet(ns + "/pool"); err == nil && p.EndpointPicker.MetricsPortNumber == want {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}
	if !waitPort(9090) {
		t.Fatal("the pool never reached the datastore with metrics port 9090")
	}

	must(c.Get(ctx, client.ObjectKeyFromObject(svc), svc))
	svc.Spec.Ports[1].Port = 9091
	must(c.Update(ctx, svc))
	if !waitPort(9091) {
		t.Error("a changed EPP metrics port never reached the reconciler: the pool still scrapes 9090")
	}
}
