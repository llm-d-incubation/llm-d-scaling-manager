package steadystate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	prometheuscollector "github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/prometheus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	variantregistry "github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

const lastAdmittedSeries = constants.LLMSMLearnedStateLastAdmittedTimestampSeconds

// reportSpy is a saturation analyzer that hands over fixed reports, once, and
// counts the takes.
type reportSpy struct {
	reports, filed map[string][]learn.Report
	takes          int
}

func (s *reportSpy) Name() string { return "report-spy" }

func (s *reportSpy) Analyze(context.Context, domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	return nil, nil
}

func (s *reportSpy) TakeLearnerReports() map[string][]learn.Report {
	s.takes++
	out := s.reports
	s.reports = nil
	return out
}

func spyReports(model, variant string) map[string][]learn.Report {
	return map[string][]learn.Report{"ns|" + model: {{Learner: learn.Mu, Namespace: "ns", ModelID: model,
		Variant: variant, Role: domain.RoleDecode, PricingKey: "key", LastAdmitted: time.Unix(1_700_000_000, 0)}}}
}

// Every built-in analyzer that reports is taken from, and one that does not
// is passed over: the slo and throughput lines are not the saturation
// analyzer's.
func TestLearnerReportsAreTakenFromEveryBuiltin(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)
	sat := &reportSpy{reports: spyReports("m", "v")}
	line := &reportSpy{reports: map[string][]learn.Report{"ns|m": {{Learner: learn.SLO, Namespace: "ns", ModelID: "m",
		Variant: "v", Role: domain.RoleDecode, PricingKey: "slo-key", LastAdmitted: time.Unix(1_700_000_000, 0)}}}}
	e.analyzers = testAnalyzers(sat, analyzerEntry{name: domain.SLOAnalyzerName, analyzer: line},
		analyzerEntry{name: "silent", analyzer: silentAnalyzer{}})
	e.publishLearnerReports()
	assert.Equal(t, 1, sat.takes)
	assert.Equal(t, 1, line.takes)
	got := 0
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() == lastAdmittedSeries {
			got = len(f.GetMetric())
		}
	}
	assert.Equal(t, 2, got, "one series of each learner")
}

// silentAnalyzer is a built-in that files no reports.
type silentAnalyzer struct{}

func (silentAnalyzer) Name() string { return "silent" }

func (silentAnalyzer) Analyze(context.Context, domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	return nil, nil
}

// EvictStaleHistory files the spy's reports: the sweep runs inside the cycle,
// after the prune, as the Analyze calls that file real ones do.
func (s *reportSpy) EvictStaleHistory() saturation.Evicted {
	s.reports = s.filed
	return saturation.Evicted{}
}

// The analyzer's reports reach the metrics on the cycle that filed them, taken
// once: a spec of the emitter passes with no caller, which is how learner
// reports came to be read by nothing but the replay.
func TestLearnerReportsAreWiredIntoTheCycle(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)
	spy := &reportSpy{filed: spyReports("m", "v")}
	e.Config = config.NewTestConfig()
	e.optimizer = allocation.NewCostAwareOptimizer()
	e.analyzers = testAnalyzers(spy)
	e.capacityStore = capacity.NewStore()

	e.optimizeV2(context.Background(), map[string][]llmdvariant.Variant{
		"ns/m": {{
			ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "ns"},
			Spec:       llmdvariant.Spec{ModelID: "m"},
		}},
	}, nil)

	assert.Equal(t, 1, spy.takes, "one take a cycle")
	assert.Equal(t, []string{learn.Mu}, seriesLabels(t, registry, lastAdmittedSeries, constants.LabelLearner))
}

// publishedSpy is an engine holding one published series of model m.
func publishedSpy(t *testing.T, model, variant string, e *Engine) {
	t.Helper()
	e.analyzers = testAnalyzers(&reportSpy{reports: spyReports(model, variant)})
	e.publishLearnerReports()
}

// parkRig is an engine over a fake cluster, driven through optimize: each
// variant a ScaledObject KEDA has called about, with a Deployment behind it.
type parkRig struct {
	t      *testing.T
	e      *Engine
	c      client.Client
	base   *clocktesting.FakePassiveClock
	series *prometheus.Registry
	// forbid makes every Deployment read Forbidden, and reads counts them.
	forbid bool
	reads  int
}

