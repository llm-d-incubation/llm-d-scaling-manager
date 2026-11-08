package learnreplay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/throughput"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/enginerec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// Generate (docs/proposals/analyzer-learner.md, 2.9, R2) is the third mode:
// it makes a recording from a capture, a file of cycle inputs that
// `whatif capture` built from Prometheus alone (internal/whatifsource).
// A capture has no engine events and no store records to hold a replay to,
// so Generate plays each cycle through the analyzers itself, in the order an
// engine cycle calls them, and compares nothing: its recording, carrying the
// commit of the build that generated it, is what it produces. It is an
// ordinary recording, so Outputs reads it as it reads a replay's, and
// learnergolden add accepts it at that commit.
//
// A capture is a learnrec stream: the start line, one window line
// (KindCaptureWindow), then one cycle line (KindCaptureCycle) per model per
// step, the lines of a step together and the steps in time order.
//
// Per step, at the step's instant, as the engine's cycle does
// (steadystate engine.go beginSLOCycle, optimizeV2's prune,
// notePresentVariants, evictStaleLearnedState; engine_v2.go runAnalyzers
// and buildAnalyzerInput):
//
//  1. slo BeginCycle;
//  2. the settle tracker keeps only the models with a variant that has
//     replicas this step (the engine's retainSettle, from optimizeV2's
//     prune): a model parked whole is in no model group, and its record is
//     forgotten. A step with no such model stops here, as the engine's
//     cycle with no active variant returns before anything below;
//  3. NoteVariantsPresent on saturation, slo and then throughput, model by
//     model in key order, for the variants with replicas and then for the
//     parked ones;
//  4. the three sweeps: saturation's EvictStaleHistory, the store's
//     EvictStale and slo's EvictStale;
//
// then per model, in key order:
//
//  5. the settle tracker: observed with the model's rows, or held for a
//     model with variants and no rows (the engine's onNothingObserved), which
//     then runs nothing more;
//  6. per variant, in name order, StableAcceleratorFor and then
//     FileScaleTarget with the capture's engine parameters (the engine's
//     LoadFromScaleTarget). A capture has no annotation history, so no
//     variant is forgotten on request;
//  7. Analyze: saturation, then slo and throughput when the policy lists
//     them and the start built them, the slo analyzer's router serving the
//     captured router latency.
//
// A model whose variants are all parked is only stamped present (3), as the
// engine, which analyzes active variants only, does.
//
// The recording's start line carries the capture window's flags as its
// Caveats, so a golden made from it says what the capture could not see.

// The kinds of a capture's lines after its start line (Generate).
const (
	// KindCaptureWindow is a capture's second line, with CaptureWindow.
	KindCaptureWindow = "capture.Window"
	// KindCaptureCycle is one model's input at one step, with CaptureCycle.
	KindCaptureCycle = "capture.Cycle"
)

// CaptureWindow describes what a capture covers and how it found the
// fleet: the window, the step that plays the optimize interval, and each
// variant's engine configuration, which the cycles do not carry.
type CaptureWindow struct {
	Namespace string
	From, To  time.Time
	// Step is the interval between cycles: the optimize interval the
	// generated run is taken to have had.
	Step time.Duration
	// Roles names where the role map came from: "kube-state-metrics",
	// "role-of", or both joined by "+".
	Roles string
	// Flags are the capture's caveats, each one line: roles inferred from
	// Pod names, Pods admitted without readiness, engine parameters from
	// flags rather than the window's workloads.
	Flags []string `learnrec:"omitzero"`
	// Variants is every variant the capture found, by model and name.
	Variants []CaptureVariant
}

// CaptureVariant is one variant's engine configuration, filed in the store
// each cycle the variant has replicas (FileScaleTarget).
type CaptureVariant struct {
	ModelID     string
	Name        string
	Role        string
	Accelerator string
	GpuCount    int
	Params      capacity.EngineParams
}

