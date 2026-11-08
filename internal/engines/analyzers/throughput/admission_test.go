package throughput

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
)

var admissionAt = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// itlPolicyOf reads the three ITL keys the saturation and slo analyzers
// read, each through its own interface, with the defaults for a config that
// implements none.
func TestITLPolicyReadsTheBandTheGateAndTheCapRate(t *testing.T) {
	rate := 0.5
	cfg := &config.ScalingPolicy{AnalyzerTuning: config.AnalyzerTuning{
		ITLBand:    config.ITLBandTuning{MinObservableK: 0.05, MaxObservableK: 0.90, CapMaxPreemptionRate: &rate},
		SteadyGate: config.SteadyGateTuning{IdleRows: 5},
	}}
	p := itlPolicyOf(cfg)
	if p.lo != 0.05 || p.hi != 0.90 {
		t.Errorf("band [%g, %g], want [0.05, 0.90]", p.lo, p.hi)
	}
	if p.capPreemption != 0.5 {
		t.Errorf("cap preemption rate %g, want 0.5", p.capPreemption)
	}
	if p.steady != cfg.ITLSteadiness() {
		t.Errorf("steadiness %+v, want the policy's %+v", p.steady, cfg.ITLSteadiness())
	}
	if p.steady.IdleRows != 5 {
		t.Errorf("idle rows %d, want 5: analyzerTuning.steadyGate must reach the admission", p.steady.IdleRows)
	}

	// A nil policy is the policy's defaults.
	var none *config.ScalingPolicy
	if p := itlPolicyOf(none); p.lo != itl.DefaultMinObservableK || p.hi != itl.DefaultMaxObservableK ||
		p.steady != none.ITLSteadiness() || p.capPreemption != none.ITLCapPreemptionRate() {
		t.Errorf("a nil policy read %+v, want the defaults", p)
	}
	// No config at all: the default band, and zero for the rest (itl's
	// defaults).
	if p := itlPolicyOf(nil); p != (itlPolicy{lo: itl.DefaultMinObservableK, hi: itl.DefaultMaxObservableK}) {
		t.Errorf("no config read %+v, want the default band and nothing else", p)
	}
}

// The admission takes the policy's keys, the slope hold, and the engine's
// settle tracker with its absence tolerance at the optimize interval. It
// reads no capacity store: a variant's cap and arguments are read per
// variant (engineOf) and set per variant (lineAdmission; itl_cap_test.go).
func TestAdmissionReadsThePolicyTheSettleViewAndTheHold(t *testing.T) {
	store := capacity.NewStore()
	store.FileScaleTarget("ns", "m", "v1", "H100", 1,
		capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, MaxNumSeqsSet: true, Image: "img:1"},
		false, admissionAt)
	view := settle.NewTracker()
	a := NewThroughputAnalyzer().WithCapacityStore(store).WithSettleView(view).
		WithOptimizationInterval(30 * time.Second).WithSlopeHoldFor(6 * time.Hour)
	p := itlPolicy{lo: 0.2, hi: 0.7, capPreemption: 0.1, steady: itl.Steadiness{IdleRows: 4}}

	adm := a.admission(p, "ns", "m")
	if adm.MinK != 0.2 || adm.MaxK != 0.7 || adm.CapMaxPreemptionRate != 0.1 || adm.Steady.IdleRows != 4 {
		t.Errorf("the policy did not reach the admission: %+v", adm)
	}
	if adm.SlopeHoldFor != 6*time.Hour {
		t.Errorf("slope hold %v, want 6h", adm.SlopeHoldFor)
	}
	if adm.Settle == nil || adm.Settle.View != settle.View(view) || adm.Settle.Model != "ns/m" {
		t.Errorf("settle %+v, want the engine's view for ns/m", adm.Settle)
	}
	if want := settle.Tolerance(30 * time.Second); adm.AbsenceTolerance != want {
		t.Errorf("absence tolerance %v, want %v", adm.AbsenceTolerance, want)
	}
	if adm.MaxNumSeqs != 0 || adm.MaxNumSeqsSource != batchCapNone || adm.Args != "" {
		t.Errorf("the model's admission read the store: cap %d from %q, args %q",
			adm.MaxNumSeqs, adm.MaxNumSeqsSource, adm.Args)
	}

	// An analyzer given nothing has no view and no hold: the admission is
	// the policy alone.
	bare := NewThroughputAnalyzer().admission(p, "ns", "m")
	if bare.MaxNumSeqs != 0 || bare.MaxNumSeqsSource != batchCapNone || bare.Args != "" ||
		bare.Settle != nil || bare.SlopeHoldFor != 0 || bare.AbsenceTolerance != 0 {
		t.Errorf("an analyzer given nothing built %+v", bare)
	}
}

// What the builders set is what the recording's first line states.
func TestStartConfigStatesTheBuilders(t *testing.T) {
	if got := NewThroughputAnalyzer().StartConfig(); got != (learnrec.ThroughputStart{}) {
		t.Errorf("a bare analyzer states %+v", got)
	}
	got := NewThroughputAnalyzer().WithSettleView(settle.NewTracker()).WithCapacityStore(capacity.NewStore()).
		WithOptimizationInterval(15 * time.Second).WithSlopeHoldFor(time.Hour).StartConfig()
	want := learnrec.ThroughputStart{SlopeHoldFor: time.Hour, OptimizationInterval: 15 * time.Second,
		SettleView: true, CapacityStore: true}
	if got != want {
		t.Errorf("StartConfig %+v, want %+v", got, want)
	}
}
