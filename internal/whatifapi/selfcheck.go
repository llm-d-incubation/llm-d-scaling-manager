package whatifapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The self-check: every answer read from a recording also checks the planner
// on that recording. The recording is cut at earlier cycles (folds); the
// planner fitted on each part up to its cut predicts the next part from its
// inputs alone, as a backtest does (the fit always ends before the part it
// predicts), and the predictions are scored against what the fleet
// measured. The verdict rides with the answer: ok, weak, inconsistent, or
// not run and why. An inconsistent recording says what its figures can and
// cannot be trusted for, and offers an issue a user reviews and files
// themselves, with diagnostics that hold no name and no recording.

// FoldFit is the planner fitted on a recording up to one of its cycles: the
// state there, priced as the answer's is.
type FoldFit struct {
	Cycle  int
	At     time.Time
	Pricer Pricer
}

// The self-check's statuses.
const (
	SelfCheckOK           = "ok"
	SelfCheckWeak         = "weak"
	SelfCheckInconsistent = "inconsistent"
	SelfCheckNotRun       = "notRun"
)

// The self-check's rules: a figure is inconsistent with its recording when
// its median ratio lies outside [selfCheckBiasLo, selfCheckBiasHi] or fewer
// than selfCheckWithinLo of its steps are within BacktestTolerance; weak
// when fewer than selfCheckWithinOK are, or it was scored over fewer than
// selfCheckStepsOK steps. Fewer than selfCheckMinSteps scored steps judge
// nothing. The planner calling the fleet overloaded in selfCheckSaturated
// or more of the judged steps, where the fleet measured a figure, is
// inconsistent too.
const (
	selfCheckBiasLo    = 0.8
	selfCheckBiasHi    = 1.25
	selfCheckWithinLo  = 0.5
	selfCheckWithinOK  = 0.7
	selfCheckMinSteps  = 6
	selfCheckStepsOK   = 12
	selfCheckSaturated = 0.5
	// selfCheckMinSpan is the shortest recording self-checked.
	selfCheckMinSpan = time.Hour
)

// selfCheckFractions are the folds' cuts, shares of the cycles up to the
// one answered: the planner fitted up to each predicts up to the next.
var selfCheckFractions = []float64{0.5, 0.75}

