package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/controllerstatus"
)

const settingsTestNS = "scaling-manager-settings-test"

type settingsHarness struct {
	t      *testing.T
	ctx    context.Context
	c      client.Client
	r      *ConfigMapReconciler
	rec    *record.FakeRecorder
	status *controllerstatus.Publisher
}

func newSettingsHarness(t *testing.T, cfg *config.Config, funcs *interceptor.Funcs, objs ...client.Object) *settingsHarness {
	t.Helper()
	t.Setenv("POD_NAMESPACE", settingsTestNS)
	t.Setenv("LLMSM_PASSIVE", "")
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	c := b.Build()
	// As main does before the bootstrap: the budget settings wait for it.
	if cfg.PolicySource() == config.PolicySourceLocal && !cfg.PolicyNamespaceIsSeparate() {
		cfg.SetPolicyNamespace(settingsTestNS, config.PolicySourceLocal)
	}
	rec := record.NewFakeRecorder(100)
	status := controllerstatus.New(c, c, settingsTestNS)
	return &settingsHarness{t: t, ctx: context.Background(), c: c, rec: rec, status: status,
		r: &ConfigMapReconciler{Reader: c, Scheme: s, Config: cfg, Recorder: rec, Status: status}}
}

func settingsConfigMap(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.SettingsConfigMapName(), Namespace: settingsTestNS},
		Data:       data,
	}
}

// patch merges data into the settings ConfigMap, as kubectl patch --type merge
// does (an empty value is kept as empty), then reconciles it.
func (h *settingsHarness) patch(data map[string]string) {
	h.t.Helper()
	var cm corev1.ConfigMap
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: settingsTestNS, Name: config.SettingsConfigMapName()}, &cm); err != nil {
		h.t.Fatal(err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	for k, v := range data {
		cm.Data[k] = v
	}
	if err := h.c.Update(h.ctx, &cm); err != nil {
		h.t.Fatal(err)
	}
	h.reconcile(config.SettingsConfigMapName())
}

func (h *settingsHarness) reconcile(name string) {
	h.t.Helper()
	if _, err := h.r.Reconcile(h.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: settingsTestNS, Name: name}}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *settingsHarness) rv() string {
	h.t.Helper()
	var cm corev1.ConfigMap
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: settingsTestNS, Name: config.SettingsConfigMapName()}, &cm); err != nil {
		h.t.Fatal(err)
	}
	return cm.ResourceVersion
}

func (h *settingsHarness) events() []string {
	var out []string
	for len(h.rec.Events) > 0 {
		out = append(out, <-h.rec.Events)
	}
	return out
}

// want checks the controller-status keys.
func (h *settingsHarness) want(step string, kv map[string]string) {
	h.t.Helper()
	d := h.status.Data()
	for k, v := range kv {
		if d[k] != v {
			h.t.Errorf("%s: status %s=%q, want %q", step, k, d[k], v)
		}
	}
}

