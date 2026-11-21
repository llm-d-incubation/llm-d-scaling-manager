package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource"
)

// The questions, as their commands name them.
const (
	replicasQuestion = "replicas-to-slo"
	rateQuestion     = "slo-to-rate"
)

// The output formats -o takes.
const (
	outputText = "text"
	outputJSON = "json"
)

// answerFlags are the flags both questions take: where to read the fleet,
// and what to assume about it.
type answerFlags struct {
	recording, cycle, model, variant, output string
	// noSelfCheck is --no-self-check; serveSelfCheck the server's request
	// to self-check (selfCheckOn). folds is the states read at the folds'
	// cuts, and checked the self-check the answer prints with it.
	noSelfCheck, serveSelfCheck bool
	folds                       map[int]*learnreplay.State
	checked                     *whatifapi.SelfCheck
	// regularOnly refuses a --recording that is no regular file rather than
	// read it whole: the server's (openRecording).
	regularOnly bool
	// hint, when hintSet, is the words a refusal names a calm cycle in, set
	// by a caller that prices for another source (the server, the report);
	// else they follow the CLI's own source (hintSource).
	hint                      hintSource
	hintSet                   bool
	decode                    int
	batchCap                  int64
	inputTokens, outputTokens float64
	headroom, tpotRatio       float64
	seed                      uint64
	// assumeITLB is --assume-itl-b: the decode line's intercept B, a
	// duration or "floor", where the recording measured none (itl_b.go).
	assumeITLB string
	// ttft, ttftPercentile, assumeVP, assumePromptCV, arrivalCV2, prefill and
	// prefillVariant are the TTFT half's flags (ttft.go); inputBucket only
	// refuses per-bucket answers.
	ttft, ttftPercentile, prefillVariant, inputBucket string
	assumeVP, assumePromptCV, arrivalCV2              float64
	assumeRouterHop                                   time.Duration
	prefill                                           int
	set                                               map[string]bool
	// prefixHit is a stated prefix-cache hit rate, nil the measured one (a
	// report shape, at-replicas --shape).
	prefixHit *float64
	// assumeA is --assume-a, a TTFT flag (ttft.go).
	assumeA time.Duration
	// assumeTransferBase is --assume-transfer-base, a TTFT flag (ttft.go,
	// transferPerToken): the KV transfer's fixed part per request; 0, the
	// default, prices none, as before.
	assumeTransferBase time.Duration
	// flowControl is --flow-control, a TTFT flag (ttft.go): the router's flow
	// control stated, on or off, in place of its inference from the queue series.
	flowControl string
	// priceRouterWait is --price-router-wait, a TTFT flag (ttft.go): opt-in,
	// off by default.
	priceRouterWait bool
	// routerWaitInPrefill is --router-wait-in-prefill, a TTFT flag (ttft.go):
	// opt-in, off by default, measured on router v1.0.0 only.
	routerWaitInPrefill bool
	// psOnsets is --ps-onsets, chargeDecodeGate --charge-decode-gate and
	// prefillRunningCap --prefill-running-cap, TTFT flags (ttft.go): off by
	// default, as in the SLO analyzer.
	psOnsets, chargeDecodeGate bool
	prefillRunningCap          int
	// engineTPOT says R_q was read from the engine's TPOT histogram
	// (tpotRatioFor).
	engineTPOT bool
	// imbalanceAcrossCounts is --imbalance-across-counts, a TTFT flag
	// (ttft.go): opt-in, off by default, as in the SLO analyzer.
	imbalanceAcrossCounts bool
	// window is the --prometheus mode's flags, capture's own, and its
	// namespace the --namespace both modes take; captured is the window
	// read, once it is. question names the command, and warn is where a
	// capture's warnings go (stderr).
	window   captureFlags
	captured *capturedWindow
	question string
	warn     io.Writer
}

// capturedWindow is what the output says of the window a --prometheus
// answer was captured from: no client, no credentials.
type capturedWindow struct {
	from, to         time.Time
	step             time.Duration
	kubeStateMetrics bool
}

