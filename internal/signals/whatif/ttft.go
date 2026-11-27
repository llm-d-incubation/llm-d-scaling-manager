package whatif

import (
	"fmt"
	"math"
	"slices"
)

// This file is the TTFT model of the what-if planner (PR P3 of
// docs/proposals/slo-capacity-planner.md; the model is
// docs/proposals/slo-capacity-planner-ttft.md):
//
//	TTFT = W_router + W_engine + own_prefill + W_decode + D(I, ITL(k_d)) + R
//
// on a P/D fleet, D the decode engine's TTFT and R the hop between the router
// and the engines, both learned (DecodeTerm). Aggregated TTFT is not built (M1).
//
// UNVALIDATED: the only caller in the controller is the SLO analyzer's shadow
// pricing, which sizes nothing (slo.SwitchTTFTPrice). The plan
// has P3 depend on P2, the prefill-stress GPU recording kept as a golden; that
// recording is blocked on cluster access, so P3 is built against closed-form
// queueing values and synthetic fleets instead, and every figure carries
// UnvalidatedTTFT. What P2 must check is listed on the TTFT page, "As built
// (P3)". The earlier TTFT = A*T + B fit is not repeated: TTFT includes the
// queue wait, which landed in that fit's intercept (82 s;
// docs/proposals/prefill-ttft-model.md). Here the wait is a queue of its own,
// and the fixed cost a is measured at low load, where there is none.

// Prefill is one prefill role as the TTFT model prices it: Replicas pods,
// each a work-conserving server of prompt tokens at V_P, the busiest of them
// fed f times its share of the arrivals. The figures are P1's
// (internal/signals/prefill; PrefillFromEstimate maps them), or assumed and
// flagged.
type Prefill struct {
	// VP is V_P, one pod's prompt-token throughput while it has work, in
	// tokens per second.
	VP float64
	// VPLowerBound marks a V_P from a source that is a lower bound
	// (VPLowerBound), VPAboveSlopeBound one above the low-load slope's
	// bound, and VPAssumed one the caller supplied (AssumedVP).
	VPLowerBound, VPAboveSlopeBound, VPAssumed bool
	// VPServedOnly marks a V_P that is only the rate a pod was seen to
	// compute, on a fleet that never queued (prefill.SourceServedPeak,
	// NeverQueued): the knee priced from it stands as a lower bound, and
	// every TTFT priced from it is refused NoPrefillCapacity, since the true
	// V_P may be far higher and the figure would be an upper bound at best
	// (issue llm-d-incubation/llm-d-scaling-manager#369). A V_P the caller
	// supplies (VPAssumed) is priced as given.
	VPServedOnly bool
	// VPFromPlateau marks a V_P read from the occupancy plateau
	// (VPFromPlateau).
	VPFromPlateau bool
	// ExperimentalTTFT marks a fleet priced with the planner's
	// --experimental-ttft: every figure priced from it carries the flag
	// ExperimentalTTFT.
	ExperimentalTTFT bool
	// FixedLatency is a, in seconds: the latency one request pays on a pod
	// besides its prompt's work. It enters the request's own latency only,
	// never the queue's work (busy-minute V_P is already net of it).
	FixedLatency float64
	// Imbalance is f, at least 1 and at most Replicas: the busiest pod's
	// share of the work over the mean share. 0 is unknown, which one pod
	// prices as 1 and more than one refuses (NoImbalance).
	// ImbalanceAssumed marks one the caller supplied (AssumedImbalance).
	Imbalance        float64
	ImbalanceAssumed bool
	// LoadAwareRouter prices f as a load-aware router spreads it
	// (prefillPlan.fAt): f up to ImbalanceUtilization, then falling to 1 as
	// the pods fill, so the knee is the pooled one and f prices only the
	// latency below it. Off, f holds at every rate. Opt-in
	// (DefaultPlannerLoadAwareRouter): no recording says what the router
	// scores.
	LoadAwareRouter bool
	// ImbalanceUtilization is rho_m, the mean prefill pod utilization f was
	// read at: P1's load over VP (prefill.Estimate.FLoad), in
	// [0, MaxImbalanceUtilization], rescaled to the question's prompt where
	// V_P is (prefillPlan.rhoM). 0 is not known, and f is then held. Read
	// only with LoadAwareRouter.
	ImbalanceUtilization float64
	// ImbalanceCurveFloor and ImbalanceCurveExcess are f against the load
	// per pod, f(u) = floor + excess/u, u = rate * E[I_eff] / N_p the
	// computed tokens a second a pod (prefill.FLoadCurve, learned at this
	// count). Set, f at a rate is the higher of the plan's f, as it would
	// price it, and the curve's, at most N_p (fAt), and the knee is where
	// f(u) * rho reaches 1 - VPMargin. Both 0 is none; set, both are above
	// 0. The planner sets them with --prefill-f-load-curve; the SLO analyzer
	// never does.
	ImbalanceCurveFloor, ImbalanceCurveExcess float64
	// Replicas is N_p.
	Replicas int
	// PromptTokens is E[I_eff], the mean prompt a pod computes (prefix
	// hits are not prefill work).
	PromptTokens float64
	// PromptCV is the coefficient of variation of I_eff, in [0, 10]: the
	// prompt is taken as gamma distributed with it (exponential at 1,
	// constant at 0), and Cs^2 = PromptCV^2. PromptCVAssumed marks one the
	// caller assumed (AssumedPromptSpread).
	PromptCV        float64
	PromptCVAssumed bool
	// PromptMaxRatio is the longest effective prompt over the mean, as the
	// computed-prompt histogram bounds it (PromptSpread.MaxRatio): a figure
	// of the traffic, as PromptCV is, so it holds at any prompt asked. Set,
	// the prompt is priced as its gamma held at that bound (censored there)
	// and processor sharing holds at any cv while it is at most
	// MaxPSPromptSupport. 0 is no bound: the gamma runs on and the spread
	// alone decides (MaxPSPromptCV). The slo analyzer sets it with
	// slo.SwitchTTFTPromptSupport, the planner where its replay turns that
	// switch on (off by default, and not in learnreplay.PlannerSwitches;
	// issue llm-d-incubation/llm-d-scaling-manager#304).
	PromptMaxRatio float64
	// ArrivalCV2 is Ca^2, the arrivals' squared coefficient of variation:
	// Kingman's 1 + Cs^2 becomes Ca^2 + Cs^2 in the mean wait, and the wait's
	// tail stretches by their ratio (engineWait). 0 means Poisson, 1.
	ArrivalCV2 float64
	// TransferSecondsPerToken is the KV transfer's time per prompt token,
	// kappa/BW. 0 omits T_xfer, flagged TransferOmitted: it is folded into
	// the request's own latency, a + I*(1/V_P + kappa/BW), not the queue's
	// work. With no decode line and the decode engine's TTFT measured
	// (TTFTFleet.DecodeTTFT) it is not priced: that TTFT holds the KV pull.
	TransferSecondsPerToken float64
	// VPPromptTokens is I_vp, the mean effective prompt V_P was read at
	// (prefill.Estimate.VPPromptTokens); 0 is unknown.
	VPPromptTokens float64
	// VPWholePromptTokens is the mean whole prompt V_P was read at, prefix
	// hits included, and VPPromptHitRate the prefix-cache hit rate then
	// (prefill.Estimate.VPWholePromptTokens, VPPromptHitRate);
	// WholePromptTokens and PromptHitRate are the question's. A whole prompt
	// of 0 is unknown: with either unknown, V_P is shaped as before
	// (shapedVP, prefixMoved).
	VPWholePromptTokens, VPPromptHitRate float64
	WholePromptTokens, PromptHitRate     float64
	// VPPromptLow and VPPromptHigh are the range of effective prompts the
	// minutes V_P rests on read (prefill.Estimate.VPPromptLow, VPPromptHigh,
	// set with saturation.SwitchPrefillVPPromptCoverage); 0 is not known.
	// Known, a mean prompt more than MaxVPShapeRatio outside them is out of
	// the region V_P was learned in (promptNotCovered): the knee carries
	// VPPromptNotCovered and no TTFT is priced. An assumed V_P is never
	// checked against them.
	VPPromptLow, VPPromptHigh float64
	// VPRequestSeconds is r, the part of a request's work on a pod that does
	// not grow with its prompt, in pod-seconds, where V_P's source measured
	// it (prefill.Estimate.VPRequestSeconds): at I_vp (VPPromptTokens) a
	// request's work is I_vp/V_P, r of it paid per request. Set with I_vp
	// known and r under I_vp/V_P, V_P is priced at the question's prompt
	// along that split (shapedVP), r held and the rest per token; 0 is not
	// measured, and V_P is one token rate at every prompt, as before. An
	// assumed V_P never reads it.
	VPRequestSeconds float64
	// PrefixAttention prices the attention over a prefix hit's cached
	// tokens in the curve's rescale (shapedVP): off, I_eff stands for the
	// context, as if no token were cached. The replay switch
	// slo.SwitchTTFTPrefixAttention, planner-only and off.
	PrefixAttention bool
	// CachedContextRead says CachedContextTokens and VPCachedContextTokens
	// were read: the cached context of the question's mean request and of
	// V_P's minutes, as the whole prompt less the computed one, every
	// tier's hits counted (read with slo.SwitchTTFTPrefixAttention, issue
	// llm-d-incubation/llm-d-scaling-manager#383). With PrefixAttention
	// on, shapedVP takes the attention span from them in place of the
	// prefix-cache hit rates, which count the GPU's hits alone.
	CachedContextRead                          bool
	CachedContextTokens, VPCachedContextTokens float64
	// TierLoadSeconds is the time a request waits for its cached KV to load
	// from a tier outside the GPU (a host, disk or peer tier), in seconds:
	// like a it enters the request's own latency, never the queue's work,
	// and it is reported apart (TTFTParts.TierLoad). 0 is none
	// (slo.SwitchTTFTTierLoad off, or no such hit).
	TierLoadSeconds float64
	// CurveSlope and CurveCurvature are b and c of P1's low-load curve
	// s0 + b*I + c*I*I (prefill.Estimate.ASlope and ACurve, with AModel
	// quadratic): a prompt of length I costs b + c*I per token in a busy
	// step. Both positive is a curve; otherwise there is none, and V_P is
	// not rescaled (shapedVP).
	CurveSlope, CurveCurvature float64
	// VPMargin is the relative margin taken off V_P near the knee, in
	// [0, MaxVPMargin]: none below MarginRampStart of the worst pod's
	// utilization, all of it from MarginRampFull, linear between
	// (prefillPlan.atRate). PrefillFromEstimate sets DefaultVPMargin; 0 is
	// none.
	VPMargin float64
	// RunningCap is the most prompts one prefill pod computes at once: past
	// it a prompt waits in the engine's queue, which is what the flow-control
	// gate reads. On vLLM with chunked prefill it is about
	// max_num_batched_tokens over the prompt, at most max_num_seqs. Read only
	// with TTFTFleet.PSOnsets. 0 is not known: the onset is then the lower of
	// processor sharing's at a cap of 1, the least an engine runs, and the
	// first-come one, so never above the onset with PSOnsets off.
	RunningCap int
	// Queue is the queue model the caller chose for the prefill pods, and
	// why. The zero value is the model's own rule, as before: with
	// TTFTFleet.ProcessorSharing, processor sharing, or the larger of it and
	// first come past MaxPSPromptCV (BeyondProcessorSharing).
	Queue QueueChoice
}

// QueueModel names a model of the prefill pods' queue. The planner chooses one
// per fleet (Prefill.Queue); issue llm-d-incubation/llm-d-scaling-manager#387
// is to choose among them by their fit on held-out data, with the engine's
// configuration as the prior.
type QueueModel string

const (
	// QueueByRule is the model's own rule (the zero value): processor sharing
	// where the spread allows it, else the larger of it and first come.
	QueueByRule QueueModel = ""
	// QueueProcessorSharing is processor sharing whatever the prompt's spread:
	// the engine runs several prompts at once in a step budget that holds
	// several typical computed prompts, so a long prompt is chunked between
	// the short ones rather than served first come. Past QueueChoice.Slots
	// prompts on the busiest pod (rho^(slots+1) above MaxPSOverSlots) the
	// excess waits first come, and the price is the larger of the two queues',
	// flagged PSSlotsExceeded.
	QueueProcessorSharing QueueModel = "processor-sharing"
)

// QueueChoice is the prefill queue model chosen for a fleet, the prompts one
// pod runs at once under it (0 is not known), and the reason, in words.
type QueueChoice struct {
	Model  QueueModel
	Slots  int
	Reason string
}

// MaxPSOverSlots is the largest chance, at the busiest pod's utilization, that
// a processor-sharing pod holds more prompts than it runs at once
// (QueueChoice.Slots) for QueueProcessorSharing to price it alone. Stated,
// not measured: past it the first-come queue in front of the running prompts
// is no longer rare.
const MaxPSOverSlots = 0.05

// MaxVPShapeRatio is how far, either way, the mean prompt may be from the one
// V_P was read at before V_P is flagged VPShapeMoved, when there is no curve
// to rescale it by (S1's 1.2x rule, docs/proposals/slo-tiers.md, decision 2).
const MaxVPShapeRatio = 1.2

// DefaultVPMargin is the margin PrefillFromEstimate puts on a learned V_P near
// the knee: 5%, ten times the 0.5% by which run 2's pod under a queue served
// away from its busy-minute V_P at the prompt it was learned on
// (docs/proposals/slo-capacity-planner-ttft.md, "As built (P3 after P2)").
// A stated margin, not a measured one, and it covers only that noise, at
// the prompt length V_P was learned at. It does not cover an error in the
// curve's rescale or in pricing a mix of prompt lengths at its mean (an even
// 8k/52k mix is 32% under its prefill time, "Where it is most wrong").
const DefaultVPMargin = 0.05

// MaxVPMargin bounds VPMargin at 1 - MarginRampFull, so the margin is whole
// before the margined pod saturates and the knee is (1 - VPMargin) of the
// unmargined one (prefillPlan.knee).
const MaxVPMargin = 1 - MarginRampFull

// MarginRampStart and MarginRampFull are the worst pod's utilization, at the
// unmargined V_P, where the margin starts and where it is whole. Near the
// knee TTFT is most sensitive to V_P: on the M/M/1 sojourn, the model's
// tail at cv 1, a V_P read (1 + e) high puts TTFT at
// (1-rho)/((1+e)(1-rho) + e*rho) of the truth. At e = DefaultVPMargin that
// is 9.1% short at rho 0.5, 10.0% at 0.55, 14.3% at 0.7 and 25% at 0.85: it
// crosses the S1 mark's 10% under-side at 0.55, and the ramp is centred on
// that crossing (TestVPMarginRampIsWhereTTFTTurnsSensitive). Below it the
// error is under the mark on its own, and no margin is spent. The ramp is
// linear, not a step, so TTFT has no jump in the rate.
const (
	MarginRampStart = 0.5
	MarginRampFull  = 0.6
)

