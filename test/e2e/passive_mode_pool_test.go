package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
	testutils "github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// Passive mode against the two features that move capacity between models
// without a scale-up of their own: the utilization-share optimizer, which
// shrinks one model so another can grow, and the warm pool, which lends a
// warm Pod to a model short of replicas. A passive model takes part in
// neither (docs/reference/scaling-policy.md, "What it does not do for a
// passive model"). Each spec holds first and then switches the model to
// active, which must start the transfer or the borrow: the control that shows
// the hold was the passive switch and not a setup that never moves anything.

var _ = Describe("Passive mode: a utilization-share group holding a passive model", Label("full", "passive"), Ordered, Serial, func() {
	const (
		quotaGPUs = 4
		msA       = "passive-ushare-a"
		msB       = "passive-ushare-b"
		loadJob   = "passive-ushare-b-load"
		window    = int32(30)
		// How long the group must stay still. The active share suite marks a
		// donor within this budget once the group is evaluated with B short.
		shareHold = 4 * time.Minute
	)
	var (
		ns, product, configName string
		modelA, modelB          string
		pc                      *testutils.PrometheusClient
	)
	soA, soB := msA+"-so", msB+"-so"
	depA, depB := msA+"-decode", msB+"-decode"
	entryA := "passive-ushare-a-model"

	BeforeAll(func() {
		var ok bool
		_, product, ok = fixtures.DiscoverAcceleratorProduct(ctx, k8sClient)
		if !ok {
			Skip("no GPU product label on any schedulable node; the quota has no accelerator to bound")
		}
		pc = passiveProm()
		modelA, modelB = suiteModelID("passive-ushare-a"), suiteModelID("passive-ushare-b")

		nsObj, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "passive-ushare-"},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ns = nsObj.Name
		Expect(fixtures.GrantPodWrites(ctx, k8sClient, ns, cfg.LLMSMNamespace)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
		})

		configName = scalingPolicyConfigMapName()
		snapshotPolicy()
		By("Declaring a spent 4-GPU quota with the utilization-share optimizer ACTING (not shadow)")
		restore, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.LLMSMNamespace, configName, ns, product, quotaGPUs,
			fixtures.WithOptimizer(map[string]any{
				"type":             "utilizationShare",
				"utilizationShare": map[string]any{"shadow": false},
			}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restore(context.Background()) })

		By("Making model A, the donor, passive per model")
		Expect(upsertSaturationConfigEntry(ctx, cfg.LLMSMNamespace, configName, entryA,
			buildSaturationConfigYAMLWithModel(0.80, 1, 0.85, 0.70, modelA, ns)+"passive: true\n")).To(Succeed())

		replicas := func(n int32) fixtures.ModelServiceOption {
			return func(d *appsv1.Deployment) { d.Spec.Replicas = ptr.To(n) }
		}
		Expect(fixtures.EnsureModelService(ctx, k8sClient, ns, msA, msA+"-pool", modelA, cfg.UseSimulator, 2, replicas(3))).To(Succeed())
		Expect(fixtures.EnsureModelService(ctx, k8sClient, ns, msB, msB+"-pool", modelB, cfg.UseSimulator, 2, replicas(1))).To(Succeed())
		for _, ms := range []string{msA, msB} {
			Expect(fixtures.EnsureService(ctx, k8sClient, ns, ms, ms+"-decode", 8000)).To(Succeed())
			Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, ns, ms, ms+"-decode")).To(Succeed())
			monitor := ms + "-monitor"
			DeferCleanup(func() {
				_ = crClient.Delete(context.Background(), &promoperator.ServiceMonitor{
					ObjectMeta: metav1.ObjectMeta{Name: monitor, Namespace: cfg.MonitoringNS},
				})
			})
		}
		Eventually(func(g Gomega) {
			a, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, depA, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(a.Status.ReadyReplicas).To(Equal(int32(3)), unreadyPods(ns))
			b, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, depB, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(b.Status.ReadyReplicas).To(Equal(int32(1)), unreadyPods(ns))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering both, A with a floor of 1 so it has GPUs to give")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, soA, depA, depA, 1, 4, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(modelA, "10.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())
		Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, soB, depB, depB, 1, 4, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(modelB, "10.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, ns, soA)
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, ns, soB)
		})

		By("Loading model B well past one replica's capacity")
		job := buildThroughputSustainedLoadJob(ns, loadJob, fmt.Sprintf("http://%s-service:8000/v1/completions", msB),
			modelB, 8, int64(1800))
		_, err = k8sClient.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	It("plans the group in shadow and moves nothing", func() {
		By("Waiting for the group to be evaluated with B short and planned in passive shadow")
		Eventually(func(g Gomega) {
			h := promValue(pc, fmt.Sprintf(`max(llmsm_utilization_share_headroom{model_name=%q})`, modelB))
			g.Expect(h).To(BeNumerically("<", 0), "B is loaded past its one replica")
			s := promValue(pc, fmt.Sprintf(`max(llmsm_utilization_share_passive_shadow{scope=%q})`, ns))
			g.Expect(s).To(Equal(1.0), "llmsm_utilization_share_passive_shadow for the group")
		}, 8*time.Minute, 15*time.Second).Should(Succeed())

		By("Holding: no pod is marked, A keeps 3 and B keeps 1")
		recA, recB := recordReplicasIn(ns, depA), recordReplicasIn(ns, depB)
		start := time.Now()
		var (
			marks          []string
			lastHold, lowA float64
		)
		for time.Since(start) < shareHold {
			for _, p := range markedSharePods(Default, ns) {
				marks = append(marks, p.Name)
			}
			dA := promValue(pc, fmt.Sprintf(`max(llmsm_desired_replicas{exported_namespace=%q,variant_name=%q})`, ns, fixtures.ScaledObjectNameFor(soA)))
			s := promValue(pc, fmt.Sprintf(`max(llmsm_utilization_share_passive_shadow{scope=%q})`, ns))
			hold := promValue(pc, fmt.Sprintf(`max(llmsm_passive_mode{exported_namespace=%q,variant_name=%q,mode="hold"})`, ns, fixtures.ScaledObjectNameFor(soA)))
			lastHold = hold
			if dA >= 0 && dA < 3 {
				lowA = dA
			}
			GinkgoWriter.Printf("PASSIVE-METRICS case12-share +%s An llmsm_desired_replicas=%v hold=%v passive_shadow=%v marks=%d\n",
				time.Since(start).Round(time.Second), dA, hold, s, len(marks))
			time.Sleep(15 * time.Second)
		}
		samplesA, samplesB := recA.Stop("case12-share-A"), recB.Stop("case12-share-B")
		Expect(marks).To(BeEmpty(), "a share group holding a passive model marked a pod")
		Expect(lastHold).To(Equal(1.0), "llmsm_passive_mode{mode=\"hold\"} for A")
		Expect(lowA).To(BeNumerically(">", 0), "the controller never wanted A below 3, so the hold proved nothing")
		for _, s := range samplesA {
			Expect(s.spec).To(Equal(int32(3)), "passive A moved to %d at +%s", s.spec, s.at)
		}
		GinkgoWriter.Printf("PASSIVE-EVIDENCE case12-share B spec.replicas values %v\n", specValues(samplesB))
	})

	It("starts the transfer once A is active (the control)", func() {
		switched := time.Now()
		Expect(upsertSaturationConfigEntry(ctx, cfg.LLMSMNamespace, configName, entryA,
			buildSaturationConfigYAMLWithModel(0.80, 1, 0.85, 0.70, modelA, ns)+"passive: false\n")).To(Succeed())
		var marked []corev1.Pod
		Eventually(func(g Gomega) {
			marked = markedSharePods(g, ns)
			g.Expect(marked).NotTo(BeEmpty())
		}, 10*time.Minute, 5*time.Second).Should(Succeed(), "A is active and still no donor pod was marked")
		GinkgoWriter.Printf("PASSIVE-EVIDENCE case12-share donor %s marked %s after A turned active\n",
			marked[0].Name, time.Since(switched).Round(time.Second))
		Expect(marked[0].Labels["app"]).To(Equal(depA))
	})
})

