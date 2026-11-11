package preflight

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8stesting "k8s.io/client-go/testing"
)

func TestKEDAMTLS(t *testing.T) {
	copyName := "scaling-manager-external-scaler-" + testNS
	cases := []struct {
		name   string
		change func(*fixture)
		want   Status
		detail string
	}{
		{"no marker: plaintext", func(f *fixture) { f.remove("ConfigMap", KEDAMarkerConfigMap) }, Warn, "plaintext"},
		{"marker forbidden", func(f *fixture) { f.kubeReact = append(f.kubeReact, forbidden("get", "configmaps")) }, Unknown, "forbidden"},
		{"no ClusterTriggerAuthentication", func(f *fixture) { f.remove("ClusterTriggerAuthentication", copyName) }, Fail, "does not exist"},
		{"before the install", func(f *fixture) { f.remove("ConfigMap", ControllerStatusConfigMap) }, Unknown, "scalerUnreachable"},
		{"KEDA refused", func(f *fixture) {
			f.remove("ConfigMap", ControllerStatusConfigMap)
			f.add(cm(testNS, ControllerStatusConfigMap, map[string]string{statusScalerUnreachable: "remote error: tls: bad certificate"}))
		}, Fail, "bad certificate"},
		{"in place", func(*fixture) {}, Pass, "no refused handshake"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(f)
			r := only(t, f.run(t), "keda.mtls")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
			if tc.want == Warn || tc.detail == "does not exist" {
				for _, want := range []string{"kubectl create secret generic " + copyName + " -n keda", "scaling-manager-keda-auth.yaml", "rollout restart"} {
					if !strings.Contains(r.Fix, want) {
						t.Errorf("fix lacks %q:\n%s", want, r.Fix)
					}
				}
			}
		})
	}
}

func TestScalerTLS(t *testing.T) {
	other := newCerts(t)
	cases := []struct {
		name   string
		change func(*fixture)
		want   Status
		detail string
	}{
		{"chain verifies", func(*fixture) {}, Pass, "verifies against its CA"},
		{"served by another CA", func(f *fixture) {
			f.dial = func(context.Context, string) ([]*x509.Certificate, error) { return parseChain(other.servingPEM), nil }
		}, Fail, "does not verify"},
		{"expired", func(f *fixture) { f.now = testNow.Add(13 * time.Hour) }, Fail, "expired"},
		{"for another host", func(f *fixture) {
			f.dial = func(context.Context, string) ([]*x509.Certificate, error) { return parseChain(f.certs.clientPEM), nil }
		}, Fail, "does not verify"},
		{"no published CA", func(f *fixture) {
			f.remove("ConfigMap", ControllerStatusConfigMap)
			f.add(cm(testNS, ControllerStatusConfigMap, map[string]string{}))
		}, Unknown, "carries no " + statusScalerCACert},
		{"not installed", func(f *fixture) { f.remove("ConfigMap", ControllerStatusConfigMap) }, Unknown, "has not run yet"},
		{"dropped by the NetworkPolicy", func(f *fixture) {
			f.dial = func(context.Context, string) ([]*x509.Certificate, error) { return nil, errors.New("i/o timeout") }
		}, Unknown, "NetworkPolicy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(f)
			r := only(t, f.run(t), "scaler.tls")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
		})
	}
	var addr string
	f := newFixture(t)
	f.dial = func(_ context.Context, a string) ([]*x509.Certificate, error) {
		addr = a
		return parseChain(f.certs.servingPEM), nil
	}
	f.run(t)
	if addr != "scaling-manager-external-scaler.team-a.svc:9090" {
		t.Errorf("dialled %q", addr)
	}
	f = newFixture(t).remove("ConfigMap", KEDAMarkerConfigMap)
	if rep := f.run(t); !notApplicable(rep, "scaler.tls") {
		t.Errorf("plaintext: scaler.tls should not apply")
	}
}

// DialTLS reads the chain from a server that demands a client certificate and
// refuses the handshake without one, as the scaler does.
func TestDialTLSReadsChainOfMTLSServer(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	chain, err := DialTLS(context.Background(), u.Host)
	if err != nil || len(chain) == 0 || !chain[0].Equal(srv.Certificate()) {
		t.Fatalf("chain %d certs, err %v", len(chain), err)
	}
	srv.Close()
	if _, err := DialTLS(context.Background(), u.Host); err == nil {
		t.Fatal("a closed server returned no error")
	}
}