// maxPrefillReplicas bounds N_p: the pooled figure's Erlang C recursion
// runs once per pod, as the admission chain runs once per slot.
const maxPrefillReplicas = maxSlots

// maxPromptCV bounds the prompt's coefficient of variation: past 10 the
// gamma's shape is below 0.01 and the model's exponential tail says nothing.
const maxPromptCV = 10

// minPromptCV is the least prompt spread priced as a gamma; below it the
// prompt is priced as constant. The gamma's shape 1/cv^2 is then above 1e8,
// where its incomplete gamma loses its digits to cancellation (cv 1e-7 read
// 8% high); the two figures differ by less than the spread itself.
const minPromptCV = 1e-4

// MaxPSPromptCV is the widest prompt spread the processor-sharing prefill is
// priced on alone. The staircase that validated it sent prompts at cv 0.058,
// and the engine's chunked prefill need not share a prompt longer than its
// step budget (max_num_batched_tokens) as processor sharing does: at a wide
// spread it may serve the long ones closer to first come. Past this cv,
// about four times the spread measured, a TTFT with
// TTFTFleet.ProcessorSharing is the larger of the two queues' figures,
// flagged SpreadBeyondProcessorSharing, until a run at a wide spread
// validates processor sharing there.
const MaxPSPromptCV = 0.25

// MaxPSPromptSupport is the longest prompt over the mean
// (Prefill.PromptMaxRatio) at which processor sharing is priced alone,
// whatever the cv read. The cv a histogram reads is the widest its buckets
// allow (ReadPromptSpread): every 8000-token prompt in a (5000, 10000] bucket
// read 0.31 against a true 0.06, past MaxPSPromptCV, although processor
// sharing held on those steps. What MaxPSPromptCV guards is a prompt long
// enough for chunked prefill to serve it closer to first come, and a support
// bound says there is none: 1.6 is under the 99th percentile of the gamma at
// MaxPSPromptCV (1.67 times the mean), so no prompt it admits is longer than
// one in a hundred of a spread the cv rule already prices under processor
// sharing alone. The 8000-token steps sit at 1.25.
const MaxPSPromptSupport = 1.6

// DefaultPriceRouterWait is TTFTFleet.PriceRouterWait's default, for the CLI
// (--price-router-wait), the planner API (assume.routerWait gateStage) and the SLO analyzer
// (slo.SwitchTTFTRouterWait) alike: off, so a TTFT past the prefill onset is
// flagged AboveFlowControlOnset, a miss. On (opt-in) prices the gate as a
// stage (ttft_gate.go). It reads at least 0.9 of the client at the mean, p90
// and p95 at the one staircase step past the prefill onset that the fleet
// held, and moves no figure below the onsets
// (TestStaircaseQ32_SwitchOn_TheGateStageHoldsTheClient); off, the price
// reads short there (TestStaircaseQ32_SwitchOff_ReadsShortAtTheHeldStep).
// One held step is not enough to make it the default, and the backtests of
// four runs' flow-control-on phases (router v0.11.0, 41 steps past the
// prefill onset) keep it off: the gate there either held or collapsed on its
// TTL, at nearly one rate, so at held steps the stage read up to 5.1 times
// the router's TTFT, at all 15 collapses it still read more than 20 % short,
// and at 2 of the 26 held steps too (docs/developer-guide/testing.md, "Flow
// control past the prefill onset, backtested"). Router v1.0.0 releases no
// burst to one pod, so the stage is not its model.
const DefaultPriceRouterWait = false

// DefaultRouterWaitInPrefill is TTFTFleet.RouterWaitInPrefill's default, for
// the CLI (--router-wait-in-prefill) and the planner API (assume.routerWait
// absent): on, so past the prefill onset, up to MaxRouterWaitKneeShare of the
// prefill knee, a TTFT is flagged FlowControlWaitPriced and stands, and a
// rate answer may pass the onset. --price-router-wait, or
// --router-wait-in-prefill=false (assume.routerWait gateStage or none), turns
// it off. The SLO analyzer's switch for it, slo.SwitchTTFTRouterWaitInPrefill,
// is off from its constructor until a live check with the controller.
//
// On by the router v1.0.0 staircase of issue #287 (Qwen3-32B, 2 prefill and
// 2 decode, three seeds at Ca^2 4 and two at Poisson), backtested against its
// warm-up: at the 15 scored steps past the onset where the switch took, the
// figure read 1.33 to 3.39 times the router's measured TTFT at the mean, p90
// and p95, never under; no step at or under 0.9 of the knee collapsed; and
// the client's mean and p95 TTFT with flow control on were 0.88 to 0.99 of
// those with it off at the same arrivals, so the wait moves rather than
// adds. One further switched step, which the backtest excludes as
// warm-up, read 0.78 at the p95: both prefill pods were replaced in its
// window. Its exclusion was decided after the run.
//
// Router v0.11.0 is not this model: there the backtests read this figure
// more than 20 % short at 18 of 41 steps past the onset, 15 of them where
// the gate collapsed on its TTL. The planner cannot read the router's
// version: on v0.11.0 set --router-wait-in-prefill=false (or
// --price-router-wait).
const DefaultRouterWaitInPrefill = true

// MaxRouterWaitKneeShare is the highest rate, as a share of the prefill knee,
// at which TTFTFleet.PriceRouterWait or RouterWaitInPrefill takes; past it
// the wait keeps AboveFlowControlOnset. The router v1.0.0 steps behind
// RouterWaitInPrefill reached 0.88 of the knee.
//
// Carried over from the switch's earlier form, which took the gate as
// moving the wait rather than adding to it, and was bounded by a paired
// simulation that does not model the stale score's burst to one pod
// (TestFlowControlWait_GatedPodsAreNoSlower: the gate's own effect up to
// 0.90 of the knee at most +1.3 %, at 0.97 +16 %). The gate stage
// (ttft_gate.go) has no basis of its own for the share yet: the one held
// staircase step it was checked at sat at 0.80 of the knee, and the next,
// void, at 0.82. Until a run places held steps near it, the share is the old
// one, not a measured edge of the stage.
const MaxRouterWaitKneeShare = 0.9

// DefaultPSOnsets is TTFTFleet.PSOnsets's default, for the CLI
// (--ps-onsets) and the SLO analyzer (slo.SwitchTTFTPSOnsets) alike: off.
// The placement it switches to is derived, not measured: the second
// staircase could not tell it from the first-come one, and on the Qwen3-32B
// staircase, at the engine's own running cap, it put bucket C's onset past a
// step that measured a wait of seconds, where the first-come onset fell
// between the last step with no wait and the first with one
// (TestPSOnsets_TheQ32CrossingKeepsThemOff).
const DefaultPSOnsets = false

// DefaultChargeDecodeGate is TTFTFleet.ChargeDecodeGate's default, for the
// CLI (--charge-decode-gate) and the SLO analyzer
// (slo.SwitchTTFTDecodeGateCharge) alike: off. No staircase has reached a
// decode onset, so the closed share it charges is not measured.
const DefaultChargeDecodeGate = false

// MaxDecodeGateClosedShare is the most of the time a mean TTFT target lets
// the decode gate be closed, with TTFTFleet.ChargeDecodeGate. A request that
// meets a closed gate waits at the router for a time the model does not
// price, so a mean has no tail to charge it to, as a percentile does
// (ttftPlan.gateClosed): it is a stated share, not a derived one, and up to
// that share of requests still wait unpriced. At the second staircase's
// flow-control steps that held, up to 2 % of requests waited over 10 ms at
// the router and the processor-sharing price still read at or above the
// client's mean (TestStaircaseR2_PSPriceReadsAboveTheClientNearTheOnset);
// this is half that. Those were the prefill gate's holds: no run has reached
// the decode gate, whose closed gate stops prefill too, so the share is not
// measured there.
const MaxDecodeGateClosedShare = 0.01

// FlowControlState is what is known of EPP flow control.
type FlowControlState int

const (
	// FlowControlOff is flow control known to be off (the upstream default):
	// the router admits everything, W_router = 0.
	FlowControlOff FlowControlState = iota
	// FlowControlNoSeries is off because its queue series does not exist:
	// priced as off, flagged FlowControlInferredOff.
	FlowControlNoSeries
	// FlowControlOn is the flowControl feature gate on, with the default
	// utilization detector under the static policy, one band.
	FlowControlOn
)

// FlowControl is EPP flow control as the TTFT model prices it. With it on,
// the gate closes when the unweighted mean of the endpoints' scores
// max(queueDepth/QueueThreshold, kvUsage/KVThreshold) reaches 1, per stage
// on P/D.
type FlowControl struct {
	State FlowControlState
	// QueueThreshold and KVThreshold are the utilization detector's
	// (upstream 5 and 0.8). Read only when State is FlowControlOn; not both
	// positive and finite there is FlowControlUnknown.
	QueueThreshold, KVThreshold float64
}

// TTFTFleet is the fleet a TTFT is priced on: the prefill role, the decode
// role it hands to, and flow control.
type TTFTFleet struct {
	Prefill Prefill
	// Decode is the decode role; nil means no decode line: W_decode is not
	// priced, and decode's part of the TTFT is DecodeTTFT, measured, in place
	// of T_xfer + ITL(k_d), flagged DecodeEngineMeasured, or left out,
	// flagged DecodeSideOmitted and DecodeTTFTUnmeasured. The SLO analyzer's
	// shadow pricing of a prefill variant (S1) with flow control off is that
	// case.
	Decode *Decode
	// DecodeTerm is decode's part with a decode line, D(I, ITL(k_d)) + R, in
	// place of T_xfer + ITL(k_d). Nil is no term: the old sum, flagged
	// DecodeTermOmitted. Read only with Decode set.
	DecodeTerm *DecodeTerm
	// LegacyDecodeSum prices T_xfer + ITL(k_d), with no flag, as the model did
	// before the term: slo.SwitchTTFTDecodeTerm off, for a replay.
	LegacyDecodeSum bool
	// ProcessorSharing prices the prefill pod, and with a decode line the KV
	// pull on the decode pod, as processor-sharing stages (ttft_ps.go): a
	// prompt is never queued, it shares the pod, and its stretch is its
	// wait. False is the first-come queue: a Kingman wait on the busiest
	// prefill pod and the pull at its unloaded time. On with
	// slo.SwitchTTFTProcessorSharing, and in the CLI.
	ProcessorSharing bool
	// LowerBoundKneeUnpriced refuses a rate at or above a prefill knee priced
	// from a V_P that is a lower bound (Prefill.VPLowerBound) as
	// PastLowerBoundKnee, not Saturated: the true knee is at or above it, so
	// a rate past it is one the model cannot price, not one the fleet cannot
	// carry. A rate past the decode line's knee stays Saturated, and one past
	// the decode gate's onset under flow control AboveDecodeOnset: both are
	// known whatever bound V_P is. Off, Saturated, as the SLO analyzer reads
	// it; the planner sets it (issue llm-d-incubation/llm-d-scaling-manager#280).
	LowerBoundKneeUnpriced bool
	// PriceRouterWait prices EPP flow control's gate past the prefill onset,
	// opt-in, off by default (DefaultPriceRouterWait). On, a TTFT at or above the
	// prefill gate's onset carries the gate as a stage in front of the
	// processor-sharing prefill (ttft_gate.go): the wait at the closed gate
	// (TTFTParts.RouterWait) and behind the burst it releases to one pod
	// (TTFTParts.BurstWait), flagged FlowControlWaitPriced in place of
	// AboveFlowControlOnset, where it takes: ProcessorSharing at a prompt
	// spread up to MaxPSPromptCV, at a rate up to MaxRouterWaitKneeShare of the
	// prefill knee. Elsewhere, and off, the wait keeps AboveFlowControlOnset
	// and no figure moves. One setting for the CLI (--price-router-wait) and
	// the SLO analyzer (slo.SwitchTTFTRouterWait), with the same default, so
	// the two answer the same.
	//
	// The gate adds time; it does not only move it from the pods to the
	// router. It reads a score a pod's engine step stale, so it releases a
	// step's arrivals at once, and the router's scorers, reading the same
	// stale poll, send them all to one pod while the others run short. At the
	// Qwen3-32B staircase's step past the onset that the fleet held, the
	// stage reads 1.8, 1.5 and 0.97 of the client's mean, p90 and p95
	// (TestStaircaseQ32_SwitchOn_TheGateStageHoldsTheClient); with the router's wait
	// taken as inside the prefill stage, as this switch did before, it read
	// 0.80, 0.84 and 0.57. At the second staircase's C-onset at 56.6 r/s,
	// past the share, it reads about 20 times short
	// (TestStaircaseR2_TheStepWithARouterWaitReadsShort).
	PriceRouterWait bool
	// RouterWaitInPrefill takes the router's wait past the prefill onset as
	// already inside the processor-sharing prefill price: the figure does not
	// move, and the answer is flagged FlowControlWaitPriced in place of
	// AboveFlowControlOnset where it takes (as PriceRouterWait: processor
	// sharing at a prompt spread up to MaxPSPromptCV, a rate up to
	// MaxRouterWaitKneeShare of the prefill knee), so a rate answer may stand
	// past the onset. The CLI and the planner API turn it on by default
	// (DefaultRouterWaitInPrefill); it is measured on llm-d-router v1.0.0
	// only. The struct's zero value is off, and so is the SLO analyzer's
	// (slo.SwitchTTFTRouterWaitInPrefill), which a replay turns on.
	//
	// On v1.0.0 the gate moves the wait from the pods to the router rather
	// than adding to it: on the Qwen3-32B flow-control A/B, at six bursty
	// steps past the prefill onset that the fleet held, the router held each
	// request 0.5 to 2.0 s on average, and at the one step also run with flow
	// control off at the same arrivals the client's mean and p95 did not grow
	// (its p90 rose 21%). The price reads at least the client there
	// (TestRouterV1_TheWaitIsInsidePrefill), though by a margin that would
	// also cover a wait that adds. On v0.11.0 the gate
	// adds time and this reads short; PriceRouterWait prices that router's
	// gate. With both set, PriceRouterWait takes.
	RouterWaitInPrefill bool
	// PSOnsets places the prefill gate's onset on the processor-sharing
	// model the price runs on, with ProcessorSharing and flow control on: a
	// pod's waiting count is the prompts on it past its running cap
	// (Prefill.RunningCap), on the stage's own law of the number on the pod,
	// at each pod's share of the arrivals (prefillPlan.psMeanScore). Off, as
	// before, it is a first-come queue's waiting count, which under processor
	// sharing the engine never holds. Past MaxPSPromptCV, and with the
	// running cap not known, the onset is the lower of the two. Off by
	// default (DefaultPSOnsets).
	PSOnsets bool
	// ChargeDecodeGate makes question 1 conservative at the decode gate, with
	// flow control on and a decode line. The decode onset is where the mean
	// score reaches 1; the gate reads the current score, so it closes for a
	// share of the time below the onset (ttftPlan.gateClosed). On, a
	// percentile target is priced with that share charged to its tail: a
	// request that meets a closed gate is taken to miss. A mean target is
	// capped where the share reaches MaxDecodeGateClosedShare. Either way the
	// bound is BoundDecodeGateCharge. TTFTAt and TTFTMeets do not move. With
	// no decode line it does nothing (DecodeGateChargeFor). Off by default
	// (DefaultChargeDecodeGate).
	ChargeDecodeGate bool
	// DecodeTTFT is the decode engine's measured TTFT, read only when Decode
	// is nil. On P/D the router's TTFT is the prefill engine's and then the
	// decode engine's (the KV pull and decode's first step): P2 found the
	// router's median less the two at -7 to +15 ms on 6 of 7 levels. Zero
	// Seconds is unmeasured: the decode term is left out, flagged
	// DecodeTTFTUnmeasured, which no caller acts on.
	DecodeTTFT  DecodeTTFT
	FlowControl FlowControl
}

