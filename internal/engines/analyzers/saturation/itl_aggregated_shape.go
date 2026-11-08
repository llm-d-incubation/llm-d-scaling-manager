package saturation

import (
	"math"
	"slices"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/fleet"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The ITL line of an aggregated variant, by request shape (issue ev-shindin/llm-scaling-manager#275,
// docs/proposals/aggregated-itl-shape.md).
//
// An aggregated pod runs the prompts it admits as chunks inside its decode
// steps, so a step takes about B + A*k + c*x, with x the prefill tokens a
// step computes, about n*IL_eff/OL for n resident sequences. At one shape n
// is k*C/KVreq, so x = g*k with
//
//	g = C * IL_eff / (KVreq * OL)
//
// and the line the tracker fits, B + A_g*k, has the slope A_g = A + c*g.
// The fit is left as it is; what this file adds:
//
//   - every line carries the g it was fitted at (lineShape.g), and the
//     variant's window is dropped when g moves past aggShapeTolerance for
//     aggShapeMoveCycles cycles, so a fit is never made from two shapes'
//     readings;
//   - c is read off the slopes of two or more shapes (lineShape.points,
//     fitShapeTerm), never from one;
//   - a line never prices a shape lighter than it is: at or below its own g
//     as it is, and a higher g through c, or with no c, at the worst case
//     c allows (resolveAggregatedPrice);
//   - the derived mu is corrected for the steps a request spends in prefill,
//     which hold KV and emit no token (sigma, from Little's law);
//   - a variant's figure sizes once the GPS check has agreed with it for
//     aggLiftCycles cycles (noteAggregatedLift), and never above a measured
//     rate of the variant's own (whatif.DerivedMu.CapAtMeasured).
//
// The analyzer runs one cycle of a model on one goroutine; a.mu is taken
// and released several times in a cycle only to guard against other models'
// cycles, never to order this one's steps.

// SwitchITLAggregatedShapeTerm is the replay switch for the shape term of an
// aggregated variant's line (issue ev-shindin/llm-scaling-manager#275): the shape tags, the window dropped
// on a shape change, the shape points and c, and the corrected figure. Off,
// the figure is ev-shindin/llm-scaling-manager#432's. The figure stays reported only: sizing from it is
// SwitchITLAggregatedSizes.
const SwitchITLAggregatedShapeTerm = "itl-aggregated-shape-term"

// SwitchITLAggregatedSizes is the replay switch for the lift (issue ev-shindin/llm-scaling-manager#275):
// an aggregated variant's figure prices its replicas once the GPS check has
// agreed with it. It needs the shape term, so with
// SwitchITLAggregatedShapeTerm off it lifts nothing.
const SwitchITLAggregatedSizes = "itl-aggregated-sizes"

// The thresholds of the shape term. None is measured: they are first
// choices, named so a measurement can move them. None is a policy key.
const (
	// aggShapeTolerance is how far g may move, as a fraction, and still be
	// the shape a line was fitted at: the shape tracker's tolerance on IL
	// and OL (shape.DefaultChangeTolerance). Unmeasured.
	aggShapeTolerance = shape.DefaultChangeTolerance
	// aggShapeMoveCycles is how many consecutive cycles g must hold outside
	// aggShapeTolerance of the window's shape before the window is dropped:
	// a scale-up whose cold replicas move the hit rate for a cycle is not a
	// shape change. Unmeasured.
	aggShapeMoveCycles = 2
	// aggShapeMinSpread is how far apart, as a ratio, the largest and the
	// smallest g of a line's points must be before c is fitted from them.
	// Unmeasured.
	aggShapeMinSpread = 1.5
	// aggLiftCycles is how many consecutive comparable cycles the GPS check
	// must agree before an aggregated figure sizes, and disagree before it
	// stops. Unmeasured.
	aggLiftCycles = 3
	// aggMaxShapePoints is how many shape points a line keeps. Unmeasured.
	aggMaxShapePoints = 6
	// aggSigmaMinReadings is how many prefill readings sigma needs, and
	// aggSigmaMaxReadings how many it keeps. Unmeasured.
	aggSigmaMinReadings = 3
	aggSigmaMaxReadings = 20
)

// The pricing cases of an aggregated line (aggCycle.priceCase): the
// derived-mu line's priceCase, and the replica's why (the /mu: part of
// replica-capacity-summary) when there is no figure.
const (
	// aggCaseCovered: the shape now is at or below the line's own g. The
	// line prices it as it is.
	aggCaseCovered = "shape-covered"
	// aggCaseShapeTerm: the shape now is above the line's g and c is
	// identified. The line prices it as A_g + c*(g* - g).
	aggCaseShapeTerm = "shape-term"
	// aggCaseWorstCase: the shape now is above the line's g by at most
	// aggShapeTolerance and c is not identified. The line prices it at
	// A_g*g*/g, the slope it would have were all of A_g the prefill term:
	// the most c can be, since A_g = A + c*g with A >= 0.
	aggCaseWorstCase = "shape-worst-case"
	// aggCaseUnpriced: the shape now is above the line's g by more than
	// aggShapeTolerance and c is not identified. No figure.
	aggCaseUnpriced = "shape-unpriced"
	// aggCaseLineShapeUnknown: the line carries no g (fitted by a build
	// before the shape term, restored from one, or a held slope that is not
	// the line it published). No figure.
	aggCaseLineShapeUnknown = "line-shape-unknown"
	// aggCaseShapeUnknown: no g could be computed for the shape now (no
	// capacity, prompt or output length). No figure.
	aggCaseShapeUnknown = "shape-unknown"
	// aggCaseOccupancyUnknown: the prefill correction could not be computed
	// from what was read (a prefill reading or a shape that is not a
	// finite number). No figure: an uncorrected rate is not priced.
	aggCaseOccupancyUnknown = "occupancy-unknown"
)

// The reasons an aggregated variant's figure is kept reported only whatever
// its line says (aggState.blocked), each logged once per variant; also
// liftReason values.
const (
	// aggBlockedSGLang: SGLang publishes a completion rate per pod, but its
	// running-request count may leave out requests in chunked prefill, so
	// Little's law would read their prefill as about none and over-state mu.
	// Not verified either way, so not sized from.
	aggBlockedSGLang = "itl-aggregated-sglang-unverified"
	// aggBlockedSpecUnknown: the engine speculates (MTP, a draft model) and
	// no replica reports the tokens a step accepts, so neither the decode
	// steps a request takes nor the tokens a step emits can be read.
	aggBlockedSpecUnknown = "itl-aggregated-spec-unknown"
)

// The liftReason values of the derived-mu line besides the blocks above and
// the no-figure cases (aggLiftReason).
const (
	// aggLiftLifted: the figure sizes.
	aggLiftLifted = "lifted"
	// aggLiftNoPrefill: no prefill reading, so no corrected figure to size.
	aggLiftNoPrefill = "no-prefill-reading"
	// aggLiftShapeChanging: a shape change is outstanding (the fleet's
	// hold); nothing lifts until it ends.
	aggLiftShapeChanging = "shape-changing"
	// aggLiftMovePending: g has moved past the tolerance for fewer than
	// aggShapeMoveCycles cycles.
	aggLiftMovePending = "shape-move-pending"
	// aggLiftDisagreed: the last comparable cycle's GPS check disagreed.
	aggLiftDisagreed = "disagreed"
	// aggLiftChecking: agreeing, but not yet for aggLiftCycles cycles.
	aggLiftChecking = "checking"
)

// aggNoComparableCycle is what the derived-mu line logs for gpsMedianErrPct
// before any comparable cycle, rather than a 0 that reads as a perfect
// match.
const aggNoComparableCycle = "no comparable cycle yet"

// shapePoint is one shape's slope: the g a line was fitted at and its A_g.
type shapePoint struct {
	g, a float64
	at   time.Time
}

// lineShape is what the shape term keeps with a shared line (learnedLine):
// the g and sigma of the fit it holds, and the points c is fitted from.
type lineShape struct {
	// g is the shape the line was fitted at; 0 is unknown.
	g float64
	// sigma is the prefill steps per prompt token measured beside the fit;
	// meaningful only with sigmaOK.
	sigma   float64
	sigmaOK bool
	points  []shapePoint
	// a0 and c are the decode slope and the prefill cost per token fitted
	// from points (fitShapeTerm); meaningful only with cOK.
	a0, c float64
	cOK   bool
}

// clone is s with its own copy of the points.
func (s lineShape) clone() lineShape {
	s.points = slices.Clone(s.points)
	return s
}

// withinShape reports whether g is within aggShapeTolerance of ref.
func withinShape(g, ref float64) bool {
	return math.Abs(g-ref) <= aggShapeTolerance*ref
}

// addPoint adds p, replacing any point within aggShapeTolerance of its g,
// keeps the newest aggMaxShapePoints, and refits c.
func (s *lineShape) addPoint(p shapePoint) {
	kept := make([]shapePoint, 0, len(s.points)+1)
	for _, q := range s.points {
		if !withinShape(p.g, q.g) {
			kept = append(kept, q)
		}
	}
	kept = append(kept, p)
	if len(kept) > aggMaxShapePoints {
		kept = kept[len(kept)-aggMaxShapePoints:]
	}
	s.points = kept
	s.a0, s.c, s.cOK = fitShapeTerm(s.points)
}

// fitShapeTerm fits A_g = a0 + c*g over the points by least squares. ok is
// false with fewer than two points, a largest g under aggShapeMinSpread times
// the smallest, or a fit that is not this physics: a prefill token cannot
// make a step faster (c < 0), and a decode slope is positive (a0 > 0).
func fitShapeTerm(points []shapePoint) (a0, c float64, ok bool) {
	if len(points) < 2 {
		return 0, 0, false
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	var sg, sa float64
	for _, p := range points {
		lo, hi = min(lo, p.g), max(hi, p.g)
		sg += p.g
		sa += p.a
	}
	if !(lo > 0) || hi < aggShapeMinSpread*lo {
		return 0, 0, false
	}
	n := float64(len(points))
	mg, ma := sg/n, sa/n
	var sxx, sxy float64
	for _, p := range points {
		sxx += (p.g - mg) * (p.g - mg)
		sxy += (p.g - mg) * (p.a - ma)
	}
	if !(sxx > 0) {
		return 0, 0, false
	}
	c = sxy / sxx
	a0 = ma - c*mg
	// The comparisons refuse NaN as well; Inf is refused apart.
	if !(c >= 0) || !(a0 > 0) || math.IsInf(c, 0) || math.IsInf(a0, 0) {
		return 0, 0, false
	}
	return a0, c, true
}

// aggState is one aggregated variant's shape state, per ITL window key
// (itlAggregated). Guarded by a.mu.
type aggState struct {
	// windowG is the shape the window's readings were taken at; 0 before
	// the first. pendingG and pendingCycles are a move away from it that has
	// not yet held for aggShapeMoveCycles.
	windowG       float64
	pendingG      float64
	pendingCycles int
	// lineG, lineSigma and lineSigmaOK are the shape of the variant's own
	// last fit, which a held slope keeps.
	lineG       float64
	lineSigma   float64
	lineSigmaOK bool
	// sigmas is the prefill readings taken at windowG, newest last.
	sigmas []float64
	// refused counts the prefill readings refused since the variant was
	// first seen: residence under the output's own steps.
	refused int64
	// specPerStep is the tokens a step accepts beyond the first, the median
	// over the replicas that report it (0 for an engine that does not
	// speculate).
	specPerStep float64
	// blocked is the reason the figure stays reported only whatever the
	// check says (aggBlockedSGLang, aggBlockedSpecUnknown), "" for none;
	// blockedLogged the reasons already logged.
	blocked       string
	blockedLogged map[string]bool
	// agree and disagree count consecutive comparable cycles; lifted is the
	// figure sizing. medErr is the last comparable cycle's median GPS error,
	// medErrOK whether there was one since the window last moved.
	agree, disagree int
	lifted          bool
	medErr          float64
	medErrOK        bool
	// noOccupancyLogged is set once the variant has been logged as having no
	// prefill reading at all.
	noOccupancyLogged bool
}

// sigma is the median of the prefill readings, false with fewer than
// aggSigmaMinReadings.
func (s *aggState) sigma() (float64, bool) {
	if len(s.sigmas) < aggSigmaMinReadings {
		return 0, false
	}
	return medianRate(s.sigmas)
}

// resetLift ends the lift and forgets the check behind it.
func (s *aggState) resetLift() {
	s.agree, s.disagree, s.lifted = 0, 0, false
	s.medErr, s.medErrOK = 0, false
}

// aggCycle is one aggregated variant's shape and price for one cycle,
// resolved in fitLines and read by priceReplicas.
type aggCycle struct {
	key string
	// g is the shape now, 0 when it could not be computed.
	g float64
	// lineG is the g of the line priced: the variant's own, or the lender's.
	lineG float64
	// sigma is the prefill steps per prompt token priced: the window's,
	// else the line's.
	sigma   float64
	sigmaOK bool
	// refused is how many prefill readings the variant has had refused
	// (aggState.refused).
	refused int64
	// specPerStep is aggState.specPerStep: a speculating step emits
	// 1+specPerStep tokens, so a request of OL tokens takes OL/(1+s) decode
	// steps.
	specPerStep float64
	// perStepLine says the line is in step time, not per token: a vLLM
	// engine that speculates with SwitchITLPerToken off.
	perStepLine bool
	c           float64
	cOK         bool
	// priceCase is one of the aggCase constants; figure is whether it
	// gives a figure, and a the slope it prices.
	priceCase string
	figure    bool
	a         float64
	// blocked is aggState.blocked.
	blocked string
	// pending is a shape move that has not yet held for aggShapeMoveCycles
	// (aggState.pendingCycles): the cycle moves no lift.
	pending bool
	// lifted is the variant's lift as the cycle began; agree the agreeing
	// cycles behind it, medErr the last comparable cycle's median GPS error
	// (medErrOK), and liftReason why it is or is not lifted (aggLiftReason).
	lifted     bool
	agree      int
	medErr     float64
	medErrOK   bool
	liftReason string
	// errs is the GPS check's error, per comparable replica, in percent.
	errs []float64
}

// decodeSteps is the decode steps a request of ol tokens takes: one token a
// step, or 1+s a step on an engine that speculates.
func (ac *aggCycle) decodeSteps(ol float64) float64 {
	return ol / (1 + ac.specPerStep)
}

// sizes reports whether the figure prices the replica rather than being
// reported only: lifted, with its prefill reading, unblocked, the lift
// switch on and no shape change outstanding.
func (ac *aggCycle) sizes(sizesOn, shapeChanging bool) bool {
	return sizesOn && ac.lifted && ac.sigmaOK && ac.blocked == "" && !shapeChanging
}

// medErrLogged is gpsMedianErrPct as the derived-mu line logs it: the
// figure, or aggNoComparableCycle before any comparable cycle.
func (ac *aggCycle) medErrLogged() any {
	if !ac.medErrOK {
		return aggNoComparableCycle
	}
	return ac.medErr
}

// aggShapeOn reports whether a variant of this role prices its line by shape.
func (a *SaturationAnalyzer) aggShapeOn(role string) bool {
	return a.itlFitsAggregated && a.itlAggShapeTerm && domain.CanonicalRole(role) == domain.RoleBoth
}

// shapeG is g = C*IL_eff/(KVreq*OL): the prefill tokens a step computes per
// unit of KV fill. 0 when any term is missing or the result is not finite.
func shapeG(capacityTokens, ilEff, kvReq, ol float64) float64 {
	if !(capacityTokens > 0) || !(ilEff > 0) || !(kvReq > 0) || !(ol > 0) {
		return 0
	}
	g := capacityTokens * ilEff / (kvReq * ol)
	if math.IsInf(g, 0) {
		return 0
	}
	return g
}

// promptWeightedHitRate is a variant's prefix-cache hit rate weighted by the
// prompt tokens each replica serves (completion rate times prompt length,
// fleet.WeightedBy): the share of the prompts the variant computes that its
// caches save. A cold replica that has just joined serves little and barely
// moves it, where a median over replicas flips when the cold ones are the
// majority. The plain mean with no weight anywhere; 0 with no reading.
func promptWeightedHitRate(rows []domain.ReplicaMetrics, variant string) float64 {
	return fleet.Mean(rows,
		func(rm domain.ReplicaMetrics) float64 { return rm.PrefixCacheHitRate },
		func(rm domain.ReplicaMetrics) bool { return rm.VariantName == variant && !rm.FromWarmPool },
		fleet.ZeroIsAReading(), fleet.Within(0, 1),
		fleet.WeightedBy(func(rm domain.ReplicaMetrics) float64 { return rm.RequestRate * rm.AvgInputTokens }))
}

// aggregatedShapeNow is the shape an aggregated variant is priced at this
// cycle: g, from the shape mu is priced at (pricingShapes) at the variant's
// prompt-weighted hit rate and its median KV capacity.
func (c *cycle) aggregatedShapeNow(variant string, params *capacity.EngineParams) float64 {
	var caps []float64
	for _, rm := range c.input.ReplicaMetrics {
		if rm.VariantName != variant || rm.FromWarmPool {
			continue
		}
		if ct := whatif.CapacityTokens(params, rm.Ranks(), rm.TotalKvCapacityTokens); ct > 0 {
			caps = append(caps, ct)
		}
	}
	capTokens, ok := medianRate(caps)
	if !ok {
		return 0
	}
	_, muShape, _, _ := c.pricingShapes(promptWeightedHitRate(c.input.ReplicaMetrics, variant))
	return shapeG(capTokens, muShape.ILeff, muShape.KVreq, c.muDivisor)
}

// aggStateLocked is key's shape state, made on first use. Callers hold a.mu.
func (a *SaturationAnalyzer) aggStateLocked(key string) *aggState {
	st, ok := a.itlAggregated[key]
	if !ok {
		st = &aggState{}
		a.itlAggregated[key] = st
	}
	return st
}

// shapeMove is a window drop noteAggregatedShape decided: the shape it left
// and whether the variant was lifted.
type shapeMove struct {
	from      float64
	wasLifted bool
}

// moveShapeLocked files g against key's window shape and reports whether the
// window is to be dropped now. Callers hold a.mu.
func (a *SaturationAnalyzer) moveShapeLocked(key string, g float64) (shapeMove, bool) {
	st := a.aggStateLocked(key)
	from := st.windowG
	switch {
	case !(from > 0):
		st.windowG = g
		return shapeMove{}, false
	case withinShape(g, from):
		st.pendingG, st.pendingCycles = 0, 0
		return shapeMove{}, false
	case st.pendingG > 0 && withinShape(g, st.pendingG):
		st.pendingCycles++
	default:
		st.pendingG, st.pendingCycles = g, 1
	}
	if st.pendingCycles < aggShapeMoveCycles {
		return shapeMove{}, false
	}
	m := shapeMove{from: from, wasLifted: st.lifted}
	st.windowG, st.pendingG, st.pendingCycles = g, 0, 0
	st.sigmas = nil
	st.resetLift()
	return m, true
}

// noteAggregatedShape files the shape now, g, against the window's: the
// first g a window sees becomes its shape, and a g past aggShapeTolerance of
// it for aggShapeMoveCycles consecutive cycles drops the window's readings
// (itl.Tracker.ClearWindow) and its prefill readings, and ends a lift and
// the check behind it, so the next fit is made at the new shape only. The
// intercept and the held slope are kept: B does not depend on shape, and the
// held slope keeps the g it was fitted at.
func (a *SaturationAnalyzer) noteAggregatedShape(key, variant string, g float64, logger logr.Logger) {
	if !(g > 0) {
		return
	}
	m, moved := func() (shapeMove, bool) {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.moveShapeLocked(key, g)
	}()
	if !moved {
		return
	}
	dropped := a.itlTracker.ClearWindow(key)
	logger.V(logging.VERBOSE).Info("itl-aggregated-shape-moved",
		"variant", variant, "key", key, "fromG", m.from, "toG", g, "tolerance", aggShapeTolerance,
		"heldCycles", aggShapeMoveCycles, "windowDropped", dropped, "wasLifted", m.wasLifted,
		"reason", "the request shape moved: the window's readings were taken at another shape, so the next fit is made from new readings; the held slope keeps the shape it was fitted at")
}

// occupancyInput is what noteOccupancy reads: the variant, its rows as the
// engine reports them (step time, not itlRows' per-token reading), its
// engine parameters, the ITL band, and the fleet's lengths and hit rate for
// a row that reports none of its own.
type occupancyInput struct {
	key, variant               string
	rows                       []domain.ReplicaMetrics
	params                     *capacity.EngineParams
	minK, maxK                 float64
	fleetIL, fleetOL, fleetHit float64
}

// occupancyInput is the cycle's occupancyInput for one variant.
func (c *cycle) occupancyInput(key, variant string, params *capacity.EngineParams) occupancyInput {
	lo, hi := c.cfg.ITLBand()
	return occupancyInput{key: key, variant: variant, rows: c.input.ReplicaMetrics, params: params,
		minK: lo, maxK: hi, fleetIL: c.fleetInput, fleetOL: c.fleetOutput, fleetHit: c.fleetHitRate}
}

// occupancyBlock is the reason in's variant takes no prefill reading at
// all: SGLang (aggBlockedSGLang), or "" for none. A speculating engine is
// judged per row (noteOccupancy).
func occupancyBlock(params *capacity.EngineParams) string {
	if params != nil && params.Engine == inferenceengine.EngineSGLang {
		return aggBlockedSGLang
	}
	return ""
}

// occupancyReadings is noteOccupancy's pass over the rows: the sigma
// readings taken, the readings refused, the accepted tokens per step read,
// and how many of the variant's ready rows there were, how many report both
// halves of Little's law, and how many report the accepted tokens.
type occupancyReadings struct {
	taken, specs                   []float64
	refused, own, readable, specOK int
}

// readOccupancy takes one prefill reading per row of in's variant: by
// Little's law a replica holding n sequences and completing lambda a second
// keeps each n/lambda seconds, n/(lambda*ITL) steps, of which OL/(1+s) are
// decode steps (s the tokens a speculating step accepts beyond the first, 0
// on an engine that does not); the rest, per prompt token, is sigma. A
// residence under the decode steps is two near-equal figures' noise and is
// refused (counted), never read as a negative prefill; refusing only that
// side biases sigma up, toward a lower mu. Rows inside the ITL band only,
// ready and the variant's own, with every figure finite.
func readOccupancy(in occupancyInput) occupancyReadings {
	var r occupancyReadings
	speculates := in.params.SpeculatesPerStep()
	for _, rm := range in.rows {
		if rm.VariantName != in.variant || !rm.Ready || rm.FromWarmPool {
			continue
		}
		r.own++
		if rm.RunningRequestsKnown && rm.RequestRateKnown {
			r.readable++
		}
		s := 0.0
		if speculates {
			if !rm.SpecAcceptedPerStepKnown || !(rm.SpecAcceptedPerStep >= 0) || math.IsInf(rm.SpecAcceptedPerStep, 0) {
				continue
			}
			s = rm.SpecAcceptedPerStep
			r.specOK++
			r.specs = append(r.specs, s)
		}
		k := rm.KvUsageForITL()
		if !(k >= in.minK && k <= in.maxK) || !(rm.AvgITL > 0) || math.IsInf(rm.AvgITL, 0) ||
			!rm.RunningRequestsKnown || !rm.RequestRateKnown || !(rm.RequestRate > 0) ||
			math.IsInf(rm.RequestRate, 0) || rm.RunningRequests <= 0 {
			continue
		}
		ol, il, hit := rm.AvgOutputTokens, rm.AvgInputTokens, rm.PrefixCacheHitRate
		// A length that is not a number is a fault, not a length the replica
		// has not read yet: no reading. Zero falls back to the fleet's.
		if math.IsNaN(ol) || math.IsInf(ol, 0) || math.IsNaN(il) || math.IsInf(il, 0) {
			continue
		}
		if !(ol > 0) {
			ol = in.fleetOL
		}
		if !(il > 0) {
			il, hit = in.fleetIL, in.fleetHit
		}
		if !(hit >= 0 && hit <= 1) {
			continue
		}
		ilEff := shape.New(il, ol, hit).ILeff
		if !(ol > 0) || !(ilEff > 0) || math.IsInf(ilEff, 0) {
			continue
		}
		decode := ol / (1 + s)
		steps := float64(rm.RunningRequests) / (rm.RequestRate * rm.AvgITL)
		if !(steps >= 0) || math.IsInf(steps, 0) {
			continue
		}
		if steps < decode {
			r.refused++
			continue
		}
		r.taken = append(r.taken, max((steps-decode)/ilEff, 0))
	}
	return r
}

// noteOccupancy takes in's variant's prefill readings (readOccupancy) into
// its shape state. An SGLang variant, and a speculating one whose replicas
// report no accepted tokens, take none and are blocked (aggState.blocked),
// each logged once.
func (a *SaturationAnalyzer) noteOccupancy(in occupancyInput, logger logr.Logger) {
	if block := occupancyBlock(in.params); block != "" {
		a.blockOccupancy(in, block, logger)
		return
	}
	r := readOccupancy(in)
	if in.params.SpeculatesPerStep() && r.own > 0 && r.specOK == 0 {
		a.blockOccupancy(in, aggBlockedSpecUnknown, logger)
		return
	}
	spec, _ := medianRate(r.specs)
	logNone := func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		st := a.aggStateLocked(in.key)
		st.refused += int64(r.refused)
		st.sigmas = append(st.sigmas, r.taken...)
		if len(st.sigmas) > aggSigmaMaxReadings {
			st.sigmas = slices.Clone(st.sigmas[len(st.sigmas)-aggSigmaMaxReadings:])
		}
		if r.own > 0 {
			st.blocked = ""
		}
		if len(r.specs) > 0 || !in.params.SpeculatesPerStep() {
			st.specPerStep = spec
		}
		none := r.readable == 0 && r.own > 0 && !st.noOccupancyLogged && len(st.sigmas) == 0
		if none {
			st.noOccupancyLogged = true
		}
		return none
	}()
	if logNone {
		logger.V(logging.DEFAULT).Info("itl-aggregated-no-occupancy",
			"variant", in.variant, "key", in.key,
			"reason", "no replica reports both the running requests and the completion rate (vllm:num_requests_running and vllm:request_generation_tokens_count), so the steps a request spends in prefill cannot be read: the variant's derived mu stays reported only")
	}
}

// blockOccupancy files block for in's variant, and logs it the first time.
func (a *SaturationAnalyzer) blockOccupancy(in occupancyInput, block string, logger logr.Logger) {
	first := func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		st := a.aggStateLocked(in.key)
		st.blocked = block
		if st.blockedLogged[block] {
			return false
		}
		if st.blockedLogged == nil {
			st.blockedLogged = map[string]bool{}
		}
		st.blockedLogged[block] = true
		return true
	}()
	if !first {
		return
	}
	reason := "an SGLang variant's running-request count may leave out requests in chunked prefill, so the steps a request spends in prefill would read as about none and over-state mu; not verified, so its derived mu stays reported only"
	if block == aggBlockedSpecUnknown {
		reason = "the engine speculates and no replica reports the tokens a step accepts (vllm:spec_decode_num_accepted_tokens_total), so the decode steps a request takes cannot be read: its derived mu stays reported only"
	}
	logger.V(logging.DEFAULT).Info(block, "variant", in.variant, "key", in.key, "reason", reason)
}

