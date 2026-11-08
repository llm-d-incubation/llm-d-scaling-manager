package scalefromzero

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
)

// fakeQueueSource is a MetricsSource whose Refresh answer is set per test.
// refreshes counts calls, which is how the throttle is observed.
type fakeQueueSource struct {
	values    []source.MetricValue
	err       error
	refreshes int
	queries   *source.QueryList
}

func newFakeQueueSource(values []source.MetricValue, err error) *fakeQueueSource {
	return &fakeQueueSource{values: values, err: err, queries: source.NewQueryList()}
}

func (f *fakeQueueSource) QueryList() *source.QueryList { return f.queries }

func (f *fakeQueueSource) Refresh(_ context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	f.refreshes++
	if f.err != nil {
		return nil, f.err
	}
	name := fallbackQueryName
	if len(spec.Queries) > 0 {
		name = spec.Queries[0]
	}
	return map[string]*source.MetricResult{
		name: {QueryName: name, Values: f.values, CollectedAt: time.Now()},
	}, nil
}

func (f *fakeQueueSource) Get(string, map[string]string) *source.CachedValue { return nil }

func queued(depth float64, modelID string, age time.Duration) source.MetricValue {
	return source.MetricValue{
		Value:     depth,
		Timestamp: time.Now().Add(-age),
		Labels:    map[string]string{"target_model_name": modelID, "model_name": modelID},
	}
}

const testPool = "llm-d-sim/optimized-baseline"

// testNamespace is testPool's namespace.
const testNamespace = "llm-d-sim"

var errScrape = errors.New("epp scrape failed: 401 Unauthorized")

// The whole point: the EPP scrape is down, the queue is in Prometheus, and the
// model gets woken anyway.
func TestFallbackReportsDemandWhenTheScrapeIsDown(t *testing.T) {
	src := newFakeQueueSource([]source.MetricValue{queued(4, "llama", 2*time.Second)}, nil)
	f := NewQueueFallback(src)

	// Below the threshold it must NOT answer — one failed scrape is ordinary.
	for i := 1; i < f.Threshold; i++ {
		if _, _, engaged := f.PendingAfterScrapeFailure(context.Background(), testPool, testNamespace, "llama", errScrape); engaged {
			t.Fatalf("engaged after %d failures; the threshold is %d, and switching on a "+
				"single blip trades a live signal for a stale one for nothing", i, f.Threshold)
		}
	}

	pending, hasPending, engaged := f.PendingAfterScrapeFailure(context.Background(), testPool, testNamespace, "llama", errScrape)
	if !engaged {
		t.Fatal("the fallback must engage once the scrape has failed Threshold times")
	}
	if !hasPending {
		t.Error("a queue of 4 in Prometheus is demand; not seeing it leaves the model parked forever")
	}
	if pending.Value != 4 {
		t.Errorf("pending = %v, want the queue depth 4", pending.Value)
	}
}

// A stale sample is not demand. A live scrape cannot return one; a query answers
// from whatever Prometheus last stored, so without this bound a queue that
// drained long ago would keep waking the model that drained it.
func TestFallbackIgnoresAStaleSample(t *testing.T) {
	src := newFakeQueueSource([]source.MetricValue{queued(9, "llama", 10*time.Minute)}, nil)
	f := NewQueueFallback(src)
	f.Threshold = 1

	_, hasPending, engaged := f.PendingAfterScrapeFailure(context.Background(), testPool, testNamespace, "llama", errScrape)
	if !engaged {
		t.Fatal("the fallback engaged, it just found nothing fresh")
	}
	if hasPending {
		t.Error("a 10-minute-old sample must not count as demand — the queue it describes is long gone")
	}
}

// Prometheus is queried at most once per MinInterval no matter how fast the loop
// spins, and every model behind the pool shares that one answer.
func TestFallbackThrottlesPrometheus(t *testing.T) {
	src := newFakeQueueSource([]source.MetricValue{queued(1, "llama", time.Second)}, nil)
	f := NewQueueFallback(src)
	f.Threshold = 1

	for i := 0; i < 50; i++ {
		f.PendingAfterScrapeFailure(context.Background(), testPool, testNamespace, "llama", errScrape)
	}
	if src.refreshes != 1 {
		t.Errorf("queried Prometheus %d times for 50 ticks; the loop runs at 10Hz and the "+
			"moment the EPP is unreachable is the moment to add least load", src.refreshes)
	}

	f.MinInterval = 0 // as if the interval had elapsed
	f.PendingAfterScrapeFailure(context.Background(), testPool, testNamespace, "llama", errScrape)
	if src.refreshes != 2 {
		t.Errorf("refreshes = %d, want a fresh query once the interval elapsed", src.refreshes)
	}
}

