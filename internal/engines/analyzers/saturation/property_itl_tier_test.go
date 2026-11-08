package saturation

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The tier threshold at the gauges' real autocorrelation (2.6, 3.5): a
// per-second path through rate[1m] on a 15 s cycle. edge3 on ols-min-spread
// holds the set value at one instant, which says nothing of a window that
// wanders around it.

// tierKPrice is the fill the saturation line is priced at by default.
const tierKPrice = 0.68

// tierGuardAdm is the admission the analyzers give the line.
func tierGuardAdm() itl.Admission {
	return itl.Admission{TierHysteresis: true, OneSidedAt: tierKPrice}
}

// tierPath is a fleet's readings along a noise-free spread path: pods spread
// over path(minute) around k 0.5, each with its own noise on k and on ITL,
// as rate[1m] reads them.
func tierPath(r *rand.Rand, pods, minutes int, path func(min float64) float64) []func(cycle int) (k, lat float64) {
	secs := minutes * 60
	out := make([]func(int) (float64, float64), pods)
	for p := range pods {
		kn, itln := arNoise(r, secs, 0.02, 0.98), arNoise(r, secs, 0.03, 0.98)
		ks, lats := make([]float64, secs), make([]float64, secs)
		for s := range secs {
			at := -0.5 + float64(p)/float64(pods-1)
			ks[s] = 0.5 + at*path(float64(s)/60) + kn[s]
			lats[s] = propITL(ks[s]) * (1 + itln[s])
		}
		k, lat := sampleTimeBase(ks), sampleTimeBase(lats)
		out[p] = func(c int) (float64, float64) { return k.Rate1m[c], lat.Rate1m[c] }
	}
	return out
}

// tierRun is one Observe per cycle along the path: whether the line stood at
// the full fit each cycle, a dwell's refusal counting as the tier it kept,
// and the line's ITL at tierKPrice.
func tierRun(pods []func(int) (float64, float64), cycles int, adm itl.Admission) (ols []bool, priced []float64) {
	tr := itl.NewTracker()
	at := false
	for c := range cycles {
		rows := make([]domain.ReplicaMetrics, len(pods))
		for p, pod := range pods {
			k, lat := pod(c)
			rows[p] = itlRow(fmt.Sprintf("r%d", p), k, lat)
		}
		r := tr.Observe(propITLKey, "v", rows, adm, propStart.Add(time.Duration(c)*propCycle))
		if r.Tier != "" && !r.TierHeld {
			at = r.Tier == itl.TierOLS
		}
		if r.Tier != "" {
			ols, priced = append(ols, at), append(priced, r.Model.ITLAt(tierKPrice))
		}
	}
	return ols, priced
}

// checkTierExcursion puts the operating point on the threshold and walks the
// noise-free path across it. It does not hold 2.6's one reversal per excursion
// everywhere, and says so: a key resting near the set value leaves for a
// pinned fit that prices the margin more, the safe side, and comes back when
// the spread sets again (DefaultTierDwell apart). Those are the flat bound. On
// the same paths with the latch off the median is 26 to 86 (the negative
// control). Past the band's noise, at 0.32, the tier all but stands still.
func checkTierExcursion(adm itl.Admission) func(uint64) error {
	return func(seed uint64) error {
		for _, pods := range []int{4, 10} {
			r := propRand(seed)
			for _, at := range []float64{0.26, 0.28, 0.30} {
				flat := tierPath(r, pods, 240, func(float64) float64 { return at })
				ols, _ := tierRun(flat, 240*4-4, adm)
				if n := reversals(ols); n > tierFlatReversals {
					return violation(kindWrongSide, "%d pods at a spread of %.2f for 4 h: %d reversals, at most %d", pods, at, n, tierFlatReversals)
				}
			}
			clear32 := tierPath(r, pods, 240, func(float64) float64 { return 0.32 })
			if ols, _ := tierRun(clear32, 240*4-4, adm); reversals(ols) > 4 {
				return violation(kindWrongSide, "%d pods at a spread of 0.32 for 4 h: %d reversals, at most 4", pods, reversals(ols))
			}
			// Up over 60 min, held, down over 60 min: two crossings.
			ramp := tierPath(r, pods, 240, func(m float64) float64 {
				return 0.16 + 0.2*min(max(m-30, 0)/60, 1) - 0.2*min(max(m-150, 0)/60, 1)
			})
			ols, priced := tierRun(ramp, 240*4-4, adm)
			if n := reversals(ols); n > 6 {
				return violation(kindWrongSide, "%d pods over two crossings: %d reversals, at most 6", pods, n)
			}
			// Lipschitz between the switches: the noise moves the line, and
			// no cycle on one tier moves it further than this.
			for i := 1; i < len(priced); i++ {
				if ols[i] == ols[i-1] && math.Abs(priced[i]/priced[i-1]-1) > tierStepBound {
					return violation(kindWrongSide, "%d pods: the priced ITL stepped %.3fx on one tier, cycle %d", pods, priced[i]/priced[i-1], i)
				}
			}
		}
		return nil
	}
}

// Off, each cycle chooses its tier alone: the negative control on the case
// above, which fails on the seed the case runs first.
func TestTierExcursion_OffReversesOnEverySeed(t *testing.T) {
	for seed := range uint64(5) {
		err := checkTierExcursion(itl.Admission{OneSidedAt: tierKPrice})(seed)
		if err == nil {
			t.Fatalf("seed %d: no violation with the latch off", seed)
		}
		t.Logf("seed %d, off: %v", seed, err)
	}
}

