package scaler_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
)

// fakeStream is the server side of StreamIsActive: it exposes a channel so a
// spec can await each push instead of sleeping.
type fakeStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent chan bool
}

func newFakeStream(ctx context.Context) *fakeStream {
	// Buffered generously: a spec asserting "nothing further was pushed" must
	// not be the reason a later send blocks.
	return &fakeStream{ctx: ctx, sent: make(chan bool, 16)}
}

func (f *fakeStream) Context() context.Context { return f.ctx }

func (f *fakeStream) Send(resp *pb.IsActiveResponse) error {
	f.sent <- resp.Result
	return nil
}

// nextPush returns the next pushed activation state, failing the spec if the
// handler stays silent.
func (f *fakeStream) nextPush() bool {
	GinkgoHelper()
	select {
	case v := <-f.sent:
		return v
	case <-time.After(3 * time.Second):
		Fail("timed out waiting for the handler to push an activation state")
		return false
	}
}

// expectNoPush asserts the handler pushed nothing within a short window.
func (f *fakeStream) expectNoPush() {
	GinkgoHelper()
	select {
	case v := <-f.sent:
		Fail("unexpected activation push: " + map[bool]string{true: "active", false: "inactive"}[v])
	case <-time.After(100 * time.Millisecond):
	}
}

// scaledObjectFor builds a ScaledObject in testNamespace whose scaleTargetRef
// names target, so the handler resolves (namespace, name) -> target the way it
// does in a cluster.
func scaledObjectFor(name, target string) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: target},
		},
	}
}

