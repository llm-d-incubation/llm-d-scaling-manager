package e2e

import (
	"os"
	"strings"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// scalerTLSOn records whether the controller serves the external scaler over
// the installer's mutual TLS (configureScalerTLS).
var scalerTLSOn bool

// configureScalerTLS reads the controller Deployment and, when it serves the
// installer's mutual TLS, points every fixture at the TLS scaler address with
// KEDA's ClusterTriggerAuthentication, as the installer's ScaledObjects are.
func configureScalerTLS() {
	dep, err := k8sClient.AppsV1().Deployments(cfg.LLMSMNamespace).Get(ctx, utils.ControllerDeploymentName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "reading the controller Deployment to learn its scaler TLS mode")
	provider := dep.Spec.Template.Annotations["scaling-manager.llmd.ai/scaler-tls"]
	certFlag := false
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name != utils.ControllerContainerName {
			continue
		}
		for _, a := range c.Args {
			if strings.HasPrefix(a, "--external-scaler-cert-path") {
				certFlag = true
			}
		}
	}
	scalerTLSOn = provider != "" && certFlag
	if !scalerTLSOn {
		// CI sets this, so an install that quietly fell back to plaintext fails
		// the run instead of skipping the TLS spec.
		Expect(os.Getenv("E2E_EXPECT_SCALER_TLS")).NotTo(Equal("true"),
			"E2E_EXPECT_SCALER_TLS=true, but the controller serves the external scaler in plaintext; the install log says why")
		GinkgoWriter.Printf("External scaler: plaintext\n")
		return
	}
	tlsAddr := "scaling-manager-external-scaler." + cfg.LLMSMNamespace + ".svc:9090"
	auth := "scaling-manager-external-scaler-" + cfg.LLMSMNamespace
	fixtures.SetExternalScalerTLS(externalScalerAddress(), tlsAddr, auth)
	GinkgoWriter.Printf("External scaler: mutual TLS (%s); fixtures use %s with ClusterTriggerAuthentication %s\n",
		provider, tlsAddr, auth)
}

// The installer turns mutual TLS on by default, and every other spec reaches
// the scaler through it. This one checks the other half: a ScaledObject without
// KEDA's client certificate is refused. Both ScaledObjects target identical
// Deployments, so the only difference between the one that wires and the one
// that does not is the trigger's TLS settings.
var _ = Describe("External scaler mutual TLS", Label("smoke", "full"), Ordered, func() {
	const (
		tlsTarget   = "scaler-tls-ok"
		plainTarget = "scaler-tls-plain"
	)

	BeforeAll(func() {
		if !scalerTLSOn {
			Skip("The controller serves the external scaler in plaintext (LLMSM_SCALER_TLS=false, or the install could not turn TLS on), so there is no TLS to refuse a client.")
		}
		for _, name := range []string{tlsTarget, plainTarget} {
			Expect(ensurePauseDeployment(cfg.LLMDNamespace, name)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, name, metav1.DeleteOptions{})
			})
		}
	})

	It("wires a ScaledObject that presents KEDA's client certificate, and refuses one that does not", func() {
		By("Creating a ScaledObject on the TLS address with KEDA's authenticationRef")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, tlsTarget, tlsTarget, tlsTarget, 1, 2, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(cfg.ModelID, "10.0"),
			fixtures.WithExternalScalerTrigger(externalScalerAddress()))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, tlsTarget) })

		By("Creating a ScaledObject on the plaintext address with no authenticationRef")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, plainTarget, plainTarget, plainTarget, 1, 2, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(cfg.ModelID, "10.0"),
			fixtures.WithExternalScalerTrigger(externalScalerAddress()),
			fixtures.WithPlaintextScalerTrigger())).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, plainTarget) })

		By("The TLS ScaledObject gets the scaling manager's external metric on its HPA")
		waitForKEDAExternalMetricWired(cfg.LLMDNamespace, tlsTarget, tlsTarget)

		By("The plaintext ScaledObject never does: the scaler refuses the handshake")
		Consistently(func(g Gomega) {
			latched, carried, why := hpaOnCPUDefault(cfg.LLMDNamespace, plainTarget)
			GinkgoWriter.Printf("plaintext ScaledObject's HPA: on CPU default=%v, metrics=%v %s\n", latched, carried, why)
			for _, m := range carried {
				g.Expect(m).NotTo(Equal("External"),
					"KEDA got a metric spec over plaintext from a scaler that should require mutual TLS")
			}
		}, 90*time.Second, 10*time.Second).Should(Succeed())

		// No External metric could also mean KEDA never looked at the object.
		// So check that it did, and was refused. KEDA before 2.20 builds the
		// HPA anyway, with no metrics, and the API server defaults it to CPU.
		// KEDA 2.20 and later builds none: the reconcile fails with "no metric
		// specs returned from scalers", is retried, and the ScaledObject reads
		// Ready=False with reason ScaledObjectCheckFailed.
		By("KEDA reconciled the plaintext ScaledObject and was refused: an HPA on the CPU fallback, or none and Ready=False")
		latched, carried, why := hpaOnCPUDefault(cfg.LLMDNamespace, plainTarget)
		if latched {
			return
		}
		so := &kedav1alpha1.ScaledObject{}
		Expect(crClient.Get(ctx, client.ObjectKey{Namespace: cfg.LLMDNamespace, Name: plainTarget + "-so"}, so)).To(Succeed())
		ready := so.Status.Conditions.GetReadyCondition()
		Expect(ready.Status).To(Equal(metav1.ConditionFalse),
			"expected an HPA on the CPU fallback (KEDA < 2.20) or a ScaledObject that is not Ready (KEDA >= 2.20); HPA carried %v %s, Ready=%s %s: %s",
			carried, why, ready.Status, ready.Reason, ready.Message)
		Expect(ready.Reason).To(Equal("ScaledObjectCheckFailed"), "Ready=False for another reason: %s", ready.Message)
	})
})

// ensurePauseDeployment creates a one-replica Deployment that only needs to
// exist as a scale target.
func ensurePauseDeployment(namespace, name string) error {
	labels := map[string]string{"app": name, "test-resource": "true"}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "pause",
					Image: "registry.k8s.io/pause:3.10",
				}}},
			},
		},
	}
	_, err := k8sClient.AppsV1().Deployments(namespace).Create(ctx, dep, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
