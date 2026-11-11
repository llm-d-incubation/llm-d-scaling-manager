package preflight

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// KEDA versions, as the installer judges them (deploy/lib/scaler_runtime.sh).
var (
	// kedaMinVersion is the oldest KEDA a release is tested on: below it,
	// preflight FAILs.
	kedaMinVersion = semver{2, 18, 0}
	// kedaSupportedMinors are the minors reported as supported; another
	// minor at or above the minimum is untested, a WARN.
	kedaSupportedMinors = []semver{{2, 18, 0}, {2, 19, 0}, {2, 21, 0}}
	// kedaCVEFixed is the first KEDA without CVE-2026-77524.
	kedaCVEFixed = semver{2, 21, 0}
	// kedaFallbackBehavior is the first KEDA whose ScaledObject has
	// spec.fallback.behavior.
	kedaFallbackBehavior = semver{2, 17, 0}
)

// kedaOperatorSelector finds KEDA's operator Deployment.
const kedaOperatorSelector = "app.kubernetes.io/name=keda-operator"

// checkKEDACRD: the ScaledObject CRD is installed.
func checkKEDACRD(ctx context.Context, s *state) ([]Result, string) {
	const crd = "scaledobjects.keda.sh"
	obj, err := s.env.Dynamic.Resource(crdGVR).Get(ctx, crd, metav1.GetOptions{})
	if err != nil {
		absent, why := readVerdict(err, "CustomResourceDefinition "+crd)
		if absent {
			return []Result{{Status: Fail, Detail: "CustomResourceDefinition " + crd + " is absent: KEDA is not installed",
				Fix: "install KEDA 2.21 or later: https://keda.sh/docs/latest/deploy/"}}, ""
		}
		return []Result{{Status: Unknown, Detail: why}}, ""
	}
	return []Result{{Status: Pass, Detail: crd + " " + strings.Join(servedVersions(obj), ",")}}, ""
}

// servedVersions lists the versions a CRD serves.
func servedVersions(obj *unstructured.Unstructured) []string {
	vs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "versions")
	var out []string
	for _, v := range vs {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if served, _ := m["served"].(bool); served {
			if name, _ := m["name"].(string); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// checkKEDAVersion reads the operator's version from its image tag.
func checkKEDAVersion(ctx context.Context, s *state) ([]Result, string) {
	list, err := s.env.Kube.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: kedaOperatorSelector})
	if err != nil {
		return []Result{unknown("deployments in every namespace (to find KEDA's operator)", err)}, ""
	}
	if len(list.Items) == 0 || len(list.Items[0].Spec.Template.Spec.Containers) == 0 {
		return []Result{{Status: Unknown, Detail: "no Deployment labelled " + kedaOperatorSelector + " (OpenShift's Custom Metrics Autoscaler and other distributions label it differently)"}}, ""
	}
	d := list.Items[0]
	image := d.Spec.Template.Spec.Containers[0].Image
	v, ok := imageVersion(image)
	if !ok {
		return []Result{{Status: Unknown, Detail: fmt.Sprintf("the operator image %s (%s/%s) names no version", image, d.Namespace, d.Name)}}, ""
	}
	where := fmt.Sprintf("KEDA %s (%s/%s)", v, d.Namespace, d.Name)
	fallback := "fallback.behavior available"
	if v.less(kedaFallbackBehavior) {
		fallback = "no fallback.behavior: write ScaledObjects without a fallback block"
	}
	upgrade := "upgrade KEDA: https://keda.sh/docs/latest/deploy/"
	switch {
	case v.less(kedaMinVersion):
		return []Result{{Status: Fail, Detail: fmt.Sprintf("%s is below %s, the oldest KEDA this release is tested on; %s", where, kedaMinVersion, fallback), Fix: upgrade}}, ""
	case !v.sameMinorAsAny(kedaSupportedMinors):
		return []Result{{Status: Warn, Detail: fmt.Sprintf("%s is untested (supported: %s); %s", where, minorsText(kedaSupportedMinors), fallback)}}, ""
	case v.less(kedaCVEFixed):
		return []Result{{Status: Warn, Detail: fmt.Sprintf("%s carries CVE-2026-77524 (fixed in %s): a tenant who may write a TriggerAuthentication can obtain the operator's token; %s", where, kedaCVEFixed, fallback), Fix: upgrade}}, ""
	}
	return []Result{{Status: Pass, Detail: where + ", supported; " + fallback}}, ""
}

