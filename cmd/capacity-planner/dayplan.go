package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// defaultDayWindow is day-plan's Prometheus window when --from and --to are
// left out: the last day, ending now.
const defaultDayWindow = 24 * time.Hour

// dayPlanFlags are day-plan's flags.
type dayPlanFlags struct {
	window                                   captureFlags
	recording, model, prefillVariant, output string
	decodeVariant, ttft, tpot, bucket        string
	sizeFor                                  string
	lead                                     string
	minRequests, headroom                    float64
	maxPrefill, maxDecode                    int
	assumeVP, assumePromptCV                 float64
	assumeA, assumeRouterHop                 time.Duration
	inputTokens, outputTokens                float64
	// assumeITLB is --assume-itl-b, as answerFlags' (itl_b.go), and
	// assumeITLBSet says it was given, empty or not.
	assumeITLB    string
	assumeITLBSet bool
	noSelfCheck   bool
	// vpPlateau is --prefill-vp-plateau, as answerFlags' (vp_plateau.go).
	vpPlateau bool
	// hitAtCount is --prefill-hit-at-count, as answerFlags'
	// (hit_at_count.go).
	hitAtCount bool
	// vpServiceLine is --prefill-vp-service-line, as answerFlags'
	// (vp_service_line.go).
	vpServiceLine bool
	// fLoadCurve is --prefill-f-load-curve, as answerFlags'
	// (f_load_curve.go).
	fLoadCurve bool
	// experimentalTTFT is --experimental-ttft, as answerFlags'
	// (experimental_ttft.go).
	experimentalTTFT bool
	// rankMixtureTTFT is --experimental-ttft=v2 and experimentalTPOT
	// --experimental-tpot=v2: the plan is priced with those experimental
	// models, fitted on the source (whatifapi.DayPlanRequest.ExperimentalModels),
	// as at-replicas and a served plan price with them.
	rankMixtureTTFT  bool
	experimentalTPOT string
	tpotDistribution bool
	// tpotModel is the distribution --experimental-tpot names.
	tpotModel string
	// rankMixtureNLLine is --experimental-ttft=v2nl-line, and
	// rankMixtureFit the variant's modes and the shortest fit window
	// (rank_mixture_fit.go): the TTFT model is fitted as serve fits it.
	rankMixtureNLLine bool
	rankMixtureFit    rankMixtureFit
	// tpotFit is --tpot-dist-min-fit-steps (rank_mixture_fit.go).
	tpotFit tpotDistFit
}

