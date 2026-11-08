package scalefromzero

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"

	appsV1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/common"
	utiltest "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	poolreconciler "github.com/llm-d-incubation/llm-d-scaling-manager/internal/controller"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/datastore"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	unittestutil "github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// The wake is the whole product of this engine, and until now nothing exercised
// it end to end: the existing engine tests stop at the plumbing, because the real
// datastore hands out a live pod-scraping source that no unit test can make
// report a queue. These tests substitute that one method, so optimize runs the
// production path — discovery, grouping, candidate build, selection, publication
// — and the assertions are made on what a wake actually produces:
//
//	decision.Set        the actuation KEDA reads to take the target off zero, and
//	decision.MarkActivated  the hold that stops scale-to-zero undoing it before
//	                        the request that caused the wake has been served.
//
// Both were reachable only through the kind e2e, so a regression in either was a
// 400-second feedback loop.

// stubMetricsSource is an EPP metrics source whose scrape result the test
// dictates. Refresh counts are recorded so the "one scrape per model per tick"
// property can be asserted rather than assumed.
type stubMetricsSource struct {
	values    []source.MetricValue
	err       error
	refreshes atomic.Int32
}

func (s *stubMetricsSource) QueryList() *source.QueryList { return nil }

func (s *stubMetricsSource) Refresh(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
	s.refreshes.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	return map[string]*source.MetricResult{
		"all_metrics": {QueryName: "all_metrics", Values: s.values, CollectedAt: time.Now()},
	}, nil
}

func (s *stubMetricsSource) Get(string, map[string]string) *source.CachedValue { return nil }

// stubSourceDatastore is the real datastore with its EPP scrape swapped out. Pool
// registration, label lookup and namespace tracking stay live, so what the test
// changes is only where the queue depth comes from.
type stubSourceDatastore struct {
	datastore.Datastore
	src source.MetricsSource
}

func (d stubSourceDatastore) PoolGetMetricsSource(string) source.MetricsSource { return d.src }

// queueDepth builds the EPP flow-control sample the engine reads as demand.
func queueDepth(metricName, modelID string, value float64) source.MetricValue {
	return source.MetricValue{
		Value:     value,
		Timestamp: time.Now(),
		Labels: map[string]string{
			metricNameLabel:      metricName,
			targetEPPMetricLabel: modelID,
		},
	}
}

// wakeFixture wires one inactive variant behind one pool, with the scrape result
// the caller wants. Names are per-test because the decision store and the
// decision cache are process-wide singletons.
type wakeFixture struct {
	engine *Engine
	src    *stubMetricsSource
	ns     string
	target string
	model  string
	// variant is the name the decision cache is keyed by. Variants are
	// synthesized from the managed HPA, so it is the HPA's name, not the
	// Deployment's.
	variant string
}

// newWakeFixture leaves the scrape result empty; each test sets f.src.values (or
// f.src.err) to the shape it is about.
func newWakeFixture(t *testing.T, name string) wakeFixture {
	t.Helper()

	ns := name + "-ns"
	poolName := name + "-pool"
	eppSvc := "epp-" + name + "-svc"
	hpaName := name + "-hpa"
	target := name + "-deployment"
	model := name + "-model"

	gvk := schema.GroupVersionKind{
		Group:   v1.GroupVersion.Group,
		Version: v1.GroupVersion.Version,
		Kind:    "InferencePool",
	}
	pool := utiltest.MakeInferencePool(poolName).
		Namespace(ns).
		Selector(selector_v1).
		TargetPorts(8080).
		EndpointPickerRef(eppSvc).ObjRef()
	pool.SetGroupVersionKind(gvk)

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha2.Install(scheme))
	require.NoError(t, v1.Install(scheme))
	require.NoError(t, kedav1alpha1.AddToScheme(scheme))
	require.NoError(t, appsV1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	// KEDA calling about this ScaledObject is what makes the workload scaling manager's.
	so := managedSO(ns, hpaName, target, model)
	reg := registry.New(0)
	registerSO(reg, so)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			pool,
			// Parked at zero: that is what makes it this engine's business.
			unittestutil.MakeDeployment(target, ns, 0, selector_v1),
			so,
			unittestutil.MakeService(eppSvc, ns),
		).
		Build()

	namespacedName := types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}
	ds := datastore.NewDatastore(nil)
	reconciler := &poolreconciler.InferencePoolReconciler{
		Client:    fakeClient,
		Datastore: ds,
		PoolGKNN: common.GKNN{
			NamespacedName: namespacedName,
			GroupKind: schema.GroupKind{
				Group: pool.GroupVersionKind().Group,
				Kind:  pool.GroupVersionKind().Kind,
			},
		},
	}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: namespacedName})
	require.NoError(t, err)

	src := &stubMetricsSource{}
	return wakeFixture{
		engine: &Engine{
			client:         fakeClient,
			recorder:       record.NewFakeRecorder(100),
			Datastore:      stubSourceDatastore{Datastore: ds, src: src},
			maxConcurrency: 30,
			Variants:       reg,
			// No gpuLimiter: with no constraints to place against, the capacity
			// check is permissive, which isolates these tests to the wake path.
			// Refusal on a full accelerator is covered by selection_test.go and
			// end to end by test/e2e/scale_from_zero_capacity_test.go.
		},
		src:     src,
		ns:      ns,
		target:  target,
		model:   model,
		variant: hpaName,
	}
}

