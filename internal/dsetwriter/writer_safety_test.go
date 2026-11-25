package dsetwriter

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// passiveModels is a PassiveResolver that answers passive for the models it
// lists.
type passiveModels map[string]bool

func (p passiveModels) Model(_, modelID string) (bool, string) { return p[modelID], "test" }

func rolesOf(doc map[string]any) []any {
	roles, _, _ := unstructured.NestedSlice(doc, "spec", "roles")
	return roles
}

func drain(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func countReason(events []string, reason string) int {
	n := 0
	for _, ev := range events {
		if strings.Contains(ev, " "+reason+" ") {
			n++
		}
	}
	return n
}

// A passive model is observed, never scaled: the external scaler holds its
// count, and the writer, the other path to the same replicas, must too. Its
// roles stay registered, so the engines keep publishing would-be counts.
func TestPassWritesNothingForAPassiveModel(t *testing.T) {
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 5)
	w, got, _ := newWriter(t, set(optIn("true"), "", 3), store, nil)
	w.Passive = passiveModels{"org/GLM": true}
	w.Pass(context.Background())
	assert.Equal(t, 0, got.calls, "a passive model's roles are not written")
	assert.Equal(t, 2, w.Registry.Len(), "but they are registered")

	w.Passive = passiveModels{"org/other": true}
	w.Pass(context.Background())
	assert.Equal(t, 1, got.calls, "passive is per model: an active one is written")
}

// Without a resolver the writer cannot tell a passive model from an active
// one, so it writes none: failing open here scaled a passive model. The roles
// are still registered, and the missing resolver is logged once, at error.
func TestPassWritesNothingWithoutAPassiveResolver(t *testing.T) {
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 5)
	w, got, _ := newWriter(t, set(optIn("force"), "", 3), store, nil)
	w.Passive = nil
	logs := &logLines{}
	ctx := logr.NewContext(context.Background(), logs.logger())
	w.Pass(ctx)
	w.Pass(ctx)
	assert.Equal(t, 0, got.calls, "no resolver writes nothing")
	assert.Equal(t, 2, w.Registry.Len(), "but the roles are registered")
	assert.Equal(t, 1, logs.count("no passive resolver"), "logged once: %v", logs.lines)
	assert.Equal(t, 1, logs.count(`"error"=`), "at error: %v", logs.lines)
}

// withCounts sets the replicas of the prefill and decode roles of s.
func withCounts(t *testing.T, s *unstructured.Unstructured, prefill, decode int64) *unstructured.Unstructured {
	t.Helper()
	roles := rolesOf(s.Object)
	for _, r := range roles {
		m := r.(map[string]any)
		switch m["name"] {
		case "prefill":
			require.NoError(t, unstructured.SetNestedField(m, prefill, "spec", "replicas"))
		case "decode":
			require.NoError(t, unstructured.SetNestedField(m, decode, "spec", "replicas"))
		}
	}
	require.NoError(t, unstructured.SetNestedSlice(s.Object, roles, "spec", "roles"))
	return s
}

// A passive model is never woken or healed. Plan lifts a Static role parked
// alone back to a group, with or without a decision, and a scale-from-zero
// wake publishes a decision for both roles of a set parked at zero; both are
// writes, and a passive model gets neither. Each case runs active first, the
// control that shows the same set and decisions would be written.
func TestPassNeverWakesOrHealsAPassiveModel(t *testing.T) {
	for _, tc := range []struct {
		name            string
		prefill, decode int64
		decided         map[string]int32
	}{
		{name: "prefill parked alone, no decision (Plan heals it)", prefill: 0, decode: 3},
		{name: "prefill parked alone, a decision to wake it", prefill: 0, decode: 3, decided: map[string]int32{"prefill": 2}},
		{name: "both parked, a scale-from-zero wake for both", prefill: 0, decode: 0, decided: map[string]int32{"prefill": 1, "decode": 1}},
		{name: "both parked, a wake for one role", prefill: 0, decode: 0, decided: map[string]int32{"decode": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, passive := range []bool{false, true} {
				store := decision.NewStore()
				for role, n := range tc.decided {
					store.Set(ns, TargetName("glm", role), n)
				}
				w, got, _ := newWriter(t, withCounts(t, set(optIn("force"), "", 3), tc.prefill, tc.decode), store, nil)
				w.Passive = passiveModels{"org/GLM": passive}
				w.Pass(context.Background())
				if !passive {
					require.Equal(t, 1, got.calls, "control: the active model is written")
					continue
				}
				assert.Equal(t, 0, got.calls, "a passive model is written nothing")
			}
		})
	}
}

