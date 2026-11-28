package whatifapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/balance"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
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
	// SelfCheck asks the planner be checked on the source beside its
	// inputs (SelfCheck); absent, the server's default (on, unless the
	// server was started with --no-self-check).
	SelfCheck *bool `json:"selfCheck,omitempty"`
	// PrefillVPPlateau reads V_P from the occupancy plateau where no
	// prefill pod queued (the CLI's --prefill-vp-plateau): experimental and
	// off by default. Every figure priced from it is flagged VPFromPlateau.
	PrefillVPPlateau bool `json:"prefillVPPlateau,omitempty"`
	// PrefillHitAtCount prices the prefix hit at a prefill count other than
	// the source's (the CLI's --prefill-hit-at-count): off by default. At
	// fewer pods the hit is the floor hit x N'/N, flagged PrefixHitAtCount;
	// at more it is held, flagged PrefixHitHeld.
	PrefillHitAtCount bool `json:"prefillHitAtCount,omitempty"`
	// PrefillVPServiceLine reads V_P from the prefill service line where no
	// prefill pod queued: a V_P read under a queue stands (the CLI's
	// --prefill-vp-service-line): opt-in, off by default, and not validated,
	// so every figure reads outside the validated region.
	PrefillVPServiceLine bool `json:"prefillVPServiceLine,omitempty"`
	// PrefillFLoadCurve prices f, the prefill imbalance, against the load
	// per pod at each prefill count whose fit is identified (the CLI's
	// --prefill-f-load-curve): opt-in, off by default, validated on lab
	// recordings only, so every figure reads outside the validated region.
	PrefillFLoadCurve bool `json:"prefillFLoadCurve,omitempty"`
	// DecodeLoopGain reports each decode TPOT's loop gain, and flags
	// DecodeLoopGain one whose fill amplifies its ITL line's slope (the
	// CLI's --decode-loop-gain): opt-in, off by default. No figure moves;
	// a flagged one reads outside the validated region.
	DecodeLoopGain bool `json:"decodeLoopGain,omitempty"`
	// ExperimentalTTFT prices TTFT with the TTFT model's switches that are
	// off by default (the CLI's --experimental-ttft): for demonstration,
	// not validated. Every prefill and TTFT figure is flagged
	// ExperimentalTTFT.
	ExperimentalTTFT bool `json:"experimentalTTFT,omitempty"`
	// TTFTModel prices the main view's TTFT with an experimental model:
	// "" is the server's default (serve --experimental-ttft=v2 or
	// =v2nl-line), whatif.QueueRankMixture or its variant
	// whatif.QueueRankMixtureNLLine. The model is fitted on the source's
	// window as a backtest fits it, and abstains where it is not
	// identified; the planner's TTFT then stands.
	TTFTModel string `json:"ttftModel,omitempty"`
	// RankMixtureStratified is BacktestRequest.RankMixtureStratified for
	// the main view's model: absent, the server's default (on, unless the
	// server was started with --rank-mixture-stratified=false). Read only
	// with TTFTModel rank-mixture; false changes the inputsId.
	RankMixtureStratified *bool `json:"rankMixtureStratified,omitempty"`
	// RankMixtureKVHold, RankMixtureRemainderSpread and
	// RankMixtureMinFitWindow are BacktestRequest's for the main view's
	// model: each, where the request does not set it, the server's default
	// (serve --rank-mixture-kv-hold, --rank-mixture-remainder-spread,
	// --rank-mixture-min-fit-window). The two modes are the variant's
	// (TTFTModel rank-mixture-nl-line) only; the window is either model's.
	// Each changes the inputsId.
	RankMixtureKVHold          bool   `json:"rankMixtureKVHold,omitempty"`
	RankMixtureRemainderSpread bool   `json:"rankMixtureRemainderSpread,omitempty"`
	RankMixtureMinFitWindow    string `json:"rankMixtureMinFitWindow,omitempty"`
	// RankMixtureRemainderServer is BacktestRequest's for the main view's
	// model, defaulted as the modes above are (serve
	// --rank-mixture-remainder-server); only with the variant and its
	// remainder spread. It changes the inputsId.
	RankMixtureRemainderServer bool `json:"rankMixtureRemainderServer,omitempty"`
	// TPOTDistMinFitSteps is BacktestRequest's for the main view's TPOT
	// distribution: absent, the server's default (serve
	// --tpot-dist-min-fit-steps), else 24. Only with a TPOTModel; another
	// count changes the inputsId.
	TPOTDistMinFitSteps int `json:"tpotDistMinFitSteps,omitempty"`
	// TPOTModel prices the main view's TPOT p90 and p95 with an
	// experimental per-request TPOT distribution: "" is the server's
	// default (serve --experimental-tpot=v2), and
	// whatif.TPOTRenewalLognormal the only other. It is fitted on the
	// source's window and abstains where its checks fail; the planner's
	// percentile then stands. The mean TPOT is the planner's either way.
	TPOTModel string `json:"tpotModel,omitempty"`
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
	// Cycles, when set, reads the recording with only these cycles
	// (learnreplay.CyclesOf): a load split's fit (LoadFitCycles). Never
	// sent: a backtest sets it.
	Cycles *CycleSet `json:"-"`
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
	// CachedTokenCost is phi, the cost of a cached prompt token, as fitted
	// on the source; absent where slo.SwitchTTFTCachedTokenCost is off or
	// there is no prefill variant.
	CachedTokenCost *CachedTokenCost `json:"cachedTokenCost,omitempty"`
	// SelfCheck is the planner checked on this source: the folds of it
	// fitted on earlier cycles predicting later ones. Absent when not asked.
	SelfCheck *SelfCheck `json:"selfCheck,omitempty"`
	// PrefillVPPlateau says the source was read with V_P from the occupancy
	// plateau (InputsRequest.PrefillVPPlateau); absent otherwise.
	PrefillVPPlateau bool `json:"prefillVPPlateau,omitempty"`
	// PrefillHitAtCount says the source was read with the prefix hit priced
	// at each prefill count (InputsRequest.PrefillHitAtCount); absent
	// otherwise.
	PrefillHitAtCount bool `json:"prefillHitAtCount,omitempty"`
	// PrefillHitModel is the reuse-gap law the prefix hit at another
	// prefill count is priced with, fitted on the source's KV residency
	// histograms (prefill.FitHitModel); absent with PrefillHitAtCount off
	// or no histograms recorded.
	PrefillHitModel *prefill.HitModel `json:"prefillHitModel,omitempty"`
	// PrefillVPServiceLine says the source was read with V_P from the
	// prefill service line (InputsRequest.PrefillVPServiceLine); absent
	// otherwise.
	PrefillVPServiceLine bool `json:"prefillVPServiceLine,omitempty"`
	// PrefillFLoadCurve says the source was read with f against the load
	// (InputsRequest.PrefillFLoadCurve); absent otherwise.
	PrefillFLoadCurve bool `json:"prefillFLoadCurve,omitempty"`
	// DecodeLoopGain says the source's decode TPOTs carry their loop gain
	// (InputsRequest.DecodeLoopGain); absent otherwise.
	DecodeLoopGain bool `json:"decodeLoopGain,omitempty"`
	// ExperimentalTTFT says the source was read with the experimental TTFT
	// model (InputsRequest.ExperimentalTTFT); absent otherwise.
	ExperimentalTTFT bool `json:"experimentalTTFT,omitempty"`
	// TTFTModel is the experimental TTFT model the main view prices with,
	// as fitted on the source (InputsRequest.TTFTModel); absent otherwise.
	TTFTModel *TTFTModelInfo `json:"ttftModel,omitempty"`
	// TPOTModel is the experimental TPOT model the main view prices with,
	// as fitted on the source (InputsRequest.TPOTModel); absent otherwise.
	TPOTModel *TPOTModelInfo `json:"tpotModel,omitempty"`
	// Measured is the source's busiest UTC hour and the TTFT and TPOT the
	// router measured over it (BusiestHourMeasured); absent with no router
	// histogram. It is measured, never priced.
	Measured *MeasuredHour `json:"measured,omitempty"`
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

