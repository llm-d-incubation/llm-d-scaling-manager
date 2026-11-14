package whatifapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

// SchemaVersion is the API's own schema version. Removing, renaming or
// retyping a key, or changing its meaning, bumps it; the pinned key list in
// testdata/schema-v<N>.txt changes either way (TestSchemaIsPinned).
const SchemaVersion = 1

// The wire types. A figure (Figure) is an answer or a refusal, never both: a
// refused figure has Refused and no number. Inputs to a question (the rate a
// point is priced at, the target a rate is read at) sit beside the figure,
// never inside it.

// InputsRequest is POST /v1/inputs: which model, from which source. With no
// source it is a Prometheus window of the last hour, ending now.
type InputsRequest struct {
	Model  string         `json:"model,omitempty"`
	Source *SourceRequest `json:"source,omitempty"`
	// PrefillVariant and DecodeVariant name the variants when the model has
	// several of a role (the CLI's --prefill-variant and --variant).
	PrefillVariant string `json:"prefillVariant,omitempty"`
	DecodeVariant  string `json:"decodeVariant,omitempty"`
}

// The source kinds.
const (
	SourcePrometheus = "prometheus"
	SourceRecording  = "recording"
)

// SourceRequest names a source. A Prometheus window gives its namespace and
// its from and to (RFC 3339; both absent is the last hour, ending now); the
// Prometheus URL and the role map are the server's flags. A recording gives
// its file name in the server's recordings directory, never a path, and the
// cycle to read it at (0 is its last).
type SourceRequest struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Name      string `json:"name,omitempty"`
	Cycle     int    `json:"cycle,omitempty"`
}

// InputsResponse is a source resolved: every term the plans price from, each
// with its source or its refusal, under InputsID.
type InputsResponse struct {
	SchemaVersion int        `json:"schemaVersion"`
	WhatifSchema  string     `json:"whatifSchema"`
	InputsID      string     `json:"inputsId"`
	Model         string     `json:"model"`
	Namespace     string     `json:"namespace"`
	Source        SourceInfo `json:"source"`
	Fleet         FleetInfo  `json:"fleet"`
	Observed      Observed   `json:"observed"`
	Policy        PolicyInfo `json:"policy"`
	Terms         []Term     `json:"terms"`
	// Assumed is every input the source did not give, in words.
	Assumed []string `json:"assumed"`
}

// SourceInfo is what the inputs were read from. Prometheus is the URL's
// scheme and host alone, and From and To the window, with a Prometheus
// source; Name the file, with a recording.
type SourceInfo struct {
	Kind        string     `json:"kind"`
	Name        string     `json:"name,omitempty"`
	Prometheus  string     `json:"prometheus,omitempty"`
	From        *time.Time `json:"from,omitempty"`
	To          *time.Time `json:"to,omitempty"`
	Cycle       int        `json:"cycle"`
	TotalCycles int        `json:"totalCycles"`
	FirstAt     time.Time  `json:"firstAt"`
	LastAt      time.Time  `json:"lastAt"`
}

// FleetInfo is the fleet as the source read it. Prefill is absent with no
// prefill variant.
type FleetInfo struct {
	Prefill *RoleInfo `json:"prefill,omitempty"`
	Decode  RoleInfo  `json:"decode"`
}

// RoleInfo is one role's variant and its replica count at the cycle.
type RoleInfo struct {
	Variant  string `json:"variant"`
	Replicas int    `json:"replicas"`
}

// Observed is the traffic the source saw.
type Observed struct {
	// Rate is the observed request rate; refused where the source does not
	// carry it, and a plan then defaults to the operating point.
	Rate         Figure `json:"rate"`
	PromptTokens Figure `json:"promptTokens"`
	OutputTokens Figure `json:"outputTokens"`
	PromptCV     Figure `json:"promptCV"`
	// Buckets are the fixed prompt buckets, each named by its upper edge,
	// and PromptBucket the one the mean prompt falls in.
	Buckets      []Bucket `json:"buckets"`
	PromptBucket string   `json:"promptBucket,omitempty"`
	// Mix is the share of requests per bucket, or why the source cannot say.
	Mix ObservedMix `json:"mix"`
}

// Bucket is one fixed prompt bucket: prompts above the bucket below, up to
// UpperTokens.
type Bucket struct {
	Name        string `json:"name"`
	UpperTokens int    `json:"upperTokens"`
}

// ObservedMix is the requests' shares per bucket, or the refusal.
type ObservedMix struct {
	Shares  []BucketShare `json:"shares,omitempty"`
	Refused *Refusal      `json:"refused,omitempty"`
}

// BucketShare is one bucket's share of the requests.
type BucketShare struct {
	Bucket string  `json:"bucket"`
	Share  float64 `json:"share"`
}

