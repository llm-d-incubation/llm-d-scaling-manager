package fleet

import (
	"math"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// ResidentRead reads a Ready row's instant KV and running count inside the
// band, and leaves out every row the KV per request must not read.
func TestResidentRead(t *testing.T) {
	row := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{Ready: true, KvUsageInstant: 0.5, KvUsageInstantKnown: true,
			RunningRequests: 50, RunningRequestsKnown: true, TotalKvCapacityTokens: 100_000}
	}
	if tokens, running, ok := ResidentRead(row(), 0.15); !ok || tokens != 50_000 || running != 50 {
		t.Errorf("read: %g, %d, %v; want 50000, 50, true", tokens, running, ok)
	}
	for name, edit := range map[string]func(*domain.ReplicaMetrics){
		"not Ready":       func(r *domain.ReplicaMetrics) { r.Ready = false },
		"warm pool":       func(r *domain.ReplicaMetrics) { r.FromWarmPool = true },
		"running unread":  func(r *domain.ReplicaMetrics) { r.RunningRequestsKnown = false },
		"KV unread":       func(r *domain.ReplicaMetrics) { r.KvUsageInstantKnown = false },
		"none running":    func(r *domain.ReplicaMetrics) { r.RunningRequests = 0 },
		"no capacity":     func(r *domain.ReplicaMetrics) { r.TotalKvCapacityTokens = 0 },
		"overflow":        func(r *domain.ReplicaMetrics) { r.TotalKvCapacityTokens = math.MaxInt64 },
		"under the band":  func(r *domain.ReplicaMetrics) { r.KvUsageInstant = 0.1 },
		"over full":       func(r *domain.ReplicaMetrics) { r.KvUsageInstant = 1.5 },
		"fill not a real": func(r *domain.ReplicaMetrics) { r.KvUsageInstant = math.NaN() },
	} {
		rm := row()
		edit(&rm)
		if tokens, running, ok := ResidentRead(rm, 0.15); ok || tokens != 0 || running != 0 {
			t.Errorf("%s: %g, %d, %v; want no reading", name, tokens, running, ok)
		}
	}
}
