package preflight

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrometheusReachable(t *testing.T) {
	cases := []struct {
		name   string
		change func(*fixture)
		want   Status
		detail string
		fix    string
	}{
		{"unset", func(f *fixture) { f.settings[keyPrometheusURL] = "" }, Fail, "not set", "prometheusURL"},
		{"unset on OpenShift", func(f *fixture) { f.settings[keyPrometheusURL] = ""; f.openshift = true }, Fail, "not set", thanosURL},
		{"plain HTTP", func(f *fixture) { f.settings[keyPrometheusURL] = "http://prometheus.monitoring.svc:9090" }, Fail, "plain HTTP", "https://prometheus.monitoring.svc:9090"},
		{"refused", func(f *fixture) {
			f.prom.reachErr = &PromError{Kind: PromUnreachable, Err: errors.New("dial tcp 10.0.0.1:9090: connect: connection refused")}
		}, Fail, "connection refused", "NetworkPolicy"},
		{"unknown authority", func(f *fixture) {
			f.prom.reachErr = &PromError{Kind: PromTLS, Err: errors.New("x509: certificate signed by unknown authority")}
		}, Fail, "unknown authority", "kubectl create secret generic " + PrometheusCASecret + " -n " + testNS},
		{"denied for the preflight account", func(f *fixture) {
			f.prom.reachErr = &PromError{Kind: PromDenied, Err: errors.New("HTTP 403: forbidden")}
		}, Pass, "network and TLS work", ""},
		{"CA Secret forbidden", func(f *fixture) { f.kubeReact = append(f.kubeReact, forbidden("get", "secrets")) }, Unknown, "forbidden", ""},
		{"CA does not parse", func(f *fixture) { f.promNewErr = errors.New("Secret holds no PEM certificate under ca.crt") }, Fail, "no PEM", PrometheusCASecret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(f)
			r := only(t, f.run(t), "prometheus.reachable")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) || !strings.Contains(r.Fix, tc.fix) {
				t.Errorf("got %s %q fix %q, want %s containing %q, fix %q", r.Status, r.Detail, r.Fix, tc.want, tc.detail, tc.fix)
			}
		})
	}
}

// The CA in PrometheusCASecret reaches the client.
func TestPrometheusCAIsPassed(t *testing.T) {
	f := newFixture(t)
	c := newCerts(t)
	f.add(secret(testNS, PrometheusCASecret, map[string][]byte{"ca.crt": c.caPEM}))
	f.settings[keyPrometheusInsecure] = "true"
	f.run(t)
	if f.captured == nil || string(f.captured.CA) != string(c.caPEM) || !f.captured.InsecureSkipVerify {
		t.Fatalf("client built from %+v", f.captured)
	}
}

// A metrics query that cannot be made is UNKNOWN in every check that needs it.
func TestUnreachablePrometheusLeavesMetricsUnknown(t *testing.T) {
	f := newFixture(t)
	f.prom.reachErr = &PromError{Kind: PromDenied, Err: errors.New("HTTP 401")}
	rep := f.run(t)
	if r := only(t, rep, "metrics.model"); r.Status != Unknown || !strings.Contains(r.Detail, "refused the preflight's account") {
		t.Errorf("metrics.model: %s %q", r.Status, r.Detail)
	}
	if r := only(t, rep, "podmonitor.port"); r.Status != Pass || !strings.Contains(r.Detail, "targets not checked") {
		t.Errorf("podmonitor.port: %s %q", r.Status, r.Detail)
	}
}

