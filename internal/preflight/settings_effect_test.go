package preflight

import (
	"context"
	"slices"
	"strings"
	"testing"

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
		if r.Status != Fail || !strings.Contains(r.Detail, "a running controller keeps the last good value") || !strings.Contains(r.Detail, tc.want) {
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
		{"own namespace, no policy namespace", func(*fixture) {}, "it refuses to start: limiter is declared"},
		{"well-known policy namespace", func(f *fixture) { f.add(namespace(config.WellKnownPolicyNamespace, nil)) },
			"not read beside the separate policy namespace (namespace " + config.WellKnownPolicyNamespace + " exists)"},
		{"labelled policy namespace", func(f *fixture) {
			f.remove("Namespace", testNS).add(namespace(testNS, map[string]string{constants.PolicyNamespaceLabelKey: "ops"}))
		}, "not read beside the separate policy namespace (" + testNS + " is labelled"},
		{"admin-owned controller ignores the well-known namespace", func(f *fixture) {
			f.add(namespace(config.WellKnownPolicyNamespace, nil), controllerDeployment(testNS, "--watch-namespace=team-b"))
		}, "it refuses to start: limiter is declared"},
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
