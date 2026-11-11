package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// ReadSettings reads the settings ConfigMap (config.SettingsConfigMapName) in
// namespace through reader. It returns nil data and no error when the
// ConfigMap does not exist. Any other failure, Forbidden included, is
// returned: a settings ConfigMap the controller may not read is not one that
// is absent, and running without it could run without the limiter it
// declares.
func ReadSettings(ctx context.Context, reader client.Reader, namespace string) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	err := reader.Get(ctx, client.ObjectKey{Name: config.SettingsConfigMapName(), Namespace: namespace}, cm)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		hint := ""
		if apierrors.IsForbidden(err) {
			hint = "; the controller's ServiceAccount needs get, list and watch on configmaps in its namespace"
		}
		return nil, fmt.Errorf("cannot read the settings ConfigMap %s/%s%s: %w",
			namespace, config.SettingsConfigMapName(), hint, err)
	}
	return cm, nil
}

// bootstrapSettings puts the settings ConfigMap's live keys in force before
// anything reads the policy. A failed read other than NotFound stops startup
// (see ReadSettings).
func (r *ConfigMapReconciler) bootstrapSettings(ctx context.Context) error {
	cm, err := ReadSettings(ctx, r.Reader, config.SystemNamespace())
	if err != nil {
		return err
	}
	if cm == nil {
		log.FromContext(ctx).Info("No settings ConfigMap; the manager ConfigMap, the scaling policy and the defaults apply",
			"namespace", config.SystemNamespace(), "configMap", config.SettingsConfigMapName())
		r.Config.ApplySettings(config.Settings{}, false, "", config.SettingsReport{})
		r.Status.RecordSettings(r.Config)
		return nil
	}
	// A limiter declared with a value that cannot be used, and no earlier
	// one to keep, would start the controller unbounded: refuse, as an
	// invalid bounding policy "default" entry does (errUnboundedAtStartup).
	// Beside a separate policy namespace the limiter settings are not read.
	if r.Config.SettingsBudgetApplies() {
		_, rep := config.ParseSettings(cm.Data, r.Config.SettingsQuotaKey())
		if err := config.RejectedLimiterUnbounded(cm.Data, rep.Rejected, r.Config.LiveSettings()); err != nil {
			r.reportSettings(ctx, cm, rep)
			return err
		}
	}
	r.handleSettingsConfigMap(ctx, cm)
	return nil
}

// handleSettingsConfigMap puts a settings ConfigMap in force. A value that
// cannot be used is reported and the value in force before kept; the live
// keys apply at once, and a change to a key read only at start is reported as
// needing a restart.
func (r *ConfigMapReconciler) handleSettingsConfigMap(ctx context.Context, cm *corev1.ConfigMap) {
	logger := log.FromContext(ctx)
	prev := r.Config.LiveSettings()
	s, rep := config.ParseSettings(cm.Data, r.Config.SettingsQuotaKey())
	r.Config.RestrictSettings(&s, &rep)
	s = s.KeepRejected(prev, rep.Rejected)
	r.reportSettings(ctx, cm, rep)
	if !r.Config.SettingsBudgetApplies() {
		var ignored []string
		if s.Limiters != nil {
			ignored = append(ignored, config.SettingLimiter)
		}
		if s.UtilizationShare != "" {
			ignored = append(ignored, config.SettingUtilizationShare)
		}
		if len(ignored) > 0 {
			logger.Info("WARNING: ignoring settings: limiters and the optimizer are read from the policy namespace, "+
				"which this controller's own settings do not override. Ask a cluster admin to change them there.",
				"settings", ignored, "policyNamespace", r.Config.PolicyNamespace())
		}
	}
	r.Config.ApplySettings(s, true, cm.GetResourceVersion(), rep)
	r.logSettingsOverrides(ctx)
	r.warnIfSettingsNeedRestart(ctx, cm, s)
	r.Status.RecordSettings(r.Config)
	mode, _ := r.Config.UtilizationShareMode()
	logger.Info("Applied the settings ConfigMap", "configMap", cm.GetName(), "resourceVersion", cm.GetResourceVersion(),
		"passive", r.Config.PassiveDefault(), "utilizationShare", mode,
		"limiter", r.Config.EffectiveLimiterSummary(), "source", r.Config.SettingsSource())
}

