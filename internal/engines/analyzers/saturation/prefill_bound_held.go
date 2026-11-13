package saturation

import (
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The prefill bound under measurement noise (issue ev-shindin/llm-scaling-manager#397).
//
// The bound's argument is that each saturated reading is a lower bound on
// prefill's rate, so the max of them is the tightest one. That holds for a
// noise-free reading. A real one carries the rate gauge's noise -- 7.5-18.9 %
// per pod on the E1 recording -- and the max of noisy readings is not a lower
// bound: it picks the noise. Measured through notePrefillBound at one pod, a
// running max read the true rate 15 % high after ten minutes at 10 % noise,
// 23 % after ten minutes at 15 %, and 50 % after a day at 15 %. That is the
// side that breaks the SLO: a bound read high says each prefill replica
// serves more than it does, so the floor orders fewer of them.
//
// What is a lower bound under noise is a lower CONFIDENCE bound on a window's
// mean: mean - t * sd / sqrt(n), with t Student's quantile on n - 1 degrees of
// freedom, since the spread is read from the same n samples. The max of lower
// confidence bounds is again a lower bound if the confidence covers every one
// of them, so the bound counts the ones it takes and takes the k-th at
// 1 - alpha/(k(k+1)): those sum to alpha, so the chance that ANY of them
// stands above the true rate stays under alpha however long the bound runs.
// At a fixed confidence the held figure drifts up again, more slowly, with
// the number of windows it has seen (at alpha/k, over the true rate on 2 of
// 10 simulated days). It takes one at three and at six complete samples while
// a window fills, and then once per full window of samples all new since the
// last, so no two full windows share a sample.
//
// So the window holds the minutes' samples as read, not raised, and the bound
// priced is the larger of two figures:
//
//   - the window's mean, the estimand under stationary noise (the mean across
//     pods, as the fold takes it within a minute, and the mean over minutes);
//   - the held figure: the max of the lower confidence bounds taken.
//
// The held figure keeps what the old max kept it for. A reading behind a full
// decode is decode's admission shared over the prefill fleet, so once the
// bound grew that fleet the readings fall, and pricing on them grew the fleet
// again on every decode episode. The held figure never falls, and carries
// past a new window after EpisodeGap as RaiseLast did. Noise-free, the spread
// is nil and it holds the level exactly. Under noise it holds the level less
// t * sd / sqrt(n): the fleet grows once by that much, then stays.
//
// The error goes to the safe side. The held figure is under the true rate,
// with confidence 1 - alpha over the bound's whole life; the mean is
// unbiased. Their max is above the mean only when the window's mean dips
// under the held figure. Measured at one pod and 19 % noise, the worst case,
// under stationary noise with one read a pod a minute: within 0.2 % of the
// true rate after ten minutes and after an hour, 1.2 % high after a day (the held figure, under the true rate, catching the
// mean's low dips). A rise reaches the bound through the mean, by its weight,
// not in one minute as the max took it: the bound reads low while the window
// turns over, which orders more prefill, not less.
//
// The confidence holds only if each minute's sample is an unbiased,
// roughly independent reading of the rate. With the within-minute fold's
// max over one pod's re-reads (SwitchMuRereadTakesLatest off, issue #55) it
// is not: each sample is biased high, and with four reads a minute at AR(1)
// noise (phi 0.3) the held figure stood above the true rate on 47 of 50 seeds
// after an hour and 50 of 50 after a day, at 10 % and at 15 % noise, and the
// bound was high by about that fold's bias (8.8 % at 10 % noise, 13 % at
// 15 %, simulated). With the switch on, the constructor's default, a sample
// is each pod's latest reading taken while it is still saturated, unbiased
// in that simulation (TestRereadFold_TheBiasAtTheMeasuredNoise).
//
// The held figure falls only when the window is evicted, put aside by an
// engine-argument change, forgotten with the variant (or a flush of the
// learned state), or lost to a restart: where the old max fell too. The
// busy-share bound's (prefill_busy_share.go), which shares this machinery,
// also falls when the key's prompt length moves past
// shape.DefaultChangeTolerance of the length it was last raised at.
//
// The back-pressure bound's held figure is working state, not in the
// snapshot. After a restart, or a window put aside and brought back, the
// bound is the window's mean until it takes a lower confidence bound again;
// a restore from a build that raised its samples brings back raised values,
// whose mean is at most the max that build priced. The busy-share bound's is
// in the snapshot with its window (ThroughputSnapshot.Held) and is restored
// with it, unless it carries no prompt length; put aside with its window by
// an engine-argument change, it is dropped like the other.

// SwitchPrefillBoundHeldConfidence is the replay switch for the prefill
// bound's estimand under noise (issue ev-shindin/llm-scaling-manager#397). On, the bound's window holds the
// minutes' samples as read, and the bound priced is the larger of the
// window's mean and the max of its lower confidence bounds over the bound's
// life (prefillHeldBound). Off, each sample is raised to the window's max
// when it started, and the bound priced is that max, as ev-shindin/llm-scaling-manager#395 shipped.
const SwitchPrefillBoundHeldConfidence = "prefill-bound-held-confidence"

// prefillBoundAlpha is the chance, over the bound's whole life, that the held
// figure stands above the true rate: the k-th lower confidence bound is taken
// at confidence 1 - prefillBoundAlpha/(k(k+1)), and those sum to it.
const prefillBoundAlpha = 0.05

// prefillBoundFillTests is the sizes, in complete samples, at which a window
// still filling takes a lower confidence bound: early enough that a bound
// learned in its first minutes holds, few enough to leave the full windows
// most of prefillBoundAlpha. Once full, a window takes one each time all its
// samples are new since the last, so no two full windows share a sample.
// Either way a window is tested only when every sample in it was taken since
// the held figure started (prefillHeldBound.fresh): a window restored from a
// build that raised each sample to its running max holds that max, equal
// values with no spread, and tested as it stands it would pin the held
// figure at the old build's high reading for good.
var prefillBoundFillTests = []int{3, 6}

// prefillHeldBound is the prefill bound's held figure for one window key.
type prefillHeldBound struct {
	// tokens is the max of the lower confidence bounds taken, in computed
	// tokens/s; 0 until one is above zero.
	tokens float64
	// tests is how many lower confidence bounds have been taken.
	tests int
	// fresh is how many samples started since the last full window's bound
	// was taken, or since the figure was first kept: a window is tested only
	// when all its samples are among them.
	fresh int
	// prompt is, for the busy-share bound only (prefill_busy_share.go), the
	// effective prompt length the held figure was last raised at; zero for
	// the back-pressure bound, which keeps none.
	prompt float64
}

// raiseHeldBoundLocked counts a sample started on key's window, whose
// complete samples before it were samples, and when they are a window to
// test -- one of prefillBoundFillTests while filling, or a full window of
// samples all new since the last full one -- raises the held figure to their
// lower confidence bound. Callers hold a.mu.
func (a *SaturationAnalyzer) raiseHeldBoundLocked(key string, samples []float64) {
	h := a.prefillBoundHeld[key]
	h.fresh++
	full := len(samples) >= capacity.RollingAverageWindowSize
	switch {
	case full && h.fresh > len(samples):
		h.fresh = 1
	case !full && h.fresh > len(samples) && slices.Contains(prefillBoundFillTests, len(samples)):
	default:
		a.prefillBoundHeld[key] = h
		return
	}
	h.tests++
	q := prefillBoundAlpha / float64(h.tests*(h.tests+1))
	if lcb := lowerConfidenceBound(samples, q); lcb > h.tokens {
		h.tokens = lcb
	}
	a.prefillBoundHeld[key] = h
}

// lowerConfidenceBound is mean - t * sd / sqrt(n) over values (n >= 2), with t
// Student's quantile on n - 1 degrees of freedom whose upper tail is q.
// Equal values give their value: no spread, no allowance.
func lowerConfidenceBound(values []float64, q float64) float64 {
	n := float64(len(values))
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= n
	ss := 0.0
	for _, v := range values {
		ss += (v - mean) * (v - mean)
	}
	if ss == 0 {
		return mean
	}
	sd := math.Sqrt(ss / (n - 1))
	return mean - studentTQuantile(q, len(values)-1)*sd/math.Sqrt(n)
}

// studentTQuantile is the t with P(T > t) = q for Student's t on nu degrees of
// freedom (0 < q < 0.5, nu >= 1), by bisection on studentTTail.
func studentTQuantile(q float64, nu int) float64 {
	lo, hi := 0.0, 1.0
	for studentTTail(hi, nu) > q && hi < 1e15 {
		lo, hi = hi, 2*hi
	}
	for range 200 {
		mid := (lo + hi) / 2
		if studentTTail(mid, nu) > q {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo <= 1e-12*hi {
			break
		}
	}
	return hi
}

// studentTTail is P(T > t) for Student's t on nu degrees of freedom, t >= 0,
// from the closed forms for integer nu (Abramowitz and Stegun 26.7.3 and
// 26.7.4): with theta = atan(t / sqrt(nu)), P(|T| < t) is a finite series in
// cos(theta).
func studentTTail(t float64, nu int) float64 {
	theta := math.Atan(t / math.Sqrt(float64(nu)))
	s, c := math.Sin(theta), math.Cos(theta)
	c2 := c * c
	var inside float64
	if nu%2 == 1 {
		sum, term := 0.0, 1.0
		if nu > 1 {
			sum = 1
			for k := 3; k <= nu-2; k += 2 {
				term *= float64(k-1) / float64(k) * c2
				sum += term
			}
		}
		inside = 2 / math.Pi * (theta + s*c*sum)
	} else {
		sum, term := 1.0, 1.0
		for k := 2; k <= nu-2; k += 2 {
			term *= float64(k-1) / float64(k) * c2
			sum += term
		}
		inside = s * sum
	}
	return (1 - inside) / 2
}
