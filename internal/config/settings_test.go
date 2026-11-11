package config

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	flag "github.com/spf13/pflag"
)

func mustPolicy(t *testing.T, doc string) ScalingPolicy {
	t.Helper()
	p, err := DecodeScalingPolicy(doc)
	if err != nil {
		t.Fatalf("decode %q: %v", doc, err)
	}
	return p
}

func ptr[T any](v T) *T { return &v }

// Each live key: a valid value is read, an empty one is unset, and an invalid
// one is rejected with a reason that names what is accepted.
func TestParseSettingsLiveKeys(t *testing.T) {
	cases := []struct {
		name       string
		data       map[string]string
		want       Settings
		rejected   string
		wantReason string
	}{
		{name: "empty is unset", data: map[string]string{SettingPassive: "", SettingUtilizationShare: " "}},
		{name: "passive true", data: map[string]string{SettingPassive: "true"}, want: Settings{Passive: ptr(true)}},
		{name: "passive false", data: map[string]string{SettingPassive: "false"}, want: Settings{Passive: ptr(false)}},
		{name: "passive invalid", data: map[string]string{SettingPassive: "yes please"},
			rejected: SettingPassive, wantReason: "not true or false"},
		{name: "shadow", data: map[string]string{SettingUtilizationShare: "shadow"}, want: Settings{UtilizationShare: "shadow"}},
		{name: "active", data: map[string]string{SettingUtilizationShare: "active"}, want: Settings{UtilizationShare: "active"}},
		{name: "off", data: map[string]string{SettingUtilizationShare: "off"}, want: Settings{UtilizationShare: "off"}},
		{name: "mode invalid", data: map[string]string{SettingUtilizationShare: "Active"},
			rejected: SettingUtilizationShare, wantReason: "not off, shadow or active"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, rep := ParseSettings(tc.data, "team-a")
			s.Restart = nil
			if !reflect.DeepEqual(s, tc.want) {
				t.Errorf("settings %+v, want %+v", s, tc.want)
			}
			if tc.rejected == "" {
				if len(rep.Rejected) != 0 {
					t.Errorf("rejected %v, want none", rep.Rejected)
				}
				return
			}
			if !strings.Contains(rep.Rejected[tc.rejected], tc.wantReason) {
				t.Errorf("rejected %v, want %s: ...%s", rep.Rejected, tc.rejected, tc.wantReason)
			}
		})
	}
}

// Each restart key and an advanced one: valid values land under the loader
// key, invalid ones are rejected and absent.
func TestParseSettingsRestartKeys(t *testing.T) {
	good := map[string]string{
		SettingPrometheusURL:                "https://prometheus.monitoring.svc:9090",
		SettingPrometheusInsecureSkipVerify: "false",
		SettingScaleToZero:                  "true",
		SettingWatchNamespaces:              "team-a, team-b",
		SettingLogVerbosity:                 "4",
		"GLOBAL_OPT_INTERVAL":               "30s",
		"LEARNED_STATE_SNAPSHOT":            "true",
	}
	s, rep := ParseSettings(good, "default")
	if len(rep.Rejected) != 0 || len(rep.Unknown) != 0 {
		t.Fatalf("rejected %v unknown %v, want none", rep.Rejected, rep.Unknown)
	}
	want := map[string]string{
		"PROMETHEUS_BASE_URL": good[SettingPrometheusURL], "PROMETHEUS_TLS_INSECURE_SKIP_VERIFY": "false",
		"LLMSM_SCALE_TO_ZERO": "true", "WATCH_NAMESPACE": "team-a, team-b", "V": "4",
		"GLOBAL_OPT_INTERVAL": "30s", "LEARNED_STATE_SNAPSHOT": "true",
	}
	if !reflect.DeepEqual(s.Restart, want) {
		t.Errorf("restart %v, want %v", s.Restart, want)
	}

	bad := map[string]string{
		SettingPrometheusURL:                "prometheus:9090",
		SettingPrometheusInsecureSkipVerify: "maybe",
		SettingScaleToZero:                  "on",
		SettingWatchNamespaces:              "Team_A",
		SettingLogVerbosity:                 "11",
		"GLOBAL_OPT_INTERVAL":               "15",
		"LEARNED_STATE_SNAPSHOT":            "sometimes",
	}
	s, rep = ParseSettings(bad, "default")
	if len(s.Restart) != 0 {
		t.Errorf("restart %v, want nothing from invalid values", s.Restart)
	}
	for k := range bad {
		if rep.Rejected[k] == "" {
			t.Errorf("%s=%q was not rejected", k, bad[k])
		}
	}
}

