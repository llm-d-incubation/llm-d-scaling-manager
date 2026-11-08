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
