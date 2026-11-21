package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// runAtReplicas answers what a fleet of N replicas gives, for each request
// shape asked and at each load asked: the load the window saw, the
// utilization there, the capacity, the SLO against load, and a verdict
// (report.go). The recording is replayed once for all of them.
func runAtReplicas(ctx context.Context, args []string, out, warn io.Writer) error {
	return asJSONError(reportQuestion, args, out, atReplicas(ctx, args, out, warn))
}

// atReplicasRefused are the answer flags at-replicas does not read: it prices
// on the API's fleet (servedState), which takes the defaults for them, and
// states lengths with --shape.
var atReplicasRefused = []string{"seed", "price-router-wait", "router-wait-in-prefill", "ps-onsets", "prefill-running-cap", "charge-decode-gate",
	"assume-input-tokens", "assume-output-tokens"}

// listFlag is a repeatable flag: each use adds its value.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(s string) error {
	*l = append(*l, s)
	return nil
}

// reportFlags are at-replicas' own flags.
type reportFlags struct {
	shapes, loads     listFlag
	loadSeen, seenX   float64
	loadStatistic     string
	tpot, tpotPercent string
}

func atReplicas(ctx context.Context, args []string, out, warn io.Writer) error {
	fs := newAnswerFlags(reportQuestion, args, out)
	f := answerFlags{question: reportQuestion, warn: warn}
	f.register(fs)
	fs.Lookup("ttft").Usage = "the TTFT target to judge by, end to end: a mean (2s) or a percentile (2s@p90); default the policy's"
	fs.Lookup("ttft-percentile").Usage = "the TTFT percentiles the tables print, in percent, comma separated (50,90; mean for the mean); a tail past p95 on an assumed prompt spread is flagged"
	fs.Lookup("ttft-percentile").DefValue = "50,90"
	fs.Lookup("headroom").Usage = "the operating point as a fraction of the fleet knee, in (0, 1); default the policy's scaleUpThreshold"
	var rf reportFlags
	fs.Var(&rf.shapes, "shape", "a request shape to answer for, repeatable: measured (the default), or IL/OL[/hit] in tokens, k for 1024 (8k/200, 1k/4k/0.3)")
	fs.Var(&rf.loads, "load", "a rate the tables price, repeatable: 5r/s, 80% of the shape's fleet knee, seen, or seen x1.5 (a growth what-if with its own verdict); default 50%, 80%, 100% and seen")
	fs.StringVar(&rf.loadStatistic, "load-statistic", whatifapi.DefaultLoadStatistic,
		"the statistic of the window's per-cycle arrival rate to judge at: mean, p95 or peak")
	fs.Float64Var(&rf.loadSeen, "load-seen", 0, "the load to judge at, requests per second, in place of the window's")
	fs.Float64Var(&rf.seenX, "load-seen-x", 0, "an alias for --load 'seen xN': a column and a verdict at the load seen times N "+
		"(1.5 asks what 50% more traffic does), beside the verdict at the load seen, which it never changes")
	fs.StringVar(&rf.tpot, "tpot", "", "the TPOT target to judge by: a mean (30ms) or a per-request percentile (60ms@p90); default the policy's")
	fs.StringVar(&rf.tpotPercent, "tpot-percentile", "90", "the TPOT percentile the tables print beside the mean, in percent; mean for the mean alone")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	if err := f.check(fs); err != nil {
		return err
	}
	q, err := f.reportQuery(rf)
	if err != nil {
		return err
	}
	st, err := f.readState(ctx)
	if err != nil {
		return err
	}
	variants, err := f.decodeVariants(st)
	if err != nil {
		return err
	}
	switch len(variants) {
	case 0:
		return usagef("no decode variant at cycle %d of %s/%s: the report judges a decode role", st.Cycle, st.Namespace, st.ModelID)
	case 1:
	default:
		names := make([]string, len(variants))
		for i, pv := range variants {
			names[i] = pv.state.Variant
		}
		return usagef("the model has %d decode variants (%s); name one with --variant%s", len(names), strings.Join(names, ", "),
			noRoleHint(variants))
	}
	if _, err := f.prefillState(st); err != nil {
		return err
	}
	s := &servedState{st: st, decodeVariant: variants[0].state.Variant, prefillVariant: f.prefillVariant, hint: f.hintSource(),
		imbalanceAcrossCounts: f.imbalanceAcrossCounts}
	rep, err := buildReport(ctx, st, s, q)
	if err != nil {
		if whatifapi.IsInputError(err) {
			return usageError{err}
		}
		return err
	}
	rep.SchemaVersion = whatifapi.SchemaVersion
	rep.Source.Recording = f.recording
	rep.SelfCheck = f.checked
	if f.output == outputJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		if _, err := io.WriteString(out, f.sourceLine(st)); err != nil {
			return err
		}
		if _, err := io.WriteString(out, reportText(rep)); err != nil {
			return err
		}
	}
	if rep.ExitCode != whatif.ExitAnswered {
		return exitStatus(rep.ExitCode)
	}
	return nil
}

