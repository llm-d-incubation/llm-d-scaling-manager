package scalefromzero

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// idleLenderProvider serves namespace "b"'s queue figures as the reader
// computes them, read and not counted (NamespaceNominal), on a cluster of 32
// H100 GPUs all in use.
type idleLenderProvider struct{ f kueue.QueueFigures }

// idleLenderName is idleLenderProvider's name.
const idleLenderName = "idle-lender-quota"

func (idleLenderProvider) Name() string { return idleLenderName }
func (p idleLenderProvider) ComputeConstraints(context.Context, map[string]int, map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	return &allocation.ResourceConstraints{
		ProviderName:   idleLenderName,
		Pools:          map[string]allocation.ResourcePool{"H100": {Limit: 32, Used: 32}},
		NamespacePools: map[string]map[string]allocation.ResourcePool{"b": {"H100": {Limit: 16}}},
		NamespaceNominal: map[string]map[string]allocation.NominalFigures{"b": {"H100": {LentOut: p.f.LentOut,
			PreemptAfter: p.f.PreemptAfter, WithinNominal: p.f.WithinNominal, Guaranteed: p.f.Guaranteed}}},
	}, nil
}

// lenderFigures is queue "b"'s figures (reclaimWithinCohort Any, nominal 16,
// idle) beside peer "a" of nominal 16 using a's usage.
func lenderFigures(t *testing.T, a kueue.QueueResource) kueue.QueueFigures {
	t.Helper()
	key := kueue.ResourceKey{Flavor: "h100", Resource: "nvidia.com/gpu"}
	topo := kueue.Topology{Queues: map[string]kueue.QueueState{
		"a": {Name: "a", Cohort: "c", Reclaim: kueue.ReclaimNever, HasStatus: true, Resources: map[kueue.ResourceKey]kueue.QueueResource{key: a}},
		"b": {Name: "b", Cohort: "c", Reclaim: kueue.ReclaimAny, HasStatus: true, Resources: map[kueue.ResourceKey]kueue.QueueResource{key: {Nominal: 16}}},
	}}
	figs := kueue.ComputeBorrowing(topo, false).Queues["b"]
	if len(figs) != 1 {
		t.Fatalf("queue b has %d figures, want 1", len(figs))
	}
	return figs[0]
}

// Review N1: an idle lender whose nominal nobody borrows wakes only into a
// node hole. The cluster's four nodes are full of other tenants' pods, which
// no queue of the cohort runs: Kueue admits b's wake from the free pool and
// preempts nothing, so a pod created without a hole stays Pending. The wake
// gets no reclaim credit and is refused. Control: a borrows all 16 of b's
// nominal on those nodes, and Kueue preempts it to admit the wake.
func TestIdleLenderGetsNoWakeCreditForUnborrowedNominal(t *testing.T) {
	ctx := context.Background()
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	allocation.DefaultBorrowInFlight.Reset()
	t.Cleanup(allocation.DefaultBorrowInFlight.Reset)
	decision.PublishGPUUsage(map[string]int{"H100": 32}, nil)
	set := []Candidate{cand("d", "decode", selHBM3, 8, 1), cand("p", "prefill", selHBM3, 8, 1)}

	for _, tc := range []struct {
		name string
		a    kueue.QueueResource
		want SelectionOutcome
	}{
		{"nobody borrows b's nominal: no credit, no capacity", kueue.QueueResource{Nominal: 16, Reserved: 16}, OutcomeNoCapacity},
		{"control: a borrows b's 16: the pair wakes", kueue.QueueResource{Nominal: 16, Reserved: 32, Borrowed: 16}, OutcomePair},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{gpuLimiter: idleLenderProvider{f: lenderFigures(t, tc.a)}}
			p := e.placementFor(ctx, "b")
			in := SelectionInput{Namespace: "b", Candidates: set, Constraints: p.constraints,
				Nodes: nodeView(0, 0, 0, 0), Reclaimable: p.reclaimable}
			got, outcome := selectServingSet(in)
			if outcome != tc.want {
				t.Fatalf("woke %v (%s), want %s; reclaimable %v", names(got), outcome, tc.want, p.reclaimable)
			}
		})
	}
}
