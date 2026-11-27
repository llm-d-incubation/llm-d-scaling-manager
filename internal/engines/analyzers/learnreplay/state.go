package learnreplay

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/balance"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The what-if planner's replay (docs/proposals/slo-capacity-planner.md,
// "Data per mode", Recording): replay a recording through the real
// analyzers, stop after the cycle asked for, and read what they had learned
// then. The answer is what the controller knew at that cycle.

// StateQuery names the point of a recording a what-if question is asked at.
type StateQuery struct {
	// Namespace and ModelID name the model. An empty ModelID takes the
	// recording's only model; an empty Namespace matches any, and a model
	// found in two namespaces is an error.
	Namespace, ModelID string
	// Cycle is the model's cycle to stop after, counting its saturation
	// Analyze calls from 1; 0 is its last.
	Cycle int
}

// QueryError is a StateQuery the recording cannot answer: a model it does
// not hold or names ambiguously, or a cycle past its last. It is the
// caller's question that is wrong, not the replay: every other error of
// StateAt is a recording that did not replay.
type QueryError struct{ msg string }

func (e *QueryError) Error() string { return "learnreplay: " + e.msg }

func queryErrorf(format string, args ...any) error {
	return &QueryError{msg: fmt.Sprintf(format, args...)}
}

// State is what the analyzers had learned of one model when the replay
// stopped.
type State struct {
	Namespace, ModelID string
	// Cycle is the model's cycle the replay stopped after, and Cycles how
	// many the recording holds.
	Cycle, Cycles int
	// FirstAt is the clock of the model's first cycle and At that of Cycle.
	FirstAt, At time.Time
	// Policy is the scaling policy the cycle was analyzed under; nil when
	// the recorded config was no policy.
	Policy *config.ScalingPolicy
	// ITLBandFloor is the ITL band floor the saturation analyzer adapted for
	// the cycle (saturation.SwitchITLAdaptiveBandFloor), and
	// ITLBandPolicyFloor the policy's floor it replaced; both 0 where the
	// policy's stood. Policy then carries the adapted floor, as the cycle was
	// analyzed under it.
	ITLBandFloor, ITLBandPolicyFloor float64
	// Variants is each variant the cycle priced, by name.
	Variants []VariantState
	// TPOTWindows is the router's per-request TPOT histogram of every cycle
	// up to Cycle that recorded one, oldest first, as bucket rates: the
	// input to whatif.TPOTRatioFromWindows. Empty in a recording made before
	// the recorder carried it. Each window carries the cycle's decode fill
	// (decodeFill), for R_q by fill.
	TPOTWindows []whatif.TPOTWindow
	// EngineTPOTWindows is the decode engines' per-request TPOT histogram of
	// every cycle up to Cycle that recorded one, pooled over the rows, oldest
	// first (engineTPOTWindow): the fall-back for TPOTWindows.
	EngineTPOTWindows []whatif.TPOTWindow
	// TPOTRatioCensored says R_q is read from TPOTWindows by
	// interval-censored maximum likelihood (saturation.SwitchTPOTRatioCensored,
	// whatif.TPOTRatioCensored), not at the q bucket's upper edge.
	TPOTRatioCensored bool
	// DecodeResidencePull says a P/D decode's residence holds the decode
	// engine's TTFT before its OL steps (saturation.SwitchDecodeResidencePull,
	// whatif.Decode.HeldBefore).
	DecodeResidencePull bool
	// DecodeBurstFill says a decode figure priced at a stated arrival Ca^2
	// above 1 reads its TPOT at the fill a request sees beside the
	// clustered arrivals (saturation.SwitchDecodeBurstFill,
	// whatif.Decode.ArrivalCV2).
	DecodeBurstFill bool
	// StepOutputDispersion says a decode figure priced at a stated output
	// spread (a backtest step's own, whatifapi.Knobs.OutputDispersion) reads
	// its KV per request at that spread, not the fit's
	// (saturation.SwitchBacktestStepOutputDispersion).
	StepOutputDispersion bool
	// Prefill is each prefill variant at the cycle, by name, as a TTFT
	// question reads it (state_ttft.go).
	Prefill []PrefillState
	// RouterPolicy is what the router scores, the prefix cache, the load,
	// both or neither, read from its series over every cycle up to Cycle by
	// the reader the controller reads its own cycles with
	// (balance.RouterCycleOf, balance.Tracker.ObserveRouter).
	RouterPolicy balance.RouterPolicy
	// FlowControlSeries says the router's flow-control queue series exists for
	// the model at the cycle, by S1's rule (domain.AnalyzerInput.FlowControlSeries).
	// False is no queue series, which a scrape gap reads as too.
	FlowControlSeries bool
	// Joint is the joint TTFT model's inputs at the cycle: a
	// prefill.JointWindow fed every cycle up to it, the model's prefill rows
	// and decode rows, with no settle tracker. JointPrefillPods and
	// JointDecodePods are the serving pods of each role at the cycle, and
	// JointRate its arrival rate: the fleet and the load that ran.
	Joint                             prefill.JointEstimate
	JointPrefillPods, JointDecodePods int
	JointRate                         float64
	// ArrivalRates is the model's request arrival rate, in requests per
	// second, at every cycle up to Cycle, oldest first, as the collector read
	// it (QueryModelArrivalRate): 0 where it read none, which a failed query
	// and an idle cycle both read as.
	ArrivalRates []float64
	// ServingReplicas is each variant's serving replicas at every cycle up
	// to Cycle, by variant, aligned with ArrivalRates: the cycle's
	// CurrentReplicas less its PendingReplicas, -1 at a cycle whose input
	// did not name the variant. It is the fleet that carried each rate.
	ServingReplicas map[string][]int
	// CycleTimes is the clock of every cycle up to Cycle, oldest first, one
	// per ArrivalRates entry: where the recording has holes
	// (whatifapi.WindowGapsOf).
	CycleTimes []time.Time
	// RoleStartSeconds is, for each role with a measured variant, how long a
	// new replica of its slowest measured variant took to become Ready, as
	// the saturation analyzer reported it at the cycle
	// (domain.AnalyzerResult.RoleStartSeconds); nil where it reported none.
	// RoleStartSeeded names each role whose start is a ScaledObject's
	// replicaStartSeconds seed, no replica of it measured.
	RoleStartSeconds map[string]float64
	RoleStartSeeded  map[string]bool
}

