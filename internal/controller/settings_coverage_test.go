package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/controllerstatus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/datastore"
)

// reqFor is the reconcile request for ConfigMap name in namespace.
func reqFor(namespace, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}

// The settings ConfigMap in the controller's namespace reaches the
// reconciler; one of the same name in a tenant namespace the controller does
// not track does not.
func TestSettingsPredicate(t *testing.T) {
	t.Setenv("POD_NAMESPACE", settingsTestNS)
	cfg := config.NewTestConfig()
	p := ConfigMapPredicate(datastore.NewDatastore(cfg), cfg)
	cm := func(ns string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: config.SettingsConfigMapName(), Namespace: ns}}
	}
	if !p.Create(event.CreateEvent{Object: cm(settingsTestNS)}) || !p.Update(event.UpdateEvent{ObjectOld: cm(settingsTestNS), ObjectNew: cm(settingsTestNS)}) ||
		!p.Delete(event.DeleteEvent{Object: cm(settingsTestNS)}) {
		t.Error("the settings ConfigMap in the controller's namespace is filtered out")
	}
	if p.Create(event.CreateEvent{Object: cm("tenant")}) {
		t.Error("a settings ConfigMap in an untracked tenant namespace passes the predicate")
	}
	other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "something-else", Namespace: settingsTestNS}}
	if p.Create(event.CreateEvent{Object: other}) {
		t.Error("an unrelated ConfigMap passes the predicate")
	}
}

// A ConfigMap named scaling-manager-settings in a namespace the controller
// watches, but not its own, neither sets nor resets the settings.
func TestSettingsInTenantNamespaceIgnored(t *testing.T) {
	cfg := config.NewTestConfig()
	config.SetWatchNamespaceForTest(cfg, "tenant")
	h := newSettingsHarness(t, cfg, nil, settingsConfigMap(map[string]string{config.SettingPassive: "true"}),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: config.SettingsConfigMapName(), Namespace: "tenant"},
			Data: map[string]string{config.SettingPassive: "false", config.SettingUtilizationShare: "active"}})
	h.reconcile(config.SettingsConfigMapName())
	if !cfg.PassiveDefault() {
		t.Fatal("the controller's own settings were not applied")
	}
	reconcileIn := func(ns string) {
		t.Helper()
		if _, err := h.r.Reconcile(h.ctx, reqFor(ns, config.SettingsConfigMapName())); err != nil {
			t.Fatal(err)
		}
	}
	reconcileIn("tenant")
	if !cfg.PassiveDefault() || cfg.LiveSettings().UtilizationShare != "" {
		t.Error("a tenant's settings ConfigMap changed the settings")
	}
	var tenant corev1.ConfigMap
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "tenant", Name: config.SettingsConfigMapName()}, &tenant); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(h.ctx, &tenant); err != nil {
		t.Fatal(err)
	}
	reconcileIn("tenant")
	if present, _ := cfg.SettingsPresent(); !present || !cfg.PassiveDefault() {
		t.Error("deleting a tenant's settings ConfigMap reset the settings")
	}
}

