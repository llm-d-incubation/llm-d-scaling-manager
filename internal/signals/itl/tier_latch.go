package itl

import "time"

// The set, clear and dwell of the choice between the full fit and the pinned
// one (analyzer-learner.md, 3.5). The set values are DefaultMinSamples and
// DefaultMinKSpread: 10 steady readings spread over 0.30 of k. The safe side
// is the line that prices more replicas, whichever tier it comes from.
const (
	// DefaultClearKSpread is the spread under which a key at the full fit
	// leaves it: 0.12 under DefaultMinKSpread, the width over which
	// autocorrelated noise made the bare set value flip. Between it and the
	// set value the full fit stays only while the pinned fit does not price
	// DefaultTierSafeMargin more (dwellTier), and it teaches no B (Observe).
	DefaultClearKSpread = 0.18
	// DefaultClearMinSamples is the fewest steady readings that keep a key
	// at the full fit: 0.8 of DefaultMinSamples.
	DefaultClearMinSamples = 8
	// DefaultTierDwell is how long after a change of tier the tier stays,
	// but for a line DefaultTierSafeMargin higher. It is the steady window's
	// memory: a reading stays up to DefaultSteadyCarry past the main
	// window's oldest, so the same readings can decide the gate for up to
	// twice that. A load that steps collapses the spread at once, past any
	// clear value, and only the dwell holds there.
	DefaultTierDwell = 2 * DefaultSteadyCarry
	// DefaultTierSafeMargin is how much more a line must price, inside the
	// dwell, to change the tier at once, and how far a line resting between
	// the clear and set values may price under the pinned fit. It is also how
	// far a held line can price under the line it refused. Over the recordings
	// a change from the full fit to the pinned fit, same B, moved the priced
	// ITL by at most 2.8 %: that is the two tiers agreeing.
	DefaultTierSafeMargin = 0.03
)

// tierLatch is the tier a key's line stands at, since when, and when the key
// last held a steady reading.
type tierLatch struct {
	tier  string
	since time.Time
	seen  time.Time
	// fitted is set once the key has stood at the full fit, which a B at or
	// under zero leaves no other mark of (Observe remembers none).
	fitted bool
}

// clearReady reports whether the steady readings keep a key at the full fit.
func clearReady(steady []Observation) bool {
	return ready(steady, DefaultClearMinSamples, DefaultClearKSpread)
}

// fullFitReady is the gate chooseFit is given: the set values, or under
// Admission.TierHysteresis the clear ones for a key already at the full fit.
//
// The latch stands on its evidence. A steady window that emptied, or a key
// that went past DefaultSteadyCarry without one, has lost what put it at the
// full fit, and it goes back to the pinned tier, as a forget does (unlatch):
// new readings earn the full fit at the set values.
func (t *Tracker) fullFitReady(key string, set bool, steady []Observation, adm Admission, now time.Time) bool {
	if !adm.TierHysteresis {
		return set
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	l, ok := t.latches[key]
	if !ok {
		return set
	}
	if len(steady) == 0 || now.Sub(l.seen) > DefaultSteadyCarry {
		l.tier = TierPinnedB
	}
	if len(steady) > 0 {
		l.seen = now
	}
	t.latches[key] = l
	return set || (l.tier == TierOLS && clearReady(steady))
}

// unlatch puts a key at the full fit back at the pinned tier, keeping when it
// last changed. Called with t.mu held.
func (t *Tracker) unlatch(key string) {
	if l, ok := t.latches[key]; ok {
		l.tier, l.fitted = TierPinnedB, false
		t.latches[key] = l
	}
}

// pricesMoreBy reports whether line prices more replicas than other by more
// than the fraction margin (Admission.PricedRate).
func (a Admission) pricesMoreBy(line, other Model, margin float64) bool {
	return a.PricedRate(other) > a.PricedRate(line)*(1+margin)
}

// tierInputs is what dwellTier prices a choice from: the key's readings, as
// chooseFit took them, and the intercept the pinned fit pins.
type tierInputs struct {
	obs, steady []Observation
	baseline    float64
	learned     bool
	// set is whether the set values held this cycle.
	set        bool
	minSettled int
}

// dwellTier latches the key's tier (Admission.TierHysteresis): c is this
// cycle's choice, and what it returns is the choice Observe acts on, with
// whether c's change of tier was refused.
//
// A choice that keeps the tier, or chooses none (a hold, a refused fit), goes
// through. Inside DefaultTierDwell of the last change a change goes through
// only for a line that prices DefaultTierSafeMargin more than the line it
// would replace, so the dwell cannot keep a fleet short by more than that.
// Otherwise a line leaving the full fit holds the key's last slope, and a line
// entering it stays on the pinned fit. With no slope to hold, or no pinned fit
// to stay on, there is nothing to prefer, and the change goes through. So too
// for a key's first full fit, which no dwell delays: until a full fit is had the
// pinned fit rests on a constant for another card, and only the full fit can
// replace it (Liveness, analyzer-learner.md 2.6).
//
// Resting between the clear and set values is not a change of tier, but the
// full fit is weaker there: a pinned fit that prices the margin more takes the
// line at once, the safe side, so resting never prices under it by more.
//
// The latch is not persisted. A key with a slope and no latch has just been
// restored: it stands at the pinned tier from its first line, the safe side,
// and with a restored B a full fit has to price more, or wait out the dwell,
// as any change does.
func (t *Tracker) dwellTier(key string, c fitChoice, in tierInputs, adm Admission, now time.Time) (fitChoice, bool) {
	if c.tier == "" {
		return c, false
	}
	t.mu.Lock()
	held, have := t.slopes[key]
	l, ok := t.latches[key]
	if !ok {
		l = tierLatch{tier: c.tier, since: now, seen: now, fitted: c.tier == TierOLS}
		if have {
			l.tier = TierPinnedB
		}
		t.latches[key] = l
	}
	t.mu.Unlock()
	if c.tier == l.tier {
		if c.tier == TierOLS && !in.set {
			if p := chooseFit(in.obs, in.steady, false, in.baseline, in.minSettled); p.tier == TierPinnedB &&
				adm.pricesMoreBy(pricedLine(p, in, adm), c.model, DefaultTierSafeMargin) {
				t.latch(key, TierPinnedB, now)
				return p, false
			}
		}
		return c, false
	}
	if now.Sub(l.since) < DefaultTierDwell {
		if c.tier == TierPinnedB {
			line := Model{A: held.a, B: in.baseline}
			if have && adm.mayHold(held, now) && !adm.pricesMoreBy(pricedLine(c, in, adm), line, DefaultTierSafeMargin) {
				return fitChoice{hold: true}, true
			}
		} else if in.learned || l.fitted {
			p := chooseFit(in.obs, in.steady, false, in.baseline, in.minSettled)
			if p.tier == TierPinnedB && !adm.pricesMoreBy(c.model, pricedLine(p, in, adm), DefaultTierSafeMargin) {
				return p, true
			}
		}
	}
	t.latch(key, c.tier, now)
	return c, false
}

// latch records a change of the key's tier.
func (t *Tracker) latch(key, tier string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.latches[key] = tierLatch{tier: tier, since: now, seen: now, fitted: t.latches[key].fitted || tier == TierOLS}
}

// pricedLine is the line Observe would take from a pinned choice, one-sided
// where the admission asks for that (oneSidedLine).
func pricedLine(c fitChoice, in tierInputs, adm Admission) Model {
	if adm.OneSidedAboveKSat && !in.learned {
		m, _, _, _ := oneSidedLine(c, in.baseline, adm)
		return m
	}
	return c.model
}
