package whatifapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// POST /v1/report answers what one fleet gives: the load the source saw, the
// utilization there, the capacity, the SLO against load and a verdict, for
// each request shape asked and at each load asked (capacity-planner
// at-replicas prints the same report). The body is built by the Pricer an
// inputsId names, which must implement Reporter.

// ReportRequest is POST /v1/report's body. Every field but InputsID
// defaults: the recorded replicas, the measured shape, 50%, 80% and 100% of
// the knee and the load seen, the policy's SLO and headroom, TTFT at p50 and
// p90 and TPOT at the mean and p90.
type ReportRequest struct {
	InputsID string     `json:"inputsId"`
	Fleet    *FleetKnob `json:"fleet,omitempty"`
	// Shapes are the request shapes to answer for: "measured", or
	// "IL/OL[/hit]" with lengths in tokens, a k suffix for 1024 ("8k/200",
	// "1k/4k/0.3"). Absent, the measured shape.
	Shapes []string `json:"shapes,omitempty"`
	// Loads are the table's rates: "5r/s", "80%" of the shape's fleet knee,
	// "seen", or the load seen grown, "seen x1.5" (also "seen×1.5",
	// "seen*1.5"): a what-if with its own column and its own verdict, which
	// never replaces the verdict at the load seen. Absent, 50%, 80%, 100% and
	// seen.
	Loads []string `json:"loads,omitempty"`
	// LoadStatistic is the statistic of the source's per-cycle arrival rate
	// the fleet is judged at: "mean", "p95" or "peak"; absent, p95.
	LoadStatistic string `json:"loadStatistic,omitempty"`
	// LoadSeen is the load to judge at in place of the source's, requests per
	// second; absent, the source's rate at LoadStatistic.
	LoadSeen *float64 `json:"loadSeen,omitempty"`
	// LoadSeenX is an alias that adds the load "seen xN" to Loads: a growth
	// what-if, not the load judged at.
	LoadSeenX *float64    `json:"loadSeenX,omitempty"`
	Headroom  *float64    `json:"headroom,omitempty"`
	SLO       *SLORequest `json:"slo,omitempty"`
	// ArrivalCV2 is the arrivals' squared coefficient of variation, Ca^2;
	// absent, Poisson (1), as for a plan's traffic.arrivalCV2.
	ArrivalCV2 *float64 `json:"arrivalCV2,omitempty"`
	// TTFTStatistics are the TTFT statistics the tables print, "mean" or
	// "pNN"; absent, p50 and p90.
	TTFTStatistics []string `json:"ttftStatistics,omitempty"`
	// TPOTStatistic is the TPOT percentile the tables print beside the mean,
	// "pNN", or "mean" for the mean alone; absent, p90.
	TPOTStatistic string  `json:"tpotStatistic,omitempty"`
	Assume        *Assume `json:"assume,omitempty"`
}

// ReportQuery is a ReportRequest checked and read into numbers: the question
// a Reporter answers. A zero field is its default.
type ReportQuery struct {
	// Prefill and Decode are the replica counts; 0 is the recorded count.
	Prefill, Decode int
	// Shapes are the shapes to answer for; none is the measured one.
	Shapes []ReportShapeAsk
	// Loads are the table's rates; none is 50%, 80%, 100% and seen.
	Loads []ReportLoadAsk
	// LoadStatistic is the statistic of the source's per-cycle arrival rate
	// judged at (LoadMean, LoadP95, LoadPeak); "" is DefaultLoadStatistic.
	LoadStatistic string
	// LoadSeen is the load to judge at; 0 is the source's. LoadSeenX, when
	// above 0 and not 1, adds the growth load "seen xLoadSeenX" to Loads.
	LoadSeen, LoadSeenX float64
	// Headroom is the operating point's fraction of the fleet knee; 0 is the
	// policy's.
	Headroom float64
	// TTFT and TPOT are the SLO targets; nil is the policy's.
	TTFT, TPOT *ReportTarget
	// TTFTPercentiles are the tables' TTFT percentiles, q in (0, 1), or 0
	// for the mean.
	TTFTPercentiles []float64
	// TPOTPercentile is the tables' TPOT percentile beside the mean; 0 asks
	// for the mean alone.
	TPOTPercentile float64
	Assume         Assume
	// ArrivalCV2 is a stated Ca^2, nil for Poisson (Knobs').
	ArrivalCV2 *float64
}

