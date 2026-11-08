package scaler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// These specs cover how the scaler answers without a usable decision: where
// it reads the current count (the scale subresource), what it costs, how the
// undecided set behind llmsm_scaler_undecided_targets moves, and the stale rule
// GetMetrics now shares with isActive. Package scaler, for the clock.

const undecidedNS = "chat"

// readCounts counts the API reads a handler makes.
type readCounts struct{ gets, scales int }

// scaleFixture is one scale target the fake API server knows: its object as a
// plain Get returns it, and the replica count its scale subresource reports
// (nil: the kind has no scale subresource).
type scaleFixture struct {
	gvk    schema.GroupVersionKind
	name   string
	object map[string]any
	scale  *int64
	// scaleErr, when set, is what the scale read fails with.
	scaleErr error
}

// crdHandler builds a handler over a ScaledObject "so" scaling f, with an
// API server emulated by interceptors for the kinds the scheme does not know
// -- which is exactly how the client reads a CRD it has no Go type for.
func crdHandler(t *testing.T, store *decision.Store, f scaleFixture, counts *readCounts) *Handler {
	t.Helper()
	s := runtime.NewScheme()
	if err := kedav1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	apiVersion, kind := f.gvk.ToAPIVersionAndKind()
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Namespace: undecidedNS, Name: "so"},
		Spec: kedav1alpha1.ScaledObjectSpec{ScaleTargetRef: &kedav1alpha1.ScaleTarget{
			APIVersion: apiVersion, Kind: kind, Name: f.name,
		}},
	}
	notFound := func(name string) error {
		return apierrors.NewNotFound(schema.GroupResource{Group: f.gvk.Group, Resource: strings.ToLower(kind) + "s"}, name)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(so).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			counts.gets++
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == f.gvk {
				if key.Name != f.name || f.object == nil {
					return notFound(key.Name)
				}
				u.Object = runtime.DeepCopyJSON(f.object)
				u.SetGroupVersionKind(f.gvk)
				u.SetNamespace(key.Namespace)
				u.SetName(key.Name)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
		SubResourceGet: func(ctx context.Context, c client.Client, sub string, obj, out client.Object, opts ...client.SubResourceGetOption) error {
			counts.scales++
			if sub != "scale" || obj.GetObjectKind().GroupVersionKind() != f.gvk || obj.GetName() != f.name {
				return c.SubResource(sub).Get(ctx, obj, out, opts...)
			}
			if f.scaleErr != nil {
				return f.scaleErr
			}
			if f.scale == nil {
				return notFound(obj.GetName() + "/scale")
			}
			u, ok := out.(*unstructured.Unstructured)
			if !ok {
				t.Fatalf("a CRD's scale must be read unstructured, got %T", out)
			}
			// The API server omits replicas when it is 0 (omitempty).
			spec := map[string]any{}
			if *f.scale != 0 {
				spec["replicas"] = *f.scale
			}
			u.Object = map[string]any{"apiVersion": "autoscaling/v1", "kind": "Scale",
				"metadata": map[string]any{"name": f.name, "namespace": undecidedNS}, "spec": spec}
			return nil
		},
	}).Build()
	return NewHandler(c, store, registry.New(0))
}

func getMetrics(ctx context.Context, h *Handler, md map[string]string) (int64, error) {
	resp, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
		ScaledObjectRef: &pb.ScaledObjectRef{Namespace: undecidedNS, Name: "so", ScalerMetadata: md},
	})
	if err != nil {
		return 0, err
	}
	return resp.MetricValues[0].MetricValue, nil
}

var managed = map[string]string{registry.ModelIDKey: "default/default"}