// CaptureCycle is one model's cycle at one step: the input every analyzer
// reads (learnrec.Input, its Config the merged policy), and the model's
// variants that were parked at zero replicas then, stamped present as the
// engine stamps the parked variants it can list.
type CaptureCycle struct {
	Input  learnrec.Input
	Parked []string `learnrec:"omitzero"`
}

// capturedCycle is a CaptureCycle as read back: the input still raw, for
// decodeInput.
type capturedCycle struct {
	Input  learnrec.Raw
	Parked []string
}

// genModel is one model's cycle of a step, decoded.
type genModel struct {
	key    string
	input  domain.AnalyzerInput
	policy *config.ScalingPolicy
	parked []string
}

// CaptureError is a capture Generate cannot play: unreadable, cut off, out
// of order, or naming a variant its window does not. The capture is at
// fault, not the analyzers; cmd/whatif exits 2 for it, and 1 for any other
// error.
type CaptureError struct{ Err error }

func (e *CaptureError) Error() string { return e.Err.Error() }
func (e *CaptureError) Unwrap() error { return e.Err }

// badCapture marks err as the capture's fault (CaptureError).
func badCapture(err error) error { return &CaptureError{Err: err} }

// generate plays a capture through the analyzers (Mode Generate).
func generate(ctx context.Context, capture []byte, opts Options) (*Report, error) {
	if len(opts.Switches) > 0 {
		return nil, errors.New("learnreplay: Generate takes no switches; generate the recording, then replay it with them")
	}
	read, err := learnrec.Read(capture)
	if err != nil {
		return nil, badCapture(err)
	}
	events := read.Events
	if read.TornTail != "" {
		return nil, badCapture(errors.New("learnreplay: the capture's last line is cut off; a capture is written whole or not at all"))
	}
	if len(events) < 2 || events[0].Kind != learnrec.KindStart || events[1].Kind != KindCaptureWindow {
		return nil, badCapture(errors.New("learnreplay: a capture starts with a start line and a window line"))
	}
	var start learnrec.Start
	if err := events[0].DecodeData(&start); err != nil {
		return nil, badCapture(fmt.Errorf("learnreplay: start: %w", err))
	}
	var window CaptureWindow
	if err := events[1].DecodeData(&window); err != nil {
		return nil, badCapture(fmt.Errorf("learnreplay: window: %w", err))
	}
	if window.Step <= 0 {
		return nil, badCapture(fmt.Errorf("learnreplay: the capture's step is %v", window.Step))
	}
	// The recording is this build's: it carries this build's commit, so a
	// replay at that commit (learnergolden add) reproduces it. It carries
	// the capture's caveats, so a golden made from it shows them.
	start.Commit = learnrec.Commit()
	start.Caveats = slices.Clone(window.Flags)
	var buf bytes.Buffer
	rec := learnrec.New(&buf, 0)
	opts.Engine = noEngine{}
	r, err := build(start, events[0].Now, opts, rec)
	if err != nil {
		// The start line is the capture's.
		return nil, badCapture(err)
	}
	r.settle.tracker.SetInterval(window.Step)
	rec.Record(learnrec.KindStart, events[0].Now, start)
	r.rep.Events = len(events)

	variants := make(map[string]CaptureVariant, len(window.Variants))
	for _, v := range window.Variants {
		variants[utils.GetNamespacedKey(v.ModelID, v.Name)] = v
	}
	steps, err := captureSteps(events[2:])
	if err != nil {
		return nil, badCapture(err)
	}
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("learnreplay: %w", err)
		}
		if err := r.generateStep(ctx, step.at, step.models, variants, window.Step); err != nil {
			return nil, fmt.Errorf("learnreplay: step %s: %w", step.at.Format(time.RFC3339), err)
		}
		r.rep.Applied++
	}
	if err := rec.Err(); err != nil {
		return nil, fmt.Errorf("learnreplay: the generated recording failed: %w", err)
	}
	r.rep.Recording = buf.Bytes()
	r.rep.Paths = r.sat.PathCounts()
	maps.Copy(r.rep.Paths, r.settle.pathCounts())
	if r.slo != nil {
		maps.Copy(r.rep.Paths, r.slo.PathCounts())
	}
	if r.tp != nil {
		maps.Copy(r.rep.Paths, r.tp.PathCounts())
	}
	return r.rep, nil
}

