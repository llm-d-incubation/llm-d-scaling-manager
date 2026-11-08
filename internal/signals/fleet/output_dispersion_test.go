package fleet

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// spread is a row of mean o at rate r whose outputs have dispersion d.
func spread(o, r, d float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{AvgOutputTokens: o, RequestRate: r,
		AvgOutputTokensSquared: d * o * o, AvgOutputTokensSquaredKnown: true}
}

// Two rows at D 4: the reading is D 4.
func TestOutputDispersion_ReadsTheSpread(t *testing.T) {
	d, ok := OutputDispersion([]domain.ReplicaMetrics{spread(256, 1, 4), spread(256, 1, 4)}, all)
	require.True(t, ok)
	require.InDelta(t, 4, d, 1e-12)
}

// One row of three with no reading of the bound, beside two at D 4, is no
// reading of the fleet: 1 and false. Each bad bound is one a guard can get
// wrong: NaN fails every comparison, +Inf passes > 0, and Mean drops both
// from E[O^2] while keeping the row's length in E[O], so a guard that let
// either through would read a spread from mismatched rows.
func TestOutputDispersion_OneUnreadRowIsNoReading(t *testing.T) {
	for name, bad := range map[string]func(*domain.ReplicaMetrics){
		"unknown": func(m *domain.ReplicaMetrics) { m.AvgOutputTokensSquaredKnown = false },
		"zero":    func(m *domain.ReplicaMetrics) { m.AvgOutputTokensSquared = 0 },
		"NaN":     func(m *domain.ReplicaMetrics) { m.AvgOutputTokensSquared = math.NaN() },
		"+Inf":    func(m *domain.ReplicaMetrics) { m.AvgOutputTokensSquared = math.Inf(1) },
	} {
		rows := []domain.ReplicaMetrics{spread(256, 1, 4), spread(256, 1, 4), spread(256, 1, 4)}
		bad(&rows[0])
		d, ok := OutputDispersion(rows, all)
		require.False(t, ok, name)
		require.Equal(t, 1.0, d, name)
		_, _, ok = OutputMoments(rows, all)
		require.False(t, ok, name)
	}
}

// A row include leaves out has no say, reading or not; with nothing
// included there is no reading.
func TestOutputDispersion_IncludeChoosesTheRows(t *testing.T) {
	unread := domain.ReplicaMetrics{AvgOutputTokens: 5, RequestRate: 50, VariantName: "out"}
	rows := []domain.ReplicaMetrics{spread(256, 1, 4), unread}
	in := func(m domain.ReplicaMetrics) bool { return m.VariantName != "out" }
	d, ok := OutputDispersion(rows, in)
	require.True(t, ok)
	require.InDelta(t, 4, d, 1e-12)
	_, ok = OutputDispersion(rows, func(domain.ReplicaMetrics) bool { return false })
	require.False(t, ok)
}

// Request-weighted, as Mean is: 9 requests a second of 100 tokens beside 1
// of 1000 mean 190, E[O^2] 109000, D 3.0194.
func TestOutputDispersion_RequestWeighted(t *testing.T) {
	rows := []domain.ReplicaMetrics{spread(100, 9, 1), spread(1000, 1, 1)}
	mean, squared, ok := OutputMoments(rows, all)
	require.True(t, ok)
	require.InDelta(t, 190, mean, 1e-9)
	require.InDelta(t, 109000, squared, 1e-9)
	d, _ := OutputDispersion(rows, all)
	require.InDelta(t, 109000.0/(190*190), d, 1e-12)
}

// Zero-rate rows, each way a caller treats them today. Under the request
// rate (saturation, throughput) a row with no rate beside one with a rate
// counts in the every-row check but not in the means; with no rate anywhere
// the means are plain. Under the caller's weights (slo: 1 for every row
// with no rate anywhere) the means are the same plain ones.
func TestOutputDispersion_ZeroRateRows(t *testing.T) {
	idle := spread(1000, 0, 1)
	busy := spread(100, 1, 1)
	d, ok := OutputDispersion([]domain.ReplicaMetrics{busy, idle}, all)
	require.True(t, ok)
	require.InDelta(t, 1, d, 1e-12, "the idle row has no weight beside a rate")

	idleUnread := idle
	idleUnread.AvgOutputTokensSquaredKnown = false
	_, ok = OutputDispersion([]domain.ReplicaMetrics{busy, idleUnread}, all)
	require.False(t, ok, "a weightless row is still in the every-row check")

	plain := []domain.ReplicaMetrics{spread(100, 0, 1), spread(1000, 0, 1)}
	mean, squared, ok := OutputMoments(plain, all)
	require.True(t, ok)
	require.InDelta(t, 550, mean, 1e-9)
	require.InDelta(t, 505000, squared, 1e-9)
	one := func(domain.ReplicaMetrics) float64 { return 1 }
	m2, s2, ok := OutputMoments(plain, all, WeightedBy(one))
	require.True(t, ok)
	require.Equal(t, mean, m2)
	require.Equal(t, squared, s2)
}