// Both transports down must not read as "no demand". Reporting false there would
// convert a visible collection failure into a silently unwoken model — exactly
// the outcome this path exists to prevent.
func TestFallbackDoesNotClaimNoDemandWhenPrometheusAlsoFails(t *testing.T) {
	src := newFakeQueueSource(nil, errors.New("prometheus unreachable"))
	f := NewQueueFallback(src)
	f.Threshold = 1

	_, hasPending, engaged := f.PendingAfterScrapeFailure(context.Background(), testPool, testNamespace, "llama", errScrape)
	if engaged {
		t.Error("with both transports down the caller must surface the scrape error, " +
			"not be handed an answer nobody could observe")
	}
	if hasPending {
		t.Error("hasPending must be false when nothing was read")
	}
}

// A recovered scrape resets the counter, so a later single failure does not
// resume the fallback where the previous outage left off.
func TestScrapeRecoveryResetsTheFailureCount(t *testing.T) {
	src := newFakeQueueSource([]source.MetricValue{queued(3, "llama", time.Second)}, nil)
	f := NewQueueFallback(src)

	for i := 0; i < f.Threshold; i++ {
		f.PendingAfterScrapeFailure(context.Background(), testPool, testNamespace, "llama", errScrape)
	}
	f.ScrapeSucceeded(context.Background(), testPool)

	if _, _, engaged := f.PendingAfterScrapeFailure(context.Background(), testPool, testNamespace, "llama", errScrape); engaged {
		t.Error("one failure after a recovery must not re-engage; the count restarts")
	}
}

// Another model's queue is not this model's demand.
func TestFallbackMatchesTheModel(t *testing.T) {
	src := newFakeQueueSource([]source.MetricValue{queued(7, "other-model", time.Second)}, nil)
	f := NewQueueFallback(src)
	f.Threshold = 1

	_, hasPending, engaged := f.PendingAfterScrapeFailure(context.Background(), testPool, testNamespace, "llama", errScrape)
	if !engaged {
		t.Fatal("the fallback engaged")
	}
	if hasPending {
		t.Error("a queue for other-model must not wake llama")
	}
}

// Another namespace's router is not this pool's demand, even for a model of the
// same name: grouped by model name alone, a parked model was woken by a
// same-named model's queue in another namespace.
func TestFallbackMatchesTheNamespace(t *testing.T) {
	inNamespace := func(depth float64, ns string) source.MetricValue {
		v := queued(depth, "llama", time.Second)
		v.Labels["namespace"] = ns
		return v
	}
	cases := []struct {
		name    string
		values  []source.MetricValue
		pending float64 // 0: no demand
	}{
		{"only another namespace queues", []source.MetricValue{inNamespace(52, "other-ns"), inNamespace(0, testNamespace)}, 0},
		{"each namespace reads its own", []source.MetricValue{inNamespace(52, "other-ns"), inNamespace(3, testNamespace)}, 3},
		// No namespace label: unattributable, read as before the label was read.
		{"a series without the label is kept", []source.MetricValue{queued(5, "llama", time.Second)}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := NewQueueFallback(newFakeQueueSource(tc.values, nil))
			f.Threshold = 1
			pending, hasPending, engaged := f.PendingAfterScrapeFailure(context.Background(), testPool, testNamespace, "llama", errScrape)
			if !engaged {
				t.Fatal("the fallback engaged")
			}
			if hasPending != (tc.pending > 0) || (hasPending && pending.Value != tc.pending) {
				t.Errorf("pending = %v (hasPending %v), want %v", pending.Value, hasPending, tc.pending)
			}
		})
	}
}

// The EPP sets model_name and only sometimes target_model_name; the served model
// wins where present, matching how the collector reads the same series.
func TestFallbackFallsBackToModelNameLabel(t *testing.T) {
	v := source.MetricValue{
		Value:     2,
		Timestamp: time.Now(),
		Labels:    map[string]string{"model_name": "llama"}, // no target_model_name
	}
	src := newFakeQueueSource([]source.MetricValue{v}, nil)
	f := NewQueueFallback(src)
	f.Threshold = 1

	if _, hasPending, _ := f.PendingAfterScrapeFailure(context.Background(), testPool, testNamespace, "llama", errScrape); !hasPending {
		t.Error("a series carrying only model_name still identifies the model")
	}
}

// A nil fallback is the configuration where no Prometheus source was available.
// It must behave exactly as the engine did before the fallback existed.
func TestNilFallbackIsAWorkingNoOp(t *testing.T) {
	var f *QueueFallback
	if _, _, engaged := f.PendingAfterScrapeFailure(context.Background(), testPool, testNamespace, "llama", errScrape); engaged {
		t.Error("a nil fallback must never engage")
	}
	f.ScrapeSucceeded(context.Background(), testPool) // must not panic
	if NewQueueFallback(nil) != nil {
		t.Error("a nil source must yield a nil fallback so callers need no conditional")
	}
}