// RoleInfo is one role's variant and its replica count at the cycle, and
// the engine it runs.
type RoleInfo struct {
	Variant  string `json:"variant"`
	Replicas int    `json:"replicas"`
	// GPUsPerReplica is the GPUs one replica requests, as the source states
	// it; absent where it states none. A size weighs replicas by it
	// (ReplicaCost).
	GPUsPerReplica int         `json:"gpusPerReplica,omitempty"`
	Engine         *EngineInfo `json:"engine,omitempty"`
	// StartSeconds is how long a new replica of the role took to become
	// Ready, as the source's saturation analyzer learned it (its slowest
	// measured variant of the role); absent where it learned none.
	// StartSeeded says it is the ScaledObject's replicaStartSeconds seed, no
	// replica of the role measured.
	StartSeconds *float64 `json:"startSeconds,omitempty"`
	StartSeeded  bool     `json:"startSeeded,omitempty"`
}

// GPUsPerReplica is each role's GPUs per replica as the inputs state them, 0
// for a role they do not state or a fleet with no prefill variant.
func (f FleetInfo) GPUsPerReplica() FleetCounts {
	out := FleetCounts{Decode: f.Decode.GPUsPerReplica}
	if f.Prefill != nil {
		out.Prefill = f.Prefill.GPUsPerReplica
	}
	return out
}

