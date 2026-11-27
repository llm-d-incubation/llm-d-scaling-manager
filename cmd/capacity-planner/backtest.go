package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// backtestFlags are backtest's flags.
type backtestFlags struct {
	fit, test, recording, split, fitUntil string
	// fitBelow and testAbove split by load (whatifapi.BacktestRequest's
	// FitBelow and TestAbove).
	fitBelow, testAbove                  string
	model, decodeVariant, prefillVariant string
	step, settle, ttft, measured, output string
	arrivalCV2, assumeVP                 float64
	assumeA                              time.Duration
	allowMismatch                        bool
	// assumePromptCV, assumeRouterHop and assumeITLB are the answers' own
	// --assume-* flags, and flowControl, routerWaitInPrefill and
	// priceRouterWait their TTFT flags of the same names (answer.go), so
	// that a backtest checks an answer with the inputs it stated (issue
	// llm-d-incubation/llm-d-scaling-manager#277): as assume.promptCV,
	// assume.routerHop, assume.itlB or assume.itlBFromFloor,
	// assume.flowControl and assume.routerWait. set is the flags given.
	assumePromptCV                       float64
	assumeRouterHop                      time.Duration
	assumeITLB                           string
	flowControl                          string
	routerWaitInPrefill, priceRouterWait bool
	set                                  map[string]bool
	// loadAwareRouter is --load-aware-router, as answerFlags' (answer.go):
	// assume.loadAwareRouter.
	loadAwareRouter bool
	// vpPlateau is --prefill-vp-plateau, as answerFlags' (vp_plateau.go):
	// both sides are read with V_P from the occupancy plateau.
	vpPlateau bool
	// hitAtCount is --prefill-hit-at-count, as answerFlags'
	// (hit_at_count.go): both sides are read with the switch on.
	hitAtCount bool
	// vpServiceLine is --prefill-vp-service-line, as answerFlags'
	// (vp_service_line.go): both sides are read with the switch on.
	vpServiceLine bool
	// fLoadCurve is --prefill-f-load-curve, as answerFlags'
	// (f_load_curve.go): both sides are read with the switch on.
	fLoadCurve bool
	// experimentalTTFT is --experimental-ttft, as answerFlags'
	// (experimental_ttft.go): both sides are read with its switches on.
	// rankMixtureTTFT is --experimental-ttft=v2 (experimental_ttft_v2.go),
	// backtest's only.
	experimentalTTFT, rankMixtureTTFT bool
	// rankMixtureFitSteps is --rank-mixture-fit-steps: the rank mixture's
	// fit-step fixes (whatifapi.BacktestRequest.RankMixtureFitSteps).
	rankMixtureFitSteps bool
	// rankMixtureStratified is --rank-mixture-stratified
	// (whatifapi.BacktestRequest.RankMixtureStratified): nil is the
	// default (whatif.DefaultRankMixtureStratified).
	rankMixtureStratified *bool
	// rankMixtureNLLine is --experimental-ttft=v2nl-line: the rank
	// mixture's variant with Z measured part by part
	// (whatif.QueueRankMixtureNLLine).
	rankMixtureNLLine bool
	// rankMixtureKVHold and rankMixtureRemainderSpread are
	// --rank-mixture-kv-hold and --rank-mixture-remainder-spread, the
	// variant's modes (whatifapi.BacktestRequest RankMixtureKVHold and
	// RankMixtureRemainderSpread).
	rankMixtureKVHold, rankMixtureRemainderSpread bool
	// rankMixtureRemainderServer is --rank-mixture-remainder-server, the
	// variant's remainder server on top of the spread
	// (whatifapi.BacktestRequest.RankMixtureRemainderServer).
	rankMixtureRemainderServer bool
	// fitWindow is --rank-mixture-min-fit-window and tpotFit
	// --tpot-dist-min-fit-steps (rank_mixture_fit.go).
	fitWindow rankMixtureFit
	tpotFit   tpotDistFit
	// experimentalTPOT is --experimental-tpot (experimental_tpot.go),
	// backtest's only.
	experimentalTPOT string
}

