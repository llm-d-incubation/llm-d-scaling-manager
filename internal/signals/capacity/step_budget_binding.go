package capacity

import (
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// Whether an engine pool's step budget binds (issue
// llm-d-incubation/llm-d-scaling-manager#499). The budget itself is an engine
// argument (--max-num-batched-tokens, StepBudget); whether the scheduler ever
// fills it is a measurement, read from vLLM's vllm:iteration_tokens_total and
// recorded per pod as domain.ReplicaMetrics.IterationTokensBuckets. A budget
// that never binds is nominal: a prompt rides in one step and no chunk term
// applies. The class is detected from the engine's own series, never keyed
// by model or fleet, so the controller's analyzers and the capacity planner
// read it alike: PoolStepTokens over a variant's rows, then
// EngineParams.StepBudgetBinding.
//
// What the series counts is not the tokens a step scheduled. vLLM V1
// observes, once per engine iteration, the computed prompt tokens (the prompt
// net of its cache hits) of every request whose prefill completed in that
// iteration, whole, plus the tokens generated (vllm/v1/metrics/loggers.py,
// IterationStats). A chunked prompt counts nothing in the iterations that ran
// its earlier chunks and its whole computed length in the last. So an
// iteration that counts more than the budget is direct evidence: prompts
// that long could not be computed in one step, and the scheduler cut them at
// the budget. An iteration that counts near the budget, but under it, is a
// step that completed that much prefill, much of it perhaps computed in
// earlier steps. The share above the near bound is therefore read as how
// often the pool's prefill work reaches the budget, at the histogram's
// resolution; a pool whose iterations stay well under it does not bind.

// StepBudgetClass is what a pool's iterations say of its step budget.
type StepBudgetClass string

const (
	// StepBudgetBinds is a pool where at least StepBudgetBindsShare of the
	// iterations counted more than the near bound: its prompts reach the
	// budget, and the scheduler chunks them.
	StepBudgetBinds StepBudgetClass = "binds"
	// StepBudgetDoesNotBind is a pool whose iterations stay under the near
	// bound but for fewer than StepBudgetBindsShare: a prompt rides in one
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
	// StepBudgetNotKnown is a pool whose budget no argument or default
	// resolves: TopTokens is still read.
	StepBudgetNotKnown = "step budget not known"
	// StepBudgetTooCoarse is a histogram with no bound under
	// StepBudgetNearFraction of the budget, so an iteration near the budget
	// cannot be told from a small one.
	StepBudgetTooCoarse = "no histogram bound under the budget"
)

const (
	// StepBudgetBindsShare is the least share of iterations above the near
	// bound that is read as a binding budget.
	StepBudgetBindsShare = 0.05
	// StepBudgetNearFraction places the near bound: the largest histogram
	// bound under this fraction of the budget. With vLLM's power-of-two
	// bounds and a budget of 8192 it is 4096.
	StepBudgetNearFraction = 0.9
	// StepBudgetTopQuantile is the share of iterations TopTokens holds.
	StepBudgetTopQuantile = 0.999
)

// StepBudgetBinding is a pool's step budget against its measured
// iterations.
type StepBudgetBinding struct {
	Class StepBudgetClass
	// Reason is why Class is StepBudgetUnknown; "" otherwise.
	Reason string
	// Budget is the budget read against, tokens per step per rank; 0 where
	// it is not known.
	Budget int64
	// StepRate is the iterations a second the pooled histogram counts,
	// summed over its pods and cycles: the weight of the reading, 0 with
	// none.
	StepRate float64
	// NearTokens is the near bound, and Share the share of iterations that
	// counted more; both 0 where not read.
	NearTokens float64
	Share      float64
	// OverShare is the share of iterations that counted more than the
	// smallest bound at or over the budget: completions of prompts the
	// budget chunked. 0 where the histogram has no such bound.
	OverShare float64
	// TopTokens is the bound at or under which StepBudgetTopQuantile of the
	// iterations counted: the observed top. TopOpen says that share reached
	// the +Inf bucket, and TopTokens is then the last finite bound, a floor.
	TopTokens float64
	TopOpen   bool
}

// StepBudgetBinding is the binding of p's step budget (StepBudget) against
// steps, a pool's pooled tokens-per-iteration histogram (PoolStepTokens). A
// nil p has no known budget.
func (p *EngineParams) StepBudgetBinding(steps []domain.LengthBucket) StepBudgetBinding {
	return ReadStepBudgetBinding(steps, p.StepBudget())
}

// PoolStepTokens adds tokens-per-iteration histograms into one: each bound's
// rate summed, sorted by bound. Pool a variant's rows, of one cycle or many,
// by passing each row's IterationTokensBuckets; an empty or nil set adds
// nothing, and nil is returned where every set was empty.
func PoolStepTokens(sets ...[]domain.LengthBucket) []domain.LengthBucket {
	sum := map[float64]float64{}
	for _, set := range sets {
		for _, b := range set {
			if math.IsNaN(b.UpperBound) || math.IsNaN(b.Rate) || math.IsInf(b.Rate, 0) || b.Rate < 0 {
				continue
			}
			sum[b.UpperBound] += b.Rate
		}
	}
	if len(sum) == 0 {
		return nil
	}
	out := make([]domain.LengthBucket, 0, len(sum))
	for ub, r := range sum {
		out = append(out, domain.LengthBucket{UpperBound: ub, Rate: r})
	}
	slices.SortFunc(out, func(a, b domain.LengthBucket) int {
		switch {
		case a.UpperBound < b.UpperBound:
			return -1
		case a.UpperBound > b.UpperBound:
			return 1
		}
		return 0
	})
	return out
}

// ReadStepBudgetBinding classifies steps, a cumulative tokens-per-iteration
// histogram (iterations a second at or under each bound, sorted by bound),
// against budget, tokens per step; a budget of 0 or below is not known. The
// cumulative rates are made monotone first: rate() extrapolates each bucket
// on its own, and a bound may read a hair under the one below.
func ReadStepBudgetBinding(steps []domain.LengthBucket, budget int64) StepBudgetBinding {
	out := StepBudgetBinding{Class: StepBudgetUnknown, Budget: max(budget, 0)}
	if len(steps) == 0 {
		out.Reason = StepBudgetNoSeries
		return out
	}
	cum := make([]domain.LengthBucket, len(steps))
	running := 0.0
	for i, b := range steps {
		running = max(running, b.Rate)
		cum[i] = domain.LengthBucket{UpperBound: b.UpperBound, Rate: running}
	}
	total := cum[len(cum)-1].Rate
	if !(total > 0) {
		out.Reason = StepBudgetNoSteps
		return out
	}
	out.StepRate = total
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
	if budget <= 0 {
		out.Reason = StepBudgetNotKnown
		return out
	}
	b := float64(budget)
	nearAt, nearFound := 0.0, false
	for _, c := range cum {
		if !math.IsInf(c.UpperBound, 1) && c.UpperBound < StepBudgetNearFraction*b {
			out.NearTokens, nearAt, nearFound = c.UpperBound, c.Rate, true
		}
	}
	for _, c := range cum {
		if !math.IsInf(c.UpperBound, 1) && c.UpperBound >= b {
			out.OverShare = (total - c.Rate) / total
			break
		}
	}
	if !nearFound {
		out.Reason = StepBudgetTooCoarse
		return out
	}
	out.Share = (total - nearAt) / total
	if out.Share >= StepBudgetBindsShare {
		out.Class = StepBudgetBinds
	} else {
		out.Class = StepBudgetDoesNotBind
	}
	return out
}
