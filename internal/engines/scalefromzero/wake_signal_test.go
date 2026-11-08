package scalefromzero

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	poolutil "github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/pool"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// --- doubles -----------------------------------------------------------------

// wakeSource is an EPP metrics source that returns a fixed sample set and counts
// how many times it was scraped, so the throttle can be asserted rather than
// assumed.
type wakeSource struct {
	values  []source.MetricValue
	err     error
	scrapes int
}

func (s *wakeSource) QueryList() *source.QueryList { return nil }
func (s *wakeSource) Get(string, map[string]string) *source.CachedValue {
	return nil
}
func (s *wakeSource) Refresh(context.Context, source.RefreshSpec) (map[string]*source.MetricResult, error) {
	s.scrapes++
	if s.err != nil {
		return nil, s.err
	}
	return map[string]*source.MetricResult{"all_metrics": {Values: s.values}}, nil
}

// wakeDatastore resolves every variant to one pool and hands back one source.
type wakeDatastore struct {
	pool   *poolutil.EndpointPool
	src    source.MetricsSource
	noPool bool
	// poolLookups counts label-based pool resolutions, which scan the store.
	poolLookups int
}

func (d *wakeDatastore) PoolGetFromLabels(string, map[string]string) (*poolutil.EndpointPool, error) {
	d.poolLookups++
	if d.noPool {
		return nil, assertErrNotSynced
	}
	return d.pool, nil
}
func (d *wakeDatastore) PoolGetMetricsSource(string) source.MetricsSource { return d.src }
func (d *wakeDatastore) PoolGet(string) (*poolutil.EndpointPool, error)   { return d.pool, nil }
func (d *wakeDatastore) PoolList() []*poolutil.EndpointPool               { return []*poolutil.EndpointPool{d.pool} }
func (d *wakeDatastore) PoolSet(context.Context, client.Client, *poolutil.EndpointPool) error {
	return nil
}
func (d *wakeDatastore) PoolDelete(string)               {}
func (d *wakeDatastore) Clear()                          {}
func (d *wakeDatastore) NamespaceTrack(_, _, _ string)   {}
func (d *wakeDatastore) NamespaceUntrack(_, _, _ string) {}
func (d *wakeDatastore) IsNamespaceTracked(string) bool  { return true }
func (d *wakeDatastore) ListTrackedNamespaces() []string { return nil }

var assertErrNotSynced = errPoolNotSyncedForTest{}

type errPoolNotSyncedForTest struct{}

func (errPoolNotSyncedForTest) Error() string { return "pool not synced" }

// --- fixtures ----------------------------------------------------------------

const (
	wakeNS    = "wake-ns"
	wakePool  = "wake-ns/pool-a"
	wakeModel = "meta/model-a"
)

func queueValues() []source.MetricValue {
	return []source.MetricValue{
		{Value: 0, Labels: map[string]string{
			metricNameLabel:      constants.EPPFlowControlQueueSize,
			targetEPPMetricLabel: wakeModel,
		}},
	}
}

// noQueueValues is an EPP that is up and exporting, just not flow control. This
// is the failure being detected: an idle queue reports 0, a queue that does not
// exist reports nothing, and only the second means nothing can wake the model.
func noQueueValues() []source.MetricValue {
	return []source.MetricValue{
		{Value: 3, Labels: map[string]string{metricNameLabel: "llm_d_epp_request_error_total"}},
	}
}

func wakeVA(name, modelID string) llmdvariant.Variant {
	return llmdvariant.Variant{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: wakeNS},
		Spec: llmdvariant.Spec{
			ModelID: modelID,
			// The scale target must be named and present in the map below:
			// resolveScaleTarget falls back to a live API fetch on a miss.
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment", Name: name, APIVersion: "apps/v1",
			},
		},
	}
}

func wakeTargets(names ...string) map[string]scaletarget.ScaleTargetAccessor {
	return wakeTargetsReady(0, names...)
}

// wakeTargetsReady builds scale targets reporting a given number of READY
// replicas each.
func wakeTargetsReady(ready int32, names ...string) map[string]scaletarget.ScaleTargetAccessor {
	out := make(map[string]scaletarget.ScaleTargetAccessor, len(names))
	for _, n := range names {
		out[wakeNS+"/"+n] = scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: wakeNS},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": n}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "server"}}},
				},
			},
			Status: appsv1.DeploymentStatus{Replicas: ready, ReadyReplicas: ready},
		})
	}
	return out
}

