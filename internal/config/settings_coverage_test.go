package config

import (
	"reflect"
	"strings"
	"testing"
)

// With a separate policy namespace the budget settings never apply, whether
// or not its cluster policy has been read yet; and before the policy namespace
// is known they wait for it, then apply when it is the controller's own.
func TestSettingsBudgetGuardWithoutClusterPolicy(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "tenant-ns")
	tenantQuota := map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=100", SettingQuotaScope: "cluster",
		SettingUtilizationShare: "active"}

	c := NewTestConfig()
	c.SetPolicyNamespace(WellKnownPolicyNamespace, PolicySourceWellKnown)
	s, _ := ParseSettings(tenantQuota, "default")
	c.ApplySettings(s, true, "1", SettingsReport{})
	if got := c.EffectiveLimiterSummary(); got != string(LimiterTypeNone) {
		t.Errorf("separate, no cluster policy read: limiter %s, want none", got)
	}

	// Settings applied before the policy namespace is resolved.
	c = NewTestConfig()
	c.ApplySettings(s, true, "1", SettingsReport{})
	if got := c.EffectiveLimiterSummary(); got != string(LimiterTypeNone) || c.SettingsBudgetApplies() {
		t.Errorf("unresolved: limiter %s, want none until the policy namespace is known", got)
	}
	if src := c.SettingsSources()[SettingLimiter]; src != SettingSourceDefault {
		t.Errorf("unresolved: limiter source %s", src)
	}
	c.SetPolicyNamespace(WellKnownPolicyNamespace, PolicySourceWellKnown)
	if got := c.EffectiveLimiterSummary(); got != string(LimiterTypeNone) {
		t.Errorf("resolved separate: limiter %s", got)
	}
	if mode, _ := c.UtilizationShareMode(); mode != UtilizationShareSettingOff {
		t.Errorf("resolved separate: mode %s", mode)
	}

	// Resolved to the controller's own namespace: SetPolicyNamespace refolds.
	c = NewTestConfig()
	c.ApplySettings(s, true, "1", SettingsReport{})
	c.SetPolicyNamespace(SystemNamespace(), PolicySourceLocal)
	if got := c.EffectiveLimiterSummary(); got != "quota/"+SettingsQuotaName {
		t.Errorf("resolved local: limiter %s, want the settings' quota", got)
	}
}

// The quota key: the one watched namespace, else default (several or all).
func TestSettingsQuotaKey(t *testing.T) {
	for ns, want := range map[string]string{"": "default", "team-a": "team-a", "team-a,team-b": "default", ",team-a,,": "team-a"} {
		c := NewTestConfig()
		SetWatchNamespaceForTest(c, ns)
		if got := c.SettingsQuotaKey(); got != want {
			t.Errorf("watch %q: key %q, want %q", ns, got, want)
		}
	}
}

func TestParseQuotaSettingTable(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]int
		err  string
	}{
		{in: "H200=8,A100=4", want: map[string]int{"H200": 8, "A100": 4}},
		{in: "H200=8 A100=-1", want: map[string]int{"H200": 8, "A100": -1}},
		{in: "H200=1048576", want: map[string]int{"H200": MaxQuotaValue}},
		{in: "H200=0", want: map[string]int{"H200": 0}},
		{in: "H200=1048577", err: "exceeds the maximum"},
		{in: "H200=-2", err: "whole number"},
		{in: "H200=1.5", err: "whole number"},
		{in: "H200=", err: "not TYPE=N"},
		{in: "=8", err: "not TYPE=N"},
		{in: "H200:8", err: "not TYPE=N"},
		{in: "H200=8,H200=4", err: "named twice"},
		{in: " , ", err: "names no accelerator"},
		{in: "", err: "names no accelerator"},
	}
	for _, tc := range cases {
		got, err := ParseQuotaSetting(tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%q: error %v, want %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: %v %v, want %v", tc.in, got, err, tc.want)
		}
	}
}

