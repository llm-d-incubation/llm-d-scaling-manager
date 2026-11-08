package e2e

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// Scale-from-zero placement: the wake must be refused when the variant's
// accelerator has no room, and allowed when it does.
//
// This is the only suite that exercises the DENY branch of the capacity check.
// The others cannot: their fixtures set no nodeSelector, so the scaling manager cannot resolve an
// accelerator, the candidate contributes no demand, and FitsGPUBudget returns
// true without evaluating anything. Three conditions all have to hold for a
// denial to be reachable, and each was broken at some point:
//
//  1. the variant's accelerator resolves (nodeSelector on a GPU product label);
//  2. a GPU-usage snapshot exists — which requires at least one ACTIVE variant,
//     since the saturation engine publishes only measured cycles; and
//  3. the usage is attributed to the same pool key the limits use.
//
// So the suite runs a live "occupier" workload that consumes its accelerator
// entirely, then asserts that a parked variant on that accelerator is refused.
//
// The occupier must sit OUTSIDE the pool under test. Scale-from-zero triggers on
// EPP flow-control queueing, and requests only queue when the pool has no ready
// endpoints — so an occupier sharing the pool would serve them itself and no wake
// would ever be considered. The project's contract is one model to one
// InferencePool, which makes this natural: the occupier serves its own model, so
// it belongs to its own pool. It is placed there with a distinct guide label,
// since the fixtures otherwise default every workload into one pool.
//
// It still needs to be MEASURED, or its GPUs are not counted as used: usage is
// summed from the saturation engine's per-model requests, which require replica
// metrics. Hence the Service and ServiceMonitor below.
var _ = Describe("Scale-From-Zero placement against GPU capacity", Serial, Label("full"), Ordered, func() {
	const (
		// Product label as the kind emulator sets it on nodes; pools are keyed by
		// its normalized short name (A100).
		fullAccelerator = "NVIDIA-A100-PCIE-80GB"

		occupierSvc  = "sfz-cap-occupier"
		occupierVar  = "sfz-cap-occupier-va"
		occupierScal = "sfz-cap-occupier"

		deniedSvc  = "sfz-cap-denied"
		deniedVar  = "sfz-cap-denied-va"
		deniedScal = "sfz-cap-denied"

		poolName = "sfz-cap-pool"
	)
	occupierDeploy := occupierSvc + "-decode"
	deniedDeploy := deniedSvc + "-decode"

	var triggerJobName string

	// gpusPerNode is what the cluster actually advertises on the node this
	// suite fills, READ from the node rather than assumed.
	//
	// It was a const 4, matching the Makefile's CLUSTER_GPUS at the time. That
	// couples the spec to a build flag: raise the emulator's GPU count and the
	// occupier no longer fills the node, so the refusal this suite asserts is
	// never staged and the spec passes or fails for a reason unrelated to
	// placement. Discovered, it holds whatever the cluster was built with.
	var gpusPerNode int64

	var (
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
	)

	BeforeAll(func() {
		if !cfg.ScaleToZeroEnabled {
			Skip("This suite requires EPP flow-control queuing: set SCALE_TO_ZERO_ENABLED=true")
		}

		gpusPerNode = int64(fixtures.AllocatableGPUsForProduct(ctx, k8sClient, fullAccelerator))
		Expect(gpusPerNode).To(BeNumerically(">", 0),
			"no schedulable node advertises %s, so this suite cannot fill one", fullAccelerator)
		GinkgoWriter.Printf("filling %s, which advertises %d GPUs per node\n", fullAccelerator, gpusPerNode)
		if ok, why := eppFlowControlAvailable(ctx, crClient,
			cfg.LLMSMNamespace, cfg.LLMDNamespace); !ok {
			Skip("EPP flow control is not available, so there is no wake signal to test: " + why)
		}

		// This suite asserts a REFUSAL, so it needs a limiter — and the shipped
		// ConfigMap declares none, because a GPU-aware optimizer cannot allocate to
		// a workload whose accelerator does not resolve and most do not set one.
		// The variants here do (WithAcceleratorNodeSelector), so declaring it for
		// the duration of this suite is safe and is what the suite is about: with
		// no limiter there is no ConstraintProvider, every wake is published
		// unchecked, and the refusal this spec waits for can never be logged.
		By("Declaring the physical limiter this suite exists to exercise")
		cmName := scalingPolicyConfigMapName()
		cm, getErr := k8sClient.CoreV1().ConfigMaps(cfg.LLMSMNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if getErr == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else {
			Expect(errors.IsNotFound(getErr)).To(BeTrue(), "unexpected error reading %s", cmName)
		}
		Expect(upsertSaturationConfigEntry(ctx, cfg.LLMSMNamespace, cmName, defaultConfigKey,
			buildSaturationConfigYAMLWithLimiter("saturation",
				saturationKVCacheThreshold, saturationQueueLengthThreshold,
				saturationScaleUpThreshold, saturationScaleDownBoundary,
				"gpu-inventory"),
		)).To(Succeed())
		DeferCleanup(func() {
			restoreSaturationConfigMap(ctx, cfg.LLMSMNamespace, cmName, cmOriginal, cmExistedBefore)
		})

		By("Cleaning up any existing scale-from-zero test resources")
		cleanupScaleFromZeroResources()

		By("Waiting for EPP pods to be ready")
		Eventually(func(g Gomega) {
			pods, err := utils.FindExistingEPPPods(ctx, k8sClient, cfg.LLMDNamespace, cfg.EPPServiceName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods).ToNot(BeEmpty())
		}).Should(Succeed())

		By("Applying InferenceObjective for GIE queuing")
		poolRefName := cfg.PoolName
		if poolRefName == "" {
			poolRefName = strings.TrimSuffix(cfg.EPPServiceName, "-epp")
		}
		ioApplied, errIO := fixtures.EnsureInferenceObjective(ctx, crClient, cfg.LLMDNamespace, poolRefName)
		Expect(errIO).NotTo(HaveOccurred())
		if !ioApplied {
			Skip("InferenceObjective API not available on cluster")
		}

		By("Running an occupier that consumes the whole " + fullAccelerator + " pool")
		// Also serves condition (2): an active variant is what makes the
		// saturation engine publish a usage snapshot at all.
		occupierModel := sfzModelID("sfz-cap-occupier")
		Expect(fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, occupierSvc, poolName,
			occupierModel, cfg.UseSimulator, cfg.MaxNumSeqs,
			fixtures.WithAcceleratorNodeSelector(fullAccelerator),
			fixtures.WithGPURequest(gpusPerNode),

			fixtures.WithPoolGuide("sfz-cap-occupier-pool"))).To(Succeed())

		// Scraped, so its replicas turn into measured usage.
		Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, occupierSvc, occupierDeploy, 8000)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, occupierSvc, occupierDeploy)).To(Succeed())
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, occupierScal, occupierDeploy,
			occupierVar, 1, 10, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(occupierModel, "10.0"),
			fixtures.WithExternalScalerPushTrigger(externalScalerAddress()),
		)).To(Succeed())

		By("Creating a parked variant on the exhausted accelerator")
		createParkedVariant(deniedSvc, deniedDeploy, deniedVar, deniedScal, poolName, fullAccelerator)

		DeferCleanup(func() {
			for _, s := range []string{occupierScal, deniedScal} {
				_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, s)
			}
			for _, d := range []string{occupierDeploy, deniedDeploy} {
				_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, d, metav1.DeleteOptions{})
			}
			if triggerJobName != "" {
				propagation := metav1.DeletePropagationBackground
				_ = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, triggerJobName, metav1.DeleteOptions{
					PropagationPolicy: &propagation,
				})
			}
		})

		By("Waiting for the occupier to be running so its GPUs are accounted for")
		Eventually(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, occupierDeploy, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">", 0),
				"the occupier must actually run, or its GPUs are not counted as used")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).
			Should(Succeed())
	})

	It("refuses to wake a variant whose accelerator is full", func() {
		By("Requiring the serving pool to be empty, or nothing will ever queue")
		// The occupier is deliberately NOT in this pool (WithPoolGuide), so it holds
		// GPUs without answering the requests this spec depends on queueing.
		requireEmptyServingPool()

		By("Sending requests so both parked variants have demand")
		triggerStart := time.Now()
		triggerJobName = fmt.Sprintf("sfz-cap-trigger-%d", time.Now().Unix())
		job := createScaleFromZeroTriggerJob(triggerJobName, cfg.LLMDNamespace, cfg.EPPServiceName, sfzModelID("sfz-cap"))
		_, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "job-name=" + triggerJobName,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(podList.Items).ToNot(BeEmpty())
			g.Expect(podList.Items[0].Status.Phase).To(Or(Equal(corev1.PodRunning), Equal(corev1.PodSucceeded)))
		}).Should(Succeed())

		By("Waiting for the engine to report the refusal, naming the capacity reason")
		// The engine's own verdict, not the absence of a scale-up: a target that
		// merely has not been woken YET looks identical from the outside.
		expectScaleFromZeroRefusal(triggerStart, "no-capacity")

		By("Verifying the denied variant stayed at zero")
		Consistently(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deniedDeploy, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var replicas int32
			if deploy.Spec.Replicas != nil {
				replicas = *deploy.Spec.Replicas
			}
			g.Expect(replicas).To(BeZero(), "a variant that cannot be placed must not be woken")
		}, 20*time.Second, 5*time.Second).Should(Succeed())
	})

})

