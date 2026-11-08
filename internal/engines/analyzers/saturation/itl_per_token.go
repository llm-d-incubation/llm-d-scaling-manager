package saturation

import (
	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// ITL per token, not per step (issue ev-shindin/llm-scaling-manager#274).
//
// vLLM observes inter_token_latency once per request per engine step, however
// many tokens the step emitted. With speculative decoding or MTP a step emits
// 1 + the accepted draft tokens, so the ITL mean is the step time, and a line
// fitted from it prices a replica's token rate low by that factor -- 1.5-3x
// on typical acceptance -- and the floor orders that many replicas too many.
//
// The ITL line is fitted from the time per token instead: each of the
// variant's rows is read through domain.ReplicaMetrics.ITLPerToken before the
// window takes it, with the accepted tokens per step the collector read over
// the ITL's own window (registration.QuerySpecAcceptedPerStep). A replica
// whose flags speculate and whose counter was not read offers no reading,
// rather than a step time taken for a token time. SGLang's ITL is per token
// already and is read as it is.
//
// The window, the baseline, the held slope and the shared line are then in
// time per token. On a vLLM engine that speculates, that differs from what a
// build without the correction learned, so the learned-state snapshot records
// the unit of each variant's ITL entries and each line (ITLPerToken in
// LearnedState), and a speculating variant's entries in the other unit are
// neither restored nor lent.
//
// The throughput and slo analyzers still read the step time; they keep their
// own lines.

// SwitchITLPerToken is the replay switch for the correction (ev-shindin/llm-scaling-manager#274): on from
// the constructor; a replay turns it off (SetSwitchesForReplay), and the ITL
// line is fitted from the step time, as before.
const SwitchITLPerToken = "itl-per-accepted-token"

// itlWithheldReasons counts, for one cycle of one variant, the readings
// ITLPerToken withheld, by reason, and the positive readings it kept: the
// evidence that withholding has ended. maxAccepted and maxSeen are the bound
// and the largest accepted count read, for the over-bound line.
type itlWithheldReasons struct {
	noCounter   int
	overMax     int
	kept        int
	maxAccepted int
	maxSeen     float64
}

// itlStepOf is how the engine params runs reports its ITL (domain.ITLStep).
func itlStepOf(params *capacity.EngineParams) domain.ITLStep {
	return domain.ITLStep{
		PerToken:    params != nil && params.Engine == inferenceengine.EngineSGLang,
		Speculates:  params.SpeculatesPerStep(),
		MaxAccepted: params.SpeculativeTokens(),
	}
}

// itlRows is rows with variant's read per token (domain.ReplicaMetrics.
// ITLPerToken) under params, the engine flags the variant runs, and how many
// of its readings were withheld, by reason. Other variants' rows are left as
// they are; the window reads only variant's. With the switch off, or when no
// row changes, rows is returned itself; otherwise a copy, since the input is
// shared with the rest of the cycle.
func (a *SaturationAnalyzer) itlRows(rows []domain.ReplicaMetrics, variant string,
	params *capacity.EngineParams) ([]domain.ReplicaMetrics, itlWithheldReasons) {
	var withheld itlWithheldReasons
	if !a.itlPerToken {
		return rows, withheld
	}
	step := itlStepOf(params)
	var out []domain.ReplicaMetrics
	for i, rm := range rows {
		if rm.VariantName != variant {
			continue
		}
		read, why := rm.ITLPerToken(step)
		switch why {
		case domain.ITLNoCounter:
			withheld.noCounter++
		case domain.ITLOverMax:
			withheld.overMax++
			withheld.maxAccepted = step.MaxAccepted
			withheld.maxSeen = max(withheld.maxSeen, rm.SpecAcceptedPerStep)
		default:
			if read.AvgITL > 0 {
				withheld.kept++
			}
		}
		if read.AvgITL == rm.AvgITL && read.AvgITLKnown == rm.AvgITLKnown {
			continue
		}
		if out == nil {
			out = make([]domain.ReplicaMetrics, len(rows))
			copy(out, rows)
		}
		out[i] = read
	}
	if out == nil {
		return rows, withheld
	}
	return out, withheld
}

// noteITLWithheld logs that variant's ITL readings are withheld (itlRows),
// once per change of the reasons, and once when they no longer are. A
// withheld reading counts as noITL in itl-window, which an idle replica reads
// too, so without this line a speculating variant with no ITL line could not
// be told from an idle one. Logged at DEFAULT, once per change rather than per
// cycle, within the default-verbosity budget (ev-shindin/llm-scaling-manager#303).
//
// Withholding has ended only on evidence: a cycle that withheld nothing and
// kept a positive reading. A cycle with neither -- the variant idle, scaled to
// zero, or its rows not read during a rollout -- leaves the state as it is,
// so a load that comes and goes does not log an end and a start each time.
func (a *SaturationAnalyzer) noteITLWithheld(key, variant string, w itlWithheldReasons, logger logr.Logger) {
	on := itlWithheldReasons{noCounter: min(w.noCounter, 1), overMax: min(w.overMax, 1)}
	none := on == (itlWithheldReasons{})
	if none && w.kept == 0 {
		return
	}
	a.mu.Lock()
	last, logged := a.itlWithheldLogged[key]
	if on == last && (logged || none) {
		a.mu.Unlock()
		return
	}
	if none {
		delete(a.itlWithheldLogged, key)
	} else {
		a.itlWithheldLogged[key] = on
	}
	a.mu.Unlock()
	switch {
	case on.noCounter > 0:
		logger.V(logging.DEFAULT).Info("itl-withheld",
			"variant", variant, "key", key, "withheld", w.noCounter+w.overMax,
			"reason", "speculative flags, no vllm:spec_decode_num_accepted_tokens_total",
			"overMax", w.overMax)
	case on.overMax > 0:
		logger.V(logging.DEFAULT).Info("itl-withheld",
			"variant", variant, "key", key, "withheld", w.overMax,
			"reason", "accepted tokens per step above num_speculative_tokens",
			"acceptedPerStep", w.maxSeen, "numSpeculativeTokens", w.maxAccepted)
	default:
		logger.V(logging.DEFAULT).Info("itl-withheld-ended", "variant", variant, "key", key)
	}
}

// itlUnitsDiffer says whether ITL state learned in the unit perToken names
// (itl_per_token.go) is in another unit than this analyzer reads for an
// engine run under params: only on vLLM with speculative decoding, where a
// step is not one token.
func (a *SaturationAnalyzer) itlUnitsDiffer(perToken bool, params *capacity.EngineParams) bool {
	return params.SpeculatesPerStep() && perToken != a.itlPerToken
}
