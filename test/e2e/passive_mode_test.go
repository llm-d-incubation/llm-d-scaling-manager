package e2e

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// Passive mode on a real KEDA, a real HPA and the real controller
// (docs/reference/scaling-policy.md, "passive"; docs/proposals/passive-mode.md).
//
// The unit tests drive a model of the HPA. These specs drive the HPA itself, and
// every one asserts the same thing: the Deployment's spec.replicas never changes
// over a window long enough for the HPA to act, while the controller WOULD
// change it. "Would" is measured, not assumed: before and during each window
// llmsm_desired_replicas must differ from the held count, in the direction the
// spec names. A window where the controller wanted nothing would prove nothing.
//
// The operating point is the one the tier and V2 suites use: the simulator's
// --fake-metrics pins kv-cache-usage at 0.3, so the would-be count is a pure
// function of the model's thresholds. scaleUpThreshold 0.30 wants one more
// replica at any count; 0.95/0.85 wants one fewer.
//
// The window is passiveHoldWindow: three HPA syncs (15 s each) plus the
// scale-down stabilization, which every ScaledObject here shortens to
// passiveScaleDownWindowSec through KEDA's advanced HPA behavior, plus a sync
// of margin. A scale-down the controller wanted would have landed inside it.
//
// Every model but the ones parked at zero runs outside the serving
// InferencePool (fixtures.WithPoolGuide): a held model at three replicas would
// otherwise answer the requests a scale-from-zero spec needs to queue.

const (
	// passiveFakeMetrics fixes what the simulator reports, so the would-be
	// count follows from the thresholds alone.
	passiveFakeMetrics = `{"kv-cache-usage":0.3,"running-requests":1,"waiting-requests":0}`

	passiveKVThreshold    = 0.80
	passiveQueueThreshold = 50

	// The scale-down stabilization every ScaledObject here sets, in seconds.
	passiveScaleDownWindowSec = 30
	// passiveHoldWindow: 3 HPA syncs + the scale-down window + one sync.
	passiveHoldWindow = 90 * time.Second

	// passiveGuide keeps a held model out of the serving InferencePool.
	passiveGuide = "passive-e2e-pool"

	// The manager ConfigMap that carries LLMSM_PASSIVE, read at start.
	passiveManagerConfigMap = "scaling-manager-manager-config"

	// Load pods: pods KEDA's kubernetes-workload trigger counts.
	passiveLoadLabel  = "e2e.llm-d.ai/passive-observe-load"
	passiveLoadPrefix = "passive-obs-load"
)

// passiveDirection is which way the controller would move a model.
type passiveDirection int

const (
	wouldScaleUp passiveDirection = iota
	wouldScaleDown
)

func (d passiveDirection) String() string {
	if d == wouldScaleUp {
		return "up"
	}
	return "down"
}

// thresholds are the scaleUpThreshold/scaleDownBoundary pair that makes the
// controller want to move a model at passiveFakeMetrics in direction d.
func (d passiveDirection) thresholds() (scaleUp, scaleDown float64) {
	if d == wouldScaleUp {
		return 0.30, 0.20
	}
	return 0.95, 0.85
}

// wants reports whether a would-be count moves away from held in direction d.
func (d passiveDirection) wants(desired float64, held int32) bool {
	if desired < 0 || math.IsNaN(desired) {
		return false
	}
	if d == wouldScaleUp {
		return desired > float64(held)
	}
	return desired < float64(held)
}

// passiveModel is one simulated model the specs scale or hold.
type passiveModel struct {
	// name is the model service's base name; the Deployment is name-decode.
	name string
	// short is the suffix of the model's ID (suiteModelID).
	short string
	// soBase is the ScaledObject's base name; the object is soBase-so.
	soBase string
	// policyKey is the key of the model's per-model scaling-policy entry.
	policyKey string
}

func newPassiveModel(short string) passiveModel {
	return passiveModel{
		name:      "passive-" + short + "-ms",
		short:     "passive-" + short,
		soBase:    "passive-" + short,
		policyKey: "passive-" + short + "-model",
	}
}

func (m passiveModel) deployment() string { return m.name + "-decode" }

// modelID is read when called, not when the container tree is built: the
// configured base (MODEL_ID) is loaded in BeforeSuite.
func (m passiveModel) modelID() string { return suiteModelID(m.short) }

func (m passiveModel) scaledObject() string { return fixtures.ScaledObjectNameFor(m.soBase) }

// policy writes the model's per-model entry: thresholds that want direction d,
// and passive when it is set.
func (m passiveModel) policy(d passiveDirection, passive *bool) {
	GinkgoHelper()
	up, down := d.thresholds()
	entry := buildSaturationConfigYAMLWithModel(passiveKVThreshold, passiveQueueThreshold, up, down, m.modelID(), cfg.LLMDNamespace)
	if passive != nil {
		entry += fmt.Sprintf("passive: %t\n", *passive)
	}
	Expect(upsertSaturationConfigEntry(ctx, cfg.LLMSMNamespace, scalingPolicyConfigMapName(), m.policyKey, entry)).To(Succeed())
	GinkgoWriter.Printf("PASSIVE-EVIDENCE policy %s: would scale %s, passive=%v\n", m.modelID(), d, ptrString(passive))
}

func ptrString(b *bool) string {
	if b == nil {
		return "inherit"
	}
	return strconv.FormatBool(*b)
}

