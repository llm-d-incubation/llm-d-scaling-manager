package whatifapi

import (
	"context"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// gridPoints is how many rates a curve is priced at below the lower knee. The
// rates are even steps in sqrt(1 - rate/knee), so they crowd toward the knee,
// where TTFT turns up; two more sit at the knee and past it, where every
// point is refused.
const gridPoints = 64

// pastKnee is the last grid rate, as a multiple of the lower knee: past it the
// graph shows only the saturated region.
const pastKnee = 1.05

// grid is the curve's rates for a lower knee.
func grid(knee float64) []float64 {
	rates := make([]float64, 0, gridPoints+1)
	for i := 1; i <= gridPoints; i++ {
		u := 1 - float64(i)/gridPoints
		rates = append(rates, knee*(1-u*u))
	}
	return append(rates, pastKnee*knee)
}

// banner is what every TTFT figure says of itself, from the constant the
// controller publishes, so that it cannot drift from the model.
const banner = "TTFT model UNVALIDATED. " + constants.SLOTTFTPredictionCaveat

// curveSet is what a fleet's graphs draw that no rate or target moves: the
// knees, the onsets and both curves. It is cached by the fleet's knobs.
type curveSet struct {
	prefillKnee, decodeKnee whatif.Result
	onsets                  whatif.Result
	// lowerKnee is the lower of the knees answered; ok false with neither.
	// The curves stop past it.
	lowerKnee float64
	kneeOK    bool
	// capRefused is the knee refusal that leaves no operating-point cap,
	// nil where the cap prices. The cap is headroom times the lower of both
	// knees: with one refused, the other alone would claim a bound no rule
	// gives, as at-replicas refuses the fleet. Decode's refusal is named
	// first. A source with no prefill role at all has no prefill knee to
	// read: its cap is decode's alone.
	capRefused whatif.Result
	ttft       []Point
	tpot       []Point
	// ttftFloor and tpotFloor are each curve's floor, read off its first
	// answered point.
	ttftFloor, tpotFloor Figure
}

// priceCurves prices a fleet's curve set, stopping with ctx's error once ctx
// ends: a client gone is no reason to price the rest of a grid. prefillRole
// is false for a source with no prefill variant: its prefill knee is absent,
// not refused.
func priceCurves(ctx context.Context, fl Fleet, tpotAsked, prefillRole bool) (*curveSet, error) {
	cs := &curveSet{prefillKnee: fl.PrefillKnee(), decodeKnee: fl.DecodeKnee(), onsets: fl.Onsets()}
	for _, k := range []whatif.Result{cs.prefillKnee, cs.decodeKnee} {
		if v, ok := valueOf(k); ok && (!cs.kneeOK || v < cs.lowerKnee) {
			cs.lowerKnee, cs.kneeOK = v, true
		}
	}
	switch {
	case isRefusal(cs.decodeKnee):
		cs.capRefused = cs.decodeKnee
	case prefillRole && isRefusal(cs.prefillKnee):
		cs.capRefused = cs.prefillKnee
	}
	cs.ttft, cs.tpot = []Point{}, []Point{}
	if !cs.kneeOK {
		cs.ttftFloor = noFloor(cs.prefillKnee)
		cs.tpotFloor = noFloor(cs.decodeKnee)
		return cs, nil
	}
	// The experimental TTFT model may draw past a prefill knee that is only
	// a lower bound, up to the load the fleet served (curveCap).
	span := cs.lowerKnee
	if x, ok := fl.(interface{ curveEnd() float64 }); ok {
		span = max(span, x.curveEnd())
	}
	for _, rate := range grid(span) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t := fl.TTFTAt(rate)
		cs.ttft = append(cs.ttft, Point{Rate: rate, Figure: markExtrapolated(fl, rate, figureOf(t, unitSeconds, true))})
		if v, ok := t.(whatif.TTFT); ok && cs.ttftFloor.Value == nil {
			cs.ttftFloor = Figure{Value: ptr(v.Floor), Unit: unitSeconds, Flags: FlagNames(v.Flags)}
		}
		if !tpotAsked {
			continue
		}
		mean, pct := fl.TPOTAt(rate)
		r := mean
		if pct != nil {
			r = pct
		}
		cs.tpot = append(cs.tpot, Point{Rate: rate, Figure: figureOf(r, unitSeconds, true)})
		if a, ok := r.(whatif.Answer); ok && cs.tpotFloor.Value == nil && a.Basis.Floor > 0 {
			cs.tpotFloor = Figure{Value: ptr(a.Basis.Floor), Unit: unitSeconds, Flags: FlagNames(a.Flags)}
		}
	}
	if cs.ttftFloor.Value == nil {
		cs.ttftFloor = noFloor(firstRefusal(cs.ttft, cs.prefillKnee))
	}
	if cs.tpotFloor.Value == nil {
		cs.tpotFloor = noFloor(firstRefusal(cs.tpot, cs.decodeKnee))
	}
	return cs, nil
}