func (f *answerFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.recording, "recording", "", "a learner recording (.jsonl or .jsonl.gz) to answer from; this or --prometheus is required")
	fs.BoolVar(&f.noSelfCheck, "no-self-check", false, "do not check the planner on the recording beside the answer")
	fs.StringVar(&f.cycle, "cycle", "last", "the model's cycle to answer at: a number from 1, or last")
	fs.StringVar(&f.model, "model", "", "the model to answer for; empty takes the recording's only model")
	fs.StringVar(&f.window.namespace, "namespace", "", "the model's namespace; empty matches any (with --prometheus, the namespace to capture: required)")
	fs.StringVar(&f.variant, "variant", "", "price this decode variant only; empty prices every decode variant")
	fs.IntVar(&f.decode, "decode", 0, "decode replicas to price; 0 takes the recorded count")
	fs.Int64Var(&f.batchCap, "batch-cap", 0, "batch cap per engine rank; 0 takes the one the analyzer priced at "+
		"(with --prometheus, the engine's batch cap the window is captured with, as capture --batch-cap)")
	fs.Float64Var(&f.inputTokens, "assume-input-tokens", 0, "mean prompt length to price at, in place of the recorded one (flagged)")
	fs.Float64Var(&f.outputTokens, "assume-output-tokens", 0, "mean output length to price at, in place of the recorded one (flagged)")
	fs.Float64Var(&f.headroom, "headroom", 0, "the operating point as a fraction of the knee; default the policy's scaleUpThreshold")
	fs.Float64Var(&f.tpotRatio, "assume-tpot-ratio", 0, "R_q, a TPOT percentile over its mean, assumed rather than learned (flagged)")
	fs.StringVar(&f.assumeITLB, "assume-itl-b", "", "the decode line's intercept B (12ms), or floor for B from the floor read below the band, "+
		"where the recording measured none (PinnedDefaultB); the slope is refitted through it (flagged AssumedITLB)")
	fs.Uint64Var(&f.seed, "seed", whatif.DefaultResampling().Seed, "the seed of the bands' bootstrap")
	fs.StringVar(&f.output, "o", outputText, "output: text or json")
	fs.StringVar(&f.output, "output", outputText, "output: text or json (the same as -o)")
	fs.StringVar(&f.ttft, "ttft", "", "the TTFT target of slo-to-rate, end to end: a mean (200ms) or a percentile (200ms@p95)")
	fs.StringVar(&f.ttftPercentile, "ttft-percentile", "", "the TTFT percentile, in percent (90); on replicas-to-slo, default the policy's, else 90")
	fs.Float64Var(&f.assumeVP, "assume-vp", 0, "V_P, a prefill pod's prompt tokens per second, in place of the learned one (flagged)")
	fs.Float64Var(&f.assumePromptCV, "assume-prompt-cv", 0, "the prompt's coefficient of variation, 0 to 3, in place of the measured one or the default 2 (flagged)")
	fs.StringVar(&f.flowControl, "flow-control", "", "the router's flow control, on or off, in place of its inference from the queue series (flagged as stated)")
	fs.Float64Var(&f.arrivalCV2, "arrival-cv2", 0, "Ca^2, the arrivals' squared coefficient of variation at the prefill queue, in place of Poisson's 1 (flagged)")
	fs.DurationVar(&f.assumeRouterHop, "assume-router-hop", 0, "the hop between the router and the engines (8ms), in place of the learned or default one (flagged)")
	fs.IntVar(&f.prefill, "prefill", 0, "prefill replicas to price; 0 takes the recorded count")
	fs.StringVar(&f.prefillVariant, "prefill-variant", "", "the prefill variant to price, when the model has several")
	fs.DurationVar(&f.assumeA, "assume-a", 0, "a, the fixed latency one request pays on a prefill pod (11ms), in place of the learned one (flagged)")
	fs.DurationVar(&f.assumeTransferBase, "assume-transfer-base", 0, "replicas-to-slo and slo-to-rate only: the fixed part of the KV transfer per request, e.g. 20ms; the rest of the measured transfer scales with the tokens moved. Held at the measured transfer; ignored with none. Default 0: all of it scales (named on the assumed line)")
	fs.BoolVar(&f.priceRouterWait, "price-router-wait", whatif.DefaultPriceRouterWait, "opt-in, validated at one staircase step: price the flow-control gate past the prefill onset, its wait at the router and behind the burst it releases to one prefill pod (FlowControlWaitPriced); off, the wait is flagged AboveFlowControlOnset, a miss")
	fs.BoolVar(&f.routerWaitInPrefill, "router-wait-in-prefill", whatif.DefaultRouterWaitInPrefill, "opt-in, measured on router v1.0.0 only, at six bursty steps: take the router's wait past the prefill onset, up to 0.9 of the prefill knee, as inside the processor-sharing prefill price, so the figure stands (FlowControlWaitPriced) and a rate answer may pass the onset; reads short on router v0.11.0")
	fs.BoolVar(&f.psOnsets, "ps-onsets", whatif.DefaultPSOnsets, "experimental, unvalidated: place the prefill flow-control onset on the processor-sharing model, the prompts past --prefill-running-cap counted as waiting")
	fs.IntVar(&f.prefillRunningCap, "prefill-running-cap", 0, "with --ps-onsets, the most prompts a prefill pod computes at once (about max_num_batched_tokens over the prompt); 0 (unknown, the default) takes the lower of processor sharing at 1 and first come")
	fs.BoolVar(&f.chargeDecodeGate, "charge-decode-gate", whatif.DefaultChargeDecodeGate, "experimental, unvalidated, slo-to-rate only: charge the time the decode flow-control gate is closed below its onset to the rate answer")
	fs.StringVar(&f.inputBucket, "input-bucket", "", "not supported: per-bucket answers are the planner's M1")
	fs.BoolVar(&f.imbalanceAcrossCounts, "imbalance-across-counts", whatif.DefaultImbalanceAcrossCounts, fmt.Sprintf("opt-in: at a prefill count P1 has not learned, price f off a curve fitted to the counts it learned, or %g with none, in place of max(%g, f) (flagged AssumedImbalance either way)", whatif.FreshImbalance, whatif.ImbalanceFloor))
	f.window.registerWindow(fs)
	fs.Lookup("prometheus").Usage = "answer from a window of this Prometheus (base URL), captured and generated in memory, in place of --recording"
	fs.Lookup("from").Usage = "with --prometheus, the window's start: RFC 3339 or Unix seconds, to the millisecond (required)"
	fs.Lookup("to").Usage = "with --prometheus, the window's end: RFC 3339 or Unix seconds, to the millisecond (required)"
}

