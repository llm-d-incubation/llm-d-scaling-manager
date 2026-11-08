package steadystate

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/locator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	promsource "github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/prometheus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/throughput"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// R2's round trip (docs/proposals/analyzer-learner.md, 2.9): a controller's
// recorded run and a capture of the same window, played by Generate, give
// the analyzers the same inputs, in the same order, and the analyzers give
// the same results.
//
// One fake Prometheus (promtest) serves both. The engine collects from it as
// a controller does, with its own collector and instant queries at each
// cycle's instant, the Pods' serving state from the API server (a fake
// client) and their variants from a locator; its learners are recorded.
// whatifsource.Capture reads the same series as range queries, and the
// Pods from the kube-state-metrics series that describe the same Pods; the
// capture is played by learnreplay's Generate mode.
//
// What cannot be equal, and how it is held instead:
//   - the variants' states. The engine's come from discovery
//     (variantmeta.Discover), which reads Deployments, nodes and Kueue that a
//     capture cannot; a capture fills the replica counts, GPUs per replica,
//     role and accelerator (whatifsource variantStates). The engine here is
//     handed states of those fields alone, so the comparison holds the
//     analyzers to everything else; what the other fields move is the
//     bound, named rather than measured.
//   - the rows' order. The collector returns them in map order; both sides
//     sort them by variant and Pod, as a capture does.
//   - the start line's commit and clock: Generate stamps its own build's.
//     The start is not an output.
// Everything else is asserted equal byte for byte: each Analyze's input and
// result, every store record read, every line and every published SLO.

const (
	wiNS      = "rt"
	wiSteps   = 12
	wiStep    = 15 * time.Second
	wiAccel   = "NVIDIA-H100-80GB-HBM3"
	wiPool    = "pool-agg"
	wiCapUnit = 6250
)

var wiStart = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// wiArgs is the engine arguments of every variant, on the Deployment the
// engine parses and on the capture's command line.
var wiArgs = []string{"--max-num-seqs=64", "--block-size=16"}

// wiVariant is one variant of the round trip's fleet.
type wiVariant struct {
	model, name, role, hash string
	pods                    int
	gpus                    int
	// kv is pod j's KV utilization at step i.
	kv func(j, i int) float64
	// until is the last step its Pods report a series, -1 for every step.
	until int
	// parked says at which steps its Deployment is at zero; nil for none.
	parked func(i int) bool
}

// isParked reports whether v is parked at step i.
func (v wiVariant) isParked(i int) bool { return v.parked != nil && v.parked(i) }

// wiParkedFrom and wiParkedTo are the steps acme/park is parked whole.
const wiParkedFrom, wiParkedTo = 4, 6

func wiFleet() []wiVariant {
	return []wiVariant{
		{model: "acme/agg", name: "agg-a", role: domain.RoleBoth, hash: "5f6", pods: 4, gpus: 1, until: -1,
			kv: func(j, i int) float64 { return 0.30 + 0.12*float64(j) + 0.01*float64(i%3) }},
		{model: "acme/pd", name: "pd-d", role: domain.RoleDecode, hash: "88b", pods: 3, gpus: 2, until: -1,
			kv: func(j, i int) float64 { return 0.25 + 0.2*float64(j) + 0.01*float64(i%2) }},
		{model: "acme/pd", name: "pd-p", role: domain.RolePrefill, hash: "77a", pods: 1, gpus: 1, until: -1,
			kv: func(int, int) float64 { return 0.2 }},
		// Its Pod stops reporting after the first step while its
		// Deployment keeps a replica: the engine holds the model's settle
		// sightings on every cycle after (onNothingObserved), and so must
		// Generate.
		{model: "acme/idle", name: "idle", role: domain.RoleBoth, hash: "99c", pods: 1, gpus: 1, until: 0,
			kv: func(int, int) float64 { return 0.1 }},
		// Parked whole partway through the window, and back under the same
		// Pod name (a StatefulSet's) within the settle tracker's horizon:
		// the engine's prune forgets the model's settle record while it is
		// parked (retainSettle), so its replica joins again, and so must
		// Generate.
		{model: "acme/park", name: "park", role: domain.RoleBoth, hash: "4d4", pods: 1, gpus: 1, until: -1,
			kv:     func(_, i int) float64 { return 0.35 + 0.01*float64(i%2) },
			parked: func(i int) bool { return i >= wiParkedFrom && i <= wiParkedTo }},
	}
}

// wiRouterPod is the router's Pod of acme/agg: kube-state-metrics lists it,
// and it has no KV series. A --role-of app=agg-a picks it by name.
const wiRouterPod = "agg-a-epp-3e1-0"

func (v wiVariant) pod(j int) string { return fmt.Sprintf("%s-%s-%d", v.name, v.hash, j) }

// wiStepOf is the step an instant falls on, ok false off the grid.
func wiStepOf(at time.Time) (int, bool) {
	d := at.Sub(wiStart)
	if d < 0 || d%wiStep != 0 {
		return 0, false
	}
	return int(d / wiStep), d/wiStep < wiSteps
}

