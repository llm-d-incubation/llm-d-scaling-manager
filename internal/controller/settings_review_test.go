package controller

import (
	"context"
	"strings"
	"testing"

	flag "github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/controllerstatus"
)

// At start a limiter declared with a value that cannot be used stops the
// controller, and a passive that cannot be read makes it passive.
func TestSettingsBootstrapFailsClosed(t *testing.T) {
	h := newSettingsHarness(t, config.NewTestConfig(), nil, settingsConfigMap(map[string]string{
		config.SettingLimiter: "quota", config.SettingQuotas: "H200:8",
	}))
	err := h.r.bootstrapSettings(h.ctx)
	if err == nil || !strings.Contains(err.Error(), "refusing to start unbounded") {
		t.Fatalf("bootstrap error %v, want a refusal to start unbounded", err)
	}
	if got := h.r.Config.EffectiveLimiterSummary(); got != "none" {
		t.Errorf("limiter %s", got)
	}

	cfg := config.NewTestConfig()
	h = newSettingsHarness(t, cfg, nil, settingsConfigMap(map[string]string{config.SettingPassive: "yes"}))
	if err := h.r.bootstrapSettings(h.ctx); err != nil {
		t.Fatal(err)
	}
	if !cfg.PassiveDefault() {
		t.Error(`passive: "yes" started the controller active`)
	}
	h.want("passive typo", map[string]string{controllerstatus.KeyPassiveDefault: "true"})
	if !strings.Contains(h.status.Data()[controllerstatus.KeySettingsRejected], config.SettingPassive) {
		t.Errorf("status %v", h.status.Data())
	}
}

// Whether the controller manages its own namespace, and so where its limits
// come from, is decided from the Deployment's --watch-namespace: narrowing it
// to the controller's own namespace with the setting does not make the
// controller self-managed.
func TestPolicyNamespaceIgnoresWatchNamespacesSetting(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "team-a")
	t.Setenv("WATCH_NAMESPACE", "")
	t.Setenv("PROMETHEUS_BASE_URL", "https://p:9090")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("watch-namespace", "", "")
	if err := fs.Parse([]string{"--watch-namespace=team-a,team-b"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadWithSettings(fs, "", map[string]string{config.SettingWatchNamespaces: "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WatchNamespace() != "team-a" {
		t.Fatalf("watch %q, want the narrowed team-a", cfg.WatchNamespace())
	}
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: config.WellKnownPolicyNamespace}},
	).Build()
	if err := ResolvePolicyNamespace(context.Background(), c, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.PolicySource() != config.PolicySourceLocal || cfg.PolicyNamespaceIsSeparate() {
		t.Errorf("policy source %s namespace %s: the setting made the controller self-managed",
			cfg.PolicySource(), cfg.PolicyNamespace())
	}
}
