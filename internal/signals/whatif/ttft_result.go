package whatif

import "slices"

// The reasons the TTFT model refuses with, beside the decode reasons in
// result.go (docs/proposals/slo-capacity-planner-ttft.md). Reasons() lists
// them with the rest. The TTFT questions also raise NoTraffic (no prompt
// length), Saturated (a rate at or above the prefill knee as well as the
// decode knee), TargetBelowFloor and InvalidInput, with the same meanings.
const (
	// NoPrefillCapacity is no V_P from a source that cannot over-state it
	// (busy minutes, the peak under a queue, the back-pressure bound), and
	// none assumed. SGLang always lands here: it publishes no computed-token
	// series.
	NoPrefillCapacity Reason = "NoPrefillCapacity"
	// NoFixedLatency is no a, the fixed latency one request pays on a prefill
	// pod: P1 refused its low-load fit, or the reading is not positive and
	// finite.
	NoFixedLatency Reason = "NoFixedLatency"
	// NoImbalance is a prefill role of more than one pod with no f, the
	// busiest pod's share of the work over the mean share, learned or
	// assumed. f = 1 is not assumed silently: it over-promises.
	NoImbalance Reason = "NoImbalance"
	// NoPrefillReplicas is a TTFT question with no prefill pod to price.
	NoPrefillReplicas Reason = "NoPrefillReplicas"
	// FlowControlUnknown is EPP flow control on, with the saturation
	// detector's queue or KV threshold neither read nor given: the onsets
	// cannot be placed.
	FlowControlUnknown Reason = "FlowControlUnknown"
	// AboveDecodeOnset is a TTFT asked at a rate at or above the decode
	// gate's onset with flow control on. A closed decode gate stops dispatch
	// for every request and is not work conserving, so the model does not
	// price TTFT there (docs/proposals/slo-capacity-planner-ttft.md, "The
	// router queue").
	AboveDecodeOnset Reason = "AboveDecodeOnset"
)

// ttftReasons are the reasons this file adds, in the order Reasons() lists
// them.
func ttftReasons() []Reason {
	return []Reason{NoPrefillCapacity, NoFixedLatency, NoImbalance, NoPrefillReplicas,
		FlowControlUnknown, AboveDecodeOnset}
}

