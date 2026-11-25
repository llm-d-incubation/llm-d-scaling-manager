package dsetwriter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/passive"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// envNS is the namespace of the envtest's sets.
const envNS = "dset-writer"

// TestWriterOnAnAPIServer runs the writer against a real API server with the
// DisaggregatedSet CRD of LWS v1.0.0 (testdata/crd): its list-map keys, its
// defaults and its all-zero-or-all-non-zero CEL rule are the API server's,
// not a fake client's.
func TestWriterOnAnAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set: this test needs envtest (make setup-envtest)")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("testdata", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err)
	defer func() { _ = env.Stop() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: envNS}}))

	// runWith opts set name in with mode, publishes decided, and runs one
	// pass of a writer that resolves passive mode with p over reg.
	runWith := func(t *testing.T, p PassiveResolver, reg *registry.Registry, name, mode string, annotations map[string]string, decided map[string]int32) *record.FakeRecorder {
		t.Helper()
		store := decision.NewStore()
		for role, n := range decided {
			store.Set(envNS, TargetName(name, role), n)
		}
		rec := record.NewFakeRecorder(10)
		live := get(ctx, t, c, name)
		anns := live.GetAnnotations()
		if anns == nil {
			anns = map[string]string{}
		}
		anns[OptInAnnotation] = mode
		anns[ModelIDAnnotation] = "org/GLM"
		for k, v := range annotations {
			anns[k] = v
		}
		live.SetAnnotations(anns)
		require.NoError(t, c.Update(ctx, live, client.FieldOwner("annotator")))
		w := &Writer{Client: c, Registry: reg, Decisions: store, Recorder: rec, Passive: p, Namespaces: []string{envNS}, ScaleDownWindow: -1}
		w.Pass(ctx)
		return rec
	}
	run := func(t *testing.T, name, mode string, decided map[string]int32) *record.FakeRecorder {
		t.Helper()
		return runWith(t, passiveModels{}, registry.New(0), name, mode, nil, decided)
	}

	t.Run("an apply that names no replicas leaves them to the writer, which changes nothing else", func(t *testing.T) {
		const name = "applied"
		gitops := envSet(name, nil)
		require.NoError(t, c.Apply(ctx, client.ApplyConfigurationFromUnstructured(gitops), client.FieldOwner("gitops")))
		before := get(ctx, t, c, name)
		require.Equal(t, map[string]int64{"prefill": 1, "decode": 1, "encode": 1}, replicasOf(t, before), "the API default")

		run(t, name, "true", map[string]int32{"decode": 5, "encode": 4})

		after := get(ctx, t, c, name)
		assert.Equal(t, map[string]int64{"prefill": 1, "decode": 5, "encode": 1}, replicasOf(t, after),
			"decode moves; prefill keeps its count; the External encode is never written")
		assert.Equal(t, specWithoutReplicas(t, before), specWithoutReplicas(t, after),
			"nothing but replicas changed, not even the order of the roles")
		assert.Equal(t, []string{
			"f:spec/f:roles/k:{\"name\":\"decode\"}/f:name",
			"f:spec/f:roles/k:{\"name\":\"decode\"}/f:spec/f:replicas",
			"f:spec/f:roles/k:{\"name\":\"prefill\"}/f:name",
			"f:spec/f:roles/k:{\"name\":\"prefill\"}/f:spec/f:replicas",
		}, ownedLeaves(t, after, FieldManager), "the writer owns the replicas of the Static roles, and their keys, and nothing else")

		// The source of truth applies again, still without replicas: it
		// neither conflicts nor resets the writer's count.
		require.NoError(t, c.Apply(ctx, client.ApplyConfigurationFromUnstructured(envSet(name, nil)), client.FieldOwner("gitops")))
		assert.Equal(t, int64(5), replicasOf(t, get(ctx, t, c, name))["decode"])
	})

	t.Run("a manager that set replicas keeps them unless forced", func(t *testing.T) {
		const name = "created"
		created := envSet(name, map[string]int64{"prefill": 2, "decode": 2})
		require.NoError(t, c.Create(ctx, created, client.FieldOwner("kubectl-create")))

		rec := run(t, name, "true", map[string]int32{"decode": 5})
		assert.Equal(t, int64(2), replicasOf(t, get(ctx, t, c, name))["decode"], "a conflict writes nothing")
		assert.True(t, hasEvent(rec, EventConflict), "the conflict is reported on the set")

		run(t, name, "force", map[string]int32{"decode": 5})
		after := get(ctx, t, c, name)
		assert.Equal(t, map[string]int64{"prefill": 2, "decode": 5, "encode": 1}, replicasOf(t, after))
		assert.NotContains(t, ownedLeaves(t, after, "kubectl-create"), "f:spec/f:roles/k:{\"name\":\"decode\"}/f:spec/f:replicas",
			"force took the field over")
	})

	// A create or client-side apply (kubectl, Helm, Argo CD without
	// ServerSideApply) is an update: its manager owns every field of the
	// stored object, the API defaults included. Leaving replicas out of the
	// manifest does not leave them to the writer; only force does.
	t.Run("a client-side create owns even the replicas it left to the API default", func(t *testing.T) {
		const name = "defaulted"
		require.NoError(t, c.Create(ctx, envSet(name, nil), client.FieldOwner("kubectl-create")))
		rec := run(t, name, "true", map[string]int32{"decode": 5})
		after := get(ctx, t, c, name)
		assert.Equal(t, int64(1), replicasOf(t, after)["decode"], "a conflict writes nothing")
		assert.True(t, hasEvent(rec, EventConflict), "the conflict is reported on the set")
		assert.Contains(t, ownedLeaves(t, after, "kubectl-create"), "f:spec/f:roles/k:{\"name\":\"decode\"}/f:spec/f:replicas")
	})

	t.Run("one Static role at zero is rejected by the API; the writer never asks for it", func(t *testing.T) {
		const name = "zero"
		require.NoError(t, c.Apply(ctx, client.ApplyConfigurationFromUnstructured(envSet(name, nil)), client.FieldOwner("gitops")))

		lone := ApplyConfiguration(get(ctx, t, c, name), map[string]int32{"prefill": 0, "decode": 3})
		err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(lone), client.FieldOwner("someone"))
		require.True(t, apierrors.IsInvalid(err), "the CEL rule must reject a lone zero: %v", err)

		run(t, name, "true", map[string]int32{"prefill": 0, "decode": 3})
		assert.Equal(t, map[string]int64{"prefill": 1, "decode": 3, "encode": 1}, replicasOf(t, get(ctx, t, c, name)),
			"prefill decided to zero alone keeps a group")

		run(t, name, "true", map[string]int32{"prefill": 0, "decode": 0})
		assert.Equal(t, map[string]int64{"prefill": 0, "decode": 0, "encode": 1}, replicasOf(t, get(ctx, t, c, name)),
			"every Static role parked together is accepted")
	})

	// Passive mode, resolved by the controller's own resolver from the
	// controller-wide flag and the scaling policy, as cmd/main.go wires it.
	// writesNothing runs two passes with decisions for both Static roles and
	// checks the set: the first may only add the annotations, the second must
	// leave the object exactly as it was.
	writesNothing := func(t *testing.T, p PassiveResolver, reg *registry.Registry, name string, anns map[string]string) {
		t.Helper()
		decided := map[string]int32{"prefill": 3, "decode": 5}
		before := get(ctx, t, c, name)
		runWith(t, p, reg, name, "force", anns, decided)
		first := get(ctx, t, c, name)
		assert.Equal(t, replicasOf(t, before), replicasOf(t, first), "a passive model's replicas are not written")
		assert.Equal(t, before.GetGeneration(), first.GetGeneration(), "nor anything else in its spec")
		assert.Empty(t, ownedLeaves(t, first, FieldManager), "the writer owns no field of a passive model's set")

		runWith(t, p, reg, name, "force", anns, decided)
		second := get(ctx, t, c, name)
		assert.Equal(t, first.GetResourceVersion(), second.GetResourceVersion(), "a second pass does not touch the set at all")
		assert.Equal(t, first.GetManagedFields(), second.GetManagedFields())
	}
	writes := func(t *testing.T, p PassiveResolver, reg *registry.Registry, name string, anns map[string]string) {
		t.Helper()
		runWith(t, p, reg, name, "force", anns, map[string]int32{"prefill": 3, "decode": 5})
		after := get(ctx, t, c, name)
		assert.Equal(t, map[string]int64{"prefill": 3, "decode": 5, "encode": 1}, replicasOf(t, after), "an active model is written")
		assert.NotEmpty(t, ownedLeaves(t, after, FieldManager))
	}

	t.Run("passive controller-wide writes nothing until a policy tier sets passive: false", func(t *testing.T) {
		const name = "passive-flag"
		require.NoError(t, c.Apply(ctx, client.ApplyConfigurationFromUnstructured(envSet(name, nil)), client.FieldOwner("gitops")))
		cfg := config.NewTestConfig()
		config.SetPassiveDefaultForTest(cfg, true)
		reg := registry.New(0)
		p := &passive.Resolver{Config: cfg, Registry: reg}
		// A model of its own: the sets of the subtests above serve org/GLM
		// under no tier, and a model is passive when any variant resolves so.
		gold := map[string]string{ModelIDAnnotation: "org/flag", constants.ScalingPolicyAnnotation: "gold"}

		writesNothing(t, p, reg, name, gold)

		f := false
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{"gold": {Passive: &f}})
		writes(t, p, reg, name, gold)
	})

	t.Run("a per-model passive entry writes nothing; passive: false for the model writes", func(t *testing.T) {
		const name = "passive-model"
		require.NoError(t, c.Apply(ctx, client.ApplyConfigurationFromUnstructured(envSet(name, nil)), client.FieldOwner("gitops")))
		cfg := config.NewTestConfig()
		reg := registry.New(0)
		p := &passive.Resolver{Config: cfg, Registry: reg}
		perModel := func(v bool) map[string]config.ScalingPolicy {
			return map[string]config.ScalingPolicy{"per-model": {ModelID: "org/per-model", Namespace: envNS, Passive: &v}}
		}
		model := map[string]string{ModelIDAnnotation: "org/per-model"}

		cfg.UpdateScalingPolicyConfig(perModel(true))
		writesNothing(t, p, reg, name, model)

		config.SetPassiveDefaultForTest(cfg, true)
		cfg.UpdateScalingPolicyConfig(perModel(false))
		writes(t, p, reg, name, model)
	})
}