// TestUndecidedReadsTheScaleSubresource: the held count is what the scale
// subresource says, which for a CRD lives wherever its specReplicasPath
// points. Reading .spec.replicas off the object guessed 1 for a CRD that keeps
// it elsewhere, and for a kind that cannot be scaled at all.
func TestUndecidedReadsTheScaleSubresource(t *testing.T) {
	lws := schema.GroupVersionKind{Group: "leaderworkerset.x-k8s.io", Version: "v1", Kind: "LeaderWorkerSet"}
	engine := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Engine"}
	for _, tc := range []struct {
		name     string
		f        scaleFixture
		want     int64
		wantCode codes.Code
	}{
		{name: "a LeaderWorkerSet", want: 2, f: scaleFixture{gvk: lws, name: "chat-lws",
			object: map[string]any{"spec": map[string]any{"replicas": int64(2)}}, scale: ptr.To(int64(2))}},
		{name: "a CRD whose specReplicasPath is .spec.size", want: 3, f: scaleFixture{gvk: engine, name: "chat-engine",
			object: map[string]any{"spec": map[string]any{"size": int64(3)}}, scale: ptr.To(int64(3))}},
		{name: "a CRD scaled to 0 (replicas omitted by the API server)", want: 0, f: scaleFixture{gvk: engine, name: "chat-engine",
			object: map[string]any{"spec": map[string]any{"size": int64(0)}}, scale: ptr.To(int64(0))}},
		{name: "a kind with no scale subresource is an error, not 1", wantCode: codes.NotFound, f: scaleFixture{gvk: engine, name: "chat-engine",
			object: map[string]any{"spec": map[string]any{}}}},
		{name: "a missing target is an error, not 1", wantCode: codes.NotFound, f: scaleFixture{gvk: engine, name: "chat-engine",
			scale: nil}},
		{name: "a forbidden scale read is PermissionDenied", wantCode: codes.PermissionDenied, f: scaleFixture{gvk: engine, name: "chat-engine",
			object: map[string]any{"spec": map[string]any{"size": int64(3)}}, scale: ptr.To(int64(3)),
			scaleErr: apierrors.NewForbidden(schema.GroupResource{Group: "example.com", Resource: "engines/scale"}, "chat-engine", errors.New("rbac"))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := crdHandler(t, decision.NewStore(), tc.f, &readCounts{})
			got, err := getMetrics(context.Background(), h, managed)
			if tc.wantCode != codes.OK {
				if status.Code(err) != tc.wantCode {
					t.Fatalf("GetMetrics = %d, %v; want code %s", got, err, tc.wantCode)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("GetMetrics = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

// TestUndecidedCostsOneReadPerAnswer: the target is resolved once per call and
// reused, so an undecided answer costs the scale read and, before the registry
// knows the target, the one ScaledObject read that resolves it. It used to read
// the ScaledObject twice and the target object once more.
func TestUndecidedCostsOneReadPerAnswer(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Engine"}
	f := scaleFixture{gvk: gvk, name: "chat-engine",
		object: map[string]any{"spec": map[string]any{"size": int64(3)}}, scale: ptr.To(int64(3))}

	t.Run("target not yet in the registry", func(t *testing.T) {
		var n readCounts
		h := crdHandler(t, decision.NewStore(), f, &n)
		if _, err := getMetrics(context.Background(), h, managed); err != nil {
			t.Fatal(err)
		}
		if n.gets != 1 || n.scales != 1 {
			t.Fatalf("reads: %d Get + %d scale; want 1 Get (the ScaledObject) + 1 scale", n.gets, n.scales)
		}
	})
	t.Run("target resolved by the enricher", func(t *testing.T) {
		var n readCounts
		h := crdHandler(t, decision.NewStore(), f, &n)
		h.registry.Observe(undecidedNS, "so", managed)
		h.registry.SetTarget(undecidedNS, "so", registry.Target{APIVersion: "example.com/v1", Kind: "Engine", Name: "chat-engine"})
		if _, err := getMetrics(context.Background(), h, managed); err != nil {
			t.Fatal(err)
		}
		if n.gets != 0 || n.scales != 1 {
			t.Fatalf("reads: %d Get + %d scale; want only the scale read", n.gets, n.scales)
		}
	})
}

// deploymentHandler is a handler over ScaledObject "so" scaling a Deployment at
// 3 replicas, with a controllable clock.
func deploymentHandler(t *testing.T, store *decision.Store, now *time.Time) *Handler {
	t.Helper()
	s := runtime.NewScheme()
	if err := kedav1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{Namespace: undecidedNS, Name: "so"},
			Spec:       kedav1alpha1.ScaledObjectSpec{ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: "chat-deploy"}},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: undecidedNS, Name: "chat-deploy"},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
		},
	).Build()
	h := NewHandler(c, store, registry.New(0))
	h.now = func() time.Time { return *now }
	return h
}

// TestUndecidedTargetsFollowsTransitions: the set behind
// llmsm_scaler_undecided_targets holds a target exactly while it is answered
// without a decision, and each transition into that state is logged -- so a
// target that loses its decision again shows up again.
func TestUndecidedTargetsFollowsTransitions(t *testing.T) {
	now := time.Now()
	store := decision.NewStore()
	h := deploymentHandler(t, store, &now)

	var lines []string
	ctx := ctrllog.IntoContext(context.Background(),
		funcr.NewJSON(func(obj string) { lines = append(lines, obj) }, funcr.Options{Verbosity: 1}))
	count := func(msg string) int {
		n := 0
		for _, l := range lines {
			if strings.Contains(l, msg) {
				n++
			}
		}
		return n
	}
	const noDecision = "external scaler has no decision for this target"
	const decidedAgain = "external scaler has a decision for this target again"
	ask := func(want int64) {
		t.Helper()
		got, err := getMetrics(ctx, h, managed)
		if err != nil || got != want {
			t.Fatalf("GetMetrics = %d, %v; want %d", got, err, want)
		}
	}

	ask(3)
	ask(3)
	if got := h.UndecidedTargets(); got[undecidedNS] != 1 {
		t.Fatalf("undecided targets = %v, want %s: 1", got, undecidedNS)
	}
	if n := count(noDecision); n != 1 {
		t.Fatalf("no-decision lines = %d, want 1 (per transition, not per poll)", n)
	}

	store.Set(undecidedNS, "chat-deploy", 5)
	ask(5)
	if got := h.UndecidedTargets(); len(got) != 0 {
		t.Fatalf("undecided targets = %v after a decided answer, want none", got)
	}
	if n := count(decidedAgain); n != 1 {
		t.Fatalf("decided-again lines = %d, want 1", n)
	}

	// The decision goes stale: undecided again, and logged again.
	now = now.Add(h.decisionTTL() + time.Second)
	ask(3)
	if got := h.UndecidedTargets(); got[undecidedNS] != 1 {
		t.Fatalf("undecided targets = %v after the decision went stale, want %s: 1", got, undecidedNS)
	}
	if n := count(noDecision); n != 2 {
		t.Fatalf("no-decision lines = %d, want 2: the second transition must be logged", n)
	}
}

// TestUndecidedTargetsIsBounded: a ScaledObject that is deleted, or that KEDA
// stops asking about, leaves the set without ever being decided.
func TestUndecidedTargetsIsBounded(t *testing.T) {
	t.Run("no longer registered", func(t *testing.T) {
		now := time.Now()
		h := deploymentHandler(t, decision.NewStore(), &now)
		if _, err := getMetrics(context.Background(), h, managed); err != nil {
			t.Fatal(err)
		}
		h.registry.Forget(undecidedNS, "so")
		if got := h.UndecidedTargets(); len(got) != 0 {
			t.Fatalf("undecided targets = %v for a forgotten ScaledObject, want none", got)
		}
	})
	t.Run("not asked about for undecidedStaleAfter", func(t *testing.T) {
		now := time.Now()
		h := deploymentHandler(t, decision.NewStore(), &now)
		h.registry = nil // isolate the clock rule from registry liveness
		if _, err := getMetrics(context.Background(), h, managed); err != nil {
			t.Fatal(err)
		}
		now = now.Add(undecidedStaleAfter + time.Second)
		if got := h.UndecidedTargets(); len(got) != 0 {
			t.Fatalf("undecided targets = %v after %s of silence, want none", got, undecidedStaleAfter)
		}
	})
}

// TestUndecidedTargetsGauge: the gauge is read from the handler at scrape
// time, and a namespace with nothing undecided has no series.
func TestUndecidedTargetsGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	store := decision.NewStore()
	h := deploymentHandler(t, store, &now)
	metrics.SetScalerUndecidedTargetsSource(h.UndecidedTargets)
	t.Cleanup(func() { metrics.SetScalerUndecidedTargetsSource(nil) })

	gauge := func() (float64, bool) {
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, mf := range mfs {
			if mf.GetName() != constants.LLMSMScalerUndecidedTargets {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == constants.LabelNamespace && l.GetValue() == undecidedNS {
						return m.GetGauge().GetValue(), true
					}
				}
			}
		}
		return 0, false
	}

	if _, err := getMetrics(context.Background(), h, managed); err != nil {
		t.Fatal(err)
	}
	if v, ok := gauge(); !ok || v != 1 {
		t.Fatalf("%s{namespace=%q} = %v (present %v), want 1", constants.LLMSMScalerUndecidedTargets, undecidedNS, v, ok)
	}
	store.Set(undecidedNS, "chat-deploy", 4)
	if _, err := getMetrics(context.Background(), h, managed); err != nil {
		t.Fatal(err)
	}
	if v, ok := gauge(); ok {
		t.Fatalf("%s{namespace=%q} = %v after a decided answer, want no series", constants.LLMSMScalerUndecidedTargets, undecidedNS, v)
	}
}

// TestGetMetricsAnswersStaleDecisionAsUndecided: GetMetrics and isActive agree
// on whether a decision is usable. A positive decision past decisionTTL is
// answered with the current count and counted as "stale", where GetMetrics used
// to go on serving the abandoned value.
func TestGetMetricsAnswersStaleDecisionAsUndecided(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatal(err)
	}
	store := decision.NewStore()
	store.Set(undecidedNS, "chat-deploy", 4)
	now := time.Now()
	h := deploymentHandler(t, store, &now)

	if got, err := getMetrics(context.Background(), h, managed); err != nil || got != 4 {
		t.Fatalf("fresh: GetMetrics = %d, %v; want 4", got, err)
	}
	now = now.Add(activationTTL + time.Second)
	if got, err := getMetrics(context.Background(), h, managed); err != nil || got != 3 {
		t.Fatalf("stale: GetMetrics = %d, %v; want the current count 3", got, err)
	}
	active, err := h.isActive(context.Background(), &pb.ScaledObjectRef{Namespace: undecidedNS, Name: "so"})
	if err != nil || !active {
		t.Fatalf("isActive = %v, %v; a running target with a stale decision is active", active, err)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var stale float64
	for _, mf := range mfs {
		if mf.GetName() != constants.LLMSMScalerUndecidedAnswersTotal {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == constants.LabelReason && l.GetValue() == constants.ScalerUndecidedStale {
					stale += m.GetCounter().GetValue()
				}
			}
		}
	}
	if stale != 1 {
		t.Fatalf("undecided answers with reason %q = %v, want 1", constants.ScalerUndecidedStale, stale)
	}
}