// The flags the TTFT model raises.
const (
	// UnvalidatedTTFT is on every TTFT figure. Repeat 1 of the staircase run
	// (M2) failed the form before the decode term. That run chose the term,
	// so it cannot validate it: validation is a fresh frozen run. On P2's
	// goldens it reads low at light load, up to about 33% under the router's
	// p90: not a bound.
	UnvalidatedTTFT Flag = "UnvalidatedTTFT"
	// AssumedVP is V_P supplied by the caller (--assume-vp) rather than
	// learned.
	AssumedVP Flag = "AssumedVP"
	// VPLowerBound is V_P from a source that is a lower bound (the peak
	// under a queue, the back-pressure bound): the answer under-promises,
	// up to the noise of a one-minute rate.
	VPLowerBound Flag = "VPLowerBound"
	// VPAboveSlopeBound is a V_P above the low-load fit's bound on it (1/slope
	// of the line, or the curve's 1/(b + c*I) at V_P's prompt length,
	// prefill.Estimate.SlopeBound): one of the two readings is wrong.
	VPAboveSlopeBound Flag = "VPAboveSlopeBound"
	// AssumedImbalance is f supplied by the caller rather than learned.
	AssumedImbalance Flag = "AssumedImbalance"
	// AssumedPromptSpread is a prompt coefficient of variation the caller
	// assumed (the page's "prompt spread assumed (cv 1)") rather than one
	// read from a prompt histogram.
	AssumedPromptSpread Flag = "AssumedPromptSpread"
	// TransferOmitted is a P/D TTFT with no KV transfer term:
	// nixl_xfer_time_seconds is not collected, so T_xfer is left out, and
	// the answer over-promises by it.
	TransferOmitted Flag = "TransferOmitted"
	// TransferHeld is a transfer read on earlier cycles: the latest had a queue or
	// no reading, and the quiet figure stands inside its age
	// (prefill.HeldDecodeAge). It sets the old sum's floor, so a held figure is a
	// floor that has not been read at this load.
	TransferHeld Flag = "TransferHeld"
	// DecodeSideOmitted is a TTFT priced with no decode line and no measured
	// decode-engine TTFT: neither the decode admission wait nor any part of
	// decode's TTFT is in it. It is raised with DecodeTTFTUnmeasured, and the
	// two always coincide; both are kept, DecodeSideOmitted for what the
	// answer leaves out and DecodeTTFTUnmeasured for why.
	DecodeSideOmitted Flag = "DecodeSideOmitted"
	// DecodeEngineMeasured is a TTFT priced with no decode line, decode's part
	// taken from the decode engine's measured TTFT (TTFTFleet.DecodeTTFT) in
	// place of the decode admission wait, T_xfer and ITL(k_d). Informational,
	// not a hold: the term's known biases (its spread, its rate) are stated on
	// the TTFT page and gated with UnvalidatedTTFT.
	DecodeEngineMeasured Flag = "DecodeEngineMeasured"
	// DecodeTermOmitted is a TTFT on a decode line with no fit of the decode
	// engine's TTFT and no reading of it: the old sum T_xfer + ITL(k_d) stands
	// for D, and the answer over-promises by the difference (DecodeTerm). On
	// the staircase run the old sum left out 13 to 101 ms.
	DecodeTermOmitted Flag = "DecodeTermOmitted"
	// DecodeStepSlopeAssumed is a TTFT on a decode line with no fit of the
	// decode engine's TTFT: its measured figure is held, and rises by
	// AssumedDecodeStepSlope from the ITL it was measured at. It never reads
	// under the old sum (DecodeTerm).
	DecodeStepSlopeAssumed Flag = "DecodeStepSlopeAssumed"
	// RouterHopAssumed is a TTFT on a decode line with no settled fit of the hop
	// between the router and the engines: R is AssumedRouterHopSeconds, or the
	// operator's (--assume-router-hop).
	RouterHopAssumed Flag = "RouterHopAssumed"
	// DecodeTermFloored is a headline or a floor where the decode term read
	// under the old sum T_xfer + ITL(k_d), which stands in its place. The term
	// holds what was measured of the pull and the step, so a term under the old
	// sum is wrong: this says its fit or its prompt does not hold here.
	DecodeTermFloored Flag = "DecodeTermFloored"
	// DecodeTermPrefixHits is a TTFT on a decode line with prefix hits past
	// MaterialPrefixHitShare: D goes with the whole prompt, which the KV pull
	// moves, and is priced at E[I_eff], so it reads low, down to the old sum.
	DecodeTermPrefixHits Flag = "DecodeTermPrefixHits"
	// DecodeTTFTUnmeasured is a TTFT priced with no decode line and no
	// measured decode-engine TTFT: decode's part, the KV pull and its first
	// token, is left out, and the answer over-promises by it (by 25% to 65%
	// on the P2 recordings). No caller acts on it.
	DecodeTTFTUnmeasured Flag = "DecodeTTFTUnmeasured"
	// OutputSpreadUnread is a decode line priced with no reading of the output
	// spread D: KVreq is ILeff + OL/2, which over-promises by 7-44% at IL = 2 OL
	// on a real spread (shape.WithOutputDispersion). P3 does not raise it: the
	// CLI and S1, which read the shape, do.
	OutputSpreadUnread Flag = "OutputSpreadUnread"
	// VPScaledToPrompt is a V_P read at one mean prompt and rescaled along
	// P1's curve to a longer one (Prefill.shapedVP): V_P * s(I_vp)/s(I),
	// s(I) = b + c*I.
	VPScaledToPrompt Flag = "VPScaledToPrompt"
	// VPShapeMoved is a V_P read at a mean prompt more than MaxVPShapeRatio
	// from the one asked, either way, or at one not known, with no curve to
	// rescale it by: the V_P is held, and on a longer prompt it reads high
	// and over-promises. No caller acts on it.
	VPShapeMoved Flag = "VPShapeMoved"
	// FlowControlInferredOff is flow control taken as off because its queue
	// series does not exist (a scrape gap looks the same).
	FlowControlInferredOff Flag = "FlowControlInferredOff"
	// AboveFlowControlOnset is a rate at or above the prefill gate's onset
	// with flow control on: the router holds the excess and the answer is the
	// per-pod model, which is not proven conservative there. The pooled
	// figure is the optimistic bound beside it. With
	// TTFTFleet.PriceRouterWait taking, FlowControlWaitPriced stands in its
	// place.
	AboveFlowControlOnset Flag = "AboveFlowControlOnset"
	// FlowControlWaitPriced is a rate at or above the prefill gate's onset
	// with flow control on, priced with the experimental
	// TTFTFleet.PriceRouterWait: the router holds the excess, and its wait is
	// taken as inside the processor-sharing prefill stage's figure. That is
	// argued and simulated, not measured: no staircase step with a real
	// router wait validates it, and at the one step where the router held,
	// the figure read about 20 times short.
	FlowControlWaitPriced Flag = "FlowControlWaitPriced"
	// HeldByDecodeAdmission is a TTFT rate the prefill side alone would
	// set higher: decode admission's wait holds it down.
	HeldByDecodeAdmission Flag = "HeldByDecodeAdmission"
	// DecodeOnsetUnchecked is a TTFT priced on the prefill side alone with
	// flow control on: the decode gate's onset, above which TTFT is not
	// priced, could not be placed, so the answer may stand above it. Pass
	// the decode role to check it.
	DecodeOnsetUnchecked Flag = "DecodeOnsetUnchecked"
	// SpreadBeyondProcessorSharing is a TTFT priced with
	// TTFTFleet.ProcessorSharing at a prompt spread past MaxPSPromptCV, wider
	// than any run has validated processor sharing at: the figure is the
	// larger of processor sharing's and the first-come queue's.
	SpreadBeyondProcessorSharing Flag = "SpreadBeyondProcessorSharing"
)

