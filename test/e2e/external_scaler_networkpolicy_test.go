package e2e

import (
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// The installer writes the scaling-manager-external-scaler NetworkPolicy with the namespace
// it believes KEDA's operator runs in -- from KEDA_NAMESPACE, the external
// metrics APIService, an earlier policy, the keda-operator Deployment, or a
// platform guess. A wrong guess fails CLOSED: KEDA is refused on :9090, its
// ScaledObjects go not-Ready, and nothing scales, with no error anywhere in
// the scaling manager. This spec compares the installer's answer with where the operator
// actually runs, read from the live cluster.
const (
	externalScalerNetworkPolicy = "scaling-manager-external-scaler"
	externalScalerPort          = 9090
	kedaOperatorPodSelector     = "app.kubernetes.io/name=keda-operator"
)

var _ = Describe("KEDA external scaler NetworkPolicy", Label("smoke", "full"), func() {
	It("admits the namespace KEDA's operator actually runs in to the external scaler port", func() {
		np, err := k8sClient.NetworkingV1().NetworkPolicies(cfg.LLMSMNamespace).Get(ctx, externalScalerNetworkPolicy, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			Skip("NetworkPolicy " + cfg.LLMSMNamespace + "/" + externalScalerNetworkPolicy +
				" is absent: the install opted out of it (LLMSM_SCALER_NETWORKPOLICY=false), so there is nothing to check")
		}
		// Forbidden or any other error is not absence: fail rather than skip.
		Expect(err).NotTo(HaveOccurred(), "reading the external-scaler NetworkPolicy")

		pods, err := k8sClient.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: kedaOperatorPodSelector})
		Expect(err).NotTo(HaveOccurred(), "listing KEDA operator pods cluster-wide")
		seen := map[string]bool{}
		for _, p := range pods.Items {
			seen[p.Namespace] = true
		}
		kedaNamespaces := make([]string, 0, len(seen))
		for ns := range seen {
			kedaNamespaces = append(kedaNamespaces, ns)
		}
		sort.Strings(kedaNamespaces)
		if len(kedaNamespaces) == 0 {
			Skip("no pod labelled " + kedaOperatorPodSelector + " runs in this cluster, so there is no KEDA namespace to compare the NetworkPolicy with")
		}
		Expect(kedaNamespaces).To(HaveLen(1),
			"KEDA's operator runs in more than one namespace; the installer refuses that ambiguity, so this policy cannot be right for all of them")
		kedaNS := kedaNamespaces[0]

		kedaNamespace, err := k8sClient.CoreV1().Namespaces().Get(ctx, kedaNS, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "reading KEDA's namespace %s", kedaNS)
		admitted := scalerPortSelectors(np)
		GinkgoWriter.Printf("KEDA operator namespace: %s (labels %v); NetworkPolicy admits to :%d the namespaces matching: %v\n",
			kedaNS, kedaNamespace.Labels, externalScalerPort, admitted)
		Expect(admitsNamespace(admitted, kedaNamespace.Labels)).To(BeTrue(),
			"the external-scaler NetworkPolicy admits the namespaces matching %v to :%d, but KEDA's operator runs in %q (labels %v): "+
				"KEDA is refused and nothing scales. Reinstall with KEDA_NAMESPACE=%s, or on the one-manifest install label the namespace "+
				"scaling-manager.llmd.ai/keda=true (scaling-manager-admin.sh upgrade --keda-namespace %s)",
			admitted, externalScalerPort, kedaNS, kedaNamespace.Labels, kedaNS, kedaNS)
	})
})

// scalerPortSelectors returns the namespaceSelector labels np admits whole
// namespaces by to the external scaler port: kubernetes.io/metadata.name,
// which names one namespace, or the one-manifest install's
// scaling-manager.llmd.ai/keda=true. A rule with a podSelector beside the
// namespaceSelector, or with matchExpressions, is not counted: neither admits
// a whole namespace by its labels alone.
func scalerPortSelectors(np *networkingv1.NetworkPolicy) []map[string]string {
	var out []map[string]string
	for _, rule := range np.Spec.Ingress {
		if !rulesPort(rule, externalScalerPort) {
			continue
		}
		for _, peer := range rule.From {
			sel := peer.NamespaceSelector
			if sel == nil || peer.PodSelector != nil || len(sel.MatchExpressions) > 0 || len(sel.MatchLabels) == 0 {
				continue
			}
			out = append(out, sel.MatchLabels)
		}
	}
	return out
}

// admitsNamespace reports whether one of selectors matches a namespace with
// labels nsLabels.
func admitsNamespace(selectors []map[string]string, nsLabels map[string]string) bool {
	for _, sel := range selectors {
		if labels.SelectorFromSet(sel).Matches(labels.Set(nsLabels)) {
			return true
		}
	}
	return false
}

// rulesPort reports whether an ingress rule covers TCP port.
func rulesPort(rule networkingv1.NetworkPolicyIngressRule, port int) bool {
	for _, p := range rule.Ports {
		if p.Port != nil && p.Port.IntValue() == port {
			return true
		}
	}
	return false
}
