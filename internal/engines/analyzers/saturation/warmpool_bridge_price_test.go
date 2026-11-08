package saturation

import (
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/aggregation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// pricedReplica is one live row with a capacity the caller chooses, so that a
// bridge and an ordinary replica can be told apart by their readings. The
// helper in warmpool_bridge_test.go fixes every row at the same capacity, which
// is what the count-focused tests there want and precisely what these cannot
// use: a blended median is invisible when every row reads the same.
func pricedReplica(pod string, demand, tokens int64, fromPool bool) capacity.ReplicaCapacity {
	return capacity.ReplicaCapacity{
		PodName:               pod,
		VariantName:           "variant-a",
		TokensInUse:           demand,
		TotalKvCapacityTokens: tokens,
		MemoryBoundCapacity:   tokens,
		ComputeBoundCapacity:  tokens,
		EffectiveCapacity:     tokens,
		ReplicaDemand:         demand,
		FromWarmPool:          fromPool,
	}
}

// P IS THE PRICE OF THE VARIANT'S OWN REPLICAS, NOT A BLEND.
//
// The linearity invariant is supply = replicas x P, and the capacity-build step
// counts replicas from the scale target -- which a lent warm pool Pod is not part
// of. So a P averaged over a bridge prices the counted replicas at a number none
// of them delivers, and the two factors describe different populations.
//
// The readings genuinely differ: the pool runs its engines at a lower
// --gpu-memory-utilization than the workload does, because it fits several
// sleepers in a Pod where a workload replica has the GPU to itself. Measured on
// the shared GPU cluster, 2026-08-31: pool 0.90, workload 0.95.
//
// One own replica and one bridge is the worst case, and the one used here: a
// two-element median is the average of the two, so the whole gap lands in P.
func TestABridgeDoesNotSetTheVariantsPerReplicaPrice(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())

	own := pricedReplica("variant-a-0", 100, 1000, false)
	bridge := pricedReplica("pool-0", 100, 600, true)

	got := a.aggregateByVariant([]capacity.ReplicaCapacity{own, bridge},
		nil, oneVariantState(1), "model", "ns", 0.9, false, 1, logr.Discard())

	if len(got) != 1 {
		t.Fatalf("variants = %+v, want one", got)
	}
	// Blended, this is median(600, 1000) = 800 -- the value this test exists to
	// reject. Every own replica would then be priced 20% under what it serves.
	if got[0].PerReplicaCapacity != 1000 {
		t.Errorf("PerReplicaCapacity = %v, want 1000 (the own replica's own reading): "+
			"a bridge runs at a lower memory utilization and must not set the price "+
			"of the replicas the scale target counts", got[0].PerReplicaCapacity)
	}
	if got[0].WarmPoolReplicas != 1 {
		t.Errorf("WarmPoolReplicas = %d, want 1", got[0].WarmPoolReplicas)
	}
	if got[0].ReplicaCount != 1 {
		t.Errorf("ReplicaCount = %d, want 1 (the bridge is not one of the variant's replicas)",
			got[0].ReplicaCount)
	}
	// The asymmetry is the whole design: demand spans own + bridge, price does not.
	if got[0].TotalDemand != 200 {
		t.Errorf("TotalDemand = %v, want 200 (both rows): a bridge serves this variant's traffic",
			got[0].TotalDemand)
	}
}

// WarmPoolCapacity IS NOT THE ANALYZER'S TO WRITE.
//
// It is WarmPoolReplicas x WarmPoolPerReplicaCapacity -- derived from two
// measurements -- and every derived capacity belongs to the capacity-build step.
// A derived value written inside one analyzer is absent from the other two.
func TestTheAnalyzerLeavesTheDerivedBridgeCapacityToTheBuilder(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())

	got := a.aggregateByVariant([]capacity.ReplicaCapacity{
		pricedReplica("variant-a-0", 100, 1000, false),
		pricedReplica("pool-0", 100, 600, true),
	}, nil, oneVariantState(1), "model", "ns", 0.9, false, 1, logr.Discard())

	if got[0].WarmPoolCapacity != 0 {
		t.Errorf("WarmPoolCapacity = %v, want 0 from the analyzer: the builder derives it "+
			"from WarmPoolReplicas x WarmPoolPerReplicaCapacity", got[0].WarmPoolCapacity)
	}
	// The measurement it derives that from is the analyzer's, and it is the
	// BRIDGE's reading -- not the variant's own price reused under another name.
	if got[0].WarmPoolPerReplicaCapacity != 600 {
		t.Errorf("WarmPoolPerReplicaCapacity = %v, want 600 (the bridge's own reading, "+
			"not the own replica's 1000)", got[0].WarmPoolPerReplicaCapacity)
	}
}

// A VARIANT CARRIED ONLY BY A BRIDGE IS STILL PRICED.
//
// Its own replicas reported nothing this cycle, so there is no own reading to
// use. The bridge's is the closest thing that was actually observed, and it errs
// low -- which asks for one replica too many rather than one too few.
//
// Zero would be worse than either: the optimizer skips a variant whose
// per-replica capacity is <= 0, so a variant a bridge is currently carrying
// would stop being scaled at all, exactly while it is short.
func TestAVariantCarriedOnlyByABridgeIsStillPriced(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())

	got := a.aggregateByVariant([]capacity.ReplicaCapacity{
		pricedReplica("pool-0", 400, 600, true),
	}, nil, oneVariantState(0), "model", "ns", 0.9, false, 1, logr.Discard())

	if got[0].PerReplicaCapacity != 600 {
		t.Errorf("PerReplicaCapacity = %v, want 600 (the only reading there was): "+
			"zero would make the optimizer skip a variant a bridge is carrying",
			got[0].PerReplicaCapacity)
	}
	if got[0].ReplicaCount != 0 {
		t.Errorf("ReplicaCount = %d, want 0: a bridge is not one of the variant's replicas",
			got[0].ReplicaCount)
	}
}