// create stands the model up at replicas, outside the serving pool unless
// inPool, and waits for every replica to be Ready.
func (m passiveModel) create(replicas int32, inPool bool) {
	GinkgoHelper()
	_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, m.soBase)
	_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, m.name)
	var opts []fixtures.ModelServiceOption
	if !inPool {
		opts = append(opts, fixtures.WithPoolGuide(passiveGuide))
	}
	Expect(fixtures.CreateModelServiceWithExtraArgs(ctx, k8sClient, cfg.LLMDNamespace, m.name, m.name+"-pool", m.modelID(),
		cfg.UseSimulator, cfg.MaxNumSeqs, []string{"--fake-metrics", passiveFakeMetrics}, opts...)).To(Succeed())
	Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, m.name, m.deployment(), 8000)).To(Succeed())
	Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, m.name, m.deployment())).To(Succeed())
	m.scale(replicas)
	if replicas > 0 {
		m.waitReady(replicas)
	}
}

// remove deletes everything create made.
func (m passiveModel) remove() {
	_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, m.soBase)
	_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: m.name + "-monitor", Namespace: cfg.MonitoringNS},
	})
	_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, m.name+"-service", metav1.DeleteOptions{})
	_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, m.name)
	_ = deleteSaturationConfigEntry(ctx, cfg.LLMSMNamespace, scalingPolicyConfigMapName(), m.policyKey)
}

// register creates the model's ScaledObject with the scaling manager's trigger (plus opts)
// and, for a trigger KEDA wires an HPA for, waits for the external metric.
func (m passiveModel) register(minReplicas, maxReplicas int32, opts ...fixtures.ScaledObjectOption) {
	GinkgoHelper()
	all := append([]fixtures.ScaledObjectOption{
		fixtures.WithScalingManagerTriggerMetadata(m.modelID(), "30.0"),
		fixtures.WithExternalScalerTrigger(externalScalerAddress()),
		fixtures.WithScaledObjectScaleDownStabilizationWindow(passiveScaleDownWindowSec),
	}, opts...)
	Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, m.soBase, m.deployment(), m.scaledObject(),
		minReplicas, maxReplicas, cfg.MonitoringNS, all...)).To(Succeed())
}

// scale is `kubectl scale`: a write to the Deployment's scale subresource.
func (m passiveModel) scale(n int32) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).UpdateScale(ctx, m.deployment(),
			&autoscalingv1.Scale{
				ObjectMeta: metav1.ObjectMeta{Name: m.deployment(), Namespace: cfg.LLMDNamespace},
				Spec:       autoscalingv1.ScaleSpec{Replicas: n},
			}, metav1.UpdateOptions{})
		g.Expect(err).NotTo(HaveOccurred())
	}, 30*time.Second, time.Second).Should(Succeed())
	GinkgoWriter.Printf("PASSIVE-EVIDENCE %s kubectl scale --replicas=%d at %s\n", m.deployment(), n, time.Now().UTC().Format(time.RFC3339))
}

// specReplicas reads the Deployment's spec.replicas.
func (m passiveModel) specReplicas() int32 {
	GinkgoHelper()
	dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, m.deployment(), metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return ptr.Deref(dep.Spec.Replicas, 1)
}

func (m passiveModel) waitReady(n int32) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, m.deployment(), metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(dep.Status.ReadyReplicas).To(Equal(n))
	}, time.Duration(cfg.PodReadyTimeout)*time.Second, 2*time.Second).Should(Succeed(),
		"%s did not reach %d ready replicas", m.deployment(), n)
}

// waitSpec waits for spec.replicas to satisfy match.
func (m passiveModel) waitSpec(what string, match func(int32) bool, timeout time.Duration) {
	GinkgoHelper()
	var last int32
	Eventually(func(g Gomega) {
		last = m.specReplicas()
		g.Expect(match(last)).To(BeTrue(), "spec.replicas is %d", last)
	}, timeout, time.Second).Should(Succeed(), "%s: spec.replicas never became %s", m.deployment(), what)
	GinkgoWriter.Printf("PASSIVE-EVIDENCE %s spec.replicas=%d (%s) at %s\n", m.deployment(), last, what, time.Now().UTC().Format(time.RFC3339))
}

// passiveProm returns a Prometheus client, failing rather than skipping:
// without llmsm_desired_replicas a hold window cannot show the controller wanted
// to move anything, and a window that shows nothing is not a pass.
func passiveProm() *utils.PrometheusClient {
	GinkgoHelper()
	pc := promClientForCheck()
	Expect(pc).NotTo(BeNil(), "no Prometheus client could be built")
	_, err := pc.QueryWithRetry(ctx, "vector(1)")
	Expect(err).NotTo(HaveOccurred(),
		"Prometheus is not reachable from the test host; run through make test-e2e-passive, which port-forwards it")
	return pc
}

// promValue is one instant query, -1 when the series is absent and NaN when
// the query failed.
func promValue(pc *utils.PrometheusClient, query string) float64 {
	v, err := pc.QueryWithRetry(ctx, "("+query+") or vector(-1)")
	if err != nil {
		return math.NaN()
	}
	return v
}

// metricsOf reads the model's would-be count, its current count as the
// controller sees it, and its llmsm_passive_mode for mode.
func (m passiveModel) metricsOf(pc *utils.PrometheusClient, mode string) (desired, current, modeOn float64) {
	sel := fmt.Sprintf(`exported_namespace=%q,variant_name=%q`, cfg.LLMDNamespace, m.scaledObject())
	desired = promValue(pc, "max(llmsm_desired_replicas{"+sel+"})")
	current = promValue(pc, "max(llmsm_current_replicas{"+sel+"})")
	modeOn = promValue(pc, fmt.Sprintf(`max(llmsm_passive_mode{%s,mode=%q})`, sel, mode))
	return desired, current, modeOn
}

