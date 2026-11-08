package steadystate

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// Review finding R7: LLMSMKueueBorrowThrashing summed two counters in different
// units -- a release per variant (a P/D pair counted twice), a preemption per
// Workload (a P/D pair's two Workloads twice, a LeaderWorkerSet group once) --
// and each counter was created by its first increment, which increase() never
// sees, so the alert's threshold of 3 took 5 losses. Both now count a loss
// event of a model, and both exist at 0 from the first cycle that counts
// borrowing for it.
var _ = Describe("the units of the borrowing loss counters", func() {
	ctx := context.Background()
	t0 := time.Unix(90000, 0)
	var registry *prometheus.Registry

	BeforeEach(func() {
		registry = prometheus.NewRegistry()
		Expect(metrics.InitMetrics(registry)).To(Succeed())
	})

	// counter is each series of name as "namespace,model" -> value.
	counter := func(name string) map[string]float64 {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		out := map[string]float64{}
		for _, mf := range mfs {
			if mf.GetName() != name {
				continue
			}
			for _, m := range mf.GetMetric() {
				var ns, model string
				for _, l := range m.GetLabel() {
					switch l.GetName() {
					case constants.LabelNamespace:
						ns = l.GetValue()
					case constants.LabelModelName:
						model = l.GetValue()
					}
				}
				out[strings.Join([]string{ns, model}, ",")] += m.GetCounter().GetValue()
			}
		}
		return out
	}

	// A P/D model in namespace "a", borrowing in cohort "c": a prefill and a
	// decode variant of 8 GPUs each, both with a replica unplaced since
	// unplacedSince (zero: none).
	pd := func(unplacedSince time.Time) []allocation.ModelScalingRequest {
		role := func(v string) domain.VariantReplicaState {
			st := domain.VariantReplicaState{VariantName: v, Role: v, CurrentReplicas: 2, GPUsPerReplica: 8, AcceleratorName: "H100"}
			if !unplacedSince.IsZero() {
				st.UnplacedReplicas, st.UnplacedSince, st.UnplacedPods = 1, unplacedSince, []string{v + "-abc-1"}
			}
			return st
		}
		return []allocation.ModelScalingRequest{{ModelID: "m", Namespace: "a", Disaggregated: true,
			VariantStates: []domain.VariantReplicaState{role("prefill"), role("decode")}}}
	}
	world := func(readAt time.Time) []*allocation.ResourceConstraints {
		return []*allocation.ResourceConstraints{{
			ProviderName:     "kq",
			NamespacePools:   map[string]map[string]allocation.ResourcePool{"a": {"H100": {Limit: 0, Used: 32}}},
			NamespaceBorrow:  map[string]map[string]allocation.NamespaceBorrow{"a": {"H100": {Cap: -1}}},
			NamespaceCohorts: map[string][]string{"a": {"c"}},
			SharedPools: []allocation.SharedPoolChunk{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
				FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: "H100"}, Free: 16, ReadAt: readAt}},
		}}
	}
	decisions := func() []domain.VariantDecision {
		d := func(v string) domain.VariantDecision {
			return domain.VariantDecision{Namespace: "a", ModelID: "m", VariantName: v, AcceleratorName: "H100",
				CurrentReplicas: 2, TargetReplicas: 2, GPUsPerReplica: 8}
		}
		return []domain.VariantDecision{d("prefill"), d("decode")}
	}
	targets := map[string]scaletarget.ScaleTargetAccessor{}
	for _, v := range []string{"prefill", "decode"} {
		targets["a/"+v] = scaletarget.NewDeploymentAccessor(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: v, Namespace: "a"}})
	}

	It("creates both counters at 0 on the first cycle that counts borrowing for the model", func() {
		e := &Engine{Config: config.NewTestConfig(), Recorder: record.NewFakeRecorder(10)}
		reqs := pd(time.Time{})
		e.prepareBorrowing(ctx, world(t0), reqs, targets, t0)
		e.releaseBorrowed(ctx, reqs, decisions(), targets, t0)
		Expect(counter(constants.LLMSMBorrowReleasesTotal)).To(Equal(map[string]float64{"a,m": 0}))
		Expect(counter(constants.LLMSMKueuePreemptedPodsTotal)).To(Equal(map[string]float64{"a,m": 0}))
	})

	// Review ev-shindin/llm-scaling-manager#199 (5): a model whose replicas may be released at Kueue's gate
	// in a cycle that counts no borrowing has its kueue-gate release counter
	// at 0 from that cycle, so its first release is an increase. No
	// preemption counter: nothing is borrowed. Before, the counters were
	// created only on cycles that count borrowing.
	It("creates the kueue-gate release counter at 0 on a cycle that counts no borrowing", func() {
		e := &Engine{Config: config.NewTestConfig(), Recorder: record.NewFakeRecorder(10)}
		reqs := pd(time.Time{})
		gatedOnly := []*allocation.ResourceConstraints{{ProviderName: "kq", NamespaceScoped: true, KueueBacked: true,
			NamespacePools: map[string]map[string]allocation.ResourcePool{"a": {"H100": {Limit: 32, Used: 32}}}}}
		e.prepareBorrowing(ctx, gatedOnly, reqs, targets, t0)
		e.releaseBorrowed(ctx, reqs, decisions(), targets, t0)
		Expect(counter(constants.LLMSMBorrowReleasesTotal)).To(Equal(map[string]float64{"a,m": 0}))
		Expect(counter(constants.LLMSMKueuePreemptedPodsTotal)).To(BeEmpty())
	})

	It("counts a P/D pair released in one cycle as one release", func() {
		e := &Engine{Config: config.NewTestConfig(), Recorder: record.NewFakeRecorder(10)}
		now := t0.Add(6 * time.Minute)
		reqs := pd(t0)
		e.prepareBorrowing(ctx, world(t0), reqs, targets, t0)
		e.releaseBorrowed(ctx, reqs, decisions(), targets, t0)
		e.prepareBorrowing(ctx, world(now), reqs, targets, now)
		ds := decisions()
		e.releaseBorrowed(ctx, reqs, ds, targets, now)
		lowered := 0
		for _, d := range ds {
			if d.TargetReplicas < 2 {
				lowered++
			}
		}
		Expect(lowered).To(Equal(2), "both roles are released: the premise of this test")
		Expect(counter(constants.LLMSMBorrowReleasesTotal)).To(Equal(map[string]float64{"a,m": 1}))
	})

	It("counts a P/D pair's two preempted Workloads as one event, and a later preemption again", func() {
		e := &Engine{Config: config.NewTestConfig()}
		e.recordReclaim("a/m", kueue.PreemptedWorkload{Owners: []string{"prefill-abc-1"}, At: t0}, t0, "pod/p1")
		e.recordReclaim("a/m", kueue.PreemptedWorkload{Owners: []string{"decode-abc-1"}, At: t0.Add(10 * time.Second)},
			t0.Add(15*time.Second), "pod/d1")
		Expect(counter(constants.LLMSMKueuePreemptedPodsTotal)).To(Equal(map[string]float64{"a,m": 1}),
			"the two halves of one pair, preempted together")
		later := t0.Add(3 * time.Minute)
		e.recordReclaim("a/m", kueue.PreemptedWorkload{Owners: []string{"decode-abc-2"}, At: later}, later, "pod/d2")
		Expect(counter(constants.LLMSMKueuePreemptedPodsTotal)).To(Equal(map[string]float64{"a,m": 2}),
			"a preemption minutes later is another event")
	})
})
