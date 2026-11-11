package config

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// DefaultSettingsConfigMapName is the name of the settings ConfigMap: the flat
// keys an operator edits, in the controller's own namespace. See
// docs/reference/configuration.md.
const DefaultSettingsConfigMapName = "scaling-manager-settings"

// SettingsConfigMapName returns the settings ConfigMap's name:
// SETTINGS_CONFIG_MAP_NAME when set, else DefaultSettingsConfigMapName.
func SettingsConfigMapName() string {
	if name := os.Getenv("SETTINGS_CONFIG_MAP_NAME"); name != "" {
		return name
	}
	return DefaultSettingsConfigMapName
}

// The keys of the settings ConfigMap. Each is one setting; a key that is
// absent or empty is unset, and the value comes from the next source down
// (docs/reference/configuration.md, "Where a value comes from").
const (
	SettingPrometheusURL                = "prometheusURL"
	SettingPrometheusInsecureSkipVerify = "prometheusInsecureSkipVerify"
	SettingPassive                      = "passive"
	SettingUtilizationShare             = "utilizationShare"
	SettingLimiter                      = "limiter"
	SettingQuotas                       = "quotas"
	SettingQuotaScope                   = "quotaScope"
	SettingKueue                        = "kueue"
	SettingKueueBorrow                  = "kueueBorrow"
	SettingKueueNamespace               = "kueueNamespace"
	SettingScaleToZero                  = "scaleToZero"
	SettingWatchNamespaces              = "watchNamespaces"
	SettingLogVerbosity                 = "logVerbosity"
)

// LiveSettingKeys are the settings that take effect within a cycle of a change.
var LiveSettingKeys = []string{
	SettingPassive, SettingUtilizationShare, SettingLimiter, SettingQuotas, SettingQuotaScope,
	SettingKueue, SettingKueueBorrow, SettingKueueNamespace,
}

// limiterSettingKeys are the live keys that together build the limiters list.
// They are kept or replaced as one: a quota without its scope, or a scope
// without its quota, is not a bound anyone asked for.
var limiterSettingKeys = []string{
	SettingLimiter, SettingQuotas, SettingQuotaScope, SettingKueue, SettingKueueBorrow, SettingKueueNamespace,
}

// restartSettingKeys maps each setting read only at start to the loader key
// (flag, environment variable and config.yaml name) it sets.
var restartSettingKeys = map[string]string{
	SettingPrometheusURL:                "PROMETHEUS_BASE_URL",
	SettingPrometheusInsecureSkipVerify: "PROMETHEUS_TLS_INSECURE_SKIP_VERIFY",
	SettingScaleToZero:                  "LLMSM_SCALE_TO_ZERO",
	SettingWatchNamespaces:              "WATCH_NAMESPACE",
	SettingLogVerbosity:                 "V",
}

// RestartSettingKeys are the settings read only at start, sorted.
func RestartSettingKeys() []string {
	return slices.Sorted(maps.Keys(restartSettingKeys))
}

// advancedSettingKeys are the loader keys the settings ConfigMap also takes
// under their config.yaml names, read only at start. They are absent from the
// shipped file (docs/reference/configuration-advanced.md).
var advancedSettingKeys = []string{
	"GLOBAL_OPT_INTERVAL",
	"PROMETHEUS_METRICS_CACHE_TTL",
	"PROMETHEUS_METRICS_CACHE_CLEANUP_INTERVAL",
	"PROMETHEUS_METRICS_CACHE_FETCH_INTERVAL",
	"PROMETHEUS_METRICS_CACHE_FRESH_THRESHOLD",
	"PROMETHEUS_METRICS_CACHE_STALE_THRESHOLD",
	"PROMETHEUS_METRICS_CACHE_UNAVAILABLE_THRESHOLD",
	"LEARNED_STATE_EPISODE_GAP",
	"LEARNED_STATE_VARIANT_TIMEOUT",
	"LEARNED_STATE_BUCKET_RETENTION",
	"LEARNED_STATE_SNAPSHOT",
}

// Values of the utilizationShare setting.
const (
	UtilizationShareSettingOff    = "off"
	UtilizationShareSettingShadow = "shadow"
	UtilizationShareSettingActive = "active"
)

// SettingsQuotaName is the name of the quota entry the limiter settings build.
// It is the name the installer gives the entry it writes into the policy, so a
// move from one to the other keeps the limiter's name in logs and metrics.
const SettingsQuotaName = "install-quota"

// MaxLogVerbosity bounds the logVerbosity setting. 5 is trace, the most
// verbose level the controller logs at.
const MaxLogVerbosity = 10

