package whatifapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// defaultSizeBound is the most replicas of a role POST /v1/size tries when
// the request sets no bound: a search past it is more likely a rate typed
// wrongly than a fleet anyone runs. A request may raise it to maxReplicas.
const defaultSizeBound = 32

// ReasonReplicaBound is the refusal of a size whose rate no fleet within the
// bound reaches, though every figure is answered: more replicas than the
// bound, or a smaller rate.
const ReasonReplicaBound = "ReplicaBoundReached"

// What a size's fleet must reach the rate with (SizeRequest.SizeOn).
const (
	// SizeOnPoint is the fleet's limit: the default.
	SizeOnPoint = "point"
	// SizeOnLow is the low end of the limit's span over V_P and f: the fleet
	// serves the rate at every corner the span sweeps.
	SizeOnLow = "low"
)

// The roles a size's refusal names: the role no count within the bound
// serves, or the fleet, when each role serves the rate alone but the two
// together do not.
const (
	roleDecode  = "decode"
	rolePrefill = "prefill"
	roleFleet   = "fleet"
)

// reachTolerance is the share under the rate a limit may fall and still
// reach it: a limit is a bisection's answer, and its last bits are no
// shortfall.
const reachTolerance = 1e-9

// reaches reports whether a limit of x r/s reaches rate, within
// reachTolerance.
func reaches(x, rate float64) bool { return x >= rate*(1-reachTolerance) }

// errNotMonotone is a search whose answer a fleet with one replica fewer
// also serves, or whose plan does not serve the rate: the limits did not
// rise with the replicas as the search takes them to. It is the server's
// error, not the caller's.
var errNotMonotone = errors.New("the size's search does not hold on this fleet")

