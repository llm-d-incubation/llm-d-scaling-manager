package learnreplay

import (
	"errors"
	"fmt"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// DecodeSeriesFrom is every cycle of the model q names, oldest first, as the
// capacity planner's experimental TPOT distribution reads it
// (whatif.DecodeCycle): the decode pods' pooled output-length histogram, each
// decode replica's token time and accepted drafts, and the running sequences
// of a rank. It is its own pass, read only when that model is asked for, so
// nothing else a series feeds moves. Its cycles are SeriesFrom's, in the
// same order. q must name the namespace and the model. A row's accepted
// drafts are read against its variant's k as the recording filed it, where
// the planner's switches say so (slo.SwitchSpecAcceptedAtMostK in
// PlannerSwitches): this series is the planner's alone.
func DecodeSeriesFrom(src Source, q StateQuery, v SeriesVariants) ([]whatif.DecodeCycle, error) {
	if q.Namespace == "" || q.ModelID == "" {
		return nil, errors.New("learnreplay: the decode series needs the namespace and the model")
	}
	var out []whatif.DecodeCycle
	atMostK := PlannerSwitches()[PackageSLO][slo.SwitchSpecAcceptedAtMostK]
	drafts := filedDrafts{}
	err := scan(src, func(ev learnrec.RawEvent) (bool, error) {
		if atMostK && ev.Kind == capacity.KindFileScaleTarget {
			var d capacity.FileScaleTargetEvent
			if err := ev.DecodeData(&d); err != nil {
				return false, fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
			}
			drafts.file(d)
			return true, nil
		}
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
			var k whatif.MaxAccepted
			if atMostK {
				k = drafts.of(in)
			}
			out = append(out, decodeCycleOf(in, ev.Now, v, k))
		}
		return true, nil
	})
	return out, err
}

// decodeCycleOf is one cycle of in, read at at, for the decode variant v
// names: whatif.DecodeCycleOf, the builder the controller would read its own
// cycles with (docs/proposals/predicted-latency-in-controller.md), each
// row's accepted drafts read against k (whatif.DecodeCycleAcceptedAtMost);
// nil reads them as they are.
func decodeCycleOf(in domain.AnalyzerInput, at time.Time, v SeriesVariants, k whatif.MaxAccepted) whatif.DecodeCycle {
	return whatif.DecodeCycleAcceptedAtMost(in, at, v.Decode, k)
}
