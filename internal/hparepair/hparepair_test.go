package hparepair

import (
	"context"
	"strings"
	"testing"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

const (
	scalerNS = "llmsm"
	modelNS  = "models"
	ourAddr  = "scaling-manager-external-scaler.llmsm.svc.cluster.local:9090"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := kedav1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// scaledObject is a ScaledObject in modelNS whose triggers dial addrs in order.
func scaledObject(name string, uid types.UID, addrs ...string) *kedav1alpha1.ScaledObject {
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: modelNS, UID: uid},
	}
	for _, a := range addrs {
		so.Spec.Triggers = append(so.Spec.Triggers, kedav1alpha1.ScaleTriggers{
			Type: "external-push", Metadata: map[string]string{"scalerAddress": a, "modelID": "m"},
		})
	}
	return so
}

func externalMetric(name string) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ExternalMetricSourceType,
		External: &autoscalingv2.ExternalMetricSource{
			Metric: autoscalingv2.MetricIdentifier{Name: name},
			Target: autoscalingv2.MetricTarget{Type: autoscalingv2.AverageValueMetricType, AverageValue: resource.NewQuantity(1, resource.DecimalSI)},
		},
	}
}

func cpuMetric() autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name:   corev1.ResourceCPU,
			Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To[int32](80)},
		},
	}
}

// hpaFor is the HPA KEDA builds for so, created at created, with metrics.
func hpaFor(so *kedav1alpha1.ScaledObject, created time.Time, ms ...autoscalingv2.MetricSpec) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: "keda-hpa-" + so.Name, Namespace: so.Namespace, UID: types.UID("hpa-" + string(so.UID)),
			CreationTimestamp: metav1.NewTime(created),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "keda.sh/v1alpha1", Kind: "ScaledObject", Name: so.Name, UID: so.UID, Controller: ptr.To(true),
			}},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: so.Name},
			MaxReplicas:    4,
			Metrics:        ms,
		},
	}
}

type harness struct {
	t        *testing.T
	client   client.WithWatch
	rec      *record.FakeRecorder
	r        *Repairer
	now      time.Time
	patches  int
	reg      *prometheus.Registry
	listErr  error
	getErr   error
	patchErr error
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	h := &harness{t: t, now: t0.Add(10 * time.Minute)}
	h.reg = prometheus.NewRegistry()
	if err := metrics.InitMetrics(h.reg); err != nil {
		t.Fatal(err)
	}
	h.client = fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if h.listErr != nil {
					return h.listErr
				}
				return c.List(ctx, list, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*kedav1alpha1.ScaledObject); ok && h.getErr != nil {
					return h.getErr
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if h.patchErr != nil {
					return h.patchErr
				}
				h.patches++
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	h.rec = record.NewFakeRecorder(100)
	h.r = &Repairer{Reader: h.client, Writer: h.client, Recorder: h.rec, ScalerNamespace: scalerNS,
		Namespaces: []string{modelNS}, now: func() time.Time { return h.now }}
	return h
}

func (h *harness) pass() Result { return h.r.Pass(context.Background()) }

func (h *harness) annotation(name string) string {
	var hpa autoscalingv2.HorizontalPodAutoscaler
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: modelNS, Name: name}, &hpa); err != nil {
		h.t.Fatal(err)
	}
	return hpa.Annotations[Annotation]
}