// A namespace-scoped install with no readable namespace has nothing to scan.
// It must not fall back to every namespace: that is a cluster-wide list and
// patch the install was never granted.
func TestPassScansOnlyItsNamespaces(t *testing.T) {
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 5)
	for _, tc := range []struct {
		name       string
		namespaces []string
		all        bool
		wantLists  []string
		wantCalls  int
	}{
		{name: "none, not all", wantLists: nil},
		{name: "another namespace only", namespaces: []string{"other"}, wantLists: []string{"other"}},
		{name: "its namespace", namespaces: []string{"other", ns}, wantLists: []string{"other", ns}, wantCalls: 1},
		{name: "every namespace", all: true, wantLists: []string{""}, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lists []string
			w, got, _ := newWriter(t, set(optIn("true"), "", 3), store, nil)
			w.Client = interceptor.NewClient(w.Client.(client.WithWatch), interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					in := (&client.ListOptions{}).ApplyOptions(opts).Namespace
					if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind() == lwsListGVK {
						// The children of a set it scales, in the set's namespace only.
						assert.Equal(t, ns, in)
						return c.List(ctx, list, opts...)
					}
					lists = append(lists, in)
					return c.List(ctx, list, opts...)
				},
				Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
					got.calls++
					return nil
				},
			})
			w.Namespaces, w.AllNamespaces = tc.namespaces, tc.all
			w.Pass(context.Background())
			assert.Equal(t, tc.wantLists, lists)
			assert.Equal(t, tc.wantCalls, got.calls)
		})
	}
}

// Bad bounds are refused on the set, once, and nothing is registered or
// written: a default in their place would scale to a count nobody chose.
func TestPassBounds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		min, max string // "-" leaves the annotation out
		decided  int32
		want     int64 // the decode count written; 0 means refused
		min0     int32
		max0     int32
	}{
		{name: "defaults", min: "-", max: "-", decided: 40, want: 10, min0: 1, max0: 10},
		{name: "a minimum above the decision", min: "3", max: "8", decided: 1, want: 3, min0: 3, max0: 8},
		{name: "minimum 0 is allowed", min: "0", max: "8", decided: 6, want: 6, min0: 0, max0: 8},
		{name: "empty maximum", min: "-", max: ""},
		{name: "maximum not a number", min: "-", max: "abc"},
		{name: "negative minimum", min: "-1", max: "8"},
		{name: "maximum 0", min: "0", max: "0"},
		{name: "minimum above maximum", min: "5", max: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			anns := map[string]any{OptInAnnotation: "true", ModelIDAnnotation: "org/GLM"}
			if tc.min != "-" {
				anns[constants.MinReplicasAnnotation] = tc.min
			}
			if tc.max != "-" {
				anns[constants.MaxReplicasAnnotation] = tc.max
			}
			store := decision.NewStore()
			store.Set(ns, "glm-decode", tc.decided)
			w, got, rec := newWriter(t, set(anns, "", 4), store, nil)
			w.Pass(context.Background())
			events := drain(rec)
			if tc.want == 0 {
				assert.Equal(t, 0, got.calls)
				assert.Equal(t, 0, w.Registry.Len(), "a refused set is not sized")
				assert.Equal(t, 1, countReason(events, EventRefused), "%v", events)
				return
			}
			require.Equal(t, 1, got.calls)
			assert.Contains(t, rolesOf(got.doc), map[string]any{"name": "decode", "spec": map[string]any{"replicas": tc.want}})
			e, ok := w.Registry.FindByScaleTarget(ns, constants.DisaggregatedSetRoleScalerKind, "glm-decode")
			require.True(t, ok)
			assert.Equal(t, tc.min0, *e.Target.MinReplicas)
			assert.Equal(t, tc.max0, *e.Target.MaxReplicas)
		})
	}
}

