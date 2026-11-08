package saturation

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// V_P's busy minutes through Analyze (SwitchPrefillBusyMinutes): a prefill
// row whose queue minimum over the minute was read and above zero, whose KV
// is not full, with decode not back-pressuring, is a busy minute, and V_P is
// the median computed-token rate of such minutes.

// busyMinute is one prefill minute: the computed-token rate, the queue
// minimum with its presence bit, the prefill pod's KV in use (of 16000) and
// data-parallel ranks, and whether decode is full and queued. The prefill
// queue's peak is 3 in every minute.
type busyMinute struct {
	rate       float64
	qmin       float64
	known      bool
	kvTokens   int64
	ranks      int
	decodeFull bool
}

// busyVP is the V_P the busy minutes below are taken at: a work-conserving
// pod that never ran out of work computes at V_P, read with the noise of a
// one-minute rate either side.
const busyVP = 24000.0

// busyMinutes is five minutes in which the pod's queue never emptied, at
// rates whose median is busyVP. Their peak under a queue, the least of the
// best three in a row, is 23500: what P1 read with no queue minimum.
func busyMinutes() []busyMinute {
	rates := []float64{23000, 25000, 24000, 23500, 24500}
	out := make([]busyMinute, 0, len(rates))
	for _, r := range rates {
		out = append(out, busyMinute{rate: r, qmin: 2, known: true})
	}
	return out
}

// busyWarmup is how many cycles the settle tracker holds a new replica out
// for (prefill.SteadyFor at a minute a cycle): they are run first, with no
// queue minimum, so that every minute of a test is admitted.
const busyWarmup = 2

// cycleWith runs one cycle of the harness's P/D model with the prefill row
// m, and returns its observed line.
func (h *observeHarness) cycleWith(m busyMinute) string {
	h.t.Helper()
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	dec := makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500)
	if m.decodeFull {
		// KV at 15000 of 16000, over the 0.8 threshold, and 10 waiting, over
		// the queue threshold of 5: roleSaturated.
		dec = makeReplicaMetrics("decode-0", "decode", 15000, 16000, 10, 4000, 500)
	}
	pre := makeReplicaMetrics("prefill-0", "prefill", m.kvTokens, 16000, 3, 4000, 1)
	pre.EngineRanks = m.ranks
	pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = m.rate, true
	pre.RequestRate, pre.RequestRateKnown = m.rate/4000, true
	pre.QueueMin, pre.QueueMinKnown = m.qmin, m.known
	h.lines = nil
	_, err := h.a.Analyze(h.ctx, makeAnalyzerInput([]domain.ReplicaMetrics{dec, pre}, states))
	require.NoError(h.t, err)
	h.clock = h.clock.Add(time.Minute)
	obs := observedLines(h.lines)
	require.Len(h.t, obs, 1)
	return obs[0]
}

// busyRun drives a harness through the warm-up and then minutes, with
// SwitchPrefillBusyMinutes set to on, and returns the last observed line.
func busyRun(t *testing.T, on bool, minutes []busyMinute) string {
	t.Helper()
	h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM,
		MaxNumBatchedTokens: 8192, EffectiveMaxBatchedTokens: 8192})
	require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillBusyMinutes: on}))
	for range busyWarmup {
		h.cycleWith(busyMinute{rate: 1000})
	}
	var last string
	for _, m := range minutes {
		last = h.cycleWith(m)
	}
	require.Contains(t, last, `"admitted"=1`)
	return last
}

// Known truth: five busy minutes read V_P exactly, the median of their
// rates, and not as a lower bound. With the switch off, the same minutes are
// only a peak under a queue, as P1 shipped: 23500, below V_P. The pod read a
// queue minimum either way (queueMinPods), and busyMinutesOn says whether it
// went to the observation.
func TestPrefillBusy_BusyMinutesRecoverVP(t *testing.T) {
	on := busyRun(t, true, busyMinutes())
	assert.Contains(t, on, `"downstreamSaturated"=false`)
	assert.Contains(t, on, `"vpSource"="busy-minutes"`)
	assert.Contains(t, on, fmt.Sprintf(`"vp"=%v`, busyVP))
	assert.Contains(t, on, `"vpLowerBound"=false`)
	assert.Contains(t, on, `"vpLowerBoundReason"=""`)
	assert.Contains(t, on, `"busyMinutes"=5`)
	assert.Contains(t, on, `"vpSamples"=5`)
	assert.Contains(t, on, `"busyMinutesOn"=true`)
	assert.Contains(t, on, `"queueMinPods"=1`)
	assert.Contains(t, on, `"kvFullPods"=0`)

	off := busyRun(t, false, busyMinutes())
	assert.Contains(t, off, `"vpSource"="peak-under-queue"`)
	assert.Contains(t, off, `"vp"=23500`)
	assert.Contains(t, off, `"busyMinutes"=0`)
	assert.Contains(t, off, `"busyMinutesOn"=false`)
	assert.Contains(t, off, `"queueMinPods"=1`)
}

// A minute whose queue emptied (a queue minimum of zero), or whose queue
// minimum was not read (a value with QueueMinKnown false, which a reader
// must not take for one), is not busy, however fast it ran: decoy minutes of
// each kind at 40000 tokens/s, a queue on their peak, interleaved with the
// five busy ones, leave five busy minutes and V_P at their median. Counted
// as busy, the decoys would raise it to 25000 or above.
func TestPrefillBusy_AnEmptiedOrUnreadMinuteIsNotBusy(t *testing.T) {
	for name, decoy := range map[string]busyMinute{
		"emptied": {rate: 40000, qmin: 0, known: true},
		"unread":  {rate: 40000, qmin: 2, known: false},
	} {
		t.Run(name, func(t *testing.T) {
			busy := busyMinutes()
			minutes := make([]busyMinute, 0, 2*len(busy))
			for _, m := range busy {
				minutes = append(minutes, m, decoy)
			}
			last := busyRun(t, true, minutes)
			assert.Contains(t, last, `"busyMinutes"=5`)
			assert.Contains(t, last, `"vpSource"="busy-minutes"`)
			assert.Contains(t, last, fmt.Sprintf(`"vp"=%v`, busyVP))
		})
	}
}

