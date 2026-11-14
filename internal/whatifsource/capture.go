package whatifsource

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	promsource "github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/prometheus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/throughput"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// The flags a capture can carry (learnreplay.CaptureWindow.Flags).
const (
	// FlagRolesInferred: a --role-of selector picked Pods by name, because
	// kube-state-metrics exported no labels for them.
	FlagRolesInferred = "roles inferred from Pod names: kube-state-metrics exports no labels for them"
	// FlagNoReadiness: no kube-state-metrics, so every Pod was taken as
	// Ready, with no Ready time or start time.
	FlagNoReadiness = "admitted without readiness: no kube-state-metrics, every Pod read as Ready"
	// FlagNoRoleLabel: kube-state-metrics exports no llm-d.ai/role label
	// and no --role-of named a role, so every variant reads as both.
	FlagNoRoleLabel = "no llm-d.ai/role label in kube-state-metrics and no --role-of: every variant read as both"
	// FlagKSMGaps: kube-state-metrics listed no Pod at some steps; their
	// Pods are attributed by --role-of alone and read as Ready. A format:
	// the steps without, and all the steps.
	FlagKSMGaps = "kube-state-metrics listed no Pod at %d of %d steps: those steps' Pods attributed by --role-of alone and read as Ready"
	// FlagParamsFromFlags: the engine parameters (batch cap, ranks, ...)
	// are not in kube-state-metrics; they came from the capture's flags or
	// their defaults, which may differ from the window's workloads.
	FlagParamsFromFlags = "engine parameters from flags, which may differ from the window's workloads"
	// FlagGPUsFromFlags and FlagAcceleratorFromFlags: the GPU count or the
	// accelerator could not be read from kube-state-metrics.
	FlagGPUsFromFlags        = "GPUs per replica from flags: kube-state-metrics has no GPU request for a variant"
	FlagAcceleratorFromFlags = "accelerator from flags: kube-state-metrics has no node label for a variant"
	// FlagNoRollouts: a capture cannot see a rollout, so Generate files every
	// scale target as settled (not rolling out), and a variant rolled out in
	// the window reads as one that was not.
	FlagNoRollouts = "rollouts not seen: every scale target filed as not rolling out"
	// FlagMultiPodGroups: a variant runs multi-Pod groups (a LeaderWorkerSet,
	// or Pods a StatefulSet owns), whose replica spans several Pods; its GPUs
	// per replica are read from one Pod's request, not the group's. A format:
	// how many variants.
	FlagMultiPodGroups = "multi-Pod groups in %d variants: GPUs per replica read from one Pod's request, not the group's"
)

// Options configure a capture.
type Options struct {
	// Prometheus is the API every query is asked of, as range queries only.
	Prometheus promv1.API
	Namespace  string
	// From and To bound the window; Step is the interval between cycles,
	// which the generated run takes as its optimize interval.
	From, To time.Time
	Step     time.Duration
	// KubeStateMetrics reads the fleet from kube-state-metrics in the same
	// Prometheus: Pods, owners, readiness, GPUs, nodes, replicas.
	KubeStateMetrics bool
	// RoleOf is every --role-of; when any is given it decides each Pod's
	// variant and role.
	RoleOf []RoleOf
	// Policy is the scaling policy's default entry; nil takes the defaults.
	Policy *config.ScalingPolicy
	// Engine is the inference engine the variants run, and EngineArgs the
	// engine's arguments, parsed as the engine parses a container's
	// (capacity.ParseEngineArgs). BatchCap, when positive, adds
	// --max-num-seqs (vLLM) or --max-running-requests (SGLang).
	Engine     inferenceengine.Engine
	EngineArgs []string
	BatchCap   int
	// GPUsPerReplica and Accelerator stand in for what kube-state-metrics
	// does not give.
	GPUsPerReplica int
	Accelerator    string
	// Force captures a window of more than MaxStepQueries steps times
	// queries, or more than MaxSamples samples.
	Force bool
	// RangeQueryPoints bounds the steps of one range query; a longer window
	// is asked in parts and joined. Zero is Prometheus' bound
	// (maxPointsPerQuery); tests set it small to split a short window.
	RangeQueryPoints int
	// MaxSamples bounds the samples held in memory; zero is MaxSamples.
	// Tests set it small.
	MaxSamples int
}

