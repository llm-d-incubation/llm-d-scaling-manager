package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	flag "github.com/spf13/pflag"
)

// A rejected passive with nothing before fails closed: the controller does
// not act on a typo.
func TestSettingsRejectedPassiveFailsClosed(t *testing.T) {
	s, rep := ParseSettings(map[string]string{SettingPassive: "yes"}, "default")
	s = s.KeepRejected(Settings{}, rep.Rejected)
	c := NewTestConfig()
	c.ApplySettings(s, true, "1", rep)
	if !c.PassiveDefault() {
		t.Fatal(`passive: "yes" left the controller active`)
	}
	// A value in force before is kept instead.
	s, rep = ParseSettings(map[string]string{SettingPassive: "yes"}, "default")
	if got := s.KeepRejected(Settings{Passive: ptr(false)}, rep.Rejected); got.Passive == nil || *got.Passive {
		t.Errorf("passive %v, want the false in force before", got.Passive)
	}
}

// A limiter declared with a rejected value and nothing before is unbounded:
// RejectedLimiterUnbounded says so, so the start refuses.
func TestRejectedLimiterUnbounded(t *testing.T) {
	cases := []struct {
		name string
		data map[string]string
		prev Settings
		want bool
	}{
		{name: "quota with a malformed budget", data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200:8"}, want: true},
		{name: "quota with no budget", data: map[string]string{SettingLimiter: "quota"}, want: true},
		{name: "unknown limiter", data: map[string]string{SettingLimiter: "quotas", SettingQuotas: "H200=8"}, want: true},
		{name: "a limiter in force before is kept", data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200:8"},
			prev: Settings{Limiters: []QuotaLimiterConfig{{Type: "gpu-inventory"}}}},
		{name: "no limiter declared: the policy's applies", data: map[string]string{SettingKueue: "y"}},
		{name: "valid", data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=8"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, rep := ParseSettings(tc.data, "default")
			err := RejectedLimiterUnbounded(tc.data, rep.Rejected, tc.prev)
			if (err != nil) != tc.want {
				t.Fatalf("error %v, want error %t", err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), "refusing to start unbounded") {
				t.Errorf("error %v", err)
			}
		})
	}
}

// The setting may only narrow the namespaces the Deployment names, and the
// ownership decision reads the Deployment's value.
func TestWatchNamespacesSettingOnlyNarrows(t *testing.T) {
	t.Setenv("WATCH_NAMESPACE", "")
	t.Setenv("PROMETHEUS_BASE_URL", "https://p:9090")
	load := func(flagValue, setting string) *Config {
		t.Helper()
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.String("watch-namespace", "", "")
		if flagValue != "" {
			if err := fs.Parse([]string{"--watch-namespace=" + flagValue}); err != nil {
				t.Fatal(err)
			}
		}
		c, err := LoadWithSettings(fs, "", map[string]string{SettingWatchNamespaces: setting})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	cases := []struct{ flag, setting, want string }{
		{"team-a", ",", "team-a"},
		{"team-a", "team-b", "team-a"},
		{"team-a", "team-a,team-b", "team-a"},
		{"team-a,team-b", "team-a", "team-a"},
		{"", "team-a", "team-a"},
	}
	for _, tc := range cases {
		c := load(tc.flag, tc.setting)
		if c.WatchNamespace() != tc.want || c.DeploymentWatchNamespace() != tc.flag {
			t.Errorf("flag %q setting %q: watch %q deployment %q, want %q and %q",
				tc.flag, tc.setting, c.WatchNamespace(), c.DeploymentWatchNamespace(), tc.want, tc.flag)
		}
	}

	// At runtime the same rule rejects, with a reason.
	c := load("team-a", "")
	s, rep := ParseSettings(map[string]string{SettingWatchNamespaces: "team-b"}, "default")
	c.RestrictSettings(&s, &rep)
	if _, set := s.Restart["WATCH_NAMESPACE"]; set || !strings.Contains(rep.Rejected[SettingWatchNamespaces], "only narrow") {
		t.Errorf("restart %v rejected %v", s.Restart, rep.Rejected)
	}
	_, rep = ParseSettings(map[string]string{SettingWatchNamespaces: " , "}, "default")
	if !strings.Contains(rep.Rejected[SettingWatchNamespaces], "lists no namespace") {
		t.Errorf("rejected %v", rep.Rejected)
	}
}

// A config.yaml path that is missing, or a directory (what the kubelet mounts
// for an optional ConfigMap that does not exist), is absent: the settings
// and the defaults apply.
func TestLoadConfigFileMissingOrDirectory(t *testing.T) {
	t.Setenv("PROMETHEUS_BASE_URL", "")
	dir := t.TempDir()
	asDir := filepath.Join(dir, "config.yaml")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(dir, "absent.yaml"), "directory": asDir} {
		c, err := LoadWithSettings(nil, path, map[string]string{SettingPrometheusURL: "https://from-settings:9090"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.PrometheusBaseURL() != "https://from-settings:9090" || c.SettingsSource() != SettingsSourceSettingsOnly {
			t.Errorf("%s: url %s source %s", name, c.PrometheusBaseURL(), c.SettingsSource())
		}
	}
}

// Restart detection compares meanings, not spellings; the shipped defaults of
// the limiter keys are not warned about.
func TestSettingsReviewNits(t *testing.T) {
	t.Setenv("PROMETHEUS_BASE_URL", "")
	c, err := LoadWithSettings(nil, "", map[string]string{SettingPrometheusURL: "https://p:9090",
		"GLOBAL_OPT_INTERVAL": "60s", SettingScaleToZero: "True"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := ParseSettings(map[string]string{SettingPrometheusURL: "https://p:9090", "GLOBAL_OPT_INTERVAL": "1m",
		SettingScaleToZero: "true"}, "default")
	if got := c.SettingsRestartChanged(s); len(got) != 0 {
		t.Errorf("changed %v for the same values spelled otherwise", got)
	}
	_, rep := ParseSettings(map[string]string{SettingPassive: "true", SettingUtilizationShare: "off",
		SettingQuotaScope: "namespace", SettingKueue: "false", SettingKueueBorrow: "true",
		SettingKueueNamespace: "kueue-system"}, "default")
	if len(rep.Warnings) != 0 || len(rep.Rejected) != 0 {
		t.Errorf("the shipped defaults warned %q rejected %v", rep.Warnings, rep.Rejected)
	}
	_, rep = ParseSettings(map[string]string{SettingPrometheusCACertPath: "/etc/prometheus-ca/ca.crt",
		SettingPrometheusServerName: "prometheus.monitoring.svc"}, "default")
	if len(rep.Rejected) != 0 {
		t.Errorf("rejected %v", rep.Rejected)
	}
	_, rep = ParseSettings(map[string]string{SettingPrometheusCACertPath: "ca.crt",
		SettingPrometheusServerName: "https://x"}, "default")
	if len(rep.Rejected) != 2 {
		t.Errorf("rejected %v, want both", rep.Rejected)
	}
}
