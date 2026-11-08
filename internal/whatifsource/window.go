package whatifsource

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// maxPointsPerQuery bounds one range query's steps: Prometheus refuses a
// query of more than 11,000 points per series, so a longer window is asked
// in parts and joined. Options.RangeQueryPoints sets a smaller bound.
const maxPointsPerQuery = 10_000

// windowAPI is a Prometheus API that answers an instant query, at the step
// it is set to, from one range query over the whole window.
//
// The collector's Prometheus source issues instant queries (api.Query at
// the wall clock). A capture plays every step of a past window through that
// same source, so each PromQL expression is asked once, as a range query
// over the window at the capture's step, and every step's instant query is
// answered with the samples the range query evaluated at that step. A range
// query evaluates its expression at start + k x step exactly as an instant
// query at that time would: the answer is the one the collector would have
// read, had it run then.
//
// Only Query is answered. QueryRange is refused, since a range asked of
// the window would bypass its steps; every other method is the embedded
// API's, which is nil, so calling one panics. The collector calls none of
// them.
type windowAPI struct {
	promv1.API
	// prom is the real client: QueryRange is the only call made on it, and
	// a range query reads, never writes.
	prom   promv1.API
	start  time.Time
	end    time.Time
	step   time.Duration
	points int
	// abort cancels the capture's context with the first range query that
	// fails, so the collector's retries (QueryPrometheusWithBackoff) stop at
	// once instead of sleeping through their backoff on an answer that is
	// memoized and cannot change.
	abort context.CancelCauseFunc

	// maxSamples bounds samples (MaxSamples); zero is no bound (Force).
	maxSamples int

	mu sync.Mutex
	at time.Time
	// memo holds every expression's answer for the whole capture: each
	// step reads its own instant from it, so nothing is dropped until the
	// capture ends. samples counts what it holds.
	memo    map[string]*rangeAnswer
	samples int
	// failed is the first range query that failed. Every query a capture
	// asks is required (Capture), so it fails the capture.
	failed error
}

// rangeAnswer is one expression's range query, fetched once.
type rangeAnswer struct {
	once     sync.Once
	byTime   map[model.Time]model.Vector
	warnings promv1.Warnings
	err      error
}

// newWindowAPI answers instant queries from range queries over start to end
// at step, each in parts of at most points steps (maxPointsPerQuery when
// points is not positive). abort, when not nil, is called with the first
// range query that fails.
func newWindowAPI(prom promv1.API, start, end time.Time, step time.Duration, points int,
	abort context.CancelCauseFunc) *windowAPI {
	if points <= 0 {
		points = maxPointsPerQuery
	}
	return &windowAPI{prom: prom, start: start, end: end, step: step, points: points, abort: abort,
		at: start, memo: map[string]*rangeAnswer{}}
}

// setStep sets the instant the next instant queries are answered at.
func (w *windowAPI) setStep(at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.at = at
}

// ranges is how many range queries were asked.
func (w *windowAPI) ranges() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.memo)
}

// failure is the first range query that failed, nil while none has.
func (w *windowAPI) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed
}

// fail records err as the window's failure, when it is the first, and
// aborts the capture with it.
func (w *windowAPI) fail(err error) {
	w.mu.Lock()
	first := w.failed == nil
	if first {
		w.failed = err
	}
	w.mu.Unlock()
	if first && w.abort != nil {
		w.abort(err)
	}
}