func (h *harness) events() []string {
	var out []string
	for {
		select {
		case e := <-h.rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func (h *harness) gauge(name string, labels ...string) float64 {
	h.t.Helper()
	mfs, err := h.reg.Gather()
	if err != nil {
		h.t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, m := range mf.GetMetric() {
			for i := 0; i+1 < len(labels); i += 2 {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == labels[i] && lp.GetValue() == labels[i+1] {
						found = true
					}
				}
				if !found {
					continue next
				}
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue()
			}
			return m.GetGauge().GetValue()
		}
	}
	return -1 // absent
}

func TestNamesScaler(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"scaling-manager-external-scaler.llmsm:9090", true},
		{"scaling-manager-external-scaler.llmsm.svc:9090", true},
		{"scaling-manager-external-scaler.llmsm.svc.cluster.local:9090", true},
		{"Scaling-Manager-External-Scaler.LLMSM.svc", true},
		{"scaling-manager-external-scaler.llmsm", true},
		{"scaling-manager-external-scaler.other:9090", false},
		{"scaling-manager-external-scaler.llmsm-other:9090", false},
		{"scaling-manager-external-scaler.llmsm.example.com:9090", false},
		{"keda-external-scaler.llmsm:9090", false},
		{"", false},
	} {
		if got := NamesScaler(tc.addr, scalerNS); got != tc.want {
			t.Errorf("NamesScaler(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
	if NamesScaler("scaling-manager-external-scaler.:9090", "") {
		t.Error("an empty scaler namespace matched")
	}
}

func TestWired(t *testing.T) {
	plain := scaledObject("a", "u1", ourAddr)
	plain.Status.ExternalMetricNames = []string{"s0-llmsm-desired"}

	twoTriggers := scaledObject("b", "u2", "prometheus.monitoring:9090", ourAddr)
	twoTriggers.Status.ExternalMetricNames = []string{"s0-other"}

	modifiers := scaledObject("c", "u3", ourAddr)
	modifiers.Spec.Advanced = &kedav1alpha1.AdvancedConfig{ScalingModifiers: kedav1alpha1.ScalingModifiers{Formula: "s0", Target: "1"}}
	modifiers.Status.ExternalMetricNames = []string{"s0-llmsm-desired"}

	modifiersUnwired := modifiers.DeepCopy()
	modifiersUnwired.Status.ExternalMetricNames = nil

	for _, tc := range []struct {
		name string
		so   *kedav1alpha1.ScaledObject
		ms   []autoscalingv2.MetricSpec
		want bool
	}{
		{"external metric and status", plain, []autoscalingv2.MetricSpec{externalMetric("s0-llmsm-desired")}, true},
		{"cpu default", plain, []autoscalingv2.MetricSpec{cpuMetric()}, false},
		{"no metrics", plain, nil, false},
		{"another trigger wired, ours not", twoTriggers, []autoscalingv2.MetricSpec{externalMetric("s0-other")}, false},
		{"scalingModifiers: composite and status", modifiers, []autoscalingv2.MetricSpec{externalMetric(kedav1alpha1.CompositeMetricName)}, true},
		{"scalingModifiers: composite without our status", modifiersUnwired, []autoscalingv2.MetricSpec{externalMetric(kedav1alpha1.CompositeMetricName)}, false},
		{"scalingModifiers: cpu only", modifiers, []autoscalingv2.MetricSpec{cpuMetric()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := OwnTriggers(tc.so, scalerNS)
			if len(idx) == 0 {
				t.Fatal("no trigger names the scaler")
			}
			if got := Wired(tc.so, hpaFor(tc.so, t0, tc.ms...), idx); got != tc.want {
				t.Errorf("Wired = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	lo, hi := 30*time.Second, 10*time.Minute
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, w := range want {
		if got := Backoff(i+1, lo, hi); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

// An unwired HPA of this scaler's ScaledObject is annotated, counted, and
// reported by event; the next request waits for the backoff; once KEDA wires
// it the state is forgotten and that is reported too.
func TestRepairsAnUnwiredHPA(t *testing.T) {
	so := scaledObject("model-a", "u1", ourAddr)
	hpa := hpaFor(so, t0, cpuMetric())
	h := newHarness(t, so, hpa)

	res := h.pass()
	if res.Unwired[modelNS] != 1 || res.Requested[modelNS] != 1 || len(res.Unread) != 0 {
		t.Fatalf("pass 1 = %+v, want one unwired, one request, nothing unread", res)
	}
	// The patch writes one annotation and nothing else: not the spec, not the
	// labels, not another annotation.
	var after autoscalingv2.HorizontalPodAutoscaler
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(hpa), &after); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(after.Spec, hpa.Spec) || !equality.Semantic.DeepEqual(after.Labels, hpa.Labels) ||
		len(after.Annotations) != 1 || !equality.Semantic.DeepEqual(after.OwnerReferences, hpa.OwnerReferences) {
		t.Errorf("the patch changed more than the annotation: spec %+v; labels %v; annotations %v", after.Spec, after.Labels, after.Annotations)
	}
	if got := h.annotation(hpa.Name); got != h.now.UTC().Format(time.RFC3339) {
		t.Errorf("annotation %s = %q, want the request time", Annotation, got)
	}
	if ev := h.events(); len(ev) != 1 || !strings.Contains(ev[0], EventRewireRequested) || !strings.Contains(ev[0], "cpu") {
		t.Errorf("events %q, want one %s naming the cpu metric", ev, EventRewireRequested)
	}
	if v := h.gauge(constants.LLMSMKEDAUnwiredHPAs, "namespace", modelNS); v != 1 {
		t.Errorf("%s = %v, want 1", constants.LLMSMKEDAUnwiredHPAs, v)
	}
	if v := h.gauge(constants.LLMSMKEDAHPARewiresTotal, "namespace", modelNS, "outcome", constants.HPARewireRequested); v != 1 {
		t.Errorf("%s{outcome=requested} = %v, want 1", constants.LLMSMKEDAHPARewiresTotal, v)
	}
	if v := h.gauge(constants.LLMSMKEDAHPAWiringUnread); v != 0 {
		t.Errorf("%s = %v, want 0", constants.LLMSMKEDAHPAWiringUnread, v)
	}

	// Still unwired, inside the backoff: counted, not annotated again.
	h.now = h.now.Add(10 * time.Second)
	if res := h.pass(); res.Unwired[modelNS] != 1 || res.Requested[modelNS] != 0 {
		t.Fatalf("pass 2 = %+v, want unwired and no request inside the backoff", res)
	}
	// Past the first backoff: asked again.
	h.now = h.now.Add(DefaultMinBackoff)
	if res := h.pass(); res.Requested[modelNS] != 1 {
		t.Fatalf("pass 3 = %+v, want a second request after the backoff", res)
	}
	if h.patches != 2 {
		t.Fatalf("patches = %d, want 2", h.patches)
	}
	_ = h.events()

	// KEDA wires it.
	var cur autoscalingv2.HorizontalPodAutoscaler
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(hpa), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Metrics = []autoscalingv2.MetricSpec{externalMetric("s0-llmsm-desired")}
	if err := h.client.Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	var curSO kedav1alpha1.ScaledObject
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(so), &curSO); err != nil {
		t.Fatal(err)
	}
	curSO.Status.ExternalMetricNames = []string{"s0-llmsm-desired"}
	if err := h.client.Update(context.Background(), &curSO); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Hour)
	if res := h.pass(); res.Unwired[modelNS] != 0 || res.Requested[modelNS] != 0 {
		t.Fatalf("pass 4 = %+v, want nothing unwired once KEDA wired it", res)
	}
	if ev := h.events(); len(ev) != 1 || !strings.Contains(ev[0], EventRewired) {
		t.Errorf("events %q, want one %s", ev, EventRewired)
	}
	if len(h.r.tracked) != 0 {
		t.Errorf("tracked %v, want it forgotten once wired", h.r.tracked)
	}
	if v := h.gauge(constants.LLMSMKEDAUnwiredHPAs, "namespace", modelNS); v != 0 {
		t.Errorf("%s = %v, want 0", constants.LLMSMKEDAUnwiredHPAs, v)
	}
}

// Nothing that is not an HPA of this scaler's ScaledObject is touched, and
// neither is one that is wired, too young, or paused.
func TestLeavesAloneWhatIsNotOursToRepair(t *testing.T) {
	other := scaledObject("other-scaler", "u-other", "scaling-manager-external-scaler.someone-else:9090")
	young := scaledObject("young", "u-young", ourAddr)
	paused := scaledObject("paused", "u-paused", ourAddr)
	paused.Annotations = map[string]string{kedav1alpha1.PausedAnnotation: "true"}
	pausedCond := scaledObject("paused-cond", "u-paused-cond", ourAddr)
	pausedCond.Status.Conditions = kedav1alpha1.Conditions{{Type: kedav1alpha1.ConditionPaused, Status: metav1.ConditionTrue}}
	wired := scaledObject("wired", "u-wired", ourAddr)
	wired.Status.ExternalMetricNames = []string{"s0-x"}
	renamed := scaledObject("recreated", "u-new", ourAddr)

	plainHPA := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "hand-written", Namespace: modelNS, CreationTimestamp: metav1.NewTime(t0)},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "x"},
			MaxReplicas:    3, Metrics: []autoscalingv2.MetricSpec{cpuMetric()},
		},
	}
	// A ScaledObject of another API group that happens to share the name and
	// UID of a KEDA one: the group alone keeps it out.
	foreign := scaledObject("foreign", "u-foreign", ourAddr)
	foreignOwner := hpaFor(foreign, t0, cpuMetric())
	foreignOwner.OwnerReferences[0].APIVersion = "example.com/v1"
	// Another install's ScaledObject with scalingModifiers: its HPA carries no
	// composite metric, which would read as unwired were it ours.
	otherModifiers := scaledObject("other-modifiers", "u-other-mod", "scaling-manager-external-scaler.someone-else:9090")
	otherModifiers.Spec.Advanced = &kedav1alpha1.AdvancedConfig{ScalingModifiers: kedav1alpha1.ScalingModifiers{Formula: "s0", Target: "1"}}
	staleOwner := hpaFor(scaledObject("recreated", "u-old", ourAddr), t0, cpuMetric())

	h := newHarness(t,
		other, hpaFor(other, t0, cpuMetric()),
		young, hpaFor(young, t0.Add(10*time.Minute-5*time.Second), cpuMetric()),
		paused, hpaFor(paused, t0, cpuMetric()),
		pausedCond, hpaFor(pausedCond, t0, cpuMetric()),
		wired, hpaFor(wired, t0, externalMetric("s0-x")),
		renamed, staleOwner,
		plainHPA, foreign, foreignOwner,
		otherModifiers, hpaFor(otherModifiers, t0, cpuMetric()),
	)
	res := h.pass()
	if h.patches != 0 || res.Unwired[modelNS] != 0 || len(res.Unread) != 0 {
		t.Fatalf("patches %d, result %+v: want nothing patched, unwired or unread", h.patches, res)
	}
	if ev := h.events(); len(ev) != 0 {
		t.Errorf("events %q, want none", ev)
	}
}

// A failed or Forbidden read is unknown, not absent: nothing is patched, the
// unread gauge says so, and the last known unwired count stays.
func TestAFailedReadIsUnknownNotAbsent(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "autoscaling", Resource: "horizontalpodautoscalers"}, "", nil)

	t.Run("list", func(t *testing.T) {
		so := scaledObject("model-a", "u1", ourAddr)
		h := newHarness(t, so, hpaFor(so, t0, cpuMetric()))
		h.pass()
		h.listErr = forbidden
		h.now = h.now.Add(time.Hour)
		res := h.pass()
		if len(res.Unread) != 1 || res.Unread[0] != modelNS {
			t.Errorf("unread %v, want [%s]", res.Unread, modelNS)
		}
		if h.patches != 1 {
			t.Errorf("patches %d, want only the first pass's", h.patches)
		}
		if v := h.gauge(constants.LLMSMKEDAUnwiredHPAs, "namespace", modelNS); v != 1 {
			t.Errorf("%s = %v after a failed list, want the last known 1", constants.LLMSMKEDAUnwiredHPAs, v)
		}
		if v := h.gauge(constants.LLMSMKEDAHPAWiringUnread); v != 1 {
			t.Errorf("%s = %v, want 1", constants.LLMSMKEDAHPAWiringUnread, v)
		}
		if len(h.r.tracked) != 1 {
			t.Errorf("tracked %d HPAs, want the one kept across the failed list", len(h.r.tracked))
		}
	})

	t.Run("list every namespace", func(t *testing.T) {
		so := scaledObject("model-a", "u1", ourAddr)
		h := newHarness(t, so, hpaFor(so, t0, cpuMetric()))
		h.r.Namespaces = nil
		h.pass()
		h.listErr = forbidden
		if res := h.pass(); len(res.Unread) != 1 || res.Unread[0] != "*" {
			t.Errorf("unread %v, want [*]", res.Unread)
		}
		if v := h.gauge(constants.LLMSMKEDAUnwiredHPAs, "namespace", modelNS); v != 1 {
			t.Errorf("%s = %v after a failed cluster-wide list, want the last known 1", constants.LLMSMKEDAUnwiredHPAs, v)
		}
	})

	t.Run("scaledobject", func(t *testing.T) {
		so := scaledObject("model-a", "u1", ourAddr)
		h := newHarness(t, so, hpaFor(so, t0, cpuMetric()))
		h.getErr = apierrors.NewForbidden(schema.GroupResource{Group: "keda.sh", Resource: "scaledobjects"}, so.Name, nil)
		res := h.pass()
		if h.patches != 0 {
			t.Errorf("patches %d, want none while the ScaledObject cannot be read", h.patches)
		}
		if len(res.Unread) != 1 || res.Unread[0] != modelNS+"/model-a" {
			t.Errorf("unread %v, want [%s/model-a]", res.Unread, modelNS)
		}
		if v := h.gauge(constants.LLMSMKEDAHPAWiringUnread); v != 1 {
			t.Errorf("%s = %v, want 1", constants.LLMSMKEDAHPAWiringUnread, v)
		}
		h.getErr = nil
		if res := h.pass(); res.Requested[modelNS] != 1 || len(res.Unread) != 0 {
			t.Errorf("after the read recovers: %+v, want one request and nothing unread", res)
		}
	})
}

// A denied patch is counted and reported, and retried on the backoff.
func TestAForbiddenPatchIsReported(t *testing.T) {
	so := scaledObject("model-a", "u1", ourAddr)
	hpa := hpaFor(so, t0, cpuMetric())
	h := newHarness(t, so, hpa)
	h.patchErr = apierrors.NewForbidden(schema.GroupResource{Group: "autoscaling", Resource: "horizontalpodautoscalers"}, hpa.Name, nil)
	res := h.pass()
	if res.Requested[modelNS] != 0 || res.Unwired[modelNS] != 1 {
		t.Fatalf("result %+v, want unwired and no accepted request", res)
	}
	if v := h.gauge(constants.LLMSMKEDAHPARewiresTotal, "namespace", modelNS, "outcome", constants.HPARewireForbidden); v != 1 {
		t.Errorf("%s{outcome=forbidden} = %v, want 1", constants.LLMSMKEDAHPARewiresTotal, v)
	}
	if ev := h.events(); len(ev) != 1 || !strings.Contains(ev[0], corev1.EventTypeWarning+" "+EventRewireFailed) {
		t.Errorf("events %q, want one Warning %s", ev, EventRewireFailed)
	}
	if h.annotation(hpa.Name) != "" {
		t.Error("the HPA was annotated although the patch was denied")
	}
	h.now = h.now.Add(time.Second)
	h.pass()
	if v := h.gauge(constants.LLMSMKEDAHPARewiresTotal, "namespace", modelNS, "outcome", constants.HPARewireForbidden); v != 1 {
		t.Errorf("retried inside the backoff: forbidden count %v, want 1", v)
	}
}

// A ScaledObject that cannot be read leaves its namespace's unwired count
// as it was, not lowered to what could be read.
func TestAnUnreadScaledObjectKeepsTheLastCount(t *testing.T) {
	a := scaledObject("model-a", "u1", ourAddr)
	b := scaledObject("model-b", "u2", ourAddr)
	h := newHarness(t, a, hpaFor(a, t0, cpuMetric()), b, hpaFor(b, t0, cpuMetric()))
	h.pass()
	if v := h.gauge(constants.LLMSMKEDAUnwiredHPAs, "namespace", modelNS); v != 2 {
		t.Fatalf("%s = %v, want 2", constants.LLMSMKEDAUnwiredHPAs, v)
	}
	h.getErr = apierrors.NewForbidden(schema.GroupResource{Group: "keda.sh", Resource: "scaledobjects"}, "", nil)
	h.pass()
	if v := h.gauge(constants.LLMSMKEDAUnwiredHPAs, "namespace", modelNS); v != 2 {
		t.Errorf("%s = %v while the ScaledObjects cannot be read, want the last known 2", constants.LLMSMKEDAUnwiredHPAs, v)
	}
}

// Cluster scope: a namespace whose HPAs are all wired again loses its series,
// and an HPA that goes away is forgotten.
func TestWiredAndGoneAreForgotten(t *testing.T) {
	so := scaledObject("model-a", "u1", ourAddr)
	hpa := hpaFor(so, t0, cpuMetric())
	gone := scaledObject("model-b", "u2", ourAddr)
	goneHPA := hpaFor(gone, t0, cpuMetric())
	h := newHarness(t, so, hpa, gone, goneHPA)
	h.r.Namespaces = nil
	h.pass()
	if v := h.gauge(constants.LLMSMKEDAUnwiredHPAs, "namespace", modelNS); v != 2 {
		t.Fatalf("%s = %v, want 2", constants.LLMSMKEDAUnwiredHPAs, v)
	}
	if len(h.r.tracked) != 2 {
		t.Fatalf("tracked %d, want 2", len(h.r.tracked))
	}

	if err := h.client.Delete(context.Background(), goneHPA); err != nil {
		t.Fatal(err)
	}
	h.pass()
	if _, still := h.r.tracked[goneHPA.UID]; still || len(h.r.tracked) != 1 {
		t.Errorf("tracked %v, want the deleted HPA forgotten and the other kept", h.r.tracked)
	}

	var cur autoscalingv2.HorizontalPodAutoscaler
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(hpa), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Metrics = []autoscalingv2.MetricSpec{externalMetric("s0-scaling-manager-desired-replicas")}
	if err := h.client.Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	var curSO kedav1alpha1.ScaledObject
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(so), &curSO); err != nil {
		t.Fatal(err)
	}
	curSO.Status.ExternalMetricNames = []string{"s0-scaling-manager-desired-replicas"}
	if err := h.client.Update(context.Background(), &curSO); err != nil {
		t.Fatal(err)
	}
	h.pass()
	if v := h.gauge(constants.LLMSMKEDAUnwiredHPAs, "namespace", modelNS); v != -1 {
		t.Errorf("%s = %v once every HPA is wired in cluster scope, want the series gone", constants.LLMSMKEDAUnwiredHPAs, v)
	}
	if len(h.r.tracked) != 0 {
		t.Errorf("tracked %v, want nothing once wired", h.r.tracked)
	}
}