// ReportShapeAsk is one request shape: Measured, or the mean prompt and
// output lengths in tokens and, when Hit is set, the prefix-cache hit rate.
type ReportShapeAsk struct {
	Measured bool
	IL, OL   float64
	Hit      *float64
	// Label is the shape as asked ("measured", "8k/200").
	Label string
}

// ReportLoadAsk is one rate of the tables: Rate requests per second, Share
// of the shape's fleet knee, or Seen, the load judged at, times X when X is
// above 0 and not 1 (a growth what-if, judged on its own).
type ReportLoadAsk struct {
	Rate, Share float64
	Seen        bool
	X           float64
	// Label is the load as asked ("5r/s", "80%", "seen", "seen x1.5").
	Label string
}

// Grown says the load is the load seen grown by X.
func (l ReportLoadAsk) Grown() bool { return l.Seen && l.X > 0 && l.X != 1 }

// GrowthLoad is the load seen grown by x, as "seen x1.5" asks it.
func GrowthLoad(x float64) ReportLoadAsk {
	return ReportLoadAsk{Seen: true, X: x, Label: "seen x" + strconv.FormatFloat(x, 'f', -1, 64)}
}

// ReportTarget is one SLO target: Seconds at Percentile q, or the mean at 0.
type ReportTarget struct {
	Seconds, Percentile float64
}

// Reporter is a Pricer that answers POST /v1/report.
type Reporter interface {
	Report(ctx context.Context, q ReportQuery) (*ReportResponse, error)
}

// The verdicts a report gives.
const (
	VerdictUnder     = "UNDER-PROVISIONED"
	VerdictOver      = "OVER-PROVISIONED"
	VerdictRight     = "RIGHT-SIZED"
	VerdictNotJudged = "NOT JUDGED"
)

// The statistics of the source's per-cycle arrival rate a fleet can be judged
// at: the mean, the 95th percentile (nearest rank) and the highest, over the
// cycles that read a rate above 0.
const (
	LoadMean = "mean"
	LoadP95  = "p95"
	LoadPeak = "peak"
)

// DefaultLoadStatistic is the statistic judged at when none is asked: p95, so
// that a window whose mean hides its busy part is judged at the busy part.
const DefaultLoadStatistic = LoadP95

// The scopes a verdict covers: the whole fleet, or decode alone when the
// prefill side does not price.
const (
	ScopeFleet      = "fleet"
	ScopeDecodeOnly = "decode only"
)

// The marks a summary cell carries. CellOnset is a TTFT miss that comes from
// the rate being past the prefill flow-control onset, where the router
// queues, and not from the figure.
const (
	CellMet       = "met"
	CellMissed    = "missed"
	CellOnset     = "missed at onset"
	CellSaturated = "saturated"
	CellUnknown   = "unknown"
	CellNoTarget  = "no target"
)

// ReportResponse is POST /v1/report's answer, and capacity-planner
// at-replicas -o json.
type ReportResponse struct {
	SchemaVersion int          `json:"schemaVersion"`
	Question      string       `json:"question"`
	Source        ReportSource `json:"source"`
	Fleet         ReportFleet  `json:"fleet"`
	Load          ReportLoad   `json:"load"`
	SLO           ReportSLO    `json:"slo"`
	// Loads names the tables' rates, in order: a summary cell's column.
	Loads []string `json:"loads"`
	// Shapes is each shape's answer, in the order asked.
	Shapes []ReportShape `json:"shapes"`
	// NotValidated says which figures no run has validated, and by how much
	// they read off where a run measured them.
	NotValidated []string `json:"notValidated"`
	ExitCode     int      `json:"exitCode"`
	// SelfCheck is the planner checked on the source the report reads.
	SelfCheck *SelfCheck `json:"selfCheck,omitempty"`
}

