package whatifapi

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A backtest step states its own output spread, D = E[O^2]/E[O]^2 over its
// decode rows, to every fleet it prices (Knobs.OutputDispersion), as it
// states its own lengths (issue llm-d-incubation/llm-d-scaling-manager#274):
// the planner prices it in place of the fit's where the replay turned
// saturation.SwitchBacktestStepOutputDispersion on. A step whose rows read
// no E[O^2] states none, and the fit's is priced.
func TestBacktestStep_ReadsItsOwnOutputSpread(t *testing.T) {
	r := &stubResolver{}
	var mu sync.Mutex
	var knobs []Knobs
	r.onFleet = func(k Knobs) { mu.Lock(); knobs = append(knobs, k); mu.Unlock() }
	fitS, fitL := btEarlier(btSeries(240, 0, 1))
	fit := btEntry(t, r, "fit", fitS, fitL, btConfig("H100"))

	// Every output 100 tokens long reads D 1; half at 50 and half at 150
	// read E[O^2] 12500, D 1.25.
	for name, c := range map[string]struct {
		squared float64
		want    float64
	}{
		"equal lengths": {squared: 100 * 100, want: 1},
		"spread":        {squared: 12500, want: 1.25},
		"unread":        {},
	} {
		t.Run(name, func(t *testing.T) {
			s, l := btSeries(240, 0, 1)
			for i := range s {
				s[i].OutputTokensSquared = c.squared
			}
			knobs = nil
			btRun(t, fit, btEntry(t, r, "test-"+name, s, l, btConfig("H100")), BacktestRequest{})
			require.NotEmpty(t, knobs, "fleets were priced")
			for _, k := range knobs {
				assert.InDelta(t, c.want, k.OutputDispersion, 1e-9)
			}
		})
	}
}

// A step's spread pools its cycles weighted by rate, as its lengths are, so
// lengths that move from cycle to cycle are a spread too; one cycle with a
// length and no E[O^2] leaves the step unread, and a cycle with no shape is
// not counted.
func TestOutputDispersionOf_PoolsTheStepsCycles(t *testing.T) {
	cyc := func(rate, ol, sq float64) SeriesSample {
		return SeriesSample{Rate: rate, OutputTokens: ol, OutputTokensSquared: sq, HasShape: true}
	}
	// Two cycles of equal lengths, 100 and 300 tokens, at one rate: E[O]
	// 200, E[O^2] 50000, D 1.25.
	assert.InDelta(t, 1.25, outputDispersionOf([]SeriesSample{cyc(1, 100, 1e4), cyc(1, 300, 9e4)}), 1e-9)
	// The faster cycle weighs three times the other: E[O] 250, E[O^2]
	// 70000, D 1.12, on the 0.05 grid 1.1.
	assert.InDelta(t, 1.1, outputDispersionOf([]SeriesSample{cyc(1, 100, 1e4), cyc(3, 300, 9e4)}), 1e-9)
	assert.Zero(t, outputDispersionOf([]SeriesSample{cyc(1, 100, 1e4), cyc(1, 300, 0)}))
	assert.InDelta(t, 1, outputDispersionOf([]SeriesSample{cyc(1, 100, 1e4), {Rate: 5}}), 1e-9)
	assert.Zero(t, outputDispersionOf(nil))
}