// DecodeTTFT is the decode engine's mean TTFT, measured: on P/D, the time from
// the request reaching the decode pod to its first token, the KV pull
// included. S1 learns it per decode variant from the R1 field
// domain.ReplicaMetrics.AvgTTFT on the variant's Ready decode rows
// (docs/proposals/slo-tiers.md, decision 3). It is held at the prompt
// scaling below and at the rate it was measured at: RateForTTFT extrapolates
// to other rates with it frozen. Seconds and PromptTokens are one pair, from
// the same cycle.
type DecodeTTFT struct {
	// Seconds is the mean, in seconds; 0 is unmeasured.
	Seconds float64
	// PromptTokens is the mean effective prompt it was measured at. The KV
	// pull grows with the prompt and there is a fixed part besides, so the
	// term is priced at Seconds * max(1, I/PromptTokens): scaled up for a
	// longer prompt (the fixed part makes that high), held for a shorter one
	// (the pull makes that high). Required, at least MinDecodeTTFTPrompt,
	// when Seconds is above 0. It is
	// E[I_eff], the computed prompt, while the KV pull moves the whole
	// prompt, prefix hits included: with prefix hits the ratio I/PromptTokens
	// tracks the pull only as far as the hit rate holds.
	PromptTokens float64
	// Waiting is the decode backlog of the same cycle: the requests a decode
	// pod held waiting on its own capacity, 0 for none. Above 0 the figure
	// is a queue, and held at every rate it would be the floor at zero load,
	// so the TTFT is refused (DecodeTTFTUnderLoad). S1 does not read it.
	Waiting int
}

// MinDecodeTTFTPrompt is the least prompt, in tokens, a measured decode-engine
// TTFT may be read at: one token. A smaller one is not a measured mean prompt,
// and dividing by it overflows the term.
const MinDecodeTTFTPrompt = 1.0

// at is the decode term for a prompt of i tokens: 0 when unmeasured or with no
// prompt to scale by (plan refuses a measured figure without one).
func (d DecodeTTFT) at(i float64) float64 {
	if d.Seconds == 0 || !(d.PromptTokens > 0) {
		return 0
	}
	return d.Seconds * max(1, i/d.PromptTokens)
}

// At is the decode term for a prompt of i tokens, as the TTFT prices it:
// 0 when unmeasured.
func (d DecodeTTFT) At(i float64) float64 { return d.at(i) }

// TTFTTarget is a TTFT service level, end to end.
type TTFTTarget struct {
	// Seconds is the target.
	Seconds float64
	// Percentile is q in (0, 1), or 0 for the mean.
	Percentile float64
}

// prefillPlan is a checked Prefill with its derived figures.
type prefillPlan struct {
	p Prefill
	// f is the imbalance used (1 for one pod with none given).
	f float64
	// cv is the prompt's coefficient of variation as priced (0 below
	// minPromptCV); ca2, cs2 are Ca^2 and Cs^2 = cv^2.
	cv, ca2, cs2 float64
	// vp is V_P at the question's prompt (shapedVP), before the margin;
	// xfer the transfer per token priced in the own latency, 0 where the
	// decode engine's measured TTFT holds it.
	vp, xfer float64
	// margin is VPMargin, and applied the share of it taken at the rate
	// this plan was made for (atRate); 0 on the plan check returns.
	margin, applied float64
	// work is E[S_work] = E[I]/V_P, the queue's work per request; ownRate
	// is the own latency per prompt token, 1/V_P + kappa/BW, each at V_P
	// less the margin applied.
	work, ownRate float64
	// pull is the part of ownRate, seconds per prompt token, that is the KV
	// pull priced as a processor-sharing stage on the decode pods: kappa/BW
	// with ProcessorSharing and a decode line, else 0.
	pull  float64
	flags []Flag
}

// withTransfer is the plan with its own latency at a transfer of x per token.
func (pl prefillPlan) withTransfer(x float64) prefillPlan {
	pl.ownRate += x - pl.xfer
	pl.xfer = x
	return pl
}

// setVP sets the work and the own latency per token at V_P = vp.
func (pl *prefillPlan) setVP(vp float64) {
	pl.work = pl.p.PromptTokens / vp
	pl.ownRate = 1/vp + pl.xfer
}

// marginAt is the share of V_P taken off at utilization rho0, read at the
// unmargined V_P: 0 up to MarginRampStart, the whole margin from
// MarginRampFull, linear between.
func (pl prefillPlan) marginAt(rho0 float64) float64 {
	return pl.margin * min(max((rho0-MarginRampStart)/(MarginRampFull-MarginRampStart), 0), 1)
}

// atRate is the plan at rate, with the margin on V_P that utilization asks
// for. The worst pod's utilization at the margined V_P,
// rho0/(1 - marginAt(rho0)), rises with rho0, so TTFT still rises with the
// rate; it reaches 1 at rho0 = 1 - margin, the margined knee.
func (pl prefillPlan) atRate(rate float64) prefillPlan {
	q := pl
	q.f = pl.fAt(rate)
	m := q.marginAt(q.utilization(rate))
	if m == 0 {
		return q
	}
	q.applied = m
	q.setVP(pl.vp * (1 - m))
	return q
}

// shapedVP is V_P at the question's mean prompt E[I], read at I_vp
// (VPPromptTokens), and the flag it raises.
//
// A busy pod's step of B tokens of prompts of length I costs about
// s0 + (b + c*I)*B (prefill.Estimate.SlopeBound), so
//
//	V_P(I)/V_P(I_vp) = (s0/B + b + c*I_vp) / (s0/B + b + c*I).
//
// With a curve, s0/B unknown, the ratio is taken at s0/B = 0,
// s(I_vp)/s(I) with s(I) = b + c*I, and V_P is rescaled down by it for I
// above I_vp, flagged VPScaledToPrompt. That reads at or below the true V_P
// only if P1's low-load b and c are the busy step's per-token cost, which
// is not measured: on run 2 at 37k it read 1.5% above what the pod served
// (TestVPRescaleOnRun2At37k), and the margin covers that only from rho 0.6
// (MarginRampFull). So VPScaledToPrompt is on S2's no-act list. For I below
// I_vp the ratio would over-read the gain, so V_P is held at the learned
// figure. Without a curve, or with I_vp unknown,
// V_P is held, and a mean prompt more than MaxVPShapeRatio from I_vp either
// way, or an unknown I_vp, is flagged VPShapeMoved. An assumed V_P is the
// caller's for the prompt asked, and is neither rescaled nor flagged.
//
// c*I stands for the attention over a fresh prompt's mean context, I/2. A
// prefix hit of h tokens is not computed, but each of the I_eff = I - h
// tokens that are attends over the h cached ones too: mean context
// h + I_eff/2, so the curve's argument is I_eff + 2h (attentionSpan). With
// PrefixAttention on, both whole prompts known (the hit rates are read with
// them) and either hit rate above MaterialPrefixHitMove, the rescale reads
// that argument at I_vp and at the question, h from each side's own hit rate.
// With CachedContextRead (the planner reads it with PrefixAttention), h is each side's cached
// context as read, the whole prompt less the computed one, and the span is
// priced where either side's h is above MaterialPrefixHitMove of its whole
// prompt: the prefix-cache hit rate counts the GPU's hits alone, and reads a
// hit from a host, disk or peer tier as a miss though the prompt attends
// over it. Otherwise I_eff stands for it, as if nothing were cached, and reads V_P
// high at a hit (issue llm-d-incubation/llm-d-scaling-manager#311).
//
// With VPRequestSeconds r measured (requestSplit), V_P is not one token
// rate. A request's work at I_vp is I_vp/V_P, of which r is paid per request
// and the rest, k = (I_vp/V_P - r)/I_vp a token, per token, so at E[I] it is
// r + E[I]*k*g: the mean work of the prompt mix, since the work is linear in
// the prompt at a given cost a token. g is how far the cost a token grows
// from I_vp to E[I]: with the curve, as the token rate is rescaled above;
// with none, 1 at or below I_vp, and above it the growth of the attention
// span, I_eff + 2h with h the cached context (spanGrowth), as if every
// per-token cost were attention, which is the most it can grow. Above I_vp
// the work is never priced above the one-rate figure, E[I]/V_P with V_P
// held, the price before the split: there V_P is held and flagged as
// before. Otherwise V_P at E[I] is E[I] over that work, flagged
// VPScaledToPrompt where it would have been flagged VPShapeMoved: it is
// priced at the prompt, not held.
//
// Held as one token rate, a prompt longer than I_vp paid r once per I_vp
// tokens, and one shorter paid it less than once a request. Where r is two
// thirds of the work at I_vp and the context is mostly cached (g near 1),
// the one-rate knee at twice I_vp is two thirds of the split's, and at half
// I_vp it is 5/3 of it, over-promising. Above I_vp the span bounds the
// raise: a line through the prompts of a warm-up read the per-token cost of
// its longest prompts low, and the split alone priced a knee above the rate
// the pods saturated at (issue llm-d-incubation/llm-d-scaling-manager#368).
// On fresh prompts the span doubles with the prompt, so at twice I_vp the
// bound reaches the one-rate figure, and the knee holds, wherever r is at
// most two thirds of the work at I_vp. Where r is more, even the span's
// growth leaves the work under the one-rate figure, and the knee rises.
func (p Prefill) shapedVP() (float64, Flag) {
	vp, flag := p.tokenRateVP()
	r, ok := p.requestSplit()
	if !ok {
		return vp, flag
	}
	i, ivp := p.PromptTokens, p.VPPromptTokens
	g := p.VP / vp
	if !(p.CurveSlope > 0 && p.CurveCurvature > 0) && i > ivp {
		g = p.spanGrowth()
	}
	work := r + i*(ivp/p.VP-r)/ivp*g
	if i > ivp && !(work < i/vp) {
		return vp, flag
	}
	if flag == VPShapeMoved {
		flag = VPScaledToPrompt
	}
	return i / work, flag
}

// spanGrowth is the attention span at the question's prompt over the one at
// I_vp, I_eff + 2h each, at least 1 (shapedVP). h is the cached context, the
// whole prompt less the computed one, every tier's hits counted: as read
// (CachedContextRead), else from the whole prompts where both are known,
// else none on either side. Not the prefix-cache hit rates, which count the
// GPU's hits alone and read a hit from another tier as computed: the span
// they give is short, and its growth read high, on one side and not the
// other (issue llm-d-incubation/llm-d-scaling-manager#383).
func (p Prefill) spanGrowth() float64 {
	i, ivp := p.PromptTokens, p.VPPromptTokens
	switch {
	case p.CachedContextRead:
		i, ivp = i+2*p.CachedContextTokens, ivp+2*p.VPCachedContextTokens
	case p.WholePromptTokens > 0 && p.VPWholePromptTokens > 0:
		i, ivp = i+2*max(p.WholePromptTokens-i, 0), ivp+2*max(p.VPWholePromptTokens-ivp, 0)
	}
	return max(1, i/ivp)
}

// requestSplit is VPRequestSeconds where shapedVP prices V_P along it: a
// learned V_P read at a known I_vp, and r above 0 and under I_vp/V_P, the
// whole of a request's work there. Otherwise false, and V_P is one token
// rate.
func (p Prefill) requestSplit() (float64, bool) {
	r := p.VPRequestSeconds
	if p.VPAssumed || !(r > 0) || !(p.VPPromptTokens > 0) || !(r < p.VPPromptTokens/p.VP) {
		return 0, false
	}
	return r, true
}

// tokenRateVP is V_P at the question's mean prompt as one token rate, and
// the flag it raises (shapedVP).
func (p Prefill) tokenRateVP() (float64, Flag) {
	if p.VPAssumed {
		return p.VP, ""
	}
	i, ivp := p.PromptTokens, p.VPPromptTokens
	if p.CurveSlope > 0 && p.CurveCurvature > 0 && ivp > 0 {
		switch {
		case !p.PrefixAttention:
		case p.CachedContextRead:
			if h, hvp := p.CachedContextTokens, p.VPCachedContextTokens; max(cachedShare(h, i), cachedShare(hvp, ivp)) > MaterialPrefixHitMove {
				i, ivp = i+2*h, ivp+2*hvp
			}
		case p.VPWholePromptTokens > 0 && p.WholePromptTokens > 0 &&
			max(p.PromptHitRate, p.VPPromptHitRate) > MaterialPrefixHitMove:
			i, ivp = attentionSpan(i, p.PromptHitRate), attentionSpan(ivp, p.VPPromptHitRate)
		}
		if i > ivp {
			s := func(x float64) float64 { return p.CurveSlope + p.CurveCurvature*x }
			return p.VP * s(ivp) / s(i), VPScaledToPrompt
		}
		return p.VP, ""
	}
	if !(ivp > 0) || i > MaxVPShapeRatio*ivp || i*MaxVPShapeRatio < ivp {
		return p.VP, VPShapeMoved
	}
	return p.VP, ""
}

// attentionSpan is the curve's argument for a computed prompt of eff tokens
// at a prefix-cache hit rate hit in [0, 1): eff + 2h, h = eff*hit/(1 - hit)
// the cached tokens (shapedVP). h is taken from the measured hit rate, not
// as the whole prompt less eff: the two prompts are read over different
// windows, and their drift with no cache would read as cached context.
func attentionSpan(eff, hit float64) float64 {
	return eff + 2*eff*hit/(1-hit)
}