// ReportShape is the fleet's answer at one request shape.
type ReportShape struct {
	Label         string  `json:"label"`
	InputTokens   float64 `json:"inputTokens"`
	OutputTokens  float64 `json:"outputTokens"`
	PrefixHitRate float64 `json:"prefixHitRate"`
	// Source is where the lengths came from: the measured window, or asked.
	Source string `json:"source"`
	// Extrapolated says IL or OL lies outside what the recording saw, and
	// ExtrapolatedWhy by how much.
	Extrapolated    bool          `json:"extrapolated"`
	ExtrapolatedWhy string        `json:"extrapolatedWhy,omitempty"`
	Utilization     ReportUse     `json:"utilization"`
	Capacity        ReportLimits  `json:"capacity"`
	Table           []ReportRow   `json:"table"`
	Verdict         ReportVerdict `json:"verdict"`
	// Growth is the verdict at each growth load asked ("seen x1.5"), in the
	// order asked: what-ifs beside Verdict, never in its place.
	Growth []ReportGrowth `json:"growth"`
	// Flags is every flag a figure of the shape carries, once each.
	Flags []string `json:"flags"`
	// Assumed is every input the shape's answer assumed, in words.
	Assumed []string `json:"assumed"`
}

// ReportSource is what the report was read from.
type ReportSource struct {
	Recording   string    `json:"recording,omitempty"`
	Namespace   string    `json:"namespace"`
	Model       string    `json:"model"`
	Cycle       int       `json:"cycle"`
	TotalCycles int       `json:"totalCycles"`
	FirstAt     time.Time `json:"firstAt"`
	LastAt      time.Time `json:"lastAt"`
	// Gaps is how much of the span from FirstAt to LastAt the cycles read
	// cover; absent with fewer than three distinct cycles.
	Gaps *WindowGaps `json:"gaps,omitempty"`
}

// ReportFleet is the fleet judged.
type ReportFleet struct {
	Prefill        int    `json:"prefill"`
	Decode         int    `json:"decode"`
	PrefillVariant string `json:"prefillVariant,omitempty"`
	DecodeVariant  string `json:"decodeVariant"`
	// PrefillFrom and DecodeFrom say where each count came from: "recording"
	// or "asked".
	PrefillFrom string `json:"prefillFrom"`
	DecodeFrom  string `json:"decodeFrom"`
	// PrefillEngine and DecodeEngine are the engine each variant runs, for
	// reference (EngineInfo); absent where the source holds none.
	PrefillEngine *EngineInfo `json:"prefillEngine,omitempty"`
	DecodeEngine  *EngineInfo `json:"decodeEngine,omitempty"`
}

// ReportLoad is the load the fleet is judged at, and the load the source saw.
// Each cycle's rate is the router's 1-minute arrival rate as the collector
// read it.
type ReportLoad struct {
	// Rate is the load judged at: Seen as a figure, which carries the
	// refusal (NoTraffic) when the source saw no load, or
	// (ReasonWindowGapAtPeak) when a gap in the source could hide its
	// peak; a stated load (LoadSeen) is never refused.
	Rate Figure `json:"rate"`
	// Seen is the load the source saw at Statistic, or the one asked in its
	// place, and Source which ("observed", "asked" or "none"). The verdict
	// is judged at it; a growth load is a what-if of its own (Growth).
	Seen      *float64 `json:"seen,omitempty"`
	Source    string   `json:"source"`
	Statistic string   `json:"statistic"`
	// Cycles is how many cycles the window holds, and CyclesWithTraffic how
	// many of them read an arrival rate above 0: Mean, P95 and Peak are of
	// those, one rate per cycle.
	Cycles            int      `json:"cycles"`
	CyclesWithTraffic int      `json:"cyclesWithTraffic"`
	Mean              *float64 `json:"mean,omitempty"`
	P95               *float64 `json:"p95,omitempty"`
	Peak              *float64 `json:"peak,omitempty"`
	// P95IsPeak says the window has too few cycles (under 20) for its p95
	// to be anything but its peak.
	P95IsPeak bool `json:"p95IsPeak,omitempty"`
	// Rolling is the busiest stretch of the window: the highest mean rate over
	// consecutive cycles spanning RollingWindow, idle and failed reads
	// counting as 0; nil with fewer cycles.
	Rolling *ReportRolling `json:"rolling,omitempty"`
}