// Waking a parked model: every Static role is at zero and the engines decide
// one of them up. The role with no decision cannot stay at zero alone.
func TestPlanWakesFromAllZero(t *testing.T) {
	parked := []Role{{Name: "prefill"}, {Name: "decode"}}
	assert.Equal(t, map[string]int32{"prefill": 1, "decode": 4}, Plan(parked, map[string]int32{"decode": 4}, 1, 10))
	assert.Equal(t, map[string]int32{"prefill": 2, "decode": 4}, Plan(parked, map[string]int32{"decode": 4}, 2, 10))
	assert.Equal(t, map[string]int32{"prefill": 0, "decode": 0}, Plan(parked, nil, 1, 10), "no decision: stays parked")
	// A lone zero the set should never hold (its CEL rule forbids it; a role
	// switched from External can leave one) is raised with the next write.
	assert.Equal(t, map[string]int32{"prefill": 1, "decode": 3},
		Plan([]Role{{Name: "prefill"}, {Name: "decode", Current: 3}}, nil, 1, 10))
}

func TestPassRecordsWhatItWritesAndRegisters(t *testing.T) {
	anns := optIn("true")
	anns[constants.ScalingPolicyAnnotation] = "premium"
	s := set(anns, "", 3)
	s.SetLabels(map[string]string{"app": "glm"})
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 5)
	w, got, rec := newWriter(t, s, store, nil)
	w.Pass(context.Background())
	require.Equal(t, 1, got.calls)
	events := drain(rec)
	require.Equal(t, 1, countReason(events, EventScaled), "%v", events)
	assert.Contains(t, events[0], "decode:5")

	e, ok := w.Registry.FindByScaleTarget(ns, constants.DisaggregatedSetRoleScalerKind, "glm-decode")
	require.True(t, ok)
	assert.Equal(t, "premium", e.Metadata[registry.ScalingPolicyKey])
	assert.Equal(t, map[string]string{"app": "glm"}, e.Target.Labels)
}

// A set that stays refused or in conflict is told once, not every 15 s; the
// warning is news again after a pass that went right.
func TestPassWarnsOnceUntilSomethingChanges(t *testing.T) {
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 5)
	conflict := apierrors.NewConflict(schema.GroupResource{Group: setGVK.Group, Resource: "disaggregatedsets"}, "glm", assertErr("conflict"))
	w, got, rec := newWriter(t, set(optIn("true"), "", 3), store, conflict)
	for range 3 {
		w.Pass(context.Background())
	}
	assert.Equal(t, 3, got.calls, "the write is retried every pass")
	assert.Equal(t, 1, countReason(drain(rec), EventConflict), "but reported once")

	// The owner lets go: one write succeeds, the next conflict is news.
	w.Client = interceptor.NewClient(w.Client.(client.WithWatch), interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return nil
		},
	})
	w.Pass(context.Background())
	assert.Equal(t, 1, countReason(drain(rec), EventScaled))
	w.Client = interceptor.NewClient(w.Client.(client.WithWatch), interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return conflict
		},
	})
	w.Pass(context.Background())
	assert.Equal(t, 1, countReason(drain(rec), EventConflict))

	// A refusal, likewise.
	w, _, rec = newWriter(t, set(map[string]any{OptInAnnotation: "true"}, "", 3), store, nil)
	w.Pass(context.Background())
	w.Pass(context.Background())
	assert.Equal(t, 1, countReason(drain(rec), EventRefused))
}

// logLines is a logr sink that keeps every line.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, prefix+" "+args)
	}, funcr.Options{})
}

func (l *logLines) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// A list that fails is logged once per kind of failure, not every pass:
// a missing CRD is the common case and not an error; a missing grant is.
func TestPassListFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"no CRD", &meta.NoKindMatchError{GroupKind: setGVK.GroupKind(), SearchedVersions: []string{"v1"}}, "CRD is not installed"},
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Group: setGVK.Group, Resource: "disaggregatedsets"}, "", assertErr("no grant")), "smt admin-setup -n <namespace> --dset-role-writer"},
		{"other", apierrors.NewServiceUnavailable("down"), "Could not list DisaggregatedSets"},
		// A plain error has no API reason; it is logged all the same.
		{"plain", assertErr("connection refused"), "Could not list DisaggregatedSets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, got, _ := newWriter(t, set(optIn("true"), "", 3), decision.NewStore(), nil)
			fail := true
			w.Client = interceptor.NewClient(w.Client.(client.WithWatch), interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if fail {
						return tc.err
					}
					return c.List(ctx, list, opts...)
				},
			})
			logs := &logLines{}
			ctx := logr.NewContext(context.Background(), logs.logger())
			w.Pass(ctx)
			w.Pass(ctx)
			assert.Equal(t, 1, logs.count(tc.want), "%v", logs.lines)
			assert.Equal(t, 0, got.calls)

			// It recovers, and a later failure is news again.
			fail = false
			w.Pass(ctx)
			fail = true
			w.Pass(ctx)
			assert.Equal(t, 2, logs.count(tc.want), "%v", logs.lines)
		})
	}
}