// MaxStepQueries bounds a capture's steps times the queries it may ask,
// checked before any query: 12,500 steps, a little over two days at a 15 s
// step, with the 80 queries a capture with kube-state-metrics registers
// (67 without). It bounds the load on Prometheus, not memory, which grows
// with the series too (MaxSamples). A larger window needs Options.Force
// (--force).
const MaxStepQueries = 1_000_000

// MaxSamples bounds the samples a capture holds: every range query's
// samples stay in memory for the whole capture, one per series per step,
// at about 60 bytes each, so 20 million is a little over a gigabyte. The
// count is taken as the answers arrive, and the capture stops at the first
// answer past it. A larger capture needs Options.Force (--force).
const MaxSamples = 20_000_000

// The errors a capture fails with, by whose fault it is (IsInputError).
var (
	// ErrInvalidOptions is a capture's options that cannot be captured: no
	// namespace, a step that is not positive, a window that ends first...
	ErrInvalidOptions = errors.New("invalid capture options")
	// ErrNoRoleMap is a capture with neither --kube-state-metrics nor
	// --role-of: nothing can tell a prefill Pod from a decode one.
	ErrNoRoleMap = errors.New("cannot tell prefill from decode pods: pass --kube-state-metrics, or --role-of ROLE=SELECTOR")
	// ErrRoleOverlap is --role-of selectors that can pick the same Pod: two
	// whose inferred Pod-name prefixes nest, or a Pod two selectors match.
	ErrRoleOverlap = errors.New("--role-of selectors overlap")
	// ErrWindowTooLarge is a window of more steps times queries than
	// MaxStepQueries, or more samples than MaxSamples, without Force.
	ErrWindowTooLarge = errors.New("the window is too large")
	// ErrNothingAttributed is a window in which no Pod the role map
	// attributes had a KV-cache series: the namespace, the window or the
	// selectors are wrong.
	ErrNothingAttributed = errors.New("no Pod the role map attributes had a KV-cache series")
	// ErrQueryFailed is a range query Prometheus failed: not the caller's
	// input, but the server's limit, load or reach.
	ErrQueryFailed = errors.New("a range query failed")
)

// IsInputError reports whether err is the caller's input at fault (the
// options, the role map, the window), rather than Prometheus: cmd/capacity-planner
// exits 2 for one and 1 for any other.
func IsInputError(err error) bool {
	for _, input := range []error{ErrInvalidOptions, ErrNoRoleMap, ErrRoleOverlap, ErrWindowTooLarge, ErrNothingAttributed} {
		if errors.Is(err, input) {
			return true
		}
	}
	return false
}

// Summary is what a capture wrote.
type Summary struct {
	// Steps is the window's steps, and Cycles the cycle lines written: one
	// per model per step at which the model had a variant.
	Steps, Cycles int
	// Rows is the replica rows the cycles hold.
	Rows int
	// Variants is the window line's variants, and Flags its caveats.
	Variants []learnreplay.CaptureVariant
	Flags    []string
	// RangeQueries is how many range queries were asked, and Samples how
	// many samples their answers held.
	RangeQueries, Samples int
}

// validate is check, its errors marked ErrInvalidOptions.
func (o *Options) validate() error {
	if err := o.check(); err != nil {
		if errors.Is(err, ErrNoRoleMap) || errors.Is(err, ErrRoleOverlap) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	}
	return nil
}

