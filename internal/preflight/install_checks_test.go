package preflight

import (
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func controllerSubject(ns string) []rbacv1.Subject {
	return []rbacv1.Subject{{Kind: "ServiceAccount", Name: ControllerServiceAccount, Namespace: ns}}
}

func managerRoleBinding(in, name, controllerNS string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: in, Name: name},
		RoleRef:  rbacv1.RoleRef{Kind: "Role", Name: "scaling-manager-manager-role"},
		Subjects: controllerSubject(controllerNS)}
}

// Another scaling manager bound where this one manages is a FAIL, found from
// the bindings alone; its own bindings and other roles are not.
func TestConflicts(t *testing.T) {
	cases := []struct {
		name   string
		change func(*fixture)
		want   Status
		detail string
	}{
		{"none", func(f *fixture) {
			f.add(managerRoleBinding(testNS, "own", testNS),
				&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "unrelated"},
					RoleRef: rbacv1.RoleRef{Kind: "Role", Name: "view"}, Subjects: controllerSubject("elsewhere")})
		}, Pass, "no other scaling manager"},
		{"a cluster-wide one elsewhere", func(f *fixture) {
			f.add(&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "scaling-manager-manager-rolebinding"},
				RoleRef:  rbacv1.RoleRef{Kind: "ClusterRole", Name: "scaling-manager-manager-role"},
				Subjects: controllerSubject("scaling-manager-system")})
		}, Fail, "cluster-wide scaling manager runs in scaling-manager-system"},
		{"another one in this namespace", func(f *fixture) {
			f.add(managerRoleBinding(testNS, "theirs", "team-z"))
		}, Fail, "also managed by the scaling manager in team-z"},
		{"another one in a namespace this one does not manage", func(f *fixture) {
			f.add(managerRoleBinding("team-y", "theirs", "team-z"))
		}, Pass, "no other scaling manager"},
		{"cluster scope, theirs in a namespace watchNamespaces leaves out", func(f *fixture) {
			f.scope = ScopeCluster
			f.manages = func(ns string) bool { return ns == "team-a" }
			f.add(managerRoleBinding("team-y", "theirs", "team-y"))
		}, Warn, "do not add team-y to watchNamespaces here"},
		{"cluster scope, theirs in a namespace watchNamespaces lists", func(f *fixture) {
			f.scope = ScopeCluster
			f.manages = func(ns string) bool { return ns == "team-y" }
			f.add(managerRoleBinding("team-y", "theirs", "team-y"))
		}, Fail, "managed here and by the scaling manager in team-y"},
		// An empty watchNamespaces manages every namespace, so another
		// install's namespace is a conflict.
		{"cluster scope, every namespace managed", func(f *fixture) {
			f.scope = ScopeCluster
			f.add(managerRoleBinding("team-y", "theirs", "team-y"))
		}, Fail, "managed here and by the scaling manager in team-y"},
		{"bindings forbidden", func(f *fixture) {
			f.kubeReact = append(f.kubeReact, forbidden("list", "rolebindings"))
		}, Unknown, "needs list on rolebindings"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(f)
			r := only(t, f.run(t), "conflicts")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
		})
	}
}

func TestPolicySource(t *testing.T) {
	cases := []struct {
		name   string
		status map[string]string
		want   Status
		detail string
	}{
		{"own namespace, settings", map[string]string{statusPolicyNamespace: testNS, statusPolicyConfigMap: "scaling-manager-scaling-policy-config",
			statusPolicyAccepted: "true", statusSettingsSource: "settings"}, Pass, "settings ConfigMap overrides its default entry"},
		{"separate policy namespace", map[string]string{statusPolicyNamespace: "scaling-manager-policy", statusPolicyConfigMap: "p",
			statusPolicyAccepted: "true", statusSettingsSource: "settings"}, Pass, "do not override it"},
		{"rejected", map[string]string{statusPolicyNamespace: testNS, statusPolicyConfigMap: "p",
			statusPolicyAccepted: "false", statusPolicyError: "bad limiter"}, Fail, "bad limiter"},
		{"not reported", map[string]string{}, Unknown, "not reported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.remove("ConfigMap", ControllerStatusConfigMap)
			f.add(cm(testNS, ControllerStatusConfigMap, tc.status))
			r := only(t, f.run(t), "policy.source")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
		})
	}
	f := newFixture(t)
	f.remove("ConfigMap", ControllerStatusConfigMap)
	if rs := results(f.run(t), "policy.source"); len(rs) != 0 {
		t.Errorf("before the controller ran: %v, want not applicable", rs)
	}
}

func TestScrapeAndEPPMetrics(t *testing.T) {
	f := newFixture(t)
	// The fixture's Prometheus holds the current EPP name.
	rep := f.run(t)
	if r := only(t, rep, "metrics.scrape"); r.Status != Pass {
		t.Errorf("metrics.scrape %s %q", r.Status, r.Detail)
	}
	if r := only(t, rep, "metrics.epp"); r.Status != Pass || !strings.Contains(r.Detail, "llm_d_epp_flow_control_queue_size") {
		t.Errorf("metrics.epp %s %q", r.Status, r.Detail)
	}

	f = newFixture(t)
	f.prom.doubles = []Sample{{Labels: map[string]string{"namespace": testNS, "pod": "llama-decode-0"}, Value: 2}}
	f.prom.epp = nil
	rep = f.run(t)
	if r := only(t, rep, "metrics.scrape"); r.Status != Fail || !strings.Contains(r.Detail, testNS+"/llama-decode-0") {
		t.Errorf("double scrape: metrics.scrape %s %q", r.Status, r.Detail)
	}
	if r := only(t, rep, "metrics.epp"); r.Status != Warn {
		t.Errorf("no EPP metric: metrics.epp %s %q, want WARN", r.Status, r.Detail)
	}
}