// Passive, then shadow, then active by patching the settings ConfigMap, each
// step live: reconcile, the effective configuration, the optimizer mode, and
// the controller status a tool reads to confirm it. Then back, and a typo
// that keeps the mode in force.
func TestSettingsPassiveShadowActiveLive(t *testing.T) {
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfig(config.ScalingPolicySet{})
	h := newSettingsHarness(t, cfg, nil, settingsConfigMap(map[string]string{
		config.SettingPassive: "true", config.SettingUtilizationShare: "off",
	}))
	if err := h.r.bootstrapSettings(h.ctx); err != nil {
		t.Fatal(err)
	}
	if !cfg.PassiveDefault() {
		t.Fatal("step 0: not passive")
	}
	if p, _ := config.ResolvePassive(cfg, "team-a", "m", nil); !p {
		t.Fatal("step 0: a model resolves active")
	}
	h.want("step 0", map[string]string{
		controllerstatus.KeyPassiveDefault: "true", controllerstatus.KeyUtilizationShare: "off",
		controllerstatus.KeySettingsResourceVersion: h.rv(), controllerstatus.KeySettingsSource: "settings",
	})

	// Before sharing: the budget.
	h.patch(map[string]string{config.SettingLimiter: "quota", config.SettingQuotas: "H200=8", config.SettingQuotaScope: "cluster"})
	h.want("budget", map[string]string{controllerstatus.KeyLimiter: "quota/install-quota"})

	// 1. Passive, sharing in shadow.
	h.patch(map[string]string{config.SettingUtilizationShare: "shadow"})
	if mode, _ := cfg.UtilizationShareMode(); mode != constants.UtilizationShareModeShadow || cfg.UtilizationShareActive() {
		t.Fatalf("step 1: mode %s", mode)
	}
	h.want("step 1", map[string]string{controllerstatus.KeyUtilizationShare: "shadow", controllerstatus.KeyPassiveDefault: "true",
		controllerstatus.KeySettingsResourceVersion: h.rv()})

	// 2. Let it scale, sharing still in shadow.
	h.patch(map[string]string{config.SettingPassive: "false"})
	if p, src := config.ResolvePassive(cfg, "team-a", "m", nil); p || src != config.PassiveSourceSettings {
		t.Fatalf("step 2: passive %t from %s", p, src)
	}
	h.want("step 2", map[string]string{controllerstatus.KeyPassiveDefault: "false", controllerstatus.KeyUtilizationShare: "shadow"})

	// 3. Let sharing act.
	h.patch(map[string]string{config.SettingUtilizationShare: "active"})
	if !cfg.UtilizationShareActive() {
		t.Fatal("step 3: the optimizer does not act")
	}
	h.want("step 3", map[string]string{controllerstatus.KeyUtilizationShare: "active",
		controllerstatus.KeySettingsResourceVersion: h.rv()})

	// A typo keeps active in force, says so, and records the version read.
	h.events()
	h.patch(map[string]string{config.SettingUtilizationShare: "activ"})
	if !cfg.UtilizationShareActive() {
		t.Fatal("typo: the mode in force was dropped")
	}
	if ev := h.events(); len(ev) != 1 || !strings.Contains(ev[0], constants.K8SEventSettingRejected) ||
		!strings.Contains(ev[0], "utilizationShare") {
		t.Errorf("typo: events %v", ev)
	}
	if !strings.Contains(h.status.Data()[controllerstatus.KeySettingsRejected], "utilizationShare") {
		t.Errorf("typo: status %v", h.status.Data())
	}

	// Back: off, then passive again.
	h.patch(map[string]string{config.SettingUtilizationShare: "off", config.SettingPassive: "true"})
	if mode, _ := cfg.UtilizationShareMode(); mode != constants.UtilizationShareModeOff || !cfg.PassiveDefault() {
		t.Fatalf("back: mode %s passive %t", mode, cfg.PassiveDefault())
	}
	if _, rejected := h.status.Data()[controllerstatus.KeySettingsRejected]; rejected {
		t.Error("back: settingsRejected still present after the value was fixed")
	}

	// Deleted: every key falls back; the controller started not passive.
	var cm corev1.ConfigMap
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: settingsTestNS, Name: config.SettingsConfigMapName()}, &cm); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(h.ctx, &cm); err != nil {
		t.Fatal(err)
	}
	h.reconcile(config.SettingsConfigMapName())
	if cfg.PassiveDefault() || cfg.EffectiveLimiterSummary() != "none" {
		t.Errorf("deleted: passive %t limiter %s", cfg.PassiveDefault(), cfg.EffectiveLimiterSummary())
	}
	h.want("deleted", map[string]string{controllerstatus.KeySettingsResourceVersion: "",
		controllerstatus.KeySettingsSource: config.SettingsSourceDefaults})
}

// The settings and the scaling-policy ConfigMap through the reconciler: a set
// key replaces the policy default's field, a tier still wins, and a policy
// edit afterwards keeps the setting.
func TestSettingsOverridePolicyThroughReconciler(t *testing.T) {
	cfg := config.NewTestConfig()
	policy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ScalingPolicyConfigMapName(), Namespace: settingsTestNS},
		Data: map[string]string{
			config.GlobalDefaultsKey: "passive: true\n",
			"batch":                  "passive: true\n",
		},
	}
	h := newSettingsHarness(t, cfg, nil, policy, settingsConfigMap(map[string]string{config.SettingPassive: "false"}))
	h.reconcile(config.ScalingPolicyConfigMapName())
	h.reconcile(config.SettingsConfigMapName())
	if p, src := config.ResolvePassive(cfg, "team-a", "m", nil); p || src != config.PassiveSourceSettings {
		t.Errorf("default: passive %t from %s, want false from settings", p, src)
	}
	if p, _ := config.ResolvePassive(cfg, "team-a", "m", []string{"batch"}); !p {
		t.Error("tier batch: the setting overrode a tier")
	}
	policy.Data[config.GlobalDefaultsKey] = "passive: true\nkvCacheThreshold: 0.7\n"
	if err := h.c.Update(h.ctx, policy); err != nil {
		t.Fatal(err)
	}
	h.reconcile(config.ScalingPolicyConfigMapName())
	if p, _ := config.ResolvePassive(cfg, "team-a", "m", nil); p {
		t.Error("a policy edit dropped the passive setting")
	}
	if got := h.status.Data()[controllerstatus.KeySettingsSources]; !strings.Contains(got, "passive=settings") {
		t.Errorf("settingsSources %q", got)
	}
}