// noteAggregatedFit files the shape of the variant's own line after this
// cycle's fit: a fit made now (not held) is at the window's shape and takes
// the window's sigma; a held slope with no shape -- one restored across a
// restart -- takes the shape of the line it published, when that line is
// still its fit (same slope). It returns the line's shape, and whether the
// fit is a shape point (pointOK).
func (a *SaturationAnalyzer) noteAggregatedFit(key, physicsKey string, r itl.Reading, shapeChanging bool) (g, sigma float64, sigmaOK, pointOK bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.aggStateLocked(key)
	if !r.Model.IsZero() && !r.SlopeHeld {
		st.lineG = st.windowG
		st.lineSigma, st.lineSigmaOK = st.sigma()
		pointOK = st.lineG > 0 && st.lineSigmaOK && !shapeChanging &&
			(r.Tier == itl.TierOLS || (r.Tier == itl.TierPinnedB && r.BaselineLearned))
	} else if r.SlopeHeld && !(st.lineG > 0) {
		if l, ok := a.itlLines[physicsKey]; ok && l.fromVariant == key && l.model.A == r.Model.A && l.shape.g > 0 {
			st.lineG, st.lineSigma, st.lineSigmaOK = l.shape.g, l.shape.sigma, l.shape.sigmaOK
		}
	}
	return st.lineG, st.lineSigma, st.lineSigmaOK, pointOK
}