// rateSource is what sets a plan's rate where the scenario gave none, with a
// knee answered: the operating point, or with the cap refused, headroom times
// the one knee that priced. That is a rate to start from, not an operating
// point: the limits and the cap stay refused.
func (cs *curveSet) rateSource() string {
	switch {
	case cs.capRefused == nil:
		return RateSourceOperatingPoint
	case isRefusal(cs.decodeKnee):
		return RateSourcePrefillKnee
	default:
		return RateSourceDecodeKnee
	}
}

// noFloor is the floor of a line with no answered point: r's refusal.
func noFloor(r whatif.Result) Figure {
	if u, ok := r.(whatif.Unanswerable); ok {
		return Figure{Refused: refusalOf(u)}
	}
	return Figure{Refused: &Refusal{Reason: "NoAnsweredPoint", Detail: "no point of the curve is answered"}}
}

// firstRefusal is the first refused point's refusal, as a Result, else
// fallback.
func firstRefusal(points []Point, fallback whatif.Result) whatif.Result {
	for _, p := range points {
		if r := p.Figure.Refused; r != nil {
			return whatif.Unanswerable{Reason: whatif.Reason(r.Reason), Detail: r.Detail}
		}
	}
	return fallback
}

// markers are a curve set's markers at headroom: the cap is headroom times the
// lower knee, as the CLI's fleet row caps it, and the refused knee's refusal
// where one is (capRefused).
func (cs *curveSet) markers(headroom float64) Markers {
	m := Markers{}
	pk, dk := figureOf(cs.prefillKnee, unitRate, false), figureOf(cs.decodeKnee, unitRate, false)
	m.PrefillKnee, m.DecodeKnee = &pk, &dk
	switch {
	case cs.capRefused != nil:
		c := noFloor(cs.capRefused)
		m.OperatingCap = &c
	case cs.kneeOK:
		m.OperatingCap = &Figure{Value: ptr(headroom * cs.lowerKnee), Unit: unitRate}
	default:
		c := noFloor(cs.prefillKnee)
		m.OperatingCap = &c
	}
	switch v := cs.onsets.(type) {
	case whatif.Onsets:
		m.Onsets = &Onsets{Prefill: ptr(v.Prefill), Decode: ptr(v.Decode)}
	case whatif.Unanswerable:
		m.Onsets = &Onsets{Refused: refusalOf(v)}
	}
	return m
}

// planner prices one scenario on one inputs entry.
type planner struct {
	s     *Server
	e     *inputsEntry
	sc    Scenario
	flags map[string][]string
	order []string
}

// note records that the flags of f qualify the figure at path.
func (p *planner) note(path string, f Figure) Figure {
	names := slices.Clone(f.Flags)
	if f.Refused != nil {
		names = append(names, f.Refused.Flags...)
	}
	for _, name := range names {
		if _, ok := p.flags[name]; !ok {
			p.order = append(p.order, name)
		}
		if !slices.Contains(p.flags[name], path) {
			p.flags[name] = append(p.flags[name], path)
		}
	}
	return f
}