// check validates the flags both questions share once they are parsed.
func (f *answerFlags) check(fs *flag.FlagSet) error {
	f.set = map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	if err := f.checkSource(); err != nil {
		return err
	}
	if err := f.checkTTFT(); err != nil {
		return err
	}
	switch {
	case f.output != outputText && f.output != outputJSON:
		return usagef("-o %q, want text or json", f.output)
	case f.decode < 0:
		return usagef("--decode %d, want 0 (the recorded count) or more", f.decode)
	case f.batchCap < 0:
		return usagef("--batch-cap %d, want 0 (the priced cap) or more", f.batchCap)
	case f.set["assume-input-tokens"] && !(f.inputTokens >= 0 && finite(f.inputTokens)):
		return usagef("--assume-input-tokens %g, want a finite length of at least 0", f.inputTokens)
	case f.set["assume-output-tokens"] && !(f.outputTokens > 0 && finite(f.outputTokens)):
		return usagef("--assume-output-tokens %g, want a finite length above 0", f.outputTokens)
	}
	if f.set["assume-itl-b"] {
		if _, _, err := parseAssumeITLB(f.assumeITLB); err != nil {
			return err
		}
	}
	if _, err := f.cycleNumber(); err != nil {
		return err
	}
	return nil
}

// checkSource checks where the answer is read from: a recording, or a
// window of Prometheus (--prometheus), never both. The window's flags are
// capture's and are refused without --prometheus. It changes no flag.
func (f *answerFlags) checkSource() error {
	switch {
	case f.recording != "" && f.prometheus():
		return usagef("--recording and --prometheus both name where to answer from; give one")
	case f.recording == "" && !f.prometheus():
		return usagef("--recording or --prometheus is required")
	case f.prometheus():
		return nil
	}
	for _, name := range windowFlags() {
		if f.set[name] {
			return usagef("--%s is for --prometheus, and the answer is read from --recording", name)
		}
	}
	return nil
}

// pricedBatchCap is the batch cap per rank --batch-cap asks to price at, 0
// for the one the analyzer priced at. With --prometheus it is always 0:
// --batch-cap is then the engine's batch cap the window is captured with,
// as capture's is (windowOptions), and the engine prices at it, so the
// answer is the one capture, generate and --recording give, flag for flag.
func (f *answerFlags) pricedBatchCap() int64 {
	if f.prometheus() {
		return 0
	}
	return f.batchCap
}