// waitWouldBe waits until the controller publishes a would-be count that
// moves away from held in direction d, with the variant in mode.
func (m passiveModel) waitWouldBe(pc *utils.PrometheusClient, held int32, d passiveDirection, mode string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		desired, current, on := m.metricsOf(pc, mode)
		g.Expect(d.wants(desired, held)).To(BeTrue(),
			"llmsm_desired_replicas=%v, current=%v: the controller does not yet want to scale %s from %d", desired, current, d, held)
		g.Expect(on).To(Equal(1.0), "llmsm_passive_mode{mode=%q} is %v", mode, on)
	}, 4*time.Minute, 5*time.Second).Should(Succeed())
}

// replicaSample is one read of the Deployment.
type replicaSample struct {
	at                    time.Duration
	spec, status, updated int32
	ready                 int32
	err                   string
}

// replicaRecorder samples a Deployment every second until stopped.
type replicaRecorder struct {
	deployment string
	start      time.Time
	mu         sync.Mutex
	samples    []replicaSample
	stop       chan struct{}
	done       chan struct{}
}

// recordReplicas samples a Deployment in the model namespace every second.
func recordReplicas(deployment string) *replicaRecorder {
	return recordReplicasIn(cfg.LLMDNamespace, deployment)
}

// recordReplicasIn samples a Deployment in namespace every second.
func recordReplicasIn(namespace, deployment string) *replicaRecorder {
	r := &replicaRecorder{deployment: deployment, start: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer GinkgoRecover()
		defer close(r.done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			s := replicaSample{at: time.Since(r.start).Round(time.Second)}
			dep, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployment, metav1.GetOptions{})
			if err != nil {
				s.err, s.spec = err.Error(), -1
			} else {
				s.spec = ptr.Deref(dep.Spec.Replicas, 1)
				s.status, s.updated, s.ready = dep.Status.Replicas, dep.Status.UpdatedReplicas, dep.Status.ReadyReplicas
			}
			r.mu.Lock()
			r.samples = append(r.samples, s)
			r.mu.Unlock()
			select {
			case <-r.stop:
				return
			case <-t.C:
			}
		}
	}()
	return r
}

// Stop ends the recording, prints the timeline (every change, and a line
// every 10 s) and returns the samples.
func (r *replicaRecorder) Stop(label string) []replicaSample {
	close(r.stop)
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	GinkgoWriter.Printf("PASSIVE-TIMELINE %s %s from %s (%d samples)\n", label, r.deployment, r.start.UTC().Format(time.RFC3339), len(r.samples))
	var prev replicaSample
	for i, s := range r.samples {
		changed := i == 0 || s.spec != prev.spec || s.status != prev.status || s.ready != prev.ready || s.updated != prev.updated
		if changed || s.at%(10*time.Second) == 0 || i == len(r.samples)-1 {
			GinkgoWriter.Printf("PASSIVE-TIMELINE %s +%-5s spec=%d status=%d updated=%d ready=%d %s\n",
				label, s.at, s.spec, s.status, s.updated, s.ready, s.err)
		}
		prev = s
	}
	return append([]replicaSample(nil), r.samples...)
}

// specValues lists the distinct spec.replicas values seen, in order.
func specValues(samples []replicaSample) []int32 {
	var out []int32
	for _, s := range samples {
		if s.spec < 0 {
			continue
		}
		if len(out) == 0 || out[len(out)-1] != s.spec {
			out = append(out, s.spec)
		}
	}
	return out
}

// metricSample is one read of the controller's published view of a model.
type metricSample struct {
	at                   time.Duration
	desired, current, on float64
	mode                 string
	wantsAway            bool
	hpa                  string
}

// hpaView is what the HPA KEDA built for the model last computed: its
// desiredReplicas and the external metric values KEDA handed it.
func (m passiveModel) hpaView() string {
	hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).Get(ctx,
		"keda-hpa-"+m.scaledObject(), metav1.GetOptions{})
	if err != nil {
		return "hpa: " + err.Error()
	}
	var values []string
	for _, cm := range hpa.Status.CurrentMetrics {
		if cm.External != nil {
			v := cm.External.Current.AverageValue
			if v == nil {
				v = cm.External.Current.Value
			}
			if v != nil {
				values = append(values, cm.External.Metric.Name+"="+v.String())
			}
		}
	}
	return fmt.Sprintf("hpa.desiredReplicas=%d hpa.currentReplicas=%d metrics=%v",
		hpa.Status.DesiredReplicas, hpa.Status.CurrentReplicas, values)
}

// expectHeld watches the model for passiveHoldWindow and fails if spec.replicas is ever
// anything but held. Every 10 s it reads llmsm_desired_replicas,
// llmsm_current_replicas and llmsm_passive_mode{mode}; at least one read must
// show the controller wanting to move the model in direction d, and the last
// must show it in mode. during, when set, runs at the start of the window
// (a restart, a rollout) while the recorder runs.
func (m passiveModel) expectHeld(pc *utils.PrometheusClient, label string, held int32, d passiveDirection, mode string,
	during func()) {
	GinkgoHelper()
	rec := recordReplicas(m.deployment())
	var (
		start   time.Time
		metrics []metricSample
	)
	sample := func() {
		desired, current, on := m.metricsOf(pc, mode)
		metrics = append(metrics, metricSample{at: time.Since(start).Round(time.Second), desired: desired,
			current: current, on: on, mode: mode, wantsAway: d.wants(desired, held), hpa: m.hpaView()})
	}
	if during != nil {
		during()
	}
	// The window starts once the disruption is over, so it covers the HPA's
	// syncs after it as well as the disruption itself.
	start = time.Now()
	for time.Since(start) < passiveHoldWindow {
		sample()
		time.Sleep(10 * time.Second)
	}
	sample()
	samples := rec.Stop(label)
	wanted := false
	for _, s := range metrics {
		GinkgoWriter.Printf("PASSIVE-METRICS %s +%-5s llmsm_desired_replicas=%v llmsm_current_replicas=%v llmsm_passive_mode{mode=%q}=%v would-scale-%s=%v %s\n",
			label, s.at, s.desired, s.current, s.mode, s.on, d, s.wantsAway, s.hpa)
		wanted = wanted || s.wantsAway
	}
	for _, s := range samples {
		Expect(s.spec).To(Equal(held), "%s: spec.replicas moved to %d at +%s; the timeline is above", label, s.spec, s.at)
	}
	Expect(wanted).To(BeTrue(), "%s: the controller never wanted to scale %s from %d during the window, so the hold proved nothing", label, d, held)
	Expect(metrics[len(metrics)-1].on).To(Equal(1.0), "%s: llmsm_passive_mode{mode=%q} was not 1 at the end of the window", label, mode)
}