func newParkRig(t *testing.T) *parkRig {
	t.Helper()
	series := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(series))
	// What other tests published is process-wide bookkeeping, not this rig's.
	metrics.NewMetricsEmitter().RetainLearnerSeries(func(string, string, string) bool { return false })
	r := &parkRig{t: t, series: series, base: clocktesting.NewFakePassiveClock(time.Unix(1_700_000_000, 0))}
	r.c = fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok {
				r.reads++
				if r.forbid {
					return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, key.Name, errors.New("no"))
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	sources := source.NewSourceRegistry()
	require.NoError(t, sources.Register("prometheus",
		prometheuscollector.NewPrometheusSource(context.Background(), nil, prometheuscollector.DefaultPrometheusSourceConfig())))
	r.e = newEngine(r.c, r.c, r.c.Scheme(), nil, sources, config.NewTestConfig(), allocation.NewNoOpLimiter("test"), r.base)
	r.e.Variants = variantregistry.New(0)
	return r
}

// variant adds a variant at replicas, with a learner series published for it.
func (r *parkRig) variant(model, name string, replicas int32) {
	r.t.Helper()
	r.workload(model, name, replicas)
	r.e.metricsEmitter.RecordLearnerReports(spyReports(model, name)["ns|"+model], r.e.learnerTarget)
}

// workload adds a variant and what it scales, and publishes nothing.
func (r *parkRig) workload(model, name string, replicas int32) {
	r.t.Helper()
	r.e.Variants.Observe("ns", name, map[string]string{variantregistry.ModelIDKey: model})
	r.e.Variants.SetTarget("ns", name, variantregistry.Target{APIVersion: "apps/v1", Kind: "Deployment", Name: name})
	require.NoError(r.t, r.c.Create(context.Background(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}))
}

// expire is KEDA no longer calling about a variant, its workload still there.
func (r *parkRig) expire(name string) {
	r.t.Helper()
	r.e.Variants.Forget("ns", name)
}

// remove deletes a variant: its ScaledObject and its workload both go.
func (r *parkRig) remove(name string) {
	r.t.Helper()
	r.expire(name)
	require.NoError(r.t, r.c.Delete(context.Background(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}))
}

// after moves the clock on.
func (r *parkRig) after(d time.Duration) *parkRig {
	r.base.SetTime(r.base.Now().Add(d))
	return r
}

// cycle runs one optimize. A model with replicas has nothing to collect from
// here, so its cycle may fail: the series do not depend on it passing.
func (r *parkRig) cycle() []string {
	r.t.Helper()
	_ = r.e.optimize(context.Background())
	return seriesLabels(r.t, r.series, lastAdmittedSeries, constants.LabelVariantName)
}

// A real park: every model at zero replicas, the path that returns before any
// model is analyzed and evicts every analyzer series. The series stay, however
// many cycles it lasts, or an alert on their age resolves instead of firing.
// A model whose ScaledObject goes loses its series, and the last one too.
func TestLearnerSeries_AllModelsParked(t *testing.T) {
	r := newParkRig(t)
	r.variant("a", "a-llmsm", 0)
	r.variant("b", "b-llmsm", 0)
	for range 3 {
		assert.ElementsMatch(t, []string{"a-llmsm", "b-llmsm"}, r.cycle())
	}
	r.remove("a-llmsm")
	assert.Equal(t, []string{"b-llmsm"}, r.after(learnerTargetCheckInterval).cycle())
	r.remove("b-llmsm")
	assert.Empty(t, r.after(learnerTargetCheckInterval).cycle())
}

// One model parked among others: it is in no model group, so the prune of the
// active set takes its analyzer series and not these. A deleted model beside
// them does lose its own.
func TestLearnerSeries_OneModelOfSeveralParked(t *testing.T) {
	r := newParkRig(t)
	r.variant("running", "running-llmsm", 1)
	r.variant("parked", "parked-llmsm", 0)
	r.variant("deleted", "deleted-llmsm", 1)
	for range 2 {
		assert.ElementsMatch(t, []string{"running-llmsm", "parked-llmsm", "deleted-llmsm"}, r.cycle())
	}
	r.remove("deleted-llmsm")
	assert.ElementsMatch(t, []string{"running-llmsm", "parked-llmsm"}, r.after(learnerTargetCheckInterval).cycle())
}

// A variant parked beside a running sibling of its model keeps its series; a
// variant deleted from a model that stays loses it.
func TestLearnerSeries_OneVariantOfAModel(t *testing.T) {
	r := newParkRig(t)
	r.variant("m", "running-llmsm", 1)
	r.variant("m", "parked-llmsm", 0)
	r.variant("m", "deleted-llmsm", 0)
	r.cycle()
	r.remove("deleted-llmsm")
	assert.ElementsMatch(t, []string{"running-llmsm", "parked-llmsm"}, r.after(learnerTargetCheckInterval).cycle())
}

