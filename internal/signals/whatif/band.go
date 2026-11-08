package whatif

import (
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// Resampling is a block bootstrap's settings (docs/proposals/slo-capacity-planner.md,
// "Uncertainty"): resamples made of Block consecutive cycles at a time, so
// that the overlap of one-minute rates read every 15 s stays inside a block,
// Resamples of them, drawn from Seed so that a band can be reproduced.
type Resampling struct {
	Block     int
	Resamples int
	Seed      uint64
}

// DefaultResampling is the page's: blocks of 4 cycles, 200 resamples, seed 1.
func DefaultResampling() Resampling {
	return Resampling{Block: 4, Resamples: 200, Seed: 1}
}

// The band's edges, as percentiles of the resampled figures.
const (
	bandLo = 0.10
	bandHi = 0.90
)

// tierChangeShare is the share of resamples changing tier above which a
// band is flagged at all: TierUnstable or LineNearGate.
const tierChangeShare = 0.10

// tierMoveShare is how far, relative to the answer, the median figure of
// the resamples that changed tier must lie for the change to move the
// answer (TierUnstable) rather than only sit near the gate (LineNearGate).
const tierMoveShare = 0.05

// Refit is how a resample's line is fitted: by the itl tracker's own choice
// of fit (itl.Refit), which takes the full fit only on steady readings that
// are ready (enough of them, spread enough in k) and otherwise the pinned
// fit, or holds the key's slope; or, for an above-band line, by the
// tracker's above-band fallback (itl.RefitAboveBand).
type Refit struct {
	// Tier is the line's own tier: TierOLS, TierPinnedB or TierAboveBand.
	// Any other has no fit to rerun.
	Tier string
	// Line is the line: a resample on which the tracker would hold the
	// key's slope holds its A.
	Line itl.Model
	// Baseline is the intercept the pinned fit pins: the key's last full
	// fit's, or itl.DefaultBaselineSec.
	Baseline float64
	// MinSettled is the steady gate's MinSettledSamples, 0 for the default.
	MinSettled int
	// OneSided, with OneSidedAboveKSat set, prices a resample's pinned fit
	// one-sided as the tracker priced the line (issue ev-shindin/llm-scaling-manager#467): set for a line
	// the tracker priced with no learned B under that admission, Baseline
	// then itl.DefaultBaselineSec. Such a line is through Baseline already
	// (itl.Reading.PinnedAtBPrior), so a resample on which the tracker
	// would hold the slope holds its A, as for any other. The zero value is
	// the pinned fit alone, as before.
	OneSided itl.Admission
}

// Resample is one bootstrap line: the line fitted from one resample, the
// tier the tracker's choice fitted it at, and whether that differs from the
// line's own tier.
type Resample struct {
	Line        itl.Model
	Tier        string
	TierChanged bool
}

// LineResamples is a block bootstrap of a line's readings.
type LineResamples struct {
	// Lines is each resample that fitted a line.
	Lines []Resample
	// Drawn is how many resamples were drawn, those that fitted no line
	// included: the denominator of the tier-change share.
	Drawn int
}

// TierChanges is how many of the drawn resamples landed at another tier
// than the line's, a resample that fitted no line at all included.
func (rs LineResamples) TierChanges() int {
	n := rs.Drawn - len(rs.Lines)
	for _, s := range rs.Lines {
		if s.TierChanged {
			n++
		}
	}
	return n
}

// BootstrapLines refits the line on r.Resamples block-bootstrap resamples
// of the windows it was fitted from: for an above-band line the above-band
// windows, for any other the main and the steady window.
//
// The readings are grouped into cycles by Timestamp, a cycle keeping its
// main and its steady readings together, so a block is Block whole cycles
// of every replica's readings. The bootstrap is stratified: the cycles that
// hold steady readings are drawn among themselves, in blocks, until the
// resample holds exactly as many steady readings as the steady window does
// (the last block cut at the reading), and the cycles with none among
// themselves, until it holds as many main-window readings again. A steady
// count that moved between resamples would test windows the tracker never
// had: whether the full fit is taken turns on that count (DefaultMinSamples)
// as much as on the spread, and a resample drawn freely over the main
// window's cycles held anywhere from 8 to 20 steady readings of a window
// that held 15. Kept
// fixed, the resamples move only what the readings can: their fills and
// their latencies.
//
// The drawn cycles are put back in the order they were read and stamped
// again one second apart, so the tracker's choice of fit, which reads the
// windows in time order (pinnedReadings), reads them in that order and two
// draws of one cycle stay two cycles.
//
// Each resample is fitted as the tracker would fit it (Refit), and the tier
// it lands at is compared with the line's in both directions: ols to
// pinned-B and pinned-B to ols are both changes, and so is a resample on
// which the tracker would hold the slope (priced at the held A) or fit
// nothing at all.
//
// With fewer than two cycles, or a line at a tier with no fit to rerun
// (held or borrowed), there is nothing to resample and Drawn is 0.
// Resampling with Block or Resamples below 1 draws nothing too.
func BootstrapLines(readings itl.KeyReadings, fit Refit, r Resampling) LineResamples {
	all, steady := readings.All, readings.Steady
	switch fit.Tier {
	case TierOLS, TierPinnedB:
	case TierAboveBand:
		all, steady = readings.Above, readings.AboveSteady
	default:
		return LineResamples{}
	}
	if r.Block < 1 || r.Resamples < 1 {
		return LineResamples{}
	}
	rs := newResampler(all, steady, r.Block)
	if rs == nil {
		return LineResamples{}
	}
	rng := rand.New(rand.NewPCG(r.Seed, r.Seed^0x9e3779b97f4a7c15))
	out := LineResamples{Drawn: r.Resamples, Lines: make([]Resample, 0, r.Resamples)}
	for range r.Resamples {
		// The buffers are reused: refit keeps only the line and its tier,
		// never the readings itl.Refit read (Refitted.Read).
		sAll, sSteady := rs.draw(rng)
		if s, ok := refit(sAll, sSteady, fit); ok {
			out.Lines = append(out.Lines, s)
		}
	}
	return out
}

// resampler draws stratified block resamples of one key's windows.
type resampler struct {
	steadyCycles, otherCycles []cycle
	// wantSteady is the steady readings a resample holds, and wantOther the
	// main-window readings of cycles with no steady one.
	wantSteady, wantOther int
	block                 int
	t0                    time.Time
	// picked, all and steady are the buffers a draw reuses.
	picked      []cycle
	all, steady []itl.Observation
}

// newResampler is a resampler of the two windows, nil with fewer than two
// cycles to draw.
func newResampler(all, steady []itl.Observation, block int) *resampler {
	cycles := cyclesOf(all, steady)
	if len(cycles) < 2 {
		return nil
	}
	rs := &resampler{block: block, t0: cycles[0].at}
	for _, c := range cycles {
		if len(c.steady) > 0 {
			rs.steadyCycles = append(rs.steadyCycles, c)
			rs.wantSteady += len(c.steady)
		} else {
			rs.otherCycles = append(rs.otherCycles, c)
			rs.wantOther += len(c.all)
		}
	}
	rs.picked = make([]cycle, 0, len(cycles)+block)
	rs.all = make([]itl.Observation, 0, len(all)+block*4)
	rs.steady = make([]itl.Observation, 0, len(steady)+block*4)
	return rs
}

// draw is one resample: its main and its steady window, in the buffers the
// next draw overwrites.
func (rs *resampler) draw(rng *rand.Rand) (all, steady []itl.Observation) {
	rs.picked = rs.picked[:0]
	rs.picked = drawStratum(rs.picked, rs.steadyCycles, rs.wantSteady, rs.block, rng,
		func(c cycle) int { return len(c.steady) }, func(c cycle, n int) cycle {
			c.steady = c.steady[:n]
			c.all = c.all[:min(n, len(c.all))]
			return c
		})
	rs.picked = drawStratum(rs.picked, rs.otherCycles, rs.wantOther, rs.block, rng,
		func(c cycle) int { return len(c.all) }, func(c cycle, n int) cycle {
			c.all = c.all[:n]
			return c
		})
	slices.SortStableFunc(rs.picked, func(a, b cycle) int { return a.at.Compare(b.at) })
	rs.all, rs.steady = rs.all[:0], rs.steady[:0]
	for j, c := range rs.picked {
		at := rs.t0.Add(time.Duration(j) * time.Second)
		for _, o := range c.all {
			o.Timestamp = at
			rs.all = append(rs.all, o)
		}
		for _, o := range c.steady {
			o.Timestamp = at
			rs.steady = append(rs.steady, o)
		}
	}
	return rs.all, rs.steady
}

// drawStratum appends to picked blocks of the stratum's cycles, drawn at
// random starts, until they hold want readings by count; the cycle that
// passes want is cut to it by cut.
func drawStratum(picked, stratum []cycle, want, block int, rng *rand.Rand,
	count func(cycle) int, cut func(cycle, int) cycle) []cycle {
	n := len(stratum)
	if n == 0 || want == 0 {
		return picked
	}
	b := min(block, n)
	got := 0
	for got < want {
		start := rng.IntN(n - b + 1)
		for _, c := range stratum[start : start+b] {
			k := count(c)
			if got+k > want {
				c = cut(c, want-got)
				k = want - got
			}
			picked = append(picked, c)
			got += k
			if got >= want {
				break
			}
		}
	}
	return picked
}

// refit fits one resample by the tracker's choice of fit, ok false when it
// fits no line.
func refit(all, steady []itl.Observation, fit Refit) (Resample, bool) {
	var got itl.Refitted
	tier := ""
	if fit.Tier == TierAboveBand {
		got = itl.RefitAboveBand(all, steady, fit.MinSettled)
		if got.Tier != "" {
			tier = TierAboveBand
		}
	} else {
		got = itl.Refit(all, steady, fit.Baseline, fit.MinSettled, fit.OneSided)
		tier = got.Tier
		if got.Hold {
			got.Model, tier = itl.Model{A: fit.Line.A, B: fit.Baseline}, TierHeld
		}
	}
	if tier == "" {
		return Resample{}, false
	}
	return Resample{Line: got.Model, Tier: tier, TierChanged: tier != fit.Tier}, true
}

// cycle is one cycle's readings: those of the main window and those of the
// steady one, read at the same instant.
type cycle struct {
	at          time.Time
	all, steady []itl.Observation
}

// cyclesOf groups two windows' readings into cycles, by Timestamp, in time
// order.
func cyclesOf(all, steady []itl.Observation) []cycle {
	var out []cycle
	at := map[time.Time]int{}
	add := func(o itl.Observation, isSteady bool) {
		i, ok := at[o.Timestamp]
		if !ok {
			i = len(out)
			at[o.Timestamp] = i
			out = append(out, cycle{at: o.Timestamp})
		}
		if isSteady {
			out[i].steady = append(out[i].steady, o)
		} else {
			out[i].all = append(out[i].all, o)
		}
	}
	for _, o := range all {
		add(o, false)
	}
	for _, o := range steady {
		add(o, true)
	}
	slices.SortStableFunc(out, func(a, b cycle) int { return a.at.Compare(b.at) })
	return out
}

// BandAnswer bands a, an answer priced on one line, from the resamples:
// value reruns the question on a resampled line and returns the figure it
// prices, ok false when that line refuses it. The band is the 10th to the
// 90th percentile of the figures (nearest rank), so it need not be
// symmetric about a.Value. With no figure to band, a comes back unchanged.
//
// More than a tenth of the drawn resamples changing tier flags the answer,
// one of two ways. TierUnstable when the change moves this figure: the
// median figure of the resamples that changed tier lies more than 5% from
// a.Value. LineNearGate otherwise: the line sits near the tracker's gate, so
// resamples cross it, but the figure does not care which side it is fitted
// on. Each answered figure is judged on its own.
func BandAnswer(a Answer, rs LineResamples, value func(itl.Model) (float64, bool)) Answer {
	var vals, changed []float64
	for _, s := range rs.Lines {
		v, ok := value(s.Line)
		if !ok || !Finite(v) {
			continue
		}
		vals = append(vals, v)
		if s.TierChanged {
			changed = append(changed, v)
		}
	}
	if len(vals) == 0 {
		return a
	}
	slices.Sort(vals)
	at := func(p float64) float64 { return vals[max(0, int(math.Ceil(p*float64(len(vals))))-1)] }
	a.Band = &Interval{Lo: at(bandLo), Hi: at(bandHi)}
	if !(float64(rs.TierChanges()) > tierChangeShare*float64(rs.Drawn)) {
		return a
	}
	flag := LineNearGate
	if len(changed) > 0 && math.Abs(median(changed)-a.Value) > tierMoveShare*math.Abs(a.Value) {
		flag = TierUnstable
	}
	if !a.HasFlag(flag) {
		a.Flags = append(slices.Clone(a.Flags), flag)
	}
	return a
}

// median is the median of xs, which it sorts.
func median(xs []float64) float64 {
	slices.Sort(xs)
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}