// TestDecisionTTLCoversASlowCycle: with GLOBAL_OPT_INTERVAL at 60 s (the
// OpenShift overlay) the sliding engine loop republishes a decision every 60 s
// plus the cycle's duration. A decision republished after a cycle of up to two
// intervals (60 s + 119 s) is still fresh; the 60 s floor alone, or two
// intervals, would call it stale between two ordinary publishes.
func TestDecisionTTLCoversASlowCycle(t *testing.T) {
	store := decision.NewStore()
	store.Set(undecidedNS, "chat-deploy", 4)
	now := time.Now()
	h := deploymentHandler(t, store, &now).WithDecisionInterval(func() time.Duration { return 60 * time.Second })

	now = now.Add(61 * time.Second)
	if got, err := getMetrics(context.Background(), h, managed); err != nil || got != 4 {
		t.Fatalf("61 s old at a 60 s interval: GetMetrics = %d, %v; want the decision 4", got, err)
	}
	now = now.Add(118 * time.Second)
	if got, err := getMetrics(context.Background(), h, managed); err != nil || got != 4 {
		t.Fatalf("179 s old at a 60 s interval (cycle of 119 s): GetMetrics = %d, %v; want the decision 4", got, err)
	}
	now = now.Add(2 * time.Second)
	if got, err := getMetrics(context.Background(), h, managed); err != nil || got != 3 {
		t.Fatalf("181 s old at a 60 s interval: GetMetrics = %d, %v; want the current count 3", got, err)
	}
}