// runDayPlan is `capacity-planner day-plan`: the prefill and decode replicas
// each bucket of a window needed, against the replicas the fleet had, priced
// as POST /v1/dayplan prices them.
func runDayPlan(ctx context.Context, args []string, out, warn io.Writer) error {
	fs := newFlags("day-plan", out)
	var f dayPlanFlags
	f.window.registerWindow(fs)
	fs.Lookup("prometheus").Usage = "plan from a window of this Prometheus (base URL), captured and generated in memory, in place of --recording"
	fs.Lookup("from").Usage = "with --prometheus, the window's start (RFC 3339 or Unix seconds); default a day before --to"
	fs.Lookup("to").Usage = "with --prometheus, the window's end (RFC 3339 or Unix seconds); default now"
	fs.Lookup("step").Usage = "with --prometheus, the interval between captured cycles (not the bucket: see --bucket)"
	fs.StringVar(&f.window.namespace, "namespace", "", "the model's namespace (with --prometheus, the namespace to capture: required)")
	fs.IntVar(&f.window.batchCap, "batch-cap", 0, "with --prometheus, the engine's batch cap the window is captured with")
	fs.StringVar(&f.recording, "recording", "", "a learner recording (.jsonl or .jsonl.gz); this or --prometheus is required")
	fs.BoolVar(&f.noSelfCheck, "no-self-check", false, "do not check the planner on the recording beside the plan")
	fs.StringVar(&f.model, "model", "", "the model; empty takes the source's only model")
	fs.StringVar(&f.prefillVariant, "prefill-variant", "", "the prefill variant, when the model has several")
	fs.StringVar(&f.decodeVariant, "variant", "", "the decode variant, when the model has several")
	fs.StringVar(&f.ttft, "ttft", "", "the TTFT target: a mean (500ms) or a percentile (500ms@p95); default the policy's")
	fs.StringVar(&f.tpot, "tpot", "", "the TPOT target: a mean (4ms) or a percentile (4ms@p90), or none; default the policy's")
	fs.Float64Var(&f.headroom, "headroom", 0, "the operating point as a fraction of the knee; default the policy's")
	fs.StringVar(&f.bucket, "bucket", whatifapi.DefaultDayPlanStep.String(), "the length of a bucket, each sized on its own")
	fs.StringVar(&f.sizeFor, "size-for", "", "the rate of a bucket to size for: mean, p95, p95-modulation (the p95 less counting noise) or max of its cycles; "+
		"default p95-modulation, but the mean, less conservative and flagged FewCyclesForP95, for a bucket of fewer than 20 cycles")
	fs.StringVar(&f.lead, "lead", "", "each role's scale-up lead time, the replicas held ahead of the buckets that need them: one duration for both roles (90s), "+
		"or prefill=3m,decode=90s; a role not given takes the start time the source learned, else none")
	fs.Float64Var(&f.minRequests, "min-requests", whatifapi.DefaultMinRequests, "the fewest requests a bucket must hold to be sized")
	fs.IntVar(&f.maxPrefill, "max-prefill", 0, "the most prefill replicas a bucket is sized to; 0 is 32")
	fs.IntVar(&f.maxDecode, "max-decode", 0, "the most decode replicas a bucket is sized to; 0 is 32")
	fs.Float64Var(&f.assumeVP, "assume-vp", 0, "V_P, a prefill pod's prompt tokens per second, in place of the learned one (flagged)")
	fs.DurationVar(&f.assumeA, "assume-a", 0, "a, the fixed latency on a prefill pod, in place of the learned one (flagged)")
	fs.Float64Var(&f.assumePromptCV, "assume-prompt-cv", 0, "the prompt's coefficient of variation, 0 to 3, in place of the measured one or the default 2 (flagged)")
	fs.DurationVar(&f.assumeRouterHop, "assume-router-hop", 0, "the router hop, in place of the learned one (flagged)")
	fs.StringVar(&f.assumeITLB, "assume-itl-b", "", "the decode line's intercept B (12ms), or floor for B from the floor read below the band, "+
		"where the source measured none (PinnedDefaultB); the slope is refitted through it (flagged AssumedITLB)")
	fs.Float64Var(&f.inputTokens, "assume-input-tokens", 0, "price every bucket at this mean prompt, in place of each bucket's own (flagged; EXTRAPOLATED outside the observed)")
	fs.Float64Var(&f.outputTokens, "assume-output-tokens", 0, "price every bucket at this mean output, in place of each bucket's own (flagged; EXTRAPOLATED outside the observed)")
	fs.BoolVar(&f.vpPlateau, vpPlateauFlag, false, vpPlateauUsage)
	fs.BoolVar(&f.hitAtCount, hitAtCountFlag, false, hitAtCountUsage)
	fs.BoolVar(&f.vpServiceLine, vpServiceLineFlag, false, vpServiceLineUsage)
	fs.BoolVar(&f.fLoadCurve, fLoadCurveFlag, false, fLoadCurveUsage)
	fs.Var(experimentalTTFTValue{switches: &f.experimentalTTFT, v2: &f.rankMixtureTTFT, nlLine: &f.rankMixtureNLLine}, experimentalTTFTFlag, experimentalTTFTUsage+
		". =v2 instead sizes with the experimental rank-mixture TTFT model fitted on the recording, as at-replicas and serve price with it, "+
		"each bucket at its own rate and its own request shape read over at least an hour and 16000 requests around it, or at the window's (WindowShape) where that shape is outside the model's fit (ShapeOutsideFit) or the requests too few (ShapeFewRequests): where it prices TTFT the planner refuses, prefill is sized on it; every figure it sets is flagged RankMixtureTTFT and reads OUTSIDE VALIDATED REGION; "+experimentalTTFTNLLineUsage)
	f.rankMixtureFit.registerModes(fs)
	f.rankMixtureFit.registerWindow(fs)
	f.tpotFit.register(fs)
	fs.StringVar(&f.experimentalTPOT, experimentalTPOTFlag, "", experimentalTPOTUsage+"; on day-plan it sizes decode with it at a p90 or p95 TPOT target, its ratio at the window's output lengths around the planner's mean at each bucket's own")
	fs.StringVar(&f.output, "o", outputText, "output: text or json")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	model, err := experimentalTPOTOf(f.experimentalTPOT)
	if err != nil {
		return err
	}
	f.tpotDistribution, f.tpotModel = model != "", model
	if err := f.rankMixtureFit.check(f.rankMixtureTTFT, f.rankMixtureNLLine); err != nil {
		return err
	}
	if err := f.tpotFit.check(f.tpotDistribution); err != nil {
		return err
	}
	warnKVHoldAlone(warn, f.rankMixtureFit.kvHold, f.rankMixtureFit.remainderSpread)
	if (f.rankMixtureTTFT || f.tpotDistribution) && f.recording == "" {
		return usagef("--%s=v2 and --%s=v2 fit their model on a --recording", experimentalTTFTFlag, experimentalTPOTFlag)
	}
	if f.output != outputText && f.output != outputJSON {
		return usagef("-o %q, want text or json", f.output)
	}
	fs.Visit(func(fl *flag.Flag) { f.assumeITLBSet = f.assumeITLBSet || fl.Name == "assume-itl-b" })
	// The analyzers the replay runs log through controller-runtime, which
	// the CLI never shows.
	ctrllog.SetLogger(logr.Discard())
	// The flags are checked before the replay, which can take minutes: what
	// the question asks wrongly is refused at once.
	req, err := f.request()
	if err != nil {
		return err
	}
	if err := whatifapi.CheckDayPlan(req); err != nil {
		return usageError{err}
	}
	res, err := resolveDay(ctx, &f, warn)
	if err != nil {
		return err
	}
	warnTPOTPercentile(warn, f.tpot)
	plan, err := planDay(ctx, res, req, "schema-v"+strconv.Itoa(schemaVersion))
	if err != nil {
		if whatifapi.IsInputError(err) {
			return usageError{err}
		}
		return err
	}
	if f.output == outputJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		err = enc.Encode(plan)
	} else {
		if _, err = io.WriteString(out, selfCheckText(plan.SelfCheck)); err == nil {
			err = writeDayPlanText(out, res.Inputs, plan)
		}
	}
	if err != nil {
		return err
	}
	return dayPlanExit(plan)
}