// H1: the account's token goes only over verified https.
func TestTokenOnlyOverVerifiedHTTPS(t *testing.T) {
	for _, tc := range []struct {
		url      string
		insecure bool
		want     bool
	}{
		{"https://p:9090", false, true},
		{"https://p:9090", true, false},
		{"http://p:9090", false, false},
	} {
		u, _ := url.Parse(tc.url)
		if got := sendsToken(u, tc.insecure); got != tc.want {
			t.Errorf("%s insecure=%v: sends token %v, want %v", tc.url, tc.insecure, got, tc.want)
		}
	}
}

func Test401IsDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p, _ := NewHTTPPrometheus(PrometheusConfig{URL: srv.URL})
	_, err := p.Query(context.Background(), "vector(1)")
	var perr *PromError
	if !errors.As(err, &perr) || perr.Kind != PromDenied {
		t.Fatalf("401: %v, want denied", err)
	}
}

// M3: a check that never returns is UNKNOWN once its time is up, and the run
// goes on.
func TestHangingCheckIsUnknown(t *testing.T) {
	saved := checks
	t.Cleanup(func() { checks = saved })
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	checks = []check{
		{"hangs", func(context.Context, *state) ([]Result, string) { <-block; return nil, "" }},
		{"after", func(context.Context, *state) ([]Result, string) { return []Result{{Status: Pass}}, "" }},
	}
	env := newFixture(t).env(t)
	env.CheckTimeout = 50 * time.Millisecond
	start := time.Now()
	rep := Run(context.Background(), env)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the run took %s", took)
	}
	if r := only(t, rep, "hangs"); r.Status != Unknown || !strings.Contains(r.Detail, "did not finish") {
		t.Fatalf("hanging check: %s %q", r.Status, r.Detail)
	}
	if r := only(t, rep, "after"); r.Status != Pass {
		t.Fatalf("the next check: %s", r.Status)
	}
}

func npWithRules(rules ...networkingv1.NetworkPolicyIngressRule) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: ScalerNetworkPolicy},
		Spec:       networkingv1.NetworkPolicySpec{Ingress: rules},
	}
}

func rule9090(namespaces ...string) networkingv1.NetworkPolicyIngressRule {
	p := intstr.FromInt32(9090)
	r := networkingv1.NetworkPolicyIngressRule{Ports: []networkingv1.NetworkPolicyPort{{Port: &p}}}
	for _, ns := range namespaces {
		r.From = append(r.From, networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}}})
	}
	return r
}

func TestNetworkPolicyShapes(t *testing.T) {
	t.Run("rule at index 0", func(t *testing.T) {
		f := newFixture(t).add(npWithRules(rule9090("keda-system")))
		r := only(t, f.run(t), "keda.networkpolicy")
		if r.Status != Fail || !strings.Contains(r.Fix, `"path":"/spec/ingress/0/from/0/`) {
			t.Errorf("got %s fix %q", r.Status, r.Fix)
		}
	})
	t.Run("several peers, KEDA among them", func(t *testing.T) {
		f := newFixture(t).add(npWithRules(rule9090("monitoring", "keda")))
		if r := only(t, f.run(t), "keda.networkpolicy"); r.Status != Pass {
			t.Errorf("got %s %q", r.Status, r.Detail)
		}
	})
	t.Run("several peers, KEDA not among them", func(t *testing.T) {
		f := newFixture(t).add(npWithRules(rule9090("monitoring", "keda-system")))
		r := only(t, f.run(t), "keda.networkpolicy")
		if r.Status != Fail || !strings.Contains(r.Fix, `"op":"add","path":"/spec/ingress/0/from/-"`) || !strings.Contains(r.Fix, `"kubernetes.io/metadata.name":"keda"`) {
			t.Errorf("got %s fix %q", r.Status, r.Fix)
		}
	})
	t.Run("not applied yet: change the file, not the cluster", func(t *testing.T) {
		f := newFixture(t).remove("APIService", externalMetricsAPIService)
		f.dyn = append(f.dyn, apiService("keda-system", "keda-operator-metrics-apiserver"))
		r := only(t, f.run(t), "keda.networkpolicy")
		if r.Status != Fail || strings.Contains(r.Fix, "kubectl patch") || !strings.Contains(r.Fix, `kubernetes.io/metadata.name: keda-system#`) {
			t.Errorf("got %s fix %q", r.Status, r.Fix)
		}
	})
}