// wiPolicy is the policy's default entry: every analyzer listed, a TPOT
// objective for the slo analyzer.
func wiPolicy(t *testing.T) config.ScalingPolicy {
	t.Helper()
	p, err := config.DecodeScalingPolicy("analyzers:\n- name: saturation\n- name: slo\n- name: throughput\n" +
		"slo:\n  tpot:\n    target: 40ms\n")
	require.NoError(t, err)
	return p
}

// wiSeries fills srv with every series the fleet exports, as a function of
// the step: the engine's, the router's and kube-state-metrics'.
func wiSeries(t *testing.T, srv *promtest.Server, q func(name string) string) {
	t.Helper()
	at := func(v wiVariant, f func(i int) float64) func(time.Time) (float64, bool) {
		return func(ts time.Time) (float64, bool) {
			i, ok := wiStepOf(ts)
			if !ok || (v.until >= 0 && i > v.until) || v.isParked(i) {
				return 0, false
			}
			return f(i), true
		}
	}
	for _, v := range wiFleet() {
		for j := range v.pods {
			pod := v.pod(j)
			labels := func(extra ...string) map[string]string {
				l := map[string]string{"model_name": v.model, "instance": fmt.Sprintf("10.0.%d.%d:8000", len(v.name), j),
					"pod": pod, "namespace": wiNS}
				for k := 0; k+1 < len(extra); k += 2 {
					l[extra[k]] = extra[k+1]
				}
				return l
			}
			kv := func(i int) float64 { return v.kv(j, i) }
			itl := func(i int) float64 { return 0.006 + 0.02*kv(i) }
			add := func(name string, f func(i int) float64, extra ...string) {
				srv.Add(q(name), promtest.Series{Labels: labels(extra...), Value: at(v, f)})
			}
			c := func(x float64) func(int) float64 { return func(int) float64 { return x } }
			queue := func(i int) float64 { return float64((j + i) % 3) }
			add(registration.QueryKvCacheUsage, kv)
			add(registration.QueryQueueLength, queue)
			add(registration.QueryMaxRankKvCacheUsage, kv)
			add(registration.QueryMaxRankQueueLength, queue)
			add(registration.QueryCapacityQueueLength, queue)
			add(registration.QueryMaxRankCapacityQueueLength, queue)
			srv.Add(q(registration.QueryCacheConfigInfo), promtest.Series{Labels: map[string]string{
				"instance": labels()["instance"], "pod": pod, "num_gpu_blocks": strconv.Itoa(wiCapUnit), "block_size": "16"},
				Value: at(v, c(1))})
			add(registration.QueryAvgOutputTokens, c(300+10*float64(j)))
			add(registration.QueryAvgOutputTokensRecent, c(290+10*float64(j)))
			add(registration.QueryOutputTokensBuckets, c(0.5), "le", "256")
			add(registration.QueryOutputTokensBuckets, c(1.2), "le", "+Inf")
			add(registration.QueryAvgInputTokens, c(900))
			add(registration.QueryAvgInputTokensRecent, c(880))
			add(registration.QueryPrefixCacheHitRate, c(0.1))
			if v.role == domain.RolePrefill {
				add(registration.QueryAvgITL, c(math.NaN()))
			} else {
				add(registration.QueryAvgITL, itl)
				add(registration.QueryGenerationTokenRate, func(i int) float64 { return kv(i) * 40 / itl(i) })
				add(registration.QueryRequestRate, func(i int) float64 { return kv(i) * 40 / itl(i) / 300 })
			}
			add(registration.QueryAvgTTFT, c(0.2+0.01*float64(j)))
			add(registration.QueryAvgServiceTime, c(2+0.1*float64(j)))
			add(registration.QueryPrefillComputedTokenRate, c(2000))
			add(registration.QueryKvUsageInstant, kv)
			add(registration.QueryKvUsageMean, kv)
			add(registration.QueryRunningRequests, func(i int) float64 { return math.Round(kv(i) * 40) })
			add(registration.QueryPreemptionRate, c(0))
			add(registration.QueryPromptTokensBuckets, c(0.8), "le", "1024")
			add(registration.QueryPromptTokensBuckets, c(1), "le", "+Inf")
			add(registration.QueryTTFTBuckets, c(0.9), "le", "0.5")
			add(registration.QueryTTFTBuckets, c(1), "le", "+Inf")
			add(registration.QueryAvgQueueTime, c(0.03))
			add(registration.QueryQueueTimeBuckets, c(1), "le", "0.1")
			add(registration.QueryQueueMin, c(0))
			add(registration.QueryAvgPrefillTime, c(0.15))
			add(registration.QueryTPOTBuckets, c(1), "le", "0.05")
			if v.model == "acme/pd" {
				add(registration.QueryAvgKVTransferTime, c(0.01))
			}
		}
	}
	// The router's and the flow-control series are acme/agg's.
	model := func(extra ...string) map[string]string {
		l := map[string]string{"model_name": "acme/agg", "namespace": wiNS}
		for k := 0; k+1 < len(extra); k += 2 {
			l[extra[k]] = extra[k+1]
		}
		return l
	}
	byStep := func(f func(i int) float64) func(time.Time) (float64, bool) {
		return func(ts time.Time) (float64, bool) {
			i, ok := wiStepOf(ts)
			return f(i), ok
		}
	}
	srv.Add(q(registration.QuerySchedulerQueueSize), promtest.Series{Labels: model(),
		Value: byStep(func(i int) float64 { return float64(i % 3) })})
	srv.Add(q(registration.QuerySchedulerQueueBytes), promtest.Series{Labels: model(),
		Value: byStep(func(i int) float64 { return float64(1000 * (i % 3)) })})
	for _, m := range []string{"acme/agg", "acme/pd"} {
		srv.Add(q(registration.QueryModelArrivalRate), promtest.Series{
			Labels: map[string]string{"namespace": wiNS, "target_model_name": m},
			Value:  byStep(func(i int) float64 { return 1.5 + 0.1*float64(i%4) })})
	}
	rate := func(i int) float64 { return 2 + float64(i%3) }
	srv.Add(q(registration.QuerySLORouterTTFTBuckets),
		promtest.Series{Labels: model("le", "0.5"), Value: byStep(func(i int) float64 { return 0.6 * rate(i) })},
		promtest.Series{Labels: model("le", "1"), Value: byStep(func(i int) float64 { return 0.9 * rate(i) })},
		promtest.Series{Labels: model("le", "+Inf"), Value: byStep(rate)})
	srv.Add(q(registration.QuerySLORouterTPOTSum), promtest.Series{Labels: model(),
		Value: byStep(func(i int) float64 { return rate(i) * (0.03 + 0.001*float64(i%5)) })})
	srv.Add(q(registration.QuerySLORouterTPOTCount), promtest.Series{Labels: model(), Value: byStep(rate)})
	srv.Add(q(registration.QuerySLORouterTPOTBuckets), promtest.Series{Labels: model("le", "0.05"), Value: byStep(rate)})
	srv.Add(q(registration.QueryFlowControlWaitBuckets), promtest.Series{Labels: model("le", "0.01"), Value: byStep(rate)})
	pool := func(extra ...string) map[string]string {
		l := map[string]string{"inference_pool": wiPool}
		for k := 0; k+1 < len(extra); k += 2 {
			l[extra[k]] = extra[k+1]
		}
		return l
	}
	srv.Add(q(registration.QueryFlowControlPoolModels), promtest.Const(pool("model_name", "acme/agg"), 1))
	srv.Add(q(registration.QueryFlowControlPoolSaturation),
		promtest.Series{Labels: pool("stage", "effective"), Value: byStep(func(i int) float64 { return 0.5 + 0.05*float64(i%4) })},
		promtest.Const(pool("stage", "prefill"), 0.4), promtest.Const(pool("stage", "decode"), 0.5))
	srv.Add(q(registration.QueryFlowControlOutcomes), promtest.Series{Labels: pool("outcome", "Dispatched"), Value: byStep(rate)})
	wiKSM(srv)
}