func (o *Options) check() error {
	switch {
	case o.Prometheus == nil:
		return errors.New("no Prometheus")
	case o.Namespace == "":
		return errors.New("no namespace")
	case o.Step <= 0:
		return fmt.Errorf("step %v is not positive", o.Step)
	case o.Step%time.Millisecond != 0:
		return fmt.Errorf("step %v is not a whole number of milliseconds, which is what Prometheus keeps", o.Step)
	case o.From.IsZero() || o.To.IsZero():
		return errors.New("the window needs both ends")
	case o.To.Before(o.From):
		return fmt.Errorf("the window ends (%s) before it starts (%s)", o.To.Format(time.RFC3339), o.From.Format(time.RFC3339))
	case !o.KubeStateMetrics && len(o.RoleOf) == 0:
		return ErrNoRoleMap
	case o.BatchCap < 0:
		return fmt.Errorf("batch cap %d is negative", o.BatchCap)
	case o.GPUsPerReplica < 0:
		return fmt.Errorf("GPUs per replica %d is negative", o.GPUsPerReplica)
	}
	return CheckRoleOf(o.RoleOf)
}

// CheckRoleOf refuses --role-of selectors whose inferred Pod-name prefixes
// nest: app=qwen infers "qwen-", which also begins every "qwen-prefill-"
// Pod, so the first selector would claim the second's Pods (and the
// router's, "qwen-epp-"). A Pod both selectors match by their labels is
// refused when it is read (fleet attribute).
func CheckRoleOf(roles []RoleOf) error {
	for i, a := range roles {
		for j, b := range roles {
			if i != j && a.prefix != "" && b.prefix != "" && strings.HasPrefix(b.prefix, a.prefix) {
				return fmt.Errorf("%w: %s infers the Pod-name prefix %q, which also begins %s's %q; select by labels "+
					"(--kube-state-metrics with --metric-labels-allowlist) or name prefixes that do not nest",
					ErrRoleOverlap, a, a.prefix, b, b.prefix)
			}
		}
	}
	return nil
}

// stepClock is the collector's clock: the step being collected.
type stepClock struct{ at time.Time }

func (c *stepClock) Now() time.Time                  { return c.at }
func (c *stepClock) Since(t time.Time) time.Duration { return c.at.Sub(t) }

// variantInfo is what the window says of one variant.
type variantInfo struct {
	model, role string
	// gpus counts the variant's Pods by their GPU request, and accels its
	// Pods' accelerators; empty without kube-state-metrics.
	gpus   map[int]int
	accels map[string]int
	// multiPod is whether kube-state-metrics shows its Pods in multi-Pod
	// groups (FlagMultiPodGroups).
	multiPod bool
}

// windowScan is the first pass over the window: every variant whose Pods
// have a KV series, its model, and the steps at which it had one.
type windowScan struct {
	variants map[string]*variantInfo
	// live is, per step, the variants with a KV series then.
	live map[time.Time]map[string]bool
	// engines is every Pod with a KV series at some step of the window: the
	// engine Pods, which a variant's replicas are counted from. A Pod a
	// selector picks that never had one (the router's, say) is not a
	// replica.
	engines map[string]bool
	// inferred and noRoleLabel are the role map's caveats, and ksmGaps the
	// steps at which kube-state-metrics listed no Pod.
	inferred, noRoleLabel bool
	ksmGaps               int
}