// Settings is what the settings ConfigMap holds, parsed and validated. A field
// at its zero value is unset.
type Settings struct {
	// Passive is the passive setting.
	Passive *bool
	// UtilizationShare is off, shadow or active; "" when unset.
	UtilizationShare string
	// Limiters is the limiters list the limiter keys build; nil when the
	// limiter key is unset.
	Limiters []QuotaLimiterConfig
	// Restart holds the settings read only at start, and the advanced keys,
	// by loader key (PROMETHEUS_BASE_URL, V, ...), as written.
	Restart map[string]string
}

// SettingsReport is what reading the settings ConfigMap found wrong.
type SettingsReport struct {
	// Rejected maps each key whose value cannot be used to the reason. The
	// value in force before stays, or the next source down when there was
	// none (Settings.KeepRejected).
	Rejected map[string]string
	// Unknown lists the keys the ConfigMap holds that are no setting, sorted.
	Unknown []string
	// Warnings are values that are valid but have no effect, or a surprising
	// one.
	Warnings []string
}

// legacySettingNames maps config.yaml names an operator may carry over by
// habit to the setting that replaces them, for the unknown-key warning.
var legacySettingNames = map[string]string{
	"PROMETHEUS_BASE_URL":                 SettingPrometheusURL,
	"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY": SettingPrometheusInsecureSkipVerify,
	"LLMSM_PASSIVE":                       SettingPassive,
	"LLMSM_SCALE_TO_ZERO":                 SettingScaleToZero,
	"ENABLE_SCALE_TO_ZERO":                SettingScaleToZero,
	"WATCH_NAMESPACE":                     SettingWatchNamespaces,
	"LLMSM_WATCH_NS":                      SettingWatchNamespaces,
	"V":                                   SettingLogVerbosity,
	"LLMSM_LIMITER":                       SettingLimiter,
	"LLMSM_QUOTAS":                        SettingQuotas,
	"LLMSM_QUOTA_SCOPE":                   SettingQuotaScope,
	"LLMSM_QUOTA_KUEUE":                   SettingKueue,
	"LLMSM_QUOTA_KUEUE_BORROW":            SettingKueueBorrow,
	"KUEUE_NAMESPACE":                     SettingKueueNamespace,
}

// UnknownSettingHint says what an unknown key probably meant: the setting a
// config.yaml or installer name became, or "" when there is no likely one.
func UnknownSettingHint(key string) string {
	if s, ok := legacySettingNames[key]; ok {
		return fmt.Sprintf("%s is now the %s setting", key, s)
	}
	for _, s := range append(slices.Clone(LiveSettingKeys), RestartSettingKeys()...) {
		if strings.EqualFold(s, key) {
			return fmt.Sprintf("setting names are case-sensitive: did you mean %s?", s)
		}
	}
	return ""
}

// ParseSettings reads the settings ConfigMap's data. quotaKey is the
// namespace a namespace-scoped quota is keyed on (Config.SettingsQuotaKey).
//
// Every key is read on its own, so one bad value costs only that key. The
// limiter keys are one group: when any of them is rejected, the limiters list
// is not built.
func ParseSettings(data map[string]string, quotaKey string) (Settings, SettingsReport) {
	s := Settings{Restart: map[string]string{}}
	rep := SettingsReport{Rejected: map[string]string{}}
	known := map[string]bool{}
	for _, k := range LiveSettingKeys {
		known[k] = true
	}
	for k := range restartSettingKeys {
		known[k] = true
	}
	for _, k := range advancedSettingKeys {
		known[k] = true
	}
	for _, k := range slices.Sorted(maps.Keys(data)) {
		if !known[k] {
			rep.Unknown = append(rep.Unknown, k)
		}
	}
	get := func(k string) string { return strings.TrimSpace(data[k]) }
	reject := func(k, format string, a ...any) { rep.Rejected[k] = fmt.Sprintf(format, a...) }

	if v := get(SettingPassive); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			reject(SettingPassive, "%q is not true or false", v)
		} else {
			s.Passive = &b
		}
	}
	if v := get(SettingUtilizationShare); v != "" {
		switch v {
		case UtilizationShareSettingOff, UtilizationShareSettingShadow, UtilizationShareSettingActive:
			s.UtilizationShare = v
		default:
			reject(SettingUtilizationShare, "%q is not off, shadow or active", v)
		}
	}
	parseRestartSettings(data, s.Restart, reject)
	s.Limiters, rep.Warnings = parseLimiterSettings(get, quotaKey, reject)
	return s, rep
}

