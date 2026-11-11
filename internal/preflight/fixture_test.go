package preflight

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testNS = "team-a"

var testNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// fixture is an install where every applicable check passes. Tests change
// one thing and look at the one check it should move.
type fixture struct {
	ns        string
	scope     Scope
	openshift bool
	settings  map[string]string // nil: no settings ConfigMap
	kube      []runtime.Object
	dyn       []runtime.Object
	// sar answers SubjectAccessReviews; nil allows everything.
	sar        func(authorizationv1.ResourceAttributes) bool
	kubeReact  []reaction
	dynReact   []reaction
	prom       *fakeProm
	promNewErr error
	captured   *PrometheusConfig
	promCA     []byte
	// dial answers the scaler handshake; certs is what the fixture serves.
	dial  func(ctx context.Context, addr string) ([]*x509.Certificate, error)
	certs certs
	// now overrides the run's clock when set.
	now time.Time
}

type reaction struct {
	verb, resource string
	fn             k8stesting.ReactionFunc
}

// forbidden is a reaction that refuses verb on resource.
func forbidden(verb, resource string) reaction {
	return reaction{verb, resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("RBAC: access denied"))
	}}
}

// notServed is a reaction that answers NotFound, as an API server does for a
// resource whose CRD is not installed.
func notServed(verb, resource string) reaction {
	return reaction{verb, resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: resource}, "")
	}}
}

// fakeProm answers the queries the checks make.
type fakeProm struct {
	reachErr error
	// series: namespace -> metric names present.
	series   map[string][]string
	countErr error
	upValue  float64
	upNone   bool
	queries  []string
}

func (p *fakeProm) Query(_ context.Context, q string) ([]Sample, error) {
	p.queries = append(p.queries, q)
	switch {
	case q == "vector(1)":
		if p.reachErr != nil {
			return nil, p.reachErr
		}
		return []Sample{{Value: 1}}, nil
	case strings.HasPrefix(q, "count by"):
		if p.countErr != nil {
			return nil, p.countErr
		}
		var out []Sample
		for ns, names := range p.series {
			for _, n := range names {
				out = append(out, Sample{Labels: map[string]string{"namespace": ns, "__name__": n}, Value: 1})
			}
		}
		return out, nil
	case strings.HasPrefix(q, "up{"):
		if p.upNone {
			return nil, nil
		}
		return []Sample{{Labels: map[string]string{"instance": "10.0.0.7:8000"}, Value: p.upValue}}, nil
	}
	return nil, fmt.Errorf("unexpected query %q", q)
}

// certs is a CA with a serving leaf and a client leaf, as the controller
// creates them.
type certs struct {
	caPEM, servingPEM, clientPEM []byte
}

func newCerts(t *testing.T) certs {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: testNow.Add(-time.Hour), NotAfter: testNow.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leaf := func(serial int64, cn string) []byte {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn},
			NotBefore: testNow.Add(-time.Hour), NotAfter: testNow.Add(12 * time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	return certs{
		caPEM:      pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		servingPEM: leaf(2, "scaling-manager-external-scaler."+testNS+".svc"),
		clientPEM:  leaf(3, "scaling-manager-keda-client"),
	}
}

func cm(ns, name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: data}
}

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func modelPod(ns, name, port string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"llm-d.ai/role": "decode"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "vllm", Ports: []corev1.ContainerPort{{Name: port, ContainerPort: 8000}},
		}}},
	}
}

func deployment(ns, name string, labels map[string]string, image string, args ...string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "manager", Image: image, Args: args}},
		}}},
	}
}

func controllerDeployment(ns string, args ...string) *appsv1.Deployment {
	return deployment(ns, ControllerDeployment,
		map[string]string{"app.kubernetes.io/name": "scaling-manager", "control-plane": "controller-manager"},
		"ghcr.io/llm-d-incubation/llm-d-scaling-manager:v0.10.2", args...)
}

func namespaceReader(ns string, names ...string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "scaling-manager-policy-namespace-reader-role-" + ns},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"namespaces"}, ResourceNames: names, Verbs: []string{"get"}}},
	}
}

func node(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func object(apiVersion, kind, ns, name string, labels map[string]string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind}}
	u.SetName(name)
	if ns != "" {
		u.SetNamespace(ns)
	}
	if labels != nil {
		u.SetLabels(labels)
	}
	if spec != nil {
		u.Object["spec"] = spec
	}
	return u
}