// Capture captures the window opts names and writes it to w, gzipped: the
// start line, the window line and one cycle line per model per step
// (learnreplay.CaptureWindow, learnreplay.CaptureCycle), which
// learnreplay.Replay in Generate mode plays through the analyzers.
//
// Every row is built by the collector itself
// (collector.ReplicaMetricsCollector), from its own query templates, as of
// each step: each template's PromQL is asked once as a range query over the
// window (windowAPI) and each step reads the samples evaluated at it. Every
// query is a range query: a capture reads, and writes nothing.
//
// Every query is required: one that fails stops the capture, naming the
// query, at once. A failed query leaves its field absent at every step of
// the window, and a capture cannot tell that from a field the window did
// not have, so a capture that went on would hand Generate inputs a
// controller never read, with nothing to say so. The usual failure is a
// limit (query.max-samples, a timeout) on a histogram over many steps; a
// shorter window or a longer step fits under it.
func Capture(ctx context.Context, opts Options, w io.Writer) (*Summary, error) {
	// Prometheus keeps milliseconds: a start finer than that is asked at
	// the millisecond below it and every step would read off the grid.
	opts.From, opts.To = opts.From.Truncate(time.Millisecond), opts.To.Truncate(time.Millisecond)
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if opts.Engine != inferenceengine.EngineSGLang {
		opts.Engine = inferenceengine.EngineVLLM
	}
	if opts.GPUsPerReplica == 0 {
		opts.GPUsPerReplica = 1
	}
	ctx, abort := context.WithCancelCause(ctx)
	defer abort(nil)
	api := newWindowAPI(opts.Prometheus, opts.From, opts.To, opts.Step, opts.RangeQueryPoints, abort)
	api.maxSamples = opts.MaxSamples
	if api.maxSamples <= 0 {
		api.maxSamples = MaxSamples
	}
	if opts.Force {
		api.maxSamples = 0
	}
	src := promsource.NewPrometheusSource(ctx, api, promsource.PrometheusSourceConfig{DefaultTTL: time.Second})
	defer src.Close()
	registry := source.NewSourceRegistry()
	if err := registry.Register("prometheus", src); err != nil {
		return nil, err
	}
	registerQueries(registry)

	steps := windowSteps(opts.From, opts.To, opts.Step)
	queries := len(src.QueryList().List())
	if opts.KubeStateMetrics {
		queries += reflect.TypeFor[KSM]().NumField()
	}
	if n := len(steps) * queries; n > MaxStepQueries && !opts.Force {
		return nil, fmt.Errorf("%w: %d steps times %d queries is %d, over the %d a capture asks without --force; "+
			"shorten the window or lengthen the step", ErrWindowTooLarge, len(steps), queries, n, MaxStepQueries)
	}
	scan, err := scanWindow(ctx, api, src, opts, steps)
	if err != nil {
		return nil, captureErr(api, err)
	}
	variants, flags := windowVariants(scan, opts)
	if len(variants) == 0 {
		return nil, fmt.Errorf("%w, in namespace %s from %s to %s", ErrNothingAttributed,
			opts.Namespace, opts.From.Format(time.RFC3339), opts.To.Format(time.RFC3339))
	}
	start, err := captureStart(opts)
	if err != nil {
		return nil, err
	}

	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	rec := learnrec.New(gz, 0)
	rec.Record(learnrec.KindStart, opts.From, start)
	rec.Record(learnreplay.KindCaptureWindow, opts.From, learnreplay.CaptureWindow{
		Namespace: opts.Namespace, From: opts.From, To: opts.To, Step: opts.Step,
		Roles: roleSource(opts), Flags: flags, Variants: variants,
	})
	sum := &Summary{Steps: len(steps), Variants: variants, Flags: flags}
	byKey := map[string]learnreplay.CaptureVariant{}
	models := map[string][]string{}
	for _, v := range variants {
		byKey[v.Name] = v
		models[v.ModelID] = append(models[v.ModelID], v.Name)
	}
	clk := &stepClock{}
	for _, at := range steps {
		if ctx.Err() != nil {
			return nil, captureErr(api, context.Cause(ctx))
		}
		api.setStep(at)
		clk.at = at
		cycles, err := captureStep(ctx, api, src, opts, clk, scan, byKey, models)
		if err == nil {
			// The collector reads a failed query as an absent field, and
			// goes on: the failure is the window's.
			err = api.failure()
		}
		if err != nil {
			return nil, fmt.Errorf("step %s: %w", at.Format(time.RFC3339), captureErr(api, err))
		}
		for _, c := range cycles {
			rec.Record(learnreplay.KindCaptureCycle, at, c)
			sum.Cycles++
			sum.Rows += len(c.Input.ReplicaMetrics)
		}
	}
	if err := rec.Err(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	sum.RangeQueries, sum.Samples = api.ranges(), api.held()
	return sum, nil
}

// windowSteps is from, from + step, ... up to to.
func windowSteps(from, to time.Time, step time.Duration) []time.Time {
	var out []time.Time
	for at := from; !at.After(to); at = at.Add(step) {
		out = append(out, at)
	}
	return out
}

// registerQueries registers every query a capture asks besides
// kube-state-metrics': the ones the engine registers
// (steadystate.NewEngine) and the one cmd/main registers whatever the
// analyzers. The metrics they read are listed for operators in
// docs/reference/capacity-planner-metrics.md, which a test keeps in step.
func registerQueries(registry *source.SourceRegistry) {
	registration.RegisterSaturationQueries(registry)
	registration.RegisterQueueingModelQueries(registry)
	registration.RegisterSLOQueries(registry)
	registration.RegisterFlowControlPoolQueries(registry)
	registration.RegisterAlwaysOnQueries(registry)
}

// captureErr is the error a capture fails with: the window's failed query
// when one failed, whatever err its abort caused downstream (a cancelled
// context, say); err otherwise.
func captureErr(api *windowAPI, err error) error {
	failed := api.failure()
	switch {
	case failed == nil:
		return err
	case errors.Is(failed, ErrWindowTooLarge):
		return failed
	}
	return fmt.Errorf("%w: %w (every query a capture asks is required: shorten the window, lengthen the step, "+
		"or raise the server's limit)", ErrQueryFailed, failed)
}

// roleSource names where the role map came from.
func roleSource(opts Options) string {
	var parts []string
	if opts.KubeStateMetrics {
		parts = append(parts, "kube-state-metrics")
	}
	if len(opts.RoleOf) > 0 {
		parts = append(parts, "role-of")
	}
	return strings.Join(parts, "+")
}

// kvQuery is the collector's KV-cache query for the namespace: the series
// that define the fleet (collector extractPodMetrics).
func kvQuery(src source.MetricsSource, opts Options) (string, error) {
	name := registration.EngineQuery(opts.Engine, registration.QueryKvCacheUsage)
	return src.QueryList().Build(name, map[string]string{source.ParamNamespace: source.EscapePromQLValue(opts.Namespace)})
}

// scanWindow finds every variant over the window: the Pods with a KV
// series, attributed by the role map, and their models.
func scanWindow(ctx context.Context, api *windowAPI, src source.MetricsSource, opts Options, steps []time.Time) (*windowScan, error) {
	q, err := kvQuery(src, opts)
	if err != nil {
		return nil, err
	}
	scan := &windowScan{variants: map[string]*variantInfo{}, live: map[time.Time]map[string]bool{},
		engines: map[string]bool{}}
	sawRoleLabel := false
	for _, at := range steps {
		api.setStep(at)
		f, err := fleetAt(ctx, api, opts.Namespace, opts.KubeStateMetrics, opts.RoleOf)
		if err != nil {
			return nil, err
		}
		val, _, err := api.Query(ctx, q, at)
		if err != nil {
			return nil, err
		}
		vec, _ := val.(model.Vector)
		live := map[string]bool{}
		for _, s := range vec {
			name := string(s.Metric["pod"])
			if name == "" {
				name = string(s.Metric["pod_name"])
			}
			p := f.lookup(name)
			if p != nil && len(p.overlap) > 1 {
				return nil, overlapErr(p)
			}
			modelID := string(s.Metric["model_name"])
			if p == nil || p.variant == "" || modelID == "" {
				continue
			}
			live[p.variant] = true
			scan.engines[name] = true
			v := scan.variants[p.variant]
			if v == nil {
				v = &variantInfo{model: modelID, role: p.role, gpus: map[int]int{}, accels: map[string]int{}}
				scan.variants[p.variant] = v
			}
			scan.inferred = scan.inferred || p.inferred
			if p.ksm {
				v.multiPod = v.multiPod || p.inGroup()
				if p.gpus > 0 {
					v.gpus[p.gpus]++
				}
				if a := f.nodeAccel[p.node]; a != "" {
					v.accels[a]++
				}
			}
			if _, ok := p.labels[ksmLabelName("llm-d.ai/role")]; ok {
				sawRoleLabel = true
			}
		}
		scan.live[at] = live
		if opts.KubeStateMetrics && len(f.pods) == 0 {
			scan.ksmGaps++
		}
	}
	scan.noRoleLabel = opts.KubeStateMetrics && len(opts.RoleOf) == 0 && !sawRoleLabel
	return scan, nil
}

// mostCommon is the key counts holds most often, the least on a tie; ok is
// false for none.
func mostCommon[K int | string](counts map[K]int) (best K, ok bool) {
	for _, k := range slices.Sorted(maps.Keys(counts)) {
		if !ok || counts[k] > counts[best] {
			best, ok = k, true
		}
	}
	return best, ok
}

// windowVariants is the window line's variants, in name order, and the
// capture's caveats.
func windowVariants(scan *windowScan, opts Options) ([]learnreplay.CaptureVariant, []string) {
	var flags []string
	gpusFromFlags, accelFromFlags := false, false
	multiPod := 0
	out := make([]learnreplay.CaptureVariant, 0, len(scan.variants))
	for _, name := range slices.Sorted(maps.Keys(scan.variants)) {
		info := scan.variants[name]
		if info.multiPod {
			multiPod++
		}
		gpus, ok := mostCommon(info.gpus)
		if !ok {
			gpus, gpusFromFlags = opts.GPUsPerReplica, opts.KubeStateMetrics
		}
		accel, ok := mostCommon(info.accels)
		if !ok {
			accel, accelFromFlags = opts.Accelerator, opts.KubeStateMetrics
		}
		st := scaleTargetOf(opts, name, info.model, gpus, accel)
		out = append(out, learnreplay.CaptureVariant{
			ModelID: info.model, Name: name, Role: info.role, Accelerator: accel, GpuCount: gpus,
			Params: capacity.ParseEngineArgs(inferenceengine.Detect(st), st),
		})
	}
	if scan.inferred {
		flags = append(flags, FlagRolesInferred)
	}
	if !opts.KubeStateMetrics {
		flags = append(flags, FlagNoReadiness)
	}
	if scan.noRoleLabel {
		flags = append(flags, FlagNoRoleLabel)
	}
	if scan.ksmGaps > 0 {
		flags = append(flags, fmt.Sprintf(FlagKSMGaps, scan.ksmGaps, len(scan.live)))
	}
	flags = append(flags, FlagParamsFromFlags)
	if gpusFromFlags {
		flags = append(flags, FlagGPUsFromFlags)
	}
	if accelFromFlags {
		flags = append(flags, FlagAcceleratorFromFlags)
	}
	if multiPod > 0 {
		flags = append(flags, fmt.Sprintf(FlagMultiPodGroups, multiPod))
	}
	flags = append(flags, FlagNoRollouts)
	return out, flags
}

// scaleTargetOf is a Deployment that stands for a variant's workload: the
// engine and its arguments, the GPUs and the accelerator. The engine
// parameters are parsed from it as the engine parses a real one, and the
// collector detects the engine from it.
func scaleTargetOf(opts Options, variant, modelID string, gpus int, accel string) scaletarget.ScaleTargetAccessor {
	// The image names are never pulled: this Deployment is never applied.
	// They are only what inferenceengine.Detect reads the engine from.
	c := corev1.Container{Name: "vllm", Image: "vllm/vllm-openai", Command: []string{"vllm", "serve", modelID}}
	batchFlag := "--max-num-seqs"
	if opts.Engine == inferenceengine.EngineSGLang {
		c = corev1.Container{Name: "sglang", Image: "lmsysorg/sglang",
			Command: []string{"python3", "-m", "sglang.launch_server", "--model-path", modelID}}
		batchFlag = "--max-running-requests"
	}
	c.Args = slices.Clone(opts.EngineArgs)
	if opts.BatchCap > 0 {
		c.Args = append(c.Args, batchFlag+"="+strconv.Itoa(opts.BatchCap))
	}
	if gpus > 0 {
		c.Resources.Requests = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse(strconv.Itoa(gpus))}
	}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: variant, Namespace: opts.Namespace},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{c},
		}}},
	}
	if accel != "" {
		d.Spec.Template.Spec.NodeSelector = map[string]string{acceleratorNodeLabel: accel}
	}
	return scaletarget.NewDeploymentAccessor(d)
}

