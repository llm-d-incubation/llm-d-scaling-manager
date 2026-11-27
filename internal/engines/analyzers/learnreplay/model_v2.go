package learnreplay

import (
	"context"
	"fmt"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// ModelV2At replays the recording src opens to the model's cycle q.Cycle
// (counting its saturation Analyze calls from 1; 0 is its last) with a slo
// analyzer observing the model's v2 inputs on each of its cycles, as the
// engine has it observe a passive model: the input the saturation analyzer
// analyzed, and the router's reading it carried. At that cycle the analyzer
// refits its v2 learners, and ModelV2At returns what they fitted: what the
// controller's v2 learners fit on the same cycles, for the parity test
// against the capacity planner's experimental models. edit, when set, changes
// each input before the analyzer observes it (a negative control).
//
// The analyzer is a fresh one reading the replay's capacity store, so it
// sees the engine flags the recording filed (the prefill and decode slots),
// and it refits only at the cycle asked, in the foreground: a refit every
// half hour of the recording would cost the fits the answer never reads.
func ModelV2At(ctx context.Context, src Source, q StateQuery, edit func(*domain.AnalyzerInput)) (slo.ModelFit, error) {
	if q.Cycle < 0 {
		return slo.ModelFit{}, queryErrorf("cycle %d, want 1 or more, or 0 for the last", q.Cycle)
	}
	ns, model, total, err := resolveModel(src, q)
	if err != nil {
		return slo.ModelFit{}, err
	}
	cycle := q.Cycle
	if cycle == 0 {
		cycle = total
	}
	if cycle > total {
		return slo.ModelFit{}, queryErrorf("cycle %d asked for, and the recording holds %d of %s/%s", cycle, total, ns, model)
	}
	var (
		a        *slo.Analyzer
		n        int
		inputErr error
	)
	opts := Options{Mode: Normal, discardRecording: true, sloOptional: true}
	opts.stop = func(r *replayer, ev learnrec.RawEvent) bool {
		if ev.Kind != saturation.KindAnalyze {
			return false
		}
		raw, _ := learnrec.Raw(ev.Data).Member("Input")
		in, err := decodeInput(raw)
		if err != nil {
			inputErr = err
			return true
		}
		if in.Namespace != ns || in.ModelID != model {
			return false
		}
		n++
		if a == nil {
			// Past any recording: the one refit is RefitModel's.
			a = slo.New(nil, r.store).WithClock(r.clock).WithModelRefits(365*24*time.Hour, false)
		}
		if edit != nil {
			edit(&in)
		}
		a.ObserveModelInputs(in, in.RouterLatency)
		return n >= cycle
	}
	if _, err := replayFrom(ctx, src, opts); err != nil {
		return slo.ModelFit{}, err
	}
	if inputErr != nil {
		return slo.ModelFit{}, fmt.Errorf("learnreplay: %w", inputErr)
	}
	if n < cycle || a == nil {
		return slo.ModelFit{}, fmt.Errorf("learnreplay: the replay ended after %d of the %d cycles of %s/%s", n, cycle, ns, model)
	}
	fit, _ := a.RefitModel(ns, model)
	return fit, nil
}
