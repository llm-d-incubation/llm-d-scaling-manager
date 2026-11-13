package prefill

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// jointPrefillRow is a settled prefill pod at rate lambda, prefill time t,
// engine queue time queue and TTFT ttft, on prompts of 8000 tokens.
func jointPrefillRow(pod string, lambda, t, queue, ttft float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{
		PodName: pod, Ready: true, RequestRate: lambda, RequestRateKnown: true,
		AvgPrefillTime: t, AvgPrefillTimeKnown: true, AvgQueueTime: queue, AvgQueueTimeKnown: true,
		AvgTTFT: ttft, AvgTTFTKnown: true, AvgInputTokensRecent: 8000,
	}
}

// jointDecodeRow is a decode pod at rate lambda, pull time pull, ITL itl and
// TTFT ttft, on 200-token outputs, with waiting requests queued and running
// in its batch now.
func jointDecodeRow(pod string, lambda, pull, itl, ttft float64, waiting, running int) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{
		PodName: pod, Ready: true, RequestRate: lambda, RequestRateKnown: true,
		AvgKVTransferTime: pull, AvgKVTransferTimeKnown: true, AvgITL: itl, AvgITLKnown: true,
		AvgTTFT: ttft, AvgTTFTKnown: true, AvgOutputTokens: 200,
		QueueLength: waiting, QueueLengthKnown: true, RunningRequests: running, RunningRequestsKnown: true,
	}
}

// Line numbers the staircase's bucket B read (docs/proposals/slo-capacity-planner-ttft.md,
// "First, does the model explain the split?"): ITL = alpha + beta*k, the
// decode pod's own part m_d*ITL + d, and the unshared prefill and pull work.
const (
	stairAlpha, stairBeta = 0.00209, 0.000218
	stairSteps, stairD    = 5.18, 0.0115
	stairWork, stairPull  = 0.025, 0.0246
	stairA0               = 0.014
)

// stairCycle is one cycle of a 2+2 fleet at rate lambda per pod, as the
// staircase's model says it reads: processor sharing on both stages, the
// closed loop on the decode batch. running is what the batch gauge reads.
func stairCycle(now time.Time, lambda float64, running func(k float64) int) JointCycle {
	rho := lambda * stairWork
	t := stairWork / (1 - rho)
	const queue = 8e-6 // vLLM's queue time on a pod that does not queue
	ttftP := queue + t + stairA0/(1-rho)
	pull := stairPull / (1 - lambda*stairPull)
	loop := 200 * lambda * stairBeta
	k := 200 * lambda * stairAlpha / (1 - loop)
	itl := stairAlpha + stairBeta*k
	c := JointCycle{Now: now}
	for i := range 2 {
		c.Prefill = append(c.Prefill, jointPrefillRow(fmt.Sprintf("p%d", i), lambda, t, queue, ttftP))
		c.Decode = append(c.Decode, jointDecodeRow(fmt.Sprintf("d%d", i), lambda, pull, itl,
			pull+stairSteps*itl+stairD, 2, running(k)))
	}
	return c
}

// stairWindow is a window fed the staircase's ladder, 10 cycles a step.
func stairWindow(t *testing.T, running func(k float64) int) *JointWindow {
	t.Helper()
	w := NewJointWindow()
	now := time.Unix(1_700_000_000, 0)
	for _, lambda := range []float64{7, 9, 11, 13, 15, 17} {
		for range 10 {
			w.Observe(stairCycle(now, lambda, running))
			now = now.Add(15 * time.Second)
		}
	}
	return w
}

