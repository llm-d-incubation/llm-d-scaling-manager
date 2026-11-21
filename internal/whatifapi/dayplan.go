package whatifapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The day plan (POST /v1/dayplan): the prefill and decode replicas a
// window's traffic needed, bucket by bucket, against the replicas the fleet
// had. Each bucket is sized as POST /v1/size sizes a rate, by the same
// search (sizer.search) on the same priced fleets: the learned state is read
// once, by the inputs, and every bucket of one shape shares one sizer, whose
// fleets are priced once whatever the rate. Nothing new is modelled.
//
// A term the source cannot price refuses every rate alike: a prefill rate
// V_P or a fixed latency a it did not learn, a TTFT target under the TTFT at
// zero load, a decode line it did not fit. Such a refusal is not a bucket's:
// the plan probes each shape once, drops the term it cannot price, sizes on
// what does price, and says so in Refused. Only a rate no fleet within the
// bound serves is a bucket's own refusal (unmet).

// SeriesSample is one cycle of the source: its time, the model's arrival
// rate (RateKnown false where it is the decode rows' sum), the serving pods
// of the priced variants, and the traffic's shape where the cycle read one
// (HasShape).
type SeriesSample struct {
	At                      time.Time
	Rate                    float64
	RateKnown               bool
	Prefill, Decode         int
	PrefillGPUs, DecodeGPUs int
	PromptTokens            float64
	OutputTokens            float64
	PrefixHitRate           float64
	HasShape                bool
	// OtherPods is the serving pods of prefill and decode variants other
	// than the priced ones.
	OtherPods int
	// PromptComputedRate, PromptRequestRate and PromptBuckets are the
	// prefill pods' computed prompt tokens and requests a second and their
	// pooled computed-token histogram (learnreplay.CycleSample), for a
	// backtest's prompt spread; zero and nil with none.
	PromptComputedRate, PromptRequestRate float64
	PromptBuckets                         map[float64]float64
	// FlowControlSeries says the cycle read the router's flow-control queue
	// series.
	FlowControlSeries bool
	// PrefillNotServing and DecodeNotServing are the priced variants'
	// replicas asked for and not serving (learnreplay.CycleSample).
	PrefillNotServing, DecodeNotServing int
}

// The day plan's defaults and bounds.
const (
	// DefaultDayPlanStep is a bucket's length when the request names none.
	DefaultDayPlanStep = 5 * time.Minute
	// minDayPlanStep and maxDayPlanStep bound a bucket's length.
	minDayPlanStep = 15 * time.Second
	maxDayPlanStep = 24 * time.Hour
	// maxDayPlanBuckets bounds the buckets one day plan sizes.
	maxDayPlanBuckets = 3000
	// DefaultMinRequests is the fewest requests a bucket must hold to be
	// sized when the request names none.
	DefaultMinRequests = 30
	// MinPercentileCycles is the fewest cycles a bucket must hold to be
	// sized for its p95: of fewer, the p95 is the highest cycle or the one
	// under it, and the bucket is sized for its mean instead.
	MinPercentileCycles = 20
	// shapeGrid is the ratio between neighbouring shapes a bucket's
	// observed lengths are rounded to, so that buckets of nearly the same
	// shape share a sizer and its priced fleets.
	shapeGrid = 1.05
)

// What a bucket is sized for (DayPlanRequest.SizeFor).
const (
	SizeForMean = "mean"
	SizeForP95  = "p95"
	SizeForMax  = "max"
)

// A bucket's status.
const (
	BucketSized         = "sized"
	BucketTooLittleData = "tooLittleData"
	BucketNoData        = "noData"
	// BucketUnmet is a bucket whose rate no fleet within the bound serves.
	BucketUnmet = "unmet"
	// BucketRefused is a bucket no role could be sized for: every term
	// that sizes decode is refused (Refused says why).
	BucketRefused = "refused"
)

// A bucket's or an hour's verdict: the fleet it had against the one it
// needed.
const (
	DayVerdictUnder   = "under"
	DayVerdictOver    = "over"
	DayVerdictMatch   = "match"
	DayVerdictRefused = "refused"
)

// The flags a day plan adds to a bucket, beside the planner's own.
const (
	// FlagWindowShape is a bucket priced at the inputs' shape: its cycles
	// read no lengths of their own.
	FlagWindowShape = "WindowShape"
	// FlagExtrapolated is a bucket priced at a stated shape outside the
	// lengths the window's buckets observed.
	FlagExtrapolated = "Extrapolated"
	// FlagFewCyclesForP95 is a bucket asked for its p95 and sized for its
	// mean: it holds fewer than MinPercentileCycles cycles.
	FlagFewCyclesForP95 = "FewCyclesForP95"
	// FlagRateFromDecodeRows is a bucket with a cycle whose input carried no
	// model arrival rate: its rate is the decode rows' request rates summed.
	FlagRateFromDecodeRows = "RateFromDecodeRows"
	// FlagTPOTNotPriced is a bucket sized without its TPOT target, which the
	// source cannot price (Refused).
	FlagTPOTNotPriced = "TPOTNotPriced"
	// FlagPrefillNotPriced is a bucket whose prefill replicas are not sized,
	// the source being unable to price its TTFT (Refused).
	FlagPrefillNotPriced = "PrefillNotPriced"
	// FlagFleetRefused is a bucket whose fleet limit the source refuses at
	// more replicas than one (Refused, term fleet).
	FlagFleetRefused = "FleetRefused"
	// FlagOtherVariantsServed is a bucket in which a prefill or decode
	// variant other than the priced ones served: the rate is the model's,
	// and part of it went to pods Actual does not count.
	FlagOtherVariantsServed = "OtherVariantsServed"
)

// DayPlanRequest is POST /v1/dayplan. Every field but InputsID defaults as a
// size's does; the traffic's classes are refused, since the rates are the
// source's own, bucket by bucket.
type DayPlanRequest struct {
	InputsID    string      `json:"inputsId"`
	Traffic     *Traffic    `json:"traffic,omitempty"`
	SLO         *SLORequest `json:"slo,omitempty"`
	Assume      *Assume     `json:"assume,omitempty"`
	Headroom    *float64    `json:"headroom,omitempty"`
	MaxReplicas *FleetKnob  `json:"maxReplicas,omitempty"`
	// Step is a bucket's length, a Go duration ("5m"); default 5m.
	Step string `json:"step,omitempty"`
	// SizeFor is the rate of a bucket the fleet is sized for: "mean", "p95"
	// or "max" of its cycles' rates; default p95. A bucket of fewer than
	// MinPercentileCycles cycles asked for its p95 is sized for its mean.
	SizeFor string `json:"sizeFor,omitempty"`
	// MinRequests is the fewest requests a bucket must hold to be sized;
	// default 30.
	MinRequests *float64 `json:"minRequests,omitempty"`
}

// DayPlanResponse is a day plan: every bucket, the hours they make, the
// summary, and what the source could not price.
type DayPlanResponse struct {
	// SelfCheck is the planner checked on the source the plan reads.
	SelfCheck     *SelfCheck `json:"selfCheck,omitempty"`
	SchemaVersion int        `json:"schemaVersion"`
	WhatifSchema  string     `json:"whatifSchema"`
	DayPlanID     string     `json:"dayPlanId"`
	InputsID      string     `json:"inputsId"`
	Banner        string     `json:"banner"`
	// From and To are the first bucket's start and the last bucket's end.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Step is a bucket's length, seconds.
	Step float64 `json:"step"`
	// SizeFor is what was asked of each bucket, and SizeForSource
	// "request" or "default"; each bucket says what it was sized for.
	SizeFor       string  `json:"sizeFor"`
	SizeForSource string  `json:"sizeForSource"`
	MinRequests   float64 `json:"minRequests"`
	// TTFT is absent with no TTFT target: neither the request nor the
	// policy sets one, and decode is sized alone (Refused, term ttft).
	TTFT *ResolvedObjective `json:"ttft,omitempty"`
	// TPOT is absent with no TPOT asked.
	TPOT        *ResolvedObjective `json:"tpot,omitempty"`
	Headroom    float64            `json:"headroom"`
	MaxReplicas FleetCounts        `json:"maxReplicas"`
	// Refused is every term the source could not price, which the plan
	// dropped, with what it did instead; empty when every term priced.
	Refused []DayRefusal `json:"refused"`
	Buckets []DayBucket  `json:"buckets"`
	Hours   []DayHour    `json:"hours"`
	Summary DaySummary   `json:"summary"`
	// Searched is how many fleets the day plan priced.
	Searched int `json:"searched"`
	// Notes are what the plan does not price, in words.
	Notes []string `json:"notes"`
}