// MaterialPrefixHitMove is how far a prefix-cache hit rate must move, or
// stand above 0, for prefixMoved to read the prefix hits as moved, and how
// high one must stand for shapedVP to price the cached tokens' attention
// (PrefixAttention): below it a hit rate is taken as noise around none.
// Stated, not measured: no committed recording has prefix hits.
const MaterialPrefixHitMove = 0.1

// prefixMoved reports whether the prefix hits V_P was read at moved from the
// question's, which the computed prompt alone does not show: the hit rate
// by more than MaterialPrefixHitMove, or, with either hit rate above it, the
// whole prompt by more than MaxVPShapeRatio either way. At 32k with a 75% hit
// a pod computes the 8k a fresh 8k prompt does, so I_vp and E[I_eff] agree,
// but each token attends over 24k cached ones (issue
// llm-d-incubation/llm-d-scaling-manager#311). False with either whole
// prompt unknown, as before, and for an assumed V_P. With no hit on either
// side it is false: the whole prompt then moves with E[I_eff], which
// shapedVP reads. With CachedContextRead, each side's hit is its cached
// share of the whole prompt, every tier's hits counted, as shapedVP prices
// the span: like with like, not the prefix-cache hit rates, which count the
// GPU's hits alone (issue llm-d-incubation/llm-d-scaling-manager#383).
func (p Prefill) prefixMoved() bool {
	if p.VPAssumed || !(p.VPWholePromptTokens > 0) || !(p.WholePromptTokens > 0) {
		return false
	}
	hit, hitVP := p.PromptHitRate, p.VPPromptHitRate
	if p.CachedContextRead {
		hit, hitVP = cachedShare(p.CachedContextTokens, p.PromptTokens), cachedShare(p.VPCachedContextTokens, p.VPPromptTokens)
	}
	if math.Abs(hit-hitVP) > MaterialPrefixHitMove {
		return true
	}
	if max(hit, hitVP) <= MaterialPrefixHitMove {
		return false
	}
	w, wvp := p.WholePromptTokens, p.VPWholePromptTokens
	return w > MaxVPShapeRatio*wvp || w*MaxVPShapeRatio < wvp
}

// cachedShare is h cached tokens' share of a whole prompt of h + eff
// computed ones; 0 where the prompt is empty.
func cachedShare(h, eff float64) float64 {
	if !(h+eff > 0) {
		return 0
	}
	return h / (h + eff)
}

// promptNotCovered reports whether the question's mean prompt lies outside
// the prompts V_P was read over (VPPromptLow, VPPromptHigh) by more than
// MaxVPShapeRatio either way. A V_P is the speed of the steps of the minutes
// it was read in: on longer prompts each token costs more attention, and on
// shorter ones each request's fixed step cost is shared by fewer tokens, so
// it is not the capacity at a prompt those minutes never ran, whatever its
// source says it bounds (issue llm-d-incubation/llm-d-scaling-manager#405).
// False with the range unknown and for an assumed V_P.
func (p Prefill) promptNotCovered() bool {
	if p.VPAssumed || !(p.VPPromptLow > 0) || !(p.VPPromptHigh > 0) {
		return false
	}
	return p.PromptTokens > MaxVPShapeRatio*p.VPPromptHigh || p.PromptTokens*MaxVPShapeRatio < p.VPPromptLow
}

// check refuses a Prefill the model cannot price, naming the first input
// that fails, in the order an operator fixes them: the question, then the
// capacity, the fixed latency, the imbalance and the shape.
func (p Prefill) check() (prefillPlan, *Unanswerable) {
	var u Unanswerable
	switch {
	case p.Replicas < 1:
		u = refuse(NoPrefillReplicas, fmt.Sprintf("prefill replicas %d", p.Replicas))
	case p.Replicas > maxPrefillReplicas:
		u = refuse(InvalidInput, fmt.Sprintf("%d prefill replicas, above the %d priced", p.Replicas, maxPrefillReplicas))
	case !Finite(p.VP) || !(p.VP > 0):
		u = refuse(NoPrefillCapacity, fmt.Sprintf("V_P %g tokens/s", p.VP))
	case !Finite(p.FixedLatency) || !(p.FixedLatency > 0):
		u = refuse(NoFixedLatency, fmt.Sprintf("a %g s", p.FixedLatency))
	case p.Imbalance == 0 && p.Replicas > 1:
		u = refuse(NoImbalance, fmt.Sprintf("no f for %d prefill pods, learned or assumed", p.Replicas))
	case p.Imbalance != 0 && !(p.Imbalance >= 1 && p.Imbalance <= float64(p.Replicas)):
		u = refuse(InvalidInput, fmt.Sprintf("f %g, want 1 <= f <= %d (the pods)", p.Imbalance, p.Replicas))
	case (p.ImbalanceCurveFloor != 0 || p.ImbalanceCurveExcess != 0) &&
		!(Finite(p.ImbalanceCurveFloor) && p.ImbalanceCurveFloor > 0 && Finite(p.ImbalanceCurveExcess) && p.ImbalanceCurveExcess > 0):
		u = refuse(InvalidInput, fmt.Sprintf("f against the load %g + %g/u, want both above 0", p.ImbalanceCurveFloor, p.ImbalanceCurveExcess))
	case !Finite(p.PromptTokens) || !(p.PromptTokens > 0):
		u = refuse(NoTraffic, fmt.Sprintf("prompt %g tokens", p.PromptTokens))
	case !(p.PromptCV >= 0 && p.PromptCV <= maxPromptCV):
		// The spread is assumed or read from a histogram; a value outside
		// [0, 10] is the caller's mistake, not the traffic's.
		u = refuse(InvalidInput, fmt.Sprintf("prompt cv %g, want 0 <= cv <= %d", p.PromptCV, maxPromptCV))
	case p.PromptMaxRatio != 0 && !(Finite(p.PromptMaxRatio) && p.PromptMaxRatio >= 1):
		// No prompt is longer than the bound, so the mean is not either.
		u = refuse(InvalidInput, fmt.Sprintf("longest prompt %g times the mean, want 0 (none) or at least 1", p.PromptMaxRatio))
	case !Finite(p.ArrivalCV2) || p.ArrivalCV2 < 0:
		u = refuse(InvalidInput, fmt.Sprintf("Ca^2 %g, want a finite value of at least 0 (0 is Poisson)", p.ArrivalCV2))
	case !Finite(p.TransferSecondsPerToken) || p.TransferSecondsPerToken < 0:
		u = refuse(InvalidInput, fmt.Sprintf("transfer %g s per token", p.TransferSecondsPerToken))
	case !Finite(p.VPPromptTokens) || p.VPPromptTokens < 0:
		u = refuse(InvalidInput, fmt.Sprintf("V_P read at %g prompt tokens", p.VPPromptTokens))
	case !Finite(p.VPRequestSeconds) || p.VPRequestSeconds < 0:
		u = refuse(InvalidInput, fmt.Sprintf("V_P's per-request work %g s, want a finite value of at least 0 (0 is not measured)", p.VPRequestSeconds))
	case !Finite(p.VPWholePromptTokens) || p.VPWholePromptTokens < 0 || !Finite(p.WholePromptTokens) || p.WholePromptTokens < 0:
		u = refuse(InvalidInput, fmt.Sprintf("whole prompt %g, V_P's %g, want finite values of at least 0 (0 is unknown)",
			p.WholePromptTokens, p.VPWholePromptTokens))
	case p.WholePromptTokens > 0 && !(p.PromptHitRate >= 0 && p.PromptHitRate < 1) ||
		p.VPWholePromptTokens > 0 && !(p.VPPromptHitRate >= 0 && p.VPPromptHitRate < 1):
		// A hit rate is read only beside a known whole prompt; with the whole
		// prompt unknown, nothing reads it (shapedVP, prefixMoved).
		u = refuse(InvalidInput, fmt.Sprintf("prefix hit rate %g, V_P's %g, want 0 <= hit < 1", p.PromptHitRate, p.VPPromptHitRate))
	case !Finite(p.CachedContextTokens) || p.CachedContextTokens < 0 || !Finite(p.VPCachedContextTokens) || p.VPCachedContextTokens < 0:
		u = refuse(InvalidInput, fmt.Sprintf("cached context %g tokens, V_P's %g, want finite values of at least 0",
			p.CachedContextTokens, p.VPCachedContextTokens))
	case !Finite(p.TierLoadSeconds) || p.TierLoadSeconds < 0:
		u = refuse(InvalidInput, fmt.Sprintf("tier load %g s, want a finite value of at least 0", p.TierLoadSeconds))
	case !Finite(p.VPPromptLow) || p.VPPromptLow < 0 || !Finite(p.VPPromptHigh) || p.VPPromptHigh < p.VPPromptLow:
		u = refuse(InvalidInput, fmt.Sprintf("V_P read over prompts %g to %g, want finite values, 0 <= low <= high (0 is unknown)",
			p.VPPromptLow, p.VPPromptHigh))
	case !Finite(p.CurveSlope) || p.CurveSlope < 0 || !Finite(p.CurveCurvature) || p.CurveCurvature < 0:
		u = refuse(InvalidInput, fmt.Sprintf("curve b %g, c %g, want finite values of at least 0", p.CurveSlope, p.CurveCurvature))
	case !(p.VPMargin >= 0 && p.VPMargin <= MaxVPMargin):
		u = refuse(InvalidInput, fmt.Sprintf("V_P margin %g, want 0 <= margin <= %g", p.VPMargin, MaxVPMargin))
	case p.RunningCap < 0 || p.RunningCap > MaxRunningCap:
		u = refuse(InvalidInput, fmt.Sprintf("running cap %d, want 0 <= cap <= %d (0 is not known)", p.RunningCap, MaxRunningCap))
	default:
		cv := p.PromptCV
		if cv < minPromptCV {
			cv = 0
		}
		pl := prefillPlan{p: p, f: max(p.Imbalance, 1), ca2: p.ArrivalCV2, cv: cv, cs2: cv * cv,
			xfer: p.TransferSecondsPerToken, margin: p.VPMargin}
		if pl.ca2 == 0 {
			pl.ca2 = 1
		}
		vp, shape := p.shapedVP()
		pl.vp = vp
		pl.setVP(vp)
		pl.flags = []Flag{UnvalidatedTTFT}
		for _, c := range []struct {
			on bool
			f  Flag
		}{
			{p.VPAssumed, AssumedVP}, {p.VPLowerBound, VPLowerBound}, {p.VPAboveSlopeBound, VPAboveSlopeBound},
			{p.VPFromPlateau, VPFromPlateau}, {p.ExperimentalTTFT, ExperimentalTTFT}, {shape != "", shape}, {shape != VPShapeMoved && p.prefixMoved(), VPShapeMoved},
			{p.promptNotCovered(), VPPromptNotCovered},
			{p.ImbalanceAssumed, AssumedImbalance}, {p.PromptCVAssumed, AssumedPromptSpread},
			{p.TransferSecondsPerToken == 0, TransferOmitted},
		} {
			if c.on {
				pl.flags = append(pl.flags, c.f)
			}
		}
		return pl, nil
	}
	return prefillPlan{}, &u
}

// knee is the prefill knee, N_p * V_P * (1 - VPMargin) / (f * E[I_eff]), V_P
// at the question's prompt: the rate at which the worst pod saturates. The
// margin is whole there (MaxVPMargin).
//
// With LoadAwareRouter f falls with the load (fAt), and the knee is where
// f(rho) * rho reaches 1 - VPMargin, rho the mean pod utilization at the
// unmargined V_P: past rho_m that is k rho^2 - (1 + k) rho + (1 - VPMargin)
// = 0, k = (f - 1)/(1 - rho_m), at its smaller root. With no margin that
// root is 1, the pooled knee, unless f - 1 > 1 - rho_m: a pod that hot
// saturates before the router has the idle pods to steer its excess to.
//
// With f against the load (ImbalanceCurveFloor) it is the lower of that
// knee and the curve's (curveKnee).
func (pl prefillPlan) knee() float64 {
	k := pl.flatKnee()
	if pl.loadCurve() {
		k = min(k, pl.curveKnee())
	}
	return k
}

// flatKnee is knee without the load curve (ImbalanceCurveFloor).
func (pl prefillPlan) flatKnee() float64 {
	f := pl.baseF()
	pooled := float64(pl.p.Replicas) * pl.vp / pl.p.PromptTokens
	held := (1 - pl.margin) / f
	rm := pl.rhoM()
	if !pl.loadAware() || held <= rm {
		// The held knee, in the order of operations it has always had, so a
		// fleet the switch does not touch prices to the bit as before.
		return float64(pl.p.Replicas) * pl.vp * (1 - pl.margin) / (f * pl.p.PromptTokens)
	}
	k := (f - 1) / (1 - rm)
	disc := (1-k)*(1-k) + 4*k*pl.margin
	// The smaller root in the form that keeps its digits as k goes to 0.
	return pooled * 2 * (1 - pl.margin) / ((1 + k) + math.Sqrt(disc))
}

// loadCurve is whether f against the load is priced (ImbalanceCurveFloor):
// set, on more than one pod.
func (pl prefillPlan) loadCurve() bool {
	return pl.p.Replicas > 1 && pl.p.ImbalanceCurveFloor > 0 && pl.p.ImbalanceCurveExcess > 0
}

// curveKnee is the rate at which the curve's f(u) * rho reaches
// 1 - VPMargin, rho = u/V_P the mean pod utilization at the unmargined V_P.
// f(u) * rho is (floor * u + excess)/V_P, held to N_p * rho, so the root is
// rho = (1 - VPMargin - excess/V_P)/floor, and at least (1 - VPMargin)/N_p
// where the cap holds there. fAt is the higher of the flat f and the curve's,
// each f(rho) * rho rising with the rate, so the knee is the lower of the
// two roots.
func (pl prefillPlan) curveKnee() float64 {
	n := float64(pl.p.Replicas)
	rho := max((1-pl.margin-pl.p.ImbalanceCurveExcess/pl.vp)/pl.p.ImbalanceCurveFloor, (1-pl.margin)/n)
	return n * pl.vp * rho / pl.p.PromptTokens
}

// baseF is the f the plan was given: Prefill.Imbalance, at least 1.
func (pl prefillPlan) baseF() float64 { return max(pl.p.Imbalance, 1) }

// loadAware is whether f falls with the load: LoadAwareRouter, on more than
// one pod, with an f above 1 and the utilization it was read at known. With
// rho_m unknown f is held: a fall from no load would lift the knee on no
// reading at all.
func (pl prefillPlan) loadAware() bool {
	return pl.p.LoadAwareRouter && pl.p.Replicas > 1 && pl.baseF() > 1 && pl.p.ImbalanceUtilization > 0
}

// rhoM is ImbalanceUtilization at the V_P the plan prices, the question's
// prompt (shapedVP): the load is the same, and where the prompt's V_P is
// lower the same load fills more of it. Held to [0, MaxImbalanceUtilization].
func (pl prefillPlan) rhoM() float64 {
	rm := pl.p.ImbalanceUtilization
	if pl.vp > 0 && pl.p.VP > 0 {
		rm *= pl.p.VP / pl.vp
	}
	return min(max(rm, 0), MaxImbalanceUtilization)
}