// Keys that are no setting are listed, and the old names say what replaced
// them.
func TestParseSettingsUnknownKeys(t *testing.T) {
	_, rep := ParseSettings(map[string]string{"LLMSM_PASSIVE": "true", "Passive": "true", "colour": "red",
		SettingPassive: "true"}, "default")
	if want := []string{"LLMSM_PASSIVE", "Passive", "colour"}; !slices.Equal(rep.Unknown, want) {
		t.Fatalf("unknown %v, want %v", rep.Unknown, want)
	}
	if h := UnknownSettingHint("LLMSM_PASSIVE"); !strings.Contains(h, SettingPassive) {
		t.Errorf("hint for LLMSM_PASSIVE %q does not name %s", h, SettingPassive)
	}
	if h := UnknownSettingHint("Passive"); !strings.Contains(h, "case-sensitive") {
		t.Errorf("hint for Passive %q", h)
	}
	if h := UnknownSettingHint("colour"); h != "" {
		t.Errorf("hint for colour %q, want none", h)
	}
}

// The limiter keys build the list the installer writes, keyed and scoped the
// same way, and are rejected as a group.
func TestParseSettingsLimiters(t *testing.T) {
	f := false
	cases := []struct {
		name     string
		data     map[string]string
		key      string
		want     []QuotaLimiterConfig
		rejected []string
		warns    int
	}{
		{name: "unset", data: map[string]string{}},
		{name: "namespace quota keyed on the managed namespace",
			data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=8,A100=-1"}, key: "team-a",
			want: []QuotaLimiterConfig{{Name: SettingsQuotaName, Type: "quota", Scope: QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"team-a": {"H200": 8, "A100": -1}}}}},
		{name: "cluster quota, spaces between pairs",
			data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=8 A100=4", SettingQuotaScope: "cluster"},
			key:  "default",
			want: []QuotaLimiterConfig{{Name: SettingsQuotaName, Type: "quota", Scope: QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H200": 8, "A100": 4}}}},
		{name: "physical and quota, with Kueue not borrowing",
			data: map[string]string{SettingLimiter: "gpu-inventory,quota", SettingQuotas: "H100=-1",
				SettingKueue: "true", SettingKueueBorrow: "false", SettingKueueNamespace: "kueue"},
			key: "default",
			want: []QuotaLimiterConfig{{Type: "gpu-inventory"}, {Name: SettingsQuotaName, Type: "quota", Scope: QuotaScopeNamespace,
				NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
				Kueue:           &KueueQuotaSource{Enabled: true, ControllerNamespace: "kueue", CountBorrowable: &f}}}},
		{name: "physical alone ignores quotas", data: map[string]string{SettingLimiter: "gpu-inventory", SettingQuotas: "H100=1"},
			key: "default", want: []QuotaLimiterConfig{{Type: "gpu-inventory"}}, warns: 1},
		{name: "quotas without limiter leave the policy alone",
			data: map[string]string{SettingQuotas: "H100=1", SettingKueue: "true"}, key: "default", warns: 2},
		{name: "quota without quotas", data: map[string]string{SettingLimiter: "quota"}, key: "default",
			rejected: []string{SettingQuotas}},
		{name: "bad quota value", data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=8,H200=4"},
			key: "default", rejected: []string{SettingQuotas}},
		{name: "unknown limiter", data: map[string]string{SettingLimiter: "quota,priority", SettingQuotas: "H200=8"},
			key: "default", rejected: []string{SettingLimiter}},
		{name: "bad scope", data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=8", SettingQuotaScope: "global"},
			key: "default", rejected: []string{SettingQuotaScope}},
		{name: "bad kueue", data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=8", SettingKueue: "y"},
			key: "default", rejected: []string{SettingKueue}},
		{name: "a zero budget is accepted with a warning",
			data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=0", SettingQuotaScope: "cluster"}, key: "default",
			want: []QuotaLimiterConfig{{Name: SettingsQuotaName, Type: "quota", Scope: QuotaScopeCluster,
				ClusterQuotas: map[string]int{"H200": 0}}}, warns: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, rep := ParseSettings(tc.data, tc.key)
			if !reflect.DeepEqual(s.Limiters, tc.want) {
				t.Errorf("limiters %+v, want %+v", s.Limiters, tc.want)
			}
			if got := slices.Sorted(maps.Keys(rep.Rejected)); !slices.Equal(got, tc.rejected) {
				t.Errorf("rejected %v, want keys %v", rep.Rejected, tc.rejected)
			}
			if len(rep.Warnings) != tc.warns {
				t.Errorf("warnings %q, want %d", rep.Warnings, tc.warns)
			}
		})
	}
}

// A rejected value keeps the one in force before; the limiter keys are kept
// as one; with nothing before, the key stays unset.
func TestSettingsKeepRejected(t *testing.T) {
	prev := Settings{Passive: ptr(true), UtilizationShare: UtilizationShareSettingShadow,
		Limiters: []QuotaLimiterConfig{{Type: "gpu-inventory"}}, Restart: map[string]string{"V": "3"}}
	next, rep := ParseSettings(map[string]string{SettingPassive: "nope", SettingUtilizationShare: "on",
		SettingLimiter: "quota", SettingLogVerbosity: "x"}, "default")
	got := next.KeepRejected(prev, rep.Rejected)
	if got.Passive == nil || !*got.Passive || got.UtilizationShare != UtilizationShareSettingShadow ||
		!reflect.DeepEqual(got.Limiters, prev.Limiters) || got.Restart["V"] != "3" {
		t.Errorf("kept %+v, want the previous values", got)
	}
	got = next.KeepRejected(Settings{}, rep.Rejected)
	// From nothing, passive fails closed; every other key stays unset.
	if got.Passive == nil || !*got.Passive || got.UtilizationShare != "" || got.Limiters != nil || len(got.Restart) != 0 {
		t.Errorf("kept %+v from nothing, want passive true and every other key unset", got)
	}
}

// policyDefault is a default entry that sets every live field; precedenceConfig
// adds a tier and a per-model entry that set passive, for the precedence tests.
const policyDefault = `passive: false
limiters:
  - type: quota
    name: policy-quota
    scope: cluster
    quotas: { H100: 2 }
optimizer:
  type: utilizationShare
  utilizationShare:
    shadow: true
    tolerance: 0.3
`

func precedenceConfig(t *testing.T) *Config {
	t.Helper()
	c := NewTestConfig()
	c.SetPolicyNamespace(SystemNamespace(), PolicySourceLocal)
	c.UpdateScalingPolicyConfig(ScalingPolicySet{
		GlobalDefaultsKey: mustPolicy(t, policyDefault),
		"gold":            mustPolicy(t, "passive: true\n"),
		"pinned":          mustPolicy(t, "model_id: m-pinned\nnamespace: ns\npassive: false\n"),
	})
	return c
}

// Settings > policy default entry > flag; per-model and tier entries still
// win over the settings.
func TestSettingsPrecedenceOverPolicy(t *testing.T) {
	c := precedenceConfig(t)

	// No settings: the policy as written.
	if p, src := ResolvePassive(c, "ns", "m", nil); p || src != PassiveSourceDefault {
		t.Fatalf("passive %t from %s, want false from the policy default", p, src)
	}
	if mode, _ := c.UtilizationShareMode(); mode != "shadow" {
		t.Fatalf("mode %s, want the policy's shadow", mode)
	}
	if got := c.EffectiveLimiterSummary(); got != "quota/policy-quota" {
		t.Fatalf("limiter %s", got)
	}

	s, rep := ParseSettings(map[string]string{SettingPassive: "true", SettingUtilizationShare: "active",
		SettingLimiter: "quota", SettingQuotas: "H200=8", SettingQuotaScope: "cluster"}, c.SettingsQuotaKey())
	if len(rep.Rejected) != 0 {
		t.Fatal(rep.Rejected)
	}
	c.ApplySettings(s, true, "7", rep)

	if p, src := ResolvePassive(c, "ns", "m", nil); !p || src != PassiveSourceSettings {
		t.Errorf("default model: passive %t from %s, want true from settings", p, src)
	}
	if p, src := ResolvePassive(c, "ns", "m", []string{"gold"}); !p || src != "tier:gold" {
		t.Errorf("tier: passive %t from %s, want true from the tier", p, src)
	}
	if p, src := ResolvePassive(c, "ns", "m-pinned", nil); p || src != PassiveSourcePerModel {
		t.Errorf("per-model entry: passive %t from %s, want false from the model entry", p, src)
	}
	us, selected, err := c.UtilizationShare()
	if err != nil || !selected || us.Shadow || us.Tolerance != 0.3 {
		t.Errorf("optimizer %+v selected %t err %v, want active with the policy's tolerance kept", us, selected, err)
	}
	if got := c.EffectiveLimiterSummary(); got != "quota/install-quota" {
		t.Errorf("limiter %s, want the settings' quota", got)
	}
	if q := c.EffectiveQuotaEntries(); len(q) != 1 || q[0].ClusterQuotas["H200"] != 8 {
		t.Errorf("quota entries %+v", q)
	}
	// The policy as written is unchanged: a rejected entry is replaced by it.
	if def, _ := c.ScalingPolicyEntry("", GlobalDefaultsKey); *def.Passive || def.Limiters[0].Name != "policy-quota" {
		t.Errorf("stored entry changed: %+v", def)
	}
	overrides := SettingsOverridePolicy(mustPolicy(t, policyDefault), s, true)
	if len(overrides) != 3 {
		t.Errorf("overrides %q, want passive, utilizationShare and limiters", overrides)
	}
	src := c.SettingsSources()
	for _, k := range []string{SettingPassive, SettingUtilizationShare, SettingLimiter, SettingQuotas} {
		if src[k] != SettingSourceSettings {
			t.Errorf("source of %s is %s, want settings", k, src[k])
		}
	}

	// A policy reload keeps the settings folded in.
	c.UpdateScalingPolicyConfig(ScalingPolicySet{GlobalDefaultsKey: mustPolicy(t, policyDefault)})
	if !c.PassiveDefault() || c.EffectiveLimiterSummary() != "quota/install-quota" {
		t.Error("a policy reload dropped the settings")
	}

	// utilizationShare off removes the block whatever the policy says.
	c.ApplySettings(Settings{UtilizationShare: UtilizationShareSettingOff}, true, "8", SettingsReport{})
	if mode, _ := c.UtilizationShareMode(); mode != UtilizationShareSettingOff {
		t.Errorf("mode %s, want off", mode)
	}
	// Unset again: the policy as written.
	c.ApplySettings(Settings{}, true, "9", SettingsReport{})
	if mode, _ := c.UtilizationShareMode(); mode != "shadow" || c.PassiveDefault() || c.EffectiveLimiterSummary() != "quota/policy-quota" {
		t.Errorf("unset settings left mode %s passive %t limiter %s", mode, c.PassiveDefault(), c.EffectiveLimiterSummary())
	}
}

// With no policy at all the settings make the default entry; a namespace's
// own map without passive falls to the settings, and one with passive wins.
func TestSettingsPassiveWithoutPolicyAndNamespaceMaps(t *testing.T) {
	c := NewTestConfig()
	c.ApplySettings(Settings{Passive: ptr(true)}, true, "1", SettingsReport{})
	if p, src := ResolvePassive(c, "ns", "m", nil); !p || src != PassiveSourceSettings {
		t.Errorf("no policy: passive %t from %s", p, src)
	}
	c.UpdateScalingPolicyConfigForNamespace("tenant", ScalingPolicySet{GlobalDefaultsKey: mustPolicy(t, "kvCacheThreshold: 0.7\n")})
	if p, src := ResolvePassive(c, "tenant", "m", nil); !p || src != PassiveSourceSettings {
		t.Errorf("namespace map without passive: %t from %s, want the settings", p, src)
	}
	c.UpdateScalingPolicyConfigForNamespace("tenant", ScalingPolicySet{GlobalDefaultsKey: mustPolicy(t, "passive: false\n")})
	if p, src := ResolvePassive(c, "tenant", "m", nil); p || src != PassiveSourceNamespaceDefault {
		t.Errorf("namespace map with passive: %t from %s, want its own false", p, src)
	}
}

// With a separate policy namespace, the limiter and optimizer settings do not
// move the cluster's limiters; passive still applies.
func TestSettingsBudgetIgnoredWithPolicyNamespace(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "tenant-ns")
	c := NewTestConfig()
	c.SetPolicyNamespace(WellKnownPolicyNamespace, PolicySourceWellKnown)
	c.UpdateClusterPolicy(&ScalingPolicy{})
	s, _ := ParseSettings(map[string]string{SettingPassive: "true", SettingUtilizationShare: "active",
		SettingLimiter: "quota", SettingQuotas: "H200=100", SettingQuotaScope: "cluster"}, "default")
	c.ApplySettings(s, true, "1", SettingsReport{})
	if c.SettingsBudgetApplies() {
		t.Fatal("budget settings apply beside a separate policy namespace")
	}
	if got := c.EffectiveLimiterSummary(); got != "none" {
		t.Errorf("limiter %s, want none: a tenant raised its own quota", got)
	}
	if mode, _ := c.UtilizationShareMode(); mode != UtilizationShareSettingOff {
		t.Errorf("mode %s, want off", mode)
	}
	if !c.PassiveDefault() {
		t.Error("passive did not apply")
	}
	if src := c.SettingsSources()[SettingLimiter]; src != SettingSourcePolicyNamespace {
		t.Errorf("limiter source %s", src)
	}
}