// The Pods' times, in whole seconds as both the API server and
// kube-state-metrics keep them.
var (
	wiCreated = wiStart.Add(-31 * time.Minute)
	wiReady   = wiStart.Add(-30 * time.Minute)
)

// wiKSM is the kube-state-metrics series of the fleet's Pods, nodes and
// Deployments.
func wiKSM(srv *promtest.Server) {
	k := whatifsource.KSMQueries(wiNS)
	unix := func(t time.Time) float64 { return float64(t.Unix()) }
	srv.Add(k.NodeLabels, promtest.Const(map[string]string{"node": "n1", "label_nvidia_com_gpu_product": wiAccel}, 1))
	for _, v := range wiFleet() {
		rs := v.name + "-" + v.hash
		srv.Add(k.ReplicaSetOwner, promtest.Const(map[string]string{"namespace": wiNS, "replicaset": rs,
			"owner_kind": "Deployment", "owner_name": v.name}, 1))
		srv.Add(k.DeploymentReplicas, promtest.Series{Labels: map[string]string{"namespace": wiNS, "deployment": v.name},
			Value: func(ts time.Time) (float64, bool) {
				if i, ok := wiStepOf(ts); ok && v.isParked(i) {
					return 0, true
				}
				return float64(v.pods), true
			}})
		for j := range v.pods {
			l := func(extra ...string) map[string]string {
				m := map[string]string{"namespace": wiNS, "pod": v.pod(j)}
				for x := 0; x+1 < len(extra); x += 2 {
					m[extra[x]] = extra[x+1]
				}
				return m
			}
			srv.Add(k.PodNode, promtest.Const(l("node", "n1"), 1))
			labels := []string{}
			if v.role != "" {
				labels = append(labels, "label_llm_d_ai_role", v.role)
			}
			srv.Add(k.PodLabels, promtest.Const(l(labels...), 1))
			srv.Add(k.PodOwner, promtest.Const(l("owner_kind", "ReplicaSet", "owner_name", rs), 1))
			srv.Add(k.PodRunning, promtest.Const(l(), 1))
			srv.Add(k.PodReady, promtest.Const(l(), 1))
			srv.Add(k.PodReadyTime, promtest.Const(l(), unix(wiReady)))
			srv.Add(k.PodCreated, promtest.Const(l(), unix(wiCreated)))
			srv.Add(k.PodStarted, promtest.Const(l(), unix(wiCreated)))
			srv.Add(k.PodGPUs, promtest.Const(map[string]string{"pod": v.pod(j)}, float64(v.gpus)))
		}
	}
	// The router's Pod: Ready, no GPU, no KV series; its own Deployment.
	r := func(extra ...string) map[string]string {
		m := map[string]string{"namespace": wiNS, "pod": wiRouterPod}
		for x := 0; x+1 < len(extra); x += 2 {
			m[extra[x]] = extra[x+1]
		}
		return m
	}
	srv.Add(k.ReplicaSetOwner, promtest.Const(map[string]string{"namespace": wiNS, "replicaset": "agg-a-epp-3e1",
		"owner_kind": "Deployment", "owner_name": "agg-a-epp"}, 1))
	srv.Add(k.DeploymentReplicas, promtest.Const(map[string]string{"namespace": wiNS, "deployment": "agg-a-epp"}, 1))
	srv.Add(k.PodNode, promtest.Const(r("node", "n1"), 1))
	srv.Add(k.PodLabels, promtest.Const(r(), 1))
	srv.Add(k.PodOwner, promtest.Const(r("owner_kind", "ReplicaSet", "owner_name", "agg-a-epp-3e1"), 1))
	srv.Add(k.PodRunning, promtest.Const(r(), 1))
	srv.Add(k.PodReady, promtest.Const(r(), 1))
	srv.Add(k.PodReadyTime, promtest.Const(r(), unix(wiReady)))
	srv.Add(k.PodCreated, promtest.Const(r(), unix(wiCreated)))
	srv.Add(k.PodStarted, promtest.Const(r(), unix(wiCreated)))
}