// ReportRolling is the highest rolling mean of the per-cycle arrival rate,
// over Cycles consecutive cycles spanning about WindowSeconds.
type ReportRolling struct {
	WindowSeconds float64 `json:"windowSeconds"`
	Cycles        int     `json:"cycles"`
	Peak          float64 `json:"peak"`
}

// RollingWindow is the span of ReportLoad.Rolling: about the scale-up lag, so
// that it tells a load the fleet must hold from one cycle's spike.
const RollingWindow = 2 * time.Minute

// ReportUse is the utilization at the load.
type ReportUse struct {
	Decode  ReportDecodeUse  `json:"decode"`
	Prefill ReportPrefillUse `json:"prefill"`
}

// ReportDecodeUse is decode's utilization: the KV fill each replica runs at,
// the batch slots it occupies, and the share of requests that wait for one.
type ReportDecodeUse struct {
	KVFill  Figure  `json:"kvFill"`
	BandTop float64 `json:"bandTop"`
	// SlotsOccupied is the sequences resident over the fleet, Slots the
	// fleet's batch slots.
	SlotsOccupied   Figure `json:"slotsOccupied"`
	Slots           int64  `json:"slots"`
	WaitProbability Figure `json:"waitProbability"`
}

// ReportPrefillUse is prefill's utilization: the load over the prefill knee
// on the busiest pod, and over the pods pooled.
type ReportPrefillUse struct {
	Pooled    Figure  `json:"pooled"`
	WorstPod  Figure  `json:"worstPod"`
	Imbalance float64 `json:"imbalance,omitempty"`
}

// ReportLimits is the fleet's capacity.
type ReportLimits struct {
	PrefillKnee ReportRate `json:"prefillKnee"`
	DecodeKnee  ReportRate `json:"decodeKnee"`
	FleetKnee   ReportRate `json:"fleetKnee"`
	Operating   ReportRate `json:"operating"`
	// MaxAtSLO is the highest rate that holds every SLO target, with no
	// headroom: slo-to-rate's fleet rate at headroom 1.
	MaxAtSLO       ReportRate `json:"maxAtSLO"`
	Headroom       float64    `json:"headroom"`
	HeadroomSource string     `json:"headroomSource"`
}

// ReportRate is a rate, the output tokens a second it carries, and what binds
// it, in words.
type ReportRate struct {
	Figure
	OutputTokensPerSecond *float64 `json:"outputTokensPerSecond,omitempty"`
	Binds                 string   `json:"binds,omitempty"`
}

// ReportSLO is the targets the report judges by.
type ReportSLO struct {
	TTFT *ReportObjective `json:"ttft,omitempty"`
	TPOT *ReportObjective `json:"tpot,omitempty"`
}

// ReportObjective is one target: its statistic, seconds and source ("policy"
// or "asked").
type ReportObjective struct {
	Statistic string  `json:"statistic"`
	Target    float64 `json:"target"`
	Source    string  `json:"source"`
}