// dagger marks a figure an experimental model set, as the page's does.
const dagger = "\u2020"

// dayModelsText is the experimental models a plan was asked to price
// with, a line each, "" with none: abstained, or how many sized buckets
// each priced, marked with the dagger; the notes say why the rest are the
// planner's own.
func dayModelsText(p *whatifapi.DayPlanResponse) string {
	var b strings.Builder
	line := func(label, abstained string, flag whatif.Flag) {
		if abstained != "" {
			fmt.Fprintf(&b, "%s: abstained (%s); the planner's own prices every bucket\n", label, abstained)
			return
		}
		n := 0
		for _, bk := range p.Buckets {
			if bk.Limit != nil && slices.Contains(bk.Limit.Flags, string(flag)) {
				n++
			}
		}
		fmt.Fprintf(&b, "%s: priced %d of the %d sized buckets, each marked %s (not validated: OUTSIDE VALIDATED REGION); the notes say why the rest are the planner's own\n",
			label, n, p.Summary.Counts.Sized, dagger)
	}
	if m := p.TTFTModel; m != nil {
		line(m.Label, m.Abstained, whatif.RankMixtureTTFT)
	}
	if m := p.TPOTModel; m != nil {
		line(m.Label, m.Abstained, whatif.TPOTDistribution)
	}
	return b.String()
}

// experimentalHours is the hours, by their Unix second, holding a sized bucket whose limit an
// experimental model set (whatif.RankMixtureTTFT, TPOTDistribution).
func experimentalHours(p *whatifapi.DayPlanResponse) map[int64]bool {
	out := map[int64]bool{}
	for _, bk := range p.Buckets {
		if bk.Limit == nil {
			continue
		}
		if slices.Contains(bk.Limit.Flags, string(whatif.RankMixtureTTFT)) || slices.Contains(bk.Limit.Flags, string(whatif.TPOTDistribution)) {
			out[bk.Start.Truncate(time.Hour).Unix()] = true
		}
	}
	return out
}

// dayPlanExit is the day plan's exit: 3 when the source refused a term, a
// bucket's rate is beyond the bound, or no bucket was sized at all, as a
// refused role exits elsewhere; else whatifapi.ExitUntrusted when a
// bucket's verdict rests on figures the self-check found inconsistent, as
// at-replicas exits; the output is printed either way.
func dayPlanExit(p *whatifapi.DayPlanResponse) error {
	if len(p.Refused) > 0 || p.Summary.Counts.Unmet > 0 || p.Summary.Counts.Sized == 0 {
		return exitStatus(3)
	}
	if p.Summary.Counts.Untrusted > 0 {
		return exitStatus(whatifapi.ExitUntrusted)
	}
	return nil
}

// planDay plans the day: whatifapi.DayPlan, which tests replace with one
// that prices each plan once per test binary.
var planDay = whatifapi.DayPlan