// envSet is a DisaggregatedSet in the test namespace with Static prefill and
// decode and an External encode, at the given Static counts (none: unset).
func envSet(name string, replicas map[string]int64) *unstructured.Unstructured {
	role := func(role, mode string) map[string]any {
		spec := map[string]any{"leaderWorkerTemplate": map[string]any{
			"size": int64(2),
			"workerTemplate": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"llm-d.ai/role": role}},
				"spec": map[string]any{"containers": []any{map[string]any{
					"name": "engine", "image": "registry.k8s.io/pause:3.9",
				}}},
			},
		}}
		if n, ok := replicas[role]; ok {
			spec["replicas"] = n
		}
		r := map[string]any{"name": role, "spec": spec}
		if mode != "" {
			r["scaling"] = map[string]any{"mode": mode}
		}
		return r
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.DisaggregatedSetAPIVersion,
		"kind":       constants.DisaggregatedSetKind,
		"metadata":   map[string]any{"name": name, "namespace": envNS},
		// Not in name order, and an External role between the Static ones:
		// the writer's apply must not reorder them.
		"spec": map[string]any{"roles": []any{
			role("prefill", ""),
			role("encode", "External"),
			role("decode", ""),
		}},
	}}
}

func get(ctx context.Context, t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(setGVK)
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: envNS, Name: name}, u))
	return u
}