// Each input comes back as the staircase's model put it in.
func TestJointWindow_LearnsEachInput(t *testing.T) {
	est := stairWindow(t, func(k float64) int { return int(math.Round(k)) }).Estimate()
	assert.InDelta(t, stairWork, est.PrefillWork, 1e-12, "S = T/(1 + lambda*T) inverts the stretch")
	assert.InDelta(t, 8000, est.PrefillWorkPrompt, 1e-9)
	assert.InDelta(t, 8000/stairWork, est.VP, 1e-6, "V_P is the prompt over S")
	assert.InDelta(t, stairA0, est.Overhead, 1e-12, "a0 is the overhead unstretched")
	assert.InDelta(t, stairAlpha, est.ITLAlpha, 1e-9)
	assert.InDelta(t, stairBeta, est.ITLBeta, 1e-9)
	assert.InDelta(t, stairSteps, est.DecodeSteps, 1e-9)
	assert.InDelta(t, stairD, est.DecodeFixed, 1e-9)
	assert.InDelta(t, 200, est.OutputTokens, 1e-9)
	require.NotEmpty(t, est.Pull)
	for _, b := range est.Pull {
		assert.InDelta(t, stairPull, b.Seconds, 1e-12, "the pull's unshared work at %.1f", b.Utilization)
		assert.Equal(t, 8000.0, b.PromptTokens)
	}
	assert.InDelta(t, 17*stairWork, est.PrefillUtilization, 1e-12, "the latest cycle's rho_p")
	assert.True(t, est.PrefillUtilizationKnown)
	for _, r := range []Refusal{est.PrefillWorkRefused, est.OverheadRefused, est.PullRefused, est.ITLRefused,
		est.DecodeRefused} {
		assert.Empty(t, r)
	}
}

// The batch is read over the minute, by Little's law, not from the gauge: an
// instant's count against a minute's ITL flattens the slope. Here the gauge
// reads the batch with an instant's noise of +-40 %, and the line is still
// the one the minutes hold.
func TestJointWindow_TheBatchIsTheMinutesNotTheGauge(t *testing.T) {
	i := 0
	noisy := func(k float64) int {
		i++
		return int(math.Round(k * (1 + 0.4*math.Sin(float64(i)*2.1))))
	}
	est := stairWindow(t, noisy).Estimate()
	assert.InDelta(t, stairBeta, est.ITLBeta, 1e-9, "beta from the gauge would read flatter")
	assert.InDelta(t, stairAlpha, est.ITLAlpha, 1e-9)
}

// A prefill pod whose engine queue held work is not processor sharing, and
// reads nothing: its prefill time is no stretch of its work.
func TestJointWindow_APodThatQueuesReadsNoWork(t *testing.T) {
	w := NewJointWindow()
	now := time.Unix(1_700_000_000, 0)
	for range MinJointReadings {
		w.Observe(JointCycle{Now: now, Prefill: []domain.ReplicaMetrics{
			jointPrefillRow("p0", 30, 0.2, 0.05, 0.3),
		}})
		now = now.Add(15 * time.Second)
	}
	est := w.Estimate()
	assert.Equal(t, NoQueueFreeMinutes, est.PrefillWorkRefused)
	assert.Equal(t, NoOverheadReadings, est.OverheadRefused)
	assert.Zero(t, est.VP)
	assert.False(t, est.PrefillUtilizationKnown, "no queue-free pod, no utilization")
	assert.Zero(t, est.PrefillUtilization)

	// vLLM's queue time is never quite 0: microseconds read queue-free.
	w.Observe(JointCycle{Now: now, Prefill: []domain.ReplicaMetrics{jointPrefillRow("p0", 10, 0.04, queueFreeSeconds, 0.06)}})
	assert.InDelta(t, 0.04/(1+10*0.04), w.work[len(w.work)-1].x, 1e-12)
}

// a0 is the high quantile, so one scrape's spike does not set it.
func TestJointWindow_OneSpikeDoesNotSetTheOverhead(t *testing.T) {
	w := NewJointWindow()
	now := time.Unix(1_700_000_000, 0)
	for i := range 20 {
		ttft := 0.025/(1-10*0.025) + 0.020/(1-10*0.025) // a0 20 ms at rho 0.25
		if i == 7 {
			ttft += 0.010
		}
		w.Observe(JointCycle{Now: now, Prefill: []domain.ReplicaMetrics{
			jointPrefillRow("p0", 10, 0.025/(1-10*0.025), 0, ttft),
		}})
		now = now.Add(15 * time.Second)
	}
	est := w.Estimate()
	assert.InDelta(t, 0.020, est.Overhead, 1e-9, "the spike is past the quantile")
	assert.Equal(t, 20, est.OverheadSamples)
}