// registerAssume registers the stated-input flags backtest and predict take.
func (f *backtestFlags) registerAssume(fs *flag.FlagSet) {
	fs.Float64Var(&f.assumeVP, "assume-vp", 0, "V_P, a prefill pod's prompt tokens per second, where the fit learned none (flagged)")
	fs.DurationVar(&f.assumeA, "assume-a", 0, "a, the fixed latency on a prefill pod, where the fit learned none (flagged)")
	fs.Float64Var(&f.assumePromptCV, "assume-prompt-cv", 0, "the prompts' coefficient of variation, 0 to 3, in place of the one each step reads from the histogram (flagged)")
	fs.DurationVar(&f.assumeRouterHop, "assume-router-hop", 0, "the hop between the router and the engines (8ms), in place of the learned or default one (flagged)")
	fs.StringVar(&f.assumeITLB, "assume-itl-b", "", "the decode line's intercept B (12ms), or floor for B from the floor read below the band, "+
		"where the fit measured none (PinnedDefaultB); the slope is refitted through it (flagged AssumedITLB)")
	fs.StringVar(&f.flowControl, "flow-control", "", "the router's flow control, on or off, in place of its inference from the fit's queue series (flagged as stated)")
	fs.BoolVar(&f.routerWaitInPrefill, "router-wait-in-prefill", whatif.DefaultRouterWaitInPrefill, "as slo-to-rate's, on by default: take the router's wait past the prefill onset as inside the prefill price (router v1.0.0); =false for router v0.11.0")
	fs.BoolVar(&f.priceRouterWait, "price-router-wait", whatif.DefaultPriceRouterWait, "as slo-to-rate's: price the flow-control gate past the prefill onset as a stage (router v0.11.0); set, it turns the default --router-wait-in-prefill off")
	fs.BoolVar(&f.loadAwareRouter, "load-aware-router", whatif.DefaultPlannerLoadAwareRouter, "as slo-to-rate's: state that the router "+
		"balances prefill by load (llm-d's EPP queue scorer), so f is held to the load it was learned at, then falls to 1 at the pooled prefill knee; off by default")
	fs.BoolVar(&f.vpPlateau, vpPlateauFlag, false, vpPlateauUsage)
	fs.BoolVar(&f.hitAtCount, hitAtCountFlag, false, hitAtCountUsage)
	fs.BoolVar(&f.vpServiceLine, vpServiceLineFlag, false, vpServiceLineUsage)
	fs.BoolVar(&f.fLoadCurve, fLoadCurveFlag, false, fLoadCurveUsage)
	fs.Var(experimentalTTFTValue{switches: &f.experimentalTTFT, v2: &f.rankMixtureTTFT, nlLine: &f.rankMixtureNLLine}, experimentalTTFTFlag,
		experimentalTTFTV2Usage+"; "+experimentalTTFTNLLineUsage)
	fs.Var(optionalBool{&f.rankMixtureStratified, whatif.DefaultRankMixtureStratified}, rankMixtureStratifiedFlag, rankMixtureStratifiedUsage)
}

// parsed records the flags given, once fs is parsed, and lets the default
// --router-wait-in-prefill give way to a --price-router-wait set alone
// (routerWaitGivesWay), as answer's flags do.
func (f *backtestFlags) parsed(fs *flag.FlagSet) {
	f.set = map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	routerWaitGivesWay(f.set, f.priceRouterWait, &f.routerWaitInPrefill)
}

