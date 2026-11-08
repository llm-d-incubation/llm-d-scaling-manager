package registration

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
)

// QuerySpecAcceptedPerStep is the query name for the draft tokens vLLM's
// speculative decoding (or MTP) accepted per decode step, per instance (the
// model_name, instance and pod labels), over ITLWindow: the accepted-token
// rate over the rate of ITL observations, summed over the engine ranks behind
// that instance, like the ITL mean (QueryAvgITL) it corrects. A pod of
// several instances is merged afterwards (mergePodInstances).
//
// vLLM observes inter_token_latency once per request per engine step, however
// many tokens the step emitted, so with speculation the ITL mean is the step
// time. A step emits 1 + this many tokens, and the time per token is the ITL
// mean over that (issue ev-shindin/llm-scaling-manager#274). The ITL count is the denominator rather than
// vllm:spec_decode_num_drafts_total: a step that proposed no draft (an n-gram
// drafter that found no match) is an ITL observation and no draft, and
// dividing by drafts would over-state the tokens per step.
//
// vLLM only, and so registered once under its bare name: SGLang observes its
// ITL per token already (each step's interval divided by the tokens it
// emitted), so it needs no correction and has no variant here. vLLM registers
// the counter only when a speculative configuration is set, so on any other
// engine the query returns no series and SpecAcceptedPerStepKnown stays
// unset; a present series over a window with no decode step is NaN, which is
// not a reading either.
// Source: vllm:spec_decode_num_accepted_tokens_total and
// vllm:inter_token_latency_seconds_count (counters)
const QuerySpecAcceptedPerStep = "spec_accepted_per_step"

// registerSpecDecodeQueries registers QuerySpecAcceptedPerStep for vLLM.
func registerSpecDecodeQueries(registry *source.QueryList) {
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySpecAcceptedPerStep,
		Type:        source.QueryTypePromQL,
		Template:    perPodMean("vllm:spec_decode_num_accepted_tokens_total", "vllm:inter_token_latency_seconds_count", ITLWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Speculative draft tokens accepted per decode step per pod over the ITL window",
	})
}
