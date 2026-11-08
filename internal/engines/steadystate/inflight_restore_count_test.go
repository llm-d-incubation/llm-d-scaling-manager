package steadystate

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/controllerstatus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// inFlightClient is a fake client holding the controller-status ConfigMap
// with the in-flight ledger raw, and objs.
func inFlightClient(t *testing.T, raw string, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs = append(objs, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: controllerstatus.ConfigMapName, Namespace: config.SystemNamespace()},
		Data: map[string]string{controllerstatus.KeyInFlightLedger: raw}})
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// scheduledPod is pod i of Deployment dep, labelled app=m and bound to a node.
func scheduledPod(dep string, i int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-abc-%d", dep, i), Namespace: "ns",
			Labels: map[string]string{"app": "m", appsv1.DefaultDeploymentUniqueLabelKey: "abc"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: dep + "-abc",
				UID: "rs", Controller: ptr.To(true)}}},
		Spec:   corev1.PodSpec{NodeName: fmt.Sprintf("node-%d", i)},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// Issue ev-shindin/llm-scaling-manager#215: a restored hold counted its variant's pods only from the first
// steady-state cycle that admitted it, so a replica whose pod landed after
// the restart kept its hole held -- from wakes too -- until then. The
// persisted ledger names the scale target, and a restored hold counts its
// pods from the moment it is read back.
//
// DefaultInFlight is process-wide: no t.Parallel.
func TestARestoredHoldCountsItsPodsFromTheRestore(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	ctx := context.Background()
	now := time.Now()
	raw := fmt.Sprintf(`{"version":1,"entries":{"ns/m-v":{"accelerator":"A100","gpus":8,"want":3,"placed":2,`+
		`"expires":%q,"ttlSeconds":300,"targetKind":"Deployment","targetName":"m-dep"}}}`,
		now.Add(4*time.Minute).UTC().Format(time.RFC3339Nano))
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "m-dep", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(3)), Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "m"}}}}}
	c := inFlightClient(t, raw, dep, scheduledPod("m-dep", 0), scheduledPod("m-dep", 1))
	e := &Engine{Config: config.NewTestConfig(), client: c}
	e.restoreInFlight(ctx, now)

	store := &decision.NodeGPUStore{}
	physical := []*allocation.ResourceConstraints{{ProviderName: allocation.PhysicalLimiterName,
		Pools: map[string]allocation.ResourcePool{"A100": {Limit: 32, Used: 24}}}}
	view := func(at time.Time) {
		store.Publish(a100Nodes(0, 0, 0, 8), at)
		if _, reason := allocation.NodeViewFor(physical, store, at); reason != "" {
			t.Fatalf("setup: no node view: %s", reason)
		}
	}
	at := now.Add(time.Second)
	view(at)
	if !allocation.DefaultInFlight.Holds("ns/m-v", at) {
		t.Fatal("control: the restored hold of a replica whose pod has not landed was ended")
	}

	// The third pod lands before any cycle admits the variant.
	if err := c.Create(ctx, scheduledPod("m-dep", 2)); err != nil {
		t.Fatal(err)
	}
	at = now.Add(2 * time.Second)
	view(at)
	if allocation.DefaultInFlight.Holds("ns/m-v", at) {
		t.Fatal("a restored hold whose pods all landed still holds its hole: it counts no pods until a cycle admits it")
	}
}