var _ = Describe("Passive mode: the warm pool lends nothing to a passive model", Label("full", "passive"), Ordered, Serial, func() {
	const (
		borrowPool   = "passive-pool-borrow"
		modelSvcName = "passive-borrow-ms"
		fixturePool  = "passive-borrow-pool"
		scalerName   = "passive-borrow"
		entryKey     = "passive-borrow-model"

		// Over the scale-up threshold, so the model wants a replica it does
		// not have: the shortfall a borrow covers.
		borrowFakeMetrics = `{"kv-cache-usage":0.9,"running-requests":8,"waiting-requests":4}`
		settle            = 5 * time.Minute
		// How long the pool must lend nothing. The active borrow suite lends
		// within this once the variant is seen.
		borrowHold = 4 * time.Minute
	)
	var (
		controller fixtures.ControllerDeployment
		modelID    string
		modelNode  string
		pc         *testutils.PrometheusClient
	)

	poolState := func() string {
		_, logs, err := testutils.ControllerLogsContain(ctx, k8sClient, controller.Namespace, controller.Name, "", 900)
		if err != nil {
			return ""
		}
		last := ""
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, `"pool": "`+borrowPool+`"`) {
				last = line
			}
		}
		return last
	}
	entry := func(passive bool) string {
		return buildSaturationConfigYAMLWithModel(0.80, 50, 0.30, 0.20, modelID, cfg.LLMDNamespace) +
			fmt.Sprintf("passive: %t\n", passive)
	}

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("--fake-metrics is simulator-only, and this spec needs demand that does not depend on timing")
		}
		pc = passiveProm()
		controller = fixtures.LLMSMController(cfg.LLMSMNamespace)
		modelID = suiteModelID("passive-borrow")
		nodes, err := fixtures.SchedulableNodes(ctx, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes).NotTo(BeEmpty())
		modelNode = nodes[0].Name

		snapshotPolicy()
		By("Making the model passive per model, with thresholds its metrics exceed")
		Expect(upsertSaturationConfigEntry(ctx, cfg.LLMSMNamespace, scalingPolicyConfigMapName(), entryKey, entry(true))).To(Succeed())

		By("Restarting the controller with the warm pool enabled")
		restoreCtl, err := fixtures.EnableWarmPool(ctx, k8sClient, controller, cfg.LLMDNamespace)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = restoreCtl(context.Background())
			_ = fixtures.WaitForControllerReady(context.Background(), k8sClient, controller)
		})

		poolSpec := fixtures.WarmPoolSpec{Name: borrowPool, Namespace: cfg.LLMDNamespace, ProxyImage: cfg.WarmPoolProxyImage, PoolName: borrowPool}
		Expect(fixtures.CreateWarmPool(ctx, k8sClient, poolSpec)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteWarmPool(context.Background(), k8sClient, poolSpec) })
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, borrowPool, borrowPool, borrowPool, 1, 4, cfg.MonitoringNS,
			fixtures.WithWarmPoolTrigger(borrowPool, map[string]string{
				"warmPoolSleepMinSize": "0",
				"warmPoolMaxHold":      "15m",
			}))).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, borrowPool)
		})

		Expect(fixtures.CreateModelServiceWithExtraArgs(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, fixturePool, modelID,
			cfg.UseSimulator, cfg.MaxNumSeqs, []string{"--fake-metrics", borrowFakeMetrics})).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteModelService(context.Background(), k8sClient, cfg.LLMDNamespace, modelSvcName)
		})
		Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelSvcName+"-decode", 8000)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelSvcName+"-decode")).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteServiceMonitor(context.Background(), crClient, cfg.MonitoringNS, modelSvcName)
			_ = fixtures.DeleteService(context.Background(), k8sClient, cfg.LLMDNamespace, modelSvcName)
		})

		By("Pinning the model to one replica per node on one node, so a scale-up cannot land")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelSvcName+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			app := dep.Spec.Template.Labels["app"]
			dep.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": modelNode}
			dep.Spec.Template.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					TopologyKey:   "kubernetes.io/hostname",
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}},
				}},
			}}
			_, err = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Update(ctx, dep, metav1.UpdateOptions{})
			g.Expect(err).NotTo(HaveOccurred())
		}, time.Minute, 5*time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelSvcName+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, settle, 5*time.Second).Should(Succeed())

		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerName, modelSvcName+"-decode", modelSvcName+"-variant",
			1, 4, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(modelID, "10.0"),
			fixtures.WithWarmPoolSelection(borrowPool, 1))).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, scalerName)
		})
	})

	It("warms and lends nothing for the passive model", func() {
		Eventually(poolState, settle, 5*time.Second).ShouldNot(BeEmpty(), "the pool was never declared")
		so := fixtures.ScaledObjectNameFor(scalerName)
		By("Waiting for the controller to want a replica the model does not have")
		Eventually(func(g Gomega) {
			d := promValue(pc, fmt.Sprintf(`max(llmsm_desired_replicas{exported_namespace=%q,variant_name=%q})`, cfg.LLMDNamespace, so))
			g.Expect(d).To(BeNumerically(">", 1), "llmsm_desired_replicas=%v", d)
		}, settle, 5*time.Second).Should(Succeed())

		rec := recordReplicas(modelSvcName + "-decode")
		start := time.Now()
		var states []string
		for time.Since(start) < borrowHold {
			st := poolState()
			states = append(states, st)
			d := promValue(pc, fmt.Sprintf(`max(llmsm_desired_replicas{exported_namespace=%q,variant_name=%q})`, cfg.LLMDNamespace, so))
			GinkgoWriter.Printf("PASSIVE-METRICS case12-pool +%s llmsm_desired_replicas=%v pool: %s\n", time.Since(start).Round(time.Second), d, st)
			time.Sleep(15 * time.Second)
		}
		samples := rec.Stop("case12-pool-model")
		for _, st := range states {
			Expect(st).NotTo(MatchRegexp(`lent=[1-9]`), "the pool lent a Pod to a passive model: %s", st)
			Expect(st).NotTo(MatchRegexp(`resident=[1-9]`), "the pool warmed a copy of a passive model: %s", st)
		}
		for _, s := range samples {
			Expect(s.spec).To(Equal(int32(1)), "the passive model moved to %d at +%s", s.spec, s.at)
		}
	})

	It("warms and lends once the model is active (the control)", func() {
		Expect(upsertSaturationConfigEntry(ctx, cfg.LLMSMNamespace, scalingPolicyConfigMapName(), entryKey, entry(false))).To(Succeed())
		Eventually(poolState, settle, 5*time.Second).Should(MatchRegexp(`resident=[1-9]`),
			"the model is active and the pool still never warmed it")
		Eventually(poolState, settle, 5*time.Second).Should(MatchRegexp(`lent=[1-9]`),
			"the model is active and the pool still never lent to it")
		GinkgoWriter.Printf("PASSIVE-EVIDENCE case12-pool after active: %s\n", poolState())
	})
})
