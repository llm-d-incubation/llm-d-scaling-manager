package whatif

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The TPOT distribution's fit on cycles (cycle_fits.go): its one free
// parameter, the renewal coefficient C, fitted by interval-censored
// likelihood over every bucket of the router's TPOT histogram on the cycles'
// steps, checked on two self-check folds, and the window it is priced on.
// The capacity planner calls it on a recording's cycles, the slo analyzer on
// the controller's own.

// MinTPOTDistFitSteps is the fewest fit steps the distribution is fitted on:
// two hours of five-minute steps.
const MinTPOTDistFitSteps = 24

// tpotDistFolds are the self-check folds' cuts of the fit window's steps:
// each fits C on the steps before it and is scored on the steps after.
var tpotDistFolds = []float64{0.5, 0.75}

// TPOTDistReport is the experimental TPOT distribution's fit, as a
// backtest's answer carries it (whatifapi.BacktestResponse.TPOTDistribution)
// and the slo analyzer logs it.
type TPOTDistReport struct {
	// Model is TPOTRenewalLognormal or TPOTPerLengthMixture; Experimental
	// is always true.
	Model        string `json:"model"`
	Experimental bool   `json:"experimental"`
	// Abstained is why the distribution prices nothing, "" where it
	// prices: every TPOT percentile is then the planner's own, flagged
	// TPOTDistributionAbstained.
	Abstained string `json:"abstained,omitempty"`
	// Assumed is the fit in words, also in the answer's assumed lines.
	Assumed string `json:"assumed"`
	// C is the fitted renewal coefficient, Renewal its floor from the
	// speculative decoding (RenewalCoefficient; 0 without), and AtBound
	// says C ended at MaxRenewalC.
	C       float64 `json:"c"`
	Renewal float64 `json:"renewal"`
	AtBound bool    `json:"atBound,omitempty"`
	// FoldC is the C each self-check fold fits alone; FitSteps the fit's
	// steps.
	FoldC    []float64 `json:"foldC"`
	FitSteps int       `json:"fitSteps"`
	// MinFitSteps is the fewest fit steps the distribution was allowed to
	// fit on, where the caller set one other than MinTPOTDistFitSteps (the
	// planner's BacktestRequest.TPOTDistMinFitSteps). ShortFit says it was
	// fitted on fewer than MinTPOTDistFitSteps all the same: every
	// percentile it prices is flagged TPOTDistributionShortFit.
	MinFitSteps int  `json:"minFitSteps,omitempty"`
	ShortFit    bool `json:"shortFit,omitempty"`
	// SpecTokens is the decode engine's num_speculative_tokens (0 none or
	// not read), Accepted the drafts a step accepted over the fit window,
	// DecodeSlots its max-num-seqs, and SlotWait the Erlang C share of a
	// rank's sequences that would wait for a slot at the fit window's
	// highest mean running per rank (SlotWait).
	SpecTokens  int     `json:"specTokens"`
	Accepted    float64 `json:"accepted"`
	DecodeSlots int     `json:"decodeSlots"`
	SlotWait    float64 `json:"slotWait"`
	// Fold is the folds' score on the steps each did not see, and Test the
	// test's, at the histogram's resolution.
	Fold []TPOTDistScore `json:"fold"`
	Test *TPOTDistScore  `json:"test,omitempty"`
}

