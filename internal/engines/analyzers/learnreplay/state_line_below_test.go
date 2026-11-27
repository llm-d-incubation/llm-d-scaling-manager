package learnreplay

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// belowRow is one pod of variant at fill k reading a mean ITL of itl.
func belowRow(pod, variant string, k, itl float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{PodName: pod, VariantName: variant, Ready: true,
		KvUsageMean: k, KvUsageMeanKnown: true, AvgITL: itl, AvgITLKnown: true}
}

// belowInput is one cycle: two decode pods at a fill of 0.05 reading itl and
// itl2, and the rows the floor must not read: a prefill pod, a decode pod at
// no fill, one at the band's bottom, one with no ITL, one reading +Inf, and
// one not ready, each reading a mean ITL far under the decode pods'.
func belowInput(itl, itl2 float64) domain.AnalyzerInput {
	notReady := belowRow("d-notready", "dec", 0.05, 0.001)
	notReady.Ready = false
	noITL := belowRow("d-noitl", "dec", 0.05, 0.001)
	noITL.AvgITLKnown = false
	return domain.AnalyzerInput{
		Config:        &config.ScalingPolicy{},
		VariantStates: []domain.VariantReplicaState{{VariantName: "dec", Role: "decode"}, {VariantName: "pf", Role: "prefill"}},
		ReplicaMetrics: []domain.ReplicaMetrics{
			belowRow("d-a", "dec", 0.05, itl),
			belowRow("d-b", "dec", 0.05, itl2),
			belowRow("p-a", "pf", 0.05, 0.001),
			belowRow("d-zero", "dec", 0, 0.001),
			belowRow("d-bottom", "dec", 0.15, 0.001),
			belowRow("d-inf", "dec", 0.05, math.Inf(1)),
			notReady,
			noITL,
		},
	}
}

// The floor below the band is a low percentile of the decode pods' readings
// off a ramp: a prefill pod, a fill of 0, a fill at the band's bottom, a
// missing or infinite ITL and a pod not ready are not read, and neither is a
// pod's first two minutes.
func TestLineSamples_TheFloorBelowTheBandReadsOnlyDecodeOffARamp(t *testing.T) {
	l := noteCycles(lineT0, repeated(30, belowInput(0.011, 0.012))...)
	// Cycles 9 to 30 are off a ramp: 22 cycles of two pods.
	assert.Len(t, l.below["dec"], 44)
	assert.Empty(t, l.below["pf"])
	assert.InDelta(t, 0.011, floorOf(l), 1e-15)
}

// One window that read low does not set the floor: the 10th percentile passes
// over it where the least would not.
func TestLineSamples_OneLowWindowDoesNotSetTheFloor(t *testing.T) {
	in := repeated(30, belowInput(0.011, 0.012))
	in[20] = belowInput(0.001, 0.012)
	l := noteCycles(lineT0, in...)
	assert.InDelta(t, 0.001, slicesMin(l.below["dec"]), 1e-15, "the outlier was read")
	assert.InDelta(t, 0.011, floorOf(l), 1e-15, "and passed over")
}

// Too few readings below the band is no floor, and the line is then not read
// below the band at all (whatif.LowestFill).
func TestLineSamples_TooFewReadingsBelowTheBandIsNoFloor(t *testing.T) {
	// Cycles 9 to 12 are off a ramp: 8 readings, under itl.BelowBandMinReadings.
	l := noteCycles(lineT0, repeated(12, belowInput(0.011, 0.012))...)
	assert.Len(t, l.below["dec"], 8)
	assert.Zero(t, floorOf(l))
	assert.Zero(t, itl.BelowBandFloor(make([]float64, itl.BelowBandMinReadings-1)))
}

// The floor is read from the latest itl.BelowBandWindow readings, so a floor read
// under an older engine ages out.
func TestLineSamples_TheFloorAgesOut(t *testing.T) {
	in := repeated(30, belowInput(0.005, 0.005))
	in = append(in, repeated(itl.BelowBandWindow/2+10, belowInput(0.011, 0.012))...)
	l := noteCycles(lineT0, in...)
	assert.Len(t, l.below["dec"], itl.BelowBandWindow)
	assert.InDelta(t, 0.011, floorOf(l), 1e-15)
}

// The floor's fill is the fill of the reading it was read from, so a planner
// that states B from the floor reads it where it was taken.
func TestLineSamples_TheFloorCarriesItsFill(t *testing.T) {
	in := belowInput(0.0105, 0.012)
	in.ReplicaMetrics[0].KvUsageMean = 0.07
	l := noteCycles(lineT0, repeated(30, in)...)
	got, fill := l.leastBelowBand("dec")
	assert.InDelta(t, 0.0105, got, 1e-15)
	assert.InDelta(t, 0.07, fill, 1e-15)
}

// running sets the requests the input's two decode pods ran, known, -1 for
// a count not read.
func running(in domain.AnalyzerInput, a, b int) domain.AnalyzerInput {
	in.ReplicaMetrics = append([]domain.ReplicaMetrics(nil), in.ReplicaMetrics...)
	for i, n := range []int{a, b} {
		in.ReplicaMetrics[i].RunningRequests, in.ReplicaMetrics[i].RunningRequestsKnown = max(n, 0), n >= 0
	}
	return in
}

// The floor is kept by the sequences it was read at (issue
// llm-d-incubation/llm-d-scaling-manager#286): one pod at 4 sequences reading
// 10 ms and one at 40 reading 20 ms are two groups, each with its floor, and
// the line through them is the floor by count; the fewest sequences read is 4.
// A count not read on one reading leaves the floor standing for every count,
// and a single group is no line.
func TestLineSamples_TheFloorIsKeptByItsCount(t *testing.T) {
	l := noteCycles(lineT0, repeated(30, running(belowInput(0.010, 0.020), 4, 40))...)
	least, byCount := l.belowBandLeastSequences("dec")
	assert.InDelta(t, 4, least, 1e-12)
	assert.InDelta(t, 0.010/36, byCount.A, 1e-12, "the slope through (4, 10 ms) and (40, 20 ms)")
	assert.InDelta(t, 0.010-4*0.010/36, byCount.B, 1e-12)
	assert.InDelta(t, 0.010, floorOf(l), 1e-15, "the floor itself is as before")

	in := repeated(30, running(belowInput(0.010, 0.020), 4, 40))
	in[20] = running(belowInput(0.010, 0.020), 4, -1)
	least, byCount = noteCycles(lineT0, in...).belowBandLeastSequences("dec")
	assert.Zero(t, least, "a count not read")
	assert.True(t, byCount.IsZero())

	least, byCount = noteCycles(lineT0, repeated(30, running(belowInput(0.010, 0.011), 5, 6))...).belowBandLeastSequences("dec")
	assert.InDelta(t, 5, least, 1e-12)
	assert.True(t, byCount.IsZero(), "one group is no line")

	least, byCount = noteCycles(lineT0, repeated(12, running(belowInput(0.010, 0.020), 4, 40))...).belowBandLeastSequences("dec")
	assert.Zero(t, least, "no floor")
	assert.True(t, byCount.IsZero())
}

// floorOf is the floor below the band of the decode variant "dec", in seconds.
func floorOf(l *lineSamples) float64 {
	f, _ := l.leastBelowBand("dec")
	return f
}

func slicesMin(xs []itl.Observation) float64 {
	m := math.Inf(1)
	for _, x := range xs {
		m = min(m, x.ITLSec)
	}
	return m
}