// DayRefusal is a term the source cannot price at any rate: the role it
// sizes, the term ("ttft", "tpot" or "decodeKnee"), the reason and its
// detail, what to do about it (Hint), what the plan did instead (Effect),
// and how many buckets it touched.
type DayRefusal struct {
	Role    string `json:"role"`
	Term    string `json:"term"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail"`
	Hint    string `json:"hint"`
	Effect  string `json:"effect"`
	Buckets int    `json:"buckets"`
}

// DayRate is a bucket's or an hour's request rate over its cycles, r/s.
type DayRate struct {
	Mean float64 `json:"mean"`
	P95  float64 `json:"p95"`
	Max  float64 `json:"max"`
}

// DayShape is the lengths a bucket was priced at, and Source "bucket" (its
// own cycles'), "window" (the inputs') or "stated" (the request's mix).
type DayShape struct {
	PromptTokens  *float64 `json:"promptTokens,omitempty"`
	OutputTokens  *float64 `json:"outputTokens,omitempty"`
	PrefixHitRate *float64 `json:"prefixHitRate,omitempty"`
	Source        string   `json:"source"`
}

// DayNeeded is the fewest replicas in all that serve a bucket's rate, ties
// going to fewer decode (SmallestFleet): one fleet that serves it, and
// another mix may serve it too. Prefill is absent where the source cannot
// price it (Refused); Decode is then sized on decode's own limit alone, a
// lower bound (AtLeast): the fleet's limit, which decode admission can hold
// down, is not priced. A need over several buckets (an hour's, the peak)
// where some priced prefill and some did not carries the most prefill of
// those that did, and is a lower bound in both roles (AtLeast).
type DayNeeded struct {
	Prefill *int `json:"prefill,omitempty"`
	Decode  int  `json:"decode"`
	AtLeast bool `json:"atLeast,omitempty"`
}

// DayBucket is one bucket: what it saw, what it was sized for, the fleet it
// needed and the fleet it had.
type DayBucket struct {
	Start time.Time `json:"start"`
	// Samples is the cycles in the bucket, and Requests the requests they
	// carried (the mean rate times the time they cover).
	Samples  int      `json:"samples"`
	Requests float64  `json:"requests"`
	Rate     *DayRate `json:"rate,omitempty"`
	// SizedFor is the statistic of the cycles' rates the bucket was sized
	// for, and SizedRate that rate.
	SizedFor  string   `json:"sizedFor,omitempty"`
	SizedRate *float64 `json:"sizedRate,omitempty"`
	Shape     DayShape `json:"shape"`
	Status    string   `json:"status"`
	// Needed is the fewest replicas that serve SizedRate (sized), and Limit
	// the limit they were sized on.
	Needed *DayNeeded `json:"needed,omitempty"`
	Limit  *Figure    `json:"limit,omitempty"`
	// HadLimit is the limit of the fleet the bucket had, priced whole at the
	// same targets and headroom: the verdict's basis.
	HadLimit *Figure `json:"hadLimit,omitempty"`
	// Unmet is why no fleet within the bound serves the rate, and UnmetRole
	// the role short: "prefill", "decode" or "fleet" (both).
	Unmet     *Refusal `json:"unmet,omitempty"`
	UnmetRole string   `json:"unmetRole,omitempty"`
	// Actual is the median serving pods of each priced variant over the
	// bucket's cycles; absent with no cycles.
	Actual *FleetCounts `json:"actual,omitempty"`
	// NotServing is the median, over the bucket's cycles, of each priced
	// role's replicas asked for and not serving: replicas that exist and
	// are not ready (starting, unschedulable, or held by an admission gate
	// such as Kueue's; learnreplay.CycleSample). Actual leaves them out;
	// absent when both are 0.
	NotServing *FleetCounts `json:"notServing,omitempty"`
	Verdict    string       `json:"verdict,omitempty"`
	Flags      []string     `json:"flags,omitempty"`
}

// DayHour is one hour of buckets, for a table: its rate over every cycle,
// the fleet that holds every one of its sized buckets, the fewest it had,
// and its verdict: under when any bucket was under or unmet, refused when
// every bucket with data was refused, over when every sized bucket was over.
type DayHour struct {
	Start time.Time `json:"start"`
	Rate  *DayRate  `json:"rate,omitempty"`
	// Needed is a fleet that holds every sized bucket of the hour, priced
	// again at each bucket's rate and shape: the fewest of the buckets' own
	// fleets that holds them all, else each role's most (holding). Busiest is the fleet of the one bucket of the highest
	// sized rate, which need not hold the others: a bucket of long outputs
	// at a lower rate can need more decode.
	Needed  *DayNeeded `json:"needed,omitempty"`
	Busiest *DayNeeded `json:"busiest,omitempty"`
	// NotConfirmed says Needed, priced again, fell short at a bucket of the
	// hour: a fleet limit that does not rise with the replicas.
	NotConfirmed bool         `json:"notConfirmed,omitempty"`
	Actual       *FleetCounts `json:"actual,omitempty"`
	// NotServing is, for each role, the replicas asked for and not serving
	// in the bucket that set the fewest it had (Actual), the most of those
	// at a tie: had and (+n) read off one bucket, so a replica serving in
	// one bucket and restarting in another is not counted twice. Absent
	// when none was.
	NotServing *FleetCounts `json:"notServing,omitempty"`
	Verdict    string       `json:"verdict,omitempty"`
	// Sized and Unmet are how many of its buckets were sized and unmet.
	Sized int `json:"sized"`
	Unmet int `json:"unmet"`
}

// DaySummary is the plan in a few figures. The needed and the had
// replica-hours over the sized buckets compare like with like; HadAll is
// what the fleet ran over every bucket with data. Every figure of a need is
// absent when no bucket was sized; a role's is absent when it was not
// priced.
type DaySummary struct {
	// PeakNeeded is a fleet that holds every sized bucket of the plan, as
	// an hour's Needed holds the hour's; BusiestNeeded the fleet of the one
	// bucket of the highest sized rate, and MinNeeded that of the lowest.
	// PeakNotConfirmed says PeakNeeded, priced again, fell short at a bucket.
	PeakNeeded       *DayNeeded `json:"peakNeeded,omitempty"`
	BusiestNeeded    *DayNeeded `json:"busiestNeeded,omitempty"`
	PeakNotConfirmed bool       `json:"peakNotConfirmed,omitempty"`
	MinNeeded        *DayNeeded `json:"minNeeded,omitempty"`
	// NeededReplicaHours and HadReplicaHours are over the sized buckets;
	// HadReplicaHoursAll over every bucket with data.
	NeededReplicaHours *RoleHours `json:"neededReplicaHours,omitempty"`
	HadReplicaHours    *RoleHours `json:"hadReplicaHours,omitempty"`
	HadReplicaHoursAll RoleHours  `json:"hadReplicaHoursAll"`
	// GPUsPerReplica is the priced variants' GPUs per replica, where the
	// source states them; the GPU-hours follow the replica-hours.
	GPUsPerReplica *FleetCounts `json:"gpusPerReplica,omitempty"`
	NeededGPUHours *float64     `json:"neededGpuHours,omitempty"`
	HadGPUHours    *float64     `json:"hadGpuHours,omitempty"`
	HadGPUHoursAll *float64     `json:"hadGpuHoursAll,omitempty"`
	// UnderHours are the hours with a bucket under or unmet (SLO risk),
	// OverHours those whose every sized bucket was over (waste), and
	// RefusedHours those whose every bucket with data was refused.
	UnderHours   []time.Time `json:"underHours"`
	OverHours    []time.Time `json:"overHours"`
	RefusedHours []time.Time `json:"refusedHours"`
	Counts       DayCounts   `json:"counts"`
}

// RoleHours is replica-hours per role; Prefill is absent where prefill was
// not priced.
type RoleHours struct {
	Prefill *float64 `json:"prefill,omitempty"`
	Decode  float64  `json:"decode"`
}

// DayCounts is how many buckets ended each way, and how many were sized for
// each statistic.
type DayCounts struct {
	Sized         int `json:"sized"`
	Unmet         int `json:"unmet"`
	Refused       int `json:"refused"`
	TooLittleData int `json:"tooLittleData"`
	NoData        int `json:"noData"`
	Under         int `json:"under"`
	Over          int `json:"over"`
	Match         int `json:"match"`
	SizedForMean  int `json:"sizedForMean"`
	SizedForP95   int `json:"sizedForP95"`
	SizedForMax   int `json:"sizedForMax"`
	// PrefillNotPriced is how many sized buckets have no prefill figure.
	PrefillNotPriced int `json:"prefillNotPriced"`
}

