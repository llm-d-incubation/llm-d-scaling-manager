package whatifapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// engineRowsSeries is n cycles of 2 prefill and 2 decode rows at 10 r/s from
// the router, the rows reporting engine r/s in all, the variants stating
// replicas serving of each role.
func engineRowsSeries(n int, engine float64, replicas int) []SeriesSample {
	s := make([]SeriesSample, n)
	for i := range s {
		s[i] = SeriesSample{At: btT0, Rate: 10, RateKnown: true, Prefill: 2, Decode: 2, EngineRate: engine,
			PrefillReplicas: replicas, DecodeReplicas: replicas}
	}
	return s
}

// The rows stand for the fleet when their rate is not more than 20% under
// the router's and a replica is a row at the median cycle; a quarter of
// either is refused, naming what was read; too few cycles, or nothing
// stated, say nothing.
func TestEngineRowsShort(t *testing.T) {
	assert.Empty(t, EngineRowsShort(engineRowsSeries(10, 10, 2)))
	assert.Empty(t, EngineRowsShort(engineRowsSeries(10, 8.01, 2)), "within 20% under")
	assert.NotEmpty(t, EngineRowsShort(engineRowsSeries(10, 7.99, 2)), "past 20% under")
	assert.Empty(t, EngineRowsShort(engineRowsSeries(10, 50, 2)), "over the router is not refused: traffic past the router")
	assert.Empty(t, EngineRowsShort(engineRowsSeries(10, 10, 0)), "no replicas stated")
	assert.Empty(t, EngineRowsShort(engineRowsSeries(3, 2.5, 0)), "too few cycles to compare")

	why := EngineRowsShort(engineRowsSeries(10, 2.5, 2))
	assert.Equal(t, "the engine rows do not stand for the fleet: the engine rows report 2.5 r/s against the router's 10 r/s (25%), "+
		"as a multi-Pod group's rows do when they report one rank of several", why)

	why = EngineRowsShort(engineRowsSeries(10, 10, 8))
	assert.Contains(t, why, "2 prefill engine rows stand for 8 serving replicas (the median cycle)")
	assert.Contains(t, why, "2 decode engine rows stand for 8 serving replicas")

	// A rate the router did not read (the rows' own sum) is not compared.
	s := engineRowsSeries(10, 2.5, 2)
	for i := range s {
		s[i].RateKnown = false
	}
	assert.Empty(t, EngineRowsShort(s))
}

// The row count is the median cycle's: short in fewer than half the cycles
// (a replica starting) stands; in more, it is refused; at 0.75 it stands.
func TestEngineRowsShort_TheMedianCycle(t *testing.T) {
	s := engineRowsSeries(11, 10, 2)
	for i := range 5 {
		s[i].DecodeReplicas = 4
	}
	assert.Empty(t, EngineRowsShort(s), "5 of 11 cycles short")
	s[5].DecodeReplicas = 4
	assert.Contains(t, EngineRowsShort(s), "2 decode engine rows stand for 4 serving replicas", "6 of 11 cycles short")

	s = engineRowsSeries(10, 10, 4)
	for i := range s {
		s[i].Prefill, s[i].Decode = 3, 3
	}
	assert.Empty(t, EngineRowsShort(s), "3 rows for 4 replicas is the limit")
}

// A router queue's arrivals run ahead of what the engines complete: the
// queued cycles are not compared, so a saturated stretch is not refused.
func TestEngineRowsShort_SkipsQueuedCycles(t *testing.T) {
	s := engineRowsSeries(20, 10, 2)
	for i := range 12 {
		s[i].EngineRate, s[i].RouterQueued = 3, true
	}
	assert.Empty(t, EngineRowsShort(s), "the queued cycles read 30%, the rest 100%")
	for i := range s {
		s[i].RouterQueued = false
	}
	assert.Contains(t, EngineRowsShort(s), "(58%)", "compared, the same cycles refuse")
}

// The self-check does not score a recording whose engine rows do not stand
// for the fleet: it is not run, with the reason. (Through the planner, the
// recording is refused before the self-check reads it; this holds the
// self-check's own gate.)
func TestSelfCheck_NotRunOnEngineRowsShort(t *testing.T) {
	e, folds := scEntry(t, 1, 1)
	for i := range e.series {
		e.series[i].RateKnown, e.series[i].EngineRate = true, e.series[i].Rate/4
	}
	sc := selfCheckEntry(t.Context(), e, folds, SelfCheckInfo{})
	assert.Equal(t, SelfCheckNotRun, sc.Status)
	assert.Contains(t, sc.Why, "the engine rows report")
	assert.Contains(t, sc.Why, "(25%)")
}
