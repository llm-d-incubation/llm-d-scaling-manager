package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
	utiltest "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/datastore"
	unittestutil "github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// A pool in a namespace the live watchNamespaces leaves out is dropped from
// the datastore, so its EPP is not scraped; once the namespace is listed, the
// same pool read again is added. RequeueAll queues every pool for that read.
func TestInferencePoolReconcileFollowsManagedNamespaces(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: v1.GroupVersion.Group, Version: v1.GroupVersion.Version, Kind: "InferencePool"}
	pool := utiltest.MakeInferencePool("pool-b").Namespace("team-b").Selector(map[string]string{"app": "b"}).
		TargetPorts(8080).EndpointPickerRef("epp-svc").ObjRef()
	pool.SetGroupVersionKind(gvk)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.Install(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects([]client.Object{pool, unittestutil.MakeService("epp-svc", "team-b")}...).Build()

	listed := map[string]bool{"team-a": true}
	ds := datastore.NewDatastore(nil)
	nn := types.NamespacedName{Namespace: "team-b", Name: "pool-b"}
	r := &InferencePoolReconciler{Client: c, Datastore: ds,
		PoolGKNN: common.GKNN{NamespacedName: nn, GroupKind: schema.GroupKind{Group: gvk.Group, Kind: gvk.Kind}},
		Managed:  func(ns string) bool { return listed[ns] }}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: nn}); err != nil {
		t.Fatal(err)
	}
	if n := len(ds.PoolList()); n != 0 {
		t.Fatalf("a pool in a namespace watchNamespaces leaves out is in the datastore (%d pools)", n)
	}

	listed["team-b"] = true
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: nn}); err != nil {
		t.Fatal(err)
	}
	if n := len(ds.PoolList()); n != 1 {
		t.Fatalf("once team-b is listed the pool must be in the datastore; %d pools", n)
	}

	listed["team-b"] = false
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: nn}); err != nil {
		t.Fatal(err)
	}
	if n := len(ds.PoolList()); n != 0 {
		t.Fatalf("a pool whose namespace left watchNamespaces stayed in the datastore (%d pools)", n)
	}

	r.requeue = make(chan event.GenericEvent, 4)
	r.RequeueAll(ctx)
	select {
	case e := <-r.requeue:
		if e.Object.GetNamespace() != "team-b" || e.Object.GetName() != "pool-b" {
			t.Fatalf("requeued %s/%s", e.Object.GetNamespace(), e.Object.GetName())
		}
	case <-time.After(time.Second):
		t.Fatal("RequeueAll queued no pool")
	}
}