// handleDayPlan is POST /v1/dayplan.
func (s *Server) handleDayPlan(w http.ResponseWriter, r *http.Request) {
	var req DayPlanRequest
	if !readJSON(w, r, &req) {
		return
	}
	body, err := s.dayPlan(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeBody(w, body)
}

// dayPlan answers req, from the cache when asked before, on one plan slot.
func (s *Server) dayPlan(ctx context.Context, req DayPlanRequest) ([]byte, error) {
	e, ok := s.inputs.get(req.InputsID)
	if !ok {
		return nil, errUnknownInputs
	}
	q, err := dayQuestionOf(e, req)
	if err != nil {
		return nil, err
	}
	id := contentID("dayplan", q.key())
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
		bctx, cancel := context.WithTimeoutCause(ctx, s.planBudget, errPlanBudget)
		defer cancel()
		out, err := q.plan(bctx)
		if err != nil {
			if context.Cause(bctx) == errPlanBudget && ctx.Err() == nil {
				return nil, BudgetError{Budget: s.planBudget}
			}
			return nil, err
		}
		out.SchemaVersion, out.WhatifSchema, out.DayPlanID, out.InputsID, out.Banner = SchemaVersion, s.whatifSchema, id, e.id, banner
		// The source's self-check was read before this plan stated anything.
		out.SelfCheck = e.doc.SelfCheck.RanWithout(q.stated(false))
		body, err := marshal(out)
		if err != nil {
			return nil, err
		}
		return s.sizes.add(id, body), nil
	})
}

// DayPlan answers req against a source resolved outside a server: the CLI's
// day-plan, on the same code path as POST /v1/dayplan. whatifSchema is the
// CLI schema the figures follow, as Config.WhatifSchema.
func DayPlan(ctx context.Context, res *Resolved, req DayPlanRequest, whatifSchema string) (*DayPlanResponse, error) {
	doc := res.Inputs
	doc.InputsID = contentID("inputs", res.Content)
	e := &inputsEntry{id: doc.InputsID, doc: doc, pricer: res.Pricer, series: res.Series}
	req.InputsID = e.id
	q, err := dayQuestionOf(e, req)
	if err != nil {
		return nil, err
	}
	out, err := q.plan(ctx)
	if err != nil {
		return nil, err
	}
	if res.Folds != nil {
		out.SelfCheck = SelfCheckOf(ctx, res, SelfCheckInfo{PlannerVersion: res.PlannerVersion, Stated: q.stated(true)})
	}
	out.SchemaVersion, out.WhatifSchema, out.DayPlanID, out.InputsID, out.Banner = SchemaVersion, whatifSchema,
		contentID("dayplan", q.key()), e.id, banner
	return out, nil
}

// dayQuestion is a day plan's request checked against its inputs.
type dayQuestion struct {
	e           *inputsEntry
	sc          Scenario
	k           Knobs
	step        time.Duration
	sizeFor     string
	sizeFromReq bool
	minRequests float64
	bound       FleetCounts
	// noTTFT says neither the request nor the policy sets a TTFT target:
	// prefill is not sized, and decode is sized alone.
	noTTFT bool
}

// key is what names the day plan's answer.
func (q *dayQuestion) key() any {
	return struct {
		Scenario    Scenario
		Step        time.Duration
		SizeFor     string
		MinRequests float64
		Bound       FleetCounts
		NoTTFT      bool
	}{q.sc, q.step, q.sizeFor, q.minRequests, q.bound, q.noTTFT}
}

// stated is what the plan states in place of what its source gave, named
// as the CLI's flags or the API's keys.
func (q *dayQuestion) stated(cli bool) *Stated {
	return &Stated{Assume: q.k.Assume, ArrivalCV2: q.k.ArrivalCV2, PromptTokens: q.k.PromptTokens, OutputTokens: q.k.OutputTokens, CLI: cli,
		arrivalKey: "traffic.arrivalCV2"}
}

// CheckDayPlan checks what of req needs no source: the step, what a bucket
// is sized for, the request floor, the bound, the traffic, the targets'
// statistics and the headroom. The CLI checks its flags with it before the
// replay, which can take minutes; dayQuestionOf checks the rest against the
// inputs. A target left to the policy is taken as set: the stand-in policy
// sets both.
func CheckDayPlan(req DayPlanRequest) error {
	if _, err := dayQuestionOfRequest(req); err != nil {
		return err
	}
	stand := InputsResponse{Fleet: FleetInfo{Prefill: &RoleInfo{Replicas: 1}, Decode: RoleInfo{Replicas: 1}},
		Policy: PolicyInfo{Headroom: 1, TTFT: &PolicyObjective{Statistic: defaultTTFTStatistic, Seconds: 1},
			TPOT: &PolicyObjective{Statistic: statisticMean, Seconds: 1}}}
	_, _, err := dayScenarioOf(&stand, "", req)
	return err
}

// dayQuestionOfRequest is the day plan's question as far as req alone says
// it: every check that needs no inputs.
func dayQuestionOfRequest(req DayPlanRequest) (*dayQuestion, error) {
	if t := req.Traffic; t != nil && t.Classes != nil {
		return nil, InputErrorf("traffic.classes: a day plan sizes each bucket for its own observed rate; give the mix and arrivalCV2 only")
	}
	q := &dayQuestion{step: DefaultDayPlanStep, minRequests: DefaultMinRequests, sizeFor: SizeForP95}
	if req.Step != "" {
		d, err := time.ParseDuration(req.Step)
		if err != nil || d < minDayPlanStep || d > maxDayPlanStep {
			return nil, InputErrorf("step %q, want a duration from %s to %s", req.Step, minDayPlanStep, maxDayPlanStep)
		}
		q.step = d
	}
	if m := req.MinRequests; m != nil {
		if !(*m >= 0) || math.IsInf(*m, 1) {
			return nil, InputErrorf("minRequests %g, want 0 or more", *m)
		}
		q.minRequests = *m
	}
	var err error
	if q.bound, err = sizeBound(req.MaxReplicas); err != nil {
		return nil, err
	}
	switch req.SizeFor {
	case "":
	case SizeForMean, SizeForP95, SizeForMax:
		q.sizeFor, q.sizeFromReq = req.SizeFor, true
	default:
		return nil, InputErrorf("sizeFor %q, want %s, %s or %s", req.SizeFor, SizeForMean, SizeForP95, SizeForMax)
	}
	return q, nil
}

// dayScenarioOf is the scenario a day plan prices on in: req's SLO, mix,
// arrivals, assumed terms and headroom, at one replica a role and no rate
// (each bucket's is its own). With no TTFT target, from req or the policy,
// the scenario carries none (noTTFT): decode is sized alone.
func dayScenarioOf(in *InputsResponse, id string, req DayPlanRequest) (sc Scenario, noTTFT bool, err error) {
	one := 1
	traffic := &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: ptr(1)}}}
	if req.Traffic != nil {
		traffic.Mix, traffic.ArrivalCV2 = req.Traffic.Mix, req.Traffic.ArrivalCV2
	}
	slo := req.SLO
	noTTFT = in.Policy.TTFT == nil && (slo == nil || slo.TTFT == nil || slo.TTFT.Target == nil)
	if noTTFT {
		// A stand-in target, so that the scenario's checks of the rest run as
		// on any source; the plan never prices TTFT at it (group).
		s := SLORequest{TPOT: nil, TTFT: &Objective{Target: ptr(1.0)}}
		if slo != nil {
			s.TPOT = slo.TPOT
			if slo.TTFT != nil {
				s.TTFT.Statistic, s.TTFT.BucketTargets = slo.TTFT.Statistic, slo.TTFT.BucketTargets
			}
		}
		slo = &s
	}
	sc, err = scenarioOf(in, PlanRequest{InputsID: id, Fleet: &FleetKnob{Prefill: &one, Decode: &one},
		Traffic: traffic, SLO: slo, Assume: req.Assume, Headroom: req.Headroom})
	sc.Rate = nil
	return sc, noTTFT, err
}

// dayQuestionOf checks req against e.
func dayQuestionOf(e *inputsEntry, req DayPlanRequest) (*dayQuestion, error) {
	if len(e.series) == 0 {
		return nil, InputErrorf("the source carries no cycles to plan a day from")
	}
	if e.doc.Fleet.Prefill == nil {
		return nil, InputErrorf("the inputs have no prefill variant: POST /v1/dayplan sizes a P/D fleet")
	}
	q, err := dayQuestionOfRequest(req)
	if err != nil {
		return nil, err
	}
	q.e = e
	if q.sc, q.noTTFT, err = dayScenarioOf(&e.doc, e.id, req); err != nil {
		return nil, err
	}
	if q.k, err = q.sc.knobs(); err != nil {
		return nil, err
	}
	if q.noTTFT {
		// The stand-in target priced nothing: no TTFT is asked.
		q.sc.TTFT.Target = 0
	}
	first, last := e.series[0].At, e.series[len(e.series)-1].At
	if n := int(last.Sub(first)/q.step) + 2; n > maxDayPlanBuckets {
		return nil, InputErrorf("step %s cuts the window into %d buckets, over the %d a day plan sizes: lengthen the step", q.step, n, maxDayPlanBuckets)
	}
	return q, nil
}

