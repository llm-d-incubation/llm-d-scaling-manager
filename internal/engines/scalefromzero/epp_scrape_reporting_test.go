package scalefromzero

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/pod"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/executor"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	prometheusutil "github.com/llm-d-incubation/llm-d-scaling-manager/internal/prometheus"
)

// realScrapeError is what the real pod source returns when its only EPP pod
// answers 500 with body.
func realScrapeError(t *testing.T, ctx context.Context, body string) error {
	t.Helper()
	_, err := failingEPPSourceWithBody(t, ctx, "scrape-err-ns", body).Refresh(ctx, source.RefreshSpec{})
	require.ErrorIs(t, err, pod.ErrScrapeFailed)
	return err
}

// The fallback says it failed only on the tick it engages. A scrape error
// quieted as already reported after that left no line at the default level
// saying wakes had stopped working, and the last one said they still work.
func TestAFailingFallbackKeepsTheScrapeErrorAnOptimizationError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, metrics.InitMetrics(prometheus.NewRegistry()))

	f := newWakeFixture(t, "fallbackdown")
	ds := f.engine.Datastore.(stubSourceDatastore)
	ds.src = failingEPPSource(t, ctx, f.ns)
	f.engine.Datastore = ds
	prom := newFakeQueueSource(nil, nil) // healthy, nothing queued
	f.engine.QueueFallback = NewQueueFallback(prom)
	f.engine.QueueFallback.MinInterval = 0 // every tick reads Prometheus

	for i := 1; i < f.engine.QueueFallback.Threshold; i++ {
		err := f.engine.optimize(ctx)
		require.Error(t, err)
		require.True(t, executor.IsReported(err), "tick %d: below the threshold the source has said it", i)
	}
	require.NoError(t, f.engine.optimize(ctx), "at the threshold the healthy fallback answers")

	prom.err = errors.New("prometheus unavailable")
	for i := range 3 {
		err := f.engine.optimize(ctx)
		require.Error(t, err, "tick %d: neither transport answers", i)
		assert.False(t, executor.IsReported(err),
			"tick %d: the fallback failing after it engaged must stay an optimization error: %v", i, err)
		assert.ErrorIs(t, err, pod.ErrScrapeFailed)
		assert.ErrorContains(t, err, "prometheus unavailable", "the Prometheus error reaches the line")
	}

	prom.err = nil
	err := f.engine.optimize(ctx)
	assert.NoError(t, err, "Prometheus is back: the fallback answers again")
}

// The body is the EPP's own text. Categorized with it, a 500 saying
// "execution timeout" was counted as a timeout.
func TestTheScrapeErrorReasonIgnoresTheBody(t *testing.T) {
	err := realScrapeError(t, context.Background(), "query execution timeout")
	require.ErrorContains(t, err, "execution timeout", "the body stays in the message")
	require.NotEqual(t, "unknown", prometheusutil.CategorizePrometheusError(err),
		"precondition: categorized on the whole message, the body picks the label")
	assert.Equal(t, "unknown", scrapeErrorReason(err), "a 500 is categorized on its status")

	plain := realScrapeError(t, context.Background(), "")
	assert.Equal(t, prometheusutil.CategorizePrometheusError(plain), scrapeErrorReason(plain),
		"an empty body changes nothing")
	refused := errors.New("dial tcp: connection refused")
	assert.Equal(t, "connection_refused", scrapeErrorReason(refused), "a transport error is categorized as before")
}

// What one tick returns for its models: quiet only when every model's error is
// an EPP scrape failure the source has logged, and nothing failed behind it.
func TestJoinModelErrors(t *testing.T) {
	scrape := realScrapeError(t, context.Background(), "Authentication failed")
	other := errors.New("EPP metrics source not found in registry for pool ns/p")
	fallback := errors.Join(scrape, fmt.Errorf("%w: %w", errFallbackFailed, errors.New("prometheus unavailable")))

	for _, tc := range []struct {
		name     string
		errs     []error
		reported bool
	}{
		{"one scrape failure", []error{scrape}, true},
		{"two models, both scrape failures", []error{scrape, scrape}, true},
		{"a scrape failure and another error", []error{scrape, other}, false},
		{"another error alone", []error{other}, false},
		{"a scrape failure with the fallback failing", []error{fallback}, false},
		{"no Ready EPP pod", []error{fmt.Errorf("service ns/epp: %w", pod.ErrNoReadyPods)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := joinModelErrors(tc.errs)
			require.Error(t, err)
			assert.Equal(t, tc.reported, executor.IsReported(err), "%v", err)
			for _, e := range tc.errs {
				assert.ErrorIs(t, err, e, "every model's error is in the join")
			}
		})
	}
	assert.NoError(t, joinModelErrors(nil))
}