// noEngine is the engine replayer of a generated run: a capture has no
// engine events, and a registered engine would add its path counters to a
// report that ran none of its paths.
type noEngine struct{}

func (noEngine) Owns(string) bool { return false }
func (noEngine) Apply(context.Context, learnrec.RawEvent, enginerec.Replayed, *learnrec.Recorder) error {
	return nil
}
func (noEngine) PathCounts() map[string]int64 { return map[string]int64{} }

// captureStep is the cycles of one step.
type captureStep struct {
	at     time.Time
	models []genModel
}

// captureSteps groups the cycle lines by step, in the order they were
// written: each step's lines together, the steps in time order, a model at
// most once a step.
func captureSteps(events []learnrec.RawEvent) ([]captureStep, error) {
	var steps []captureStep
	seen := map[string]bool{}
	for _, ev := range events {
		if ev.Kind != KindCaptureCycle {
			return nil, fmt.Errorf("learnreplay: seq %d: a %s line in a capture, which holds only cycles after its window", ev.Seq, ev.Kind)
		}
		var c capturedCycle
		if err := ev.DecodeData(&c); err != nil {
			return nil, fmt.Errorf("learnreplay: seq %d: %w", ev.Seq, err)
		}
		in, err := decodeInput(c.Input)
		if err != nil {
			return nil, fmt.Errorf("learnreplay: seq %d: %w", ev.Seq, err)
		}
		policy, _ := in.Config.(*config.ScalingPolicy)
		m := genModel{key: utils.GetNamespacedKey(in.Namespace, in.ModelID), input: in, policy: policy, parked: c.Parked}
		switch n := len(steps); {
		case n == 0 || ev.Now.After(steps[n-1].at):
			steps = append(steps, captureStep{at: ev.Now})
			seen = map[string]bool{}
		case !ev.Now.Equal(steps[n-1].at):
			return nil, fmt.Errorf("learnreplay: seq %d: a cycle at %s after one at %s", ev.Seq, ev.Now, steps[n-1].at)
		}
		if seen[m.key] {
			return nil, fmt.Errorf("learnreplay: seq %d: model %s twice in one step", ev.Seq, m.key)
		}
		seen[m.key] = true
		last := &steps[len(steps)-1]
		last.models = append(last.models, m)
	}
	for i := range steps {
		slices.SortFunc(steps[i].models, func(a, b genModel) int { return strings.Compare(a.key, b.key) })
	}
	return steps, nil
}