// parseRestartSettings validates the restart and advanced keys that are set
// and puts each valid one into out, by loader key.
func parseRestartSettings(data map[string]string, out map[string]string, reject func(k, format string, a ...any)) {
	for k, loaderKey := range restartSettingKeys {
		v := strings.TrimSpace(data[k])
		if v == "" {
			continue
		}
		if err := validateRestartSetting(k, v); err != nil {
			reject(k, "%v", err)
			continue
		}
		out[loaderKey] = v
	}
	for _, k := range advancedSettingKeys {
		v := strings.TrimSpace(data[k])
		if v == "" {
			continue
		}
		if k == "LEARNED_STATE_SNAPSHOT" {
			if _, err := strconv.ParseBool(v); err != nil {
				reject(k, "%q is not true or false", v)
				continue
			}
		} else if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			reject(k, "%q is not a positive Go duration with a unit, such as 30s or 168h", v)
			continue
		}
		out[k] = v
	}
}

// validateRestartSetting checks one restart setting's value.
func validateRestartSetting(key, v string) error {
	switch key {
	case SettingPrometheusURL:
		u, err := url.Parse(v)
		if err != nil {
			return fmt.Errorf("%q is not a URL: %w", v, err)
		}
		if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("%q is not an http(s) URL with a host, such as https://prometheus.monitoring.svc:9090", v)
		}
	case SettingPrometheusInsecureSkipVerify, SettingScaleToZero:
		if _, err := strconv.ParseBool(v); err != nil {
			return fmt.Errorf("%q is not true or false", v)
		}
	case SettingWatchNamespaces:
		for _, ns := range ParseWatchNamespaces(v) {
			if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
				return fmt.Errorf("%q is not a namespace name: %s", ns, strings.Join(errs, "; "))
			}
		}
	case SettingLogVerbosity:
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > MaxLogVerbosity {
			return fmt.Errorf("%q is not a whole number from 0 to %d (2 is info, 4 debug, 5 trace)", v, MaxLogVerbosity)
		}
	}
	return nil
}

// parseLimiterSettings builds the limiters list from the limiter keys, or
// returns nil when limiter is unset or a limiter key is rejected.
func parseLimiterSettings(get func(string) string, quotaKey string,
	reject func(k, format string, a ...any)) ([]QuotaLimiterConfig, []string) {
	var warnings []string
	rejected := false
	rej := func(k, format string, a ...any) {
		rejected = true
		reject(k, format, a...)
	}
	parseBool := func(k string, def bool) bool {
		v := get(k)
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			rej(k, "%q is not true or false", v)
			return def
		}
		return b
	}

	var physical, quota bool
	if v := get(SettingLimiter); v != "" {
		for _, t := range strings.Split(v, ",") {
			switch strings.TrimSpace(t) {
			case limiterTypeGPUInventory, string(LimiterTypeInventory):
				physical = true
			case string(LimiterTypeQuota):
				quota = true
			default:
				rej(SettingLimiter, "%q is not gpu-inventory, quota, or both comma-separated (gpu-inventory,quota)", v)
			}
		}
	}
	limiterSet := get(SettingLimiter) != ""

	scope := QuotaScopeNamespace
	if v := get(SettingQuotaScope); v != "" {
		switch QuotaScope(v) {
		case QuotaScopeNamespace, QuotaScopeCluster:
			scope = QuotaScope(v)
		default:
			rej(SettingQuotaScope, "%q is not namespace or cluster", v)
		}
	}
	var caps map[string]int
	if v := get(SettingQuotas); v != "" {
		var err error
		if caps, err = ParseQuotaSetting(v); err != nil {
			rej(SettingQuotas, "%v", err)
		}
		for _, t := range slices.Sorted(maps.Keys(caps)) {
			if caps[t] == 0 {
				warnings = append(warnings, fmt.Sprintf("quotas gives %s a budget of 0, which denies it: no workload on %s scales up. Use -1 for no cap", t, t))
			}
		}
	}
	kueue := parseBool(SettingKueue, false)
	borrow := parseBool(SettingKueueBorrow, true)
	kueueNS := get(SettingKueueNamespace)
	if kueueNS != "" {
		if errs := validation.IsDNS1123Label(kueueNS); len(errs) > 0 {
			rej(SettingKueueNamespace, "%q is not a namespace name: %s", kueueNS, strings.Join(errs, "; "))
		}
	}

	if !limiterSet {
		for _, k := range limiterSettingKeys[1:] {
			if get(k) != "" {
				warnings = append(warnings, fmt.Sprintf("%s is set but limiter is not, so it has no effect: "+
					"the limiters of the scaling policy's default entry stay in force", k))
			}
		}
		return nil, warnings
	}
	if !quota {
		for _, k := range limiterSettingKeys[1:] {
			if get(k) != "" {
				warnings = append(warnings, fmt.Sprintf("%s is set but limiter has no quota, so it has no effect: "+
					"the gpu-inventory limiter takes no budget and reads no Kueue quota", k))
			}
		}
	} else {
		if get(SettingQuotas) == "" {
			rej(SettingQuotas, "limiter %q needs quotas, a budget per accelerator type such as H200=8,A100=4 "+
				"(-1 is no cap on a type). An accelerator a quota does not name gets zero, so there is no safe default", get(SettingLimiter))
		}
		if !kueue {
			for _, k := range []string{SettingKueueBorrow, SettingKueueNamespace} {
				if get(k) != "" {
					warnings = append(warnings, k+" is set but kueue is not true, so it has no effect")
				}
			}
		}
		if scope == QuotaScopeNamespace && quotaKey == "" {
			rej(SettingQuotaScope, "a namespace-scoped quota needs the namespace to key on and none is known")
		}
	}
	if rejected {
		return nil, warnings
	}

	limiters := []QuotaLimiterConfig{}
	if physical {
		limiters = append(limiters, QuotaLimiterConfig{Type: limiterTypeGPUInventory})
	}
	if quota {
		q := QuotaLimiterConfig{Name: SettingsQuotaName, Type: string(LimiterTypeQuota), Scope: scope}
		if scope == QuotaScopeCluster {
			q.ClusterQuotas = caps
		} else {
			q.NamespaceQuotas = map[string]map[string]int{quotaKey: caps}
		}
		if kueue {
			q.Kueue = &KueueQuotaSource{Enabled: true, ControllerNamespace: kueueNS}
			if !borrow {
				f := false
				q.Kueue.CountBorrowable = &f
			}
		}
		limiters = append(limiters, q)
	}
	if err := (&ScalingPolicy{Limiters: limiters}).validateLimiters(); err != nil {
		reject(SettingLimiter, "the limiters these keys build are invalid: %v", err)
		return nil, warnings
	}
	return limiters, warnings
}

