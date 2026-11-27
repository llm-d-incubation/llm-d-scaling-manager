package whatifapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The variant's Z is measured part by part at spread 1, A0 is the sum of the
// parts' non-negative intercepts, and the report names the parts; v2's
// report gains none of it. On a fit window under 12 h it abstains, as v2
// does, for its own reason.
func TestBacktest_RankMixtureNLLineFits(t *testing.T) {
	r := &stubResolver{}
	short := rmFit(t, r, 480)
	ts0, tl0 := btSeries(240, 0, 1)
	test0 := btEntry(t, r, "test0", ts0, tl0, btConfig("H100"))
	test0.mixture = btMixture(ts0)
	sh := btRun(t, short, test0, BacktestRequest{TTFTModel: string(whatif.QueueRankMixtureNLLine), PrefillSlots: 4})
	require.NotNil(t, sh.RankMixture)
	assert.Contains(t, sh.RankMixture.Abstained, "too short for its own checks")
	fit := rmFit(t, r, 3120)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)

	v2 := btRun(t, fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4})
	require.NotNil(t, v2.RankMixture)

	out := btRun(t, fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixtureNLLine), PrefillSlots: 4})
	require.NotNil(t, out.RankMixture)
	rm := out.RankMixture
	assert.Equal(t, string(whatif.QueueRankMixtureNLLine), rm.Model)
	require.NotNil(t, rm.ZParts)
	assert.GreaterOrEqual(t, rm.A0, 0.0)
	assert.InDelta(t, rm.ZParts.OverheadA+rm.ZParts.DecodeA+rm.ZParts.RemainderA, rm.A0, 1e-12)
	assert.Equal(t, 1.0, rm.Spread)
	assert.Equal(t, v2.RankMixture.S0, rm.S0, "v2's service line")
	assert.Equal(t, v2.RankMixture.V, rm.V)
	assert.NotContains(t, rm.Abstained, "too short")
	assert.Positive(t, rm.FitMean)
	assert.Contains(t, rm.Assumed, "KV-deferral and prompt slopes fitted on", "v2's fit-step switches, always on")
	assert.Contains(t, strings.Join(out.Assumed, "; "), string(whatif.QueueRankMixtureNLLine))
	b, err := json.Marshal(v2.RankMixture)
	require.NoError(t, err)
	for _, k := range []string{`"zParts"`, `"fitMean"`} {
		assert.NotContains(t, string(b), k, "v2's report is unchanged")
	}
	if rm.Abstained == "" {
		priced := 0
		for _, st := range out.Steps {
			for _, p := range st.TTFT {
				if p.Predicted.Value != nil && st.RankMixture != nil && st.RankMixture.Refused == "" {
					assert.Contains(t, p.Predicted.Flags, string(whatif.RankMixtureTTFT))
					priced++
				}
			}
		}
		assert.Positive(t, priced)
	} else {
		assert.Contains(t, rm.Abstained, "fit window's own")
	}
}

// The variant's own checks: v2's spread and A0 rules do not apply; the fit
// window must price its own mean and p90. identified dispatches to them by
// the model's name.
func TestRankMixtureNL_Identified(t *testing.T) {
	ok := func() *rankMixtureQ {
		return &rankMixtureQ{report: RankMixtureReport{Model: string(whatif.QueueRankMixtureNLLine), Spread: 1, Slots: 4, A0: 0.4,
			FitMean: 1.02, FitP90: 0.97, FoldSpread: []float64{whatif.MaxSpread}}}
	}
	assert.Empty(t, ok().identified().report.Abstained, "v2's spread rule does not apply")
	low := ok()
	low.report.FitMean = 0.7
	assert.Contains(t, low.identified().report.Abstained, "own mean not priced")
	high := ok()
	high.report.FitMean = 1.3
	assert.Contains(t, high.identified().report.Abstained, "own mean not priced")
	short := ok()
	short.report.FitP90 = 0.85
	assert.Contains(t, short.identified().report.Abstained, "own p90 not priced")
	nan := ok()
	nan.report.FitP90 = 0
	assert.Contains(t, nan.identified().report.Abstained, "own p90 not priced")
	brief := ok()
	brief.report.SpreadDefaulted = true
	assert.Contains(t, brief.identified().report.Abstained, "too short")
	k := ok()
	k.report.Slots = 0
	assert.Contains(t, k.identified().report.Abstained, "max-num-seqs not read")
	prop := ok()
	prop.report.ServiceProportional = true
	assert.Contains(t, prop.identified().report.Abstained, "one prompt length")
	// Negative control: the same report as v2's abstains on v2's rule.
	v2 := ok()
	v2.report.Model = string(whatif.QueueRankMixture)
	assert.Contains(t, v2.identified().report.Abstained, "spread not identified")
}

// An unknown model names both; a source without the per-rank series makes
// the variant abstain, named.
func TestBacktest_UnknownTTFTModelNamesTheVariant(t *testing.T) {
	r := &stubResolver{}
	fit := rmFit(t, r, 480)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)
	_, err := backtestQuestionOf(fit, test, BacktestRequest{TTFTModel: "nope"}, time.Time{}, "")
	require.Error(t, err)
	assert.True(t, IsInputError(err))
	assert.Contains(t, err.Error(), string(whatif.QueueRankMixtureNLLine))
	_, err = backtestQuestionOf(fit, test, BacktestRequest{TTFTModel: "rank-mixture-nl"}, time.Time{}, "")
	require.Error(t, err, "the attention variant is not exposed")
	test.mixture = nil
	q, err := backtestQuestionOf(fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixtureNLLine)}, time.Time{}, "")
	require.NoError(t, err)
	assert.Equal(t, string(whatif.QueueRankMixtureNLLine), q.mixture.report.Model)
	assert.Contains(t, q.mixture.report.Abstained, "per-rank series")
	assert.Contains(t, strings.Join(q.assumed, "; "), string(whatif.QueueRankMixtureNLLine)+" abstained")
}