func TestPassSkipsASetBeingDeleted(t *testing.T) {
	s := set(optIn("true"), "", 3)
	now := metav1.Now()
	s.SetDeletionTimestamp(&now)
	s.SetFinalizers([]string{"example.com/hold"})
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 5)
	w, got, _ := newWriter(t, s, store, nil)
	w.Pass(context.Background())
	assert.Equal(t, 0, got.calls)
	assert.Equal(t, 0, w.Registry.Len())
}

func TestStartPassesAtOnceAndStopsOnCancel(t *testing.T) {
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 5)
	w, _, rec := newWriter(t, set(optIn("true"), "", 3), store, nil)
	w.Interval = time.Hour
	assert.True(t, w.NeedLeaderElection())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Start(ctx) }()
	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, EventScaled, "the first pass runs at once, not after an Interval")
	case <-time.After(10 * time.Second):
		t.Fatal("no pass ran")
	}
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return on cancel")
	}
}

// A scale-down waits out the window, as the HPA in front of a KEDA-scaled
// role makes it; a scale-up does not wait.
func TestPassHoldsScaleDownForTheWindow(t *testing.T) {
	store := decision.NewStore()
	t0 := time.Now()
	clock := t0
	w, got, _ := newWriter(t, set(optIn("true"), "", 3), store, nil)
	w.ScaleDownWindow = 0 // the default: the HPA's 5 minutes
	w.DecisionTTL = time.Hour
	w.Now = func() time.Time { return clock }

	store.Set(ns, "glm-decode", 1)
	w.Pass(context.Background())
	assert.Equal(t, 0, got.calls, "the first sight counts the current 3 as planned now")

	clock = t0.Add(DefaultScaleDownWindow - time.Second)
	w.Pass(context.Background())
	assert.Equal(t, 0, got.calls, "inside the window")

	clock = t0.Add(DefaultScaleDownWindow + time.Second)
	w.Pass(context.Background())
	require.Equal(t, 1, got.calls, "past the window")
	assert.Contains(t, rolesOf(got.doc), map[string]any{"name": "decode", "spec": map[string]any{"replicas": int64(1)}})

	// A scale-up lands at once, and a scale-down right after it waits again.
	w, got, _ = newWriter(t, set(optIn("true"), "", 3), store, nil)
	w.ScaleDownWindow, w.DecisionTTL, clock = 0, time.Hour, t0
	w.Now = func() time.Time { return clock }
	store.Set(ns, "glm-decode", 6)
	w.Pass(context.Background())
	require.Equal(t, 1, got.calls, "up at once")
	store.Set(ns, "glm-decode", 2)
	clock = t0.Add(time.Minute)
	// The set the fake holds is unchanged at 3 (the apply is intercepted), so
	// read the plan: 2 is held at the highest planned in the window, 6, and
	// never above the current count.
	w.Pass(context.Background())
	assert.Equal(t, 1, got.calls, "the drop to 2 is held")
}

// Parking waits out the window too, and the roles still go to zero together.
func TestPassParksAfterTheWindow(t *testing.T) {
	store := decision.NewStore()
	store.Set(ns, "glm-decode", 0)
	store.Set(ns, "glm-prefill", 0)
	t0 := time.Now()
	clock := t0
	w, got, _ := newWriter(t, set(optIn("true"), "", 3), store, nil)
	w.ScaleDownWindow = 0
	w.Now = func() time.Time { return clock }
	w.Pass(context.Background())
	assert.Equal(t, 0, got.calls)
	clock = t0.Add(DefaultScaleDownWindow + time.Second)
	w.Pass(context.Background())
	require.Equal(t, 1, got.calls)
	assert.Equal(t, []any{
		map[string]any{"name": "prefill", "spec": map[string]any{"replicas": int64(0)}},
		map[string]any{"name": "decode", "spec": map[string]any{"replicas": int64(0)}},
	}, rolesOf(got.doc))
}