// A KEDA outage past the registry's TTL, through the registry itself: every
// entry expires, every target stands, and the series stay, or an alert on
// their age resolves in the middle of the outage.
func TestLearnerSeries_RegistryExpiryWithTheTargetPresentKeeps(t *testing.T) {
	r := newParkRig(t)
	const ttl = 300 * time.Millisecond
	r.e.Variants = variantregistry.New(ttl)
	r.variant("a", "a-llmsm", 0)
	r.variant("b", "b-llmsm", 1)
	require.Eventually(t, func() bool { return r.e.Variants.Len() == 0 || len(r.e.Variants.Snapshot()) == 0 },
		10*ttl, ttl/10, "premise: the entries expire")
	for range 3 {
		assert.ElementsMatch(t, []string{"a-llmsm", "b-llmsm"}, r.after(learnerTargetCheckInterval).cycle())
	}
}

// A trigger that stops parsing drops the variant from the known set, and only
// from it: the entry and the workload stand.
func TestLearnerSeries_AParseFailureKeeps(t *testing.T) {
	r := newParkRig(t)
	r.variant("m", "m-llmsm", 0)
	r.e.Variants.Observe("ns", "m-llmsm", map[string]string{"typo": "m"})
	require.Empty(t, utils.KnownVariants(context.Background(), r.e.Variants), "premise: the variant is not known")
	assert.Equal(t, []string{"m-llmsm"}, r.after(learnerTargetCheckInterval).cycle())
}

// NotFound is an answer, and the first variant to go missing is read at once:
// a cycle that read nothing does not start the interval.
func TestLearnerSeries_NotFoundDeletes(t *testing.T) {
	r := newParkRig(t)
	r.variant("m", "m-llmsm", 0)
	r.cycle()
	r.remove("m-llmsm")
	assert.Empty(t, r.cycle())
}

// The engine's publish records the target: the read that ends a series has
// nothing to go on otherwise, and the series would wait out the cap.
func TestLearnerSeries_ThePublishRecordsTheTarget(t *testing.T) {
	r := newParkRig(t)
	r.workload("m", "m-llmsm", 0)
	r.e.analyzers = testAnalyzers(&reportSpy{reports: spyReports("m", "m-llmsm")})
	r.e.publishLearnerReports()
	r.remove("m-llmsm")
	assert.Empty(t, r.cycle())
}

// A scale target being deleted is gone already.
func TestLearnerSeries_ADeletionTimestampDeletes(t *testing.T) {
	r := newParkRig(t)
	r.variant("m", "m-llmsm", 0)
	var d appsv1.Deployment
	key := client.ObjectKey{Namespace: "ns", Name: "m-llmsm"}
	require.NoError(t, r.c.Get(context.Background(), key, &d))
	d.Finalizers = []string{"test/hold"}
	require.NoError(t, r.c.Update(context.Background(), &d))
	require.NoError(t, r.c.Delete(context.Background(), &d))
	r.expire("m-llmsm")
	assert.Empty(t, r.cycle())
}

// Forbidden is not absent: the read could not ask. Nor is a kind the check
// does not read.
func TestLearnerSeries_ForbiddenKeeps(t *testing.T) {
	r := newParkRig(t)
	r.variant("m", "m-llmsm", 0)
	r.forbid = true
	r.expire("m-llmsm")
	for range 3 {
		assert.Equal(t, []string{"m-llmsm"}, r.after(learnerTargetCheckInterval).cycle())
	}
	assert.Equal(t, 3, r.reads, "premise: the reads were made")
	assert.False(t, r.e.scaleTargetGone(context.Background(), metrics.LearnerVariant{Namespace: "ns",
		Target: metrics.LearnerTarget{Kind: "StatefulSet", Name: "m-llmsm"}}))
}

