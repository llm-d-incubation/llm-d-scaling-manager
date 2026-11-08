package locator_test

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/locator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// launcherPod models an FMA server-providing pod. Its controller is a
// LauncherConfig, a kind the owner walk cannot follow, so the walk stops without
// reaching a scale target and without erroring — which is precisely the state the
// pairing hop exists to rescue. Using a genuinely unknown kind rather than an
// ownerless pod keeps the fixture faithful to the real object.
func launcherPod(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: testNamespace, Labels: labels,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "fma.llm-d.ai/v1alpha1", Kind: "LauncherConfig",
			Name: "lc", UID: "uid-lc", Controller: ptr.To(true),
		}},
	}}
}

// requesterChain returns the FMA server-requesting pod and the ReplicaSet and
// Deployment above it — the half a scaler actually moves.
func requesterChain(labels map[string]string) []runtime.Object {
	const podName = "requester"
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "req-deploy", Namespace: testNamespace, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "req-rs", Namespace: testNamespace, UID: "uid-rs",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment",
			Name: "req-deploy", UID: "uid-d", Controller: ptr.To(true)}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNamespace, Labels: labels,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet",
			Name: "req-rs", UID: "uid-rs", Controller: ptr.To(true)}}}}
	return []runtime.Object{deploy, rs, pod}
}

func paired(partner, model string) map[string]string {
	l := map[string]string{constants.DualPodsPairLabelKey: partner}
	if model != "" {
		l[constants.ModelLabelKey] = model
	}
	return l
}

// A bound launcher must resolve to the ScaledObject that drives its requester.
// This is the whole point: the pod that reports the engine metrics is not the pod
// that is owned, and without the hop its load is attributed to nothing.
func TestLocate_FMAPairing_ResolvesToRequestersScaler(t *testing.T) {
	ns := testNamespace
	objs := requesterChain(paired("launcher", "qwen-0-6b"))
	objs = append(objs, launcherPod("launcher", paired("requester", "qwen-0-6b")))

	loc, _ := locator.New(newClients(t, objs...), variantsOf(registered(ns, "Deployment", "req-deploy")))
	got, err := loc.Locate(context.Background(), ns, "launcher")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got == nil || got.Name != "h" {
		t.Fatalf("launcher did not resolve through its pairing: got=%v", got)
	}
}

// An unbound launcher carries no pairing label. FMA removes it when the pair
// breaks, so its absence is how a warm spare declines to be counted as capacity.
func TestLocate_FMAPairing_UnboundLauncherNotAttributed(t *testing.T) {
	ns := testNamespace
	loc, _ := locator.New(
		newClients(t, launcherPod("launcher", map[string]string{constants.ModelLabelKey: "qwen-0-6b"})),
		variantsOf(registered(ns, "Deployment", "req-deploy")))

	got, err := loc.Locate(context.Background(), ns, "launcher")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != nil {
		t.Errorf("unbound launcher was attributed: got=%v", got)
	}
}