// dayBucket is one bucket's cycles.
type dayBucket struct {
	start   time.Time
	samples []SeriesSample
}

// bucketize cuts series into buckets of step, aligned to step in UTC (a
// step that divides a day starts a bucket at midnight UTC), from the first
// cycle's bucket to the last's: a bucket no cycle fell in is kept, empty. A
// clock change is no change in UTC: a day across one has the buckets of 24
// hours.
func bucketize(series []SeriesSample, step time.Duration) []dayBucket {
	if len(series) == 0 {
		return nil
	}
	sorted := slices.Clone(series)
	slices.SortStableFunc(sorted, func(a, b SeriesSample) int { return a.At.Compare(b.At) })
	first := sorted[0].At.UTC().Truncate(step)
	last := sorted[len(sorted)-1].At.UTC().Truncate(step)
	out := make([]dayBucket, int(last.Sub(first)/step)+1)
	for i := range out {
		out[i].start = first.Add(time.Duration(i) * step)
	}
	for _, x := range sorted {
		i := int(x.At.UTC().Truncate(step).Sub(first) / step)
		out[i].samples = append(out[i].samples, x)
	}
	return out
}

// sampleStep is the series' cycle: the median gap between cycles, the step
// when there is one cycle.
func sampleStep(series []SeriesSample, step time.Duration) time.Duration {
	var gaps []time.Duration
	for i := 1; i < len(series); i++ {
		if d := series[i].At.Sub(series[i-1].At); d > 0 {
			gaps = append(gaps, d)
		}
	}
	if len(gaps) == 0 {
		return step
	}
	slices.Sort(gaps)
	return gaps[len(gaps)/2]
}

// rateOf is the rate over samples: the mean, the nearest-rank p95 and the
// most.
func rateOf(samples []SeriesSample) DayRate {
	r := make([]float64, len(samples))
	var sum float64
	for i, x := range samples {
		r[i] = x.Rate
		sum += x.Rate
	}
	slices.Sort(r)
	rank := int(math.Ceil(0.95*float64(len(r)))) - 1
	return DayRate{Mean: sum / float64(len(r)), P95: r[max(rank, 0)], Max: r[len(r)-1]}
}

// medianInt is the median of xs, the upper one of an even count.
func medianInt(xs []int) int {
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[len(s)/2]
}

// actualOf is the median serving pods of each role over samples.
func actualOf(samples []SeriesSample) FleetCounts {
	p, d := make([]int, len(samples)), make([]int, len(samples))
	for i, x := range samples {
		p[i], d[i] = x.Prefill, x.Decode
	}
	return FleetCounts{Prefill: medianInt(p), Decode: medianInt(d)}
}

// hourNotServing is, for each role, the replicas asked for and not serving
// in the bucket that had the fewest of the role serving (had, the hour's
// Actual), the most of those at a tie; nil when none.
func hourNotServing(had *FleetCounts, buckets []*DayBucket) *FleetCounts {
	if had == nil {
		return nil
	}
	var out FleetCounts
	for _, b := range buckets {
		if b.NotServing == nil || b.Actual == nil {
			continue
		}
		if b.Actual.Prefill == had.Prefill {
			out.Prefill = max(out.Prefill, b.NotServing.Prefill)
		}
		if b.Actual.Decode == had.Decode {
			out.Decode = max(out.Decode, b.NotServing.Decode)
		}
	}
	if out == (FleetCounts{}) {
		return nil
	}
	return &out
}

// countFunc is how many of xs f holds for.
func countFunc[T any](xs []T, f func(T) bool) int {
	n := 0
	for _, x := range xs {
		if f(x) {
			n++
		}
	}
	return n
}

// notServingOf is each role's replicas asked for and not serving over
// samples, beside actualOf's median serving: the median of serving plus
// not serving, less the median serving, floored at 0. Two medians taken
// apart could pair the serving of some cycles with the not serving of
// others, a fleet no cycle had.
func notServingOf(samples []SeriesSample) FleetCounts {
	p, d := make([]int, len(samples)), make([]int, len(samples))
	for i, x := range samples {
		p[i], d[i] = x.Prefill+x.PrefillNotServing, x.Decode+x.DecodeNotServing
	}
	had := actualOf(samples)
	return FleetCounts{Prefill: max(medianInt(p)-had.Prefill, 0), Decode: max(medianInt(d)-had.Decode, 0)}
}

// shapeKnob is x rounded onto the shape grid, a whole token.
func shapeKnob(x float64) float64 {
	g := math.Log(shapeGrid)
	return math.Round(math.Exp(math.Round(math.Log(x)/g) * g))
}

// bucketShape is the lengths samples read, weighted by rate (equally where
// none carried one), and whether any read one.
func bucketShape(samples []SeriesSample) (prompt, output, hit float64, ok bool) {
	var w float64
	for _, x := range samples {
		if !x.HasShape {
			continue
		}
		wx := x.Rate
		if !(wx > 0) {
			wx = 1e-9
		}
		w += wx
		prompt += wx * x.PromptTokens
		output += wx * x.OutputTokens
		hit += wx * x.PrefixHitRate
	}
	if w == 0 {
		return 0, 0, 0, false
	}
	return prompt / w, output / w, hit / w, true
}

// sizedRate is the statistic a bucket of n cycles is sized for, and its
// rate: the one asked, but the mean in place of a p95 of fewer than
// MinPercentileCycles cycles (a nearest-rank p95 of so few is the highest
// cycle). The mean is the less conservative of the two: the bucket is
// flagged FewCyclesForP95.
func (q *dayQuestion) sizedRate(r DayRate, n int) (string, float64) {
	switch {
	case q.sizeFor == SizeForP95 && n >= MinPercentileCycles:
		return SizeForP95, r.P95
	case q.sizeFor == SizeForMax:
		return SizeForMax, r.Max
	}
	return SizeForMean, r.Mean
}

// The terms a DayRefusal names.
const (
	termTTFT       = "ttft"
	termTPOT       = "tpot"
	termDecodeKnee = "decodeKnee"
	// termFleet is a fleet's limit refused at more replicas than one, which
	// the probe at one replica a role did not see.
	termFleet = "fleet"
)

// addRefusal adds r to refused, once per role, term and reason.
func addRefusal(refused *[]DayRefusal, r DayRefusal) {
	for _, x := range *refused {
		if x.Role == r.Role && x.Term == r.Term && x.Reason == r.Reason {
			return
		}
	}
	*refused = append(*refused, r)
}

// dayGroup is one shape's sizing: the sizer of the terms that price, and
// whether decode is sized alone (prefill refused) or nothing is (decode's
// knee refused too).
type dayGroup struct {
	z *sizer
	// decodeOnly sizes decode alone, on decodeLimit; refused sizes nothing.
	decodeOnly, refused bool
	// flags are the day plan's own flags every bucket of the group carries.
	flags []string
	// limits is decode's own limit by replica count, priced once.
	limits map[int]whatif.Result
	// noTTFTHow is how decode alone is sized with no TTFT target, in words;
	// "" with one.
	noTTFTHow string
}

// isRefusal reports whether r is a refusal.
func isRefusal(r whatif.Result) bool {
	_, ok := r.(whatif.Unanswerable)
	return ok
}

// dayRefusalOf is r's refusal as a DayRefusal of role and term, with the hint
// and the effect.
func dayRefusalOf(role, term string, r whatif.Result, effect string) DayRefusal {
	u, _ := r.(whatif.Unanswerable)
	return DayRefusal{Role: role, Term: term, Reason: string(u.Reason), Detail: u.Detail, Hint: refusalHint(term, u.Reason), Effect: effect}
}

