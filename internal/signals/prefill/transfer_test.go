package prefill

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// transferRows is n serving decode rows reading seconds, each with queue waiting.
func transferRows(n, queue int, seconds float64) []domain.ReplicaMetrics {
	rows := make([]domain.ReplicaMetrics, n)
	for i := range rows {
		rows[i] = domain.ReplicaMetrics{Ready: true, QueueLength: queue, AvgKVTransferTime: seconds, AvgKVTransferTimeKnown: true}
	}
	return rows
}

func TestTransferWindow_AQuietFigureIsHeldUntilItsAge(t *testing.T) {
	var w TransferWindow
	at := time.Unix(1000, 0)
	w.Observe(at, transferRows(2, 0, 0.03))
	assert.Equal(t, TransferReading{Quiet: 0.03, AllRows: 0.03}, w.Reading())

	w.Observe(at.Add(HeldDecodeAge), transferRows(2, 9, 0.34))
	r := w.Reading()
	assert.InDelta(t, 0.03, r.Figure(), 1e-12, "at the bound it stands")
	assert.True(t, r.Held)

	w.Observe(at.Add(HeldDecodeAge+time.Second), transferRows(2, 9, 0.34))
	r = w.Reading()
	assert.InDelta(t, 0.34, r.Figure(), 1e-12, "past the bound every row's figure stands")
	assert.False(t, r.Held)
	assert.Zero(t, r.Quiet)
}

func TestTransferWindow_NoReadingAgesToNothing(t *testing.T) {
	var w TransferWindow
	at := time.Unix(1000, 0)
	w.Observe(at, transferRows(2, 0, 0.03))
	w.Observe(at.Add(time.Minute), nil)
	assert.Positive(t, w.Reading().Figure(), "held inside the age")
	assert.True(t, w.Reading().Held)
	w.Observe(at.Add(2*HeldDecodeAge), nil)
	assert.Zero(t, w.Reading().Figure(), "the caller's TransferOmitted")
}

func TestTransferWindow_AChangeOfEngineSweeps(t *testing.T) {
	for name, under := range map[string][2]string{"accelerator": {"B", "args"}, "args": {"A", "other"}} {
		var w TransferWindow
		at := time.Unix(1000, 0)
		w.Under("A", "args")
		w.Observe(at, transferRows(2, 0, 0.03))
		w.Under(under[0], under[1])
		assert.Zero(t, w.Reading().Figure(), name)
	}
	var w TransferWindow
	w.Under("A", "args")
	w.Observe(time.Unix(1000, 0), transferRows(2, 0, 0.03))
	w.Under("", "")
	assert.Positive(t, w.Reading().Figure(), "an unknown value changes nothing")
}

func TestTransferWindow_OneQuietPodIsNotAQuietFleet(t *testing.T) {
	var w TransferWindow
	mix := append(transferRows(1, 0, 0.01), transferRows(1, 9, 2)...)
	w.Observe(time.Unix(1000, 0), mix)
	r := w.Reading()
	assert.Zero(t, r.Quiet)
	assert.InDelta(t, 1.005, r.Figure(), 1e-12)
}

func TestTransferWindow_AveragesItsCycles(t *testing.T) {
	var w TransferWindow
	at := time.Unix(1000, 0)
	for i := range TransferWindowCycles + 5 {
		s := 0.01
		if i < 5 {
			s = 9 // pushed out by count, inside the age
		}
		w.Observe(at.Add(time.Duration(i)*time.Second), transferRows(1, 0, s))
	}
	assert.InDelta(t, 0.01, w.Reading().Figure(), 1e-12)
}

func TestTransferWindow_SkipsARowThatReadsNothing(t *testing.T) {
	var w TransferWindow
	rows := transferRows(4, 0, 0.03)
	rows[0].AvgKVTransferTime = math.NaN()
	rows[1].AvgKVTransferTime = 0
	rows[2].AvgKVTransferTimeKnown = false
	rows[1].QueueLength, rows[2].QueueLength = 9, 9 // no reading, so no say
	w.Observe(time.Unix(1000, 0), rows)
	assert.InDelta(t, 0.03, w.Reading().Quiet, 1e-12)
}

// With PullWork a row's reading is the pull's work at the row's request rate,
// T/(1 + lambda T): the work whose processor-sharing time at lambda is the time
// measured, so pricing it at lambda reads the measured time back, where pricing
// the time itself stretched it twice (issue
// llm-d-incubation/llm-d-scaling-manager#301). Off, the time as read. A row
// with no rate read is no stretch.
func TestTransferWindow_PullWorkTakesTheStretchOut(t *testing.T) {
	// The public P/D staircase's last cycles: 60 ms at 20.8 r/s a decode pod.
	rows := transferRows(2, 3, 0.060)
	for i := range rows {
		rows[i].RequestRate, rows[i].RequestRateKnown = 20.8, true
	}
	at := time.Unix(1000, 0)

	var off TransferWindow
	off.Observe(at, rows)
	assert.InDelta(t, 0.060, off.Reading().Figure(), 1e-12, "off, the time as read")

	on := TransferWindow{PullWork: true}
	on.Observe(at, rows)
	s := on.Reading().Figure()
	assert.InDelta(t, 0.060/(1+20.8*0.060), s, 1e-12)
	// The round trip: the processor-sharing time of that work at the rate
	// it was read at is the time measured.
	assert.InDelta(t, 0.060, s/(1-20.8*s), 1e-12)
	// At a light load the work is the time, near enough.
	light := transferRows(1, 0, 0.0319)
	light[0].RequestRate, light[0].RequestRateKnown = 0.5, true
	var lw TransferWindow
	lw.PullWork = true
	lw.Observe(at, light)
	assert.InDelta(t, 0.0319, lw.Reading().Figure(), 0.001)

	unread := transferRows(1, 0, 0.060)
	unread[0].RequestRate = 20.8 // not known: not read
	var uw TransferWindow
	uw.PullWork = true
	uw.Observe(at, unread)
	assert.InDelta(t, 0.060, uw.Reading().Figure(), 1e-12, "no rate read, no stretch taken out")

	// A rate read but not finite and above 0 takes nothing out either.
	for _, rate := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		bad := transferRows(1, 0, 0.060)
		bad[0].RequestRate, bad[0].RequestRateKnown = rate, true
		bw := TransferWindow{PullWork: true}
		bw.Observe(at, bad)
		assert.InDelta(t, 0.060, bw.Reading().Figure(), 1e-12, "rate %g", rate)
	}
}