// routerSource stands for the router the controller's slo analyzer judges
// with, for the start line's Judging: the engine's always has one.
type routerSource struct{}

func (routerSource) CollectRouterLatency(context.Context, string, string) *domain.RouterLatency {
	return nil
}

// captureStart is the start line: the analyzers as a controller with the
// policy and an optimize interval of the step would build them
// (steadystate.NewEngine and recordLearners).
func captureStart(opts Options) (learnrec.Start, error) {
	store := capacity.NewStore()
	sat, replaced := saturation.NewSaturationAnalyzer(store).WithHorizons(capacity.DefaultHorizons())
	if len(replaced) > 0 {
		return learnrec.Start{}, fmt.Errorf("default horizons replaced: %v", replaced)
	}
	sat = sat.WithOptimizationInterval(opts.Step)
	h := sat.Horizons()
	start := learnrec.Start{Commit: learnrec.Commit(), Saturation: learnrec.SaturationStart{
		EpisodeGap: h.EpisodeGap, VariantTimeout: h.VariantTimeout, BucketRetention: h.BucketRetention,
		BorrowTickGap: sat.BorrowTickGap(),
	}}
	s := slo.New(routerSource{}, store).WithOptimizationInterval(opts.Step).
		WithStateTimeout(h.VariantTimeout).WithSlopeHoldFor(h.BucketRetention)
	start.SLO = &learnrec.SLOStart{StateTimeout: s.StateTimeout(), SlopeHoldFor: s.SlopeHoldFor(), Judging: s.Judging()}
	// cmd/main registers the throughput analyzer when the policy lists it.
	if policyFor(opts, "", opts.Namespace).AnalyzerEnabled(throughput.AnalyzerName) {
		tp := throughput.NewThroughputAnalyzer().WithSlopeHoldFor(h.BucketRetention).
			WithOptimizationInterval(opts.Step).WithSettleView(settle.NewTracker()).WithCapacityStore(store)
		start.Throughput = true
		c := tp.StartConfig()
		start.ThroughputConfig = &c
	}
	return start, nil
}

