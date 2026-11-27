package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// Every answer read from a recording also checks the planner on it
// (whatifapi.SelfCheckOf): the replay that reads the answer's state also
// reads the states at the folds' cuts (learnreplay.StatesFrom); the
// self-check adds one replay of the series and latency it scores against.
// --no-self-check, and selfCheck false on the API, turn it off.

// defaultSelfCheck is whether an answer self-checks when not told: true.
// The package's tests turn it off, so that the answers' goldens pin the
// answer alone; the self-check has goldens of its own.
var defaultSelfCheck = true

// selfCheckOn says the answer self-checks: the server's request, or the
// CLI's default less --no-self-check.
func (f *answerFlags) selfCheckOn() bool {
	if f.question == "serve" {
		return f.serveSelfCheck
	}
	return defaultSelfCheck && !f.noSelfCheck
}

// testReplaySwitches is the replay switches every answer reads its states
// with, beside the planner's own (learnreplay.PlannerSwitches): nil but in a
// test that turns a planner-only switch on through the CLI's whole path.
var testReplaySwitches map[string]map[string]bool

// readStates reads the state at the answer's cycle and, when the answer
// self-checks, the states at the folds' cuts, in one replay.
func (f *answerFlags) readStates(ctx context.Context, src learnreplay.Source, q learnreplay.StateQuery) (*learnreplay.State, error) {
	if !f.selfCheckOn() {
		return learnreplay.StateFrom(ctx, src, f.replayOptions(), q)
	}
	st, folds, err := learnreplay.StatesFrom(ctx, src, f.replayOptions(), q, whatifapi.SelfCheckCycles)
	f.folds = folds
	return st, err
}

// foldFits is the folds' states, priced as the answer's state is.
func (f *answerFlags) foldFits(decodeVariant, prefillVariant string) []whatifapi.FoldFit {
	out := make([]whatifapi.FoldFit, 0, len(f.folds))
	for c, st := range f.folds {
		out = append(out, whatifapi.FoldFit{Cycle: c, At: st.At, Pricer: &servedState{st: st, decodeVariant: decodeVariant,
			prefillVariant: prefillVariant, hint: f.hintSource(), imbalanceAcrossCounts: f.imbalanceAcrossCounts, loadAwareRouter: f.loadAwareRouter,
			experimentalTTFT: f.experimentalTTFT, vpServiceLine: f.vpServiceLine, fLoadCurve: f.fLoadCurve}})
	}
	return out
}

// checkRecording self-checks the recording the CLI answered from, src at
// its state st, into f.checked: what the answer prints with it.
func (f *answerFlags) checkRecording(ctx context.Context, st *learnreplay.State, src *recordingSource) {
	if !f.selfCheckOn() {
		return
	}
	t0 := time.Now()
	ss := &servedSources{warn: io.Discard}
	info := whatifapi.SourceInfo{Kind: whatifapi.SourceRecording, Cycle: st.Cycle, TotalCycles: st.Cycles, FirstAt: st.FirstAt, LastAt: st.At}
	res, err := ss.resolved(ctx, f, st, src, info)
	if err != nil {
		f.checked = &whatifapi.SelfCheck{Status: whatifapi.SelfCheckNotRun, Why: err.Error(), Line: "Self-check not run: " + err.Error() + ".",
			Folds: []whatifapi.SelfCheckFold{}, Checks: []whatifapi.SelfCheckRule{}}
		return
	}
	stated, err := f.stated()
	if err != nil {
		f.checked = &whatifapi.SelfCheck{Status: whatifapi.SelfCheckNotRun, Why: err.Error(), Line: "Self-check not run: " + err.Error() + ".",
			Folds: []whatifapi.SelfCheckFold{}, Checks: []whatifapi.SelfCheckRule{}}
		return
	}
	ci := whatifapi.SelfCheckInfo{PlannerVersion: selfCheckVersion(), Stated: stated}
	if f.tpotDistribution {
		// The answer prices its TPOT percentiles with the experimental
		// model: so does the self-check, on the decode series it fits on.
		if err := decodeSeriesFrom(src.open, res); err != nil {
			f.checked = &whatifapi.SelfCheck{Status: whatifapi.SelfCheckNotRun, Why: err.Error(), Line: "Self-check not run: " + err.Error() + ".",
				Folds: []whatifapi.SelfCheckFold{}, Checks: []whatifapi.SelfCheckRule{}}
			return
		}
		res.Engine.DecodeSlots, res.Engine.SpecTokens = decodeEngineOf(res)
		ci.TPOTModel = f.tpotModel
	}
	f.checked = whatifapi.SelfCheckOf(ctx, res, ci)
	f.checked.Seconds = time.Since(t0).Seconds()
}