func crd(name string) *unstructured.Unstructured {
	return object("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", name, nil,
		map[string]any{"versions": []any{map[string]any{"name": "v1alpha1", "served": true}}})
}

func apiService(ns, svc string) *unstructured.Unstructured {
	return object("apiregistration.k8s.io/v1", "APIService", "", externalMetricsAPIService, nil,
		map[string]any{"service": map[string]any{"namespace": ns, "name": svc}})
}

func podMonitor(ns, name, port string, labels map[string]string) *unstructured.Unstructured {
	return object("monitoring.coreos.com/v1", "PodMonitor", ns, name, labels, map[string]any{
		"selector": map[string]any{"matchExpressions": []any{
			map[string]any{"key": "llm-d.ai/role", "operator": "In", "values": []any{"decode", "prefill"}},
		}},
		"podMetricsEndpoints": []any{map[string]any{"port": port, "path": "/metrics"}},
	})
}

func prometheusResource(ns, name string, spec map[string]any) *unstructured.Unstructured {
	return object("monitoring.coreos.com/v1", "Prometheus", ns, name, nil, spec)
}

// newFixture is a namespaced install in team-a, before the controller's own
// objects exist only where a check needs them: KEDA 2.21 in keda with mutual
// TLS in place, a reachable Prometheus that holds the vLLM series and selects
// the model-server PodMonitor, the controller's ServiceAccount and its
// namespace reader.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	c := newCerts(t)
	ns := testNS
	ourPM := map[string]string{"app.kubernetes.io/component": "modelserver-metrics", "release": "kps"}
	return &fixture{
		ns:    ns,
		scope: ScopeNamespace,
		settings: map[string]string{
			keyPrometheusURL: "https://prometheus.monitoring.svc:9090",
			keyPassive:       "true", keyUtilizationShare: "off", keyLimiter: "", keyQuotas: "",
			keyQuotaScope: "namespace", keyKueue: "false", keyKueueBorrow: "true", keyKueueNamespace: "kueue-system",
			keyScaleToZero: "false", keyWatchNamespaces: "", keyLogVerbosity: "2", keyPrometheusInsecure: "false",
		},
		kube: []runtime.Object{
			namespace(ns, nil),
			namespace("monitoring", nil),
			namespace("keda", nil),
			modelPod(ns, "llama-decode-0", "modelserver"),
			deployment("keda", "keda-operator", map[string]string{"app.kubernetes.io/name": "keda-operator"}, "ghcr.io/kedacore/keda:2.21.0"),
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: ControllerServiceAccount}},
			namespaceReader(ns, ns, "scaling-manager-policy"),
			cm(ns, KEDAMarkerConfigMap, map[string]string{"clusterTriggerAuthentication": "scaling-manager-external-scaler-" + ns, "secretNamespace": "keda"}),
			cm(ns, ControllerStatusConfigMap, map[string]string{statusScalerCACert: string(c.caPEM)}),
		},
		dyn: []runtime.Object{
			crd("scaledobjects.keda.sh"),
			apiService("keda", "keda-operator-metrics-apiserver"),
			object("keda.sh/v1alpha1", "ClusterTriggerAuthentication", "", "scaling-manager-external-scaler-"+ns, nil, map[string]any{}),
			podMonitor(ns, "scaling-manager-modelserver-metrics", "modelserver", ourPM),
			prometheusResource("monitoring", "kps", map[string]any{
				"podMonitorSelector":          map[string]any{"matchLabels": map[string]any{"release": "kps"}},
				"podMonitorNamespaceSelector": map[string]any{},
			}),
		},
		prom:  &fakeProm{series: map[string][]string{ns: requiredModelMetrics}, upValue: 1},
		certs: c,
	}
}

// dialer is the fixture's scaler: it serves the serving certificate.
func (f *fixture) dialer() func(context.Context, string) ([]*x509.Certificate, error) {
	if f.dial != nil {
		return f.dial
	}
	return func(context.Context, string) ([]*x509.Certificate, error) {
		return parseChain(f.certs.servingPEM), nil
	}
}

func parseChain(pemBytes []byte) []*x509.Certificate {
	var out []*x509.Certificate
	for rest := pemBytes; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return out
		}
		if c, err := x509.ParseCertificate(b.Bytes); err == nil {
			out = append(out, c)
		}
	}
}