// policyFor is the merged policy a model reads: the policy's default
// entry, or none, resolved as the engine resolves it.
func policyFor(opts Options, modelID, namespace string) config.ScalingPolicy {
	entries := map[string]config.ScalingPolicy{}
	if opts.Policy != nil {
		entries[config.GlobalDefaultsKey] = *opts.Policy
	}
	return config.ResolveScalingPolicy(entries, modelID, namespace)
}

// captureStep collects every model's cycle at the step api is set to.
func captureStep(ctx context.Context, api *windowAPI, src source.MetricsSource, opts Options, clk *stepClock,
	scan *windowScan, variants map[string]learnreplay.CaptureVariant, models map[string][]string) ([]learnreplay.CaptureCycle, error) {
	f, err := fleetAt(ctx, api, opts.Namespace, opts.KubeStateMetrics, opts.RoleOf)
	if err != nil {
		return nil, err
	}
	var reader client.Reader
	if opts.KubeStateMetrics {
		reader = f
	}
	c := collector.NewReplicaMetricsCollector(src, nil, reader, nil, f)
	c.SetClock(clk)
	// Every query a recording reads, the TTFT model's inputs included, as a
	// controller recording its learners collects them.
	c.BeginRecordingCycle(true)
	defer c.EndCycle()

	live := scan.live[clk.at]
	var out []learnreplay.CaptureCycle
	for _, modelID := range slices.Sorted(maps.Keys(models)) {
		var active, parked []string
		for _, name := range models[modelID] {
			if n, ok := f.replicas[name]; ok {
				if n > 0 {
					active = append(active, name)
				} else {
					parked = append(parked, name)
				}
				continue
			}
			if live[name] {
				active = append(active, name)
			} else if !opts.KubeStateMetrics {
				// Seen in the window and not now: parked, as far as a
				// capture without kube-state-metrics can tell.
				parked = append(parked, name)
			}
		}
		if len(active) == 0 && len(parked) == 0 {
			continue
		}
		cycle := learnreplay.CaptureCycle{Parked: parked}
		in := domain.AnalyzerInput{ModelID: modelID, Namespace: opts.Namespace}
		if len(active) > 0 {
			if err := collectModel(ctx, c, f, opts, scan.engines, modelID, active, variants, &in); err != nil {
				return nil, fmt.Errorf("model %s: %w", modelID, err)
			}
		}
		cycle.Input = learnrec.InputOf(in)
		out = append(out, cycle)
	}
	return out, nil
}