// runBacktest is `capacity-planner backtest`: the planner fitted on one
// window predicts another it did not see, step by step from that window's
// inputs alone, beside what the fleet measured there (POST /v1/backtest).
func runBacktest(ctx context.Context, args []string, out, warn io.Writer) error {
	fs := newFlags("backtest", out)
	var f backtestFlags
	fs.StringVar(&f.recording, "recording", "", "one recording to split: fit on its first part, test on the rest (with --split or --fit-until)")
	fs.StringVar(&f.split, "split", "", "with --recording, the share of its span to fit on, as 60%; default 60%")
	fs.StringVar(&f.fitUntil, "fit-until", "", "with --recording, fit on the cycles up to this time (RFC 3339), test on the rest")
	fs.StringVar(&f.fitBelow, "fit-below", "", "split by load: fit only on the steps whose arrival rate is below this, a rate (12.5) or a percentile of the steps' rates (p50); with --test-above")
	fs.StringVar(&f.testAbove, "test-above", "", "split by load: judge only the steps whose arrival rate is at or above this, a rate or a percentile (p75); with --fit-below")
	fs.StringVar(&f.fit, "fit", "", "the recording to fit on (with --test)")
	fs.StringVar(&f.test, "test", "", "the recording to test on (with --fit): its inputs are priced, its latency is what it is scored against")
	fs.StringVar(&f.model, "model", "", "the model; empty takes the source's only model")
	fs.StringVar(&f.prefillVariant, "prefill-variant", "", "the prefill variant, when the model has several")
	fs.StringVar(&f.decodeVariant, "variant", "", "the decode variant, when the model has several")
	fs.StringVar(&f.step, "step", whatifapi.DefaultBacktestStep.String(), "the length of a step, 1m to 1h, each judged on its own")
	fs.StringVar(&f.settle, "settle", whatifapi.DefaultBacktestSettle.String(), "how long after a change of replicas, and after the test's start, a step is not judged")
	fs.StringVar(&f.ttft, "ttft-target", "", "a TTFT target to judge each step by, predicted and measured: 500ms or 500ms@p90")
	fs.Float64Var(&f.arrivalCV2, "arrival-cv2", 0, "the test's arrivals' Ca^2, when known; default Poisson (1), flagged")
	fs.StringVar(&f.measured, "measured", whatifapi.MeasuredAuto, "the latency scored against: auto (the router's where recorded), router or engine")
	f.registerAssume(fs)
	// Backtest's only, as the rank mixture it changes: predict refuses
	// --experimental-ttft=v2.
	fs.BoolVar(&f.rankMixtureFitSteps, "rank-mixture-fit-steps", false, "with --experimental-ttft=v2, fit the KV-deferral lines and "+
		"the prompt slopes only on steps whose busiest prefill rank is under utilization 0.9, and read the ranks' shares at the "+
		"priced load (the fit steps of nearest rate) instead of the fit window's mean; off by default")
	fs.BoolVar(&f.rankMixtureKVHold, "rank-mixture-kv-hold", false, "with --experimental-ttft=v2nl-line, read the fitted "+
		"KV-deferral wait no further than the fit window's 95th percentile rank utilization: held past it, not extrapolated; "+
		"off by default")
	fs.BoolVar(&f.rankMixtureRemainderSpread, "rank-mixture-remainder-spread", false, "with --experimental-ttft=v2nl-line, draw "+
		"the router's remainder from the router's measured request-processing time where the fit window recorded it "+
		"(its line where not, as off); off by default")
	fs.BoolVar(&f.rankMixtureRemainderServer, rankMixtureRemainderServerFlag, false, rankMixtureRemainderServerUsage)
	f.fitWindow.registerWindow(fs)
	f.tpotFit.register(fs)
	fs.StringVar(&f.experimentalTPOT, experimentalTPOTFlag, "", experimentalTPOTUsage)
	fs.BoolVar(&f.allowMismatch, "allow-config-mismatch", false, "price a test whose configuration differs from the fit's, with the difference as a caveat")
	fs.StringVar(&f.output, "o", outputText, "output: text or json")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	f.parsed(fs)
	if f.output != outputText && f.output != outputJSON {
		return usagef("-o %q, want text or json", f.output)
	}
	if f.rankMixtureFitSteps && !f.rankMixtureTTFT {
		return usagef("--rank-mixture-fit-steps changes the fit of the rank mixture: give it with --experimental-ttft=v2")
	}
	if (f.rankMixtureKVHold || f.rankMixtureRemainderSpread) && !f.rankMixtureNLLine {
		return usagef("--rank-mixture-kv-hold and --rank-mixture-remainder-spread are modes of the rank mixture's variant: " +
			"give them with --experimental-ttft=v2nl-line")
	}
	if err := checkRemainderServer(f.rankMixtureRemainderServer, f.rankMixtureRemainderSpread, f.rankMixtureNLLine); err != nil {
		return err
	}
	if err := f.fitWindow.check(f.rankMixtureTTFT, f.rankMixtureNLLine); err != nil {
		return err
	}
	tpotModel, err := experimentalTPOTOf(f.experimentalTPOT)
	if err != nil {
		return err
	}
	if err := f.tpotFit.check(tpotModel != ""); err != nil {
		return err
	}
	warnKVHoldAlone(warn, f.rankMixtureKVHold, f.rankMixtureRemainderSpread)
	ctrllog.SetLogger(logr.Discard())
	req, err := f.request()
	if err != nil {
		return err
	}
	var bt *whatifapi.BacktestResponse
	byLoad := f.fitBelow != "" || f.testAbove != ""
	switch {
	case f.recording != "" && (f.fit != "" || f.test != ""):
		return usagef("--recording splits one recording; --fit and --test name two: give one way")
	case byLoad && (f.fitBelow == "" || f.testAbove == ""):
		return usagef("--fit-below and --test-above split by load together: give both")
	case byLoad && (f.split != "" || f.fitUntil != ""):
		return usagef("--fit-below and --test-above split by load, --split and --fit-until by time: give one")
	case byLoad && f.recording == "" && (f.fit == "" || f.test == ""):
		return usagef("give --recording (split one recording by load) or --fit and --test (the fit's low steps, the test's high ones)")
	case byLoad:
		fitPath, testPath := f.fit, f.test
		if f.recording != "" {
			fitPath, testPath = f.recording, f.recording
		}
		fitWhole, err := f.resolve(ctx, fitPath, 0, warn)
		if err != nil {
			return err
		}
		test := fitWhole
		if testPath != fitPath {
			if test, err = f.resolve(ctx, testPath, 0, warn); err != nil {
				return err
			}
		}
		fit, err := f.loadFit(ctx, &req, fitPath, fitWhole, warn)
		if err != nil {
			return err
		}
		if err := f.rankMixture(&req, testPath, test, fitPath, fit); err != nil {
			return err
		}
		if err := f.tpotDistribution(&req, testPath, test, fitPath, fit); err != nil {
			return err
		}
		bt, err = whatifapi.Backtest(ctx, fit, test, req, time.Time{}, "", "schema-v"+strconv.Itoa(schemaVersion))
		if err != nil {
			return backtestError(err)
		}
	case f.recording != "":
		whole, err := f.resolve(ctx, f.recording, 0, warn)
		if err != nil {
			return err
		}
		cycle, cutoff, words, err := whatifapi.SplitOf(whole, f.split, f.fitUntil)
		if err != nil {
			return usageError{err}
		}
		fit, err := f.resolve(ctx, f.recording, cycle, warn)
		if err != nil {
			return err
		}
		if err := f.rankMixture(&req, f.recording, whole, f.recording, fit); err != nil {
			return err
		}
		if err := f.tpotDistribution(&req, f.recording, whole, f.recording, fit); err != nil {
			return err
		}
		bt, err = whatifapi.Backtest(ctx, fit, whole, req, cutoff, words, "schema-v"+strconv.Itoa(schemaVersion))
		if err != nil {
			return backtestError(err)
		}
	case f.fit != "" && f.test != "":
		if f.split != "" || f.fitUntil != "" {
			return usagef("--split and --fit-until split one --recording; with --fit and --test leave them out")
		}
		fit, err := f.resolve(ctx, f.fit, 0, warn)
		if err != nil {
			return err
		}
		test, err := f.resolve(ctx, f.test, 0, warn)
		if err != nil {
			return err
		}
		if err := f.rankMixture(&req, f.test, test, f.fit, fit); err != nil {
			return err
		}
		if err := f.tpotDistribution(&req, f.test, test, f.fit, fit); err != nil {
			return err
		}
		bt, err = whatifapi.Backtest(ctx, fit, test, req, time.Time{}, "", "schema-v"+strconv.Itoa(schemaVersion))
		if err != nil {
			return backtestError(err)
		}
	default:
		return usagef("give --recording (split one recording) or --fit and --test (two)")
	}
	if f.output == outputJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		err = enc.Encode(bt)
	} else {
		err = writeBacktestText(out, bt)
	}
	if err != nil {
		return err
	}
	if bt.Summary.Judged == 0 {
		return exitStatus(3)
	}
	return nil
}