// tierFlatReversals is the most tier changes in 4 h on a flat path near the set
// value: at most 13 on 40 seeds at the time of writing.
const tierFlatReversals = 16

// tierStepBound is the largest move of the priced ITL in one cycle on one
// tier: a bound against a step, well over the noise.
const tierStepBound = 0.08

// tierProbeRows is rows at ks, each ITL scale times the line's: a pod is named
// for its k, as a k from the first cycle is steady.
func tierProbeRows(ks []float64, scale float64) []domain.ReplicaMetrics {
	rows := make([]domain.ReplicaMetrics, len(ks))
	for i, k := range ks {
		rows[i] = itlRow(fmt.Sprintf("p%.3f", k), k, scale*propITL(k))
	}
	return rows
}

// tierProbeFleet is every pod of a probe: fourteen, spread over 0.39 of k.
func tierProbeFleet() []float64 {
	ks := make([]float64, 24)
	for i := range ks {
		ks[i] = 0.40 + 0.015*float64(i)
	}
	return ks
}

// tierAtOLS is a tracker whose key stands at the full fit, whose steady window
// then holds only ks, at scale times the line, four cycles of them having
// pushed the fleet's readings out: the evidence the latch stands on.
func tierAtOLS(ks []float64, scale float64) (*itl.Tracker, error) {
	tr := itl.NewTracker()
	if r := tr.Observe(propITLKey, "v", tierProbeRows(tierProbeFleet(), 1), tierGuardAdm(), propStart); r.Tier != itl.TierOLS {
		return nil, fmt.Errorf("the probe's fleet fitted %q", r.Tier)
	}
	for i := 1; i <= 4; i++ {
		tr.Observe(propITLKey, "v", tierProbeRows(ks, scale), tierGuardAdm(), propStart.Add(time.Duration(i)*propCycle))
	}
	return tr, nil
}

// tierProbePods is the probe's pods with a k in [lo, hi].
func tierProbePods(lo, hi float64) []float64 {
	var ks []float64
	for _, k := range tierProbeFleet() {
		if k >= lo-1e-9 && k <= hi+1e-9 {
			ks = append(ks, k)
		}
	}
	return ks
}

// probeClearSpread: a key at the full fit, rows spread over s from k 0.40:
// the full fit is kept from the clear spread up.
func probeClearSpread(_ *rand.Rand, s float64) (bool, error) {
	ks := tierProbePods(0.40, 0.40+s)
	tr, err := tierAtOLS(ks, 1)
	if err != nil {
		return false, err
	}
	r := tr.Observe(propITLKey, "v", tierProbeRows(ks, 1), tierGuardAdm(), propStart.Add(5*propCycle))
	return r.Tier == itl.TierOLS, nil
}

// probeDwell: s seconds after a key took the full fit its spread collapses:
// inside the dwell the tier change is refused.
func probeDwell(_ *rand.Rand, s float64) (bool, error) {
	ks := tierProbePods(0.55, 0.685)
	tr, err := tierAtOLS(ks, 1)
	if err != nil {
		return false, err
	}
	var r itl.Reading
	for at := 5 * propCycle; at <= time.Duration(s)*time.Second; at += propCycle {
		r = tr.Observe(propITLKey, "v", tierProbeRows(ks, 1), tierGuardAdm(), propStart.Add(at))
	}
	return r.TierHeld, nil
}

// probeSafeMargin: readings at x times the line, a collapsed spread: refused
// but for a line the margin higher.
func probeSafeMargin(_ *rand.Rand, x float64) (bool, error) {
	ks := tierProbePods(0.61, 0.745)
	tr, err := tierAtOLS(ks, 1)
	if err != nil {
		return false, err
	}
	// Two cycles at x, so the main window holds nothing else.
	var r itl.Reading
	for i := 5; i <= 6; i++ {
		r = tr.Observe(propITLKey, "v", tierProbeRows(ks, x), tierGuardAdm(), propStart.Add(time.Duration(i)*propCycle))
	}
	return r.TierHeld, nil
}

// tierEdgeCases is the tier's clear spread, dwell and margin, each held at its
// value, and the threshold at autocorrelated noise. The set value is
// ols-min-spread and ols-min-rows; the clear rows want a window no fleet here
// can age to, and TestDwellTier_ClearValuesKeepTheFullFit holds them.
func tierEdgeCases() []propCase {
	return []propCase{
		edgeCase("itl", "ols-clear-spread", tunedDefault, nil, edge3("the full fit kept at spread", probeClearSpread, 0.165, 0.18, 0.195, [3]bool{false, true, true})),
		edgeCase("itl", "tier-dwell", tunedDefault, nil, edge3("a change of tier refused after s", probeDwell, 585, 600, 615, [3]bool{true, false, false})),
		edgeCase("itl", "tier-safe-margin", tunedDefault, nil, edge3("a change of tier refused at a price of", probeSafeMargin, 0.97, 1.029, 1.031, [3]bool{true, true, false})),
		{class: classEdges, learner: "itl", name: "ols-spread-reversals-bounded", family: familyTimeBase, tuning: tunedDefault,
			seeds: cheapSeeds, check: checkTierExcursion(tierGuardAdm())},
	}
}