func TestModelMetrics(t *testing.T) {
	cases := []struct {
		name   string
		change func(*fixture)
		want   Status
		detail string
	}{
		{"no series", func(f *fixture) { f.prom.series = nil }, Fail, "no vllm: series"},
		{"some series", func(f *fixture) { f.prom.series[testNS] = requiredModelMetrics[:1] }, Warn, "missing vllm:num_requests_waiting"},
		{"query denied", func(f *fixture) { f.prom.countErr = &PromError{Kind: PromDenied, Err: errors.New("HTTP 403")} }, Unknown, "denied"},
		{"no model server yet", func(f *fixture) { f.prom.series = nil; f.remove("Pod", "llama-decode-0") }, Warn, "no model-server pod"},
		{"pods forbidden, series present", func(f *fixture) { f.kubeReact = append(f.kubeReact, forbidden("list", "pods")) }, Pass, "not listed"},
		{"pods forbidden, no series", func(f *fixture) {
			f.prom.series = nil
			f.kubeReact = append(f.kubeReact, forbidden("list", "pods"))
		}, Fail, "no vllm: series"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(f)
			r := only(t, f.run(t), "metrics.model")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
		})
	}
	// A cluster-wide install reads the namespaces that run model servers.
	f := newFixture(t)
	f.scope = ScopeCluster
	f.add(modelPod("team-b", "qwen-decode-0", "modelserver"))
	r := only(t, f.run(t), "metrics.model")
	if r.Status != Fail || !strings.Contains(r.Detail, "team-b") || strings.Contains(r.Detail, testNS) {
		t.Errorf("cluster scope: %s %q, want FAIL naming team-b only", r.Status, r.Detail)
	}
}

func TestPodMonitorPort(t *testing.T) {
	pm := "scaling-manager-modelserver-metrics"
	ours := map[string]string{"app.kubernetes.io/component": "modelserver-metrics", "release": "kps"}
	cases := []struct {
		name   string
		change func(*fixture)
		want   Status
		detail string
	}{
		{"port named otherwise", func(f *fixture) {
			f.remove("Pod", "llama-decode-0")
			f.add(modelPod(testNS, "llama-decode-0", "http"))
		}, Fail, "no model-server pod declares it: llama-decode-0 (http)"},
		{"sidecar declares it", func(f *fixture) {
			f.remove("Pod", "llama-decode-0")
			p := modelPod(testNS, "llama-decode-0", "http")
			p.Spec.InitContainers = append(p.Spec.InitContainers, p.Spec.Containers[0])
			p.Spec.InitContainers[0].Ports = append(p.Spec.InitContainers[0].Ports[:0:0], p.Spec.Containers[0].Ports[0])
			p.Spec.InitContainers[0].Ports[0].Name = "modelserver"
			f.add(p)
		}, Pass, "selects the model servers"},
		{"no PodMonitor", func(f *fixture) { f.remove("PodMonitor", pm) }, Fail, "no PodMonitor selects"},
		{"ours selects nothing", func(f *fixture) {
			f.remove("PodMonitor", pm)
			o := podMonitor(testNS, pm, "modelserver", ours)
			o.Object["spec"].(map[string]any)["selector"] = map[string]any{"matchLabels": map[string]any{"app": "other"}}
			f.dyn = append(f.dyn, o)
		}, Fail, "selects none of the 1 model-server pods"},
		{"targets down", func(f *fixture) { f.prom.upValue = 0 }, Fail, "down: 10.0.0.7:8000"},
		{"list forbidden", func(f *fixture) { f.dynReact = append(f.dynReact, forbidden("list", "podmonitors")) }, Unknown, "forbidden"},
		{"API not served", func(f *fixture) { f.dynReact = append(f.dynReact, notServed("list", "podmonitors")) }, Warn, "not served"},
		{"pods forbidden", func(f *fixture) { f.kubeReact = append(f.kubeReact, forbidden("list", "pods")) }, Unknown, "forbidden"},
		{"no model server yet", func(f *fixture) { f.remove("Pod", "llama-decode-0") }, Warn, "not checked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(f)
			r := only(t, f.run(t), "podmonitor.port")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
		})
	}
}