// expectLog waits for a controller log line holding every fragment, logged
// since since.
func expectLog(since time.Time, timeout time.Duration, fragments ...string) {
	GinkgoHelper()
	var hit string
	Eventually(func(g Gomega) {
		secs := int64(time.Since(since).Seconds()) + 1
		_, logs, err := utils.ControllerLogsContain(ctx, k8sClient, cfg.LLMSMNamespace, utils.ControllerDeploymentName, fragments[0], secs)
		g.Expect(err).NotTo(HaveOccurred())
		hit = ""
		for _, line := range strings.Split(logs, "\n") {
			all := true
			for _, f := range fragments {
				if !strings.Contains(line, f) {
					all = false
					break
				}
			}
			if all {
				hit = line
				break
			}
		}
		g.Expect(hit).NotTo(BeEmpty(), "no controller log line since %s holds all of %q", since.UTC().Format(time.RFC3339), fragments)
	}, timeout, 5*time.Second).Should(Succeed())
	GinkgoWriter.Printf("PASSIVE-LOG %s\n", hit)
}

// logLines returns every controller log line logged from since on that holds every fragment.
func logLines(since time.Time, fragments ...string) []string {
	GinkgoHelper()
	secs := int64(time.Since(since).Seconds()) + 1
	_, logs, err := utils.ControllerLogsContain(ctx, k8sClient, cfg.LLMSMNamespace, utils.ControllerDeploymentName, fragments[0], secs)
	Expect(err).NotTo(HaveOccurred())
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		all := true
		for _, f := range fragments {
			if !strings.Contains(line, f) {
				all = false
				break
			}
		}
		if all {
			out = append(out, line)
		}
	}
	return out
}

// setControllerPassive writes LLMSM_PASSIVE into scaling-manager-manager-config and
// restarts the controller, which reads it only at start. It returns the
// ConfigMap as it was, for restoreManagerConfig.
func setControllerPassive(on bool) *corev1.ConfigMap {
	GinkgoHelper()
	cm, err := k8sClient.CoreV1().ConfigMaps(cfg.LLMSMNamespace).Get(ctx, passiveManagerConfigMap, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	original := cm.DeepCopy()
	settings := map[string]any{}
	Expect(yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &settings)).To(Succeed())
	settings["LLMSM_PASSIVE"] = strconv.FormatBool(on)
	out, err := yaml.Marshal(settings)
	Expect(err).NotTo(HaveOccurred())
	cm.Data["config.yaml"] = string(out)
	_, err = k8sClient.CoreV1().ConfigMaps(cfg.LLMSMNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(restartScalingManagerController(ctx)).To(Succeed())
	GinkgoWriter.Printf("PASSIVE-EVIDENCE LLMSM_PASSIVE=%t written to %s and the controller restarted at %s\n",
		on, passiveManagerConfigMap, time.Now().UTC().Format(time.RFC3339))
	return original
}

// restoreManagerConfig puts scaling-manager-manager-config back and restarts the controller.
func restoreManagerConfig(original *corev1.ConfigMap) {
	if original == nil {
		return
	}
	cm, err := k8sClient.CoreV1().ConfigMaps(cfg.LLMSMNamespace).Get(ctx, passiveManagerConfigMap, metav1.GetOptions{})
	if err != nil {
		GinkgoWriter.Printf("Warning: could not read %s to restore it: %v\n", passiveManagerConfigMap, err)
		return
	}
	cm.Data = original.Data
	if _, err := k8sClient.CoreV1().ConfigMaps(cfg.LLMSMNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		GinkgoWriter.Printf("Warning: could not restore %s: %v\n", passiveManagerConfigMap, err)
		return
	}
	if err := restartScalingManagerController(ctx); err != nil {
		GinkgoWriter.Printf("Warning: could not restart the controller after restoring %s: %v\n", passiveManagerConfigMap, err)
	}
}

// snapshotPolicy saves the scaling-policy ConfigMap and restores it when the
// container ends.
func snapshotPolicy() {
	GinkgoHelper()
	name := scalingPolicyConfigMapName()
	cm, err := k8sClient.CoreV1().ConfigMaps(cfg.LLMSMNamespace).Get(ctx, name, metav1.GetOptions{})
	existed := err == nil
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}
	var original *corev1.ConfigMap
	if existed {
		original = cm.DeepCopy()
	}
	DeferCleanup(func() { restoreSaturationConfigMap(ctx, cfg.LLMSMNamespace, name, original, existed) })
}

// leaderPod is the controller pod holding the leader lease.
func leaderPod() string {
	GinkgoHelper()
	lease, err := k8sClient.CoordinationV1().Leases(cfg.LLMSMNamespace).Get(ctx, "72dd1cf1.llm-d.ai", metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(lease.Spec.HolderIdentity).NotTo(BeNil())
	pod, _, _ := strings.Cut(*lease.Spec.HolderIdentity, "_")
	return pod
}

// loadPod is a pod KEDA's kubernetes-workload trigger counts; it serves nothing.
func loadPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.LLMDNamespace,
			Labels:    map[string]string{passiveLoadLabel: "true", "test-resource": "true"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "pause",
				Image: "registry.k8s.io/pause:3.10",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("5m"),
					corev1.ResourceMemory: resource.MustParse("8Mi"),
				}},
			}},
			TerminationGracePeriodSeconds: ptr.To(int64(0)),
		},
	}
}