// EngineInfo is the engine a variant runs: which one, for vLLM the release
// and where it was read from (annotation, image tag, --vllm-version,
// --engine-image, none; both absent in a recording made before the release
// was read), and the figures that release's defaults decide where the
// engine's flags set none, each with its source (flag, default, not
// recorded). It is reported for reference: no figure of an answer reads it
// yet. See docs/reference/vllm-versions.md.
type EngineInfo struct {
	Engine                     string  `json:"engine"`
	Summary                    string  `json:"summary"`
	VLLMVersion                string  `json:"vllmVersion,omitempty"`
	VLLMVersionFrom            string  `json:"vllmVersionFrom,omitempty"`
	StepBudget                 int64   `json:"stepBudget"`
	StepBudgetSource           string  `json:"stepBudgetSource"`
	GPUMemoryUtilization       float64 `json:"gpuMemoryUtilization"`
	GPUMemoryUtilizationSource string  `json:"gpuMemoryUtilizationSource"`
	BlockSize                  int64   `json:"blockSize"`
	BlockSizeSource            string  `json:"blockSizeSource"`
	// Attention is the model's attention kind and MoE its MoE class, read
	// from its config.json: "unknown" where none was read (issues #501,
	// #503). ModelConfigSource is where the config was read, and Model both
	// in words, as the engine row prints them; absent where neither is
	// known. Reported, not priced.
	Attention         string `json:"attention"`
	MoE               string `json:"moe"`
	ModelConfigSource string `json:"modelConfigSource,omitempty"`
	Model             string `json:"model,omitempty"`
	// StepBudgetBinding is whether the step budget binds, read from the
	// engine's tokens per iteration; absent where no row of the variant carried
	// that series (a recording made before it was recorded, or SGLang).
	StepBudgetBinding *StepBudgetBinding `json:"stepBudgetBinding,omitempty"`
}

// StepBudgetBinding is whether a variant's step budget binds, read from the
// engine's tokens per iteration (vllm:iteration_tokens_total: the computed
// prompt tokens of the prefills an iteration completed, each whole, and the
// tokens it generated) over the window against StepBudget
// (capacity.StepTokens.Binding). Class is "binds", "does-not-bind" or
// "unknown", and Reason says why it is unknown. The class is decided on
// OverShare, the share of iterations above BoundTokens, the smallest bound at
// or over the budget: completions of prompts the budget chunked. Share, the
// share above NearTokens (the largest bound under 90% of the budget), is a
// diagnostic. TopTokens is the bound at or under which 99.9% of the
// iterations counted, a floor where TopOpen. Reported only: no figure reads
// it yet.
type StepBudgetBinding struct {
	Class       string  `json:"class"`
	Reason      string  `json:"reason,omitempty"`
	BoundTokens float64 `json:"boundTokens,omitempty"`
	OverShare   float64 `json:"overShare"`
	NearTokens  float64 `json:"nearTokens,omitempty"`
	Share       float64 `json:"share"`
	TopTokens   float64 `json:"topTokens,omitempty"`
	TopOpen     bool    `json:"topOpen,omitempty"`
}