// wiPods is the fleet's Pods as the API server holds them.
func wiPods() []client.Object {
	out := make([]client.Object, 0, 16)
	for _, v := range wiFleet() {
		for j := range v.pods {
			out = append(out, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: v.pod(j), Namespace: wiNS, CreationTimestamp: metav1.NewTime(wiCreated),
					Labels: map[string]string{"pod-template-hash": v.hash}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &metav1.Time{Time: wiCreated},
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue,
						LastTransitionTime: metav1.NewTime(wiReady)}}},
			})
		}
	}
	return out
}

// wiDeployment is a variant's Deployment, as a capture describes it from
// its flags (whatifsource scaleTargetOf).
func wiDeployment(v wiVariant) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: v.name, Namespace: wiNS},
		Spec: appsv1.DeploymentSpec{Replicas: utils.Ptr(int32(v.pods)), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"nvidia.com/gpu.product": wiAccel},
			Containers: []corev1.Container{{Name: "vllm", Image: "vllm/vllm-openai", Command: []string{"vllm", "serve", v.model},
				Args: wiArgs, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse(strconv.Itoa(v.gpus))}}}},
		}}},
		// Settled: a capture cannot see a rollout, and files none.
		Status: appsv1.DeploymentStatus{Replicas: int32(v.pods), ReadyReplicas: int32(v.pods), UpdatedReplicas: int32(v.pods)},
	}
}

// wiLocator attributes each Pod to its variant, as the owner walk would.
type wiLocator struct{ variants map[string]string }

func (l wiLocator) Locate(_ context.Context, ns, pod string) (*locator.ManagedScaler, error) {
	if v := l.variants[pod]; v != "" {
		return &locator.ManagedScaler{Namespace: ns, Name: v}, nil
	}
	return nil, nil
}

func (wiLocator) ResolveScaleTarget(context.Context, string, string) (autoscalingv2.CrossVersionObjectReference, bool, error) {
	return autoscalingv2.CrossVersionObjectReference{}, false, nil
}

func (wiLocator) GetPodLabels(context.Context, string, string) map[string]string { return nil }

// atClock asks every instant query at the clock's instant, not the wall
// clock's: the cycle's instant, as a controller running then would have.
type atClock struct {
	promv1.API
	clk *clocktesting.FakePassiveClock
}

func (a atClock) Query(ctx context.Context, q string, _ time.Time, opts ...promv1.Option) (model.Value, promv1.Warnings, error) {
	return a.API.Query(ctx, q, a.clk.Now(), opts...)
}