// woken reports what the two sides of a wake say about this fixture's variant.
func (f wakeFixture) woken() (replicas int32, published bool, held bool) {
	d, published := decision.Get(f.ns, f.target)
	return d.DesiredReplicas, published, decision.WithinActivationRetention(f.ns, f.model, time.Minute)
}

// TestWakePublishesActuationAndRetentionHold is the end-to-end case: a queued
// request for a model parked at zero must produce BOTH halves of a wake. Only the
// first is the actuation; without the second the saturation engine's enforcer sees
// a replica that has served nothing, reads its idle counter as zero, and zeroes it
// straight back out from under the request that asked for it.
func TestWakePublishesActuationAndRetentionHold(t *testing.T) {
	f := newWakeFixture(t, "wake")
	f.src.values = []source.MetricValue{queueDepth(constants.EPPFlowControlQueueSize, f.model, 3)}

	require.NoError(t, f.engine.optimize(context.Background()))

	replicas, published, held := f.woken()
	require.True(t, published, "a queued request must publish an activation for the target")
	assert.Equal(t, int32(1), replicas, "a wake takes the target to exactly one replica")
	assert.True(t, held, "the wake must be marked so scale-to-zero holds off for its retention")

	assert.Equal(t, int32(1), f.src.refreshes.Load(),
		"one EPP scrape per model per tick; every variant of a model shares the queue")
}

// TestNoWakeWithoutQueuedDemand: the engine polls at 10Hz over every parked
// model, so "nothing is queued" is the overwhelmingly common path. It must be
// inert — no actuation, and no retention hold that would later suppress a
// legitimate scale-to-zero.
func TestNoWakeWithoutQueuedDemand(t *testing.T) {
	f := newWakeFixture(t, "quiet")
	f.src.values = []source.MetricValue{queueDepth(constants.EPPFlowControlQueueSize, f.model, 0)}

	require.NoError(t, f.engine.optimize(context.Background()))

	_, published, held := f.woken()
	assert.False(t, published, "an empty queue must not wake anything")
	assert.False(t, held, "nothing was woken, so nothing may be held up")
}

// TestNoWakeForAnotherModelsQueue guards the label match. The flow-control gauge
// is exported per model behind the same EPP, so a busy neighbour must not wake a
// model that has no demand of its own.
func TestNoWakeForAnotherModelsQueue(t *testing.T) {
	f := newWakeFixture(t, "neighbour")
	f.src.values = []source.MetricValue{queueDepth(constants.EPPFlowControlQueueSize, "some-other-model", 7)}

	require.NoError(t, f.engine.optimize(context.Background()))

	_, published, held := f.woken()
	assert.False(t, published, "another model's queue is not this model's demand")
	assert.False(t, held)
}

// TestWakeReadsTheDeprecatedGaugeWhenItIsAllTheEPPExports: llm-d renamed the
// family, and a cluster running an older EPP exports only the old name. Pinning
// either name alone breaks silently on one side of that upgrade.
func TestWakeReadsTheDeprecatedGaugeWhenItIsAllTheEPPExports(t *testing.T) {
	f := newWakeFixture(t, "deprecated")
	f.src.values = []source.MetricValue{queueDepth(constants.SchedulerFlowControlQueueSize, f.model, 2)}

	require.NoError(t, f.engine.optimize(context.Background()))

	_, published, held := f.woken()
	assert.True(t, published, "the deprecated gauge is authoritative when it is the only one exported")
	assert.True(t, held)
}

// TestScrapeFailureSurfacesAndWakesNothing: a failed scrape is not evidence of an
// empty queue. It must be reported so the executor retries, and must not leave a
// half-applied wake behind.
func TestScrapeFailureSurfacesAndWakesNothing(t *testing.T) {
	f := newWakeFixture(t, "scrapefail")
	f.src.err = errors.New("epp unreachable")

	err := f.engine.optimize(context.Background())
	require.Error(t, err, "a failed EPP scrape must surface, not read as an empty queue")

	_, published, held := f.woken()
	assert.False(t, published)
	assert.False(t, held)
}