// createParkedVariant builds a model service pinned to an accelerator, parks it
// at zero, and puts an annotated ScaledObject in front of it.
func createParkedVariant(svcName, deployName, variantName, scalerName, poolName, accelerator string) {
	GinkgoHelper()
	Expect(fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, svcName, poolName,
		sfzModelID("sfz-cap"), cfg.UseSimulator, cfg.MaxNumSeqs,
		fixtures.WithAcceleratorNodeSelector(accelerator),
		fixtures.WithGPURequest(1))).To(Succeed())

	Eventually(func(g Gomega) {
		deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deployName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		deploy.Spec.Replicas = ptr.To(int32(0))
		_, err = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Update(ctx, deploy, metav1.UpdateOptions{})
		g.Expect(err).NotTo(HaveOccurred())
	}, time.Duration(cfg.EventuallyShortSec)*time.Second, time.Duration(cfg.PollIntervalQuickSec)*time.Second).
		Should(Succeed())

	Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerName, deployName, variantName,
		0, 10, cfg.MonitoringNS,
		fixtures.WithScalingManagerTriggerMetadata(sfzModelID("sfz-cap"), "30.0"),
		fixtures.WithExternalScalerPushTrigger(externalScalerAddress()),
	)).To(Succeed())
}

// expectScaleFromZeroRefusal waits until the engine reports that it declined to
// wake anything, for the given reason.
//
// Deliberately not scoped to a variant: the refusal is a per-MODEL verdict — the
// engine picks a serving set for the model as a whole and logs namespace, modelID
// and reason, never a candidate it rejected. Asserting on a variant name would
// mean asserting on a string the engine does not emit.
//
// The refusal is logged once per verdict change rather than once per poll, so the
// window has to start before the trigger.
func expectScaleFromZeroRefusal(since time.Time, reason string) {
	GinkgoHelper()
	// Record the demand chain before the Ordered container's AfterAll tears it
	// down; see the same guard in expectScaleFromZeroEngineActivation.
	defer func() {
		if CurrentSpecReport().Failed() {
			utils.DumpDemandEvidence(ctx, k8sClient, cfg.LLMDNamespace, GinkgoWriter)
		}
	}()

	const pattern = "no variant woken for a model with pending requests"
	Eventually(func(g Gomega) {
		sinceSeconds := int64(time.Since(since).Seconds()) + 1
		ok, logs, logErr := utils.ControllerLogsContain(ctx, k8sClient, cfg.LLMSMNamespace, utils.ControllerDeploymentName, pattern, sinceSeconds)
		g.Expect(logErr).NotTo(HaveOccurred())
		g.Expect(ok).To(BeTrue(), "the engine never reported declining to wake anything")
		matched := false
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, pattern) && strings.Contains(line, reason) {
				matched = true
				break
			}
		}
		g.Expect(matched).To(BeTrue(), "no refusal line carried reason %q", reason)
	}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}