// TPOTDistScore is the distribution scored at the router histogram's
// resolution over Steps steps: the median and p90 over steps of KS, each
// step's largest gap between the predicted and the measured share under a
// bucket edge; and the steps whose predicted p90 or p95 lies in the
// measured one's joint bracket (JointBracket: the router's TPOT histogram
// and, above their first edge, the decode engines', at their sampling
// noise), beside the planner's own percentile's (Default).
// ShapeKSMedian and ShapeKSP90 are KS with the distribution placed at the
// measured mean: the shape's miss alone, the mean's taken out.
type TPOTDistScore struct {
	Steps            int     `json:"steps"`
	KSMedian         float64 `json:"ksMedian"`
	KSP90            float64 `json:"ksP90"`
	ShapeKSMedian    float64 `json:"shapeKsMedian"`
	ShapeKSP90       float64 `json:"shapeKsP90"`
	P90Inside        int     `json:"p90Inside"`
	P95Inside        int     `json:"p95Inside"`
	DefaultP90Inside *int    `json:"defaultP90Inside,omitempty"`
	DefaultP95Inside *int    `json:"defaultP95Inside,omitempty"`
	// P90Below, P90Above, P95Below and P95Above are the steps whose
	// predicted p90 or p95 fell under or over the measured one's bracket.
	P90Below int `json:"p90Below"`
	P90Above int `json:"p90Above"`
	P95Below int `json:"p95Below"`
	P95Above int `json:"p95Above"`
	// EngineJoined is the steps whose brackets the decode engines' TPOT
	// histogram joined; Conflicts the p90 and p95 brackets read as the
	// router's own bucket, the histograms disagreeing at their noise.
	EngineJoined int `json:"engineJoined"`
	Conflicts    int `json:"conflicts"`
}

// TPOTDistFitted is the distribution fitted on cycles.
type TPOTDistFitted struct {
	C      float64
	Report TPOTDistReport
	// PerLength says the distribution is the per-length mixture
	// (TPOTPerLengthMixture).
	PerLength bool
	// StepRates is each fit step's mean arrival rate over the cycles it
	// holds that read one, beside the steps the fit read: the loads the fit
	// window served.
	StepRates []float64
}

// TPOTWindowOf is the router's per-request TPOT histogram of one reading,
// as R_q is learned from it (TPOTRatioFromWindows): bucket rates for counts,
// which give the same quantile and mean. ok is false with no bucket.
func TPOTWindowOf(rl *domain.RouterLatency) (TPOTWindow, bool) {
	if rl == nil || len(rl.TPOTBuckets) == 0 {
		return TPOTWindow{}, false
	}
	w := TPOTWindow{Sum: rl.TPOTSumRate, Count: rl.TPOTCountRate, SumMeasured: true,
		Buckets: make([]HistogramBucket, len(rl.TPOTBuckets))}
	for i, b := range rl.TPOTBuckets {
		w.Buckets[i] = HistogramBucket{UpperEdge: b.UpperBound, Count: b.Rate}
	}
	return w, true
}

// RouterTPOTStepAt is the step anchored at the cycle anchor: its router TPOT
// histogram and mean over the router's window, the decode cycles of cycles
// in that window, and the decode engines' TPOT histogram the anchor read
// (DecodeCycle.EngineTPOT, its [1m] rates counted over that minute,
// WithEngineTPOT), which the step's joint bracket reads (JointBracket). ok
// is false where the router's TPOT or the output lengths are not read.
func RouterTPOTStepAt(cycles []FitCycle, anchor *FitCycle) (TPOTDistStep, bool) {
	if !anchor.Latency || anchor.Router == nil {
		return TPOTDistStep{}, false
	}
	r := anchor.Router
	if !(r.TPOTCountRate > 0) || len(r.TPOTBuckets) == 0 {
		return TPOTDistStep{}, false
	}
	w := RouterWindowOf(r)
	at := anchor.At
	edges, cum := make([]float64, len(r.TPOTBuckets)), make([]float64, len(r.TPOTBuckets))
	for i, b := range r.TPOTBuckets {
		edges[i], cum[i] = b.UpperBound, b.Rate
	}
	st, ok := TPOTDistStepOf(edges, cum, w, r.TPOTSumRate/r.TPOTCountRate,
		DecodeCyclesIn(cycles, at.Add(-time.Duration(w*float64(time.Second))), at))
	if !ok {
		return st, false
	}
	return WithEngineTPOT(st, anchor.Decode.EngineTPOT), true
}