// refusalHint is what to do about a refusal, in words.
func refusalHint(term string, reason whatif.Reason) string {
	switch {
	case reason == whatif.NoPrefillCapacity:
		return "The source learned no prefill rate V_P (it saw no prefill pod busy enough). Assume one: --assume-vp, or on the page Assume a term, V_P."
	case reason == whatif.NoFixedLatency:
		return "The source learned no fixed prefill latency a (too few low-load minutes). Assume one: --assume-a, or on the page Assume a term, a."
	case reason == whatif.TargetBelowFloor && term == termTTFT:
		return "The TTFT target is under the TTFT at zero load: no replica count meets it. Raise the TTFT target above the floor in the detail."
	case reason == whatif.TargetBelowFloor:
		return "The TPOT target is under decode's TPOT at zero load: no replica count meets it. Raise the TPOT target."
	case (term == termTPOT || term == termDecodeKnee) && reason == whatif.PinnedDefaultB:
		// The line prices decode's knee too: asking no TPOT does not help.
		return "The decode ITL line rests on the 6 ms default intercept B (PinnedDefaultB), so neither TPOT nor the decode knee is priced: " +
			"unless the detail says no stated B can price it, state B, flagged as assumed, as the detail says: --assume-itl-b floor " +
			"where the detail names the B the floor gives, else a duration at most the floor it names (in the API assume.itlBFromFloor or assume.itlB)."
	case (term == termTPOT || term == termDecodeKnee) && reason == whatif.NoLine:
		// The line prices decode's knee too: asking no TPOT does not help.
		return "The source fitted no decode ITL line it can price (" + string(reason) + "), so neither TPOT nor the decode knee is priced: " +
			"record a window with decode under varied load (docs/guides/record-for-capacity-planner)."
	case term == termDecodeKnee:
		// Whatever the reason, asking no TPOT does not price the knee.
		return "Decode's knee is not priced (" + string(reason) + "): supply the decode input the detail names, or record a window that " +
			"carries it (docs/guides/record-for-capacity-planner); docs/guides/capacity-planner/troubleshooting.md lists every reason."
	case term == termTPOT:
		return "The source fitted no decode ITL line it can price (" + string(reason) + "): record a window with decode under varied load, or ask no TPOT target (--tpot none)."
	}
	return "See the detail; docs/guides/capacity-planner/troubleshooting.md lists every reason."
}

// group probes k's fleet once, at one replica a role, and builds its
// sizing: a TPOT term that refuses is dropped, a TTFT term that refuses
// leaves decode sized alone, and a decode knee that refuses sizes nothing,
// since then no rule bounds decode. Each refusal is added to refused once.
func (q *dayQuestion) group(ctx context.Context, k Knobs, refused *[]DayRefusal) (*dayGroup, error) {
	add := func(r DayRefusal) { addRefusal(refused, r) }
	sc := q.sc
	g := &dayGroup{limits: map[int]whatif.Result{}}
	newSizer := func() *sizer {
		return &sizer{e: q.e, sc: sc, k: k, bound: q.bound, seen: map[FleetCounts]sizeEval{}, plain: map[FleetCounts]sizeEval{}}
	}
	g.z = newSizer()
	if q.noTTFT {
		return q.decodeGroup(ctx, g, refused)
	}
	one := FleetCounts{Prefill: 1, Decode: 1}
	v, err := g.z.at(ctx, one)
	if err != nil {
		return nil, err
	}
	if sc.TPOT != nil && isRefusal(v.decode) {
		tpot := v.decode
		g.flags = append(g.flags, FlagTPOTNotPriced)
		sc.TPOT = nil
		g.z = newSizer()
		if v, err = g.z.at(ctx, one); err != nil {
			return nil, err
		}
		effect := "TPOT is not priced: decode is sized on the fleet limit, TTFT and its knee times headroom."
		if isRefusal(v.decode) {
			effect = "TPOT is not priced, and the decode knee is not priced either: decode is not sized."
		}
		add(dayRefusalOf(roleDecode, termTPOT, tpot, effect))
	}
	if !isRefusal(v.prefill) {
		if isRefusal(v.decode) {
			// With no TPOT, the sizer sets decode's limit only to the
			// decode knee's refusal: no rule bounds decode then, and a count
			// sized on prefill's limit alone would be unbounded. Nothing is
			// sized, as at-replicas refuses the fleet.
			add(dayRefusalOf(roleDecode, termDecodeKnee, v.decode, "Nothing is sized: the decode knee is not priced, so no rule bounds decode."))
			g.refused = true
		}
		return g, nil
	}
	how := howKnee
	if sc.TPOT != nil {
		how = "its TPOT target and its knee times headroom"
	}
	add(dayRefusalOf(rolePrefill, termTTFT, v.prefill, "Prefill is not sized: decode is sized alone, on "+how+"."))
	g.decodeOnly = true
	g.flags = append(g.flags, FlagPrefillNotPriced)
	lim, err := q.decodeLimit(ctx, g, 1)
	if err != nil {
		return nil, err
	}
	if isRefusal(lim) {
		add(dayRefusalOf(roleDecode, termDecodeKnee, lim, "Nothing is sized: decode's own limit is refused too."))
		g.refused = true
	}
	return g, nil
}

// howKnee is decode sized alone on its knee, in words.
const howKnee = "its knee times headroom"

// decodeGroup is g with no TTFT target: prefill is not sized, and decode is
// sized alone on its own limit, as at-replicas judges decode alone with no
// TTFT target. That is no refusal (the plan says so in a note): nothing
// asked was refused. A TPOT target decode's line cannot price is dropped for
// the knee times headroom, and refused; a knee that refuses too sizes
// nothing.
func (q *dayQuestion) decodeGroup(ctx context.Context, g *dayGroup, refused *[]DayRefusal) (*dayGroup, error) {
	add := func(r DayRefusal) { addRefusal(refused, r) }
	how := howKnee
	if g.z.sc.TPOT != nil {
		how = "its TPOT target and its knee times headroom"
	}
	g.decodeOnly = true
	g.flags = append(g.flags, FlagPrefillNotPriced)
	lim, err := q.decodeLimit(ctx, g, 1)
	if err != nil {
		return nil, err
	}
	if g.z.sc.TPOT != nil && isRefusal(lim) {
		tpot := lim
		g.flags = append(g.flags, FlagTPOTNotPriced)
		g.z.sc.TPOT = nil
		g.limits = map[int]whatif.Result{}
		if lim, err = q.decodeLimit(ctx, g, 1); err != nil {
			return nil, err
		}
		how = howKnee
		effect := "TPOT is not priced: decode is sized on its knee times headroom."
		if isRefusal(lim) {
			effect = "TPOT is not priced, and the decode knee is not priced either: decode is not sized."
		}
		add(dayRefusalOf(roleDecode, termTPOT, tpot, effect))
	}
	g.noTTFTHow = how
	if isRefusal(lim) {
		add(dayRefusalOf(roleDecode, termDecodeKnee, lim, "Nothing is sized: decode's own limit is refused too."))
		g.refused = true
	}
	return g, nil
}