// resolveDay resolves a day plan's source: f.resolve, which tests replace
// with one that replays each recording once per test binary.
var resolveDay = func(ctx context.Context, f *dayPlanFlags, warn io.Writer) (*whatifapi.Resolved, error) {
	return f.resolve(ctx, warn)
}

// resolve reads the source as serve resolves it: a recording by its file,
// or a Prometheus window, the last day by default.
func (f *dayPlanFlags) resolve(ctx context.Context, warn io.Writer) (*whatifapi.Resolved, error) {
	check := defaultSelfCheck && !f.noSelfCheck
	in := whatifapi.InputsRequest{Model: f.model, PrefillVariant: f.prefillVariant, DecodeVariant: f.decodeVariant, SelfCheck: &check,
		PrefillVPPlateau: f.vpPlateau, PrefillHitAtCount: f.hitAtCount, PrefillVPServiceLine: f.vpServiceLine, PrefillFLoadCurve: f.fLoadCurve, ExperimentalTTFT: f.experimentalTTFT}
	if f.rankMixtureTTFT {
		in.TTFTModel = rankMixtureModel(f.rankMixtureNLLine)
	}
	if f.tpotDistribution {
		in.TPOTModel = f.tpotModel
	}
	src := &servedSources{warn: warn}
	switch {
	case f.recording != "" && f.window.prometheus != "":
		return nil, usagef("--recording and --prometheus both name the source; give one")
	case f.recording != "":
		src.recordings = filepath.Dir(f.recording)
		in.Source = &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(f.recording)}
	case f.window.prometheus != "":
		if f.window.namespace == "" {
			return nil, usagef("--prometheus needs --namespace")
		}
		src.window, src.allowed = f.window, []string{f.window.namespace}
		from, to := f.window.from, f.window.to
		switch {
		case (from == "") != (to == ""):
			return nil, usagef("--from and --to: give both, or neither for the last day")
		case from == "":
			end := time.Now().UTC().Truncate(time.Second)
			from, to = end.Add(-defaultDayWindow).Format(time.RFC3339), end.Format(time.RFC3339)
		}
		in.Source = &whatifapi.SourceRequest{Kind: whatifapi.SourcePrometheus, Namespace: f.window.namespace, From: from, To: to}
	default:
		return nil, usagef("--recording or --prometheus is required")
	}
	res, err := src.Resolve(ctx, in)
	if err != nil && whatifapi.IsInputError(err) {
		return nil, usageError{err}
	}
	if res != nil && f.rankMixtureTTFT {
		res.TTFTModel = f.rankMixtureFit.options(in.TTFTModel, whatif.DefaultRankMixtureStratified)
	}
	if res != nil && f.tpotDistribution {
		res.TPOTMinFitSteps = f.tpotFit.steps()
	}
	return res, err
}

// request is the day plan the flags ask.
func (f *dayPlanFlags) request() (whatifapi.DayPlanRequest, error) {
	req := whatifapi.DayPlanRequest{Step: f.bucket, SizeFor: f.sizeFor, MinRequests: &f.minRequests,
		ExperimentalModels: f.rankMixtureTTFT || f.tpotDistribution}
	slo := &whatifapi.SLORequest{}
	var err error
	if req.Lead, err = leadOf(f.lead); err != nil {
		return req, err
	}
	if slo.TTFT, err = objectiveOf("ttft", f.ttft); err != nil {
		return req, err
	}
	if slo.TPOT, err = objectiveOf("tpot", f.tpot); err != nil {
		return req, err
	}
	if slo.TTFT != nil || slo.TPOT != nil {
		req.SLO = slo
	}
	if f.headroom != 0 {
		req.Headroom = &f.headroom
	}
	if f.maxPrefill != 0 || f.maxDecode != 0 {
		req.MaxReplicas = &whatifapi.FleetKnob{}
		if f.maxPrefill != 0 {
			req.MaxReplicas.Prefill = &f.maxPrefill
		}
		if f.maxDecode != 0 {
			req.MaxReplicas.Decode = &f.maxDecode
		}
	}
	a := whatifapi.Assume{}
	typed := false
	if f.assumeVP > 0 {
		a.VP, typed = &f.assumeVP, true
	}
	if f.assumeA > 0 {
		a.A, typed = ptrTo(f.assumeA.Seconds()), true
	}
	if f.assumePromptCV > 0 {
		a.PromptCV, typed = &f.assumePromptCV, true
	}
	if f.assumeRouterHop > 0 {
		a.RouterHop, typed = ptrTo(f.assumeRouterHop.Seconds()), true
	}
	if f.assumeITLBSet {
		floor, b, err := parseAssumeITLB(f.assumeITLB)
		if err != nil {
			return req, err
		}
		if floor {
			a.ITLBFromFloor = true
		} else {
			a.ITLB = ptrTo(b.Seconds())
		}
		typed = true
	}
	if typed {
		req.Assume = &a
	}
	if f.inputTokens > 0 || f.outputTokens > 0 {
		req.Traffic = &whatifapi.Traffic{Mix: &whatifapi.Mix{PromptTokens: f.inputTokens, OutputTokens: f.outputTokens}}
	}
	return req, nil
}

