package domain

import "testing"

// TestBusiestRankFillRatio: the busiest rank's peak over the ranks' mean peak,
// read only with several ranks and both readings.
func TestBusiestRankFillRatio(t *testing.T) {
	for _, c := range []struct {
		name   string
		m      ReplicaMetrics
		want   float64
		wantOK bool
	}{
		{"DP8, busiest 1.25 of the mean", ReplicaMetrics{EngineRanks: 8, KvCacheUsage: 0.64, MaxRankKvCacheUsage: 0.8}, 1.25, true},
		{"one rank", ReplicaMetrics{EngineRanks: 1, KvCacheUsage: 0.64, MaxRankKvCacheUsage: 0.8}, 0, false},
		{"busiest not read", ReplicaMetrics{EngineRanks: 8, KvCacheUsage: 0.64}, 0, false},
		{"mean not read", ReplicaMetrics{EngineRanks: 8, MaxRankKvCacheUsage: 0.8}, 0, false},
		{"never under one", ReplicaMetrics{EngineRanks: 2, KvCacheUsage: 0.5, MaxRankKvCacheUsage: 0.4}, 1, true},
	} {
		got, ok := c.m.BusiestRankFillRatio()
		if got != c.want || ok != c.wantOK {
			t.Errorf("%s: BusiestRankFillRatio = %v, %v; want %v, %v", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

// TestBusiestRankKvUsageForITL: the minute's mean fill carried to the busiest
// rank by the ratio, at most full; the mean where no ratio was read.
func TestBusiestRankKvUsageForITL(t *testing.T) {
	m := ReplicaMetrics{EngineRanks: 8, KvCacheUsage: 0.64, MaxRankKvCacheUsage: 0.8, KvUsageMean: 0.6, KvUsageMeanKnown: true}
	if got := m.BusiestRankKvUsageForITL(); got != 0.75 {
		t.Errorf("BusiestRankKvUsageForITL = %v, want 0.6 x 1.25 = 0.75", got)
	}
	m.KvUsageMean = 0.9
	if got := m.BusiestRankKvUsageForITL(); got != 1 {
		t.Errorf("BusiestRankKvUsageForITL = %v, want it held at 1", got)
	}
	one := ReplicaMetrics{EngineRanks: 1, KvCacheUsage: 0.64, MaxRankKvCacheUsage: 0.8, KvUsageMean: 0.6, KvUsageMeanKnown: true}
	if got := one.BusiestRankKvUsageForITL(); got != 0.6 {
		t.Errorf("one rank: BusiestRankKvUsageForITL = %v, want KvUsageForITL 0.6", got)
	}
}
