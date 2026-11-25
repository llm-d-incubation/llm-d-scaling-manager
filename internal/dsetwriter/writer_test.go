package dsetwriter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

const ns = "serving"

// set is a P/D DisaggregatedSet: prefill and decode in the given modes at
// prefill at 2 groups and decode at the given count, a third role "encode" always External.
func set(annotations map[string]any, prefillMode string, decode int64) *unstructured.Unstructured {
	role := func(name, mode string, replicas int64) map[string]any {
		r := map[string]any{"name": name, "spec": map[string]any{
			"replicas": replicas,
			"leaderWorkerTemplate": map[string]any{"size": int64(2), "workerTemplate": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"llm-d.ai/role": name}},
			}},
		}}
		if mode != "" {
			r["scaling"] = map[string]any{"mode": mode}
		}
		return r
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.DisaggregatedSetAPIVersion,
		"kind":       constants.DisaggregatedSetKind,
		"metadata":   map[string]any{"name": "glm", "namespace": ns, "uid": "uid-glm", "annotations": annotations},
		"spec": map[string]any{"roles": []any{
			role("prefill", prefillMode, 2),
			role("decode", "", decode),
			role("encode", "External", 1),
		}},
	}}
}

func optIn(mode string) map[string]any {
	return map[string]any{OptInAnnotation: mode, ModelIDAnnotation: "org/GLM", constants.MaxReplicasAnnotation: "8"}
}

func TestPlanHonoursAllZeroOrAllNonZero(t *testing.T) {
	roles := []Role{{Name: "prefill", Current: 2}, {Name: "decode", Current: 3}}
	for _, tc := range []struct {
		name    string
		decided map[string]int32
		lo, hi  int32
		want    map[string]int32
	}{
		{"no decision keeps every count", nil, 1, 10, map[string]int32{"prefill": 2, "decode": 3}},
		{"each role moves on its own decision", map[string]int32{"prefill": 1, "decode": 7}, 1, 10,
			map[string]int32{"prefill": 1, "decode": 7}},
		{"clamped to the bounds", map[string]int32{"decode": 40}, 1, 10, map[string]int32{"prefill": 2, "decode": 10}},
		{"one role parked alone keeps a group", map[string]int32{"prefill": 0}, 0, 10,
			map[string]int32{"prefill": 1, "decode": 3}},
		{"one role parked alone keeps the minimum", map[string]int32{"prefill": 0, "decode": 4}, 2, 10,
			map[string]int32{"prefill": 2, "decode": 4}},
		{"every role parked: all zero", map[string]int32{"prefill": 0, "decode": 0}, 0, 10,
			map[string]int32{"prefill": 0, "decode": 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Plan(roles, tc.decided, tc.lo, tc.hi))
		})
	}
}

func TestStaticRoles(t *testing.T) {
	roles, why := StaticRoles(set(nil, "", 3))
	assert.Empty(t, why)
	assert.Equal(t, []Role{{"prefill", 2}, {"decode", 3}}, roles, "the External encode role is the role scaler's")

	roles, _ = StaticRoles(set(nil, "External", 3))
	assert.Equal(t, []Role{{"decode", 3}}, roles)

	s := set(nil, "", 3)
	require.NoError(t, unstructured.SetNestedField(s.Object, int64(2), "spec", "slices"))
	roles, why = StaticRoles(s)
	assert.Nil(t, roles)
	assert.Contains(t, why, "spec.slices is 2")

	s = set(nil, "", 3)
	r, _, _ := unstructured.NestedSlice(s.Object, "spec", "roles")
	r[0].(map[string]any)["subRoles"] = []any{map[string]any{"name": "batch"}}
	unstructured.RemoveNestedField(r[1].(map[string]any), "spec", "replicas")
	require.NoError(t, unstructured.SetNestedSlice(s.Object, r, "spec", "roles"))
	roles, _ = StaticRoles(s)
	assert.Equal(t, []Role{{"decode", 1}}, roles, "a partitioned role is skipped; an unset count is the API default 1")
}