// curves is the fleet's curve set at k, from the cache or priced, one pricing
// per key at a time; a set whose pricing ctx stopped is not cached.
func (p *planner) curves(ctx context.Context, k Knobs, fl Fleet) (*curveSet, error) {
	key := curveKey(p.e.id, k, p.sc.TPOT != nil)
	if cs, ok := p.s.curves.get(key); ok {
		return cs, nil
	}
	return p.s.curveFlight.doFor(ctx, key, func() (*curveSet, error) {
		if cs, ok := p.s.curves.get(key); ok {
			return cs, nil
		}
		if p.s.onCurves != nil {
			p.s.onCurves()
		}
		cs, err := priceCurves(ctx, fl, p.sc.TPOT != nil, p.e.doc.Fleet.Prefill != nil)
		if err != nil {
			return nil, err
		}
		return p.s.curves.add(key, cs), nil
	})
}

// limitEntry is what the targets and the headroom set and no rate moves: the
// limits, the marginal steps, and each bucket line's point B. It is cached
// by the fleet's knobs, the targets and the headroom.
type limitEntry struct {
	set LimitSet
	// plusOne is the fleet's plain limits with one more prefill replica, then
	// one more decode replica: decode, prefill and fleet each.
	plusOne [2][3]whatif.Result
	// plusOneExtrapolated says each step's fleet limit is one the
	// experimental TTFT or TPOT model set past the highest load served
	// (inputsEntry.markLimit).
	plusOneExtrapolated [2]bool
	// buckets is each bucket line's rate at its target, with the view asked.
	buckets map[string]whatif.Result
}

// limits is the scenario's limit entry, from the cache or priced, one pricing
// per key at a time.
func (p *planner) limits(ctx context.Context, k Knobs, fl Fleet) (*limitEntry, error) {
	sc := p.sc
	key := contentID("limits", struct {
		Curves   string
		TTFT     ResolvedObjective
		TPOT     *ResolvedObjective
		Headroom float64
		Views    ViewRequest
		Edges    []int `json:",omitempty"`
	}{curveKey(p.e.id, k, sc.TPOT != nil), sc.TTFT, sc.TPOT, sc.Headroom, sc.Views, sc.BucketEdges})
	if le, ok := p.s.limits.get(key); ok {
		return le, nil
	}
	return p.s.limitFlight.doFor(ctx, key, func() (*limitEntry, error) {
		if le, ok := p.s.limits.get(key); ok {
			return le, nil
		}
		le, err := p.priceLimits(ctx, k, fl)
		if err != nil {
			return nil, err
		}
		return p.s.limits.add(key, le), nil
	})
}

// priceLimits prices the limit entry, stopping with ctx's error once ctx
// ends.
func (p *planner) priceLimits(ctx context.Context, k Knobs, fl Fleet) (*limitEntry, error) {
	sc := p.sc
	if p.s.onLimits != nil {
		p.s.onLimits()
	}
	tpot := p.tpotTarget()
	le := &limitEntry{set: fl.Limits(sc.TTFT.Target, tpot, sc.Headroom)}
	for i, add := range []func(*Knobs){func(k *Knobs) { k.Prefill++ }, func(k *Knobs) { k.Decode++ }} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		kk := k
		add(&kk)
		more, err := p.e.pricer.Fleet(kk)
		if err != nil {
			return nil, err
		}
		more = p.e.modelFleet(more, kk)
		d, pr, f := more.PlainLimits(sc.TTFT.Target, tpot, sc.Headroom)
		le.plusOne[i] = [3]whatif.Result{d, pr, f}
		le.plusOneExtrapolated[i] = p.e.markLimit(more, kk, figureOf(f, unitRate, false)).Extrapolated
	}
	if sc.Views.AllTrafficPerBucket {
		le.buckets = map[string]whatif.Result{}
		for _, b := range sc.buckets() {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			bf, err := p.e.pricer.Fleet(bucketKnobs(k, b))
			if err != nil {
				return nil, err
			}
			_, le.buckets[b.Name], _ = bf.PlainLimits(sc.TTFT.BucketTargets[b.Name], 0, sc.Headroom)
		}
	}
	return le, nil
}

// tpotTarget is the TPOT target, 0 with no TPOT asked.
func (p *planner) tpotTarget() float64 {
	if p.sc.TPOT == nil {
		return 0
	}
	return p.sc.TPOT.Target
}

