package steadystate

import (
	"context"
	"errors"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// PR-C, what on-call sees of a release to a waiting lender: the reason on the
// lowered model, a Normal Event naming the lender, the release counted by
// cause, and the latency the lender-waiting alert adds to the cooldown.
var _ = Describe("TestLenderReleaseSignals: what on-call sees of a release to a lender", func() {
	ctx := context.Background()
	now := time.Unix(50000, 0)
	var registry *prometheus.Registry

	BeforeEach(func() {
		registry = prometheus.NewRegistry()
		Expect(metrics.InitMetrics(registry)).To(Succeed())
	})

	gather := func(name string, labels ...string) map[string]float64 {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		out := map[string]float64{}
		for _, mf := range mfs {
			if mf.GetName() != name {
				continue
			}
			for _, m := range mf.GetMetric() {
				var vals []string
				for _, want := range labels {
					for _, l := range m.GetLabel() {
						if l.GetName() == want {
							vals = append(vals, l.GetValue())
						}
					}
				}
				v := m.GetGauge().GetValue()
				if m.GetCounter() != nil {
					v = m.GetCounter().GetValue()
				}
				out[strings.Join(vals, ",")] = v
			}
		}
		return out
	}

	releaseCfg := func(release bool) *config.Config {
		cfg := config.NewTestConfig()
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{"default": {Limiters: []config.QuotaLimiterConfig{{
			Name: "kq", Type: "quota", Scope: config.QuotaScopeNamespace,
			NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
			Kueue:           &config.KueueQuotaSource{Enabled: true, ReleaseToNonReclaimingLenders: release},
		}}}})
		return cfg
	}

	It("reports the release on the model, as an Event, as a count, and its latency", func() {
		cfg := releaseCfg(true)
		Expect(cfg.KueueReleasesToNonReclaiming()).To(BeTrue(), "premise: the entry opts in")
		rec := record.NewFakeRecorder(10)
		e := &Engine{Config: cfg, Recorder: rec}
		cs, reqs := lenderReleaseWorld(now)
		targets := map[string]scaletarget.ScaleTargetAccessor{"b/x-v": scaletarget.NewDeploymentAccessor(
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "x-v", Namespace: "b"}})}
		e.borrowing.inFlight = allocation.NewBorrowInFlight()
		e.borrowing.governor = allocation.NewBorrowGovernor(allocation.DefaultBorrowSettings())
		e.borrowing.ledgerLoaded = true
		cs = e.governBorrowing(ctx, cs, reqs, targets, now)
		ds := lenderDecisions(reqs)
		e.releaseBorrowed(ctx, reqs, ds, targets, now)
		Expect(ds[0].TargetReplicas).To(Equal(4))
		e.publishBorrowSignals(cs, reqs, ds, now)

		Expect(gather(constants.LLMSMModelScalingBlocked, constants.LabelModelName, constants.LabelReason)).To(
			HaveKeyWithValue("x,"+constants.ScalingBlockedReleasedToLender, 1.0))
		Expect(gather(constants.LLMSMBorrowReleasesTotal, constants.LabelModelName, constants.LabelCause)).To(
			HaveKeyWithValue("x,lender-waiting", 1.0))
		// No ScaledObject is known: the cluster's defaults, 300s window, 15s
		// HPA sync, 30s poll, 30s grace; for the cohort that opts in alone.
		Expect(gather(constants.LLMSMKueueBorrowReleaseLatencySeconds, constants.LabelCohort)).To(
			Equal(map[string]float64{"c": 375.0}))
		var events []string
		for len(rec.Events) > 0 {
			events = append(events, <-rec.Events)
		}
		Expect(events).To(ContainElement(SatisfyAll(
			HavePrefix("Normal "+constants.K8SEventBorrowedCapacityReleased),
			ContainSubstring("to ClusterQueue lender"), ContainSubstring("cause lender-waiting"))))
	})

	// PR #203 review SF1: a lender this controller cannot serve is handed
	// nothing, and on-call is told, on a gauge and as a Warning Event.
	It("reports a lender it cannot serve on a gauge and as a Warning Event, and hands nothing back", func() {
		rec := record.NewFakeRecorder(10)
		e := &Engine{Config: releaseCfg(true), Recorder: rec}
		cs, reqs := lenderReleaseWorld(now)
		five := 5
		reqs[0].VariantStates[0].MinReplicas = &five // 8 of the lender's 16 can be given
		targets := map[string]scaletarget.ScaleTargetAccessor{"b/x-v": scaletarget.NewDeploymentAccessor(
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "x-v", Namespace: "b"}})}
		e.borrowing.inFlight = allocation.NewBorrowInFlight()
		e.borrowing.ledgerLoaded = true
		e.governBorrowing(ctx, cs, reqs, targets, now)
		ds := lenderDecisions(reqs)
		e.releaseBorrowed(ctx, reqs, ds, targets, now)
		Expect(ds[0].TargetReplicas).To(Equal(6), "nothing handed back to sit idle")
		Expect(gather(constants.LLMSMKueueLenderReleaseBlocked, constants.LabelCohort, constants.LabelClusterQueue,
			constants.LabelReason)).To(Equal(map[string]float64{"c,lender," + allocation.LenderUnservable: 1}))
		var events []string
		for len(rec.Events) > 0 {
			events = append(events, <-rec.Events)
		}
		Expect(events).To(ConsistOf(SatisfyAll(HavePrefix("Warning "+constants.K8SEventLenderReleaseBlocked),
			ContainSubstring("ClusterQueue lender waits in cohort c for 16 GPUs"), ContainSubstring("hand back only 8"))))
		// Once per change: the same report next cycle records nothing new.
		e.borrowing.governor.Observe(e.borrowing.cycle)
		e.releaseBorrowed(ctx, reqs, lenderDecisions(reqs), targets, now.Add(time.Minute))
		Expect(rec.Events).To(BeEmpty())
	})

	// PR #203 review (GT minor): the release is per entry, so are its series.
	It("counts no lender-waiting series and publishes no latency where the pool does not opt in, whatever another entry says", func() {
		e := &Engine{Config: releaseCfg(true), Recorder: record.NewFakeRecorder(10)}
		cs, reqs := lenderReleaseWorld(now)
		cs[0].SharedPools[0].ReleaseToNonReclaiming = false
		e.borrowing.inFlight = allocation.NewBorrowInFlight()
		e.borrowing.ledgerLoaded = true
		cs = e.governBorrowing(ctx, cs, reqs, nil, now)
		ds := lenderDecisions(reqs)
		e.releaseBorrowed(ctx, reqs, ds, nil, now)
		e.publishBorrowSignals(cs, reqs, ds, now)
		Expect(gather(constants.LLMSMBorrowReleasesTotal, constants.LabelModelName, constants.LabelCause)).NotTo(
			HaveKey("x,lender-waiting"))
		Expect(gather(constants.LLMSMKueueBorrowReleaseLatencySeconds)).To(BeEmpty())
	})

	// PR #203 review (GT minor): a ledger that cannot be read turns the
	// release off until it can; on-call sees why.
	It("says why no release is decided while the ledger cannot be read", func() {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		forbidden := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, errors.New("no"))
				}
				return c.Get(ctx, key, obj, opts...)
			}}).Build()
		e := &Engine{Config: releaseCfg(true), Recorder: record.NewFakeRecorder(10), client: forbidden}
		cs, reqs := lenderReleaseWorld(now)
		e.borrowing.inFlight = allocation.NewBorrowInFlight()
		cs = e.governBorrowing(ctx, cs, reqs, nil, now)
		ds := lenderDecisions(reqs)
		e.releaseBorrowed(ctx, reqs, ds, nil, now)
		e.publishBorrowSignals(cs, reqs, ds, now)
		Expect(ds[0].TargetReplicas).To(Equal(6))
		Expect(gather(constants.LLMSMKueueBorrowLedgerUnread, constants.LabelReason)).To(
			Equal(map[string]float64{"forbidden": 1}))
	})

	It("publishes no release latency, and counts no lender-waiting series, with the option off", func() {
		e := &Engine{Config: releaseCfg(false), Recorder: record.NewFakeRecorder(10)}
		cs, reqs := lenderReleaseWorld(now)
		cs[0].SharedPools[0].ReleaseToNonReclaiming = false
		e.borrowing.inFlight = allocation.NewBorrowInFlight()
		e.borrowing.ledgerLoaded = true
		cs = e.governBorrowing(ctx, cs, reqs, nil, now)
		ds := lenderDecisions(reqs)
		e.releaseBorrowed(ctx, reqs, ds, nil, now)
		e.publishBorrowSignals(cs, reqs, ds, now)
		Expect(ds[0].TargetReplicas).To(Equal(6))
		Expect(gather(constants.LLMSMKueueBorrowReleaseLatencySeconds)).To(BeEmpty())
		Expect(gather(constants.LLMSMBorrowReleasesTotal, constants.LabelModelName, constants.LabelCause)).NotTo(
			HaveKey("x,lender-waiting"))
		Expect(gather(constants.LLMSMModelScalingBlocked, constants.LabelModelName, constants.LabelReason)).NotTo(
			HaveKey("x," + constants.ScalingBlockedReleasedToLender))
	})
})