// Observed is the traffic the source saw.
type Observed struct {
	// Rate is the observed request rate; refused where the source does not
	// carry it, and a plan then defaults to the operating point.
	Rate         Figure `json:"rate"`
	PromptTokens Figure `json:"promptTokens"`
	OutputTokens Figure `json:"outputTokens"`
	PromptCV     Figure `json:"promptCV"`
	// Buckets are the default prompt buckets, each named by its upper edge,
	// and PromptBucket the one the mean prompt falls in. A plan's own edges
	// (PlanRequest.BucketEdges) change neither: nothing learned is per
	// bucket.
	Buckets      []Bucket `json:"buckets"`
	PromptBucket string   `json:"promptBucket,omitempty"`
	// Mix is the share of requests per bucket, or why the source cannot say.
	Mix ObservedMix `json:"mix"`
}

// Bucket is one prompt bucket: prompts above the bucket below, up to
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
	// Router is what the router scores, the prefix cache, the load, both or
	// neither, read from its series (balance.DetectRouterPolicy), with the
	// evidence; absent where the source holds none of them, so such a
	// source's document is as before. It sets no price:
	// --load-aware-router (Assume.LoadAwareRouter) still does.
	Router *balance.RouterPolicy `json:"router,omitempty"`
}

// PolicyObjective is one SLO objective: its statistic and target.
type PolicyObjective struct {
	Statistic string  `json:"statistic"`
	Seconds   float64 `json:"seconds"`
}

// TermMaxModelLen is the term holding the engines' context window, tokens:
// the smallest --max-model-len their recorded arguments set, refused where
// none is set.
const TermMaxModelLen = "maxModelLen"

// FlagEdgeBeyondContext marks an "if all traffic were bucket b" line whose
// edge is past the context window (TermMaxModelLen): the engines refuse
// prompts that long, so the line prices traffic they would not serve. It is
// raised only where the context window is known.
const FlagEdgeBeyondContext = "EdgeBeyondContext"

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
	Value *float64 `json:"value,omitempty"`
	Unit  string   `json:"unit,omitempty"`
	Band  *Band    `json:"band,omitempty"`
	// Kappa is the decode loop's condition number at the figure's rate,
	// g/(1-g) with g the loop gain (whatif.Kappa): a relative error in the
	// ITL line's slope moves the fill about kappa times as much, and the
	// TPOT read there as much or, with a KV pull, up to once more. It is on every forward decode figure (the decode knee,
	// the fill and the operating rate, and the TPOTs at a rate) and moves
	// none of them; absent on an inverse figure (a rate for a target), on a
	// TTFT, and where the gain is not under 1.
	Kappa           *float64 `json:"kappa,omitempty"`
	Sensitivity     *Span    `json:"sensitivity,omitempty"`
	Floor           *float64 `json:"floor,omitempty"`
	Uncapped        *float64 `json:"uncapped,omitempty"`
	PrefillOnlyRate *float64 `json:"prefillOnlyRate,omitempty"`
	BoundBy         []string `json:"boundBy,omitempty"`
	Flags           []string `json:"flags,omitempty"`
	Refused         *Refusal `json:"refused,omitempty"`
	// Validation is the envelope's verdict on the figure, on the figures a
	// plan reads (the points A and B, the knees and the limits), not on a
	// curve's points.
	Validation *Validation `json:"validation,omitempty"`
	// UpperBound is never set. It said a TTFT priced from a V_P known only
	// as a lower bound was an upper bound (#369), but only the figure's V_P
	// terms are bounded by that: the others are point estimates, or not
	// priced, and the figure read under the measured TTFT in backtests
	// (issue llm-d-incubation/llm-d-scaling-manager#515). Such a figure is a
	// point figure flagged VPLowerBound (TTFTFromLowerBoundVP). The key stays
	// in the pinned schema, absent.
	//
	// Deprecated: never set; read the figure's flags (VPLowerBound).
	UpperBound bool `json:"upperBound,omitempty"`
	// Extrapolated says the figure, priced by the experimental TTFT or
	// TPOT model, is at a rate above the highest load the source's fit
	// window served (TTFTModelView.MeasuredUpTo,
	// TPOTModelView.MeasuredUpTo).
	Extrapolated bool `json:"extrapolated,omitempty"`
	// BatchBeyondFit is the batch a decode figure's fill runs past the most
	// the ITL line's fit rows held, where it is (flag BatchBeyondFit): the
	// figure is priced all the same, and its TPOT may read low and its knee
	// high.
	BatchBeyondFit *BatchBeyondFit `json:"batchBeyondFit,omitempty"`
	// LoopGain is a decode TPOT's loop gain at its rate, read with
	// decodeLoopGain (whatif.LoopGainAt): how much the fill it is priced at
	// amplifies an error in the ITL line's slope. A figure whose slope 5%
	// steeper moves it 1.25x or more, or saturates, is flagged
	// DecodeLoopGain. Absent otherwise.
	LoopGain *LoopGain `json:"loopGain,omitempty"`
}