// writeConfigYAML writes a config.yaml for the loader and returns its path.
func writeConfigYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Restart keys: settings > flag > env > config.yaml > default, and a missing
// key falls back to config.yaml for one release.
func TestLoadWithSettingsPrecedence(t *testing.T) {
	for _, k := range []string{"PROMETHEUS_BASE_URL", "LLMSM_SCALE_TO_ZERO", "WATCH_NAMESPACE", "LLMSM_PASSIVE"} {
		t.Setenv(k, "")
	}
	file := writeConfigYAML(t, "PROMETHEUS_BASE_URL: https://from-file:9090\nLLMSM_SCALE_TO_ZERO: \"true\"\n"+
		"LLMSM_PASSIVE: \"true\"\nGLOBAL_OPT_INTERVAL: 1m0s\nPROMETHEUS_METRICS_CACHE_TTL: 30s\n")

	t.Run("no settings: config.yaml", func(t *testing.T) {
		c, err := LoadWithSettings(nil, file, nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.PrometheusBaseURL() != "https://from-file:9090" || !c.ScaleToZeroEnabled() || !c.PassiveDefault() {
			t.Errorf("url %s scaleToZero %t passive %t", c.PrometheusBaseURL(), c.ScaleToZeroEnabled(), c.PassiveDefault())
		}
		if c.SettingsSource() != SettingsSourceManagerConfig {
			t.Errorf("source %s", c.SettingsSource())
		}
		// The cache TTL equals the default, so deleting config.yaml would not change it.
		want := []string{"GLOBAL_OPT_INTERVAL", "LLMSM_PASSIVE", "LLMSM_SCALE_TO_ZERO", "PROMETHEUS_BASE_URL"}
		if got := c.SettingsFromManagerConfig(); !slices.Equal(got, want) {
			t.Errorf("from manager-config %v, want %v", got, want)
		}
		src := c.SettingsSources()
		if src[SettingPrometheusURL] != SettingSourceManagerConfig || src[SettingWatchNamespaces] != SettingSourceDefault ||
			src[SettingPassive] != SettingSourceManagerConfig {
			t.Errorf("sources %v", src)
		}
	})

	t.Run("settings win over config.yaml and fall back key by key", func(t *testing.T) {
		c, err := LoadWithSettings(nil, file, map[string]string{SettingPrometheusURL: "https://from-settings:9090"})
		if err != nil {
			t.Fatal(err)
		}
		if c.PrometheusBaseURL() != "https://from-settings:9090" {
			t.Errorf("url %s", c.PrometheusBaseURL())
		}
		if !c.ScaleToZeroEnabled() {
			t.Error("scaleToZero unset in settings did not fall back to config.yaml")
		}
		if c.SettingsSource() != SettingsSourceBoth {
			t.Errorf("source %s", c.SettingsSource())
		}
		// The live passive setting takes LLMSM_PASSIVE off the list.
		c.ApplySettings(Settings{Passive: ptr(false)}, true, "1", SettingsReport{})
		if c.PassiveDefault() || slices.Contains(c.SettingsFromManagerConfig(), "LLMSM_PASSIVE") {
			t.Error("the passive setting did not replace config.yaml's")
		}
	})

	t.Run("settings that cover config.yaml: settings only", func(t *testing.T) {
		c, err := LoadWithSettings(nil, file, map[string]string{SettingPrometheusURL: "https://p:9090",
			SettingScaleToZero: "false", "GLOBAL_OPT_INTERVAL": "60s"})
		if err != nil {
			t.Fatal(err)
		}
		c.ApplySettings(Settings{Passive: ptr(true)}, true, "1", SettingsReport{})
		if c.ScaleToZeroEnabled() || c.SettingsSource() != SettingsSourceSettingsOnly {
			t.Errorf("scaleToZero %t source %s (%v)", c.ScaleToZeroEnabled(), c.SettingsSource(), c.SettingsFromManagerConfig())
		}
	})

	t.Run("settings win over the environment and narrow a flag", func(t *testing.T) {
		t.Setenv("LLMSM_SCALE_TO_ZERO", "true")
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.String("watch-namespace", "", "")
		if err := fs.Parse([]string{"--watch-namespace=from-flag,from-settings"}); err != nil {
			t.Fatal(err)
		}
		c, err := LoadWithSettings(fs, file, map[string]string{SettingWatchNamespaces: "from-settings",
			SettingScaleToZero: "false"})
		if err != nil {
			t.Fatal(err)
		}
		if c.WatchNamespace() != "from-settings" || c.ScaleToZeroEnabled() {
			t.Errorf("watch %s scaleToZero %t", c.WatchNamespace(), c.ScaleToZeroEnabled())
		}
		c2, err := LoadWithSettings(fs, file, map[string]string{})
		if err != nil {
			t.Fatal(err)
		}
		src := c2.SettingsSources()
		if c2.WatchNamespace() != "from-flag,from-settings" || src[SettingWatchNamespaces] != SettingSourceFlag ||
			!c2.ScaleToZeroEnabled() || src[SettingScaleToZero] != SettingSourceEnv {
			t.Errorf("without settings: watch %s scaleToZero %t sources %v", c2.WatchNamespace(), c2.ScaleToZeroEnabled(), src)
		}
	})

	t.Run("an invalid setting falls back and a missing config.yaml is no error", func(t *testing.T) {
		c, err := LoadWithSettings(nil, filepath.Join(t.TempDir(), "absent.yaml"),
			map[string]string{SettingPrometheusURL: "https://p:9090", SettingScaleToZero: "perhaps"})
		if err != nil {
			t.Fatal(err)
		}
		if c.ScaleToZeroEnabled() {
			t.Error("an invalid scaleToZero turned scale-to-zero on; the default is off")
		}
		if c.SettingsSource() != SettingsSourceSettingsOnly {
			t.Errorf("source %s", c.SettingsSource())
		}
	})

	t.Run("no Prometheus URL anywhere names the setting", func(t *testing.T) {
		_, err := LoadWithSettings(nil, "", map[string]string{})
		if err == nil || !strings.Contains(err.Error(), SettingPrometheusURL) {
			t.Errorf("error %v, want one naming %s", err, SettingPrometheusURL)
		}
	})
}

// Scale-to-zero is off by default (decision 8).
func TestScaleToZeroDefaultOff(t *testing.T) {
	t.Setenv("LLMSM_SCALE_TO_ZERO", "")
	c, err := LoadWithSettings(nil, "", map[string]string{SettingPrometheusURL: "https://p:9090"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ScaleToZeroEnabled() || c.SettingsSources()[SettingScaleToZero] != SettingSourceDefault {
		t.Errorf("scaleToZero %t from %s, want off by default", c.ScaleToZeroEnabled(), c.SettingsSources()[SettingScaleToZero])
	}
}

// A restart key that changes is reported; the same effective value is not.
func TestSettingsRestartChanged(t *testing.T) {
	t.Setenv("PROMETHEUS_BASE_URL", "")
	file := writeConfigYAML(t, "PROMETHEUS_BASE_URL: https://a:9090\n")
	c, err := LoadWithSettings(nil, file, map[string]string{SettingLogVerbosity: "3"})
	if err != nil {
		t.Fatal(err)
	}
	same, _ := ParseSettings(map[string]string{SettingLogVerbosity: "3", SettingPrometheusURL: "https://a:9090"}, "default")
	if got := c.SettingsRestartChanged(same); len(got) != 0 {
		t.Errorf("changed %v for the values in force", got)
	}
	moved, _ := ParseSettings(map[string]string{SettingPrometheusURL: "https://b:9090", "GLOBAL_OPT_INTERVAL": "30s"}, "default")
	if got, want := c.SettingsRestartChanged(moved), []string{"GLOBAL_OPT_INTERVAL", SettingLogVerbosity, SettingPrometheusURL}; !slices.Equal(got, want) {
		t.Errorf("changed %v, want %v", got, want)
	}
}
