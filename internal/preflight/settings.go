package preflight

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The settings keys (config/release/settings-configmap.yaml).
const (
	keyPrometheusURL      = "prometheusURL"
	keyPrometheusInsecure = "prometheusInsecureSkipVerify"
	keyPassive            = "passive"
	keyUtilizationShare   = "utilizationShare"
	keyLimiter            = "limiter"
	keyQuotas             = "quotas"
	keyQuotaScope         = "quotaScope"
	keyKueue              = "kueue"
	keyKueueBorrow        = "kueueBorrow"
	keyKueueNamespace     = "kueueNamespace"
	keyScaleToZero        = "scaleToZero"
	keyWatchNamespaces    = "watchNamespaces"
	keyLogVerbosity       = "logVerbosity"
	// keyAllowHTTP is an advanced key, read under its current name.
	keyAllowHTTP = "PROMETHEUS_ALLOW_HTTP"
)

// settingsDefaults are the values the shipped settings file holds, used for a
// key neither the settings nor the legacy configuration holds.
var settingsDefaults = map[string]string{
	keyPrometheusURL:      "",
	keyPrometheusInsecure: "false",
	keyPassive:            "true",
	keyUtilizationShare:   "off",
	keyLimiter:            "",
	keyQuotas:             "",
	keyQuotaScope:         "namespace",
	keyKueue:              "false",
	keyKueueBorrow:        "true",
	keyKueueNamespace:     "kueue-system",
	keyScaleToZero:        "false",
	keyWatchNamespaces:    "",
	keyLogVerbosity:       "2",
}

// legacyKeys maps a settings key to the config.yaml keys of
// LegacyConfigMap that held it before v0.11.0, first match wins.
var legacyKeys = map[string][]string{
	keyPrometheusURL:      {"PROMETHEUS_BASE_URL"},
	keyPrometheusInsecure: {"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY"},
	keyPassive:            {"LLMSM_PASSIVE"},
	keyScaleToZero:        {"LLMSM_SCALE_TO_ZERO", "ENABLE_SCALE_TO_ZERO"},
	keyAllowHTTP:          {"PROMETHEUS_ALLOW_HTTP"},
}

// settingsState is the settings as the controller will read them.
type settingsState struct {
	// found: the settings ConfigMap exists. legacy: the legacy ConfigMap does.
	found, legacy bool
	// unread is why the settings could not be read, when they could not.
	unread string
	// set holds the keys the settings or legacy configuration hold.
	set map[string]string
	// fromLegacy names the keys read from the legacy configuration.
	fromLegacy []string
}

// value is the effective value of key: set, else the shipped default.
func (st settingsState) value(key string) string {
	if v, ok := st.set[key]; ok {
		return strings.TrimSpace(v)
	}
	return settingsDefaults[key]
}

// loadSettings reads the settings ConfigMap, and LegacyConfigMap's
// config.yaml for a key it does not hold.
func (s *state) loadSettings(ctx context.Context) settingsState {
	s.settingsOnce.Do(func() {
		st := settingsState{set: map[string]string{}}
		cms := s.env.Kube.CoreV1().ConfigMaps(s.env.Namespace)
		cm, err := cms.Get(ctx, SettingsConfigMap, metav1.GetOptions{})
		switch {
		case err == nil:
			st.found = true
			for k, v := range cm.Data {
				st.set[k] = v
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
				for key, names := range legacyKeys {
					if _, ok := st.set[key]; ok {
						continue
					}
					for _, n := range names {
						if v, ok := old[n]; ok && v != nil {
							st.set[key] = fmt.Sprint(v)
							st.fromLegacy = append(st.fromLegacy, key)
							break
						}
					}
				}
			}
		}
		s.settings = st
	})
	return s.settings
}