func TestPrometheusSelects(t *testing.T) {
	cases := []struct {
		name   string
		spec   map[string]any
		want   Status
		detail string
	}{
		{"selector misses", map[string]any{
			"podMonitorSelector":          map[string]any{"matchLabels": map[string]any{"release": "other"}},
			"podMonitorNamespaceSelector": map[string]any{},
		}, Fail, "release=other"},
		{"no PodMonitor selector", map[string]any{"podMonitorNamespaceSelector": map[string]any{}}, Fail, "podMonitorSelector unset"},
		{"own namespace only", map[string]any{"podMonitorSelector": map[string]any{}}, Fail, "own namespace only"},
		{"namespace not labelled", map[string]any{
			"podMonitorSelector":          map[string]any{},
			"podMonitorNamespaceSelector": map[string]any{"matchLabels": map[string]any{"monitoring": "on"}},
		}, Fail, "monitoring=on"},
		{"selects all", map[string]any{"podMonitorSelector": map[string]any{}, "podMonitorNamespaceSelector": map[string]any{}}, Pass, "monitoring/kps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t).remove("Prometheus", "kps")
			f.dyn = append(f.dyn, prometheusResource("monitoring", "kps", tc.spec))
			r := only(t, f.run(t), "prometheus.selects")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
		})
	}
	f := newFixture(t)
	f.dynReact = append(f.dynReact, forbidden("list", "prometheuses"))
	if r := only(t, f.run(t), "prometheus.selects"); r.Status != Unknown {
		t.Errorf("forbidden: %s %q", r.Status, r.Detail)
	}
	f = newFixture(t).remove("PodMonitor", "scaling-manager-modelserver-metrics")
	if rep := f.run(t); !notApplicable(rep, "prometheus.selects") {
		t.Errorf("with no PodMonitor, prometheus.selects should not apply: %+v", results(rep, "prometheus.selects"))
	}
}

func TestOpenShiftUserMonitoring(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   Status
	}{
		{"opted out", map[string]string{userMonitoringLabel: "false"}, Fail},
		{"unlabelled", nil, Warn},
		{"labelled", map[string]string{userMonitoringLabel: "true"}, Pass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t).remove("Namespace", testNS)
			f.openshift = true
			f.add(namespace(testNS, tc.labels))
			r := only(t, f.run(t), "openshift.user-monitoring")
			if r.Status != tc.want {
				t.Errorf("got %s %q, want %s", r.Status, r.Detail, tc.want)
			}
			if r.Status != Pass && !strings.Contains(r.Fix, "kubectl label namespace "+testNS+" "+userMonitoringLabel+"=true") {
				t.Errorf("fix %q", r.Fix)
			}
		})
	}
	f := newFixture(t)
	f.openshift = true
	f.kubeReact = append(f.kubeReact, forbidden("get", "namespaces"))
	if r := only(t, f.run(t), "openshift.user-monitoring"); r.Status != Unknown {
		t.Errorf("forbidden: %s %q", r.Status, r.Detail)
	}
}

// NewHTTPPrometheus against a real TLS server: the CA from the Secret
// verifies it; without it the error is a TLS one; a 403 is denied.
func TestHTTPPrometheus(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/api/v1/query" || r.URL.Query().Get("query") != "vector(1)" {
			http.Error(w, "bad path "+r.URL.String(), http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"a"},"value":[1,"1"]}]}}`)
	}))
	defer srv.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	ctx := context.Background()

	p, err := NewHTTPPrometheus(PrometheusConfig{URL: srv.URL + "/prefix/", CA: caPEM})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Query(ctx, "vector(1)")
	if err != nil || len(got) != 1 || got[0].Value != 1 || got[0].Labels["namespace"] != "a" {
		t.Fatalf("with the CA: %v %v", got, err)
	}

	p, _ = NewHTTPPrometheus(PrometheusConfig{URL: srv.URL + "/prefix"})
	_, err = p.Query(ctx, "vector(1)")
	var perr *PromError
	if !errors.As(err, &perr) || perr.Kind != PromTLS {
		t.Fatalf("without the CA: %v, want a TLS error", err)
	}

	p, _ = NewHTTPPrometheus(PrometheusConfig{URL: srv.URL + "/prefix", InsecureSkipVerify: true})
	status = http.StatusForbidden
	_, err = p.Query(ctx, "vector(1)")
	if !errors.As(err, &perr) || perr.Kind != PromDenied {
		t.Fatalf("403: %v, want denied", err)
	}

	if _, err := NewHTTPPrometheus(PrometheusConfig{URL: srv.URL, CA: []byte("not pem")}); err == nil {
		t.Fatal("a CA that is not PEM was accepted")
	}

	srv.Close()
	p, _ = NewHTTPPrometheus(PrometheusConfig{URL: srv.URL, InsecureSkipVerify: true})
	_, err = p.Query(ctx, "vector(1)")
	if !errors.As(err, &perr) || perr.Kind != PromUnreachable {
		t.Fatalf("closed server: %v, want unreachable", err)
	}
	_ = tls.VersionTLS12
	_ = x509.NewCertPool
}