// VariantState is one variant's learned state at the cycle: everything the
// decode planner prices from.
type VariantState struct {
	Variant, Role string
	// Replicas is the variant's replica count as the cycle priced it.
	Replicas int
	// Accelerator is the variant's accelerator as the cycle's input named
	// it, "" where it named none.
	Accelerator string
	// GPUsPerReplica is the GPUs a replica requests, and TensorParallel the
	// engine's tensor parallelism as its arguments set it; 0 where the cycle
	// named none.
	GPUsPerReplica, TensorParallel int
	// Line is the ITL line the variant was priced on, zero with none; Tier
	// is where it came from (whatif.TierOLS and the rest, "" with no line)
	// and BorrowedFrom the donor of a borrowed one (namespace/variant).
	Line         itl.Model
	Tier         string
	BorrowedFrom string
	// PricingKey is the ITL key the line was fitted under, "" for a line
	// the variant did not fit, and Readings that key's windows.
	PricingKey string
	Readings   itl.KeyReadings
	// LineReadings is the readings the line was fitted from, found by
	// making the tracker's choice of fit again on Readings (itl.Refit, or
	// itl.RefitAboveBand for an above-band line); Refitted says the refit
	// gave the line back. A line it does not give back (one priced per
	// token or by shape, say) has none, and is not banded.
	LineReadings []itl.Observation
	Refitted     bool
	// Baseline is the intercept the tracker's pinned fit pins for the key,
	// and MinSettled the steady gate's MinSettledSamples: what a refit takes.
	Baseline   float64
	MinSettled int
	// OneSided is how the line was priced one-sided
	// (itl.Admission.OneSidedAboveKSat, issue ev-shindin/llm-scaling-manager#467), the zero Admission for
	// a line that was not: priced at the prior on B, B unlearned and
	// Baseline itl.DefaultBaselineSec. A refit takes it (whatif.Refit.OneSided)
	// to price the line the controller priced. analyzerOneSided is the
	// admission the analyzer last priced the key with, and analyzerRaised
	// whether that line was priced at the prior: refit takes the admission on
	// a raised line only.
	OneSided         itl.Admission
	analyzerOneSided itl.Admission
	analyzerRaised   bool
	// CapacityTokens is C, the replica's whole KV capacity
	// (whatif.CapacityTokens).
	CapacityTokens float64
	// BatchCap is the cap per engine rank the analyzer prices the variant
	// at, BatchCapSource where it came from (flags, declared, learned,
	// default, or step-budget with saturation.SwitchPlannerCapAtStepBudget
	// on; "" with none), and Ranks the engine ranks per replica.
	BatchCap       int64
	BatchCapSource string
	Ranks          int
	// MaxModelLen is the engine's context window in tokens, as its recorded
	// arguments set it (--max-model-len, SGLang's --context-length); 0 where
	// they set none (the model's own, which the recording does not hold) or
	// were not recorded. MaxModelLenUnresolved says the arguments set one the
	// parser could not read: the window is then unknown, not unset.
	MaxModelLen           int64
	MaxModelLenUnresolved bool
	// Params is the engine parameters the capacity store holds for the
	// variant, as the analyzers priced it: the vLLM release and where it was
	// read from, and the step budget, memory share and block size that
	// release's defaults resolve to where the arguments set none. Nil where
	// the store holds none.
	Params *capacity.EngineParams
	// Shape is the traffic the variant served (shapeOf).
	Shape MeasuredShape
	// Resident is the KV a running request held, measured over the same
	// window (residentOf); not read for a prefill variant.
	Resident ResidentKV
	// ShapeRange is the range of the per-cycle mean lengths over every cycle
	// with traffic up to the one asked: what the recording saw, and what a
	// stated shape is extrapolated past.
	ShapeRange ShapeRange
	// ShapeSeries is the variant's shape at every cycle with traffic up to
	// the one asked, oldest first: what a load read at some cycles is priced
	// at the shape of (the planner's load shape).
	ShapeSeries []CycleShape
	// ReadingsFrom and ReadingsTo are the first and the last cycle of the
	// LineReadings, 0 with none.
	ReadingsFrom, ReadingsTo int
	// DecodeTTFT is what the variant's rows say of a TTFT's decode side; the
	// zero value on a variant that is not decode (state_ttft.go).
	DecodeTTFT DecodeTTFTState
	// LineRows is the variant's rows of its latest cycles that have one, off
	// a ramp and on one, for the line check (state_line.go).
	LineRows []whatif.LineRow
	// FitRows is the variant's rows the line was fitted on, steady and in
	// the band (above it for an above-band line): a refitted line's over the
	// cycles its readings span, a held
	// line's over the window of its last own fit (lineSamples.fitRows). The
	// planner reads the most sequences a rank they held, and flags a figure
	// priced past it (BatchBeyondFit). Nil where they are not known: the
	// planner then reads LineRows.
	FitRows []whatif.LineRow
	// BelowBandITL is the floor below the band, in seconds: the 10th
	// percentile of the variant's latest mean ITLs read off a ramp at a fill
	// under the band's bottom (whatif.BelowBand.LeastITL). 0 with fewer than
	// itl.BelowBandMinReadings, where the line is not read below the band.
	// BelowBandFill is the fill that reading was taken at, 0 with none.
	BelowBandITL, BelowBandFill float64
	// BelowBandLeastSequences is the fewest sequences a replica ran in the
	// readings the floor was read from (whatif.BelowBand.LeastSequences): a
	// floor read at 20 sequences a replica does not stand for one (issue
	// llm-d-incubation/llm-d-scaling-manager#286). 0 with no floor, or where
	// a reading's count was not read. BelowBandByCount is the floor kept by
	// those sequences (whatif.BelowBand.ByCount), the zero Model where it was
	// not read.
	BelowBandLeastSequences float64
	BelowBandByCount        itl.Model
	// BusiestRankFill says the line, its readings, its band and its rows are
	// read at the variant's busiest engine rank, not the mean over its ranks:
	// a data-parallel MoE engine under saturation.SwitchMoEBusiestRank, whose
	// ranks step in lockstep with the busiest (issue
	// llm-d-incubation/llm-d-scaling-manager#312). BusiestRankRatio is the
	// median busiest/mean rank fill ratio of its latest rows whose busiest
	// rank filled the band's bottom or more
	// (domain.ReplicaMetrics.BusiestRankFillRatio): the planner prices the
	// mean fill at a busiest-rank fill k at k over it. 0 where none was read,
	// and always with BusiestRankFill false.
	BusiestRankFill  bool
	BusiestRankRatio float64
	// BatchFit is the ITL batch term fitted on the variant's rows up to the
	// cycle, on the residuals of Line (whatif.FitBatchTerm,
	// saturation.SwitchITLBatchTerm); its Term is what the planner prices on
	// top of Line, zero where a gate refused it. The zero value while the
	// switch is off.
	BatchFit whatif.BatchFit
	// LineAtFloor says the replay lifts a decode line to that floor
	// (saturation.SwitchITLLineAtFloor, whatif.LiftToFloor), and LineLift
	// how far, in seconds, Line's intercept was raised to pass through it; 0
	// where it was not. Baseline and LineReadings are the fit's, before the
	// lift. A resample of the line is lifted by the same rule wherever
	// LineAtFloor is set, whether or not the line itself was.
	LineAtFloor bool
	LineLift    float64
}

// MeasuredShape is the shape a variant served over the cycles it had
// traffic in.
type MeasuredShape struct {
	// InputTokens, OutputTokens and PrefixHitRate are the means.
	InputTokens, OutputTokens, PrefixHitRate float64
	// OutputDispersion is the spread of the output lengths, D =
	// E[O^2]/E[O]^2, at least 1, over the cycles whose every row read the
	// bound on E[O^2] (domain.SquaredMeanLowerBound); 0 with no such cycle:
	// no reading, which is not D = 1.
	OutputDispersion float64
	// FromCycle and ToCycle are the first and the last cycle they were
	// taken over, and Cycles how many of the cycles between had traffic;
	// all 0 when no cycle up to the one asked had any.
	FromCycle, ToCycle, Cycles int
}

// shapeCycles is how many of a variant's latest cycles with traffic its
// shape is taken over: five minutes at the default 15 s cycle.
const shapeCycles = whatif.PromptWindowCycles

// shapeSample is one cycle's shape of a variant, and its request rate.
// squared is the rows' mean bound on E[O^2], 0 unless every row read one, so
// that it and ol are one reading of the same rows.
type shapeSample struct {
	cycle                     int
	il, ol, squared, hit, req float64
}