// shapePublishedLine stamps the line just published under physicsKey with
// its shape, and adds its slope as a shape point when the fit is one.
func (a *SaturationAnalyzer) shapePublishedLine(physicsKey string, g, sigma float64, sigmaOK, point bool,
	slope float64, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, ok := a.itlLines[physicsKey]
	if !ok {
		return
	}
	l.shape = l.shape.clone()
	l.shape.g, l.shape.sigma, l.shape.sigmaOK = g, sigma, sigmaOK
	if point {
		l.shape.addPoint(shapePoint{g: g, a: slope, at: now})
	}
	a.itlLines[physicsKey] = l
}

// lineShapeOf is the shape kept with the shared line under physicsKey.
func (a *SaturationAnalyzer) lineShapeOf(physicsKey string) lineShape {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.itlLines[physicsKey].shape.clone()
}

// aggLine is the line an aggregated variant is priced on this cycle and the
// shape it was fitted at: its own fit, its held slope, or a lender's line
// (whose g and sigma are the lender's, never the borrower's shape now).
type aggLine struct {
	model           itl.Model
	g, sigma        float64
	sigmaOK         bool
	perStep         bool
	shapeChanging   bool
	gNow            float64
	key, physicsKey string
}

// resolveAggregated is one aggregated variant's price for this cycle: its
// line at the shape the line was fitted at, the shape now, the shape term
// kept with the line under physicsKey, and the prefill steps per prompt
// token -- the window's, measured at the shape now, else the line's.
func (a *SaturationAnalyzer) resolveAggregated(l aggLine) *aggCycle {
	term := a.lineShapeOf(l.physicsKey)
	ac, disagree := func() (*aggCycle, int) {
		a.mu.Lock()
		defer a.mu.Unlock()
		st := a.aggStateLocked(l.key)
		ac := &aggCycle{key: l.key, g: l.gNow, lineG: l.g, refused: st.refused, specPerStep: st.specPerStep,
			perStepLine: l.perStep, c: term.c, cOK: term.cOK, blocked: st.blocked,
			lifted: st.lifted, agree: st.agree, medErr: st.medErr, medErrOK: st.medErrOK,
			pending: st.pendingCycles > 0}
		ac.sigma, ac.sigmaOK = st.sigma()
		return ac, st.disagree
	}()
	if !ac.sigmaOK {
		ac.sigma, ac.sigmaOK = l.sigma, l.sigmaOK
	}
	ac.a, ac.priceCase, ac.figure = resolveAggregatedPrice(l.model.A, l.g, l.gNow, term.c, term.cOK)
	ac.liftReason = aggLiftReason(ac, disagree, l.shapeChanging)
	return ac
}

