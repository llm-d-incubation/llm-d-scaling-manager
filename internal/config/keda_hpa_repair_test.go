package config

import "testing"

// The KEDA HPA repair ships off; KEDA_HPA_REPAIR=true turns it on from the
// environment, and the settings ConfigMap takes the same key, true or false.
func TestKEDAHPARepairFlag(t *testing.T) {
	t.Run("off when unset", func(t *testing.T) {
		t.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
		cfg, err := Load(nil, "")
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		if cfg.KEDAHPARepairEnabled() {
			t.Error("the repair is on with KEDA_HPA_REPAIR unset; it ships off")
		}
	})

	t.Run("on from the environment", func(t *testing.T) {
		t.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
		t.Setenv("KEDA_HPA_REPAIR", "true")
		cfg, err := Load(nil, "")
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		if !cfg.KEDAHPARepairEnabled() {
			t.Error("KEDA_HPA_REPAIR=true was not read")
		}
	})

	t.Run("settings ConfigMap", func(t *testing.T) {
		s, rep := ParseSettings(map[string]string{"KEDA_HPA_REPAIR": "true"}, "default")
		if len(rep.Rejected) != 0 || len(rep.Unknown) != 0 {
			t.Fatalf("rejected %v unknown %v, want none", rep.Rejected, rep.Unknown)
		}
		if s.Restart["KEDA_HPA_REPAIR"] != "true" {
			t.Errorf("restart %v, want KEDA_HPA_REPAIR=true", s.Restart)
		}
		_, rep = ParseSettings(map[string]string{"KEDA_HPA_REPAIR": "sometimes"}, "default")
		if _, bad := rep.Rejected["KEDA_HPA_REPAIR"]; !bad {
			t.Errorf("rejected %v, want KEDA_HPA_REPAIR=sometimes refused", rep.Rejected)
		}
	})
}
