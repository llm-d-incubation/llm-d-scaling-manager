package preflight

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
	// kedaUnwiredFixed is the first KEDA that does not leave a new
	// ScaledObject unwired after a scaler restart.
	kedaUnwiredFixed = semver{2, 20, 0}
)

// kedaOperatorSelector finds KEDA's operator Deployment.
const kedaOperatorSelector = "app.kubernetes.io/name=keda-operator"

// kedaOperatorDeployment is the operator Deployment's name in KEDA's chart.
const kedaOperatorDeployment = "keda-operator"

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

// checkKEDAVersion reads the operator's version from its image tag. A
// cluster-wide preflight lists the Deployments labelled as KEDA's operator; a
// namespaced one may not list Deployments, so it reads keda-operator by name in
// the namespace KEDA serves external metrics from.
func checkKEDAVersion(ctx context.Context, s *state) ([]Result, string) {
	var d *appsv1.Deployment
	list, err := s.env.Kube.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: kedaOperatorSelector})
	switch {
	case err == nil && len(list.Items) > 0:
		d = &list.Items[0]
	case err == nil:
		return []Result{{Status: Unknown, Detail: "no Deployment labelled " + kedaOperatorSelector + " (OpenShift's Custom Metrics Autoscaler and other distributions label it differently)"}}, ""
	default:
		keda, kind, why := s.kedaNamespace(ctx)
		if kind != kedaNSFound {
			_, lwhy := readVerdict(err, "deployments in every namespace")
			return []Result{{Status: Unknown, Detail: lwhy + ", and where KEDA runs is not known (" + why + ")"}}, ""
		}
		d, err = s.env.Kube.AppsV1().Deployments(keda).Get(ctx, kedaOperatorDeployment, metav1.GetOptions{})
		if err != nil {
			return []Result{unknown("Deployment "+kedaOperatorDeployment+" in "+keda, err)}, ""
		}
	}
	if len(d.Spec.Template.Spec.Containers) == 0 {
		return []Result{{Status: Unknown, Detail: fmt.Sprintf("Deployment %s/%s has no container", d.Namespace, d.Name)}}, ""
	}
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
	if v.less(kedaMinVersion) {
		return []Result{{Status: Fail, Detail: fmt.Sprintf("%s is below %s, the oldest KEDA this release is tested on; %s", where, kedaMinVersion, fallback), Fix: upgrade}}, ""
	}
	// The same notes the installer gives (llmsm_keda_version_judge).
	var notes []string
	if !v.sameMinorAsAny(kedaSupportedMinors) {
		notes = append(notes, "untested (supported: "+minorsText(kedaSupportedMinors)+")")
	}
	if v.less(kedaCVEFixed) {
		notes = append(notes, fmt.Sprintf("carries CVE-2026-77524 (fixed in %s): a tenant who may write a TriggerAuthentication can obtain the operator's token", kedaCVEFixed))
	}
	if v.less(kedaUnwiredFixed) {
		notes = append(notes, fmt.Sprintf("has the bug fixed in %s that leaves a ScaledObject created within about two minutes of a controller restart unwired, its HPA on the CPU default: restart KEDA's operator after every controller restart until it is upgraded", kedaUnwiredFixed))
	}
	if len(notes) > 0 {
		return []Result{{Status: Warn, Detail: where + " " + strings.Join(notes, "; ") + "; " + fallback, Fix: upgrade}}, ""
	}
	return []Result{{Status: Pass, Detail: where + ", supported; " + fallback}}, ""
}

