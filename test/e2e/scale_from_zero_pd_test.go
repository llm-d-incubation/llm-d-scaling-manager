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

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// Scale-from-zero for a P/D-disaggregated model.
//
// The scale-from-zero engine decides per MODEL, not per variant: it wakes a
// serving set. Decode is mandatory and prefill is best-effort, because with no
// prefill endpoint selected the llm-d router runs both stages on the decode
// worker — so decode alone serves, and refusing to wake without prefill would
// trade a slower model for no model.
//
// What this suite covers that the unit tests cannot: that the role really is
// read off a live Deployment's pod template, that two workloads sharing a model
// ID are discovered as ONE model with two roles, and that the engine's chosen
// set reaches KEDA. The selection arithmetic itself (exhaustive pair search,
// joint GPU budget, requirePrefill) is unit-tested in
// internal/engines/scalefromzero.
//
// Note this exercises the scaling manager's side of P/D only. The e2e EPP is not configured for
// disaggregation (a single "default" scheduling profile, no pd plugins), so
// requests are served by whichever endpoint comes up — which is precisely the
// decode-only behaviour the router falls back to anyway.
var _ = Describe("Scale-From-Zero for a P/D-disaggregated model", Serial, Label("full"), Ordered, func() {
	const (
		decodeSvc   = "sfz-pd-decode-ms"
		prefillSvc  = "sfz-pd-prefill-ms"
		poolName    = "sfz-pd-pool"
		decodeVar   = "sfz-pd-decode-va"
		prefillVar  = "sfz-pd-prefill-va"
		decodeScal  = "sfz-pd-decode"
		prefillScal = "sfz-pd-prefill"
	)
	decodeDeploy := decodeSvc + "-decode"
	prefillDeploy := prefillSvc + "-decode"

	var triggerJobName string

	// ensureRoleWorkloadAtZero creates a role-labelled model service, parks it at
	// zero, and puts an annotated ScaledObject in front of it.
	ensureRoleWorkloadAtZero := func(svcName, deployName, variantName, scalerName, role string) {
		GinkgoHelper()
		Expect(fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, svcName, poolName,
			sfzModelID("sfz-pd"), cfg.UseSimulator, cfg.MaxNumSeqs,
			fixtures.WithRole(role))).To(Succeed(), "creating the %s model service", role)

		Eventually(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			deploy.Spec.Replicas = ptr.To(int32(0))
			_, err = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Update(ctx, deploy, metav1.UpdateOptions{})
			g.Expect(err).NotTo(HaveOccurred())
		}, time.Duration(cfg.EventuallyShortSec)*time.Second, time.Duration(cfg.PollIntervalQuickSec)*time.Second).
			Should(Succeed(), "parking the %s workload at zero", role)

		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerName, deployName, variantName, 0, 10, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(sfzModelID("sfz-pd"), "30.0"),
			fixtures.WithExternalScalerPushTrigger(externalScalerAddress()),
		)).To(Succeed(), "creating the %s ScaledObject", role)
	}

	BeforeAll(func() {
		if !cfg.ScaleToZeroEnabled {
			Skip("This suite requires EPP flow-control queuing: set SCALE_TO_ZERO_ENABLED=true")
		}
		if ok, why := eppFlowControlAvailable(ctx, crClient,
			cfg.LLMSMNamespace, cfg.LLMDNamespace); !ok {
			Skip("EPP flow control is not available, so there is no wake signal to test: " + why)
		}

		By("Cleaning up any existing scale-from-zero test resources")
		cleanupScaleFromZeroResources()

		By("Waiting for EPP pods to be ready")
		Eventually(func(g Gomega) {
			pods, err := utils.FindExistingEPPPods(ctx, k8sClient, cfg.LLMDNamespace, cfg.EPPServiceName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods).ToNot(BeEmpty(), "EPP pods should exist")
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

		By("Creating a decode workload and a prefill workload for the same model, both at zero")
		ensureRoleWorkloadAtZero(decodeSvc, decodeDeploy, decodeVar, decodeScal, domain.RoleDecode)
		ensureRoleWorkloadAtZero(prefillSvc, prefillDeploy, prefillVar, prefillScal, domain.RolePrefill)

		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, decodeScal)
			_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, prefillScal)
			for _, d := range []string{decodeDeploy, prefillDeploy} {
				_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, d, metav1.DeleteOptions{})
			}
			if triggerJobName != "" {
				propagation := metav1.DeletePropagationBackground
				_ = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, triggerJobName, metav1.DeleteOptions{
					PropagationPolicy: &propagation,
				})
			}
		})
	})

	It("carries the P/D role on each workload's pod template", func() {
		// The role scaling manager selects on comes from this label; if the fixture stopped
		// setting it both workloads would read as "both" and the suite would be
		// silently testing the non-disaggregated path instead.
		for deployName, wantRole := range map[string]string{
			decodeDeploy:  domain.RoleDecode,
			prefillDeploy: domain.RolePrefill,
		} {
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deployName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(deploy.Spec.Template.Labels).To(HaveKeyWithValue("llm-d.ai/role", wantRole),
				"%s should carry the %s role label", deployName, wantRole)
		}
	})

	It("wakes the model's decode variant when requests queue", func() {
		By("Discovering the gateway service")
		gatewayServiceName := cfg.EPPServiceName
		svcList, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, svc := range svcList.Items {
			if strings.Contains(svc.Name, "inference-gateway") {
				gatewayServiceName = svc.Name
				break
			}
		}

		By("Requiring the serving pool to be empty, or nothing will ever queue")
		requireEmptyServingPool()

		By("Sending requests while both roles are parked at zero")
		triggerStart := time.Now()
		triggerJobName = fmt.Sprintf("sfz-pd-trigger-%d", time.Now().Unix())
		job := createScaleFromZeroTriggerJob(triggerJobName, cfg.LLMDNamespace, gatewayServiceName, sfzModelID("sfz-pd"))
		_, err = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "job-name=" + triggerJobName,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(podList.Items).ToNot(BeEmpty())
			g.Expect(podList.Items[0].Status.Phase).To(Or(Equal(corev1.PodRunning), Equal(corev1.PodSucceeded)))
		}).Should(Succeed())

		By("Waiting for the engine to publish an activation for the decode variant")
		// Decode is the mandatory half of the serving set: whatever happens to
		// prefill, a model that cannot serve is a failure.
		expectScaleFromZeroEngineActivation(decodeScal+"-so", triggerStart)

		By("Waiting for the engine to publish an activation for the prefill variant")
		// BOTH halves must be woken — that is what "discovered as one model with
		// two roles" means, and it is the thing this suite exists to catch.
		//
		// The serving-set LABEL is deliberately not asserted. Each half is
		// discovered when KEDA first calls about it, so the two routinely become
		// visible on different ticks: sometimes the engine pairs them in one
		// decision ("decode+prefill"), sometimes it wakes decode first and
		// completes the model on a later tick ("prefill-catchup"). Both are
		// correct, and pinning one of them made this spec assert a race rather
		// than the contract. What must hold either way is that prefill is woken
		// and stops being stranded at zero.
		expectScaleFromZeroEngineActivation(prefillScal+"-so", triggerStart)

		By("Verifying the prefill workload also leaves zero")
		Eventually(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, prefillDeploy, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var specReplicas int32
			if deploy.Spec.Replicas != nil {
				specReplicas = *deploy.Spec.Replicas
			}
			g.Expect(specReplicas).To(BeNumerically(">", 0),
				"a disaggregated model whose prefill never leaves zero runs degraded forever")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).
			Should(Succeed())

		By("Verifying the decode workload leaves zero")
		Eventually(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, decodeDeploy, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var specReplicas int32
			if deploy.Spec.Replicas != nil {
				specReplicas = *deploy.Spec.Replicas
			}
			g.Expect(specReplicas).To(BeNumerically(">", 0), "decode should be woken from zero")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).
			Should(Succeed())
	})

	It("never wakes prefill alone", func() {
		// The one P/D outcome that is always wrong. Prefill may or may not be
		// woken depending on capacity, but a running prefill with decode still at
		// zero cannot serve a request.
		deployList, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())

		replicasOf := func(name string) int32 {
			for _, d := range deployList.Items {
				if d.Name == name && d.Spec.Replicas != nil {
					return *d.Spec.Replicas
				}
			}
			return 0
		}

		prefillReplicas := replicasOf(prefillDeploy)
		decodeReplicas := replicasOf(decodeDeploy)
		GinkgoWriter.Printf("decode=%d prefill=%d\n", decodeReplicas, prefillReplicas)

		if prefillReplicas > 0 {
			Expect(decodeReplicas).To(BeNumerically(">", 0),
				"prefill was woken without decode: the model cannot serve in that state")
		}
	})

	AfterAll(func() {
		By("Cleaning up P/D scale-from-zero resources")
		if triggerJobName != "" {
			propagation := metav1.DeletePropagationBackground
			err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, triggerJobName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			})
			if err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: failed to delete trigger job %s: %v\n", triggerJobName, err)
			}
		}
	})
})