// LoopGain is whatif.LoopGain on the wire: the loop gain at the figure's
// rate and at the knee, and the TPOT a slope 5% steeper prices over the
// figure (Move), or Saturates where that slope carries no fill.
type LoopGain struct {
	Gain      float64 `json:"gain"`
	KneeGain  float64 `json:"kneeGain"`
	Move      float64 `json:"move,omitempty"`
	Saturates bool    `json:"saturates,omitempty"`
}

// BatchBeyondFit is a decode figure's batch against its line's fit, in
// sequences an engine rank: Priced at the figure's fill, Fitted the most the
// rows the line was fitted on held (the held fit's for a held line). Ranks
// is the data-parallel engine ranks a pod runs, where more than one: the
// batches are then a rank's, not the pod's; absent for one.
type BatchBeyondFit struct {
	Priced int64 `json:"priced"`
	Fitted int64 `json:"fitted"`
	Ranks  int   `json:"ranks,omitempty"`
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
	// BucketEdges are the prompt buckets' upper edges, tokens, ascending: 1
	// to 8 of them, each at most 1048576 (1024k). Each bucket is named by
	// its edge: in k where it is a whole number of 1024 tokens ("8k"), else
	// in tokens ("8000"). Absent, the default 1k, 4k, 8k, 32k and 128k. They
	// shape only the "if all traffic were bucket b" view, which they need.
	BucketEdges []int `json:"bucketEdges,omitempty"`
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

// ClassRate is one class's rate, requests per second over the model, or its
// load as a concurrency, the mean number of requests in the system: a plan
// translates it into the rate at which the scenario's fleet holds that many
// (Little's law), below the fleet's knee. Give one of the two.
type ClassRate struct {
	Rate        *float64 `json:"rate,omitempty"`
	Concurrency *float64 `json:"concurrency,omitempty"`
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
// ITLB is the decode line's intercept B in seconds, and ITLBFromFloor takes
// it from the floor read below the band: either is the CLI's --assume-itl-b,
// used only on a line on the default intercept (PinnedDefaultB).
//
// FlowControl and RouterWait are the router's flow control, as the CLI's
// --flow-control, --router-wait-in-prefill and --price-router-wait state it.
type Assume struct {
	VP            *float64 `json:"vp,omitempty"`
	A             *float64 `json:"a,omitempty"`
	PromptCV      *float64 `json:"promptCV,omitempty"`
	TPOTRatio     *float64 `json:"tpotRatio,omitempty"`
	RouterHop     *float64 `json:"routerHop,omitempty"`
	ITLB          *float64 `json:"itlB,omitempty"`
	ITLBFromFloor bool     `json:"itlBFromFloor,omitempty"`
	// FlowControl is the router's flow control stated, FlowControlOn or
	// FlowControlOff, in place of its inference from the queue series
	// (--flow-control); absent, inferred.
	FlowControl string `json:"flowControl,omitempty"`
	// RouterWait is how a TTFT past the prefill flow-control onset carries
	// the router's wait: RouterWaitInPrefill (--router-wait-in-prefill,
	// router v1.0.0), RouterWaitGateStage (--price-router-wait, router
	// v0.11.0) or RouterWaitNone (not priced: the figure is flagged
	// AboveFlowControlOnset, a miss); absent, the CLI's default
	// (whatif.DefaultRouterWaitInPrefill, whatif.DefaultPriceRouterWait).
	RouterWait string `json:"routerWait,omitempty"`
	// LoadAwareRouter states that the router balances prefill by load, as
	// llm-d's EPP queue scorer does (--load-aware-router): f is held to the
	// utilization it was read at, then falls to 1 at the pooled prefill knee
	// (whatif.Prefill.LoadAwareRouter). No recording says what the router
	// scores, so it is stated, never read; absent or false, f is held at
	// every load (whatif.DefaultPlannerLoadAwareRouter).
	LoadAwareRouter bool `json:"loadAwareRouter,omitempty"`
}

// The values Assume.FlowControl takes.
const (
	FlowControlOn  = "on"
	FlowControlOff = "off"
)

// CheckFlowControl refuses an Assume.FlowControl or Assume.RouterWait no
// CLI flag takes, as an input error.
func (a Assume) CheckFlowControl() error {
	switch a.FlowControl {
	case "", FlowControlOn, FlowControlOff:
	default:
		return InputErrorf("assume.flowControl %q, want %s or %s", a.FlowControl, FlowControlOn, FlowControlOff)
	}
	switch a.RouterWait {
	case "", RouterWaitNone, RouterWaitInPrefill, RouterWaitGateStage:
	default:
		return InputErrorf("assume.routerWait %q, want %s, %s or %s", a.RouterWait, RouterWaitNone, RouterWaitInPrefill, RouterWaitGateStage)
	}
	return nil
}

// The values Assume.RouterWait takes.
const (
	// RouterWaitNone leaves the wait unpriced, flagged.
	RouterWaitNone = "none"
	// RouterWaitInPrefill takes the router's wait as inside the
	// processor-sharing prefill price (whatif.TTFTFleet.RouterWaitInPrefill).
	RouterWaitInPrefill = "inPrefill"
	// RouterWaitGateStage prices the gate as a stage in front of prefill
	// (whatif.TTFTFleet.PriceRouterWait).
	RouterWaitGateStage = "gateStage"
)

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
	Rate *float64 `json:"rate,omitempty"`
	// Concurrency is the concurrency asked in place of a rate: the rate is
	// the one at which the fleet holds that many requests on average
	// (limits.chosen.fromConcurrency).
	Concurrency *float64           `json:"concurrency,omitempty"`
	Mix         Mix                `json:"mix"`
	ArrivalCV2  *float64           `json:"arrivalCV2,omitempty"`
	TTFT        ResolvedObjective  `json:"ttft"`
	TPOT        *ResolvedObjective `json:"tpot,omitempty"`
	Headroom    float64            `json:"headroom"`
	Assume      Assume             `json:"assume"`
	Views       ViewRequest        `json:"views"`
	// BucketEdges are the request's edges; absent with the default ones,
	// asked or not, so that they hash to the same scenarioId.
	BucketEdges []int `json:"bucketEdges,omitempty"`
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
	// Model is the experimental TTFT model at the plan's fleet, where the
	// source was resolved with one (InputsResponse.TTFTModel).
	Model *TTFTModelView `json:"model,omitempty"`
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
	// Model is the experimental TPOT model at the plan's fleet, where the
	// source was resolved with one (InputsResponse.TPOTModel).
	Model *TPOTModelView `json:"model,omitempty"`
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
	// RateSource is what set Rate: one of the RateSource constants.
	RateSource string `json:"rateSource"`
	Headroom   Figure `json:"headroom"`
	// Concurrency is the mean number of requests in the system at Rate
	// (Little's law), by role: unbounded at or past the knee.
	Concurrency *Concurrency `json:"concurrency,omitempty"`
	// FromConcurrency is the concurrency asked (scenario.concurrency) and
	// the rate it translates into; AtKnee says the fleet holds fewer
	// requests than that below its knee, so Rate is the knee's.
	FromConcurrency *ConcurrencyRate `json:"fromConcurrency,omitempty"`
}

// The values of Chosen.RateSource.
const (
	// RateSourceRequest is a rate the scenario gave.
	RateSourceRequest = "request"
	// RateSourceConcurrency is the rate at which the fleet holds the
	// concurrency the scenario gave (Chosen.FromConcurrency).
	RateSourceConcurrency = "concurrency"
	// RateSourceOperatingPoint is headroom times the lower of both knees,
	// where the scenario gave no rate.
	RateSourceOperatingPoint = "operating point"
	// RateSourcePrefillKnee is headroom times the prefill knee, where the
	// scenario gave no rate and the decode knee is refused: a rate to start
	// from, not an operating point, which the decode knee would bound.
	RateSourcePrefillKnee = "prefill knee; decode not priced"
	// RateSourceDecodeKnee is headroom times the decode knee, where the
	// scenario gave no rate and the prefill knee is refused.
	RateSourceDecodeKnee = "decode knee; prefill not priced"
)

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
	// Recordings are the file names a recording source may name, and
	// RecordingsTruncated whether the server listed only the first of them.
	Recordings          []string `json:"recordings"`
	RecordingsTruncated bool     `json:"recordingsTruncated,omitempty"`
	// Upload is set when the server takes uploads (POST /v1/recordings).
	Upload *UploadInfo `json:"upload,omitempty"`
	// Uploads are the uploads kept, newest first, as Recordings lists their
	// names, each with the file name it was sent under, its size and when.
	Uploads []UploadedRecording `json:"uploads,omitempty"`
}

