package preflight

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// The settings keys (config/release/settings-configmap.yaml), as
// internal/config names them.
const (
	keyPrometheusURL      = config.SettingPrometheusURL
	keyPrometheusInsecure = config.SettingPrometheusInsecureSkipVerify
	keyPassive            = config.SettingPassive
	keyUtilizationShare   = config.SettingUtilizationShare
	keyLimiter            = config.SettingLimiter
	keyQuotas             = config.SettingQuotas
	keyQuotaScope         = config.SettingQuotaScope
	keyKueue              = config.SettingKueue
	keyKueueBorrow        = config.SettingKueueBorrow
	keyKueueNamespace     = config.SettingKueueNamespace
	keyScaleToZero        = config.SettingScaleToZero
	keyWatchNamespaces    = config.SettingWatchNamespaces
	keyLogVerbosity       = config.SettingLogVerbosity
	// keyAllowHTTP is read from the environment or config.yaml only: the
	// settings ConfigMap does not take it.
	keyAllowHTTP = "PROMETHEUS_ALLOW_HTTP"
)

// settingsDefaults are the values the shipped settings file holds, used for a
// key neither the settings nor the legacy configuration holds.
var settingsDefaults = map[string]string{
	keyPrometheusURL:      "",
	keyPrometheusInsecure: "false",
	keyPassive:            "true",
	keyUtilizationShare:   config.UtilizationShareSettingOff,
	keyLimiter:            "",
	keyQuotas:             "",
	keyQuotaScope:         string(config.QuotaScopeNamespace),
	keyKueue:              "false",
	keyKueueBorrow:        "true",
	keyKueueNamespace:     "kueue-system",
	keyScaleToZero:        "false",
	keyWatchNamespaces:    "",
	keyLogVerbosity:       "2",
}

// Values the settings keys take.
const (
	shareActive  = config.UtilizationShareSettingActive
	limiterQuota = string(config.LimiterTypeQuota)
	valueTrue    = "true"
)

// legacyKeys maps a key to the config.yaml key of LegacyConfigMap the
// controller reads it from when the settings do not hold it: the loader key
// internal/config gives the setting.
func legacyKeys() map[string]string {
	out := map[string]string{keyAllowHTTP: keyAllowHTTP}
	for _, k := range append(config.RestartSettingKeys(), keyPassive) {
		if loaderKey, ok := config.SettingLoaderKey(k); ok {
			out[k] = loaderKey
		}
	}
	return out
}

// settingsState is the settings as the controller will read them.
type settingsState struct {
	// found: the settings ConfigMap exists. legacy: the legacy ConfigMap does.
	found, legacy bool
	// unread is why the settings could not be read, when they could not.
	unread string
	// set holds the keys the controller reads from the settings, and from the
	// legacy configuration for a key the settings do not hold.
	set map[string]string
	// fromLegacy names the keys read from the legacy configuration.
	fromLegacy []string
	// parsed and report are the settings ConfigMap as the controller's parser,
	// config.ParseSettings, reads it.
	parsed config.Settings
	report config.SettingsReport
	// unbounded is why the controller refuses to start on these settings: a
	// limiter is declared and the keys that build it are rejected.
	unbounded error
}

// value is the effective value of key: set, else the shipped default.
func (st settingsState) value(key string) string {
	if v, ok := st.set[key]; ok {
		return strings.TrimSpace(v)
	}
	return settingsDefaults[key]
}

// loadSettings reads the settings ConfigMap through the controller's parser,
// and LegacyConfigMap's config.yaml for a key it does not hold.
func (s *state) loadSettings(ctx context.Context) settingsState {
	s.settingsOnce.Do(func() {
		st := settingsState{set: map[string]string{}}
		cms := s.env.Kube.CoreV1().ConfigMaps(s.env.Namespace)
		cm, err := cms.Get(ctx, SettingsConfigMap, metav1.GetOptions{})
		switch err {
		case nil:
			st.found = true
			st.parsed, st.report = config.ParseSettings(cm.Data, s.settingsQuotaKey(cm.Data))
			// At start there is no value in force before: what KeepRejected
			// and RejectedLimiterUnbounded decide with an empty one is what
			// the controller does on its first read.
			st.parsed = st.parsed.KeepRejected(config.Settings{}, st.report.Rejected)
			st.unbounded = config.RejectedLimiterUnbounded(cm.Data, st.report.Rejected, config.Settings{})
			for k, v := range cm.Data {
				// A key the controller does not know is not read: the
				// next source down gives its value.
				if !slices.Contains(st.report.Unknown, k) {
					st.set[k] = v
				}
			}
		default:
			absent, why := readVerdict(err, "ConfigMap "+SettingsConfigMap+" in "+s.env.Namespace)
			if !absent {
				st.unread = why
			}
		}
		legacy, err := cms.Get(ctx, LegacyConfigMap, metav1.GetOptions{})
		if err == nil {
			st.legacy = true
			var old map[string]any
			if yaml.Unmarshal([]byte(legacy.Data["config.yaml"]), &old) == nil {
				for key, name := range legacyKeys() {
					if _, ok := st.set[key]; ok {
						continue
					}
					if v, ok := old[name]; ok && v != nil {
						st.set[key] = fmt.Sprint(v)
						st.fromLegacy = append(st.fromLegacy, key)
					}
				}
			}
		}
		s.settings = st
	})
	return s.settings
}

