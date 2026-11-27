package config

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

// A controller waiting for its settings starts once prometheusURL is set and
// the CA file the settings name is mounted; the CA Secret is mounted some
// seconds after it is created, so its absence is waited for, not fatal.
func TestReadyToStart(t *testing.T) {
	t.Setenv("PROMETHEUS_BASE_URL", "")
	mounted := map[string]bool{}
	statFile = func(p string) (os.FileInfo, error) {
		if mounted[p] {
			return nil, nil
		}
		return nil, fs.ErrNotExist
	}
	t.Cleanup(func() { statFile = os.Stat })
	cases := []struct {
		name string
		data map[string]string
		ok   bool
		why  string
	}{
		{"no settings", nil, false, "prometheusURL is not set"},
		{"url, no CA", map[string]string{SettingPrometheusURL: "https://p:9090"}, true, ""},
		{"caFromSecret, not mounted", map[string]string{SettingPrometheusURL: "https://p:9090", SettingCAFromSecret: "true"}, false, "is not there yet"},
		{"CA path, not mounted", map[string]string{SettingPrometheusURL: "https://p:9090", SettingPrometheusCACertPath: "/etc/x/ca.crt"}, false, "/etc/x/ca.crt"},
	}
	for _, c := range cases {
		ok, why := ReadyToStart(c.data)
		if ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("%s: %v %q, want %v containing %q", c.name, ok, why, c.ok, c.why)
		}
	}
	mounted[PrometheusCAFromSecretPath] = true
	if ok, why := ReadyToStart(map[string]string{SettingPrometheusURL: "https://p:9090", SettingCAFromSecret: "true"}); !ok {
		t.Errorf("mounted CA: not ready: %s", why)
	}
}
