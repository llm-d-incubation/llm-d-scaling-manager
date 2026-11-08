package prometheus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	sourcepkg "github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
)

func registerQueries(t *testing.T, src *PrometheusSource, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := src.QueryList().Register(sourcepkg.QueryTemplate{
			Name: n, Type: sourcepkg.QueryTypePromQL, Template: "up{job=\"" + n + "\"}",
		}); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
	}
}

// TestRefreshReportsAnOutage: when every query fails, Refresh must say so in
// its error. Callers that count collection errors look only at that error, so a
// Prometheus outage used to count nothing anywhere.
func TestRefreshReportsAnOutage(t *testing.T) {
	api := &mockPrometheusAPI{queryFunc: func(context.Context, string, time.Time, ...v1.Option) (model.Value, v1.Warnings, error) {
		return nil, nil, errors.New("dial tcp: connection refused")
	}}
	src := NewPrometheusSource(context.Background(), api, PrometheusSourceConfig{DefaultTTL: time.Minute, QueryTimeout: 50 * time.Millisecond})
	defer src.Close()
	registerQueries(t, src, "a", "b")

	results, err := src.Refresh(context.Background(), sourcepkg.RefreshSpec{})
	if err == nil {
		t.Fatal("Refresh returned no error although every query failed")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q does not carry the query failure", err)
	}
	if len(results) != 2 || results["a"] == nil || !results["a"].HasError() {
		t.Errorf("per-query results must still be returned with their errors, got %v", results)
	}
}

// TestRefreshPartialFailureIsNotAnError: one failing query among working ones
// stays a per-result error, as before.
func TestRefreshPartialFailureIsNotAnError(t *testing.T) {
	api := &mockPrometheusAPI{queryFunc: func(_ context.Context, q string, _ time.Time, _ ...v1.Option) (model.Value, v1.Warnings, error) {
		if strings.Contains(q, `"bad"`) {
			return nil, nil, errors.New("bad_data")
		}
		return model.Vector{}, nil, nil
	}}
	src := NewPrometheusSource(context.Background(), api, PrometheusSourceConfig{DefaultTTL: time.Minute, QueryTimeout: 50 * time.Millisecond})
	defer src.Close()
	registerQueries(t, src, "good", "bad")

	results, err := src.Refresh(context.Background(), sourcepkg.RefreshSpec{})
	if err != nil {
		t.Fatalf("partial failure returned %v", err)
	}
	if !results["bad"].HasError() || results["good"].HasError() {
		t.Errorf("per-result errors wrong: %+v", results)
	}
}

// TestGetDoesNotWaitForARefresh: a cached read must not block behind a refresh
// that is waiting on Prometheus. Refresh used to hold the source's lock across
// every query, up to QueryTimeout each.
func TestGetDoesNotWaitForARefresh(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	api := &mockPrometheusAPI{queryFunc: func(ctx context.Context, q string, _ time.Time, _ ...v1.Option) (model.Value, v1.Warnings, error) {
		if strings.Contains(q, `"slow"`) {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return model.Vector{}, nil, nil
	}}
	src := NewPrometheusSource(context.Background(), api, PrometheusSourceConfig{DefaultTTL: time.Minute, QueryTimeout: 10 * time.Second})
	defer src.Close()
	registerQueries(t, src, "fast", "slow")

	if _, err := src.Refresh(context.Background(), sourcepkg.RefreshSpec{Queries: []string{"fast"}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = src.Refresh(context.Background(), sourcepkg.RefreshSpec{Queries: []string{"slow"}})
	}()
	<-started

	got := make(chan *sourcepkg.CachedValue, 1)
	go func() { got <- src.Get("fast", nil) }()
	select {
	case v := <-got:
		if v == nil {
			t.Error("cached value missing")
		}
	case <-time.After(2 * time.Second):
		t.Error("Get blocked behind an in-flight Refresh")
	}
	close(release)
	<-done
}