// WithEngineTPOT is st with the engines' TPOT histogram bs, its rates
// counted over the minute they were read over (DefaultMeasureWindow), not
// the router's longer window: the requests the engines' shares rest on,
// which set their sampling noise and MinEngineAboveFirstEdge.
func WithEngineTPOT(st TPOTDistStep, bs []domain.LatencyBucket) TPOTDistStep {
	if len(bs) == 0 {
		return st
	}
	edges, cum := make([]float64, len(bs)), make([]float64, len(bs))
	for i, b := range bs {
		edges[i], cum[i] = b.UpperBound, b.Rate
	}
	return st.WithEngine(edges, cum, DefaultMeasureWindow)
}

// FitTPOTDistCycles fits the distribution on cycles, cut into steps of step,
// each anchored at its last cycle; slots is the decode engine's max-num-seqs
// and spec its num_speculative_tokens. model is the distribution
// (TPOTRenewalLognormal, or TPOTPerLengthMixture; "" the former). minSteps is
// the fewest steps it fits on, 0 MinTPOTDistFitSteps.
func FitTPOTDistCycles(cycles []FitCycle, step time.Duration, slots, spec int, model string, minSteps int) *TPOTDistFitted {
	if model == "" {
		model = TPOTRenewalLognormal
	}
	q := &TPOTDistFitted{Report: TPOTDistReport{Model: model, Experimental: true, FoldC: []float64{},
		Fold: []TPOTDistScore{}, SpecTokens: spec, DecodeSlots: slots}, PerLength: model == TPOTPerLengthMixture}
	least := MinTPOTDistFitSteps
	if minSteps > 0 && minSteps != MinTPOTDistFitSteps {
		least, q.Report.MinFitSteps = minSteps, minSteps
	}
	cycles = sortedByTime(cycles)
	order, anchors := stepAnchors(cycles, step)
	rates := map[time.Time][2]float64{}
	for i := range cycles {
		if s := &cycles[i]; s.RateKnown {
			k := s.At.UTC().Truncate(step)
			r := rates[k]
			rates[k] = [2]float64{r[0] + s.Rate, r[1] + 1}
		}
	}
	var steps []TPOTDistStep
	var accepted []float64
	busiest := 0.0
	for _, k := range order {
		a := anchors[k]
		st, ok := RouterTPOTStepAt(cycles, &cycles[a])
		if !ok {
			continue
		}
		steps = append(steps, st)
		if r := rates[k]; r[1] > 0 {
			q.StepRates = append(q.StepRates, r[0]/r[1])
		}
		c := cycles[a].Decode
		busiest = max(busiest, c.MeanRunning)
		var w, s float64
		for _, g := range c.Groups {
			if g.AcceptedKnown {
				w += g.Rate
				s += g.Rate * g.Accepted
			}
		}
		if w > 0 {
			accepted = append(accepted, s/w)
		}
	}
	q.Report.FitSteps = len(steps)
	if len(accepted) > 0 {
		slices.Sort(accepted)
		q.Report.Accepted = accepted[len(accepted)/2]
	}
	q.Report.Renewal = RenewalCoefficient(spec, q.Report.Accepted)
	if slots > 0 {
		q.Report.SlotWait = SlotWait(slots, busiest)
	}
	var why []string
	switch {
	case len(steps) < least:
		why = append(why, fmt.Sprintf("%d fit steps read the router's TPOT histogram and the decode pods' output lengths, under %d",
			len(steps), least))
	case slots <= 0:
		why = append(why, "the decode engine's max-num-seqs not read: whether sequences wait for a slot is not known")
	case q.Report.SlotWait > MaxSlotWait:
		why = append(why, fmt.Sprintf("%.1f%% of a decode rank's sequences would wait for one of its %d slots (Erlang C at %.3g running): "+
			"a slot queue the distribution leaves out", 100*q.Report.SlotWait, slots, busiest))
	}
	if len(why) > 0 {
		q.Report.Abstained = strings.Join(why, "; ")
		return q
	}
	fitC := FitTPOTDist
	if q.PerLength {
		fitC = FitTPOTDistPerLength
	}
	c, atBound, err := fitC(steps, q.Report.Renewal)
	if err != nil {
		q.Report.Abstained = err.Error()
		return q
	}
	q.C, q.Report.C, q.Report.AtBound = c, c, atBound
	for _, cut := range tpotDistFolds {
		n := int(math.Round(cut * float64(len(steps))))
		fc, fb, ferr := fitC(steps[:n], q.Report.Renewal)
		if ferr != nil {
			q.Report.Abstained = ferr.Error()
			return q
		}
		atBound = atBound || fb
		q.Report.FoldC = append(q.Report.FoldC, fc)
		var sc TPOTDistScorer
		for _, st := range steps[n:] {
			sc.Add(q.DistAt(st.GroupCV2, fc, st.Law), st, st.Mean, nil)
		}
		q.Report.Fold = append(q.Report.Fold, sc.Score())
	}
	if atBound {
		q.Report.Abstained = fmt.Sprintf("renewal coefficient not identified (the fit, or a self-check fold's, ends at its bound %g)", MaxRenewalC)
		return q
	}
	shape := "lognormal around the planner's mean, log-variance the decode groups' spread plus C E[1/(L-1)] over the step's output lengths"
	if q.PerLength {
		shape = "a mixture over the step's output lengths of one lognormal per length around the planner's mean, each of log-variance " +
			"the decode groups' spread plus C/(L-1)"
	}
	q.Report.Assumed = fmt.Sprintf("experimental TPOT distribution %s: a request's TPOT is %s (the renewal of %d-token speculative "+
		"steps), C %.3g fitted by interval-censored likelihood on every bucket of the fit window's router TPOT histogram "+
		"(floor %.3g; self-check folds %s); no slot queue (Erlang C %.2g%% at %d slots)",
		model, shape, spec, c, q.Report.Renewal, joinFloats(q.Report.FoldC), 100*q.Report.SlotWait, slots)
	if len(steps) < MinTPOTDistFitSteps {
		q.Report.ShortFit = true
		q.Report.Assumed += fmt.Sprintf("; fitted on %d steps, under the default %d, down to the %d asked: every percentile it prices "+
			"is also flagged %s", len(steps), MinTPOTDistFitSteps, least, TPOTDistributionShortFit)
	}
	return q
}