// setLoadPods makes exactly n load pods named prefix-<i> exist.
func setLoadPods(n int) {
	GinkgoHelper()
	for i := range 8 {
		name := fmt.Sprintf("%s-%d", passiveLoadPrefix, i)
		if i < n {
			_, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Create(ctx, loadPod(name), metav1.CreateOptions{})
			if err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			continue
		}
		err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0))})
		if err != nil && !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	}
	Eventually(func(g Gomega) {
		pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{LabelSelector: passiveLoadLabel + "=true"})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods.Items).To(HaveLen(n))
	}, 2*time.Minute, time.Second).Should(Succeed())
	GinkgoWriter.Printf("PASSIVE-EVIDENCE %d load pods (%s=true) at %s\n", n, passiveLoadLabel, time.Now().UTC().Format(time.RFC3339))
}

// sendQueuedRequests starts a Job that sends requests for modelID through
// the gateway, which queue in the EPP while nothing serves the model: the
// demand the scale-from-zero engine wakes a model for.
func sendQueuedRequests(name, modelID string) {
	GinkgoHelper()
	requireEmptyServingPool()
	poolRef := cfg.PoolName
	if poolRef == "" {
		poolRef = strings.TrimSuffix(cfg.EPPServiceName, "-epp")
	}
	ioApplied, err := fixtures.EnsureInferenceObjective(ctx, crClient, cfg.LLMDNamespace, poolRef)
	Expect(err).NotTo(HaveOccurred())
	Expect(ioApplied).To(BeTrue(), "the InferenceObjective API is not on this cluster, so nothing queues")
	DeferCleanup(func() { _ = fixtures.DeleteInferenceObjective(ctx, crClient, cfg.LLMDNamespace) })
	job := createScaleFromZeroTriggerJob(name, cfg.LLMDNamespace, discoverInferenceGateway(), modelID)
	_, err = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Create(ctx, job, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		propagation := metav1.DeletePropagationBackground
		_ = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	})
	Eventually(func(g Gomega) {
		pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods.Items).NotTo(BeEmpty())
		g.Expect(pods.Items[0].Status.Phase).To(Equal(corev1.PodRunning))
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	GinkgoWriter.Printf("PASSIVE-EVIDENCE queued requests for %s from job %s at %s\n", modelID, name, time.Now().UTC().Format(time.RFC3339))
}

// requireScaleFromZeroPath skips a spec whose environment has no
// scale-from-zero path, as the scale-from-zero suites do.
func requireScaleFromZeroPath() {
	GinkgoHelper()
	if !cfg.ScaleToZeroEnabled {
		Skip("needs EPP flow-control queuing: set SCALE_TO_ZERO_ENABLED=true")
	}
	if ok, why := eppFlowControlAvailable(ctx, crClient, cfg.LLMSMNamespace, cfg.LLMDNamespace); !ok {
		Skip("EPP flow control is not available, so there is no wake signal to test: " + why)
	}
}

// expectNoWake checks the scale-from-zero engine logged the passive refusal
// for modelID and published no activation for the ScaledObject.
func expectNoWake(since time.Time, modelID, scaledObject string) {
	GinkgoHelper()
	expectLog(since, 2*time.Minute, "Scale-from-zero: passive model has pending requests", modelID)
	wakes := logLines(since, "Published scale-from-zero activation", scaledObject)
	Expect(wakes).To(BeEmpty(), "the scale-from-zero engine published an activation for a passive model: %v", wakes)
}

func boolPtr(b bool) *bool { return &b }