// PlannerSwitches is the replay switches the capacity planner's replay turns
// on (StateAt), by package: the planner-only changes, off from every
// analyzer's constructor, so the controller never runs them and no golden of
// its replays moves. Each is a change to how the planner reads a fleet that
// the controller's learners do not make:
//
//   - saturation.SwitchPrefillALowLoadPerRank: the low-load test of a per
//     engine rank, for a row of an LWS group.
//   - saturation.SwitchPrefillAEngineTiming: a from the engine's prefill
//     timing where there are too few low-load minutes.
//   - saturation.SwitchPrefillQueueByCapacity: a prefill queue for V_P
//     counted by its capacity part only, so deferred waits are not a queue.
//   - saturation.SwitchPrefillFBusyMinutes: f, the prefill imbalance, the
//     median of the busiest minutes, not of every minute.
//   - saturation.SwitchPrefillFPooledMinutes: f from consecutive minutes
//     pooled to prefill.MinRequestsPerPodMinute requests a pod, where a
//     minute alone carries fewer (a fleet of long prompts), in place of no
//     f and whatif.ImbalanceFloor at every count above one.
//   - saturation.SwitchPrefillALowLoadByRate: a's low-load minutes read by
//     their rate against their prompt cluster's service time, not their own.
//   - saturation.SwitchPrefillAMedianOverhead: a's overhead the median of
//     the low-load minutes', not the least. Both raise a; on the staircase
//     backtests and the Qwen3-32B validation table no figure moves under
//     what was measured (issue llm-d-incubation/llm-d-scaling-manager#306).
//   - saturation.SwitchITLAdaptiveBandFloor: the ITL band's floor lowered to
//     the fill a low-fill fleet has.
//   - saturation.SwitchPrefillVPFloorServed: a lower-bound V_P raised to the
//     highest computed-token rate a pod was seen to sustain.
//   - saturation.SwitchPrefillVPServedUnqueued: where no source has a
//     reading, that served rate as a lower-bound V_P (prefill.NeverQueued),
//     not NoPrefillCapacity. The planner prices only the prefill knee from
//     it, as at least its rate, raised to the computed-token work the
//     fleet's prefill pods sustained (servedKneeFloor), and refuses every
//     TTFT priced from it: no figure is priced where none was, so none can
//     read under what was measured (issue
//     llm-d-incubation/llm-d-scaling-manager#369).
//   - saturation.SwitchPlannerCapAtStepBudget: the batch cap per rank held
//     to the batch the step token budget runs, ceil(budget / (1 + k)) with k
//     speculative tokens (issue llm-d-incubation/llm-d-scaling-manager#302).
//     It only ever lowers the cap: the knee and the sequences a replica is
//     priced at fall, and the line is read at the knee's lower fill, so the
//     ITL and TPOT at the knee fall with it. No committed recording runs a
//     budget under its cap times 1 + k, so no golden moves with it on.
//   - slo.SwitchTTFTHopUnqueued: the router hop of the decode term read off
//     windows whose prefill pods held no queue. The planner reads its own
//     decode term windows (state_ttft.go), so it reads the switch from here
//     even where the recording has no slo analyzer to set it on.
//   - slo.SwitchTTFTDecodeSpeculatingHeld: on a decode fleet that
//     speculates (MTP), whose every row D's fit refuses, the decode
//     engine's measured TTFT of a quiet speculating row is held for D, its
//     ITL read per accepted token, in place of T_xfer + ITL. It only adds a
//     held D where the term was omitted, so every TTFT figure it moves
//     rises. No committed recording has a decode row that speculates: on the
//     cross-validation's 66 public runs no figure moved. On a P/D recording
//     with MTP on every decode pod, the held figure, fitted on the first half,
//     read the decode engine's measured TTFT of the second within 20% on
//     most five-minute steps, where the old sum read a few percent of it.
//   - saturation.SwitchBacktestStepOutputDispersion: a backtest step priced
//     at its own output spread, not the fit's (issue
//     llm-d-incubation/llm-d-scaling-manager#274). Only a backtest states
//     one (whatifapi.Knobs.OutputDispersion), so no other answer moves. On
//     the 0.6B staircase's 1000/1000 A phases, fitted on warm-ups that end
//     on short outputs (D 1.26), the predicted decode fill falls from 1.87
//     to 1.59 times what the pods held, and the Qwen3-32B A-off phase's
//     from 1.19 to 1.05. On
//     the 8000/200 B and C phases, where the prompt holds most of the KV,
//     the first B-off phase's rises from 1.24 to 1.37, as two steps it
//     refused are now priced. No backtest figure newly reads more than 20%
//     under what was measured, in TTFT or TPOT. With the hold and the lift
//     (both off, below) it deepens two Qwen3-32B A-off TTFT steps already
//     under (mean 0.69 to 0.46 of measured): look at those again before
//     the hold and the lift go on.
//
// slo.SwitchTTFTHopWindowMeans, planner-only, reads the router hop on its
// windows' means: the median of at least prefill.MinHopMeanWindows windows,
// held at their median prompt (prefill.DecodeTermCycle.HopWindowMeans). It is
// not listed: on the public cross-validation it moved four TTFT figures of
// the Qwen3-32B staircase's C phases from at or above 0.80 of measured to
// just under it (0.798 to 0.799). It goes on once no public figure newly reads under what was measured.
//
// saturation.SwitchITLHoldSteadySlope is planner-only too but not listed:
// the line it holds is right at a given fill, and the planner then reads
// TTFT and TPOT under what was measured, because it predicts less decode KV
// fill than the pods held. Nor is saturation.SwitchITLLineAtFloor, which
// fixes that fill (issue llm-d-incubation/llm-d-scaling-manager#274) and
// only helps with the hold: alone it lifts the steeper line the hold
// replaces, and reads TPOT higher still. With both, the planner's mean TTFT
// and TPOT on the public staircase's B phases (8000/200, Poisson) come
// within 13% of measured (TPOT 0.80 on the second warm-up's B-onset phase),
// but its p90 TTFT on the B-off and B-on phases still reads 15% to 23% under
// and its p90 TPOT up to 22% under there (32% on the first warm-up's
// B-onset, 39% on the bursty C phases), so both stay off.
//
// saturation.SwitchDecodeResidencePull, planner-only, holds a P/D decode's
// KV from the pull (issue llm-d-incubation/llm-d-scaling-manager#301): alone
// it adds no step read under measured, but it raises the over-reads; with
// the hold and the lift it brings most of the bursty phases' under-reads
// back. It goes on with those two or not at all, so it stays off with them.
// saturation.SwitchTPOTRatioCensored, planner-only, reads R_q from the
// bucket counts, not the bucket's upper edge (issue
// llm-d-incubation/llm-d-scaling-manager#279): it reads a p90 TPOT lower
// than the edge does, and on the Qwen3-32B recordings lower than the
// measured p90 interpolated in its bucket, so it is off until a backtest
// shows it at or above what was measured.
//
// saturation.SwitchDecodeBurstFill, planner-only, reads a decode's TPOT at
// the fill a request sees beside clustered arrivals, from a stated
// --arrival-cv2 (issue llm-d-incubation/llm-d-scaling-manager#291 (b)): at
// Poisson, and with no Ca^2 stated, it moves no figure. It brings the
// bursty phases' mean TPOT back to what was measured, but it is off: with
// it, the hold, the lift and the residence all on, other figures still read
// under what was measured.
//
// saturation.SwitchPlannerCapAtStepBudget is planner-only and not listed
// either: no recording holds an engine whose step budget runs fewer
// sequences than its cap (MTP, wide EP), so there is no backtest to turn it
// on with (issue llm-d-incubation/llm-d-scaling-manager#302).
//
// slo.SwitchTTFTPromptSupport is planner-only too but not listed (issue
// llm-d-incubation/llm-d-scaling-manager#304): it holds the prompt at the
// longest its computed-token histogram allows, keeping the measured mean,
// and a bound at whatif.MaxPSPromptSupport or under prices processor sharing
// alone at Poisson arrivals. On the Qwen3-32B and 0.6B staircase backtests,
// each step priced at its own histogram's cv and bound, 14 TTFT figures that
// read at or above measured read under it, the lowest at 0.92 of measured.
// The hold moves them, not processor sharing (the same 14 go under with the
// bound kept from clearing it), and each lands in the same router histogram
// bucket as its measured figure: the bucket-wide cv was standing in for a
// spread of the service time that no term prices. It stays off until one
// does (docs/proposals/slo-capacity-planner-ttft.md).
//
// saturation.SwitchPrefillVPOccupancyPlateau is planner-only and not listed
// (issue llm-d-incubation/llm-d-scaling-manager#368): it reads V_P without a
// queue, from the plateau of the served computed tokens against the prefill
// occupancy. On the public recordings its level reads 0.96 to 1.08 of the
// V_P learned from a queue, and with it on no backtest figure moves under
// what was measured. But where V_P was refused it mostly gives a lower bound
// whose fleets still lack a (NoFixedLatency), and the only TTFT figures it
// newly prices, on the 0.6B staircase warm-ups at a 75% split, read 1.5 to
// 1500 times what was measured: they sit just under a knee priced from the
// lower bound. One warm-up's self-check falls from weak to inconsistent with
// it. It stays off until TTFT under a lower-bound knee is priced.
//
// saturation.SwitchPrefillVPPromptCoverage is planner-only and not listed
// (issue llm-d-incubation/llm-d-scaling-manager#405): it keeps the prompts
// the minutes V_P rests on read, and prices no TTFT from a V_P at a mean
// prompt more than whatif.MaxVPShapeRatio outside them
// (whatif.VPPromptNotCovered). It goes on once the public cross-validation
// shows no figure newly under what was measured and no worse self-check.
//
// saturation.SwitchPrefillVPLoneRate is planner-only and not listed (issue
// llm-d-incubation/llm-d-scaling-manager#406): it reads V_P from light load,
// one request's computed tokens over its prefill time at the prompt priced,
// where no queue gives one and the served peak agrees with it. It goes on
// once the public cross-validation shows no figure newly under what was
// measured, no worse self-check, and the prefill knee within 15% of the
// measured one on the staircases whose knee was measured.
//
// saturation.SwitchPrefillVPServiceLine is planner-only and not listed
// (issues llm-d-incubation/llm-d-scaling-manager#368, #416): it reads V_P
// from the prefill service line, the engines' prefill time against their
// computed tokens a request, s0 kept in the capacity, where no queue gives
// one. On the public recordings whose line is read beside a V_P from a
// queue it reads 0.85 to 1.05 of it (0.6B and Qwen3-32B warm-ups), but on
// the 0.6B warm-up's 50% split, fitted on the light half, it reads under
// the busy V_P, three TTFT figures it newly prices read 0.67 to 0.79 of
// what was measured, and at-replicas' self-check falls from not run to
// inconsistent on two warm-ups (the fleet called overloaded where it
// served). It goes on once the public cross-validation shows no figure
// newly under what was measured and no worse self-check.
//
// saturation.SwitchPrefillHitAtCount is planner-only and not listed (issue
// llm-d-incubation/llm-d-scaling-manager#384): it prices the prefix hit at a
// prefill count other than the fit's, the floor hit x N'/N at fewer pods and
// the fit's hit held at more. It goes on once the public cross-validation
// shows no figure newly under what was measured.
//
// saturation.SwitchPrefillFLoadCurve is planner-only and not listed (issue
// llm-d-incubation/llm-d-scaling-manager#482): it fits f against the load
// per pod at each prefill count, f(u) = f_inf + delta/u, and the planner
// prices f at a rate as the higher of its f and the curve's. It passed one
// pre-registered offline test on the lab's Qwen3-32B recordings (a later
// run at the same count, held out); it goes on once the public
// cross-validation shows no figure newly under what was measured.
//
// saturation.SwitchPrefillVPWindowBest is planner-only and not listed: it
// keeps the peak under a queue read anywhere in the recording, not only in
// the last 6 hours, flagged with its age (VPFromEarlierInWindow). No public
// figure moves with it, but on a production P/D fleet whose prefill pods
// queued only at its busiest hours it prices the rest of the day's TTFT from
// that lower bound, and the TTFT read many times what was measured. It is
// on only with --experimental-ttft.
//
// slo.SwitchTTFTTierLoad and slo.SwitchTTFTTierKVWait are planner-only and
// not listed (issue llm-d-incubation/llm-d-scaling-manager#386). Their
// first real test is a private production P/D recording whose prefill pods
// serve a material share of the prompt from a host tier and a little from a
// peer tier, both priced from the recorded offload load with no flag. Its
// default TTFT is refused (no V_P learned), so it was scored with
// --experimental-ttft, the two switches alone changed. No figure newly reads
// under 0.8 of measured with either, on a time split, a load split or the
// busiest hour of the test, and with the KV wait the median mean TTFT of
// the 75% time split comes closer to measured. But at the 50%
// split, whose figures rest on a lower-bound V_P and read high, both raise
// the median error of the mean and p90 TTFT and take figures from within
// 20% of measured to more than 20% over, which the issue's rule forbids;
// the load split's mean does the same. The engine's KV wait is several times
// the transfer's own seconds there, so the tier load alone moves little.
// They go on once a recording with a tier and a V_P learned under a queue
// passes that rule.
//
// saturation.SwitchMoEBusiestRank is not listed either: it reads a
// data-parallel MoE engine's line at its busiest rank (issue
// llm-d-incubation/llm-d-scaling-manager#312), and its only evidence is
// synthetic. No recording in the goldens is of such an engine, so nothing
// shows it does not read a figure under what was measured; a replay turns it
// on by name.
//
// saturation.SwitchITLBatchTerm is not listed either: the ITL term past the
// roofline (issue llm-d-incubation/llm-d-scaling-manager#309) is fitted on
// the residuals of a variant's own line, and no committed recording
// identifies it. On the Qwen3-32B warm-up alone the fitted slope is 0.003 ms
// per sequence and removes 0.4% of the residuals' error, which the gates
// refuse; it takes the warm-up and the 1k-prompt A-off phase together (n_roof
// 120 per rank, 0.034 ms per sequence) to read it, so a recording of one
// shape cannot. A replay turns it on by name.
//
// Two controller switches, on from their constructor, are turned off here:
//
//   - saturation.SwitchITLBaselineFromFloor: a decode line with no full fit
//     pinned through the floor read below the band in the last 30 minutes,
//     not the 6 ms default B (issue
//     llm-d-incubation/llm-d-scaling-manager#169 item 1, on from the
//     constructor since #318, after a GPU check of the controller's decode
//     asks). The planner keeps such a line on the default and refuses it
//     (PinnedDefaultB), as v0.12.2 did; --assume-itl-b floor prices it
//     through the floor's B and flags every figure on it AssumedITLB. On,
//     the planner priced 14 backtest TPOT steps it had refused, on seven
//     recordings, more than 20% under what was measured (ttft-three-pods at
//     0.33 to 0.46 of measured), with no flag saying B was not measured, and
//     judged pd-restart's decode under-provisioned where it had not judged
//     it (issue llm-d-incubation/llm-d-scaling-manager#318).
//   - saturation.SwitchITLFloorBaselineHeld: a decode line with no full fit
//     keeps the B it last read from its floor below the band after the
//     floor's readings are 30 minutes old (issue
//     llm-d-incubation/llm-d-scaling-manager#333). On from the controller's
//     constructor, for a fleet held in the band. It holds nothing with the
//     floor off; it is off here in its own right too: held, it priced
//     decode on hours of the staircase and three-pods recordings the planner
//     refused, and no check yet says those figures do not read under what
//     was measured.
func PlannerSwitches() map[string]map[string]bool {
	return map[string]map[string]bool{PackageSaturation: {
		saturation.SwitchPrefillALowLoadPerRank:       true,
		saturation.SwitchPrefillAEngineTiming:         true,
		saturation.SwitchPrefillQueueByCapacity:       true,
		saturation.SwitchPrefillFBusyMinutes:          true,
		saturation.SwitchPrefillFPooledMinutes:        true,
		saturation.SwitchPrefillALowLoadByRate:        true,
		saturation.SwitchPrefillAMedianOverhead:       true,
		saturation.SwitchITLAdaptiveBandFloor:         true,
		saturation.SwitchPrefillVPFloorServed:         true,
		saturation.SwitchPrefillVPServedUnqueued:      true,
		saturation.SwitchPlannerCapAtStepBudget:       true,
		saturation.SwitchBacktestStepOutputDispersion: true,
		saturation.SwitchITLBaselineFromFloor:         false,
		saturation.SwitchITLFloorBaselineHeld:         false,
	}, PackageSLO: {
		slo.SwitchTTFTHopUnqueued:           true,
		slo.SwitchTTFTDecodeSpeculatingHeld: true,
	}}
}

