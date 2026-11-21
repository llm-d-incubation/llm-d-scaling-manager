package whatif

import (
	"math"
)

// The censored fit's search range for the lognormal's sigma: a TPOT
// spread from almost none to a tail far past any engine's.
const (
	censoredSigmaLo = 0.01
	censoredSigmaHi = 2.5
)

// A window spreads over two buckets, and so helps identify sigma, only
// where its second-fullest bucket holds real mass: at least
// censoredSpreadCount requests and censoredSpreadShare of the window. A
// sliver spilled past an edge (one request in a hundred) otherwise sets the
// spread for every window pooled with it.
const (
	censoredSpreadCount = 3.0
	censoredSpreadShare = 0.02
)

// TPOTRatioCensored learns R_q as TPOTRatioFromWindows does, but reads it
// by interval-censored maximum likelihood rather than at the q bucket's
// upper edge (#279). Each window is a lognormal with its own mean
// (_sum/_count) and one log-spread sigma shared by every window; each bucket
// count is the mass between two edges, and sigma is the one that best
// explains every window's counts at once. R_q is that lognormal's
// q-quantile over its mean, exp(sigma*z_q - sigma^2/2), with no
// order-statistic margin, never above the edge reading. One window does
// not identify sigma where every request sits in one bucket (the
// likelihood rises as sigma falls); pooled, the windows that spill into the
// next bucket set it. With no window over two buckets (censoredSpreads), or
// a fit at the floor of the sigma range, the edge reading stands. The flags
// and the refusals are TPOTRatioFromWindows's. ByFill is learned the same
// way, each bucket on its own windows, and never read above that bucket's
// own edge reading: a bucket whose windows do not identify sigma takes the
// global R_q capped there, and a thin bucket the global R_q, as the edge
// read gives it. Learned is the bucket's reading before the raise over fill.
func TPOTRatioCensored(windows []TPOTWindow, q float64) (TPOTRatio, []Flag, *Unanswerable) {
	r, flags, refused, _ := TPOTRatioCensoredFit(windows, q)
	return r, flags, refused
}

// TPOTRatioCensoredFit is TPOTRatioCensored, and whether the fit read R_q:
// false where the edge reading stands (no window spreads over two buckets,
// a fit at the sigma floor, or a refusal).
func TPOTRatioCensoredFit(windows []TPOTWindow, q float64) (TPOTRatio, []Flag, *Unanswerable, bool) {
	edge, flags, refused := TPOTRatioFromWindows(windows, q)
	if refused != nil {
		return edge, flags, refused, false
	}
	global, ok := censoredPooled(windows, q)
	if !ok {
		return edge, flags, nil, false
	}
	r := TPOTRatio{Value: max(1, min(global, edge.Value))}
	if len(edge.ByFill) > 0 {
		r.ByFill = make([]FillRatio, len(edge.ByFill))
		for i, b := range edge.ByFill {
			r.ByFill[i] = b
			var in []TPOTWindow
			for _, w := range windows {
				if w.FillKnown && Finite(w.Fill) && min(fillRatioBuckets-1, max(0, int(math.Floor(w.Fill/FillRatioBucketWidth)))) == i {
					in = append(in, w)
				}
			}
			switch {
			case b.Thin:
				r.ByFill[i].Value = r.Value
			default:
				v, ok := censoredPooled(in, q)
				if !ok {
					v = r.Value
				}
				r.ByFill[i].Value = max(1, min(v, b.Learned))
				r.ByFill[i].Learned = r.ByFill[i].Value
			}
		}
		// Raised, never falling, as the fill rises: as fillRatios keeps it.
		for i := 1; i < len(r.ByFill); i++ {
			r.ByFill[i].Value = max(r.ByFill[i].Value, r.ByFill[i-1].Value)
		}
	}
	return r, flags, nil, true
}

// censoredCell is a bucket's count between two edges.
type censoredCell struct{ lo, hi, n float64 }

// censoredWindow is a window's mean and its counts by bucket; ok false for
// a window with no observation, no positive mean, or buckets that do not
// read.
type censoredWindow struct {
	mean  float64
	cells []censoredCell
}