// The bounds a TTFT rate or a prefill knee can be set by.
const (
	// BoundTTFT is the TTFT target itself.
	BoundTTFT Bound = "TTFT"
	// BoundDecodeAdmission is a TTFT target met at a lower rate than the
	// prefill side alone allows, because of the decode admission wait.
	BoundDecodeAdmission Bound = "decode admission"
	// BoundDecodeOnset is the decode gate's onset with flow control on: an
	// extra knee, above which the model does not price TTFT.
	BoundDecodeOnset Bound = "decode flow-control onset"
	// BoundPrefillKnee is the prefill knee, N_p * V_P / (f * E[I_eff]).
	BoundPrefillKnee Bound = "prefill knee"
)

// TTFTBasis is what a TTFT rate rests on, beside the decode fields of Basis
// (Fill, BoundBy, Tier), which keep their meanings.
type TTFTBasis struct {
	// Floor is the TTFT floor at the target's percentile (or the mean), in
	// seconds: TTFT.Floor.
	Floor float64
	// KneeRate is the lower of the roles' knees, in requests per second over
	// the fleet: the prefill knee, or decode's where that is lower.
	KneeRate float64
	// PrefillOnlyRate is the rate the prefill side alone allows, the decode
	// admission wait left out; above the answer when decode admission holds
	// it down (HeldByDecodeAdmission).
	PrefillOnlyRate float64
}

