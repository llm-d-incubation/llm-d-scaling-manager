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
	lowerKnee float64
	kneeOK    bool
	ttft      []Point
	tpot      []Point
	// ttftFloor and tpotFloor are each curve's floor, read off its first
	// answered point.
	ttftFloor, tpotFloor Figure
}

// onCurves, when set, is called each time a curve set is priced: the caching
// test counts it.
var onCurves func()

// priceCurves prices a fleet's curve set, stopping with ctx's error once ctx
// ends: a client gone is no reason to price the rest of a grid.
func priceCurves(ctx context.Context, fl Fleet, tpotAsked bool) (*curveSet, error) {
	if onCurves != nil {
		onCurves()
	}
	cs := &curveSet{prefillKnee: fl.PrefillKnee(), decodeKnee: fl.DecodeKnee(), onsets: fl.Onsets()}
	for _, k := range []whatif.Result{cs.prefillKnee, cs.decodeKnee} {
		if v, ok := valueOf(k); ok && (!cs.kneeOK || v < cs.lowerKnee) {
			cs.lowerKnee, cs.kneeOK = v, true
		}
	}
	cs.ttft, cs.tpot = []Point{}, []Point{}
	if !cs.kneeOK {
		cs.ttftFloor = noFloor(cs.prefillKnee)
		cs.tpotFloor = noFloor(cs.decodeKnee)
		return cs, nil
	}
	for _, rate := range grid(cs.lowerKnee) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t := fl.TTFTAt(rate)
		cs.ttft = append(cs.ttft, Point{Rate: rate, Figure: figureOf(t, unitSeconds, true)})
		if v, ok := t.(whatif.TTFT); ok && cs.ttftFloor.Value == nil {
			cs.ttftFloor = Figure{Value: ptr(v.Floor), Unit: unitSeconds, Flags: flagNames(v.Flags)}
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
			cs.tpotFloor = Figure{Value: ptr(a.Basis.Floor), Unit: unitSeconds, Flags: flagNames(a.Flags)}
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
// lower knee, as the CLI's fleet row caps it.
func (cs *curveSet) markers(headroom float64) Markers {
	m := Markers{}
	pk, dk := figureOf(cs.prefillKnee, unitRate, false), figureOf(cs.decodeKnee, unitRate, false)
	m.PrefillKnee, m.DecodeKnee = &pk, &dk
	if cs.kneeOK {
		m.OperatingCap = &Figure{Value: ptr(headroom * cs.lowerKnee), Unit: unitRate}
	} else {
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
	// ctx is the request's: pricing stops between figures once it ends.
	ctx   context.Context
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

// curves is the fleet's curve set at k, from the cache or priced; a set whose
// pricing the request's end stopped is not cached.
func (p *planner) curves(k Knobs, fl Fleet) (*curveSet, error) {
	key := curveKey(p.e.id, k, p.sc.TPOT != nil)
	if cs, ok := p.s.curves.get(key); ok {
		return cs, nil
	}
	cs, err := priceCurves(p.ctx, fl, p.sc.TPOT != nil)
	if err != nil {
		return nil, err
	}
	return p.s.curves.add(key, cs), nil
}

// limitEntry is what the targets and the headroom set and no rate moves: the
// limits, the marginal steps, and each bucket line's point B. It is cached
// by the fleet's knobs, the targets and the headroom.
type limitEntry struct {
	set LimitSet
	// plusOne is the fleet's plain limits with one more prefill replica, then
	// one more decode replica: decode, prefill and fleet each.
	plusOne [2][3]whatif.Result
	// buckets is each bucket line's rate at its target, with the view asked.
	buckets map[string]whatif.Result
}

// onLimits, when set, is called each time a limit entry is priced: the
// caching test counts it.
var onLimits func()

// limits is the scenario's limit entry, from the cache or priced.
func (p *planner) limits(k Knobs, fl Fleet) (*limitEntry, error) {
	sc := p.sc
	key := contentID("limits", struct {
		Curves   string
		TTFT     ResolvedObjective
		TPOT     *ResolvedObjective
		Headroom float64
		Views    ViewRequest
	}{curveKey(p.e.id, k, sc.TPOT != nil), sc.TTFT, sc.TPOT, sc.Headroom, sc.Views})
	if le, ok := p.s.limits.get(key); ok {
		return le, nil
	}
	if onLimits != nil {
		onLimits()
	}
	tpot := p.tpotTarget()
	le := &limitEntry{set: fl.Limits(sc.TTFT.Target, tpot, sc.Headroom)}
	for i, add := range []func(*Knobs){func(k *Knobs) { k.Prefill++ }, func(k *Knobs) { k.Decode++ }} {
		if err := p.ctx.Err(); err != nil {
			return nil, err
		}
		kk := k
		add(&kk)
		more, err := p.e.pricer.Fleet(kk)
		if err != nil {
			return nil, err
		}
		d, pr, f := more.PlainLimits(sc.TTFT.Target, tpot, sc.Headroom)
		le.plusOne[i] = [3]whatif.Result{d, pr, f}
	}
	if sc.Views.AllTrafficPerBucket {
		le.buckets = map[string]whatif.Result{}
		for _, b := range buckets {
			if err := p.ctx.Err(); err != nil {
				return nil, err
			}
			bf, err := p.e.pricer.Fleet(bucketKnobs(k, b))
			if err != nil {
				return nil, err
			}
			_, le.buckets[b.Name], _ = bf.PlainLimits(sc.TTFT.BucketTargets[b.Name], 0, sc.Headroom)
		}
	}
	return p.s.limits.add(key, le), nil
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

// plan prices the scenario.
func (p *planner) plan() (*PlanResponse, error) {
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
	cs, err := p.curves(k, fl)
	if err != nil {
		return nil, err
	}
	rate, rateSource := 0.0, "request"
	switch {
	case sc.Rate != nil:
		rate = *sc.Rate
	case cs.kneeOK:
		rate, rateSource = sc.Headroom*cs.lowerKnee, "operating point"
	default:
		return nil, inputErrorf("no knee answered to set the operating point (prefill: %s; decode: %s): give traffic.classes.critical.rate",
			reasonOf(cs.prefillKnee), reasonOf(cs.decodeKnee))
	}
	le, err := p.limits(k, fl)
	if err != nil {
		return nil, err
	}
	ls := le.set

	out := &PlanResponse{SchemaVersion: SchemaVersion, WhatifSchema: p.s.whatifSchema, InputsID: p.e.id, Banner: banner, Scenario: sc}
	markers := cs.markers(sc.Headroom)
	out.TTFT = TTFTView{Statistic: sc.TTFT.Statistic, Markers: p.markers("ttft.markers", markers)}
	at, notes := fl.TTFTPoint(rate)
	out.TTFT.Line = p.line("ttft.line", Line{Bucket: SemanticsMix, Semantics: SemanticsMix, Target: sc.TTFT.Target}, cs, at, ls.Prefill, rate)

	if sc.TPOT != nil {
		mean, pct := fl.TPOTAt(rate)
		at := mean
		if pct != nil {
			at = pct
		}
		tv := &TPOTView{Statistic: sc.TPOT.Statistic, Target: sc.TPOT.Target, Floor: p.note("tpot.floor", cs.tpotFloor),
			Curve: p.points("tpot.curve", cs.tpot), Markers: Markers{DecodeKnee: markers.DecodeKnee}}
		if r := fl.TPOTRatio(); r.Value != nil || r.Refused != nil {
			tv.Ratio = &r
		}
		tv.AtRate = RatePoint{Rate: rate, Figure: p.note("tpot.atRate", figureOf(at, unitSeconds, true))}
		tv.AtTarget = TargetPoint{Seconds: sc.TPOT.Target, Rate: p.note("tpot.atTarget", figureOf(ls.Decode, unitRate, false))}
		out.TPOT = tv
	}

	out.Limits = p.limitsOf(rate, rateSource, ls)
	out.Marginal = p.marginal(le, ls.Fleet)
	if sc.Views.AllTrafficPerBucket {
		if out.TTFT.PerBucket, err = p.perBucket(k, le, rate); err != nil {
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
		return []string{"prefill"}
	}
	dv, dok := valueOf(decode)
	switch {
	case !pok && !dok:
		return []string{"prefill", "decode"}
	case !pok:
		return []string{"prefill"}
	case !dok:
		return []string{"decode"}
	case pv < dv:
		return []string{"prefill"}
	case dv < pv:
		return []string{"decode"}
	}
	return []string{"prefill", "decode"}
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
// and points. No limit reads it.
func (p *planner) perBucket(k Knobs, le *limitEntry, rate float64) ([]Line, error) {
	var lines []Line
	for _, b := range buckets {
		kb := bucketKnobs(k, b)
		fl, err := p.e.pricer.Fleet(kb)
		if err != nil {
			return nil, err
		}
		cs, err := p.curves(kb, fl)
		if err != nil {
			return nil, err
		}
		path := "ttft.perBucket." + b.Name
		knees := p.markers(path+".knees", cs.markers(p.sc.Headroom))
		l := Line{Bucket: b.Name, Semantics: SemanticsAllTraffic, PromptTokens: ptr(kb.PromptTokens),
			Target: p.sc.TTFT.BucketTargets[b.Name], Knees: &knees}
		at, _ := fl.TTFTPoint(rate)
		lines = append(lines, p.line(path, l, cs, at, le.buckets[b.Name], rate))
	}
	return lines, nil
}
