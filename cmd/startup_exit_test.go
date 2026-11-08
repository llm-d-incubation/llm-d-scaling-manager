package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
)

const testStep = "CRITICAL: Failed to connect to Prometheus"

// A SIGTERM during a startup step cancels the step. That must log an info
// line, not report the step as failed, and exit 143, not 0: the startup probe
// sends the same SIGTERM to a controller stuck waiting for Prometheus, and 0
// would show that outage as "Completed". Anything else -- a real failure, with
// or without a shutdown under way -- exits 1 with the step's error.
func TestStartupExitCode(t *testing.T) {
	if exitCodeTerminated != 128+15 {
		t.Fatalf("exitCodeTerminated = %d, want 143 (128 + SIGTERM)", exitCodeTerminated)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()

	cases := []struct {
		name     string
		ctx      context.Context
		err      error
		wantCode int
	}{
		{
			name:     "shutdown cancels the step",
			ctx:      cancelled,
			err:      fmt.Errorf("listing namespaces: %w", context.Canceled),
			wantCode: exitCodeTerminated,
		},
		{
			name:     "the context's deadline ends the step",
			ctx:      expired,
			err:      fmt.Errorf("query: %w", context.DeadlineExceeded),
			wantCode: exitCodeTerminated,
		},
		{
			name:     "real failure, no shutdown",
			ctx:      context.Background(),
			err:      errors.New("connection refused"),
			wantCode: 1,
		},
		{
			// The step failed for its own reason; that the signal arrived at
			// the same moment does not make it a clean shutdown.
			name:     "real failure while shutting down",
			ctx:      cancelled,
			err:      errors.New("namespaces is forbidden"),
			wantCode: 1,
		},
		{
			// A cancellation that is not this context's: some inner context
			// was cancelled while the signal context is still live.
			name:     "inner cancellation, no shutdown",
			ctx:      context.Background(),
			err:      fmt.Errorf("query: %w", context.Canceled),
			wantCode: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			log := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
			if code := startupExitCode(tc.ctx, log, tc.err, testStep); code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
			if len(lines) != 1 {
				t.Fatalf("log = %q, want one line", lines)
			}
			if shutdown := strings.Contains(lines[0], "Shutdown requested during startup"); shutdown != (tc.wantCode == exitCodeTerminated) {
				t.Errorf("log = %q: shutdown line = %v, want %v", lines[0], shutdown, tc.wantCode == exitCodeTerminated)
			}
			if !strings.Contains(lines[0], testStep) {
				t.Errorf("log = %q, want the step named", lines[0])
			}
		})
	}
}