// DistAt is the fitted distribution at a step's group spread g2 and renewal
// coefficient c, at the step's lengths law where it is the per-length
// mixture.
func (q *TPOTDistFitted) DistAt(g2, c float64, law []LengthGroup) TPOTDist {
	d := TPOTDist{GroupCV2: g2, C: c, Renewal: q.Report.Renewal}
	if q.PerLength {
		d.PerLength = law
	}
	return d
}

// Flags is the flags every percentile the distribution prices carries:
// TPOTDistribution, and TPOTDistributionShortFit where it was fitted on
// fewer steps than the default (ShortFit).
func (r TPOTDistReport) Flags() []Flag {
	if r.ShortFit {
		return []Flag{TPOTDistribution, TPOTDistributionShortFit}
	}
	return []Flag{TPOTDistribution}
}

// joinFloats is xs in three significant digits, comma separated.
func joinFloats(xs []float64) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%.3g", x)
	}
	return strings.Join(parts, ", ")
}

// TPOTDistScorer pools steps' scores at the histogram's resolution.
type TPOTDistScorer struct {
	ks, shape []float64
	// side counts, per percentile (p90, p95), where the predicted figure
	// fell against the measured one's joint bracket, and def the planner's
	// own figure.
	side, def [2][4]int
	// engine and conflicts count the steps the engines' histogram joined
	// and the brackets read as the router's own bucket.
	engine, conflicts int
	// compared says the planner's own percentiles were scored beside.
	compared bool
	n        int
}

// scoredQuantiles is the percentiles the scorer brackets.
var scoredQuantiles = [2]float64{0.9, 0.95}

