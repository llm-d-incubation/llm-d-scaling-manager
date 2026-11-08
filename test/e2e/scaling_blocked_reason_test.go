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

// A blocked reason that actually FIRES.
//
// Every other scale-to/from-zero spec asserts llmsm_model_scaling_blocked is
// SILENT — that a reason does not appear for a correctly configured model. Those
// are the right assertions and they caught real bugs, but together they leave the
// metric's positive path untested end to end: a change that stopped emitting the
// series altogether would satisfy all of them. So would one that emitted it with
// the wrong label set, since a query for a reason that is never expected returns
// nothing either way.
//
// That matters more than a missing unit test would, because this metric is the
// only channel the condition has. The scaling manager owns no API object, so there is no status
// condition to fall back on: if the series is wrong, the dashboard panel and the
// alert built on it are wrong too, and the operator's first symptom is a model
// that silently refuses to park.
//
// variant-floor is the reason chosen because it is the one an operator can
// produce deliberately and unambiguously: the policy says a model may park, and a
// variant's own minReplicaCount says it may not. Half a configuration, which is
// exactly the contradiction the metric exists to name.
var _ = Describe("Scale-To-Zero Feature - a blocked reason is reported", Serial, Label("full"), Ordered, func() {
	const (
		modelSvcName = "stz-floor-ms"
		decodeDeploy = modelSvcName + "-decode"
		poolName     = "stz-floor-pool"
		scalerBase   = "stz-floor"
		variantName  = "stz-floor-so"
	)

	var (
		modelID         string
		cmName          string
		cmNamespace     string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
	)

	BeforeAll(func() {
		if !cfg.ScaleToZeroEnabled {
			Skip("This suite needs the scale-to-zero enforcement path: set SCALE_TO_ZERO_ENABLED=true")
		}

		modelID = sfzModelID("stz-floor")
		cmName = scalingPolicyConfigMapName()
		cmNamespace = cfg.LLMSMNamespace

		By("Snapshotting the scaling policy ConfigMap for restore in AfterAll")
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "reading existing scaling policy ConfigMap")
		}

		By("Enabling scale-to-zero in the policy for this model")
		// Half one of the contradiction. The other half is minReplicaCount below.
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, defaultConfigKey,
			buildSaturationConfigYAML())).To(Succeed())
		modelEntry := buildSaturationConfigYAMLWithModel(
			0.80, 1, 0.85, 0.70, modelID, cfg.LLMDNamespace,
		) + "scaleToZero:\n  enabled: true\n  retentionPeriod: 10m\n"
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, "stz-floor-model", modelEntry)).To(Succeed())

		By("Creating the model service")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName,
			poolName, modelID, cfg.UseSimulator, cfg.MaxNumSeqs)).To(Succeed())
		Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, decodeDeploy, 8000)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS,
			cfg.LLMDNamespace, modelSvcName, decodeDeploy)).To(Succeed())

		By("Waiting for it to be ready")
		// The model must be ACTIVE for the steady-state engine to evaluate it at
		// all — a variant at zero replicas never reaches the enforcement path, so a
		// suite that parked it first could never observe the reason.
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, decodeDeploy, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", int32(1)))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering it with minReplicaCount=1, contradicting the policy")
		_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBase)
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBase,
			decodeDeploy, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(modelID, "30.0"))).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBase)
		})
	})

	AfterAll(func() {
		restoreSaturationConfigMap(ctx, cmNamespace, cmName, cmOriginal, cmExistedBefore)
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
	})

	It("reports variant-floor for a model the policy allows to park but a variant does not", func() {
		pc := promClientForCheck()
		if pc == nil {
			Skip("no Prometheus client could be built")
		}
		if _, err := pc.QueryWithRetry(ctx, "vector(1)"); err != nil {
			Skip("Prometheus is not reachable from the test host: " + err.Error())
		}

		// exported_namespace, not namespace: the scrape job carries the controller's
		// own namespace, so Prometheus renames the one the series declares.
		By("Asserting the reason appears, with the label set the dashboard and alert query")
		Eventually(func(g Gomega) {
			v, err := pc.QueryWithRetry(ctx, fmt.Sprintf(
				`count(llmsm_model_scaling_blocked{exported_namespace=%q,model_name=%q,reason="variant-floor"})`,
				cfg.LLMDNamespace, modelID))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v).To(BeNumerically(">", 0),
				"the policy enables scale-to-zero and the variant floors it at 1, which is the "+
					"contradiction variant-floor names; if this is absent the metric is not being "+
					"emitted, and the dashboard panel and alert built on it are reporting nothing")
		}, 5*time.Minute, 15*time.Second).Should(Succeed())
	})

	It("does not also report the reasons that do not apply", func() {
		pc := promClientForCheck()
		if pc == nil {
			Skip("no Prometheus client could be built")
		}
		if _, err := pc.QueryWithRetry(ctx, "vector(1)"); err != nil {
			Skip("Prometheus is not reachable from the test host: " + err.Error())
		}

		// The reasons are a set, not a flag, and the two producers own disjoint
		// halves of it. A regression that emitted the whole owned set whenever any
		// member applied would satisfy the spec above and make the metric useless —
		// every model would report every reason.
		v, err := pc.QueryWithRetry(ctx, fmt.Sprintf(
			`count(llmsm_model_scaling_blocked{exported_namespace=%q,model_name=%q,`+
				`reason=~"policy-forbids-zero|engine-unsupported"}) or vector(0)`,
			cfg.LLMDNamespace, modelID))
		Expect(err).NotTo(HaveOccurred())
		Expect(v).To(BeNumerically("==", 0),
			"only variant-floor applies here: the policy permits zero, and the model runs one engine")
	})
})