// A pairing label naming a pod that no longer exists must degrade to "not
// attributed", not to an error. This is the FMA-uninstalled-but-labels-remain
// case, and the mid-unbind race.
func TestLocate_FMAPairing_PartnerDoesNotExist(t *testing.T) {
	ns := testNamespace
	loc, _ := locator.New(
		newClients(t, launcherPod("launcher", paired("ghost", "qwen-0-6b"))),
		variantsOf(registered(ns, "Deployment", "req-deploy")))

	got, err := loc.Locate(context.Background(), ns, "launcher")
	if err != nil {
		t.Fatalf("Locate returned an error for a missing partner: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
}

// Two pods naming each other must terminate. The hop resolves the partner's
// OWNER chain rather than calling Locate again, so there is one hop and no
// recursion — this test is what keeps that true.
func TestLocate_FMAPairing_MutualNamesDoNotRecurse(t *testing.T) {
	ns := testNamespace
	loc, _ := locator.New(newClients(t,
		launcherPod("a", paired("b", "qwen-0-6b")),
		launcherPod("b", paired("a", "qwen-0-6b")),
	), variantsOf(registered(ns, "Deployment", "req-deploy")))

	got, err := loc.Locate(context.Background(), ns, "a")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil (neither pod reaches a scale target)", got)
	}
}

// A pod naming itself is not a pairing.
func TestLocate_FMAPairing_SelfReferenceIgnored(t *testing.T) {
	ns := testNamespace
	loc, _ := locator.New(
		newClients(t, launcherPod("launcher", paired("launcher", "qwen-0-6b"))),
		variantsOf(registered(ns, "Deployment", "req-deploy")))

	got, err := loc.Locate(context.Background(), ns, "launcher")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
}

// FMA permits one launcher to host instances for several models, while the
// pairing label is singular and can name only one requester. When the two halves
// disagree on the model, the safe answer is "not attributed" — under-measuring is
// recoverable, charging one model's load to another variant is not.
func TestLocate_FMAPairing_RejectsDifferentModel(t *testing.T) {
	ns := testNamespace
	objs := requesterChain(paired("launcher", "llama-8b"))
	objs = append(objs, launcherPod("launcher", paired("requester", "qwen-0-6b")))

	loc, _ := locator.New(newClients(t, objs...), variantsOf(registered(ns, "Deployment", "req-deploy")))
	got, err := loc.Locate(context.Background(), ns, "launcher")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != nil {
		t.Errorf("cross-model pairing was followed: got=%v", got)
	}
}

// The model label reaches a launcher from the InferenceServerConfig's label map,
// which FMA treats as arbitrary. Its absence therefore proves nothing and must
// not veto a pairing FMA itself declared.
func TestLocate_FMAPairing_AcceptsWhenModelLabelAbsent(t *testing.T) {
	ns := testNamespace
	objs := requesterChain(paired("launcher", "qwen-0-6b"))
	objs = append(objs, launcherPod("launcher", paired("requester", "")))

	loc, _ := locator.New(newClients(t, objs...), variantsOf(registered(ns, "Deployment", "req-deploy")))
	got, err := loc.Locate(context.Background(), ns, "launcher")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got == nil || got.Name != "h" {
		t.Fatalf("pairing rejected for a missing model label: got=%v", got)
	}
}

// countingClient wraps the fake client and tallies Get calls.
func countingClient(t *testing.T, n *int, objs ...runtime.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithRuntimeObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption) error {
				*n++
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
}

// The no-FMA path must cost exactly what it cost before the hop existed. Asserting
// the call count rather than eyeballing the code is what stops a future edit from
// adding an API call to the common path invisibly — a regression that would show
// up as cluster load, not as a failing test.
func TestLocate_NoPairingLabel_CostsNoExtraAPICall(t *testing.T) {
	ns := testNamespace
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: ns}}

	var gets int
	loc, _ := locator.New(countingClient(t, &gets, pod), variantsOf(registered(ns, "Deployment", "d")))
	got, err := loc.Locate(context.Background(), ns, "plain")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
	if gets != 1 {
		t.Errorf("unmanaged pod cost %d Get(s), want exactly 1 (the pod itself)", gets)
	}
}

// And the hop itself must cost one additional lookup chain, only for pods that
// were already going to be discarded.
func TestLocate_PairingCostsOnlyThePartnerLookup(t *testing.T) {
	ns := testNamespace
	objs := requesterChain(paired("launcher", "qwen-0-6b"))
	objs = append(objs, launcherPod("launcher", paired("requester", "qwen-0-6b")))

	var gets int
	loc, _ := locator.New(countingClient(t, &gets, objs...), variantsOf(registered(ns, "Deployment", "req-deploy")))
	if _, err := loc.Locate(context.Background(), ns, "launcher"); err != nil {
		t.Fatalf("Locate: %v", err)
	}
	// launcher pod, then the requester's own chain: pod, ReplicaSet, Deployment.
	if gets != 4 {
		t.Errorf("pairing cost %d Get(s), want 4 (launcher, requester, rs, deployment)", gets)
	}
}

// A launcher must stop resolving once its partner is deleted, even though an
// earlier Locate already cached that partner's chain.
//
// The resolution cache is correct "for the lifetime of the cached pod" — the
// immutability it relies on is ownerReferences, not continued existence. Every
// ordinary caller resolves the pod it was handed, so the distinction never shows.
// The pairing hop resolves somebody ELSE's pod on the strength of a label, and
// that partner can go while the label still names it.
//
// Left uninvalidated, the cached chain answers forever: the launcher stays
// attributed to a variant that has no requester. For a PARKED model that reads as
// serving, and scale-from-zero declines to wake a model whose decode it believes
// is covered — parked, unwakeable, reported healthy. The first Locate here is not
// scene-setting; it is what populates the cache that made this wrong.
func TestLocate_FMAPairing_PartnerDeletedAfterCaching(t *testing.T) {
	ns := testNamespace
	objs := requesterChain(paired("launcher", "qwen-0-6b"))
	objs = append(objs, launcherPod("launcher", paired("requester", "qwen-0-6b")))

	cl := newClients(t, objs...)
	loc, _ := locator.New(cl, variantsOf(registered(ns, "Deployment", "req-deploy")))

	got, err := loc.Locate(context.Background(), ns, "launcher")
	if err != nil {
		t.Fatalf("Locate before delete: %v", err)
	}
	if got == nil {
		t.Fatalf("precondition failed: the bound launcher did not resolve, so nothing was cached")
	}

	if err := cl.Delete(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "requester", Namespace: ns},
	}); err != nil {
		t.Fatalf("delete requester: %v", err)
	}

	got, err = loc.Locate(context.Background(), ns, "launcher")
	if err != nil {
		t.Fatalf("Locate after delete: %v", err)
	}
	if got != nil {
		t.Errorf("launcher still attributed to %v after its requester was deleted; the partner's "+
			"cached chain is being trusted, so a parked FMA model would read as serving", got)
	}
}
