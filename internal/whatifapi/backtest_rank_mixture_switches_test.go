package whatifapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// rankMixtureFitSteps and rankMixtureStratified are independent: each of the
// four combinations fits, and each switch reaches the model on its own.
func TestBacktest_RankMixtureSwitchesCompose(t *testing.T) {
	r := &stubResolver{}
	fit := rmFit(t, r, 3120)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)
	for _, c := range []struct{ fitSteps, stratified bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		req := BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4,
			RankMixtureFitSteps: c.fitSteps, RankMixtureStratified: &c.stratified}
		q, err := backtestQuestionOf(fit, test, req, time.Time{}, "")
		require.NoError(t, err, "%+v", c)
		require.NotNil(t, q.mixture, "%+v", c)
		assert.Equal(t, c.fitSteps, len(q.mixture.m.ShareRows) > 0, "%+v", c)
		assert.Equal(t, c.stratified, q.mixture.m.Stratified, "%+v", c)
	}
}