// decodeLimit is decode's own limit at d replicas: with a TPOT target, the
// TPOT rate under the operating-point cap (the size's decode limit); with
// none, decode's knee times headroom. Priced once per count.
func (q *dayQuestion) decodeLimit(ctx context.Context, g *dayGroup, d int) (whatif.Result, error) {
	if r, ok := g.limits[d]; ok {
		return r, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	k := g.z.k
	k.Prefill, k.Decode = 1, d
	fl, err := q.e.pricer.Fleet(k)
	if err != nil {
		return nil, err
	}
	var r whatif.Result
	if g.z.sc.TPOT != nil {
		r, _, _ = fl.PlainLimits(g.z.sc.TTFT.Target, g.z.sc.TPOT.Target, g.z.sc.Headroom)
	} else {
		r = fl.DecodeKnee()
		if a, ok := r.(whatif.Answer); ok {
			a.Value *= g.z.sc.Headroom
			r = a
		}
	}
	g.limits[d] = r
	return r, nil
}

// sizeBucket sizes db for rate on g, then judges the fleet it had: priced
// whole, at the same rate, targets and headroom (hadVerdict).
func (q *dayQuestion) sizeBucket(ctx context.Context, g *dayGroup, db *DayBucket, rate float64, refused *[]DayRefusal) error {
	db.Flags = appendNew(db.Flags, g.flags...)
	switch {
	case g.refused:
		db.Status, db.Verdict = BucketRefused, DayVerdictRefused
		return nil
	case g.decodeOnly:
		d, ok, err := smallest(1, q.bound.Decode, func(d int) (bool, error) {
			r, err := q.decodeLimit(ctx, g, d)
			return err == nil && resultReaches(r, rate, false), err
		})
		if err != nil {
			return err
		}
		if !ok {
			r, err := q.decodeLimit(ctx, g, q.bound.Decode)
			if err != nil {
				return err
			}
			x, _ := valueOf(r)
			db.Status, db.UnmetRole, db.Verdict = BucketUnmet, roleDecode, DayVerdictUnder
			db.Unmet = &Refusal{Reason: ReasonReplicaBound, Detail: fmt.Sprintf("At %d decode replicas, the bound, decode's limit is %.4g r/s, short of %.4g r/s",
				q.bound.Decode, x, rate)}
			return nil
		}
		lim := figureOf(g.limits[d], unitRate, false)
		db.Status, db.Needed, db.Limit = BucketSized, &DayNeeded{Decode: d, AtLeast: true}, &lim
		db.Flags = appendNew(db.Flags, lim.Flags...)
		return q.hadVerdict(ctx, g, db, rate)
	}
	z := g.z
	z.rate = rate
	res, err := z.search(ctx)
	if err != nil {
		return err
	}
	if u := res.Unmet; u != nil {
		if u.Refused.Reason != ReasonReplicaBound {
			// A refusal the probe at one replica a role did not see: it
			// holds at more replicas, whatever the rate. It is the
			// source's, not the bucket's.
			addRefusal(refused, DayRefusal{Role: u.Role, Term: termFleet, Reason: u.Refused.Reason, Detail: u.Refused.Detail,
				Hint:   refusalHint(termFleet, whatif.Reason(u.Refused.Reason)),
				Effect: "These buckets are not sized: the source refuses the fleet's limit at more replicas than one."})
			db.Status, db.Verdict = BucketRefused, DayVerdictRefused
			db.Flags = appendNew(db.Flags, FlagFleetRefused)
			return nil
		}
		db.Status, db.Unmet, db.UnmetRole, db.Verdict = BucketUnmet, &u.Refused, u.Role, DayVerdictUnder
		db.Flags = appendNew(db.Flags, u.AtBound.Limit.Flags...)
		return nil
	}
	n := res.Answer.Fleet
	p := n.Prefill
	lim := figureOf(z.seen[n].fleet, unitRate, false)
	db.Status, db.Needed, db.Limit = BucketSized, &DayNeeded{Prefill: &p, Decode: n.Decode}, &lim
	db.Flags = appendNew(db.Flags, lim.Flags...)
	return q.hadVerdict(ctx, g, db, rate)
}

// hadVerdict judges the fleet a bucket had, as one fleet, at the bucket's
// rate: under when its limit does not reach the rate; over when it does and
// a fleet of one replica fewer of either role does too (as at-replicas
// judges over); else match. The fewest replicas found (Needed) are one fleet
// that serves the rate, the fewest in all: another mix may serve it as well,
// and the fleet it had is judged on its own mix, never role by role against
// Needed. With prefill not priced, decode is judged alone.
func (q *dayQuestion) hadVerdict(ctx context.Context, g *dayGroup, db *DayBucket, rate float64) error {
	a := *db.Actual
	serves := func(n FleetCounts) (bool, whatif.Result, error) {
		if n.Decode < 1 || (!g.decodeOnly && n.Prefill < 1) {
			return false, nil, nil
		}
		if g.decodeOnly {
			r, err := q.decodeLimit(ctx, g, n.Decode)
			return err == nil && resultReaches(r, rate, false), r, err
		}
		v, err := g.z.at(ctx, n)
		if err != nil {
			return false, nil, err
		}
		g.z.rate = rate
		return g.z.meets(v), v.fleet, nil
	}
	ok, r, err := serves(a)
	if err != nil {
		return err
	}
	if r != nil {
		f := figureOf(r, unitRate, false)
		db.HadLimit = &f
	}
	if !ok {
		db.Verdict = DayVerdictUnder
		return nil
	}
	fewer := []FleetCounts{{Prefill: a.Prefill, Decode: a.Decode - 1}}
	if !g.decodeOnly {
		fewer = append(fewer, FleetCounts{Prefill: a.Prefill - 1, Decode: a.Decode})
	}
	for _, n := range fewer {
		ok, _, err := serves(n)
		if err != nil {
			return err
		}
		if ok {
			db.Verdict = DayVerdictOver
			return nil
		}
	}
	db.Verdict = DayVerdictMatch
	return nil
}

// plan sizes every bucket.
func (q *dayQuestion) plan(ctx context.Context) (*DayPlanResponse, error) {
	series := q.e.series
	buckets := bucketize(series, q.step)
	cycle := sampleStep(series, q.step).Seconds()
	out := &DayPlanResponse{Step: q.step.Seconds(), SizeFor: q.sizeFor, SizeForSource: "default", MinRequests: q.minRequests,
		TPOT: q.sc.TPOT, Headroom: q.sc.Headroom, MaxReplicas: q.bound, Refused: []DayRefusal{},
		From: buckets[0].start, To: buckets[len(buckets)-1].start.Add(q.step)}
	if q.sizeFromReq {
		out.SizeForSource = "request"
	}
	if !q.noTTFT {
		t := q.sc.TTFT
		out.TTFT = &t
	}
	stated := !q.sc.Mix.Observed
	var lo, hi [2]float64
	groups := map[[2]float64]*dayGroup{}
	groupFor := func(k Knobs) (*dayGroup, error) {
		key := [2]float64{k.PromptTokens, k.OutputTokens}
		if g := groups[key]; g != nil {
			return g, nil
		}
		g, err := q.group(ctx, k, &out.Refused)
		if err != nil {
			return nil, err
		}
		groups[key] = g
		return g, nil
	}
	// sized is each sized bucket's group and rate, by its index: what the
	// fleet that holds an hour is priced again at.
	sized := map[int]sizedBucket{}
	gpus := FleetCounts{}
	for _, x := range series {
		gpus.Prefill, gpus.Decode = max(gpus.Prefill, x.PrefillGPUs), max(gpus.Decode, x.DecodeGPUs)
	}
	for _, b := range buckets {
		db := DayBucket{Start: b.start, Samples: len(b.samples), Shape: DayShape{Source: "window"}}
		if len(b.samples) == 0 {
			db.Status = BucketNoData
			out.Buckets = append(out.Buckets, db)
			continue
		}
		r := rateOf(b.samples)
		db.Rate = &r
		db.Requests = r.Mean * cycle * float64(len(b.samples))
		a := actualOf(b.samples)
		db.Actual = &a
		if n := notServingOf(b.samples); n != (FleetCounts{}) {
			db.NotServing = &n
		}
		if slices.ContainsFunc(b.samples, func(x SeriesSample) bool { return !x.RateKnown }) {
			db.Flags = append(db.Flags, FlagRateFromDecodeRows)
		}
		if slices.ContainsFunc(b.samples, func(x SeriesSample) bool { return x.OtherPods > 0 }) {
			db.Flags = append(db.Flags, FlagOtherVariantsServed)
		}
		k := q.k
		if p, o, h, ok := bucketShape(b.samples); ok {
			for i, v := range []float64{p, o} {
				if lo[i] == 0 || v < lo[i] {
					lo[i] = v
				}
				hi[i] = max(hi[i], v)
			}
			db.Shape.PrefixHitRate = ptr(h)
			if !stated {
				k.PromptTokens, k.OutputTokens = shapeKnob(p), shapeKnob(o)
				db.Shape.Source = "bucket"
				db.Shape.PromptTokens, db.Shape.OutputTokens = ptr(p), ptr(o)
			}
		} else if !stated {
			db.Flags = append(db.Flags, FlagWindowShape)
		}
		if stated {
			db.Shape.Source = "stated"
			if k.PromptTokens > 0 {
				db.Shape.PromptTokens = ptr(k.PromptTokens)
			}
			if k.OutputTokens > 0 {
				db.Shape.OutputTokens = ptr(k.OutputTokens)
			}
		}
		if db.Requests < q.minRequests || !(r.Mean > 0) {
			db.Status = BucketTooLittleData
			out.Buckets = append(out.Buckets, db)
			continue
		}
		stat, rate := q.sizedRate(r, len(b.samples))
		if q.sizeFor == SizeForP95 && stat != SizeForP95 {
			db.Flags = append(db.Flags, FlagFewCyclesForP95)
		}
		db.SizedFor, db.SizedRate = stat, ptr(rate)
		g, err := groupFor(k)
		if err != nil {
			return nil, err
		}
		if err := q.sizeBucket(ctx, g, &db, rate, &out.Refused); err != nil {
			return nil, err
		}
		if db.Needed != nil {
			sized[len(out.Buckets)] = sizedBucket{g: g, rate: rate}
		}
		out.Buckets = append(out.Buckets, db)
	}
	q.countRefused(out)
	if stated {
		k := q.k
		outside := func(v float64, i int) bool { return v > 0 && hi[i] > 0 && (v < lo[i] || v > hi[i]) }
		if outside(k.PromptTokens, 0) || outside(k.OutputTokens, 1) {
			for i := range out.Buckets {
				out.Buckets[i].Flags = appendNew(out.Buckets[i].Flags, FlagExtrapolated)
			}
			out.Notes = append(out.Notes, fmt.Sprintf("EXTRAPOLATED: the stated lengths lie outside what the window's buckets observed "+
				"(prompt %.0f to %.0f tokens, output %.0f to %.0f): every bucket is priced at a shape the source never served.", lo[0], hi[0], lo[1], hi[1]))
		}
	}
	out.Hours = hoursOf(series, out.Buckets)
	out.Summary = summaryOf(out.Buckets, out.Hours, q.step, gpus)
	if err := q.confirmHolding(ctx, out, sized); err != nil {
		return nil, err
	}
	for _, g := range groups {
		out.Searched += g.z.searched() + len(g.limits)
	}
	out.Notes = append(out.Notes,
		"Steady state: each bucket is sized as if its rate held, with no scale-up lag. A fleet that follows the plan must start replicas one start time before the rate arrives.",
		fmt.Sprintf("Each bucket is sized for the %s of its cycles' rates. A p95 needs %d cycles in the bucket; with fewer the bucket is sized for its mean, "+
			"which is less conservative (FewCyclesForP95).", q.sizeFor, MinPercentileCycles),
		fmt.Sprintf("Needed is the fewest replicas whose fleet limit, at the SLO and headroom %.2f, reaches the rate: the fewest in all, ties to fewer decode "+
			"(POST /v1/size's search, whatifapi.SmallestFleet). Another mix may serve the rate too. The fleet the bucket had is judged as one fleet at the same rate: "+
			"under when its limit falls short, over when one replica fewer of either role would still serve it.", q.sc.Headroom),
		"An hour's need, and the peak, is a fleet that holds every sized bucket in it, priced again at each bucket's rate and shape: the fewest of "+
			"the buckets' own fleets that holds them all, else each role's most over them. It is not searched for the fewest: a smaller mix may hold "+
			"them too. The busiest bucket's own fleet is given beside it (busiest); it need not hold a bucket of a lower rate but longer requests.",
		fmt.Sprintf("A bucket with fewer than %.0f requests is not sized (too little data); a bucket no cycle fell in has no data.", q.minRequests),
		"Had is the median serving pods of the priced prefill and decode variants over the bucket's cycles; the rate is the model's. "+
			"A bucket in which another variant also served is flagged OtherVariantsServed. The prefix hit rate is reported, not priced.")
	if n := countFunc(out.Buckets, func(b DayBucket) bool { return b.NotServing != nil }); n > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("Had counts the replicas serving. In %d bucket(s) more were asked for and not serving "+
			"(notServing: replicas that exist and are not ready, starting, unschedulable, held by an admission gate such as Kueue's, or a "+
			"rolling update's surge). Each role's had and (+n) of an hour come from its bucket of the fewest of that role serving; the verdict judges what served. A replica whose pod was never "+
			"created (a ResourceQuota refusing it) is not in the recording.", n))
	}
	if q.noTTFT {
		how := "its own limit"
		for _, g := range groups {
			how = g.noTTFTHow
			break
		}
		out.Notes = append(out.Notes, "No TTFT target: the request asks none and the policy sets none, so prefill is not sized "+
			"(PrefillNotPriced) and decode is sized alone, on "+how+". State one to size prefill: --ttft 2s@p90, or in the API slo.ttft.target.")
	}
	if q.noTTFT || slices.ContainsFunc(out.Refused, func(r DayRefusal) bool { return r.Term == termTTFT }) {
		out.Notes = append(out.Notes, "Where prefill is not priced, decode is sized on its own limit alone: a lower bound (at least). "+
			"The fleet's limit, which decode admission can hold down, is not priced.")
	}
	return out, nil
}