func TestParseLimiterSettingsValidation(t *testing.T) {
	// kueueBorrow unset: borrowing is the controller default (no countBorrowable).
	s, rep := ParseSettings(map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=8", SettingKueue: "true"}, "team-a")
	if len(rep.Rejected) != 0 || len(s.Limiters) != 1 || s.Limiters[0].Kueue == nil ||
		s.Limiters[0].Kueue.CountBorrowable != nil || s.Limiters[0].Kueue.ControllerNamespace != "" {
		t.Errorf("kueue default: %+v %v", s.Limiters, rep.Rejected)
	}
	cases := []struct {
		name string
		data map[string]string
		key  string
		bad  string
		why  string
	}{
		{name: "kueueNamespace not a name", data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=8",
			SettingKueue: "true", SettingKueueNamespace: "Kueue_NS"}, key: "default", bad: SettingKueueNamespace, why: "not a namespace name"},
		{name: "kueueBorrow not a bool", data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=8",
			SettingKueue: "true", SettingKueueBorrow: "sometimes"}, key: "default", bad: SettingKueueBorrow, why: "not true or false"},
		{name: "quota below -1", data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=-5"},
			key: "default", bad: SettingQuotas, why: "whole number"},
		{name: "empty quotas", data: map[string]string{SettingLimiter: "quota", SettingQuotas: " , "},
			key: "default", bad: SettingQuotas, why: "names no accelerator"},
		{name: "the built entry fails validation", data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=8"},
			key: " ", bad: SettingLimiter, why: "limiters these keys build are invalid"},
		{name: "no namespace to key on", data: map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=8"},
			key: "", bad: SettingQuotaScope, why: "needs the namespace to key on"},
	}
	for _, tc := range cases {
		s, rep := ParseSettings(tc.data, tc.key)
		if s.Limiters != nil || !strings.Contains(rep.Rejected[tc.bad], tc.why) {
			t.Errorf("%s: limiters %+v rejected %v, want %s: ...%s", tc.name, s.Limiters, rep.Rejected, tc.bad, tc.why)
		}
	}
}

// Keys the settings leave unset report where the policy gave them.
func TestSettingsSourcesFromPolicy(t *testing.T) {
	c := precedenceConfig(t)
	src := c.SettingsSources()
	for _, k := range []string{SettingPassive, SettingUtilizationShare, SettingLimiter, SettingKueue} {
		if src[k] != SettingSourcePolicy {
			t.Errorf("%s from %s, want policy", k, src[k])
		}
	}
	c = NewTestConfig()
	c.SetPolicyNamespace(SystemNamespace(), PolicySourceLocal)
	src = c.SettingsSources()
	for _, k := range []string{SettingPassive, SettingUtilizationShare, SettingLimiter} {
		if src[k] != SettingSourceDefault {
			t.Errorf("no policy: %s from %s, want default", k, src[k])
		}
	}
}