// Query answers an instant query at the step setStep set, whatever ts the
// caller passed (the collector passes the wall clock).
func (w *windowAPI) Query(ctx context.Context, query string, _ time.Time, _ ...promv1.Option) (model.Value, promv1.Warnings, error) {
	w.mu.Lock()
	a := w.memo[query]
	if a == nil {
		a = &rangeAnswer{}
		w.memo[query] = a
	}
	at := w.at
	w.mu.Unlock()
	// The first caller fetches, under its own ctx; a caller that comes while
	// it does waits on the Once, and every later one reads the answer, a
	// failure included: an answer is never asked again. The capture's ctx
	// is every caller's parent, and a failure aborts it, so a ctx cancelled
	// under the first caller fails the capture rather than one field.
	a.once.Do(func() {
		a.byTime, a.warnings, a.err = w.fetch(ctx, query)
		if a.err != nil {
			w.fail(a.err)
		}
	})
	if a.err != nil {
		return nil, a.warnings, a.err
	}
	vec := a.byTime[model.TimeFromUnixNano(at.UnixNano())]
	if vec == nil {
		vec = model.Vector{}
	}
	return vec, a.warnings, nil
}

// fetch asks query over the window, in parts of at most w.points steps,
// and indexes its samples by the instant they were evaluated at.
func (w *windowAPI) fetch(ctx context.Context, query string) (map[model.Time]model.Vector, promv1.Warnings, error) {
	out := map[model.Time]model.Vector{}
	var warnings promv1.Warnings
	chunk := time.Duration(w.points-1) * w.step
	for from := w.start; !from.After(w.end); from = from.Add(chunk + w.step) {
		to := from.Add(chunk)
		if to.After(w.end) {
			to = w.end
		}
		val, warn, err := w.prom.QueryRange(ctx, query, promv1.Range{Start: from, End: to, Step: w.step})
		warnings = append(warnings, warn...)
		if err != nil {
			return nil, warnings, fmt.Errorf("range query %q: %w", query, err)
		}
		matrix, ok := val.(model.Matrix)
		if !ok {
			if val == nil {
				continue
			}
			return nil, warnings, fmt.Errorf("range query %q: a %s, not a matrix", query, val.Type())
		}
		for _, stream := range matrix {
			for _, p := range stream.Values {
				if !w.onGrid(p.Timestamp) {
					return nil, warnings, fmt.Errorf("range query %q: a sample at %s, off the step grid from %s every %s: "+
						"the server did not evaluate at the start it was asked, as a query frontend that aligns start "+
						"to step (Thanos, Mimir) does; capture from a --from that is a multiple of --step, or turn that alignment off",
						query, p.Timestamp.Time().UTC().Format(time.RFC3339Nano), w.start.Format(time.RFC3339Nano), w.step)
				}
				// Every step's sample shares the stream's label map: one
				// map per series, not per sample. Nothing writes to it
				// (the collector copies the labels it keeps).
				out[p.Timestamp] = append(out[p.Timestamp], &model.Sample{Metric: stream.Metric, Value: p.Value, Timestamp: p.Timestamp})
			}
			if err := w.hold(len(stream.Values)); err != nil {
				return nil, warnings, fmt.Errorf("range query %q: %w", query, err)
			}
		}
	}
	return out, warnings, nil
}

// hold counts n more samples held, and refuses a count past maxSamples.
func (w *windowAPI) hold(n int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.samples += n
	if w.maxSamples > 0 && w.samples > w.maxSamples {
		return fmt.Errorf("%w: past %d samples held in memory; shorten the window, lengthen the step, or pass --force",
			ErrWindowTooLarge, w.maxSamples)
	}
	return nil
}

// held is how many samples the memo holds.
func (w *windowAPI) held() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.samples
}

// onGrid reports whether ts is one of the window's steps, to the
// millisecond Prometheus keeps.
func (w *windowAPI) onGrid(ts model.Time) bool {
	d := int64(ts) - w.start.UnixMilli()
	return d >= 0 && d%w.step.Milliseconds() == 0
}

// errNotCaptured is a method of the Prometheus API a capture does not
// answer.
var errNotCaptured = errors.New("whatifsource: only instant queries are answered from a capture's window")

// QueryRange is refused: the collector asks instant queries only.
func (w *windowAPI) QueryRange(context.Context, string, promv1.Range, ...promv1.Option) (model.Value, promv1.Warnings, error) {
	return nil, nil, errNotCaptured
}