// collectModel fills in with one model's cycle input, as the engine
// collects it (prepareModelData, buildAnalyzerInput): the rows, the
// variants' states, the policy, the scheduler queue, the arrival rate, the
// router latency and the flow-control pools.
func collectModel(ctx context.Context, c *collector.ReplicaMetricsCollector, f *fleet, opts Options, engines map[string]bool,
	modelID string, active []string, variants map[string]learnreplay.CaptureVariant, in *domain.AnalyzerInput) error {
	targets := make(map[string]scaletarget.ScaleTargetAccessor, len(active))
	for _, name := range active {
		v := variants[name]
		targets[utils.GetNamespacedKey(opts.Namespace, name)] = scaleTargetOf(opts, name, modelID, v.GpuCount, v.Accelerator)
	}
	rows, err := c.CollectReplicaMetrics(ctx, modelID, opts.Namespace, targets, nil, nil)
	if err != nil {
		return err
	}
	// The collector's rows come in map order; a capture's are in variant
	// and Pod order, so the same window captures to the same bytes.
	slices.SortFunc(rows, func(a, b domain.ReplicaMetrics) int {
		if c := strings.Compare(a.VariantName, b.VariantName); c != 0 {
			return c
		}
		return strings.Compare(a.PodName, b.PodName)
	})
	policy := policyFor(opts, modelID, opts.Namespace)
	in.ReplicaMetrics = rows
	in.Config = &policy
	in.VariantStates = variantStates(f, opts, engines, active, variants, rows)
	if len(rows) > 0 {
		in.SchedulerQueue = c.CollectSchedulerQueueMetrics(ctx, modelID, opts.Namespace)
		in.ArrivalRate = c.CollectModelArrivalRate(ctx, modelID, opts.Namespace)
		in.RouterLatency = c.CollectRouterLatency(ctx, modelID, opts.Namespace)
		in.FlowControlPools = c.CollectFlowControlPools(ctx, modelID, opts.Namespace)
	}
	return nil
}