// replicasOf is spec.roles[].spec.replicas of set, by role.
func replicasOf(t *testing.T, set *unstructured.Unstructured) map[string]int64 {
	t.Helper()
	roles, _, err := unstructured.NestedSlice(set.Object, "spec", "roles")
	require.NoError(t, err)
	out := map[string]int64{}
	for _, r := range roles {
		m := r.(map[string]any)
		n, _, err := unstructured.NestedInt64(m, "spec", "replicas")
		require.NoError(t, err)
		out[m["name"].(string)] = n
	}
	return out
}

// specWithoutReplicas is set's spec with every role's replicas removed.
func specWithoutReplicas(t *testing.T, set *unstructured.Unstructured) map[string]any {
	t.Helper()
	cp := set.DeepCopy()
	roles, _, err := unstructured.NestedSlice(cp.Object, "spec", "roles")
	require.NoError(t, err)
	for _, r := range roles {
		unstructured.RemoveNestedField(r.(map[string]any), "spec", "replicas")
	}
	require.NoError(t, unstructured.SetNestedSlice(cp.Object, roles, "spec", "roles"))
	spec, _, _ := unstructured.NestedMap(cp.Object, "spec")
	return spec
}

// ownedLeaves lists the leaf fields manager owns on set, as slash-joined
// managedFields paths, sorted. A list-map item's own "." entry is not a leaf.
func ownedLeaves(t *testing.T, set *unstructured.Unstructured, manager string) []string {
	t.Helper()
	var out []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			if k == "." {
				continue
			}
			p := k
			if prefix != "" {
				p = prefix + "/" + k
			}
			if child, _ := v.(map[string]any); len(child) > 0 {
				walk(p, child)
				continue
			}
			out = append(out, p)
		}
	}
	for _, mf := range set.GetManagedFields() {
		if mf.Manager != manager || mf.FieldsV1 == nil {
			continue
		}
		var f map[string]any
		require.NoError(t, json.Unmarshal(mf.FieldsV1.GetRawBytes(), &f))
		walk("", f)
	}
	slices.Sort(out)
	return out
}

func hasEvent(rec *record.FakeRecorder, reason string) bool {
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, reason) {
				return true
			}
		default:
			return false
		}
	}
}
