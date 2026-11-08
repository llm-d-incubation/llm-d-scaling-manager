package config

import "testing"

func TestLearnedStateSnapshotFlag(t *testing.T) {
	t.Run("off when unset", func(t *testing.T) {
		t.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
		cfg, err := Load(nil, "")
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		if cfg.LearnedStateSnapshotEnabled() {
			t.Error("the snapshot is on with LEARNED_STATE_SNAPSHOT unset; it ships off")
		}
	})

	t.Run("on from the environment", func(t *testing.T) {
		t.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
		t.Setenv("LEARNED_STATE_SNAPSHOT", "true")
		cfg, err := Load(nil, "")
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		if !cfg.LearnedStateSnapshotEnabled() {
			t.Error("LEARNED_STATE_SNAPSHOT=true was not read")
		}
	})
}
