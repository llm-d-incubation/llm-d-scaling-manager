package config

import (
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	ctrl "sigs.k8s.io/controller-runtime"
)

// capturedLog collects what ctrl.Log prints. controller-runtime takes its
// root logger once per process, so the sink is installed once and each test
// reads only what it logged after reset.
var capturedLog = struct {
	once  sync.Once
	mu    sync.Mutex
	lines []string
}{}

func captureCtrlLog(t *testing.T) func() []string {
	t.Helper()
	capturedLog.once.Do(func() {
		ctrl.SetLogger(funcr.New(func(prefix, args string) {
			capturedLog.mu.Lock()
			defer capturedLog.mu.Unlock()
			capturedLog.lines = append(capturedLog.lines, prefix+" "+args)
		}, funcr.Options{}))
	})
	capturedLog.mu.Lock()
	capturedLog.lines = nil
	capturedLog.mu.Unlock()
	return func() []string {
		capturedLog.mu.Lock()
		defer capturedLog.mu.Unlock()
		return append([]string(nil), capturedLog.lines...)
	}
}

// horizonErrors are the "Unusable learned-state horizon" lines that name key.
func horizonErrors(lines []string, key string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, "Unusable learned-state horizon") && strings.Contains(l, key) {
			out = append(out, l)
		}
	}
	return out
}

// Issue #319 removed the startup error for an UNSET horizon. A value that was
// set and that viper reads as 0 -- one it cannot parse, or a deliberate 0 --
// must still be reported: past the loader it is indistinguishable from unset,
// and the startup line then prints the default exactly as an unconfigured
// controller does, so an operator who wrote 30d reads their typo as accepted.
func TestLoad_ASetHorizonThatReadsAsZeroIsReported(t *testing.T) {
	const key = "LEARNED_STATE_BUCKET_RETENTION"
	for _, raw := range []string{"30d", "7d", "abc", " 24h", "0", "0s"} {
		t.Run(raw, func(t *testing.T) {
			logged := captureCtrlLog(t)
			t.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
			t.Setenv(key, raw)

			cfg, err := Load(nil, "")
			if err != nil {
				t.Fatalf("Load() failed: %v", err)
			}
			if _, _, bucket := cfg.LearnedStateHorizons(); bucket != 0 {
				t.Errorf("want 0 (the default downstream), got %v", bucket)
			}
			if got := horizonErrors(logged(), key); len(got) != 1 {
				t.Errorf("%s=%q: want one Unusable learned-state horizon error naming it, got %d: %v",
					key, raw, len(got), logged())
			}
		})
	}
}

func TestLoad_UnsetOrUsableHorizonIsNotReported(t *testing.T) {
	keys := []string{"LEARNED_STATE_EPISODE_GAP", "LEARNED_STATE_VARIANT_TIMEOUT", "LEARNED_STATE_BUCKET_RETENTION"}
	t.Run("unset", func(t *testing.T) {
		logged := captureCtrlLog(t)
		t.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
		if _, err := Load(nil, ""); err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		for _, k := range keys {
			if got := horizonErrors(logged(), k); len(got) != 0 {
				t.Errorf("unset %s was reported: %v", k, got)
			}
		}
	})
	t.Run("usable, and below the floor (named later by Sanitized, not here)", func(t *testing.T) {
		logged := captureCtrlLog(t)
		t.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
		t.Setenv("LEARNED_STATE_EPISODE_GAP", "90m")
		t.Setenv("LEARNED_STATE_VARIANT_TIMEOUT", "24")
		if _, err := Load(nil, ""); err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		for _, k := range keys {
			if got := horizonErrors(logged(), k); len(got) != 0 {
				t.Errorf("%s was reported by the loader: %v", k, got)
			}
		}
	})
}