// checkKEDANetworkPolicy: the scaler NetworkPolicy admits the namespace KEDA
// runs in (decision 6). Before the install it compares with what the release
// file will apply: keda, or openshift-keda on OpenShift.
func checkKEDANetworkPolicy(ctx context.Context, s *state) ([]Result, string) {
	ns := s.env.Namespace
	keda, kind, why := s.kedaNamespace(ctx)
	switch kind {
	case kedaNSUnread:
		return []Result{{Status: Unknown, Detail: "where KEDA runs is not known: " + why}}, ""
	case kedaNSAbsent:
		return []Result{{Status: Unknown, Detail: why + " (see keda.crd)"}}, ""
	case kedaNSOther:
		return []Result{{Status: Warn, Detail: why + ": KEDA's ScaledObjects get no metrics through it, and the NetworkPolicy is not checked"}}, ""
	}
	admitted, ruleIndex, source := "", 1, ""
	np, err := s.env.Kube.NetworkingV1().NetworkPolicies(ns).Get(ctx, ScalerNetworkPolicy, metav1.GetOptions{})
	switch err {
	case nil:
		admitted, ruleIndex = scalerAdmits(np)
		source = "NetworkPolicy " + ScalerNetworkPolicy
		if ruleIndex < 0 {
			return []Result{{Status: Unknown, Detail: source + " in " + ns + " admits port 9090 from no single namespace; what it lets in is not known here"}}, ""
		}
	default:
		absent, why := readVerdict(err, "NetworkPolicy "+ScalerNetworkPolicy+" in "+ns)
		if !absent {
			return []Result{{Status: Unknown, Detail: why}}, ""
		}
		osh, osWhy := s.isOpenShift()
		switch osh {
		case triYes:
			admitted = "openshift-keda"
		case triNo:
			admitted = "keda"
		default:
			return []Result{{Status: Unknown, Detail: "not installed yet, and which release file applies is not known: " + osWhy}}, ""
		}
		source = "the release file's NetworkPolicy " + ScalerNetworkPolicy + " (not applied yet)"
	}
	if admitted == keda {
		return []Result{{Status: Pass, Detail: fmt.Sprintf("%s admits %s, where KEDA runs", source, keda)}}, ""
	}
	return []Result{{Status: Fail,
		Detail: fmt.Sprintf("%s admits port 9090 from %s, but KEDA runs in %s: KEDA's calls are dropped and no ScaledObject scales", source, admitted, keda),
		Fix: fmt.Sprintf(`kubectl patch networkpolicy %s -n %s --type json -p '[{"op":"replace","path":"/spec/ingress/%d/from/0/namespaceSelector/matchLabels/kubernetes.io~1metadata.name","value":"%s"}]'`,
			ScalerNetworkPolicy, ns, ruleIndex, keda)}}, ""
}

// scalerAdmits returns the one namespace the policy admits to port 9090 and
// the index of that ingress rule, or -1 when it is not that shape.
func scalerAdmits(np *networkingv1.NetworkPolicy) (string, int) {
	for i, rule := range np.Spec.Ingress {
		has9090 := false
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntValue() == 9090 {
				has9090 = true
			}
		}
		if !has9090 {
			continue
		}
		if len(rule.From) != 1 || rule.From[0].NamespaceSelector == nil {
			return "", -1
		}
		name := rule.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
		if name == "" {
			return "", -1
		}
		return name, i
	}
	return "", -1
}