// rankMixture asks req for the rank mixture where --experimental-ttft=v2 is
// given, and reads each side's cycles for it: test from testPath, fit from
// fitPath.
func (f *backtestFlags) rankMixture(req *whatifapi.BacktestRequest, testPath string, test *whatifapi.Resolved, fitPath string, fit *whatifapi.Resolved) error {
	if !f.rankMixtureTTFT {
		return nil
	}
	req.TTFTModel, req.PrefillSlots = rankMixtureModel(f.rankMixtureNLLine), prefillSlotsOf(fit)
	req.RankMixtureFitSteps = f.rankMixtureFitSteps
	req.RankMixtureStratified = f.rankMixtureStratified
	req.RankMixtureKVHold, req.RankMixtureRemainderSpread = f.rankMixtureKVHold, f.rankMixtureRemainderSpread
	req.RankMixtureRemainderServer = f.rankMixtureRemainderServer
	req.RankMixtureMinFitWindow = f.fitWindow.windowField()
	if err := withRankMixture(testPath, test, nil); err != nil {
		return err
	}
	return withRankMixture(fitPath, fit, req.FitCycles)
}

// backtestError is err as the CLI exits with it: a request the inputs
// refuse (a configuration mismatch among them) is a usage error.
func backtestError(err error) error {
	if whatifapi.IsInputError(err) {
		return usageError{err}
	}
	return err
}

// request is the backtest the flags ask.
func (f *backtestFlags) request() (whatifapi.BacktestRequest, error) {
	req := whatifapi.BacktestRequest{Step: f.step, Settle: f.settle, Measured: f.measured, AllowConfigMismatch: f.allowMismatch,
		FitBelow: f.fitBelow, TestAbove: f.testAbove}
	if f.arrivalCV2 != 0 {
		req.ArrivalCV2 = &f.arrivalCV2
	}
	a, err := f.assume()
	if err != nil {
		return req, err
	}
	if a != (whatifapi.Assume{}) {
		req.Assume = &a
	}
	o, err := objectiveOf("ttft-target", f.ttft)
	if err != nil {
		return req, err
	}
	req.TTFT = o
	return req, nil
}