// bucketKnobs is k with every request at bucket b's upper edge.
func bucketKnobs(k Knobs, b Bucket) Knobs {
	k.PromptTokens = float64(b.UpperTokens)
	return k
}

// plan prices the scenario, stopping with ctx's error once ctx ends.
func (p *planner) plan(ctx context.Context) (*PlanResponse, error) {
	p.flags = map[string][]string{}
	sc := p.sc
	k, err := sc.knobs()
	if err != nil {
		return nil, err
	}
	fl, err := p.e.pricer.Fleet(k)
	if err != nil {
		return nil, err
	}
	// The experimental TTFT model, where the source was resolved with one,
	// prices the TTFT curve and point A; every other figure is the
	// planner's.
	fl = p.e.modelFleet(fl, k)
	cs, err := p.curves(ctx, k, fl)
	if err != nil {
		return nil, err
	}
	// The fleet priced at the mean TTFT: a concurrency is a rate times a
	// mean time in system.
	mean, err := p.meanFleet(k, fl)
	if err != nil {
		return nil, err
	}
	ol := p.outputTokens(k)
	rate, rateSource := 0.0, RateSourceRequest
	var fromConcurrency *ConcurrencyRate
	switch {
	case sc.Rate != nil:
		rate = *sc.Rate
	case sc.Concurrency != nil:
		cr, err := planRateAtConcurrency(mean, *sc.Concurrency, cs, ol)
		if err != nil {
			return nil, err
		}
		rate, rateSource, fromConcurrency = cr.Rate, RateSourceConcurrency, &cr
	case cs.kneeOK:
		rate, rateSource = sc.Headroom*cs.lowerKnee, cs.rateSource()
	default:
		return nil, InputErrorf("no knee answered to set the operating point (prefill: %s; decode: %s): give traffic.classes.critical.rate",
			reasonOf(cs.prefillKnee), reasonOf(cs.decodeKnee))
	}
	le, err := p.limits(ctx, k, fl)
	if err != nil {
		return nil, err
	}
	ls := le.set

	out := &PlanResponse{SchemaVersion: SchemaVersion, WhatifSchema: p.s.whatifSchema, InputsID: p.e.id, Banner: banner, Scenario: sc}
	markers := cs.markers(sc.Headroom)
	out.TTFT = TTFTView{Statistic: sc.TTFT.Statistic, Markers: p.markers("ttft.markers", markers), Model: p.e.ttftModel.view(fl, k)}
	at, notes := fl.TTFTPoint(rate)
	out.TTFT.Line = p.line("ttft.line", Line{Bucket: SemanticsMix, Semantics: SemanticsMix, Target: sc.TTFT.Target}, cs, at, ls.Prefill, rate)
	out.TTFT.Line.AtRate.Figure = markExtrapolated(fl, rate, out.TTFT.Line.AtRate.Figure)

	if sc.TPOT != nil {
		mean, pct := fl.TPOTAt(rate)
		at := mean
		if pct != nil {
			at = pct
		}
		// The experimental TPOT model, where the source was resolved with
		// one, prices the percentile; a figure it prices past the highest
		// load measured is marked.
		tm := p.e.tpotModel
		tv := &TPOTView{Statistic: sc.TPOT.Statistic, Target: sc.TPOT.Target, Floor: p.note("tpot.floor", cs.tpotFloor),
			Curve: p.points("tpot.curve", tm.markPoints(k, cs.tpot)), Markers: Markers{DecodeKnee: markers.DecodeKnee},
			Model: tm.view(fl, k)}
		if r := fl.TPOTRatio(); r.Value != nil || r.Refused != nil {
			tv.Ratio = &r
		}
		tv.AtRate = RatePoint{Rate: rate, Figure: p.note("tpot.atRate", tm.mark(k, rate, figureOf(at, unitSeconds, true)))}
		tv.AtTarget = TargetPoint{Seconds: sc.TPOT.Target, Rate: p.note("tpot.atTarget", tm.mark(k, 0, figureOf(ls.Decode, unitRate, false)))}
		out.TPOT = tv
	}

	out.Limits = p.limitsOf(rate, rateSource, ls)
	tm, _ := mean.TTFTPoint(rate)
	pm, _ := mean.TPOTAt(rate)
	out.Limits.Chosen.Concurrency = ConcurrencyAt(rate, tm, pm, ol, k.Decode, cs.saturatedAt(rate))
	out.Limits.Chosen.FromConcurrency = fromConcurrency
	// A limit the experimental TTFT model set past the highest load the
	// source served is an extrapolation.
	out.Limits.Prefill = markExtrapolatedRate(fl, out.Limits.Prefill)
	out.Limits.Fleet.Figure = markExtrapolatedRate(fl, out.Limits.Fleet.Figure)
	out.Limits.Fleet.WithoutHeadroom = markExtrapolatedRate(fl, out.Limits.Fleet.WithoutHeadroom)
	// So is one the experimental TPOT model set.
	if d := out.Limits.Decode; d != nil {
		*d = p.e.tpotModel.mark(k, 0, *d)
	}
	out.Limits.Fleet.Figure = p.e.tpotModel.mark(k, 0, out.Limits.Fleet.Figure)
	out.Limits.Fleet.WithoutHeadroom = p.e.tpotModel.mark(k, 0, out.Limits.Fleet.WithoutHeadroom)
	validatePlan(out, k, fl.Class(), cs, rate)
	out.Marginal = p.marginal(le, ls.Fleet)
	if sc.Views.AllTrafficPerBucket {
		if out.TTFT.PerBucket, err = p.perBucket(ctx, k, le, rate); err != nil {
			return nil, err
		}
	}
	out.Assumed = []string{}
	for _, words := range [][]string{fl.Assumed(), ls.Notes, notes} {
		for _, w := range words {
			if !slices.Contains(out.Assumed, w) {
				out.Assumed = append(out.Assumed, w)
			}
		}
	}
	out.Flags = []FlagUse{}
	for _, name := range p.order {
		out.Flags = append(out.Flags, FlagUse{Flag: name, On: p.flags[name]})
	}
	return out, nil
}

