package allocation

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// freeAt is the free GPUs after u of a cohort chunk Kueue reads as 16 free,
// read at readAt.
func freeAt(readAt time.Time, u Unreserved) int {
	q := borrowQuota(16, nsBorrow{ns: "a", cap: config.QuotaUnlimited})
	q.SharedPools[0].ReadAt = readAt
	return DeductUnreservedAt([]*ResourceConstraints{q}, u)[0].SharedPools[0].Free
}

// Review W3: an admitted 8-GPU replica is not free again in a read taken
// before it was admitted. The draw lags once its pods exist, and stays
// deducted from a chunk read no later than that; a read after it counts the
// replica as reserved, and the deduction ends.
func TestAResolvedDrawLagsUntilARead(t *testing.T) {
	t0 := time.Unix(50_000, 0)
	draw := BorrowDraw{Namespace: "a", Variant: "v", Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu", GPUs: 8, Replicas: 1, Target: 3}
	b := NewBorrowInFlight()
	b.RecordAt([]BorrowDraw{draw}, t0)
	b.ObserveAt(map[string]VariantObservation{"a/v": {Current: 3, Desired: 3}}, t0.Add(30*time.Second))
	if got := b.Entries(); len(got) != 0 {
		t.Fatalf("the draw is still in flight once its pods exist: %+v", got)
	}
	u := b.Unreserved(t0.Add(30 * time.Second))
	if got := freeAt(t0, u); got != 8 {
		t.Fatalf("a read from before the pods existed shows %d free, want 8: the admitted replica is not free", got)
	}
	if got := freeAt(t0.Add(time.Minute), u); got != 16 {
		t.Fatalf("a read after the pods existed shows %d free, want 16: it counts the replica already", got)
	}
	if got := freeAt(t0, Unreserved{InFlight: u.InFlight, Gated: map[string]map[string]int{"a": {"": 8}}}); got != 8 {
		t.Fatalf("a lagging draw whose pod is at the gate reads %d free, want 8: counted once", got)
	}
	// Gated, then admitted: the same, from when the admission was seen.
	admitted := Unreserved{Admitted: []AdmittedGPUs{{Namespace: "a", GPUs: 8, At: t0.Add(30 * time.Second)}}}
	if got := freeAt(t0, admitted); got != 8 {
		t.Fatalf("an admitted pod reads %d free in an older read, want 8", got)
	}
	if got := freeAt(t0.Add(time.Minute), admitted); got != 16 {
		t.Fatalf("an admitted pod reads %d free in a newer read, want 16", got)
	}
	b.ObserveAt(map[string]VariantObservation{"a/v": {Current: 3, Desired: 3}}, t0.Add(30*time.Second+InFlightTTL+time.Second))
	if got := b.Unreserved(t0.Add(30 * time.Second)).InFlight; len(got) != 0 {
		t.Fatalf("a lagging draw outlived InFlightTTL: %+v", got)
	}
}

// Review W6: a draw whose desired count reads back 0 -- unknown -- held the
// pool for good. A target the engine decided, 0 included, cuts it, and every
// draw ends InFlightTTL after it was ordered.
func TestADrawEndsOnADecidedTargetOrItsTTL(t *testing.T) {
	t0 := time.Unix(60_000, 0)
	draw := BorrowDraw{Namespace: "a", Variant: "v", Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu", GPUs: 16, Replicas: 2, Target: 3}
	unknown := map[string]VariantObservation{"a/v": {Current: 1, Desired: 0}}

	b := NewBorrowInFlight()
	b.RecordAt([]BorrowDraw{draw}, t0)
	b.ObserveAt(unknown, t0.Add(time.Minute))
	if len(b.Entries()) != 1 {
		t.Fatal("premise: a desired count read as 0 is unknown and keeps the draw")
	}
	b.Trim(map[string]int{"a/v": 2})
	if got := b.Entries(); len(got) != 1 || got[0].Replicas != 1 || got[0].GPUs != 8 || got[0].Target != 2 {
		t.Fatalf("a decided target of 2 leaves %+v, want one replica of 8 GPUs at target 2", got)
	}
	b.Trim(map[string]int{"a/v": 0})
	if got := b.Entries(); len(got) != 0 {
		t.Fatalf("a decided target of 0 keeps %+v", got)
	}

	b.RecordAt([]BorrowDraw{draw}, t0)
	b.ObserveAt(unknown, t0.Add(InFlightTTL))
	if len(b.Entries()) != 1 {
		t.Fatal("control: within InFlightTTL the draw is held")
	}
	b.ObserveAt(unknown, t0.Add(InFlightTTL+time.Second))
	if got := b.Entries(); len(got) != 0 {
		t.Fatalf("a draw ordered past InFlightTTL ago is still held: %+v", got)
	}
}

// Review A4: a variant's own draw in flight re-places on its own GPUs. Its
// pods are not created yet (current 2 of target 3), the pool reads the 8 it
// drew as free still, the record deducts them: without the credit the pass
// refuses the replica it ordered a cycle ago, and the target alternates.
func TestAVariantRePlacesItsOwnDrawInFlight(t *testing.T) {
	t0 := time.Unix(70_000, 0)
	b := NewBorrowInFlight()
	pass := func(now time.Time, current int) int {
		q := borrowQuota(8, nsBorrow{ns: "a", guaranteed: 16, used: 16, cap: config.QuotaUnlimited})
		b.ObserveAt(map[string]VariantObservation{"a/m-v": {Current: current, Desired: 3}}, now)
		cs := DeductUnreservedAt([]*ResourceConstraints{physicalH100(64), q}, b.Unreserved(now))
		o := NewGreedyByScoreOptimizer()
		o.BorrowCredit = b.Entries()
		target := 0
		ds := o.Optimize(t.Context(), []ModelScalingRequest{borrowModel("a", "m", 8, current, 1, nil)}, cs)
		for _, d := range ds {
			target = d.TargetReplicas
		}
		targets := map[string]int{}
		for _, d := range ds {
			targets[d.Namespace+"/"+d.VariantName] = d.TargetReplicas
		}
		b.Trim(targets)
		b.RecordAt(o.BorrowDraws(), now)
		return target
	}
	got := make([]int, 0, 3)
	for i := range 3 {
		got = append(got, pass(t0.Add(time.Duration(i)*30*time.Second), 2))
	}
	for i, n := range got {
		if n != 3 {
			t.Fatalf("targets over three cycles %v, want 3 each: cycle %d refused its own draw", got, i+1)
		}
	}
	if e := b.Entries(); len(e) != 1 || e[0].GPUs != 8 || e[0].Replicas != 1 {
		t.Fatalf("the record holds %+v, want the one 8-GPU draw, not one per cycle", e)
	}
}