// KEDA's version, read by name when Deployments cannot be listed (a
// namespaced preflight), and the installer's notes on 2.19 and 2.20.
func TestKEDAVersionNotes(t *testing.T) {
	cases := []struct {
		image string
		has   []string
		not   []string
	}{
		{"ghcr.io/kedacore/keda:2.20.1", []string{"untested", "CVE-2026-77524"}, []string{"unwired"}},
		{"ghcr.io/kedacore/keda:2.19.0", []string{"CVE-2026-77524", "unwired"}, []string{"untested"}},
	}
	for _, tc := range cases {
		f := newFixture(t).remove("Deployment", "keda-operator")
		f.add(deployment("keda", "keda-operator", map[string]string{"app.kubernetes.io/name": "keda-operator"}, tc.image))
		r := only(t, f.run(t), "keda.version")
		for _, w := range tc.has {
			if r.Status != Warn || !strings.Contains(r.Detail, w) {
				t.Errorf("%s: %s %q lacks %q", tc.image, r.Status, r.Detail, w)
			}
		}
		for _, w := range tc.not {
			if strings.Contains(r.Detail, w) {
				t.Errorf("%s: %q says %q", tc.image, r.Detail, w)
			}
		}
	}
	f := newFixture(t)
	f.kubeReact = append(f.kubeReact, forbidden("list", "deployments"))
	if r := only(t, f.run(t), "keda.version"); r.Status != Pass || !strings.Contains(r.Detail, "keda/keda-operator") {
		t.Errorf("list forbidden, read by name: %s %q", r.Status, r.Detail)
	}
	f = newFixture(t)
	f.kubeReact = append(f.kubeReact, forbidden("list", "deployments"), forbidden("get", "deployments"))
	if r := only(t, f.run(t), "keda.version"); r.Status != Unknown {
		t.Errorf("both forbidden: %s %q", r.Status, r.Detail)
	}
}

func TestOverlapByName(t *testing.T) {
	listForbidden := func(f *fixture) { f.kubeReact = append(f.kubeReact, forbidden("list", "deployments")) }
	f := newFixture(t)
	listForbidden(f)
	f.add(controllerDeployment(ClusterNamespace))
	if r := only(t, f.run(t), "overlap"); r.Status != Fail || !strings.Contains(r.Detail, "cluster-wide") {
		t.Errorf("cluster-wide by name: %s %q", r.Status, r.Detail)
	}
	f = newFixture(t)
	listForbidden(f)
	if r := only(t, f.run(t), "overlap"); r.Status != Unknown || !strings.Contains(r.Detail, "not visible") {
		t.Errorf("none by name: %s %q", r.Status, r.Detail)
	}
	f = newFixture(t)
	f.add(controllerDeployment("ops", "--watch-namespace", testNS))
	if r := only(t, f.run(t), "overlap"); r.Status != Fail || !strings.Contains(r.Detail, "already manages "+testNS) {
		t.Errorf("--watch-namespace <ns>: %s %q", r.Status, r.Detail)
	}
}

func TestSettingsEdges(t *testing.T) {
	for _, tc := range []struct{ key, value, detail string }{
		{keyLogVerbosity, "-1", "0 or more"},
		{keyQuotas, "H200=8,H200=4", "twice"},
	} {
		f := newFixture(t)
		f.settings[tc.key] = tc.value
		if r := only(t, f.run(t), "settings"); r.Status != Fail || !strings.Contains(r.Detail, tc.detail) {
			t.Errorf("%s=%s: %s %q", tc.key, tc.value, r.Status, r.Detail)
		}
	}
	// ENABLE_SCALE_TO_ZERO, the older name of the legacy key, is read too.
	f := newFixture(t)
	f.settings = nil
	f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": "ENABLE_SCALE_TO_ZERO: \"true\"\n"}))
	s := &state{env: f.env(t)}
	if v := s.loadSettings(context.Background()).value(keyScaleToZero); v != "true" {
		t.Errorf("scaleToZero from ENABLE_SCALE_TO_ZERO: %q", v)
	}
}

