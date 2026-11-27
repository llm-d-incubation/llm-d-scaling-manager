package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// servedAt is cycles 15 s apart from t, each the given sum at pods, every pod
// read, no backlog.
func servedAt(w *servedWork, t time.Time, pods int, sums ...float64) time.Time {
	for _, s := range sums {
		w.note(servedWorkSample{at: t, pods: pods, sum: s, ok: true, queueOK: true})
		t = t.Add(15 * time.Second)
	}
	return t
}

// queuedAt is servedAt at two pods with the backlog of each cycle given.
func queuedAt(w *servedWork, t time.Time, sum float64, queues ...int) time.Time {
	for _, q := range queues {
		w.note(servedWorkSample{at: t, pods: 2, sum: sum, ok: true, queue: q, queueOK: true})
		t = t.Add(15 * time.Second)
	}
	return t
}

// cycles is n cycles of v; 21 cycles 15 s apart span ServedWorkWindow.
func cycles(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// The peak is the highest mean over a stretch of ServedWorkWindow: 21 cycles
// 15 s apart span five minutes. One high cycle does not make it.
func TestServedWork_TheBusiestStretch(t *testing.T) {
	var w servedWork
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := servedAt(&w, t0, 2, cycles(21, 10)...)
	at = servedAt(&w, at, 2, 100)
	servedAt(&w, at, 2, cycles(21, 10)...)
	got := w.peaks()
	assert.InDelta(t, (20*10+100)/21.0, got[2], 1e-9, "the stretch holding the spike, averaged over 21 cycles")
	assert.Len(t, got, 1)
}

// A stretch shorter than the window, broken by a gap, by a cycle with a pod
// unread, or by a change of pod count, is no reading.
func TestServedWork_BrokenStretchesAreNoReading(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var short servedWork
	servedAt(&short, t0, 2, cycles(20, 50)...)
	assert.Empty(t, short.peaks(), "20 cycles span 285 s")

	var gap servedWork
	at := servedAt(&gap, t0, 2, cycles(10, 50)...)
	servedAt(&gap, at.Add(2*time.Minute), 2, cycles(11, 50)...)
	assert.Empty(t, gap.peaks())

	var unread servedWork
	at = servedAt(&unread, t0, 2, cycles(10, 50)...)
	unread.note(servedWorkSample{at: at, pods: 2, sum: 50, queueOK: true})
	servedAt(&unread, at.Add(15*time.Second), 2, cycles(10, 50)...)
	assert.Empty(t, unread.peaks())

	var counts servedWork
	at = servedAt(&counts, t0, 2, cycles(11, 50)...)
	servedAt(&counts, at, 3, cycles(11, 50)...)
	assert.Empty(t, counts.peaks())
}

// #477: a two-minute burst over a steady load, carried with the backlog
// growing, is not a rate the pods hold. The served work is the steady load,
// not the burst, and the count is not flagged: a steady stretch was read.
func TestServedWork_ABurstWithABacklogGrowingFloorsAtTheSteadyLoad(t *testing.T) {
	var w servedWork
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := queuedAt(&w, t0, 100, make([]int, 40)...)
	at = queuedAt(&w, at, 200, 4, 8, 12, 16, 20, 24, 28, 32)
	queuedAt(&w, at, 100, append([]int{24, 16, 8}, make([]int, 37)...)...)
	got, unsteady := w.peaksUnsteady()
	assert.InDelta(t, 100.0, got[2], 1.0, "the steady load, within 1%")
	assert.InDelta(t, 100.0, w.windowPeaks()[2], 1.0)
	assert.Empty(t, unsteady)
}

// A backlog that stands, neither growing nor draining, is pods computing at
// what they hold: the stretch is read. One request a pod over the first
// cycle is gauge noise, not growth.
func TestServedWork_AStandingBacklogIsSteady(t *testing.T) {
	var w servedWork
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	queues := make([]int, 21)
	for i := range queues {
		queues[i] = 10 + i%3
	}
	queuedAt(&w, t0, 80, queues...)
	assert.InDelta(t, 80.0, w.peaks()[2], 1e-9)
}

// Served work read only with the backlog growing, or with it unread, floors
// nothing; where a full stretch grew, the count is reported unsteady. A
// backlog not read is not reported as growing, and a growing stretch
// forgotten past prefill.Retention is reported no longer.
func TestServedWork_AGrowingBacklogIsNoReadingAndIsFlagged(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var w servedWork
	queues := make([]int, 30)
	for i := range queues {
		queues[i] = 3 * i
	}
	queuedAt(&w, t0, 150, queues...)
	got, unsteady := w.peaksUnsteady()
	assert.Empty(t, got)
	assert.Equal(t, map[int]bool{2: true}, unsteady)
	servedAt(&w, t0.Add(prefill.Retention+time.Hour), 3, cycles(21, 90)...)
	got, unsteady = w.peaksUnsteady()
	assert.Equal(t, map[int]float64{3: 90}, got)
	assert.Empty(t, unsteady, "the growing stretch at 2 pods is forgotten")

	var unread servedWork
	at := t0
	for range 21 {
		unread.note(servedWorkSample{at: at, pods: 2, sum: 150, ok: true})
		at = at.Add(15 * time.Second)
	}
	got, unsteady = unread.peaksUnsteady()
	assert.Empty(t, got, "a backlog unread is no evidence it was steady")
	assert.Empty(t, unsteady, "nor that it grew")
}

// Work older than prefill.Retention is forgotten, as V_P's samples are.
func TestServedWork_KeepsRetention(t *testing.T) {
	var w servedWork
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	servedAt(&w, t0, 2, cycles(21, 90)...)
	servedAt(&w, t0.Add(prefill.Retention+time.Hour), 2, cycles(21, 20)...)
	assert.InDelta(t, 20.0, w.peaks()[2], 1e-9)
}

// windowPeaks keeps the stretch peaks keeps for prefill.Retention: work the
// pods kept up hours ago still counts, read over the whole window, and a later
// higher stretch wins.
func TestServedWork_WindowPeaksKeepTheForgotten(t *testing.T) {
	var w servedWork
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	servedAt(&w, t0, 2, cycles(21, 90)...)
	servedAt(&w, t0.Add(prefill.Retention+time.Hour), 2, cycles(21, 20)...)
	assert.InDelta(t, 20.0, w.peaks()[2], 1e-9, "peaks forgets, as before")
	assert.InDelta(t, 90.0, w.windowPeaks()[2], 1e-9, "the window keeps the busiest stretch")
	servedAt(&w, t0.Add(2*prefill.Retention), 2, cycles(21, 120)...)
	assert.InDelta(t, 120.0, w.windowPeaks()[2], 1e-9)
}

// The prefill state reads the window's peaks for the V_P raise only with
// saturation.SwitchPrefillVPAtServedWork, and the retention's for the knee
// floor either way.
func TestServedWork_TheStateReadsTheWindowWithTheSwitch(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, on := range []bool{false, true} {
		s := newTTFTSamples(false)
		s.vpAtServed = on
		w := &servedWork{}
		servedAt(w, t0, 5, cycles(21, 90)...)
		servedAt(w, t0.Add(prefill.Retention+time.Hour), 5, cycles(21, 20)...)
		s.served["pre"] = w
		peaks, window, unsteady := s.servedWorkOf("pre")
		assert.InDelta(t, 20.0, peaks[5], 1e-9)
		assert.Empty(t, unsteady)
		if on {
			assert.InDelta(t, 90.0, window[5], 1e-9)
		} else {
			assert.Nil(t, window)
		}
	}
	none, window, unsteady := newTTFTSamples(false).servedWorkOf("pre")
	assert.Nil(t, none)
	assert.Nil(t, window)
	assert.Nil(t, unsteady)
}
