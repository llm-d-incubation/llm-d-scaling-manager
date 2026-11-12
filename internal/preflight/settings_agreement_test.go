package preflight

import (
	"maps"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// The settings check and the controller agree on every value: preflight fails
// exactly the settings the controller's parser, config.ParseSettings, rejects.
// Each case is the shipped settings with the keys below changed, fed to both.
func TestSettingsAgreeWithController(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
	}{
		{"shipped", nil},
		{"passive false", map[string]string{keyPassive: "false"}},
		{"passive yes", map[string]string{keyPassive: "yes"}},
		{"utilizationShare on", map[string]string{keyUtilizationShare: "on"}},
		{"share with gpu-inventory", map[string]string{keyUtilizationShare: "shadow", keyLimiter: "gpu-inventory"}},
		{"limiter gpu-inventory", map[string]string{keyLimiter: "gpu-inventory"}},
		{"limiter inventory", map[string]string{keyLimiter: "inventory"}},
		{"limiter gpu", map[string]string{keyLimiter: "gpu"}},
		{"limiter both", map[string]string{keyLimiter: "gpu-inventory,quota", keyQuotas: "H200=8"}},
		{"quota", map[string]string{keyLimiter: "quota", keyQuotas: "H200=8"}},
		{"quota, spaces and no cap", map[string]string{keyLimiter: "quota", keyQuotas: "H200=8 A100=-1"}},
		{"quota cluster scope", map[string]string{keyLimiter: "quota", keyQuotas: "H200=8", keyQuotaScope: "cluster"}},
		{"quota no budget", map[string]string{keyLimiter: "quota"}},
		{"quota, kueue, no budget", map[string]string{keyLimiter: "quota", keyKueue: "true"}},
		{"quota not a number", map[string]string{keyLimiter: "quota", keyQuotas: "H200=eight"}},
		{"quota below -1", map[string]string{keyLimiter: "quota", keyQuotas: "H200=-2"}},
		{"quota over the maximum", map[string]string{keyLimiter: "quota", keyQuotas: "H200=2000000"}},
		{"quota named twice", map[string]string{keyLimiter: "quota", keyQuotas: "H200=8,H200=4"}},
		{"quota no name", map[string]string{keyLimiter: "quota", keyQuotas: "=8"}},
		{"bad quotas, no limiter", map[string]string{keyQuotas: "H200=eight"}},
		{"quotaScope global", map[string]string{keyQuotaScope: "global"}},
		{"kueue maybe", map[string]string{keyKueue: "maybe"}},
		{"kueueBorrow nah", map[string]string{keyKueueBorrow: "nah"}},
		{"kueueNamespace bad", map[string]string{keyKueueNamespace: "Kueue_System"}},
		{"watchNamespaces two", map[string]string{keyWatchNamespaces: testNS + ",team-b"}},
		{"watchNamespaces bad", map[string]string{keyWatchNamespaces: "Team_B"}},
		{"logVerbosity 0", map[string]string{keyLogVerbosity: "0"}},
		{"logVerbosity 10", map[string]string{keyLogVerbosity: "10"}},
		{"logVerbosity 11", map[string]string{keyLogVerbosity: "11"}},
		{"logVerbosity -1", map[string]string{keyLogVerbosity: "-1"}},
		{"prometheusURL ftp", map[string]string{keyPrometheusURL: "ftp://prometheus.monitoring.svc:9090"}},
		{"prometheusURL no host", map[string]string{keyPrometheusURL: "https://"}},
		{"insecure 1", map[string]string{keyPrometheusInsecure: "1"}},
		{"insecure yes", map[string]string{keyPrometheusInsecure: "yes"}},
		{"scaleToZero yes", map[string]string{keyScaleToZero: "yes"}},
		{"CA path relative", map[string]string{"prometheusCACertPath": "ca.crt"}},
		{"server name with a port", map[string]string{"prometheusServerName": "prometheus:9090"}},
		{"advanced duration", map[string]string{"GLOBAL_OPT_INTERVAL": "30s"}},
		{"advanced duration without a unit", map[string]string{"GLOBAL_OPT_INTERVAL": "30"}},
		{"advanced bool", map[string]string{"LEARNED_STATE_SNAPSHOT": "maybe"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			maps.Copy(f.settings, tc.set)
			_, rep := config.ParseSettings(maps.Clone(f.settings), testNS)
			controllerRejects := len(rep.Rejected) > 0
			r := only(t, f.run(t), "settings")
			if preflightFails := r.Status == Fail; preflightFails != controllerRejects {
				t.Errorf("preflight %s %q; controller rejects %v", r.Status, r.Detail, rep.Rejected)
			}
		})
	}
}