// ParseQuotaSetting parses the quotas setting: TYPE=N pairs separated by
// commas or spaces, N a whole number of GPUs from 0 to MaxQuotaValue or -1 for
// no cap on that type.
func ParseQuotaSetting(v string) (map[string]int, error) {
	caps := map[string]int{}
	for _, pair := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		name, value, ok := strings.Cut(pair, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("%q is not TYPE=N, for example H200=8", pair)
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < QuotaUnlimited {
			return nil, fmt.Errorf("%q: the budget must be a whole number of GPUs, or -1 for no cap on that type", pair)
		}
		if n > MaxQuotaValue {
			return nil, fmt.Errorf("%q exceeds the maximum quota of %d GPUs", pair, MaxQuotaValue)
		}
		if _, dup := caps[name]; dup {
			return nil, fmt.Errorf("%q is named twice; give each accelerator type one budget", name)
		}
		caps[name] = n
	}
	if len(caps) == 0 {
		return nil, fmt.Errorf("%q names no accelerator; a quota with no budget is zero for every type", v)
	}
	return caps, nil
}

// KeepRejected returns s with every rejected key's value taken from prev, the
// settings in force before: a typo keeps the last good value instead of
// dropping to the next source down. With no previous value the key stays
// unset. The limiter keys are kept as one.
func (s Settings) KeepRejected(prev Settings, rejected map[string]string) Settings {
	if len(rejected) == 0 {
		return s
	}
	out := s
	out.Restart = maps.Clone(s.Restart)
	if out.Restart == nil {
		out.Restart = map[string]string{}
	}
	if _, bad := rejected[SettingPassive]; bad {
		out.Passive = prev.Passive
	}
	if _, bad := rejected[SettingUtilizationShare]; bad {
		out.UtilizationShare = prev.UtilizationShare
	}
	for _, k := range limiterSettingKeys {
		if _, bad := rejected[k]; bad {
			out.Limiters = prev.Limiters
			break
		}
	}
	for k := range rejected {
		loaderKey, ok := restartSettingKeys[k]
		if !ok && slices.Contains(advancedSettingKeys, k) {
			loaderKey, ok = k, true
		}
		if !ok {
			continue
		}
		if v, had := prev.Restart[loaderKey]; had {
			out.Restart[loaderKey] = v
		} else {
			delete(out.Restart, loaderKey)
		}
	}
	return out
}