// A VARIANT CARRIED ONLY BY A BRIDGE REPORTS THE BORROW ITS PRICE CAME FROM.
//
// A bridge is priced under the variant's own learned state, so its k2 can be
// a sibling's, borrowed. With no own replica reporting, that bridge's reading
// IS the variant's price (and the cycle log says P2-borrow), so the borrow is
// reported too -- otherwise the metric says "own figures" while the variant is
// sized on a sibling's.
func TestAVariantCarriedOnlyByABridgeReportsItsBorrow(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())

	bridge := pricedReplica("pool-0", 400, 600, true)
	bridge.K2Priority = capacity.K2SrcBorrowed
	bridge.K2BorrowedFrom = "main"
	bridge.K2BorrowRelaxed = 0.25

	got := a.aggregateByVariant([]capacity.ReplicaCapacity{bridge},
		nil, oneVariantState(0), "model", "ns", 0.9, false, 1, logr.Discard())

	want := []domain.LearnedStateBorrow{
		{Quantity: domain.BorrowedK2, Donor: "main", DonorNamespace: "ns", Relaxed: 0.25},
	}
	if len(got[0].Borrowed) != 1 || got[0].Borrowed[0] != want[0] {
		t.Errorf("Borrowed = %+v, want %+v: the bridge's borrowed k2 is the variant's price",
			got[0].Borrowed, want)
	}

	// With an own replica reporting, the price is the own replica's, and a
	// bridge's borrow is not the variant's.
	own := pricedReplica("variant-a-0", 100, 1000, false)
	got = a.aggregateByVariant([]capacity.ReplicaCapacity{own, bridge},
		nil, oneVariantState(1), "model", "ns", 0.9, false, 1, logr.Discard())
	if len(got[0].Borrowed) != 0 {
		t.Errorf("Borrowed = %+v, want none: the own replica set the price", got[0].Borrowed)
	}
}

// The k2 source and age of a variant carried only by a bridge are the
// bridge's: it is the row the variant's price came from. With an own replica
// reporting, they are the own replica's.
func TestAVariantCarriedOnlyByABridgeReportsItsK2Source(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())

	bridge := pricedReplica("pool-0", 300, 600, true)
	bridge.ComputeBoundCapacity, bridge.EffectiveCapacity = 400, 400
	bridge.K2Priority = capacity.K2SrcBorrowed
	bridge.K2BorrowedFrom = "main"
	bridge.K2WriteAge, bridge.K2WriteAgeKnown = 2*time.Hour, true

	got := a.aggregateByVariant([]capacity.ReplicaCapacity{bridge},
		nil, oneVariantState(0), "model", "ns", 0.9, false, 1, logr.Discard())
	if got[0].K2Source != "P2-borrow" || !got[0].HasK2Age || got[0].K2Age != 2*time.Hour {
		t.Errorf("K2Source = %q, age %v (known %v), want P2-borrow, 2h: the bridge's k2 is the variant's price",
			got[0].K2Source, got[0].K2Age, got[0].HasK2Age)
	}

	own := pricedReplica("variant-a-0", 100, 1000, false)
	own.ComputeBoundCapacity, own.EffectiveCapacity = 800, 800
	own.K2Priority = capacity.K2SrcObserved
	own.K2WriteAgeKnown = true
	got = a.aggregateByVariant([]capacity.ReplicaCapacity{own, bridge},
		nil, oneVariantState(1), "model", "ns", 0.9, false, 1, logr.Discard())
	if got[0].K2Source != "P1-obs" || got[0].K2Age != 0 {
		t.Errorf("K2Source = %q, age %v, want P1-obs, 0: the own replica set the price",
			got[0].K2Source, got[0].K2Age)
	}
}

// ANTICIPATED SUPPLY IS STILL COMPUTED, AND AT THE OWN PRICE.
//
// It is (ReplicaCount + PendingReplicas) x P, and a replica that is still
// starting is one of the VARIANT'S OWN -- it will run on the workload's terms,
// not the pool's. Pricing it at a median blended with a bridge would understate
// what the in-flight scale-up is about to deliver, and anticipated supply exists
// precisely to stop a second scale-up being ordered while the first is booting.
func TestAnticipatedSupplyIsPricedAtTheOwnReading(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())

	states := []domain.VariantReplicaState{{
		VariantName:     "variant-a",
		CurrentReplicas: 2, // one ready, one still coming up
		PendingReplicas: 1,
		GPUsPerReplica:  1,
	}}

	got := a.aggregateByVariant([]capacity.ReplicaCapacity{
		pricedReplica("variant-a-0", 100, 1000, false),
		pricedReplica("pool-0", 100, 600, true),
	}, nil, states, "model", "ns", 0.9, false, 1, logr.Discard())

	if got[0].ReplicaCount != 1 || got[0].PendingReplicas != 1 {
		t.Fatalf("counts = %d ready / %d pending, want 1 / 1",
			got[0].ReplicaCount, got[0].PendingReplicas)
	}
	// Blended this would be (1+1) x 800 = 1600, understating the arriving
	// replica by a fifth.
	if got := aggregation.SumTotalAnticipatedSupply(got); got != 2000 {
		t.Errorf("TotalAnticipatedSupply = %v, want 2000 ((1 ready + 1 pending) x 1000): "+
			"a pending replica is the variant's own and arrives at the own price", got)
	}
}
