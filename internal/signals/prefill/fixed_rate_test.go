package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noisyLowLoad is six minutes a prompt length at 1000, 3000 and 6000 tokens
// on a pod whose service time is m*(8 ms + 2 us a token), m cycling 1.0,
// 1.0, 0.8, 1.25, 0.8, 1.25, at a rate that holds the pod at occupancy 0.085
// at m = 1: the minutes at 1.25 read 0.106 by their own service time. The
// overhead cycles with the minute too, 2 to 6 ms. byRate and median set
// Cycle.ALowLoadByRate and Cycle.AMedianOverhead.
func noisyLowLoad(byRate, median bool) Estimate {
	mults := []float64{1.0, 1.0, 0.8, 1.25, 0.8, 1.25}
	overheads := []float64{0.002, 0.004, 0.006, 0.003, 0.005, 0.004}
	o := NewObserver()
	var est Estimate
	i := 0
	for _, ieff := range []float64{1000, 3000, 6000} {
		for j, m := range mults {
			svc := 0.008 + 2e-6*ieff
			rm := lowLoad(ieff, m*svc, 0.085/svc, overheads[j])
			c := cycle(t0.Add(time.Duration(i)*minute), rm)
			c.ALowLoadByRate, c.AMedianOverhead = byRate, median
			est = o.Observe(c)
			i++
		}
	}
	return est
}

// Issue llm-d-incubation/llm-d-scaling-manager#306: by its own service time,
// a minute whose prefill ran slow is cut at the rate a fast one is kept at,
// so the fit reads the fast minutes and its intercept low. By rate, against
// the prompt cluster's service time, every minute at that rate is kept.
func TestLowLoadByRate_ASlowMinuteIsNotCut(t *testing.T) {
	off, on := noisyLowLoad(false, false), noisyLowLoad(true, false)
	require.Empty(t, off.ARefused, "%+v", off)
	require.Empty(t, on.ARefused, "%+v", on)
	// Steady-gated: the first minute at 1000 and the first two at each
	// later length are not steady; of the 13 left, 6 are at m 1.25.
	assert.Equal(t, 13, on.ASamples, "by rate, every steady minute is low load")
	assert.Equal(t, 7, off.ASamples, "by its own service time, the slow minutes are cut")
	assert.Greater(t, on.AIntercept, off.AIntercept)
	assert.Less(t, off.AIntercept, 0.008, "the fast minutes put the intercept under the pod's")
	assert.InDelta(t, 0.008, on.AIntercept, 0.0008, "by rate, the intercept is near the pod's")
}

// A loaded minute is still not low load by rate: thirty requests a second
// against the cluster's 20 ms is occupancy 0.6, whatever its own service
// time. A queued minute is not a candidate at all.
func TestLowLoadByRate_LoadedAndQueuedMinutesAreExcluded(t *testing.T) {
	cs := lowLoadMinutes(0.008, 2e-6, steadyPrompts(1000, 3000, 6000)...)
	for i := range 6 {
		loaded := lowLoad(6000, 3*(0.008+2e-6*6000), 30, 0.0025)
		cs = append(cs, cycle(time.Time{}, loaded))
		if i%2 == 0 {
			q := lowLoad(6000, 3*(0.008+2e-6*6000), 0.5, 0.0025)
			q.QueueLength = 2
			cs = append(cs, cycle(time.Time{}, q))
		}
	}
	for i := range cs {
		cs[i].ALowLoadByRate = true
	}
	est := run(NewObserver(), cs...)
	require.Empty(t, est.ARefused)
	assert.InDelta(t, 0.0105, est.A, 1e-12)
}

// Issue llm-d-incubation/llm-d-scaling-manager#306: a adds the median
// overhead of the low-load minutes, not the least of them, which sits under
// the typical minute.
func TestMedianOverhead_TheTypicalMinuteNotTheLeast(t *testing.T) {
	off, on := noisyLowLoad(true, false), noisyLowLoad(true, true)
	require.Empty(t, off.ARefused, "%+v", off)
	require.Empty(t, on.ARefused, "%+v", on)
	require.True(t, on.AOverheadKnown)
	assert.InDelta(t, 0.003, off.AOverhead, 1e-12, "off, the least")
	// The 13 steady minutes' overheads: 1000 keeps j 1..5 (4, 6, 3, 5, 4
	// ms), 3000 and 6000 keep j 2..5 (6, 3, 5, 4 ms each): median 4 ms.
	assert.InDelta(t, 0.004, on.AOverhead, 1e-12, "on, the median")
	assert.InDelta(t, on.AIntercept+on.AOverhead, on.A, 1e-12)
	assert.InDelta(t, off.AIntercept, on.AIntercept, 1e-12, "the overhead alone moves")
}
