package scaler_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
)

// testNamespace is the single namespace these specs operate in. The handler
// resolves a ScaledObject and its decision by (namespace, name), so the refs,
// the seeded ScaledObjects and the decision-store entries must all agree on it —
// naming it keeps that pairing explicit rather than repeating a bare literal.
const testNamespace = "chat"

func ref(name string, metadata map[string]string) *pb.ScaledObjectRef {
	return &pb.ScaledObjectRef{Namespace: testNamespace, Name: name, ScalerMetadata: metadata}
}

func ptr(v int32) *int32 { return &v }

// deployment is a scale target with the given spec.replicas; nil leaves the
// field absent.
func deployment(name string, replicas *int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name},
		Spec:       appsv1.DeploymentSpec{Replicas: replicas},
	}
}

// undecidedCount reads llmsm_scaler_undecided_answers_total for testNamespace
// and reason from reg; 0 when the series does not exist.
func undecidedCount(reg *prometheus.Registry, reason string) float64 {
	mfs, err := reg.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, mf := range mfs {
		if mf.GetName() != constants.LLMSMScalerUndecidedAnswersTotal {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels[constants.LabelNamespace] == testNamespace && labels[constants.LabelReason] == reason {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

var _ = Describe("External scaler handler", func() {
	var (
		ctx   context.Context
		store *decision.Store
	)

	// newHandler builds a Handler backed by a fake client seeded with objs and
	// the fresh per-spec decision store.
	newHandler := func(objs ...client.Object) *scaler.Handler {
		s := runtime.NewScheme()
		Expect(kedav1alpha1.AddToScheme(s)).To(Succeed())
		// apps/v1 so the handler can read a Deployment scale target when it
		// answers with the current replica count.
		Expect(appsv1.AddToScheme(s)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
		return scaler.NewHandler(c, store, registry.New(0))
	}

	scaledObject := func(namespace, name, target string) *kedav1alpha1.ScaledObject {
		return &kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: target},
			},
		}
	}

	BeforeEach(func() {
		ctx = context.Background()
		store = decision.NewStore()
	})

	Describe("GetMetricSpec", func() {
		It("advertises the scaling manager metric with a target of 1", func() {
			h := newHandler()
			resp, err := h.GetMetricSpec(ctx, ref("chat-decode", nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.MetricSpecs).To(HaveLen(1))
			Expect(resp.MetricSpecs[0].MetricName).To(Equal(scaler.MetricName))
			Expect(resp.MetricSpecs[0].TargetSize).To(Equal(int64(1)))
		})
	})

	Describe("GetMetrics", func() {
		It("returns the desired replicas resolved via the ScaledObject's scaleTargetRef", func() {
			h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy"))
			store.Set(testNamespace, "chat-decode-deploy", 5)

			resp, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("chat-decode", nil),
				MetricName:      scaler.MetricName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.MetricValues).To(HaveLen(1))
			Expect(resp.MetricValues[0].MetricName).To(Equal(scaler.MetricName))
			Expect(resp.MetricValues[0].MetricValue).To(Equal(int64(5)))
		})

		// Before the first decision -- every restart, every lease hand-off --
		// the answer is the target's CURRENT count, so the HPA computes the
		// count it already has and holds. A 0 would cut every running fleet to
		// its floor; an error is a trigger failure, which on a min-0 object
		// with a zero fallback reaches KEDA's scale-to-zero path.
		DescribeTable("answers the scale target's current replica count before any decision exists",
			func(target client.Object, want int64) {
				h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy"), target)

				resp, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
					ScaledObjectRef: ref("chat-decode", nil),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.MetricValues).To(HaveLen(1))
				Expect(resp.MetricValues[0].MetricName).To(Equal(scaler.MetricName))
				Expect(resp.MetricValues[0].MetricValue).To(Equal(want))
			},
			Entry("a running target holds its count", deployment("chat-decode-deploy", ptr(4)), int64(4)),
			Entry("a parked target answers 0", deployment("chat-decode-deploy", ptr(0)), int64(0)),
			Entry("an absent spec.replicas is the API default of 1", deployment("chat-decode-deploy", nil), int64(1)),
		)

		It("errors, with the code of the failed read, only when the current count cannot be read", func() {
			// No Deployment: nothing to hold, and an invented number would be an
			// answer the HPA acts on.
			h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy"))

			resp, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("chat-decode", nil),
			})
			Expect(status.Code(err)).To(Equal(codes.NotFound), "got %v", err)
			Expect(resp).To(BeNil())
		})

		It("counts undecided answers by why there is no decision", func() {
			reg := prometheus.NewRegistry()
			Expect(metrics.InitMetrics(reg)).To(Succeed())
			h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy"),
				deployment("chat-decode-deploy", ptr(2)))
			ask := func(md map[string]string) {
				_, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{ScaledObjectRef: ref("chat-decode", md)})
				Expect(err).NotTo(HaveOccurred())
			}

			ask(map[string]string{registry.ModelIDKey: "default/default"})
			ask(map[string]string{registry.ModelIDKey: "default/default"})
			// No modelID: ParseMeta rejects it, every engine skips the workload,
			// and it will never be decided.
			ask(nil)

			Expect(undecidedCount(reg, constants.ScalerUndecidedNoDecision)).To(Equal(2.0))
			Expect(undecidedCount(reg, constants.ScalerUndecidedNotManaged)).To(Equal(1.0))

			By("a decided answer is not counted")
			store.Set(testNamespace, "chat-decode-deploy", 5)
			ask(map[string]string{registry.ModelIDKey: "default/default"})
			Expect(undecidedCount(reg, constants.ScalerUndecidedNoDecision)).To(Equal(2.0))
		})

		It("ignores a scale target named in trigger metadata; the ScaledObject decides", func() {
			// A second, hand-written copy of scaleTargetRef.name can disagree with
			// the spec that actually decides what KEDA scales, and then a variant's
			// metrics are attributed to another workload. Cloning a ScaledObject to
			// add a variant used to carry such a key across and point the new entry
			// at the original's Deployment.
			h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy"))
			store.Set(testNamespace, "chat-decode-deploy", 7)
			store.Set(testNamespace, "impostor", 99)

			resp, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("chat-decode", map[string]string{"variantName": "impostor"}),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.MetricValues[0].MetricValue).To(Equal(int64(7)),
				"the metric must come from the ScaledObject's own target")
		})

		It("errors when the ScaledObject is missing", func() {
			h := newHandler()

			_, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("missing", nil),
			})
			Expect(status.Code(err)).To(Equal(codes.NotFound), "got %v", err)
		})

		// Forbidden is not absent, and a timeout is not either: reporting both as
		// NotFound tells the operator reading KEDA's trigger error to look for a
		// missing object when the fix is RBAC, or nothing at all.
		DescribeTable("maps a failed ScaledObject read to the code that names its cause",
			func(readErr error, want codes.Code) {
				s := runtime.NewScheme()
				Expect(kedav1alpha1.AddToScheme(s)).To(Succeed())
				c := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						return readErr
					},
				}).Build()
				h := scaler.NewHandler(c, store, registry.New(0))

				_, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{ScaledObjectRef: ref("chat-decode", nil)})
				Expect(status.Code(err)).To(Equal(want), "got %v", err)
			},
			Entry("NotFound -> NotFound",
				apierrors.NewNotFound(schema.GroupResource{Group: "keda.sh", Resource: "scaledobjects"}, "chat-decode"),
				codes.NotFound),
			Entry("Forbidden -> PermissionDenied",
				apierrors.NewForbidden(schema.GroupResource{Group: "keda.sh", Resource: "scaledobjects"}, "chat-decode", errors.New("rbac")),
				codes.PermissionDenied),
			Entry("Unauthorized -> PermissionDenied",
				apierrors.NewUnauthorized("token expired"),
				codes.PermissionDenied),
			Entry("timeout -> Unavailable",
				apierrors.NewTimeoutError("slow apiserver", 1),
				codes.Unavailable),
			Entry("anything else -> Unavailable",
				errors.New("connection refused"),
				codes.Unavailable),
		)
	})

	Describe("IsActive", func() {
		DescribeTable("gates on the scaling manager's decision",
			func(seed *int32, wantActive bool) {
				h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy"))
				if seed != nil {
					store.Set(testNamespace, "chat-decode-deploy", *seed)
				}
				resp, err := h.IsActive(ctx, ref("chat-decode", nil))
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Result).To(Equal(wantActive))
			},
			Entry("desired > 0 -> active", ptr(3), true),
			Entry("desired == 0 -> inactive", ptr(0), false),
			Entry("no decision yet -> active", nil, true),
		)
	})
})