// aggLiftReason says why a variant's figure sizes or does not, for the
// derived-mu line: the block (aggBlockedSGLang, aggBlockedSpecUnknown), the
// price case when there is no figure, or one of the aggLift values.
func aggLiftReason(ac *aggCycle, disagree int, shapeChanging bool) string {
	switch {
	case ac.blocked != "":
		return ac.blocked
	case !ac.figure:
		return ac.priceCase
	case !ac.sigmaOK:
		return aggLiftNoPrefill
	case shapeChanging:
		return aggLiftShapeChanging
	case ac.pending && !ac.lifted:
		return aggLiftMovePending
	case ac.lifted:
		return aggLiftLifted
	case disagree > 0:
		return aggLiftDisagreed
	default:
		return aggLiftChecking
	}
}

// resolveAggregatedPrice is the slope an aggregated line prices the shape
// now at, and which case gave it. It never prices a shape lighter than it
// is (c >= 0 throughout):
//
//   - at or below the line's own g, the line's slope as it is: a line fitted
//     at a higher g states the ITL of a lower one too high;
//   - above it with c identified, A_g + c*(g* - g), however small the move;
//     an over-estimated c then over-provisions and never under-provisions;
//   - above it by at most aggShapeTolerance with no c, A_g*g*/g. With
//     A_g = A + c*g and A >= 0, c is at most A_g/g, so A_g + c*(g* - g) is at
//     most A_g*g*/g: this is the true slope's upper bound;
//   - above it by more with no c, no figure.
func resolveAggregatedPrice(lineA, lineG, g, c float64, cOK bool) (priceA float64, priceCase string, figure bool) {
	switch {
	case !(g > 0) || math.IsInf(g, 0):
		return 0, aggCaseShapeUnknown, false
	case !(lineG > 0) || math.IsInf(lineG, 0):
		return 0, aggCaseLineShapeUnknown, false
	case g <= lineG:
		return lineA, aggCaseCovered, true
	case cOK:
		return lineA + c*(g-lineG), aggCaseShapeTerm, true
	case g <= lineG*(1+aggShapeTolerance):
		return lineA * g / lineG, aggCaseWorstCase, true
	default:
		return 0, aggCaseUnpriced, false
	}
}