// assume is the terms the --assume-* flags state, checked as an answer
// checks them: V_P and a above 0, as before, and the rest where given.
func (f *backtestFlags) assume() (whatifapi.Assume, error) {
	a := whatifapi.Assume{FlowControl: f.flowControl, LoadAwareRouter: f.loadAwareRouter}
	if err := a.CheckFlowControl(); err != nil {
		return a, usagef("--flow-control %q, want on or off", f.flowControl)
	}
	switch {
	case f.routerWaitInPrefill && f.priceRouterWait:
		return a, usagef("--price-router-wait and --router-wait-in-prefill price the same wait two ways (router v0.11.0's gate, v1.0.0's): set one")
	case f.priceRouterWait == whatif.DefaultPriceRouterWait && f.routerWaitInPrefill == whatif.DefaultRouterWaitInPrefill:
	case f.priceRouterWait:
		a.RouterWait = whatifapi.RouterWaitGateStage
	case f.routerWaitInPrefill:
		a.RouterWait = whatifapi.RouterWaitInPrefill
	default:
		a.RouterWait = whatifapi.RouterWaitNone
	}
	if f.assumeVP > 0 {
		a.VP = &f.assumeVP
	}
	if f.assumeA > 0 {
		a.A = ptrTo(f.assumeA.Seconds())
	}
	if f.set["assume-prompt-cv"] {
		if !(f.assumePromptCV >= 0 && f.assumePromptCV <= whatif.PromptCVStop) {
			return a, usagef("--assume-prompt-cv %g, want a cv from 0 to %g: past that the rate stops falling as the cv rises", f.assumePromptCV, whatif.PromptCVStop)
		}
		a.PromptCV = &f.assumePromptCV
	}
	if f.set["assume-router-hop"] {
		if f.assumeRouterHop < 0 {
			return a, usagef("--assume-router-hop %s, want a duration of 0 or more", f.assumeRouterHop)
		}
		a.RouterHop = ptrTo(f.assumeRouterHop.Seconds())
	}
	if f.set["assume-itl-b"] {
		floor, b, err := parseAssumeITLB(f.assumeITLB)
		if err != nil {
			return a, err
		}
		if floor {
			a.ITLBFromFloor = true
		} else {
			a.ITLB = ptrTo(b.Seconds())
		}
	}
	return a, nil
}

// resolveBacktest resolves one recording at cycle (0 the last): the
// served path, which tests replace with one that replays each once.
var resolveBacktest = func(ctx context.Context, f *backtestFlags, path string, cycle int, warn io.Writer) (*whatifapi.Resolved, error) {
	return resolveCycles(ctx, f, path, cycle, nil, warn)
}

// resolveCycles is resolveBacktest, of only the cycles the set holds where it
// is not nil: a load split's fit (whatifapi.LoadFitCycles).
func resolveCycles(ctx context.Context, f *backtestFlags, path string, cycle int, cycles *whatifapi.CycleSet, warn io.Writer) (*whatifapi.Resolved, error) {
	src := &servedSources{warn: warn, recordings: filepath.Dir(path)}
	in := whatifapi.InputsRequest{Model: f.model, PrefillVariant: f.prefillVariant, DecodeVariant: f.decodeVariant,
		PrefillVPPlateau: f.vpPlateau, PrefillHitAtCount: f.hitAtCount, PrefillVPServiceLine: f.vpServiceLine, PrefillFLoadCurve: f.fLoadCurve, ExperimentalTTFT: f.experimentalTTFT,
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(path), Cycle: cycle, Cycles: cycles}}
	res, err := src.Resolve(ctx, in)
	if err != nil && whatifapi.IsInputError(err) {
		return nil, usageError{err}
	}
	return res, err
}

// resolve is resolveBacktest.
func (f *backtestFlags) resolve(ctx context.Context, path string, cycle int, warn io.Writer) (*whatifapi.Resolved, error) {
	return resolveBacktest(ctx, f, path, cycle, warn)
}

// loadFit is the fit of a split by load: the recording at path read from
// its steps below --fit-below alone, and those cycles, set in req.
func (f *backtestFlags) loadFit(ctx context.Context, req *whatifapi.BacktestRequest, path string, whole *whatifapi.Resolved, warn io.Writer) (*whatifapi.Resolved, error) {
	cs, err := whatifapi.LoadFitCycles(whole, *req)
	if err != nil {
		return nil, backtestError(err)
	}
	req.FitCycles = cs
	return resolveCycles(ctx, f, path, 0, cs, warn)
}

// seriesAndLatencyOf is seriesOf, and each cycle's measured latency beside
// it, read in the same pass.
func seriesAndLatencyOf(src learnreplay.Source, st *learnreplay.State, v learnreplay.SeriesVariants) ([]whatifapi.SeriesSample, []whatifapi.LatencySample, error) {
	cycles, lat, err := learnreplay.SeriesWithLatencyFrom(src, learnreplay.StateQuery{Namespace: st.Namespace, ModelID: st.ModelID}, v)
	if err != nil {
		return nil, nil, err
	}
	return seriesSamplesOf(cycles), latencySamplesOf(lat), nil
}

// latencySamplesOf is each cycle's measured latency as the planner API's.
func latencySamplesOf(lat []learnreplay.CycleLatency) []whatifapi.LatencySample {
	ls := make([]whatifapi.LatencySample, len(lat))
	for i, l := range lat {
		ls[i] = whatifapi.LatencySample{At: l.At, Router: l.Router, EngineTTFT: l.EngineTTFT, EngineTTFTMean: l.EngineTTFTMean,
			EngineTPOT: l.EngineTPOT, EngineITLMean: l.EngineITLMean}
	}
	return ls
}