// windowOptions is the capture the --prometheus flags ask for, as capture
// builds it from the same flags: --batch-cap is the engine's batch cap.
func (f *answerFlags) windowOptions() (whatifsource.Options, error) {
	w := f.window
	w.batchCap = int(f.batchCap)
	return w.options(f.question, f.warn)
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// cycleNumber is --cycle as a StateQuery's Cycle: 0 for last.
func (f *answerFlags) cycleNumber() (int, error) {
	if f.cycle == "last" {
		return 0, nil
	}
	n, err := strconv.Atoi(f.cycle)
	if err != nil || n < 1 {
		return 0, usagef("--cycle %q, want a cycle number from 1, or last", f.cycle)
	}
	return n, nil
}

// readState replays the recording to the cycle asked for: the --recording
// file, or with --prometheus the window captured and generated in memory
// (windowRecording). A file that cannot be read or is no recording, and a
// model or a cycle the recording does not hold, are input errors; a
// recording that fails inside the analyzers, or a replay cancelled, is an
// internal one.
func (f *answerFlags) readState(ctx context.Context) (*learnreplay.State, error) {
	name, src, err := f.recordingSource(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()
	st, err := f.stateOf(ctx, name, src)
	if err != nil {
		return nil, err
	}
	f.checkRecording(ctx, st, src)
	return st, nil
}

// stateOf is readState on a recording already opened: src, and the name its
// errors call it by. It checks the recording first (recordingSource.check),
// which sums it, and that the file did not change under its passes
// (recordingSource.unchanged), before any error a changed file would cause:
// a file rewritten under the check pass itself reads short or mixed, and
// that is the change, not the bytes it left.
func (f *answerFlags) stateOf(ctx context.Context, name string, src *recordingSource) (*learnreplay.State, error) {
	if err := src.check(name, f.recording); err != nil {
		if cerr := src.unchanged(); cerr != nil {
			return nil, cerr
		}
		return nil, err
	}
	cycle, _ := f.cycleNumber()
	st, err := f.readStates(ctx, src.open, learnreplay.StateQuery{Namespace: f.window.namespace, ModelID: f.model, Cycle: cycle})
	if cerr := src.unchanged(); cerr != nil {
		return nil, cerr
	}
	var q *learnreplay.QueryError
	switch {
	case errors.As(err, &q):
		return nil, usagef("%s: %v", name, err)
	case err != nil:
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return st, nil
}

// recordingSource is the recording to answer from, and the name errors call
// it by: the --recording file, read in passes, or the window --prometheus
// names, captured and generated in memory. The caller closes it. With
// regularOnly (the server), a --recording that is no regular file is
// refused (openRecording).
func (f *answerFlags) recordingSource(ctx context.Context) (string, *recordingSource, error) {
	if f.prometheus() {
		data, err := f.windowRecording(ctx)
		if err != nil {
			return "", nil, err
		}
		return redactedURL(f.window.prometheus), memoryRecording(data), nil
	}
	src, err := openRecording(f.recording, f.regularOnly)
	if err != nil {
		return "", nil, usageError{err}
	}
	return f.recording, src, nil
}

// windowRecording is the recording `capacity-planner capture` and then `capacity-planner
// generate` would write for the window the flags name, made in memory
// along the same path: whatifsource.Capture writes the capture as capture
// writes its file, gzipped, and generateRecording plays it as generate
// does. The answer is therefore the one --recording gives on the files.
// Errors carry capture's and generate's prefixes and exit codes: 2 for
// the caller's input (a window too large without --force, a role map that
// attributes nothing), 1 for a failed query or a replay that fails inside
// the analyzers. An error names no secret the URL carries (redactedError).
func (f *answerFlags) windowRecording(ctx context.Context) ([]byte, error) {
	data, err := f.captureAndGenerate(ctx)
	return data, redactedError(err, f.window.prometheus)
}

// captureAndGenerate is windowRecording before its errors are redacted.
func (f *answerFlags) captureAndGenerate(ctx context.Context) ([]byte, error) {
	opts, err := f.windowOptions()
	if err != nil {
		return nil, err
	}
	f.captured = &capturedWindow{from: opts.From, to: opts.To, step: opts.Step, kubeStateMetrics: opts.KubeStateMetrics}
	var capture bytes.Buffer
	if _, err := whatifsource.Capture(ctx, opts, &capture); err != nil {
		return nil, captureError(err)
	}
	raw, err := gunzipped(capture.Bytes())
	if err != nil {
		return nil, fmt.Errorf("capture: %w", err)
	}
	rep, err := generateRecording(ctx, raw)
	if err != nil {
		// generateRecording's usage error, if any, keeps its exit code.
		return nil, fmt.Errorf("generate: %w", err)
	}
	// What the capture could not see, as generate prints it, to stderr so
	// that -o json stays one object.
	start, err := startOf(rep.Recording)
	if err != nil {
		return nil, fmt.Errorf("generate: the generated recording: %w", err)
	}
	for _, c := range start.Caveats {
		fmt.Fprintf(f.warn, "capacity-planner: caveat: %s\n", c)
	}
	return rep.Recording, nil
}

// sourceLine is the first line of a human answer in --prometheus mode:
// what it was answered for, and from which window. Empty in recording
// mode, whose footer names the recording. The JSON carries the same in
// source.window, and its note stays the footer, as the files' does.
func (f *answerFlags) sourceLine(st *learnreplay.State) string {
	o := f.captured
	if o == nil {
		return selfCheckText(f.checked)
	}
	return fmt.Sprintf("answered for %s/%s from %s, %s to %s every %s (captured and generated in memory)\n",
		st.Namespace, st.ModelID, redactedURL(f.window.prometheus),
		o.from.Format(time.RFC3339), o.to.Format(time.RFC3339), o.step) + selfCheckText(f.checked)
}

// prometheus reports whether the flags ask to answer from Prometheus
// rather than from a recording.
func (f *answerFlags) prometheus() bool { return f.window.prometheus != "" }

// hintSource is the words a calm cycle is named in: the caller's, when set,
// else the CLI's flag for the source it read.
func (f *answerFlags) hintSource() hintSource {
	switch {
	case f.hintSet:
		return f.hint
	case f.prometheus():
		return hintCLIPrometheus
	}
	return hintCLIRecording
}

// pricedVariant is one decode variant as a question prices it: the state
// read from the recording, the Decode built from it, and the flags every
// answer on it carries.
type pricedVariant struct {
	state learnreplay.VariantState
	// perRank is the batch cap per engine rank priced at: --batch-cap, or
	// the analyzer's.
	perRank int64
	cycle   int
	decode  whatif.Decode
	shape   shape.Shape
	// check is the line against the recording's rows, the zero value with no
	// line.
	check whatif.LineCheck
	// assumedIL and assumedOL say which lengths the flags assumed.
	assumedIL, assumedOL bool
	// assumedHit says the prefix-hit rate was stated.
	assumedHit bool
	// kvSource says where the decode KV per request came from (residentKV).
	kvSource string
	// bandFloor is the ITL band floor the replay adapted to the fleet's fill
	// (learnreplay.State.ITLBandFloor), and bandPolicyFloor the policy's it
	// replaced; 0 where the policy's stood.
	bandFloor, bandPolicyFloor float64
	flags                      []whatif.Flag
	lines                      whatif.LineResamples
	// model is the model served, and stated the pricing flags set that put
	// every figure outside the envelope (statedOutside).
	model  string
	stated []string
	// itlB is what --assume-itl-b did to the line (assumeIntercept), and api
	// says the question came through the API, whose words a hint uses.
	itlB interceptAssumption
	api  bool
}

// Where the decode KV per request came from (pricedVariant.residentKV), as
// the inputs carry it.
const (
	// kvMeasured is the KV a running request held on the recording's rows.
	kvMeasured = "measured"
	// kvMeasuredScaled is that reading scaled to the lengths asked.
	kvMeasuredScaled = "measured, scaled to the shape asked"
	// kvContext is no reading: the whole context, IL + D x OL/2.
	kvContext = "context, no resident reading"
	// kvDiscountFloor is a reading below the shape's prefix-hit discount,
	// priced at the discount.
	kvDiscountFloor = "measured below the prefix-hit discount, priced at it"
	// kvAboveContext marks a reading above shape.AboveContextFactor times the
	// whole context, priced as read: an engine's own KV beyond the context (an
	// indexer's keys, a draft model's KV) is real, but a reader should see it.
	kvAboveContext = ", above the context"
	// kvSGLangContext is an SGLang reading above the whole context, priced at
	// the context.
	kvSGLangContext = "measured above the context on SGLang, priced at the context"
)

// residentKV is the KV one running request holds on the decode side, as the
// decode line is priced at, and sets kvSource.
//
// A prefix-cache hit saves compute, but on the decode side the whole context
// is resident: a hit saves KV only where another running request holds the
// same blocks (saturation's prefix_cache_kv.go). So the shape's KVreq, IL x
// (1 - hit) + D x OL/2, is never the price. The price is the measured KV per
// running request (learnreplay.ResidentKV), which also counts what the engine
// holds beyond the context (an indexer's keys, a draft model's KV) and so is
// not capped at it. A shape the flags state scales the reading by the ratio
// of its context to the measured one's, keeping the measured overhead factor.
// With no reading, the whole context, IL + D x OL/2. The reading is priced by
// shape.PriceResident, the rule the controller's analyzer shares
// (saturation.SwitchKVPerRequestAboveContext).
//
// A reading is held above the discount, as the controller's analyzer holds
// it (withResidentKV): below it is requests part way through their prefill,
// or a spread of output lengths D under-states, not more sharing than the hit
// rate allows, and pricing it would raise the knee.
//
// On SGLang a reading is held at the whole context: SGLang publishes the
// busiest rank's usage and running count against the pod's whole capacity
// (prefix_cache_kv.go), so on data-parallel attention it over-reads by about
// the rank count, and the knee would come out that many times low.
//
// Not gated by count as the analyzer's reading is: few requests read a prefix
// they truly share high, each carrying the whole of it, so a window of low
// concurrency on a shared prefix prices the knee low, at most by 1/(1 - hit).
// That errs towards more replicas; a reading above shape.AboveContextFactor
// times the context says so in its source.
func (pv *pricedVariant) residentKV() float64 {
	d := pv.shape.OutputDispersion
	whole := pv.shape.Context()
	r, m := pv.state.Resident, pv.state.Shape
	measured := shape.New(m.InputTokens, m.OutputTokens, 0).WithOutputDispersion(d).KVreq
	var kv float64
	switch {
	case !r.Read() || !(measured > 0):
		pv.kvSource = kvContext
		return whole
	case pv.assumedIL || pv.assumedOL:
		pv.kvSource, kv = kvMeasuredScaled, r.PerRequest*whole/measured
	default:
		pv.kvSource, kv = kvMeasured, r.PerRequest
	}
	sglang := pv.state.Params != nil && pv.state.Params.Engine == inferenceengine.EngineSGLang
	kv, how := pv.shape.PriceResident(kv, sglang)
	switch how {
	case shape.ResidentAtDiscount:
		pv.kvSource = kvDiscountFloor
	case shape.ResidentHeldAtContext:
		pv.kvSource = kvSGLangContext
	case shape.ResidentAboveContext:
		pv.kvSource += kvAboveContext
	}
	return kv
}

// assumed reports a shape the flags assumed, in part or whole.
func (pv pricedVariant) assumed() bool { return pv.assumedIL || pv.assumedOL || pv.assumedHit }

// noRoleHint is what to add to "the model has N decode variants" when every
// one of them reads as both roles (an empty role reads as both), which is what
// a capture gives a P/D fleet when it could not tell the roles apart. Naming
// one would then price a prefill variant as decode; the fix is the role map.
func noRoleHint(variants []pricedVariant) string {
	for _, pv := range variants {
		if domain.CanonicalRole(pv.state.Role) != domain.RoleBoth {
			return ""
		}
	}
	return fmt.Sprintf("; all %d were recorded as serving both roles. For a P/D fleet, record again with each Pod's role "+
		"(capture --role-of, or a role label such as llm-d.ai/role)", len(variants))
}

// decodeVariants is the variants a question prices: every variant that is not
// prefill (decode, and aggregated, whose line prices decode too), or the one
// --variant names.
func (f *answerFlags) decodeVariants(st *learnreplay.State) ([]pricedVariant, error) {
	lo, hi := st.Policy.ITLBand()
	band := whatif.FillBand{Lo: lo, Hi: hi}
	var out []pricedVariant
	for _, vs := range st.Variants {
		if domain.CanonicalRole(vs.Role) == domain.RolePrefill {
			continue
		}
		if f.variant != "" && vs.Variant != f.variant {
			continue
		}
		pv := pricedVariant{state: vs, cycle: st.Cycle, model: st.ModelID, stated: f.statedOutside(),
			api: f.hintSource() == hintAPIRecording || f.hintSource() == hintAPIPrometheus}
		f.assumeIntercept(&pv)
		vs = pv.state
		il, ol := vs.Shape.InputTokens, vs.Shape.OutputTokens
		if f.set["assume-input-tokens"] {
			il, pv.assumedIL = f.inputTokens, true
		}
		if f.set["assume-output-tokens"] {
			ol, pv.assumedOL = f.outputTokens, true
		}
		// A spread is a figure of the traffic the histogram counted, whatever
		// length is asked: as the prompt's cv (prefillShape).
		hit := vs.Shape.PrefixHitRate
		if f.prefixHit != nil {
			hit, pv.assumedHit = *f.prefixHit, true
		}
		pv.shape = shape.New(il, ol, hit).WithOutputDispersion(vs.Shape.OutputDispersion)
		replicas := vs.Replicas
		if f.decode > 0 {
			replicas = f.decode
		}
		pv.perRank = vs.BatchCap
		if c := f.pricedBatchCap(); c > 0 {
			pv.perRank = c
		}
		perRank := pv.perRank
		pv.decode = whatif.Decode{
			Line: vs.Line, Band: band, CapacityTokens: vs.CapacityTokens,
			KVPerRequest: pv.residentKV(), OutputTokens: pv.shape.AvgOutputTokens,
			ReplicaCap: perRank * int64(max(vs.Ranks, 1)), Replicas: replicas, Tier: vs.Tier,
			// A typed B is a guess: the line is not read below the band from
			// it. A B from the floor puts the line through the floor itself.
			BelowBand: whatif.BelowBand{Read: !pv.itlB.Applied || pv.itlB.Source == itlBFromFloor, LeastITL: vs.BelowBandITL},
			AssumedB:  pv.itlB.Applied,
		}
		if pv.assumed() {
			pv.flags = append(pv.flags, whatif.AssumedShape)
		}
		if pv.itlB.Applied {
			pv.flags = append(pv.flags, assumedITLB)
		}
		if vs.Shape.OutputDispersion == 0 {
			pv.flags = append(pv.flags, whatif.OutputSpreadUnread)
		}
		if st.ITLBandFloor > 0 {
			pv.bandFloor, pv.bandPolicyFloor = st.ITLBandFloor, st.ITLBandPolicyFloor
			pv.flags = append(pv.flags, adaptedBandFloor)
		}
		switch vs.Tier {
		case whatif.TierBorrowed:
			pv.flags = append(pv.flags, whatif.BorrowedLine)
		case whatif.TierHeld:
			pv.flags = append(pv.flags, whatif.HeldLine)
		}
		if !vs.Line.IsZero() {
			pv.check = whatif.CheckLine(pv.decode, vs.LineRows, st.At)
			if pv.check.Mismatch() {
				pv.flags = append(pv.flags, lineMismatch)
			}
		}
		if vs.Refitted {
			fit := whatif.Refit{Tier: vs.Tier, Line: vs.Line, Baseline: vs.Baseline, MinSettled: vs.MinSettled, OneSided: vs.OneSided}
			if vs.LineAtFloor {
				// Each resample lifted by the line's rule
				// (saturation.SwitchITLLineAtFloor), whether or not
				// the line itself was.
				fit.Floor = whatif.LineFloor{ITL: vs.BelowBandITL, Fill: vs.BelowBandFill}
			}
			pv.lines = whatif.BootstrapLines(vs.Readings, fit, f.resampling())
		}
		out = append(out, pv)
	}
	if f.variant != "" && len(out) == 0 {
		return nil, usagef("--variant %q: no decode variant of that name at this cycle", f.variant)
	}
	return out, nil
}

// resampling is the bands' bootstrap: the page's, from --seed.
func (f *answerFlags) resampling() whatif.Resampling {
	r := whatif.DefaultResampling()
	r.Seed = f.seed
	return r
}

// observedFill is the range of fill among the readings the line was fitted
// from (VariantState.LineReadings), ok false with none.
func (pv pricedVariant) observedFill() (lo, hi float64, ok bool) {
	rs := pv.state.LineReadings
	if len(rs) == 0 {
		return 0, 0, false
	}
	lo, hi = rs[0].K, rs[0].K
	for _, o := range rs[1:] {
		lo, hi = min(lo, o.K), max(hi, o.K)
	}
	return lo, hi, true
}

// observedFillMax is the top of observedFill.
func (pv pricedVariant) observedFillMax() (float64, bool) {
	_, hi, ok := pv.observedFill()
	return hi, ok
}

// qualify adds the variant's own flags to an answer, and ExtrapolatedFill
// when its fill lies above every reading; a NoLine refusal on a variant that
// learned no line says so by the cycle.
func (pv pricedVariant) qualify(r whatif.Result) whatif.Result {
	a, ok := r.(whatif.Answer)
	if !ok {
		if u, ok := r.(whatif.Unanswerable); ok && u.Reason == whatif.NoLine && pv.state.Line.IsZero() {
			u.Detail = fmt.Sprintf("no ITL line learned by cycle %d", pv.cycle)
			return u
		}
		if u, ok := r.(whatif.Unanswerable); ok && u.Reason == whatif.PinnedDefaultB {
			u.Detail += pv.pinnedDefaultBHint()
			return u
		}
		return r
	}
	flags := slices.Clone(a.Flags)
	for _, fl := range pv.flags {
		if !slices.Contains(flags, fl) {
			flags = append(flags, fl)
		}
	}
	if top, ok := pv.observedFillMax(); ok && a.Basis.Fill > top && !slices.Contains(flags, whatif.ExtrapolatedFill) {
		flags = append(flags, whatif.ExtrapolatedFill)
	}
	a.Flags = flags
	return a
}

// band bands an answer by rerunning value on every resampled line.
func (pv pricedVariant) band(r whatif.Result, value func(whatif.Decode) whatif.Result) whatif.Result {
	a, ok := r.(whatif.Answer)
	if !ok {
		return r
	}
	return whatif.BandAnswer(a, pv.lines, func(line itl.Model) (float64, bool) {
		d := pv.decode
		d.Line = line
		got, ok := value(d).(whatif.Answer)
		return got.Value, ok
	})
}

// headroomFor is the operating point's fraction of the knee: --headroom, else
// the policy's scaleUpThreshold, else the default.
func (f *answerFlags) headroomFor(p *config.ScalingPolicy) float64 {
	switch {
	case f.set["headroom"]:
		return f.headroom
	case p != nil && p.ScaleUpThreshold > 0:
		return p.ScaleUpThreshold
	}
	return config.DefaultScaleUpThreshold
}

// tpotRatioFor resolves R_q for a percentile q: --assume-tpot-ratio, else
// learned from the router's TPOT windows, which are what the client sees; else,
// flagged, from the engine's, where the router's hold no observation or read
// coarse and the engine's do not. With no router to fall back to, a refusal of
// the engine's is the answer (whatif.EngineTPOTRatio): it says more than that
// no histogram exists. Any other refusal of the router's stands: it says that
// data is wrong. A refusal comes back as the Result the percentile answers
// with.
func (f *answerFlags) tpotRatioFor(st *learnreplay.State, q float64) (whatif.TPOTRatio, []whatif.Flag, *whatif.Unanswerable) {
	if f.set["assume-tpot-ratio"] {
		return whatif.TPOTRatio{Value: f.tpotRatio, Assumed: true}, nil, nil
	}
	r, flags, refused := whatif.TPOTRatio{}, []whatif.Flag(nil), &whatif.Unanswerable{Reason: whatif.NoTPOTHistogram,
		Detail: "the recording carries no TPOT histogram; pass --assume-tpot-ratio for a flagged answer"}
	if len(st.TPOTWindows) > 0 {
		r, flags, refused = whatif.TPOTRatioFromWindows(st.TPOTWindows, q)
	}
	coarse := refused == nil && slices.Contains(flags, whatif.CoarseTPOTBucket)
	if !coarse && (refused == nil || refused.Reason != whatif.NoTPOTHistogram) || len(st.EngineTPOTWindows) == 0 {
		return r, flags, refused
	}
	er, eflags, erefused := whatif.EngineTPOTRatio(st.EngineTPOTWindows, q)
	switch {
	case erefused == nil && (!coarse || !slices.Contains(eflags, whatif.CoarseTPOTBucket)):
		f.engineTPOT = true
		return er, eflags, nil
	case coarse, erefused == nil, erefused.Reason == whatif.NoTPOTHistogram:
		return r, flags, refused
	}
	erefused.Detail += "; pass --assume-tpot-ratio for a flagged answer"
	return whatif.TPOTRatio{}, nil, erefused
}

// resolvedRatio is R_q as a question resolved it (ratioFor): the ratio, its
// flags, and its refusal, nil with none.
type resolvedRatio struct {
	ratio   whatif.TPOTRatio
	flags   []whatif.Flag
	refused *whatif.Unanswerable
}

// ratioFor is R_q for a percentile question at q: tpotRatioFor inside (0, 1),
// and outside it the assumed ratio, the percentile being the library's to
// refuse.
func (f *answerFlags) ratioFor(st *learnreplay.State, q float64) resolvedRatio {
	if q > 0 && q < 1 {
		var r resolvedRatio
		r.ratio, r.flags, r.refused = f.tpotRatioFor(st, q)
		return r
	}
	return resolvedRatio{ratio: whatif.TPOTRatio{Value: f.tpotRatio, Assumed: true}}
}

// tpotWindowCount is how many windows R_q was read from.
func (f *answerFlags) tpotWindowCount(st *learnreplay.State) int {
	if f.engineTPOT {
		return len(st.EngineTPOTWindows)
	}
	return len(st.TPOTWindows)
}

// placeholderRatio prices a percentile whose R_q was refused, so that the
// library still judges the rest of the question (withRatioRefusal).
var placeholderRatio = whatif.TPOTRatio{Value: 1}

// withRatioRefusal is r, priced with placeholderRatio, once R_q was refused:
// the refusal, unless r is InvalidInput, since a question asked wrongly
// outranks what the data says (whatif.ExitCode).
func withRatioRefusal(r whatif.Result, refused *whatif.Unanswerable) whatif.Result {
	if refused == nil {
		return r
	}
	if u, ok := r.(whatif.Unanswerable); ok && u.Reason == whatif.InvalidInput {
		return r
	}
	return *refused
}

// onRatio is r, a figure or a refusal priced on R_q, saying where R_q came
// from. A refusal is priced on it too: a floor at an assumed ratio is the
// assumption's. InvalidInput is the question's, and rests on nothing.
func onRatio(r whatif.Result, ratio whatif.TPOTRatio, flags []whatif.Flag) whatif.Result {
	switch v := r.(type) {
	case whatif.Answer:
		v.Flags = append(v.Flags, flags...)
		return v
	case whatif.Unanswerable:
		if v.Reason == whatif.InvalidInput {
			return v
		}
		add := slices.Clone(flags)
		if ratio.Assumed {
			add = append(add, whatif.AssumedTPOTRatio)
		}
		for _, fl := range add {
			if !slices.Contains(v.Flags, fl) {
				v.Flags = append(slices.Clone(v.Flags), fl)
			}
		}
		return v
	}
	return r
}

// ratioNote says where R_q came from, for the human output.
func (f *answerFlags) ratioNote(st *learnreplay.State, r whatif.TPOTRatio) string {
	switch {
	case f.engineTPOT:
		return fmt.Sprintf("R_q %s from %d engine TPOT windows, not the router's", num(r.Value), f.tpotWindowCount(st)) + worstNote(st.EngineTPOTWindows) + byFillNote(r)
	case r.Assumed && len(st.TPOTWindows) == 0:
		return fmt.Sprintf("R_q %s ASSUMED (no TPOT histogram)", num(r.Value))
	case r.Assumed:
		return fmt.Sprintf("R_q %s ASSUMED (the recording's %d TPOT windows not used)", num(r.Value), len(st.TPOTWindows))
	}
	return fmt.Sprintf("R_q %s from %d TPOT windows", num(r.Value), len(st.TPOTWindows)) + worstNote(st.TPOTWindows) + byFillNote(r)
}

// worstNote says R_q is the worst window where so few windows held requests
// that the median's margin reaches the largest (whatif.WorstWindow).
func worstNote(windows []whatif.TPOTWindow) string {
	n := 0
	for _, w := range windows {
		if w.Count > 0 {
			n++
		}
	}
	if !whatif.WorstWindow(n) {
		return ""
	}
	return fmt.Sprintf(" (the worst of %d)", n)
}

// byFillNote is R_q by fill for the human output: each fill bucket's ratio,
// its own reading where the rise over fill raised it, and the global ratio
// standing in where the bucket was thin; "" with one ratio at every fill.
func byFillNote(r whatif.TPOTRatio) string {
	if len(r.ByFill) == 0 {
		return ""
	}
	parts := make([]string, 0, len(r.ByFill))
	for _, fr := range r.ByFill {
		p := fmt.Sprintf("%.1f-%.1f %.3g", fr.Lo, fr.Hi, fr.Value)
		switch {
		case fr.Thin:
			p += fmt.Sprintf(" (global, %d windows)", fr.Windows)
		case fr.Learned != fr.Value:
			p += fmt.Sprintf(" (own %.3g, raised)", fr.Learned)
		case whatif.WorstWindow(fr.Windows):
			p += fmt.Sprintf(" (the worst of %d)", fr.Windows)
		}
		parts = append(parts, p)
	}
	return ", by fill " + strings.Join(parts, ", ")
}

// num prints a ratio or fill with no trailing zeros.
func num(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }

// parsePercent reads a percentile given in percent ("90", "99.9") as a
// fraction. Zero is no percentile at all, and an error; the library refuses
// any other outside (0, 1).
func parsePercent(s string) (float64, error) {
	p, err := strconv.ParseFloat(strings.TrimPrefix(s, "p"), 64)
	if err != nil {
		return 0, err
	}
	if p == 0 {
		return 0, errors.New("a percentile of 0 asks for no percentile; give one above 0, as p90")
	}
	return p / 100, nil
}

// footer is the line every human answer ends with: the readings the answer
// rests on, the cycles they were taken at, and the recording they came
// from, read to which cycle when not to its last.
func footer(st *learnreplay.State, variants []pricedVariant) string {
	n, from, to := 0, 0, 0
	for _, pv := range variants {
		n += len(pv.state.LineReadings)
		if f := pv.state.ReadingsFrom; f > 0 && (from == 0 || f < from) {
			from = f
		}
		to = max(to, pv.state.ReadingsTo)
	}
	readings := fmt.Sprintf("%d readings (cycles %d-%d)", n, from, to)
	if n == 0 {
		readings = "no readings of the line's own"
	}
	of := fmt.Sprintf("a %d-cycle recording", st.Cycles)
	if st.Cycle != st.Cycles {
		of += fmt.Sprintf(" read to cycle %d", st.Cycle)
	}
	return fmt.Sprintf("estimate from %s of %s, not a guarantee; see basis (-o json)", readings, of)
}

// shapeText is the shape a variant was priced at and where each length came
// from, and the spread of its output lengths where one was read.
func (pv pricedVariant) shapeText() string {
	if d := pv.state.Shape.OutputDispersion; d > 0 {
		return fmt.Sprintf("%s, output spread D %.3f (measured)", pv.lengthsText(), d)
	}
	return pv.lengthsText()
}

// lengthsText is the lengths a variant was priced at and where each came from:
// the flags, or the cycles it was measured over.
func (pv pricedVariant) lengthsText() string {
	m := pv.state.Shape
	measured := fmt.Sprintf("measured, cycles %d-%d", m.FromCycle, m.ToCycle)
	if m.Cycles == 0 {
		measured = fmt.Sprintf("no traffic by cycle %d", pv.cycle)
	}
	il, ol := num(round(pv.shape.AvgInputTokens)), num(round(pv.shape.AvgOutputTokens))
	switch {
	case pv.assumedIL && pv.assumedOL:
		return fmt.Sprintf("IL %s OL %s (assumed)", il, ol)
	case pv.assumedIL:
		return fmt.Sprintf("IL %s (assumed) OL %s (%s)", il, ol, measured)
	case pv.assumedOL:
		return fmt.Sprintf("IL %s (%s) OL %s (assumed)", il, measured, ol)
	}
	return fmt.Sprintf("IL %s OL %s (%s)", il, ol, measured)
}

// round is x to the nearest token.
func round(x float64) float64 { return math.Round(x) }

// newAnswerFlags is newFlags for an answer command: when args ask for JSON,
// and not for help, a flag error is not printed to out, so that the output
// stays one JSON object (asJSONError writes the error).
func newAnswerFlags(name string, args []string, out io.Writer) *flag.FlagSet {
	fs := newFlags(name, out)
	if wantsJSON(args) && !slices.ContainsFunc(args, func(a string) bool {
		return a == "-h" || a == "-help" || a == "--help" || a == "--h"
	}) {
		fs.SetOutput(io.Discard)
	}
	return fs
}

// wantsJSON reports whether args ask for JSON output, read before the
// command's flags are parsed so that a usage error is written as JSON too.
// The flag package reads -o and --output itself, in every form it accepts
// (-o json, -o=json, --output json, ...): a flag set of those two alone
// parses each argument that looks like a flag with the one after it, and an
// argument it does not know is skipped rather than stopping the scan. The
// last -o wins, as it does for the command.
func wantsJSON(args []string) bool {
	want := false
	for i, a := range args {
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		fs := flag.NewFlagSet("", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var o string
		fs.StringVar(&o, "o", "", "")
		fs.StringVar(&o, "output", "", "")
		if fs.Parse(args[i:min(i+2, len(args))]) == nil && o != "" {
			want = o == outputJSON
		}
	}
	return want
}

// asJSONError writes err, a command's error before any answer was written,
// as a JSON error object when args asked for JSON, and ends with its exit
// code. Any other error, an exit status (the output already written), and a
// run stopped by its context (a signal: execute says so) pass through.
func asJSONError(question string, args []string, out io.Writer, err error) error {
	var s exitStatus
	if err == nil || errors.As(err, &s) || errors.Is(err, context.Canceled) || !wantsJSON(args) {
		return err
	}
	code := exitCode(err)
	w := wireOutput{SchemaVersion: schemaVersion, Question: question, ExitCode: code, Roles: []wireRole{},
		Error: &wireError{Message: err.Error()}}
	if werr := writeJSON(out, w); werr != nil {
		return err
	}
	return exitStatus(code)
}

// gunzipped is data, unzipped when it is gzip.
func gunzipped(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		return data, nil
	}
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}