// occupancyFactor is D/(D + sigma*IL_eff), D the decode steps a request
// takes: the share of a resident sequence's steps that emit tokens. A
// sequence being prefilled holds KV and emits none, so a rate priced from k
// and ITL(k) alone over-states by the inverse. ok is false when a term is
// not a finite number in range: the rate is then not priced at all, never
// left uncorrected.
func occupancyFactor(decodeSteps, ilEff, sigma float64) (float64, bool) {
	if !(decodeSteps > 0) || math.IsInf(decodeSteps, 0) || !(ilEff >= 0) || math.IsInf(ilEff, 0) ||
		!(sigma >= 0) || math.IsInf(sigma, 0) {
		return 0, false
	}
	return decodeSteps / (decodeSteps + sigma*ilEff), true
}

// gpsErrorPct is the GPS check of an aggregated line with its shape term and
// its occupancy correction, at one replica's own k: the generation tokens a
// second the priced slope predicts there against the replica's own. ok false
// when the replica gives no comparison (as itl.GPSErrorPct).
//
// capacityTokens is the whole replica's KV (whatif.CapacityTokens), every rank
// of a data-parallel pod, as the sequences are counted over it. The line is
// per token on an engine that speculates (itl_per_token.go), so its rate is
// already in tokens; a line in step time (perStepLine) emits 1+s tokens a
// step and is scaled by it.
func gpsErrorPct(ac *aggCycle, b float64, rm domain.ReplicaMetrics, capacityTokens, kvReq, ol, ilEff float64) (float64, bool) {
	k := rm.KvUsageForITL()
	if rm.FromWarmPool || !rm.GenerationTokenRateKnown || !(k >= itl.DefaultGPSMinKForVerification) {
		return 0, false
	}
	occ, ok := occupancyFactor(ac.decodeSteps(ol), ilEff, ac.sigma)
	if !ok {
		return 0, false
	}
	observed := rm.GenerationTokenRate
	pred := itl.TokenRate(itl.Model{A: ac.a, B: b}, k, capacityTokens, kvReq) * occ
	if ac.perStepLine {
		pred *= 1 + ac.specPerStep
	}
	if !(observed > 0) || !(pred > 0) || math.IsInf(observed, 0) {
		return 0, false
	}
	return math.Abs(pred-observed) / observed * 100, true
}