func wakeEngine(t *testing.T, src source.MetricsSource) (*Engine, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	return &Engine{
		config: config.NewTestConfig(),
		Datastore: &wakeDatastore{
			pool: &poolutil.EndpointPool{Name: "pool-a", Namespace: wakeNS},
			src:  src,
		},
	}, registry
}

func wakeReasons(t *testing.T, registry *prometheus.Registry) map[string]string {
	t.Helper()
	mfs, err := registry.Gather()
	require.NoError(t, err)
	out := map[string]string{}
	for _, mf := range mfs {
		if mf.GetName() != constants.LLMSMModelScalingBlocked {
			continue
		}
		for _, m := range mf.GetMetric() {
			var model, reason string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case constants.LabelModelName:
					model = l.GetValue()
				case constants.LabelReason:
					reason = l.GetValue()
				}
			}
			out[model] = reason
		}
	}
	return out
}

// --- tests -------------------------------------------------------------------

func TestQueueFamilyExported(t *testing.T) {
	assert.True(t, queueFamilyExported(queueValues()),
		"a queue reading 0 still proves the family exists")
	assert.False(t, queueFamilyExported(noQueueValues()),
		"an EPP exporting other families but no queue cannot wake anything")
	assert.False(t, queueFamilyExported(nil))
}

// The point of moving this off the per-model path: a model that is SERVING right
// now, behind an EPP with no flow control, is the dangerous case. The enforcer
// will park it on the vLLM request counter — which has nothing to do with flow
// control — and only then is it unwakeable. Reporting when it parks reports after
// the trap has closed.
func TestReportWakeSignal_CoversServingModelsToo(t *testing.T) {
	src := &wakeSource{values: noQueueValues()}
	e, registry := wakeEngine(t, src)

	e.reportWakeSignal(context.Background(),
		[]llmdvariant.Variant{wakeVA("parked", "model-parked")}, wakeTargets("parked"),
		[]llmdvariant.Variant{wakeVA("serving", "model-serving")}, wakeTargets("serving"),
		time.Now())

	got := wakeReasons(t, registry)
	assert.Equal(t, constants.ScalingBlockedNoWakeSignal, got["model-parked"])
	assert.Equal(t, constants.ScalingBlockedNoWakeSignal, got["model-serving"],
		"a serving model behind a flow-control-less EPP is the case worth warning about early")
}

func TestReportWakeSignal_SaysNothingWhenTheQueueExists(t *testing.T) {
	src := &wakeSource{values: queueValues()}
	e, registry := wakeEngine(t, src)

	e.reportWakeSignal(context.Background(),
		[]llmdvariant.Variant{wakeVA("parked", wakeModel)}, wakeTargets("parked"),
		nil, nil, time.Now())

	assert.Empty(t, wakeReasons(t, registry))
}

// A failure to LOOK is not an observation of absence. Reporting one would send an
// operator to restart a perfectly healthy EPP.
func TestReportWakeSignal_UnreadablePoolReportsNothing(t *testing.T) {
	src := &wakeSource{err: assertErrNotSynced}
	e, registry := wakeEngine(t, src)

	e.reportWakeSignal(context.Background(),
		[]llmdvariant.Variant{wakeVA("parked", wakeModel)}, wakeTargets("parked"),
		nil, nil, time.Now())

	assert.Empty(t, wakeReasons(t, registry))
}

// An unresolvable pool is the normal bootstrap state, not a missing wake signal.
func TestReportWakeSignal_UnresolvedPoolReportsNothing(t *testing.T) {
	src := &wakeSource{values: noQueueValues()}
	e, registry := wakeEngine(t, src)
	e.Datastore.(*wakeDatastore).noPool = true

	e.reportWakeSignal(context.Background(),
		[]llmdvariant.Variant{wakeVA("parked", wakeModel)}, wakeTargets("parked"),
		nil, nil, time.Now())

	assert.Empty(t, wakeReasons(t, registry))
	assert.Zero(t, src.scrapes, "an unresolvable pool must not be scraped")
}

