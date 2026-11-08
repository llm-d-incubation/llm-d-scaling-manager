package domain

import (
	"math"
	"testing"
	"time"
)

func TestNewestRevision(t *testing.T) {
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	row := func(rev string, ready time.Time) ReplicaMetrics {
		return ReplicaMetrics{VariantName: "v", Ready: true, Revision: rev, ReadySince: ready}
	}
	bridge, notReady, other := row("x", at.Add(time.Hour)), row("x", at.Add(time.Hour)), row("x", at.Add(time.Hour))
	bridge.FromWarmPool, notReady.Ready, other.VariantName = true, false, "w"
	for _, c := range []struct {
		name     string
		rows     []ReplicaMetrics
		want     string
		distinct int
	}{
		{"none", nil, "", 0},
		{"one, no Ready time", []ReplicaMetrics{row("a", time.Time{})}, "a", 1},
		{"the younger", []ReplicaMetrics{row("a", at), row("b", at.Add(time.Minute))}, "b", 2},
		{"by each one's oldest pod", []ReplicaMetrics{row("a", at), row("a", at.Add(time.Hour)), row("b", at.Add(time.Minute))}, "b", 2},
		{"an unread Ready time among others", []ReplicaMetrics{row("a", time.Time{}), row("a", at), row("b", at.Add(time.Minute))}, "b", 2},
		{"no order", []ReplicaMetrics{row("a", time.Time{}), row("b", at)}, "", 2},
		{"a tie", []ReplicaMetrics{row("a", at), row("b", at)}, "", 2},
		{"what is not the variant's own", []ReplicaMetrics{row("a", at), bridge, notReady, other, row("", at.Add(time.Hour))}, "a", 1},
	} {
		if got, n := NewestRevision(c.rows, "v"); got != c.want || n != c.distinct {
			t.Errorf("%s: %q, %d, want %q, %d", c.name, got, n, c.want, c.distinct)
		}
	}
}

// A record of one rank is its own busiest rank, whatever the busiest-rank
// fields hold: the guards read exactly what they read before ranks existed.
func TestBusiestRankOfOneRankIsTheRecord(t *testing.T) {
	for _, ranks := range []int{0, 1} {
		m := ReplicaMetrics{EngineRanks: ranks, QueueLength: 7, KvCacheUsage: 0.4,
			TotalKvCapacityTokens: 1000, TokensInUse: 400,
			MaxRankQueueLength: 99, MaxRankKvCacheUsage: 0.99}
		if got := m.BusiestRankQueue(); got != 7 {
			t.Errorf("ranks %d: BusiestRankQueue = %d, want the record's queue 7", ranks, got)
		}
		if got := m.BusiestRankKvUsage(); got != 0.4 {
			t.Errorf("ranks %d: BusiestRankKvUsage = %v, want the record's 0.4", ranks, got)
		}
		if got := m.BusiestRankTokens(); got != 400 {
			t.Errorf("ranks %d: BusiestRankTokens = %d, want TokensInUse 400", ranks, got)
		}
	}
}

// A record of eight ranks reads its busiest rank, and its tokens are that
// rank's utilization scaled to the record's capacity.
func TestBusiestRankOfEightRanks(t *testing.T) {
	m := ReplicaMetrics{EngineRanks: 8, QueueLength: 8, KvCacheUsage: 0.125,
		TotalKvCapacityTokens: 8000, TokensInUse: 1000,
		MaxRankQueueLength: 5, MaxRankKvCacheUsage: 1.0}
	if got := m.BusiestRankQueue(); got != 5 {
		t.Errorf("BusiestRankQueue = %d, want 5", got)
	}
	if got := m.BusiestRankKvUsage(); got != 1.0 {
		t.Errorf("BusiestRankKvUsage = %v, want 1.0", got)
	}
	if got := m.BusiestRankTokens(); got != 8000 {
		t.Errorf("BusiestRankTokens = %d, want 8000: one full rank, as the pod", got)
	}
	// An idle pod is idle on every rank.
	idle := ReplicaMetrics{EngineRanks: 8, TotalKvCapacityTokens: 8000}
	if q, kv, tok := idle.BusiestRankQueue(), idle.BusiestRankKvUsage(), idle.BusiestRankTokens(); q != 0 || kv != 0 || tok != 0 {
		t.Errorf("idle pod reads busiest rank %d / %v / %d, want zeros", q, kv, tok)
	}
}

