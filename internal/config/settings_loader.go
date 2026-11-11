package config

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"
	ctrl "sigs.k8s.io/controller-runtime"
)

// loadSettingsLayer puts the settings ConfigMap's restart and advanced keys
// above every other source in v, and records what the controller needs later
// to say where each value came from. data is the ConfigMap's data, nil when
// there is none; fileRead says config.yaml was read.
func loadSettingsLayer(v *viper.Viper, flagSet *flag.FlagSet, data map[string]string, fileRead bool) settingsState {
	st := settingsState{
		present:      data != nil,
		atStart:      map[string]string{},
		otherAtStart: map[string]string{},
		sources:      map[string]string{},
		fileRead:     fileRead,
	}
	restart := map[string]string{}
	parseRestartSettings(data, restart, func(k, format string, a ...any) {
		ctrl.Log.Error(fmt.Errorf(format, a...), "Settings value rejected; it is read only at start, so the value "+
			"from config.yaml, the environment, a flag or the default applies until it is fixed and the controller restarted",
			"configMap", SettingsConfigMapName(), "key", k)
	})
	for _, k := range settingsLoaderKeys() {
		st.otherAtStart[k] = v.GetString(k)
	}
	for name, loaderKey := range restartSettingKeys {
		st.sources[name] = layerSource(v, flagSet, loaderKey)
	}
	st.sources[SettingPassive] = layerSource(v, flagSet, "LLMSM_PASSIVE")
	for _, k := range slices.Sorted(maps.Keys(restart)) {
		val := restart[k]
		if src := layerSource(v, flagSet, k); src != SettingSourceDefault && !sameSettingValue(v.GetString(k), val) {
			ctrl.Log.Info("The settings ConfigMap overrides a value set elsewhere", "configMap", SettingsConfigMapName(),
				"key", k, "settingsValue", val, "overridden", v.GetString(k), "overriddenSource", src)
		}
		v.Set(k, val)
		st.atStart[k] = val
	}
	for name, loaderKey := range restartSettingKeys {
		if _, ok := restart[loaderKey]; ok {
			st.sources[name] = SettingSourceSettings
		}
	}
	if fileRead {
		st.fileContributes, st.filePassive = managerConfigContributes(v, flagSet, restart)
	}
	return st
}

// settingsLoaderKeys are the loader keys the settings ConfigMap can set.
func settingsLoaderKeys() []string {
	keys := slices.Clone(advancedSettingKeys)
	for _, k := range restartSettingKeys {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// layerSource says which source below the settings gives loader key its
// value in v: a flag given on the command line, the environment, config.yaml
// or the default.
func layerSource(v *viper.Viper, flagSet *flag.FlagSet, key string) string {
	if flagName, ok := flagBindings[key]; ok && flagSet != nil {
		if f := flagSet.Lookup(flagName); f != nil && f.Changed {
			return SettingSourceFlag
		}
	}
	if os.Getenv(key) != "" {
		return SettingSourceEnv
	}
	if v.InConfig(key) {
		return SettingSourceManagerConfig
	}
	return SettingSourceDefault
}

// managerConfigContributes lists the config.yaml keys whose value is in force
// (no setting, flag or environment variable overrides it) and differs from
// the default, LLMSM_PASSIVE apart: whether that one is in force depends on
// the live passive setting, so it is reported on its own.
func managerConfigContributes(v *viper.Viper, flagSet *flag.FlagSet, settings map[string]string) ([]string, bool) {
	defaults := viper.New()
	setLoaderDefaults(defaults)
	var keys []string
	passive := false
	for _, lower := range v.AllKeys() {
		key := strings.ToUpper(lower)
		if !v.InConfig(key) {
			continue
		}
		if _, overridden := settings[key]; overridden {
			continue
		}
		if layerSource(v, flagSet, key) != SettingSourceManagerConfig {
			continue
		}
		if sameSettingValue(v.GetString(key), defaults.GetString(key)) {
			continue
		}
		if key == "LLMSM_PASSIVE" {
			passive = true
			continue
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys, passive
}

// sameSettingValue reports whether two values as written mean the same: equal
// strings, or equal as durations, booleans or integers ("1m" and "1m0s",
// "false" and "0").
func sameSettingValue(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return true
	}
	if da, err := time.ParseDuration(a); err == nil {
		if db, err := time.ParseDuration(b); err == nil {
			return da == db
		}
	}
	if ia, err := strconv.Atoi(a); err == nil {
		if ib, err := strconv.Atoi(b); err == nil {
			return ia == ib
		}
	}
	if ba, err := strconv.ParseBool(a); err == nil {
		if bb, err := strconv.ParseBool(b); err == nil {
			return ba == bb
		}
	}
	return false
}