// UploadedRecording is one upload kept: Name, which a recording source
// gives; FileName, the file name it was last sent under (UploadFileName),
// empty when none was given, a label never used to name a file; Bytes, its
// size as sent; UploadedAt, when it was last sent (RFC 3339, UTC).
type UploadedRecording struct {
	Name       string `json:"name"`
	FileName   string `json:"fileName,omitempty"`
	Bytes      int    `json:"bytes"`
	UploadedAt string `json:"uploadedAt"`
}

// UploadInfo is what a server that takes uploads takes.
type UploadInfo struct {
	// MaxBytes is the largest upload, in bytes as sent.
	MaxBytes int `json:"maxBytes"`
}

// UploadResponse is POST /v1/recordings: the name a recording source gives
// the recording uploaded, which the server chose, its size as sent, and the
// file name and time it is listed with (UploadedRecording).
type UploadResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	Name          string `json:"name"`
	Bytes         int    `json:"bytes"`
	FileName      string `json:"fileName,omitempty"`
	UploadedAt    string `json:"uploadedAt"`
}

// SizeRequest is POST /v1/size: the fleet of the fewest GPUs (SizeAnswer)
// whose fleet limit reaches the rate, under the SLO. The rate is required; every other
// field defaults as a plan's does. MaxReplicas bounds the search per role.
type SizeRequest struct {
	InputsID    string      `json:"inputsId"`
	Traffic     *Traffic    `json:"traffic,omitempty"`
	SLO         *SLORequest `json:"slo,omitempty"`
	Assume      *Assume     `json:"assume,omitempty"`
	Headroom    *float64    `json:"headroom,omitempty"`
	MaxReplicas *FleetKnob  `json:"maxReplicas,omitempty"`
	// SizeOn is what a fleet must reach the rate with: "point", its limit
	// (the default), or "low", the low end of its span over V_P and f.
	SizeOn string `json:"sizeOn,omitempty"`
	// Spare is the replicas of each role to add to the answer so that the
	// fleet still serves the rate with that many of each role lost
	// (SizeAnswer.WithSpare); 0 to maxReplicas, absent for none.
	Spare *int `json:"spare,omitempty"`
	// BucketEdges are refused: a size prices the mix line alone, and the
	// edges shape only a plan's "if all traffic were bucket b" view.
	BucketEdges []int `json:"bucketEdges,omitempty"`
}