// aggPrice is one replica's derived figure from an aggregated line: the
// figure, the line it was priced from, the occupancy correction, and
// whether it is reported only.
type aggPrice struct {
	derived    whatif.DerivedMu
	model      itl.Model
	occupancy  float64
	reportOnly bool
}

// priceAggregated prices one replica of an aggregated variant from its
// line by shape (ac), records the replica's GPS check in ac, and says
// whether the figure sizes (aggCycle.sizes, never above a measured rate of
// the variant's own) or is reported only.
func (c *cycle) priceAggregated(ac *aggCycle, rm domain.ReplicaMetrics, line itl.Model,
	params *capacity.EngineParams, batchCap int64, fleetShape, muShape shape.Shape, kPrice float64) aggPrice {
	p := aggPrice{model: line, occupancy: 1, reportOnly: true}
	if !ac.figure {
		// No figure for the shape now: nothing is derived, not even to
		// report (resolveAggregatedPrice).
		p.derived = whatif.DerivedMu{Why: ac.priceCase}
		return p
	}
	p.model = itl.Model{A: ac.a, B: line.B}
	p.derived = whatif.DeriveMuAtCap(p.model, params, batchCap, rm.Ranks(), rm.TotalKvCapacityTokens,
		muShape, kPrice, c.muDivisor)
	if p.derived.OK && ac.sigmaOK {
		occ, ok := occupancyFactor(ac.decodeSteps(c.muDivisor), muShape.ILeff, ac.sigma)
		if !ok {
			p.derived = whatif.DerivedMu{Why: aggCaseOccupancyUnknown}
			return p
		}
		p.occupancy = occ
		p.derived.Rate *= occ
		p.derived.TokenSec *= occ
	}
	if ac.sigmaOK && !rm.FromWarmPool {
		if errPct, ok := gpsErrorPct(ac, line.B, rm, whatif.CapacityTokens(params, rm.Ranks(), rm.TotalKvCapacityTokens),
			fleetShape.KVreq, fleetShape.AvgOutputTokens, fleetShape.ILeff); ok {
			ac.errs = append(ac.errs, errPct)
		}
	}
	if ac.sizes(c.a.itlAggSizes, c.shapeChanging) {
		p.reportOnly = false
		p.derived.CapAtMeasured = true
	}
	return p
}