// The cap ends a series no read can: the target stands, the variant has been
// missing for a day. Just under it the series stays, and a variant that comes
// back starts the count again.
func TestLearnerSeries_TheCapDeletes(t *testing.T) {
	r := newParkRig(t)
	r.variant("m", "m-llmsm", 0)
	r.expire("m-llmsm")
	require.Equal(t, []string{"m-llmsm"}, r.cycle())
	r.after(learnerSeriesMissingMax - time.Second)
	require.Equal(t, []string{"m-llmsm"}, r.cycle())
	r.e.Variants.Observe("ns", "m-llmsm", map[string]string{variantregistry.ModelIDKey: "m"})
	r.e.Variants.SetTarget("ns", "m-llmsm", variantregistry.Target{APIVersion: "apps/v1", Kind: "Deployment", Name: "m-llmsm"})
	require.Equal(t, []string{"m-llmsm"}, r.after(time.Second).cycle())
	r.expire("m-llmsm")
	require.Equal(t, []string{"m-llmsm"}, r.after(learnerSeriesMissingMax-time.Second).cycle(), "the count starts again")
	require.Equal(t, []string{"m-llmsm"}, r.after(learnerSeriesMissingMax-time.Second).cycle())
	assert.Empty(t, r.after(time.Second).cycle())
	assert.Empty(t, r.e.learnerMissingSince, "and nothing is kept of it")
}

// A variant with no target on record has nothing to read: it waits for the cap.
func TestLearnerSeries_NoRecordedTargetWaitsForTheCap(t *testing.T) {
	r := newParkRig(t)
	r.e.metricsEmitter.RecordLearnerReports(spyReports("m", "m-llmsm")["ns|m"], nil)
	r.reads = 0
	require.Equal(t, []string{"m-llmsm"}, r.cycle())
	assert.Zero(t, r.reads)
	assert.Empty(t, r.after(learnerSeriesMissingMax).cycle())
}

// The reads are bounded whatever the client is: one per missing variant per
// interval, however many cycles and however many learners publish.
func TestLearnerSeries_ReadsAreOnePerVariantPerInterval(t *testing.T) {
	r := newParkRig(t)
	r.variant("m", "m-llmsm", 0)
	r.e.metricsEmitter.RecordLearnerReports([]learn.Report{{Learner: learn.K2, Namespace: "ns", ModelID: "m", Variant: "m-llmsm",
		Role: domain.RoleDecode, PricingKey: "k", LastAdmitted: time.Unix(1_700_000_000, 0)}}, r.e.learnerTarget)
	r.variant("n", "n-llmsm", 0)
	r.expire("m-llmsm")
	r.expire("n-llmsm")
	r.reads = 0
	for range 5 {
		r.after(time.Second).cycle()
	}
	assert.Equal(t, 2, r.reads, "two variants, read once in the interval")
	r.after(learnerTargetCheckInterval).cycle()
	assert.Equal(t, 4, r.reads, "and once in the next")
}

// A variant still known costs no read.
func TestLearnerSeries_AKnownVariantCostsNoRead(t *testing.T) {
	r := newParkRig(t)
	r.variant("m", "m-llmsm", 0)
	r.reads = 0
	r.e.retainLearnerSeries(context.Background())
	assert.Zero(t, r.reads)
}

// The analyzer series follow what analyzers report, and these do not: a
// variant no analyzer reports may only be parked.
func TestLearnerSeries_StayWhenNoAnalyzerReportsTheVariant(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{namedResult("saturation", nil, 1, "v", "w")})
	publishedSpy(t, "m", "v", e)
	publishedSpy(t, "m", "w", e)
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{namedResult("saturation", nil, 1, "w")})
	assert.Len(t, seriesLabels(t, registry, lastAdmittedSeries, constants.LabelVariantName), 2)
	e.pruneAnalyzerSeries(map[string]bool{utils.GetNamespacedKey("ns", "other"): true})
	e.evictAllAnalyzerSeries()
	assert.Len(t, seriesLabels(t, registry, lastAdmittedSeries, constants.LabelVariantName), 2)
}

// A cycle with no replica rows keeps them: the borrow and k2-source series
// describe this cycle's pricing and go, these describe the evidence.
func TestLearnerSeriesStayThroughACycleWithNoRows(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)
	publishedSpy(t, "m", "v", e)
	e.zeroObservedReplicas("ns", "m")
	assert.Len(t, seriesLabels(t, registry, lastAdmittedSeries, constants.LabelVariantName), 1)
}

// An analyzer that reports nothing, and one that cannot, publish nothing.
func TestPublishLearnerReportsWithNoReports(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)
	e.publishLearnerReports() // no analyzer registered
	e.analyzers = testAnalyzers(&reportSpy{})
	e.publishLearnerReports()
	assert.Empty(t, seriesLabels(t, registry, lastAdmittedSeries, constants.LabelVariantName))
}