// seriesSamplesOf is the cycles as the planner API's series.
func seriesSamplesOf(cycles []learnreplay.CycleSample) []whatifapi.SeriesSample {
	out := make([]whatifapi.SeriesSample, len(cycles))
	for i, c := range cycles {
		out[i] = whatifapi.SeriesSample{At: c.At, Rate: c.ArrivalRate, RateKnown: c.RateKnown, Prefill: c.PrefillPods, Decode: c.DecodePods,
			PrefillGPUs: c.PrefillGPUs, DecodeGPUs: c.DecodeGPUs, PromptTokens: c.PromptTokens, OutputTokens: c.OutputTokens,
			PrefixHitRate: c.PrefixHitRate, HasShape: c.HasShape, OutputTokensSquared: c.OutputTokensSquared, OtherPods: c.OtherPods,
			PromptComputedRate: c.PromptComputedRate, PromptRequestRate: c.PromptRequestRate, PromptBuckets: c.PromptBuckets,
			FlowControlSeries: c.FlowControlSeries, EngineRate: c.EngineRate, PrefillReplicas: c.PrefillReplicas,
			DecodeReplicas: c.DecodeReplicas, RouterQueued: c.RouterQueued,
			PrefillNotServing: c.PrefillNotServing, DecodeNotServing: c.DecodeNotServing,
			DecodeKVBusiest: c.DecodeKVBusiest, DecodeKVMean: c.DecodeKVMean, DecodeKVPods: c.DecodeKVPods}
	}
	return out
}

// runConfigOf is what the state says of the configuration the fleet ran:
// the model, the roles priced, the flow-control series, and each priced
// role's accelerator and GPUs and every engine setting the capacity
// fingerprint holds as identity (capacity.FingerprintFields: parallelism,
// dtypes, batch and context limits, speculative decoding, KV connector and
// offload, prefix caching, checkpoint, image). A value the state does not
// carry, or one the parser could not read (EngineParams.Unresolved), is ""
// and not compared: a default standing in for an unknown would make two
// different engines compare equal. A priced variant the state does not hold
// reads "not found".
func runConfigOf(st *learnreplay.State, decodeVariant, prefillVariant string) whatifapi.RunConfig {
	c := whatifapi.RunConfig{}
	add := func(name, value string) { c.Fields = append(c.Fields, whatifapi.ConfigField{Name: name, Value: value}) }
	add("model", st.ModelID)
	roles := roleDecodeWord
	if prefillVariant != "" {
		roles = rolePrefill + "+" + roleDecodeWord
	}
	add("roles", roles)
	fc := "absent (off)"
	if st.FlowControlSeries {
		fc = "present (on)"
	}
	add(whatifapi.ConfigFlowControl, fc)
	role := func(name, variant string) {
		i := slices.IndexFunc(st.Variants, func(v learnreplay.VariantState) bool { return v.Variant == variant })
		if i < 0 {
			add(name+" variant", "not found")
			return
		}
		v := st.Variants[i]
		add(name+" accelerator", v.Accelerator)
		add(name+" GPUs per replica", intWords(v.GPUsPerReplica))
		p := v.Params
		if p == nil {
			return
		}
		if w := engineOf(p); w != nil {
			add(name+" vLLM release", w.VLLMVersion)
		}
		unresolved := map[string]bool{}
		for _, f := range p.Unresolved {
			unresolved[strings.ReplaceAll(strings.TrimLeft(f, "-"), "-", "_")] = true
		}
		values := p.FingerprintValues()
		for j, label := range capacity.FingerprintFields() {
			if j >= len(values) || label == "unresolved" {
				continue
			}
			v := values[j]
			switch {
			case unresolved[label]:
				v = ""
			case v == "":
				// The parser read the engine's flags and found none set.
				v = "none"
			}
			add(name+" "+strings.ReplaceAll(label, "_", "-"), v)
		}
	}
	role(roleDecodeWord, decodeVariant)
	if prefillVariant != "" {
		role(rolePrefill, prefillVariant)
	}
	return c
}

// intWords is n, or "" for 0 (not stated).
func intWords(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// btMs is seconds in ms under one second, in seconds above, or "-".
func btMs(x *float64) string {
	switch {
	case x == nil:
		return "-"
	case *x >= 1:
		return fmt.Sprintf("%.1fs", *x)
	}
	return fmt.Sprintf("%.0fms", *x*1000)
}

// pointText is a step's statistic, predicted/measured, with * where the
// prediction is inside the validated region, "overloaded" where it gave no
// figure, calling the fleet overloaded, and "<=" before a prediction that is
// an upper bound (whatifapi.Figure.UpperBound).
func pointText(ps []whatifapi.BacktestPoint, stat string) string {
	for _, p := range ps {
		if p.Statistic != stat {
			continue
		}
		pred := btMs(p.Predicted.Value)
		if p.Predicted.UpperBound {
			pred = "<=" + pred
		}
		if r := p.Predicted.Refused; r != nil {
			pred = "no figure"
			if r.Reason == string(whatif.Saturated) {
				pred = "overloaded"
			}
		}
		mark := ""
		if p.InRegion {
			mark = "*"
		}
		meas := btMs(p.Measured)
		switch {
		case p.AboveHistogram || p.Unscored == whatifapi.UnscoredAboveHistogram:
			meas = ">top"
		case p.MeasuredBucket != nil:
			meas += "~"
		}
		return pred + "/" + meas + mark
	}
	return "-"
}

// shareText is a share as a percentage, or "-".
func shareText(x *float64) string {
	if x == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *x*100)
}

