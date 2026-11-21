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
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
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
	// Prefill is each prefill variant at the cycle, by name, as a TTFT
	// question reads it (state_ttft.go).
	Prefill []PrefillState
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
	// default; "" with none), and Ranks the engine ranks per replica.
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
	// ReadingsFrom and ReadingsTo are the first and the last cycle of the
	// LineReadings, 0 with none.
	ReadingsFrom, ReadingsTo int
	// DecodeTTFT is what the variant's rows say of a TTFT's decode side; the
	// zero value on a variant that is not decode (state_ttft.go).
	DecodeTTFT DecodeTTFTState
	// LineRows is the variant's rows of its latest cycles that have one, off
	// a ramp and on one, for the line check (state_line.go).
	LineRows []whatif.LineRow
	// BelowBandITL is the floor below the band, in seconds: the 10th
	// percentile of the variant's latest mean ITLs read off a ramp at a fill
	// under the band's bottom (whatif.BelowBand.LeastITL). 0 with fewer than
	// itl.BelowBandMinReadings, where the line is not read below the band.
	// BelowBandFill is the fill that reading was taken at, 0 with none.
	BelowBandITL, BelowBandFill float64
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
//   - saturation.SwitchITLAdaptiveBandFloor: the ITL band's floor lowered to
//     the fill a low-fill fleet has.
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
// saturation.SwitchMoEBusiestRank is not listed either: it reads a
// data-parallel MoE engine's line at its busiest rank (issue
// llm-d-incubation/llm-d-scaling-manager#312), and its only evidence is
// synthetic. No recording in the goldens is of such an engine, so nothing
// shows it does not read a figure under what was measured; a replay turns it
// on by name.
func PlannerSwitches() map[string]map[string]bool {
	return map[string]map[string]bool{PackageSaturation: {
		saturation.SwitchPrefillALowLoadPerRank: true,
		saturation.SwitchPrefillAEngineTiming:   true,
		saturation.SwitchPrefillQueueByCapacity: true,
		saturation.SwitchPrefillFBusyMinutes:    true,
		saturation.SwitchITLAdaptiveBandFloor:   true,
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
		rows                     map[string][]whatif.LineRow
		below                    map[string][2]float64 // ITL seconds, fill
		ratios                   map[string]float64
	}
	snaps := map[int]*snap{}
	opts.Mode = Normal
	opts.Switches = withPlannerSwitches(opts.Switches)
	opts.discardRecording = true
	var (
		st       *State
		stateErr error
		n        int
		firstAt  time.Time
		windows  []whatif.TPOTWindow
		engine   []whatif.TPOTWindow
		shapes   = map[string][]shapeSample{}
		resident = map[string][]residentSample{}
		ttft     = newTTFTSamples()
		lines    = newLineSamples()
		cycleAt  = map[time.Time]int{}
		arrivals []float64
		ranges   = map[string]ShapeRange{}
	)
	// Read once: the switches are set before the replay starts.
	lineAtFloor := opts.Switches[PackageSaturation][saturation.SwitchITLLineAtFloor]
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
		if w, ok := tpotWindow(in.RouterLatency); ok {
			windows = append(windows, withFill(w, fill, fillOK))
		}
		if w, ok := engineTPOTWindow(in.ReplicaMetrics, rolesOf(in)); ok {
			engine = append(engine, withFill(w, fill, fillOK))
		}
		noteShapes(shapes, in.ReplicaMetrics, n)
		noteRanges(ranges, shapes, n)
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
		ttft.noteTerm(in, ev.Now, args)
		if n == cycle || early[n] {
			s := r.stateOf(in, n, ev.Now, shapes)
			for i := range s.Variants {
				v := &s.Variants[i]
				v.ShapeRange = ranges[v.Variant]
				if domain.CanonicalRole(v.Role) != domain.RolePrefill {
					v.Resident = residentOf(resident[v.Variant], v.Shape)
				}
			}
			r.fillTTFT(s, in, ttft)
			if n == cycle {
				st = s
				return true
			}
			sn := &snap{st: s, windows: len(windows), engine: len(engine), arrived: len(arrivals),
				rows: map[string][]whatif.LineRow{}, below: map[string][2]float64{}, ratios: map[string]float64{}}
			for _, v := range s.Variants {
				sn.rows[v.Variant] = slices.Clone(lines.rows(v.Variant))
				itlSec, fill := lines.leastBelowBand(v.Variant)
				sn.below[v.Variant] = [2]float64{itlSec, fill}
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
	st.ArrivalRates = arrivals
	for i := range st.Variants {
		st.Variants[i].ReadingsFrom, st.Variants[i].ReadingsTo = cyclesSpanned(st.Variants[i].LineReadings, cycleAt)
		st.Variants[i].LineRows = lines.rows(st.Variants[i].Variant)
		st.Variants[i].BelowBandITL, st.Variants[i].BelowBandFill = lines.leastBelowBand(st.Variants[i].Variant)
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
		for i := range s.Variants {
			s.Variants[i].ReadingsFrom, s.Variants[i].ReadingsTo = cyclesSpanned(s.Variants[i].LineReadings, cycleAt)
			s.Variants[i].LineRows = sn.rows[s.Variants[i].Variant]
			b := sn.below[s.Variants[i].Variant]
			s.Variants[i].BelowBandITL, s.Variants[i].BelowBandFill = b[0], b[1]
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
func tpotWindow(rl *domain.RouterLatency) (whatif.TPOTWindow, bool) {
	if rl == nil || len(rl.TPOTBuckets) == 0 {
		return whatif.TPOTWindow{}, false
	}
	w := whatif.TPOTWindow{Sum: rl.TPOTSumRate, Count: rl.TPOTCountRate, SumMeasured: true,
		Buckets: make([]whatif.HistogramBucket, len(rl.TPOTBuckets))}
	for i, b := range rl.TPOTBuckets {
		w.Buckets[i] = whatif.HistogramBucket{UpperEdge: b.UpperBound, Count: b.Rate}
	}
	return w, true
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