// stated is what the flags state in place of what the recording gave: the
// self-check replays it with the answer's own terms (issue
// llm-d-incubation/llm-d-scaling-manager#277).
func (f *answerFlags) stated() (*whatifapi.Stated, error) {
	a, err := f.assume()
	if err != nil {
		return nil, err
	}
	st := &whatifapi.Stated{Assume: a, CLI: true}
	if f.set["arrival-cv2"] {
		st.ArrivalCV2 = &f.arrivalCV2
	}
	if f.set["assume-input-tokens"] {
		st.PromptTokens = f.inputTokens
	}
	if f.set["assume-output-tokens"] {
		st.OutputTokens = f.outputTokens
	}
	// The router's flow control and wait replay as assume.flowControl and
	// assume.routerWait, as backtest's flags of the same names state them.
	if f.set["flow-control"] {
		st.Assume.FlowControl = f.flowControl
	}
	switch {
	case f.priceRouterWait == whatif.DefaultPriceRouterWait && f.routerWaitInPrefill == whatif.DefaultRouterWaitInPrefill:
	case f.priceRouterWait:
		st.Assume.RouterWait = whatifapi.RouterWaitGateStage
	case f.routerWaitInPrefill:
		st.Assume.RouterWait = whatifapi.RouterWaitInPrefill
	default:
		st.Assume.RouterWait = whatifapi.RouterWaitNone
	}
	// The fits price at the other switches' defaults (servedState.fleetAt):
	// what the answer set of them off its default is named as run without.
	for _, o := range []struct {
		name, value string
		moved       bool
	}{
		{"batch-cap", strconv.FormatInt(f.batchCap, 10), f.batchCap != 0},
		{"ps-onsets", strconv.FormatBool(f.psOnsets), f.psOnsets != whatif.DefaultPSOnsets},
		{"prefill-running-cap", strconv.Itoa(f.prefillRunningCap), f.prefillRunningCap != 0},
		{"charge-decode-gate", strconv.FormatBool(f.chargeDecodeGate), f.chargeDecodeGate != whatif.DefaultChargeDecodeGate},
		{"assume-transfer-base", f.assumeTransferBase.String(), f.assumeTransferBase != 0},
	} {
		if f.set[o.name] && o.moved {
			st.Other = append(st.Other, "--"+o.name+"="+o.value)
		}
	}
	// The fits do price with --imbalance-across-counts and --load-aware-router
	// (foldFits): replayed, and outside the validated region.
	if f.set["imbalance-across-counts"] && f.imbalanceAcrossCounts != whatif.DefaultPlannerImbalanceAcrossCounts {
		st.Replayed, st.Outside = append(st.Replayed, "--imbalance-across-counts="+strconv.FormatBool(f.imbalanceAcrossCounts)), true
	}
	if f.set["load-aware-router"] && f.loadAwareRouter != whatif.DefaultPlannerLoadAwareRouter {
		st.Replayed, st.Outside = append(st.Replayed, "--load-aware-router="+strconv.FormatBool(f.loadAwareRouter)), true
	}
	// The folds are read in the answer's own replay (readStates), so they
	// learn V_P from the plateau too.
	if f.vpPlateau {
		st.Replayed, st.Outside = append(st.Replayed, "--"+vpPlateauFlag), true
	}
	if f.hitAtCount {
		st.Replayed = append(st.Replayed, "--"+hitAtCountFlag)
	}
	if f.vpServiceLine {
		st.Replayed, st.Outside = append(st.Replayed, "--"+vpServiceLineFlag), true
	}
	if f.fLoadCurve {
		st.Replayed, st.Outside = append(st.Replayed, "--"+fLoadCurveFlag), true
	}
	if f.experimentalTTFT {
		st.Replayed, st.Outside = append(st.Replayed, "--"+experimentalTTFTFlag), true
	}
	return st, nil
}

// assume is the terms the --assume-* flags state, as the API's assume
// takes them.
func (f *answerFlags) assume() (whatifapi.Assume, error) {
	var a whatifapi.Assume
	if f.set["assume-vp"] {
		a.VP = &f.assumeVP
	}
	if f.set["assume-a"] {
		a.A = ptrTo(f.assumeA.Seconds())
	}
	if f.set["assume-prompt-cv"] {
		a.PromptCV = &f.assumePromptCV
	}
	if f.set["assume-router-hop"] {
		a.RouterHop = ptrTo(f.assumeRouterHop.Seconds())
	}
	if f.set["assume-tpot-ratio"] {
		a.TPOTRatio = &f.tpotRatio
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

// selfCheckHeld is how the CLI and the page say a self-check whose status
// is ok: the planner held on the recording's own steps.
const selfCheckHeld = "held on its own steps"

// selfCheckText is the self-check as the CLI prints it beside an answer:
// the line, and on a result short of ok why, what to trust, and the issue to
// review and file.
func selfCheckText(sc *whatifapi.SelfCheck) string {
	if sc == nil {
		return ""
	}
	var b strings.Builder
	status := sc.Status
	if status == whatifapi.SelfCheckOK {
		// ok means its own steps held, not that the figures are right.
		status = selfCheckHeld
	}
	fmt.Fprintf(&b, "%s [%s]\n", sc.Line, status)
	if sc.Status == whatifapi.SelfCheckWeak || sc.Status == whatifapi.SelfCheckInconsistent {
		fmt.Fprintf(&b, "  why: %s\n", sc.Why)
	}
	if sc.Trust != "" {
		fmt.Fprintf(&b, "  %s\n", sc.Trust)
	}
	if sc.IssueURL != "" {
		fmt.Fprintf(&b, "  to report it, review and file this issue (nothing is sent until you do; it holds no name and no recording):\n  %s\n", sc.IssueURL)
	}
	return b.String()
}

// selfCheckVersion is the release an issue is stamped with; the tests fix
// it, so that a golden does not hold the commit it was built at.
var selfCheckVersion = plannerVersion
