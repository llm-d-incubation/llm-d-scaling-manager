package preflight

import (
	"context"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// A watchNamespaces the controller rejects for widening what its Deployment
// names is not what the checks below read: they read the Deployment's.
func TestRejectedWatchNamespacesIsNotManaged(t *testing.T) {
	f := newFixture(t)
	f.add(controllerDeployment(testNS, "--watch-namespace=team-a"))
	f.settings[keyWatchNamespaces] = otherNS
	s := &state{env: f.env(t)}
	if got, named := s.managed(); !named || !slices.Equal(got, []string{"team-a"}) {
		t.Errorf("managed = %v %v, want [team-a] true", got, named)
	}
	f = newFixture(t)
	f.add(controllerDeployment(testNS, "--watch-namespace=team-a,team-b"))
	f.settings[keyWatchNamespaces] = otherNS
	s = &state{env: f.env(t)}
	if got, _ := s.managed(); !slices.Equal(got, []string{"team-b"}) {
		t.Errorf("narrowed: managed = %v, want [team-b]", got)
	}
}

// With the controller's Deployment unreadable, whether watchNamespaces only
// narrows it cannot be known: UNKNOWN, not a pass.
func TestWatchNamespacesUncheckedIsUnknown(t *testing.T) {
	f := newFixture(t)
	f.settings[keyWatchNamespaces] = otherNS
	f.kubeReact = append(f.kubeReact, forbidden("get", "deployments"), forbidden("list", "deployments"))
	if r := only(t, f.run(t), "settings"); r.Status != Unknown || !strings.Contains(r.Detail, "only narrows") {
		t.Errorf("got %s %q", r.Status, r.Detail)
	}
	// Get forbidden, list allowed (the cluster-wide preflight): read by list.
	f = newFixture(t)
	f.settings[keyWatchNamespaces] = otherNS
	f.add(controllerDeployment(testNS, "--watch-namespace="+testNS+",team-b"))
	f.kubeReact = append(f.kubeReact, forbidden("get", "deployments"))
	if r := only(t, f.run(t), "settings"); r.Status != Pass {
		t.Errorf("Deployment read by list: %s %q", r.Status, r.Detail)
	}
	f = newFixture(t)
	f.kubeReact = append(f.kubeReact, forbidden("get", "deployments"), forbidden("list", "deployments"))
	if r := only(t, f.run(t), "settings"); r.Status != Pass {
		t.Errorf("unset watchNamespaces, Deployment unreadable: %s %q", r.Status, r.Detail)
	}
}

// A FAIL says what the controller does with a rejected key: a running one
// keeps the last good value, and its next start depends on the key.
func TestRejectedSettingsSayWhatTheControllerDoes(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  map[string]string
		want string
	}{
		{"passive", map[string]string{keyPassive: "yes"}, "passive is true (a rejected passive fails closed)"},
		{"utilizationShare", map[string]string{keyUtilizationShare: "on"}, "utilizationShare is read from the scaling policy"},
		{"restart key", map[string]string{keyLogVerbosity: "11"}, "logVerbosity from the next source down"},
		{"quotas, no limiter", map[string]string{keyQuotas: "H200=eight"}, "the limiters of the scaling policy's default entry apply"},
		{"limiter declared", map[string]string{keyLimiter: "quota", keyQuotas: "H200=eight"}, "refuses to start"},
	} {
		f := newFixture(t)
		for k, v := range tc.set {
			f.settings[k] = v
		}
		r := only(t, f.run(t), "settings")
		if r.Status != Fail || !strings.Contains(r.Detail, "A running controller keeps the last good value") || !strings.Contains(r.Detail, tc.want) {
			t.Errorf("%s: %s %q, want %q", tc.name, r.Status, r.Detail, tc.want)
		}
	}
	// A rejected passive is read as true by the checks below, not from the
	// legacy configuration.
	f := newFixture(t)
	f.settings[keyPassive] = "yes"
	f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": "LLMSM_PASSIVE: \"false\"\n"}))
	s := &state{env: f.env(t)}
	if v := s.loadSettings(context.Background()).value(keyPassive); v != "true" {
		t.Errorf("rejected passive reads %q, want true", v)
	}
	// A rejected limiter key drops the whole group: kueue=true is not read.
	f = newFixture(t)
	f.settings[keyLimiter] = limiterQuota
	f.settings[keyKueue] = valueTrue
	s = &state{env: f.env(t)}
	if v := s.loadSettings(context.Background()).value(keyKueue); v != "false" {
		t.Errorf("kueue beside a rejected limiter reads %q, want the default false", v)
	}
}