// clone returns a deep copy of s.
func (s Settings) clone() Settings {
	out := s
	if s.Passive != nil {
		v := *s.Passive
		out.Passive = &v
	}
	if s.Limiters != nil {
		out.Limiters = make([]QuotaLimiterConfig, 0, len(s.Limiters))
		for _, l := range s.Limiters {
			out.Limiters = append(out.Limiters, l.clone())
		}
	}
	out.Restart = maps.Clone(s.Restart)
	return out
}

// applySettings returns policy with the live settings folded into its
// "default" entry: each setting that is set replaces that field of the entry,
// and one that is unset leaves it as written. Per-model and tier entries are
// untouched, so they still win where they set the field. budget says whether
// the limiters and the optimizer are read from this policy: with a separate
// policy namespace they are not, and the settings do not move them.
//
// policy itself is not modified.
func applySettings(policy ScalingPolicySet, s Settings, budget bool) ScalingPolicySet {
	touches := s.Passive != nil || (budget && (s.Limiters != nil || s.UtilizationShare != ""))
	if !touches {
		return policy
	}
	out := make(ScalingPolicySet, len(policy)+1)
	maps.Copy(out, policy)
	def := out[GlobalDefaultsKey]
	if s.Passive != nil {
		v := *s.Passive
		def.Passive = &v
	}
	if budget {
		if s.Limiters != nil {
			def.Limiters = make([]QuotaLimiterConfig, 0, len(s.Limiters))
			for _, l := range s.Limiters {
				def.Limiters = append(def.Limiters, l.clone())
			}
		}
		switch s.UtilizationShare {
		case UtilizationShareSettingOff:
			def.Optimizer = nil
		case UtilizationShareSettingShadow, UtilizationShareSettingActive:
			// The rest of the block as written stays: its tuning, and its
			// decode error if it has one, which makes the mode invalid
			// rather than silently untuned.
			opt := OptimizerConfig{}
			if def.Optimizer != nil {
				opt = *def.Optimizer
			}
			opt.Type = OptimizerTypeUtilizationShare
			us := UtilizationShareConfig{}
			if opt.UtilizationShare != nil {
				us = *opt.UtilizationShare
			}
			us.Shadow = s.UtilizationShare == UtilizationShareSettingShadow
			opt.UtilizationShare = &us
			def.Optimizer = &opt
		}
	}
	out[GlobalDefaultsKey] = def
	return out
}

// SettingsOverridePolicy describes, one string per field, where a live setting
// replaces a different value in the policy's "default" entry as written:
// "passive: settings true, policy false". Empty when no setting does.
func SettingsOverridePolicy(def ScalingPolicy, s Settings, budget bool) []string {
	var out []string
	if s.Passive != nil && def.Passive != nil && *s.Passive != *def.Passive {
		out = append(out, fmt.Sprintf("passive: settings %t, policy %t", *s.Passive, *def.Passive))
	}
	if !budget {
		return out
	}
	if s.UtilizationShare != "" && def.Optimizer != nil {
		policyMode := UtilizationShareSettingOff
		if def.Optimizer.Type == OptimizerTypeUtilizationShare {
			policyMode = UtilizationShareSettingActive
			if def.Optimizer.UtilizationShare != nil && def.Optimizer.UtilizationShare.Shadow {
				policyMode = UtilizationShareSettingShadow
			}
		}
		if policyMode != s.UtilizationShare {
			out = append(out, fmt.Sprintf("utilizationShare: settings %s, policy %s", s.UtilizationShare, policyMode))
		}
	}
	if len(s.Limiters) > 0 && len(def.Limiters) > 0 && !reflect.DeepEqual(s.Limiters, def.Limiters) {
		out = append(out, fmt.Sprintf("limiters: settings %s, policy %s",
			limiterSummary(s.Limiters), limiterSummary(def.Limiters)))
	}
	return out
}

// limiterSummary names a limiters list as "<types>/<names>", the form the
// controller status uses (Config.EffectiveLimiterSummary).
func limiterSummary(limiters []QuotaLimiterConfig) string {
	types := limiterTypesOf(limiters)
	names := make([]string, 0, len(limiters))
	for _, l := range limiters {
		name := l.Name
		if name == "" {
			name = l.Type
		}
		names = append(names, name)
	}
	return types.String() + "/" + strings.Join(names, ",")
}