// recordWindow runs a controller over the fleet, recorded, one cycle per
// step, and returns its recording and its settle tracker's verdicts. At
// step failAt, when not negative, the cycle fails before it begins, as an
// optimize that returns an error before beginSLOCycle does, and the
// failure lasts an interval: the next cycle begins twice
// (beginSLOCycle's catch-up).
func recordWindow(t *testing.T, prom promv1.API, failAt int) ([]byte, map[settle.Reason]int64) {
	t.Helper()
	ctx := context.Background()
	base := clocktesting.NewFakePassiveClock(wiStart)
	policy := wiPolicy(t)
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfigForNamespace(wiNS, map[string]config.ScalingPolicy{config.GlobalDefaultsKey: policy})
	src := promsource.NewPrometheusSource(ctx, atClock{API: prom, clk: base}, promsource.DefaultPrometheusSourceConfig())
	defer src.Close()
	registry := source.NewSourceRegistry()
	registry.MustRegister("prometheus", src)
	k8s := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(wiPods()...).Build()
	e := newEngine(k8s, k8s, clientgoscheme.Scheme, nil, registry, cfg, allocation.NewNoOpLimiter("test"), base)
	registration.RegisterAlwaysOnQueries(registry)

	variants := map[string]string{}
	for _, v := range wiFleet() {
		for j := range v.pods {
			variants[v.pod(j)] = v.name
		}
	}
	coll := collector.NewReplicaMetricsCollector(src, k8s, k8s, nil, wiLocator{variants: variants})
	coll.SetClock(base)
	e.ReplicaMetricsCollector = coll
	// The slo analyzer judges with the collector, as NewEngine builds it.
	sat := e.saturationAnalyzer().(*saturation.SaturationAnalyzer)
	judging := slo.New(coll, e.capacityStore).WithClock(e.clock).WithOptimizationInterval(cfg.OptimizationInterval()).
		WithStateTimeout(sat.Horizons().VariantTimeout).WithSlopeHoldFor(sat.Horizons().BucketRetention).
		WithSettleView(e.settleTracker())
	e.sloAnalyzer = judging
	e.analyzers.replaceBuiltin(domain.SLOAnalyzerName, judging)
	require.NoError(t, e.RegisterAnalyzer(throughput.AnalyzerName, e.NewThroughputAnalyzer()))
	var buf bytes.Buffer
	e.recordLearners(learnrec.New(&buf, 0))

	byModel := map[string][]wiVariant{}
	for _, v := range wiFleet() {
		byModel[v.model] = append(byModel[v.model], v)
	}
	models := slices.Sorted(maps.Keys(byModel))
	for i := range wiSteps {
		if i == failAt {
			// beginSLOCycle reads the failure's age off the wall clock.
			e.sloFailedSince = time.Now().Add(-time.Hour)
			continue
		}
		wiCycle(t, ctx, e, base, wiStart.Add(time.Duration(i)*wiStep), models, byModel)
	}
	require.NoError(t, e.learnRec.Err())
	return buf.Bytes(), e.settleTracker().Verdicts()
}