// "Refuses to start" holds only where the controller reads the limiter
// settings (Config.SettingsBudgetApplies): not beside a separate policy
// namespace.
func TestUnboundedRefusalFollowsTheBudget(t *testing.T) {
	bad := func(f *fixture) {
		f.settings[keyLimiter] = limiterQuota
		f.settings[keyQuotas] = "H200=eight"
	}
	for _, tc := range []struct {
		name   string
		change func(*fixture)
		want   string
	}{
		{"own namespace, no policy namespace", func(*fixture) {}, "it refuses to start: limiter is set but the settings cannot build it"},
		{"well-known policy namespace", func(f *fixture) { f.add(namespace(config.WellKnownPolicyNamespace, nil)) },
			"not read beside the separate policy namespace (namespace " + config.WellKnownPolicyNamespace + " exists)"},
		{"labelled policy namespace", func(f *fixture) {
			f.remove("Namespace", testNS).add(namespace(testNS, map[string]string{constants.PolicyNamespaceLabelKey: "ops"}))
		}, "not read beside the separate policy namespace (" + testNS + " is labelled"},
		{"admin-owned controller ignores the well-known namespace", func(f *fixture) {
			f.add(namespace(config.WellKnownPolicyNamespace, nil), controllerDeployment(testNS, "--watch-namespace=team-b"))
		}, "it refuses to start: limiter is set but the settings cannot build it"},
		{"namespaces unreadable", func(f *fixture) { f.kubeReact = append(f.kubeReact, forbidden("get", "namespaces")) },
			"refuses to start unless a separate policy namespace supplies the limiters"},
	} {
		f := newFixture(t)
		bad(f)
		tc.change(f)
		if r := only(t, f.run(t), "settings"); r.Status != Fail || !strings.Contains(r.Detail, tc.want) {
			t.Errorf("%s: %s %q, want %q", tc.name, r.Status, r.Detail, tc.want)
		}
	}
}

// A WARN for an unknown key still says what was read from the legacy
// configuration.
func TestSettingsWarnKeepsTheLegacyNote(t *testing.T) {
	f := newFixture(t)
	delete(f.settings, keyScaleToZero)
	f.settings["bogus"] = "1"
	f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": "LLMSM_SCALE_TO_ZERO: \"true\"\n"}))
	r := only(t, f.run(t), "settings")
	if r.Status != Warn || !strings.Contains(r.Detail, "bogus is no setting") || !strings.Contains(r.Detail, "read scaleToZero from "+LegacyConfigMap) {
		t.Errorf("got %s %q", r.Status, r.Detail)
	}
}

