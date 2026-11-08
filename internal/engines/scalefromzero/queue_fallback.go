package scalefromzero

import (
	"context"
	"sync"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// The wake signal has one transport, and it is not the one everything else uses.
//
// This engine reads the EPP flow-control queue by scraping the EPP pod directly
// — pod IP, EPP metrics port, bearer token projected at
// /var/run/secrets/epp-metrics/token (see datastore.PoolSet). Every other metric
// the scaling manager consumes comes from Prometheus. The two paths therefore fail
// INDEPENDENTLY, and several ordinary failures take out the scrape while leaving
// the queue depth sitting in Prometheus, fully readable:
//
//   - the projected token is unreadable or expired — getEPPMetricsToken logs
//     "EPP authentication will be disabled" and every scrape 401s;
//   - the EPP's tokenreview RBAC is absent, or bound to a differently-named
//     release, so the EPP rejects the scaling manager's token specifically;
//   - a NetworkPolicy admits the monitoring namespace but not the scaling manager's — direct
//     scraping needs pod-IP egress that a Prometheus query does not;
//   - PoolGetMetricsSource returns nil, which the engine itself calls a
//     "Datastore inconsistency";
//   - the EPP is restarting: a live scrape errors immediately, while Prometheus
//     still answers from its last sample.
//
// In every one of those the demand IS observable and a parked model would never
// be woken — the failure mode with no symptom, because a model that is asleep
// looks exactly like a model nobody is asking for.
//
// So this is a second transport for the same metric, read from Prometheus, used
// only once the scrape has failed repeatedly. It is deliberately NOT a second
// engine: two components able to wake the same workload publish into one
// decision store, and the last write wins. One waker, two ways to see the queue.

const (
	// DefaultFallbackThreshold is how many consecutive scrape failures for a pool
	// must accumulate before the fallback engages. A single failed scrape is
	// ordinary — an EPP rolling, a connection reset — and switching transport on
	// one blip would trade a fast, fresh signal for a slow, stale one for no
	// reason. Three ticks of this loop is 300ms, so the delay costs nothing
	// against a real outage.
	DefaultFallbackThreshold = 3

	// DefaultFallbackMinInterval bounds how often Prometheus is queried while the
	// fallback is engaged. This loop runs at 10Hz; querying per tick would put 10
	// PromQL executions a second on the shared Prometheus for as long as the EPP
	// is unreachable, which is the moment to add least load. Prometheus cannot
	// answer faster than it scrapes anyway, so anything below the scrape interval
	// buys nothing.
	DefaultFallbackMinInterval = 5 * time.Second

	// DefaultFallbackMaxAge is how old a Prometheus sample may be and still count
	// as demand. It has to exist: unlike a live scrape, a query answers from the
	// last stored sample, so without a bound a queue that drained minutes ago
	// would keep waking the model it no longer needs. Set above a typical 15-30s
	// scrape interval so an ordinary sample is never discarded, and far below the
	// point where a drained queue still reads as pressure.
	DefaultFallbackMaxAge = 90 * time.Second
)

// QueueFallback reads the EPP flow-control queue from Prometheus when the direct
// EPP scrape is failing. A nil *QueueFallback is a working no-op: every method
// guards its receiver, so an install without a Prometheus source keeps exactly
// the behaviour it had before this existed — the scrape error is returned and
// nothing is woken.
type QueueFallback struct {
	src   source.MetricsSource
	query string

	// Threshold, MinInterval and MaxAge are the three bounds documented on the
	// Default* constants above. Exported so a test can shrink them; production
	// uses the defaults.
	Threshold   int
	MinInterval time.Duration
	MaxAge      time.Duration

	mu sync.Mutex
	// failures counts consecutive scrape failures per pool, and engaged records
	// which pools are currently on the fallback. Keyed by pool rather than by
	// model because the transport is per-EPP: when it breaks, it breaks for every
	// model behind that pool at once, and a per-model counter would make each
	// model discover the same outage separately.
	failures map[string]int
	engaged  map[string]bool

	// lastAt, last and lastErr memoize one Prometheus answer for MinInterval, so
	// N models behind one pool share a single query rather than issuing one each.
	lastAt  time.Time
	last    []source.MetricValue
	lastErr error
}

// fallbackQueryName is this fallback's own query registration. It deliberately
// does not reuse the collector's scheduler_queue_size: sharing a name would
// share a cache key and a QueryList entry with a component on a different
// cadence, and coupling the wake path to the steady-state collector's setup is
// the opposite of what a fallback is for. Registering its own costs one
// template.
const fallbackQueryName = "sfz_flow_control_queue_size"

// fallbackQuery reads both flow-control families, preferring llm-d's. The EPP
// renamed the metric to the llm_d_epp_ prefix and upstream
// gateway-api-inference-extension kept inference_extension_, so which exists
// depends on the EPP build. PromQL `or` is exactly that preference: the left
// side wherever it has series, the right side only where it does not.
//
// The namespace stays in the grouping key so that each pool reads its own
// router's queue: grouped by model name alone, a parked model in one namespace
// was woken by the queue of a same-named model in another.
const fallbackQuery = `sum by (namespace, model_name, target_model_name) (llm_d_epp_flow_control_queue_size)` +
	` or sum by (namespace, model_name, target_model_name) (inference_extension_flow_control_queue_size)`

// NewQueueFallback builds the fallback over a Prometheus metrics source and
// registers the query it needs on that source, so it depends on nothing else
// having been set up. A nil source yields a nil fallback, so callers need no
// conditional.
func NewQueueFallback(src source.MetricsSource) *QueueFallback {
	if src == nil {
		return nil
	}
	// Upsert, not MustRegister: idempotent, so building a second fallback over
	// the same source (tests, a future second pool) cannot panic.
	if err := src.QueryList().Upsert(source.QueryTemplate{
		Name:        fallbackQueryName,
		Type:        source.QueryTypePromQL,
		Template:    fallbackQuery,
		Description: "EPP flow-control queue depth per model, read as the scale-from-zero fallback",
	}); err != nil {
		// A template that will not register is a programming error in a constant
		// above, not a runtime condition; the fallback simply does not exist.
		return nil
	}
	return &QueueFallback{
		src:         src,
		query:       fallbackQueryName,
		Threshold:   DefaultFallbackThreshold,
		MinInterval: DefaultFallbackMinInterval,
		MaxAge:      DefaultFallbackMaxAge,
		failures:    make(map[string]int),
		engaged:     make(map[string]bool),
	}
}

// ScrapeSucceeded reports that the direct EPP scrape worked for a pool, clearing
// its failure count. If the pool was on the fallback, that recovery is reported
// once — the operator was told the primary path had failed and is owed the other
// half of the story.
func (f *QueueFallback) ScrapeSucceeded(ctx context.Context, pool string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	wasEngaged := f.engaged[pool]
	delete(f.failures, pool)
	delete(f.engaged, pool)
	f.mu.Unlock()

	// Published on every healthy scrape, not only on recovery, so the series
	// EXISTS for a pool scaling manager is watching. A gauge that only appears while broken
	// cannot be alerted on: absence would mean both "healthy" and "the scaling manager is not
	// looking at this pool at all", and those need different responses.
	metrics.SetScaleFromZeroQueueFallbackActive(pool, false)

	if wasEngaged {
		log.FromContext(ctx).Info("Scale-from-zero: EPP metrics scrape recovered; "+
			"reading the flow-control queue directly again", "pool", pool)
	}
}

// PendingAfterScrapeFailure answers the demand question from Prometheus after the
// direct scrape has failed.
//
// It returns engaged=false when the fallback cannot or should not answer — no
// fallback configured, the failure count is still below Threshold, or Prometheus
// failed too. The caller must then behave exactly as it did before: surface the
// scrape error. Reporting a false "no demand" on an unanswerable question would
// convert a visible collection failure into a silently unwoken model, which is
// the failure this whole path exists to prevent.
//
// namespace is the pool's: only the series of the router in that namespace are
// this model's demand (see eppSeriesInNamespace).
func (f *QueueFallback) PendingAfterScrapeFailure(
	ctx context.Context, pool, namespace, modelID string, scrapeErr error,
) (pending source.MetricValue, hasPending, engaged bool) {
	if f == nil {
		return source.MetricValue{}, false, false
	}
	logger := log.FromContext(ctx)

	f.mu.Lock()
	f.failures[pool]++
	count := f.failures[pool]
	if count < f.Threshold {
		f.mu.Unlock()
		return source.MetricValue{}, false, false
	}
	firstTime := !f.engaged[pool]
	f.engaged[pool] = true
	values, err := f.valuesLocked(ctx)
	f.mu.Unlock()

	if firstTime {
		metrics.SetScaleFromZeroQueueFallbackActive(pool, true)
		// Info, and only on the transition: this runs at 10Hz, and the condition
		// persists for as long as the EPP is unreachable.
		logger.Info("Scale-from-zero: EPP metrics scrape failing; reading the "+
			"flow-control queue from Prometheus instead. Wakes still work but are "+
			"slower and bounded by the scrape interval — fix the direct path.",
			"pool", pool, "consecutiveFailures", count, "scrapeError", scrapeErr)
	}

	if err != nil {
		// Both transports are down. Say so once per transition rather than per
		// tick; the caller returns the scrape error, which is the one to chase.
		if firstTime {
			logger.Error(err, "Scale-from-zero: the Prometheus fallback failed too; "+
				"no way to observe the flow-control queue", "pool", pool)
		}
		return source.MetricValue{}, false, false
	}

	pending, hasPending = pendingFromPromValues(values, namespace, modelID, f.MaxAge)
	if hasPending {
		logger.V(logging.DEBUG).Info("Scale-from-zero: pending requests seen via the Prometheus fallback",
			"pool", pool, "modelID", modelID, "value", pending.Value, "sampleAge", pending.Age())
	}
	return pending, hasPending, true
}

// PrometheusFailed returns the error of the Prometheus read the fallback last
// answered from, when the fallback is engaged for pool and that read failed;
// nil otherwise, and on a nil fallback.
func (f *QueueFallback) PrometheusFailed(pool string) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.engaged[pool] {
		return nil
	}
	return f.lastErr
}

