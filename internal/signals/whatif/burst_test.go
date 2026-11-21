package whatif

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// burstDecode is residenceDecode at 200 output tokens, the staircase's C
// shape, with no pull held, on a band from 0.02 so that its fills are read
// on the line: Ca^2 cv2.
func burstDecode(cv2 float64) Decode {
	d := residenceDecode(0)
	d.OutputTokens, d.ArrivalCV2 = 200, cv2
	d.Band.Lo = 0.02
	return d
}

// Clustered arrivals put more sequences beside a request than Little's law's
// time average (#291 (b)): (Ca^2 - 1)/N_d of them. Its steps run at
// ITL(k + e), so the fill Little's law reads is the fixed point
// k*C/KVreq = lam*S*ITL(k + e), and the mean TPOT is ITL(k + e). Poisson,
// and a Ca^2 under 1, price as before to the last bit.
func TestDecode_ClusteredArrivalsRaiseTheFillARequestSees(t *testing.T) {
	poisson := burstDecode(0)
	rate := 0.6 * poisson.kneeRate()
	for _, cv2 := range []float64{1, 0.5, 0} {
		d := burstDecode(cv2)
		assert.Equal(t, SLOAtReplicas(poisson, ReplicasQuery{Rate: rate}), SLOAtReplicas(d, ReplicasQuery{Rate: rate}), "Ca^2 %g", cv2)
		assert.Equal(t, poisson.kneeRate(), d.kneeRate(), "Ca^2 %g", cv2)
	}

	d := burstDecode(4)
	e := 3.0 / 2 * 8200 / 1.16e6
	require.InDelta(t, e, d.burstFill(), 1e-15)
	k, ok := d.fillAtReplicaRate(rate / 2)
	require.True(t, ok)
	k0, ok := poisson.fillAtReplicaRate(rate / 2)
	require.True(t, ok)
	require.Greater(t, k0, poisson.Band.Lo, "a fill inside the band, where the line is read at it")
	assert.Greater(t, k, k0, "longer steps hold each request longer")
	assert.InDelta(t, k*1.16e6/8200, rate/2*200*d.Line.ITLAt(k+e), 1e-9, "Little's law at the steps a request runs")

	slo := SLOAtReplicas(d, ReplicasQuery{Rate: rate})
	mean, ok := slo.MeanTPOT.(Answer)
	require.True(t, ok, "%+v", slo.MeanTPOT)
	assert.InDelta(t, d.Line.ITLAt(k+e), mean.Value, 1e-15)
	assert.InDelta(t, k, mean.Basis.Fill, 1e-15, "the basis is the replica's own fill")
	base := SLOAtReplicas(poisson, ReplicasQuery{Rate: rate}).MeanTPOT.(Answer)
	assert.Greater(t, mean.Value, base.Value)

	// More replicas split the clusters: each holds fewer of them.
	d4 := d
	d4.Replicas = 4
	assert.InDelta(t, e/2, d4.burstFill(), 1e-15)

	// The knee is where the slots are full, whatever the arrivals: a full
	// batch is no fuller for them.
	assert.Equal(t, poisson.kneeRate(), d.kneeRate())
}

// The rate for a TPOT target inverts the mean TPOT at a rate, with the
// burst as without it, and a target the burst alone misses has no rate.
func TestDecode_RateForTPOTInvertsTheClusteredFill(t *testing.T) {
	d := burstDecode(4)
	for _, share := range []float64{0.4, 0.6, 0.8} {
		rate := share * d.kneeRate()
		mean, ok := SLOAtReplicas(d, ReplicasQuery{Rate: rate}).MeanTPOT.(Answer)
		require.True(t, ok)
		got, ok := RateForTPOT(d, RateQuery{Target: TPOTTarget{Seconds: mean.Value}, Headroom: 1}).(Answer)
		require.True(t, ok)
		assert.InDelta(t, rate, got.Value, 1e-9*rate, "rate %g", rate)
		assert.InDelta(t, mean.Basis.Fill, got.Basis.Fill, 1e-12, "rate %g", rate)
	}
	// A band whose bottom sits under the burst's fill: a target between the
	// band's floor and the TPOT at the burst alone has no rate.
	low := d
	low.Band.Lo = d.burstFill() / 4
	u, ok := RateForTPOT(low, RateQuery{Target: TPOTTarget{Seconds: low.Line.ITLAt(low.burstFill() / 2)}, Headroom: 1}).(Unanswerable)
	require.True(t, ok)
	assert.Equal(t, TargetBelowFloor, u.Reason)
	assert.Contains(t, u.Detail, "clustering")
}

// Near the knee the fill a request sees stops at k_full, and Little's law
// reads the replica's fill at the full batch's step: the fill stays under
// the slots, and the rate it carries never exceeds the knee.
func TestDecode_TheClusteredFillStopsAtTheFullBatch(t *testing.T) {
	d := burstDecode(16)
	full := d.full()
	knee := d.kneeRate()
	for _, share := range []float64{0.9, 0.97, 0.995} {
		k, ok := d.fillAtReplicaRate(share * knee / 2)
		if !ok {
			continue
		}
		assert.LessOrEqual(t, d.seenFill(k), full.fill+1e-15)
		assert.LessOrEqual(t, k, full.slotFill)
		assert.False(t, math.IsNaN(k))
	}
}