// PolicyInfo is the policy's SLO and headroom, the plan's defaults.
type PolicyInfo struct {
	TTFT        *PolicyObjective `json:"ttft,omitempty"`
	TPOT        *PolicyObjective `json:"tpot,omitempty"`
	Headroom    float64          `json:"headroom"`
	FlowControl string           `json:"flowControl"`
}

// PolicyObjective is one SLO objective: its statistic and target.
type PolicyObjective struct {
	Statistic string  `json:"statistic"`
	Seconds   float64 `json:"seconds"`
}

// Term is one input the plans price from: its figure and where it came from.
type Term struct {
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
	Figure
}

// Figure is one priced figure: Value, or Refused, never both. Band is the
// bootstrap band over the ITL line's readings (the 10th to the 90th
// percentile); Sensitivity the span over V_P and f either way.
type Figure struct {
	Value           *float64 `json:"value,omitempty"`
	Unit            string   `json:"unit,omitempty"`
	Band            *Band    `json:"band,omitempty"`
	Sensitivity     *Span    `json:"sensitivity,omitempty"`
	Floor           *float64 `json:"floor,omitempty"`
	Uncapped        *float64 `json:"uncapped,omitempty"`
	PrefillOnlyRate *float64 `json:"prefillOnlyRate,omitempty"`
	BoundBy         []string `json:"boundBy,omitempty"`
	Flags           []string `json:"flags,omitempty"`
	Refused         *Refusal `json:"refused,omitempty"`
}

// Band is a bootstrap band, Lo to Hi.
type Band struct {
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
}

// Span is a figure's span over V_P and f. A corner the model refused leaves
// the worse side with no number: LoRefused (a rate) or HiRefused (a TTFT)
// names the reason in its place.
type Span struct {
	Lo        *float64 `json:"lo,omitempty"`
	Hi        *float64 `json:"hi,omitempty"`
	LoRefused string   `json:"loRefused,omitempty"`
	HiRefused string   `json:"hiRefused,omitempty"`
}

// Refusal is whatif.Unanswerable on the wire.
type Refusal struct {
	Reason             string   `json:"reason"`
	Detail             string   `json:"detail"`
	Flags              []string `json:"flags,omitempty"`
	AssumedTermDecides bool     `json:"assumedTermDecides,omitempty"`
}

// PlanRequest is POST /v1/plan: a scenario priced against inputsId. Every
// field but InputsID defaults: the recorded replicas, the operating point's
// rate, the observed mix, the policy's SLO and headroom.
type PlanRequest struct {
	InputsID string       `json:"inputsId"`
	Fleet    *FleetKnob   `json:"fleet,omitempty"`
	Traffic  *Traffic     `json:"traffic,omitempty"`
	SLO      *SLORequest  `json:"slo,omitempty"`
	Assume   *Assume      `json:"assume,omitempty"`
	Headroom *float64     `json:"headroom,omitempty"`
	Views    *ViewRequest `json:"views,omitempty"`
}

// FleetKnob is the replicas per role.
type FleetKnob struct {
	Prefill *int `json:"prefill,omitempty"`
	Decode  *int `json:"decode,omitempty"`
}

// Traffic is the rate per class, the mix and the arrivals. In v1 the critical
// rate is the total rate, and regular and batch must be 0.
type Traffic struct {
	Classes    *Classes `json:"classes,omitempty"`
	Mix        *Mix     `json:"mix,omitempty"`
	ArrivalCV2 *float64 `json:"arrivalCV2,omitempty"`
}

// Classes is the rate per traffic class.
type Classes struct {
	Critical *ClassRate `json:"critical,omitempty"`
	Regular  *ClassRate `json:"regular,omitempty"`
	Batch    *ClassRate `json:"batch,omitempty"`
}

// ClassRate is one class's rate, requests per second over the model.
type ClassRate struct {
	Rate *float64 `json:"rate,omitempty"`
}

// Mix is the traffic's lengths: "observed", or stated mean lengths, priced as
// the CLI's --assume-input-tokens and --assume-output-tokens (flagged
// AssumedShape). A length left out stays observed.
type Mix struct {
	Observed     bool    `json:"-"`
	PromptTokens float64 `json:"promptTokens,omitempty"`
	OutputTokens float64 `json:"outputTokens,omitempty"`
}

// mixObserved is the observed mix, as the request and the scenario write it.
const mixObserved = "observed"

// UnmarshalJSON reads "observed" or an object of stated lengths.
func (m *Mix) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		if s != mixObserved {
			return errors.New(`mix: want "observed" or {"promptTokens": N, "outputTokens": N}`)
		}
		*m = Mix{Observed: true}
		return nil
	}
	type stated Mix
	var v stated
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	*m = Mix(v)
	return nil
}