// wiCycle is one engine cycle in optimize's order (engine.go optimize and
// optimizeV2, engine_v2.go runAnalyzers), the rows collected from
// Prometheus as prepareModelData collects them.
func wiCycle(t *testing.T, ctx context.Context, e *Engine, base *clocktesting.FakePassiveClock, at time.Time,
	models []string, byModel map[string][]wiVariant) {
	t.Helper()
	base.SetTime(at)
	e.beginCollectorCycle(nil)
	defer e.ReplicaMetricsCollector.EndCycle()
	e.beginSLOCycle()
	defer e.clock.Pin()()
	step, _ := wiStepOf(at)
	// The model groups are the variants with replicas; a parked variant is
	// in none (engine.go ActiveVariants).
	groups := map[string][]llmdvariant.Variant{}
	parked := map[string][]string{}
	activeKeys := map[string]bool{}
	var active []string
	for _, m := range models {
		for _, v := range byModel[m] {
			if v.isParked(step) {
				parked[m] = append(parked[m], v.name)
				continue
			}
			groups[utils.GetNamespacedKey(wiNS, m)] = append(groups[utils.GetNamespacedKey(wiNS, m)],
				llmdvariant.Variant{
					ObjectMeta: metav1.ObjectMeta{Name: v.name, Namespace: wiNS},
					Spec: llmdvariant.Spec{ModelID: m, MaxReplicas: 8,
						ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: v.name, APIVersion: "apps/v1"}},
				})
		}
		if len(groups[utils.GetNamespacedKey(wiNS, m)]) > 0 {
			activeKeys[utils.GetNamespacedKey(wiNS, m)] = true
			active = append(active, m)
		}
	}
	// optimizeV2's prune, before the presence stamps.
	e.retainSettle(activeKeys)
	e.notePresentVariants(ctx, groups)
	// notePresentVariants stamps the parked variants it lists from the
	// variant registry, which this harness has none of; it stamps them
	// here, after the active ones and in model order, as it would.
	for _, m := range models {
		if names := parked[m]; len(names) > 0 {
			slices.Sort(names)
			if noter := e.analyzers.saturationCaps().noter; noter != nil {
				noter.NoteVariantsPresent(wiNS, m, names)
			}
			if e.sloAnalyzer != nil {
				e.sloAnalyzer.NoteVariantsPresent(wiNS, m, names)
			}
			for _, entry := range e.analyzers.builtins() {
				if a, ok := entry.analyzer.(*throughput.ThroughputAnalyzer); ok {
					a.NoteVariantsPresent(wiNS, m, names)
				}
			}
		}
	}
	e.evictStaleLearnedState(ctx)
	policy := config.ResolveScalingPolicy(map[string]config.ScalingPolicy{config.GlobalDefaultsKey: wiPolicy(t)}, "", wiNS)
	for _, m := range active {
		targets := map[string]scaletarget.ScaleTargetAccessor{}
		vas := map[string]*llmdvariant.Variant{}
		states := make([]domain.VariantReplicaState, 0, len(byModel[m]))
		for _, va := range groups[utils.GetNamespacedKey(wiNS, m)] {
			vas[va.Name] = &va
		}
		for _, v := range byModel[m] {
			if v.isParked(step) {
				continue
			}
			targets[utils.GetNamespacedKey(wiNS, v.name)] = scaletarget.NewDeploymentAccessor(wiDeployment(v))
			states = append(states, domain.VariantReplicaState{VariantName: v.name, CurrentReplicas: v.pods,
				DesiredReplicas: v.pods, GPUsPerReplica: v.gpus, Role: v.role, AcceleratorName: wiAccel})
		}
		slices.SortFunc(states, func(a, b domain.VariantReplicaState) int { return strings.Compare(a.VariantName, b.VariantName) })
		rows, err := e.ReplicaMetricsCollector.CollectReplicaMetrics(ctx, m, wiNS, targets, vas, nil)
		require.NoError(t, err)
		if len(rows) == 0 {
			e.onNothingObserved(wiNS, m)
			continue
		}
		slices.SortFunc(rows, func(a, b domain.ReplicaMetrics) int {
			if c := strings.Compare(a.VariantName, b.VariantName); c != 0 {
				return c
			}
			return strings.Compare(a.PodName, b.PodName)
		})
		sq := e.ReplicaMetricsCollector.CollectSchedulerQueueMetrics(ctx, m, wiNS)
		ar := e.ReplicaMetricsCollector.CollectModelArrivalRate(ctx, m, wiNS)
		_, err = e.runAnalyzers(ctx, m, wiNS, rows, policy, states, nil, targets, vas, sq, ar)
		require.NoError(t, err)
	}
}

// generateWindow captures the fleet's window, each range query in parts of
// at most points steps (0 for the default), and plays it with Generate.
func generateWindow(t *testing.T, prom promv1.API, points int, roleOf ...string) (*learnreplay.Report, []byte) {
	t.Helper()
	policy := wiPolicy(t)
	opts := whatifsource.Options{
		Prometheus: prom, Namespace: wiNS, From: wiStart, To: wiStart.Add((wiSteps - 1) * wiStep), Step: wiStep,
		KubeStateMetrics: true, Policy: &policy, EngineArgs: wiArgs, RangeQueryPoints: points,
	}
	for _, r := range roleOf {
		parsed, err := whatifsource.ParseRoleOf(r)
		require.NoError(t, err)
		opts.RoleOf = append(opts.RoleOf, parsed)
	}
	var buf bytes.Buffer
	_, err := whatifsource.Capture(context.Background(), opts, &buf)
	require.NoError(t, err)
	r, err := gzip.NewReader(&buf)
	require.NoError(t, err)
	capture, err := io.ReadAll(r)
	require.NoError(t, err)
	rep, err := learnreplay.Replay(context.Background(), capture, learnreplay.Options{Mode: learnreplay.Generate})
	require.NoError(t, err)
	return rep, capture
}

// wiEvents is a recording's events, the start left out.
func wiEvents(t *testing.T, recording []byte) []learnrec.RawEvent {
	t.Helper()
	read, err := learnrec.Read(recording)
	require.NoError(t, err)
	require.NotEmpty(t, read.Events)
	require.Equal(t, learnrec.KindStart, read.Events[0].Kind)
	return read.Events[1:]
}

func wiPromAPI(t *testing.T, srv *promtest.Server) promv1.API {
	t.Helper()
	c, err := api.NewClient(api.Config{Address: srv.URL})
	require.NoError(t, err)
	return promv1.NewAPI(c)
}

// wiRun records the controller over the fleet's window and generates the
// same window from a capture, its range queries in parts of at most points
// steps: the engine's recording, Generate's, and the fake Prometheus.
func wiRun(t *testing.T, points int, roleOf ...string) (recorded, generated []byte, srv *promtest.Server) {
	t.Helper()
	srv, prom := wiProm(t)
	recorded, verdicts := recordWindow(t, prom, -1)
	rep, _ := generateWindow(t, prom, points, roleOf...)
	return wiCheckRun(t, recorded, verdicts, rep), rep.Recording, srv
}