// ratioText is a ratio, or "-".
func ratioText(x *float64) string {
	if x == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *x)
}

// factorText is an error share e^x - 1 as the factor e^x, or "-".
func factorText(x *float64) string {
	if x == nil {
		return "-"
	}
	return fmt.Sprintf("%.2fx", 1+*x)
}

// excludedWords is a reason a step was not judged, in words.
var excludedWords = map[string]string{whatifapi.ExcludedNoTraffic: "no traffic", whatifapi.ExcludedNoMeasurement: "no measured requests",
	whatifapi.ExcludedScaling: "replicas changing", whatifapi.ExcludedWarmUp: "warm-up", whatifapi.ExcludedNotPriced: "not priced",
	whatifapi.ExcludedOtherVariants: "other variants serving", whatifapi.ExcludedBurst: "a burst near the knee in its window",
	whatifapi.ExcludedLoad: "outside the test's load"}

// writeBacktestText is the backtest's human output: the verdict first, what
// was not judged, the windows and the configuration, the steps, the scores
// and the notes.
func writeBacktestText(out io.Writer, bt *whatifapi.BacktestResponse) error {
	var b strings.Builder
	for _, l := range bt.VerdictLines {
		fmt.Fprintf(&b, "%s\n", l)
	}
	fmt.Fprintf(&b, "%s\n\n", bt.Excluded)
	at := func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") }
	fmt.Fprintf(&b, "fit:  %s, %s to %s UTC, %d cycles\n", bt.Fit.Source, at(bt.Fit.From), at(bt.Fit.To), bt.Fit.Cycles)
	fmt.Fprintf(&b, "test: %s, %s to %s UTC, %d cycles", bt.Test.Source, at(bt.Test.From), at(bt.Test.To), bt.Test.Cycles)
	switch ls := bt.LoadSplit; {
	case ls != nil:
		fmt.Fprintf(&b, "\nsplit %s: the fit read %s, the test judges %s at or above its floor", bt.Split, stepsWords(ls.FitSteps), stepsWords(ls.TestSteps))
		if ls.FitRead > 0 {
			fmt.Fprintf(&b, ", less %s the fit read", stepsWords(ls.FitRead))
		}
	case bt.Split != "":
		fmt.Fprintf(&b, " (one recording split: %s)", bt.Split)
	}
	b.WriteString("\n")
	switch {
	case bt.Config.Match:
		fmt.Fprintf(&b, "configuration: identical where both recordings record it; not compared: %s\n", strings.Join(bt.Config.NotCompared, "; "))
	default:
		b.WriteString("CONFIGURATION DIFFERS (priced anyway, --allow-config-mismatch):\n")
		for _, d := range bt.Config.Differences {
			fmt.Fprintf(&b, "  %s: fit %s, test %s\n", d.Field, d.Fit, d.Test)
		}
	}
	fmt.Fprintf(&b, "measured: %s\n", bt.MeasuredNote)
	for _, a := range bt.Assumed {
		fmt.Fprintf(&b, "assumed: %s\n", a)
	}
	if t := bt.TTFT; t != nil && t.Target != nil {
		fmt.Fprintf(&b, "TTFT target: %s %s\n", t.Statistic, fmtSeconds(*t.Target))
	}
	b.WriteString("\neach step: predicted/measured (* inside the conditions our accuracy benchmarks cover; " +
		"~ measured in a histogram bucket too wide to place it, scored only where the prediction falls outside it")
	if anyUpperBound(bt.Steps) {
		b.WriteString("; <= an upper bound, priced from a lower bound of V_P: scored only where the fleet measured more than 20% over it")
	}
	b.WriteString(")\n")
	const layout = "%-16s %7s %5s  %-19s %-19s %-17s %-17s %s\n"
	fmt.Fprintf(&b, layout, "step (UTC)", "rate", "P+D", "TTFT mean", "TTFT p90", "TPOT mean", "TPOT p90", "")
	for _, s := range bt.Steps {
		status := "judged"
		if s.Excluded != "" {
			status = "not judged: " + excludedWords[s.Excluded]
			if s.Excluded == whatifapi.ExcludedNotPriced {
				status += " (" + s.Detail + ")"
			}
		}
		if s.Target != nil && s.Excluded == "" {
			status += fmt.Sprintf(" (target: predicted %s, measured %s)", s.Target.Predicted, s.Target.Measured)
		}
		fmt.Fprintf(&b, layout, at(s.Start), fmt.Sprintf("%.2f", s.Rate), fmt.Sprintf("%d+%d", s.Prefill, s.Decode),
			pointText(s.TTFT, backtestMean), pointText(s.TTFT, backtestP90), pointText(s.TPOT, backtestMean), pointText(s.TPOT, backtestP90), status)
	}
	b.WriteString("\nscore over the steps with a figure\n")
	const sl = "%-10s %6s %7s %7s %8s %10s %13s %12s %s\n"
	fmt.Fprintf(&b, sl, "", "steps", "median", "within", "read", "typical", "worst-10%", "under upper", "")
	fmt.Fprintf(&b, sl, "", "", "ratio", "20%", ">20% low", "miss", "miss", "bound", "")
	region := 0
	for _, part := range []struct {
		name   string
		scores []whatifapi.BacktestScore
	}{{"TTFT", bt.Summary.TTFT}, {"TPOT", bt.Summary.TPOT}} {
		for _, sc := range part.scores {
			a := sc.All
			region += sc.InRegion.Steps
			fmt.Fprintf(&b, sl, part.name+" "+sc.Statistic, strconv.Itoa(a.Steps), ratioText(a.Bias), shareText(a.Within), strconv.Itoa(a.Low),
				factorText(a.MedianError), factorText(a.P90Error), shareText(a.Coverage), unscoredText(sc))
		}
	}
	if region > 0 {
		b.WriteString("\ninside the conditions our accuracy benchmarks cover: steps, median ratio, within 20%\n")
		for _, part := range []struct {
			name   string
			scores []whatifapi.BacktestScore
		}{{"TTFT", bt.Summary.TTFT}, {"TPOT", bt.Summary.TPOT}} {
			for _, sc := range part.scores {
				if r := sc.InRegion; r.Steps > 0 {
					fmt.Fprintf(&b, "%-10s %3d %6s %6s\n", part.name+" "+sc.Statistic, r.Steps, ratioText(r.Bias), shareText(r.Within))
				}
			}
		}
	}
	b.WriteString("\nby load (the step's rate over the load the planner thinks the fleet can carry): steps, median ratio, within 20%\n")
	for _, l := range bt.Summary.ByLoad {
		fmt.Fprintf(&b, "%-24s", l.Load)
		for _, part := range []struct {
			name   string
			steps  int
			scores []whatifapi.BacktestScore
		}{{"TTFT", l.Steps, l.TTFT}, {"TPOT", l.DecodeSteps, l.TPOT}} {
			fmt.Fprintf(&b, "  %s (%s):", part.name, stepsWords(part.steps))
			for _, sc := range part.scores {
				if sc.Statistic == backtestMean || sc.Statistic == backtestP90 {
					fmt.Fprintf(&b, " %s %d %s %s", sc.Statistic, sc.All.Steps, ratioText(sc.All.Bias), shareText(sc.All.Within))
				}
			}
		}
		b.WriteString("\n")
	}
	if t := bt.Summary.Target; t != nil {
		fmt.Fprintf(&b, "\nTTFT target over %d judged steps: both met %d, both missed %d, predicted met but measured missed %d, "+
			"predicted missed but measured met %d, not judged %d\n", t.Steps, t.BothMet, t.BothMissed, t.MissedMisses, t.FalseAlarms, t.NotJudged)
	}
	b.WriteString("\n")
	for _, n := range bt.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	fmt.Fprintf(&b, "%s\n", bt.Banner)
	_, err := io.WriteString(out, b.String())
	return err
}

