package logging

import (
	goflag "flag"
	"testing"

	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// The settings ConfigMap's logVerbosity, applied at start: it changes the
// level the logger was built with (above --v and LOG_LEVEL), except under
// --zap-log-level, which wins.
func TestSettingsLogVerbosity(t *testing.T) {
	level := func(opts *zap.Options) zapcore.Level {
		t.Helper()
		al, ok := opts.Level.(uberzap.AtomicLevel)
		if !ok {
			t.Fatalf("level %T is not atomic", opts.Level)
		}
		return al.Level()
	}

	// Built from --v=2 and LOG_LEVEL=info, as the shipped Deployment does.
	opts := &zap.Options{}
	newLogger(opts, DEFAULT, false, "info")
	if !ApplySettingsVerbosity(opts, TRACE, false) || level(opts) != zapcore.Level(-TRACE) {
		t.Errorf("level %v, want the setting's %v", level(opts), zapcore.Level(-TRACE))
	}
	if !ApplySettingsVerbosity(opts, 0, false) || level(opts) != zapcore.InfoLevel {
		t.Errorf("level %v, want info for logVerbosity 0", level(opts))
	}

	// --zap-log-level given: it wins, and the level is left as it set it.
	fs := goflag.NewFlagSet("zap", goflag.ContinueOnError)
	opts = &zap.Options{}
	opts.BindFlags(fs)
	if err := fs.Parse([]string{"--zap-log-level=error"}); err != nil {
		t.Fatal(err)
	}
	newLogger(opts, DEFAULT, false, "")
	if ApplySettingsVerbosity(opts, TRACE, true) || level(opts) != zapcore.ErrorLevel {
		t.Errorf("level %v, want --zap-log-level's error", level(opts))
	}

	// A level that cannot be changed in place is reported, not panicked on.
	if SetVerbosity(&zap.Options{Level: zapcore.InfoLevel}, TRACE) {
		t.Error("SetVerbosity claimed to change a fixed level")
	}
}