// MarshalJSON writes "observed", or the stated lengths.
func (m Mix) MarshalJSON() ([]byte, error) {
	if m.Observed || (m.PromptTokens == 0 && m.OutputTokens == 0) {
		return json.Marshal(mixObserved)
	}
	type stated Mix
	return json.Marshal(stated(m))
}

// SLORequest is the targets: each objective's statistic and target.
type SLORequest struct {
	TTFT *Objective `json:"ttft,omitempty"`
	TPOT *Objective `json:"tpot,omitempty"`
}

// Objective is one SLO objective. Statistic is "mean" or "pNN" (p50 to
// p99.9), or for TPOT "none", which asks no TPOT; Target is seconds.
// BucketTargets are the TTFT targets per bucket, drawn on the "if all traffic
// were bucket b" view only.
type Objective struct {
	Statistic     string             `json:"statistic,omitempty"`
	Target        *float64           `json:"target,omitempty"`
	BucketTargets map[string]float64 `json:"bucketTargets,omitempty"`
}

// Assume is a typed value for a learned term, as the CLI's --assume-* flags
// give it, each raising the engine's own flag. A and RouterHop are seconds.
type Assume struct {
	VP        *float64 `json:"vp,omitempty"`
	A         *float64 `json:"a,omitempty"`
	PromptCV  *float64 `json:"promptCV,omitempty"`
	TPOTRatio *float64 `json:"tpotRatio,omitempty"`
	RouterHop *float64 `json:"routerHop,omitempty"`
}

// ViewRequest switches the optional views on.
type ViewRequest struct {
	AllTrafficPerBucket bool `json:"allTrafficPerBucket,omitempty"`
}

// PlanResponse is a scenario priced: everything the planner's screen draws.
type PlanResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	WhatifSchema  string `json:"whatifSchema"`
	ScenarioID    string `json:"scenarioId"`
	InputsID      string `json:"inputsId"`
	// Banner is what every TTFT figure says of itself.
	Banner   string    `json:"banner"`
	Scenario Scenario  `json:"scenario"`
	TTFT     TTFTView  `json:"ttft"`
	TPOT     *TPOTView `json:"tpot,omitempty"`
	Limits   Limits    `json:"limits"`
	Marginal Marginal  `json:"marginal"`
	// Flags lists every flag in the response once, with where it is.
	Flags []FlagUse `json:"flags"`
	// Assumed is every assumed input of the scenario's fleet, in words.
	Assumed []string `json:"assumed"`
}

// Scenario is the request with its defaults applied: what was priced.
type Scenario struct {
	InputsID string      `json:"inputsId"`
	Fleet    FleetCounts `json:"fleet"`
	// Rate is the rate asked; absent, the chosen rate is the operating
	// point, headroom times the lower knee (limits.chosen.rate).
	Rate       *float64           `json:"rate,omitempty"`
	Mix        Mix                `json:"mix"`
	ArrivalCV2 *float64           `json:"arrivalCV2,omitempty"`
	TTFT       ResolvedObjective  `json:"ttft"`
	TPOT       *ResolvedObjective `json:"tpot,omitempty"`
	Headroom   float64            `json:"headroom"`
	Assume     Assume             `json:"assume"`
	Views      ViewRequest        `json:"views"`
}

// FleetCounts is the replicas per role.
type FleetCounts struct {
	Prefill int `json:"prefill"`
	Decode  int `json:"decode"`
}

// ResolvedObjective is one objective as priced. BucketTargets are a TTFT
// objective's per bucket, every shown bucket's set.
type ResolvedObjective struct {
	Statistic     string             `json:"statistic"`
	Target        float64            `json:"target"`
	BucketTargets map[string]float64 `json:"bucketTargets,omitempty"`
}

// TTFTView is the TTFT graph: the mix line, and with the view asked the "if
// all traffic were bucket b" lines.
type TTFTView struct {
	Statistic string  `json:"statistic"`
	Line      Line    `json:"line"`
	PerBucket []Line  `json:"perBucket,omitempty"`
	Markers   Markers `json:"markers"`
}

// The semantics a TTFT line has.
const (
	// SemanticsMix is the current mix: the line the limits are read from.
	SemanticsMix = "mix"
	// SemanticsAllTraffic is "if all traffic were bucket b": never a limit.
	SemanticsAllTraffic = "allTraffic"
)

// Line is one TTFT line: its curve, its floor and its two points.
type Line struct {
	Bucket    string `json:"bucket"`
	Semantics string `json:"semantics"`
	// PromptTokens is the prompt a bucket's line is priced at; absent on the
	// mix line, which is priced at the mix's.
	PromptTokens *float64 `json:"promptTokens,omitempty"`
	Target       float64  `json:"target"`
	Floor        Figure   `json:"floor"`
	// Knees are the line's own, on a bucket's line: the bucket moves them.
	Knees    *Markers    `json:"knees,omitempty"`
	Curve    []Point     `json:"curve"`
	AtRate   RatePoint   `json:"atRate"`
	AtTarget TargetPoint `json:"atTarget"`
}

