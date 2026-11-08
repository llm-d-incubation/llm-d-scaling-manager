package scalefromzero

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// lentOutProvider is "chat" on a reclaimWithinCohort Never queue of nominal 8
// that lent 6 to a peer: guaranteed 2, and the cohort's free pool 0, so within
// nominal is 2, not 8. lent false is the control: the same 2 GPUs of room
// gone, nothing lent.
type lentOutProvider struct{ notLent bool }

// lentOutName is lentOutProvider's name.
const lentOutName = "lent-out-quota"

func (lentOutProvider) Name() string { return lentOutName }
func (p lentOutProvider) ComputeConstraints(context.Context, map[string]int, map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	lent := 6
	if p.notLent {
		lent = 0
	}
	return &allocation.ResourceConstraints{
		ProviderName:   lentOutName,
		Pools:          map[string]allocation.ResourcePool{"H100": {Limit: config.QuotaUnlimited}},
		NamespacePools: map[string]map[string]allocation.ResourcePool{"chat": {"H100": {Limit: 2, Used: 2}}},
		NamespaceBorrow: map[string]map[string]allocation.NamespaceBorrow{
			"chat": {"H100": {Cap: config.QuotaUnlimited, WithinNominal: 2, NominalLentOut: lent}},
		},
		NamespaceCohorts: map[string][]string{"chat": {"c"}},
	}, nil
}

// B10, review T2: a wake is checked against guaranteed plus the free pool --
// within nominal -- never the whole nominal a peer holds and need not give
// back. The wake's outcome path names the refusal nominal-lent-out, and
// llmsm_model_scaling_blocked carries wake-nominal-lent-out; a namespace that
// lent nothing is refused as no-capacity, and the reason becomes
// wake-no-capacity (SR-M1: a refused wake names its cause).
func TestWakeIntoLentNominalReportsNominalLentOut(t *testing.T) {
	ctx := context.Background()
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	decision.PublishGPUUsage(map[string]int{"H100": 2}, nil)
	allocation.DefaultBorrowInFlight.Reset()
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	group := modelGroup{namespace: "chat", modelID: "m"}
	set := []Candidate{cand("d", "decode", "H100", 1, 1)}

	for _, tc := range []struct {
		name    string
		p       lentOutProvider
		outcome SelectionOutcome
		reason  string
	}{
		{"nominal lent out: nominal-lent-out outcome, wake-nominal-lent-out metric", lentOutProvider{}, OutcomeNominalLentOut, "wake-nominal-lent-out"},
		{"control: nothing lent out: no-capacity, and the reason becomes wake-no-capacity", lentOutProvider{notLent: true}, OutcomeNoCapacity, "wake-no-capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{gpuLimiter: tc.p}
			if tc.outcome == OutcomeNoCapacity {
				// The same model refused for the lent nominal by an earlier tick.
				e.reportWakeBlocked(group, "wake-nominal-lent-out")
			}
			_, selected, outcome := e.wakeVerdict(ctx, group, set, coverage{})
			if len(selected) != 0 || outcome != tc.outcome {
				t.Fatalf("woke %v (%s), want nothing (%s)", names(selected), outcome, tc.outcome)
			}
			if got := wakeReasons(t, registry)["m"]; got != tc.reason {
				t.Fatalf("llmsm_model_scaling_blocked reason %q, want %q", got, tc.reason)
			}
		})
	}
}

// SR-M1 and the one-owner rule: the steady-state engine sets nominal-lent-out
// for a capped serving model. A wake this loop refuses, then stops refusing,
// must clear only its own reason and leave the steady-state engine's series
// standing; and a refused wake names a reason, where it used to name none.
func TestARefusedWakeNeverDeletesTheSteadyStateReason(t *testing.T) {
	ctx := context.Background()
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	decision.PublishGPUUsage(map[string]int{"H100": 2}, nil)
	allocation.DefaultBorrowInFlight.Reset()
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	group := modelGroup{namespace: "chat", modelID: "m"}
	set := []Candidate{cand("d", "decode", "H100", 1, 1)}
	held := func() map[string]bool {
		out := map[string]bool{}
		mfs, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, mf := range mfs {
			if mf.GetName() != "llmsm_model_scaling_blocked" {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "reason" {
						out[l.GetValue()] = true
					}
				}
			}
		}
		return out
	}

	// The steady-state engine: the model is capped while its nominal is lent.
	metrics.SetModelScalingBlockedReasons("chat", "m", []string{"lender-reclaimed", "borrow-unplaced", "nominal-lent-out"},
		[]string{"nominal-lent-out"})

	e := &Engine{gpuLimiter: lentOutProvider{}}
	if _, selected, _ := e.wakeVerdict(ctx, group, set, coverage{}); len(selected) != 0 {
		t.Fatalf("woke %v, want nothing", names(selected))
	}
	e.gpuLimiter = lentOutProvider{notLent: true}
	if _, selected, _ := e.wakeVerdict(ctx, group, set, coverage{}); len(selected) != 0 {
		t.Fatalf("woke %v, want nothing", names(selected))
	}
	got := held()
	if !got["nominal-lent-out"] {
		t.Fatalf("the steady-state engine's nominal-lent-out was deleted by the wake loop: %v", got)
	}
	if !got["wake-no-capacity"] {
		t.Fatalf("a wake refused for want of capacity names no reason: %v", got)
	}
}