// leadOf reads --lead: one duration for both roles, or role=duration pairs
// separated by commas. Empty asks none.
func leadOf(s string) (*whatifapi.DayLead, error) {
	if s == "" {
		return nil, nil
	}
	bad := usagef("--lead %q: want a duration (90s), or prefill=3m,decode=90s", s)
	secs := func(v string) (*float64, error) {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return nil, bad
		}
		return ptrTo(d.Seconds()), nil
	}
	l := &whatifapi.DayLead{}
	if !strings.Contains(s, "=") {
		v, err := secs(s)
		if err != nil {
			return nil, err
		}
		w := *v
		l.Prefill, l.Decode = v, &w
		return l, nil
	}
	for _, part := range strings.Split(s, ",") {
		role, v, _ := strings.Cut(part, "=")
		d, err := secs(v)
		if err != nil {
			return nil, err
		}
		switch {
		case role == rolePrefill && l.Prefill == nil:
			l.Prefill = d
		case role == domain.RoleDecode && l.Decode == nil:
			l.Decode = d
		default:
			return nil, bad
		}
	}
	return l, nil
}

// statNone is a TPOT target of none, and a list of nothing.
const statNone = "none"

// ptrTo is a pointer to x.
func ptrTo(x float64) *float64 { return &x }

// objectiveOf reads --ttft or --tpot: a duration, and optionally "@pNN";
// "none" (TPOT only) asks no target. Empty is the policy's.
func objectiveOf(name, s string) (*whatifapi.Objective, error) {
	if s == "" {
		return nil, nil
	}
	if s == statNone && name == "tpot" {
		return &whatifapi.Objective{Statistic: statNone}, nil
	}
	value, stat, hasStat := strings.Cut(s, "@")
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return nil, usagef("--%s %q: want a duration, as 500ms or 500ms@p95", name, s)
	}
	o := &whatifapi.Objective{Statistic: "mean", Target: ptrTo(d.Seconds())}
	if hasStat {
		o.Statistic = stat
	}
	return o, nil
}