// reportQuery reads the flags into the API's question, so that the command
// and POST /v1/report ask the same one.
func (f *answerFlags) reportQuery(rf reportFlags) (whatifapi.ReportQuery, error) {
	var q whatifapi.ReportQuery
	for _, name := range atReplicasRefused {
		if f.set[name] {
			return q, usagef("--%s is not read by %s (state lengths with --shape)", name, reportQuestion)
		}
	}
	if f.set["batch-cap"] && !f.prometheus() {
		return q, usagef("--batch-cap is not read by %s from a recording: it prices the batch cap the analyzer priced at", reportQuestion)
	}
	switch {
	case f.set["load-seen"] && !(rf.loadSeen > 0 && finite(rf.loadSeen)):
		return q, usagef("--load-seen %g, want a finite rate above 0", rf.loadSeen)
	case f.set["load-seen-x"] && !(rf.seenX > 0 && finite(rf.seenX)):
		return q, usagef("--load-seen-x %g, want a finite factor above 0", rf.seenX)
	case f.set["headroom"] && !(f.headroom > 0 && f.headroom < 1):
		return q, usagef("--headroom %g, want a fraction of the knee in (0, 1)", f.headroom)
	case f.set["load-statistic"] && f.set["load-seen"]:
		return q, usagef("--load-statistic and --load-seen: the load is the window's at a statistic or the one stated, not both")
	}
	if err := whatifapi.CheckLoadStatistic(rf.loadStatistic); err != nil {
		return q, usagef("--load-statistic %v", err)
	}
	q.Prefill, q.Decode, q.LoadSeen, q.LoadSeenX = f.prefill, f.decode, rf.loadSeen, rf.seenX
	if f.set["load-statistic"] {
		q.LoadStatistic = rf.loadStatistic
	}
	if f.set["headroom"] {
		q.Headroom = f.headroom
	}
	for _, s := range rf.shapes {
		sh, err := whatifapi.ParseReportShape(s)
		if err != nil {
			return q, usagef("--shape: %v", err)
		}
		q.Shapes = append(q.Shapes, sh)
	}
	for _, s := range rf.loads {
		l, err := whatifapi.ParseReportLoad(s)
		if err != nil {
			return q, usagef("--load: %v", err)
		}
		q.Loads = append(q.Loads, l)
	}
	var err error
	if f.ttft != "" {
		if q.TTFT, err = parseTarget("--ttft", f.ttft); err != nil {
			return q, err
		}
	}
	if rf.tpot != "" {
		if q.TPOT, err = parseTarget("--tpot", rf.tpot); err != nil {
			return q, err
		}
	}
	q.TTFTPercentiles = []float64{0.5, 0.9}
	if f.ttftPercentile != "" {
		q.TTFTPercentiles = nil
		for _, s := range strings.Split(f.ttftPercentile, ",") {
			p, err := percentOrMean("--ttft-percentile", strings.TrimSpace(s))
			if err != nil {
				return q, err
			}
			q.TTFTPercentiles = append(q.TTFTPercentiles, p)
		}
	}
	if q.TPOTPercentile, err = percentOrMean("--tpot-percentile", rf.tpotPercent); err != nil {
		return q, err
	}
	if f.set["assume-tpot-ratio"] && q.TPOTPercentile == 0 && (q.TPOT == nil || q.TPOT.Percentile == 0) {
		return q, usagef("--assume-tpot-ratio needs a TPOT percentile (--tpot-percentile 90)")
	}
	a := &q.Assume
	if f.set["assume-vp"] {
		a.VP = &f.assumeVP
	}
	if f.set["assume-a"] {
		x := f.assumeA.Seconds()
		a.A = &x
	}
	if f.set["assume-prompt-cv"] {
		a.PromptCV = &f.assumePromptCV
	}
	if f.set["assume-router-hop"] {
		x := f.assumeRouterHop.Seconds()
		a.RouterHop = &x
	}
	if f.set["assume-tpot-ratio"] {
		a.TPOTRatio = &f.tpotRatio
	}
	if f.set["assume-itl-b"] {
		floor, b, err := parseAssumeITLB(f.assumeITLB)
		if err != nil {
			return q, err
		}
		if floor {
			a.ITLBFromFloor = true
		} else {
			x := b.Seconds()
			a.ITLB = &x
		}
	}
	if f.set["arrival-cv2"] {
		q.ArrivalCV2 = &f.arrivalCV2
	}
	return q, nil
}