// sizedBucket is a sized bucket's group and the rate it was sized for.
type sizedBucket struct {
	g    *dayGroup
	rate float64
}

// holdingFleet is a fleet that holds every one of bs, each a sized bucket's
// need: each role's most over them. Where any sized decode alone, the need
// is a lower bound (AtLeast), and its prefill the most of the buckets that
// priced it, absent where none did. Nil with none.
func holdingFleet(bs []*DayBucket) *DayNeeded {
	var out *DayNeeded
	var prefill *int
	for _, b := range bs {
		if b.Needed == nil {
			continue
		}
		if out == nil {
			out = &DayNeeded{}
		}
		out.Decode = max(out.Decode, b.Needed.Decode)
		out.AtLeast = out.AtLeast || b.Needed.AtLeast
		if p := b.Needed.Prefill; p != nil && (prefill == nil || *p > *prefill) {
			prefill = ptrInt(*p)
		}
	}
	if out != nil {
		out.Prefill = prefill
	}
	return out
}

// ptrInt is a pointer to n.
func ptrInt(n int) *int { return &n }

// holds reports whether a fleet of p prefill and d decode replicas serves
// every bucket of idx at its rate, priced again on its own group: decode
// alone on its limit where the group sized decode alone, else the fleet
// whole. A bucket not sized is skipped.
func (q *dayQuestion) holds(ctx context.Context, p, d int, idx []int, sized map[int]sizedBucket) (bool, error) {
	for _, i := range idx {
		s, ok := sized[i]
		if !ok {
			continue
		}
		if s.g.decodeOnly {
			r, err := q.decodeLimit(ctx, s.g, d)
			if err != nil {
				return false, err
			}
			if !resultReaches(r, s.rate, false) {
				return false, nil
			}
			continue
		}
		v, err := s.g.z.at(ctx, FleetCounts{Prefill: p, Decode: d})
		if err != nil {
			return false, err
		}
		s.g.z.rate = s.rate
		if !s.g.z.decodeMeets(v) || !s.g.z.meets(v) {
			return false, nil
		}
	}
	return true, nil
}

// confirmHolding sets each hour's need, and the peak, to a fleet priced
// again at every sized bucket it is to hold (holding).
func (q *dayQuestion) confirmHolding(ctx context.Context, out *DayPlanResponse, sized map[int]sizedBucket) error {
	byHour := map[time.Time][]int{}
	var all []int
	for i := range out.Buckets {
		if _, ok := sized[i]; !ok {
			continue
		}
		h := out.Buckets[i].Start.Truncate(time.Hour)
		byHour[h] = append(byHour[h], i)
		all = append(all, i)
	}
	for i := range out.Hours {
		hr := &out.Hours[i]
		if hr.Needed == nil {
			continue
		}
		n, ok, err := q.holding(ctx, out.Buckets, byHour[hr.Start], sized)
		if err != nil {
			return err
		}
		hr.Needed, hr.NotConfirmed = n, !ok
	}
	if out.Summary.PeakNeeded != nil {
		n, ok, err := q.holding(ctx, out.Buckets, all, sized)
		if err != nil {
			return err
		}
		out.Summary.PeakNeeded, out.Summary.PeakNotConfirmed = n, !ok
	}
	return nil
}

// holding is a fleet that holds every sized bucket of idx, and whether it
// was confirmed to. The buckets' own fleets are tried first, fewest
// replicas first and the busiest bucket's first among equals: the search
// breaks a tie between mixes toward fewer decode, so two buckets of one
// shape can need 2+1 and 1+2, and the busier one's 1+2 holds both, where
// each role's most, 2+2, buys a replica no bucket needs. Only when none of
// them holds every bucket is each role's most taken (holdingFleet), which
// holds them where a fleet's limit rises with its replicas, and is not
// confirmed where it does not. A bucket sized decode alone is held on
// decode; where any was, the need is a lower bound (AtLeast).
func (q *dayQuestion) holding(ctx context.Context, buckets []DayBucket, idx []int, sized map[int]sizedBucket) (*DayNeeded, bool, error) {
	bs := make([]*DayBucket, 0, len(idx))
	for _, i := range idx {
		bs = append(bs, &buckets[i])
	}
	most := holdingFleet(bs)
	if most == nil {
		return nil, true, nil
	}
	priced := slices.ContainsFunc(bs, func(b *DayBucket) bool { return b.Needed.Prefill != nil })
	cands := slices.Clone(bs)
	slices.SortStableFunc(cands, func(a, b *DayBucket) int {
		ta, tb := a.Needed.Decode+valueOrZeroInt(a.Needed.Prefill), b.Needed.Decode+valueOrZeroInt(b.Needed.Prefill)
		if ta != tb {
			return ta - tb
		}
		switch {
		case *a.SizedRate > *b.SizedRate:
			return -1
		case *a.SizedRate < *b.SizedRate:
			return 1
		}
		return 0
	})
	for _, c := range cands {
		n := c.Needed
		if priced && n.Prefill == nil {
			continue
		}
		ok, err := q.holds(ctx, valueOrZeroInt(n.Prefill), n.Decode, idx, sized)
		if err != nil {
			return nil, false, err
		}
		if ok {
			out := &DayNeeded{Decode: n.Decode, AtLeast: most.AtLeast}
			if n.Prefill != nil {
				out.Prefill = ptrInt(*n.Prefill)
			}
			return out, true, nil
		}
	}
	ok, err := q.holds(ctx, valueOrZeroInt(most.Prefill), most.Decode, idx, sized)
	return most, ok, err
}

