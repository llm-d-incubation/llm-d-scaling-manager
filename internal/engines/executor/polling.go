/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package executor

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
)

// Reported marks err as one its source has already logged, once per change of
// state. The executor still retries it exactly as any other error, but logs it
// at debug level instead of as an optimization error on every attempt. Only an
// error returned as Reported itself is quieted, never one that merely wraps or
// joins a Reported error, so a new failure joined to a known one stays loud.
func Reported(err error) error {
	if err == nil {
		return nil
	}
	return reportedError{err: err}
}

// reportedError is the error Reported returns.
type reportedError struct{ err error }

func (r reportedError) Error() string { return r.err.Error() }
func (r reportedError) Unwrap() error { return r.err }

// IsReported reports whether err was returned by Reported. It looks at err
// itself only, not at the errors it wraps.
func IsReported(err error) bool {
	_, ok := err.(reportedError)
	return ok
}

// PollingExecutor executes the optimization function at fixed intervals.
type PollingExecutor struct {
	config       Config
	interval     time.Duration // polling interval
	retryBackoff time.Duration // backoff duration between retries
}

// PollingConfig holds polling-specific configuration.
type PollingConfig struct {
	Config
	Interval     time.Duration
	RetryBackoff time.Duration
}

// NewPollingExecutor creates a new polling executor.
func NewPollingExecutor(config PollingConfig) *PollingExecutor {
	return &PollingExecutor{
		config:       config.Config,
		interval:     config.Interval,
		retryBackoff: config.RetryBackoff,
	}
}

func (e *PollingExecutor) Start(ctx context.Context) {
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		e.executeWithRetry(ctx)
	}, e.interval)
}

func (e *PollingExecutor) executeWithRetry(ctx context.Context) {
	logger := log.FromContext(ctx)
	backoff := e.retryBackoff
	for { // infinite retry loop
		select {
		case <-ctx.Done():
			logger.Info("Context cancelled, stopping optimization loop")
			return
		default:
		}

		err := e.config.OptimizeFunc(ctx)
		if err == nil {
			return
		}

		if IsReported(err) {
			logger.V(logging.DEBUG).Info("Optimization error already reported by its source", "error", err.Error())
		} else {
			logger.Error(err, "Optimization error")
		}

		select {
		case <-ctx.Done():
			logger.Info("Context cancelled during retry delay")
			return
		case <-time.After(backoff):
			backoff *= 2

			if backoff > 4*time.Second {
				backoff = 4 * time.Second
			}
		}
	}
}
