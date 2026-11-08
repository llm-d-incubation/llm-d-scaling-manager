package main

import (
	"encoding/json"
	"io"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The -o json wire format. The library carries no json tags
// (internal/signals/whatif); these types are the one place the planner's
// answers are given names on the wire, and schemaVersion says which names.
// Removing, renaming or retyping a key, or changing its meaning, bumps
// schemaVersion; a key absent unless a new flag asks for it does not. Either
// way the pinned key list in testdata/schema-v<N>.txt changes
// (TestJSONSchemaIsPinned).

// schemaVersion is the version of the JSON output's schema.
const schemaVersion = 2

// wireOutput is one command's whole JSON output.
type wireOutput struct {
	SchemaVersion int         `json:"schemaVersion"`
	Question      string      `json:"question"`
	Source        *wireSource `json:"source,omitempty"`
	Bands         *wireBands  `json:"bands,omitempty"`
	Roles         []wireRole  `json:"roles"`
	ExitCode      int         `json:"exitCode"`
	Note          string      `json:"note,omitempty"`
	// Error is a usage or internal error met before any answer, with -o
	// json: the output then holds only it, the question and the exit code.
	Error *wireError `json:"error,omitempty"`
}

// wireError is an error met before any answer.
type wireError struct {
	Message string `json:"message"`
}

// wireSource is what the answer was read from: a recording file, or a
// window of Prometheus captured and generated in memory (Window, and
// Recording empty).
type wireSource struct {
	Recording string `json:"recording"`
	// Window is the Prometheus window the recording was made from, with
	// --prometheus; absent with --recording.
	Window     *wireWindow `json:"window,omitempty"`
	Namespace  string      `json:"namespace"`
	Model      string      `json:"model"`
	FirstCycle int         `json:"firstCycle"`
	LastCycle  int         `json:"lastCycle"`
	// TotalCycles is how many cycles of the model the recording holds;
	// LastCycle is the one the answer was read at.
	TotalCycles int       `json:"totalCycles"`
	FirstAt     time.Time `json:"firstAt"`
	LastAt      time.Time `json:"lastAt"`
}

// wireWindow is the window of Prometheus an answer was captured from.
type wireWindow struct {
	// Prometheus is the URL's scheme and host alone (redactedURL).
	Prometheus  string    `json:"prometheus"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	StepSeconds float64   `json:"stepSeconds"`
	// Roles is where the role map came from: kube-state-metrics, or
	// --role-of.
	Roles string `json:"roles"`
}

// wireBands is how the bands were drawn.
type wireBands struct {
	Method      string  `json:"method"`
	BlockCycles int     `json:"blockCycles"`
	Resamples   int     `json:"resamples"`
	Seed        uint64  `json:"seed"`
	LoPercent   float64 `json:"loPercent"`
	HiPercent   float64 `json:"hiPercent"`
}

// wireRole is one role's answer: for decode, one variant's. With a TTFT
// asked, role prefill is the prefill variant, with its inputs in TTFT, and
// role fleet (question 1) the fleet's rate, named by its decode variant.
type wireRole struct {
	Role    string      `json:"role"`
	Variant string      `json:"variant"`
	Inputs  *wireInputs `json:"inputs,omitempty"`
	TTFT    *wireTTFT   `json:"ttft,omitempty"`
	Results wireResults `json:"results"`
}

// wireTTFT is what a TTFT was priced from: each input and where it came
// from. Assumed names every input the recording did not give, in the words
// the text prints. An input not reached before a refusal is absent.
type wireTTFT struct {
	DecodeVariant string `json:"decodeVariant,omitempty"`
	// Percentile is q in (0, 1), absent for the mean; TargetSeconds is
	// question 1's target. Rate is the rate question 2 was priced at, and
	// RateSource what set it.
	Percentile    float64  `json:"percentile,omitempty"`
	TargetSeconds float64  `json:"targetSeconds,omitempty"`
	Rate          *float64 `json:"rate,omitempty"`
	RateSource    string   `json:"rateSource,omitempty"`
	Replicas      int      `json:"replicas,omitempty"`
	ReplicasFrom  string   `json:"replicasFrom,omitempty"`
	// VP is V_P, tokens/s, as learned or assumed (VPSource); VPOrigin is P1's
	// source, and VPPromptTokens the prompt it was read at. VPLearned is P1's
	// figure an --assume-vp stands in place of, and FixedLatencyLearned the
	// one an --assume-a does.
	VP                  float64  `json:"vp,omitempty"`
	VPSource            string   `json:"vpSource,omitempty"`
	VPOrigin            string   `json:"vpOrigin,omitempty"`
	VPPromptTokens      float64  `json:"vpPromptTokens,omitempty"`
	VPLearned           *float64 `json:"vpLearned,omitempty"`
	FixedLatencySeconds float64  `json:"fixedLatencySeconds,omitempty"`
	FixedLatencySource  string   `json:"fixedLatencySource,omitempty"`
	FixedLatencyLearned *float64 `json:"fixedLatencyLearned,omitempty"`
	Imbalance           float64  `json:"imbalance,omitempty"`
	ImbalanceSource     string   `json:"imbalanceSource,omitempty"`
	PromptTokens        float64  `json:"promptTokens,omitempty"`
	PromptSource        string   `json:"promptSource,omitempty"`
	PromptFromCycle     int      `json:"promptFromCycle,omitempty"`
	PromptToCycle       int      `json:"promptToCycle,omitempty"`
	PromptCV            float64  `json:"promptCV,omitempty"`
	PromptCVSource      string   `json:"promptCVSource,omitempty"`
	// PromptCVRead is the histogram's figure an --assume-prompt-cv stands in
	// place of, and ArrivalCV2 Ca^2 from --arrival-cv2: absent with neither.
	PromptCVRead   *float64 `json:"promptCVRead,omitempty"`
	ArrivalCV2     float64  `json:"arrivalCV2,omitempty"`
	ArrivalsSource string   `json:"arrivalsSource,omitempty"`
	// TransferSecondsPerToken is T_xfer per prompt token, and TransferSource
	// measured, omitted, or "decode engine" where that TTFT holds it.
	TransferSecondsPerToken float64 `json:"transferSecondsPerToken,omitempty"`
	TransferSource          string  `json:"transferSource,omitempty"`
	// DecodeSide is how decode's part was priced: line, "decode engine" or
	// omitted; the two figures are the decode engine's measured pair.
	DecodeSide             string  `json:"decodeSide,omitempty"`
	DecodeTTFTSeconds      float64 `json:"decodeTTFTSeconds,omitempty"`
	DecodeTTFTPromptTokens float64 `json:"decodeTTFTPromptTokens,omitempty"`
	// DecodeTermSource and RouterHopSource say where the line's decode term
	// and hop came from (measured, assumed; the term may be omitted), each
	// with why no fit stands. The figures are the term as priced: on no fit
	// the base is the held D, read at the held prompt and the from-ITL.
	DecodeTermRefused           string   `json:"decodeTermRefused,omitempty"`
	RouterHopRefused            string   `json:"routerHopRefused,omitempty"`
	DecodeBaseSeconds           float64  `json:"decodeBaseSeconds,omitempty"`
	DecodePerTokenSeconds       float64  `json:"decodePerTokenSeconds,omitempty"`
	DecodeHeldPromptTokens      float64  `json:"decodeHeldPromptTokens,omitempty"`
	DecodeFromITLSeconds        float64  `json:"decodeFromITLSeconds,omitempty"`
	RouterHopBaseSeconds        float64  `json:"routerHopBaseSeconds,omitempty"`
	RouterHopPerTokenSeconds    float64  `json:"routerHopPerTokenSeconds,omitempty"`
	RouterHopHeldPromptTokens   float64  `json:"routerHopHeldPromptTokens,omitempty"`
	RouterHopLearnedBaseSeconds *float64 `json:"routerHopLearnedBaseSeconds,omitempty"`
	DecodeTermSource            string   `json:"decodeTermSource,omitempty"`
	RouterHopSource             string   `json:"routerHopSource,omitempty"`
	DecodePerStep               float64  `json:"decodePerStep,omitempty"`
	FlowControl                 string   `json:"flowControl,omitempty"`
	Assumed                     []string `json:"assumed"`
}

// wireInputs is what a variant was priced from.
type wireInputs struct {
	Line           wireLine     `json:"line"`
	FillBand       wireInterval `json:"fillBand"`
	CapacityTokens float64      `json:"capacityTokens"`
	KVPerRequest   float64      `json:"kvPerRequest"`
	Shape          wireShape    `json:"shape"`
	BatchCap       int64        `json:"batchCap"`
	BatchCapSource string       `json:"batchCapSource"`
	Ranks          int          `json:"ranks"`
	Replicas       int          `json:"replicas"`
	ReplicasFrom   string       `json:"replicasFrom"`
	Headroom       *float64     `json:"headroom,omitempty"`
	Rate           *float64     `json:"rate,omitempty"`
	// RateSource says what set Rate where a flag did not: the prefill knee.
	RateSource string    `json:"rateSource,omitempty"`
	TPOT       *wireTPOT `json:"tpot,omitempty"`
}

// wireShape is the traffic shape a variant was priced at: each length
// measured (the request-rate-weighted mean of the variant's last cycles with
// traffic, FromCycle to ToCycle) or assumed by a flag.
type wireShape struct {
	InputTokens  float64 `json:"inputTokens"`
	InputSource  string  `json:"inputSource"`
	OutputTokens float64 `json:"outputTokens"`
	OutputSource string  `json:"outputSource"`
	// OutputDispersion is D = E[O^2]/E[O]^2 as read, the KV per request priced
	// at ILeff + D*OL/2; absent with no output-length histogram, where it is
	// flagged OutputSpreadUnread and priced at ILeff + OL/2.
	OutputDispersion float64 `json:"outputDispersion,omitempty"`
	PrefixHitRate    float64 `json:"prefixHitRate"`
	FromCycle        int     `json:"fromCycle"`
	ToCycle          int     `json:"toCycle"`
	Cycles           int     `json:"cycles"`
}

// The sources a shape's length comes from.
const (
	shapeMeasured = "measured"
	shapeAssumed  = "assumed"
	// shapeClamped is a measured prompt spread held at whatif.PromptCVStop.
	shapeClamped = "clamped"
)

// wireLine is the ITL line and its readings.
type wireLine struct {
	A            float64 `json:"a"`
	B            float64 `json:"b"`
	Tier         string  `json:"tier"`
	BorrowedFrom string  `json:"borrowedFrom,omitempty"`
	Readings     int     `json:"readings"`
	// ObservedFill is the range of fill the readings were taken at, absent
	// with no readings; FromCycle and ToCycle are the cycles they span.
	ObservedFill *wireInterval `json:"observedFill,omitempty"`
	FromCycle    int           `json:"fromCycle"`
	ToCycle      int           `json:"toCycle"`
	// Resamples is how many bootstrap resamples were drawn, and
	// TierChanges how many of them the tracker's choice of fit put at
	// another tier (TierUnstable above a tenth).
	Resamples   int `json:"resamples"`
	TierChanges int `json:"tierChanges"`
	// Check is the line against the recording's rows, absent with no line.
	Check *wireLineCheck `json:"check,omitempty"`
}

// wireLineCheck is whatif.LineCheck on the wire. Status is ok,
// ok-at-another-shape, mismatch, or not-run under whatif.MinLineRows rows.
// OtherShape says the same of a mismatch.
type wireLineCheck struct {
	Status         string  `json:"status"`
	Compared       int     `json:"compared"`
	OnRamp         int     `json:"onRamp"`
	AboveBand      int     `json:"aboveBand"`
	TooOld         int     `json:"tooOld"`
	Past           int     `json:"past"`
	MedianErrorPct float64 `json:"medianErrorPct"`
	WorstErrorPct  float64 `json:"worstErrorPct"`
	SignedErrorPct float64 `json:"signedErrorPct"`
	FromCycle      int     `json:"fromCycle"`
	ToCycle        int     `json:"toCycle"`
	// RowKVPerRequest is the median KV a request held on the rows compared.
	RowKVPerRequest float64 `json:"rowKVPerRequest"`
	OtherShape      bool    `json:"otherShape"`
}

// The statuses of a line check.
const (
	checkOK         = "ok"
	checkOtherShape = "ok-at-another-shape"
	checkMismatch   = "mismatch"
	checkNotRun     = "not-run"
)

// wireCheck is the line check on the wire, nil with no line.
func wireCheck(pv pricedVariant) *wireLineCheck {
	if pv.state.Line.IsZero() {
		return nil
	}
	c := pv.check
	w := &wireLineCheck{Status: checkOK, Compared: c.Compared, OnRamp: c.OnRamp, AboveBand: c.AboveBand, TooOld: c.TooOld, Past: c.Past,
		MedianErrorPct: c.MedianErrorPct, WorstErrorPct: c.WorstErrorPct, SignedErrorPct: c.SignedErrorPct, FromCycle: c.FromCycle,
		ToCycle: c.ToCycle, RowKVPerRequest: c.RowKVPerRequest, OtherShape: c.OtherShape}
	switch {
	case !c.Ran():
		w.Status = checkNotRun
	case c.Mismatch():
		w.Status = checkMismatch
	case c.OtherShape:
		w.Status = checkOtherShape
	}
	return w
}

// wireTPOT is the TPOT question: the target (question 1 only), the
// percentile and R_q.
type wireTPOT struct {
	TargetSeconds float64 `json:"targetSeconds,omitempty"`
	Percentile    float64 `json:"percentile,omitempty"`
	Ratio         float64 `json:"ratio,omitempty"`
	RatioSource   string  `json:"ratioSource,omitempty"`
	Windows       int     `json:"windows"`
}

// wireResults is a role's results by name; a question fills its own.
type wireResults struct {
	Knee           *wireResult `json:"knee,omitempty"`
	OperatingRate  *wireResult `json:"operatingRate,omitempty"`
	MeanTPOT       *wireResult `json:"meanTPOT,omitempty"`
	PercentileTPOT *wireResult `json:"percentileTPOT,omitempty"`
	Rate           *wireResult `json:"rate,omitempty"`
	// TTFT is the one result with parts, so it has a type of its own.
	TTFT *wireTTFTResult `json:"ttft,omitempty"`
}

// wireResult is an answer or a refusal, never both.
type wireResult struct {
	Answer  *wireAnswer  `json:"answer,omitempty"`
	Refused *wireRefusal `json:"refused,omitempty"`
}

// wireAnswer is whatif.Answer on the wire.
type wireAnswer struct {
	Value float64       `json:"value"`
	Unit  string        `json:"unit"`
	Band  *wireInterval `json:"band,omitempty"`
	Basis wireBasis     `json:"basis"`
	Flags []string      `json:"flags"`
	// Sensitivity is the figure's span at V_P and f either way, absent on a
	// figure that does not rest on them.
	Sensitivity *wireSensitivity `json:"sensitivity,omitempty"`
}

// wireSensitivity is whatif.Sensitivity on the wire: Refused is a corner's
// reason, and then the worse side of the span says nothing.
type wireSensitivity struct {
	Lo      float64 `json:"lo"`
	Hi      float64 `json:"hi"`
	Refused string  `json:"refused,omitempty"`
}

// toWireSensitivity is a span on the wire.
func toWireSensitivity(s *whatif.Sensitivity) *wireSensitivity {
	if s == nil {
		return nil
	}
	return &wireSensitivity{Lo: s.Lo, Hi: s.Hi, Refused: string(s.Refused)}
}

// wireTTFTResult is a priced TTFT or a refusal, never both.
type wireTTFTResult struct {
	Answer  *wireTTFTAnswer `json:"answer,omitempty"`
	Refused *wireRefusal    `json:"refused,omitempty"`
}

// wireTTFTAnswer is whatif.TTFT on the wire: the value, its flags, and in
// Parts what it is made of.
type wireTTFTAnswer struct {
	Value float64       `json:"value"`
	Unit  string        `json:"unit"`
	Basis wireBasis     `json:"basis"`
	Flags []string      `json:"flags"`
	Parts wireTTFTParts `json:"parts"`
	// Sensitivity is as on wireAnswer.
	Sensitivity *wireSensitivity `json:"sensitivity,omitempty"`
}

// wireTTFTParts is the figures beside the union bound, and its parts' means,
// in seconds.
type wireTTFTParts struct {
	Independent           float64 `json:"independent"`
	Pooled                float64 `json:"pooled"`
	DecodeShare           float64 `json:"decodeShare"`
	EngineWait            float64 `json:"engineWait"`
	OwnPrefill            float64 `json:"ownPrefill"`
	DecodeWait            float64 `json:"decodeWait"`
	FirstStep             float64 `json:"firstStep"`
	DecodeEngine          float64 `json:"decodeEngine"`
	RouterHop             float64 `json:"routerHop"`
	PrefillUtilization    float64 `json:"prefillUtilization"`
	VPMarginApplied       float64 `json:"vpMarginApplied"`
	WaitScale             float64 `json:"waitScale"`
	WaitOffset            float64 `json:"waitOffset"`
	DecodeWaitProbability float64 `json:"decodeWaitProbability"`
	PrefillKnee           float64 `json:"prefillKnee"`
	DecodeKnee            float64 `json:"decodeKnee"`
	// Onsets are the flow-control onsets, absent with flow control off.
	Onsets *wireOnsets `json:"onsets,omitempty"`
}

// wireOnsets is whatif.Onsets on the wire: each stage's rate, r/s.
type wireOnsets struct {
	Prefill float64 `json:"prefill"`
	Decode  float64 `json:"decode"`
}

// wireBasis is whatif.Basis on the wire; a field that does not apply is 0.
type wireBasis struct {
	Fill     float64  `json:"fill"`
	KneeRate float64  `json:"kneeRate"`
	Floor    float64  `json:"floor"`
	Uncapped float64  `json:"uncapped"`
	BoundBy  []string `json:"boundBy"`
	Tier     string   `json:"tier"`
	// PrefillOnlyRate is the rate the prefill side alone allows, on a TTFT
	// rate; Floor and KneeRate then read the TTFT's (whatif.TTFTBasis).
	PrefillOnlyRate float64 `json:"prefillOnlyRate,omitempty"`
}

// wireRefusal is whatif.Unanswerable on the wire.
type wireRefusal struct {
	Reason string `json:"reason"`
	Detail string `json:"detail"`
	// Flags are what the refusal rests on; AssumedTermDecides says an assumed
	// term alone refused it.
	Flags              []string `json:"flags,omitempty"`
	AssumedTermDecides bool     `json:"assumedTermDecides,omitempty"`
}

// toWireRefusal is a refusal on the wire.
func toWireRefusal(u whatif.Unanswerable) *wireRefusal {
	w := &wireRefusal{Reason: string(u.Reason), Detail: u.Detail, AssumedTermDecides: u.AssumedTermDecides}
	for _, f := range u.Flags {
		w.Flags = append(w.Flags, string(f))
	}
	return w
}

// wireInterval is a range, Lo to Hi.
type wireInterval struct {
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
}

// The units a result's value is in.
const (
	unitRate    = "r/s"
	unitSeconds = "s"
)

// toWire is a Result on the wire, in unit; nil for a question not asked.
func toWire(r whatif.Result, unit string) *wireResult {
	switch v := r.(type) {
	case whatif.Answer:
		a := &wireAnswer{Value: v.Value, Unit: unit, Flags: []string{}, Sensitivity: toWireSensitivity(v.Sensitivity), Basis: wireBasis{
			Fill: v.Basis.Fill, KneeRate: v.Basis.KneeRate, Floor: v.Basis.Floor, Uncapped: v.Basis.Uncapped,
			BoundBy: []string{}, Tier: v.Basis.Tier,
		}}
		if v.Band != nil {
			a.Band = &wireInterval{Lo: v.Band.Lo, Hi: v.Band.Hi}
		}
		for _, b := range v.Basis.BoundBy {
			a.Basis.BoundBy = append(a.Basis.BoundBy, string(b))
		}
		if t := v.Basis.TTFT; t != nil {
			a.Basis.Floor, a.Basis.KneeRate, a.Basis.PrefillOnlyRate = t.Floor, t.KneeRate, t.PrefillOnlyRate
		}
		for _, f := range v.Flags {
			a.Flags = append(a.Flags, string(f))
		}
		return &wireResult{Answer: a}
	case whatif.Unanswerable:
		return &wireResult{Refused: toWireRefusal(v)}
	}
	return nil
}

// toWireTTFT is a priced TTFT or a refusal on the wire.
func toWireTTFT(r whatif.Result) *wireTTFTResult {
	switch v := r.(type) {
	case whatif.TTFT:
		a := &wireTTFTAnswer{Value: v.Seconds, Unit: unitSeconds, Flags: []string{}, Sensitivity: toWireSensitivity(v.Sensitivity), Basis: wireBasis{
			Floor: v.Floor, KneeRate: v.PrefillKnee, BoundBy: []string{}, Tier: v.Tier,
		}, Parts: wireTTFTParts{
			Independent: v.Independent, Pooled: v.Pooled, DecodeShare: v.DecodeShare,
			EngineWait: v.Parts.EngineWait, OwnPrefill: v.Parts.OwnPrefill, DecodeWait: v.Parts.DecodeWait,
			FirstStep: v.Parts.FirstStep, DecodeEngine: v.Parts.DecodeEngine, RouterHop: v.Parts.RouterHop,
			PrefillUtilization: v.PrefillUtilization, VPMarginApplied: v.VPMarginApplied,
			WaitScale: v.WaitScale, WaitOffset: v.WaitOffset, DecodeWaitProbability: v.DecodeWaitProbability,
			PrefillKnee: v.PrefillKnee, DecodeKnee: v.DecodeKnee,
		}}
		if v.Onsets != (whatif.Onsets{}) {
			a.Parts.Onsets = &wireOnsets{Prefill: v.Onsets.Prefill, Decode: v.Onsets.Decode}
		}
		for _, f := range v.Flags {
			a.Flags = append(a.Flags, string(f))
		}
		return &wireTTFTResult{Answer: a}
	case whatif.Unanswerable:
		return &wireTTFTResult{Refused: toWireRefusal(v)}
	}
	return nil
}

// newWireOutput starts a command's JSON output from the state it read.
func newWireOutput(question string, f *answerFlags, st *learnreplay.State, variants []pricedVariant, code int) wireOutput {
	r := f.resampling()
	w := wireOutput{
		SchemaVersion: schemaVersion, Question: question, ExitCode: code, Roles: []wireRole{},
		Source: &wireSource{Recording: f.recording, Namespace: st.Namespace, Model: st.ModelID,
			FirstCycle: 1, LastCycle: st.Cycle, TotalCycles: st.Cycles, FirstAt: st.FirstAt, LastAt: st.At},
		Bands: &wireBands{Method: "block bootstrap", BlockCycles: r.Block, Resamples: r.Resamples, Seed: r.Seed,
			LoPercent: 10, HiPercent: 90},
		Note: footer(st, variants),
	}
	if o := f.captured; o != nil {
		roles := "role-of"
		if o.kubeStateMetrics {
			roles = "kube-state-metrics"
		}
		w.Source.Window = &wireWindow{Prometheus: redactedURL(f.window.prometheus), From: o.from, To: o.to,
			StepSeconds: o.step.Seconds(), Roles: roles}
	}
	return w
}

// addTTFTNote closes the note with what every TTFT answer says of itself;
// nothing with no TTFT asked.
func (w *wireOutput) addTTFTNote(fleets []*pricedFleet) {
	if len(fleets) > 0 {
		w.Note += "; " + footerNote(fleets)
	}
}

// sourcePrefillKnee names the prefill knee as what set an operating rate.
const sourcePrefillKnee = "headroom x prefill knee"

// wire is the fleet's prefill role.
func (o operatingTTFT) wire() wireRole {
	return wireRole{Role: rolePrefill, Variant: o.fleet.variant, TTFT: &o.fleet.in, Results: wireResults{
		Knee: toWire(o.knee, unitRate), TTFT: toWireTTFT(o.ttft)}}
}

// inputsOf is the wire's account of what a variant was priced from.
func inputsOf(f *answerFlags, pv pricedVariant) *wireInputs {
	in := &wireInputs{
		Line: wireLine{A: pv.decode.Line.A, B: pv.decode.Line.B, Tier: pv.state.Tier,
			BorrowedFrom: pv.state.BorrowedFrom, Readings: len(pv.state.LineReadings),
			Resamples: pv.lines.Drawn, TierChanges: pv.lines.TierChanges(),
			FromCycle: pv.state.ReadingsFrom, ToCycle: pv.state.ReadingsTo, Check: wireCheck(pv)},
		FillBand:       wireInterval{Lo: pv.decode.Band.Lo, Hi: pv.decode.Band.Hi},
		CapacityTokens: pv.decode.CapacityTokens, KVPerRequest: pv.decode.KVPerRequest,
		Shape: wireShape{InputTokens: pv.shape.AvgInputTokens, InputSource: shapeMeasured,
			OutputTokens: pv.shape.AvgOutputTokens, OutputSource: shapeMeasured, PrefixHitRate: pv.shape.PrefixHitRate,
			FromCycle: pv.state.Shape.FromCycle, ToCycle: pv.state.Shape.ToCycle, Cycles: pv.state.Shape.Cycles,
			OutputDispersion: pv.state.Shape.OutputDispersion},
		BatchCap: pv.perRank, BatchCapSource: pv.state.BatchCapSource,
		Ranks: max(pv.state.Ranks, 1), Replicas: pv.decode.Replicas, ReplicasFrom: "recording",
	}
	if pv.assumedIL {
		in.Shape.InputSource = shapeAssumed
	}
	if pv.assumedOL {
		in.Shape.OutputSource = shapeAssumed
	}
	if f.pricedBatchCap() > 0 {
		in.BatchCapSource = "--batch-cap"
	}
	if f.decode > 0 {
		in.ReplicasFrom = "--decode"
	}
	if lo, hi, ok := pv.observedFill(); ok {
		in.Line.ObservedFill = &wireInterval{Lo: lo, Hi: hi}
	}
	return in
}

// writeJSON writes v indented, with a final newline.
func writeJSON(out io.Writer, v wireOutput) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
