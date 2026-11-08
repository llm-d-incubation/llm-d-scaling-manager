package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// runUntilNil runs executeWithRetry with an OptimizeFunc that returns errs in
// order and then nil, and returns the lines logged at the default level.
func runUntilNil(t *testing.T, errs ...error) []string {
	t.Helper()
	var lines []string
	logger := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{Verbosity: 0})
	ctx := log.IntoContext(context.Background(), logger)

	calls := 0
	e := NewPollingExecutor(PollingConfig{
		Config: Config{OptimizeFunc: func(context.Context) error {
			calls++
			if calls <= len(errs) {
				return errs[calls-1]
			}
			return nil
		}},
		RetryBackoff: time.Millisecond,
	})
	e.executeWithRetry(ctx)
	if calls != len(errs)+1 {
		t.Fatalf("OptimizeFunc ran %d times, want %d: a reported error must still be retried", calls, len(errs)+1)
	}
	return lines
}

func TestReportedErrorIsRetriedWithoutAnOptimizationErrorLine(t *testing.T) {
	reported := Reported(errors.New("pod epp-0 returned status 500"))
	lines := runUntilNil(t, reported, reported, reported)
	for _, l := range lines {
		if strings.Contains(l, "Optimization error") {
			t.Fatalf("a reported error was logged at the default level: %s", l)
		}
	}
}

func TestUnreportedErrorStillLogsAnOptimizationError(t *testing.T) {
	cause := errors.New("boom")
	lines := runUntilNil(t,
		cause,
		errors.Join(Reported(errors.New("known")), cause), // wrapping a reported error does not quiet it
		fmt.Errorf("wrapped: %w", Reported(cause)),
	)
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "Optimization error") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("got %d optimization error lines, want 3: %q", n, lines)
	}
}

func TestReportedKeepsTheCause(t *testing.T) {
	cause := errors.New("boom")
	err := Reported(cause)
	if !errors.Is(err, cause) || err.Error() != "boom" || !IsReported(err) {
		t.Fatalf("Reported(%v) = %v, IsReported %v", cause, err, IsReported(err))
	}
	if Reported(nil) != nil {
		t.Fatal("Reported(nil) must be nil")
	}
}
