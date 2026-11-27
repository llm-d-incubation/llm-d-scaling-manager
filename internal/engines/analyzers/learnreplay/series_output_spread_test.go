package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// A cycle's E[O^2] is its counted decode rows' bound, weighted by rate as
// the output length is (issue llm-d-incubation/llm-d-scaling-manager#274):
// a backtest step reads its own output spread from it. One counted row
// with a length and no E[O^2] leaves it unread, so that the length and the
// bound are one reading of the same rows; another variant's rows count in
// neither.
func TestSeries_TheDecodeRowsOutputSpread(t *testing.T) {
	a := seriesDecodeRow("d1", 3, 1000, 100, 0)
	a.AvgOutputTokensSquared, a.AvgOutputTokensSquaredKnown = 100*100, true
	b := seriesDecodeRow("d1", 1, 1000, 300, 0)
	b.AvgOutputTokensSquared, b.AvgOutputTokensSquaredKnown = 2*300*300, true
	other := seriesDecodeRow("d2", 5, 1000, 900, 0)

	s := sampleOf(seriesInput(9, a, b, other), time.Time{}, SeriesVariants{Prefill: "p1", Decode: "d1"})
	assert.InDelta(t, (3*100+1*300)/4.0, s.OutputTokens, 1e-9)
	assert.InDelta(t, (3*1e4+1*1.8e5)/4.0, s.OutputTokensSquared, 1e-9, "weighted by rate, d1 alone")

	b.AvgOutputTokensSquaredKnown = false
	s = sampleOf(seriesInput(9, a, b, other), time.Time{}, SeriesVariants{Prefill: "p1", Decode: "d1"})
	assert.True(t, s.HasShape)
	assert.Zero(t, s.OutputTokensSquared, "a row with a length and no bound")
}