// handleSize is POST /v1/size: the fleet of the fewest GPUs (or replicas,
// where GPUs are not stated) whose fleet limit reaches a rate, or why none
// within the bound does.
func (s *Server) handleSize(w http.ResponseWriter, r *http.Request) {
	var req SizeRequest
	if !s.readLongJSON(w, r, &req, maxBody) {
		return
	}
	body, err := s.size(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// planAt is the plan a size request asks of one fleet.
func (req SizeRequest) planAt(n FleetCounts) PlanRequest {
	return PlanRequest{InputsID: req.InputsID, Fleet: &FleetKnob{Prefill: &n.Prefill, Decode: &n.Decode},
		Traffic: req.Traffic, SLO: req.SLO, Assume: req.Assume, Headroom: req.Headroom}
}

// sizeBound is the request's bound per role, each defaultSizeBound where
// left out, checked against maxReplicas.
func sizeBound(k *FleetKnob) (FleetCounts, error) {
	out := FleetCounts{Prefill: defaultSizeBound, Decode: defaultSizeBound}
	if k == nil {
		return out, nil
	}
	for _, c := range []struct {
		role string
		n    *int
		into *int
	}{{rolePrefill, k.Prefill, &out.Prefill}, {roleDecode, k.Decode, &out.Decode}} {
		if c.n == nil {
			continue
		}
		if *c.n < 1 || *c.n > maxReplicas {
			return out, InputErrorf("maxReplicas.%s %d, want 1 to %d replicas", c.role, *c.n, maxReplicas)
		}
		*c.into = *c.n
	}
	return out, nil
}

// sizeOn is the request's SizeOn, checked: "point" where left out.
func sizeOn(s string) (string, error) {
	switch s {
	case "", SizeOnPoint:
		return SizeOnPoint, nil
	case SizeOnLow:
		return SizeOnLow, nil
	}
	return "", InputErrorf("sizeOn %q, want %s (the fleet's limit) or %s (the low end of its span over V_P and f)", s, SizeOnPoint, SizeOnLow)
}

// size sizes req and returns the response body: from the cache when the
// same question was sized before. A size not cached is sized once however
// many ask for it at once, on one plan slot (errBusy with none free) held
// from its search through its answer's plan, which is a plan like any other,
// cached as one. The search is kept by sizeId before the plan is priced, so
// a size whose plan failed (its client gone) is not searched again.
func (s *Server) size(ctx context.Context, req SizeRequest) ([]byte, error) {
	// Refused before the inputs are looked up: no inputs make them right.
	if req.BucketEdges != nil {
		return nil, InputErrorf("bucketEdges: a size prices the mix line alone, and the edges shape only a plan's " +
			"\"if all traffic were bucket b\" view; ask them of POST /v1/plan")
	}
	e, ok := s.inputs.get(req.InputsID)
	if !ok {
		return nil, errUnknownInputs
	}
	if e.doc.Fleet.Prefill == nil {
		return nil, InputErrorf("the inputs have no prefill variant: POST /v1/size sizes a P/D fleet")
	}
	bound, err := sizeBound(req.MaxReplicas)
	if err != nil {
		return nil, err
	}
	on, err := sizeOn(req.SizeOn)
	if err != nil {
		return nil, err
	}
	spare := 0
	if req.Spare != nil {
		if spare = *req.Spare; spare < 0 || spare > MaxSpare {
			return nil, InputErrorf("spare %d, want 0 to %d replicas of each role", spare, MaxSpare)
		}
	}
	sc, err := scenarioOf(&e.doc, req.planAt(FleetCounts{Prefill: 1, Decode: 1}))
	if err != nil {
		return nil, err
	}
	if sc.Concurrency != nil {
		return nil, InputErrorf("traffic.classes.critical.concurrency: a size needs a rate; a concurrency is a rate only on a given fleet, " +
			"so translate it with POST /v1/plan on a fleet and size for that rate")
	}
	if sc.Rate == nil {
		return nil, InputErrorf("traffic.classes.critical.rate: give the rate to size the fleet for")
	}
	k, err := sc.knobs()
	if err != nil {
		return nil, err
	}
	// Spare hashes only when asked, so that a size with none keeps the id it
	// had before spares were asked of it.
	id := contentID("size", struct {
		Scenario Scenario
		Bound    FleetCounts
		On       string
		Spare    int `json:",omitempty"`
	}{sc, bound, on, spare})
	// The search does not depend on the spare: it is kept under the id of
	// the same size with none, so that asking for a spare searches once.
	searchID := contentID("size", struct {
		Scenario Scenario
		Bound    FleetCounts
		On       string
	}{sc, bound, on})
	if b, ok := s.sizes.get(id); ok {
		return b, nil
	}
	return s.sizeFlight.doFor(ctx, id, func() ([]byte, error) {
		if b, ok := s.sizes.get(id); ok {
			return b, nil
		}
		if !acquire(s.planSlots) {
			return nil, errBusy
		}
		defer func() { <-s.planSlots }()
		found, ok := s.searches.get(searchID)
		if !ok {
			if s.onSearch != nil {
				s.onSearch()
			}
			z := &sizer{e: e, sc: sc, k: k, rate: *sc.Rate, bound: bound, low: on == SizeOnLow, models: e.models(),
				seen: map[FleetCounts]sizeEval{}, plain: map[FleetCounts]sizeEval{}}
			if found, err = z.search(ctx); err != nil {
				return nil, err
			}
			found = s.searches.add(searchID, found)
		}
		out := *found
		if found.Answer != nil {
			a := *found.Answer
			out.Answer = &a
			if err := s.planAnswer(ctx, req, *sc.Rate, on == SizeOnLow, out.Answer); err != nil {
				return nil, err
			}
			if g := e.doc.Fleet.GPUsPerReplica(); g.Prefill > 0 && g.Decode > 0 {
				a.GPUsPerReplica, a.GPUs = &g, FleetGPUs(a.Fleet, g)
			}
			a.WithSpare = WithSpare(a.Fleet, spare, e.doc.Fleet.GPUsPerReplica())
		}
		out.SchemaVersion, out.WhatifSchema, out.SizeID, out.InputsID, out.Banner = SchemaVersion, s.whatifSchema, id, e.id, banner
		out.Rate, out.MaxReplicas, out.SizeOn, out.Spare = *sc.Rate, bound, on, spare
		_, out.Minimizes = ReplicaCost(e.doc.Fleet.GPUsPerReplica())
		body, err := marshal(out)
		if err != nil {
			return nil, err
		}
		return s.sizes.add(id, body), nil
	})
}

// planAnswer prices the plan at the answer's fleet, as POST /v1/plan does,
// on the slot the size holds, and reads the answer's limit, headroom and
// binding from it, so that the answer is what a plan of that fleet says. A
// plan that does not reach the rate the search found it reaches is the
// server's error, not a refusal.
func (s *Server) planAnswer(ctx context.Context, req SizeRequest, rate float64, low bool, a *SizeAnswer) error {
	body, err := s.planHolding(ctx, req.planAt(a.Fleet), true)
	if err != nil {
		return err
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&a.Plan); err != nil {
		return err
	}
	l := a.Plan.Limits.Fleet
	if !figureReaches(l.Figure, rate, low) {
		return fmt.Errorf("%w: the plan at %d prefill and %d decode replicas does not reach %g r/s, though the search found it does: %s",
			errNotMonotone, a.Fleet.Prefill, a.Fleet.Decode, rate, figurePlain(l.Figure))
	}
	// Sized on the low end, the headroom is the low end's, as the one-fewer
	// steps' shortfalls are: one basis for every figure the size compares
	// to the rate.
	x := *l.Value
	if low && l.Sensitivity != nil && l.Sensitivity.Lo != nil {
		x = *l.Sensitivity.Lo
	}
	a.Limit, a.Binding = l.Figure, l.Binding
	a.Headroom = Figure{Value: ptr(x - rate), Unit: unitRate}
	return nil
}

// figureReaches reports whether a limit on the wire reaches rate: its value,
// and with low its span's low end too (a refused corner does not).
func figureReaches(f Figure, rate float64, low bool) bool {
	if f.Value == nil || !reaches(*f.Value, rate) {
		return false
	}
	if !low || f.Sensitivity == nil {
		return true
	}
	return f.Sensitivity.Lo != nil && reaches(*f.Sensitivity.Lo, rate)
}

// resultReaches is figureReaches on a priced limit.
func resultReaches(r whatif.Result, rate float64, low bool) bool {
	a, ok := r.(whatif.Answer)
	if !ok || !reaches(a.Value, rate) {
		return false
	}
	if !low || a.Sensitivity == nil {
		return true
	}
	return a.Sensitivity.Refused == "" && reaches(a.Sensitivity.Lo, rate)
}

// figurePlain is a figure in words, for an error.
func figurePlain(f Figure) string {
	if f.Refused != nil {
		return "refused " + f.Refused.Reason
	}
	if f.Value == nil {
		return "not priced"
	}
	return fmt.Sprintf("%.4g r/s", *f.Value)
}

// sizer searches one size question's replica counts.
type sizer struct {
	e     *inputsEntry
	sc    Scenario
	k     Knobs
	rate  float64
	bound FleetCounts
	// low sizes on the low end of each limit's span over V_P and f.
	low bool
	// models is the experimental models (TTFT and TPOT) the search prices
	// with: the inputs' for a size, and for a day plan asked for them
	// (DayPlanRequest.ExperimentalModels); nil for the planner's own.
	models *experimentalModels
	// seen is every fleet priced, by its counts; plain, with low, every
	// fleet priced for decode's own limit alone, without the span.
	seen, plain map[FleetCounts]sizeEval
}

// sizeEval is one fleet's limits at the scenario's targets and headroom:
// decode's (nil with no TPOT asked, but decode's knee's refusal where it is
// refused, and then the fleet's too), the prefill role's, and the fleet's.
type sizeEval struct {
	decode, prefill, fleet whatif.Result
	// extrapolated says the fleet limit is one the experimental TTFT or TPOT
	// model set past the highest load served (experimentalModels.markLimit).
	extrapolated bool
}

// at prices the fleet of n, once per search, stopping with ctx's error once
// ctx ends. Its limits are the plan's +1 steps' (PlainLimits, no span), or
// with low the plan's own limits (Limits), whose fleet limit carries the
// span the low end is read from.
func (z *sizer) at(ctx context.Context, n FleetCounts) (sizeEval, error) {
	return z.priced(ctx, z.seen, n, z.low)
}

// atDecode prices the fleet of n for decode's own limit, which has no span
// over V_P and f: with no span, a fifth of the cost, whatever the size is on.
func (z *sizer) atDecode(ctx context.Context, n FleetCounts) (sizeEval, error) {
	if !z.low {
		return z.at(ctx, n)
	}
	return z.priced(ctx, z.plain, n, false)
}

// cost is what the search weighs a replica of each role by, and the
// objective that makes (ReplicaCost): the inputs' GPUs per replica.
func (z *sizer) cost() (FleetCounts, string) {
	return ReplicaCost(z.e.doc.Fleet.GPUsPerReplica())
}

// searched is how many fleets the search priced.
func (z *sizer) searched() int { return len(z.seen) + len(z.plain) }

// priced is the fleet of n priced into cache, with its span or without.
func (z *sizer) priced(ctx context.Context, cache map[FleetCounts]sizeEval, n FleetCounts, span bool) (sizeEval, error) {
	if v, ok := cache[n]; ok {
		return v, nil
	}
	if err := ctx.Err(); err != nil {
		return sizeEval{}, err
	}
	k := z.k
	k.Prefill, k.Decode = n.Prefill, n.Decode
	fl, err := z.e.pricer.Fleet(k)
	if err != nil {
		return sizeEval{}, err
	}
	fl = z.models.fleet(fl, k)
	tpot := 0.0
	if z.sc.TPOT != nil {
		tpot = z.sc.TPOT.Target
	}
	var v sizeEval
	if span {
		ls := fl.Limits(z.sc.TTFT.Target, tpot, z.sc.Headroom)
		v = sizeEval{decode: ls.Decode, prefill: ls.Prefill, fleet: ls.Fleet}
	} else {
		v.decode, v.prefill, v.fleet = fl.PlainLimits(z.sc.TTFT.Target, tpot, z.sc.Headroom)
	}
	v.extrapolated = z.models.markLimit(fl, k, figureOf(v.fleet, unitRate, false)).Extrapolated
	cache[n] = v
	return v, nil
}

// meets reports whether the fleet's limit reaches the rate.
func (z *sizer) meets(v sizeEval) bool { return resultReaches(v.fleet, z.rate, z.low) }

// decodeMeets reports whether decode's own limit, TPOT and the
// operating-point cap on decode's knee, reaches the rate. With no TPOT asked
// decode has no limit of its own and the fleet's says the rest, but a refused
// knee meets no rate (priced). Decode's limit has no span over V_P and f, the
// prefill side's terms.
func (z *sizer) decodeMeets(v sizeEval) bool {
	return v.decode == nil || resultReaches(v.decode, z.rate, false)
}

// step is the fleet of n as a size's step: its limit, the headroom over the
// rate of what the size is on (the limit, or with low its span's low end),
// and the role that binds.
func (z *sizer) step(ctx context.Context, n FleetCounts) (*SizeStep, error) {
	v, err := z.at(ctx, n)
	if err != nil {
		return nil, err
	}
	st := &SizeStep{Fleet: n, Limit: figureOf(v.fleet, unitRate, false), Binding: binding(v.prefill, v.decode)}
	st.Limit.Extrapolated = v.extrapolated
	x, ok := valueOf(v.fleet)
	if a, isAnswer := v.fleet.(whatif.Answer); isAnswer && z.low && a.Sensitivity != nil {
		if a.Sensitivity.Refused != "" {
			st.Headroom = Figure{Refused: &Refusal{Reason: string(a.Sensitivity.Refused), Detail: "refused at the worst corner of V_P and f"}}
			return st, nil
		}
		x = a.Sensitivity.Lo
	}
	switch {
	case ok:
		st.Headroom = Figure{Value: ptr(x - z.rate), Unit: unitRate}
	case st.Limit.Refused != nil:
		st.Headroom = refusedFigure(st.Limit.Refused)
	default:
		// A fleet limit neither answered nor refused is not one the engine
		// returns; say so rather than panic.
		st.Headroom = Figure{Refused: &Refusal{Reason: "NotPriced", Detail: "the fleet's limit was not priced"}}
	}
	return st, nil
}

// smallest is the smallest n in [lo, hi] for which ok holds, and whether
// one does. It gallops (lo, lo+1, lo+3, lo+7, ... up to hi) to the first
// count that holds, then bisects back to the count after the last that
// failed, so its cost grows with the answer, not with hi. It takes ok to be
// monotone, as a limit is in replicas: where it is not, a count that holds
// below the answer can be skipped, but never the one just below it, which is
// always asked and failed.
func smallest(lo, hi int, ok func(int) (bool, error)) (int, bool, error) {
	if lo > hi {
		return 0, false, nil
	}
	failed, n, stride := lo-1, lo, 1
	for {
		held, err := ok(n)
		if err != nil {
			return 0, false, err
		}
		if held {
			break
		}
		if n == hi {
			return 0, false, nil
		}
		failed, n, stride = n, min(hi, n+stride), 2*stride
	}
	lo, hi = failed+1, n
	for lo < hi {
		mid := lo + (hi-lo)/2
		held, err := ok(mid)
		if err != nil {
			return 0, false, err
		}
		if held {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return hi, true, nil
}

// search is the size, its plan aside: SmallestFleet on the fleets' limits
// at the scenario's targets. A fleet with one replica fewer of either role
// that also serves the rate is errNotMonotone.
func (z *sizer) search(ctx context.Context) (*SizeResponse, error) {
	cost, _ := z.cost()
	n, unmet, err := SmallestFleet(z.bound, cost, func(n FleetCounts) (bool, error) {
		v, err := z.atDecode(ctx, n)
		return err == nil && z.decodeMeets(v), err
	}, func(n FleetCounts) (bool, error) {
		v, err := z.at(ctx, n)
		return err == nil && z.meets(v), err
	})
	switch {
	case err != nil:
		return nil, err
	case unmet != "":
		return z.unmet(ctx, unmet, z.bound)
	}
	p, d := n.Prefill, n.Decode
	a := &SizeAnswer{Fleet: FleetCounts{Prefill: p, Decode: d}}
	for _, f := range []struct {
		into **SizeStep
		n    FleetCounts
	}{{&a.FewerPrefill, FleetCounts{Prefill: p - 1, Decode: d}}, {&a.FewerDecode, FleetCounts{Prefill: p, Decode: d - 1}}} {
		if f.n.Prefill < 1 || f.n.Decode < 1 {
			continue
		}
		if *f.into, err = z.step(ctx, f.n); err != nil {
			return nil, err
		}
		if z.meets(z.seen[f.n]) {
			return nil, fmt.Errorf("%w: %d prefill and %d decode replicas, one fewer than the answer %d and %d, also serve %g r/s",
				errNotMonotone, f.n.Prefill, f.n.Decode, p, d, z.rate)
		}
	}
	return &SizeResponse{Answer: a, Searched: z.searched()}, nil
}

// The objectives a size's search minimises (SizeResponse.Minimizes).
const (
	// MinimizesGPUs is the fewest GPUs: each role's replicas times its GPUs
	// per replica, as the source states them.
	MinimizesGPUs = "gpus"
	// MinimizesReplicas is the fewest replicas in all, where the source does
	// not state both roles' GPUs per replica.
	MinimizesReplicas = "replicas"
)

// ReplicaCost is what a size's search weighs one replica of each role by:
// its GPUs per replica, prefill and decode, when both are stated (above 0),
// else 1 each, which counts replicas. The objective it minimises is
// MinimizesGPUs with GPUs stated, else MinimizesReplicas.
func ReplicaCost(gpusPerReplica FleetCounts) (FleetCounts, string) {
	if gpusPerReplica.Prefill > 0 && gpusPerReplica.Decode > 0 {
		return gpusPerReplica, MinimizesGPUs
	}
	return FleetCounts{Prefill: 1, Decode: 1}, MinimizesReplicas
}

// Cost is n's replicas weighed by w, a role's weight below 1 counting as 1:
// with w a role's GPUs per replica, the fleet's GPUs.
func (n FleetCounts) Cost(w FleetCounts) int {
	return n.Prefill*max(w.Prefill, 1) + n.Decode*max(w.Decode, 1)
}

// cheaper reports whether fleet a ranks before b in a size's search: the
// lower cost under w, then the fewer replicas in all, then the fewer decode
// replicas. With every weight 1 it is the fewer replicas, then the fewer
// decode, the order the search took before it weighed GPUs.
func cheaper(a, b, w FleetCounts) bool {
	if ca, cb := a.Cost(w), b.Cost(w); ca != cb {
		return ca < cb
	}
	if ta, tb := a.Prefill+a.Decode, b.Prefill+b.Decode; ta != tb {
		return ta < tb
	}
	return a.Decode < b.Decode
}

// SmallestFleet is the search POST /v1/size runs, on any test of a fleet:
// the fleet that holds at the least cost, cost being each role's replicas
// times its weight in cost (Cost: a role's GPUs per replica, or 1 each to
// count replicas, ReplicaCost), ties going to the fewer replicas in all,
// then to the fewer decode replicas. With 8-GPU prefill and 1-GPU decode
// replicas, 8 prefill + 5 decode (69 GPUs) is taken over 9 + 4 (76 GPUs);
// with the roles' weights equal it is the fewest replicas in all.
//
//  1. decode's floor, on decodeHolds, its own limit, which the prefill count
//     does not move: tried at one prefill replica. No fleet with fewer
//     decode replicas holds, since holds asks decode's limit too.
//  2. from that floor up, the fewest prefill replicas that hold at each
//     decode count: never more than at the count before, so each search is
//     bounded by the last. At a decode count, the fewest prefill replicas
//     are the cheapest fleet on every key. It stops once one prefill
//     replica beside the decode count ranks no better than the best found
//     (every fleet with more decode replicas is at least that on each key,
//     so none ranks better), or one prefill replica holds.
//
// Taking decode's floor and then the prefill that joins it is not enough:
// where decode admission sets most of a TTFT, more prefill replicas buy
// little of it, and the floor's prefill count can be several times what
// one more decode replica needs. The cheapest in all is also what makes the
// answer's cost rise with the rate, as the set of fleets that hold shrinks.
//
// With none within the bound, unmet names the role that fell short:
// "decode" for decode's own limit, "fleet" for the two together. The tests
// are taken to be monotone in each count, as a limit is.
func SmallestFleet(bound, cost FleetCounts, decodeHolds, holds func(FleetCounts) (bool, error)) (n FleetCounts, unmet string, err error) {
	maxP, maxD := bound.Prefill, bound.Decode
	d0, ok, err := smallest(1, maxD, func(d int) (bool, error) { return decodeHolds(FleetCounts{Prefill: 1, Decode: d}) })
	if err != nil || !ok {
		return n, roleDecode, err
	}
	found, pMax := false, maxP
	for d := d0; d <= maxD; d++ {
		if found && !cheaper(FleetCounts{Prefill: 1, Decode: d}, n, cost) {
			break
		}
		p, ok, err := smallest(1, pMax, func(p int) (bool, error) { return holds(FleetCounts{Prefill: p, Decode: d}) })
		if err != nil {
			return n, "", err
		}
		if !ok {
			continue
		}
		if c := (FleetCounts{Prefill: p, Decode: d}); !found || cheaper(c, n, cost) {
			n, found = c, true
		}
		if p == 1 {
			break
		}
		pMax = p
	}
	if !found {
		return n, roleFleet, nil
	}
	return n, "", nil
}

// SmallestCount is the smallest n in [lo, hi] for which ok holds, and
// whether one does, by the search SmallestFleet runs on each role.
func SmallestCount(lo, hi int, ok func(int) (bool, error)) (int, bool, error) {
	return smallest(lo, hi, ok)
}

// DefaultSizeBound is the most replicas of a role a size tries unless asked
// for more.
const DefaultSizeBound = defaultSizeBound

// MaxSpare is the most spare replicas of each role a size or a report adds
// to its fleet: the most replicas of a role a scenario prices.
const MaxSpare = maxReplicas

// unmet is the size refused at the bound fleet n: the refusal of the role's
// figure there, when the model refuses it (a term refused, a target under
// the floor), else ReasonReplicaBound with the figure it reached. A fleet
// that falls short where decode alone does not is named by the prefill
// role when its own limit is short.
func (z *sizer) unmet(ctx context.Context, role string, n FleetCounts) (*SizeResponse, error) {
	st, err := z.step(ctx, n)
	if err != nil {
		return nil, err
	}
	v := z.seen[n]
	r, low := v.fleet, z.low
	switch {
	case role == roleDecode:
		r, low = v.decode, false
	case !resultReaches(v.prefill, z.rate, z.low):
		// The prefill role's own limit is short, or refused: name it.
		role, r = rolePrefill, v.prefill
	}
	u := &SizeUnmet{Role: role, AtBound: *st}
	out := &SizeResponse{Unmet: u, Searched: z.searched()}
	if fig := figureOf(r, unitRate, false); fig.Refused != nil {
		u.Refused = *fig.Refused
		return out, nil
	}
	x, _ := valueOf(r)
	what := map[string]string{roleDecode: "decode's own limit (TPOT and the operating-point cap)",
		rolePrefill: "the prefill role's limit (TTFT)", roleFleet: "the fleet's limit"}[role]
	if a, ok := r.(whatif.Answer); ok && low && a.Sensitivity != nil {
		if reason := a.Sensitivity.Refused; reason != "" {
			// The worst corner refuses the figure, at any count: no bound
			// reached, and more replicas do not help.
			u.Refused = Refusal{Reason: string(reason), Detail: lowRefusedDetail(n, what, reason, z.floorOf(v.prefill))}
			return out, nil
		}
		x, what = a.Sensitivity.Lo, what+" at its worst corner of V_P and f"
	}
	advice := fmt.Sprintf("raise maxReplicas (up to %d) or lower the rate", maxReplicas)
	if n.Prefill >= maxReplicas && n.Decode >= maxReplicas {
		advice = fmt.Sprintf("%d replicas a role is the most the API sizes: lower the rate or loosen the targets", maxReplicas)
	}
	u.Refused = Refusal{Reason: ReasonReplicaBound, Detail: fmt.Sprintf(
		"At %d prefill and %d decode replicas, the bound searched, %s is %.4g r/s, short of %.4g r/s: %s",
		n.Prefill, n.Decode, what, x, z.rate, advice)}
	return out, nil
}

// floorOf is the TTFT floor a prefill limit rests on, at the learned V_P and
// f; 0 where it carries none.
func (z *sizer) floorOf(prefill whatif.Result) float64 {
	if a, ok := prefill.(whatif.Answer); ok && a.Basis.TTFT != nil {
		return a.Basis.TTFT.Floor
	}
	return 0
}

// lowRefusedDetail is a size on the low end refused at the worst corner of
// V_P and f: which figure, why, and, under a floor, that the target must
// rise above it, since no replica count lowers a floor.
func lowRefusedDetail(n FleetCounts, what string, reason whatif.Reason, floor float64) string {
	d := fmt.Sprintf("At the worst corner of V_P and f (each %.0f%% worse than learned), %s is refused (%s) at %d prefill and %d decode replicas",
		100*whatif.SensitivityShare, what, reason, n.Prefill, n.Decode)
	if reason != whatif.TargetBelowFloor {
		return d + ": size on the point instead, or learn V_P and f"
	}
	d += ": the TTFT target is under that corner's floor"
	if floor > 0 {
		d += fmt.Sprintf(" (at the learned V_P and f the floor is %.4g s, and the corner's is higher)", floor)
	}
	return d + ". No replica count lowers a floor: raise the TTFT target above it, or size on the point"
}