// A review the API server could not evaluate is no answer.
func TestSARWithEvaluationErrorIsUnknown(t *testing.T) {
	f := newFixture(t)
	f.kubeReact = append(f.kubeReact, reaction{"create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sar := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview).DeepCopy()
		sar.Status.EvaluationError = "webhook authorizer unavailable"
		return true, sar, nil
	}})
	if r := only(t, f.run(t), "rbac.controller"); r.Status != Unknown || !strings.Contains(r.Detail, "could not be evaluated") {
		t.Errorf("got %s %q", r.Status, r.Detail)
	}
}

// A product name the family rule cannot reach is matched by its short name.
func TestAcceleratorShortName(t *testing.T) {
	f := newFixture(t)
	f.settings[keyLimiter] = limiterQuota
	f.settings[keyQuotas] = "Gaudi-2=8"
	f.add(node("hpu-1", map[string]string{"habana.ai/product.name": "Intel-Gaudi-2-96GB"}))
	if r := only(t, f.run(t), "accelerators"); r.Status != Pass {
		t.Errorf("got %s %q", r.Status, r.Detail)
	}
}

// A namespace reader whose rule names no namespace reads them all.
func TestUnnarrowedReaderCoversAll(t *testing.T) {
	f := newFixture(t).remove("ClusterRole", "scaling-manager-policy-namespace-reader-role-"+testNS)
	f.add(&rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "scaling-manager-policy-namespace-reader-role-" + testNS},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get"}}},
	})
	f.settings[keyWatchNamespaces] = testNS + ",team-b"
	if r := only(t, f.run(t), "rbac.namespaces"); r.Status != Pass {
		t.Errorf("got %s %q", r.Status, r.Detail)
	}
}

func TestKueueControllerSAUnread(t *testing.T) {
	f := newFixture(t)
	f.settings[keyKueue] = valueTrue
	f.settings[keyLimiter] = limiterQuota
	for _, n := range []string{"clusterqueues", "localqueues", "resourceflavors", "workloads"} {
		f.dyn = append(f.dyn, crd(n+".kueue.x-k8s.io"))
	}
	f.kubeReact = append(f.kubeReact, forbidden("get", "serviceaccounts"))
	if r := only(t, f.run(t), "kueue"); r.Status != Unknown {
		t.Errorf("got %s %q", r.Status, r.Detail)
	}
}

// Settings that cannot be read leave the checks they gate UNKNOWN, not
// "not applicable".
func TestUnreadSettingsGateNothingSilently(t *testing.T) {
	f := newFixture(t)
	f.kubeReact = append(f.kubeReact, forbidden("get", "configmaps"))
	rep := f.run(t)
	for _, name := range []string{"kueue", "activation", "accelerators"} {
		if r := only(t, rep, name); r.Status != Unknown {
			t.Errorf("%s: %s %q", name, r.Status, r.Detail)
		}
	}
}

// A cluster-wide OpenShift run that may not list pods cannot name the model
// namespaces.
func TestOpenShiftClusterPodsRefused(t *testing.T) {
	f := newFixture(t)
	f.openshift = true
	f.scope = ScopeCluster
	f.kubeReact = append(f.kubeReact, forbidden("list", "pods"))
	if r := only(t, f.run(t), "openshift.user-monitoring"); r.Status != Unknown {
		t.Errorf("got %s %q", r.Status, r.Detail)
	}
}

// The preflight creates its result ConfigMap and nothing else.
func TestCreatesOnlyTheResult(t *testing.T) {
	f := newFixture(t)
	var created []string
	f.kubeReact = append(f.kubeReact, reaction{"create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Resource != "subjectaccessreviews" {
			if o, ok := a.(k8stesting.CreateAction).GetObject().(metav1.Object); ok {
				created = append(created, a.GetResource().Resource+"/"+o.GetName())
			}
		}
		return false, nil, nil
	}})
	var out, errb strings.Builder
	Execute(context.Background(), f.env(t), true, &out, &errb)
	if len(created) != 1 || created[0] != "configmaps/"+ResultConfigMap {
		t.Fatalf("created %v", created)
	}
}
