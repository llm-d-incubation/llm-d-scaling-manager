package capacity

import (
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// Whether an engine pool's step budget binds (issue
// llm-d-incubation/llm-d-scaling-manager#499). The budget itself is an engine
// argument (--max-num-batched-tokens, StepBudget); whether the scheduler ever
// reaches it is a measurement, read from vLLM's vllm:iteration_tokens_total and
// recorded per pod as domain.ReplicaMetrics.IterationTokensBuckets. A budget
// that never binds is nominal: a prompt rides in one step and no chunk term
// applies. The class is detected from the engine's own series, never keyed
// by model or fleet, so the controller's analyzers and the capacity planner
// read it alike: a StepTokens pool of a variant's rows (Add), then
// EngineParams.StepBudgetBinding.
//
// What the series counts is not the tokens a step scheduled. vLLM V1
// observes, once per engine iteration of each engine core (one per
// data-parallel rank, labelled engine) that emitted any request output, the
// computed prompt tokens (the prompt net of its cache hits) of every request
// whose prefill completed in that iteration, whole, plus the tokens generated
// (vllm/v1/metrics/loggers.py, PrometheusStatLogger.record; IterationStats,
// made in AsyncLLM's output handler only for a batch with outputs). An
// iteration that only ran chunks of unfinished prompts emits nothing and is
// not counted. A chunked prompt counts
// nothing in the iterations that ran its earlier chunks and its whole computed
// length in the last. So the one direct evidence of a binding budget is an
// iteration that counts more than the budget: prompts that long could not be
// computed in one step, and the scheduler cut them at the budget. An iteration
// that counts under the budget, however near, says nothing: it is a prompt
// that fit in one step, or several that fit together, or the last chunk of
// one. The class is therefore decided on the share of iterations above the
// budget (OverShare), read at the smallest histogram bound at or over the
// budget; the share above the near bound (Share) is reported as a diagnostic.

// StepBudgetClass is what a pool's iterations say of its step budget.
type StepBudgetClass string

const (
	// StepBudgetBinds is a pool where at least StepBudgetBindsShare of the
	// iterations counted more than the budget's bound: prompts the budget
	// chunked.
	StepBudgetBinds StepBudgetClass = "binds"
	// StepBudgetDoesNotBind is a pool where fewer did: its prompts fit in a
	// step.
	StepBudgetDoesNotBind StepBudgetClass = "does-not-bind"
	// StepBudgetUnknown is a pool the series cannot classify; Reason says
	// why.
	StepBudgetUnknown StepBudgetClass = "unknown"
)

// The reasons a binding is StepBudgetUnknown (StepBudgetBinding.Reason).
const (
	// StepBudgetNoSeries is a pool with no tokens-per-iteration series:
	// SGLang, a recording made before the series was recorded, or a
	// Prometheus that drops it.
	StepBudgetNoSeries = "no tokens-per-iteration series"
	// StepBudgetNoSteps is a series that counted no iteration.
	StepBudgetNoSteps = "no engine iteration counted"
	// StepBudgetTooFew is a pool whose rows counted, all together, fewer than
	// StepBudgetMinIterations iterations: a near-idle pool or a short
	// window, too few to read a share of StepBudgetBindsShare from.
	StepBudgetTooFew = "too few iterations counted"
	// StepBudgetMixedBounds is a pool whose rows were read on different
	// histogram bounds (engines of two releases, a bucket missing): their
	// cumulative counts do not add.
	StepBudgetMixedBounds = "rows on different histogram bounds"
	// StepBudgetNotKnown is a pool whose budget no argument or default
	// resolves: TopTokens is still read.
	StepBudgetNotKnown = "step budget not known"
	// StepBudgetTooCoarse is a histogram with no finite bound at or over the
	// budget: an iteration above the budget cannot be told from one under
	// it.
	StepBudgetTooCoarse = "no finite histogram bound at or over the budget"
)

const (
	// StepBudgetBindsShare is the least share of iterations above the
	// budget's bound that is read as a binding budget.
	StepBudgetBindsShare = 0.05
	// StepBudgetMinIterations is the fewest iterations a pool must have
	// counted, over all its rows, for the pool to be read: at 400 a share of
	// StepBudgetBindsShare is read to about one point (its binomial standard
	// error is 1.1%). It is a count, not a rate: a prefill pod emits an
	// output once per prompt it completes, a few times a second, where a
	// decode pod emits one every step.
	StepBudgetMinIterations = 400.0
	// StepBudgetNearFraction places the near bound, a diagnostic: the
	// largest histogram bound under this fraction of the budget. With
	// vLLM's power-of-two bounds and a budget of 8192 it is 4096.
	StepBudgetNearFraction = 0.9
	// StepBudgetTopQuantile is the share of iterations TopTokens holds.
	StepBudgetTopQuantile = 0.999
)

// StepTokens is a pool's tokens-per-iteration histogram: its rows' cumulative
// histograms, each made monotone on its own, weighted by the seconds it stands
// for and added bound by bound, so the pool counts iterations.
type StepTokens struct {
	// Buckets is the pooled cumulative histogram, iterations at or under
	// each bound, sorted by bound; nil with no row added.
	Buckets []domain.LengthBucket
	// Samples is how many rows were added: pods times cycles.
	Samples int
	// MixedBounds says a row was read on other bounds than the first one's,
	// so the pool is not read (StepBudgetMixedBounds).
	MixedBounds bool
}

// Add pools one row's histogram, domain.ReplicaMetrics.IterationTokensBuckets
// (iterations a second at or under each bound), as the iterations of the
// seconds it stands for: the time since the row before it of the same pod,
// at most the window its rate is taken over. An empty row adds nothing, and a
// row of no seconds adds its bounds and no iteration. Its rates are made
// monotone first: rate() extrapolates each bucket on its own, and a bound may
// read a hair under the one below. A row whose bounds are not the pool's
// marks the pool MixedBounds and adds nothing.
func (s *StepTokens) Add(row []domain.LengthBucket, seconds float64) {
	seconds = max(seconds, 0)
	if len(row) == 0 {
		return
	}
	cum := make([]domain.LengthBucket, 0, len(row))
	running := 0.0
	for _, b := range row {
		if math.IsNaN(b.UpperBound) || math.IsNaN(b.Rate) || math.IsInf(b.Rate, 0) || b.Rate < 0 {
			continue
		}
		running = max(running, b.Rate)
		cum = append(cum, domain.LengthBucket{UpperBound: b.UpperBound, Rate: running * seconds})
	}
	if len(cum) == 0 {
		return
	}
	if !slices.IsSortedFunc(cum, func(a, b domain.LengthBucket) int { return cmpBound(a.UpperBound, b.UpperBound) }) {
		s.MixedBounds = true
		return
	}
	if s.Samples == 0 {
		s.Buckets, s.Samples = cum, 1
		return
	}
	if !slices.EqualFunc(s.Buckets, cum, func(a, b domain.LengthBucket) bool { return a.UpperBound == b.UpperBound }) {
		s.MixedBounds = true
		return
	}
	for i := range cum {
		s.Buckets[i].Rate += cum[i].Rate
	}
	s.Samples++
}

// Clone is a copy of s that shares nothing with it.
func (s StepTokens) Clone() StepTokens {
	s.Buckets = slices.Clone(s.Buckets)
	return s
}

func cmpBound(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// StepBudgetBinding is a pool's step budget against its measured
// iterations.
type StepBudgetBinding struct {
	Class StepBudgetClass
	// Reason is why Class is StepBudgetUnknown; "" otherwise.
	Reason string
	// Budget is the budget read against, tokens per step of one engine core
	// (one data-parallel rank; its tensor- and expert-parallel GPUs share
	// it), as the histogram counts each engine core's iterations; 0 where it
	// is not known.
	Budget int64
	// Iterations is how many iterations the pool counted: the weight of the
	// reading, 0 with none.
	Iterations float64
	// BoundTokens is the smallest finite bound at or over the budget, and
	// OverShare the share of iterations that counted more: completions of
	// prompts the budget chunked, the evidence the class is decided on. A
	// floor on the share above the budget where BoundTokens is above it.
	// Both 0 where not read.
	BoundTokens float64
	OverShare   float64
	// NearTokens is the near bound, and Share the share of iterations that
	// counted more: a diagnostic, not the class. Both 0 where not read.
	NearTokens float64
	Share      float64
	// TopTokens is the bound at or under which StepBudgetTopQuantile of the
	// iterations counted: the observed top. TopOpen says that share reached
	// the +Inf bucket, and TopTokens is then the last finite bound, a floor.
	TopTokens float64
	TopOpen   bool
}

// StepBudgetBinding is the binding of p's step budget (StepBudget) against
// steps, a pool's pooled iterations. A nil p has no known budget.
func (p *EngineParams) StepBudgetBinding(steps StepTokens) StepBudgetBinding {
	return steps.Binding(p.StepBudget())
}

// ReadStepBudgetBinding classifies one cumulative tokens-per-iteration
// histogram of counts, iterations at or under each bound (a window's
// increase), against budget: StepTokens.Binding of a pool of that histogram
// alone.
func ReadStepBudgetBinding(steps []domain.LengthBucket, budget int64) StepBudgetBinding {
	var s StepTokens
	s.Add(steps, 1)
	return s.Binding(budget)
}

// Binding classifies the pool against budget, tokens per step of one engine
// core; a budget of 0 or below is not known.
func (s StepTokens) Binding(budget int64) StepBudgetBinding {
	out := StepBudgetBinding{Class: StepBudgetUnknown, Budget: max(budget, 0)}
	switch {
	case s.MixedBounds:
		out.Reason = StepBudgetMixedBounds
		return out
	case s.Samples == 0 || len(s.Buckets) == 0:
		out.Reason = StepBudgetNoSeries
		return out
	}
	cum := s.Buckets
	total := cum[len(cum)-1].Rate
	if !(total > 0) {
		out.Reason = StepBudgetNoSteps
		return out
	}
	out.Iterations = total
	lastFinite := 0.0
	for _, b := range cum {
		if !math.IsInf(b.UpperBound, 1) {
			lastFinite = b.UpperBound
		}
	}
	for _, b := range cum {
		if b.Rate >= StepBudgetTopQuantile*total {
			out.TopTokens, out.TopOpen = b.UpperBound, math.IsInf(b.UpperBound, 1)
			if out.TopOpen {
				out.TopTokens = lastFinite
			}
			break
		}
	}
	if total < StepBudgetMinIterations {
		out.Reason = StepBudgetTooFew
		return out
	}
	if budget <= 0 {
		out.Reason = StepBudgetNotKnown
		return out
	}
	b := float64(budget)
	for _, c := range cum {
		if !math.IsInf(c.UpperBound, 1) && c.UpperBound < StepBudgetNearFraction*b {
			out.NearTokens, out.Share = c.UpperBound, (total-c.Rate)/total
		}
	}
	found := false
	for _, c := range cum {
		if !math.IsInf(c.UpperBound, 1) && c.UpperBound >= b {
			out.BoundTokens, out.OverShare, found = c.UpperBound, (total-c.Rate)/total, true
			break
		}
	}
	if !found {
		out.Reason = StepBudgetTooCoarse
		return out
	}
	if out.OverShare >= StepBudgetBindsShare {
		out.Class = StepBudgetBinds
	} else {
		out.Class = StepBudgetDoesNotBind
	}
	return out
}