// withPlannerSwitches is sw with every switch of PlannerSwitches it does not
// set, so a caller's own setting of one wins (a test turns one off).
func withPlannerSwitches(sw map[string]map[string]bool) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(sw)+1)
	for pkg, set := range sw {
		out[pkg] = maps.Clone(set)
	}
	for pkg, set := range PlannerSwitches() {
		if out[pkg] == nil {
			out[pkg] = map[string]bool{}
		}
		for name, on := range set {
			if _, ok := out[pkg][name]; !ok {
				out[pkg][name] = on
			}
		}
	}
	return out
}

// StateAt replays recording through the analyzers (in Normal mode, whatever
// opts.Mode says), with the planner's switches on where opts does not set
// them (PlannerSwitches), and returns the state of the model q names after q's
// cycle. A *QueryError is a model the recording does not hold or names
// ambiguously, or a cycle past its last; any other error is a recording
// that does not replay.
//
// The model and its cycle are resolved first, from the recording's
// saturation Analyze events alone, so the last cycle is a cycle number like
// any other: the replay stops there and the state is read once.
func StateAt(ctx context.Context, recording []byte, opts Options, q StateQuery) (*State, error) {
	return StateFrom(ctx, BytesSource(recording), opts, q)
}

// StateFrom is StateAt of the recording src opens, read in two passes and
// never held: one to resolve the model and its cycles, one to replay to the
// cycle.
func StateFrom(ctx context.Context, src Source, opts Options, q StateQuery) (*State, error) {
	st, _, err := StatesFrom(ctx, src, opts, q, nil)
	return st, err
}

