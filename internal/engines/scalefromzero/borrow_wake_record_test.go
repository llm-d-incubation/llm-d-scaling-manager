package scalefromzero

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// Review N3: the shared record was written by the steady-state engine only;
// the wake read it and wrote nothing. A wake on a reclaimWithinCohort Never
// queue (guaranteed 0) takes the cohort's last free 8 GPUs; its pods do not
// exist yet, so no Kueue read shows them, and the steady-state engine, in the
// same window, borrowed the same 8 for a peer. The wake now records what it
// takes of the pool, which the steady-state engine deducts, for as long as
// its pods do not exist -- whether or not the steady-state engine observes
// the woken variant yet -- and only once across the ticks that re-decide the
// same wake.
func TestWakeRecordsWhatItTakesOfThePool(t *testing.T) {
	ctx := context.Background()
	decision.DefaultGPUUsage.Reset()
	t.Cleanup(decision.DefaultGPUUsage.Reset)
	decision.PublishGPUUsage(map[string]int{"H100": 0}, nil)
	record := allocation.DefaultBorrowInFlight
	record.Reset()
	t.Cleanup(record.Reset)

	provider := neverQueueProvider{counted: true}
	e := &Engine{gpuLimiter: provider}
	group := modelGroup{namespace: "chat", modelID: "m"}
	set := []Candidate{cand("d", "decode", "H100", 8, 1)}

	for tick := 1; tick <= 2; tick++ {
		e.beginTick()
		_, selected, outcome := e.decideWake(ctx, group, set, coverage{})
		if len(selected) != 1 {
			t.Fatalf("tick %d: woke %v (%s), want the decode: its own record does not refuse it", tick, names(selected), outcome)
		}
	}
	entries := record.Entries()
	if len(entries) != 1 || entries[0].Variant != "d" || entries[0].GPUs != 8 || entries[0].Cohort != "c" {
		t.Fatalf("the record holds %+v, want one 8-GPU draw of chat/d on cohort c", entries)
	}

	// The steady-state engine's next cycle, in the same window, before it
	// observes the woken variant.
	now := time.Now()
	record.ObserveAt(map[string]allocation.VariantObservation{}, now)
	record.Trim(map[string]int{"chat/d": 0})
	constraints, err := provider.ComputeConstraints(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	after := allocation.DeductUnreservedAt([]*allocation.ResourceConstraints{constraints}, record.Unreserved(now))
	if free := after[0].SharedPools[0].Free; free != 0 {
		t.Fatalf("the steady-state engine sees %d free GPUs in the cohort, want 0: the wake took the last 8", free)
	}

	// Its pods exist: the draw resolves, and lags until a read shows it.
	record.ObserveAt(map[string]allocation.VariantObservation{"chat/d": {Current: 1}}, now)
	if len(record.Entries()) != 0 {
		t.Fatalf("the draw is still in flight after its replica exists: %+v", record.Entries())
	}
}
