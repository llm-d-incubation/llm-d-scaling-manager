package logging

import (
	"testing"

	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// LOG_LEVEL was set on the shipped Deployment and read by no code: an operator
// who set it to debug got the same log as before. These pin that it decides the
// level, and where it sits among the flags that also do.
func TestLevelHonoursLogLevel(t *testing.T) {
	cases := []struct {
		name         string
		verbosity    int
		verbositySet bool
		logLevel     string
		want         int // the most verbose V() that is logged; -1 when Info itself is not
		ignored      bool
	}{
		{name: "unset keeps the --v default", verbosity: DEFAULT, want: DEFAULT},
		{name: "info is the shipped level", verbosity: DEFAULT, logLevel: "info", want: DEFAULT},
		{name: "debug", verbosity: DEFAULT, logLevel: "debug", want: DEBUG},
		{name: "trace", verbosity: DEFAULT, logLevel: "trace", want: TRACE},
		{name: "case and space are ignored", verbosity: DEFAULT, logLevel: " DEBUG ", want: DEBUG},
		{name: "warn logs no info line", verbosity: DEFAULT, logLevel: "warn", want: -1},
		{name: "error logs no info line", verbosity: DEFAULT, logLevel: "error", want: -1},
		{name: "an explicit --v beats LOG_LEVEL", verbosity: VERBOSE, verbositySet: true, logLevel: "debug", want: VERBOSE},
		{name: "an unknown name falls back to --v", verbosity: DEFAULT, logLevel: "loud", want: DEFAULT, ignored: true},
		{name: "an unknown name under an explicit --v is not reported", verbosity: VERBOSE, verbositySet: true,
			logLevel: "loud", want: VERBOSE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lvl, ignored := Level(tc.verbosity, tc.verbositySet, tc.logLevel)
			assert.Equal(t, tc.ignored, ignored)

			// On a real sink, so the sign of the level cannot be backwards.
			core, logs := observer.New(lvl)
			logger := zapr.NewLogger(uberzap.New(core))
			for v := 0; v <= TRACE; v++ {
				logger.V(v).Info("line", "v", v)
			}
			got := -1
			for _, e := range logs.All() {
				if v := int(e.ContextMap()["v"].(int64)); v > got {
					got = v
				}
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// InitLogging is what the controller calls: the level must reach ctrl.Log, and
// --zap-log-level must still beat everything. ctrl.SetLogger takes effect once
// per process, so InitLogging is called once and the rest is checked on
// newLogger.
func TestInitLoggingAppliesLogLevel(t *testing.T) {
	InitLogging(&zap.Options{}, DEFAULT, false, "debug")
	assert.True(t, ctrl.Log.V(DEBUG).Enabled(), "LOG_LEVEL=debug must enable V(%d)", DEBUG)
	assert.False(t, ctrl.Log.V(TRACE).Enabled())

	plain := newLogger(&zap.Options{}, DEFAULT, false, "")
	assert.True(t, plain.V(DEFAULT).Enabled())
	assert.False(t, plain.V(DEFAULT+1).Enabled(), "no LOG_LEVEL keeps the --v default")

	zapFlag := newLogger(&zap.Options{Level: zapcore.ErrorLevel}, DEFAULT, true, "trace")
	assert.False(t, zapFlag.V(0).Enabled(), "--zap-log-level beats --v and LOG_LEVEL")
}

func TestLevelFromNameRefusesUnknown(t *testing.T) {
	_, ok := LevelFromName("verbose")
	assert.False(t, ok)
	_, ok = LevelFromName("")
	assert.False(t, ok)
}
