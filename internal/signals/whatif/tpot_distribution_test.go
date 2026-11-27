package whatif

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// E[1/(L-1)] counts each integer length of a bucket at a weight 1/L, leaves
// out one-token requests, and counts the mass past the last edge at 0.
func TestInverseOutputLength_LogUniformInsideABucket(t *testing.T) {
	v, ok := InverseOutputLength(map[float64]float64{1: 5, 2: 15})
	require.True(t, ok)
	assert.InDelta(t, 1, v, 1e-12, "only L = 2: 1/(L-1) = 1; the one-token requests have no TPOT")
	// L = 3 and 4 at weights 1/3 and 1/4: (1/3 * 1/2 + 1/4 * 1/3) / (7/12) = 3/7.
	v, ok = InverseOutputLength(map[float64]float64{2: 0, 4: 10})
	require.True(t, ok)
	assert.InDelta(t, 3.0/7, v, 1e-12)
	// Half the mass past the last edge halves it.
	v, ok = InverseOutputLength(map[float64]float64{2: 0, 4: 10, math.Inf(1): 20})
	require.True(t, ok)
	assert.InDelta(t, 1.5/7, v, 1e-12)
	// A bucket from 0: its mass is spread over L = 2, 3, 4 at 1/2, 1/3, 1/4:
	// (1/2 + 1/6 + 1/12) / (13/12) = 9/13.
	v, ok = InverseOutputLength(map[float64]float64{4: 10})
	require.True(t, ok)
	assert.InDelta(t, 9.0/13, v, 1e-12)
	_, ok = InverseOutputLength(map[float64]float64{1: 3})
	assert.False(t, ok, "no request of two tokens or more")
}

// The Euler-Maclaurin harmonic agrees with the sum past where it takes over.
func TestHarmonicSpan_AgreesWithTheSum(t *testing.T) {
	s := 0.0
	for l := 10.0; l <= 5000; l++ {
		s += 1 / l
	}
	assert.InDelta(t, s, harmonicSpan(10, 5000), 1e-9)
}

// Without speculation a step emits one token: no renewal spread. With one
// draft accepted half the time, Var(1+A)/(1+E A) = 0.25/1.5.
func TestRenewalCoefficient(t *testing.T) {
	assert.Zero(t, RenewalCoefficient(0, 1.5))
	assert.InDelta(t, 0.25/1.5, RenewalCoefficient(1, 0.5), 1e-9)
	// Three drafts in a chain: p solves p + p^2 + p^3 = accepted.
	p := chainAcceptance(3, 1.8)
	assert.InDelta(t, 1.8, p+p*p+p*p*p, 1e-9)
	assert.Positive(t, RenewalCoefficient(3, 1.8))
}

// Erlang C: one slot is M/M/1, P(wait) = rho; two slots at a = 1 wait 1/3.
func TestSlotWait_ErlangC(t *testing.T) {
	assert.InDelta(t, 0.5, SlotWait(1, 0.5), 1e-12)
	assert.InDelta(t, 1.0/3, SlotWait(2, 1), 1e-12)
	assert.Equal(t, 1.0, SlotWait(4, 4))
	assert.Less(t, SlotWait(64, 10), 1e-12, "far under max-num-seqs no sequence waits")
}

// The lognormal is placed at the mean: its mean is the mean, its quantile
// inverts its CDF, and its spread grows with E[1/(L-1)].
func TestTPOTDist_QuantileCDFAndMean(t *testing.T) {
	d := TPOTDist{GroupCV2: 0.01, C: 3}
	const mean, inv = 0.012, 0.02
	for _, q := range []float64{0.5, 0.9, 0.95} {
		assert.InDelta(t, q, d.CDF(mean, inv, d.Quantile(mean, inv, q)), 1e-9)
	}
	s2 := d.Sigma2(inv)
	assert.InDelta(t, 0.07, s2, 1e-12)
	m := 0.0
	const n = 20000
	for i := range n {
		m += d.Quantile(mean, inv, (float64(i)+0.5)/n) / n
	}
	assert.InEpsilon(t, mean, m, 0.002)
	assert.Greater(t, d.Quantile(mean, 0.05, 0.9), d.Quantile(mean, 0.01, 0.9), "short requests spread more")
	assert.Equal(t, mean, TPOTDist{}.Quantile(mean, inv, 0.9), "no spread is the mean")
}