// settingsQuotaKey is the namespace a namespace-scoped quota from the settings
// is keyed on, as Config.SettingsQuotaKey chooses it: the watched namespace
// when the controller watches exactly one, else "default".
func (s *state) settingsQuotaKey(data map[string]string) string {
	watched := config.ParseWatchNamespaces(data[keyWatchNamespaces])
	if len(watched) == 0 && s.env.Scope != ScopeCluster {
		watched = []string{s.env.Namespace}
	}
	if len(watched) == 1 {
		return watched[0]
	}
	return config.QuotaLimiterReservedNamespaceKey
}

// checkSettings reports whether the settings exist and the controller accepts
// every key: the verdict is config.ParseSettings', the controller's own.
func checkSettings(ctx context.Context, s *state) ([]Result, string) {
	st := s.loadSettings(ctx)
	ns := s.env.Namespace
	create := fmt.Sprintf(`sed "s/__NAMESPACE__/%s/g" scaling-manager-settings.yaml | kubectl create -f -`, ns)
	if st.unread != "" {
		return []Result{{Status: Unknown, Detail: st.unread + "; every check below reads the shipped defaults"}}, ""
	}
	var bad []string
	for _, k := range slices.Sorted(maps.Keys(st.report.Rejected)) {
		bad = append(bad, k+": "+st.report.Rejected[k])
	}
	us := st.parsed.UtilizationShare
	if (us == config.UtilizationShareSettingShadow || us == shareActive) && st.value(keyLimiter) == "" {
		bad = append(bad, fmt.Sprintf("%s=%q needs a limiter: sharing divides a budget, and %s is empty", keyUtilizationShare, us, keyLimiter))
	}
	patch := fmt.Sprintf(`kubectl patch configmap %s -n %s --type merge -p '{"data":{"<key>":"<value>"}}'`, SettingsConfigMap, ns)
	switch {
	case len(bad) > 0:
		detail := "settings the controller rejects: " + strings.Join(bad, "; ")
		if st.unbounded != nil {
			detail += "; the controller refuses to start on them"
		}
		return []Result{{Status: Fail, Detail: detail, Fix: patch}}, ""
	case !st.found && !st.legacy:
		return []Result{{Status: Fail,
			Detail: fmt.Sprintf("ConfigMap %s does not exist in %s: the checks below read the shipped defaults, and prometheusURL has none", SettingsConfigMap, ns),
			Fix:    create + "   # after setting prometheusURL in the file"}}, ""
	case !st.found:
		return []Result{{Status: Warn,
			Detail: fmt.Sprintf("no ConfigMap %s in %s; read %s from %s (config.yaml), which the controller falls back to for one release",
				SettingsConfigMap, ns, listText(st.fromLegacy, 10), LegacyConfigMap),
			Fix: create + "   # with the values above"}}, ""
	case len(st.report.Unknown) > 0 || len(st.report.Warnings) > 0:
		var notes []string
		for _, k := range st.report.Unknown {
			note := k + " is no setting, and the controller ignores it"
			if hint := config.UnknownSettingHint(k); hint != "" {
				note += " (" + hint + ")"
			}
			notes = append(notes, note)
		}
		notes = append(notes, st.report.Warnings...)
		return []Result{{Status: Warn, Detail: strings.Join(notes, "; "), Fix: patch}}, ""
	case len(st.fromLegacy) > 0:
		return []Result{{Status: Pass, Detail: fmt.Sprintf("read %s from %s; %s from %s (config.yaml)",
			SettingsConfigMap, ns, listText(st.fromLegacy, 10), LegacyConfigMap)}}, ""
	}
	return []Result{{Status: Pass, Detail: fmt.Sprintf("%s in %s parses: passive=%s, utilizationShare=%s, limiter=%q",
		SettingsConfigMap, ns, st.value(keyPassive), st.value(keyUtilizationShare), st.value(keyLimiter))}}, ""
}
