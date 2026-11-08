package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
)

// Parking an SGLang model, on its own request counter.
//
// This behaviour changed and had unit coverage only. The enforcer used to ask for
// vllm:request_success_total whatever engine a model ran, so an SGLang model had no
// such series, read as permanently idle, and was refused outright to keep it safe.
// The engine-specific query already existed — sglang:num_requests_total, registered
// beside the vLLM one — and simply was never reached. The engine is now detected
// and passed to the enforcer, so this asserts the counter actually consulted is the
// SGLang one.
//
// The vLLM suite in scale_to_zero_test.go carries the shared reasoning about why a
// model must serve before it can be parked, and about retentionPeriod and KEDA's
// cooldownPeriod being sequential. Only what differs is repeated here.
//
// The emulator needs a SERVING WINDOW. Its counters are synthetic and grow with
// elapsed time for as long as the pod lives, deliberately, so that rate() and
// increase() are non-zero for the saturation suites. Against that, increase() never
// reaches zero and the model can never be idle — so this asks for IDLE_AFTER, which
// freezes the counters and is what "served, then went quiet" looks like to a
// counter. Without it this suite could not pass, however correct the product.
var _ = Describe("Scale-To-Zero Feature - parking an SGLang model", Serial, Label("full"), Ordered, func() {
	const (
		baseName = "stz-sglang"
		// The emulator's Deployment carries the -decode suffix, and that — not
		// baseName — is what KEDA scales and what this suite must read.
		appLabel    = baseName + "-decode"
		variantName = baseName + "-so"
		// The emitter serves metrics on this port.
		emulatorPort = 8000
		// The emulator serves for this long, then its counters stop advancing, so
		// the idle window opens while the suite is still running.
		servingWindowSeconds = 60
		// Short enough to park inside a test run; the default is 10m. The policy
		// entry and the earliest-legal-park check read this one value, so they
		// cannot drift apart.
		retentionDuration = time.Minute
		parkingBudget     = 12 * time.Minute
	)

	var (
		servingSince    time.Time
		modelID         string
		cmName          string
		cmNamespace     string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
	)

	BeforeAll(func() {
		if !cfg.ScaleToZeroEnabled {
			Skip("This suite parks a model, which requires scale-to-zero: set SCALE_TO_ZERO_ENABLED=true")
		}

		modelID = sfzModelID("stz-sglang")
		cmName = scalingPolicyConfigMapName()
		cmNamespace = cfg.LLMSMNamespace

		By("Snapshotting the scaling policy ConfigMap for restore in AfterAll")
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			// A false "did not exist" here would make the restore delete the shared
			// ConfigMap without recreating it.
			Expect(err).NotTo(HaveOccurred(), "reading existing scaling policy ConfigMap")
		}

		By("Enabling scale-to-zero for this model with a short retention")
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, defaultConfigKey,
			buildSaturationConfigYAML())).To(Succeed())
		modelEntry := buildSaturationConfigYAMLWithModel(
			0.80, 1, 0.85, 0.70, modelID, cfg.LLMDNamespace,
		) + fmt.Sprintf("scaleToZero:\n  enabled: true\n  retentionPeriod: %s\n", retentionDuration)
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, "stz-sglang-model", modelEntry)).To(Succeed())

		By("Deploying the SGLang emulator with a serving window")
		_ = fixtures.DeleteSGLangEmulator(ctx, k8sClient, cfg.LLMDNamespace, baseName)
		Expect(fixtures.CreateSGLangEmulatorIdleAfter(ctx, k8sClient, cfg.LLMDNamespace,
			baseName, modelID, servingWindowSeconds)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteSGLangEmulator(ctx, k8sClient, cfg.LLMDNamespace, baseName) })

		By("Exposing it via a Service and ServiceMonitor")
		Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, baseName, appLabel, emulatorPort)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteService(ctx, k8sClient, cfg.LLMDNamespace, baseName) })
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, baseName, appLabel)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteServiceMonitor(ctx, crClient, cfg.MonitoringNS, baseName) })

		By("Waiting for the emulator to be ready")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, appLabel, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", int32(1)))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		// The clock the parking spec's earliest-legal-park check is measured against.
		servingSince = time.Now()

		By("Registering it with minReplicaCount=0")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, baseName,
			appLabel, variantName, 0, 10, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(modelID, "30.0"))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, baseName) })
	})

	AfterAll(func() {
		By("Restoring the scaling policy ConfigMap")
		restoreSaturationConfigMap(ctx, cmNamespace, cmName, cmOriginal, cmExistedBefore)
	})

	It("exports the SGLang request counter, not the vLLM one", func() {
		pc := promClientForCheck()
		if pc == nil {
			Skip("no Prometheus client could be built")
		}
		if _, err := pc.QueryWithRetry(ctx, "vector(1)"); err != nil {
			Skip("Prometheus is not reachable from the test host: " + err.Error())
		}

		// The point of the change: this model is measured on sglang:num_requests_total.
		// Had the enforcer gone on asking for the vLLM counter it would find no series
		// here and — by the absence guarantee — decline to park forever, which is
		// exactly the old behaviour.
		Eventually(func(g Gomega) {
			v, err := pc.QueryWithRetry(ctx,
				fmt.Sprintf(`count(sglang:num_requests_total{model_name=%q})`, modelID))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v).To(BeNumerically(">", 0), "the SGLang counter must exist for this model")
		}, 3*time.Minute, 10*time.Second).Should(Succeed())

		v, err := pc.QueryWithRetry(ctx,
			fmt.Sprintf(`count(vllm:request_success_total{model_name=%q}) or vector(0)`, modelID))
		Expect(err).NotTo(HaveOccurred())
		Expect(v).To(BeNumerically("==", 0),
			"and the vLLM counter must NOT exist, or this suite would prove nothing")
	})

	It("parks the SGLang model once its counter stops advancing", func() {
		// There is no load to stop: the emulator's serving window closes on its own,
		// after which increase() over the retention window falls to zero.
		//
		// What makes this a real assertion rather than a coincidence: the emulator
		// holds a SATURATED operating point (token_usage 0.85, num_queue_reqs 3), so
		// the optimizer's own decision here is scale-UP — observed as
		// {"curr":2,"tgt":3,"action":"scale-up"} immediately before the enforcer
		// overrode it. Zero is therefore reachable ONLY through the idle signal. Had
		// the enforcer gone on querying the vLLM counter it would find no series,
		// report an error, keep the decisions by design, and this deployment would
		// climb instead of park.
		By("Waiting for the Deployment to reach zero replicas")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, appLabel, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(*dep.Spec.Replicas).To(Equal(int32(0)),
				"an idle SGLang model must park on sglang:num_requests_total; if it does not, "+
					"check llmsm_model_scaling_blocked for this model — engine-unsupported would "+
					"mean it was not seen as running a single SGLang engine")
		}, parkingBudget, 10*time.Second).Should(Succeed())

		// A zero that arrives TOO SOON is not this feature working, and on a cluster
		// carrying state from an earlier run it is the likely outcome: the variant
		// name is deterministic, so a parked decision left in Prometheus makes KEDA
		// deactivate the fresh deployment within seconds. That is how a re-run of this
		// suite once "passed" in 15 seconds — less than the serving window alone.
		//
		// Idleness cannot be established before the counters stop advancing AND the
		// retention window has then elapsed, so anything faster is contamination.
		//
		// The grace is not slack. servingSince is stamped when the pod reports
		// READY, while the emulator's IDLE_AFTER clock starts at PROCESS START, so
		// the idle window opens before servingSince and elapsed-since-ready is short
		// by the pod's startup time. Against an exact floor this suite passes on a
		// cold controller (its first run took 380s) and fails on a warm one, which
		// is how it failed in a full-suite run at 57/58. The floor only has to
		// separate a real park from a leftover decision that lands in seconds, and
		// 105s still does that by a wide margin.
		const graceForPodStartup = 15 * time.Second
		earliest := time.Duration(servingWindowSeconds)*time.Second + retentionDuration - graceForPodStartup
		Expect(time.Since(servingSince)).To(BeNumerically(">=", earliest),
			"parked sooner than idleness could possibly be established: the counter was still "+
				"advancing, so this is stale state, not a decision")
	})

	It("records zero as the scaling manager's own decision, not KEDA's default", func() {
		pc := promClientForCheck()
		if pc == nil {
			Skip("no Prometheus client could be built")
		}
		if _, err := pc.QueryWithRetry(ctx, "vector(1)"); err != nil {
			Skip("Prometheus is not reachable from the test host: " + err.Error())
		}

		// A deployment at zero replicas is not by itself proof that scaling manager parked it —
		// KEDA deactivates a workload whose trigger metric is missing too, which would
		// look identical. llmsm_desired_replicas is the scaling manager's own published decision, so a
		// zero there is the enforcer's verdict and not an absence.
		By("Asserting the scaling manager published a desired replica count of zero for the variant")
		Eventually(func(g Gomega) {
			v, err := pc.QueryWithRetry(ctx, fmt.Sprintf(
				`sum(llmsm_desired_replicas{variant_name=%q,exported_namespace=%q})`,
				variantName, cfg.LLMDNamespace))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v).To(BeNumerically("==", 0),
				"The scaling manager must publish 0 for a parked SGLang variant; an absent series here would "+
					"mean the deployment reached zero without the scaling manager deciding it should")
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		By("And reporting the model as serving nothing")
		Eventually(func(g Gomega) {
			v, err := pc.QueryWithRetry(ctx, fmt.Sprintf(
				`sum(llmsm_model_replicas{exported_namespace=%q,model_name=%q})`,
				cfg.LLMDNamespace, modelID))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v).To(BeNumerically("==", 0), "a parked model must report zero replicas")
		}, 2*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("reports no configuration contradiction for it", func() {
		pc := promClientForCheck()
		if pc == nil {
			Skip("no Prometheus client could be built")
		}
		if _, err := pc.QueryWithRetry(ctx, "vector(1)"); err != nil {
			Skip("Prometheus is not reachable from the test host: " + err.Error())
		}

		// engine-unsupported now means MORE THAN ONE engine. A single-engine SGLang
		// model must report nothing — the assertion that would have failed before the
		// change.
		v, err := pc.QueryWithRetry(ctx, fmt.Sprintf(
			`count(llmsm_model_scaling_blocked{exported_namespace=%q,model_name=%q,`+
				`reason=~"variant-floor|policy-forbids-zero|engine-unsupported"}) or vector(0)`,
			cfg.LLMDNamespace, modelID))
		Expect(err).NotTo(HaveOccurred())
		Expect(v).To(BeNumerically("==", 0))
	})
})