// tdStep is a step whose histogram is exactly the distribution's at c.
func tdStep(c, mean, inv float64, edges []float64, n float64) TPOTDistStep {
	d := TPOTDist{C: c}
	st := TPOTDistStep{Mean: mean, Inv: inv, Edges: edges, Counts: make([]float64, len(edges))}
	prev := 0.0
	for i, e := range edges {
		f := d.CDF(mean, inv, e)
		st.Counts[i] = n * (f - prev)
		prev = f
	}
	return st
}

// The interval-censored fit recovers C from histograms as coarse as the
// router's, steps of different lengths and means identifying it together;
// a floor above it holds C at the floor, and no step is an error.
func TestFitTPOTDist_RecoversC(t *testing.T) {
	edges := []float64{0.005, 0.01, 0.02, 0.04, 0.06, math.Inf(1)}
	steps := make([]TPOTDistStep, 0, 40)
	for i := range 40 {
		steps = append(steps, tdStep(2.5, 0.011+0.0002*float64(i%10), 0.015+0.001*float64(i%7), edges, 5000))
	}
	c, atBound, err := FitTPOTDist(steps, 0)
	require.NoError(t, err)
	assert.False(t, atBound)
	assert.InDelta(t, 2.5, c, 0.01)
	c, _, err = FitTPOTDist(steps, 4)
	require.NoError(t, err)
	assert.InDelta(t, 4, c, 0.01)
	_, _, err = FitTPOTDist(nil, 0)
	require.ErrorIs(t, err, ErrNoTPOTSteps)
	_, _, err = FitTPOTDist(steps, -1)
	assert.Error(t, err)
}

// A spread the buckets cannot hold (all mass far past every finite edge)
// pushes C to its bound, which the fit reports.
func TestFitTPOTDist_AtTheBound(t *testing.T) {
	edges := []float64{0.005, 0.01, math.Inf(1)}
	st := TPOTDistStep{Mean: 0.002, Inv: 0.01, Edges: edges, Counts: []float64{10, 10, 1000}}
	c, atBound, err := FitTPOTDist([]TPOTDistStep{st, st}, 0)
	require.NoError(t, err)
	assert.True(t, atBound)
	assert.InDelta(t, MaxRenewalC, c, 0.01)
}

// At the histogram's resolution the distribution scores 0 against its own
// histogram, its p90 and p95 in the measured ones' joint brackets; one
// placed at twice the mean misses by the share between.
func TestTPOTDist_BucketScore(t *testing.T) {
	edges := []float64{0.005, 0.01, 0.02, 0.04, math.Inf(1)}
	st := tdStep(3, 0.012, 0.02, edges, 10000)
	d := TPOTDist{C: 3}
	ks, in := d.BucketScore(st, 0.012, 0.9, 0.95)
	assert.InDelta(t, 0, ks, 1e-9)
	assert.Equal(t, []BracketSide{BracketInside, BracketInside}, in)
	ks, _ = d.BucketScore(st, 0.024, 0.9)
	assert.Greater(t, ks, 0.3)
}

// The step reads counts from cumulative rates over the window, repairs a dip,
// and needs the output lengths.
func TestTPOTDistStepOf(t *testing.T) {
	cyc := []DecodeCycle{{Output: map[float64]float64{2: 1, 4: 2}, Groups: []DecodeGroup{{Rate: 1, TokenTime: 0.01}, {Rate: 1, TokenTime: 0.02}}}}
	st, ok := TPOTDistStepOf([]float64{0.01, 0.02, math.Inf(1)}, []float64{1, 0.9, 2}, 300, 0.015, cyc)
	require.True(t, ok)
	assert.Equal(t, []float64{300, 0, 300}, st.Counts)
	assert.InDelta(t, 1.0/9, st.GroupCV2, 1e-12, "token times 0.01 and 0.02: sd 0.005 over mean 0.015")
	_, ok = TPOTDistStepOf([]float64{0.01, math.Inf(1)}, []float64{1, 2}, 300, 0.015, []DecodeCycle{{}})
	assert.False(t, ok, "no output lengths")
}
