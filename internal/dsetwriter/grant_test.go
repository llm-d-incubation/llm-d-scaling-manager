package dsetwriter

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// With NamespacesFunc the writer lists and writes the sets of the namespaces
// it returns only: on the one-manifest install, those holding the
// per-namespace grant. A set elsewhere is neither listed nor written, so the
// writer needs no list or patch on every set in the cluster.
func TestPassScansOnlyTheGrantedNamespaces(t *testing.T) {
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 5)
	store.Set("other", "glm-decode", 5)
	other := set(optIn("true"), "", 3)
	other.SetNamespace("other")
	other.SetUID("uid-other")

	scheme := runtime.NewScheme()
	gv := schema.GroupVersion{Group: setGVK.Group, Version: setGVK.Version}
	scheme.AddKnownTypeWithName(setGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gv.WithKind(constants.DisaggregatedSetKind+"List"), &unstructured.UnstructuredList{})
	var listedIn []string
	applied := map[string]int{}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(set(optIn("true"), "", 3), other).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lo := (&client.ListOptions{}).ApplyOptions(opts)
			listedIn = append(listedIn, lo.Namespace)
			return cl.List(ctx, list, opts...)
		},
		Apply: func(_ context.Context, _ client.WithWatch, o runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			doc := o.(interface{ UnstructuredContent() map[string]any }).UnstructuredContent()
			n, _, _ := unstructured.NestedString(doc, "metadata", "namespace")
			applied[n]++
			return nil
		},
	}).Build()
	w := &Writer{Client: c, Registry: registry.New(0), Decisions: store, Passive: passiveModels{}, ScaleDownWindow: -1,
		AllNamespaces:  true, // NamespacesFunc wins over it
		NamespacesFunc: func(context.Context) ([]string, error) { return []string{ns}, nil }}
	w.Pass(context.Background())

	require.NotEmpty(t, listedIn)
	for _, n := range listedIn {
		assert.Equal(t, ns, n, "the writer listed outside the granted namespaces (\"\" is every namespace)")
	}
	assert.Equal(t, map[string]int{ns: 1}, applied)

	listedIn = nil
	w.NamespacesFunc = func(context.Context) ([]string, error) { return nil, errors.New("forbidden") }
	w.Pass(context.Background())
	assert.Empty(t, listedIn, "a failed namespace read must skip the pass, not fall back to every namespace")
}

// GrantedNamespaces: a RoleBinding labelled as the role writer's grant that
// binds this controller, in a namespace it manages.
func TestGrantedNamespaces(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, rbacv1.AddToScheme(scheme))
	rb := func(namespace, label, saNS string) *rbacv1.RoleBinding {
		b := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "scaling-manager-dset-role-writer-sys"},
			RoleRef:  rbacv1.RoleRef{Kind: "Role", Name: "scaling-manager-dset-role-writer-role"},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "scaling-manager-controller-manager", Namespace: saNS}}}
		if label != "" {
			b.Labels = map[string]string{GrantLabel: label}
		}
		return b
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		rb("team-a", GrantName, "sys"),
		rb("team-b", GrantName, "sys"),    // not managed
		rb("team-c", "pod-writer", "sys"), // another grant
		rb("team-d", GrantName, "other"),  // another install's controller
		rb("team-e", "", "sys"),           // unlabelled
	).Build()
	got, err := GrantedNamespaces(c, "sys", "scaling-manager-controller-manager",
		func(n string) bool { return n != "team-b" })(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"team-a"}, got)
}
