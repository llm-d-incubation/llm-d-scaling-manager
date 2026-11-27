package controller

import (
	"slices"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// With RestartOnSettingsChange set (--restart-on-settings-change), a changed
// restart key -- here prometheusURL, edited after the controller started --
// calls it once with the key, and a live key never does. Without it, the
// change is only reported, as before.
func TestSettingsRestartKeyRestartsItself(t *testing.T) {
	for _, wired := range []bool{true, false} {
		t.Run(map[bool]string{true: "wired", false: "not wired"}[wired], func(t *testing.T) {
			t.Setenv("PROMETHEUS_BASE_URL", "")
			cfg, err := config.LoadWithSettings(nil, "", map[string]string{config.SettingPrometheusURL: "https://a:9090"})
			if err != nil {
				t.Fatal(err)
			}
			h := newSettingsHarness(t, cfg, nil, settingsConfigMap(map[string]string{config.SettingPrometheusURL: "https://a:9090", config.SettingPassive: "true"}))
			var calls [][]string
			if wired {
				h.r.RestartOnSettingsChange = func(changed []string) { calls = append(calls, changed) }
			}
			if err := h.r.bootstrapSettings(h.ctx); err != nil {
				t.Fatal(err)
			}
			h.patch(map[string]string{config.SettingPassive: "false"})
			if len(calls) != 0 {
				t.Fatalf("a live key restarted the controller: %v", calls)
			}
			h.patch(map[string]string{config.SettingPrometheusURL: "https://b:9090"})
			h.patch(map[string]string{config.SettingPassive: "true"})
			if !wired {
				if len(calls) != 0 {
					t.Fatalf("restarted without the switch: %v", calls)
				}
				return
			}
			if len(calls) != 1 || !slices.Contains(calls[0], config.SettingPrometheusURL) {
				t.Fatalf("restart calls %v, want one naming %s", calls, config.SettingPrometheusURL)
			}
		})
	}
}