// valuesLocked returns the flow-control queue series, refreshing from Prometheus
// at most once per MinInterval. Must be called with f.mu held.
func (f *QueueFallback) valuesLocked(ctx context.Context) ([]source.MetricValue, error) {
	if !f.lastAt.IsZero() && time.Since(f.lastAt) < f.MinInterval {
		return f.last, f.lastErr
	}
	results, err := f.src.Refresh(ctx, source.RefreshSpec{Queries: []string{f.query}})
	f.lastAt = time.Now()
	if err != nil {
		f.last, f.lastErr = nil, err
		return nil, err
	}
	result := results[f.query]
	if result == nil {
		f.last, f.lastErr = nil, nil
		return nil, nil
	}
	if result.HasError() {
		f.last, f.lastErr = nil, result.Error
		return nil, result.Error
	}
	f.last, f.lastErr = result.Values, nil
	return result.Values, nil
}

// pendingFromPromValues finds this model's queue depth in the Prometheus result.
//
// The matching differs from pendingRequestsForModel deliberately. That one reads
// a raw scrape, where every series still carries __name__ and the engine picks
// the metric family itself. Here the query is a `sum by (model_name,
// target_model_name)`, which drops __name__ — the family was already chosen in
// PromQL — so identity is the two model labels alone: target_model_name when the
// EPP set one, else model_name. That is the same rule the collector applies to
// the same series (see eppSeriesModel in internal/collector).
//
// A sample older than maxAge is not demand. A live scrape cannot return a stale
// value; a query answers from whatever Prometheus last stored, so without this a
// queue that drained long ago would keep waking the model that drained it.
func pendingFromPromValues(values []source.MetricValue, namespace, modelID string, maxAge time.Duration) (source.MetricValue, bool) {
	for _, v := range values {
		if v.Value <= 0 {
			continue
		}
		if eppSeriesModel(v.Labels) != modelID || !eppSeriesInNamespace(v.Labels, namespace) {
			continue
		}
		if v.IsStale(maxAge) {
			continue
		}
		return v, true
	}
	return source.MetricValue{}, false
}

// eppSeriesModel resolves which model an EPP flow-control series belongs to.
// target_model_name is the served model and wins where present; model_name is
// the requested name and is the only one set when no rewrite applied.
func eppSeriesModel(labels map[string]string) string {
	if v := labels[targetEPPMetricLabel]; v != "" {
		return v
	}
	return labels["model_name"]
}

// eppSeriesInNamespace reports whether an EPP series belongs to namespace: the
// label Prometheus stamps on it at scrape time names the router's namespace. A
// series without the label cannot be attributed and is kept, as it was before
// the label was read. The collector applies the same rule to the same series
// (eppSeriesInNamespace in internal/collector).
func eppSeriesInNamespace(labels map[string]string, namespace string) bool {
	ns := labels["namespace"]
	return ns == "" || ns == namespace
}