// add appends kube objects.
func (f *fixture) add(objs ...runtime.Object) *fixture {
	f.kube = append(f.kube, objs...)
	return f
}

// remove drops the kube or dynamic object of that kind and name.
func (f *fixture) remove(kind, name string) *fixture {
	keep := func(list []runtime.Object) []runtime.Object {
		var out []runtime.Object
		for _, o := range list {
			m, err := metaOf(o)
			if err == nil && m.kind == kind && m.name == name {
				continue
			}
			out = append(out, o)
		}
		return out
	}
	f.kube, f.dyn = keep(f.kube), keep(f.dyn)
	return f
}

type objMeta struct{ kind, name string }

func metaOf(o runtime.Object) (objMeta, error) {
	if u, ok := o.(*unstructured.Unstructured); ok {
		return objMeta{u.GetKind(), u.GetName()}, nil
	}
	acc, err := metaAccessor(o)
	if err != nil {
		return objMeta{}, err
	}
	kind := fmt.Sprintf("%T", o)
	kind = kind[strings.LastIndex(kind, ".")+1:]
	return objMeta{kind, acc}, nil
}

func metaAccessor(o runtime.Object) (string, error) {
	m, ok := o.(metav1.Object)
	if !ok {
		return "", errors.New("no metadata")
	}
	return m.GetName(), nil
}

func (f *fixture) env(t *testing.T) *Env {
	t.Helper()
	objs := append([]runtime.Object(nil), f.kube...)
	if f.settings != nil {
		objs = append(objs, cm(f.ns, SettingsConfigMap, f.settings))
	}
	kube := k8sfake.NewClientset(objs...)
	groups := []*metav1.APIResourceList{{GroupVersion: "v1"}, {GroupVersion: "apps/v1"}}
	if f.openshift {
		groups = append(groups, &metav1.APIResourceList{GroupVersion: "route.openshift.io/v1"})
	}
	kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = groups
	allow := f.sar
	if allow == nil {
		allow = func(authorizationv1.ResourceAttributes) bool { return true }
	}
	kube.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sar := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview).DeepCopy()
		if sar.Spec.ResourceAttributes != nil {
			sar.Status.Allowed = allow(*sar.Spec.ResourceAttributes)
		}
		return true, sar, nil
	})
	for _, r := range f.kubeReact {
		kube.PrependReactor(r.verb, r.resource, r.fn)
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crdGVR:        "CustomResourceDefinitionList",
		apiServiceGVR: "APIServiceList",
		ctaGVR:        "ClusterTriggerAuthenticationList",
		podMonitorGVR: "PodMonitorList",
		prometheusGVR: "PrometheusList",
	}, f.dyn...)
	for _, r := range f.dynReact {
		dyn.PrependReactor(r.verb, r.resource, r.fn)
	}
	return &Env{
		Kube: kube, Dynamic: dyn, Namespace: f.ns, Scope: f.scope,
		NewPrometheus: func(cfg PrometheusConfig) (Prometheus, error) {
			c := cfg
			f.captured = &c
			if f.promNewErr != nil {
				return nil, f.promNewErr
			}
			return f.prom, nil
		},
		PrometheusCA: f.promCA,
		DialTLS:      f.dialer(),
		CheckTimeout: 5 * time.Second,
		Now: func() time.Time {
			if !f.now.IsZero() {
				return f.now
			}
			return testNow
		},
	}
}

// run runs every check against the fixture.
func (f *fixture) run(t *testing.T) Report {
	t.Helper()
	return Run(context.Background(), f.env(t))
}

// results returns the results of one check.
func results(rep Report, name string) []Result {
	var out []Result
	for _, r := range rep.Results {
		if r.Check == name {
			out = append(out, r)
		}
	}
	return out
}

// only returns the one result of a check, failing the test otherwise.
func only(t *testing.T, rep Report, name string) Result {
	t.Helper()
	rs := results(rep, name)
	if len(rs) != 1 {
		var b strings.Builder
		Print(&b, rep)
		t.Fatalf("check %s: %d results, want 1\n%s", name, len(rs), b.String())
	}
	return rs[0]
}

// notApplicable reports whether a check was skipped as not applicable.
func notApplicable(rep Report, name string) bool {
	for _, n := range rep.NotApplicable {
		if strings.HasPrefix(n, name+" (") {
			return true
		}
	}
	return false
}