// checkKEDANetworkPolicy: the scaler NetworkPolicy admits the namespace KEDA
// runs in (decision 6). Before the install it compares with what the release
// file will apply as shipped (keda, or openshift-keda on OpenShift) and warns
// on a mismatch, since it cannot see an edited file; the live policy after the
// install fails on one.
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
	np, err := s.env.Kube.NetworkingV1().NetworkPolicies(ns).Get(ctx, ScalerNetworkPolicy, metav1.GetOptions{})
	if err != nil {
		absent, why := readVerdict(err, "NetworkPolicy "+ScalerNetworkPolicy+" in "+ns)
		if !absent {
			return []Result{{Status: Unknown, Detail: why}}, ""
		}
		var shipped string
		switch osh, osWhy := s.isOpenShift(); osh {
		case triYes:
			shipped = "openshift-keda"
		case triNo:
			shipped = "keda"
		default:
			return []Result{{Status: Unknown, Detail: "not installed yet, and which release file applies is not known: " + osWhy}}, ""
		}
		if shipped == keda {
			return []Result{{Status: Pass, Detail: fmt.Sprintf("the release file's NetworkPolicy %s (not applied yet) admits %s, where KEDA runs", ScalerNetworkPolicy, keda)}}, ""
		}
		// The policy does not exist yet, so a kubectl patch would answer
		// NotFound: change the file before applying it. The preflight cannot
		// read the file, so it judges the file as shipped and only warns: an
		// operator who already edited it could not clear a FAIL until the
		// install, and the run after the install checks the live policy.
		files := "scaling-manager-namespaced*.yaml"
		if s.env.Scope == ScopeCluster {
			files = "scaling-manager-cluster*.yaml"
		}
		return []Result{{Status: Warn,
			Detail: fmt.Sprintf("the release file's NetworkPolicy %s (not applied yet), as shipped, admits port 9090 from %s, but KEDA runs in %s: unless the file is changed, KEDA's calls would be dropped and no ScaledObject would scale; if you edited it, the run after the install checks it live",
				ScalerNetworkPolicy, shipped, keda),
			Fix: fmt.Sprintf(`sed -i "s#kubernetes.io/metadata.name: %s\$#kubernetes.io/metadata.name: %s#" %s   # before applying it`, shipped, keda, files)}}, ""
	}
	admitted, rule := scalerAdmits(np)
	source := "NetworkPolicy " + ScalerNetworkPolicy
	switch {
	case rule < 0:
		return []Result{{Status: Unknown, Detail: source + " in " + ns + " admits port 9090 by no namespace name; what it lets in is not known here"}}, ""
	case slices.Contains(admitted, keda):
		return []Result{{Status: Pass, Detail: fmt.Sprintf("%s admits %s, where KEDA runs", source, keda)}}, ""
	}
	fix := fmt.Sprintf(`kubectl patch networkpolicy %s -n %s --type json -p '[{"op":"replace","path":"/spec/ingress/%d/from/0/namespaceSelector/matchLabels/kubernetes.io~1metadata.name","value":"%s"}]'`,
		ScalerNetworkPolicy, ns, rule, keda)
	if len(admitted) > 1 {
		fix = fmt.Sprintf(`kubectl patch networkpolicy %s -n %s --type json -p '[{"op":"add","path":"/spec/ingress/%d/from/-","value":{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"%s"}}}}]'`,
			ScalerNetworkPolicy, ns, rule, keda)
	}
	return []Result{{Status: Fail,
		Detail: fmt.Sprintf("%s admits port 9090 from %s, but KEDA runs in %s: KEDA's calls are dropped and no ScaledObject scales", source, strings.Join(admitted, ", "), keda),
		Fix:    fix}}, ""
}

// scalerAdmits returns the namespaces the policy admits to port 9090 by name,
// and the index of that ingress rule; -1 when no rule admits 9090 by namespace
// name alone (a pod selector, a label other than the name, or no from).
func scalerAdmits(np *networkingv1.NetworkPolicy) ([]string, int) {
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
		var names []string
		for _, peer := range rule.From {
			if peer.NamespaceSelector == nil || peer.PodSelector != nil {
				return nil, -1
			}
			name := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
			if name == "" {
				return nil, -1
			}
			names = append(names, name)
		}
		if len(names) == 0 {
			return nil, -1
		}
		return names, i
	}
	return nil, -1
}

// checkKEDAMTLS: KEDA's side of mutual TLS. Without the marker the scaler
// serves plaintext (WARN). With it, the ClusterTriggerAuthentication must
// exist. Whether KEDA's copy of the certificates matches the scaler is not
// read here: the copy holds a private key, and the preflight reads no Secret.
// After the install the controller reports a refused handshake as
// scalerUnreachable in its status ConfigMap, which this reads.
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
	status, err := s.env.Kube.CoreV1().ConfigMaps(ns).Get(ctx, ControllerStatusConfigMap, metav1.GetOptions{})
	if err != nil {
		absent, why := readVerdict(err, "ConfigMap "+ControllerStatusConfigMap+" in "+ns)
		if absent {
			why = "the controller has not run yet (no ConfigMap " + ControllerStatusConfigMap + ")"
		}
		return []Result{{Status: Unknown,
			Detail: fmt.Sprintf("ClusterTriggerAuthentication %s is in place; whether KEDA's copy of the certificates matches the scaler is not readable here (%s). After the install, scalerUnreachable in %s and the alert LLMSMExternalScalerUnreachable report a refused handshake", cta, why, ControllerStatusConfigMap)}}, ""
	}
	if why := status.Data[statusScalerUnreachable]; why != "" {
		restart := fmt.Sprintf("kubectl rollout restart deployment -n %s -l %s   # KEDA keeps a connection made before the CA changed", keda, kedaOperatorSelector)
		return []Result{{Status: Fail,
			Detail: "the controller reports KEDA cannot reach the scaler (scalerUnreachable): " + why,
			Fix:    restart + "\n# or KEDA's copy does not match the scaler; copy it again:\n" + steps}}, ""
	}
	return []Result{{Status: Pass, Detail: fmt.Sprintf("ClusterTriggerAuthentication %s in place, and the controller reports no refused handshake (no scalerUnreachable in %s)", cta, ControllerStatusConfigMap)}}, ""
}

// mtlsSteps are the commands that put KEDA's side of mutual TLS in place:
// copy the client Secret's data keys into KEDA's namespace, apply the KEDA
// file, restart the controller (it picks mTLS at start) and KEDA's operator
// (it keeps the connection it opened before). The operator runs them; the
// preflight reads none of these Secrets.
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

// verifyChain checks that chain, leaf first, chains to a CA in caPEM for
// dnsName, at now: expired or not-yet-valid certificates fail.
func verifyChain(caPEM []byte, chain []*x509.Certificate, dnsName string, now time.Time) error {
	if len(chain) == 0 {
		return errors.New("no certificate")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return errors.New("the CA is not a PEM certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: inter, CurrentTime: now, DNSName: dnsName,
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