// TTFT is an end-to-end TTFT priced at one rate: the answer (Seconds) and
// what it is made of (docs/proposals/slo-capacity-planner-ttft.md). Every
// TTFT carries UnvalidatedTTFT.
type TTFT struct {
	// Seconds is the answer: at a percentile, the optimised union bound over
	// the prefill side (block 1) and the decode admission wait; for the mean,
	// the sum of the parts' means. On the first-come queue block 1 is a
	// request on the busiest prefill pod, so the percentile and the mean are
	// that pod's. With TTFTFleet.ProcessorSharing block 1 is every prefill
	// pod's requests mixed by their shares, so they are the population's: a
	// request on any pod.
	Seconds float64
	// Independent is the same percentile with the two blocks taken
	// independent, printed beside the union bound; never above it. Equal to
	// Seconds for the mean, and when there is no decode side.
	Independent float64
	// Pooled is the optimistic figure: the union bound with the prefill
	// pods pooled into one M/G/N_p queue (Allen-Cunneen) and no imbalance,
	// in place of the worst pod's M/G/1 (with TTFTFleet.ProcessorSharing,
	// every prefill pod shared at the mean share). Never above Seconds. It describes a
	// router that holds requests and sends each to the first free pod, which
	// is what flow control does only above the prefill onset; below it, each
	// pod queues for itself and Pooled is a bound, not an estimate.
	Pooled float64
	// Floor is the lowest TTFT any number of replicas reaches at this
	// percentile: a + I_q/V_P plus decode's part at no load, which on a
	// decode line is the DecodeTerm at the line's ITL at fill 0 (the decode
	// engine's measured TTFT with no line). V_P is the unmargined one,
	// rescaled to the prompt: there is no queue at no load.
	Floor float64
	// DecodeShare is beta, the share 1 - q of the tail given to the decode
	// admission wait by the optimised split; 0 for the mean.
	DecodeShare float64
	// Parts are the means of the terms, in seconds: their sum is the mean
	// TTFT.
	Parts TTFTParts
	// PrefillUtilization is rho_w, the worst prefill pod's utilization,
	// f * rate * E[I_eff] / (N_p * V_P), at V_P rescaled to the prompt and
	// less the margin applied; it is also the probability a request waits
	// on that pod.
	PrefillUtilization float64
	// VPMarginApplied is the share of V_P taken off at this rate: 0 below
	// MarginRampStart, Prefill.VPMargin from MarginRampFull.
	VPMarginApplied float64
	// WaitScale is 1/r and WaitOffset s, in seconds: the worst pod's wait
	// tail is rho_w * exp(-r (t - s)) from s on, and rho_w before it
	// (engineWait: s is 0 except under the Lundberg bound, at cv < 1). The
	// mean wait in Parts is Pollaczek-Khinchine's (Kingman's) rho_w * theta,
	// not this tail's mean. Both are 0 with TTFTFleet.ProcessorSharing,
	// which has no first-come wait.
	WaitScale, WaitOffset float64
	// DecodeWaitProbability is the share of requests that wait for a decode
	// slot; 0 with no decode side.
	DecodeWaitProbability float64
	// PrefillKnee and DecodeKnee are the roles' knees, requests per second
	// over the fleet; DecodeKnee is 0 with no decode side.
	PrefillKnee, DecodeKnee float64
	// Onsets are the flow-control onsets; zero with flow control off.
	Onsets Onsets
	// Tier is the decode line's tier, as Decode.Tier gave it; "" with no
	// decode side.
	Tier  string
	Flags []Flag
	// Sensitivity is the figure's span over V_P and f either way, set by the
	// caller that swept them (SensitivityFleets); nil when none did.
	Sensitivity *Sensitivity
}

func (TTFT) result() {}

// HasFlag reports whether the TTFT carries f.
func (t TTFT) HasFlag(f Flag) bool {
	return slices.Contains(t.Flags, f)
}

// TTFTParts are the means of the TTFT terms, in seconds.
type TTFTParts struct {
	// EngineWait is W_engine on the worst prefill pod, rho_w * theta
	// (Pollaczek-Khinchine at Poisson arrivals, Kingman otherwise). W_router
	// is 0 below the onsets and is not priced apart above the prefill onset.
	// It is the worst pod's mean, not the fleet's: the mean TTFT (Seconds at
	// q = 0) is the mean a request on the busiest pod sees. With
	// TTFTFleet.ProcessorSharing it is the prefill pods' mean stretch over
	// the requests, the busiest pod's and the others' by their shares: the
	// time a prompt shares its pod beyond its own work.
	EngineWait float64
	// KVPullWait is the KV pull's mean stretch on the decode pod, the time
	// the pull shares the pod beyond its own work: 0 unless
	// TTFTFleet.ProcessorSharing prices it, with a decode line. The pull's
	// own work is in OwnPrefill.
	KVPullWait float64
	// OwnPrefill is a + E[I_eff]/V_P, plus the part of decode's term that
	// goes with the prompt: T_xfer on the old sum, the per-token parts of D
	// and R with a DecodeTerm. It follows the prompt's quantile.
	OwnPrefill float64
	// DecodeWait is W_decode, the admission chain's mean wait.
	DecodeWait float64
	// FirstStep is ITL(k_d), decode's first step at its closed-form fill,
	// read at the band's bottom below it, on the old sum alone: 0 with a
	// DecodeTerm that holds a D, or with no decode line.
	FirstStep float64
	// DecodeEngine is the decode engine's TTFT: with a DecodeTerm its part
	// that does not go with the prompt, D less OwnPrefill's share; with no
	// decode line the measured figure at the prompt (TTFTFleet.DecodeTTFT).
	// 0 on the old sum, or unmeasured.
	DecodeEngine float64
	// RouterHop is the fixed part of R, the hop between the router and the
	// engines; 0 with no DecodeTerm.
	RouterHop float64
}

// Onsets are the rates, in requests per second over the fleet, at which EPP
// flow control's gate starts holding requests in the router: where the mean
// endpoint score of each stage reaches 1. Zero when flow control is off, and
// Decode is zero with no decode side.
type Onsets struct {
	Prefill, Decode float64
}

func (Onsets) result() {}
