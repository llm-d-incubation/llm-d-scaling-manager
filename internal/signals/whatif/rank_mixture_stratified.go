package whatif

import "math"

// Stratified service moments (RankMixture.Stratified, a switch on by
// default: DefaultRankMixtureStratified).
//
// Each rank's mean service and its cs^2 set its utilization, and the
// sojourn is steep in both near the knee. Draws reads them from the rank's
// own few hundred draws of the computed-token histogram. On a heavy-tailed
// law, where a top bucket holds about 1e-4 of the requests at a whole
// uncached prompt, whether those draws catch that bucket is chance: on a
// production P/D recording the busiest rank's mean service moved by about
// 11 % (sd) from one draw set to another, and its cs^2 by more, so a priced
// figure moved with the seed. With the switch, the moments are integrated
// once per priced rate from the histogram itself (serviceMoments), and each
// rank's draws are stratified: rank draw j falls in the j-th of its per
// equal slices of the law. That leaves the draws' own noise in the
// percentiles only, so a figure moves far less with the seed.

// DefaultRankMixtureStratified is whether the planner prices the rank
// mixture with stratified service moments
// (RankMixtureOptions.StratifiedMoments) where the caller does not say: on.
// On a production P/D recording, a 50 % time split priced with three draw
// seeds, a step's figure moved across the seeds by a median 2.4 to 2.9 %
// and at most 12 to 17 % without them, and by 0.5 to 1.0 % and at most 1.2
// to 2.6 % with them; the median ratios of priced to measured did not move.
// The CLI turns it off with --rank-mixture-stratified=false, a request with
// rankMixtureStratified false.
const DefaultRankMixtureStratified = true

// minStratifiedDraws is the fewest draws a rank takes with the switch, where
// the fleet's total stays within maxStratifiedDraws.
const minStratifiedDraws = 4096

// maxStratifiedDraws bounds what minStratifiedDraws adds over the fleet:
// a rank takes at least min(minStratifiedDraws, maxStratifiedDraws/ranks),
// so up to 16 ranks every rank takes 4096, and 64 ranks take 1024 each.
// Draws holds every draw of a step, and a fit holds every step's draws.
const maxStratifiedDraws = 1 << 16

// stratifiedPer is the draws a rank takes with the switch, where per is
// what it takes without it.
func stratifiedPer(per, ranks int) int {
	return max(per, min(minStratifiedDraws, maxStratifiedDraws/ranks))
}

// serviceMoments is the service line's mean and cs^2 under the
// computed-token histogram, integrated exactly over the law ppf samples:
// uniform inside a bucket starting at or under 1, log-uniform inside any
// other. A fixed grid of the law misreads a wide top bucket's share of the
// second moment (4 % of cs^2 on a bucket from 20k to 1M tokens at 2^16
// points). es is 0 where the law gives no positive mean.
func (m RankMixture) serviceMoments(hx cumHist) (es, cs2 float64) {
	var ex, ex2, prev float64
	for b := range hx.cum {
		w := hx.cum[b] - prev
		prev = hx.cum[b]
		lo, hi := hx.lo[b], hx.hi[b]
		var e1, e2 float64
		switch {
		case lo <= 1:
			e1, e2 = (lo+hi)/2, (lo*lo+lo*hi+hi*hi)/3
		case hi <= lo:
			e1, e2 = lo, lo*lo
		default:
			r := math.Log(hi / lo)
			e1, e2 = (hi-lo)/r, (hi*hi-lo*lo)/(2*r)
		}
		ex += w * e1
		ex2 += w * e2
	}
	es = m.S0 + ex/m.V
	if !(es > 0) || !Finite(es) {
		return 0, 0
	}
	es2 := m.S0*m.S0 + 2*m.S0*ex/m.V + ex2/(m.V*m.V)
	return es, max(es2/(es*es)-1, 0)
}
