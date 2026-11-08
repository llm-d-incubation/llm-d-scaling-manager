package scalefromzero

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// lenderProvider is a cohort of two queues of nominal 16: "a" borrows the 16
// of "b" (reclaimWithinCohort Any) with an 8+8 P/D pair on b's two nodes, so
// all 32 GPUs of the cluster are in use. "b" is read and not counted (not
// selected): its figures come as NamespaceNominal. lentOut 0 is the control,
// a queue that lent nothing.
type lenderProvider struct{ lentOut int }

func (lenderProvider) Name() string { return "cohort-quota" }
func (p lenderProvider) ComputeConstraints(context.Context, map[string]int, map[string]map[string]int) (*allocation.ResourceConstraints, error) {
	c := &allocation.ResourceConstraints{
		ProviderName:   "cohort-quota",
		Pools:          map[string]allocation.ResourcePool{"H100": {Limit: 32, Used: 32}},
		NamespacePools: map[string]map[string]allocation.ResourcePool{"b": {"H100": {Limit: 16}}},
	}
	if p.lentOut > 0 {
		c.NamespaceNominal = map[string]map[string]allocation.NominalFigures{"b": {"H100": {LentOut: p.lentOut, WithinNominal: 16}}}
	}
	return c, nil
}

// Review W1: the lender wakes at zero while the borrower's pair fills its two
// nodes -- no node hole, and the cluster reads full. Kueue preempts the
// borrower to admit the wake, so the wake must create its pods; refusing it as
// no-capacity left the lender at zero for good.
func TestLenderWakesIntoLentNominalWhileBorrowersFillItsNodes(t *testing.T) {
	ctx := context.Background()
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	decision.PublishGPUUsage(map[string]int{"H100": 32}, nil)
	set := []Candidate{cand("d", "decode", selHBM3, 8, 1), cand("p", "prefill", selHBM3, 8, 1)}

	for _, tc := range []struct {
		name    string
		lentOut int
		want    SelectionOutcome
	}{
		{"its queue lent 16 the borrower holds: the pair wakes", 16, OutcomePair},
		{"control: nothing lent out: no capacity", 0, OutcomeNoCapacity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{gpuLimiter: lenderProvider{lentOut: tc.lentOut}}
			p := e.placementFor(ctx, "b")
			in := SelectionInput{Namespace: "b", Candidates: set, Constraints: p.constraints,
				Nodes: nodeView(0, 0, 0, 0), Reclaimable: p.reclaimable}
			got, outcome := selectServingSet(in)
			if outcome != tc.want {
				t.Fatalf("woke %v (%s), want %s", names(got), outcome, tc.want)
			}
		})
	}
}
