package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// fitRowInput is lineInput at fill k with seqs sequences running: 100 tokens
// a second at an ITL of seqs/100 s.
func fitRowInput(k, seqs float64) domain.AnalyzerInput {
	in := lineInput(k, 1000)
	in.ReplicaMetrics[0].AvgITL, in.ReplicaMetrics[0].AvgITLKnown = seqs/100, true
	return in
}

// A row's sequences are its token rate times its mean ITL (Little), over the
// tokens a speculating step emits, else the running-requests gauge, else
// none.
func TestRowSequences(t *testing.T) {
	rm := fitRowInput(0.5, 210).ReplicaMetrics[0]
	assert.InDelta(t, 210, rowSequences(rm), 1e-9, "Little: 100 tokens/s x 2.1 s")
	spec := rm
	spec.SpecAcceptedPerStep, spec.SpecAcceptedPerStepKnown = 1, true
	assert.InDelta(t, 105, rowSequences(spec), 1e-9, "a step of 2 tokens: 2.1 s is the step's time")
	rm.AvgITL, rm.AvgITLKnown = 0, false
	rm.RunningRequests, rm.RunningRequestsKnown = 180, true
	assert.InDelta(t, 180, rowSequences(rm), 1e-9, "no ITL: the gauge")
	rm.RunningRequestsKnown = false
	assert.Zero(t, rowSequences(rm), "neither")
}

// cyclesOf is the cycles of rows, in order.
func cyclesOf(rows []whatif.LineRow) []int {
	out := make([]int, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Cycle)
	}
	return out
}

// A refitted line's fit rows are the steady rows of the cycles its readings
// span; a held line's, the rows of the window that ends where the line was
// last its own fit, however much later the state is read. Any other line
// has none, and the planner reads the latest rows.
func TestFitRows_TheLineOwnOrTheHeldFits(t *testing.T) {
	// Cycles 1-12 at 210 sequences, 13-40 at 120: each level steady from
	// its ninth cycle (the gate's two minutes at 15 s).
	inputs := append(repeated(12, fitRowInput(0.6, 210)), repeated(28, fitRowInput(0.35, 120))...)
	l := newLineSamples()
	at := func(c int) time.Time { return lineT0.Add(time.Duration(c-1) * 15 * time.Second) }
	for i, in := range inputs {
		c := i + 1
		l.note(in, c, at(c), nil)
		if c <= 12 {
			l.noteOwnFit("v", at(c))
		}
	}

	own := l.fitRows(VariantState{Variant: "v", Tier: whatif.TierOLS, Refitted: true, ReadingsFrom: 30, ReadingsTo: 40})
	assert.Equal(t, []int{30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40}, cyclesOf(own), "the readings' cycles")
	for _, r := range own {
		assert.InDelta(t, 120, r.Sequences, 1e-9)
	}

	held := l.fitRows(VariantState{Variant: "v", Tier: whatif.TierHeld})
	assert.Equal(t, []int{9, 10, 11, 12}, cyclesOf(held), "the held fit's: steady rows up to its last own cycle")
	for _, r := range held {
		assert.InDelta(t, 210, r.Sequences, 1e-9)
	}

	assert.Nil(t, l.fitRows(VariantState{Variant: "v", Tier: whatif.TierBorrowed}), "a borrowed line: none")
	assert.Nil(t, l.fitRows(VariantState{Variant: "v", Tier: whatif.TierOLS}), "not refitted: none")
	assert.Nil(t, l.fitRows(VariantState{Variant: "w", Tier: whatif.TierHeld}), "never its own fit: none")
}

// A line fitted above the band (whatif.TierAboveBand) was fitted on the rows
// above it, and its fit rows are those too; any other line's are in the band
// alone, even over the same cycles.
func TestFitRows_AnAboveBandLineReadsTheRowsAboveIt(t *testing.T) {
	// Cycles 1-12 in the band at 120 sequences, 13-40 above it at 250.
	inputs := append(repeated(12, fitRowInput(0.4, 120)), repeated(28, fitRowInput(0.9, 250))...)
	l := newLineSamples()
	for i, in := range inputs {
		l.note(in, i+1, lineT0.Add(time.Duration(i)*15*time.Second), nil)
	}
	above := l.fitRows(VariantState{Variant: "v", Tier: whatif.TierAboveBand, Refitted: true, ReadingsFrom: 9, ReadingsTo: 40})
	require.NotEmpty(t, above)
	most := 0.0
	for _, r := range above {
		most = max(most, r.Sequences)
	}
	assert.InDelta(t, 250, most, 1e-9, "the rows above the band the line was fitted on")

	inBand := l.fitRows(VariantState{Variant: "v", Tier: whatif.TierOLS, Refitted: true, ReadingsFrom: 9, ReadingsTo: 40})
	assert.Equal(t, []int{9, 10, 11, 12}, cyclesOf(inBand), "a line in the band: its rows in the band")
	l.noteOwnFit("v", lineT0.Add(39*15*time.Second))
	held := l.fitRows(VariantState{Variant: "v", Tier: whatif.TierHeld})
	require.NotEmpty(t, held)
	for _, r := range held {
		assert.False(t, r.AboveBand, "a held line: its rows in the band, cycle %d", r.Cycle)
	}
}

// The fit rows reach back fitRowsAge from the latest cycle, or from the last
// own fit where the line has been held since: a hold outlives the window.
func TestFitRows_AHoldKeepsItsFitsRows(t *testing.T) {
	l := newLineSamples()
	for c := 1; c <= 12; c++ {
		l.note(fitRowInput(0.6, 210), c, lineT0.Add(time.Duration(c-1)*15*time.Second), nil)
	}
	l.noteOwnFit("v", lineT0.Add(11*15*time.Second))
	late := lineT0.Add(2 * fitRowsAge)
	l.note(fitRowInput(0.6, 210), 13, late, nil)
	held := l.fitRows(VariantState{Variant: "v", Tier: whatif.TierHeld})
	require.NotEmpty(t, held, "held since cycle 12: its rows are kept")
	assert.Equal(t, 12, held[len(held)-1].Cycle)

	gone := newLineSamples()
	for c := 1; c <= 12; c++ {
		gone.note(fitRowInput(0.6, 210), c, lineT0.Add(time.Duration(c-1)*15*time.Second), nil)
	}
	require.Len(t, gone.fit["v"], 4, "cycles 9-12 off the ramp")
	gone.note(fitRowInput(0.6, 210), 13, late, nil)
	assert.Equal(t, []int{13}, cyclesOf(gone.fit["v"]), "no own fit: the rows past the window are dropped")
}