// MaxImbalanceUtilization is the most of rho_m that fAt reads: rho_m is
// clamped to it, so the fall past it is never steeper than (f - 1)/0.05.
const MaxImbalanceUtilization = 0.95

// PrefillLoadAt is f at rate (fAt) and the pooled prefill utilization there,
// rate * E[I] / (N_p * V_P * (1 - VPMargin)), V_P at the question's prompt;
// false where p cannot be priced. The busiest pod's utilization is their
// product.
func PrefillLoadAt(p Prefill, rate float64) (f, pooled float64, ok bool) {
	pl, u := p.check()
	if u != nil {
		return 0, 0, false
	}
	return pl.fAt(rate), rate * p.PromptTokens / (float64(p.Replicas) * pl.vp * (1 - pl.margin)), true
}

// fAt is f at rate: flatFAt, or with a load curve (ImbalanceCurveFloor) the
// higher of it and the curve's f at the rate's load.
//
// flatFAt off LoadAwareRouter is the plan's f at every rate.
// On, it is f up to rho_m (ImbalanceUtilization), the mean pod utilization
// it was read at, and past it the excess falls in proportion to the pods'
// idle capacity:
//
//	f(rho) = 1 + (f - 1)(1 - rho)/(1 - rho_m),
//
// rho the mean pod utilization at the unmargined V_P, so 1 at rho = 1. A
// router that scores each pod's queue (llm-d's EPP queue scorer) sends less
// to a pod as its queue grows: what spreads the shares at light load (a
// prefix the router keeps on one pod, a slower node) loses to the queue as
// the pods fill, and near the pooled knee the busiest pod's share is the
// mean share (join-the-shortest-queue). Measured: on the public staircase
// the busiest pod's raw share fell from about 1.5 at 10 requests/s to 1.01
// to 1.06 near capacity (busiestMinutes, issue #266). The linear fall is a
// stated shape, not a fit: f is held, never raised, below rho_m, where the
// minutes it was read from say it held, and the shares near the knee are
// not read until a run sits there.
func (pl prefillPlan) fAt(rate float64) float64 {
	f := pl.flatFAt(rate)
	if !pl.loadCurve() {
		return f
	}
	// f against the load (ImbalanceCurveFloor): the higher of the two, at
	// most the pods, where the whole load is on one.
	n := float64(pl.p.Replicas)
	u := rate * pl.p.PromptTokens / n
	c := n
	if u > 0 {
		c = min(n, pl.p.ImbalanceCurveFloor+pl.p.ImbalanceCurveExcess/u)
	}
	return max(f, c)
}

// flatFAt is fAt without the load curve (ImbalanceCurveFloor).
func (pl prefillPlan) flatFAt(rate float64) float64 {
	f := pl.baseF()
	if !pl.loadAware() {
		return f
	}
	rho := rate * pl.p.PromptTokens / (float64(pl.p.Replicas) * pl.vp)
	rm := pl.rhoM()
	if !(rho > rm) {
		return f
	}
	return loadAwareFall(f, rho, rm)
}

// loadAwareFall is f past rho_m (fAt): 1 + (f - 1)(1 - rho)/(1 - rho_m), at
// least 1. It is a variable only so a test can put a non-monotone f(rho) in
// its place and show the TTFT sweeps catch it (ttft_load_aware_test.go):
// nothing else assigns it.
var loadAwareFall = func(f, rho, rm float64) float64 {
	return max(1, 1+(f-1)*(1-rho)/(1-rm))
}

// utilization is rho_w, the worst pod's: f * rate * E[I_eff] / (N_p * V_P),
// at the plan's V_P (unmargined on a checked plan, margined after atRate).
func (pl prefillPlan) utilization(rate float64) float64 {
	return pl.f * rate * pl.work / float64(pl.p.Replicas)
}

// overSlots says the busiest pod, processor sharing by its engine's
// configuration (QueueProcessorSharing), is likely to hold more prompts than it
// runs at once at rate: rho^(slots+1), the M/G/1-PS chance of more than
// slots prompts on the pod, above MaxPSOverSlots.
func (pl prefillPlan) overSlots(rate float64) bool {
	if pl.p.Queue.Model != QueueProcessorSharing || pl.p.Queue.Slots < 1 {
		return false
	}
	rho := pl.atRate(rate).utilization(rate)
	return !(rho < 1) || math.Pow(rho, float64(pl.p.Queue.Slots+1)) > MaxPSOverSlots
}

// waitScale is theta, E[S_work](Ca^2 + Cs^2) / (2(1 - rho)), Kingman's mean
// wait given a wait: the mean wait is rho * theta.
func (pl prefillPlan) waitScale(rho float64) float64 {
	return pl.work * (pl.ca2 + pl.cs2) / (2 * (1 - rho))
}

// own is the request's own latency less a: I * (1/V_P + kappa/BW), held at
// the longest prompt with a bound (Prefill.PromptMaxRatio).
func (pl prefillPlan) own() ownTime {
	mean := pl.p.PromptTokens * pl.ownRate
	return newOwnTime(mean, pl.cv).capped(pl.p.PromptMaxRatio * mean)
}

// BeyondProcessorSharing says processor sharing is not priced alone on p's
// prompt (SpreadBeyondProcessorSharing): a spread past MaxPSPromptCV that no
// support bound at MaxPSPromptSupport or under clears. The bound clears it
// only at a fleet Ca^2 no burstier than Poisson (ArrivalCV2 at most 1, 0
// being Poisson), not at each pod's split one: the steps processor sharing
// was measured to hold on were all at Poisson arrivals, and on the
// 8000-token phase run at Ca^2 4 the bound read p50 7% under the measured,
// where the larger of the two held it (issue
// llm-d-incubation/llm-d-scaling-manager#304). The cut at 1 is where that
// evidence brackets it (Ca^2 1 held, 4 did not), not a calibrated threshold.
// A step it does not clear is priced as with the switch off, the router's
// wait included: no gate stage and no processor-sharing onsets
// (ttftPlan.routerWait, psOnsets).
func (p Prefill) BeyondProcessorSharing() bool {
	if p.Queue.Model == QueueProcessorSharing {
		return false
	}
	if r := p.PromptMaxRatio; r > 0 && r <= MaxPSPromptSupport && p.ArrivalCV2 <= 1 {
		return false
	}
	return p.PromptCV > MaxPSPromptCV
}

// engineWait is the worst pod's wait tail at utilization rho, and its decay
// rate r. The rule errs only toward a longer tail, and so more replicas:
//
//   - Poisson arrivals (Ca^2 = 1), cv >= 1: rho * exp(-r t), r the smaller of
//     Kingman's 1/theta and the Cramer-Lundberg root gamma of
//     lambda_w (M_S(gamma) - 1) = gamma (M_S the work's moment generating
//     function), the exact asymptotic decay of an M/G/1 wait. At cv 1 the two
//     are equal (mu - lambda) and the tail is exact (M/M/1). Above 1 the root
//     is the smaller: 1/theta alone read a p99 up to 10% short of a Lindley
//     simulation.
//   - Poisson arrivals, cv < 1: min(rho, C exp(-gamma t)), Taylor's upper
//     bound on an M/G/1 wait: C = sup_x P(S > x) / E[exp(gamma (S - x)); S > x].
//     Gamma work at cv <= 1 has an increasing failure rate, so the ratio
//     rises with x and its sup is the limit, C = 1 - gamma/b with b the
//     gamma's rate k/E[S] (1 for constant work, which is the Lundberg
//     inequality). At cv 1, C = rho, so the bound meets the exact M/M/1 tail
//     and TTFT does not jump at the boundary. An exponential body behind the
//     atom under-states the wait of light-tailed work just past the atom:
//     against Erlang's exact M/D/1 wait it read a p90 sojourn 10% short at
//     rho 0.2.
//   - Any other Ca^2: the Poisson tail stretched in time by Kingman's ratio
//     s = (Ca^2 + Cs^2) / (1 + Cs^2), so its decay is r/s, the shape and its
//     atom as they were, and never above the work's own tail rate b =
//     1/(cv^2 E[S]): a wait cannot thin out faster than one request's work
//     does. So a burstier Ca^2 lengthens the wait at every t, Ca^2 = 1 is the
//     Poisson tail to the bit, and above 1 no decay beats Poisson's root. In
//     heavy traffic gamma tends to 2(1 - rho)/(E[S](1 + Cs^2)), so r/s tends
//     to Kingman's 1/theta. Below heavy traffic it is an interpolation, not a
//     theorem: not checked against a simulation of a bursty queue. Kingman's
//     1/theta alone is no bound on it: above cv 1 it decays faster than
//     Poisson's root, which priced burstier arrivals better than Poisson.
//
// A rate that is not finite (no Cramer-Lundberg root) comes back as such;
// price refuses it.
func (pl prefillPlan) engineWait(rho float64) (waitTail, float64) {
	g := lundbergRoot(rho/pl.work, pl.work, pl.cv)
	r := g
	if pl.cv >= 1 {
		// The Poisson theta: at Ca^2 = 1 this is waitScale.
		r = min(2*(1-rho)/(pl.work*(1+pl.cs2)), g)
	}
	// A quotient, so that Ca^2 = 1 is exactly 1 and the Poisson tail its own bits.
	stretched := r / ((pl.ca2 + pl.cs2) / (1 + pl.cs2))
	if pl.cv > 0 {
		stretched = min(stretched, 1/(pl.cs2*pl.work))
	}
	if pl.cv >= 1 {
		return expWait(rho, stretched), stretched
	}
	c := 1.0
	if pl.cv > 0 {
		c = 1 - g*pl.cs2*pl.work // 1 - g/b, b = 1/(cv^2 E[S])
	}
	return boundWait(rho, c, stretched), stretched
}

// queueLength is a pod's mean waiting count at utilization rho,
// rho^2 (Ca^2 + Cs^2) / (2(1 - rho)) (Little's law on the mean wait).
func (pl prefillPlan) queueLength(rho float64) float64 {
	return rho * rho * (pl.ca2 + pl.cs2) / (2 * (1 - rho))
}

// meanScore is the prefill stage's mean endpoint score at rate: the worst
// pod at f, the other N_p - 1 sharing the rest evenly, each scoring its mean
// waiting count over the threshold. Prefill KV usage is not in it: a prefill
// pod's queue binds long before its KV does (run PK held 730 waiting at 25%
// KV).
func (pl prefillPlan) meanScore(rate, threshold float64) float64 {
	pl = pl.atRate(rate)
	n := float64(pl.p.Replicas)
	rw := pl.utilization(rate)
	if rw >= 1 {
		return math.Inf(1)
	}
	sum := pl.queueLength(rw)
	if n > 1 {
		sum += (n - 1) * pl.queueLength(rate*pl.work*(n-pl.f)/(n*(n-1)))
	}
	return sum / n / threshold
}

// psMeanScore is meanScore on the processor-sharing stage the price runs on
// (psStages): each pod at its share of the arrivals (podShares) and its own
// Ca^2 (splitCA2), scoring the prompts on it past its running cap k
// (psWaiting) over the threshold.
func (pl prefillPlan) psMeanScore(rate, threshold float64, k int) float64 {
	pl = pl.atRate(rate)
	var sum float64
	for _, w := range podShares(pl.f, pl.p.Replicas) {
		rho := rate * w * pl.work
		if rho >= 1 {
			return math.Inf(1)
		}
		s, _ := psBurst(splitCA2(pl.ca2, w), pl.cs2)
		sum += psWaiting(rho, s, k)
	}
	return sum / float64(pl.p.Replicas) / threshold
}

// psWaiting is one processor-sharing pod's mean waiting count at utilization
// rho, with the stage's burst factor s (psBurst) and a running cap of k
// prompts: E[(N - k)+], N the prompts on the pod. N takes the law the stage
// prices: P(N >= 1) = rho, as for any work-conserving pod, and past 1
// geometric at a ratio sigma set so its mean is the stage's. Little's law on
// the stage's sojourn x(1 + s rho/(1 - rho)) gives E[N] = rho + s rho^2/(1 - rho),
// so sigma = s rho/(1 - rho + s rho), and
// E[(N - k)+] = rho sigma^k/(1 - sigma). At Poisson s is 1, sigma is rho, and
// N is the M/G/1-PS law exactly, whatever the work's spread. The law is the
// processor-sharing one even past the cap, where the engine serves the
// waiting prompts closer to first come; at a narrow spread that holds fewer
// on the pod, so the count errs high, toward a lower onset.
func psWaiting(rho, s float64, k int) float64 {
	if !(rho > 0) {
		return 0
	}
	if !(rho < 1) {
		return math.Inf(1)
	}
	sigma := s * rho / (1 - rho + s*rho)
	return rho * math.Pow(sigma, float64(k)) / (1 - sigma)
}

// ttftPlan is a checked fleet.
type ttftPlan struct {
	pf prefillPlan
	d  *Decode
	// decodeTTFT is the decode engine's measured TTFT, set only with no
	// decode line.
	decodeTTFT DecodeTTFT
	// term is decode's part with a decode line; nil with none, or with
	// LegacyDecodeSum.
	term   *termPlan
	fc     FlowControl
	onsets Onsets
	flags  []Flag
	// ps is TTFTFleet.ProcessorSharing.
	ps bool
	// psCapped is ps at a prompt spread past MaxPSPromptCV, with no support
	// bound clearing it (Prefill.BeyondProcessorSharing): each price is the larger of
	// processor sharing's and first come's.
	psCapped bool
	// routerWait is TTFTFleet.PriceRouterWait or RouterWaitInPrefill where it
	// takes: processor sharing alone, at a prompt spread up to MaxPSPromptCV.
	// gateStage is PriceRouterWait among them: the gate priced as a stage
	// (ttft_gate.go); without it the wait is inside the prefill price.
	routerWait, gateStage bool
	// psOnsets is TTFTFleet.PSOnsets where it takes: processor sharing.
	psOnsets bool
	// chargeGate is TTFTFleet.ChargeDecodeGate where it takes: flow control
	// on and a decode line.
	chargeGate bool
	// kneeUnpriced is TTFTFleet.LowerBoundKneeUnpriced on a V_P that is a
	// lower bound and not the caller's.
	kneeUnpriced bool
	// servedOnly is Prefill.VPServedOnly on a V_P not the caller's: no TTFT
	// is priced from it.
	servedOnly bool
	// notCovered is Prefill.promptNotCovered: no TTFT is priced from V_P.
	notCovered bool
}

