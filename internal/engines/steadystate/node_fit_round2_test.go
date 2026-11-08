package steadystate

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// Review round 2 of node fit. The engine's stores are process-wide: no
// t.Parallel.

// Architect/SRE M1: the ResourceConstrained event names what held the
// decision back, node fit included.
func TestTheConstrainedEventNamesWhatHeldItBack(t *testing.T) {
	d := domain.VariantDecision{WasLimited: true, LimitedBy: allocation.NodeFitLimiterName}
	d.SetDecisionReason(domain.ActionNoChange, domain.DecisionReason("V2"), "V2 (optimizer: greedy-by-score)")
	if got, want := constrainedMessage(&d), "V2 (optimizer: greedy-by-score) (limited by node-fit)"; got != want {
		t.Fatalf("event message %q, want %q", got, want)
	}
	d.LimitedBy = ""
	if got := constrainedMessage(&d); got != "V2 (optimizer: greedy-by-score)" {
		t.Fatalf("no limiter named: %q", got)
	}
	// Two limiters tied: the event names both.
	d.LimitedBy, d.LimitedByAlso = "gpu-limiter", []string{"team-quota"}
	if got, want := constrainedMessage(&d), "V2 (optimizer: greedy-by-score) (limited by gpu-limiter and team-quota)"; got != want {
		t.Fatalf("event message %q, want %q", got, want)
	}
}

// Go minor 1: a cycle with no physical limiter logged "fitted to nodes again"
// after a skip, though nothing was fitted. It logs nothing; the next skip is
// reported afresh.
func TestNoPhysicalLimiterIsNotFittedAgain(t *testing.T) {
	ctx, logs := observe(t)
	e := &Engine{}
	e.reportNodeFitSkipped(ctx, true, "no node snapshot")
	e.reportNodeFitSkipped(ctx, false, "")
	if n := logs.FilterMessage("Physical limiter: replicas are fitted to nodes again").Len(); n != 0 {
		t.Fatalf("logged %d 'fitted again' lines with no physical limiter", n)
	}
	e.reportNodeFitSkipped(ctx, true, "no node snapshot")
	if n := logs.FilterMessage("Physical limiter: replicas are not fitted to nodes this cycle").Len(); n != 2 {
		t.Fatalf("the skip after the gap was logged %d times in all, want 2", n)
	}
	e.reportNodeFitSkipped(ctx, true, "")
	if n := logs.FilterMessage("Physical limiter: replicas are fitted to nodes again").Len(); n != 1 {
		t.Fatalf("a real recovery logged %d lines, want 1", n)
	}
}
