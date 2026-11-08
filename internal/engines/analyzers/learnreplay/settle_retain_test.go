package learnreplay

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
)

// A model parked whole is in no model group, so the engine forgets its
// settle record (retainSettle), and Generate does the same. A Fidelity
// replay of the generated run must reach the same verdicts: back after its
// parked step, the model's replica joins again, where a replay that kept
// the stale sighting would carry it on as already watched.
func TestReplay_ForgetsTheSettleRecordOfAModelParkedWhole(t *testing.T) {
	for _, tc := range []struct {
		name      string
		allParked int
		joined    int64
	}{
		{"one model parked", -1, 3},
		// A step with every model parked is the engine's cycle with no
		// active variant: it forgets every model, so n joins again too.
		{"every model parked", 1, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gen, err := Replay(context.Background(), parkCaptureFrom(t, fullStart(), 3, tc.allParked), Options{Mode: Generate})
			require.NoError(t, err)
			require.Equal(t, tc.joined, gen.Paths[settlePathPrefix+string(settle.Joined)], "Generate is the engine's")

			rep, err := Replay(context.Background(), gen.Recording, Options{Mode: Fidelity})
			require.NoError(t, err)
			require.Empty(t, rep.Failed())
			for _, r := range settle.Reasons() {
				name := settlePathPrefix + string(r)
				assert.Equal(t, gen.Paths[name], rep.Paths[name], "settle verdict %s", r)
			}
			assert.Zero(t, rep.Paths[settlePathPrefix+string(settle.Rejoined)])
		})
	}
}

// The replay forgets a model's record exactly where the engine's prune
// did: a cycle that observed or held the model keeps it; a cycle that did
// neither forgets it, and so does a cycle with no active variant; two
// BeginCycles under half an interval apart are one cycle (beginSLOCycle's
// catch-up after failed cycles, both on one clock reading), and two a
// little under an interval apart are two; a recording with no BeginCycle
// forgets nothing.
func TestSettleReplay_RetainsWhatTheEngineKept(t *testing.T) {
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	const step = 15 * time.Second
	ev := settle.ObserveEvent{Namespace: "ns", ModelID: "m", Interval: step}
	in := domain.AnalyzerInput{Namespace: "ns", ModelID: "m",
		ReplicaMetrics: []domain.ReplicaMetrics{{PodName: "p", VariantName: "v", Ready: true}}}
	key := settle.Key{Pod: "p", Variant: "v"}
	firstSeen := func(s *settleReplay) time.Time {
		r, ok := s.tracker.Replica("ns/m", key)
		require.True(t, ok, "the replica is not known")
		return r.FirstSeen
	}
	observe := func(s *settleReplay, now time.Time) {
		s.observe(ev, now)
		s.analyze(in, now)
	}
	cycle := func(s *settleReplay, now time.Time, seen bool) {
		s.beginCycle(now)
		if seen {
			observe(s, now)
		}
	}

	t.Run("a cycle that observed the model keeps it", func(t *testing.T) {
		s := newSettleReplay()
		cycle(s, at, true)
		cycle(s, at.Add(step), true)
		assert.Equal(t, at, firstSeen(s))
	})
	t.Run("a held cycle keeps it", func(t *testing.T) {
		s := newSettleReplay()
		cycle(s, at, true)
		s.beginCycle(at.Add(step))
		s.hold(ev, at.Add(step))
		cycle(s, at.Add(2*step), true)
		assert.Equal(t, at, firstSeen(s))
	})
	t.Run("a cycle without the model forgets it", func(t *testing.T) {
		s := newSettleReplay()
		cycle(s, at, true)
		cycle(s, at.Add(step), false)
		cycle(s, at.Add(2*step), true)
		assert.Equal(t, at.Add(2*step), firstSeen(s))
		assert.Equal(t, int64(2), s.pathCounts()[settlePathPrefix+string(settle.Joined)])
	})
	t.Run("a cycle with no active variant forgets it", func(t *testing.T) {
		s := newSettleReplay()
		cycle(s, at, true)
		s.beginCycle(at.Add(step))
		cycle(s, at.Add(2*step), true)
		assert.Equal(t, at.Add(2*step), firstSeen(s))
	})
	t.Run("a cycle begun twice is one cycle", func(t *testing.T) {
		s := newSettleReplay()
		cycle(s, at, true)
		s.beginCycle(at.Add(time.Minute))
		cycle(s, at.Add(time.Minute), true)
		assert.Equal(t, at, firstSeen(s))
	})
	// Real cycles are an interval apart on a wall clock that can step back:
	// 50 ms short of an interval is still the next cycle, so the model it
	// did not observe is forgotten.
	t.Run("a cycle a little under an interval later is a cycle", func(t *testing.T) {
		s := newSettleReplay()
		cycle(s, at, true)
		short := step - 50*time.Millisecond
		cycle(s, at.Add(short), false)
		cycle(s, at.Add(short+step), true)
		assert.Equal(t, at.Add(short+step), firstSeen(s))
		assert.Equal(t, 3, s.cycles)
	})
	// A merged BeginCycle does not move the cycle's start, so a chain of
	// them closer than the threshold cannot stretch one cycle: at 0, 5 and
	// 10 s on a 15 s interval, 5 merges with 0, and 10, 10 s after the
	// cycle began, is the next cycle. Measured from the merged 5 it would
	// merge too. (At 0, 10 and 20 s nothing merges: each is over half an
	// interval after the last.)
	t.Run("a merged BeginCycle does not move the cycle's start", func(t *testing.T) {
		s := newSettleReplay()
		s.beginCycle(at)
		s.beginCycle(at.Add(5 * time.Second))
		assert.Equal(t, at, s.begunAt)
		s.beginCycle(at.Add(10 * time.Second))
		assert.Equal(t, 2, s.cycles)
		assert.Equal(t, at.Add(10*time.Second), s.begunAt)

		s = newSettleReplay()
		for _, d := range []time.Duration{0, 10 * time.Second, 20 * time.Second} {
			s.beginCycle(at.Add(d))
		}
		assert.Equal(t, 3, s.cycles)

		// Half an interval to the nanosecond is the next cycle.
		s = newSettleReplay()
		s.beginCycle(at)
		s.beginCycle(at.Add(step / 2))
		assert.Equal(t, 2, s.cycles)
	})
	// Cycles are an interval apart, the interval the observations record:
	// at 5 s a cycle without the model is a cycle, not the catch-up of the
	// one before, as it would be at the 15 s default.
	t.Run("the recorded interval sets what one cycle is", func(t *testing.T) {
		const short = 5 * time.Second
		s := newSettleReplay()
		s.beginCycle(at)
		s.observe(settle.ObserveEvent{Namespace: "ns", ModelID: "m", Interval: short}, at)
		s.analyze(in, at)
		s.beginCycle(at.Add(short))
		s.beginCycle(at.Add(2 * short))
		s.observe(settle.ObserveEvent{Namespace: "ns", ModelID: "m", Interval: short}, at.Add(2*short))
		s.analyze(in, at.Add(2*short))
		assert.Equal(t, at.Add(2*short), firstSeen(s))
	})
	t.Run("a recording with no BeginCycle forgets nothing", func(t *testing.T) {
		s := newSettleReplay()
		observe(s, at)
		observe(s, at.Add(time.Minute))
		assert.Equal(t, at, firstSeen(s))
	})
}