// plan checks a fleet and places its flow-control onsets.
func (fl TTFTFleet) plan() (ttftPlan, *Unanswerable) {
	pf, u := fl.Prefill.check()
	if u != nil {
		return ttftPlan{}, u
	}
	if fl.Decode != nil {
		if u := fl.Decode.check(); u != nil {
			return ttftPlan{}, u
		}
	}
	pl := ttftPlan{pf: pf, d: fl.Decode, fc: fl.FlowControl, ps: fl.ProcessorSharing,
		psCapped:     fl.ProcessorSharing && fl.Prefill.BeyondProcessorSharing(),
		kneeUnpriced: fl.LowerBoundKneeUnpriced && fl.Prefill.VPLowerBound && !fl.Prefill.VPAssumed,
		servedOnly:   fl.Prefill.VPServedOnly && !fl.Prefill.VPAssumed,
		notCovered:   fl.Prefill.promptNotCovered()}
	pl.routerWait = (fl.PriceRouterWait || fl.RouterWaitInPrefill) && pl.ps && !pl.psCapped
	pl.gateStage = pl.routerWait && fl.PriceRouterWait
	pl.psOnsets = fl.PSOnsets && pl.ps
	if fl.ProcessorSharing && fl.Decode != nil {
		// The pull is its own stage on the decode pods. With no decode line
		// there are no pods to share, and it stays in the own time.
		pf.pull = pf.xfer
	}
	if fl.Decode != nil && !fl.LegacyDecodeSum {
		t := DecodeTerm{EngineOmitted: true}
		if fl.DecodeTerm != nil {
			t = *fl.DecodeTerm
		}
		if u := t.check(); u != nil {
			return ttftPlan{}, u
		}
		pl.term = termOn(t, &pf)
	}
	if fl.Decode == nil {
		dt := fl.DecodeTTFT
		switch {
		case !Finite(dt.Seconds) || dt.Seconds < 0:
			u := refuse(InvalidInput, fmt.Sprintf("decode engine TTFT %g s", dt.Seconds))
			return ttftPlan{}, &u
		case dt.Seconds == 0:
			pf.flags = append(pf.flags, DecodeTTFTUnmeasured, DecodeSideOmitted)
		case !Finite(dt.PromptTokens) || !(dt.PromptTokens >= MinDecodeTTFTPrompt):
			u := refuse(InvalidInput, fmt.Sprintf("decode engine TTFT %g s measured at %g prompt tokens: a measured TTFT (Seconds > 0) requires a prompt of at least %g tokens",
				dt.Seconds, dt.PromptTokens, MinDecodeTTFTPrompt))
			return ttftPlan{}, &u
		case !Finite(dt.at(pf.p.PromptTokens)):
			u := refuse(InvalidInput, fmt.Sprintf("decode engine TTFT %g s at %g prompt tokens overflows at the prompt asked, %g tokens",
				dt.Seconds, dt.PromptTokens, pf.p.PromptTokens))
			return ttftPlan{}, &u
		case dt.Waiting > 0:
			u := refuse(DecodeTTFTUnderLoad, fmt.Sprintf("decode TTFT %.1f s was measured with %d requests waiting on decode capacity",
				dt.Seconds, dt.Waiting))
			return ttftPlan{}, &u
		default:
			// The decode engine's TTFT holds the KV pull: pricing T_xfer
			// beside it would count the pull twice.
			pl.decodeTTFT = dt
			pf.xfer = 0
			pf.setVP(pf.vp)
			pf.flags = slices.DeleteFunc(pf.flags, func(f Flag) bool { return f == TransferOmitted })
			pf.flags = append(pf.flags, DecodeEngineMeasured)
		}
	}
	pl.pf, pl.flags = pf, pf.flags
	if pl.psCapped {
		pl.flags = append(pl.flags, SpreadBeyondProcessorSharing)
	}
	switch fl.FlowControl.State {
	case FlowControlOff:
	case FlowControlNoSeries:
		pl.flags = append(pl.flags, FlowControlInferredOff)
	case FlowControlOn:
		qt, kt := fl.FlowControl.QueueThreshold, fl.FlowControl.KVThreshold
		if !Finite(qt) || !(qt > 0) || !Finite(kt) || !(kt > 0) {
			u := refuse(FlowControlUnknown, fmt.Sprintf("queue threshold %g, KV threshold %g", qt, kt))
			return ttftPlan{}, &u
		}
		if fl.Decode == nil {
			pl.flags = append(pl.flags, DecodeOnsetUnchecked)
		}
		pl.chargeGate = fl.ChargeDecodeGate && fl.Decode != nil
		on, u := pl.placeOnsets()
		if u != nil {
			return ttftPlan{}, u
		}
		pl.onsets = on
	default:
		u := refuse(InvalidInput, fmt.Sprintf("flow-control state %d", fl.FlowControl.State))
		return ttftPlan{}, &u
	}
	return pl, nil
}

// OnsetPlacement is how the prefill flow-control onset is placed
// (TTFTFleet.PSOnsets; PrefillOnsetPlacement).
type OnsetPlacement string

const (
	// PlacedFirstCome is a first-come waiting count: PSOnsets off, or the
	// prefill pod not priced as processor sharing.
	PlacedFirstCome OnsetPlacement = "first come"
	// PlacedProcessorSharing is the processor-sharing stage at the stated
	// running cap.
	PlacedProcessorSharing OnsetPlacement = "processor sharing at the running cap"
	// PlacedLowerCapUnknown is the lower of processor sharing at a cap of 1
	// and first come, the running cap not known. Cap 1 alone can sit above
	// first come: the stage reads each pod at its own Ca^2 (splitCA2), near 1
	// on many pods, where first come reads the fleet's.
	PlacedLowerCapUnknown OnsetPlacement = "the lower of processor sharing at a running cap of 1 and first come (running cap not known)"
	// PlacedLowerWideSpread is the lower of processor sharing at the running
	// cap and first come, the prompt cv past MaxPSPromptCV, where the price
	// is the larger of the two queues'.
	PlacedLowerWideSpread OnsetPlacement = "the lower of processor sharing at the running cap and first come (prompt cv past MaxPSPromptCV)"
)

// placement is how the plan places its prefill onset.
func (pl ttftPlan) placement() OnsetPlacement {
	switch {
	case !pl.psOnsets:
		return PlacedFirstCome
	case pl.psCapped:
		return PlacedLowerWideSpread
	case pl.pf.p.RunningCap == 0:
		return PlacedLowerCapUnknown
	}
	return PlacedProcessorSharing
}

// PrefillOnsetPlacement is how FlowControlOnsets places fl's prefill onset,
// and false where it places none: flow control not on, or the fleet refused.
func PrefillOnsetPlacement(fl TTFTFleet) (OnsetPlacement, bool) {
	pl, u := fl.plan()
	if u != nil || pl.fc.State != FlowControlOn {
		return "", false
	}
	return pl.placement(), true
}

// GateCharge is what TTFTFleet.ChargeDecodeGate does to a rate answer
// (DecodeGateChargeFor).
type GateCharge string

const (
	// GateChargedToTail is the closed share charged to a percentile's tail.
	GateChargedToTail GateCharge = "the decode gate's closed share charged to the percentile's tail"
	// GateCappedForMean is a mean target capped where the closed share
	// reaches MaxDecodeGateClosedShare.
	GateCappedForMean GateCharge = "the mean capped where the decode gate's closed share reaches MaxDecodeGateClosedShare"
	// GateNoDecodeLine is the switch on with no decode line: there is no
	// decode gate to charge.
	GateNoDecodeLine GateCharge = "not applied: no decode line"
	// GateFlowControlNotOn is the switch on with flow control not on: there
	// is no gate.
	GateFlowControlNotOn GateCharge = "not applied: flow control is not on"
)

// DecodeGateChargeFor is what ChargeDecodeGate does to RateForTTFT's answer
// on fl at target t, and false with the switch off or the fleet refused.
func DecodeGateChargeFor(fl TTFTFleet, t TTFTTarget) (GateCharge, bool) {
	if !fl.ChargeDecodeGate {
		return "", false
	}
	pl, u := fl.plan()
	switch {
	case u != nil:
		return "", false
	case pl.fc.State != FlowControlOn:
		return GateFlowControlNotOn, true
	case !pl.chargeGate:
		return GateNoDecodeLine, true
	case t.Percentile == 0:
		return GateCappedForMean, true
	}
	return GateChargedToTail, true
}

// MaxRunningCap is the largest Prefill.RunningCap priced: one prompt per
// slot the admission chain holds.
const MaxRunningCap = maxSlots

// placeOnsets finds each stage's onset by bisection on the rate: the least
// rate at which the stage's mean endpoint score reaches 1, below the stage's
// knee. A stage whose score stays under 1 up to its knee has its knee as the
// onset. The prefill score is the placement's: a larger score is a lower
// onset.
func (pl ttftPlan) placeOnsets() (Onsets, *Unanswerable) {
	qt, kt := pl.fc.QueueThreshold, pl.fc.KVThreshold
	firstCome := func(r float64) float64 { return pl.pf.meanScore(r, qt) }
	ps := func(r float64) float64 { return pl.pf.psMeanScore(r, qt, max(1, pl.pf.p.RunningCap)) }
	var score func(float64) float64
	switch pl.placement() {
	case PlacedFirstCome:
		score = firstCome
	case PlacedProcessorSharing:
		score = ps
	default:
		score = func(r float64) float64 { return max(ps(r), firstCome(r)) }
	}
	var on Onsets
	on.Prefill = onset(pl.pf.knee(), score)
	if pl.d == nil {
		return on, nil
	}
	d := *pl.d
	if d.full().slots > maxSlots {
		u := refuse(TooManySlots, fmt.Sprintf("%g slots on one replica, above the %d the chain holds", d.full().slots, maxSlots))
		return Onsets{}, &u
	}
	on.Decode = onset(d.kneeRate(), func(r float64) float64 { return decodeScore(d, r, qt, kt) })
	return on, nil
}

// decodeScore is a decode pod's score at rate, the decode pods balanced:
// max(Lq/QueueThreshold, fill/KVThreshold) from the admission chain, with Lq
// the mean waiting count, lambda_rep times the mean admission wait (Little),
// and fill the chain's mean fill. +Inf where the chain has no stationary
// state.
func decodeScore(d Decode, rate, qt, kt float64) float64 {
	adm, ok := DecodeAdmission(d, rate).(Admission)
	if !ok {
		return math.Inf(1)
	}
	lq := rate / float64(d.Replicas) * adm.MeanWait()
	return max(lq/qt, adm.Basis.Fill/kt)
}

// decodeScoreTail is the share of the time a decode pod's score at rate is
// at least 1, the decode pods balanced, on the admission chain decodeScore
// reads the means of: P(n >= n*), n* the least state whose score,
// max((n - c)+/QueueThreshold, min(n, c) KVPerRequest/(CapacityTokens KVThreshold)),
// reaches 1. 1 where the chain has no stationary state.
func decodeScoreTail(d Decode, rate, qt, kt float64) float64 {
	p := d.full()
	if p.slots > maxSlots || !(rate < d.kneeRate()) {
		return 1
	}
	c := int(p.slots)
	w, ok := solveChain(rate/float64(d.Replicas), func(m int) float64 { return d.chainMu(float64(m)) }, c)
	if !ok {
		return 1
	}
	// The waiting count n - c reaches qt at c + ceil(qt); the fill reaches
	// kt at ceil(kt C/KVreq) requests, where the slots hold that many.
	n := float64(c) + math.Ceil(qt)
	if m := math.Ceil(kt * d.CapacityTokens / d.KVPerRequest); m <= float64(c) {
		n = min(n, m)
	}
	if !(n < math.MaxInt32) {
		return 0
	}
	return w.atLeast(int(n))
}

// gateClosed is the most of the time the decode gate is closed at rate, and
// with Poisson arrivals the most of the requests that meet it closed. The
// gate closes when the decode pods' mean score reaches 1, which takes at
// least one pod's reaching 1: with the pods independent, each on its chain's
// law, that is 1 - (1 - p)^N_d, p one pod's decodeScoreTail. It errs high,
// toward a lower rate: one pod over 1 closes the gate only if the others'
// scores bring the mean there, so the bound loosens as the pods grow: at
// 0.9 of the decode onset one pod is over 1 for 1.2 % of the time, and the
// bound reads 2.3 % at two pods, 17 % at 16 and 31 % at 32.
func (pl ttftPlan) gateClosed(rate float64) float64 {
	p := decodeScoreTail(*pl.d, rate, pl.fc.QueueThreshold, pl.fc.KVThreshold)
	return -math.Expm1(float64(pl.d.Replicas) * math.Log1p(-p))
}

// onset is the least rate in [0, knee) at which score reaches 1, or knee if
// it never does there. score is non-decreasing in the rate.
func onset(knee float64, score func(float64) float64) float64 {
	_, hi := bisectRate(knee, func(r float64) bool { return score(r) < 1 })
	return hi
}

