package whatifapi

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The self-check's steps are not independent: a staircase holds each load
// level for several steps, and both folds' tests come from one recording.
// These rules read the steps level by level (issue
// llm-d-incubation/llm-d-scaling-manager#298).
const (
	// selfCheckLevelTol is how far a step's rate may lie from its level's
	// first rate, as a share of it, and still be the same level: a level is
	// a run of steps, in time, at one fleet and about one rate.
	selfCheckLevelTol = 0.15
	// selfCheckLevelsOK is the fewest independent load levels an ok rests
	// on; fewer is weak, however many steps they hold.
	selfCheckLevelsOK = 4
	// selfCheckBootstrap is the resamples of the levels the within share's
	// bound is read from, and selfCheckBoundQ the quantile read: an ok
	// needs the share's lower bound at selfCheckWithinLo or more.
	selfCheckBootstrap = 2000
	selfCheckBoundQ    = 0.1
	// selfCheckLowShare is the largest share of the scored steps that may
	// read more than BacktestTolerance low, and selfCheckFloor the lowest
	// ratio any one step may read: either past is inconsistent. Reading low
	// is the side that costs an SLO.
	selfCheckLowShare = 0.15
	selfCheckFloor    = 0.67
	// selfCheckNearKneeMin is the fewest scored steps at 0.8 to 1.0 of the
	// knee the worst-decile rule judges, and selfCheckNearKneeHi the
	// highest ratio its worst tenth may read: near the knee a median hides
	// the steps that size the fleet.
	selfCheckNearKneeMin = 3
	selfCheckNearKneeHi  = 2.0
	// selfCheckServedX is how far over the median measured figure of the
	// steps below 0.8 of the knee a step the planner called overloaded may
	// measure and still count as served unqueued: its queue did not grow,
	// so the knee read low there. selfCheckServedShare of the steps called
	// overloaded (and at least selfCheckServedMin) served so is
	// inconsistent.
	selfCheckServedX     = 1.5
	selfCheckServedShare = 0.2
	selfCheckServedMin   = 2
)

// scPoint is one scored step of a figure: when, its rate and fleet, its
// load band, its ratio of predicted to measured, and the measured figure.
type scPoint struct {
	at       time.Time
	rate     float64
	np, nd   int
	band     string
	ratio    float64
	within   bool
	measured float64
}

// selfCheckPoints is the figure's scored steps at statistic among steps
// (TTFT, or TPOT with ttft false), oldest first, and the measured figure of
// each judged step the planner called overloaded. statistic is the one the
// figure is judged at (SelfCheckFigure.Statistic), so that the step-level
// rules read the steps its score was drawn from.
func selfCheckPoints(steps []BacktestStep, ttft bool, statistic string) (pts []scPoint, overloaded []float64) {
	for _, st := range steps {
		if st.Excluded != "" {
			continue
		}
		ps, band := st.TPOT, st.DecodeLoad
		if ttft {
			ps, band = st.TTFT, st.Load
		}
		i := slices.IndexFunc(ps, func(p BacktestPoint) bool { return p.Statistic == statistic })
		if i < 0 {
			continue
		}
		p := ps[i]
		if p.Ratio == nil {
			if r := p.Predicted.Refused; r != nil && r.Reason == string(whatif.Saturated) && p.Measured != nil && whatif.Finite(*p.Measured) {
				overloaded = append(overloaded, *p.Measured)
			}
			continue
		}
		m := math.NaN()
		if p.Measured != nil {
			m = *p.Measured
		}
		pts = append(pts, scPoint{at: st.Start, rate: st.Rate, np: st.Prefill, nd: st.Decode, band: band, ratio: *p.Ratio,
			within: !p.AboveHistogram && math.Abs(*p.Ratio-1) <= BacktestTolerance, measured: m})
	}
	slices.SortStableFunc(pts, func(a, b scPoint) int { return a.at.Compare(b.at) })
	return pts, overloaded
}

// levelsOf cuts pts, oldest first, into load levels: a step starts a new
// level when its fleet differs from the level's, or its rate lies more
// than selfCheckLevelTol from the level's first rate. One step off a level
// that the next steps return to is noise, not a level: it stays in the
// level, so that a noisy step never splits one level into three.
func levelsOf(pts []scPoint) [][]scPoint {
	same := func(l []scPoint, p scPoint) bool {
		first := l[0]
		return p.np == first.np && p.nd == first.nd && math.Abs(p.rate-first.rate) <= selfCheckLevelTol*first.rate
	}
	var out [][]scPoint
	for i := 0; i < len(pts); i++ {
		p := pts[i]
		if n := len(out); n > 0 {
			last := out[n-1]
			switch {
			case same(last, p):
				out[n-1] = append(last, p)
				continue
			case i+1 < len(pts) && same(last, pts[i+1]):
				out[n-1] = append(last, p, pts[i+1])
				i++
				continue
			}
		}
		out = append(out, []scPoint{p})
	}
	return out
}

// withinBound is the cluster bootstrap's selfCheckBoundQ quantile of the
// within share: the levels resampled with replacement, each carrying all
// its steps, from a fixed seed so that one recording always reads the same.
func withinBound(levels [][]scPoint) float64 {
	if len(levels) == 0 {
		return 0
	}
	rng := rand.New(rand.NewPCG(1, 298))
	shares := make([]float64, selfCheckBootstrap)
	for b := range shares {
		within, n := 0, 0
		for range levels {
			for _, p := range levels[rng.IntN(len(levels))] {
				n++
				if p.within {
					within++
				}
			}
		}
		shares[b] = float64(within) / float64(n)
	}
	return nearestRank(shares, selfCheckBoundQ)
}