// The sweep runs off the 100ms wake loop, so it must not scrape on every tick.
// EPP's answer changes when EPP restarts, not ten times a second.
func TestReportWakeSignal_ThrottlesScrapes(t *testing.T) {
	src := &wakeSource{values: noQueueValues()}
	e, _ := wakeEngine(t, src)
	inactive := []llmdvariant.Variant{wakeVA("parked", wakeModel)}
	targets := wakeTargets("parked")

	start := time.Now()
	for i := 0; i < 20; i++ {
		e.reportWakeSignal(context.Background(), inactive, targets, nil, nil,
			start.Add(time.Duration(i)*100*time.Millisecond))
	}
	assert.Equal(t, 1, src.scrapes, "twenty ticks inside the TTL is one scrape")

	e.reportWakeSignal(context.Background(), inactive, targets, nil, nil,
		start.Add(wakeSignalTTL+time.Second))
	assert.Equal(t, 2, src.scrapes, "past the TTL it checks again")
}

// A pool with something parked is scraped by the wake path anyway, so the sweep
// must reuse that verdict rather than paying for a second scrape.
func TestReportWakeSignal_ReusesTheWakePathVerdict(t *testing.T) {
	src := &wakeSource{values: noQueueValues()}
	e, registry := wakeEngine(t, src)
	now := time.Now()

	e.recordPoolWakeVerdict(wakePool, false, now)
	e.reportWakeSignal(context.Background(),
		[]llmdvariant.Variant{wakeVA("parked", wakeModel)}, wakeTargets("parked"),
		nil, nil, now)

	assert.Zero(t, src.scrapes, "the wake path already proved this")
	assert.Equal(t, constants.ScalingBlockedNoWakeSignal, wakeReasons(t, registry)[wakeModel])
}

func TestCachedPoolWakeVerdict_Expires(t *testing.T) {
	e, _ := wakeEngine(t, &wakeSource{})
	now := time.Now()

	e.recordPoolWakeVerdict(wakePool, true, now)
	exported, fresh := e.cachedPoolWakeVerdict(wakePool, now.Add(time.Second))
	assert.True(t, fresh)
	assert.True(t, exported)

	_, fresh = e.cachedPoolWakeVerdict(wakePool, now.Add(wakeSignalTTL+time.Second))
	assert.False(t, fresh, "a stale verdict must be re-checked, not trusted")

	_, fresh = e.cachedPoolWakeVerdict("wake-ns/never-seen", now)
	assert.False(t, fresh)
}

// A model that leaves the fleet keeps its reason until something clears it, and
// only this producer may clear its own reason.
func TestReportWakeSignal_ClearsDepartedModels(t *testing.T) {
	src := &wakeSource{values: noQueueValues()}
	e, registry := wakeEngine(t, src)
	start := time.Now()

	e.reportWakeSignal(context.Background(),
		[]llmdvariant.Variant{wakeVA("a", "model-a"), wakeVA("b", "model-b")},
		wakeTargets("a", "b"), nil, nil, start)
	require.Len(t, wakeReasons(t, registry), 2)

	e.reportWakeSignal(context.Background(),
		[]llmdvariant.Variant{wakeVA("a", "model-a")}, wakeTargets("a"),
		nil, nil, start.Add(wakeSignalTTL+time.Second))

	got := wakeReasons(t, registry)
	assert.Contains(t, got, "model-a")
	assert.NotContains(t, got, "model-b", "a departed model must not keep alerting")
}

// "Cannot tell" must not look like "resolved". Both failure modes — an
// unresolvable pool and an unreadable one — used to differ: the first dropped the
// model from the ref set, which the prune then read as "departed" and cleared its
// reason. Clearing is an answer, and a sweep that determined nothing has none.
func TestReportWakeSignal_UnresolvablePoolHoldsThePreviousAnswer(t *testing.T) {
	src := &wakeSource{values: noQueueValues()}
	e, registry := wakeEngine(t, src)
	inactive := []llmdvariant.Variant{wakeVA("parked", wakeModel)}
	targets := wakeTargets("parked")
	start := time.Now()

	e.reportWakeSignal(context.Background(), inactive, targets, nil, nil, start)
	require.Equal(t, constants.ScalingBlockedNoWakeSignal, wakeReasons(t, registry)[wakeModel])

	// The pool stops resolving — a datastore resync, a bootstrap window.
	e.Datastore.(*wakeDatastore).noPool = true
	e.reportWakeSignal(context.Background(), inactive, targets, nil, nil,
		start.Add(wakeSignalTTL+time.Second))

	assert.Equal(t, constants.ScalingBlockedNoWakeSignal, wakeReasons(t, registry)[wakeModel],
		"a model scaling manager cannot currently resolve has not gone away, and its last answer stands")
}

