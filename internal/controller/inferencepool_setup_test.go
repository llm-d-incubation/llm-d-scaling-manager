package controller

import (
	"errors"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
)

var _ = Describe("InferencePool controller setup", func() {
	// The suite's API server has no Gateway API Inference Extension CRDs, which
	// is exactly the cluster this guards: registering a controller for an
	// unserved kind succeeds, then the manager exits on the cache-sync timeout
	// once the replica is elected.
	for _, group := range []string{v1.GroupName, v1alpha2.GroupName} {
		It("refuses to register when the "+group+" InferencePool kind is not served", func() {
			// The types ARE in the scheme: what is missing is the CRD, not the Go type.
			s := runtime.NewScheme()
			Expect(clientgoscheme.AddToScheme(s)).To(Succeed())
			Expect(v1.Install(s)).To(Succeed())
			Expect(v1alpha2.Install(s)).To(Succeed())
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme:  s,
				Metrics: metricsserver.Options{BindAddress: "0"},
				// Both groups register a controller named "inferencepool".
				Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
			})
			Expect(err).NotTo(HaveOccurred())

			r := &InferencePoolReconciler{
				Client:   mgr.GetClient(),
				PoolGKNN: common.GKNN{GroupKind: schema.GroupKind{Group: group, Kind: inferencePoolKind}},
			}
			err = r.SetupWithManager(mgr)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, ErrInferencePoolCRDNotServed)).To(BeTrue(), "got %v", err)
		})
	}
})

func TestInferencePoolPredicateIgnoresStatusOnlyUpdates(t *testing.T) {
	p := inferencePoolPredicate()
	old := &v1.InferencePool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", Generation: 3}}

	statusOnly := old.DeepCopy()
	statusOnly.ResourceVersion = "2"
	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: statusOnly}) {
		t.Error("a status-only update (same generation) passed the predicate")
	}

	specChange := old.DeepCopy()
	specChange.Generation = 4
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: specChange}) {
		t.Error("a spec change (new generation) was filtered out")
	}
	if !p.Create(event.CreateEvent{Object: old}) {
		t.Error("a create was filtered out")
	}
	if !p.Delete(event.DeleteEvent{Object: old}) {
		t.Error("a delete was filtered out")
	}
}