// valueOrZeroInt is *p, or 0 when p is nil.
func valueOrZeroInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// countRefused counts, for each refusal, the buckets of its role and term
// that were sized or refused under it.
func (q *dayQuestion) countRefused(out *DayPlanResponse) {
	for i := range out.Refused {
		r := &out.Refused[i]
		flag := map[string]string{termTPOT: FlagTPOTNotPriced, termTTFT: FlagPrefillNotPriced, termFleet: FlagFleetRefused}[r.Term]
		for _, b := range out.Buckets {
			if (flag != "" && slices.Contains(b.Flags, flag)) || (r.Term == termDecodeKnee && b.Status == BucketRefused) {
				r.Buckets++
			}
		}
	}
}

// appendNew appends each of xs not already in s.
func appendNew(s []string, xs ...string) []string {
	for _, x := range xs {
		if !slices.Contains(s, x) {
			s = append(s, x)
		}
	}
	return s
}

// peakBucket reports whether b is a sized bucket of a higher sized rate than
// best (nil, none yet), or lower with low.
func peakBucket(b DayBucket, best *DayBucket, low bool) bool {
	if b.Needed == nil || b.SizedRate == nil {
		return false
	}
	if best == nil {
		return true
	}
	if low {
		return *b.SizedRate < *best.SizedRate
	}
	return *b.SizedRate > *best.SizedRate
}

// hoursOf is the buckets by UTC hour. An hour's need is a fleet that holds
// every sized bucket in it: here each role's most over them (holdingFleet),
// which the plan then replaces with the fewest of the buckets' own fleets
// that holds them all, priced again at each (confirmHolding). The fleet its
// busiest bucket needed need not hold a bucket of a lower rate and longer
// requests, which can need more of one role; it is kept beside the need
// (Busiest).
func hoursOf(series []SeriesSample, buckets []DayBucket) []DayHour {
	var out []DayHour
	byHour := map[time.Time][]SeriesSample{}
	for _, x := range series {
		h := x.At.UTC().Truncate(time.Hour)
		byHour[h] = append(byHour[h], x)
	}
	type tally struct {
		under, over, match, refused, withData int
		peak                                  *DayBucket
		sized, had                            []*DayBucket
	}
	var t []tally
	for i := range buckets {
		b := &buckets[i]
		h := b.Start.Truncate(time.Hour)
		if len(out) == 0 || !out[len(out)-1].Start.Equal(h) {
			out = append(out, DayHour{Start: h})
			t = append(t, tally{})
			if s := byHour[h]; len(s) > 0 {
				r := rateOf(s)
				out[len(out)-1].Rate = &r
			}
		}
		hr, c := &out[len(out)-1], &t[len(t)-1]
		if b.Actual != nil {
			c.withData++
			if hr.Actual == nil {
				a := *b.Actual
				hr.Actual = &a
			} else {
				hr.Actual.Prefill, hr.Actual.Decode = min(hr.Actual.Prefill, b.Actual.Prefill), min(hr.Actual.Decode, b.Actual.Decode)
			}
		}
		if b.Actual != nil {
			c.had = append(c.had, b)
		}
		switch b.Status {
		case BucketUnmet:
			hr.Unmet++
			c.under++
		case BucketRefused:
			c.refused++
		}
		if b.Needed == nil {
			continue
		}
		hr.Sized++
		switch b.Verdict {
		case DayVerdictUnder:
			c.under++
		case DayVerdictOver:
			c.over++
		case DayVerdictMatch:
			c.match++
		}
		if peakBucket(*b, c.peak, false) {
			c.peak = b
		}
		c.sized = append(c.sized, b)
	}
	for i := range out {
		c := t[i]
		out[i].NotServing = hourNotServing(out[i].Actual, c.had)
		if c.peak != nil {
			n := *c.peak.Needed
			out[i].Busiest = &n
			out[i].Needed = holdingFleet(c.sized)
		}
		switch {
		case c.under > 0:
			out[i].Verdict = DayVerdictUnder
		case c.refused > 0 && c.refused == c.withData:
			out[i].Verdict = DayVerdictRefused
		case c.over > 0 && c.match == 0:
			out[i].Verdict = DayVerdictOver
		case c.match > 0 || c.over > 0:
			out[i].Verdict = DayVerdictMatch
		}
	}
	return out
}

// summaryOf is the plan's summary. The peak need is a fleet that holds every
// sized bucket (holdingFleet); the busiest and the least are the fleets of
// the sized buckets of the highest and the lowest sized rate. Prefill's
// replica-hours, and the GPU-hours, are given only when every sized bucket
// priced prefill: over fewer, they would compare a subset under the label
// of the whole.
func summaryOf(buckets []DayBucket, hours []DayHour, step time.Duration, gpus FleetCounts) DaySummary {
	s := DaySummary{UnderHours: []time.Time{}, OverHours: []time.Time{}, RefusedHours: []time.Time{}}
	h := step.Hours()
	var need, had RoleHours
	var needP, hadP float64
	var peak, least *DayBucket
	var all []*DayBucket
	for i := range buckets {
		b := &buckets[i]
		switch b.Status {
		case BucketNoData:
			s.Counts.NoData++
		case BucketTooLittleData:
			s.Counts.TooLittleData++
		case BucketUnmet:
			s.Counts.Unmet++
		case BucketRefused:
			s.Counts.Refused++
		}
		if b.Actual != nil {
			s.HadReplicaHoursAll.Prefill = ptr(valueOr(s.HadReplicaHoursAll.Prefill) + float64(b.Actual.Prefill)*h)
			s.HadReplicaHoursAll.Decode += float64(b.Actual.Decode) * h
		}
		if b.Needed == nil {
			continue
		}
		s.Counts.Sized++
		switch b.SizedFor {
		case SizeForMean:
			s.Counts.SizedForMean++
		case SizeForP95:
			s.Counts.SizedForP95++
		case SizeForMax:
			s.Counts.SizedForMax++
		}
		switch b.Verdict {
		case DayVerdictUnder:
			s.Counts.Under++
		case DayVerdictOver:
			s.Counts.Over++
		case DayVerdictMatch:
			s.Counts.Match++
		}
		if peakBucket(*b, peak, false) {
			peak = b
		}
		if peakBucket(*b, least, true) {
			least = b
		}
		all = append(all, b)
		n := *b.Needed
		need.Decode += float64(n.Decode) * h
		had.Decode += float64(b.Actual.Decode) * h
		if n.Prefill == nil {
			s.Counts.PrefillNotPriced++
			continue
		}
		needP += float64(*n.Prefill) * h
		hadP += float64(b.Actual.Prefill) * h
	}
	if s.Counts.Sized > 0 {
		p, l := *peak.Needed, *least.Needed
		s.BusiestNeeded, s.MinNeeded = &p, &l
		s.PeakNeeded = holdingFleet(all)
		if s.Counts.PrefillNotPriced == 0 {
			need.Prefill, had.Prefill = ptr(needP), ptr(hadP)
		}
		s.NeededReplicaHours, s.HadReplicaHours = &need, &had
	}
	if gpus.Prefill > 0 && gpus.Decode > 0 {
		g := gpus
		s.GPUsPerReplica = &g
		gh := func(r RoleHours) *float64 {
			return ptr(valueOr(r.Prefill)*float64(g.Prefill) + r.Decode*float64(g.Decode))
		}
		s.HadGPUHoursAll = gh(s.HadReplicaHoursAll)
		// A need with prefill unpriced in any bucket has no GPU-hours: half
		// a fleet's GPUs set beside a whole fleet's would read as waste.
		if s.Counts.Sized > 0 && s.Counts.PrefillNotPriced == 0 {
			s.NeededGPUHours, s.HadGPUHours = gh(need), gh(had)
		}
	}
	for _, hr := range hours {
		switch hr.Verdict {
		case DayVerdictUnder:
			s.UnderHours = append(s.UnderHours, hr.Start)
		case DayVerdictOver:
			s.OverHours = append(s.OverHours, hr.Start)
		case DayVerdictRefused:
			s.RefusedHours = append(s.RefusedHours, hr.Start)
		}
	}
	return s
}

// valueOr is *p, or 0 when p is nil.
func valueOr(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