func TestApplyConfigurationNamesOnlyReplicas(t *testing.T) {
	doc := ApplyConfiguration(set(nil, "", 3), map[string]int32{"decode": 5, "prefill": 2})
	assert.Equal(t, map[string]any{
		"apiVersion": constants.DisaggregatedSetAPIVersion, "kind": constants.DisaggregatedSetKind,
		"metadata": map[string]any{"name": "glm", "namespace": ns},
		"spec": map[string]any{"roles": []any{
			map[string]any{"name": "prefill", "spec": map[string]any{"replicas": int64(2)}},
			map[string]any{"name": "decode", "spec": map[string]any{"replicas": int64(5)}},
		}},
	}, doc.Object, "the roles in the set's own order, never re-sorted")
}

type applied struct {
	calls   int
	doc     map[string]any
	manager string
	force   bool
}

func newWriter(t *testing.T, obj *unstructured.Unstructured, store *decision.Store, applyErr error) (*Writer, *applied, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	gv := schema.GroupVersion{Group: setGVK.Group, Version: setGVK.Version}
	scheme.AddKnownTypeWithName(setGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gv.WithKind(constants.DisaggregatedSetKind+"List"), &unstructured.UnstructuredList{})
	got := &applied{}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, o runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			got.calls++
			got.doc = o.(interface{ UnstructuredContent() map[string]any }).UnstructuredContent()
			ao := (&client.ApplyOptions{}).ApplyOptions(opts)
			got.manager = ao.FieldManager
			got.force = ao.Force != nil && *ao.Force
			return applyErr
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			t.Error("the writer changes a set only by server-side apply")
			return nil
		},
	}).Build()
	rec := record.NewFakeRecorder(10)
	// No scale-down window: the tests of the window set their own.
	return &Writer{Client: c, Registry: registry.New(0), Decisions: store, Recorder: rec, Passive: passiveModels{}, AllNamespaces: true, ScaleDownWindow: -1}, got, rec
}

func TestPassAppliesDecisionsToStaticRolesOnly(t *testing.T) {
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 5)
	store.Set(ns, "glm-encode", 4) // External: the role scaler's, never written
	w, got, _ := newWriter(t, set(optIn("true"), "", 3), store, nil)
	w.Pass(context.Background())

	require.Equal(t, 1, got.calls)
	assert.Equal(t, FieldManager, got.manager)
	assert.False(t, got.force)
	roles, _, _ := unstructured.NestedSlice(got.doc, "spec", "roles")
	assert.Equal(t, []any{
		map[string]any{"name": "prefill", "spec": map[string]any{"replicas": int64(2)}},
		map[string]any{"name": "decode", "spec": map[string]any{"replicas": int64(5)}},
	}, roles)

	// Registered as a variant the engines size, under the scale-target name a
	// role scaler would have, so pods attribute the same way on both paths.
	e, ok := w.Registry.FindByScaleTarget(ns, constants.DisaggregatedSetRoleScalerKind, "glm-decode")
	require.True(t, ok)
	assert.Equal(t, "glm-decode-llmsm", e.Name)
	assert.Equal(t, "org/GLM", e.Metadata[registry.ModelIDKey])
	assert.Equal(t, registry.SourceRoleWriter, e.Metadata[registry.SourceKey])
	assert.Equal(t, int32(8), *e.Target.MaxReplicas)
	_, ok = w.Registry.FindByScaleTarget(ns, constants.DisaggregatedSetRoleScalerKind, "glm-encode")
	assert.False(t, ok, "an External role is registered by KEDA, not by the writer")
}

func TestPassWritesNothingWithoutOptInOrChange(t *testing.T) {
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 5)
	w, got, _ := newWriter(t, set(map[string]any{ModelIDAnnotation: "org/GLM"}, "", 3), store, nil)
	w.Pass(context.Background())
	assert.Equal(t, 0, got.calls, "not opted in")
	assert.Equal(t, 0, w.Registry.Len())

	store = decision.NewStore()
	store.Set(ns, "glm-decode", 4)
	w, got, _ = newWriter(t, set(optIn("true"), "", 4), store, nil)
	w.Pass(context.Background())
	assert.Equal(t, 0, got.calls, "the decision is the current count")
	assert.Equal(t, 2, w.Registry.Len(), "but both Static roles are registered")
}