// checkKEDAMTLS: KEDA's side of mutual TLS. Without the marker the scaler
// serves plaintext (WARN). With it, the ClusterTriggerAuthentication exists,
// and the certificates KEDA was given chain to the controller's: the check
// PR #70 added to the installer (llmsm_scaler_keda_trust_state).
func checkKEDAMTLS(ctx context.Context, s *state) ([]Result, string) {
	ns := s.env.Namespace
	keda, kind, _ := s.kedaNamespace(ctx)
	if kind != kedaNSFound {
		keda = "<KEDA namespace>"
	}
	copyName := "scaling-manager-external-scaler-" + ns
	steps := mtlsSteps(ns, keda, copyName)
	marker, err := s.env.Kube.CoreV1().ConfigMaps(ns).Get(ctx, KEDAMarkerConfigMap, metav1.GetOptions{})
	if err != nil {
		absent, why := readVerdict(err, "ConfigMap "+KEDAMarkerConfigMap+" in "+ns)
		if !absent {
			return []Result{{Status: Unknown, Detail: why}}, ""
		}
		return []Result{{Status: Warn,
			Detail: "KEDA's side of mutual TLS is not in place (no ConfigMap " + KEDAMarkerConfigMap + "): the scaler serves plaintext, and any Pod the NetworkPolicy admits can call it",
			Fix:    steps}}, ""
	}
	cta := marker.Data["clusterTriggerAuthentication"]
	if cta == "" {
		cta = copyName
	}
	if _, err := s.env.Dynamic.Resource(ctaGVR).Get(ctx, cta, metav1.GetOptions{}); err != nil {
		absent, why := readVerdict(err, "ClusterTriggerAuthentication "+cta)
		if !absent {
			return []Result{{Status: Unknown, Detail: why}}, ""
		}
		return []Result{{Status: Fail,
			Detail: "the marker names ClusterTriggerAuthentication " + cta + ", which does not exist: the controller serves mutual TLS and KEDA presents no certificate",
			Fix:    steps}}, ""
	}
	copyNS := marker.Data["secretNamespace"]
	if copyNS == "" {
		copyNS = keda
	}
	kedaCopy, err := s.env.Kube.CoreV1().Secrets(copyNS).Get(ctx, copyName, metav1.GetOptions{})
	if err != nil {
		absent, why := readVerdict(err, "Secret "+copyName+" in "+copyNS)
		if !absent {
			return []Result{{Status: Unknown, Detail: why + " (KEDA's copy of the client certificate), so whether KEDA's certificates match is not checked"}}, ""
		}
		return []Result{{Status: Fail,
			Detail: "KEDA's copy of the client certificate, Secret " + copyName + " in " + copyNS + ", does not exist: KEDA presents no certificate and the scaler refuses it",
			Fix:    steps}}, ""
	}
	serving, err := s.env.Kube.CoreV1().Secrets(ns).Get(ctx, ScalerServingSecret, metav1.GetOptions{})
	if err != nil {
		absent, why := readVerdict(err, "Secret "+ScalerServingSecret+" in "+ns)
		if absent {
			why = "the controller has not created its serving certificate (Secret " + ScalerServingSecret + ") yet; run preflight again after the install"
		}
		return []Result{{Status: Unknown, Detail: why}}, ""
	}
	client, err := s.env.Kube.CoreV1().Secrets(ns).Get(ctx, ScalerClientSecret, metav1.GetOptions{})
	if err != nil {
		absent, why := readVerdict(err, "Secret "+ScalerClientSecret+" in "+ns)
		if absent {
			why = "the controller has not created the client CA (Secret " + ScalerClientSecret + ") yet; run preflight again after the install"
		}
		return []Result{{Status: Unknown, Detail: why}}, ""
	}
	now := s.env.Now()
	if err := verifies(kedaCopy.Data["ca.crt"], serving.Data["tls.crt"], now); err != nil {
		return []Result{{Status: Fail,
			Detail: fmt.Sprintf("the CA KEDA trusts (ca.crt of Secret %s in %s) does not verify the scaler's serving certificate (Secret %s): %v. KEDA rejects the scaler",
				copyName, copyNS, ScalerServingSecret, err),
			Fix: steps}}, ""
	}
	if err := verifies(client.Data["ca.crt"], kedaCopy.Data["tls.crt"], now); err != nil {
		return []Result{{Status: Fail,
			Detail: fmt.Sprintf("the controller's client CA (Secret %s) does not verify KEDA's client certificate (tls.crt of Secret %s in %s): %v. The scaler refuses KEDA",
				ScalerClientSecret, copyName, copyNS, err),
			Fix: steps}}, ""
	}
	return []Result{{Status: Pass, Detail: fmt.Sprintf("ClusterTriggerAuthentication %s in place, and KEDA's certificates (Secret %s in %s) chain to the scaler's", cta, copyName, copyNS)}}, ""
}