// SelfCheckCycles is the cycles a recording answered at cycle is cut at.
func SelfCheckCycles(cycle int) []int {
	out := make([]int, 0, len(selfCheckFractions))
	for _, f := range selfCheckFractions {
		if c := int(math.Round(f * float64(cycle))); c >= 1 && c < cycle && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// SelfCheck is a recording's check of the planner on itself.
type SelfCheck struct {
	Status string `json:"status"`
	// Line is the result in one sentence; Why what made the status, and
	// Trust what the answer's figures can and cannot be trusted for.
	Line  string `json:"line"`
	Why   string `json:"why,omitempty"`
	Trust string `json:"trust,omitempty"`
	// TTFT and TPOT are the p90 figures scored over every fold's judged
	// steps; absent where not scored.
	TTFT *SelfCheckFigure `json:"ttft,omitempty"`
	TPOT *SelfCheckFigure `json:"tpot,omitempty"`
	// Folds is each cut: what was fitted, what was predicted.
	Folds []SelfCheckFold `json:"folds"`
	// Checks is each rule, passed or not.
	Checks []SelfCheckRule `json:"checks"`
	// IssueURL is a new-issue page, prefilled with diagnostics that hold no
	// name, for an inconsistent recording; the user reviews and files it.
	IssueURL string `json:"issueUrl,omitempty"`
	// Seconds is what the self-check took, its fits' replay apart (shared
	// with the answer's).
	Seconds float64 `json:"seconds"`
	// Stated is the answer's stated inputs the recording was replayed with,
	// in the words of the question's source: every figure they move is
	// flagged. WithoutStated is the answer's stated inputs the self-check
	// ran without. Each is absent with none.
	Stated        []string `json:"stated,omitempty"`
	WithoutStated []string `json:"withoutStated,omitempty"`
	// notUsed is what the line adds of a stated ITL B the fits did not use;
	// outside says a replayed term puts the figures outside the validated
	// region.
	notUsed string
	outside bool
}

// StatedITLBNotUsed opens the words of a refusal whose stated ITL B was not
// used (a held line, a floor that gives no intercept), as a Pricer writes
// them: the self-check counts its steps.
const StatedITLBNotUsed = "ITL B not assumed ("

// tail is what the line adds after its result: a stated ITL B the fits did
// not use, and the stated inputs the self-check ran without.
func (sc *SelfCheck) tail() string {
	return sc.notUsed + withoutStatedSentence(sc.WithoutStated)
}

// itlBNotUsed is how many judged steps a stated ITL B was not used in, at
// p90 TPOT, and the first one's why; 0 with none.
func itlBNotUsed(steps []BacktestStep) (int, string) {
	n, why := 0, ""
	for _, st := range steps {
		if st.Excluded != "" {
			continue
		}
		for _, p := range st.TPOT {
			r := p.Predicted.Refused
			if p.Statistic != statP90 || r == nil {
				continue
			}
			i := strings.Index(r.Detail, StatedITLBNotUsed)
			if i < 0 {
				continue
			}
			if n++; why == "" {
				why = r.Detail[i+len(StatedITLBNotUsed):]
				if j := strings.Index(why, "): "); j >= 0 {
					why = why[j+len("): "):]
				}
			}
			break
		}
	}
	return n, why
}

// Stated is what an answer stated in place of what its source gave. The
// self-check replays the recording with the planner's terms and the
// router's flow control and wait (Assume, but TPOTRatio) and the arrivals'
// Ca^2, which price the recording's own traffic; it runs without a TPOT ratio, one percentile's where the check
// scores p90 on the recording's own, and without stated lengths, a traffic
// other than the one it scores against.
type Stated struct {
	Assume     Assume
	ArrivalCV2 *float64
	// PromptTokens and OutputTokens are stated mean lengths, 0 the
	// observed ones.
	PromptTokens, OutputTokens float64
	// Other is what else the answer set that its fits do not price with
	// (the CLI's opt-in switches, a stated batch cap), in words: the
	// self-check names it as run without. Replayed is what else the fits
	// do price with, in words, and Outside says it puts the figures
	// outside the validated region.
	Other    []string
	Replayed []string
	Outside  bool
	// CLI names them as the CLI's flags; false, as the API's keys.
	CLI bool
	// arrivalKey is the API's key of ArrivalCV2; empty, arrivalCV2.
	arrivalKey string
}

// outside says the terms st replays put a figure outside the validated
// region (the CLI's flagsOutside, and a stated B, which does once it
// applies); a stated prompt spread and arrivals are inside.
func (st *Stated) outside() bool {
	if st == nil {
		return false
	}
	a := st.Assume
	return st.Outside || a.VP != nil || a.A != nil || a.RouterHop != nil || a.ITLB != nil || a.ITLBFromFloor || a.RouterWait != ""
}

// replayed is the request terms the self-check replays st with, and their
// words.
func (st *Stated) replayed() (BacktestRequest, []string) {
	var req BacktestRequest
	if st == nil {
		return req, nil
	}
	a := st.Assume
	a.TPOTRatio = nil
	if a != (Assume{}) {
		req.Assume = &a
	}
	req.ArrivalCV2 = st.ArrivalCV2
	return req, append(st.words(a, st.ArrivalCV2, 0, 0), st.Replayed...)
}

// notReplayed is the words of st the self-check runs without.
func (st *Stated) notReplayed() []string {
	if st == nil {
		return nil
	}
	return append(st.words(Assume{TPOTRatio: st.Assume.TPOTRatio}, nil, st.PromptTokens, st.OutputTokens), st.Other...)
}

// all is every input st states, in words.
func (st *Stated) all() []string {
	if st == nil {
		return nil
	}
	return append(append(st.words(st.Assume, st.ArrivalCV2, st.PromptTokens, st.OutputTokens), st.Replayed...), st.Other...)
}

// words names the inputs given, as the CLI's flags or the API's keys, each
// with its value.
func (st *Stated) words(a Assume, cv2 *float64, prompt, output float64) []string {
	var out []string
	num := func(x float64) string { return strconv.FormatFloat(x, 'g', -1, 64) }
	dur := func(x float64) string { return time.Duration(math.Round(x * float64(time.Second))).String() }
	add := func(flag, key string, x *float64, cli func(float64) string) {
		switch {
		case x == nil:
		case st.CLI:
			out = append(out, flag+" "+cli(*x))
		default:
			out = append(out, key+" "+num(*x))
		}
	}
	add("--assume-vp", "assume.vp", a.VP, num)
	add("--assume-a", "assume.a", a.A, dur)
	add("--assume-prompt-cv", "assume.promptCV", a.PromptCV, num)
	add("--assume-router-hop", "assume.routerHop", a.RouterHop, dur)
	add("--assume-tpot-ratio", "assume.tpotRatio", a.TPOTRatio, num)
	switch {
	case a.ITLBFromFloor && st.CLI:
		out = append(out, "--assume-itl-b floor")
	case a.ITLBFromFloor:
		out = append(out, "assume.itlBFromFloor")
	}
	add("--assume-itl-b", "assume.itlB", a.ITLB, dur)
	switch {
	case a.FlowControl == "":
	case st.CLI:
		out = append(out, "--flow-control "+a.FlowControl)
	default:
		out = append(out, "assume.flowControl "+a.FlowControl)
	}
	switch {
	case a.RouterWait == "":
	case !st.CLI:
		out = append(out, "assume.routerWait "+a.RouterWait)
	case a.RouterWait == RouterWaitInPrefill:
		out = append(out, "--router-wait-in-prefill")
	case a.RouterWait == RouterWaitGateStage:
		out = append(out, "--price-router-wait")
	default:
		out = append(out, "--router-wait-in-prefill=false --price-router-wait=false")
	}
	arrivalKey := st.arrivalKey
	if arrivalKey == "" {
		arrivalKey = "arrivalCV2"
	}
	add("--arrival-cv2", arrivalKey, cv2, num)
	if prompt > 0 {
		add("--assume-input-tokens", "traffic.mix.promptTokens", &prompt, num)
	}
	if output > 0 {
		add("--assume-output-tokens", "traffic.mix.outputTokens", &output, num)
	}
	return out
}

// RanWithout is sc as the answer of an input st states prints it: a
// self-check read with the source, before any answer stated anything, ran
// without every input st states, and says so. sc is not changed; with
// nothing stated it is returned as it is.
func (sc *SelfCheck) RanWithout(st *Stated) *SelfCheck {
	words := st.all()
	if sc == nil || len(words) == 0 {
		return sc
	}
	out := *sc
	out.WithoutStated = words
	if sc.Status == SelfCheckNotRun {
		out.Line = sc.Line + notRunWithSentence(words)
	} else {
		out.Line = sc.Line + withoutStatedSentence(words)
	}
	return &out
}

// notRunWithSentence is what the line of a self-check not run adds of the
// stated inputs it was not run with.
func notRunWithSentence(words []string) string {
	return " It was not run with the stated " + strings.Join(words, ", ") + " either."
}

// withoutStatedSentence is what a self-check's line adds of the stated
// inputs it ran without.
func withoutStatedSentence(words []string) string {
	if len(words) == 0 {
		return ""
	}
	return " It ran without the stated " + strings.Join(words, ", ") + ": what the answer prices on them is not checked."
}

// selfCheckNotRunHead opens the line of a self-check not run.
const selfCheckNotRunHead = "Self-check not run"

// lead is the line's opening, head, naming the stated inputs the recording
// was replayed with.
func (sc *SelfCheck) lead(head string) string {
	if len(sc.Stated) == 0 {
		return head
	}
	flag := "flagged as stated"
	if sc.outside {
		flag += ", outside the validated region"
	}
	if head == selfCheckNotRunHead {
		return fmt.Sprintf("Self-check with the stated %s (%s) not run", strings.Join(sc.Stated, ", "), flag)
	}
	return fmt.Sprintf("%s, replayed with the stated %s (%s)", head, strings.Join(sc.Stated, ", "), flag)
}

// SelfCheckFigure is one figure's score over the folds.
type SelfCheckFigure struct {
	Statistic string   `json:"statistic"`
	Steps     int      `json:"steps"`
	Within    int      `json:"within"`
	Bias      *float64 `json:"bias,omitempty"`
	Low       int      `json:"low"`
	// Judged is the folds' judged steps; Overloaded those the planner gave
	// no figure for, calling the fleet overloaded, and PastKnee what the
	// fleet measured there.
	Judged     int   `json:"judged"`
	Overloaded int   `json:"overloaded"`
	PastKnee   *Band `json:"pastKnee,omitempty"`
	// Refused is the other refusals, by reason.
	Refused map[string]int `json:"refused"`
}

// SelfCheckFold is one fold: the fit up to FitTo, the test from TestFrom to
// TestTo.
type SelfCheckFold struct {
	FitTo    time.Time `json:"fitTo"`
	TestFrom time.Time `json:"testFrom"`
	TestTo   time.Time `json:"testTo"`
	Judged   int       `json:"judged"`
}

// SelfCheckRule is one rule of the self-check.
type SelfCheckRule struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// SelfCheckInfo is what a self-check is stamped with.
type SelfCheckInfo struct {
	PlannerVersion string
	// Now is the clock the cost is read with; nil is time.Now.
	Now func() time.Time
	// Stated is what the answer stated, nil nothing: the self-check
	// replays the recording with what of it prices the recording's own
	// traffic, and names the rest it ran without (Stated).
	Stated *Stated
}

// SelfCheckOf checks the planner on the recording res was resolved from,
// with the fits res.Folds holds.
func SelfCheckOf(ctx context.Context, res *Resolved, info SelfCheckInfo) *SelfCheck {
	doc := res.Inputs
	e := &inputsEntry{id: contentID("inputs", res.Content), doc: doc, pricer: res.Pricer, series: res.Series, latency: res.Latency, config: res.Config}
	return selfCheckEntry(ctx, e, res.Folds, info)
}

// selfCheckEntry is SelfCheckOf on an inputs entry.
func selfCheckEntry(ctx context.Context, e *inputsEntry, folds []FoldFit, info SelfCheckInfo) *SelfCheck {
	now := time.Now
	if info.Now != nil {
		now = info.Now
	}
	t0 := now()
	sc := &SelfCheck{Status: SelfCheckNotRun, Folds: []SelfCheckFold{}, Checks: []SelfCheckRule{}}
	defer func() { sc.Seconds = now().Sub(t0).Seconds() }()
	notRun := func(why string) *SelfCheck {
		sc.Why = why
		sc.Line = sc.lead(selfCheckNotRunHead) + ": " + why + "." + sc.tail()
		if sc.Stated == nil {
			// Stopped before any replay: nothing stated was replayed.
			if words := info.Stated.all(); len(words) > 0 {
				sc.WithoutStated = words
				sc.Line += notRunWithSentence(words)
			}
		}
		return sc
	}
	short := EngineRowsShort(e.series)
	switch {
	case len(e.series) < 2:
		return notRun("the recording holds too few cycles")
	case e.series[len(e.series)-1].At.Sub(e.series[0].At) < selfCheckMinSpan:
		return notRun(fmt.Sprintf("the recording spans %s; a self-check needs %s or more", durationWords(e.series[len(e.series)-1].At.Sub(e.series[0].At)),
			durationWords(selfCheckMinSpan)))
	case !HasLatency(e.latency):
		return notRun("the recording carries no measured latency to check against")
	case e.doc.Fleet.Prefill == nil:
		return notRun("the planner prices a P/D fleet, and the recording has no prefill variant")
	case len(folds) == 0:
		return notRun("no fit was read at an earlier cycle")
	case short != "":
		// A backstop: the CLI and the server refuse such a recording
		// before a self-check runs.
		return notRun(short)
	}
	req, stated := info.Stated.replayed()
	sc.Stated, sc.WithoutStated, sc.outside = stated, info.Stated.notReplayed(), info.Stated.outside()
	folds = slices.Clone(folds)
	slices.SortFunc(folds, func(a, b FoldFit) int { return a.At.Compare(b.At) })
	answered := e.series[len(e.series)-1].At
	if c := e.doc.Source.Cycle; c > 0 && c <= len(e.series) {
		answered = e.series[c-1].At
	}
	var steps []BacktestStep
	var refusals []string
	for i, f := range folds {
		until := answered
		if i+1 < len(folds) {
			until = folds[i+1].At
		}
		fit := &inputsEntry{id: e.id + "#fit", doc: e.doc, pricer: f.Pricer, series: e.series, config: e.config}
		q, err := questionOf(fit, e, req, f.At, until, "self-check", false)
		if err != nil {
			refusals = append(refusals, err.Error())
			sc.Checks = append(sc.Checks, SelfCheckRule{Name: "fold", Passed: false,
				Detail: fmt.Sprintf("the fold fitted to cycle %d was not backtested: %v", f.Cycle, err)})
			continue
		}
		out, err := q.run(ctx)
		if err != nil {
			if ctx.Err() != nil {
				if errors.Is(context.Cause(ctx), errPlanBudget) {
					return notRun("the self-check ran out of its time budget")
				}
				return notRun("stopped: " + ctx.Err().Error())
			}
			refusals = append(refusals, err.Error())
			continue
		}
		steps = append(steps, out.Steps...)
		sc.Folds = append(sc.Folds, SelfCheckFold{FitTo: f.At, TestFrom: out.Test.From, TestTo: out.Test.To, Judged: out.Summary.Judged})
	}
	if len(sc.Folds) == 0 {
		return notRun("no fold could be backtested (" + strings.Join(refusals, "; ") + ")")
	}
	if n, why := itlBNotUsed(steps); n > 0 && len(sc.Stated) > 0 {
		sc.notUsed = fmt.Sprintf(" The stated ITL B was not used in %d steps: %s.", n, strings.TrimSuffix(why, "."))
	}
	sum := summarize(steps, false)
	sc.TTFT, sc.TPOT = selfCheckFigure(sum.TTFT, sum.Judged), selfCheckFigure(sum.TPOT, sum.Judged)
	span := sc.Folds[len(sc.Folds)-1].TestTo.Sub(sc.Folds[0].FitTo)
	sc.judge(e, span, info)
	return sc
}

// selfCheckFigure is the p90 score among scores, over judged steps.
func selfCheckFigure(scores []BacktestScore, judged int) *SelfCheckFigure {
	i := slices.IndexFunc(scores, func(x BacktestScore) bool { return x.Statistic == statP90 })
	if i < 0 {
		return nil
	}
	s := scores[i]
	f := &SelfCheckFigure{Statistic: s.Statistic, Steps: s.All.Steps, Bias: s.All.Bias, Low: s.All.Low, Judged: judged,
		Overloaded: s.Refused[string(whatif.Saturated)], PastKnee: s.PastKnee, Refused: map[string]int{}}
	if s.All.Within != nil {
		f.Within = int(math.Round(*s.All.Within * float64(s.All.Steps)))
	}
	for r, n := range s.Refused {
		if r != string(whatif.Saturated) {
			f.Refused[r] = n
		}
	}
	return f
}

// judge sets the status, the line, why, what to trust and the issue.
func (sc *SelfCheck) judge(e *inputsEntry, span time.Duration, info SelfCheckInfo) {
	bad, weak, judged := []string{}, []string{}, []string{}
	trustOK, trustNot := []string{}, []string{}
	parts := []string{}
	for _, x := range []struct {
		name, what string
		f          *SelfCheckFigure
	}{{"TTFT", "prefill sizing and the TTFT figures", sc.TTFT}, {"TPOT", "decode sizing and the TPOT figures", sc.TPOT}} {
		f := x.f
		if f == nil {
			continue
		}
		if f.Judged > 0 && f.PastKnee != nil && float64(f.Overloaded) >= selfCheckSaturated*float64(f.Judged) {
			rule := SelfCheckRule{Name: strings.ToLower(x.name) + "-overloaded", Passed: false,
				Detail: fmt.Sprintf("the planner called the fleet overloaded in %d of %d judged steps, where the fleet served with a p90 %s of %s to %s",
					f.Overloaded, f.Judged, x.name, secondsWords(f.PastKnee.Lo), secondsWords(f.PastKnee.Hi))}
			sc.Checks = append(sc.Checks, rule)
			bad = append(bad, rule.Detail)
			trustNot = append(trustNot, x.what)
			parts = append(parts, fmt.Sprintf("%s: called the fleet overloaded in %d of %d steps it served", x.name, f.Overloaded, f.Judged))
			judged = append(judged, x.name)
			continue
		}
		if f.Steps < selfCheckMinSteps || f.Bias == nil {
			sc.Checks = append(sc.Checks, SelfCheckRule{Name: strings.ToLower(x.name) + "-scored", Passed: false,
				Detail: fmt.Sprintf("p90 %s scored in %d steps, fewer than %d: not judged", x.name, f.Steps, selfCheckMinSteps)})
			continue
		}
		judged = append(judged, x.name)
		within := float64(f.Within) / float64(f.Steps)
		parts = append(parts, fmt.Sprintf("p90 %s within 20%% in %d of %d steps (median ratio %.2f)", x.name, f.Within, f.Steps, *f.Bias))
		biasOK := *f.Bias >= selfCheckBiasLo && *f.Bias <= selfCheckBiasHi
		sc.Checks = append(sc.Checks,
			SelfCheckRule{Name: strings.ToLower(x.name) + "-bias", Passed: biasOK,
				Detail: fmt.Sprintf("median ratio %.2f, want %.2f to %.2f", *f.Bias, selfCheckBiasLo, selfCheckBiasHi)},
			SelfCheckRule{Name: strings.ToLower(x.name) + "-within", Passed: within >= selfCheckWithinLo,
				Detail: fmt.Sprintf("within 20%% in %.0f%% of the steps, want %.0f%% or more", within*100, selfCheckWithinLo*100)})
		switch {
		case !biasOK || within < selfCheckWithinLo:
			bad = append(bad, fmt.Sprintf("p90 %s: median ratio %.2f, within 20%% in %.0f%% of %d steps", x.name, *f.Bias, within*100, f.Steps))
			trustNot = append(trustNot, x.what)
		case within < selfCheckWithinOK || f.Steps < selfCheckStepsOK:
			weak = append(weak, fmt.Sprintf("p90 %s within 20%% in %.0f%% of %d steps", x.name, within*100, f.Steps))
			trustOK = append(trustOK, x.what+" (loosely)")
		default:
			trustOK = append(trustOK, x.what)
		}
	}
	if len(judged) == 0 {
		sc.Status, sc.Why = SelfCheckNotRun, "too few steps were scored: a longer recording, with busy minutes in its first half, gives the planner more to fit and to check"
		var why []string
		for _, x := range []struct {
			name string
			f    *SelfCheckFigure
		}{{"TTFT", sc.TTFT}, {"TPOT", sc.TPOT}} {
			if x.f == nil {
				continue
			}
			for _, r := range slices.Sorted(maps.Keys(x.f.Refused)) {
				why = append(why, fmt.Sprintf("%s refused %s in %d steps", x.name, r, x.f.Refused[r]))
			}
		}
		if len(why) > 0 {
			sc.Why += " (" + strings.Join(why, "; ") + ")"
		}
		sc.Line = sc.lead(selfCheckNotRunHead) + ": " + sc.Why + "." + sc.tail()
		return
	}
	sc.Line = fmt.Sprintf("%s: tested on the last %s of this recording, each part predicted from a fit on what came before it, %s.",
		sc.lead("Self-check"), durationWords(span), strings.Join(parts, "; ")) + sc.tail()
	switch {
	case len(bad) > 0:
		sc.Status = SelfCheckInconsistent
		sc.Why = "the planner and this recording disagree: " + strings.Join(bad, "; ")
	case len(weak) > 0:
		sc.Status, sc.Why = SelfCheckWeak, "few or loose agreements: "+strings.Join(weak, "; ")
	default:
		sc.Status = SelfCheckOK
	}
	switch {
	case len(trustNot) > 0 && len(trustOK) > 0:
		sc.Trust = fmt.Sprintf("Do not trust %s from this recording; %s held.", strings.Join(trustNot, " or "), strings.Join(trustOK, " and "))
	case len(trustNot) > 0:
		sc.Trust = fmt.Sprintf("Do not trust %s from this recording.", strings.Join(trustNot, " or "))
	}
	if sc.Status == SelfCheckInconsistent {
		sc.IssueURL = selfCheckIssueURL(sc, e, info)
	}
}

// selfCheckIssueRepo is the repository a self-check's issue is offered in.
const selfCheckIssueRepo = "https://github.com/llm-d-incubation/llm-d-scaling-manager/issues/new"

// selfCheckIssueTemplate is the issue form the URL fills.
const selfCheckIssueTemplate = "planner-self-check.yml"

// selfCheckIssueURL is a new-issue page prefilled with the self-check's
// diagnostics: the planner's version, the rules that failed with their
// numbers, the refusals, the shape's token-size classes, the fleet's roles
// and replica counts, and the engine and router versions. It holds no
// model, namespace, variant or pod name, no recording, and no rate.
func selfCheckIssueURL(sc *SelfCheck, e *inputsEntry, info SelfCheckInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "planner: %s\nself-check: %s\n", info.PlannerVersion, sc.Status)
	if len(sc.Stated) > 0 {
		fmt.Fprintf(&b, "stated: %s\n", strings.Join(sc.Stated, ", "))
	}
	for _, r := range sc.Checks {
		switch {
		case r.Passed:
		case r.Name == "fold":
			// Its detail carries an error's free text: the name alone.
			b.WriteString("failed: fold: not backtested\n")
		default:
			fmt.Fprintf(&b, "failed: %s: %s\n", r.Name, r.Detail)
		}
	}
	for _, x := range []struct {
		name string
		f    *SelfCheckFigure
	}{{"ttft", sc.TTFT}, {"tpot", sc.TPOT}} {
		if x.f == nil {
			continue
		}
		bias := "-"
		if x.f.Bias != nil {
			bias = fmt.Sprintf("%.2f", *x.f.Bias)
		}
		fmt.Fprintf(&b, "%s p90: steps %d, within20 %d, median ratio %s, read low %d, judged %d, overloaded %d", x.name, x.f.Steps, x.f.Within,
			bias, x.f.Low, x.f.Judged, x.f.Overloaded)
		for _, r := range slices.Sorted(maps.Keys(x.f.Refused)) {
			fmt.Fprintf(&b, ", refused %s %d", r, x.f.Refused[r])
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "shape: prompt %s tokens, output %s tokens\n", tokenClass(figureValue(e.doc.Observed.PromptTokens)),
		tokenClass(figureValue(e.doc.Observed.OutputTokens)))
	fleet := fmt.Sprintf("decode %d", e.doc.Fleet.Decode.Replicas)
	if p := e.doc.Fleet.Prefill; p != nil {
		fleet = fmt.Sprintf("prefill %d, %s", p.Replicas, fleet)
	}
	fmt.Fprintf(&b, "fleet: %s; flow control %s\n", fleet, e.doc.Policy.FlowControl)
	fmt.Fprintf(&b, "engine: %s; router: not recorded\n", engineWords(e.doc.Fleet))
	if n := len(sc.Folds); n > 0 {
		fmt.Fprintf(&b, "folds: %d, span %s\n", n, durationWords(sc.Folds[n-1].TestTo.Sub(sc.Folds[0].FitTo)))
	}
	v := url.Values{}
	v.Set("template", selfCheckIssueTemplate)
	v.Set("title", "planner self-check: "+sc.Status)
	v.Set("diagnostics", b.String())
	return selfCheckIssueRepo + "?" + v.Encode()
}

// figureValue is f's value, 0 where refused.
func figureValue(f Figure) float64 {
	if f.Value == nil {
		return 0
	}
	return *f.Value
}

// tokenClass is a length in tokens as a class: a power-of-two band.
func tokenClass(x float64) string {
	if !(x > 0) {
		return "unknown"
	}
	lo := 1.0
	for lo*2 <= x {
		lo *= 2
	}
	return fmt.Sprintf("%g-%g", lo, lo*2)
}

// engineWords is the roles' engines and vLLM releases, no image or name.
func engineWords(f FleetInfo) string {
	role := func(name string, r *RoleInfo) string {
		if r == nil || r.Engine == nil {
			return name + " not recorded"
		}
		// A release is digits and dots; anything else, which a build may
		// name, is not written.
		rel := r.Engine.VLLMVersion
		switch {
		case rel == "":
			rel = "release not recorded"
		case strings.Trim(rel, "0123456789.") != "":
			rel = "release not a plain version"
		}
		// An engine is a word: vllm, sglang; anything else is not written.
		engine := r.Engine.Engine
		if engine == "" || strings.Trim(engine, "abcdefghijklmnopqrstuvwxyz") != "" || len(engine) > 12 {
			engine = "other"
		}
		return fmt.Sprintf("%s %s %s", name, engine, rel)
	}
	return role("prefill", f.Prefill) + ", " + role("decode", &f.Decode)
}