func TestReportWakeSignal_UnreadablePoolHoldsThePreviousAnswer(t *testing.T) {
	src := &wakeSource{values: noQueueValues()}
	e, registry := wakeEngine(t, src)
	inactive := []llmdvariant.Variant{wakeVA("parked", wakeModel)}
	targets := wakeTargets("parked")
	start := time.Now()

	e.reportWakeSignal(context.Background(), inactive, targets, nil, nil, start)
	require.Equal(t, constants.ScalingBlockedNoWakeSignal, wakeReasons(t, registry)[wakeModel])

	src.err = assertErrNotSynced
	e.reportWakeSignal(context.Background(), inactive, targets, nil, nil,
		start.Add(wakeSignalTTL+time.Second))

	assert.Equal(t, constants.ScalingBlockedNoWakeSignal, wakeReasons(t, registry)[wakeModel],
		"failing to look must leave the same trace as failing to resolve")
}

// A sweep that answered nothing is a cluster still coming up, not a settled
// answer, so it must not wait a full TTL to try again.
func TestReportWakeSignal_RetriesSoonerAfterDeterminingNothing(t *testing.T) {
	src := &wakeSource{err: assertErrNotSynced}
	e, _ := wakeEngine(t, src)
	inactive := []llmdvariant.Variant{wakeVA("parked", wakeModel)}
	targets := wakeTargets("parked")
	start := time.Now()

	e.reportWakeSignal(context.Background(), inactive, targets, nil, nil, start)
	require.Equal(t, 1, src.scrapes)

	// Still inside the short retry window: no second attempt.
	e.reportWakeSignal(context.Background(), inactive, targets, nil, nil,
		start.Add(wakeSignalRetry/2))
	assert.Equal(t, 1, src.scrapes)

	// Past it, and well short of the full TTL.
	e.reportWakeSignal(context.Background(), inactive, targets, nil, nil,
		start.Add(wakeSignalRetry+time.Second))
	assert.Equal(t, 2, src.scrapes, "a failed sweep retries on the short clock, not the TTL")

	// Once it succeeds, the long clock applies again.
	src.err = nil
	src.values = queueValues()
	e.reportWakeSignal(context.Background(), inactive, targets, nil, nil,
		start.Add(2*wakeSignalRetry+2*time.Second))
	before := src.scrapes
	e.reportWakeSignal(context.Background(), inactive, targets, nil, nil,
		start.Add(2*wakeSignalRetry+3*time.Second))
	assert.Equal(t, before, src.scrapes, "a determined answer holds for the full TTL")
}

// modelReplicas reads the model -> replica-count series.
func modelReplicas(t *testing.T, registry *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := registry.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != constants.LLMSMModelReplicas {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == constants.LabelModelName {
					out[l.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}
	return out
}

// The bug this placement exists to avoid. llmsm_model_replicas == 0 is the whole
// subject of the symptom alert, and the steady-state engine cannot produce it:
// that engine lists variants with isActive, meaning spec replicas > 0, so a
// PARKED model never enters it. Emitted there, the series would only ever hold
// counts of 1 or more and the alert would be unreachable while looking correct.
func TestReportWakeSignal_PublishesZeroForAParkedModel(t *testing.T) {
	src := &wakeSource{values: queueValues()}
	e, registry := wakeEngine(t, src)

	e.reportWakeSignal(context.Background(),
		[]llmdvariant.Variant{wakeVA("parked", wakeModel)}, wakeTargetsReady(0, "parked"),
		nil, nil, time.Now())

	got := modelReplicas(t, registry)
	require.Contains(t, got, wakeModel, "a parked model must still publish a replica count")
	assert.Zero(t, got[wakeModel], "zero is the sample the symptom alert reads")
}

// A model's variants are split between the inactive and active lists, so neither
// alone is the model's total.
func TestReportWakeSignal_SumsReplicasAcrossBothLists(t *testing.T) {
	src := &wakeSource{values: queueValues()}
	e, registry := wakeEngine(t, src)

	parked := wakeVA("parked", wakeModel)
	serving := wakeVA("serving", wakeModel)

	e.reportWakeSignal(context.Background(),
		[]llmdvariant.Variant{parked}, wakeTargetsReady(0, "parked"),
		[]llmdvariant.Variant{serving}, wakeTargetsReady(3, "serving"),
		time.Now())

	assert.Equal(t, float64(3), modelReplicas(t, registry)[wakeModel])
}

// Ready, not spec: a workload scaled up whose pods are all pending serves
// nothing, and reporting the spec count would hide exactly that outage.
func TestReportWakeSignal_CountsReadyNotSpecReplicas(t *testing.T) {
	src := &wakeSource{values: queueValues()}
	e, registry := wakeEngine(t, src)

	targets := map[string]scaletarget.ScaleTargetAccessor{
		wakeNS + "/parked": scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "parked", Namespace: wakeNS},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(4)),
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "parked"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "server"}}},
				},
			},
			Status: appsv1.DeploymentStatus{Replicas: 4, ReadyReplicas: 0},
		}),
	}

	e.reportWakeSignal(context.Background(),
		[]llmdvariant.Variant{wakeVA("parked", wakeModel)}, targets,
		nil, nil, time.Now())

	assert.Zero(t, modelReplicas(t, registry)[wakeModel],
		"four replicas that are not ready serve nothing")
}

