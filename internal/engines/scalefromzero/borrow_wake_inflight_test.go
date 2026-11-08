package scalefromzero

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// neverQueueProvider is "chat" on a reclaimWithinCohort Never queue of nominal
// 8 that lent all of it: guaranteed 0, and the cohort's free pool of 8 is all
// it reaches within nominal. counted false reads the namespace without
// counting its borrowing (outside the pilot): its figures come as
// NamespaceNominal.
type neverQueueProvider struct{ counted bool }

func (neverQueueProvider) Name() string { return "cohort-quota" }
func (p neverQueueProvider) ComputeConstraints(context.Context, map[string]int, map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	c := &allocation.ResourceConstraints{
		ProviderName:     "cohort-quota",
		NamespaceScoped:  true,
		NamespaceCohorts: map[string][]string{"chat": {"c"}, "peer": {"c"}},
		NamespaceBorrow: map[string]map[string]allocation.NamespaceBorrow{
			"peer": {"H100": {Cap: config.QuotaUnlimited}}},
		NamespacePools: map[string]map[string]allocation.ResourcePool{"peer": {"H100": {Limit: 0}}},
		SharedPools: []allocation.SharedPoolChunk{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu", Free: 8,
			FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: "H100"}}},
	}
	if p.counted {
		c.NamespacePools["chat"] = map[string]allocation.ResourcePool{"H100": {Limit: 0}}
		c.NamespaceBorrow["chat"] = map[string]allocation.NamespaceBorrow{"H100": {Cap: config.QuotaUnlimited, WithinNominal: 8}}
	} else {
		c.NamespacePools["chat"] = map[string]allocation.ResourcePool{"H100": {Limit: 8}}
		c.NamespaceNominal = map[string]map[string]allocation.NominalFigures{"chat": {"H100": {WithinNominal: 8}}}
	}
	return c, nil
}

// Review A1, W7: the wake did not see what the steady-state engine borrowed
// and Kueue had not reserved yet. Its +1 decode took the cohort's last free 8
// GPUs; a cold wake on a Never queue, within nominal only through that pool,
// woke into the same 8, and one pod was gated. The wake now deducts the draws
// in flight, the gated pods and the closed cohorts from the process-wide
// record -- for a namespace read and not counted too.
func TestWakeSeesBorrowingInFlight(t *testing.T) {
	ctx := context.Background()
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	decision.PublishGPUUsage(map[string]int{"H100": 0}, nil)
	record := allocation.DefaultBorrowInFlight
	t.Cleanup(record.Reset)
	now := time.Now()

	for _, counted := range []bool{true, false} {
		for _, tc := range []struct {
			name string
			set  func()
			fits bool
		}{
			{"control: nothing in flight, the wake fits the free pool", func() {}, true},
			{"a steady-state draw in flight on the pool", func() {
				record.RecordAt([]allocation.BorrowDraw{{Namespace: "peer", Variant: "d", Cohort: "c", Flavor: "h100",
					Resource: "nvidia.com/gpu", GPUs: 8, Replicas: 1, Target: 2}}, now)
			}, false},
			{"a peer's pod at Kueue's gate", func() {
				record.SetCycle(map[string]map[string]int{"peer": {"": 8}}, nil, nil, now)
			}, false},
			{"the cohort closed: a lender waits", func() {
				record.SetCycle(nil, nil, map[string]bool{"c": true}, now)
			}, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				record.Reset()
				tc.set()
				e := &Engine{gpuLimiter: neverQueueProvider{counted: counted}}
				p := e.placementFor(ctx, "chat")
				if got := allocation.FitsGPUBudget(p.constraints, "chat", map[string]int{"H100": 8}); got != tc.fits {
					t.Fatalf("counted %t: an 8-GPU wake fits = %t, want %t", counted, got, tc.fits)
				}
			})
		}
	}
}

// Review W7: B10 holds for a namespace read and not counted too: its wake is
// checked against its queue's within-nominal figure, not its nominal, and a
// refusal says nominal-lent-out.
func TestUncountedWakeIntoLentNominalReportsNominalLentOut(t *testing.T) {
	ctx := context.Background()
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	decision.PublishGPUUsage(map[string]int{"H100": 2}, nil)
	allocation.DefaultBorrowInFlight.Reset()

	e := &Engine{gpuLimiter: uncountedLentOutProvider{}}
	p := e.placementFor(ctx, "chat")
	if !p.nominalLentOut {
		t.Error("the placement does not say the nominal is lent out")
	}
	if allocation.FitsGPUBudget(p.constraints, "chat", map[string]int{"H100": 1}) {
		t.Error("a wake fits 6 GPUs of nominal a peer holds and need not give back")
	}
}

// uncountedLentOutProvider is lentOutProvider for a namespace read and not
// counted: nominal 8, 6 of it lent by a Never queue, within nominal 2.
type uncountedLentOutProvider struct{}

func (uncountedLentOutProvider) Name() string { return lentOutName }
func (uncountedLentOutProvider) ComputeConstraints(context.Context, map[string]int, map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	return &allocation.ResourceConstraints{
		ProviderName:   lentOutName,
		Pools:          map[string]allocation.ResourcePool{"H100": {Limit: config.QuotaUnlimited}},
		NamespacePools: map[string]map[string]allocation.ResourcePool{"chat": {"H100": {Limit: 8, Used: 2}}},
		NamespaceNominal: map[string]map[string]allocation.NominalFigures{
			"chat": {"H100": {WithinNominal: 2, Guaranteed: 2, NominalLentOut: 6}}},
	}, nil
}