// A decode pod behind on its batch says nothing of the line or the pull.
func TestJointWindow_ADecodePodBehindReadsNothing(t *testing.T) {
	w := NewJointWindow()
	now := time.Unix(1_700_000_000, 0)
	pre := []domain.ReplicaMetrics{jointPrefillRow("p0", 10, 0.03, 0, 0.05)}
	// 1 + lambda*pull = 1.4 requests its own pulls hold; 4x that is 5.6.
	w.Observe(JointCycle{Now: now, Prefill: pre, Decode: []domain.ReplicaMetrics{
		jointDecodeRow("d0", 10, 0.04, 0.005, 0.1, 6, 50),
	}})
	assert.Empty(t, w.pull)
	assert.Empty(t, w.itl)
	assert.Empty(t, w.own)
	w.Observe(JointCycle{Now: now, Prefill: pre, Decode: []domain.ReplicaMetrics{
		jointDecodeRow("d0", 10, 0.04, 0.005, 0.1, 5, 50),
	}})
	assert.Len(t, w.pull, 1)
	assert.Len(t, w.itl, 1)
	assert.Len(t, w.own, 1)
}

// The pull is bucketed by the cycle's prefill utilization, and a bucket
// needs MinJointReadings.
func TestJointWindow_ThePullTableIsByPrefillUtilization(t *testing.T) {
	w := NewJointWindow()
	now := time.Unix(1_700_000_000, 0)
	for _, c := range []struct {
		lambda, pull float64
		n            int
	}{{6, 0.025, 4}, {14, 0.030, 4}, {18, 0.035, 3}} {
		for range c.n {
			w.Observe(JointCycle{Now: now,
				Prefill: []domain.ReplicaMetrics{jointPrefillRow("p0", c.lambda, 0.025/(1-c.lambda*0.025), 0, 0.06)},
				Decode:  []domain.ReplicaMetrics{jointDecodeRow("d0", 5, c.pull, 0.004, 0.08, 0, 4)}})
			now = now.Add(15 * time.Second)
		}
	}
	est := w.Estimate()
	require.Len(t, est.Pull, 2, "the third bucket has 3 readings")
	assert.InDelta(t, 0.1, est.Pull[0].Utilization, 1e-12, "rho 0.15 is the bucket from 0.1")
	assert.InDelta(t, 0.025/(1+5*0.025), est.Pull[0].Seconds, 1e-12)
	assert.InDelta(t, 0.3, est.Pull[1].Utilization, 1e-12)
	assert.InDelta(t, 0.030/(1+5*0.030), est.Pull[1].Seconds, 1e-12)
}

// A change of engine drops every reading; an unknown tag keeps them; and a
// reading ages out at jointRetention.
func TestJointWindow_UnderAndRetention(t *testing.T) {
	w := stairWindow(t, func(k float64) int { return int(k) })
	w.Under("")
	w.Under("h200|args-a")
	require.NotEmpty(t, w.work)
	w.Under("h200|args-a")
	assert.NotEmpty(t, w.work, "the same tag keeps them")
	w.Under("h200|args-b")
	assert.Empty(t, w.work)
	assert.Equal(t, NoQueueFreeMinutes, w.Estimate().PrefillWorkRefused)

	w = stairWindow(t, func(k float64) int { return int(k) })
	last := time.Unix(1_700_000_000, 0).Add(59 * 15 * time.Second)
	w.Observe(JointCycle{Now: last.Add(jointRetention + time.Second)})
	assert.Empty(t, w.work, "an hour and a second after the last reading, none is kept")
	assert.Empty(t, w.pull)
}

// jointReads is readings at x with y.
func jointReads(xs, ys []float64) []jointRead {
	out := make([]jointRead, len(xs))
	for i := range xs {
		out[i] = jointRead{x: xs[i], y: ys[i]}
	}
	return out
}

// itl.ValidModel's intercept guard is for a line over KV fill: it admits
// alpha -0.1 ms with beta 0.2 ms. A batch of none takes no negative time, so
// the ITL line refuses it.
func TestJointWindow_TheITLLineRefusesANegativeAlpha(t *testing.T) {
	w := NewJointWindow()
	w.itl = jointReads([]float64{10, 11, 30, 31}, []float64{0.0019, 0.0021, 0.0059, 0.0061})
	est := w.Estimate()
	assert.Equal(t, NoITLLine, est.ITLRefused)
	assert.Zero(t, est.ITLBeta)
}