// meanFleet is the fleet at k priced at the mean TTFT: fl itself where k
// asks the mean.
func (p *planner) meanFleet(k Knobs, fl Fleet) (Fleet, error) {
	if k.TTFTPercentile == 0 {
		return fl, nil
	}
	k.TTFTPercentile = 0
	mean, err := p.e.pricer.Fleet(k)
	if err != nil {
		return nil, err
	}
	return p.e.ttftModel.fleet(mean, k), nil
}

// saturatedAt says rate is at or past the lower knee, where requests queue
// without bound. A prefill knee that is only a lower bound (V_P a lower
// bound) is not known to saturate there: the figures past it are refused for
// their own reason.
func (cs *curveSet) saturatedAt(rate float64) bool {
	if !cs.kneeOK || rate < cs.lowerKnee {
		return false
	}
	if cs.lowerKneeIsLowerBound() {
		d, ok := valueOf(cs.decodeKnee)
		return ok && rate >= d
	}
	return true
}

// lowerKneeIsLowerBound says the lower knee is a prefill knee priced from a
// V_P known only as a lower bound: the true knee is at least it.
func (cs *curveSet) lowerKneeIsLowerBound() bool {
	a, ok := cs.prefillKnee.(whatif.Answer)
	return ok && a.Value == cs.lowerKnee && slices.Contains(a.Flags, whatif.VPLowerBound)
}