// deploymentWithReplicas is the scale target backing a ScaledObject, used by the
// specs that exercise the pre-first-decision fallback.
func deploymentWithReplicas(name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

var _ = Describe("External scaler StreamIsActive", func() {
	var store *decision.Store

	newHandler := func(objs ...client.Object) *scaler.Handler {
		s := runtime.NewScheme()
		Expect(kedav1alpha1.AddToScheme(s)).To(Succeed())
		// apps/v1 so the handler can read a Deployment scale target when it
		// falls back to the current replica count.
		Expect(appsv1.AddToScheme(s)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
		return scaler.NewHandler(c, store, registry.New(0))
	}

	BeforeEach(func() {
		store = decision.NewStore()
	})

	// runStream starts StreamIsActive in the background and returns the stream
	// plus a stop func that closes it and waits for the handler to return.
	runStream := func(h *scaler.Handler, r *pb.ScaledObjectRef) (*fakeStream, func() error) {
		ctx, cancel := context.WithCancel(context.Background())
		stream := newFakeStream(ctx)
		errCh := make(chan error, 1)
		go func() { errCh <- h.StreamIsActive(r, stream) }()
		return stream, func() error {
			cancel()
			select {
			case err := <-errCh:
				return err
			case <-time.After(3 * time.Second):
				Fail("StreamIsActive did not return after the stream was closed")
				return nil
			}
		}
	}

	It("pushes the opening state immediately, before any decision changes", func() {
		// KEDA must never be left holding a stream that has said nothing — with
		// no decision yet the target counts as active, same as the poll path.
		h := newHandler(scaledObjectFor("chat-decode", "chat-decode-deploy"))
		stream, stop := runStream(h, ref("chat-decode", nil))
		defer func() { Expect(stop()).To(Succeed()) }()

		Expect(stream.nextPush()).To(BeTrue())
	})

	It("pushes activation when a decision wakes a target parked at zero", func() {
		// The scale-from-zero path: the target sits at desired 0, the engine
		// spots pending requests and writes 1, and KEDA learns immediately.
		h := newHandler(scaledObjectFor("chat-decode", "chat-decode-deploy"))
		store.Set(testNamespace, "chat-decode-deploy", 0)

		stream, stop := runStream(h, ref("chat-decode", nil))
		defer func() { Expect(stop()).To(Succeed()) }()
		Expect(stream.nextPush()).To(BeFalse(), "opens inactive at desired 0")

		store.Set(testNamespace, "chat-decode-deploy", 1)
		Expect(stream.nextPush()).To(BeTrue(), "activation pushed without waiting for a poll")
	})

	It("pushes deactivation when the decision drops to zero", func() {
		h := newHandler(scaledObjectFor("chat-decode", "chat-decode-deploy"))
		store.Set(testNamespace, "chat-decode-deploy", 2)

		stream, stop := runStream(h, ref("chat-decode", nil))
		defer func() { Expect(stop()).To(Succeed()) }()
		Expect(stream.nextPush()).To(BeTrue())

		store.Set(testNamespace, "chat-decode-deploy", 0)
		Expect(stream.nextPush()).To(BeFalse())
	})

	It("stays silent while the decision changes without crossing zero", func() {
		// Replica-count changes are the metric's job (GetMetrics); the stream
		// exists only to report activation, so re-pushing "still active" would
		// be noise KEDA has to process on every optimize cycle.
		h := newHandler(scaledObjectFor("chat-decode", "chat-decode-deploy"))
		store.Set(testNamespace, "chat-decode-deploy", 2)

		stream, stop := runStream(h, ref("chat-decode", nil))
		defer func() { Expect(stop()).To(Succeed()) }()
		Expect(stream.nextPush()).To(BeTrue())

		store.Set(testNamespace, "chat-decode-deploy", 5)
		store.Set(testNamespace, "chat-decode-deploy", 3)
		stream.expectNoPush()
	})

	It("ignores decisions for other targets", func() {
		h := newHandler(
			scaledObjectFor("chat-decode", "chat-decode-deploy"),
			scaledObjectFor("chat-prefill", "chat-prefill-deploy"),
		)
		store.Set(testNamespace, "chat-decode-deploy", 0)

		stream, stop := runStream(h, ref("chat-decode", nil))
		defer func() { Expect(stop()).To(Succeed()) }()
		Expect(stream.nextPush()).To(BeFalse())

		store.Set(testNamespace, "chat-prefill-deploy", 4)
		stream.expectNoPush()
	})

	It("resolves the target from the ScaledObject, never from scalerMetadata", func() {
		// The stream path resolves the target the same way the poll path does: by
		// reading the object. Metadata naming a different target is ignored, so a
		// stale key on a cloned trigger cannot redirect the stream.
		h := newHandler(scaledObjectFor("chat-decode", "chat-decode-deploy"))
		store.Set(testNamespace, "chat-decode-deploy", 0)

		stream, stop := runStream(h, ref("chat-decode", map[string]string{"variantName": "impostor"}))
		defer func() { Expect(stop()).To(Succeed()) }()
		Expect(stream.nextPush()).To(BeFalse())

		store.Set(testNamespace, "chat-decode-deploy", 1)
		Expect(stream.nextPush()).To(BeTrue())
	})

	It("returns an error when the scale target cannot be resolved", func() {
		// A misconfigured ScaledObject must surface to KEDA rather than leaving
		// a stream open that can never activate.
		h := newHandler()
		stream := newFakeStream(context.Background())
		Expect(h.StreamIsActive(ref("missing", nil), stream)).To(HaveOccurred())
	})

	It("returns nil when KEDA closes the stream", func() {
		h := newHandler(scaledObjectFor("chat-decode", "chat-decode-deploy"))
		stream, stop := runStream(h, ref("chat-decode", nil))
		Expect(stream.nextPush()).To(BeTrue())

		Expect(stop()).To(Succeed(), "a closed stream is normal shutdown, not an error")
	})

	It("stops waking a closed stream", func() {
		// Guards the unsubscribe: a leaked registration would keep the store
		// signalling a stream nobody is serving.
		h := newHandler(scaledObjectFor("chat-decode", "chat-decode-deploy"))
		store.Set(testNamespace, "chat-decode-deploy", 0)

		stream, stop := runStream(h, ref("chat-decode", nil))
		Expect(stream.nextPush()).To(BeFalse())
		Expect(stop()).To(Succeed())

		store.Set(testNamespace, "chat-decode-deploy", 1)
		stream.expectNoPush()
	})
	It("keeps a target parked at zero asleep until a decision wakes it", func() {
		// Regression guard for the defect that made the scale-from-zero e2e pass
		// trivially: with no decision yet, reporting "active" woke every workload
		// sitting at zero the moment KEDA first asked, so the scale-from-zero
		// engine never saw it as inactive and the EPP signal was never exercised.
		h := newHandler(
			scaledObjectFor("chat-decode", "chat-decode-deploy"),
			deploymentWithReplicas("chat-decode-deploy", 0),
		)

		stream, stop := runStream(h, ref("chat-decode", nil))
		defer func() { Expect(stop()).To(Succeed()) }()
		Expect(stream.nextPush()).To(BeFalse(), "no decision + zero replicas must stay inactive")

		store.Set(testNamespace, "chat-decode-deploy", 1)
		Expect(stream.nextPush()).To(BeTrue(), "the decision is what wakes it")
	})

	It("keeps a running target active before the first decision", func() {
		// The other direction: a workload already serving must not be scaled to
		// zero just because the scaling manager has not looked at it yet.
		h := newHandler(
			scaledObjectFor("chat-decode", "chat-decode-deploy"),
			deploymentWithReplicas("chat-decode-deploy", 2),
		)
		stream, stop := runStream(h, ref("chat-decode", nil))
		defer func() { Expect(stop()).To(Succeed()) }()
		Expect(stream.nextPush()).To(BeTrue())
	})
})