// An HPA that already carries this scaler's metric costs no ScaledObject
// read; one with composite-metric, or one the repair asked about, does.
func TestAWiredHPAIsNotRead(t *testing.T) {
	plain := scaledObject("plain", "u1", ourAddr)
	modifiers := scaledObject("modifiers", "u2", ourAddr)
	modifiers.Spec.Advanced = &kedav1alpha1.AdvancedConfig{ScalingModifiers: kedav1alpha1.ScalingModifiers{Formula: "s0", Target: "1"}}
	h := newHarness(t,
		plain, hpaFor(plain, t0, externalMetric("s0-scaling-manager-desired-replicas")),
		modifiers, hpaFor(modifiers, t0, externalMetric(kedav1alpha1.CompositeMetricName)))
	h.getErr = apierrors.NewForbidden(schema.GroupResource{Group: "keda.sh", Resource: "scaledobjects"}, "", nil)
	res := h.pass()
	if len(res.Unread) != 1 || res.Unread[0] != modelNS+"/modifiers" {
		t.Errorf("unread %v, want only %s/modifiers: the wired HPA must not be read", res.Unread, modelNS)
	}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"s0-scaling-manager-desired-replicas", true},
		{"s12-scaling-manager-desired-replicas", true},
		{"s-scaling-manager-desired-replicas", false},
		{"sx-scaling-manager-desired-replicas", false},
		{"s0-other", false},
		{"scaling-manager-desired-replicas", false},
	} {
		got := carriesScalerMetric(hpaFor(plain, t0, externalMetric(tc.name)))
		if got != tc.want {
			t.Errorf("carriesScalerMetric(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	if carriesScalerMetric(hpaFor(plain, t0, externalMetric("s0-scaling-manager-desired-replicas"), externalMetric(kedav1alpha1.CompositeMetricName))) {
		t.Error("an HPA with composite-metric was taken as wired without reading the ScaledObject")
	}
}

// The repair runs on the leader only.
func TestNeedsLeaderElection(t *testing.T) {
	if !(&Repairer{}).NeedLeaderElection() {
		t.Error("the repair must run on the leader only")
	}
}