// mtlsSteps are the commands that put KEDA's side of mutual TLS in place:
// copy the client Secret's data keys into KEDA's namespace, apply the KEDA
// file, restart the controller (it picks mTLS at start) and KEDA's operator
// (it keeps the connection it opened before).
func mtlsSteps(ns, keda, copyName string) string {
	get := func(key string) string {
		return fmt.Sprintf(`"$(kubectl get secret %s -n %s -o jsonpath='{.data.%s}' | base64 -d)"`, ScalerClientSecret, ns, strings.ReplaceAll(key, ".", `\.`))
	}
	return strings.Join([]string{
		fmt.Sprintf("kubectl create secret generic %s -n %s --from-literal=ca.crt=%s --from-literal=tls.crt=%s --from-literal=tls.key=%s --dry-run=client -o yaml | kubectl apply -f -",
			copyName, keda, get("ca.crt"), get("tls.crt"), get("tls.key")),
		fmt.Sprintf(`sed -e "s/__NAMESPACE__/%s/g" -e "s/__KEDA_NAMESPACE__/%s/g" scaling-manager-keda-auth.yaml | kubectl apply -f -`, ns, keda),
		fmt.Sprintf("kubectl rollout restart deployment/%s -n %s", ControllerDeployment, ns),
		fmt.Sprintf("kubectl rollout restart deployment -n %s -l %s", keda, kedaOperatorSelector),
	}, "\n")
}

// verifies checks that the first certificate of leafPEM chains to a CA in
// caPEM, at now.
func verifies(caPEM, leafPEM []byte, now time.Time) error {
	if len(caPEM) == 0 {
		return errors.New("no CA certificate")
	}
	if len(leafPEM) == 0 {
		return errors.New("no certificate")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return errors.New("the CA is not a PEM certificate")
	}
	var leaf *x509.Certificate
	inter := x509.NewCertPool()
	for rest := leafPEM; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return fmt.Errorf("parsing the certificate: %w", err)
		}
		if leaf == nil {
			leaf = c
		} else {
			inter.AddCert(c)
		}
	}
	if leaf == nil {
		return errors.New("the certificate is not PEM")
	}
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: inter, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	return err
}

// semver is a major.minor.patch version.
type semver struct{ major, minor, patch int }

func (v semver) String() string { return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch) }

func (v semver) less(o semver) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	return v.patch < o.patch
}

func (v semver) sameMinorAsAny(list []semver) bool {
	for _, o := range list {
		if v.major == o.major && v.minor == o.minor {
			return true
		}
	}
	return false
}

func minorsText(list []semver) string {
	parts := make([]string, len(list))
	for i, v := range list {
		parts[i] = fmt.Sprintf("%d.%d", v.major, v.minor)
	}
	return strings.Join(parts, ", ")
}

// imageVersion reads the version from an image tag: the part after the last
// ':' of the last path segment, so a registry port is not read as one. A
// digest names no version.
func imageVersion(image string) (semver, bool) {
	if strings.Contains(image, "@") {
		return semver{}, false
	}
	last := image[strings.LastIndex(image, "/")+1:]
	i := strings.LastIndex(last, ":")
	if i < 0 {
		return semver{}, false
	}
	parts := strings.Split(strings.TrimPrefix(last[i+1:], "v"), ".")
	if len(parts) < 2 || len(parts) > 3 {
		return semver{}, false
	}
	var n [3]int
	for j, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return semver{}, false
		}
		n[j] = x
	}
	return semver{n[0], n[1], n[2]}, true
}