var _ = Describe("Passive mode: hold, controller-wide", Serial, Label("full", "passive"), Ordered, func() {
	m := newPassiveModel("cw")
	var (
		pc       *utils.PrometheusClient
		original *corev1.ConfigMap
	)

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("needs the simulator's --fake-metrics: set USE_SIMULATOR=true")
		}
		pc = passiveProm()
		snapshotPolicy()
		DeferCleanup(m.remove)

		By("Writing the model's thresholds, which want one more replica, with no passive key of its own")
		m.policy(wouldScaleUp, nil)

		By("Turning passive mode on controller-wide: LLMSM_PASSIVE=true, then a restart")
		since := time.Now()
		original = setControllerPassive(true)
		DeferCleanup(func() { restoreManagerConfig(original) })
		expectLog(since, 2*time.Minute, "Passive mode is on controller-wide")

		By("Standing the model up at 3 replicas and registering it, our trigger alone")
		m.create(3, false)
		m.register(1, 8)
		waitForKEDAExternalMetricWired(cfg.LLMDNamespace, m.deployment(), m.soBase)
		expectLog(since, 3*time.Minute, "Passive mode entered", m.modelID(), `"mode": "hold"`, `"source": "LLMSM_PASSIVE"`)
	})

	It("holds while the controller would scale up", func() {
		m.waitWouldBe(pc, 3, wouldScaleUp, "hold")
		m.expectHeld(pc, "case1-up", 3, wouldScaleUp, "hold", nil)
	})

	It("holds while the controller would scale down", func() {
		m.policy(wouldScaleDown, nil)
		m.waitWouldBe(pc, 3, wouldScaleDown, "hold")
		m.expectHeld(pc, "case1-down", 3, wouldScaleDown, "hold", nil)
	})

	It("keeps a kubectl scale from 3 to 5", func() {
		m.scale(5)
		m.waitWouldBe(pc, 5, wouldScaleDown, "hold")
		m.expectHeld(pc, "case4-scale-3-to-5", 5, wouldScaleDown, "hold", nil)
		m.waitReady(5)
	})

	It("holds through a controller restart and a leader change", func() {
		before := leaderPod()
		m.expectHeld(pc, "case7-restart", 5, wouldScaleDown, "hold", func() {
			By("Restarting the controller: a new pod, and the lease handed to it")
			Expect(restartScalingManagerController(ctx)).To(Succeed())
			after := leaderPod()
			GinkgoWriter.Printf("PASSIVE-EVIDENCE leader %s -> %s at %s\n", before, after, time.Now().UTC().Format(time.RFC3339))
			Expect(after).NotTo(Equal(before), "the restart did not change the leader")
		})
		By("Deleting the leader pod outright, so the lease is taken without a clean hand-off")
		leader := leaderPod()
		m.expectHeld(pc, "case7-leader-deleted", 5, wouldScaleDown, "hold", func() {
			Expect(k8sClient.CoreV1().Pods(cfg.LLMSMNamespace).Delete(ctx, leader, metav1.DeleteOptions{})).To(Succeed())
			Eventually(func(g Gomega) {
				lease, err := k8sClient.CoordinationV1().Leases(cfg.LLMSMNamespace).Get(ctx, "72dd1cf1.llm-d.ai", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(lease.Spec.HolderIdentity).NotTo(BeNil())
				g.Expect(*lease.Spec.HolderIdentity).NotTo(HavePrefix(leader + "_"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())
			GinkgoWriter.Printf("PASSIVE-EVIDENCE leader pod %s deleted; lease now %s at %s\n", leader, leaderPod(), time.Now().UTC().Format(time.RFC3339))
		})
	})

	// The control: the same model, the same KEDA and HPA, scales once it is
	// no longer passive. Without it a hold could be a pipeline that never
	// moves anything.
	It("scales once the model is switched to active", func() {
		// Recorded from the write: the scale-down waits the stabilization
		// window, counted from the last held recommendation, so the switch
		// time is what the timeline is measured against.
		rec := recordReplicas(m.deployment())
		since := time.Now()
		m.policy(wouldScaleDown, boolPtr(false))
		m.waitSpec("below 5", func(n int32) bool { return n < 5 }, 4*time.Minute)
		moved := time.Since(since).Round(time.Second)
		samples := rec.Stop("case8-passive-to-active")
		expectLog(since, 2*time.Minute, "Passive mode left", m.modelID(), `"source": "perModel"`)
		GinkgoWriter.Printf("PASSIVE-EVIDENCE case8 spec.replicas values %v, first below 5 at +%s after the switch (scale-down window %ds)\n",
			specValues(samples), moved, passiveScaleDownWindowSec)
		desired, _, on := m.metricsOf(pc, "active")
		GinkgoWriter.Printf("PASSIVE-METRICS case8 llmsm_desired_replicas=%v llmsm_passive_mode{mode=\"active\"}=%v\n", desired, on)
	})
})

var _ = Describe("Passive mode: per-model hold beside an active model", Serial, Label("full", "passive"), Ordered, func() {
	held := newPassiveModel("pm")
	active := newPassiveModel("nb")
	var pc *utils.PrometheusClient

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("needs the simulator's --fake-metrics: set USE_SIMULATOR=true")
		}
		pc = passiveProm()
		snapshotPolicy()
		DeferCleanup(held.remove)
		DeferCleanup(active.remove)

		By("Making one model passive per model, in an otherwise active controller; both would scale up")
		held.policy(wouldScaleUp, boolPtr(true))
		active.policy(wouldScaleUp, nil)

		since := time.Now()
		held.create(2, false)
		active.create(1, false)
		// One replica a minute, so a scale-up can be caught between two steps.
		held.register(1, 6, fixtures.WithScaleUpPodsPerPeriod(1, 60))
		active.register(1, 3)
		waitForKEDAExternalMetricWired(cfg.LLMDNamespace, held.deployment(), held.soBase)
		waitForKEDAExternalMetricWired(cfg.LLMDNamespace, active.deployment(), active.soBase)
		expectLog(since, 3*time.Minute, "Passive mode entered", held.modelID(), `"mode": "hold"`, `"source": "perModel"`)
	})

	It("holds the passive model while its active neighbour scales", func() {
		neighbour := recordReplicas(active.deployment())
		held.waitWouldBe(pc, 2, wouldScaleUp, "hold")
		held.expectHeld(pc, "case2-held", 2, wouldScaleUp, "hold", nil)
		active.waitSpec("at least 2", func(n int32) bool { return n >= 2 }, 3*time.Minute)
		GinkgoWriter.Printf("PASSIVE-EVIDENCE case2 neighbour spec.replicas values %v\n", specValues(neighbour.Stop("case2-neighbour")))
		d, _, on := active.metricsOf(pc, "active")
		GinkgoWriter.Printf("PASSIVE-METRICS case2-neighbour llmsm_desired_replicas=%v llmsm_passive_mode{mode=\"active\"}=%v\n", d, on)
		Expect(on).To(Equal(1.0))
	})

	It("stops a scale-up where spec.replicas was when the model turns passive", func() {
		By("Switching the model to active, so it starts to scale up one replica a minute")
		rec := recordReplicas(held.deployment())
		held.policy(wouldScaleUp, boolPtr(false))
		held.waitSpec("3, the first step of the scale-up", func(n int32) bool { return n >= 3 }, 4*time.Minute)

		By("Switching it back to passive between two steps")
		since := time.Now()
		held.policy(wouldScaleUp, boolPtr(true))
		at := held.specReplicas()
		GinkgoWriter.Printf("PASSIVE-EVIDENCE case9 passive written at %s with spec.replicas=%d\n", since.UTC().Format(time.RFC3339), at)
		GinkgoWriter.Printf("PASSIVE-EVIDENCE case9 spec.replicas values before the hold %v\n", specValues(rec.Stop("case9-scale-up")))
		held.expectHeld(pc, "case9-held", at, wouldScaleUp, "hold", nil)
		// The scaler reads passive live, the engine once a cycle: a flip
		// shorter than a cycle may leave no engine line, so the scaler's own
		// line is what must explain the stop.
		expectLog(since, 2*time.Minute, "Passive mode entered: the external scaler", held.scaledObject(), `"previousMode": "active"`)
		for _, l := range logLines(since.Add(-3*time.Minute), "Passive mode", held.modelID()) {
			GinkgoWriter.Printf("PASSIVE-LOG case9 %s\n", l)
		}
	})
})