// writeDayPlanText is the day plan's human output: what the source could
// not price, the hours, then the summary and the notes.
func writeDayPlanText(out io.Writer, in whatifapi.InputsResponse, p *whatifapi.DayPlanResponse) error {
	var b strings.Builder
	days := p.From.Format("2006-01-02") != p.To.Add(-time.Nanosecond).Format("2006-01-02")
	at := func(t time.Time) string {
		if days {
			return t.Format("01-02 15:04")
		}
		return t.Format("15:04")
	}
	sizedFor := p.SizeFor
	if sizedFor == whatifapi.SizeForP95Modulation {
		sizedFor = "p95 less counting noise"
	}
	fmt.Fprintf(&b, "Day plan for %s/%s, %s to %s UTC, buckets of %s, each sized for the %s of its cycle rates (%s; the mean where a bucket holds fewer than %d cycles)\n",
		in.Namespace, in.Model, p.From.Format("2006-01-02 15:04"), p.To.Format("2006-01-02 15:04"),
		time.Duration(p.Step*float64(time.Second)), sizedFor, p.SizeForSource, whatifapi.MinPercentileCycles)
	if l := p.Lead; l != nil {
		fmt.Fprintf(&b, "scale-up lead: prefill %s, decode %s; held = the need and the replicas starting in the bucket for the buckets after it\n",
			leadText(l.Prefill), leadText(l.Decode))
	}
	if p.TTFT != nil {
		fmt.Fprintf(&b, "SLO: TTFT %s %s", p.TTFT.Statistic, fmtSeconds(p.TTFT.Target))
	} else {
		b.WriteString("SLO: no TTFT target (prefill not sized)")
	}
	if p.TPOT != nil {
		fmt.Fprintf(&b, ", TPOT %s %s", p.TPOT.Statistic, fmtSeconds(p.TPOT.Target))
	}
	fmt.Fprintf(&b, "; headroom %.2f. %s\n", p.Headroom, p.Banner)
	b.WriteString(dayModelsText(p))
	for _, r := range p.Refused {
		what := map[string]string{"ttft": "prefill's TTFT", "tpot": "decode's TPOT", "decodeKnee": "decode's knee", "window": "the busiest bucket"}[r.Term]
		fmt.Fprintf(&b, "\nREFUSED %s (%s), in %d buckets: %s\n  what to do: %s\n  what the plan did: %s\n",
			what, r.Reason, r.Buckets, r.Detail, r.Hint, r.Effect)
	}
	b.WriteString("\n")
	b.WriteString("each hour: a fleet that holds every bucket in it (priced again at each), the busiest bucket's own fleet, the fewest pods it had, " +
		"and the fleet it had judged whole at each bucket's rate\n")
	layout := "%-17s %9s %9s %6s %6s %7s %6s %6s  %s\n"
	row := func(cells ...any) { fmt.Fprintf(&b, layout, cells[:len(cells)-1]...) }
	if p.Lead != nil {
		layout = "%-17s %9s %9s %6s %6s %7s %7s %6s %6s  %s\n"
		row = func(cells ...any) {
			fmt.Fprintf(&b, layout, slices.Concat(cells[:6], cells[len(cells)-1:], cells[6:len(cells)-1])...)
		}
	}
	row("hour (UTC)", "rate mean", "rate p95", "need P", "need D", "busiest", "had P", "had D", "", "held")
	modelHours := experimentalHours(p)
	notServing := false
	for _, h := range p.Hours {
		mean, p95 := "-", "-"
		if h.Rate != nil {
			mean, p95 = fmt.Sprintf("%.2f", h.Rate.Mean), fmt.Sprintf("%.2f", h.Rate.P95)
		}
		np, nd, ap, ad := "-", "-", "-", "-"
		if h.Needed != nil {
			np, nd = neededCells(h.Needed)
			if modelHours[h.Start.Unix()] {
				np, nd = np+dagger, nd+dagger
			}
		}
		busiest := "-"
		if n := h.Busiest; n != nil {
			busiest = busiestCell(n)
		}
		if h.Actual != nil {
			ap, ad = strconv.Itoa(h.Actual.Prefill), strconv.Itoa(h.Actual.Decode)
		}
		if n := h.NotServing; n != nil {
			ap, ad = withNotServing(ap, n.Prefill), withNotServing(ad, n.Decode)
			notServing = true
		}
		verdict := dayVerdictWords(h.Verdict, h.Scope)
		if h.Unmet > 0 {
			verdict += fmt.Sprintf(", %d bucket(s) beyond %d replicas", h.Unmet, max(p.MaxReplicas.Prefill, p.MaxReplicas.Decode))
		}
		if h.NotConfirmed {
			verdict += ", need NOT CONFIRMED (falls short at a bucket)"
		}
		if h.Trust != "" {
			verdict += "; " + h.Trust
		}
		held := "-"
		if n := h.Held; n != nil {
			held = heldCell(n)
		}
		row(h.Start.Format("2006-01-02 15:04"), mean, p95, np, nd, busiest, ap, ad, verdict, held)
	}
	if notServing {
		b.WriteString("had (+n): n more replicas were asked for and not serving (they exist and are not ready: starting, unschedulable,\n" +
			"  held by an admission gate, or a rollout's surge), each role in its bucket of the fewest serving; the verdict judges what served\n")
	}
	s := p.Summary
	c := s.Counts
	stats := fmt.Sprintf("%d for the p95, ", c.SizedForP95)
	if p.SizeFor == whatifapi.SizeForP95Modulation {
		stats = fmt.Sprintf("%d for the p95 less counting noise, ", c.SizedForP95Modulation)
	}
	fmt.Fprintf(&b, "\nbuckets: %d sized (%d under, %d over, %d match; %s%d for the mean, %d for the max), "+
		"%d beyond the bound, %d refused, %d too little data, %d no data\n",
		c.Sized, c.Under, c.Over, c.Match, stats, c.SizedForMean, c.SizedForMax, c.Unmet, c.Refused, c.TooLittleData, c.NoData)
	b.WriteString(dayTrustText(p))
	hours := func(r *whatifapi.RoleHours) string {
		if r.Prefill == nil {
			return fmt.Sprintf("%.2f decode (prefill not priced in %d of the %d)", r.Decode, c.PrefillNotPriced, c.Sized)
		}
		return fmt.Sprintf("%.2f prefill + %.2f decode", *r.Prefill, r.Decode)
	}
	if s.PeakNeeded != nil {
		fewest := "the fewest replicas in all"
		if g := s.SizedOnGPUs; g != nil {
			fewest = fmt.Sprintf("the fewest GPUs, at %d a prefill and %d a decode replica", g.Prefill, g.Decode)
		}
		peak := neededText(s.PeakNeeded)
		if s.PeakNotConfirmed {
			peak += " (NOT CONFIRMED: falls short at a bucket)"
		}
		fmt.Fprintf(&b, "needed: to hold every bucket %s; the busiest bucket alone %s, the quietest %s "+
			"(each %s; another mix may serve too)\n",
			peak, neededText(s.BusiestNeeded), neededText(s.MinNeeded), fewest)
		fmt.Fprintf(&b, "replica-hours over the %d sized buckets: needed %s, had %s\n",
			c.Sized, hours(s.NeededReplicaHours), hours(s.HadReplicaHours))
		if s.NeededGPUHours != nil {
			fmt.Fprintf(&b, "GPU-hours over the sized buckets: needed %.2f, had %.2f\n", *s.NeededGPUHours, *s.HadGPUHours)
		}
		if r := s.HeldReplicaHours; r != nil {
			fmt.Fprintf(&b, "replica-hours held with the scale-up lead, each scale-up's replicas from one lead before their bucket: %s", hours(r))
			if s.HeldGPUHours != nil {
				fmt.Fprintf(&b, ", %.2f GPU-hours", *s.HeldGPUHours)
			}
			b.WriteString("\n")
		}
	} else {
		b.WriteString("needed: no bucket was sized\n")
	}
	fmt.Fprintf(&b, "had over every bucket with data: %.2f prefill + %.2f decode replica-hours", valueOrZero(s.HadReplicaHoursAll.Prefill), s.HadReplicaHoursAll.Decode)
	if s.HadGPUHoursAll != nil {
		fmt.Fprintf(&b, ", %.2f GPU-hours (GPUs per replica: %d prefill, %d decode)", *s.HadGPUHoursAll, s.GPUsPerReplica.Prefill, s.GPUsPerReplica.Decode)
	}
	b.WriteString("\n")
	if c.Sized > 0 {
		fmt.Fprintf(&b, "hours under (SLO risk): %s\n", hourList(s.UnderHours, at))
		fmt.Fprintf(&b, "hours over (waste): %s\n", hourList(s.OverHours, at))
		if c.MeasuredTTFT > 0 && p.TTFT != nil {
			fmt.Fprintf(&b, "measured TTFT over its target (%s %s): %d of the %d buckets the router measured\n",
				p.TTFT.Statistic, msText(p.TTFT.Target), c.MeasuredTTFTOver, c.MeasuredTTFT)
		}
		if len(s.DecodeOverHours) > 0 {
			fmt.Fprintf(&b, "hours decode only had a replica to spare: %s (%s)\n", hourList(s.DecodeOverHours, at), decodeOnlyWords)
		}
	}
	if len(s.RefusedHours) > 0 {
		fmt.Fprintf(&b, "hours refused: %s\n", hourList(s.RefusedHours, at))
	}
	if flags := dayFlags(p); len(flags) > 0 {
		fmt.Fprintf(&b, "flags: %s\n", strings.Join(flags, ", "))
	}
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// decodeOnlyWords says why a verdict judged on decode alone is not the
// fleet's.
const decodeOnlyWords = "prefill was not sized, so this is not a fleet recommendation"

// dayVerdictWords is an hour's verdict in the table's words. A verdict on
// decode alone says so: decode's spare replica is no fleet's waste while
// prefill is not sized.
func dayVerdictWords(verdict, scope string) string {
	if scope == whatifapi.ScopeDecodeOnly {
		switch verdict {
		case whatifapi.DayVerdictUnder:
			return "UNDER (SLO risk), decode only"
		case whatifapi.DayVerdictOver:
			return "decode only: a decode replica to spare"
		case whatifapi.DayVerdictMatch:
			return "decode only: ok"
		}
	}
	return map[string]string{whatifapi.DayVerdictUnder: "UNDER (SLO risk)", whatifapi.DayVerdictOver: "over (waste)",
		whatifapi.DayVerdictMatch: "ok", whatifapi.DayVerdictRefused: "REFUSED (see above)", "": "not sized"}[verdict]
}

// withNotServing is a had cell with n replicas asked for and not serving
// beside it, "2(+1)"; the cell alone when n is 0.
func withNotServing(cell string, n int) string {
	if n == 0 {
		return cell
	}
	return fmt.Sprintf("%s(+%d)", cell, n)
}

// dayTrustText is what the self-check says of the buckets' verdicts: a line
// for each set of figures the verdicts not trusted rest on, with how many,
// then one line for those it does not cover; empty when it says nothing of
// any.
func dayTrustText(p *whatifapi.DayPlanResponse) string {
	var b strings.Builder
	var figures []string
	n := map[string]int{}
	for _, bk := range p.Buckets {
		f := p.UntrustedFigures(bk)
		if f == "" {
			continue
		}
		if n[f] == 0 {
			figures = append(figures, f)
		}
		n[f]++
	}
	for _, f := range figures {
		fmt.Fprintf(&b, "self-check: %d bucket(s) %s: their verdicts rest on the %s figures, which the self-check found inconsistent with this recording.\n",
			n[f], whatifapi.TrustNot, f)
	}
	if c := p.Summary.Counts.NotCovered; c > 0 {
		tested := "the recorded traffic"
		if p.SelfCheck != nil && p.SelfCheck.Covered != nil {
			tested = p.SelfCheck.Covered.Words()
		}
		fmt.Fprintf(&b, "self-check: %d bucket(s) not covered: it tested %s, not the rate, fleet or shape each was priced at.\n",
			c, tested)
	}
	return b.String()
}

// neededText is a needed fleet in words: decode sized alone is a lower
// bound.
func neededText(n *whatifapi.DayNeeded) string {
	switch {
	case n.Prefill == nil:
		return fmt.Sprintf("at least %d decode (prefill not priced)", n.Decode)
	case n.AtLeast:
		return fmt.Sprintf("at least %d prefill + %d decode (prefill not priced in some buckets)", *n.Prefill, n.Decode)
	}
	return fmt.Sprintf("%d prefill + %d decode", *n.Prefill, n.Decode)
}

// neededCells is a need's prefill and decode cells: "-" for prefill not
// priced, and decode marked ">=" where it is a lower bound.
func neededCells(n *whatifapi.DayNeeded) (prefill, decode string) {
	prefill, decode = "-", strconv.Itoa(n.Decode)
	if n.AtLeast {
		decode = ">=" + decode
	}
	if n.Prefill != nil {
		prefill = strconv.Itoa(*n.Prefill)
		if n.AtLeast {
			prefill = ">=" + prefill
		}
	}
	return prefill, decode
}

// busiestCell is the busiest bucket's fleet in one cell: "4+2", or "D>=3"
// where it sized decode alone.
func busiestCell(n *whatifapi.DayNeeded) string {
	if n.Prefill == nil {
		return "D>=" + strconv.Itoa(n.Decode)
	}
	return strconv.Itoa(*n.Prefill) + "+" + strconv.Itoa(n.Decode)
}

// heldCell is a held fleet in one cell: busiestCell's, ">=" in front where
// it is a lower bound with prefill priced.
func heldCell(n *whatifapi.DayNeeded) string {
	if n.AtLeast && n.Prefill != nil {
		return ">=" + busiestCell(n)
	}
	return busiestCell(n)
}

// leadText is a role's scale-up lead in words.
func leadText(l whatifapi.RoleLead) string {
	switch l.Source {
	case whatifapi.LeadFromRequest:
		return fmt.Sprintf("%s (asked)", time.Duration(l.Seconds*float64(time.Second)).Round(time.Second))
	case whatifapi.LeadLearned:
		return fmt.Sprintf("%s (learned)", time.Duration(l.Seconds*float64(time.Second)).Round(time.Second))
	case whatifapi.LeadSeeded:
		return fmt.Sprintf("%s (a ScaledObject's seed)", time.Duration(l.Seconds*float64(time.Second)).Round(time.Second))
	}
	return "none (not learned; --lead states one)"
}

// valueOrZero is *p, or 0 when p is nil.
func valueOrZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// fmtSeconds is a target in ms.
func fmtSeconds(x float64) string { return fmt.Sprintf("%gms", x*1000) }

// hourList is hours as at writes them, or none.
func hourList(hs []time.Time, at func(time.Time) string) string {
	if len(hs) == 0 {
		return statNone
	}
	s := make([]string, len(hs))
	for i, h := range hs {
		s[i] = at(h)
	}
	return strings.Join(s, ", ")
}

// dayFlags is every flag of the plan's buckets once, in order met, with
// Extrapolated written EXTRAPOLATED.
func dayFlags(p *whatifapi.DayPlanResponse) []string {
	var out []string
	seen := map[string]bool{}
	for _, b := range p.Buckets {
		for _, fl := range b.Flags {
			if seen[fl] {
				continue
			}
			seen[fl] = true
			if fl == whatifapi.FlagExtrapolated {
				fl = "EXTRAPOLATED"
			}
			out = append(out, fl)
		}
	}
	return out
}