// percentOrMean reads a percentile in percent, or "mean" as 0.
func percentOrMean(flagName, s string) (float64, error) {
	if s == "mean" {
		return 0, nil
	}
	return parseTTFTPercent(flagName, s)
}

// parseTarget reads a target: a duration, and optionally "@p" and a
// percentile in percent.
func parseTarget(flagName, s string) (*whatifapi.ReportTarget, error) {
	value, pct, hasPct := strings.Cut(s, "@")
	d, err := time.ParseDuration(value)
	if err != nil {
		return nil, usagef("%s %q: %v", flagName, s, err)
	}
	if d <= 0 {
		return nil, usagef("%s %q: want a target above 0", flagName, s)
	}
	t := &whatifapi.ReportTarget{Seconds: d.Seconds()}
	if hasPct {
		if !strings.HasPrefix(pct, "p") {
			return nil, usagef("%s %q: a percentile is written @p90", flagName, s)
		}
		if t.Percentile, err = parseTTFTPercent(flagName, pct); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// errNoFigure is a figure with neither a value nor a refusal.
var errNoFigure = errors.New("no figure")

// figureValue is a figure's value, or what stands in its place: "saturated"
// or the refusal's reason.
func figureValue(f whatifapi.Figure) (float64, string, error) {
	switch {
	case f.Value != nil:
		return *f.Value, "", nil
	case f.Refused != nil:
		return 0, plainReason(f.Refused.Reason), nil
	}
	return 0, "", errNoFigure
}

// figText prints a figure in format, scaled, or what stands in its place.
func figText(f whatifapi.Figure, format string, scale float64) string {
	v, words, err := figureValue(f)
	switch {
	case err != nil:
		return "-"
	case words != "":
		return words
	}
	return fmt.Sprintf(format, scale*v)
}

// windowText is the window a report was read from.
func windowText(s whatifapi.ReportSource) string {
	from, to := s.FirstAt.UTC(), s.LastAt.UTC()
	if from.Format(time.DateOnly) == to.Format(time.DateOnly) {
		return fmt.Sprintf("%s %s-%sZ", from.Format(time.DateOnly), from.Format("15:04"), to.Format("15:04"))
	}
	return from.Format(time.RFC3339) + " to " + to.Format(time.RFC3339)
}

// cellMarks are the summary's marks in words, so that a cell does not rely on
// color.
var cellMarks = map[string]string{
	whatifapi.CellMet: "ok", whatifapi.CellMissed: "MISS", whatifapi.CellOnset: "MISS*", whatifapi.CellSaturated: "SAT",
	whatifapi.CellUnknown: "?", whatifapi.CellNoTarget: "",
}

// shapeName is a shape's name in the summary and its section.
func shapeName(s whatifapi.ReportShape) string {
	name := s.Label
	if s.Extrapolated {
		name += " EXTRAPOLATED"
	}
	return name
}

// reportText is the report in the page's layout: the fleet and the load, the
// shape-by-load summary and each shape's verdict first, then each shape's
// detail.
func reportText(r *whatifapi.ReportResponse) string {
	var b strings.Builder
	fl := r.Fleet
	prefill := fmt.Sprintf("%d prefill", fl.Prefill)
	if fl.PrefillVariant != "" {
		prefill += " (" + fl.PrefillVariant + ")"
	}
	fmt.Fprintf(&b, "%s/%s  P/D %s + %d decode (%s)  window %s, cycle %d of %d\n", r.Source.Namespace, r.Source.Model,
		prefill, fl.Decode, fl.DecodeVariant, windowText(r.Source), r.Source.Cycle, r.Source.TotalCycles)
	if e := fl.DecodeEngine; e != nil {
		fmt.Fprintf(&b, "%-16s%s\n", "decode engine", engineInfoText(e))
	}
	if e := fl.PrefillEngine; e != nil {
		fmt.Fprintf(&b, "%-16s%s\n", "prefill engine", engineInfoText(e))
	}
	fmt.Fprintf(&b, "%-16s%s\n", "load judged", loadText(r.Load))
	fmt.Fprintf(&b, "%-16s%s\n", "SLO", sloText(r.SLO))

	b.WriteString(summaryText(r))
	verdicts := make([][]string, 1, 1+len(r.Shapes))
	verdicts[0] = []string{"verdicts"}
	for _, s := range r.Shapes {
		verdicts = append(verdicts, []string{"  " + shapeName(s), s.Verdict.Summary})
		for _, g := range s.Growth {
			verdicts = append(verdicts, []string{"", growthText(g)})
		}
	}
	b.WriteString(columns(verdicts))
	for _, s := range r.Shapes {
		b.WriteString("\n")
		shapeText(&b, s)
	}
	b.WriteString("\nnot validated:\n")
	for _, n := range r.NotValidated {
		fmt.Fprintf(&b, "  - %s\n", n)
	}
	return b.String()
}

// loadText is the load judged at and where it came from, and the load the
// window saw: its mean, p95 and peak per cycle, and its busiest stretch.
func loadText(l whatifapi.ReportLoad) string {
	s := figText(l.Rate, "%.2f r/s", 1)
	switch l.Source {
	case "observed":
		s += fmt.Sprintf(", the %s of the %d of %d cycles with traffic", l.Statistic, l.CyclesWithTraffic, l.Cycles)
	case sourceAsked:
		s += " (--load-seen)"
	}
	if l.Mean != nil {
		p95 := fmt.Sprintf("%.2f", *l.P95)
		if l.P95IsPeak {
			p95 += " (= peak: fewer than 20 cycles)"
		}
		s += fmt.Sprintf("\n%-16sper cycle (1-minute rates): mean %.2f, p95 %s, peak %.2f r/s", "load seen", *l.Mean, p95, *l.Peak)
		if l.Rolling != nil {
			s += fmt.Sprintf("; busiest %s (%d cycles): %.2f r/s", time.Duration(l.Rolling.WindowSeconds*float64(time.Second)).Round(time.Second),
				l.Rolling.Cycles, l.Rolling.Peak)
		}
	}
	return s
}

// summaryText is the shape-by-load matrix: each cell the TTFT and TPOT at
// the statistics the SLO judges (TTFT p90 and the table's TPOT percentile
// where no target is set), and its mark in words. A cell whose TPOT
// percentile is refused prints the mean, marked m.
func summaryText(r *whatifapi.ReportResponse) string {
	ttft, tpot := whatifapi.ReportStatisticName(cellTTFTQ), whatifapi.ReportStatisticName(0)
	if r.SLO.TTFT != nil {
		ttft = r.SLO.TTFT.Statistic
	}
	if r.SLO.TPOT != nil {
		tpot = r.SLO.TPOT.Statistic
	} else {
		for _, s := range r.Shapes {
			for _, row := range s.Table {
				if row.TPOTPercentile != nil {
					tpot = row.TPOTPercentile.Statistic
				}
			}
		}
	}
	head := append(make([]string, 0, 1+len(r.Loads)), "summary")
	head = append(head, r.Loads...)
	cells := make([][]string, 1, 1+len(r.Shapes))
	cells[0] = head
	for _, s := range r.Shapes {
		row := []string{"  " + shapeName(s)}
		for _, l := range r.Loads {
			i := -1
			for j, t := range s.Table {
				if t.Load == l {
					i = j
					break
				}
			}
			if i < 0 {
				row = append(row, "-")
				continue
			}
			c := s.Table[i].Cell
			tp := cellText(c.TPOT.Figure, "%.1f")
			if c.TPOT.Value != nil && c.TPOT.Statistic != tpot {
				tp += "m"
			}
			text := fmt.Sprintf("%s/%s @%.2f", cellText(c.TTFT.Figure, "%.0f"), tp, s.Table[i].Rate)
			if m := cellMarks[c.Mark]; m != "" {
				text += " " + m
			}
			row = append(row, text)
		}
		cells = append(cells, row)
	}
	legend := fmt.Sprintf("cell = TTFT %s ms / TPOT %s ms @ the rate priced, r/s; - refused (the shape's table says why)", ttft, tpot)
	if tpot != whatifapi.ReportStatisticName(0) {
		legend += fmt.Sprintf("; m the TPOT mean, where the %s is refused", tpot)
	}
	return legend + "\nmark = " + markLegend(r.SLO) + "\n" + columns(cells)
}

// markLegend says what a cell's mark judges: the SLO's own statistics, which
// need not be the ones the cell prints.
func markLegend(s whatifapi.ReportSLO) string {
	if s.TTFT == nil && s.TPOT == nil {
		return "SAT saturated (no SLO target to judge)"
	}
	return fmt.Sprintf("the SLO (%s) at that rate: ok met, MISS missed, MISS* missed past the prefill flow-control onset "+
		"(the router queues) though the figure is within, SAT saturated, ? not judged", sloText(s))
}

// shapeText is one shape's detail.
func shapeText(b *strings.Builder, s whatifapi.ReportShape) {
	fmt.Fprintf(b, "shape %s: IL %s OL %s, prefix hit %.2f (%s)\n", s.Label, num(round(s.InputTokens)), num(round(s.OutputTokens)),
		s.PrefixHitRate, s.Source)
	if s.Extrapolated {
		fmt.Fprintf(b, "  EXTRAPOLATED: %s\n", s.ExtrapolatedWhy)
	}
	b.WriteString("utilization at the load judged\n")
	d := s.Utilization.Decode
	fmt.Fprintf(b, "  %-14sKV fill %s (band top %s), slots %s of %d, wait p %s\n", "decode",
		figText(d.KVFill, "%.3f", 1), num(d.BandTop), figText(d.SlotsOccupied, "%.0f", 1), d.Slots, figText(d.WaitProbability, "%.3f", 1))
	p := s.Utilization.Prefill
	pre := fmt.Sprintf("utilization pooled %s, worst pod %s", figText(p.Pooled, "%.2f", 1), figText(p.WorstPod, "%.2f", 1))
	if p.Imbalance > 0 {
		pre += fmt.Sprintf(" (f %.3g)", p.Imbalance)
	}
	fmt.Fprintf(b, "  %-14s%s\n", rolePrefill, pre)
	fmt.Fprintf(b, "  %-14s%s\n", "verdict", s.Verdict.Summary)
	for _, g := range s.Growth {
		fmt.Fprintf(b, "  %-14s%s\n", "", growthText(g))
	}

	c := s.Capacity
	const capLayout = "  %-18s%-10s%-12s%s\n"
	fmt.Fprintf(b, "%-20s%-10s%-12s%s\n", "capacity", "req/s", "out tok/s", "bound by")
	for _, row := range []struct {
		label string
		rate  whatifapi.ReportRate
	}{{"prefill knee", c.PrefillKnee}, {"decode knee", c.DecodeKnee}, {"fleet knee", c.FleetKnee},
		{fmt.Sprintf("operating (%s)", num(c.Headroom)), c.Operating}, {"max at SLO", c.MaxAtSLO}} {
		if row.rate.Value == nil {
			fmt.Fprintf(b, capLayout, row.label, figText(row.rate.Figure, "", 1), "", refusedDetail(row.rate.Figure))
			continue
		}
		tok := ""
		if row.rate.OutputTokensPerSecond != nil {
			tok = fmt.Sprintf("%.0f", *row.rate.OutputTokensPerSecond)
		}
		fmt.Fprintf(b, capLayout, row.label, fmt.Sprintf("%.2f", *row.rate.Value), tok, row.rate.Binds)
	}
	b.WriteString(tableText(s.Table))
	b.WriteString("checks\n")
	for _, ch := range s.Verdict.Checks {
		state := "holds"
		switch {
		case ch.Holds:
		case ch.Known:
			state = "does not hold"
		default:
			state = "not judged"
		}
		fmt.Fprintf(b, "  %-22s%s: %s\n", fleetText(ch.Prefill, ch.Decode), state, plainText(ch.Reason))
	}
	if len(s.Assumed) > 0 {
		fmt.Fprintf(b, "assumed: %s\n", strings.Join(s.Assumed, "; "))
	}
	if len(s.Flags) > 0 {
		fmt.Fprintf(b, "flags: %s\n", strings.Join(s.Flags, "; "))
	}
}

// growthText is a growth what-if's verdict line.
func growthText(g whatifapi.ReportGrowth) string {
	return fmt.Sprintf("x%s (%s): %s", num(g.X), rateWords(g.Rate), g.Verdict.Summary)
}

// refusedDetail is a refused figure's detail, "" otherwise.
func refusedDetail(f whatifapi.Figure) string {
	if f.Refused == nil {
		return ""
	}
	return plainText(f.Refused.Detail)
}

// sloText names the targets judged by.
func sloText(s whatifapi.ReportSLO) string {
	var parts []string
	for _, o := range []struct {
		name string
		obj  *whatifapi.ReportObjective
	}{{"TTFT", s.TTFT}, {"TPOT", s.TPOT}} {
		if o.obj == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s %s (%s)", o.name, o.obj.Statistic, msText(o.obj.Target), o.obj.Source))
	}
	if len(parts) == 0 {
		return "none (the policy sets none; pass --ttft or --tpot)"
	}
	return strings.Join(parts, ", ")
}

// tableText is a shape's SLO against load.
func tableText(rows []whatifapi.ReportRow) string {
	if len(rows) == 0 {
		return "SLO vs load      no rate to price: no knee and no load\n"
	}
	var stats []string
	for _, s := range rows[0].TTFT {
		stats = append(stats, strings.TrimPrefix(s.Statistic, "p"))
	}
	ttftHead := "TTFT " + strings.Join(stats, "/") + " ms"
	if len(stats) > 0 && rows[0].TTFT[0].Statistic != "mean" {
		ttftHead = "TTFT p" + strings.Join(stats, "/") + " ms"
	}
	tpotHead := "TPOT mean ms"
	if p := rows[0].TPOTPercentile; p != nil {
		tpotHead = "TPOT mean/" + p.Statistic + " ms"
	}
	cells := [][]string{{"SLO vs load", "r/s", ttftHead, tpotHead, "e2e mean (sum)"}}
	for _, row := range rows {
		label := "  " + row.Label
		if strings.HasPrefix(row.Label, "load seen") && row.ShareOfKnee != nil {
			label = fmt.Sprintf("  %s (%.0f%%)", row.Label, 100**row.ShareOfKnee)
		}
		cells = append(cells, []string{label, fmt.Sprintf("%.2f", row.Rate), statsText(row.TTFT, "%.1f"),
			tpotText(row), figText(row.E2EMeanSum, "%.2f s", 1)})
	}
	return columns(cells)
}

// statsText is a row's figures at its statistics, "/" between them; one
// reason in place of them all when every one is refused for it.
func statsText(stats []whatifapi.ReportStat, format string) string {
	out := make([]string, len(stats))
	for i, s := range stats {
		out[i] = figText(s.Figure, format, 1000)
	}
	if len(out) > 1 && out[0] == out[len(out)-1] && stats[0].Value == nil {
		same := true
		for _, s := range out {
			same = same && s == out[0]
		}
		if same {
			return out[0]
		}
	}
	return strings.Join(out, "/")
}

// tpotText is a row's TPOT mean and percentile.
func tpotText(row whatifapi.ReportRow) string {
	stats := []whatifapi.ReportStat{{Statistic: "mean", Figure: row.TPOTMean}}
	if p := row.TPOTPercentile; p != nil {
		stats = append(stats, *p)
	}
	return statsText(stats, "%.2f")
}

// columns lays cells out in columns two spaces apart, each as wide as its
// widest cell.
func columns(cells [][]string) string {
	var widths []int
	for _, row := range cells {
		for i, c := range row {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], len(c))
		}
	}
	var b strings.Builder
	for _, row := range cells {
		var line strings.Builder
		for i, c := range row {
			if i == len(row)-1 {
				line.WriteString(c)
				break
			}
			fmt.Fprintf(&line, "%-*s  ", widths[i], c)
		}
		b.WriteString(strings.TrimRight(line.String(), " ") + "\n")
	}
	return b.String()
}

// cellText is a summary cell's figure in ms; a refusal is "-", its reason is
// in the shape's table.
func cellText(f whatifapi.Figure, format string) string {
	if f.Value == nil {
		return "-"
	}
	return fmt.Sprintf(format, 1000**f.Value)
}
