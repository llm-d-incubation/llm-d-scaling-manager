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

// DecodeSeriesFrom is every cycle of the model q names, oldest first, as the
// capacity planner's experimental TPOT distribution reads it
// (whatif.DecodeCycle): the decode pods' pooled output-length histogram, each
// decode replica's token time and accepted drafts, and the running sequences
// of a rank. It is its own pass, read only when that model is asked for, so
// nothing else a series feeds moves. Its cycles are SeriesFrom's, in the
// same order. q must name the namespace and the model.
func DecodeSeriesFrom(src Source, q StateQuery, v SeriesVariants) ([]whatif.DecodeCycle, error) {
	if q.Namespace == "" || q.ModelID == "" {
		return nil, errors.New("learnreplay: the decode series needs the namespace and the model")
	}
	var out []whatif.DecodeCycle
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
			out = append(out, decodeCycleOf(in, ev.Now, v))
		}
		return true, nil
	})
	return out, err
}

// decodeCycleOf is one cycle of in, read at at, for the decode variant v
// names: whatif.DecodeCycleOf, the builder the controller would read its own
// cycles with (docs/proposals/predicted-latency-in-controller.md).
func decodeCycleOf(in domain.AnalyzerInput, at time.Time, v SeriesVariants) whatif.DecodeCycle {
	return whatif.DecodeCycleOf(in, at, v.Decode)
}
