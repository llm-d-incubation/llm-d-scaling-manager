package logging

import (
	"strings"

	"github.com/go-logr/logr"
	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	DEFAULT = 2
	VERBOSE = 3
	DEBUG   = 4
	TRACE   = 5
)

// LogLevelEnv is the environment variable that names the level the controller
// logs at: error, warn, info, debug or trace. The shipped Deployment sets it to
// info, the level --v=2 logs at.
const LogLevelEnv = "LOG_LEVEL"

// levelNames maps a LOG_LEVEL name to its zap level. info, debug and trace are
// the levels --v=2, --v=4 and --v=5 log at (logr's V(n) is zap level -n).
// logr has no warning call -- every line is Info or Error -- so warn and error
// log the same: only the errors.
var levelNames = map[string]zapcore.Level{
	"error":   zapcore.ErrorLevel,
	"warn":    zapcore.WarnLevel,
	"warning": zapcore.WarnLevel,
	"info":    zapcore.Level(-DEFAULT),
	"debug":   zapcore.Level(-DEBUG),
	"trace":   zapcore.Level(-TRACE),
}

// LevelFromName returns the zap level a LOG_LEVEL value names, ignoring case
// and surrounding space. ok is false for a name it does not know.
func LevelFromName(name string) (zapcore.Level, bool) {
	lvl, ok := levelNames[strings.ToLower(strings.TrimSpace(name))]
	return lvl, ok
}

// Level resolves the level the controller logs at when --zap-log-level is not
// given: an explicit --v (verbositySet) wins, then LOG_LEVEL (logLevel), then
// --v's default (verbosity). A LOG_LEVEL it does not know is ignored, and
// ignored reports so, for the caller to log once a logger exists.
func Level(verbosity int, verbositySet bool, logLevel string) (lvl zapcore.Level, ignored bool) {
	if !verbositySet && strings.TrimSpace(logLevel) != "" {
		if named, ok := LevelFromName(logLevel); ok {
			return named, false
		}
		ignored = true
	}
	// See https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/log/zap#Options.Level
	return zapcore.Level(int8(-1 * verbosity)), ignored
}

// InitLogging initializes the controller-runtime logger with zap backend.
//
// The level comes from, in order: --zap-log-level (BindFlags sets opts.Level
// only when it is given), an explicit --v, the LOG_LEVEL environment variable
// (logLevel), and --v's default. See Level.
func InitLogging(opts *zap.Options, verbosity int, verbositySet bool, logLevel string) {
	ctrl.SetLogger(newLogger(opts, verbosity, verbositySet, logLevel))
}

// newLogger builds the logger InitLogging installs. It is separate because
// ctrl.SetLogger takes effect once per process, and the precedence is tested
// on more than one logger.
func newLogger(opts *zap.Options, verbosity int, verbositySet bool, logLevel string) logr.Logger {
	ignored := false
	if opts.Level == nil {
		var lvl zapcore.Level
		lvl, ignored = Level(verbosity, verbositySet, logLevel)
		opts.Level = uberzap.NewAtomicLevelAt(lvl)
	}

	logger := zap.New(zap.UseFlagOptions(opts), zap.RawZapOpts(uberzap.AddCaller()))
	if ignored {
		logger.WithName("setup").Info("Ignoring "+LogLevelEnv+": not a level it knows; logging at --v",
			LogLevelEnv, logLevel, "v", verbosity, "known", "error, warn, info, debug, trace")
	}
	return logger
}

// SetVerbosity changes the level of the logger InitLogging built from opts to
// --v=verbosity. It reports false, and changes nothing, when opts holds no
// level that can be changed in place.
func SetVerbosity(opts *zap.Options, verbosity int) bool {
	lvl, ok := opts.Level.(uberzap.AtomicLevel)
	if !ok {
		return false
	}
	lvl.SetLevel(zapcore.Level(int8(-1 * verbosity)))
	return true
}

// ApplySettingsVerbosity applies the settings ConfigMap's logVerbosity at
// start: it wins over --v and LOG_LEVEL, as every setting wins over the
// Deployment, and --zap-log-level (zapLevelGiven), a development flag, wins
// over it. It reports whether the level was changed.
func ApplySettingsVerbosity(opts *zap.Options, verbosity int, zapLevelGiven bool) bool {
	if zapLevelGiven {
		return false
	}
	return SetVerbosity(opts, verbosity)
}

// Sync flushes any buffered log entries.
func Sync() error {
	logger := ctrl.Log.WithName("logger-sync").GetSink()
	if syncer, ok := logger.(interface{ Sync() error }); ok {
		return syncer.Sync()
	}
	return nil
}