// SizeResponse is a size: the answer, or why no fleet within the bound
// serves the rate (Unmet), never both.
type SizeResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	WhatifSchema  string `json:"whatifSchema"`
	SizeID        string `json:"sizeId"`
	InputsID      string `json:"inputsId"`
	// Banner is what every TTFT figure says of itself.
	Banner string `json:"banner"`
	// Rate is the rate sized for, and MaxReplicas the bound searched.
	Rate        float64     `json:"rate"`
	MaxReplicas FleetCounts `json:"maxReplicas"`
	// SizeOn is "point" or "low", as asked.
	SizeOn string `json:"sizeOn"`
	// Minimizes is what the search minimised: "gpus" (MinimizesGPUs) where
	// the inputs state both roles' GPUs per replica, else "replicas"
	// (MinimizesReplicas). Ties go to fewer replicas in all, then to fewer
	// decode replicas.
	Minimizes string `json:"minimizes"`
	// Spare is the spare replicas of each role asked, absent for none.
	Spare int `json:"spare,omitempty"`
	// Searched is how many fleets the search priced, the answer's plan aside.
	Searched int         `json:"searched"`
	Answer   *SizeAnswer `json:"answer,omitempty"`
	Unmet    *SizeUnmet  `json:"unmet,omitempty"`
}

