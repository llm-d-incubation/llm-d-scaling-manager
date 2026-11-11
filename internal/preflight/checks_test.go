package preflight

import (
	"context"
	"slices"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// wantChecks is every check `check` mode runs. Written out, not read from
// checks: a check dropped from the list must fail a test.
var wantChecks = []string{
	"settings", "keda.crd", "keda.version", "keda.networkpolicy", "keda.mtls", "scaler.tls",
	"prometheus.reachable", "metrics.model", "podmonitor.port", "prometheus.selects",
	"openshift.user-monitoring", "rbac.controller", "rbac.namespaces", "kueue",
	"overlap", "accelerators", "activation",
}

func TestCheckList(t *testing.T) {
	if got := CheckNames(); !slices.Equal(got, wantChecks) {
		t.Fatalf("checks = %v\nwant     %v", got, wantChecks)
	}
}

// A check that returns no verdict and no reason is UNKNOWN, never dropped.
func TestSilentCheckIsUnknown(t *testing.T) {
	saved := checks
	t.Cleanup(func() { checks = saved })
	checks = []check{{"silent", func(context.Context, *state) ([]Result, string) { return nil, "" }}}
	rep := newFixture(t).run(t)
	if r := only(t, rep, "silent"); r.Status != Unknown {
		t.Fatalf("got %s %q", r.Status, r.Detail)
	}
	if len(rep.NotApplicable) != 0 {
		t.Fatalf("not applicable: %v", rep.NotApplicable)
	}
}

// The fixture passes every applicable check, and every check either reports
// or says why it does not apply.
func TestHealthyInstallPasses(t *testing.T) {
	rep := newFixture(t).run(t)
	var b strings.Builder
	Print(&b, rep)
	for _, r := range rep.Results {
		if r.Status != Pass {
			t.Errorf("%s: %s %s", r.Check, r.Status, r.Detail)
		}
	}
	for _, name := range wantChecks {
		if len(results(rep, name)) == 0 && !notApplicable(rep, name) {
			t.Errorf("check %s neither reported nor said why it does not apply", name)
		}
	}
	for _, name := range []string{"openshift.user-monitoring", "kueue", "accelerators", "activation"} {
		if !notApplicable(rep, name) {
			t.Errorf("%s should not apply to the fixture", name)
		}
	}
	if rep.ExitCode() != 0 {
		t.Errorf("exit code %d, want 0", rep.ExitCode())
	}
	if t.Failed() {
		t.Log(b.String())
	}
}

func TestSettings(t *testing.T) {
	cases := []struct {
		name   string
		change func(*fixture)
		want   Status
		detail string
	}{
		{"absent, no legacy", func(f *fixture) { f.settings = nil }, Fail, "does not exist"},
		{"bad bool", func(f *fixture) { f.settings[keyPassive] = "yes" }, Fail, "passive"},
		{"unknown mode", func(f *fixture) { f.settings[keyUtilizationShare] = "on" }, Fail, "off, shadow or active"},
		{"sharing without a limiter", func(f *fixture) { f.settings[keyUtilizationShare] = "shadow" }, Fail, "needs a limiter"},
		{"bad quota", func(f *fixture) { f.settings[keyLimiter] = "quota"; f.settings[keyQuotas] = "H200=eight" }, Fail, "NAME=N"},
		{"quota limiter, no budget", func(f *fixture) { f.settings[keyLimiter] = "quota" }, Fail, "budget"},
		{"unknown limiter", func(f *fixture) { f.settings[keyLimiter] = "gpu" }, Fail, "gpu-inventory and quota"},
		{"bad watch namespace", func(f *fixture) { f.settings[keyWatchNamespaces] = "Team_B" }, Fail, "not a namespace name"},
		{"legacy only", func(f *fixture) {
			f.settings = nil
			f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": "PROMETHEUS_BASE_URL: \"https://prometheus.monitoring.svc:9090\"\nLLMSM_PASSIVE: \"false\"\n"}))
		}, Warn, "falls back"},
		{"forbidden", func(f *fixture) { f.kubeReact = append(f.kubeReact, forbidden("get", "configmaps")) }, Unknown, "forbidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(f)
			r := only(t, f.run(t), "settings")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
		})
	}
}

// The legacy configuration supplies the URL the reachability check uses.
func TestSettingsLegacyFallbackFeedsPrometheus(t *testing.T) {
	f := newFixture(t)
	f.settings = nil
	f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": "PROMETHEUS_BASE_URL: https://old.monitoring.svc:9090\n"}))
	rep := f.run(t)
	if r := only(t, rep, "prometheus.reachable"); r.Status != Pass {
		t.Fatalf("prometheus.reachable %s %s", r.Status, r.Detail)
	}
	if f.captured == nil || f.captured.URL != "https://old.monitoring.svc:9090" {
		t.Fatalf("Prometheus built from %+v, want the legacy URL", f.captured)
	}
}