// Point is one curve point: the figure at Rate.
type Point struct {
	Rate   float64 `json:"rate"`
	Figure Figure  `json:"figure"`
}

// RatePoint is point A: the figure at the chosen rate, priced there, never
// read off the curve.
type RatePoint struct {
	Rate   float64 `json:"rate"`
	Figure Figure  `json:"figure"`
}

// TargetPoint is point B: the rate at which the figure reaches the target.
type TargetPoint struct {
	Seconds float64 `json:"seconds"`
	Rate    Figure  `json:"rate"`
}

// Markers are the rates the graphs mark: the knees, the operating-point cap
// (headroom times the lower knee) and the flow-control onsets.
type Markers struct {
	PrefillKnee  *Figure `json:"prefillKnee,omitempty"`
	DecodeKnee   *Figure `json:"decodeKnee,omitempty"`
	OperatingCap *Figure `json:"operatingCap,omitempty"`
	Onsets       *Onsets `json:"onsets,omitempty"`
}

// Onsets are the flow-control onsets, r/s, or their refusal; absent with
// flow control off.
type Onsets struct {
	Prefill *float64 `json:"prefill,omitempty"`
	Decode  *float64 `json:"decode,omitempty"`
	Refused *Refusal `json:"refused,omitempty"`
}

// TPOTView is the TPOT graph: one line for the mix.
type TPOTView struct {
	Statistic string      `json:"statistic"`
	Target    float64     `json:"target"`
	Floor     Figure      `json:"floor"`
	Ratio     *Ratio      `json:"ratio,omitempty"`
	Curve     []Point     `json:"curve"`
	AtRate    RatePoint   `json:"atRate"`
	AtTarget  TargetPoint `json:"atTarget"`
	Markers   Markers     `json:"markers"`
}

// Ratio is R_q, a TPOT percentile over its mean: its value and source
// (learned, engine histogram, assumed), or its refusal.
type Ratio struct {
	Value   *float64 `json:"value,omitempty"`
	Source  string   `json:"source,omitempty"`
	Flags   []string `json:"flags,omitempty"`
	Refused *Refusal `json:"refused,omitempty"`
}

// Limits is the most the fleet serves under the SLO: each role's limit, the
// fleet's, and the chosen rate's headroom under it.
type Limits struct {
	Prefill Figure     `json:"prefill"`
	Decode  *Figure    `json:"decode,omitempty"`
	Fleet   FleetLimit `json:"fleet"`
	Chosen  Chosen     `json:"chosen"`
}

// FleetLimit is the fleet's limit, the operating-point cap applied, with the
// figure without it and the role that binds.
type FleetLimit struct {
	Figure
	WithoutHeadroom Figure   `json:"withoutHeadroom"`
	CapApplied      bool     `json:"capApplied"`
	Binding         []string `json:"binding"`
}

// Chosen is the chosen rate and its headroom under the fleet's limit:
// negative where the SLO is missed.
type Chosen struct {
	Rate float64 `json:"rate"`
	// RateSource is "request", or "operating point" where the scenario gave
	// no rate.
	RateSource string `json:"rateSource"`
	Headroom   Figure `json:"headroom"`
}

// Marginal is the fleet's limit with one more replica of each role.
type Marginal struct {
	PrefillPlusOne MarginalStep `json:"prefillPlusOne"`
	DecodePlusOne  MarginalStep `json:"decodePlusOne"`
}

// MarginalStep is the fleet's limit at one more replica of a role, and its
// change from the scenario's.
type MarginalStep struct {
	Fleet   FleetCounts `json:"fleet"`
	Limit   Figure      `json:"limit"`
	Delta   Figure      `json:"delta"`
	Binding []string    `json:"binding"`
}

// FlagUse is one flag and every place in the response it qualifies.
type FlagUse struct {
	Flag string   `json:"flag"`
	On   []string `json:"on"`
}

// SourcesResponse is GET /v1/sources: the sources the server reads, which the
// page's source picker offers.
type SourcesResponse struct {
	SchemaVersion int `json:"schemaVersion"`
	// Prometheus is whether the server reads a Prometheus (--prometheus).
	Prometheus bool `json:"prometheus"`
	// Namespace is the one a prometheus source reads when a request names
	// none, and Namespaces every one it may read.
	Namespace  string   `json:"namespace,omitempty"`
	Namespaces []string `json:"namespaces"`
	// Recordings are the file names a recording source may name.
	Recordings []string `json:"recordings"`
}

// ErrorResponse is a request refused before any pricing.
type ErrorResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	Error         string `json:"error"`
}