// ReportRow is one rate of a shape's SLO against load. E2EMeanSum is a sum of
// means, TTFT mean + (OL - 1) x TPOT mean, not a percentile.
type ReportRow struct {
	// Load is the rate as asked ("80%"), Label the row's name.
	Load           string       `json:"load"`
	Label          string       `json:"label"`
	ShareOfKnee    *float64     `json:"shareOfKnee,omitempty"`
	Rate           float64      `json:"rate"`
	TTFT           []ReportStat `json:"ttft"`
	TPOTMean       Figure       `json:"tpotMean"`
	TPOTPercentile *ReportStat  `json:"tpotPercentile,omitempty"`
	E2EMeanSum     Figure       `json:"e2eMeanSum"`
	// Cell is the row in the summary matrix.
	Cell ReportCell `json:"cell"`
}

// ReportCell is one cell of the shape-by-load summary: TTFT p90 and the
// TPOT percentile (the mean where the percentile is refused: TPOT.Statistic
// says which), and Mark, whether the SLO targets hold at the rate (CellMet,
// CellMissed, CellOnset, CellSaturated, CellUnknown, CellNoTarget).
type ReportCell struct {
	TTFT ReportStat `json:"ttft"`
	TPOT ReportStat `json:"tpot"`
	Mark string     `json:"mark"`
}

// ReportStat is a figure at a statistic.
type ReportStat struct {
	Statistic string `json:"statistic"`
	Figure
}

// ReportVerdict is the fleet judged at the load: Verdict, Summary in one
// plain sentence, why in numbers (Reason), the roles that can lose a replica
// (Shrink), the roles Needed has more of than the fleet (Grow), and each
// fleet checked. On OVER-PROVISIONED, Shrink and Grow can name the same
// role: Needed may trade a role with no spare for more of one with a spare. Scope is ScopeDecodeOnly when the prefill side does not
// price: such a verdict is never OVER-PROVISIONED.
type ReportVerdict struct {
	Verdict string `json:"verdict"`
	Summary string `json:"summary"`
	Scope   string `json:"scope"`
	Reason  string `json:"reason"`
	// Needed is the smallest fleet that holds every target at the load with
	// headroom, on an UNDER-PROVISIONED or OVER-PROVISIONED verdict; nil on
	// the others. A role it cuts holds its own check at the peak too, as
	// Spare asks, so Needed cuts a role with no spare only in trade for
	// more of the other role (Grow).
	Needed *ReportNeeded `json:"needed,omitempty"`
	// Roles is each role's own standing at the load: whether it has a
	// replica to spare, and why.
	Roles  []ReportRole  `json:"roles"`
	Shrink []string      `json:"shrink"`
	Grow   []string      `json:"grow"`
	Checks []ReportCheck `json:"checks"`
}

// ReportNeeded is the smallest fleet that holds every target at the load
// with headroom, found by POST /v1/size's search (SmallestFleet) on the
// report's own check, a role it cuts holding its own check at the peak.
// DecodeOnly is a prefill side that does not price:
// Decode is then the fewest decode replicas, and Prefill is 0. Alternatives
// are the other mixes of the same total that also hold. Unmet says no fleet
// within MaxReplicas of each role holds, and which role fell short; the
// counts are then 0.
type ReportNeeded struct {
	Prefill      int           `json:"prefill"`
	Decode       int           `json:"decode"`
	DecodeOnly   bool          `json:"decodeOnly,omitempty"`
	Alternatives []FleetCounts `json:"alternatives,omitempty"`
	Unmet        string        `json:"unmet,omitempty"`
	MaxReplicas  int           `json:"maxReplicas"`
}

// ReportRole is one role's standing at the load: Spare says one replica
// fewer of it still holds (decode's knee and TPOT, or the prefill knee and
// TTFT, at the peak; the whole fleet at the load judged), and Detail says
// why or how close the role is to its edge.
type ReportRole struct {
	Role   string `json:"role"`
	Spare  bool   `json:"spare"`
	Detail string `json:"detail"`
}

// ReportGrowth is the fleet judged at the load seen grown by X: Load is the
// load as labelled ("seen x1.5"), Rate the rate judged at.
type ReportGrowth struct {
	Load    string        `json:"load"`
	X       float64       `json:"x"`
	Rate    float64       `json:"rate"`
	Verdict ReportVerdict `json:"verdict"`
}