func TestKEDACRD(t *testing.T) {
	f := newFixture(t).remove("CustomResourceDefinition", "scaledobjects.keda.sh")
	if r := only(t, f.run(t), "keda.crd"); r.Status != Fail || !strings.Contains(r.Fix, "keda.sh") {
		t.Errorf("absent CRD: %s %q fix %q", r.Status, r.Detail, r.Fix)
	}
	f = newFixture(t)
	f.dynReact = append(f.dynReact, forbidden("get", "customresourcedefinitions"))
	if r := only(t, f.run(t), "keda.crd"); r.Status != Unknown || !strings.Contains(r.Detail, "forbidden") {
		t.Errorf("forbidden CRD read: %s %q, want UNKNOWN", r.Status, r.Detail)
	}
}

func TestKEDAVersion(t *testing.T) {
	cases := []struct {
		image  string
		want   Status
		detail string
	}{
		{"ghcr.io/kedacore/keda:2.21.0", Pass, "supported"},
		{"ghcr.io/kedacore/keda:2.22.1", Warn, "untested"},
		{"ghcr.io/kedacore/keda:2.20.0", Warn, "untested"},
		{"ghcr.io/kedacore/keda:2.19.0", Warn, "CVE-2026-77524"},
		{"ghcr.io/kedacore/keda:v2.18.1", Warn, "CVE-2026-77524"},
		{"ghcr.io/kedacore/keda:2.17.0", Fail, "below 2.18.0"},
		{"localhost:5000/keda:2.16", Fail, "no fallback.behavior"},
		{"ghcr.io/kedacore/keda@sha256:abc", Unknown, "names no version"},
		{"ghcr.io/kedacore/keda:main", Unknown, "names no version"},
	}
	for _, tc := range cases {
		t.Run(tc.image, func(t *testing.T) {
			f := newFixture(t).remove("Deployment", "keda-operator")
			f.add(deployment("keda", "keda-operator", map[string]string{"app.kubernetes.io/name": "keda-operator"}, tc.image))
			r := only(t, f.run(t), "keda.version")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
		})
	}
	f := newFixture(t).remove("Deployment", "keda-operator")
	if r := only(t, f.run(t), "keda.version"); r.Status != Unknown {
		t.Errorf("no operator Deployment: %s %q, want UNKNOWN", r.Status, r.Detail)
	}
}

func scalerPolicy(ns, admits string) *networkingv1.NetworkPolicy {
	p8443, p9090 := intstr.FromInt32(8443), intstr.FromInt32(9090)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: ScalerNetworkPolicy},
		Spec: networkingv1.NetworkPolicySpec{Ingress: []networkingv1.NetworkPolicyIngressRule{
			{Ports: []networkingv1.NetworkPolicyPort{{Port: &p8443}}},
			{Ports: []networkingv1.NetworkPolicyPort{{Port: &p9090}}, From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": admits}},
			}}},
		}},
	}
}

func TestKEDANetworkPolicy(t *testing.T) {
	t.Run("applied, mismatch", func(t *testing.T) {
		f := newFixture(t).add(scalerPolicy(testNS, "keda-system"))
		r := only(t, f.run(t), "keda.networkpolicy")
		if r.Status != Fail || !strings.Contains(r.Detail, "keda-system") || !strings.Contains(r.Fix, `"value":"keda"`) {
			t.Errorf("got %s %q fix %q", r.Status, r.Detail, r.Fix)
		}
	})
	t.Run("applied, match", func(t *testing.T) {
		f := newFixture(t).add(scalerPolicy(testNS, "keda"))
		if r := only(t, f.run(t), "keda.networkpolicy"); r.Status != Pass {
			t.Errorf("got %s %q", r.Status, r.Detail)
		}
	})
	t.Run("OpenShift file admits openshift-keda", func(t *testing.T) {
		f := newFixture(t).remove("APIService", externalMetricsAPIService)
		f.openshift = true
		f.dyn = append(f.dyn, apiService("openshift-keda", "keda-metrics-apiserver"))
		if r := only(t, f.run(t), "keda.networkpolicy"); r.Status != Pass {
			t.Errorf("got %s %q", r.Status, r.Detail)
		}
	})
	t.Run("policy forbidden", func(t *testing.T) {
		f := newFixture(t)
		f.kubeReact = append(f.kubeReact, forbidden("get", "networkpolicies"))
		if r := only(t, f.run(t), "keda.networkpolicy"); r.Status != Unknown {
			t.Errorf("got %s %q", r.Status, r.Detail)
		}
	})
	t.Run("APIService forbidden", func(t *testing.T) {
		f := newFixture(t)
		f.dynReact = append(f.dynReact, forbidden("get", "apiservices"))
		if r := only(t, f.run(t), "keda.networkpolicy"); r.Status != Unknown || !strings.Contains(r.Detail, "forbidden") {
			t.Errorf("got %s %q", r.Status, r.Detail)
		}
	})
	t.Run("served by another adapter", func(t *testing.T) {
		f := newFixture(t).remove("APIService", externalMetricsAPIService)
		f.dyn = append(f.dyn, apiService("monitoring", "prometheus-adapter"))
		if r := only(t, f.run(t), "keda.networkpolicy"); r.Status != Warn {
			t.Errorf("got %s %q", r.Status, r.Detail)
		}
	})
}