// StatesFrom is StateFrom, and in the same replay the state at each of the
// earlier cycles also names, given the cycle q answers at: what StateFrom
// would answer at each, from one pass. A cycle at or past q's, or below 1,
// is not read; a nil also reads none.
func StatesFrom(ctx context.Context, src Source, opts Options, q StateQuery, also func(cycle int) []int) (*State, map[int]*State, error) {
	if q.Cycle < 0 {
		return nil, nil, queryErrorf("cycle %d, want 1 or more, or 0 for the last", q.Cycle)
	}
	ns, model, total, err := resolveModel(src, q)
	if err != nil {
		return nil, nil, err
	}
	cycle := q.Cycle
	if cycle == 0 {
		cycle = total
	}
	if cycle > total {
		return nil, nil, queryErrorf("cycle %d asked for, and the recording holds %d of %s/%s", cycle, total, ns, model)
	}
	early := map[int]bool{}
	if also != nil {
		for _, c := range also(cycle) {
			if c >= 1 && c < cycle {
				early[c] = true
			}
		}
	}
	// snap is a state read before the replay's end, with what the closing
	// of a state reads as of its cycle.
	type snap struct {
		st                       *State
		windows, engine, arrived int
		rows, fit                map[string][]whatif.LineRow
		below                    map[string]belowBand
		ratios                   map[string]float64
	}
	snaps := map[int]*snap{}
	opts.Mode = Normal
	opts.Switches = withPlannerSwitches(opts.Switches)
	opts.discardRecording = true
	opts.sloOptional = true
	var (
		st       *State
		stateErr error
		n        int
		firstAt  time.Time
		windows  []whatif.TPOTWindow
		engine   []whatif.TPOTWindow
		shapes   = map[string][]shapeSample{}
		resident = map[string][]residentSample{}
		ttft     = newTTFTSamples(opts.Switches[PackageSLO][slo.SwitchTTFTHopUnqueued])
		lines    = newLineSamples()
		batch    = newBatchSamples(opts.Switches[PackageSaturation][saturation.SwitchITLBatchTerm])
		cycleAt  = map[time.Time]int{}
		arrivals []float64
		serving  = map[string][]int{}
		times    []time.Time
		ranges   = map[string]ShapeRange{}
		series   = map[string][]CycleShape{}
	)
	ttft.promptSupport = opts.Switches[PackageSLO][slo.SwitchTTFTPromptSupport]
	ttft.hopWindowMeans = opts.Switches[PackageSLO][slo.SwitchTTFTHopWindowMeans]
	// Read once: the switches are set before the replay starts.
	lineAtFloor := opts.Switches[PackageSaturation][saturation.SwitchITLLineAtFloor]
	censored := opts.Switches[PackageSaturation][saturation.SwitchTPOTRatioCensored]
	residence := opts.Switches[PackageSaturation][saturation.SwitchDecodeResidencePull]
	burst := opts.Switches[PackageSaturation][saturation.SwitchDecodeBurstFill]
	stepSpread := opts.Switches[PackageSaturation][saturation.SwitchBacktestStepOutputDispersion]
	ttft.prefixAttention = opts.Switches[PackageSLO][slo.SwitchTTFTPrefixAttention]
	ttft.computedHit = opts.Switches[PackageSLO][slo.SwitchTTFTComputedHit]
	ttft.tierLoad = opts.Switches[PackageSLO][slo.SwitchTTFTTierLoad]
	ttft.tierKVWait = opts.Switches[PackageSLO][slo.SwitchTTFTTierKVWait]
	ttft.psByEngine = opts.Switches[PackageSLO][slo.SwitchTTFTPSByEngine]
	ttft.pullWork = opts.Switches[PackageSLO][slo.SwitchTTFTPullWork]
	ttft.servedOn = opts.Switches[PackageSaturation][saturation.SwitchPrefillVPServedUnqueued]
	ttft.vpAtServed = opts.Switches[PackageSaturation][saturation.SwitchPrefillVPAtServedWork]
	ttft.hitAtCount = opts.Switches[PackageSaturation][saturation.SwitchPrefillHitAtCount]
	ttft.cachedTokenCost = opts.Switches[PackageSLO][slo.SwitchTTFTCachedTokenCost]
	ttft.specHeld = opts.Switches[PackageSLO][slo.SwitchTTFTDecodeSpeculatingHeld]
	// SwitchITLPerToken is on from the saturation analyzer's constructor: a
	// replay that does not name it fits the line per accepted token.
	perToken, named := opts.Switches[PackageSaturation][saturation.SwitchITLPerToken]
	ttft.itlPerToken = perToken || !named
	opts.stop = func(r *replayer, ev learnrec.RawEvent) bool {
		if ev.Kind != saturation.KindAnalyze {
			return false
		}
		// The replay applied the event before asking: its data decoded in
		// full there, so the input alone is read here.
		raw, _ := learnrec.Raw(ev.Data).Member("Input")
		in, err := decodeInput(raw)
		if err != nil {
			stateErr = err
			return true
		}
		if in.Namespace != ns || in.ModelID != model {
			return false
		}
		// The cycle's policy as the saturation analyzer read it: with the
		// band floor it adapted (saturation.SwitchITLAdaptiveBandFloor), so
		// every reader of the band below reads the line's.
		in.Config = r.analyzedPolicy(in)
		n++
		if n == 1 {
			firstAt = ev.Now
		}
		cycleAt[ev.Now] = n
		// The variants whose line the analyzer reads at the busiest rank
		// (saturation.SwitchMoEBusiestRank): every fill below is read there.
		busiest := func(variant string) bool {
			rec := r.store.Get(in.Namespace, in.ModelID, variant)
			return rec != nil && r.sat.BusiestRankFillFor(rec.EngineParams)
		}
		fill, fillOK := decodeFill(in.ReplicaMetrics, rolesOf(in), busiest)
		arrivals = append(arrivals, in.ArrivalRate)
		noteServing(serving, in.VariantStates, len(arrivals))
		times = append(times, ev.Now)
		if w, ok := tpotWindow(in.RouterLatency); ok {
			windows = append(windows, withFill(w, fill, fillOK))
		}
		if w, ok := engineTPOTWindow(in.ReplicaMetrics, rolesOf(in)); ok {
			engine = append(engine, withFill(w, fill, fillOK))
		}
		noteShapes(shapes, in.ReplicaMetrics, n)
		noteRanges(ranges, shapes, n)
		noteSeries(series, shapes, n)
		noteResident(resident, in, n)
		ttft.note(in, n, ev.Now)
		args := func(variant string) string {
			if rec := r.store.Get(in.Namespace, in.ModelID, variant); rec != nil {
				return rec.EngineParams.ArgsTag()
			}
			return ""
		}
		sglang := func(variant string) bool {
			rec := r.store.Get(in.Namespace, in.ModelID, variant)
			return rec != nil && rec.EngineParams != nil && rec.EngineParams.Engine == inferenceengine.EngineSGLang
		}
		ttft.noteJoint(in, ev.Now, args, sglang)
		lines.note(in, n, ev.Now, busiest)
		for _, rep := range r.sat.LearnerReports()[in.Namespace+"|"+in.ModelID] {
			if rep.Learner != learn.ITL {
				continue
			}
			if t := saturation.ITLTier(rep.Source, false); t == whatif.TierOLS || t == whatif.TierPinnedB {
				lines.noteOwnFit(rep.Variant, ev.Now)
			}
		}
		batch.note(in)
		ttft.noteTerm(in, ev.Now, args)
		if n == cycle || early[n] {
			s := r.stateOf(in, n, ev.Now, shapes)
			for i := range s.Variants {
				v := &s.Variants[i]
				v.ShapeRange = ranges[v.Variant]
				v.ShapeSeries = slices.Clone(series[v.Variant])
				if domain.CanonicalRole(v.Role) != domain.RolePrefill {
					v.Resident = residentOf(resident[v.Variant], v.Shape)
				}
			}
			// The window's first cycle, for the age of a V_P read before the
			// samples kept (PrefillState.WindowFrom); set again below.
			s.FirstAt = firstAt
			r.fillTTFT(s, in, ttft)
			batch.fit(s)
			// The replay applied the cycle's Analyze before asking: its
			// result is the cycle's.
			if res, ok := r.Result(domain.SaturationAnalyzerName, in.Namespace, in.ModelID); ok {
				s.RoleStartSeconds, s.RoleStartSeeded = maps.Clone(res.RoleStartSeconds), maps.Clone(res.RoleStartSeeded)
			}
			if n == cycle {
				st = s
				return true
			}
			sn := &snap{st: s, windows: len(windows), engine: len(engine), arrived: len(arrivals),
				rows: map[string][]whatif.LineRow{}, fit: map[string][]whatif.LineRow{}, below: map[string]belowBand{}, ratios: map[string]float64{}}
			for _, v := range s.Variants {
				sn.rows[v.Variant] = slices.Clone(lines.rows(v.Variant))
				v.ReadingsFrom, v.ReadingsTo = cyclesSpanned(v.LineReadings, cycleAt)
				sn.fit[v.Variant] = lines.fitRows(v)
				itlSec, fill := lines.leastBelowBand(v.Variant)
				least, byCount := lines.belowBandLeastSequences(v.Variant)
				sn.below[v.Variant] = belowBand{itlSec: itlSec, fill: fill, least: least, byCount: byCount}
				sn.ratios[v.Variant] = lines.busiestRatio(v.Variant)
			}
			snaps[n] = sn
		}
		return false
	}
	// resolveModel read every line: the recording reads.
	if _, err := replayFrom(ctx, src, opts); err != nil {
		return nil, nil, err
	}
	if stateErr != nil {
		return nil, nil, fmt.Errorf("learnreplay: %w", stateErr)
	}
	if st == nil {
		return nil, nil, fmt.Errorf("learnreplay: the replay ended after %d of the %d cycles of %s/%s", n, cycle, ns, model)
	}
	st.FirstAt, st.TPOTWindows, st.EngineTPOTWindows, st.Cycles = firstAt, windows, engine, total
	st.ArrivalRates, st.ServingReplicas = arrivals, servingUpTo(serving, len(arrivals))
	st.CycleTimes, st.TPOTRatioCensored, st.DecodeResidencePull, st.DecodeBurstFill = times, censored, residence, burst
	st.StepOutputDispersion = stepSpread
	for i := range st.Variants {
		st.Variants[i].ReadingsFrom, st.Variants[i].ReadingsTo = cyclesSpanned(st.Variants[i].LineReadings, cycleAt)
		st.Variants[i].LineRows = lines.rows(st.Variants[i].Variant)
		st.Variants[i].FitRows = lines.fitRows(st.Variants[i])
		st.Variants[i].BelowBandITL, st.Variants[i].BelowBandFill = lines.leastBelowBand(st.Variants[i].Variant)
		st.Variants[i].BelowBandLeastSequences, st.Variants[i].BelowBandByCount = lines.belowBandLeastSequences(st.Variants[i].Variant)
		st.Variants[i].BusiestRankRatio = lines.busiestRatio(st.Variants[i].Variant)
		st.Variants[i].liftToFloor(lineAtFloor && st.disaggregated())
	}
	var out map[int]*State
	if len(snaps) > 0 {
		out = make(map[int]*State, len(snaps))
	}
	for c, sn := range snaps {
		s := sn.st
		s.FirstAt, s.Cycles = firstAt, total
		s.TPOTWindows, s.EngineTPOTWindows = slices.Clone(windows[:sn.windows]), slices.Clone(engine[:sn.engine])
		s.ArrivalRates = slices.Clone(arrivals[:sn.arrived])
		s.ServingReplicas = servingUpTo(serving, sn.arrived)
		s.TPOTRatioCensored, s.DecodeResidencePull, s.DecodeBurstFill = censored, residence, burst
		s.StepOutputDispersion = stepSpread
		s.CycleTimes = slices.Clone(times[:sn.arrived])
		for i := range s.Variants {
			s.Variants[i].ReadingsFrom, s.Variants[i].ReadingsTo = cyclesSpanned(s.Variants[i].LineReadings, cycleAt)
			s.Variants[i].LineRows = sn.rows[s.Variants[i].Variant]
			s.Variants[i].FitRows = sn.fit[s.Variants[i].Variant]
			b := sn.below[s.Variants[i].Variant]
			s.Variants[i].BelowBandITL, s.Variants[i].BelowBandFill = b.itlSec, b.fill
			s.Variants[i].BelowBandLeastSequences, s.Variants[i].BelowBandByCount = b.least, b.byCount
			s.Variants[i].BusiestRankRatio = sn.ratios[s.Variants[i].Variant]
			s.Variants[i].liftToFloor(lineAtFloor && s.disaggregated())
		}
		out[c] = s
	}
	return st, out, nil
}