// checkSettings reports whether the settings exist and every key parses.
func checkSettings(ctx context.Context, s *state) ([]Result, string) {
	st := s.loadSettings(ctx)
	ns := s.env.Namespace
	create := fmt.Sprintf(`sed "s/__NAMESPACE__/%s/g" scaling-manager-settings.yaml | kubectl create -f -`, ns)
	if st.unread != "" {
		return []Result{{Status: Unknown, Detail: st.unread + "; every check below reads the shipped defaults"}}, ""
	}
	var bad []string
	for _, k := range []string{keyPrometheusInsecure, keyPassive, keyKueue, keyKueueBorrow, keyScaleToZero} {
		if _, err := strconv.ParseBool(st.value(k)); err != nil {
			bad = append(bad, fmt.Sprintf("%s=%q is not true or false", k, st.value(k)))
		}
	}
	us := st.value(keyUtilizationShare)
	switch us {
	case "off", "shadow", "active":
	default:
		bad = append(bad, fmt.Sprintf("%s=%q is not off, shadow or active", keyUtilizationShare, us))
	}
	limiters, lerr := parseLimiters(st.value(keyLimiter))
	if lerr != nil {
		bad = append(bad, lerr.Error())
	}
	if us != "off" && us != "" && lerr == nil && len(limiters) == 0 {
		bad = append(bad, fmt.Sprintf("%s=%q needs a limiter: sharing divides a budget, and %s is empty", keyUtilizationShare, us, keyLimiter))
	}
	if _, err := parseQuotas(st.value(keyQuotas)); err != nil {
		bad = append(bad, err.Error())
	}
	if limiters["quota"] && st.value(keyQuotas) == "" && st.value(keyKueue) != "true" {
		bad = append(bad, fmt.Sprintf("%s names quota, and neither %s nor %s=\"true\" gives it a budget", keyLimiter, keyQuotas, keyKueue))
	}
	switch st.value(keyQuotaScope) {
	case "namespace", "cluster":
	default:
		bad = append(bad, fmt.Sprintf("%s=%q is not namespace or cluster", keyQuotaScope, st.value(keyQuotaScope)))
	}
	if v := st.value(keyKueueNamespace); v != "" && len(validation.IsDNS1123Label(v)) > 0 {
		bad = append(bad, fmt.Sprintf("%s=%q is not a namespace name", keyKueueNamespace, v))
	}
	for _, w := range splitList(st.value(keyWatchNamespaces)) {
		if len(validation.IsDNS1123Label(w)) > 0 {
			bad = append(bad, fmt.Sprintf("%s names %q, not a namespace name", keyWatchNamespaces, w))
		}
	}
	if n, err := strconv.Atoi(st.value(keyLogVerbosity)); err != nil || n < 0 {
		bad = append(bad, fmt.Sprintf("%s=%q is not a whole number of 0 or more", keyLogVerbosity, st.value(keyLogVerbosity)))
	}
	if u := st.value(keyPrometheusURL); u != "" {
		if p, err := url.Parse(u); err != nil || p.Host == "" || (p.Scheme != "https" && p.Scheme != "http") {
			bad = append(bad, fmt.Sprintf("%s=%q is not an http(s) URL with a host", keyPrometheusURL, u))
		}
	}
	patch := fmt.Sprintf(`kubectl patch configmap %s -n %s --type merge -p '{"data":{"<key>":"<value>"}}'`, SettingsConfigMap, ns)
	switch {
	case len(bad) > 0:
		return []Result{{Status: Fail, Detail: "settings that do not parse: " + strings.Join(bad, "; "), Fix: patch}}, ""
	case !st.found && !st.legacy:
		return []Result{{Status: Fail,
			Detail: fmt.Sprintf("ConfigMap %s does not exist in %s: the checks below read the shipped defaults, and prometheusURL has none", SettingsConfigMap, ns),
			Fix:    create + "   # after setting prometheusURL in the file"}}, ""
	case !st.found:
		return []Result{{Status: Warn,
			Detail: fmt.Sprintf("no ConfigMap %s in %s; read %s from %s (config.yaml), which the controller falls back to for one release",
				SettingsConfigMap, ns, listText(st.fromLegacy, 10), LegacyConfigMap),
			Fix: create + "   # with the values above"}}, ""
	case len(st.fromLegacy) > 0:
		return []Result{{Status: Pass, Detail: fmt.Sprintf("read %s from %s; %s from %s (config.yaml)",
			SettingsConfigMap, ns, listText(st.fromLegacy, 10), LegacyConfigMap)}}, ""
	}
	return []Result{{Status: Pass, Detail: fmt.Sprintf("%s in %s parses: passive=%s, utilizationShare=%s, limiter=%q",
		SettingsConfigMap, ns, st.value(keyPassive), us, st.value(keyLimiter))}}, ""
}

// parseLimiters reads the limiter key: empty, gpu-inventory, quota, or both.
func parseLimiters(v string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, l := range splitList(v) {
		switch l {
		case "gpu-inventory", "quota":
			out[l] = true
		default:
			return nil, fmt.Errorf("%s names %q; the limiters are gpu-inventory and quota", keyLimiter, l)
		}
	}
	return out, nil
}

// parseQuotas reads the quotas key: NAME=N pairs, comma-separated, N a whole
// number or -1 for no cap.
func parseQuotas(v string) (map[string]int, error) {
	out := map[string]int{}
	for _, pair := range splitList(v) {
		name, n, ok := strings.Cut(pair, "=")
		name = strings.TrimSpace(name)
		q, err := strconv.Atoi(strings.TrimSpace(n))
		if !ok || name == "" || err != nil || q < -1 {
			return nil, fmt.Errorf("%s holds %q; write NAME=N with N a whole number, or -1 for no cap", keyQuotas, pair)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%s names %s twice", keyQuotas, name)
		}
		out[name] = q
	}
	return out, nil
}