// No settings ConfigMap at start: no error, the fallback in force, and the
// status says where the settings come from.
func TestSettingsAbsentAtStart(t *testing.T) {
	t.Setenv("PROMETHEUS_BASE_URL", "")
	t.Setenv("LLMSM_PASSIVE", "")
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte("PROMETHEUS_BASE_URL: https://p:9090\nLLMSM_PASSIVE: \"true\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadWithSettings(nil, file, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newSettingsHarness(t, cfg, nil)
	cm, err := ReadSettings(h.ctx, h.c, settingsTestNS)
	if err != nil || cm != nil {
		t.Fatalf("ReadSettings %v %v, want nothing and no error", cm, err)
	}
	if err := h.r.bootstrapSettings(h.ctx); err != nil {
		t.Fatal(err)
	}
	if !cfg.PassiveDefault() {
		t.Error("the manager ConfigMap's LLMSM_PASSIVE did not apply")
	}
	h.want("absent", map[string]string{
		controllerstatus.KeySettingsSource:            config.SettingsSourceManagerConfig,
		controllerstatus.KeySettingsResourceVersion:   "",
		controllerstatus.KeyPassiveDefault:            "true",
		controllerstatus.KeyPassiveAtStart:            "true",
		controllerstatus.KeySettingsFromManagerConfig: "LLMSM_PASSIVE,PROMETHEUS_BASE_URL",
	})
	if src := h.status.Data()[controllerstatus.KeySettingsSources]; src == "" {
		t.Error("settingsSources not recorded")
	}
}

// Deleting the settings ConfigMap drops every setting: the mode, the
// limiters, the restart values, and the restart warning already given.
func TestSettingsDeletedResetsEverything(t *testing.T) {
	t.Setenv("PROMETHEUS_BASE_URL", "")
	cfg, err := config.LoadWithSettings(nil, "", map[string]string{config.SettingPrometheusURL: "https://a:9090"})
	if err != nil {
		t.Fatal(err)
	}
	h := newSettingsHarness(t, cfg, nil, settingsConfigMap(map[string]string{config.SettingPrometheusURL: "https://a:9090"}))
	if err := h.r.bootstrapSettings(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.patch(map[string]string{config.SettingPrometheusURL: "https://b:9090", config.SettingUtilizationShare: "shadow",
		config.SettingLimiter: "quota", config.SettingQuotas: "H200=8", config.SettingQuotaScope: "cluster", config.SettingPassive: "true"})
	if h.r.settingsRestartWarned == "" || cfg.LiveSettings().UtilizationShare != "shadow" {
		t.Fatalf("setup: warned %q settings %+v", h.r.settingsRestartWarned, cfg.LiveSettings())
	}
	var cm corev1.ConfigMap
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: settingsTestNS, Name: config.SettingsConfigMapName()}, &cm); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(h.ctx, &cm); err != nil {
		t.Fatal(err)
	}
	h.reconcile(config.SettingsConfigMapName())
	live := cfg.LiveSettings()
	if live.UtilizationShare != "" || live.Limiters != nil || live.Passive != nil || len(live.Restart) != 0 {
		t.Errorf("settings left after delete: %+v", live)
	}
	if h.r.settingsRestartWarned != "" {
		t.Errorf("restart warning %q kept after delete", h.r.settingsRestartWarned)
	}
	if present, rv := cfg.SettingsPresent(); present || rv != "" {
		t.Errorf("present %t rv %q after delete", present, rv)
	}
}

// A controller managing several namespaces keys a namespace-scoped quota from
// the settings on default, the per-namespace budget, never on the first one.
func TestSettingsQuotaKeyThroughReconciler(t *testing.T) {
	for watch, key := range map[string]string{"team-a": "team-a", "team-a,team-b": "default", "": "default"} {
		cfg := config.NewTestConfig()
		config.SetWatchNamespaceForTest(cfg, watch)
		h := newSettingsHarness(t, cfg, nil, settingsConfigMap(map[string]string{
			config.SettingLimiter: "quota", config.SettingQuotas: "H200=8"}))
		h.reconcile(config.SettingsConfigMapName())
		q := cfg.EffectiveQuotaEntries()
		if len(q) != 1 || q[0].NamespaceQuotas[key]["H200"] != 8 || len(q[0].NamespaceQuotas) != 1 {
			t.Errorf("watch %q: quota %+v, want it keyed on %s", watch, q, key)
		}
	}
}

// Beside a separate policy namespace whose cluster policy has not been read,
// a tenant's limiter and utilizationShare settings bound nothing and select
// nothing; passive still applies.
func TestSettingsBudgetGuardThroughReconciler(t *testing.T) {
	t.Setenv("POD_NAMESPACE", settingsTestNS)
	cfg := config.NewTestConfig()
	cfg.SetPolicyNamespace(config.WellKnownPolicyNamespace, config.PolicySourceWellKnown)
	h := newSettingsHarness(t, cfg, nil, settingsConfigMap(map[string]string{config.SettingPassive: "true",
		config.SettingLimiter: "quota", config.SettingQuotas: "H200=100", config.SettingQuotaScope: "cluster",
		config.SettingUtilizationShare: "active"}))
	if err := h.r.bootstrapSettings(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveLimiterSummary(); got != string(config.LimiterTypeNone) {
		t.Errorf("limiter %s: a tenant's settings bound the controller beside the policy namespace", got)
	}
	if mode, _ := cfg.UtilizationShareMode(); mode != "off" || !cfg.PassiveDefault() {
		t.Errorf("mode %s passive %t", mode, cfg.PassiveDefault())
	}
	h.want("separate", map[string]string{controllerstatus.KeyLimiter: string(config.LimiterTypeNone)})
	if src := h.status.Data()[controllerstatus.KeySettingsSources]; !strings.Contains(src, "limiter=policy-namespace") {
		t.Errorf("settingsSources %q", src)
	}
}