// Four busy minutes are not enough (prefill.MinBusyMinutes): with the fifth
// replaced by an emptied minute V_P falls back to the peak under a queue.
func TestPrefillBusy_FourBusyMinutesAreNotASource(t *testing.T) {
	minutes := busyMinutes()
	minutes[4].qmin = 0
	last := busyRun(t, true, minutes)
	assert.Contains(t, last, `"busyMinutes"=4`)
	assert.Contains(t, last, `"vpSource"="peak-under-queue"`)
}

// A minute on a prefill pod whose KV is full (kvFull at the policy's 0.8:
// 15000 of 16000 tokens) is not busy, whatever its queue minimum: the pod
// can hold a queue it cannot schedule. The five busy minutes with a full
// KV on each give none; full on one alone leaves four, not a source.
func TestPrefillBusy_AKVFullMinuteIsNotBusy(t *testing.T) {
	all := busyMinutes()
	for i := range all {
		all[i].kvTokens = 15000
	}
	last := busyRun(t, true, all)
	assert.Contains(t, last, `"busyMinutes"=0`)
	assert.Contains(t, last, `"kvFullPods"=1`)
	assert.Contains(t, last, `"vpSource"="peak-under-queue"`)

	one := busyMinutes()
	one[2].kvTokens = 15000
	last = busyRun(t, true, one)
	assert.Contains(t, last, `"busyMinutes"=4`)
	assert.Contains(t, last, `"vpSource"="peak-under-queue"`)

	// Exactly at the threshold is full (kvFull is at or over k1): 12800 of
	// 16000 at 0.8.
	at := busyMinutes()
	for i := range at {
		at[i].kvTokens = 12800
	}
	last = busyRun(t, true, at)
	assert.Contains(t, last, `"busyMinutes"=0`)
	assert.Contains(t, last, `"kvFullPods"=1`)

	// Just under the threshold is not full: 12799 of 16000.
	under := busyMinutes()
	for i := range under {
		under[i].kvTokens = 12799
	}
	last = busyRun(t, true, under)
	assert.Contains(t, last, `"busyMinutes"=5`)
	assert.Contains(t, last, `"kvFullPods"=0`)
}

// Minutes with a queue minimum while decode is back-pressuring prefill
// (decode full and queued, the test computeK2 applies) are not busy: the
// five minutes give none, and V_P is only the peak under a queue.
func TestPrefillBusy_DecodeBackPressureIsNotBusy(t *testing.T) {
	minutes := busyMinutes()
	for i := range minutes {
		minutes[i].decodeFull = true
	}
	last := busyRun(t, true, minutes)
	assert.Contains(t, last, `"downstreamSaturated"=true`)
	assert.Contains(t, last, `"queueMinPods"=1`)
	assert.Contains(t, last, `"busyMinutes"=0`)
	assert.Contains(t, last, `"vpSource"="peak-under-queue"`)
}

// A data-parallel prefill pod's busy minutes are a lower bound: its queue
// minimum is the ranks' least summed, so one rank may have idled. Two ranks
// read V_P at the median, flagged, with the reason.
func TestPrefillBusy_ADataParallelPodIsALowerBound(t *testing.T) {
	minutes := busyMinutes()
	for i := range minutes {
		minutes[i].ranks = 2
	}
	last := busyRun(t, true, minutes)
	assert.Contains(t, last, `"vpSource"="busy-minutes"`)
	assert.Contains(t, last, fmt.Sprintf(`"vp"=%v`, busyVP))
	assert.Contains(t, last, `"vpLowerBound"=true`)
	assert.Contains(t, last, `"vpLowerBoundReason"="DataParallelRanks"`)
}

// After a recording stops, the collector reads no queue minimum
// (queueMinPods=0), but the busy minutes already read stay V_P's source
// until the window passes: five busy minutes, then minutes still queued with
// no minimum read. Thirty minutes after the first busy one, all five count;
// a minute later the first has left, four are not a source, and V_P falls
// back to the peak under a queue.
func TestPrefillBusy_BusyMinutesOutliveARecordingForTheWindow(t *testing.T) {
	h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM,
		MaxNumBatchedTokens: 8192, EffectiveMaxBatchedTokens: 8192})
	for range busyWarmup {
		h.cycleWith(busyMinute{rate: 1000})
	}
	for _, m := range busyMinutes() {
		h.cycleWith(m)
	}
	stopped := busyMinute{rate: busyVP}
	var last string
	// The busy minutes ran at minutes 0 to 4; minutes 5 to 30 read none.
	for range 26 {
		last = h.cycleWith(stopped)
	}
	assert.Contains(t, last, `"queueMinPods"=0`)
	assert.Contains(t, last, `"busyMinutesOn"=true`)
	assert.Contains(t, last, `"busyMinutes"=5`)
	assert.Contains(t, last, `"vpSource"="busy-minutes"`)
	assert.Contains(t, last, fmt.Sprintf(`"vp"=%v`, busyVP))

	last = h.cycleWith(stopped)
	assert.Contains(t, last, `"queueMinPods"=0`)
	assert.Contains(t, last, `"busyMinutes"=4`)
	assert.Contains(t, last, `"vpSource"="peak-under-queue"`)
}
