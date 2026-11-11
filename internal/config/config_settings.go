package config

import (
	"maps"
	"slices"
	"strings"
)

// Where a setting's value in force came from, as SettingsSources reports it.
const (
	// SettingSourceSettings is the settings ConfigMap.
	SettingSourceSettings = "settings"
	// SettingSourceManagerConfig is config.yaml of the manager ConfigMap
	// (scaling-manager-manager-config), read for one release for a key the
	// settings ConfigMap does not set.
	SettingSourceManagerConfig = "manager-config"
	// SettingSourcePolicy is the "default" entry of the scaling-policy
	// ConfigMap in the controller's namespace.
	SettingSourcePolicy = "policy"
	// SettingSourcePolicyNamespace is the scaling-policy ConfigMap of a
	// separate policy namespace, which the settings do not override.
	SettingSourcePolicyNamespace = "policy-namespace"
	// SettingSourceFlag is a command-line flag of the controller.
	SettingSourceFlag = "flag"
	// SettingSourceEnv is an environment variable of the controller.
	SettingSourceEnv = "env"
	// SettingSourceDefault is the built-in default.
	SettingSourceDefault = "default"
)

// Values of SettingsSource: where the controller's settings come from as a
// whole.
const (
	// SettingsSourceSettingsOnly: the settings ConfigMap exists and the
	// manager ConfigMap contributes no value that differs from a default, so
	// deleting the manager ConfigMap changes nothing.
	SettingsSourceSettingsOnly = "settings"
	// SettingsSourceBoth: the settings ConfigMap exists and some values still
	// come from the manager ConfigMap (SettingsFromManagerConfig names them).
	SettingsSourceBoth = "settings+manager-config"
	// SettingsSourceManagerConfig: there is no settings ConfigMap; the manager
	// ConfigMap's config.yaml is read.
	SettingsSourceManagerConfig = "manager-config"
	// SettingsSourceDefaults: neither ConfigMap is read.
	SettingsSourceDefaults = "defaults"
)

// settingsState is what the controller knows of the settings ConfigMap.
type settingsState struct {
	// present: the settings ConfigMap exists (as last read).
	present bool
	// resourceVersion of the settings ConfigMap last applied; "" when absent.
	resourceVersion string
	// live is the settings in force: the last read, with rejected values
	// kept from the read before (Settings.KeepRejected).
	live Settings
	// report is what the last read found wrong.
	report SettingsReport

	// The rest is set once, by the loader.

	// atStart is the restart and advanced settings in force at start, by
	// loader key.
	atStart map[string]string
	// otherAtStart is the value each restart and advanced loader key had at
	// start from the flags, the environment, config.yaml and the defaults:
	// what it falls back to without the setting.
	otherAtStart map[string]string
	// sources is where each restart setting, and the passive flag below the
	// policy, came from at start; by setting name.
	sources map[string]string
	// fileRead: config.yaml was read at start.
	fileRead bool
	// fileContributes lists the loader keys whose value in force came from
	// config.yaml and differs from the default, LLMSM_PASSIVE excepted.
	fileContributes []string
	// filePassive: config.yaml's LLMSM_PASSIVE is in force below the
	// settings and differs from the default.
	filePassive bool
	// deploymentWatchNamespace is the watched namespaces the Deployment
	// names (flag, environment, config.yaml), normalized, before the
	// watchNamespaces setting narrows them.
	deploymentWatchNamespace string
}

// DeploymentWatchNamespace returns the namespaces the controller's Deployment
// names to watch (--watch-namespace, WATCH_NAMESPACE or config.yaml),
// normalized, whatever the watchNamespaces setting narrows them to; "" for
// every namespace. Decisions about who owns the controller are made from this
// value, never from the setting. Thread-safe.
func (c *Config) DeploymentWatchNamespace() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.settings.atStart == nil {
		// Not built by the loader (a test config): nothing narrowed it.
		return c.infrastructure.watchNamespace
	}
	return c.settings.deploymentWatchNamespace
}

// RestrictSettings rejects, in s and rep, the settings whose value is valid on
// its own but not for this controller: a watchNamespaces that is not a subset
// of the namespaces its Deployment names. Thread-safe.
func (c *Config) RestrictSettings(s *Settings, rep *SettingsReport) {
	ws, ok := s.Restart["WATCH_NAMESPACE"]
	if !ok {
		return
	}
	if err := NarrowsWatchNamespaces(c.DeploymentWatchNamespace(), ws); err != nil {
		delete(s.Restart, "WATCH_NAMESPACE")
		if rep.Rejected == nil {
			rep.Rejected = map[string]string{}
		}
		rep.Rejected[SettingWatchNamespaces] = err.Error()
	}
}

// SettingsQuotaKey is the namespace a namespace-scoped quota built from the
// settings is keyed on: the watched namespace when the controller watches
// exactly one, else `default`, the reserved key that gives each namespace not
// listed its own budget. This is the key the installer chooses.
// Thread-safe.
func (c *Config) SettingsQuotaKey() string {
	if ns := c.WatchNamespaces(); len(ns) == 1 {
		return ns[0]
	}
	return "default"
}

// ApplySettings puts settings in force: the live keys are folded into the
// global "default" entry at once. present says the settings ConfigMap exists,
// at resourceVersion; report is what reading it found wrong.
// Thread-safe.
func (c *Config) ApplySettings(s Settings, present bool, resourceVersion string, report SettingsReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settings.live = s.clone()
	c.settings.present = present
	c.settings.resourceVersion = resourceVersion
	c.settings.report = SettingsReport{
		Rejected: maps.Clone(report.Rejected),
		Unknown:  slices.Clone(report.Unknown),
		Warnings: slices.Clone(report.Warnings),
	}
	c.refoldGlobalLocked()
}