// Forbidden is not absent: at start it stops the controller; at runtime the
// settings in force stay, and nothing is reported as deleted.
func TestSettingsForbiddenRead(t *testing.T) {
	forbidden := false
	funcs := &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if forbidden && key.Name == config.SettingsConfigMapName() {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, errors.New("no grant"))
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	cfg := config.NewTestConfig()
	h := newSettingsHarness(t, cfg, funcs, settingsConfigMap(map[string]string{config.SettingPassive: "true"}))

	forbidden = true
	err := h.r.bootstrapSettings(h.ctx)
	if err == nil || !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "get, list and watch on configmaps") {
		t.Fatalf("bootstrap error %v, want a Forbidden that names the grant", err)
	}
	if err := h.r.BootstrapInitialConfigMaps(h.ctx); err == nil {
		t.Fatal("the bootstrap went on with an unreadable settings ConfigMap")
	}

	forbidden = false
	if err := h.r.bootstrapSettings(h.ctx); err != nil {
		t.Fatal(err)
	}
	forbidden = true
	if _, err := h.r.Reconcile(h.ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: settingsTestNS, Name: config.SettingsConfigMapName()}}); err == nil {
		t.Error("a Forbidden read reconciled without an error")
	}
	if !cfg.PassiveDefault() {
		t.Error("a Forbidden read dropped the settings in force")
	}
	if present, _ := cfg.SettingsPresent(); !present {
		t.Error("a Forbidden read was taken for a deleted settings ConfigMap")
	}
}

// A restart key that changed is reported once, as needing a restart, and the
// value in force does not move; unknown keys are reported with a hint.
func TestSettingsRestartKeyAndUnknownKeys(t *testing.T) {
	t.Setenv("PROMETHEUS_BASE_URL", "")
	cfg, err := config.LoadWithSettings(nil, "", map[string]string{config.SettingPrometheusURL: "https://a:9090"})
	if err != nil {
		t.Fatal(err)
	}
	h := newSettingsHarness(t, cfg, nil, settingsConfigMap(map[string]string{config.SettingPrometheusURL: "https://a:9090"}))
	if err := h.r.bootstrapSettings(h.ctx); err != nil {
		t.Fatal(err)
	}
	if ev := h.events(); len(ev) != 0 {
		t.Fatalf("bootstrap events %v, want none", ev)
	}
	h.patch(map[string]string{config.SettingPrometheusURL: "https://b:9090"})
	h.patch(map[string]string{config.SettingPassive: "true"}) // an unrelated edit: no second warning
	ev := h.events()
	if len(ev) != 1 || !strings.Contains(ev[0], constants.K8SEventSettingsRestartRequired) || !strings.Contains(ev[0], config.SettingPrometheusURL) {
		t.Fatalf("events %v, want one %s naming %s", ev, constants.K8SEventSettingsRestartRequired, config.SettingPrometheusURL)
	}
	if cfg.PrometheusBaseURL() != "https://a:9090" {
		t.Errorf("url %s moved without a restart", cfg.PrometheusBaseURL())
	}

	h.patch(map[string]string{"LLMSM_LIMITER": "quota"})
	ev = h.events()
	if len(ev) != 1 || !strings.Contains(ev[0], constants.K8SEventSettingsUnknownKeys) || !strings.Contains(ev[0], "now the limiter setting") {
		t.Errorf("events %v, want one %s with the hint", ev, constants.K8SEventSettingsUnknownKeys)
	}
	if got := h.status.Data()[controllerstatus.KeySettingsUnknownKeys]; got != "LLMSM_LIMITER" {
		t.Errorf("settingsUnknownKeys %q", got)
	}
}

// The manager ConfigMap's LLMSM_PASSIVE is read only at start, and warned
// about when it changes -- unless the settings ConfigMap's passive, which is
// live, is in force.
func TestPassiveRestartWarningYieldsToSettings(t *testing.T) {
	cfg := config.NewTestConfig()
	h := newSettingsHarness(t, cfg, nil)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapName(), Namespace: settingsTestNS},
		Data: map[string]string{config.ManagerConfigKey: "LLMSM_PASSIVE: \"true\"\n"}}
	cfg.ApplySettings(config.Settings{Passive: new(bool)}, true, "1", config.SettingsReport{})
	h.r.warnIfPassiveChanged(h.ctx, cm)
	if ev := h.events(); len(ev) != 0 {
		t.Errorf("events %v with the passive setting in force", ev)
	}
	cfg.ApplySettings(config.Settings{}, true, "2", config.SettingsReport{})
	h.r.warnIfPassiveChanged(h.ctx, cm)
	if ev := h.events(); len(ev) != 1 {
		t.Errorf("events %v, want the restart warning once the setting is unset", ev)
	}
}