func TestReportWakeSignal_ClearsReplicasForDepartedModels(t *testing.T) {
	src := &wakeSource{values: queueValues()}
	e, registry := wakeEngine(t, src)
	start := time.Now()

	e.reportWakeSignal(context.Background(),
		[]llmdvariant.Variant{wakeVA("a", "model-a"), wakeVA("b", "model-b")},
		wakeTargetsReady(0, "a", "b"), nil, nil, start)
	require.Len(t, modelReplicas(t, registry), 2)

	e.reportWakeSignal(context.Background(),
		[]llmdvariant.Variant{wakeVA("a", "model-a")}, wakeTargetsReady(0, "a"),
		nil, nil, start.Add(wakeSignalTTL+time.Second))

	got := modelReplicas(t, registry)
	assert.Contains(t, got, "model-a")
	assert.NotContains(t, got, "model-b",
		"a deleted workload must not keep a parked-at-zero sample alive")
}

// Replica counts are the current state of the fleet and cost nothing to compute
// — the scale targets are already in hand. Holding them behind the wake-check
// throttle made "is anything serving this model" up to 30 seconds stale for no
// saving, on the series the symptom alert reads.
func TestReportWakeSignal_ReplicaCountsAreNotThrottled(t *testing.T) {
	src := &wakeSource{values: queueValues()}
	e, registry := wakeEngine(t, src)
	vas := []llmdvariant.Variant{wakeVA("v", wakeModel)}
	start := time.Now()

	e.reportWakeSignal(context.Background(), vas, wakeTargetsReady(2, "v"), nil, nil, start)
	require.Equal(t, float64(2), modelReplicas(t, registry)[wakeModel])

	// Well inside the wake-check TTL: the verdict is not rechecked, but the fleet
	// has changed and the series must say so.
	e.reportWakeSignal(context.Background(), vas, wakeTargetsReady(0, "v"), nil, nil,
		start.Add(200*time.Millisecond))

	assert.Zero(t, modelReplicas(t, registry)[wakeModel],
		"a model that just parked must report zero now, not in 30 seconds")
	assert.Equal(t, 1, src.scrapes, "and it must not have cost a scrape")
}

// The throttle still has to bite where it matters: pool resolution scans the
// datastore for every model, on the 100ms loop.
func TestReportWakeSignal_SkipsPoolResolutionWhenNotDue(t *testing.T) {
	src := &wakeSource{values: noQueueValues()}
	e, _ := wakeEngine(t, src)
	ds := e.Datastore.(*wakeDatastore)
	vas := []llmdvariant.Variant{wakeVA("v", wakeModel)}
	start := time.Now()

	e.reportWakeSignal(context.Background(), vas, wakeTargets("v"), nil, nil, start)
	before := ds.poolLookups

	for i := 1; i <= 5; i++ {
		e.reportWakeSignal(context.Background(), vas, wakeTargets("v"), nil, nil,
			start.Add(time.Duration(i)*100*time.Millisecond))
	}

	assert.Equal(t, before, ds.poolLookups,
		"ticks inside the TTL must not re-scan the datastore for every model")
}