// wiProm is the fake Prometheus serving the fleet's series.
func wiProm(t *testing.T) (*promtest.Server, promv1.API) {
	t.Helper()
	srv := promtest.New()
	t.Cleanup(srv.Close)
	reg := source.NewSourceRegistry()
	require.NoError(t, reg.Register("prometheus", source.NewNoOpSource()))
	registration.RegisterSaturationQueries(reg)
	registration.RegisterQueueingModelQueries(reg)
	registration.RegisterSLOQueries(reg)
	registration.RegisterFlowControlPoolQueries(reg)
	registration.RegisterAlwaysOnQueries(reg)
	wiSeries(t, srv, func(name string) string {
		params := map[string]string{source.ParamNamespace: wiNS}
		if qt := reg.Get("prometheus").QueryList().Get(name); qt != nil && len(qt.Params) == 0 {
			params = nil
		}
		s, err := reg.Get("prometheus").QueryList().Build(name, params)
		require.NoError(t, err, name)
		return s
	})
	return srv, wiPromAPI(t, srv)
}

// wiCheckRun holds the engine's run and Generate's to the fleet's, and a
// Fidelity replay of the engine's recording to its settle verdicts.
func wiCheckRun(t *testing.T, recorded []byte, verdicts map[settle.Reason]int64, rep *learnreplay.Report) []byte {
	t.Helper()

	// The run exercised what it is meant to: every analyzer judged, the slo
	// one with the router, lines published, and a model held.
	run := map[string]int{}
	for _, ev := range wiEvents(t, recorded) {
		run[ev.Kind]++
	}
	// Two models every cycle, acme/idle on its first, and acme/park when it
	// is not parked.
	analyzed := 2*wiSteps + 1 + wiSteps - (wiParkedTo - wiParkedFrom + 1)
	require.Equal(t, analyzed, run[saturation.KindAnalyze])
	require.Equal(t, analyzed, run[slo.KindAnalyze], "every analyzed model lists slo")
	require.Equal(t, analyzed, run[throughput.KindAnalyze])
	require.Positive(t, run[saturation.KindPublishLine], "no ITL line was published")
	require.Equal(t, wiSteps-1, run[settle.KindHold], "acme/idle is held on every cycle after its first")

	// The settle tracker reached the same verdicts in both, which the
	// recording does not show: every replica joined once, and acme/park's
	// again after its parked steps, forgotten by the prune.
	pods := 0
	for _, v := range wiFleet() {
		pods += v.pods
	}
	require.Equal(t, int64(pods+1), verdicts[settle.Joined], "acme/park's replica did not join again")
	for _, r := range settle.Reasons() {
		require.Equal(t, verdicts[r], rep.Paths["settle-"+string(r)], "settle verdict %s", r)
	}
	// And a replay of the engine's own recording reaches them too: the
	// engine records no event for its prune, and the replay forgets the
	// parked model's record where the engine did.
	replayed, err := learnreplay.Replay(context.Background(), recorded, learnreplay.Options{Mode: learnreplay.Fidelity})
	require.NoError(t, err)
	require.Empty(t, replayed.Failed())
	for _, r := range settle.Reasons() {
		require.Equal(t, verdicts[r], replayed.Paths["settle-"+string(r)], "replayed settle verdict %s", r)
	}
	return recorded
}

// A cycle that failed for an interval is the next cycle begun twice
// (beginSLOCycle's catch-up), which the replay counts as one: acme/park,
// observed before the failure and after it, kept its settle record in the
// engine, whose prune did not run while the cycle failed, and keeps it in
// the replay. Counted as two cycles, the replay would forget it and its
// replica would join again. Generate is not compared: a capture plays
// every step, and has no failed cycle.
func TestWhatifCapture_ReplayCountsACatchUpCycleOnce(t *testing.T) {
	_, prom := wiProm(t)
	const failAt = 2 // acme/park has replicas on both sides
	require.Less(t, failAt+1, wiParkedFrom)
	recorded, verdicts := recordWindow(t, prom, failAt)

	begun := map[time.Time]int{}
	for _, ev := range wiEvents(t, recorded) {
		if ev.Kind == slo.KindBeginCycle {
			begun[ev.Now]++
		}
	}
	require.Len(t, begun, wiSteps-1, "the failed step began no cycle")
	require.Equal(t, 2, begun[wiStart.Add((failAt+1)*wiStep)], "the cycle after the failure begins twice")

	replayed, err := learnreplay.Replay(context.Background(), recorded, learnreplay.Options{Mode: learnreplay.Fidelity})
	require.NoError(t, err)
	require.Empty(t, replayed.Failed())
	pods := 0
	for _, v := range wiFleet() {
		pods += v.pods
	}
	require.Equal(t, int64(pods+1), verdicts[settle.Joined], "only acme/park's parked steps join it again")
	for _, r := range settle.Reasons() {
		require.Equal(t, verdicts[r], replayed.Paths["settle-"+string(r)], "replayed settle verdict %s", r)
	}
}

func TestWhatifCapture_GenerateReproducesTheControllersRun(t *testing.T) {
	recorded, generated, _ := wiRun(t, 0)
	wiSameRun(t, recorded, generated)
}