var _ = Describe("Passive mode: hold at zero", Serial, Label("full", "passive"), Ordered, func() {
	m := newPassiveModel("zero")
	var pc *utils.PrometheusClient

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("needs the simulator's --fake-metrics: set USE_SIMULATOR=true")
		}
		requireScaleFromZeroPath()
		pc = passiveProm()
		snapshotPolicy()
		DeferCleanup(m.remove)

		m.policy(wouldScaleUp, boolPtr(true))
		By("Standing the model up at 0 replicas, in the serving pool, with minReplicaCount 0 and a push trigger")
		m.create(0, true)
		m.register(0, 4, fixtures.WithExternalScalerPushTrigger(externalScalerAddress()))
	})

	It("stays at zero under queued requests", func() {
		since := time.Now()
		sendQueuedRequests(fmt.Sprintf("passive-zero-trigger-%d", time.Now().Unix()), m.modelID())
		rec := recordReplicas(m.deployment())
		time.Sleep(passiveHoldWindow)
		samples := rec.Stop("case3-zero")
		for _, s := range samples {
			Expect(s.spec).To(Equal(int32(0)), "a passive model at zero was woken at +%s", s.at)
		}
		expectNoWake(since, m.modelID(), m.scaledObject())
		_, _, on := m.metricsOf(pc, "hold")
		GinkgoWriter.Printf("PASSIVE-METRICS case3 llmsm_passive_mode{mode=\"hold\"}=%v\n", on)
		Expect(on).To(Equal(1.0), "a passive variant at zero replicas must still publish its mode")
	})

	It("keeps a kubectl scale from zero to 2", func() {
		m.scale(2)
		rec := recordReplicas(m.deployment())
		time.Sleep(passiveHoldWindow)
		samples := rec.Stop("case3-scale-0-to-2")
		for _, s := range samples {
			Expect(s.spec).To(Equal(int32(2)), "the count set by hand did not stick: %d at +%s", s.spec, s.at)
		}
		d, cur, on := m.metricsOf(pc, "hold")
		GinkgoWriter.Printf("PASSIVE-METRICS case3-after-scale llmsm_desired_replicas=%v llmsm_current_replicas=%v llmsm_passive_mode{mode=\"hold\"}=%v\n", d, cur, on)
	})
})

