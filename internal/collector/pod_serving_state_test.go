package collector

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

func servingPod(name string, ready bool, terminating bool) *corev1.Pod {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}},
		},
	}
	if terminating {
		now := metav1.Now()
		p.DeletionTimestamp = &now
		p.Finalizers = []string{"test/hold"} // the fake client requires one to keep it
	}
	return p
}

func collectorWithPods(t *testing.T, pods ...*corev1.Pod) *ReplicaMetricsCollector {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	objs := make([]client.Object, 0, len(pods))
	for _, p := range pods {
		objs = append(objs, p)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	c := NewReplicaMetricsCollector(nil, nil, reader, nil, nil)
	c.BeginCycle()
	return c
}

// A Pod the listing does not contain is GONE, and its series must not be counted.
//
// Prometheus keeps a Pod's series for about five minutes after it goes, and the
// locator resolves a name to its scale target from a cache that is never
// invalidated -- so without this the capacity of a deleted replica goes on
// being counted long after the replica does not exist. Measured on the shared GPU cluster: a
// variant with one replica reported four.
func TestAPodMissingFromTheListingIsGone(t *testing.T) {
	c := collectorWithPods(t, servingPod("alive", true, false))

	if !c.podIsGone(context.Background(), "tenant", "vanished") {
		t.Error("a Pod absent from a good listing must be treated as gone")
	}
	if c.podIsGone(context.Background(), "tenant", "alive") {
		t.Error("a Pod present and not terminating must be kept")
	}
}

// A TERMINATING Pod is gone too. Its capacity is on its way out, and counting it
// holds the fleet up on supply that is about to vanish.
func TestATerminatingPodIsGone(t *testing.T) {
	c := collectorWithPods(t, servingPod("draining", true, true))

	if !c.podIsGone(context.Background(), "tenant", "draining") {
		t.Error("a Pod with a DeletionTimestamp must be treated as gone")
	}
}

// READY is read from the Pod, not inferred from the fact that it answered a
// scrape. An engine serves /metrics before it passes readiness, so a starting
// replica reports while nothing routes to it.
func TestReadinessComesFromThePodNotTheScrape(t *testing.T) {
	c := collectorWithPods(t,
		servingPod("in-rotation", true, false),
		servingPod("starting", false, false),
	)
	ctx := context.Background()

	if !c.podReady(ctx, "tenant", "in-rotation") {
		t.Error("a Ready Pod must read ready")
	}
	if c.podReady(ctx, "tenant", "starting") {
		t.Error("a Pod that has not passed its readiness probe must not read ready")
	}
	// Still kept as capacity: it holds its GPU and its KV cache is real.
	if c.podIsGone(ctx, "tenant", "starting") {
		t.Error("a starting Pod is not gone; it is capacity that is not yet in the rotation")
	}
}

// failingReader is a Reader whose List always fails.
type failingReader struct{ client.Reader }

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("the API server said no")
}

// A LISTING THAT FAILED MEANS UNKNOWN, NOT EMPTY.
//
// This is the dangerous direction and the reason the two are kept apart. If a
// failed List were read as "no Pod exists", every series in the namespace would
// be dropped, the analyzer would be handed zero supply, and an RBAC error or a
// moment of API unavailability would scale the fleet to its floor.
//
// So both helpers fail OPEN, and the behaviour under failure is exactly what it
// was before Pod state was consulted at all.
func TestAFailedListingKeepsEverything(t *testing.T) {
	c := NewReplicaMetricsCollector(nil, nil, failingReader{}, nil, nil)
	c.BeginCycle()
	ctx := context.Background()

	if c.podIsGone(ctx, "tenant", "anything") {
		t.Error("a failed listing must not be read as the Pod being gone")
	}
	if !c.podReady(ctx, "tenant", "anything") {
		t.Error("a failed listing must not publish every Pod as unready: zero is a " +
			"load-bearing answer meaning the replicas are demonstrably not serving")
	}
}

// forbiddenReader is a Reader whose List fails as an RBAC refusal does.
type forbiddenReader struct{ client.Reader }

func (forbiddenReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no list"))
}

// A failed listing is not silent. Every Pod reads Ready on it, which lets a
// not-Ready Pod's saturated reading become its variant's k2 -- so it is
// counted, once per namespace per cycle, with the reason it failed.
func TestAFailedListingIsCounted(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	count := func(reason string) float64 {
		families, err := registry.Gather()
		require.NoError(t, err)
		for _, f := range families {
			if f.GetName() != constants.LLMSMMetricsCollectionErrorsTotal {
				continue
			}
			for _, m := range f.GetMetric() {
				labels := map[string]string{}
				for _, l := range m.GetLabel() {
					labels[l.GetName()] = l.GetValue()
				}
				if labels[constants.LabelQueryType] == constants.QueryTypePodList && labels[constants.LabelReason] == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
		return 0
	}

	c := NewReplicaMetricsCollector(nil, nil, forbiddenReader{}, nil, nil)
	c.BeginCycle()
	ctx := context.Background()
	for range 3 {
		assert.True(t, c.podReady(ctx, "tenant", "anything"), "still fails open")
	}
	assert.InDelta(t, 1, count("forbidden"), 0, "one listing per namespace per cycle, one count")

	c = NewReplicaMetricsCollector(nil, nil, failingReader{}, nil, nil)
	c.BeginCycle()
	c.podReady(ctx, "tenant", "anything")
	assert.InDelta(t, 1, count("other"), 0)
}

// With no reader wired at all, nothing is filtered. Keeps every existing caller
// -- and every test that constructs a collector without one -- on the behaviour
// it had before.
func TestWithoutAReaderNothingIsFiltered(t *testing.T) {
	c := NewReplicaMetricsCollector(nil, nil, nil, nil, nil)
	c.BeginCycle()
	ctx := context.Background()

	if c.podIsGone(ctx, "tenant", "anything") {
		t.Error("no reader means no opinion, so nothing may be dropped")
	}
	if !c.podReady(ctx, "tenant", "anything") {
		t.Error("no reader means no opinion, so readiness must not be denied")
	}
}

// The namespace is listed ONCE per cycle, not once per series.
//
// Every series of every query passes through the builder, so a per-series read
// would be dozens of API calls a cycle. The memo is what makes putting the check
// there affordable.
func TestTheNamespaceIsListedOncePerCycle(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	counting := &countingReader{Reader: fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(servingPod("alive", true, false)).Build()}

	c := NewReplicaMetricsCollector(nil, nil, counting, nil, nil)
	c.BeginCycle()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		c.podIsGone(ctx, "tenant", "alive")
		c.podReady(ctx, "tenant", "alive")
	}
	if counting.lists != 1 {
		t.Errorf("listed %d times in one cycle, want 1", counting.lists)
	}

	// A new cycle re-reads: Pod state is mutable and must not be carried over.
	c.EndCycle()
	c.BeginCycle()
	c.podIsGone(ctx, "tenant", "alive")
	if counting.lists != 2 {
		t.Errorf("listed %d times across two cycles, want 2 -- state must not survive a cycle",
			counting.lists)
	}
}

type countingReader struct {
	client.Reader
	lists int
}

func (r *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.lists++
	return r.Reader.List(ctx, list, opts...)
}