// anyUpperBound says a step's prediction is an upper bound
// (whatifapi.Figure.UpperBound).
func anyUpperBound(steps []whatifapi.BacktestStep) bool {
	for _, s := range steps {
		for _, p := range s.TTFT {
			if p.Predicted.UpperBound {
				return true
			}
		}
	}
	return false
}

// stepsWords is n steps, singular or plural.
func stepsWords(n int) string {
	if n == 1 {
		return "1 step"
	}
	return strconv.Itoa(n) + " steps"
}

// unscoredText is why a statistic's other judged steps were not scored.
func unscoredText(sc whatifapi.BacktestScore) string {
	var parts []string
	for _, r := range slices.Sorted(maps.Keys(sc.Refused)) {
		parts = append(parts, fmt.Sprintf("%d refused %s", sc.Refused[r], r))
	}
	for _, k := range slices.Sorted(maps.Keys(sc.Unscored)) {
		switch {
		case k == whatifapi.UnscoredRefused:
		case k == whatifapi.UnscoredUnresolvable && sc.SameBucket > 0:
			// The third outcome: consistent at the bucket's resolution.
			parts = append(parts, fmt.Sprintf("%d %s (%d in the measured bucket)", sc.Unscored[k], k, sc.SameBucket))
		default:
			parts = append(parts, fmt.Sprintf("%d %s", sc.Unscored[k], k))
		}
	}
	return strings.Join(parts, ", ")
}

// backtestMean and backtestP90 are the statistics the by-load lines print.
const (
	backtestMean = "mean"
	backtestP90  = "p90"
)