// Without a busiest-rank reading, the busiest rank is read at the least it can
// hold: the mean, rounded up for the queue. It never reads below the mean.
func TestBusiestRankWithoutAReading(t *testing.T) {
	m := ReplicaMetrics{EngineRanks: 8, QueueLength: 9, KvCacheUsage: 0.25,
		TotalKvCapacityTokens: 8000, TokensInUse: 2000}
	if got := m.BusiestRankQueue(); got != 2 {
		t.Errorf("BusiestRankQueue = %d, want ceil(9/8) = 2", got)
	}
	if got := m.BusiestRankKvUsage(); got != 0.25 {
		t.Errorf("BusiestRankKvUsage = %v, want the mean 0.25", got)
	}
	if got := m.BusiestRankTokens(); got != 2000 {
		t.Errorf("BusiestRankTokens = %d, want TokensInUse 2000", got)
	}
	// A reading above the cache is clamped to it, as TokensInUse is.
	m.MaxRankKvCacheUsage = 1.5
	if got := m.BusiestRankTokens(); got != 8000 {
		t.Errorf("BusiestRankTokens = %d, want the capacity 8000", got)
	}
}

// ITLPerToken reads a speculating replica's step time per token, withholds
// it when the counter is missing under speculative flags, and leaves every
// other reading alone (#274).
func TestITLPerToken(t *testing.T) {
	finite := ReplicaMetrics{AvgITL: 0.03, AvgITLKnown: true}
	withCounter := finite
	withCounter.SpecAcceptedPerStep, withCounter.SpecAcceptedPerStepKnown = 2, true
	spec := ITLStep{Speculates: true}
	withRounding := withCounter
	withRounding.SpecAcceptedPerStep = 2 + 1e-9
	negative := withCounter
	negative.SpecAcceptedPerStep = -0.5
	for _, tc := range []struct {
		name  string
		m     ReplicaMetrics
		step  ITLStep
		itl   float64
		state ITLState
		why   ITLWithheld
	}{
		{"counter read", withCounter, spec, 0.01, ITLFinite, ITLKept},
		{"counter read, flags silent", withCounter, ITLStep{}, 0.01, ITLFinite, ITLKept},
		{"counter at the stated draft length", withCounter, ITLStep{Speculates: true, MaxAccepted: 2}, 0.01, ITLFinite, ITLKept},
		{"counter above the stated draft length", withCounter, ITLStep{Speculates: true, MaxAccepted: 1}, 0, ITLAbsent, ITLOverMax},
		{"counter a rounding error over the draft length", withRounding, ITLStep{Speculates: true, MaxAccepted: 2}, 0.03 / (3 + 1e-9), ITLFinite, ITLKept},
		{"a negative counter is not a count", negative, spec, 0, ITLAbsent, ITLNoCounter},
		{"counter missing under speculative flags", finite, spec, 0, ITLAbsent, ITLNoCounter},
		{"no speculation", finite, ITLStep{}, 0.03, ITLFinite, ITLKept},
		{"an engine whose ITL is per token already", withCounter, ITLStep{PerToken: true, Speculates: true}, 0.03, ITLFinite, ITLKept},
		{"idle", ReplicaMetrics{AvgITLEmpty: true}, spec, 0, ITLEmpty, ITLKept},
		{"absent", ReplicaMetrics{}, spec, 0, ITLAbsent, ITLKept},
	} {
		got, why := tc.m.ITLPerToken(tc.step)
		if math.Abs(got.AvgITL-tc.itl) > 1e-12 || got.ITLState() != tc.state || why != tc.why {
			t.Errorf("%s: AvgITL %v (%s, %v), want %v (%s, %v)", tc.name, got.AvgITL, got.ITLState(), why, tc.itl, tc.state, tc.why)
		}
	}
}