// generateStep plays one step's cycles, in the engine's order (Generate).
func (r *replayer) generateStep(ctx context.Context, now time.Time, models []genModel,
	variants map[string]CaptureVariant, interval time.Duration) error {
	r.clock.SetTime(now)
	// 1. The slo analyzer counts its judging windows by the cycle.
	if r.slo != nil {
		r.slo.BeginCycle()
	}
	// 2. The settle tracker keeps the models with a variant that has
	// replicas, as the engine's prune does; a step with none is the
	// engine's cycle with no active variant, which ends here.
	active := map[string]bool{}
	for _, m := range models {
		if len(m.input.VariantStates) > 0 {
			active[m.key] = true
		}
	}
	r.settle.tracker.Retain(active)
	if len(active) == 0 {
		return nil
	}
	// 3. Presence, before the sweeps: the active variants, then the parked.
	note := func(ns, model string, names []string) {
		if len(names) == 0 {
			return
		}
		names = slices.Clone(names)
		slices.Sort(names)
		r.sat.NoteVariantsPresent(ns, model, names)
		if r.slo != nil {
			r.slo.NoteVariantsPresent(ns, model, names)
		}
		if r.tp != nil {
			r.tp.NoteVariantsPresent(ns, model, names)
		}
	}
	for _, m := range models {
		names := make([]string, 0, len(m.input.VariantStates))
		for _, vs := range m.input.VariantStates {
			names = append(names, vs.VariantName)
		}
		note(m.input.Namespace, m.input.ModelID, names)
	}
	for _, m := range models {
		note(m.input.Namespace, m.input.ModelID, m.parked)
	}
	// 4. The sweeps, before anything stamps state this cycle.
	r.sat.EvictStaleHistory()
	r.store.EvictStale(now, capacity.EvictionTimeout)
	if r.slo != nil {
		r.slo.EvictStale(now)
	}
	for _, m := range models {
		if len(m.input.VariantStates) == 0 {
			continue
		}
		if err := r.generateModel(ctx, now, m, variants, interval); err != nil {
			return fmt.Errorf("model %s: %w", m.key, err)
		}
	}
	return nil
}

// generateModel plays one model's cycle: the settle tracker, the store, the
// analyzers (steps 5 to 7 of Generate).
func (r *replayer) generateModel(ctx context.Context, now time.Time, m genModel,
	variants map[string]CaptureVariant, interval time.Duration) error {
	in := m.input
	ev := settle.ObserveEvent{Namespace: in.Namespace, ModelID: in.ModelID, Interval: interval}
	// 5. The settle tracker first; a model with no rows is held, and the
	// analyzers do not run for it.
	if len(in.ReplicaMetrics) == 0 || m.policy == nil {
		r.rec.Record(settle.KindHold, now, ev)
		r.settle.tracker.Hold(m.key, now)
		return nil
	}
	r.rec.Record(settle.KindObserve, now, ev)
	roles := make(map[string]string, len(in.VariantStates))
	for _, vs := range in.VariantStates {
		roles[vs.VariantName] = vs.Role
	}
	r.settle.tracker.Observe(m.key, settlerow.Of(in.ReplicaMetrics, roles), now)

	// 6. Each variant's scale target, filed under its stabilised
	// accelerator.
	names := make([]string, 0, len(in.VariantStates))
	for _, vs := range in.VariantStates {
		names = append(names, vs.VariantName)
	}
	slices.Sort(names)
	for _, name := range names {
		v, ok := variants[utils.GetNamespacedKey(in.ModelID, name)]
		if !ok {
			return badCapture(fmt.Errorf("variant %s is in a cycle and not in the capture's window", name))
		}
		accel := r.sat.StableAcceleratorFor(in.Namespace, name, v.Accelerator)
		r.store.FileScaleTarget(in.Namespace, in.ModelID, name, accel, v.GpuCount, v.Params, false, now)
	}

	// 7. Saturation first, then the listed analyzers.
	r.rep.Analyzes[saturation.KindAnalyze]++
	if _, err := r.sat.Analyze(ctx, in); err != nil {
		// The engine's cycle fails the model here, and so does this one. The
		// error is not lost: the analyzer's own recorder writes it into the
		// Analyze event (saturation.AnalyzeEvent.Error, recording.go), which
		// is what a replay and the goldens compare; the run goes on to the
		// next model, as the engine's does.
		return nil
	}
	if r.slo != nil && m.policy.AnalyzerEnabled(domain.SLOAnalyzerName) {
		if r.router != nil {
			r.router.latency, r.router.called = in.RouterLatency, false
		}
		r.rep.Analyzes[slo.KindAnalyze]++
		_, _ = r.slo.Analyze(ctx, in)
	}
	if r.tp != nil && m.policy.AnalyzerEnabled(throughput.AnalyzerName) {
		r.rep.Analyzes[throughput.KindAnalyze]++
		_, _ = r.tp.Analyze(ctx, in)
	}
	return nil
}