// A rejected advanced key and a rejected kueue key keep what was in force.
func TestSettingsKeepRejectedAdvancedAndKueue(t *testing.T) {
	prev := Settings{Restart: map[string]string{"GLOBAL_OPT_INTERVAL": "30s"},
		Limiters: []QuotaLimiterConfig{{Name: SettingsQuotaName, Type: "quota", Scope: QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H200": 8}}}}
	next, rep := ParseSettings(map[string]string{"GLOBAL_OPT_INTERVAL": "soon", SettingLimiter: "quota",
		SettingQuotas: "H200=16", SettingQuotaScope: "cluster", SettingKueue: "true", SettingKueueBorrow: "maybe"}, "default")
	got := next.KeepRejected(prev, rep.Rejected)
	if got.Restart["GLOBAL_OPT_INTERVAL"] != "30s" {
		t.Errorf("advanced key %v, want the 30s in force", got.Restart)
	}
	if !reflect.DeepEqual(got.Limiters, prev.Limiters) {
		t.Errorf("limiters %+v, want the H200=8 in force: a bad kueueBorrow rejects the group", got.Limiters)
	}
}

// A config.yaml or installer name for a restart key gets a hint, as a live
// one does.
func TestUnknownSettingHintRestartKeys(t *testing.T) {
	for key, want := range map[string]string{
		"PROMETHEUS_BASE_URL":     SettingPrometheusURL,
		"PROMETHEUS_CA_CERT_PATH": SettingPrometheusCACertPath,
		"LLMSM_WATCH_NS":          SettingWatchNamespaces,
		"prometheusurl":           "case-sensitive",
	} {
		if h := UnknownSettingHint(key); !strings.Contains(h, want) {
			t.Errorf("%s: hint %q, want %q", key, h, want)
		}
	}
}

// Each limiter key, kueueNamespace last among them, rejects the whole group.
func TestSettingsKeepRejectedEveryLimiterKey(t *testing.T) {
	prev := Settings{Limiters: []QuotaLimiterConfig{{Type: "gpu-inventory"}}}
	base := map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=8", SettingQuotaScope: "cluster", SettingKueue: "true"}
	bad := map[string]string{SettingLimiter: "quotas", SettingQuotas: "H200", SettingQuotaScope: "global",
		SettingKueue: "y", SettingKueueBorrow: "y", SettingKueueNamespace: "Kueue_NS"}
	for _, k := range limiterSettingKeys {
		data := map[string]string{}
		for bk, bv := range base {
			data[bk] = bv
		}
		data[k] = bad[k]
		next, rep := ParseSettings(data, "default")
		if _, ok := rep.Rejected[k]; !ok {
			t.Fatalf("%s=%q not rejected: %v", k, bad[k], rep.Rejected)
		}
		if got := next.KeepRejected(prev, rep.Rejected); !reflect.DeepEqual(got.Limiters, prev.Limiters) {
			t.Errorf("%s rejected: limiters %+v, want the ones in force", k, got.Limiters)
		}
	}
}

// What LiveSettings returns and what ApplySettings took are copies: a caller
// changing them does not change the settings in force.
func TestSettingsAreCopied(t *testing.T) {
	c := NewTestConfig()
	c.SetPolicyNamespace(SystemNamespace(), PolicySourceLocal)
	s, _ := ParseSettings(map[string]string{SettingPassive: "true", SettingLimiter: "quota", SettingQuotas: "H200=8",
		SettingQuotaScope: "cluster", SettingLogVerbosity: "3"}, "default")
	c.ApplySettings(s, true, "1", SettingsReport{})
	*s.Passive = false
	s.Limiters[0].ClusterQuotas["H200"] = 99
	s.Restart["V"] = "9"
	got := c.LiveSettings()
	*got.Passive = false
	got.Limiters[0].ClusterQuotas["H200"] = 77
	got.Restart["V"] = "8"
	again := c.LiveSettings()
	if !*again.Passive || again.Limiters[0].ClusterQuotas["H200"] != 8 || again.Restart["V"] != "3" || !c.PassiveDefault() {
		t.Errorf("settings in force changed through a copy: %+v", again)
	}
}

// Only a real disagreement with the policy is reported: an equal value, or a
// field the policy does not set, is not.
func TestSettingsOverridePolicyOnlyDisagreements(t *testing.T) {
	on := true
	s := Settings{Passive: &on, UtilizationShare: UtilizationShareSettingActive}
	if got := SettingsOverridePolicy(ScalingPolicy{Passive: &on}, s, true); len(got) != 0 {
		t.Errorf("equal passive reported: %q", got)
	}
	if got := SettingsOverridePolicy(ScalingPolicy{}, s, true); len(got) != 0 {
		t.Errorf("fields the policy does not set reported: %q", got)
	}
	off := false
	if got := SettingsOverridePolicy(ScalingPolicy{Passive: &off}, s, true); len(got) != 1 {
		t.Errorf("a different passive: %q", got)
	}
}

// A settings limiter list equal to the policy's is no disagreement.
func TestSettingsOverridePolicyEqualLimiters(t *testing.T) {
	s, _ := ParseSettings(map[string]string{SettingLimiter: "quota", SettingQuotas: "H200=8", SettingQuotaScope: "cluster"}, "default")
	def := ScalingPolicy{Limiters: s.clone().Limiters}
	if got := SettingsOverridePolicy(def, s, true); len(got) != 0 {
		t.Errorf("equal limiters reported: %q", got)
	}
	def.Limiters[0].ClusterQuotas["H200"] = 4
	if got := SettingsOverridePolicy(def, s, true); len(got) != 1 {
		t.Errorf("different budgets: %q", got)
	}
}