// planRateAtConcurrency is the rate at which mean, the fleet at the mean
// TTFT, holds n requests in the system on average, below the lower knee.
func planRateAtConcurrency(mean Fleet, n float64, cs *curveSet, ol float64) (ConcurrencyRate, error) {
	if !cs.kneeOK {
		return ConcurrencyRate{}, InputErrorf("traffic.classes.critical.concurrency: no knee answered to translate a concurrency below (prefill: %s; decode: %s): give a rate",
			reasonOf(cs.prefillKnee), reasonOf(cs.decodeKnee))
	}
	if !(ol > 0) {
		return ConcurrencyRate{}, InputErrorf("traffic.classes.critical.concurrency: no output length to price the time in the system at: give traffic.mix.outputTokens or a rate")
	}
	var refused whatif.Result
	w := func(rate float64) (float64, bool) {
		t, _ := mean.TTFTPoint(rate)
		tm, _ := mean.TPOTAt(rate)
		v, ok := meanTimeInSystem(t, tm, ol)
		if !ok && refused == nil {
			refused = t
			if _, tok := valueOf(t); tok {
				refused = tm
			}
		}
		return v, ok
	}
	cr, err := RateAtConcurrency(n, cs.lowerKnee, w)
	switch {
	case ConcurrencyUnpriced(err):
		return cr, InputErrorf("traffic.classes.critical.concurrency: the mean time in the system is refused (%s): give a rate", reasonOf(refused))
	case err != nil:
		return cr, InputErrorf("traffic.classes.critical.concurrency: %v", err)
	case cr.AtKnee && cr.Rate >= cs.lowerKnee && cs.lowerKneeIsLowerBound():
		// Whether the fleet saturates at a knee that is only a lower bound
		// is not known (#280).
		return cr, InputErrorf("traffic.classes.critical.concurrency: the fleet holds fewer than %g requests below its prefill knee of at least %.4g r/s, "+
			"which rests on a lower bound of V_P: the true knee may be higher, so the rate that holds them is not priced; give a rate or assume.vp", n, cs.lowerKnee)
	}
	t, _ := mean.TTFTPoint(cr.Rate)
	cr.LowerBound = upperBoundTTFT(t)
	return cr, nil
}

// meanTimeInSystem is TTFT mean + (OL - 1) x TPOT mean, the mean time a
// request spends in the system; ok false where either mean is refused.
func meanTimeInSystem(ttft, tpot whatif.Result, ol float64) (float64, bool) {
	t, tok := valueOf(ttft)
	p, pok := valueOf(tpot)
	if !tok || !pok {
		return 0, false
	}
	return t + max(ol-1, 0)*p, true
}

// reasonOf is a refusal's reason, or "answered".
func reasonOf(r whatif.Result) string {
	if u, ok := r.(whatif.Unanswerable); ok {
		return string(u.Reason)
	}
	return "answered"
}

// markers notes the markers' flags under path.
func (p *planner) markers(path string, m Markers) Markers {
	for _, c := range []struct {
		name string
		f    *Figure
	}{{"prefillKnee", m.PrefillKnee}, {"decodeKnee", m.DecodeKnee}, {"operatingCap", m.OperatingCap}} {
		if c.f != nil {
			p.note(path+"."+c.name, *c.f)
		}
	}
	return m
}

// points notes the flags of a curve's points under path.
func (p *planner) points(path string, points []Point) []Point {
	for _, pt := range points {
		p.note(path, pt.Figure)
	}
	return points
}

// line fills a TTFT line from its curve set: the floor, the curve, point A
// (at, the TTFT priced at the rate) and point B (target, the rate at the
// target).
func (p *planner) line(path string, l Line, cs *curveSet, at whatif.Result, target whatif.Result, rate float64) Line {
	l.Floor = p.note(path+".floor", cs.ttftFloor)
	l.Curve = p.points(path+".curve", cs.ttft)
	l.AtRate = RatePoint{Rate: rate, Figure: p.note(path+".atRate", figureOf(at, unitSeconds, true))}
	l.AtTarget = TargetPoint{Seconds: l.Target, Rate: p.note(path+".atTarget", figureOf(target, unitRate, false))}
	return l
}

