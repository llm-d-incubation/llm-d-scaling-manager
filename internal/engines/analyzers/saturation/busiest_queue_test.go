package saturation

import (
	"math"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A variant's busiest queue is its deepest per-pod waiting queue against the
// saturation queue: one pod queued deep among idle siblings reads busy,
// however calm the fleet.
func TestBusiestQueueLoadByVariant(t *testing.T) {
	agg := map[string]string{"v": ""}
	rows := []domain.ReplicaMetrics{
		{PodName: "a", VariantName: "v", QueueLength: 0, Ready: true},
		{PodName: "b", VariantName: "v", QueueLength: 235, Ready: true},
		{PodName: "c", VariantName: "v", QueueLength: 1, Ready: true},
		// Not Ready: not routed to, so its queue is not routing.
		{PodName: "d", VariantName: "v", QueueLength: 900},
		// No role known for the variant.
		{PodName: "e", VariantName: "stray", QueueLength: 900, Ready: true},
	}
	got := busiestQueueLoadByVariant(rows, agg, 5, 0.8)
	if got["v"] != 47 {
		t.Errorf("busiest load %v, want 235/5 = 47 from the one deep Ready pod", got["v"])
	}
	if _, ok := got["stray"]; ok {
		t.Errorf("a variant with no known role was reported: %v", got)
	}
	for _, q := range []float64{0, -5, math.NaN()} {
		if got := busiestQueueLoadByVariant(rows, agg, q, 0.8); got != nil {
			t.Errorf("with a queue threshold of %v it reported %v", q, got)
		}
	}
}

// Prefill is left out: its queue is decode's back-pressure or the router's
// choice, which a replica kept does not relieve. A P/D decode's queue counts
// only while its KV is full and within its ceiling, since requests waiting
// for their remote KV sit in the same count; below that it reads zero.
func TestBusiestQueueLoadPrefillAndDecode(t *testing.T) {
	pd := map[string]string{"d": domain.RoleDecode, "p": domain.RolePrefill}
	decode := func(tokens, capacity int64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: "d1", VariantName: "d", QueueLength: 20, Ready: true,
			TokensInUse: tokens, TotalKvCapacityTokens: capacity}
	}
	prefill := domain.ReplicaMetrics{PodName: "p1", VariantName: "p", QueueLength: 439, Ready: true}
	for _, c := range []struct {
		name     string
		rm       domain.ReplicaMetrics
		kvThresh float64
		want     float64
	}{
		{"KV half full", decode(500, 1000), 0.8, 0},
		{"KV full", decode(900, 1000), 0.8, 4},
		{"no cache size", decode(900, 0), 0.8, 0},
		{"KV above the physical ceiling (a scrape artifact)", decode(1200, 1000), 0.8, 0},
		{"kvCacheThreshold 0: any KV in use is full, as roleSaturated has it", decode(1, 1000), 0, 4},
		{"kvCacheThreshold 0 with no KV in use", decode(0, 1000), 0, 0},
	} {
		got := busiestQueueLoadByVariant([]domain.ReplicaMetrics{c.rm, prefill}, pd, 5, c.kvThresh)
		if got["d"] != c.want {
			t.Errorf("decode, %s: %v, want %v", c.name, got["d"], c.want)
		}
		if _, ok := got["p"]; ok {
			t.Errorf("decode, %s: prefill was reported (%v); its queue is not one a kept replica relieves", c.name, got["p"])
		}
	}
}

// roleSaturated keeps its rule now that the KV half is shared (kvFull).
func TestRoleSaturatedSharesKvFull(t *testing.T) {
	roles := map[string]string{"d": domain.RoleDecode}
	row := domain.ReplicaMetrics{VariantName: "d", Ready: true, QueueLength: 5, TokensInUse: 900, TotalKvCapacityTokens: 1000}
	if !roleSaturated([]domain.ReplicaMetrics{row}, roles, domain.RoleDecode, 5, 0.8) {
		t.Error("a decode queued at the threshold with its KV full is not saturated")
	}
	row.QueueLength = 4
	if roleSaturated([]domain.ReplicaMetrics{row}, roles, domain.RoleDecode, 5, 0.8) {
		t.Error("a decode queued below the threshold is saturated")
	}
	row.QueueLength, row.TokensInUse = 5, 0
	if roleSaturated([]domain.ReplicaMetrics{row}, roles, domain.RoleDecode, 5, 0) {
		t.Error("a decode with no KV in use is saturated at a kvCacheThreshold of 0")
	}
}
