package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
)

// SGLang backend e2e: deploys a synthetic SGLang model server (CPU-only emitter of
// sglang:* metrics, launched with a faithful `sglang.launch_server` command),
// wires Prometheus scraping, and registers it with the scaling manager via an annotated scaler
// (annotation-based discovery — no the old CRD). It asserts that scaling manager —
// with no engine configuration — detects SGLang, collects the sglang:* metrics,
// and emits llmsm_desired_replicas driving a scale-up.
//
// This exercises the same code path as vLLM, only with SGLang metric names and
// flags. It runs in the kind-emulator environment (cfg.UseSimulator); a real
// SGLang server requires a GPU.
var _ = Describe("SGLang backend", Label("full"), Ordered, func() {
	const (
		baseName = "e2e-sglang"
		// variantName MUST equal the annotated scaler's object name, which is
		// baseName+"-so" for a KEDA ScaledObject (see fixtures.EnsureScaledObject).
		// The scaling manager uses that object name as the variant identity: it attributes the decode
		// pods' sglang:* metrics by matching their llm-d.ai/variant label to it, and
		// emits llmsm_desired_replicas{variant_name=<object name>}. A mismatch leaves the
		// pods unattributed, so the scaling manager falls back to a safety-net desiredReplicas=current
		// and never scales up. (Was the stale "e2e-sglang-hpa" from the HPA era.)
		variantName = baseName + "-so"
		// decodeSuffix mirrors the fixtures' "<base>-decode" naming convention.
		decodeSuffix = "-decode"
		// sglangEmulatorPort is the container/Service port the emitter serves on.
		sglangEmulatorPort = 8000

		// Saturation config this suite pins (see BeforeAll). Calibrated against the
		// fixture's fixed operating point: token_usage=0.85 of
		// sglang:max_total_num_tokens=100000 is 85000 tokens resident, against a
		// usable ceiling of 100000 x 0.80 = 80000 — utilization ~1.06, clear of
		// scaleUpThreshold. The spare triggers do not affect the V2 scale-up path;
		// they carry safe values so config validation passes
		sglangKVCacheThreshold     = 0.80
		sglangQueueLengthThreshold = 1
		sglangScaleUpThreshold     = 0.85
		sglangScaleDownBoundary    = 0.70
	)
	var (
		ctx      = context.Background()
		appLabel = baseName + decodeSuffix
		modelID  = "e2e-scaling-manager/sglang-model"

		// Saturation ConfigMap snapshot, restored in AfterAll (see BeforeAll).
		cmName          string
		cmNamespace     string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
	)

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("SGLang e2e uses a CPU-only metrics emitter; it runs in the kind-emulator environment (UseSimulator=true)")
		}

		// Pin an explicit saturation config for this suite instead of inheriting the
		// shipped ConfigMap, so the scale-up assertion cannot be invalidated by a
		// change to the defaults. Restored in AfterAll; config is re-resolved from the
		// watched ConfigMap each cycle, so no controller restart is needed.
		//
		// The calibration is deterministic under V2. The fixture emits a fixed
		// operating point — token_usage=0.85 against sglang:max_total_num_tokens=100000,
		// so 85000 tokens resident — while kvCacheThreshold=0.80 puts the usable
		// ceiling at 80000. Utilization is therefore 85000/80000 ≈ 1.06, comfortably
		// past scaleUpThreshold=0.85, and the scaling manager scales out.
		//
		// (This replaces a "pin to V1" guard that predated the V1 removal. It claimed
		// the fixture sat exactly at V2's 0.85 threshold, which only holds if the KV
		// ceiling is ignored — applying it leaves the 25% margin above. The config it
		// wrote already ran on V2 regardless, since V1 no longer exists.)
		cmNamespace = cfg.LLMSMNamespace
		cmName = scalingPolicyConfigMapName()
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			// Fail fast on a real API error: a false "did not exist" here would make
			// AfterAll delete the shared ConfigMap without recreating it.
			Expect(err).NotTo(HaveOccurred(), "reading existing saturation configmap")
		}
		By("Pinning an explicit saturation config so the scale-up calibration is self-contained")
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, defaultConfigKey,
			buildSaturationConfigYAMLWithThresholds(
				"saturation",
				sglangKVCacheThreshold,
				sglangQueueLengthThreshold,
				sglangScaleUpThreshold,
				sglangScaleDownBoundary,
			))).To(Succeed())

		By("Deploying the synthetic SGLang model server")
		Expect(fixtures.CreateSGLangEmulator(ctx, k8sClient, cfg.LLMDNamespace, baseName, modelID)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteSGLangEmulator(ctx, k8sClient, cfg.LLMDNamespace, baseName) })

		By("Exposing it via a Service and ServiceMonitor")
		Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, baseName, appLabel, sglangEmulatorPort)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteService(ctx, k8sClient, cfg.LLMDNamespace, baseName) })
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, baseName, appLabel)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteServiceMonitor(ctx, crClient, cfg.MonitoringNS, baseName) })

		By("Registering the SGLang deployment with the scaling manager via an annotated scaler")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, baseName, appLabel, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(modelID, "30.0"))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, baseName) })
	})

	AfterAll(func() {
		By("Restoring the saturation ConfigMap")
		restoreSaturationConfigMap(ctx, cmNamespace, cmName, cmOriginal, cmExistedBefore)
	})

	It("detects SGLang and emits llmsm_desired_replicas from sglang:* metrics", func() {
		// A correctly routed SGLang collection must produce llmsm_desired_replicas for
		// the variant; vLLM queries would return nothing here, so a scale-up proves
		// the SGLang path collected sglang:* metrics. The fixture emits a saturated
		// operating point (token_usage=0.85, num_queue_reqs=3), so the scaling manager recommends
		// scale-up and KEDA drives the Deployment above a single replica. Assert the
		// observable Deployment replica count — the ground truth — rather than the
		// KEDA HPA CurrentMetrics surface, which only proves the metric was consumed.
		By("Asserting KEDA actuates a scale-up above a single replica for the SGLang variant")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, appLabel, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", int32(2)),
				"saturated SGLang metrics (token_usage=0.85, num_queue_reqs=3) should drive the Deployment above a single replica")
		}).WithTimeout(time.Duration(cfg.ScaleUpTimeout) * time.Second).
			WithPolling(time.Duration(cfg.PollIntervalSlowSec) * time.Second).
			Should(Succeed())
	})
})