// What the controller's Deployment names, read as the loader reads
// WATCH_NAMESPACE: the flag when passed (an empty one is every namespace),
// else the container's WATCH_NAMESPACE with $(VAR) resolved, else config.yaml.
func TestDeploymentWatchSources(t *testing.T) {
	withEnv := func(d *appsv1.Deployment, env ...corev1.EnvVar) *appsv1.Deployment {
		d.Spec.Template.Spec.Containers[0].Env = env
		return d
	}
	watchEnv := func(v string) corev1.EnvVar { return corev1.EnvVar{Name: "WATCH_NAMESPACE", Value: v} }
	for _, tc := range []struct {
		name   string
		deploy *appsv1.Deployment
		legacy string
		want   []string
	}{
		{"flag", controllerDeployment(testNS, "--watch-namespace="+otherNS), "", []string{otherNS}},
		{"flag, then env", withEnv(controllerDeployment(testNS, "--watch-namespace="+testNS), watchEnv(otherNS)), "", []string{testNS}},
		{"flag repeated: the last wins", controllerDeployment(testNS, "--watch-namespace="+otherNS, "--watch-namespace", testNS), "", []string{testNS}},
		{"empty flag: every namespace, env not read", withEnv(controllerDeployment(testNS, "--watch-namespace="), watchEnv(otherNS)), "", nil},
		{"env", withEnv(controllerDeployment(testNS), watchEnv(otherNS)), "", []string{otherNS}},
		{"env with $(VAR)", withEnv(controllerDeployment(testNS),
			corev1.EnvVar{Name: "LLMSM_WATCH_NAMESPACE", Value: "$(POD_NAMESPACE)"}, watchEnv("$(LLMSM_WATCH_NAMESPACE),"+otherNS)), "", []string{testNS, otherNS}},
		{"empty env: config.yaml", withEnv(controllerDeployment(testNS), watchEnv("")), otherNS, []string{otherNS}},
		{"config.yaml", controllerDeployment(testNS), otherNS, []string{otherNS}},
		{"nothing: every namespace", controllerDeployment(testNS), "", nil},
	} {
		f := newFixture(t)
		f.add(tc.deploy)
		if tc.legacy != "" {
			f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": "WATCH_NAMESPACE: " + tc.legacy + "\n"}))
		}
		s := &state{env: f.env(t)}
		got, named := s.managed()
		if !slices.Equal(got, tc.want) || named != (tc.want != nil) {
			t.Errorf("%s: managed = %v %v, want %v", tc.name, got, named, tc.want)
		}
	}
}

// How the controller's Deployment is read: by get, else by list (the
// cluster-wide preflight may only list); absent, the install files' scope.
func TestDeploymentWatchReads(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fixture)
		want   []string
		named  bool
	}{
		{"get forbidden, list finds none: the namespaced files", func(f *fixture) {
			f.kubeReact = append(f.kubeReact, forbidden("get", "deployments"))
		}, []string{testNS}, true},
		{"get forbidden, list finds none, cluster-wide files: every namespace", func(f *fixture) {
			f.scope = ScopeCluster
			f.kubeReact = append(f.kubeReact, forbidden("get", "deployments"))
		}, nil, false},
		{"list forbidden, get allowed", func(f *fixture) {
			f.add(controllerDeployment(testNS, "--watch-namespace="+otherNS))
			f.kubeReact = append(f.kubeReact, forbidden("list", "deployments"))
		}, []string{otherNS}, true},
		{"cluster-wide, before the install: every namespace", func(f *fixture) { f.scope = ScopeCluster }, nil, false},
	} {
		f := newFixture(t)
		tc.change(f)
		s := &state{env: f.env(t)}
		got, named := s.managed()
		if !slices.Equal(got, tc.want) || named != tc.named {
			t.Errorf("%s: managed = %v %v, want %v %v", tc.name, got, named, tc.want, tc.named)
		}
		if r := only(t, f.run(t), "settings"); r.Status == Unknown {
			t.Errorf("%s: settings %s %q", tc.name, r.Status, r.Detail)
		}
	}
}

// The fixes: a widening watchNamespaces is removed or narrowed; an
// unreadable Deployment needs a read, or an admin.
func TestWatchNamespacesFixes(t *testing.T) {
	f := newFixture(t)
	f.settings[keyWatchNamespaces] = otherNS
	r := only(t, f.run(t), "settings")
	if r.Status != Fail || !strings.Contains(r.Fix, `"op":"remove","path":"/data/watchNamespaces"`) || !strings.Contains(r.Fix, "subset of "+testNS) {
		t.Errorf("widening: %s fix %q", r.Status, r.Fix)
	}
	f = newFixture(t)
	f.settings[keyWatchNamespaces] = otherNS
	f.kubeReact = append(f.kubeReact, forbidden("get", "deployments"), forbidden("list", "deployments"))
	r = only(t, f.run(t), "settings")
	if r.Status != Unknown || !strings.Contains(r.Fix, "get or list Deployment "+ControllerDeployment) {
		t.Errorf("unreadable: %s fix %q", r.Status, r.Fix)
	}
}