// SizeAnswer is the cheapest fleet that serves the rate: the fewest GPUs, or
// the fewest replicas where GPUs are not stated (SizeResponse.Minimizes).
// Limit, Headroom and Binding are the plan's at the fleet, and Plan is that
// plan, as POST /v1/plan returns it.
type SizeAnswer struct {
	Fleet FleetCounts `json:"fleet"`
	// GPUsPerReplica is each role's GPUs per replica and GPUs the fleet's,
	// both absent where the inputs do not state both roles'.
	GPUsPerReplica *FleetCounts `json:"gpusPerReplica,omitempty"`
	GPUs           *int         `json:"gpus,omitempty"`
	// WithSpare is the fleet with the spare replicas asked added to each
	// role: it still serves the rate with that many replicas of each role
	// lost at once, since Fleet does. Absent with no spare asked.
	WithSpare *SpareFleet `json:"withSpare,omitempty"`
	// Limit is the fleet's limit (the plan's limits.fleet). Headroom is what
	// the size is on less the rate: the limit, or sized on the low end the
	// limit's span's low end, the same basis as the steps' Headroom.
	Limit    Figure   `json:"limit"`
	Headroom Figure   `json:"headroom"`
	Binding  []string `json:"binding"`
	// FewerPrefill and FewerDecode are the fleet with one replica fewer of
	// the role, and its limit short of the rate: why one fewer is not
	// enough. Absent at one replica.
	FewerPrefill *SizeStep    `json:"fewerPrefill,omitempty"`
	FewerDecode  *SizeStep    `json:"fewerDecode,omitempty"`
	Plan         PlanResponse `json:"plan"`
}

// SpareFleet is a fleet needed plus Spare replicas of each role: Fleet, and
// its GPUs where both roles' GPUs per replica are stated. Losing up to Spare
// replicas of each role at once leaves at least the fleet needed, which
// holds. A plan of 2 prefill + 2 decode loses half a role to one replica
// lost; with a spare of 1 it is 3 + 3.
type SpareFleet struct {
	Spare int         `json:"spare"`
	Fleet FleetCounts `json:"fleet"`
	GPUs  *int        `json:"gpus,omitempty"`
}

// WithSpare is n plus spare replicas of each role, with its GPUs where
// gpusPerReplica states both roles'; nil for a spare of 0. A role at 0 (a
// decode-only need, with no prefill counted) stays at 0.
func WithSpare(n FleetCounts, spare int, gpusPerReplica FleetCounts) *SpareFleet {
	if spare <= 0 {
		return nil
	}
	f := FleetCounts{Prefill: n.Prefill, Decode: n.Decode + spare}
	if n.Prefill > 0 {
		f.Prefill += spare
	}
	return &SpareFleet{Spare: spare, Fleet: f, GPUs: FleetGPUs(f, gpusPerReplica)}
}

// FleetGPUs is n's GPUs at gpusPerReplica, or nil where it does not state a
// role n counts.
func FleetGPUs(n, gpusPerReplica FleetCounts) *int {
	if gpusPerReplica.Decode <= 0 || (n.Prefill > 0 && gpusPerReplica.Prefill <= 0) {
		return nil
	}
	g := n.Prefill*gpusPerReplica.Prefill + n.Decode*gpusPerReplica.Decode
	return &g
}

// SizeStep is one fleet the search priced: its limit, as the plan's +1
// steps price it (no span over V_P and f; sized on the low end, the plan's
// limit with its span), the headroom over the rate of what the size is on
// (the limit, or the span's low end; negative short of it), and the role
// that binds.
type SizeStep struct {
	Fleet    FleetCounts `json:"fleet"`
	Limit    Figure      `json:"limit"`
	Headroom Figure      `json:"headroom"`
	Binding  []string    `json:"binding"`
}

// SizeUnmet is a size refused: the refusal, the role no count within the
// bound serves ("decode", "prefill", or "fleet" when each role's own limit
// reaches the rate and the fleet's does not), and the fleet at the bound.
type SizeUnmet struct {
	Refused Refusal  `json:"refused"`
	Role    string   `json:"role"`
	AtBound SizeStep `json:"atBound"`
}

// ErrorResponse is a request refused before any pricing, or a plan stopped
// before it was priced. Reason names a refusal a client may act on
// (ReasonPlanBudgetExceeded); it is absent from every other error.
type ErrorResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	Error         string `json:"error"`
	Reason        string `json:"reason,omitempty"`
}