// censoredCells reads a window for the fit. Requests past the last edge a
// window carries (one with no +Inf bucket) are a cell from that edge up:
// the mean counts them, and dropping them would read the spread short.
func censoredCells(w TPOTWindow, q float64) (censoredWindow, bool) {
	if !(w.Count > 0) || !(w.Sum > 0) || len(w.Buckets) == 0 {
		return censoredWindow{}, false
	}
	if _, empty, err := readWindow(w, q); err != nil || empty {
		return censoredWindow{}, false
	}
	cw := censoredWindow{mean: w.Sum / w.Count}
	lo, prev, running := 0.0, 0.0, 0.0
	for _, b := range w.Buckets {
		running = max(running, b.Count)
		if n := running - prev; n > 0 {
			cw.cells = append(cw.cells, censoredCell{lo: lo, hi: b.UpperEdge, n: n})
		}
		lo, prev = b.UpperEdge, running
	}
	if n := w.Count - running; n > 0 && !math.IsInf(lo, 1) {
		cw.cells = append(cw.cells, censoredCell{lo: lo, hi: math.Inf(1), n: n})
	}
	return cw, len(cw.cells) > 0
}

// spreads says a window holds real mass in two buckets: its second-fullest
// holds at least censoredSpreadCount requests and censoredSpreadShare of
// them all.
func (w censoredWindow) spreads() bool {
	var total, first, second float64
	for _, c := range w.cells {
		total += c.n
		switch {
		case c.n > first:
			first, second = c.n, first
		case c.n > second:
			second = c.n
		}
	}
	return second >= censoredSpreadCount && second >= censoredSpreadShare*total
}

// censoredPooled is the q-quantile over the mean of the lognormal whose
// shared sigma best explains every window's counts, each at its own mean;
// ok false where no window spreads over two buckets (spreads), which alone
// identify sigma, or where the fit lands on the floor of the sigma range,
// a spread the counts do not set.
func censoredPooled(windows []TPOTWindow, q float64) (float64, bool) {
	var ws []censoredWindow
	spread := false
	for _, w := range windows {
		if cw, ok := censoredCells(w, q); ok {
			ws = append(ws, cw)
			spread = spread || cw.spreads()
		}
	}
	if !spread {
		return 0, false
	}
	ll := func(sigma float64) float64 {
		s := 0.0
		for _, w := range ws {
			mu := math.Log(w.mean) - sigma*sigma/2
			cdf := func(x float64) float64 {
				switch {
				case x <= 0:
					return 0
				case math.IsInf(x, 1):
					return 1
				}
				return 0.5 * math.Erfc(-(math.Log(x)-mu)/(sigma*math.Sqrt2))
			}
			for _, c := range w.cells {
				s += c.n * math.Log(max(cdf(c.hi)-cdf(c.lo), 1e-300))
			}
		}
		return s
	}
	// A grid in log sigma, then a golden-section search around its best.
	const steps = 64
	best, bestLL := censoredSigmaLo, math.Inf(-1)
	ratio := math.Pow(censoredSigmaHi/censoredSigmaLo, 1.0/steps)
	for i, s := 0, censoredSigmaLo; i <= steps; i, s = i+1, s*ratio {
		if v := ll(s); v > bestLL {
			best, bestLL = s, v
		}
	}
	a, b := max(best/ratio, censoredSigmaLo), min(best*ratio, censoredSigmaHi)
	g := (math.Sqrt(5) - 1) / 2
	for range 60 {
		c, d := b-g*(b-a), a+g*(b-a)
		if ll(c) > ll(d) {
			b = d
		} else {
			a = c
		}
	}
	sigma := (a + b) / 2
	if sigma <= censoredSigmaLo*ratio {
		return 0, false
	}
	z := math.Sqrt2 * math.Erfinv(2*q-1)
	r := math.Exp(sigma*z - sigma*sigma/2)
	if !Finite(r) || !(r > 0) {
		return 0, false
	}
	return r, true
}
