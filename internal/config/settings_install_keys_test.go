package config

import (
	"slices"
	"strings"
	"testing"
)

// The settings keys the install from the one release manifest adds
// (proposal PR #404, appendix B).
func TestSettingsInstallKeys(t *testing.T) {
	const enabled = "true"
	cases := []struct {
		name     string
		data     map[string]string
		rejected string
		check    func(t *testing.T, s Settings)
	}{
		{name: "kedaNamespace", data: map[string]string{SettingKEDANamespace: "keda-system"},
			check: func(t *testing.T, s Settings) {
				if got := s.Restart["LLMSM_KEDA_NAMESPACE"]; got != "keda-system" {
					t.Fatalf("LLMSM_KEDA_NAMESPACE %q", got)
				}
			}},
		{name: "kedaNamespace not a name", data: map[string]string{SettingKEDANamespace: "Keda_NS"}, rejected: SettingKEDANamespace},
		{name: "caFromSecret true", data: map[string]string{SettingCAFromSecret: enabled},
			check: func(t *testing.T, s Settings) {
				if got := s.Restart["PROMETHEUS_CA_CERT_PATH"]; got != PrometheusCAFromSecretPath {
					t.Fatalf("PROMETHEUS_CA_CERT_PATH %q", got)
				}
			}},
		{name: "caFromSecret false", data: map[string]string{SettingCAFromSecret: "false"},
			check: func(t *testing.T, s Settings) {
				if got, ok := s.Restart["PROMETHEUS_CA_CERT_PATH"]; ok {
					t.Fatalf("PROMETHEUS_CA_CERT_PATH %q set by caFromSecret false", got)
				}
			}},
		{name: "caFromSecret with the same path", data: map[string]string{SettingCAFromSecret: enabled,
			SettingPrometheusCACertPath: PrometheusCAFromSecretPath},
			check: func(t *testing.T, s Settings) {
				if got := s.Restart["PROMETHEUS_CA_CERT_PATH"]; got != PrometheusCAFromSecretPath {
					t.Fatalf("PROMETHEUS_CA_CERT_PATH %q", got)
				}
			}},
		{name: "caFromSecret with another path", data: map[string]string{SettingCAFromSecret: enabled,
			SettingPrometheusCACertPath: "/etc/other/ca.crt"},
			rejected: SettingCAFromSecret,
			check: func(t *testing.T, s Settings) {
				if got := s.Restart["PROMETHEUS_CA_CERT_PATH"]; got != "/etc/other/ca.crt" {
					t.Fatalf("PROMETHEUS_CA_CERT_PATH %q; the explicit path must stay", got)
				}
			}},
		{name: "caFromSecret yes", data: map[string]string{SettingCAFromSecret: "yes"}, rejected: SettingCAFromSecret},
		{name: "dsetRoleWriter true", data: map[string]string{SettingDSetRoleWriter: enabled},
			check: func(t *testing.T, s Settings) {
				if got := s.Restart["DSET_ROLE_WRITER"]; got != enabled {
					t.Fatalf("DSET_ROLE_WRITER %q", got)
				}
			}},
		{name: "dsetRoleWriter maybe", data: map[string]string{SettingDSetRoleWriter: "maybe"}, rejected: SettingDSetRoleWriter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, rep := ParseSettings(c.data, "team-a")
			if len(rep.Unknown) != 0 {
				t.Fatalf("unknown keys %v", rep.Unknown)
			}
			_, rejected := rep.Rejected[c.rejected]
			if c.rejected != "" && !rejected {
				t.Fatalf("%s not rejected: %v", c.rejected, rep.Rejected)
			}
			if c.rejected == "" && len(rep.Rejected) != 0 {
				t.Fatalf("rejected %v", rep.Rejected)
			}
			if c.check != nil {
				c.check(t, s)
			}
		})
	}
}

// The restart keys include dsetRoleWriter and kedaNamespace; caFromSecret is
// known but has no loader key of its own; namespaceSelector, the label opt-in
// of the first draft, is gone and points at watchNamespaces.
func TestInstallKeysClassified(t *testing.T) {
	if k, ok := SettingLoaderKey(SettingDSetRoleWriter); !ok || k != "DSET_ROLE_WRITER" {
		t.Errorf("dsetRoleWriter loader key %q, %v", k, ok)
	}
	if k, ok := SettingLoaderKey(SettingKEDANamespace); !ok || k != "LLMSM_KEDA_NAMESPACE" {
		t.Errorf("kedaNamespace loader key %q, %v", k, ok)
	}
	if _, ok := SettingLoaderKey(SettingCAFromSecret); ok {
		t.Error("caFromSecret has a loader key of its own; it sets PROMETHEUS_CA_CERT_PATH")
	}
	_, rep := ParseSettings(map[string]string{"namespaceSelector": "x=y"}, "team-a")
	if !slices.Equal(rep.Unknown, []string{"namespaceSelector"}) ||
		!strings.Contains(UnknownSettingHint("namespaceSelector"), SettingWatchNamespaces) {
		t.Errorf("namespaceSelector: unknown %v, hint %q", rep.Unknown, UnknownSettingHint("namespaceSelector"))
	}
}