// handleSettingsDeleted takes the settings ConfigMap out of force: every key
// falls back to the next source down.
func (r *ConfigMapReconciler) handleSettingsDeleted(ctx context.Context) {
	r.Config.ApplySettings(config.Settings{}, false, "", config.SettingsReport{})
	r.logSettingsOverrides(ctx)
	if changed := r.Config.SettingsRestartChanged(config.Settings{}); len(changed) > 0 {
		log.FromContext(ctx).Info("Restart required: the settings ConfigMap was deleted and these settings are read "+
			"only at start; the values set there stay in force until the controller restarts",
			"settings", changed)
	}
	r.settingsRestartWarned = ""
	r.Status.RecordSettings(r.Config)
	log.FromContext(ctx).Info("Settings ConfigMap deleted; the manager ConfigMap, the scaling policy and the defaults apply",
		"passive", r.Config.PassiveDefault(), "limiter", r.Config.EffectiveLimiterSummary())
}

// reportSettings makes what reading the settings found wrong visible: a log
// line and a Warning event per rejected key and for unknown keys, and a log
// line per warning. The controller status carries the same (RecordSettings).
func (r *ConfigMapReconciler) reportSettings(ctx context.Context, cm *corev1.ConfigMap, rep config.SettingsReport) {
	logger := log.FromContext(ctx)
	for _, k := range slices.Sorted(maps.Keys(rep.Rejected)) {
		msg := fmt.Sprintf("setting %q rejected: %s; the value in force before stays", k, rep.Rejected[k])
		logger.Info("WARNING: "+msg, "namespace", cm.GetNamespace(), "configMap", cm.GetName())
		if r.Recorder != nil {
			r.Recorder.Event(cm, corev1.EventTypeWarning, constants.K8SEventSettingRejected, msg)
		}
	}
	if len(rep.Unknown) > 0 {
		var hints []string
		for _, k := range rep.Unknown {
			if h := config.UnknownSettingHint(k); h != "" {
				hints = append(hints, h)
			}
		}
		msg := fmt.Sprintf("keys %s are no setting and are ignored", strings.Join(rep.Unknown, ", "))
		if len(hints) > 0 {
			msg += " (" + strings.Join(hints, "; ") + ")"
		}
		logger.Info("WARNING: settings ConfigMap: "+msg, "namespace", cm.GetNamespace(), "configMap", cm.GetName())
		if r.Recorder != nil {
			r.Recorder.Event(cm, corev1.EventTypeWarning, constants.K8SEventSettingsUnknownKeys, msg)
		}
	}
	for _, w := range rep.Warnings {
		logger.Info("WARNING: settings ConfigMap: "+w, "namespace", cm.GetNamespace(), "configMap", cm.GetName())
	}
}

// logSettingsOverrides logs, once per change, where a live setting replaces a
// different value in the scaling policy's "default" entry: both values, and
// that the setting wins.
func (r *ConfigMapReconciler) logSettingsOverrides(ctx context.Context) {
	def, _ := r.Config.ScalingPolicyEntry("", config.GlobalDefaultsKey)
	overrides := config.SettingsOverridePolicy(def, r.Config.LiveSettings(), r.Config.SettingsBudgetApplies())
	text := strings.Join(overrides, "; ")
	if text == r.settingsOverrideLogged {
		return
	}
	r.settingsOverrideLogged = text
	if text == "" {
		return
	}
	log.FromContext(ctx).Info("The settings ConfigMap overrides the scaling policy's default entry; the setting wins. "+
		"Per-model and tier entries still override both.", "overrides", overrides)
}

// warnIfSettingsNeedRestart warns, once per change, when a setting read only at
// start differs from the value in force: it takes effect on the next restart.
//
// r.settingsRestartWarned is not locked, for the reason warnIfPassiveChanged
// gives.
func (r *ConfigMapReconciler) warnIfSettingsNeedRestart(ctx context.Context, cm *corev1.ConfigMap, s config.Settings) {
	changed := r.Config.SettingsRestartChanged(s)
	key := strings.Join(changed, ",")
	if key == r.settingsRestartWarned {
		return
	}
	r.settingsRestartWarned = key
	if key == "" {
		return
	}
	msg := fmt.Sprintf("settings %s are read only at start and changed: restart the controller to apply them "+
		"(kubectl rollout restart deployment/scaling-manager-controller-manager -n %s)", key, cm.GetNamespace())
	log.FromContext(ctx).Info("Restart required: "+msg, "namespace", cm.GetNamespace(), "configMap", cm.GetName())
	if r.Recorder != nil {
		r.Recorder.Event(cm, corev1.EventTypeWarning, constants.K8SEventSettingsRestartRequired, msg)
	}
}
