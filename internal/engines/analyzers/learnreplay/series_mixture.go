package learnreplay

import (
	"errors"
	"fmt"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// MixtureSeriesFrom is every cycle of the model q names, oldest first, as the
// capacity planner's experimental rank-mixture TTFT model reads it
// (whatif.MixtureCycle): the prefill ranks of the variants v names, and the
// prefill and decode pods' pooled histograms and TTFT. It is its own pass,
// read only when that model is asked for, so nothing else a series feeds
// moves. Its cycles are SeriesFrom's, in the same order. q must name the
// namespace and the model.
func MixtureSeriesFrom(src Source, q StateQuery, v SeriesVariants) ([]whatif.MixtureCycle, error) {
	if q.Namespace == "" || q.ModelID == "" {
		return nil, errors.New("learnreplay: the rank mixture's series needs the namespace and the model")
	}
	var out []whatif.MixtureCycle
	err := scan(src, func(ev learnrec.RawEvent) (bool, error) {
		if ev.Kind != saturation.KindAnalyze {
			return true, nil
		}
		var d saturation.AnalyzeEvent
		if err := ev.DecodeData(&d); err != nil {
			return false, fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
		in, err := decodeInput(d.Input)
		if err != nil {
			return false, fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
		if in.Namespace == q.Namespace && in.ModelID == q.ModelID {
			out = append(out, mixtureCycleOf(in, ev.Now, v))
		}
		return true, nil
	})
	return out, err
}

// mixtureCycleOf is one cycle of in, read at at, for the variants v names:
// whatif.MixtureCycleOf, the builder the controller would read its own
// cycles with (docs/proposals/predicted-latency-in-controller.md).
func mixtureCycleOf(in domain.AnalyzerInput, at time.Time, v SeriesVariants) whatif.MixtureCycle {
	return whatif.MixtureCycleOf(in, at, v.Prefill, v.Decode)
}
