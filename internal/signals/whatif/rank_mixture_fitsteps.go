package whatif

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Two fit-step fixes for the rank mixture (rank_mixture_fit.go), each a
// switch of RankMixtureOptions that defaults off.
//
// Clean fit steps (CleanFitSteps). The KV-deferral lines and the prompt
// slopes are fitted on the fit window's steps. A step whose busiest rank sits
// at or over MaxCleanFitRho at the fitted service line is overloaded: its
// queue time is the admission wait of the overload, not a wait for KV, and
// its front end's time grows with the overload, not with the prompt. On a
// public warm-up that ramps into overload, those steps put the KV-deferral
// line's mean at tens of seconds and the location a0 near -25 s. With the
// switch the two fits read only the steps under MaxCleanFitRho (every step
// where fewer than minCleanFitSteps are).
//
// Shares at the load (SharesAtLoad). A router that balances by load spreads
// a busy fleet's requests more evenly than a quiet one's, so the fit
// window's mean share profile, weighted toward its quiet steps, prices a
// busy step as more lopsided than it is: its busiest rank's utilization,
// and so its TTFT, high and its knee low. With the switch the profile priced
// at a rate is the mean of the nearShareSteps fit steps whose rate is
// nearest it.

// MaxCleanFitRho is the busiest rank's utilization from which a fit step is
// left out of the KV-deferral and prompt-slope fits (CleanFitSteps).
const MaxCleanFitRho = 0.9

// minCleanFitSteps is the fewest clean steps those fits read; with fewer,
// every step.
const minCleanFitSteps = 4

// nearShareSteps is the fit steps sharesAt averages (SharesAtLoad).
const nearShareSteps = 5

// ShareRow is one fit step's share profile, busiest first, at its rate.
type ShareRow struct {
	Rate   float64
	Shares []float64
}

// cleanFitSteps is the steps whose busiest rank is under MaxCleanFitRho at
// the service line S0 + x/V, each rank at its own rate and mean computed
// prompt; every step where fewer than minCleanFitSteps are. clean is the
// steps under it, and overloaded the rest.
func cleanFitSteps(steps []MixtureStep, s0, v float64) (fitOn []MixtureStep, clean, overloaded int) {
	out := make([]MixtureStep, 0, len(steps))
	for _, st := range steps {
		top := 0.0
		for _, r := range st.Ranks {
			if r.Rate > 0 && r.Computed > 0 {
				top = max(top, r.Rate*(s0+r.Computed/v))
			}
		}
		if top < MaxCleanFitRho {
			out = append(out, st)
		}
	}
	clean, overloaded = len(out), len(steps)-len(out)
	if clean < minCleanFitSteps {
		return steps, clean, overloaded
	}
	return out, clean, overloaded
}

// fitStepsAssumed is the fit-step fixes in words, for Assumed; "" where
// neither is on.
func (m RankMixture) fitStepsAssumed() string {
	var parts []string
	switch {
	case m.CleanSteps+m.OverloadedSteps == 0:
	case m.CleanSteps < minCleanFitSteps:
		parts = append(parts, fmt.Sprintf("KV-deferral and prompt slopes fitted on every step: %d under busiest-rank utilization %.2g, "+
			"too few to fit on", m.CleanSteps, MaxCleanFitRho))
	default:
		parts = append(parts, fmt.Sprintf("KV-deferral and prompt slopes fitted on the %d steps under busiest-rank utilization %.2g "+
			"(%d left out)", m.CleanSteps, MaxCleanFitRho, m.OverloadedSteps))
	}
	switch {
	case m.ShareRows == nil:
	case len(m.ShareRows) < nearShareSteps:
		parts = append(parts, fmt.Sprintf("shares the fit window's mean: %d steps read them, too few to read them at the load",
			len(m.ShareRows)))
	default:
		parts = append(parts, fmt.Sprintf("shares read at the priced rate, the mean of the %d of %d fit steps nearest it",
			nearShareSteps, len(m.ShareRows)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ")
}

// shareRowsOf is steps' share profiles: each step with all n ranks and a
// positive rate whose ranks carried at least one request a second, as
// ShareProfile reads them, its ranks' shares busiest first. A quieter step's
// profile is a handful of requests.
func shareRowsOf(steps []MixtureStep, n int) []ShareRow {
	var out []ShareRow
	for _, st := range steps {
		if len(st.Ranks) != n || !(st.Rate > 0) {
			continue
		}
		var tot float64
		r := make([]float64, n)
		for i, rk := range st.Ranks {
			r[i] = max(rk.Rate, 0)
			tot += r[i]
		}
		if !(tot > 1) || !Finite(tot) {
			continue
		}
		slices.SortFunc(r, func(a, b float64) int { return cmp.Compare(b, a) })
		for i := range r {
			r[i] /= tot
		}
		out = append(out, ShareRow{Rate: st.Rate, Shares: r})
	}
	return out
}

// sharesAt is the share profile priced at rate: the mean of the
// nearShareSteps rows nearest it where ShareRows holds that many, else
// Shares.
func (m RankMixture) sharesAt(rate float64) []float64 {
	if len(m.ShareRows) < nearShareSteps {
		return m.Shares
	}
	rows := slices.Clone(m.ShareRows)
	slices.SortStableFunc(rows, func(a, b ShareRow) int { return cmp.Compare(math.Abs(a.Rate-rate), math.Abs(b.Rate-rate)) })
	out := make([]float64, len(m.Shares))
	for _, r := range rows[:nearShareSteps] {
		for i := range out {
			if i < len(r.Shares) {
				out[i] += r.Shares[i] / nearShareSteps
			}
		}
	}
	return out
}