// variantStates is each active variant's state, in name order: its replicas
// from kube-state-metrics (current, desired, not Ready) or, without it, its
// rows; its GPUs per replica, role and accelerator from the window line.
// Only engine Pods count as replicas: those with a KV series at some step
// of the window (windowScan.engines). A Pod that never had one, as the
// router's Pod a name prefix also picks, is not a replica.
func variantStates(f *fleet, opts Options, engines map[string]bool, active []string,
	variants map[string]learnreplay.CaptureVariant, rows []domain.ReplicaMetrics) []domain.VariantReplicaState {
	out := make([]domain.VariantReplicaState, 0, len(active))
	for _, name := range slices.Sorted(slices.Values(active)) {
		v := variants[name]
		vs := domain.VariantReplicaState{VariantName: name, GPUsPerReplica: v.GpuCount, Role: v.Role,
			AcceleratorName: v.Accelerator}
		if opts.KubeStateMetrics && len(f.pods) > 0 {
			for _, p := range f.pods {
				if p.variant != name || p.deleting || !engines[p.name] {
					continue
				}
				vs.CurrentReplicas++
				if !p.ready {
					vs.PendingReplicas++
				}
			}
		} else {
			for _, r := range rows {
				if r.VariantName == name {
					vs.CurrentReplicas++
				}
			}
		}
		vs.DesiredReplicas = vs.CurrentReplicas
		if n, ok := f.replicas[name]; ok {
			vs.DesiredReplicas = n
		}
		out = append(out, vs)
	}
	return out
}