// resolveModel is the model q names and how many saturation cycles the
// recording holds of it, read from its Analyze events without replaying
// them. It reads every line, so a recording that does not read is
// learnrec.Read's error.
func resolveModel(src Source, q StateQuery) (ns, model string, cycles int, err error) {
	counts := map[[2]string]int{}
	err = scan(src, func(ev learnrec.RawEvent) (bool, error) {
		if ev.Kind != saturation.KindAnalyze {
			return true, nil
		}
		id, err := analyzedModel(ev.Data)
		if err != nil {
			return false, fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
		if q.ModelID != "" && id.ModelID != q.ModelID || q.Namespace != "" && id.Namespace != q.Namespace {
			return true, nil
		}
		counts[[2]string{id.Namespace, id.ModelID}]++
		return true, nil
	})
	if err != nil {
		return "", "", 0, err
	}
	keys := make([]string, 0, len(counts))
	for k, c := range counts {
		ns, model, cycles = k[0], k[1], c
		keys = append(keys, k[0]+"/"+k[1])
	}
	slices.Sort(keys)
	switch {
	case len(keys) == 0 && q.ModelID == "":
		return "", "", 0, queryErrorf("the recording holds no saturation cycle")
	case len(keys) == 0:
		return "", "", 0, queryErrorf("the recording holds no cycle of model %q", q.ModelID)
	case len(keys) > 1:
		return "", "", 0, queryErrorf("the recording holds %s; name one with its namespace and model", strings.Join(keys, ", "))
	}
	return ns, model, cycles, nil
}

// analyzedModel is the namespace and the model of a saturation Analyze
// event's input: two fields of it are all a scan reads, where learnrec's own
// decoder would hold every field to the type (the replay decodes the event in
// full). The two are found in the data, which a recording read has checked is
// JSON, without decoding the rest (learnrec.Raw.Member); data of another
// shape is read by encoding/json, as it was, for its error.
func analyzedModel(data json.RawMessage) (struct{ Namespace, ModelID string }, error) {
	var id struct{ Namespace, ModelID string }
	if in, ok := learnrec.Raw(data).Member("Input"); ok && len(in) > 0 && in[0] == '{' {
		ns, nsOK := in.Member("Namespace")
		model, modelOK := in.Member("ModelID")
		if nsOK && modelOK && isString(ns) && isString(model) &&
			json.Unmarshal(ns, &id.Namespace) == nil && json.Unmarshal(model, &id.ModelID) == nil {
			return id, nil
		}
	}
	var d struct {
		Input struct{ Namespace, ModelID string }
	}
	err := json.Unmarshal(data, &d)
	return d.Input, err
}

// isString reports whether the JSON value raw is a string.
func isString(raw learnrec.Raw) bool { return len(raw) > 0 && raw[0] == '"' }

// cyclesSpanned is the first and the last cycle readings were taken at: a
// reading is stamped with the clock of the Analyze that offered it.
func cyclesSpanned(readings []itl.Observation, cycleAt map[time.Time]int) (from, to int) {
	for _, o := range readings {
		c, ok := cycleAt[o.Timestamp]
		if !ok {
			continue
		}
		if from == 0 || c < from {
			from = c
		}
		to = max(to, c)
	}
	return from, to
}

// noteShapes keeps, per variant, the shape of each of its latest
// shapeCycles cycles with traffic: a row serving requests, or generating
// tokens, at the cycle.
func noteShapes(shapes map[string][]shapeSample, rows []domain.ReplicaMetrics, cycle int) {
	type acc struct {
		weight, il, ol, squared, hit, req float64
		unread                            bool
	}
	byVariant := map[string]*acc{}
	traffic := map[string]bool{}
	rate := func(rm domain.ReplicaMetrics) float64 {
		if rm.RequestRateKnown && rm.RequestRate > 0 {
			return rm.RequestRate
		}
		return 0
	}
	for _, rm := range rows {
		a := byVariant[rm.VariantName]
		if a == nil {
			a = &acc{}
			byVariant[rm.VariantName] = a
		}
		if rm.RunningRequests > 0 || rm.RequestRate > 0 || rm.GenerationTokenRate > 0 {
			traffic[rm.VariantName] = true
		}
		a.req += rate(rm)
	}
	// Rows weighted by their request rate, as the analyzers weight them, and
	// alike when none read one: a busy row of short requests and a quiet one
	// of long ones are one mean only by what each served.
	for _, rm := range rows {
		a := byVariant[rm.VariantName]
		w := 1.0
		if a.req > 0 {
			w = rate(rm)
		}
		if !(rm.AvgOutputTokens > 0) || !(w > 0) {
			continue
		}
		a.weight += w
		a.il += w * rm.AvgInputTokens
		a.ol += w * rm.AvgOutputTokens
		a.hit += w * rm.PrefixCacheHitRate
		a.squared += w * rm.AvgOutputTokensSquared
		a.unread = a.unread || !rm.AvgOutputTokensSquaredKnown || !(rm.AvgOutputTokensSquared > 0)
	}
	for v, a := range byVariant {
		if !traffic[v] || a.weight == 0 {
			continue
		}
		s := shapes[v]
		sample := shapeSample{cycle: cycle, il: a.il / a.weight, ol: a.ol / a.weight, hit: a.hit / a.weight, req: a.req}
		if !a.unread {
			sample.squared = a.squared / a.weight
		}
		s = append(s, sample)
		if len(s) > shapeCycles {
			s = s[len(s)-shapeCycles:]
		}
		shapes[v] = s
	}
}

// shapeOf is a variant's measured shape: the mean of its latest shapeCycles
// cycles with traffic, each weighted by its request rate (equally when no
// cycle read one). The rows' own lengths are means over a window already;
// weighting by rate keeps a cycle of a light trickle from counting as much
// as one at load, and taking cycles with traffic only means a recording
// that ends idle is priced at the traffic it last served rather than at
// none.
//
// The spread is taken over the cycles that read it, with their own weights
// and their own mean, so a cycle with a row that read none (a rollout) costs
// the window that cycle and not its reading. Across a change of shape the
// window holds both, and their difference reads as spread: D reads high, and
// the question asks for more replicas, the side the planner may err on.
// Saturation prices D 1 there (resolvePricing); this mean is a mix of the two
// shapes too.
func shapeOf(samples []shapeSample) MeasuredShape {
	if len(samples) == 0 {
		return MeasuredShape{}
	}
	var total float64
	for _, s := range samples {
		total += s.req
	}
	var out MeasuredShape
	var readWeight, readMean, readSquared float64
	for _, s := range samples {
		w := 1 / float64(len(samples))
		if total > 0 {
			w = s.req / total
		}
		out.InputTokens += w * s.il
		out.OutputTokens += w * s.ol
		out.PrefixHitRate += w * s.hit
		if s.squared > 0 {
			readWeight += w
			readMean += w * s.ol
			readSquared += w * s.squared
		}
	}
	if readWeight > 0 {
		out.OutputDispersion = shape.Dispersion(readMean/readWeight, readSquared/readWeight)
	}
	out.FromCycle, out.ToCycle, out.Cycles = samples[0].cycle, samples[len(samples)-1].cycle, len(samples)
	return out
}

// tpotWindow is the router's per-request TPOT histogram of one cycle, as the
// what-if library reads it: bucket rates for counts, which give the same
// quantile and mean.
// It is whatif.TPOTWindowOf, the window the slo analyzer's default TPOT
// percentile reads from its own cycles.
func tpotWindow(rl *domain.RouterLatency) (whatif.TPOTWindow, bool) {
	return whatif.TPOTWindowOf(rl)
}

// analyzedPolicy is in's recorded config, or, where the saturation analyzer
// adapted the ITL band floor for the cycle, a copy of its policy with that
// floor (saturation.SwitchITLAdaptiveBandFloor).
func (r *replayer) analyzedPolicy(in domain.AnalyzerInput) domain.AnalyzerConfig {
	floor, _, ok := r.sat.ITLBandFloor(in.Namespace, in.ModelID)
	if !ok {
		return in.Config
	}
	var p config.ScalingPolicy
	if rec, isPolicy := in.Config.(*config.ScalingPolicy); isPolicy && rec != nil {
		p = *rec
	}
	_, hi := p.ITLBand()
	p.AnalyzerTuning.ITLBand.MinObservableK, p.AnalyzerTuning.ITLBand.MaxObservableK = floor, hi
	return &p
}

// stateOf reads the analyzers' learned state of the model in after its
// saturation Analyze.
func (r *replayer) stateOf(in domain.AnalyzerInput, cycle int, now time.Time, shapes map[string][]shapeSample) *State {
	st := &State{Namespace: in.Namespace, ModelID: in.ModelID, Cycle: cycle, At: now}
	st.Policy, _ = in.Config.(*config.ScalingPolicy)
	if floor, policy, ok := r.sat.ITLBandFloor(in.Namespace, in.ModelID); ok {
		st.ITLBandFloor, st.ITLBandPolicyFloor = floor, policy
	}
	res := r.results[resultKey(domain.SaturationAnalyzerName, in.Namespace, in.ModelID)]
	if res == nil {
		return st
	}
	reports := map[string]learn.Report{}
	for _, rep := range r.sat.LearnerReports()[in.Namespace+"|"+in.ModelID] {
		if rep.Learner == learn.ITL {
			reports[rep.Variant] = rep
		}
	}
	learning := st.Policy != nil && st.Policy.EnableBatchCapLearning
	minSettled := st.Policy.ITLSteadiness().MinSettledSamples
	for _, vc := range res.VariantCapacities {
		vs := VariantState{Variant: vc.VariantName, Role: vc.Role, Replicas: vc.ReplicaCount, MinSettled: minSettled}
		for _, rs := range in.VariantStates {
			if rs.VariantName == vc.VariantName {
				vs.Accelerator, vs.GPUsPerReplica = rs.AcceleratorName, rs.GPUsPerReplica
			}
		}
		if sr := vc.ServiceRate; sr != nil && sr.HasLine {
			vs.Line = itl.Model{A: sr.LineSlope, B: sr.LineIntercept}
		}
		for _, b := range vc.Borrowed {
			if b.Quantity == domain.BorrowedITLLine {
				vs.BorrowedFrom = b.DonorNamespace + "/" + b.Donor
			}
		}
		rep := reports[vc.VariantName]
		if !vs.Line.IsZero() {
			vs.Tier = saturation.ITLTier(rep.Source, vs.BorrowedFrom != "")
		}
		if rep.PricingKey != "" {
			vs.PricingKey = rep.PricingKey
			vs.Readings = r.sat.ITLReadings(rep.PricingKey)
			vs.analyzerOneSided, vs.analyzerRaised = r.sat.ITLOneSided(rep.PricingKey)
		}
		vs.refit()
		var params *capacity.EngineParams
		var live int64
		if rec := r.store.Get(in.Namespace, in.ModelID, vc.VariantName); rec != nil {
			if rec.EngineParams != nil {
				p := *rec.EngineParams
				params = &p
			}
			live = rec.TotalKvCapacityTokens
		}
		for _, rm := range in.ReplicaMetrics {
			if rm.VariantName == vc.VariantName {
				vs.Ranks = max(vs.Ranks, rm.EngineRanks)
			}
		}
		if vs.Ranks < 1 {
			vs.Ranks = 1
			if params != nil && params.DataParallelSize > 1 {
				vs.Ranks = params.DataParallelSize
			}
		}
		vs.Shape = shapeOf(shapes[vc.VariantName])
		vs.CapacityTokens = whatif.CapacityTokens(params, vs.Ranks, live)
		vs.Params = params
		if params != nil {
			vs.TensorParallel = params.TensorParallelSize
			if params.MaxModelLen > 0 {
				vs.MaxModelLen = params.MaxModelLen
			}
			vs.MaxModelLenUnresolved = slices.Contains(params.Unresolved, "max_model_len") || slices.Contains(params.Unresolved, "context_length")
		}
		vs.BusiestRankFill = r.sat.BusiestRankFillFor(params)
		vs.BatchCap, vs.BatchCapSource = r.sat.PricingBatchCap(in.Namespace, in.ModelID, vc.VariantName, params, learning)
		st.Variants = append(st.Variants, vs)
	}
	slices.SortFunc(st.Variants, func(a, b VariantState) int { return strings.Compare(a.Variant, b.Variant) })
	return st
}

// sameLine reports two lines equal to a part in a billion: a refit of the
// readings a line was fitted from gives it back to the last bits.
func sameLine(a, b itl.Model) bool {
	near := func(x, y float64) bool { return math.Abs(x-y) <= 1e-9*max(math.Abs(x), math.Abs(y)) }
	return near(a.A, b.A) && near(a.B, b.B)
}

// refit finds the readings the line was fitted from, by making the tracker's
// choice of fit again on the key's windows: in the band, at the line's B,
// and, when that does not give the line back, the above-band fallback, which
// marks the line TierAboveBand. A held or borrowed line was fitted from no
// readings of the variant's own here.
//
// A pinned line the analyzer priced at the prior on B (issue ev-shindin/llm-scaling-manager#467; the
// recorded itl.Reading.PinnedAtBPrior) is refit with the analyzer's
// one-sided admission, which the state keeps for the bands (OneSided), so a
// resample prices as the controller did.
func (vs *VariantState) refit() {
	if vs.Tier != whatif.TierOLS && vs.Tier != whatif.TierPinnedB {
		return
	}
	vs.Baseline = vs.Line.B
	var adm itl.Admission
	if vs.Tier == whatif.TierPinnedB && vs.analyzerRaised {
		adm = vs.analyzerOneSided
	}
	adm.OLSLatched = vs.Tier == whatif.TierOLS && vs.Readings.OLSLatched
	if got := itl.Refit(vs.Readings.All, vs.Readings.Steady, vs.Baseline, vs.MinSettled, adm); got.Tier != "" && sameLine(got.Model, vs.Line) {
		vs.Tier, vs.LineReadings, vs.Refitted, vs.OneSided = got.Tier, got.Read, true, adm
		return
	}
	if got := itl.RefitAboveBand(vs.Readings.Above, vs.Readings.AboveSteady, vs.MinSettled); got.Tier != "" && sameLine(got.Model, vs.Line) {
		vs.Tier, vs.LineReadings, vs.Refitted = whatif.TierAboveBand, got.Read, true
	}
}

// CycleShape is one cycle's shape of a variant: its mean lengths and prefix
// hit rate, and its rows' summed request rate, 0 where none read one. Cycle
// counts from 1, as State.Cycle does: State.ArrivalRates[Cycle-1] is the
// model's arrival rate at it.
type CycleShape struct {
	Cycle                                    int
	InputTokens, OutputTokens, PrefixHitRate float64
	RequestRate                              float64
}

// noteSeries adds each variant's sample of cycle n, if it has one, to its
// series.
func noteSeries(series map[string][]CycleShape, shapes map[string][]shapeSample, n int) {
	for v, s := range shapes {
		if len(s) == 0 || s[len(s)-1].cycle != n {
			continue
		}
		x := s[len(s)-1]
		series[v] = append(series[v], CycleShape{Cycle: n, InputTokens: x.il, OutputTokens: x.ol, PrefixHitRate: x.hit, RequestRate: x.req})
	}
}

// disaggregated reports a P/D model: one with a prefill variant
// (domain.HasPrefill), as the analyzer reads it.
func (st *State) disaggregated() bool {
	roles := make(map[string]string, len(st.Variants))
	for _, v := range st.Variants {
		roles[v.Variant] = v.Role
	}
	return domain.HasPrefill(roles)
}

// liftToFloor raises a decode variant's line to the floor read below the
// band, when on is set (saturation.SwitchITLLineAtFloor; whatif.LiftToFloor):
// the caller sets it only for a P/D model (State.disaggregated). It runs
// after the refit, which finds the readings of the line as fitted. Only a
// decode role of a P/D model: an aggregated variant's readings below the
// band carry prefill chunks, which would read a floor its decode steps do
// not have, and the evidence for the lift is P/D decode alone.
func (vs *VariantState) liftToFloor(on bool) {
	if !on || domain.CanonicalRole(vs.Role) != domain.RoleDecode {
		return
	}
	vs.LineAtFloor = true
	vs.Line, vs.LineLift = whatif.LiftToFloor(vs.Tier, vs.Line, whatif.LineFloor{ITL: vs.BelowBandITL, Fill: vs.BelowBandFill})
}

// ShapeRange is the range of a variant's per-cycle mean lengths and prefix-hit
// rate, over the cycles with traffic; Cycles is how many, 0 with none.
type ShapeRange struct {
	ILMin, ILMax, OLMin, OLMax float64
	HitMin, HitMax             float64
	Cycles                     int
}

// noteRanges widens each variant's range by its sample of cycle n, if it has
// one: the latest of its shape window.
func noteRanges(ranges map[string]ShapeRange, shapes map[string][]shapeSample, n int) {
	for v, s := range shapes {
		if len(s) == 0 || s[len(s)-1].cycle != n {
			continue
		}
		x, r := s[len(s)-1], ranges[v]
		if r.Cycles == 0 {
			r = ShapeRange{ILMin: x.il, ILMax: x.il, OLMin: x.ol, OLMax: x.ol, HitMin: x.hit, HitMax: x.hit}
		}
		r.ILMin, r.ILMax = min(r.ILMin, x.il), max(r.ILMax, x.il)
		r.OLMin, r.OLMax = min(r.OLMin, x.ol), max(r.OLMax, x.ol)
		r.HitMin, r.HitMax = min(r.HitMin, x.hit), max(r.HitMax, x.hit)
		r.Cycles++
		ranges[v] = r
	}
}

// noteServing appends each variant's serving replicas at cycle n (1-based)
// to serving: CurrentReplicas less PendingReplicas, never below 0. A
// variant first named at a later cycle reads -1 at the cycles before it,
// and one the cycle does not name reads -1 at it.
func noteServing(serving map[string][]int, states []domain.VariantReplicaState, n int) {
	for _, vs := range states {
		row := serving[vs.VariantName]
		for len(row) < n-1 {
			row = append(row, -1)
		}
		if len(row) < n {
			row = append(row, max(vs.CurrentReplicas-vs.PendingReplicas, 0))
		}
		serving[vs.VariantName] = row
	}
}

// servingUpTo is serving cut or padded with -1 to n cycles, each row its own
// copy; nil with no variant.
func servingUpTo(serving map[string][]int, n int) map[string][]int {
	if len(serving) == 0 {
		return nil
	}
	out := make(map[string][]int, len(serving))
	for v, row := range serving {
		r := slices.Clone(row[:min(len(row), n)])
		for len(r) < n {
			r = append(r, -1)
		}
		out[v] = r
	}
	return out
}