var _ = Describe("Passive mode: observe beside a foreign trigger", Serial, Label("full", "passive"), Ordered, func() {
	m := newPassiveModel("obs")
	var pc *utils.PrometheusClient

	foreign := kedav1alpha1.ScaleTriggers{
		Type: "kubernetes-workload",
		Name: "pods",
		Metadata: map[string]string{
			"podSelector": passiveLoadLabel + "=true",
			"value":       "1",
		},
	}

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("needs the simulator's --fake-metrics: set USE_SIMULATOR=true")
		}
		pc = passiveProm()
		snapshotPolicy()
		DeferCleanup(m.remove)
		DeferCleanup(func() { setLoadPods(0) })

		m.policy(wouldScaleUp, boolPtr(true))
		// In the serving pool: the last spec parks it at zero and queues requests for it.
		m.create(1, true)
		setLoadPods(2)
		since := time.Now()
		m.register(0, 6, fixtures.WithForeignTrigger(foreign))
		waitForKEDAExternalMetricWired(cfg.LLMDNamespace, m.deployment(), m.soBase)
		expectLog(since, 3*time.Minute, "Passive mode entered", m.modelID(), `"mode": "observe"`, `"foreignTriggers": 1`)
	})

	It("follows the other trigger to 2 and never ours above it", func() {
		m.waitSpec("2, the load pods' count", func(n int32) bool { return n == 2 }, 3*time.Minute)
		m.waitWouldBe(pc, 2, wouldScaleUp, "observe")
		m.expectHeld(pc, "case5-observe-2", 2, wouldScaleUp, "observe", nil)
	})

	It("follows the other trigger to 4", func() {
		setLoadPods(4)
		m.waitSpec("4, the load pods' count", func(n int32) bool { return n == 4 }, 3*time.Minute)
		m.waitReady(4)
		m.waitWouldBe(pc, 4, wouldScaleUp, "observe")
		m.expectHeld(pc, "case5-observe-4", 4, wouldScaleUp, "observe", nil)
	})

	It("follows the other trigger to zero, and queued requests do not wake it", func() {
		setLoadPods(0)
		m.waitSpec("0, after the cooldown", func(n int32) bool { return n == 0 }, 4*time.Minute)
		since := time.Now()
		sendQueuedRequests(fmt.Sprintf("passive-obs-trigger-%d", time.Now().Unix()), m.modelID())
		rec := recordReplicas(m.deployment())
		time.Sleep(passiveHoldWindow)
		for _, s := range rec.Stop("case5-observe-zero") {
			Expect(s.spec).To(Equal(int32(0)), "an observed model at zero was woken at +%s", s.at)
		}
		expectNoWake(since, m.modelID(), m.scaledObject())
	})

	It("reports no error for its trigger in KEDA", func() {
		so := &kedav1alpha1.ScaledObject{}
		Expect(crClient.Get(ctx, types.NamespacedName{Namespace: cfg.LLMDNamespace, Name: m.scaledObject()}, so)).To(Succeed())
		for _, c := range so.Status.Conditions {
			GinkgoWriter.Printf("PASSIVE-EVIDENCE case5 ScaledObject condition %s=%s %s: %s\n", c.Type, c.Status, c.Reason, c.Message)
		}
		ready := so.Status.Conditions.GetReadyCondition()
		Expect(ready.IsTrue()).To(BeTrue(), "the ScaledObject is not Ready")
		for name, h := range so.Status.Health {
			GinkgoWriter.Printf("PASSIVE-EVIDENCE case5 health %s: %s failures=%d\n", name, h.Status, ptr.Deref(h.NumberOfFailures, 0))
		}
		events, err := k8sClient.CoreV1().Events(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
			// This object only: an earlier one of the same name leaves its
			// ScaledObjectDeleted warning behind.
			FieldSelector: "involvedObject.name=" + m.scaledObject() + ",involvedObject.uid=" + string(so.UID),
		})
		Expect(err).NotTo(HaveOccurred())
		var warnings []string
		for _, e := range events.Items {
			GinkgoWriter.Printf("PASSIVE-EVIDENCE case5 event %s %s: %s\n", e.Type, e.Reason, e.Message)
			if e.Type == corev1.EventTypeWarning {
				warnings = append(warnings, e.Reason+": "+e.Message)
			}
		}
		Expect(warnings).To(BeEmpty(), "KEDA reported a warning on the ScaledObject")
	})

	It("observe-unsafe: a scalingModifiers formula naming our trigger is warned about", func() {
		By("Replacing the ScaledObject with one whose formula adds our trigger to the other")
		_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, m.soBase)
		m.scale(1)
		setLoadPods(2)
		since := time.Now()
		rec := recordReplicas(m.deployment())
		m.register(0, 6, fixtures.WithOwnTriggerName("llmsm"), fixtures.WithForeignTrigger(foreign),
			fixtures.WithScalingModifiers("llmsm + pods", "1"))
		expectLog(since, 3*time.Minute, "Passive observe is unsupported here", m.modelID(), `"mode": "observe-unsafe"`)
		time.Sleep(passiveHoldWindow)
		samples := rec.Stop("case6-observe-unsafe")
		desired, current, on := m.metricsOf(pc, "observe-unsafe")
		GinkgoWriter.Printf("PASSIVE-METRICS case6 llmsm_desired_replicas=%v llmsm_current_replicas=%v llmsm_passive_mode{mode=\"observe-unsafe\"}=%v spec.replicas values %v\n",
			desired, current, on, specValues(samples))
		Expect(on).To(Equal(1.0))
	})
})

var _ = Describe("Passive mode: a rollout at 10 replicas in hold", Serial, Label("full", "passive"), Ordered, func() {
	m := newPassiveModel("roll")
	var pc *utils.PrometheusClient

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("needs the simulator's --fake-metrics: set USE_SIMULATOR=true")
		}
		pc = passiveProm()
		snapshotPolicy()
		DeferCleanup(m.remove)

		// It would scale DOWN, so a count that rises can only be the HPA's
		// tolerance acting on the surge pods, never the controller.
		m.policy(wouldScaleDown, boolPtr(true))
		since := time.Now()
		m.create(10, false)
		m.register(1, 14)
		waitForKEDAExternalMetricWired(cfg.LLMDNamespace, m.deployment(), m.soBase)
		expectLog(since, 3*time.Minute, "Passive mode entered", m.modelID(), `"mode": "hold"`)
		m.waitWouldBe(pc, 10, wouldScaleDown, "hold")
	})

	// Measured, not assumed: the docs say a rollout at 9 or more can add one
	// replica (how exact the hold is). The spec records each rollout and fails
	// only past that bound.
	It("drifts at most one replica per rollout", func() {
		for i := 1; i <= 2; i++ {
			before := m.specReplicas()
			rec := recordReplicas(m.deployment())
			patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"e2e.llm-d.ai/passive-rollout":"%d-%d"}}}}}`, i, time.Now().Unix())
			_, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Patch(ctx, m.deployment(), types.StrategicMergePatchType,
				[]byte(patch), metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, m.deployment(), metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(dep.Status.ObservedGeneration).To(Equal(dep.Generation))
				spec := ptr.Deref(dep.Spec.Replicas, 1)
				g.Expect(dep.Status.UpdatedReplicas).To(Equal(spec))
				g.Expect(dep.Status.Replicas).To(Equal(spec))
				g.Expect(dep.Status.ReadyReplicas).To(Equal(spec))
			}, 6*time.Minute, 2*time.Second).Should(Succeed())
			// Past the rollout, three syncs and the scale-down window.
			time.Sleep(passiveHoldWindow)
			samples := rec.Stop(fmt.Sprintf("case11-rollout-%d", i))
			after := m.specReplicas()
			desired, current, on := m.metricsOf(pc, "hold")
			GinkgoWriter.Printf("PASSIVE-EVIDENCE case11 rollout %d: spec.replicas %d -> %d, values %v; llmsm_desired_replicas=%v llmsm_current_replicas=%v hold=%v\n",
				i, before, after, specValues(samples), desired, current, on)
			Expect(after).To(BeNumerically("<=", before+1),
				"rollout %d moved the held count from %d to %d, past the one replica the docs allow", i, before, after)
		}
	})
})