// A set the writer cannot scale is refused, on the set, and nothing is
// written or registered for it.
func TestPassRefusesWhatItCannotScale(t *testing.T) {
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 5)

	sliced := set(optIn("true"), "", 3)
	require.NoError(t, unstructured.SetNestedField(sliced.Object, int64(2), "spec", "slices"))
	noModel := set(map[string]any{OptInAnnotation: "true"}, "", 3)
	for name, s := range map[string]*unstructured.Unstructured{"slices above 1": sliced, "no model-id": noModel} {
		t.Run(name, func(t *testing.T) {
			w, got, rec := newWriter(t, s, store, nil)
			w.Pass(context.Background())
			assert.Equal(t, 0, got.calls)
			assert.Equal(t, 0, w.Registry.Len())
			select {
			case ev := <-rec.Events:
				assert.True(t, strings.Contains(ev, EventRefused), ev)
			default:
				t.Fatal("a refused set must be told why")
			}
		})
	}
}

func TestPassIgnoresStalePositiveDecisions(t *testing.T) {
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 6)
	w, got, _ := newWriter(t, set(optIn("true"), "", 3), store, nil)
	w.Now = func() time.Time { return time.Now().Add(2 * DefaultDecisionTTL) }
	w.Pass(context.Background())
	assert.Equal(t, 0, got.calls)

	// A decision to park never goes stale: it stands until the engines wake
	// the model.
	store = decision.NewStore()
	store.Set(ns, "glm-decode", 0)
	store.Set(ns, "glm-prefill", 0)
	w, got, _ = newWriter(t, set(optIn("true"), "", 3), store, nil)
	w.Now = func() time.Time { return time.Now().Add(2 * DefaultDecisionTTL) }
	w.Pass(context.Background())
	require.Equal(t, 1, got.calls)
	roles, _, _ := unstructured.NestedSlice(got.doc, "spec", "roles")
	assert.Equal(t, []any{
		map[string]any{"name": "prefill", "spec": map[string]any{"replicas": int64(0)}},
		map[string]any{"name": "decode", "spec": map[string]any{"replicas": int64(0)}},
	}, roles)
}

func TestPassNeverParksOneStaticRoleAlone(t *testing.T) {
	store := decision.NewStore()
	store.Set(ns, "glm-prefill", 0)
	w, got, _ := newWriter(t, set(optIn("true"), "", 3), store, nil)
	w.Pass(context.Background())
	require.Equal(t, 1, got.calls)
	roles, _, _ := unstructured.NestedSlice(got.doc, "spec", "roles")
	assert.Contains(t, roles, map[string]any{"name": "prefill", "spec": map[string]any{"replicas": int64(1)}})
}

func TestPassForceAndConflict(t *testing.T) {
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 5)
	w, got, _ := newWriter(t, set(optIn("force"), "", 3), store, nil)
	w.Pass(context.Background())
	assert.True(t, got.force, "force takes the field over")

	conflict := apierrors.NewConflict(schema.GroupResource{Group: setGVK.Group, Resource: "disaggregatedsets"}, "glm",
		assertErr("Apply failed with 1 conflict: conflict with \"argocd-controller\""))
	w, got, rec := newWriter(t, set(optIn("true"), "", 3), store, conflict)
	w.Pass(context.Background())
	assert.Equal(t, 1, got.calls)
	assert.False(t, got.force)
	select {
	case ev := <-rec.Events:
		assert.True(t, strings.Contains(ev, EventConflict), ev)
	default:
		t.Fatal("a conflict must be reported on the set")
	}

	// Any other failure, a rejected write say, is reported on the set too:
	// the log alone is not where an operator looks.
	invalid := apierrors.NewInvalid(setGVK.GroupKind(), "glm", nil)
	w, _, rec = newWriter(t, set(optIn("true"), "", 3), store, invalid)
	w.Pass(context.Background())
	select {
	case ev := <-rec.Events:
		assert.True(t, strings.Contains(ev, EventFailed), ev)
	default:
		t.Fatal("a failed write must be reported on the set")
	}
}

func TestStaticRoleAccessorReadsTheSet(t *testing.T) {
	acc, err := scaletarget.NewDisaggregatedSetStaticRoleAccessor(set(nil, "", 3), "decode")
	require.NoError(t, err)
	assert.Equal(t, "glm-decode", acc.GetName())
	assert.Equal(t, int32(3), *acc.GetReplicas())
	_, role, ok := scaletarget.DisaggregatedSetRole(acc)
	assert.True(t, ok)
	assert.Equal(t, "decode", role)
	_, err = scaletarget.NewDisaggregatedSetStaticRoleAccessor(set(nil, "", 3), "encode")
	assert.ErrorContains(t, err, "External")
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
