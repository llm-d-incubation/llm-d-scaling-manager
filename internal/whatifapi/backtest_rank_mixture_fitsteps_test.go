package whatifapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// rankMixtureFitSteps turns the rank mixture's fit-step fixes on for the
// fit; without it the fit and the answer are the default's.
func TestBacktest_RankMixtureFitSteps(t *testing.T) {
	r := &stubResolver{}
	fit := rmFit(t, r, 3120)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)
	req := BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4}
	q, err := backtestQuestionOf(fit, test, req, time.Time{}, "")
	require.NoError(t, err)
	assert.Nil(t, q.mixture.m.ShareRows, "off by default")
	req.RankMixtureFitSteps = true
	q, err = backtestQuestionOf(fit, test, req, time.Time{}, "")
	require.NoError(t, err)
	assert.NotEmpty(t, q.mixture.m.ShareRows)
	assert.Contains(t, q.mixture.report.Assumed, "shares read at the priced rate")
	// Without the rank mixture the switch has no fit to change: a 400, not
	// a request answered as if it were not given.
	_, err = backtestQuestionOf(fit, test, BacktestRequest{RankMixtureFitSteps: true}, time.Time{}, "")
	require.Error(t, err)
	assert.True(t, IsInputError(err), "%v", err)
	// The request names the switch only when it is on.
	b, err := json.Marshal(BacktestRequest{TTFTModel: string(whatif.QueueRankMixture)})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "rankMixtureFitSteps")
}