// LiveSettings returns the settings in force. Thread-safe.
func (c *Config) LiveSettings() Settings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.settings.live.clone()
}

// SettingsPresent reports whether the settings ConfigMap exists, and the
// resourceVersion last applied. Thread-safe.
func (c *Config) SettingsPresent() (present bool, resourceVersion string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.settings.present, c.settings.resourceVersion
}

// SettingsReport returns what the last read of the settings ConfigMap found
// wrong. Thread-safe.
func (c *Config) SettingsReport() SettingsReport {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r := c.settings.report
	return SettingsReport{Rejected: maps.Clone(r.Rejected), Unknown: slices.Clone(r.Unknown),
		Warnings: slices.Clone(r.Warnings)}
}

// SettingsBudgetApplies reports whether the limiter and utilizationShare
// settings take effect: they do unless a separate policy namespace holds the
// cluster's limiters, which a setting in the controller's own namespace must
// not override (whoever writes it could otherwise raise their own quota).
// Thread-safe.
func (c *Config) SettingsBudgetApplies() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return !c.policyNamespaceIsSeparateLocked()
}

// policyNamespaceIsSeparateLocked is PolicyNamespaceIsSeparate for a caller
// holding c.mu.
func (c *Config) policyNamespaceIsSeparateLocked() bool {
	return c.saturation.policyNamespace != "" && c.saturation.policyNamespace != SystemNamespace()
}

// refoldGlobalLocked rebuilds the global set from the policy as written and
// the settings in force. Caller holds c.mu for writing.
func (c *Config) refoldGlobalLocked() {
	c.saturation.global = applySettings(c.saturation.globalPolicy, c.settings.live,
		!c.policyNamespaceIsSeparateLocked())
}

// SettingsRestartChanged returns the restart and advanced settings whose
// value in s differs from the one in force since start, by setting name,
// sorted: a change that takes effect only when the controller restarts.
// Thread-safe.
func (c *Config) SettingsRestartChanged(s Settings) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	effective := func(set map[string]string, k string) string {
		if v, ok := set[k]; ok {
			return v
		}
		return c.settings.otherAtStart[k]
	}
	var out []string
	for name, loaderKey := range restartSettingKeys {
		if !sameSettingValue(effective(s.Restart, loaderKey), effective(c.settings.atStart, loaderKey)) {
			out = append(out, name)
		}
	}
	for _, k := range advancedSettingKeys {
		if !sameSettingValue(effective(s.Restart, k), effective(c.settings.atStart, k)) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// SettingsAtStart returns the restart setting a loader key was given by the
// settings ConfigMap at start, and whether it was. Thread-safe.
func (c *Config) SettingsAtStart(loaderKey string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.settings.atStart[loaderKey]
	return v, ok
}

// SettingsSources returns, for each setting, where its value in force comes
// from: one of the SettingSource* values.
// Thread-safe.
func (c *Config) SettingsSources() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := map[string]string{}
	for name := range restartSettingKeys {
		src := c.settings.sources[name]
		if src == "" {
			src = SettingSourceDefault
		}
		out[name] = src
	}
	def := c.saturation.globalPolicy[GlobalDefaultsKey]
	switch {
	case c.settings.live.Passive != nil:
		out[SettingPassive] = SettingSourceSettings
	case def.Passive != nil:
		out[SettingPassive] = SettingSourcePolicy
	default:
		out[SettingPassive] = c.settings.sources[SettingPassive]
		if out[SettingPassive] == "" {
			out[SettingPassive] = SettingSourceDefault
		}
	}
	separate := c.policyNamespaceIsSeparateLocked()
	budgetSource := func(fromSettings, inPolicy bool) string {
		switch {
		case separate:
			return SettingSourcePolicyNamespace
		case fromSettings:
			return SettingSourceSettings
		case inPolicy:
			return SettingSourcePolicy
		}
		return SettingSourceDefault
	}
	out[SettingUtilizationShare] = budgetSource(c.settings.live.UtilizationShare != "", def.Optimizer != nil)
	limiterSrc := budgetSource(c.settings.live.Limiters != nil, len(def.Limiters) > 0)
	for _, k := range limiterSettingKeys {
		out[k] = limiterSrc
	}
	return out
}

// SettingsFromManagerConfig lists the config.yaml keys of the manager
// ConfigMap whose value is in force and differs from the default, sorted:
// what deleting that ConfigMap would change. Empty when nothing would.
// Thread-safe.
func (c *Config) SettingsFromManagerConfig() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := slices.Clone(c.settings.fileContributes)
	if c.settings.filePassive && c.settings.live.Passive == nil {
		out = append(out, "LLMSM_PASSIVE")
	}
	slices.Sort(out)
	return out
}

// SettingsSource says where the controller's settings come from as a whole:
// one of the SettingsSource* values.
// Thread-safe.
func (c *Config) SettingsSource() string {
	fallback := len(c.SettingsFromManagerConfig()) > 0
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch {
	case c.settings.present && !fallback:
		return SettingsSourceSettingsOnly
	case c.settings.present:
		return SettingsSourceBoth
	case c.settings.fileRead:
		return SettingsSourceManagerConfig
	}
	return SettingsSourceDefaults
}

// SettingsSourcesText renders SettingsSources as "key=source" pairs, sorted
// by key and joined by ",".
func SettingsSourcesText(sources map[string]string) string {
	parts := make([]string, 0, len(sources))
	for _, k := range slices.Sorted(maps.Keys(sources)) {
		parts = append(parts, k+"="+sources[k])
	}
	return strings.Join(parts, ",")
}
