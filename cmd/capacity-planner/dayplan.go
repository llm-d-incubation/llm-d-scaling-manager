package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

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
	fs.StringVar(&f.sizeFor, "size-for", "", "the rate of a bucket to size for: mean, p95 or max of its cycles; default p95, but the mean, less conservative and flagged FewCyclesForP95, for a bucket of fewer than 20 cycles")
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
	fs.StringVar(&f.output, "o", outputText, "output: text or json")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	if f.output != outputText && f.output != outputJSON {
		return usagef("-o %q, want text or json", f.output)
	}
	fs.Visit(func(fl *flag.Flag) { f.assumeITLBSet = f.assumeITLBSet || fl.Name == "assume-itl-b" })
	// The analyzers the replay runs log through controller-runtime, which
	// the CLI never shows.
	ctrllog.SetLogger(logr.Discard())
	res, err := resolveDay(ctx, &f, warn)
	if err != nil {
		return err
	}
	req, err := f.request()
	if err != nil {
		return err
	}
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
	in := whatifapi.InputsRequest{Model: f.model, PrefillVariant: f.prefillVariant, DecodeVariant: f.decodeVariant, SelfCheck: &check}
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
	return res, err
}

// request is the day plan the flags ask.
func (f *dayPlanFlags) request() (whatifapi.DayPlanRequest, error) {
	req := whatifapi.DayPlanRequest{Step: f.bucket, SizeFor: f.sizeFor, MinRequests: &f.minRequests}
	slo := &whatifapi.SLORequest{}
	var err error
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
	fmt.Fprintf(&b, "Day plan for %s/%s, %s to %s UTC, buckets of %s, each sized for the %s of its cycle rates (%s; the mean where a bucket holds fewer than %d cycles)\n",
		in.Namespace, in.Model, p.From.Format("2006-01-02 15:04"), p.To.Format("2006-01-02 15:04"),
		time.Duration(p.Step*float64(time.Second)), p.SizeFor, p.SizeForSource, whatifapi.MinPercentileCycles)
	fmt.Fprintf(&b, "SLO: TTFT %s %s", p.TTFT.Statistic, fmtSeconds(p.TTFT.Target))
	if p.TPOT != nil {
		fmt.Fprintf(&b, ", TPOT %s %s", p.TPOT.Statistic, fmtSeconds(p.TPOT.Target))
	}
	fmt.Fprintf(&b, "; headroom %.2f. %s\n", p.Headroom, p.Banner)
	for _, r := range p.Refused {
		what := map[string]string{"ttft": "prefill's TTFT", "tpot": "decode's TPOT", "decodeKnee": "decode's knee"}[r.Term]
		fmt.Fprintf(&b, "\nREFUSED %s (%s), in %d buckets: %s\n  what to do: %s\n  what the plan did: %s\n",
			what, r.Reason, r.Buckets, r.Detail, r.Hint, r.Effect)
	}
	b.WriteString("\n")
	b.WriteString("each hour: the need of its busiest bucket, the fewest pods it had, and the fleet it had judged whole at each bucket's rate\n")
	const layout = "%-17s %9s %9s %6s %6s %6s %6s  %s\n"
	fmt.Fprintf(&b, layout, "hour (UTC)", "rate mean", "rate p95", "need P", "need D", "had P", "had D", "")
	for _, h := range p.Hours {
		mean, p95 := "-", "-"
		if h.Rate != nil {
			mean, p95 = fmt.Sprintf("%.2f", h.Rate.Mean), fmt.Sprintf("%.2f", h.Rate.P95)
		}
		np, nd, ap, ad := "-", "-", "-", "-"
		if h.Needed != nil {
			nd = strconv.Itoa(h.Needed.Decode)
			if h.Needed.AtLeast {
				nd = ">=" + nd
			}
			if h.Needed.Prefill != nil {
				np = strconv.Itoa(*h.Needed.Prefill)
			}
		}
		if h.Actual != nil {
			ap, ad = strconv.Itoa(h.Actual.Prefill), strconv.Itoa(h.Actual.Decode)
		}
		verdict := map[string]string{whatifapi.DayVerdictUnder: "UNDER (SLO risk)", whatifapi.DayVerdictOver: "over (waste)",
			whatifapi.DayVerdictMatch: "ok", whatifapi.DayVerdictRefused: "REFUSED (see above)", "": "not sized"}[h.Verdict]
		if h.Unmet > 0 {
			verdict += fmt.Sprintf(", %d bucket(s) beyond %d replicas", h.Unmet, max(p.MaxReplicas.Prefill, p.MaxReplicas.Decode))
		}
		if h.Trust != "" {
			verdict += "; " + h.Trust
		}
		fmt.Fprintf(&b, layout, h.Start.Format("2006-01-02 15:04"), mean, p95, np, nd, ap, ad, verdict)
	}
	s := p.Summary
	c := s.Counts
	fmt.Fprintf(&b, "\nbuckets: %d sized (%d under, %d over, %d match; %d for the p95, %d for the mean, %d for the max), "+
		"%d beyond the bound, %d refused, %d too little data, %d no data\n",
		c.Sized, c.Under, c.Over, c.Match, c.SizedForP95, c.SizedForMean, c.SizedForMax, c.Unmet, c.Refused, c.TooLittleData, c.NoData)
	b.WriteString(dayTrustText(p))
	hours := func(r *whatifapi.RoleHours) string {
		if r.Prefill == nil {
			return fmt.Sprintf("%.2f decode (prefill not priced in %d of the %d)", r.Decode, c.PrefillNotPriced, c.Sized)
		}
		return fmt.Sprintf("%.2f prefill + %.2f decode", *r.Prefill, r.Decode)
	}
	if s.PeakNeeded != nil {
		fmt.Fprintf(&b, "needed: at the busiest bucket %s, at the quietest %s (the fewest replicas in all; another mix may serve too)\n",
			neededText(s.PeakNeeded), neededText(s.MinNeeded))
		fmt.Fprintf(&b, "replica-hours over the %d sized buckets: needed %s, had %s\n",
			c.Sized, hours(s.NeededReplicaHours), hours(s.HadReplicaHours))
		if s.NeededGPUHours != nil {
			fmt.Fprintf(&b, "GPU-hours over the sized buckets: needed %.2f, had %.2f\n", *s.NeededGPUHours, *s.HadGPUHours)
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
	if n.Prefill == nil {
		return fmt.Sprintf("at least %d decode (prefill not priced)", n.Decode)
	}
	return fmt.Sprintf("%d prefill + %d decode", *n.Prefill, n.Decode)
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
