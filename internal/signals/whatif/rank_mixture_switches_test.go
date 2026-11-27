package whatif

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fit-step switches (SharesAtLoad, CleanFitSteps) and the stratified
// service moments are independent: each of the four combinations prices, and
// both on together is neither alone. Shares at the load move each rank's
// rate; the stratified moments move each rank's service; both move both.
func TestRankMixture_FitStepsAndStratifiedCompose(t *testing.T) {
	base := RankMixture{S0: 0.1, V: 10000, Shares: []float64{0.7, 0.3}}
	atLoad := base
	for range nearShareSteps {
		atLoad.ShareRows = append(atLoad.ShareRows, ShareRow{Rate: 1.5, Shares: []float64{0.5, 0.5}})
	}
	stratOn, both := base, atLoad
	stratOn.Stratified, both.Stratified = true, true

	const rate = 1.5
	draws := map[string]RankMixtureDraws{}
	for name, m := range map[string]RankMixture{"base": base, "atLoad": atLoad, "stratified": stratOn, "both": both} {
		d, u := m.Draws(rate, expHist(2000), pointHist(40000), 4000)
		require.Nil(t, u, name)
		require.NotEmpty(t, d.QP, name)
		draws[name] = d
	}
	hx, ok := newCumHist(expHist(2000))
	require.True(t, ok)
	es, _ := stratOn.serviceMoments(hx)
	require.Positive(t, es)
	// Stratified reads the law's mean service at the share it prices: the
	// window's profile alone, the profile at the load with both.
	assert.InEpsilon(t, rate*0.7*es, draws["stratified"].Checks.Rho[0], 1e-9)
	assert.InEpsilon(t, rate*0.5*es, draws["both"].Checks.Rho[0], 1e-9)
	assert.InEpsilon(t, rate*0.5*es, draws["both"].Checks.Rho[1], 1e-9)
	// The shares at the load price the busiest rank lower, with or without
	// the stratified moments.
	assert.Less(t, draws["atLoad"].Checks.Rho[0], draws["base"].Checks.Rho[0])
	assert.Less(t, draws["both"].Checks.Rho[0], draws["stratified"].Checks.Rho[0])
	for _, other := range []string{"base", "atLoad", "stratified"} {
		assert.NotEqual(t, draws[other].QP, draws["both"].QP, "both on is not %s", other)
		assert.NotEqual(t, draws[other].Checks.Rho, draws["both"].Checks.Rho, "both on is not %s", other)
	}

	// FitCore carries each switch on its own, and all of them together.
	st := syntheticSteps(t, RankMixture{S0: 0.05, V: 20000, Shares: []float64{0.6, 0.4}, Spread: 1}, 12)
	for _, c := range []struct {
		o                  RankMixtureOptions
		rows, stratifiedOn bool
	}{
		{RankMixtureOptions{}, false, false},
		{RankMixtureOptions{CleanFitSteps: true, SharesAtLoad: true}, true, false},
		{RankMixtureOptions{StratifiedMoments: true}, false, true},
		{RankMixtureOptions{CleanFitSteps: true, SharesAtLoad: true, StratifiedMoments: true}, true, true},
	} {
		m, _, err := FitCore(st, c.o)
		require.NoError(t, err, "%+v", c.o)
		assert.Equal(t, c.rows, len(m.ShareRows) > 0, "%+v", c.o)
		assert.Equal(t, c.stratifiedOn, m.Stratified, "%+v", c.o)
	}
}