// Add scores d at st around mean; def is the planner's own p90 and p95,
// nil where not compared. It returns the step's KS and whether the
// predicted p90 and p95 lie in their joint brackets.
func (s *TPOTDistScorer) Add(d TPOTDist, st TPOTDistStep, mean float64, def []*float64) (float64, bool, bool) {
	ks, side := d.BucketScore(st, mean, scoredQuantiles[:]...)
	s.ks = append(s.ks, ks)
	shape, _ := d.BucketScore(st, st.Mean)
	s.shape = append(s.shape, shape)
	s.n++
	engine := false
	for j, q := range scoredQuantiles {
		s.side[j][side[j]]++
		b, ok := JointBracket(st, q)
		if !ok {
			continue
		}
		engine = engine || b.Engine
		if b.Conflict {
			s.conflicts++
		}
		if len(def) == 2 && def[j] != nil {
			s.def[j][b.Side(*def[j])]++
		}
	}
	if engine {
		s.engine++
	}
	if len(def) == 2 {
		s.compared = true
	}
	return ks, side[0] == BracketInside, side[1] == BracketInside
}

// Score is the pooled score.
func (s *TPOTDistScorer) Score() TPOTDistScore {
	in, below, above := BracketInside, BracketBelow, BracketAbove
	out := TPOTDistScore{Steps: s.n, P90Inside: s.side[0][in], P95Inside: s.side[1][in], P90Below: s.side[0][below],
		P90Above: s.side[0][above], P95Below: s.side[1][below], P95Above: s.side[1][above], EngineJoined: s.engine, Conflicts: s.conflicts}
	if s.compared {
		dp90, dp95 := s.def[0][in], s.def[1][in]
		out.DefaultP90Inside, out.DefaultP95Inside = &dp90, &dp95
	}
	out.KSMedian, out.KSP90 = medianAndP90(s.ks)
	out.ShapeKSMedian, out.ShapeKSP90 = medianAndP90(s.shape)
	return out
}

// medianAndP90 is the median and the nearest-rank p90 of xs, 0 with none.
func medianAndP90(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[len(s)/2], s[min(len(s)-1, int(math.Ceil(0.9*float64(len(s))))-1)]
}

// TPOTWindowFit is the distribution fitted on a window of cycles for pricing
// at any load, as the capacity planner's main view prices it and the slo
// analyzer prices its current load: a percentile is the mean times a ratio
// that does not move with the rate (Ratio).
type TPOTWindowFit struct {
	// Fitted is the fit; Abstained why it prices nothing, "" where it
	// prices.
	Fitted    *TPOTDistFitted
	Abstained string
	// Dist is the fitted distribution and Inv the window's E[1/(L-1)],
	// pooled over its cycles: what every rate is priced at. Top is the
	// highest step rate fitted, requests a second.
	Dist TPOTDist
	Inv  float64
	Top  float64
}

// FitTPOTWindow fits the distribution model on cycles, cut into steps of
// step, with the decode engine's max-num-seqs slots and
// num_speculative_tokens spec, on at least minSteps steps (0
// MinTPOTDistFitSteps). With the per-length mixture every rate is priced at
// the window's pooled lengths, as the one lognormal is at their pooled
// E[1/(L-1)].
func FitTPOTWindow(cycles []FitCycle, step time.Duration, slots, spec int, model string, minSteps int) TPOTWindowFit {
	tq := FitTPOTDistCycles(cycles, step, slots, spec, model, minSteps)
	w := TPOTWindowFit{Fitted: tq}
	if tq.Report.Abstained != "" {
		w.Abstained = tq.Report.Abstained
		return w
	}
	sorted := sortedByTime(cycles)
	first, last := sorted[0].At, sorted[len(sorted)-1].At
	window := DecodeCyclesIn(cycles, first.Add(-time.Nanosecond), last)
	inv, g2, ok := DecodeWindow(window)
	if !ok {
		w.Abstained = "the source's decode pods read no output-length histogram"
		return w
	}
	var law []LengthGroup
	if tq.PerLength {
		law, _ = DecodeWindowLaw(window)
	}
	w.Inv, w.Dist = inv, tq.DistAt(g2, tq.C, law)
	for _, rate := range tq.StepRates {
		w.Top = max(w.Top, rate)
	}
	return w
}

// Ratio is the q percentile over the mean, the same at every rate; 0 where
// the fit abstained.
func (w TPOTWindowFit) Ratio(q float64) float64 {
	if w.Abstained != "" {
		return 0
	}
	return w.Dist.Quantile(1, w.Inv, q)
}