// bisectRate brackets the edge of a predicate that holds below some rate in
// [0, limit) and fails above it: lo is the largest rate found where it holds
// and hi the least where it fails, within a relative 1e-12. When it holds
// just under limit, both are limit.
func bisectRate(limit float64, holds func(float64) bool) (lo, hi float64) {
	if holds(limit * (1 - 1e-12)) {
		return limit, limit
	}
	lo, hi = 0, limit
	for range 200 {
		mid := lo + (hi-lo)/2
		if holds(mid) {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo <= 1e-12*hi {
			break
		}
	}
	return lo, hi
}

// PrefillKnee is the prefill knee, N_p * V_P / (f * E[I_eff]), in requests
// per second over the fleet (question 2, step 1): the rate at which the
// busiest prefill pod saturates. Basis.TTFT.KneeRate repeats it.
func PrefillKnee(p Prefill) Result {
	pl, u := p.check()
	if u != nil {
		return *u
	}
	k := pl.knee()
	return Answer{Value: k, Flags: pl.flags, Basis: Basis{BoundBy: []Bound{BoundPrefillKnee}, TTFT: &TTFTBasis{KneeRate: k}}}
}

// FlowControlOnsets are the rates at which EPP flow control's gate starts
// holding requests (docs/proposals/slo-capacity-planner-ttft.md, "The router
// queue"): zero with flow control off. The decode onset is an extra knee:
// RateForTTFT caps at it and TTFTAt refuses above it.
func FlowControlOnsets(fl TTFTFleet) Result {
	pl, u := fl.plan()
	if u != nil {
		return *u
	}
	return pl.onsets
}

// lowestKnee is the lower of the two roles' knees.
func (pl ttftPlan) lowestKnee() float64 {
	k := pl.pf.knee()
	if pl.d != nil {
		k = min(k, pl.d.kneeRate())
	}
	return k
}

// priced is one TTFT at one rate, before it becomes a TTFT result.
type priced struct {
	out TTFT
	// u is set when the rate cannot be priced.
	u *Unanswerable
}

// price is the TTFT at rate and percentile q (0 for the mean). With
// withDecodeWait false the decode admission wait is left out: the prefill
// side alone, which RateForTTFT compares against.
//
// Under processor sharing past MaxPSPromptCV it is the larger of the two
// queues' prices (the plan carries SpreadBeyondProcessorSharing): the figure
// and its parts are the larger one's. A refusal from either is the answer.
func (pl ttftPlan) price(rate, q float64, withDecodeWait, extras bool) priced {
	overSlots := pl.ps && pl.pf.overSlots(rate)
	if !pl.psCapped && !overSlots {
		return pl.priceOne(rate, q, withDecodeWait, extras)
	}
	ps := pl.priceOne(rate, q, withDecodeWait, extras)
	if ps.u != nil {
		return ps
	}
	fc := pl
	fc.ps = false
	first := fc.priceOne(rate, q, withDecodeWait, extras)
	if first.u != nil {
		return first
	}
	out := ps
	if first.out.Seconds > ps.out.Seconds {
		out = first
	}
	if overSlots && !pl.psCapped {
		out.out.Flags = addFlags(out.out.Flags, PSSlotsExceeded)
	}
	return out
}

// decodeSaturates says rate is at or above the decode line's knee (the line
// was checked with the plan, as lowestKnee reads it): there the fleet
// saturates on decode, whatever bound V_P is.
func (pl ttftPlan) decodeSaturates(rate float64) bool {
	return pl.d != nil && rate >= pl.d.kneeRate()
}

// aboveDecodeOnset is the refusal of a rate at or above the decode gate's
// onset under flow control, nil below it or with flow control off.
func (pl ttftPlan) aboveDecodeOnset(rate float64) *Unanswerable {
	if pl.fc.State != FlowControlOn || pl.d == nil || rate < pl.onsets.Decode {
		return nil
	}
	u := refuse(AboveDecodeOnset, fmt.Sprintf("%.4g r/s at or above the decode gate's onset %.4g r/s", rate, pl.onsets.Decode))
	return &u
}

// priceOne is price on the plan's one queue: processor sharing with pl.ps,
// first come without.
func (pl ttftPlan) priceOne(rate, q float64, withDecodeWait, extras bool) priced {
	pk := pl.pf.knee()
	out := TTFT{PrefillKnee: pk, Onsets: pl.onsets, Flags: append([]Flag(nil), pl.flags...)}
	if pl.servedOnly {
		// A V_P known only as the rate a pod computed prices no TTFT, at any
		// rate: under its knee the figure would rest on a V_P that may be
		// far higher, and at or past it the knee is only a lower bound, which
		// the planner may raise to the work the pods served besides.
		u := refuse(NoPrefillCapacity, fmt.Sprintf("V_P %.0f tokens/s is only the rate a prefill pod was seen to compute "+
			"(it never queued for capacity): no TTFT is priced from it; pass --assume-vp for a flagged answer", pl.pf.p.VP))
		u.Flags = []Flag{VPLowerBound}
		return priced{u: &u}
	}
	if pl.notCovered {
		// A V_P read at other prompts prices no TTFT here: the queue under it
		// would rest on a speed the pods were never seen to run at this
		// prompt, and a lower bound there is not one here.
		p := pl.pf.p
		u := refuse(NoPrefillCapacity, fmt.Sprintf("V_P %.0f tokens/s was read over prompts of %.0f to %.0f tokens; "+
			"a mean prompt of %.0f is outside them, so no TTFT is priced from it; pass --assume-vp for a flagged answer",
			p.VP, p.VPPromptLow, p.VPPromptHigh, p.PromptTokens))
		u.Flags = []Flag{VPPromptNotCovered}
		return priced{u: &u}
	}
	if rate >= pk {
		if pl.kneeUnpriced && !pl.decodeSaturates(rate) {
			// The decode gate's onset is known whatever bound V_P is: a
			// rate past it is the miss it was before the knee went unpriced.
			if u := pl.aboveDecodeOnset(rate); u != nil {
				return priced{u: u}
			}
			u := refuse(PastLowerBoundKnee, fmt.Sprintf("%.4g r/s at or above the prefill knee %.4g r/s, "+
				"priced from a V_P that is only a lower bound: the true knee may be higher, so it is not priced", rate, pk))
			u.Flags = []Flag{VPLowerBound}
			return priced{u: &u}
		}
		u := refuse(Saturated, fmt.Sprintf("%.4g r/s at or above the prefill knee %.4g r/s", rate, pk))
		return priced{u: &u}
	}
	// gated is the flow-control gate priced as a stage (ttft_gate.go): with
	// TTFTFleet.PriceRouterWait taking, at or above the prefill onset. With
	// RouterWaitInPrefill instead the wait is flagged priced and the figure
	// is the processor-sharing price.
	var gated bool
	if pl.fc.State == FlowControlOn {
		if u := pl.aboveDecodeOnset(rate); u != nil {
			return priced{u: u}
		}
		if rate >= pl.onsets.Prefill {
			f := AboveFlowControlOnset
			if pl.routerWait && rate <= MaxRouterWaitKneeShare*pk {
				f, gated = FlowControlWaitPriced, pl.gateStage
			}
			out.Flags = append(out.Flags, f)
		}
	}

	pf := pl.pf.atRate(rate)
	out.VPMarginApplied = pf.applied
	rho := pf.utilization(rate)
	own := pf.own()
	// The tier load waits as a does: a request's own, before its prefill.
	a := pl.pf.p.FixedLatency + pl.pf.p.TierLoadSeconds
	out.Parts.TierLoad = pl.pf.p.TierLoadSeconds
	out.PrefillUtilization = rho
	out.Parts.OwnPrefill = a + own.mean
	out.Parts.PrefillWork = pf.work
	// side is block 1 for a plan's own time: the worst pod's wait and the own
	// time, or the PS stages; old at the old sum's transfer; pooled, the pods
	// pooled with no imbalance. Under processor sharing each of the four is
	// worked out once a price, and a refusal of any is the price's.
	var side func(old, pooled bool) (quantiler, *Unanswerable)
	var wait waitTail
	if pl.ps {
		m, parts, u := pf.psStages(rate, pl.decodePods(), false)
		if u != nil {
			return priced{u: u}
		}
		// gate is the flow-control gate's wait in front of every part, gated;
		// the pooled figure, a router that feeds the first free pod, has none.
		var gate waitTail
		if gated {
			g, ok := newGateStage(pf, rate, parts.prefill)
			if ok {
				gate = g.wait()
				_, ok = m.withWait(gate)
			}
			if ok {
				out.Parts.RouterWait, out.Parts.BurstWait = g.routerWait().mean(), g.burstWait().mean()
			} else {
				// No finite gate to price, or one the mixture does not take:
				// the wait is left unpriced and flagged so, a miss.
				gate = waitTail{}
				out.Flags = unpriceGate(out.Flags)
			}
		}
		withGate := func(mix stageMix, pooled bool) stageMix {
			if pooled || len(gate.w) == 0 {
				return mix
			}
			gm, ok := mix.withWait(gate)
			if !ok {
				// Checked above on the plan's own mixture, whose parts the
				// others share in shape: not reached.
				return mix
			}
			return gm
		}
		type key struct{ old, pooled bool }
		mixes := map[key]stageMix{}
		side = func(old, pooled bool) (quantiler, *Unanswerable) {
			k := key{old, pooled}
			if m, ok := mixes[k]; ok {
				return m, nil
			}
			p := pf
			if old {
				p = pf.withTransfer(pl.term.oldTransfer)
			}
			m, _, u := p.psStages(rate, pl.decodePods(), pooled)
			if u != nil {
				return nil, u
			}
			m = withGate(m, pooled)
			mixes[k] = m
			return m, nil
		}
		mixes[key{}] = withGate(m, false)
		out.Parts.EngineWait, out.Parts.KVPullWait, out.Parts.KVPullUtilization = parts.prefill, parts.pull, parts.pullRho
	} else {
		theta := pf.waitScale(rho)
		var tail float64
		wait, tail = pf.engineWait(rho)
		if rho > 0 && (!Finite(tail) || !(tail > 0)) {
			u := refuse(Saturated, fmt.Sprintf("no finite wait tail at %.4g r/s (decay rate %.3g)", rate, tail))
			return priced{u: &u}
		}
		out.WaitScale = 1 / tail
		if len(wait.s) > 0 {
			out.WaitOffset = wait.s[0]
		}
		out.Parts.EngineWait = rho * theta
		side = func(old, pooled bool) (quantiler, *Unanswerable) {
			p := pf
			if old {
				p = pf.withTransfer(pl.term.oldTransfer)
			}
			if pooled {
				return block{wait: pf.pooledWait(rate), own: p.own()}, nil
			}
			return block{wait: wait, own: p.own()}, nil
		}
	}
	// stretch is the mean of the waits on the prefill side, the KV pull's
	// stretch with them.
	stretch := out.Parts.EngineWait + out.Parts.KVPullWait + out.Parts.RouterWait + out.Parts.BurstWait
	b1, _ := side(false, false)

	var dec waitTail
	var first, floorStep float64
	// oldFirst and oldFloorStep are the old sum's steps, with a term.
	var oldFirst, oldFloorStep float64
	if pl.d != nil {
		d := *pl.d
		out.DecodeKnee, out.Tier = d.kneeRate(), d.Tier
		// The admission chain is the costly part, and the prefill side
		// alone (withDecodeWait false, RateForTTFT's second bisection)
		// needs none of it: its first step comes from the closed-form fill,
		// which refuses past the knee as the chain does.
		var adm Admission
		if withDecodeWait || extras {
			switch v := DecodeAdmission(d, rate).(type) {
			case Admission:
				adm = v
			case Unanswerable:
				return priced{u: &v}
			}
		}
		k, ok := d.fillAtReplicaRate(rate / float64(d.Replicas))
		if !ok {
			u := refuse(Saturated, fmt.Sprintf("no decode fill carries %.4g r/s", rate))
			return priced{u: &u}
		}
		// fill is the fill the request runs at (seenFill): its first step
		// and the term are read there, as its TPOT is; k for Poisson
		// arrivals, higher where they cluster.
		fill := d.seenFill(k)
		if fill < d.Band.Lo {
			out.Flags = addFlags(out.Flags, BelowFittedBand)
		}
		// The floor is the one at the request's own count (lowestFillAt),
		// as its TPOT reads it (issue
		// llm-d-incubation/llm-d-scaling-manager#485).
		lowest, _ := d.lowestFillAt(fill)
		if _, extrapolated := d.readFill(fill); extrapolated {
			out.Flags = addFlags(out.Flags, ExtrapolatedBelowFloor)
		}
		k = max(fill, lowest)
		first, floorStep = d.itlAt(k), d.itlAt(lowest)
		oldFirst, oldFloorStep = first, floorStep
		if t := pl.term; t != nil {
			// The term reads the line at the actual fill: D is several steps,
			// so the clamp's reading, harmless on one step, would be the
			// figure's largest error at light load. No rate has a fill under
			// 0, which is the floor's.
			engine, hop := t.at(d.itlAt(fill), first)
			out.Parts.RouterHop = hop
			if t.omitted {
				out.Parts.FirstStep = engine
			} else {
				out.Parts.DecodeEngine = engine
			}
			first = engine + hop
			engine, hop = t.at(d.itlAt(0), floorStep)
			floorStep = engine + hop
		}
		out.Flags = addFlags(out.Flags, adm.Flags...)
		out.DecodeWaitProbability = adm.WaitProbability
		if withDecodeWait {
			dec = expWait(adm.WaitProbability, adm.Decay)
			out.Parts.DecodeWait = adm.MeanWait()
		}
	} else {
		// No decode line: decode's part is the decode engine's TTFT,
		// measured (0, flagged DecodeTTFTUnmeasured, when it is not). It is
		// a mean, added at its mean to every percentile: its own spread is
		// not priced.
		first = pl.decodeTTFT.at(pl.pf.p.PromptTokens)
		if !Finite(first) {
			// plan refuses an overflowing term; kept so that no infinite
			// figure is ever answered as a TTFT.
			u := refuse(InvalidInput, fmt.Sprintf("decode engine TTFT %g s at %g prompt tokens", first, pl.pf.p.PromptTokens))
			return priced{u: &u}
		}
		floorStep = first
		out.Parts.DecodeEngine = first
	}

	if pl.d != nil && pl.term == nil {
		out.Parts.FirstStep = first
	}

	// The old sum floors the term on every path, at this rate and at this
	// percentile: it is what was measured of the pull and the step. A term
	// under it is a fit that does not hold here.
	var oldOwn, oldOwnZero ownTime
	if pl.term != nil {
		oldOwn = pf.withTransfer(pl.term.oldTransfer).own()
		oldOwnZero = pl.pf.withTransfer(pl.term.oldTransfer).own()
		mean := stretch + out.Parts.OwnPrefill + out.Parts.DecodeWait + first
		if excess := stretch + a + oldOwn.mean + out.Parts.DecodeWait + oldFirst - mean; excess > 0 {
			out.Parts.DecodeEngine += excess
		}
	}
	// floored applies the old sum's floor, which it works out only with a term:
	// with none oldOwnZero is a zero ownTime, whose quantile runs to
	// maxSeriesTerms for a NaN nothing reads, about 100 ms a RateForTTFT.
	floored := func(seconds, old *float64, floor float64, oldFloorAt func() float64) {
		if pl.term == nil {
			out.Floor = floor
			return
		}
		oldFloor := oldFloorAt()
		out.Floor = max(floor, oldFloor)
		if *old > *seconds || oldFloor > floor {
			out.Flags = addFlags(out.Flags, DecodeTermFloored)
		}
		*seconds = max(*seconds, *old)
	}

	if q == 0 {
		mean := stretch + out.Parts.OwnPrefill + out.Parts.DecodeWait + first
		oldMean := stretch + a + oldOwn.mean + out.Parts.DecodeWait + oldFirst
		oldMeanFloor := func() float64 {
			noteOldFloor()
			return a + oldOwnZero.mean + oldFloorStep
		}
		floored(&mean, &oldMean, a+pl.pf.own().mean+floorStep, oldMeanFloor)
		if !Finite(mean) {
			u := refuse(Saturated, fmt.Sprintf("no finite mean TTFT at %.4g r/s", rate))
			return priced{u: &u}
		}
		out.Seconds, out.Independent = mean, mean
		if extras {
			pooledWait := pl.pooledMeanWait(pf, rate)
			out.Pooled = pooledWait + out.Parts.OwnPrefill + out.Parts.DecodeWait + first
			if pl.term != nil {
				out.Pooled = max(out.Pooled, pooledWait+a+oldOwn.mean+out.Parts.DecodeWait+oldFirst)
			}
		}
		return priced{out: out}
	}
	v, beta := split(b1, dec, q)
	if !Finite(v) {
		// Only within rounding of a knee, where the wait's scale overflows.
		u := refuse(Saturated, fmt.Sprintf("no finite TTFT at %.4g r/s", rate))
		return priced{u: &u}
	}
	if !Finite(a + v + first) {
		u := refuse(InvalidInput, fmt.Sprintf("no finite TTFT at %.4g r/s: a %g s, decode %g s", rate, a, first))
		return priced{u: &u}
	}
	out.Seconds, out.DecodeShare = a+v+first, beta
	var oldSeconds float64
	// The split is the costly part: with neither of the term's parts under the
	// old sum's, the old sum cannot stand over it, in any figure below.
	oldCounts := pl.term != nil && (first < oldFirst || pf.xfer < pl.term.oldTransfer)
	if oldCounts {
		oldSide, u := side(true, false)
		if u != nil {
			return priced{u: u}
		}
		oldV, oldBeta := split(oldSide, dec, q)
		oldSeconds = a + oldV + oldFirst
		if oldSeconds > out.Seconds {
			out.DecodeShare = oldBeta
		}
	}
	oldQuantileFloor := func() float64 {
		noteOldFloor()
		return a + oldOwnZero.quantile(q) + oldFloorStep
	}
	floored(&out.Seconds, &oldSeconds, a+pl.pf.own().quantile(q)+floorStep, oldQuantileFloor)
	out.Independent = out.Seconds
	if extras {
		// The two waits always sum (a single-piece wait and the decode
		// chain's unshifted one); were they ever not to, the union bound
		// stands for the independent figure, which it never undercuts. Each
		// figure takes the old sum's floor on its own, so each stays under
		// the headline, which takes it too.
		if mix, ok := b1.(stageMix); ok && len(dec.w) > 0 {
			// Each part's own time behind the decode wait, independent, and
			// behind the flow-control gate's where it is priced: the two
			// waits sum (stageMix.withWait), all unshifted pieces.
			if withDec, ok := mix.withWait(dec); ok {
				out.Independent = a + withDec.quantile(1-q) + first
			}
			if oldCounts {
				// side(true, false) priced above, when oldCounts.
				old, _ := side(true, false)
				if withDec, ok := old.(stageMix).withWait(dec); ok {
					out.Independent = max(out.Independent, a+withDec.quantile(1-q)+oldFirst)
				}
			}
		} else if both, ok := wait.sum(dec); ok && len(dec.w) > 0 && !pl.ps {
			out.Independent = a + block{wait: both, own: own}.quantile(1-q) + first
			if oldCounts {
				out.Independent = max(out.Independent, a+block{wait: both, own: oldOwn}.quantile(1-q)+oldFirst)
			}
		}
		pooledSide, u := side(false, true)
		if u != nil {
			return priced{u: u}
		}
		pv, _ := split(pooledSide, dec, q)
		out.Pooled = a + pv + first
		if oldCounts {
			oldPooled, u := side(true, true)
			if u != nil {
				return priced{u: u}
			}
			oldPv, _ := split(oldPooled, dec, q)
			out.Pooled = max(out.Pooled, a+oldPv+oldFirst)
		}
		// Allen-Cunneen decays at Kingman's rate. Below cv 1 that is slower than
		// the root the worst pod takes, so far enough out it would read above
		// the figure it is the optimistic side of.
		out.Pooled = min(out.Pooled, out.Seconds)
	}
	return priced{out: out}
}

// decodePods is N_d, or 0 with no decode line.
func (pl ttftPlan) decodePods() int {
	if pl.d == nil {
		return 0
	}
	return pl.d.Replicas
}

// pooledMeanWait is the optimistic side's mean wait at rate: the pooled
// queue's (pooledWait), or under processor sharing the PS stages' stretch
// with every prefill pod at the mean share.
func (pl ttftPlan) pooledMeanWait(pf prefillPlan, rate float64) float64 {
	if !pl.ps {
		return pf.pooledWait(rate).mean()
	}
	m, _, u := pf.psStages(rate, pl.decodePods(), true)
	if u != nil {
		return math.Inf(1)
	}
	return m.mean() - pf.own().mean
}

// pooledWait is the optimistic wait: the N_p pods as one M/G/N_p queue with
// no imbalance (Allen-Cunneen), P(W > t) = C * exp(-t/theta_p), C Erlang C at
// the offered load and theta_p = E[S](Ca^2 + Cs^2) / (2 N_p (1 - rho)).
func (pl prefillPlan) pooledWait(rate float64) waitTail {
	n := pl.p.Replicas
	load := rate * pl.work
	rho := load / float64(n)
	thetaP := pl.work * (pl.ca2 + pl.cs2) / (2 * float64(n) * (1 - rho))
	return expWait(erlangCProb(n, load), 1/thetaP)
}

// checkPercentile refuses a TTFT percentile outside (0, 1); 0 is the mean.
func checkPercentile(q float64) *Unanswerable {
	if q == 0 || (q > 0 && q < 1) {
		return nil
	}
	u := refuse(InvalidInput, fmt.Sprintf("TTFT percentile %g, want 0 < q < 1, or 0 for the mean", q))
	return &u
}

// TTFTAt is the end-to-end TTFT at rate requests per second over the fleet,
// at percentile q, or the mean for q = 0 (question 2's TTFT): the union bound
// with its parts, the independent figure beside it, the pooled optimistic
// figure and the floor.
//
// It refuses (Saturated) at or above the lower knee, and, with flow control
// on, at or above the decode gate's onset (AboveDecodeOnset); at or above the
// prefill gate's onset it answers, flagged AboveFlowControlOnset, or
// FlowControlWaitPriced where TTFTFleet.PriceRouterWait or
// RouterWaitInPrefill takes. TTFTMeets
// reads the answer against a target.
func TTFTAt(fl TTFTFleet, rate, q float64) Result {
	// The question first: a usage error outranks what the fleet's data says,
	// and placing the onsets is not free.
	if u := checkRate(rate); u != nil {
		return *u
	}
	if u := checkPercentile(q); u != nil {
		return *u
	}
	pl, u := fl.plan()
	if u != nil {
		return *u
	}
	p := pl.price(rate, q, true, true)
	if p.u != nil {
		return *p.u
	}
	return p.out
}

// RateForTTFT is question 1 for TTFT: the largest rate, in requests per
// second over the fleet, whose TTFT at the target's percentile (or mean) is at
// most the target, decode admission priced at the same rate
// (docs/proposals/slo-capacity-planner.md, "Question 1").
//
// TTFT rises with the rate, so the rate is bisected below the lower knee and,
// with flow control on, below the decode gate's onset, which caps it
// (BoundDecodeOnset). When the prefill side alone would allow more (by more
// than a relative 1e-6), the rate is the one decode admission holds it to
// (BoundBy TTFT then BoundDecodeAdmission, flagged HeldByDecodeAdmission);
// Basis.TTFT.PrefillOnlyRate is the prefill side's own rate. If
// the TTFT at no load, the floor, already reaches the target, it refuses
// (TargetBelowFloor). The operating-point cap is not applied: the fleet
// carries the smaller of the roles' rates, then the cap, as the caller
// combines them.
//
// Basis.TTFT carries the lower knee, the TTFT floor and the prefill side's
// own rate; the TPOT fields (KneeRate, Floor, Uncapped) stay 0. Basis.Fill is
// the decode fill at the answer and Basis.Tier the decode line's tier (both
// empty with no decode side). Band is nil: the TTFT model is not banded yet.
//
// With flow control on it is also capped where the router starts holding
// requests the price does not take as priced (BoundPrefillOnset): the prefill
// onset, or with TTFTFleet.PriceRouterWait or RouterWaitInPrefill taking,
// MaxRouterWaitKneeShare of the prefill knee where that is higher. Above that cap a TTFT is flagged
// AboveFlowControlOnset, which TTFTMeets reads as a miss, so a rate answer
// never stands there: the answer is always a rate whose own verdict is met.
//
// The cap at the decode onset alone is not conservative: the onset is where
// the MEAN endpoint score reaches 1, while the gate reads the current score,
// and E[max] >= max E, so the gate closes for part of the time below it. With
// TTFTFleet.ChargeDecodeGate that share (ttftPlan.gateClosed) is charged: a
// percentile target is met where the price at q plus the share meets it, a
// request that meets a closed gate taken as a miss; a mean target is capped
// where the share reaches MaxDecodeGateClosedShare, bound
// BoundDecodeGateCharge. When the charge lowers a percentile's rate, BoundBy
// is TTFT then BoundDecodeGateCharge, and BoundDecodeAdmission after it
// where decode admission holds the rate as well. The charge never names
// BoundDecodeOnset: the rate is still the prefill side's to raise.
func RateForTTFT(fl TTFTFleet, t TTFTTarget) Result {
	if !Finite(t.Seconds) || !(t.Seconds > 0) {
		return refuse(InvalidInput, fmt.Sprintf("TTFT target %g s", t.Seconds))
	}
	if u := checkPercentile(t.Percentile); u != nil {
		return *u
	}
	pl, u := fl.plan()
	if u != nil {
		return *u
	}
	zero := pl.price(0, t.Percentile, true, false)
	if zero.u != nil {
		return *zero.u
	}
	floor := zero.out.Floor
	if floor >= t.Seconds {
		return pl.belowFloor(fl, t, fmt.Sprintf("target %.4g s at or below the TTFT floor %.4g s", t.Seconds, floor), zero.out.Flags)
	}
	limit, limitBy := pl.lowestKnee(), Bound("")
	if pl.fc.State == FlowControlOn && pl.d != nil && pl.onsets.Decode < limit {
		limit, limitBy = pl.onsets.Decode, BoundDecodeOnset
	}
	if pl.chargeGate && t.Percentile == 0 {
		// The last rate the share is under the constant at, not the first
		// past it.
		if lo, hi := bisectRate(limit, func(r float64) bool { return pl.gateClosed(r) < MaxDecodeGateClosedShare }); hi < limit {
			limit, limitBy = lo, BoundDecodeGateCharge
		}
	}
	if c := pl.prefillCap(); pl.fc.State == FlowControlOn && c < limit {
		limit, limitBy = c, BoundPrefillOnset
	}
	charge := pl.chargeGate && t.Percentile > 0
	full := pl.bisect(t, limit, bisectOpts{withDecodeWait: true, chargeGate: charge})
	if !(full > 0) {
		// The floor is under the target, but by less than the bisection
		// resolves: no positive rate is known to meet it.
		return pl.belowFloor(fl, t, fmt.Sprintf("target %.4g s: no positive rate meets it (TTFT floor %.4g s)", t.Seconds, floor), zero.out.Flags)
	}
	alone := pl.bisect(t, limit, bisectOpts{chargeGate: charge})
	// uncharged is the rate with the gate's closed share left out, to tell
	// whether the charge set the rate.
	uncharged := full
	if charge {
		uncharged = pl.bisect(t, limit, bisectOpts{withDecodeWait: true})
	}

	var by []Bound
	flags := append([]Flag(nil), pl.flags...)
	switch {
	case limitBy != "" && full >= limit:
		by = []Bound{limitBy}
	default:
		by = []Bound{BoundTTFT}
		if uncharged > full*(1+1e-6) {
			// The decode gate's closed share, charged to the tail, holds
			// the rate below the target's own.
			by = append(by, BoundDecodeGateCharge)
		}
		if alone > full*(1+1e-6) {
			// Decode admission's share of the tail holds it below what the
			// prefill side alone allows.
			by = append(by, BoundDecodeAdmission)
			flags = append(flags, HeldByDecodeAdmission)
		}
	}
	// The answer is the rate priced, under the cap: at an onset itself the
	// verdict there (TTFTMeets) is a miss, which a rate answer never is.
	full = min(full, limit*(1-1e-12))
	at := pl.price(full, t.Percentile, true, false)
	if at.u != nil {
		return *at.u
	}
	for _, f := range at.out.Flags {
		if f == BelowFittedBand || f == KVBoundSlots || f == AboveFlowControlOnset || f == FlowControlWaitPriced || f == DecodeTermFloored {
			flags = addFlags(flags, f)
		}
	}
	if slices.Contains(zero.out.Flags, DecodeTermFloored) {
		flags = addFlags(flags, DecodeTermFloored)
	}
	var fill float64
	var tier string
	if pl.d != nil {
		fill, _ = pl.d.fillAtReplicaRate(full / float64(pl.d.Replicas))
		tier = pl.d.Tier
	}
	return Answer{Value: full, Flags: flags, Basis: Basis{
		Fill: fill, BoundBy: by, Tier: tier,
		TTFT: &TTFTBasis{Floor: floor, KneeRate: pl.lowestKnee(), PrefillOnlyRate: alone},
	}}
}

// prefillCap is the highest rate a rate answer may stand at on the prefill
// side with flow control on: the prefill onset, past which a TTFT is flagged
// AboveFlowControlOnset, or where the router's wait is taken as priced, the
// higher of that and MaxRouterWaitKneeShare of the prefill knee.
func (pl ttftPlan) prefillCap() float64 {
	if pl.routerWait {
		return max(pl.onsets.Prefill, MaxRouterWaitKneeShare*pl.pf.knee())
	}
	return pl.onsets.Prefill
}

// bisectOpts are what bisect prices. withDecodeWait prices the decode
// admission wait; without it the prefill side alone. chargeGate meets a
// percentile q at a rate where the TTFT at q plus the decode gate's closed
// share there (gateClosed) meets it: P(miss) <= P(TTFT > t) + P(gate
// closed), a request that meets the gate closed taken as a miss.
type bisectOpts struct {
	withDecodeWait, chargeGate bool
}

// bisect is the largest rate below limit whose TTFT meets the target, or
// limit itself when the TTFT just under it still does (only an onset: at a
// knee the TTFT is unbounded).
func (pl ttftPlan) bisect(t TTFTTarget, limit float64, o bisectOpts) float64 {
	lo, _ := bisectRate(limit, func(rate float64) bool {
		q := t.Percentile
		if o.chargeGate {
			q += pl.gateClosed(rate)
			if !(q < 1) {
				return false
			}
		}
		p := pl.price(rate, q, o.withDecodeWait, false)
		return p.u == nil && p.out.Seconds <= t.Seconds
	})
	return lo
}

// onOldFloor, when set, is called each time a price works out the old sum's
// floor. It is a test hook, nil outside tests, which run serially.
var onOldFloor func()

// noteOldFloor calls onOldFloor when it is set.
func noteOldFloor() {
	if onOldFloor != nil {
		onOldFloor()
	}
}

// addFlags appends each flag not already in flags.
func addFlags(flags []Flag, add ...Flag) []Flag {
	for _, f := range add {
		if !slices.Contains(flags, f) {
			flags = append(flags, f)
		}
	}
	return flags
}
