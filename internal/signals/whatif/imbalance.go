package whatif

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The imbalance f across pod counts (llm-d-incubation/llm-d-scaling-manager#169,
// item 5). P1 learns f only at a count the fleet has run at
// (prefill.Estimate.FByPods); the planner asks about the counts it has not.
// With the switch on (DefaultImbalanceAcrossCounts for the SLO analyzer,
// DefaultPlannerImbalanceAcrossCounts for the capacity planner), both read
// f at any count through ImbalanceReadings.At: a count P1 learned keeps its
// own f, any other is read off a curve fitted to the counts learned and the
// raw share, and with no count learned f is at least FreshImbalance (the
// analyzer; the planner keeps ImbalanceFloor with none learned).
//
// The curve. Each pod's share of the prefill work is 1 + c·z, z a standard
// normal draw per pod, from the router's scoring, a slower node, or a prefix
// the router keeps sending to one pod: c is the population spread of the
// shares, with no sqrt(n/(n-1)) sample correction. Shares are read against
// their own mean, and since that mean moves every z alike, E[max_i (z_i -
// mean z)] = E[max_i z_i]: the busiest of n pods reads, on average,
// f(n) = 1 + c·g(n), g = prefill.NormalMaxMean (0.564 at two pods, 0.846 at
// three, 1.029 at four, 1.424 at eight). P1's f is de-biased for chance
// (Poisson counts), so c is what stays once chance is taken off.
//
// The fit. c is the least-squares fit through f = 1, each count weighted by
// the minutes P1 kept at it (prefill.Estimate.FSamplesByPods): one count
// gives c = (f - 1)/g(n) exactly. The raw share at the count it was read at,
// when P1 has not learned that count, is one more point, of
// prefill.MinImbalanceMinutes minutes. It is not de-biased, so it errs high,
// and it only ever raises c: c is the larger of the fits with and without it.
// A hot pod read now is carried to the counts around it.
//
// Not carried across a restart: P1's minutes live in memory, for
// prefill.Retention. Right after a restart, with nothing learned, f is
// FreshImbalance or the raw share's curve, whichever is higher. Persisting
// FByPods in the learned state blocks flipping the default.
//
// Rows, not engines. P1 reads one share per row. A data-parallel pod, or an
// LWS group whose every Pod runs an engine, is one row of k engines, and the
// spread inside it is not seen: the row's spread is about c/sqrt(k), so f
// reads low, about 1.05 for a true 1.20 at k = 4. Any f read from such rows
// stays flagged AssumedImbalance (prefill.Estimate.FMultiRank). A per-engine
// fit, g(N·k), is a follow-up.
//
// Measured on Qwen3-32B TP2 prefill pods on H200 (8000-token prompts): P1
// learned f = 1.069 over 29 minutes at two pods (2+2), c = 0.123, so the
// curve reads 1.104 at three. That is a de-biased f. What three pods (3+1)
// measured is raw: the busiest pod's share of prefill busy time over each
// step read 1.12 to 1.14 at the five steps below saturation, and of prompt
// tokens 1.08 to 1.10. A step holds about 500 requests a pod, whose chance
// excess at three pods is about 0.04, so de-biased those read about 1.08 to
// 1.10 and 1.04 to 1.06: the curve stands at or above them.

// DefaultImbalanceAcrossCounts is the SLO analyzer's default of reading f
// across pod counts (ImbalanceReadings.At,
// slo.SwitchTTFTImbalanceAcrossCounts): off, so an unlearned count is
// priced at min(N, max(ImbalanceFloor, f)) as before. Opt-in first: one GPU run at three pods backs the curve and
// FreshImbalance. The default flips after a check at a count none of the
// learned ones covers, and once FByPods survives a restart.
const DefaultImbalanceAcrossCounts = false

// DefaultPlannerImbalanceAcrossCounts is the capacity planner's default of
// reading f across pod counts (--imbalance-across-counts, and its server):
// on, where the SLO analyzer's (DefaultImbalanceAcrossCounts) is off. The
// planner prices counts the recording never ran at, which is where the floor
// misprices (issue llm-d-incubation/llm-d-scaling-manager#300): with f 1.00
// learned at two pods, three at max(ImbalanceFloor, f) = 1.5 have the same
// busiest share as two, so the third pod buys no prefill knee and the count
// search steps from two to four. The curve reads 1.00 at three. Neither
// reason the analyzer's default waits on holds for a replay: it reads its
// recording whole, so no restart loses FByPods, and the report prints the
// answer at f 1 and at ImbalanceFloor beside it. With no count learned the
// planner keeps the floor: FreshImbalance is the analyzer's figure for the
// minutes after a restart, not a reading of the fleet.
const DefaultPlannerImbalanceAcrossCounts = true

// FreshImbalance is the least f at a count above one with no count learned,
// with DefaultImbalanceAcrossCounts on. Not a constant of any fleet: it stands
// above what every Qwen3-32B run measured, 1.00 and 1.07 learned at two pods
// and a busiest share of 1.08 to 1.14 at three, and above three of the four
// staircase phases of a smaller model at two pods (1.00 to 1.03). The fourth
// learned 1.36 there: a fleet like that is priced short until P1 learns its
// own f or a raw share reads it, and the answer is flagged AssumedImbalance
// either way.
const FreshImbalance = 1.15

// Where an f read across counts came from.
const (
	// ImbalanceLearned is P1's own f at the count.
	ImbalanceLearned = "learned"
	// ImbalanceDefault is FreshImbalance: no count learned.
	ImbalanceDefault = "default"
	// ImbalanceRawShare is the raw share at the count it was read at, priced
	// as it reads.
	ImbalanceRawShare = "raw share"
	// ImbalanceOnePod is one pod, or none: no imbalance.
	ImbalanceOnePod = "one pod"
	// imbalanceMultiRank is added to a source read from rows of several
	// engine ranks.
	imbalanceMultiRank = "per row of several engines"
)

// ImbalanceReadings is what f at any pod count is read from.
type ImbalanceReadings struct {
	// ByPods is P1's f at each count it learned, and Minutes how many
	// minutes it kept at each (prefill.Estimate.FByPods, FSamplesByPods).
	ByPods  map[int]float64
	Minutes map[int]int
	// Pods is the count the raw share was read at, and RawShare that share,
	// the window's largest; 0 with none.
	Pods     int
	RawShare float64
	// MultiRank is prefill.Estimate.FMultiRank.
	MultiRank bool
}

// ImbalanceReadingsOf is the readings of P1's estimate and a raw share read
// at pods.
func ImbalanceReadingsOf(est prefill.Estimate, pods int, rawShare float64) ImbalanceReadings {
	return ImbalanceReadings{ByPods: est.FByPods, Minutes: est.FSamplesByPods, Pods: pods, RawShare: rawShare,
		MultiRank: est.FMultiRank}
}

// Imbalance is f at one pod count, read across counts.
type Imbalance struct {
	// F is f at the count: 0 on one pod; else at least 1 and at most the
	// count, but a learned f is as P1 gave it.
	F float64
	// Assumed is any f not learned at the count (fitted, the default, or the
	// raw share), and any f read from rows of several engines. The caller
	// flags it AssumedImbalance.
	Assumed bool
	// Source says where F came from: ImbalanceLearned, ImbalanceDefault,
	// ImbalanceRawShare, ImbalanceOnePod, or "fitted from counts {2, 4}",
	// with "and the raw share at 3" when that point raised the curve.
	Source string
	// Counts are the learned counts the curve was fitted from, ascending;
	// Spread is its c. Empty and 0 unless fitted.
	Counts []int
	Spread float64
}

// imbalancePoint is one point of the curve: f at a count, and its weight.
type imbalancePoint struct {
	pods   int
	f, wgt float64
}

// At is f at pods. Pods of 1 or fewer give none (ImbalanceOnePod). A count P1
// learned gives its own f. Any other count reads the curve, never below 1 nor
// above pods, and with no count learned never below FreshImbalance; at the
// raw share's own count, never below the raw share.
func (r ImbalanceReadings) At(pods int) Imbalance {
	if pods <= 1 {
		return Imbalance{Source: ImbalanceOnePod}
	}
	if f, ok := r.ByPods[pods]; ok && learnedF(f) {
		return r.multiRank(Imbalance{F: f, Source: ImbalanceLearned})
	}
	learned := r.learnedPoints()
	raw, rawOK := r.rawPoint()
	counts := make([]int, len(learned))
	for i, p := range learned {
		counts[i] = p.pods
	}
	cLearned := fitSpread(learned)
	c, rawRaised := cLearned, false
	if rawOK {
		if withRaw := fitSpread(append(slices.Clone(learned), raw)); withRaw > cLearned {
			c, rawRaised = withRaw, true
		}
	}
	n := float64(pods)
	out := Imbalance{Assumed: true, Counts: counts, Spread: c}
	switch {
	case len(learned) == 0 && !rawRaised:
		out = Imbalance{F: min(n, FreshImbalance), Assumed: true, Source: ImbalanceDefault}
	case len(learned) == 0:
		out = Imbalance{F: min(n, FreshImbalance), Assumed: true, Source: ImbalanceDefault}
		if f := min(n, 1+c*prefill.NormalMaxMean(pods)); f > out.F {
			out = Imbalance{F: f, Assumed: true, Source: fmt.Sprintf("fitted from the raw share at %d", r.Pods), Spread: c}
		}
	default:
		out.F = min(n, max(1, 1+c*prefill.NormalMaxMean(pods)))
		out.Source = fittedSource(counts)
		if rawRaised {
			out.Source += fmt.Sprintf(" and the raw share at %d", r.Pods)
		}
	}
	if rawOK && pods == r.Pods && raw.f >= out.F {
		out = Imbalance{F: raw.f, Assumed: true, Source: ImbalanceRawShare}
	}
	return r.multiRank(out)
}

// multiRank flags i, and names why, when f was read from rows of several
// engine ranks.
func (r ImbalanceReadings) multiRank(i Imbalance) Imbalance {
	if r.MultiRank {
		i.Assumed = true
		i.Source += " (" + imbalanceMultiRank + ")"
	}
	return i
}

// learnedPoints is the counts P1 learned that the curve reads, ascending: a
// count above one with a finite f of at least 1, weighted by its minutes
// (prefill.MinImbalanceMinutes where none are given).
func (r ImbalanceReadings) learnedPoints() []imbalancePoint {
	var counts []int
	for pods, f := range r.ByPods {
		if pods >= 2 && learnedF(f) {
			counts = append(counts, pods)
		}
	}
	// In count order, so the same map gives the same c to the bit.
	slices.Sort(counts)
	points := make([]imbalancePoint, len(counts))
	for i, pods := range counts {
		w := r.Minutes[pods]
		if w <= 0 {
			w = prefill.MinImbalanceMinutes
		}
		points[i] = imbalancePoint{pods: pods, f: r.ByPods[pods], wgt: float64(w)}
	}
	return points
}

// rawPoint is the raw share as a point of the curve, at most its count, of
// prefill.MinImbalanceMinutes minutes: only at a count above one that P1 has
// not learned, and only a finite share above 1.
func (r ImbalanceReadings) rawPoint() (imbalancePoint, bool) {
	if r.Pods < 2 || !(r.RawShare > 1) || math.IsInf(r.RawShare, 0) {
		return imbalancePoint{}, false
	}
	if f, ok := r.ByPods[r.Pods]; ok && learnedF(f) {
		return imbalancePoint{}, false
	}
	return imbalancePoint{pods: r.Pods, f: min(float64(r.Pods), r.RawShare), wgt: prefill.MinImbalanceMinutes}, true
}

// fitSpread is c fitted through f = 1 by weighted least squares: the sum of
// w·(f - 1)·g(n) over the sum of w·g(n)^2; 0 with no point.
func fitSpread(points []imbalancePoint) float64 {
	var num, den float64
	for _, p := range points {
		g := prefill.NormalMaxMean(p.pods)
		num += p.wgt * (p.f - 1) * g
		den += p.wgt * g * g
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// learnedF is an f P1 reports: finite and at least 1.
func learnedF(f float64) bool { return f >= 1 && !math.IsInf(f, 0) }

// LearnedAt is P1's f at pods, and whether it learned one there: finite and
// at least 1.
func (r ImbalanceReadings) LearnedAt(pods int) (float64, bool) {
	f, ok := r.ByPods[pods]
	return f, ok && learnedF(f)
}

// LearnedAny is whether P1 learned f at any count above one: the curve has a
// point of its own.
func (r ImbalanceReadings) LearnedAny() bool { return len(r.learnedPoints()) > 0 }

// fittedSource names the counts a curve was fitted from.
func fittedSource(counts []int) string {
	s := make([]string, len(counts))
	for i, n := range counts {
		s[i] = strconv.Itoa(n)
	}
	return fmt.Sprintf("fitted from counts {%s}", strings.Join(s, ", "))
}