// The own line refuses a fit that reads the decode pod's own part at or
// below 0 at the least ITL it was read over.
func TestJointWindow_TheOwnLineRefusesANonPositivePart(t *testing.T) {
	w := NewJointWindow()
	w.own = jointReads([]float64{0.002, 0.0021, 0.004, 0.0041}, []float64{0.001, 0.001, 0.2, 0.2})
	assert.Equal(t, NoDecodeOwnLine, w.Estimate().DecodeRefused)
	w.own = jointReads([]float64{0.002, 0.0021, 0.004, 0.0041}, []float64{0.02, 0.0205, 0.03, 0.0305})
	assert.Empty(t, w.Estimate().DecodeRefused, "a line positive over its range stands")
}

// Flat load gives least squares no slope to read: under minJointSpread of x
// neither line is fitted.
func TestJointWindow_FlatLoadFitsNoLine(t *testing.T) {
	w := NewJointWindow()
	w.itl = jointReads([]float64{10, 10.2, 10.4, 10.1}, []float64{0.004, 0.0042, 0.0045, 0.0041})
	w.own = jointReads([]float64{0.004, 0.0041, 0.0042, 0.00405}, []float64{0.03, 0.031, 0.033, 0.032})
	est := w.Estimate()
	assert.Equal(t, NoITLLine, est.ITLRefused)
	assert.Equal(t, NoDecodeOwnLine, est.DecodeRefused)
}

// The batch is read from one-minute figures: the generated-token rate times
// ITL, or else the minute's output length; the five-minute output length
// only where the minute's is unread. n_out is the minute's too.
func TestJointWindow_TheBatchIsReadOverOneMinute(t *testing.T) {
	rm := jointDecodeRow("d0", 10, 0.025, 0.005, 0.08, 1, 20)
	rm.AvgOutputTokens, rm.AvgOutputTokensRecent = 600, 200 // a shape that just changed
	k, out, ok := minuteBatch(rm, 10)
	require.True(t, ok)
	assert.InDelta(t, 10*200*0.005, k, 1e-12)
	assert.Equal(t, 200.0, out)

	rm.GenerationTokenRate, rm.GenerationTokenRateKnown = 2100, true
	k, _, _ = minuteBatch(rm, 10)
	assert.InDelta(t, 2100*0.005, k, 1e-12, "tokens a second over steps a second")

	rm.AvgOutputTokensRecent, rm.GenerationTokenRateKnown = 0, false
	k, out, _ = minuteBatch(rm, 10)
	assert.InDelta(t, 10*600*0.005, k, 1e-12, "the five-minute figure where the minute's is unread")
	assert.Equal(t, 600.0, out)

	w := NewJointWindow()
	rm = jointDecodeRow("d0", 10, 0.025, 0.005, 0.08, 1, 20)
	rm.AvgOutputTokens, rm.AvgOutputTokensRecent = 600, 200
	w.Observe(JointCycle{Now: time.Unix(1_700_000_000, 0), Decode: []domain.ReplicaMetrics{rm}})
	require.Len(t, w.output, 1)
	assert.Equal(t, 200.0, w.output[0].x, "n_out is the minute's")
}

// A queue time that is not a number is no empty queue.
func TestJointWindow_ANaNQueueTimeIsNotQueueFree(t *testing.T) {
	rm := jointPrefillRow("p0", 10, 0.03, math.NaN(), 0.05)
	_, _, ok := queueFreeWork(rm, nil, time.Unix(1_700_000_000, 0))
	assert.False(t, ok)
}

// a0 is the 0.9 quantile of the overhead readings, interpolated between
// order statistics: over 20 readings evenly from 10 to 20 ms, position 17.1
// of 19, 19 ms.
func TestJointWindow_TheOverheadIsTheNinetiethPercentile(t *testing.T) {
	w := NewJointWindow()
	for i := range 20 {
		w.overhead = append(w.overhead, jointRead{x: 0.010 + 0.010*float64(i)/19})
	}
	assert.InDelta(t, 0.019, w.Estimate().Overhead, 1e-12)
}
