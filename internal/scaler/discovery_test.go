package scaler_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
)

// Being called is what being managed means: The scaling manager no longer lists the cluster
// looking for annotated objects, so if an RPC does not register its ref, the
// workload it names is invisible to every engine and simply never scales. These
// specs hold each entry point to that contract.
//
// See docs/plans/engine/keda-driven-discovery.md.
var _ = Describe("Call-driven discovery", func() {
	var (
		ctx   context.Context
		store *decision.Store
		reg   *registry.Registry
	)

	newHandler := func(objs ...client.Object) *scaler.Handler {
		s := runtime.NewScheme()
		Expect(kedav1alpha1.AddToScheme(s)).To(Succeed())
		Expect(appsv1.AddToScheme(s)).To(Succeed())
		// The scale target is resolved by READING the ScaledObject; it is not in
		// trigger metadata, precisely so a hand-written copy cannot disagree with
		// the spec that decides what KEDA scales. So the object has to exist.
		objs = append(objs, &kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{Name: "chat-decode", Namespace: testNamespace},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: "chat-decode-deploy"},
			},
		})
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
		return scaler.NewHandler(c, store, reg)
	}

	triggerMetadata := map[string]string{
		registry.ModelIDKey:     "default/default",
		registry.VariantCostKey: "12.5",
	}

	BeforeEach(func() {
		ctx = context.Background()
		store = decision.NewStore()
		reg = registry.New(time.Minute)
	})

	It("registers the workload from GetMetricSpec", func() {
		// The earliest notice the scaling manager gets: KEDA asks for the spec when it starts
		// managing a ScaledObject, before it has any reason to ask anything else.
		h := newHandler()
		_, err := h.GetMetricSpec(ctx, ref("chat-decode", triggerMetadata))
		Expect(err).NotTo(HaveOccurred())

		e, ok := reg.Get(testNamespace, "chat-decode")
		Expect(ok).To(BeTrue(), "the spec request must register the workload it names")
		Expect(e.Metadata).To(HaveKeyWithValue(registry.ModelIDKey, "default/default"))
		Expect(e.Metadata).To(HaveKeyWithValue(registry.VariantCostKey, "12.5"))
	})

	It("registers the workload from IsActive", func() {
		h := newHandler()
		_, err := h.IsActive(ctx, ref("chat-decode", triggerMetadata))
		Expect(err).NotTo(HaveOccurred())

		_, ok := reg.Get(testNamespace, "chat-decode")
		Expect(ok).To(BeTrue(), "the poll path must keep a parked workload registered")
	})

	It("registers the workload from GetMetrics", func() {
		h := newHandler(deployment("chat-decode-deploy", ptr(2)))
		_, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
			ScaledObjectRef: ref("chat-decode", triggerMetadata),
			MetricName:      scaler.MetricName,
		})
		// No decision exists yet, so the answer is the current count -- and the
		// call registers, which is what lets the engines ever produce one.
		Expect(err).NotTo(HaveOccurred())

		_, ok := reg.Get(testNamespace, "chat-decode")
		Expect(ok).To(BeTrue(), "the metrics path must keep a running workload registered")
	})

	It("holds the workload registered for the life of a StreamIsActive", func() {
		// The case a timestamp TTL gets wrong. On a push trigger a workload parked
		// at zero is called about exactly once — this stream — and then nothing
		// else asks about it at all. Expiring it would evict precisely the entries
		// whose purpose is to be woken from zero.
		h := newHandler()
		streamCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		stream := &recordingStream{ctx: streamCtx}
		done := make(chan error, 1)
		go func() { done <- h.StreamIsActive(ref("chat-decode", triggerMetadata), stream) }()

		Eventually(func() bool {
			e, ok := reg.Get(testNamespace, "chat-decode")
			return ok && e.Streaming
		}).Should(BeTrue(), "an open stream must register the workload and hold it live")

		cancel()
		Eventually(done).Should(Receive(BeNil()))

		// A closed stream hands the entry to the TTL rather than deleting it:
		// KEDA closes and re-opens streams across its own reconciles.
		Eventually(func() bool {
			e, ok := reg.Get(testNamespace, "chat-decode")
			return ok && !e.Streaming
		}).Should(BeTrue(), "closing the stream must release the hold, not the entry")
	})

	It("registers a workload whose trigger metadata does not parse", func() {
		// Registration is deliberately unconditional. A trigger missing modelID is
		// a misconfiguration the operator has to see, and an entry that exists but
		// reports a bad trigger — with its name attached, once per cycle — is a far
		// better diagnostic than a workload that silently never appears anywhere.
		h := newHandler()
		_, err := h.IsActive(ctx, ref("chat-decode", map[string]string{
			registry.VariantCostKey: "12.5", // no modelID: this must not parse
		}))
		Expect(err).NotTo(HaveOccurred())

		e, ok := reg.Get(testNamespace, "chat-decode")
		Expect(ok).To(BeTrue(), "a bad trigger must still be visible, not invisible")

		_, parseErr := registry.ParseMeta(e.Metadata)
		Expect(parseErr).To(HaveOccurred(), "and the metadata must still be reported as bad")
	})

	It("does not register anything for a nil ref", func() {
		h := newHandler()
		_, err := h.GetMetricSpec(ctx, nil)
		Expect(err).NotTo(HaveOccurred(), "the spec does not depend on the ref")
		Expect(reg.Len()).To(BeZero(), "a nil ref names no workload")
	})
})

// recordingStream is a minimal ExternalScaler_StreamIsActiveServer whose only job
// is to stay open until its context is cancelled.
type recordingStream struct {
	pb.ExternalScaler_StreamIsActiveServer
	ctx context.Context
}

func (s *recordingStream) Context() context.Context        { return s.ctx }
func (s *recordingStream) Send(*pb.IsActiveResponse) error { return nil }
