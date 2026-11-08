package learnrec

import (
	"fmt"
	"runtime/debug"
	"sort"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// Input is an Analyze call's input as a recording holds it: the rows raw, as
// Analyze received them, with every presence bit; the variants' states; the
// merged policy, analyzerTuning and loadImbalance included, every cycle; the
// scheduler queue, the arrival rate, the router latency and the flow-control
// pools.
//
// It mirrors domain.AnalyzerInput field for field, less VariantMetadata,
// which no analyzer reads: the engine's composite event records the part of
// it the capacity-build step reads (enginerec). Config is written as its
// dynamic value, and ConfigType names that value's type, so a replay knows
// what to read it back into: the engine passes a *config.ScalingPolicy. This
// package does not import config, whose tests import the analyzers that
// import this package.
type Input struct {
	ModelID        string
	Namespace      string
	ReplicaMetrics []domain.ReplicaMetrics
	VariantStates  []domain.VariantReplicaState
	Config         any
	// ConfigType is Config's dynamic type, empty for a nil Config.
	ConfigType     string
	SchedulerQueue *domain.SchedulerQueueMetrics
	ArrivalRate    float64
	// RouterLatency and FlowControlPools are left out while zero, so a
	// recording made before they existed is written again byte for byte
	// (OmitZeroTag).
	RouterLatency    *domain.RouterLatency    `learnrec:"omitzero"`
	FlowControlPools []domain.FlowControlPool `learnrec:"omitzero"`
}

// InputOf is the recorded form of in.
func InputOf(in domain.AnalyzerInput) Input {
	out := Input{
		ModelID:          in.ModelID,
		Namespace:        in.Namespace,
		ReplicaMetrics:   in.ReplicaMetrics,
		VariantStates:    in.VariantStates,
		SchedulerQueue:   in.SchedulerQueue,
		ArrivalRate:      in.ArrivalRate,
		RouterLatency:    in.RouterLatency,
		FlowControlPools: in.FlowControlPools,
	}
	if in.Config != nil {
		out.Config, out.ConfigType = in.Config, fmt.Sprintf("%T", in.Config)
	}
	return out
}

// VariantNames is every variant an input names, in its states or its rows,
// sorted.
func VariantNames(in domain.AnalyzerInput) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, vs := range in.VariantStates {
		add(vs.VariantName)
	}
	for _, rm := range in.ReplicaMetrics {
		add(rm.VariantName)
	}
	sort.Strings(out)
	return out
}

// ResultOf is what a recording holds of an Analyze result: the result
// without AnalyzedAt, which is the clock and not an output. Nil stays nil.
func ResultOf(r *domain.AnalyzerResult) *domain.AnalyzerResult {
	if r == nil {
		return nil
	}
	c := *r
	c.AnalyzedAt = time.Time{}
	return &c
}

// ErrorOf is an error as a recording holds it: its text, empty for nil.
func ErrorOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// KindStart is the first line of every recording, with Start.
const KindStart = "start"

// Start is how the run was configured when the recording began: what the
// analyzers' configuration builders set, and which analyzers ran. A replay
// builds its analyzers from it and refuses a recording that has none.
type Start struct {
	// Saturation is the saturation analyzer's configuration.
	Saturation SaturationStart
	// SLO is the slo analyzer's configuration; nil when the run had none.
	SLO *SLOStart
	// Throughput is whether the run registered the throughput analyzer.
	Throughput bool
	// ThroughputConfig is what the throughput analyzer's builders set; nil
	// when the run had no throughput analyzer, or was recorded before the
	// analyzer had builders of its own (a replay then builds it with none).
	ThroughputConfig *ThroughputStart `json:",omitempty"`
	// StoreRecords is how many records the capacity store held. A recording
	// starts only at a controller start, when the store is new and empty; a
	// replay refuses any other count, since it has no way to file them.
	StoreRecords int
	// Commit is the commit the controller was built from (BuildCommit),
	// "unknown" when the build did not say. A golden is added only at the
	// commit that recorded it, since its replay must reproduce it byte for
	// byte.
	Commit string
	// Caveats is what the run could not see, one line each: a recording
	// learnreplay's Generate mode made from a capture carries the capture's
	// flags (rollouts not seen, roles inferred from Pod names, engine
	// parameters from flags, ...), so a golden made from it says so. Empty
	// for a controller's recording.
	Caveats []string `learnrec:"omitzero"`
}

// BuildCommit is the commit this binary was built from, set at link time:
//
//	go build -ldflags "-X github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec.BuildCommit=$(git rev-parse HEAD)"
//
// make build, the Dockerfile and the image workflows set it.
var BuildCommit = "unknown"

// SaturationStart is what WithHorizons and WithOptimizationInterval set: the
// horizons in force, and the borrow clock's longest counted gap
// (BorrowTickGap), which is what the optimization interval sets.
type SaturationStart struct {
	EpisodeGap, VariantTimeout, BucketRetention time.Duration
	BorrowTickGap                               time.Duration
}

// SLOStart is what WithStateTimeout and WithSlopeHoldFor set, and whether the
// analyzer had a router to judge with (Judging): a replay serves the recorded
// latency through a router exactly when the run had one.
type SLOStart struct {
	StateTimeout, SlopeHoldFor time.Duration
	Judging                    bool
}

// ThroughputStart is what the throughput analyzer's builders set: how long
// its ITL line may hold a fitted slope (WithSlopeHoldFor), the controller's
// optimize interval (WithOptimizationInterval), and whether it was handed the
// engine's settle tracker (WithSettleView) and the capacity store
// (WithCapacityStore). A replay hands it its own of each exactly when the run
// did.
type ThroughputStart struct {
	SlopeHoldFor, OptimizationInterval time.Duration
	SettleView, CapacityStore          bool
}

// Commit is the commit this binary was built from: BuildCommit when the
// build set it, else the revision Go stamps into a binary built from a
// checkout (vcs.revision), else "unknown".
func Commit() string {
	if BuildCommit != "" && BuildCommit != "unknown" {
		return BuildCommit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "unknown"
}