// A recording with settle events and no slo analyzer has no BeginCycle to
// count cycles by, so its replay retains nothing, where Generate does: it
// says so, once, and does not fail for it. One with an slo analyzer does
// not.
func TestReplay_SettleEventsWithoutSLOAreReported(t *testing.T) {
	count := func(rep *Report, first string) int {
		n := 0
		for _, m := range rep.Mismatches {
			if m.What == settleNoCycles {
				n++
				assert.False(t, m.Fatal)
				assert.Equal(t, first, m.Kind, "reported at the first settle event")
			}
		}
		return n
	}
	gen, err := Replay(context.Background(), parkCapture(t, 3, -1), Options{Mode: Generate})
	require.NoError(t, err)
	rep, err := Replay(context.Background(), gen.Recording, Options{Mode: Fidelity})
	require.NoError(t, err)
	assert.Equal(t, 1, count(rep, settle.KindObserve))
	assert.Empty(t, rep.Failed())

	// The first settle event a hold: reported there, once, as at an
	// observation.
	var buf bytes.Buffer
	rec := learnrec.New(&buf, 0)
	rec.Record(learnrec.KindStart, genAt, defaultStart())
	ev := settle.ObserveEvent{Namespace: "ns", ModelID: "m", Interval: 15 * time.Second}
	rec.Record(settle.KindHold, genAt, ev)
	rec.Record(settle.KindObserve, genAt.Add(15*time.Second), ev)
	require.NoError(t, rec.Err())
	rep, err = Replay(context.Background(), buf.Bytes(), Options{Mode: Fidelity})
	require.NoError(t, err)
	assert.Equal(t, 1, count(rep, settle.KindHold))
	assert.Empty(t, rep.Failed())

	gen, err = Replay(context.Background(), parkCaptureFrom(t, fullStart(), 3, -1), Options{Mode: Generate})
	require.NoError(t, err)
	rep, err = Replay(context.Background(), gen.Recording, Options{Mode: Fidelity})
	require.NoError(t, err)
	assert.Zero(t, count(rep, settle.KindObserve))
}