// limitsOf is the limits at the rate: each role's, the fleet's with the cap
// and without it, and the rate's headroom under the fleet's.
func (p *planner) limitsOf(rate float64, rateSource string, ls LimitSet) Limits {
	l := Limits{Prefill: p.note("limits.prefill", figureOf(ls.Prefill, unitRate, false))}
	if ls.Decode != nil {
		d := p.note("limits.decode", figureOf(ls.Decode, unitRate, false))
		l.Decode = &d
	}
	l.Fleet = FleetLimit{Figure: p.note("limits.fleet", figureOf(ls.Fleet, unitRate, false)),
		WithoutHeadroom: p.note("limits.fleet.withoutHeadroom", figureOf(ls.WithoutHeadroom, unitRate, false)),
		Binding:         binding(ls.Prefill, ls.Decode)}
	l.Fleet.CapApplied = slices.Contains(l.Fleet.BoundBy, string(whatif.BoundOperatingPoint))
	l.Chosen = Chosen{Rate: rate, RateSource: rateSource}
	if v, ok := valueOf(ls.Fleet); ok {
		l.Chosen.Headroom = Figure{Value: ptr(v - rate), Unit: unitRate}
	} else {
		l.Chosen.Headroom = refusedFigure(l.Fleet.Refused)
	}
	return l
}

// binding is the role whose limit is lower, both on a tie; a refused role
// binds, since the fleet's limit is its refusal.
func binding(prefill, decode whatif.Result) []string {
	pv, pok := valueOf(prefill)
	if decode == nil {
		return []string{rolePrefill}
	}
	dv, dok := valueOf(decode)
	switch {
	case !pok && !dok:
		return []string{rolePrefill, roleDecode}
	case !pok:
		return []string{rolePrefill}
	case !dok:
		return []string{roleDecode}
	case pv < dv:
		return []string{rolePrefill}
	case dv < pv:
		return []string{roleDecode}
	}
	return []string{rolePrefill, roleDecode}
}

// marginal is the fleet's limit with one more replica of each role, and its
// change from fleet, the scenario's.
func (p *planner) marginal(le *limitEntry, fleet whatif.Result) Marginal {
	var m Marginal
	sc := p.sc
	for i, step := range []struct {
		path  string
		into  *MarginalStep
		count FleetCounts
	}{
		{"marginal.prefillPlusOne", &m.PrefillPlusOne, FleetCounts{Prefill: sc.Fleet.Prefill + 1, Decode: sc.Fleet.Decode}},
		{"marginal.decodePlusOne", &m.DecodePlusOne, FleetCounts{Prefill: sc.Fleet.Prefill, Decode: sc.Fleet.Decode + 1}},
	} {
		decode, prefill, limit := le.plusOne[i][0], le.plusOne[i][1], le.plusOne[i][2]
		s := MarginalStep{Fleet: step.count, Limit: p.note(step.path, figureOf(limit, unitRate, false)), Binding: binding(prefill, decode)}
		s.Limit.Extrapolated = le.plusOneExtrapolated[i]
		now, nowOK := valueOf(fleet)
		then, thenOK := valueOf(limit)
		switch {
		case nowOK && thenOK:
			s.Delta = Figure{Value: ptr(then - now), Unit: unitRate}
		case !thenOK:
			s.Delta = refusedFigure(s.Limit.Refused)
		default:
			s.Delta = figureOf(fleet, unitRate, false)
		}
		*step.into = s
	}
	return m
}