// A window asked in parts (whatifsource maxPointsPerQuery, set small here)
// is joined with no step lost or doubled: the same run as asked whole.
func TestWhatifCapture_GenerateReproducesTheControllersRunAcrossSplitRangeQueries(t *testing.T) {
	const points = 5
	recorded, generated, srv := wiRun(t, points)
	node := whatifsource.KSMQueries(wiNS).PodNode
	var parts []promtest.Request
	for _, r := range srv.Requests() {
		if r.Path == "/api/v1/query_range" && r.Query == node {
			parts = append(parts, r)
		}
	}
	require.Len(t, parts, (wiSteps+points-1)/points, "the window was not split")
	wiSameRun(t, recorded, generated)
}

// A capture whose role map is --role-of by Pod name: app=agg-a also picks
// the router's Pod (wiRouterPod), which has no KV series and is not a
// replica, so the variant's replicas are still the engine's.
func TestWhatifCapture_GenerateReproducesTheControllersRunWithRolesByName(t *testing.T) {
	roles := make([]string, 0, len(wiFleet()))
	for _, v := range wiFleet() {
		roles = append(roles, v.role+"=app="+v.name)
	}
	recorded, generated, _ := wiRun(t, 0, roles...)
	wiSameRun(t, recorded, generated)
}

// wiSameRun holds a generated run to the controller's recorded one.
func wiSameRun(t *testing.T, recorded, generated []byte) {
	t.Helper()
	// Every event, in order: the same kind at the same instant with the same
	// data. Each Analyze carries its whole input, its result and the store
	// records it read, so this holds the captured input to the collected
	// one and the generated result to the recorded one, field for field.
	rec, gen := wiEvents(t, recorded), wiEvents(t, generated)
	for i := range min(len(rec), len(gen)) {
		if d := wiEventDiff(rec[i], gen[i]); d != "" {
			t.Fatalf("event %d (step %d) differs: %s", i, wiStepIndex(rec[i].Now), d)
		}
	}
	require.Equal(t, len(rec), len(gen), "the engine recorded %d events, Generate %d", len(rec), len(gen))

	// And so the goldens' outputs are the same.
	want, err := learnreplay.Outputs(recorded)
	require.NoError(t, err)
	got, err := learnreplay.Outputs(generated)
	require.NoError(t, err)
	for _, name := range learnreplay.OutputNames() {
		assert.Equal(t, string(want[name]), string(got[name]), "output %s", name)
	}
	assert.NotEmpty(t, want[learnreplay.OutputResults])
	assert.NotEmpty(t, want[learnreplay.OutputLines])
}

// Generate's call order is the engine's, cycle by cycle: the slo cycle, the
// presence stamps, the three sweeps, then per model the settle tracker,
// each variant's accelerator and scale target, and the analyzers. Compared
// as the kinds each recorded at each instant, so a change of order names
// itself even where it would change no value.
func TestWhatifCapture_GenerateCallsInTheEnginesOrder(t *testing.T) {
	recorded, generated, _ := wiRun(t, 0)
	want, got := wiKindsByStep(t, recorded), wiKindsByStep(t, generated)
	require.Len(t, want, wiSteps)
	for i := range want {
		require.Equal(t, want[i], got[i], "step %d", i)
	}
	assert.Len(t, got, len(want))
	assert.Equal(t, slo.KindBeginCycle, want[0][0], "the slo cycle opens the step")
}

// wiKindsByStep is a recording's event kinds, one list per clock reading.
func wiKindsByStep(t *testing.T, recording []byte) [][]string {
	t.Helper()
	var out [][]string
	var at time.Time
	for _, ev := range wiEvents(t, recording) {
		if len(out) == 0 || !ev.Now.Equal(at) {
			out = append(out, nil)
			at = ev.Now
		}
		out[len(out)-1] = append(out[len(out)-1], ev.Kind)
	}
	return out
}

// wiStepIndex is the step an instant is, -1 off the grid.
func wiStepIndex(at time.Time) int {
	if i, ok := wiStepOf(at); ok {
		return i
	}
	return -1
}

// wiEventDiff says how two events differ, "" when they do not.
func wiEventDiff(want, got learnrec.RawEvent) string {
	switch {
	case want.Kind != got.Kind:
		return fmt.Sprintf("the engine recorded %s, Generate %s", want.Kind, got.Kind)
	case !want.Now.Equal(got.Now):
		return fmt.Sprintf("%s at %s, generated at %s", want.Kind, want.Now, got.Now)
	case string(want.Data) == string(got.Data):
		return ""
	}
	w, g := string(want.Data), string(got.Data)
	i := 0
	for i < len(w) && i < len(g) && w[i] == g[i] {
		i++
	}
	clip := func(s string) string { return s[max(0, i-200):min(len(s), i+120)] }
	return fmt.Sprintf("%s at byte %d:\n engine   ...%s...\n generate ...%s...", want.Kind, i, clip(w), clip(g))
}