// ReportCheck is one fleet checked at the load: whether it holds every SLO
// target with headroom, and why.
type ReportCheck struct {
	Prefill int    `json:"prefill"`
	Decode  int    `json:"decode"`
	Holds   bool   `json:"holds"`
	Known   bool   `json:"known"`
	Reason  string `json:"reason"`
}

// FigureOf is r on the wire, in unit, as every other figure the API returns
// (worseIsHigh: a refused corner of a span takes the higher side).
func FigureOf(r whatif.Result, unit string, worseIsHigh bool) Figure {
	return figureOf(r, unit, worseIsHigh)
}

// handleReport is POST /v1/report.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	var req ReportRequest
	if !readJSON(w, r, &req) {
		return
	}
	out, err := s.report(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	body, err := marshal(out)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// errNoReporter is inputs whose Pricer cannot report.
var errNoReporter = errors.New("the inputs this inputsId names cannot be reported on")

// report answers req on one of the server's plan slots, within its plan
// budget.
func (s *Server) report(ctx context.Context, req ReportRequest) (*ReportResponse, error) {
	e, ok := s.inputs.get(req.InputsID)
	if !ok {
		return nil, errUnknownInputs
	}
	rep, ok := e.pricer.(Reporter)
	if !ok {
		return nil, errNoReporter
	}
	q, err := ReportQueryOf(req)
	if err != nil {
		return nil, err
	}
	if !acquire(s.planSlots) {
		return nil, errBusy
	}
	defer func() { <-s.planSlots }()
	bctx, cancel := context.WithTimeoutCause(ctx, s.planBudget, errPlanBudget)
	defer cancel()
	out, err := rep.Report(bctx, q)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil && errors.Is(context.Cause(bctx), errPlanBudget) {
		return nil, BudgetError{Budget: s.planBudget}
	}
	if err != nil {
		return nil, err
	}
	out.SchemaVersion = SchemaVersion
	// The source's self-check was read before this report stated anything.
	out.SelfCheck = e.doc.SelfCheck.RanWithout(&Stated{Assume: q.Assume, ArrivalCV2: q.ArrivalCV2})
	return out, nil
}

// The most shapes and loads one report prices: each shape prices its fleets
// at every load, and a fleet one fewer of each role for its verdict.
const (
	maxReportShapes     = 6
	maxReportLoads      = 8
	maxReportStatistics = 4
)

// ReportQueryOf checks req and reads it into numbers. A field asked wrongly
// is an InputError.
func ReportQueryOf(req ReportRequest) (ReportQuery, error) {
	q := ReportQuery{TTFTPercentiles: []float64{0.5, 0.9}, TPOTPercentile: 0.9}
	if k := req.Fleet; k != nil {
		for _, c := range []struct {
			name string
			n    *int
			to   *int
		}{{"fleet.prefill", k.Prefill, &q.Prefill}, {"fleet.decode", k.Decode, &q.Decode}} {
			if c.n == nil {
				continue
			}
			if *c.n < 1 {
				return q, InputErrorf("%s %d, want 1 or more", c.name, *c.n)
			}
			*c.to = *c.n
		}
	}
	if len(req.Shapes) > maxReportShapes {
		return q, InputErrorf("shapes: %d, want at most %d", len(req.Shapes), maxReportShapes)
	}
	for _, s := range req.Shapes {
		sh, err := ParseReportShape(s)
		if err != nil {
			return q, InputError{Err: fmt.Errorf("shapes: %w", err)}
		}
		q.Shapes = append(q.Shapes, sh)
	}
	if len(req.Loads) > maxReportLoads {
		return q, InputErrorf("loads: %d, want at most %d", len(req.Loads), maxReportLoads)
	}
	for _, s := range req.Loads {
		l, err := ParseReportLoad(s)
		if err != nil {
			return q, InputError{Err: fmt.Errorf("loads: %w", err)}
		}
		q.Loads = append(q.Loads, l)
	}
	if req.LoadStatistic != "" {
		if err := CheckLoadStatistic(req.LoadStatistic); err != nil {
			return q, InputError{Err: fmt.Errorf("loadStatistic: %w", err)}
		}
		if req.LoadSeen != nil {
			return q, InputErrorf("loadStatistic and loadSeen: the load is the source's at a statistic or the one stated, not both")
		}
		q.LoadStatistic = req.LoadStatistic
	}
	if a := req.ArrivalCV2; a != nil {
		if !usable(*a) {
			return q, InputErrorf("arrivalCV2 %g, want a finite value above 0: 1 is Poisson", *a)
		}
		q.ArrivalCV2 = ptr(*a)
	}
	for _, c := range []struct {
		name string
		x    *float64
		to   *float64
	}{{"loadSeen", req.LoadSeen, &q.LoadSeen}, {"loadSeenX", req.LoadSeenX, &q.LoadSeenX}} {
		if c.x == nil {
			continue
		}
		if !usable(*c.x) || math.IsInf(*c.x, 0) {
			return q, InputErrorf("%s %g, want a finite value above 0", c.name, *c.x)
		}
		*c.to = *c.x
	}
	if req.Headroom != nil {
		if h := *req.Headroom; !(h > 0 && h < 1) {
			return q, InputErrorf("headroom %g, want a fraction of the knee in (0, 1)", h)
		}
		q.Headroom = *req.Headroom
	}
	if o := req.SLO; o != nil {
		var err error
		if q.TTFT, err = reportTarget("slo.ttft", o.TTFT); err != nil {
			return q, err
		}
		if q.TPOT, err = reportTarget("slo.tpot", o.TPOT); err != nil {
			return q, err
		}
	}
	if len(req.TTFTStatistics) > 0 {
		if len(req.TTFTStatistics) > maxReportStatistics {
			return q, InputErrorf("ttftStatistics: %d, want at most %d", len(req.TTFTStatistics), maxReportStatistics)
		}
		q.TTFTPercentiles = nil
		for _, s := range req.TTFTStatistics {
			p, err := parseStatistic("ttftStatistics", s)
			if err != nil {
				return q, err
			}
			q.TTFTPercentiles = append(q.TTFTPercentiles, p)
		}
	}
	if req.TPOTStatistic != "" {
		p, err := parseStatistic("tpotStatistic", req.TPOTStatistic)
		if err != nil {
			return q, err
		}
		q.TPOTPercentile = p
	}
	if a := req.Assume; a != nil {
		sc := Scenario{Assume: *a, TPOT: &ResolvedObjective{Statistic: statisticName(q.TPOTPercentile)}}
		if err := sc.checkAssume(); err != nil {
			return q, err
		}
		q.Assume = *a
	}
	return q, nil
}

// CheckLoadStatistic checks a load statistic: LoadMean, LoadP95 or LoadPeak.
func CheckLoadStatistic(s string) error {
	switch s {
	case LoadMean, LoadP95, LoadPeak:
		return nil
	}
	return fmt.Errorf("%q, want %s, %s or %s", s, LoadMean, LoadP95, LoadPeak)
}

// ParseReportShape reads a shape: "measured", or "IL/OL[/hit]", lengths in
// tokens with an optional k (1024 tokens), and hit a rate in [0, 1).
func ParseReportShape(s string) (ReportShapeAsk, error) {
	if s == "measured" {
		return ReportShapeAsk{Measured: true, Label: s}, nil
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return ReportShapeAsk{}, fmt.Errorf("shape %q, want measured or IL/OL[/hit] (8k/200, 1k/4k/0.3)", s)
	}
	sh := ReportShapeAsk{Label: s}
	var err error
	if sh.IL, err = tokens(parts[0]); err != nil {
		return sh, fmt.Errorf("shape %q: IL: %w", s, err)
	}
	if sh.OL, err = tokens(parts[1]); err != nil {
		return sh, fmt.Errorf("shape %q: OL: %w", s, err)
	}
	if len(parts) == 3 {
		h, err := strconv.ParseFloat(parts[2], 64)
		if err != nil || !(h >= 0 && h < 1) {
			return sh, fmt.Errorf("shape %q: hit %q, want a rate from 0 to below 1", s, parts[2])
		}
		sh.Hit = &h
	}
	return sh, nil
}

// maxShapeTokens bounds a stated length: 1024k tokens.
const maxShapeTokens = 1 << 20

// tokens reads a length in tokens: a number, or one with a k suffix.
func tokens(s string) (float64, error) {
	scale := 1.0
	if strings.HasSuffix(s, "k") {
		s, scale = strings.TrimSuffix(s, "k"), 1024
	}
	x, err := strconv.ParseFloat(s, 64)
	if err != nil || !(x*scale >= 1 && x*scale <= maxShapeTokens) {
		return 0, fmt.Errorf("%q, want a length from 1 to 1024k tokens", s)
	}
	return x * scale, nil
}

// ParseReportLoad reads a load: "seen", the load seen grown ("seen x1.5",
// "seen×1.5" or "seen*1.5"), "80%" of the knee, or "5r/s".
func ParseReportLoad(s string) (ReportLoadAsk, error) {
	switch {
	case s == "seen":
		return ReportLoadAsk{Seen: true, Label: s}, nil
	case strings.HasPrefix(s, "seen"):
		g := strings.TrimSpace(strings.TrimPrefix(s, "seen"))
		for _, p := range []string{"x", "×", "*"} {
			if strings.HasPrefix(g, p) {
				g = strings.TrimSpace(strings.TrimPrefix(g, p))
				break
			}
		}
		x, err := strconv.ParseFloat(g, 64)
		if err != nil || !usable(x) || math.IsInf(x, 0) {
			return ReportLoadAsk{}, fmt.Errorf("load %q, want the load seen grown by a finite factor above 0 (seen x1.5)", s)
		}
		return GrowthLoad(x), nil
	case strings.HasSuffix(s, "%"):
		x, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		if err != nil || !(x > 0 && x <= 100) {
			return ReportLoadAsk{}, fmt.Errorf("load %q, want a share of the knee above 0%% and at most 100%%", s)
		}
		return ReportLoadAsk{Share: x / 100, Label: s}, nil
	case strings.HasSuffix(s, "r/s"):
		x, err := strconv.ParseFloat(strings.TrimSuffix(s, "r/s"), 64)
		if err != nil || !usable(x) || math.IsInf(x, 0) {
			return ReportLoadAsk{}, fmt.Errorf("load %q, want a finite rate above 0", s)
		}
		return ReportLoadAsk{Rate: x, Label: s}, nil
	}
	return ReportLoadAsk{}, fmt.Errorf("load %q, want seen, a share of the knee (80%%) or a rate (5r/s)", s)
}

// reportTarget reads one SLO target; nil with none asked.
func reportTarget(field string, o *Objective) (*ReportTarget, error) {
	if o == nil {
		return nil, nil
	}
	if len(o.BucketTargets) > 0 {
		return nil, InputErrorf("%s.bucketTargets: a report judges one target per objective", field)
	}
	if o.Target == nil || !usable(*o.Target) || math.IsInf(*o.Target, 0) {
		return nil, InputErrorf("%s.target: want a finite target above 0, in seconds", field)
	}
	stat := o.Statistic
	if stat == "" {
		stat = statisticMean
	}
	p, err := parseStatistic(field+".statistic", stat)
	if err != nil {
		return nil, err
	}
	return &ReportTarget{Seconds: *o.Target, Percentile: p}, nil
}

// ReportStatisticName is q as a statistic: "mean", or "p" and the percentile.
func ReportStatisticName(q float64) string { return statisticName(q) }