// perBucket is the "if all traffic were bucket b" view: each bucket's fleet,
// every request at the bucket's upper edge, with its own knees, curve, target
// and points. No limit reads it. A line whose edge and the mix's output
// together pass the context window, where the inputs know it, is flagged
// EdgeBeyondContext on its points: it is priced, as the model would, but
// vLLM refuses a request whose prompt and max_tokens pass --max-model-len.
func (p *planner) perBucket(ctx context.Context, k Knobs, le *limitEntry, rate float64) ([]Line, error) {
	buckets := p.sc.buckets()
	lines := make([]Line, 0, len(buckets))
	window, known := p.contextWindow()
	for _, b := range buckets {
		kb := bucketKnobs(k, b)
		fl, err := p.e.pricer.Fleet(kb)
		if err != nil {
			return nil, err
		}
		cs, err := p.curves(ctx, kb, fl)
		if err != nil {
			return nil, err
		}
		path := "ttft.perBucket." + b.Name
		knees := p.markers(path+".knees", cs.markers(p.sc.Headroom))
		l := Line{Bucket: b.Name, Semantics: SemanticsAllTraffic, PromptTokens: ptr(kb.PromptTokens),
			Target: p.sc.TTFT.BucketTargets[b.Name], Knees: &knees}
		at, _ := fl.TTFTPoint(rate)
		l = p.line(path, l, cs, at, le.buckets[b.Name], rate)
		if known && float64(b.UpperTokens)+p.outputTokens(k) > window {
			l.AtRate.Figure = p.note(path+".atRate", withFlag(l.AtRate.Figure, FlagEdgeBeyondContext))
			l.AtTarget.Rate = p.note(path+".atTarget", withFlag(l.AtTarget.Rate, FlagEdgeBeyondContext))
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// outputTokens is the output length a request of the scenario asks: the
// stated one, else the observed mean, else 0 where neither is known.
func (p *planner) outputTokens(k Knobs) float64 {
	if k.OutputTokens > 0 {
		return k.OutputTokens
	}
	if v := p.e.doc.Observed.OutputTokens.Value; v != nil && *v > 0 {
		return *v
	}
	return 0
}

// contextWindow is the engines' context window in tokens, from the inputs'
// TermMaxModelLen, and whether they know it.
func (p *planner) contextWindow() (float64, bool) { return contextWindowOf(&p.e.doc) }

// contextWindowOf is the engines' context window in tokens from doc's
// TermMaxModelLen, and whether doc knows it.
func contextWindowOf(doc *InputsResponse) (float64, bool) {
	for _, t := range doc.Terms {
		if t.Name == TermMaxModelLen && t.Refused == nil && t.Value != nil && *t.Value > 0 {
			return *t.Value, true
		}
	}
	return 0, false
}

// withFlag is f with flag added: on the figure, or on its refusal.
func withFlag(f Figure, flag string) Figure {
	if f.Refused != nil {
		r := *f.Refused
		r.Flags = append(slices.Clone(r.Flags), flag)
		f.Refused = &r
		return f
	}
	f.Flags = append(slices.Clone(f.Flags), flag)
	return f
}

// validatePlan sets the envelope's verdict on the figures a plan reads: the
// knees, the TTFT at the rate and the rate at its target, the TPOT's two
// points, and the limits. A marginal step is another fleet's, and carries
// none.
func validatePlan(out *PlanResponse, k Knobs, c whatif.FigureClass, cs *curveSet, rate float64) {
	m := out.TTFT.Markers
	if m.PrefillKnee != nil {
		*m.PrefillKnee = validate(*m.PrefillKnee, whatif.FigurePrefillKnee, 0, c, 0)
	}
	if m.DecodeKnee != nil {
		*m.DecodeKnee = validate(*m.DecodeKnee, whatif.FigureDecodeKnee, 0, c, 0)
	}
	q := k.TTFTPercentile
	knee := 0.0
	if cs.kneeOK {
		knee = cs.lowerKnee
	}
	l := &out.TTFT.Line
	l.AtRate.Figure = validate(l.AtRate.Figure, whatif.FigureTTFT, q, c, shareOf(rate, knee))
	l.AtTarget.Rate = validateRate(l.AtTarget.Rate, q, c, knee)
	if t := out.TPOT; t != nil {
		kind := whatif.FigureMeanTPOT
		if k.TPOTPercentile != 0 {
			kind = whatif.FigurePercentileTPOT
		}
		dk, _ := valueOf(cs.decodeKnee)
		t.AtRate.Figure = validate(t.AtRate.Figure, kind, k.TPOTPercentile, c, shareOf(rate, dk))
		t.AtTarget.Rate = validate(t.AtTarget.Rate, whatif.FigureRateAtTPOT, 0, c, 0)
	}
	lim := &out.Limits
	lim.Prefill = validateRate(lim.Prefill, q, c, knee)
	if lim.Decode != nil {
		*lim.Decode = validate(*lim.Decode, whatif.FigureRateAtTPOT, 0, c, 0)
	}
	lim.Fleet.Figure = validateRate(lim.Fleet.Figure, q, c, knee)
	lim.Fleet.WithoutHeadroom = validateRate(lim.Fleet.WithoutHeadroom, q, c, knee)
}