// liveConfig loads a cluster-scoped controller with watchNamespaces live
// (--watch-namespaces-live, here its environment variable) and the settings
// data.
func liveConfig(t *testing.T, data map[string]string) *Config {
	t.Helper()
	t.Setenv("WATCH_NAMESPACE", "")
	t.Setenv("WATCH_NAMESPACES_LIVE", "true")
	t.Setenv("PROMETHEUS_BASE_URL", "https://p:9090")
	c, err := LoadWithSettings(nil, "", data)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// apply reads data as the settings reconciler does and puts it in force.
func apply(c *Config, prev Settings, data map[string]string, rv string) Settings {
	s, rep := ParseSettings(data, c.SettingsQuotaKey())
	c.RestrictSettings(&s, &rep)
	s = s.KeepRejected(prev, rep.Rejected)
	c.ApplySettings(s, true, rv, rep)
	return s
}

// With watchNamespaces live, the setting selects the managed namespaces on
// every change, with no restart: the cache stays cluster-wide, a change is
// never a restart setting, and an empty value manages every namespace.
func TestWatchNamespacesLive(t *testing.T) {
	c := liveConfig(t, map[string]string{SettingWatchNamespaces: "team-a"})
	if c.WatchNamespace() != "" {
		t.Fatalf("the cache was narrowed to %q; it must stay cluster-wide", c.WatchNamespace())
	}
	if names, all := c.ManagedNamespaces(); !slices.Equal(names, []string{"team-a"}) || all {
		t.Fatalf("at start: %v all %v", names, all)
	}

	s := apply(c, Settings{}, map[string]string{SettingWatchNamespaces: "team-a,team-b"}, "2")
	if !c.Manages("team-b") || c.Manages("team-c") {
		t.Fatalf("after the change: team-b %v team-c %v", c.Manages("team-b"), c.Manages("team-c"))
	}
	if changed := c.SettingsRestartChanged(s); slices.Contains(changed, SettingWatchNamespaces) {
		t.Fatalf("watchNamespaces is a restart change while live: %v", changed)
	}
	if src := c.SettingsSources()[SettingWatchNamespaces]; src != SettingSourceSettings {
		t.Errorf("source %q", src)
	}

	// A typo keeps the list in force; it never widens to every namespace.
	s = apply(c, s, map[string]string{SettingWatchNamespaces: "team-a,Team_B"}, "3")
	if !c.Manages("team-b") || c.Manages("team-c") {
		t.Fatalf("a rejected value changed what is managed: team-b %v team-c %v", c.Manages("team-b"), c.Manages("team-c"))
	}

	apply(c, s, map[string]string{}, "4")
	if names, all := c.ManagedNamespaces(); len(names) != 0 || !all || !c.Manages("team-c") {
		t.Fatalf("empty watchNamespaces: %v all %v; it must manage every namespace", names, all)
	}
}

// A rejected first value, with nothing in force before it, manages nothing:
// it fails closed rather than to every namespace.
func TestWatchNamespacesLiveRejectedFirstValue(t *testing.T) {
	c := liveConfig(t, nil)
	apply(c, Settings{}, map[string]string{SettingWatchNamespaces: "Team_A"}, "2")
	if names, all := c.ManagedNamespaces(); len(names) != 0 || all || c.Manages("team-a") {
		t.Fatalf("rejected first value: %v all %v", names, all)
	}
}

// Without --watch-namespaces-live, watchNamespaces stays a restart setting:
// the existing installs are unchanged.
func TestWatchNamespacesNotLive(t *testing.T) {
	t.Setenv("WATCH_NAMESPACE", "")
	t.Setenv("WATCH_NAMESPACES_LIVE", "")
	t.Setenv("PROMETHEUS_BASE_URL", "https://p:9090")
	c, err := LoadWithSettings(nil, "", map[string]string{SettingWatchNamespaces: "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	if c.WatchNamespacesLive() || c.WatchNamespace() != "team-a" {
		t.Fatalf("live %v watchNamespace %q", c.WatchNamespacesLive(), c.WatchNamespace())
	}
	s, _ := ParseSettings(map[string]string{SettingWatchNamespaces: "team-a,team-b"}, "team-a")
	if !slices.Contains(c.SettingsRestartChanged(s), SettingWatchNamespaces) {
		t.Fatal("watchNamespaces must stay a restart setting on every other install")
	}
}

// --watch-namespaces-live on a Deployment that names namespaces is not live:
// the setting may only narrow those, at start, as before.
func TestWatchNamespacesLiveNeedsClusterScope(t *testing.T) {
	t.Setenv("WATCH_NAMESPACE", "team-a,team-b")
	t.Setenv("WATCH_NAMESPACES_LIVE", "true")
	t.Setenv("PROMETHEUS_BASE_URL", "https://p:9090")
	c, err := LoadWithSettings(nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.WatchNamespacesLive() {
		t.Fatal("live on a namespaced Deployment")
	}
}

// kedaNamespace overrides --keda-namespace, as every restart setting
// overrides its flag.
func TestKEDANamespaceSetting(t *testing.T) {
	t.Setenv("LLMSM_KEDA_NAMESPACE", "keda")
	t.Setenv("PROMETHEUS_BASE_URL", "https://p:9090")
	c, err := LoadWithSettings(nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.KEDANamespace() != "keda" {
		t.Fatalf("without the setting %q", c.KEDANamespace())
	}
	c, err = LoadWithSettings(nil, "", map[string]string{SettingKEDANamespace: "keda-system"})
	if err != nil {
		t.Fatal(err)
	}
	if c.KEDANamespace() != "keda-system" {
		t.Fatalf("with the setting %q", c.KEDANamespace())
	}
}
