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
	// fit's, the floor's below the band, or itl.DefaultBaselineSec.
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
	// Floor, when set, lifts each resample's line to it as the line itself
	// was lifted (LiftToFloor), so the band is around the line the planner
	// prices. The zero value lifts nothing.
	Floor LineFloor
	// BelowBandITL is the floor read below the band, 0 with none: every
	// reading there is A k + B, so it bounds a drawn intercept from above,
	// never under Baseline (interceptDraw).
	BelowBandITL float64
	// HoldB pins every resample's pinned fit at Baseline, as before issue
	// llm-d-incubation/llm-d-scaling-manager#307: set for an intercept the
	// operator stated, which is an assumption and not a reading.
	HoldB bool
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
	// Blocks is the bootstrap's effective sample size: the distinct blocks
	// of Block cycles the windows hold, each stratum's cycles over Block
	// rounded up. A window of 20 readings from 4 replicas is 5 cycles, 2
	// blocks: the resamples are mostly the same few blocks again, and the
	// band is widened for it (bandWidening).
	Blocks int
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
// A resample the tracker fits pinned is fitted through its own intercept,
// not Baseline (interceptDraw; issue llm-d-incubation/llm-d-scaling-manager#307):
// the pinned B is an estimate too, and its error reaches a fill k as
// (B_true - B)(1 - k/kbar), 1.83 times the error in B at k 0.85 from a mean
// fill of 0.3. Held at Baseline, it added nothing to the band. The drawn
// intercepts have median Baseline, so they widen the band about the line
// and do not move it.
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
	newRNG := func() *rand.Rand { return rand.New(rand.NewPCG(r.Seed, r.Seed^0x9e3779b97f4a7c15)) }
	ic := newInterceptDraw(all, fit)
	if ic != nil {
		// A first pass over the same draws finds the centre of the
		// resamples' intercepts, so the drawn B has median Baseline.
		ic.centre(rs, newRNG(), r.Resamples)
	}
	rng := newRNG()
	out := LineResamples{Drawn: r.Resamples, Lines: make([]Resample, 0, r.Resamples), Blocks: rs.blocks()}
	for range r.Resamples {
		// The buffers are reused: refit keeps only the line and its tier,
		// never the readings itl.Refit read (Refitted.Read).
		sAll, sSteady := rs.draw(rng)
		if s, ok := refit(sAll, sSteady, fit, ic); ok {
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

// blocks is the distinct blocks the strata hold (LineResamples.Blocks).
func (rs *resampler) blocks() int {
	per := func(n int) int {
		if n == 0 {
			return 0
		}
		b := min(rs.block, n)
		return (n + b - 1) / b
	}
	return per(len(rs.steadyCycles)) + per(len(rs.otherCycles))
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
// fits no line. A pinned fit is fitted again through the resample's own
// intercept where ic draws one.
func refit(all, steady []itl.Observation, fit Refit, ic *interceptDraw) (Resample, bool) {
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
		} else if tier == TierPinnedB && ic != nil {
			// The tier is chosen before B is drawn, so drawing it changes
			// no tier: a B no slope fits through keeps Baseline.
			if b, ok := ic.at(all); ok {
				if again := itl.Refit(all, steady, b, fit.MinSettled, fit.OneSided); again.Tier == TierPinnedB && !again.Hold {
					got = again
				}
			}
		}
	}
	if tier == "" {
		return Resample{}, false
	}
	got.Model, _ = LiftToFloor(tier, got.Model, fit.Floor)
	return Resample{Line: got.Model, Tier: tier, TierChanged: tier != fit.Tier}, true
}

// minDrawnB is the least intercept a resample is fitted through: the least
// B --assume-itl-b takes. B is a time per token above zero.
const minDrawnB = 1e-6

// interceptDraw draws a pinned fit's intercept per resample (issue
// llm-d-incubation/llm-d-scaling-manager#307): B_r = Baseline + (Bhat_r -
// centre), where Bhat_r is the full least-squares intercept of a resample's
// main-window readings and centre the median of the Bhat_r over the
// resamples. The block bootstrap of the intercept's estimator, its spread
// what the readings can say about B, autocorrelation carried by the blocks
// as it is for A, centred so that the drawn B has median Baseline.
//
// A drawn B is held to [lo, ceiling]: lo the least of minDrawnB and
// Baseline, ceiling the floor read below the band (a reading at a fill
// above 0 is A k + B, so at least B) and never under Baseline. Bounds that
// bracket Baseline keep it the median however hard they bite, so the band
// they shape widens on one side and never moves off its point. The least
// single reading is no bound: as the least of many noisy readings it sits
// under the line by their noise, under Baseline, and held there every draw
// would fall below the line it bands.
type interceptDraw struct {
	baseline, lo, ceiling float64
	centreB               float64
}

// newInterceptDraw is the draw for a line's main-window readings, nil where
// B is not drawn: an above-band line, one priced one-sided at a prior (its B
// is the prior by design), a stated B (HoldB), a Baseline that is no time,
// or readings with no spread in k to read an intercept from. Its centre is
// set by centre before at is used.
func newInterceptDraw(all []itl.Observation, fit Refit) *interceptDraw {
	if fit.Tier == TierAboveBand || fit.OneSided.OneSidedAboveKSat || fit.HoldB {
		return nil
	}
	if !(fit.Baseline > 0) || math.IsInf(fit.Baseline, 0) {
		return nil
	}
	b, ok := olsIntercept(all)
	if !ok {
		return nil
	}
	ceiling := math.Inf(1)
	if fit.BelowBandITL > 0 {
		ceiling = max(fit.BelowBandITL, fit.Baseline)
	}
	return &interceptDraw{baseline: fit.Baseline, lo: min(minDrawnB, fit.Baseline), ceiling: ceiling, centreB: b}
}

// centre sets the draw's centre to the median intercept of n resamples
// drawn from rng, the same draws the bootstrap then fits. With none read,
// it stays the main window's own.
func (ic *interceptDraw) centre(rs *resampler, rng *rand.Rand, n int) {
	bs := make([]float64, 0, n)
	for range n {
		all, _ := rs.draw(rng)
		if b, ok := olsIntercept(all); ok {
			bs = append(bs, b)
		}
	}
	if len(bs) > 0 {
		ic.centreB = median(bs)
	}
}

// at is the intercept a resample's pinned fit runs through, ok false where
// the resample's readings read no intercept (no spread in k): it keeps
// Baseline.
func (ic *interceptDraw) at(all []itl.Observation) (float64, bool) {
	b, ok := olsIntercept(all)
	if !ok {
		return 0, false
	}
	return min(max(ic.baseline+(b-ic.centreB), ic.lo), ic.ceiling), true
}

// usableReading is a reading a fit can take: finite positive k and ITL.
func usableReading(o itl.Observation) bool {
	return o.K > 0 && o.ITLSec > 0 && !math.IsInf(o.K, 0) && !math.IsInf(o.ITLSec, 0)
}

// olsIntercept is the least-squares intercept of ITL on k over the usable
// readings, unconstrained (it may be at or under zero), ok false with fewer
// than two or no spread in k.
func olsIntercept(obs []itl.Observation) (float64, bool) {
	var n, sk, sy float64
	for _, o := range obs {
		if usableReading(o) {
			n++
			sk += o.K
			sy += o.ITLSec
		}
	}
	if n < 2 {
		return 0, false
	}
	kbar, ybar := sk/n, sy/n
	var sxx, sxy float64
	for _, o := range obs {
		if usableReading(o) {
			sxx += (o.K - kbar) * (o.K - kbar)
			sxy += (o.K - kbar) * (o.ITLSec - ybar)
		}
	}
	if !(sxx > 1e-12*n) {
		return 0, false
	}
	return ybar - sxy/sxx*kbar, true
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
// symmetric about a.Value, stretched to hold a.Value where it falls outside,
// then widened about a.Value for few blocks (bandWidening). With no figure
// to band, a comes back unchanged.
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
	lo, hi := at(bandLo), at(bandHi)
	c := min(max(a.Value, lo), hi)
	if Finite(a.Value) {
		// The band holds its point: a band beside the figure it bands
		// would say the figure is not what it is.
		lo, hi, c = min(lo, a.Value), max(hi, a.Value), a.Value
	}
	if f := bandWidening(rs.Blocks); f > 1 {
		// Each edge moved away from the answer by f.
		lo, hi = c-f*(c-lo), c+f*(hi-c)
		if vals[0] >= 0 {
			lo = max(lo, 0)
		}
	}
	a.Band = &Interval{Lo: lo, Hi: hi, Blocks: rs.Blocks}
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

// FewBlocks is the effective sample size under which a band is named with
// its blocks: about five independent blocks are what a percentile of a
// block bootstrap needs before its edges mean what they say.
const FewBlocks = 5

// bandZ is the standard normal's 90th percentile, the band's upper edge.
const bandZ = 1.2815515655446004

// bandT is Student's t at 0.90 for 1 to 30 degrees of freedom.
var bandT = [...]float64{3.078, 1.886, 1.638, 1.533, 1.476, 1.440, 1.415, 1.397, 1.383, 1.372,
	1.363, 1.356, 1.350, 1.345, 1.341, 1.337, 1.333, 1.330, 1.328, 1.325,
	1.323, 1.321, 1.319, 1.318, 1.316, 1.315, 1.314, 1.313, 1.311, 1.310}

// bandWidening is the factor a band of g distinct blocks is widened by: a
// bootstrap of g independent units reads their spread low by sqrt((g-1)/g),
// and its percentiles are normal ones where the spread of g units is
// Student's with g-1 degrees of freedom. 3.40 at 2 blocks, 1.80 at 3, 1.48
// at 4, 1.34 at 5, 1.14 at 10, toward 1. Under 2 there is nothing to scale:
// 1.
func bandWidening(g int) float64 {
	if g < 2 {
		return 1
	}
	df := g - 1
	t := bandZ * (1 + (1+bandZ*bandZ)/(4*float64(df)))
	if df <= len(bandT) {
		t = bandT[df-1]
	}
	return math.Sqrt(float64(g)/float64(df)) * t / bandZ
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