// SelfCheckNearKnee is the scored steps at 0.8 to 1.0 of the knee, and the
// ratio of the worst tenth of them: the step at the nearest-rank 90th
// percentile of |ln ratio|.
type SelfCheckNearKnee struct {
	Steps int     `json:"steps"`
	Worst float64 `json:"worst"`
}

// nearKneeOf is pts' steps at 0.8 to 1.0 of the knee; nil with none.
func nearKneeOf(pts []scPoint) *SelfCheckNearKnee {
	var near []scPoint
	for _, p := range pts {
		if p.band == LoadNearKnee {
			near = append(near, p)
		}
	}
	if len(near) == 0 {
		return nil
	}
	slices.SortFunc(near, func(a, b scPoint) int {
		return cmpFloat(math.Abs(math.Log(a.ratio)), math.Abs(math.Log(b.ratio)))
	})
	return &SelfCheckNearKnee{Steps: len(near), Worst: near[max(int(math.Ceil(0.9*float64(len(near))))-1, 0)].ratio}
}

// cmpFloat orders a before b.
func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// servedPastKnee is how many steps of overloaded, the measured figures of
// steps the planner called overloaded, measured within selfCheckServedX of
// the median measured figure of pts' steps below 0.8 of the knee: the
// fleet served them unqueued. 0 with no such step below 0.8.
func servedPastKnee(pts []scPoint, overloaded []float64) int {
	var below []float64
	for _, p := range pts {
		if p.band == LoadBelow80 && whatif.Finite(p.measured) {
			below = append(below, p.measured)
		}
	}
	if len(below) == 0 {
		return 0
	}
	base := median(below)
	n := 0
	for _, m := range overloaded {
		if m <= selfCheckServedX*base {
			n++
		}
	}
	return n
}

// scoreSteps fills f's step-level reading of pts and overloaded: the
// levels, the within share's bound, the lowest ratio, the steps near the
// knee, and, for TTFT (served), the steps called overloaded the fleet
// served unqueued. A queue at a saturated decode shows in TTFT, not in
// TPOT, which stays near the full batch's ITL: TPOT does not score the
// knee.
func (f *SelfCheckFigure) scoreSteps(pts []scPoint, overloaded []float64, served bool) {
	levels := levelsOf(pts)
	f.Levels = len(levels)
	if len(pts) == 0 {
		return
	}
	f.WithinBound = ptr(withinBound(levels))
	lo := pts[0].ratio
	for _, p := range pts[1:] {
		lo = min(lo, p.ratio)
	}
	f.MinRatio = ptr(lo)
	f.NearKnee = nearKneeOf(pts)
	if served {
		f.ServedPastKnee = servedPastKnee(pts, overloaded)
	}
}

// SelfCheckCoverage is what the self-check tested: the rates of its judged
// steps and the fleets they ran. An answer at another rate or fleet is not
// covered by it.
type SelfCheckCoverage struct {
	RateLo float64       `json:"rateLo"`
	RateHi float64       `json:"rateHi"`
	Fleets []FleetCounts `json:"fleets"`
}

// coverageOf is the judged steps' rates and fleets; nil with none judged.
func coverageOf(steps []BacktestStep) *SelfCheckCoverage {
	var c *SelfCheckCoverage
	for _, st := range steps {
		if st.Excluded != "" || !(st.Rate > 0) {
			continue
		}
		if c == nil {
			c = &SelfCheckCoverage{RateLo: st.Rate, RateHi: st.Rate, Fleets: []FleetCounts{}}
		}
		c.RateLo, c.RateHi = min(c.RateLo, st.Rate), max(c.RateHi, st.Rate)
		fc := FleetCounts{Prefill: st.Prefill, Decode: st.Decode}
		if !slices.Contains(c.Fleets, fc) {
			c.Fleets = append(c.Fleets, fc)
		}
	}
	if c != nil {
		slices.SortFunc(c.Fleets, func(a, b FleetCounts) int { return cmpFloat(float64(a.Prefill+a.Decode), float64(b.Prefill+b.Decode)) })
	}
	return c
}

// Covers says the self-check tested rate on a fleet of np prefill and nd
// decode replicas: the rate within its steps' rates and the fleet one its
// steps ran.
func (c *SelfCheckCoverage) Covers(rate float64, np, nd int) bool {
	return c != nil && rate >= c.RateLo && rate <= c.RateHi && slices.Contains(c.Fleets, FleetCounts{Prefill: np, Decode: nd})
}

// Words is the coverage in words: "2.10 to 9.84 r/s at 2 prefill + 2
// decode".
func (c *SelfCheckCoverage) Words() string {
	fleets := make([]string, len(c.Fleets))
	for i, f := range c.Fleets {
		fleets[i] = fmt.Sprintf("%d prefill + %d decode", f.Prefill, f.Decode)
	}
	at := ""
	for i, f := range fleets {
		switch {
		case i == 0:
			at = f
		case i == len(fleets)-1:
			at += " or " + f
		default:
			at += ", " + f
		}
	}
	return fmt.Sprintf("%.2f to %.2f r/s at %s", c.RateLo, c.RateHi, at)
}