// noteAggregatedLift moves one variant's lift on this cycle's GPS check: its
// median comparable replica within itl.DefaultGPSMismatchThresholdPct on
// aggLiftCycles consecutive comparable cycles lifts it, outside on as many
// drops it. A cycle with no figure, no sigma, a block, or a shape change
// outstanding ends the lift and the counts; a cycle with no comparable
// replica, or with a shape move that has not yet held (a scale-up's cold
// replicas moving the hit rate for a cycle), moves nothing: the move ends
// the lift when it holds (noteAggregatedShape).
func (a *SaturationAnalyzer) noteAggregatedLift(variant string, ac *aggCycle, shapeChanging bool, logger logr.Logger) {
	type lift struct {
		was, now        bool
		med             float64
		agree, disagree int
	}
	l := func() lift {
		a.mu.Lock()
		defer a.mu.Unlock()
		st := a.aggStateLocked(ac.key)
		l := lift{was: st.lifted}
		switch {
		case ac.pending:
		case !ac.figure || !ac.sigmaOK || ac.blocked != "" || shapeChanging:
			st.agree, st.disagree, st.lifted = 0, 0, false
		case len(ac.errs) == 0:
		default:
			l.med, _ = medianRate(ac.errs)
			st.medErr, st.medErrOK = l.med, true
			if l.med <= itl.DefaultGPSMismatchThresholdPct {
				st.agree, st.disagree = st.agree+1, 0
				if st.agree >= aggLiftCycles {
					st.lifted = true
				}
			} else {
				st.agree, st.disagree = 0, st.disagree+1
				if st.disagree >= aggLiftCycles {
					st.lifted = false
				}
			}
		}
		l.now, l.agree, l.disagree = st.lifted, st.agree, st.disagree
		return l
	}()
	if l.now == l.was {
		return
	}
	reason := "the GPS check agreed with the figure on consecutive cycles: it sizes"
	if !l.now {
		reason = "the figure is reported only again: the GPS check disagreed on consecutive cycles, the shape moved, or the figure or its prefill reading is missing"
	}
	logger.V(logging.DEFAULT).Info("itl-aggregated-lift",
		"variant", variant, "key", ac.key, "lifted", l.now, "medianErrPct", l.med,
		"thresholdPct", itl.DefaultGPSMismatchThresholdPct, "agree", l.agree, "disagree", l.disagree,
		"cycles", aggLiftCycles, "priceCase", ac.priceCase, "sigmaOK", ac.sigmaOK, "shapeChanging", shapeChanging,
		"reason", reason)
}
