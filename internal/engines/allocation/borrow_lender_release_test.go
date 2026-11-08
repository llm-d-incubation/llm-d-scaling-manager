package allocation

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// PR-C: a waiting lender Kueue does not reclaim for is handed back what it
// waits for, when the entry opts in (releaseToNonReclaimingLenders). Every
// replica here is a whole 8-GPU node unless a case says otherwise.

// longAgo is when the lenders of these tests started to wait: past Kueue's
// admission lag, so one read is enough to release (releaseDue).
var longAgo = time.Unix(1000, 0)

// waitingLender is a lender on Never whose smallest admissible Workload asks
// pending GPUs, lending unused, waiting since longAgo.
func waitingLender(pending, unused int) kueue.LenderState {
	return kueue.LenderState{Queue: "lender", Waiting: true, Reclaim: kueue.ReclaimNever, Unused: unused,
		PendingGPUs: pending, PendingSince: longAgo}
}

// relWorld is namespace "b" at guaranteed g holding used GPUs on H100, in
// cohort "c", whose pool reads l (and more), Kueue's raw free pool rawFree
// and Kueue's borrowed figure borrowed; release marks the pool as opted in.
type relWorld struct {
	g, used, borrowed, rawFree int
	l                          kueue.LenderState
	more                       []kueue.LenderState
	release                    bool
	readAt                     time.Time
	classPriority              map[string]int32
	borrowerPriority           *int32
	borrowerLowest             *int32
	evicting                   int
	evictionsRead              bool
}

func (w relWorld) constraints() []*ResourceConstraints {
	cs := govWorld{g: w.g, used: w.used, lenders: append([]kueue.LenderState{w.l}, w.more...), readAt: w.readAt}.constraints()
	ch := &cs[0].SharedPools[0]
	ch.ReleaseToNonReclaiming = w.release
	ch.Borrowed = w.borrowed
	if w.borrowed == 0 {
		ch.Borrowed = max(0, w.used-w.g)
	}
	ch.RawFree = w.rawFree
	ch.ClassPriority = w.classPriority
	ch.BorrowerPriority = w.borrowerPriority
	ch.BorrowerLowest = w.borrowerLowest
	ch.EvictingGPUs, ch.EvictionsRead = w.evicting, w.evictionsRead
	return cs
}

// cycleOf runs one cycle: Observe, then Releases on no-change decisions.
func cycleOf(g *BorrowGovernor, at time.Time, w relWorld, reqs ...ModelScalingRequest) ([]BorrowRelease, []domain.VariantDecision) {
	if w.readAt.IsZero() {
		w.readAt = at
	}
	c := BorrowCycle{Now: at, Constraints: w.constraints(), Requests: reqs}
	g.Observe(c)
	ds := decisionsOf(reqs)
	for i := range ds {
		ds[i].Role = roleOf(reqs, ds[i].Namespace, ds[i].VariantName)
	}
	return g.Releases(c, ds), ds
}

// roleOf is a variant's role as its request states it.
func roleOf(reqs []ModelScalingRequest, ns, variant string) string {
	for _, r := range reqs {
		for _, s := range r.VariantStates {
			if r.Namespace == ns && s.VariantName == variant {
				return s.Role
			}
		}
	}
	return ""
}

// lowered is how many replicas the decisions take off each variant.
func lowered(ds []domain.VariantDecision) map[string]int {
	out := map[string]int{}
	for _, d := range ds {
		if n := d.CurrentReplicas - d.TargetReplicas; n > 0 {
			out[d.VariantName] = n
		}
	}
	return out
}

func withClass(r ModelScalingRequest, class string) ModelScalingRequest {
	for i := range r.VariantStates {
		r.VariantStates[i].PriorityClass = class
	}
	return r
}

func withMin(r ModelScalingRequest, n int) ModelScalingRequest {
	for i := range r.VariantStates {
		r.VariantStates[i].MinReplicas = &n
	}
	return r
}

// blockedOf is the one block the governor reported for the lender, if any.
func blockedOf(g *BorrowGovernor, lender string) (LenderReleaseBlock, bool) {
	for _, b := range g.LenderReport().Blocked {
		if b.Lender.Lender == lender {
			return b, true
		}
	}
	return LenderReleaseBlock{}, false
}

func TestLenderReleaseOffChangesNothing(t *testing.T) {
	t0 := time.Unix(50000, 0)
	g := NewBorrowGovernor(DefaultBorrowSettings())
	w := relWorld{g: 16, used: 48, l: waitingLender(24, 32)}
	for i := range 4 {
		rel, ds := cycleOf(g, t0.Add(time.Duration(i)*time.Minute), w, govModel("x", 6, 8))
		if len(rel) != 0 || len(lowered(ds)) != 0 {
			t.Fatalf("cycle %d: released %+v (lowered %v) with releaseToNonReclaimingLenders off", i, rel, lowered(ds))
		}
	}
	raw, err := g.MarshalLedger()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "version") || strings.Contains(raw, "lenderReleases") {
		t.Errorf("ledger %s carries release fields with the option off; it must stay what it always was", raw)
	}
}

func TestLenderReleaseUnits(t *testing.T) {
	t0 := time.Unix(50000, 0)
	settings := DefaultBorrowSettings()
	on := func(g, used, pending, unused int) relWorld {
		return relWorld{g: g, used: used, l: waitingLender(pending, unused), release: true}
	}

	t.Run("a waiting lender gets ceil(shortfall/unit) units in one pass", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		rel, ds := cycleOf(gov, t0, on(16, 48, 20, 32), govModel("x", 6, 8))
		if got := lowered(ds)["x-v"]; got != 3 {
			t.Fatalf("lowered x by %d, want ceil(20/8) = 3 at once", got)
		}
		if len(rel) != 1 || rel[0].Cause != ReleaseLenderWaiting || rel[0].From != 6 || rel[0].To != 3 || rel[0].Lender.Lender != "lender" {
			t.Errorf("releases = %+v, want one x 6->3, cause lender-waiting, to lender", rel)
		}
	})

	t.Run("never more than Kueue's borrowed figure; a lender pending nothing gets nothing", func(t *testing.T) {
		for _, tc := range []struct {
			name                      string
			pending, unused, borrowed int
			want                      int
		}{
			{"a need past what is borrowed: nothing", 16, 32, 8, 0},
			{"a need within it", 8, 32, 8, 1},
			{"a lender pending nothing gets nothing", 0, 32, 32, 0},
		} {
			gov := NewBorrowGovernor(settings)
			w := on(16, 48, tc.pending, tc.unused)
			w.borrowed = tc.borrowed
			_, ds := cycleOf(gov, t0, w, govModel("x", 6, 8))
			if got := lowered(ds)["x-v"]; got != tc.want {
				t.Errorf("%s: lowered x by %d, want %d", tc.name, got, tc.want)
			}
		}
	})

	t.Run("never below the guaranteed figure: a need past what lies above it is not served at all", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		_, ds := cycleOf(gov, t0, on(40, 48, 24, 32), govModel("x", 6, 8))
		if got := lowered(ds)["x-v"]; got != 0 {
			t.Errorf("lowered x by %d, want 0: only 8 GPUs lie past guaranteed 40, the lender lacks 24", got)
		}
		b, ok := blockedOf(gov, "lender")
		if !ok || b.Reason != LenderUnservable || b.Need != 24 || b.Givable != 8 {
			t.Errorf("report %+v, want the lender unservable: needs 24, 8 can be given", b)
		}
	})

	t.Run("never below minReplicas", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		_, ds := cycleOf(gov, t0, on(16, 48, 16, 32), withMin(govModel("x", 6, 8), 4))
		if got := lowered(ds)["x-v"]; got != 2 {
			t.Errorf("lowered x by %d, want 2 down to minReplicas 4", got)
		}
		gov = NewBorrowGovernor(settings)
		_, ds = cycleOf(gov, t0, on(16, 48, 16, 32), withMin(govModel("x", 6, 8), 5))
		if got := lowered(ds)["x-v"]; got != 0 {
			t.Errorf("lowered x by %d, want 0: minReplicas 5 leaves 8 of the 16 the lender lacks", got)
		}
	})

	t.Run("lowest priority model first, a class not read last", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		w := on(0, 72, 16, 72)
		w.classPriority = map[string]int32{"serving": 1000, "batch": 10}
		_, ds := cycleOf(gov, t0, w, withClass(govModel("x", 3, 8), "serving"),
			withClass(govModel("y", 3, 8), "batch"), withClass(govModel("z", 3, 8), "unread"))
		if got := lowered(ds); got["y-v"] != 2 || got["x-v"] != 0 || got["z-v"] != 0 {
			t.Errorf("lowered %v, want y (batch, 10) by 2 and nothing else", got)
		}
		gov = NewBorrowGovernor(settings)
		w.l = waitingLender(56, 72)
		_, ds = cycleOf(gov, t0, w, withClass(govModel("x", 3, 8), "serving"),
			withClass(govModel("y", 3, 8), "batch"), withClass(govModel("z", 3, 8), "unread"))
		if got := lowered(ds); got["y-v"] != 3 || got["x-v"] != 3 || got["z-v"] != 1 {
			t.Errorf("lowered %v, want y 3, then x 3, then z (priority not read) 1", got)
		}
	})

	t.Run("a 16-GPU gang goes whole for an 8-GPU need, never below guaranteed", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		_, ds := cycleOf(gov, t0, on(16, 48, 8, 32), govModel("x", 3, 16))
		if got := lowered(ds)["x-v"]; got != 1 {
			t.Errorf("lowered x by %d groups, want 1: a unit one replica past the need still goes", got)
		}
		gov = NewBorrowGovernor(settings)
		_, ds = cycleOf(gov, t0, on(40, 48, 8, 32), govModel("x", 3, 16))
		if got := lowered(ds)["x-v"]; got != 0 {
			t.Errorf("lowered x by %d groups, want 0: a 16-GPU group past only 8 borrowed GPUs takes guaranteed", got)
		}
	})

	t.Run("Kueue's own reclaim is left to Kueue", func(t *testing.T) {
		any := waitingLender(16, 32)
		any.Reclaim = kueue.ReclaimAny
		p := func(n int32) *int32 { return &n }
		outranks := waitingLender(16, 32)
		outranks.Reclaim, outranks.Priority, outranks.PendingPriority = kueue.ReclaimLowerPriority, p(5), p(500)
		below := outranks
		below.PendingPriority = p(5)
		for _, tc := range []struct {
			name string
			l    kueue.LenderState
			want int
		}{{"a lender on Any", any, 0}, {"a LowerPriority lender whose head is above the model", outranks, 0},
			{"a LowerPriority lender whose head is below the model", below, 2}} {
			gov := NewBorrowGovernor(settings)
			w := relWorld{g: 16, used: 48, l: tc.l, release: true, classPriority: map[string]int32{"batch": 10}}
			_, ds := cycleOf(gov, t0, w, withClass(govModel("x", 6, 8), "batch"))
			if got := lowered(ds)["x-v"]; got != tc.want {
				t.Errorf("%s: lowered x by %d, want %d", tc.name, got, tc.want)
			}
		}
	})

	t.Run("a stale read hands nothing back", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		w := on(16, 48, 16, 32)
		c := BorrowCycle{Now: t0, Constraints: w.constraints(), Requests: []ModelScalingRequest{govModel("x", 6, 8)}}
		c.Constraints[0].SharedPools[0].Stale = true
		gov.Observe(c)
		ds := decisionsOf(c.Requests)
		if rel := gov.Releases(c, ds); len(rel) != 0 {
			t.Errorf("released %+v on a stale read", rel)
		}
	})
}

// PR #203 review: a release is sized to what unblocks the lender, net of
// everything already covering it, and handed back only when it can be.
func TestLenderReleaseSizing(t *testing.T) {
	t0 := time.Unix(50000, 0)
	settings := DefaultBorrowSettings()
	p := func(n int32) *int32 { return &n }

	// KC1: Kueue admits into the lender's kept part, and from the free pool,
	// with nothing handed back.
	t.Run("net of the lender's kept part and of Kueue's free pool", func(t *testing.T) {
		kept := waitingLender(16, 0)
		kept.Kept = 16
		gov := NewBorrowGovernor(settings)
		_, ds := cycleOf(gov, t0, relWorld{g: 16, used: 32, l: kept, release: true}, govModel("x", 4, 8))
		if got := lowered(ds)["x-v"]; got != 0 {
			t.Errorf("lowered x by %d, want 0: the 16-GPU gang fits the 16 the lender keeps", got)
		}
		gov = NewBorrowGovernor(settings)
		_, ds = cycleOf(gov, t0, relWorld{g: 16, used: 48, rawFree: 8, l: waitingLender(16, 32), release: true}, govModel("x", 6, 8))
		if got := lowered(ds)["x-v"]; got != 1 {
			t.Errorf("lowered x by %d, want 1: the free 8 cover half of the 16", got)
		}
	})

	// GT1, WP4: one budget per pool.
	t.Run("several waiting lenders of one pool share the free pool and Kueue's borrowed figure", func(t *testing.T) {
		second := waitingLender(8, 16)
		second.Queue = "lender-2"
		gov := NewBorrowGovernor(settings)
		w := relWorld{g: 16, used: 48, borrowed: 8, l: waitingLender(8, 16), more: []kueue.LenderState{second}, release: true}
		rel, ds := cycleOf(gov, t0, w, govModel("x", 6, 8))
		if got := lowered(ds)["x-v"]; got != 1 || len(rel) != 1 {
			t.Fatalf("lowered x by %d (%+v), want 1: Kueue reports 8 borrowed, two lenders of 8 are not both handed it", got, rel)
		}
		if b, ok := blockedOf(gov, "lender-2"); !ok || b.Reason != LenderUnservable {
			t.Errorf("report %+v, want lender-2 reported unservable", gov.LenderReport())
		}
		gov = NewBorrowGovernor(settings)
		w = relWorld{g: 16, used: 48, rawFree: 8, l: waitingLender(8, 16), more: []kueue.LenderState{second}, release: true}
		_, ds = cycleOf(gov, t0, w, govModel("x", 6, 8))
		if got := lowered(ds)["x-v"]; got != 1 {
			t.Errorf("lowered x by %d, want 1: the free 8 serve one lender, the other is handed 8", got)
		}
	})

	// SF1: what this controller cannot hand back in full it does not start
	// handing back.
	t.Run("a lender this controller cannot serve is handed nothing and reported", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		w := relWorld{g: 16, used: 24, borrowed: 16, l: waitingLender(16, 16), release: true}
		rel, ds := cycleOf(gov, t0, w, govModel("x", 3, 8))
		if len(rel) != 0 || lowered(ds)["x-v"] != 0 {
			t.Errorf("released %+v, want nothing: our 8 of the 16 borrowed cannot admit a 16-GPU gang", rel)
		}
		b, ok := blockedOf(gov, "lender")
		if !ok || b.Reason != LenderUnservable || b.Need != 16 || b.Givable != 8 || len(b.Models) != 1 || b.Models[0] != "b/x" {
			t.Errorf("report %+v, want lender unservable, need 16, givable 8, candidate b/x", b)
		}
		if sh := gov.LenderReport().Shared; len(sh) != 1 || sh[0].Kueue != 16 || sh[0].Ours != 8 {
			t.Errorf("shared %+v, want Kueue's 16 against our 8", sh)
		}
	})

	// GT2: only replicas held and kept count.
	t.Run("a target above current frees nothing; only replicas it holds count", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		c := BorrowCycle{Now: t0, Constraints: relWorld{g: 16, used: 48, l: waitingLender(8, 32), release: true, readAt: t0}.constraints(),
			Requests: []ModelScalingRequest{govModel("x", 4, 8), govModel("y", 2, 8)}}
		gov.Observe(c)
		ds := decisionsOf(c.Requests)
		ds[0].TargetReplicas = 5 // x grows 4 -> 5
		rel := gov.Releases(c, ds)
		if ds[0].TargetReplicas != 3 || len(rel) != 1 || rel[0].From != 4 || rel[0].To != 3 {
			t.Errorf("x target %d, releases %+v; want x 4 -> 3: cancelling its growth frees nothing, one held replica does", ds[0].TargetReplicas, rel)
		}
		if e := gov.lenderRel[LenderKey{Cohort: "c", Lender: "lender", Flavor: "h100", Resource: "nvidia.com/gpu"}]; e == nil || e.GPUs != 8 {
			t.Errorf("in flight %+v, want 8 GPUs handed back", e)
		}
	})

	// WP5.
	t.Run("a single-role model gives a replica on the lender's flavor", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		w := relWorld{g: 0, used: 16, l: waitingLender(8, 16), release: true, readAt: t0}
		cs := w.constraints()
		cs[0].NamespacePools["b"]["A100"] = ResourcePool{Limit: 0, Used: 16}
		req := govModel("x", 2, 8)
		req.VariantStates = append([]domain.VariantReplicaState{{VariantName: "a-v", CurrentReplicas: 2, GPUsPerReplica: 8,
			AcceleratorName: "A100"}}, req.VariantStates...)
		c := BorrowCycle{Now: t0, Constraints: cs, Requests: []ModelScalingRequest{req}}
		gov.Observe(c)
		ds := decisionsOf(c.Requests)
		gov.Releases(c, ds)
		if got := lowered(ds); got["a-v"] != 0 || got["x-v"] != 1 {
			t.Errorf("lowered %v, want the H100 replica (x-v), not the first variant by name on A100", got)
		}
	})

	// KC4, SF3.
	t.Run("a LowerPriority lender is judged by its head's priority; Kueue's preemption counts", func(t *testing.T) {
		l := waitingLender(16, 32)
		l.Reclaim, l.Priority, l.PendingPriority = kueue.ReclaimLowerPriority, p(10), p(100)
		w := relWorld{g: 0, used: 32, l: l, release: true, classPriority: map[string]int32{"mid": 50, "top": 500}}
		gov := NewBorrowGovernor(settings)
		_, ds := cycleOf(gov, t0, w, withClass(govModel("m", 2, 8), "mid"), withClass(govModel("s", 2, 8), "top"))
		if got := lowered(ds); got["m-v"] != 0 || got["s-v"] != 0 {
			t.Errorf("lowered %v, want nothing: Kueue preempts m (50) for the head at 100, which covers the 16", got)
		}
		l.PendingGPUs = 24
		w.l = l
		gov = NewBorrowGovernor(settings)
		_, ds = cycleOf(gov, t0, w, withClass(govModel("m", 2, 8), "mid"), withClass(govModel("s", 2, 8), "top"))
		if got := lowered(ds); got["m-v"] != 0 || got["s-v"] != 1 {
			t.Errorf("lowered %v, want s by 1: Kueue takes m's 16, the controller the 8 left", got)
		}
	})

	t.Run("under a LowerPriority lender a model whose priority was not read is left alone", func(t *testing.T) {
		l := waitingLender(8, 32)
		l.Reclaim, l.PendingPriority = kueue.ReclaimLowerPriority, p(100)
		gov := NewBorrowGovernor(settings)
		_, ds := cycleOf(gov, t0, relWorld{g: 0, used: 16, l: l, release: true}, withClass(govModel("u", 2, 8), "unread"))
		if got := lowered(ds)["u-v"]; got != 0 {
			t.Errorf("lowered u by %d, want 0: Kueue may preempt it, the controller does not lower it too", got)
		}
		if b, ok := blockedOf(gov, "lender"); !ok || b.Reason != LenderUnservable || b.Need != 8 {
			t.Errorf("report %+v, want the lender unservable for 8: u is neither lowered nor counted as preempted", b)
		}
	})

	t.Run("a LowerPriority lender above every borrower is left to Kueue", func(t *testing.T) {
		l := waitingLender(8, 32)
		l.Reclaim, l.PendingPriority = kueue.ReclaimLowerPriority, p(100)
		gov := NewBorrowGovernor(settings)
		w := relWorld{g: 0, used: 16, l: l, release: true, borrowerPriority: p(50), classPriority: map[string]int32{"top": 500}}
		_, ds := cycleOf(gov, t0, w, withClass(govModel("s", 2, 8), "top"))
		if got := lowered(ds)["s-v"]; got != 0 {
			t.Errorf("lowered s by %d, want 0: everything borrowed is held at 50, below the head", got)
		}
	})

	// KC5.
	t.Run("one read of a fresh wait triggers nothing; a second read, or a long wait, does", func(t *testing.T) {
		fresh := waitingLender(8, 32)
		fresh.PendingSince = t0.Add(-10 * time.Second)
		w := relWorld{g: 16, used: 48, l: fresh, release: true}
		gov := NewBorrowGovernor(settings)
		if _, ds := cycleOf(gov, t0, w, govModel("x", 6, 8)); lowered(ds)["x-v"] != 0 {
			t.Fatalf("lowered %v on the first read of a 10s wait", lowered(ds))
		}
		if _, ds := cycleOf(gov, t0.Add(20*time.Second), w, govModel("x", 6, 8)); lowered(ds)["x-v"] != 1 {
			t.Errorf("lowered %v on the second fresh read, want 1", lowered(ds))
		}
	})

	t.Run("a release never names more variants than a ledger entry may hold", func(t *testing.T) {
		reqs := make([]ModelScalingRequest, 0, maxLedgerUnits+1)
		for i := range maxLedgerUnits + 1 {
			reqs = append(reqs, govModel(fmt.Sprintf("m%03d", i), 1, 8))
		}
		n := 8 * (maxLedgerUnits + 1)
		gov := NewBorrowGovernor(settings)
		rel, _ := cycleOf(gov, t0, relWorld{g: 0, used: n, l: waitingLender(n, n), release: true}, reqs...)
		if len(rel) != 0 {
			t.Errorf("released %d variants, want none: %d would pass the ledger's %d", len(rel), maxLedgerUnits+1, maxLedgerUnits)
		}
		gov = NewBorrowGovernor(settings)
		rel, _ = cycleOf(gov, t0, relWorld{g: 0, used: n, l: waitingLender(n-8, n), release: true}, reqs...)
		if len(rel) != maxLedgerUnits {
			t.Errorf("released %d variants, want %d, the ledger's limit", len(rel), maxLedgerUnits)
		}
	})

	t.Run("two entries over one cohort release only when both opt in", func(t *testing.T) {
		w := relWorld{g: 16, used: 48, l: waitingLender(8, 32), release: true, readAt: t0}
		for _, other := range []bool{true, false} {
			cs := w.constraints()
			second := *cs[0]
			second.SharedPools = []SharedPoolChunk{cs[0].SharedPools[0]}
			second.SharedPools[0].ReleaseToNonReclaiming = other
			c := BorrowCycle{Now: t0, Constraints: append(cs, &second), Requests: []ModelScalingRequest{govModel("x", 6, 8)}}
			gov := NewBorrowGovernor(settings)
			gov.Observe(c)
			ds := decisionsOf(c.Requests)
			gov.Releases(c, ds)
			if got, want := lowered(ds)["x-v"], map[bool]int{true: 1, false: 0}[other]; got != want {
				t.Errorf("second entry opts in %t: lowered x by %d, want %d", other, got, want)
			}
		}
	})

	// WP6: the cooldown runs from the landing, when the model gave the GPUs
	// back, not from the decision; until then the pin holds it.
	t.Run("a model released starts its cooldown in the lender's cohort when the release lands", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		w := relWorld{g: 16, used: 48, l: waitingLender(8, 32), release: true}
		cycleOf(gov, t0, w, govModel("x", 6, 8))
		if gov.BorrowBlockedIn("b", "x", "c", t0.Add(time.Second)) {
			t.Error("x's cooldown started before its replica was gone")
		}
		landed := t0.Add(6 * time.Minute)
		w.used = 40
		w.borrowed = 32 // Kueue has not taken it in yet: the entry stays
		cycleOf(gov, landed, w, govModel("x", 5, 8))
		if !gov.BorrowBlockedIn("b", "x", "c", landed.Add(settings.Cooldown-time.Second)) {
			t.Error("x may borrow in cohort c within a cooldown of its release landing")
		}
		if gov.BorrowBlockedIn("b", "x", "other", landed.Add(time.Second)) {
			t.Error("x's cooldown reaches a cohort the lender is not in")
		}
	})
}

// PR #203 review KC2, KC3, WP3, WP7 and the ledger minors: when a release
// in flight clears, and what a restored one may pin.
func TestLenderReleaseLanding(t *testing.T) {
	t0 := time.Unix(50000, 0)
	settings := DefaultBorrowSettings()
	key := LenderKey{Cohort: "c", Lender: "lender", Flavor: "h100", Resource: "nvidia.com/gpu"}

	t.Run("a landing alone clears nothing: a read that lags it releases nothing again", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		w := relWorld{g: 16, used: 48, l: waitingLender(16, 32), release: true}
		cycleOf(gov, t0, w, govModel("x", 6, 8))
		landed := t0.Add(3 * time.Minute)
		w.readAt = landed.Add(-5 * time.Second)
		cycleOf(gov, landed, w, govModel("x", 4, 8)) // lands
		// A fresh read after the landing, Kueue's figure not updated yet.
		w.readAt = landed.Add(15 * time.Second)
		if rel, _ := cycleOf(gov, landed.Add(20*time.Second), w, govModel("x", 4, 8)); len(rel) != 0 {
			t.Fatalf("released %+v on a read that lags the landing", rel)
		}
		if gov.lenderRel[key] == nil {
			t.Fatal("the release cleared before Kueue took it in")
		}
		w.used, w.readAt = 32, landed.Add(30*time.Second) // borrowed 32 -> 16
		gov.Observe(BorrowCycle{Now: landed.Add(35 * time.Second), Constraints: w.constraints(),
			Requests: []ModelScalingRequest{govModel("x", 4, 8)}})
		if gov.lenderRel[key] != nil {
			t.Error("the release is still in flight after Kueue's borrowed figure fell by its 16")
		}
	})

	t.Run("a release another borrower took stops releases to that lender while it waits", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		w := relWorld{g: 16, used: 48, l: waitingLender(16, 32), release: true}
		cycleOf(gov, t0, w, govModel("x", 6, 8))
		landed := t0.Add(3 * time.Minute)
		w.borrowed = 32 // x's 16 went, someone else's 16 came: Kueue's figure stands
		w.readAt = landed
		cycleOf(gov, landed, w, govModel("x", 4, 8))
		w.readAt = landed.Add(kueueSchedulingInterval + time.Second)
		rel, ds := cycleOf(gov, w.readAt, w, govModel("x", 4, 8))
		if len(rel) != 0 || lowered(ds)["x-v"] != 0 {
			t.Errorf("released %+v, lowered %v; want nothing more, and no pin left", rel, lowered(ds))
		}
		if b, ok := blockedOf(gov, "lender"); !ok || b.Reason != LenderTaken {
			t.Errorf("report %+v, want the lender reported %s", gov.LenderReport(), LenderTaken)
		}
		later := t0.Add(2*gov.lenderTimeout("c") - time.Minute)
		w.readAt = later
		if rel, _ := cycleOf(gov, later, w, govModel("x", 4, 8)); len(rel) != 0 {
			t.Errorf("released %+v while the lender still waits for what another borrower took", rel)
		}
		stopped := w
		stopped.l.Waiting = false
		cycleOf(gov, later.Add(30*time.Second), stopped, govModel("x", 4, 8))
		if gov.lenderRel[key] != nil {
			t.Error("the taken release outlived the lender's wait")
		}
	})

	// PR #203 re-check (minor 1): a taken release, restored across restarts
	// as it may be, must not starve the lender for good.
	t.Run("a taken release ends after twice the timeout, and one new release may follow", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		gov.Restore(BorrowLedger{LenderReleases: []LenderRelease{{LenderKey: key, GPUs: 16, BorrowedBefore: 48, ReleasedAt: t0,
			Taken: true}}})
		w := relWorld{g: 16, used: 48, l: waitingLender(8, 32), release: true}
		at := t0.Add(2*gov.lenderTimeout("c") - time.Minute)
		if rel, _ := cycleOf(gov, at, w, govModel("x", 6, 8)); len(rel) != 0 {
			t.Fatalf("released %+v within twice the timeout of a taken release", rel)
		}
		at = t0.Add(2*gov.lenderTimeout("c") + time.Minute)
		if rel, _ := cycleOf(gov, at, w, govModel("x", 6, 8)); len(rel) != 1 {
			t.Errorf("released %+v past twice the timeout, want one new release", rel)
		}
	})

	t.Run("a pair half landed at the timeout keeps its pins until it lands", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		w := relWorld{g: 0, used: 48, l: waitingLender(24, 48), release: true}
		pd := func(p, d int) ModelScalingRequest {
			return pdFleet(pdRole{"p", "prefill", p}, pdRole{"d", "decode", d})
		}
		if _, ds := cycleOf(gov, t0, w, pd(2, 4)); lowered(ds)["p"] != 1 || lowered(ds)["d"] != 2 {
			t.Fatalf("lowered %v, want one 1P:2D pair", lowered(ds))
		}
		late := t0.Add(gov.lenderTimeout("c") + time.Minute)
		w.readAt = late
		w.borrowed = 48
		_, ds := cycleOf(gov, late, w, pd(1, 4)) // prefill gone, decodes still scaling down
		if lowered(ds)["d"] != 2 {
			t.Errorf("lowered %v past the timeout, want the decodes still pinned at 2: no prefill without its decodes", lowered(ds))
		}
		gone := t0.Add(2*gov.lenderTimeout("c") + time.Minute)
		w.readAt = gone
		if _, ds := cycleOf(gov, gone, w, pd(1, 4)); lowered(ds)["d"] != 0 {
			t.Errorf("lowered %v past twice the timeout, want the pins gone", lowered(ds))
		}
		// PR #203 re-check (minor 2): the pair is left half lowered; say so.
		if got := gov.LenderReport().HalfLowered; len(got) != 1 || got[0] != "b/pd" {
			t.Errorf("half lowered %v, want b/pd reported", got)
		}
	})

	t.Run("a restored release naming a deleted variant clears", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		gov.Restore(BorrowLedger{LenderReleases: []LenderRelease{{LenderKey: key, GPUs: 8, BorrowedBefore: 32, ReleasedAt: t0,
			Units: []ReleasedUnit{{Namespace: "b", Model: "gone", Variant: "gone-v", To: 0}}}}})
		rel, ds := cycleOf(gov, t0.Add(time.Minute), relWorld{g: 16, used: 48, l: waitingLender(8, 32), release: true}, govModel("x", 6, 8))
		if len(rel) != 1 || lowered(ds)["x-v"] != 1 {
			t.Errorf("released %+v, want x handed to the lender: the restored release named a variant that is gone", rel)
		}
	})

	t.Run("a restored pin is never below minReplicas", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		gov.Restore(BorrowLedger{LenderReleases: []LenderRelease{{LenderKey: key, GPUs: 32, BorrowedBefore: 32, ReleasedAt: t0,
			Units: []ReleasedUnit{{Namespace: "b", Model: "x", Variant: "x-v", To: 2}}}}})
		_, ds := cycleOf(gov, t0.Add(time.Minute), relWorld{g: 16, used: 48, l: waitingLender(8, 32), release: true},
			withMin(govModel("x", 6, 8), 4))
		if got := ds[0].TargetReplicas; got != 4 {
			t.Errorf("x's target is %d, want 4: the restored pin of 2 clamped to minReplicas", got)
		}
	})

	t.Run("duplicate and out-of-bounds restored entries are dropped and counted", func(t *testing.T) {
		good := LenderRelease{LenderKey: key, GPUs: 200000, ReleasedAt: t0, Units: []ReleasedUnit{{Namespace: "b", Variant: "x-v", To: 4}}}
		dup := good
		huge := good
		huge.Lender, huge.GPUs = "other", 2*maxLedgerGPUs
		taken := LenderRelease{LenderKey: LenderKey{Cohort: "c", Lender: "taken", Flavor: "h100", Resource: "nvidia.com/gpu"},
			GPUs: 8, ReleasedAt: t0, Taken: true}
		gov := NewBorrowGovernor(settings)
		if dropped := gov.Restore(BorrowLedger{LenderReleases: []LenderRelease{good, dup, huge, taken}}); dropped != 2 {
			t.Errorf("dropped %d, want the duplicate and the 2M-GPU entry", dropped)
		}
		if got := gov.Ledger().LenderReleases; len(got) != 2 {
			t.Errorf("kept %+v, want the 200000-GPU entry and the taken one", got)
		}
	})

	t.Run("HoldReleases drops a release that landed and keeps one that has not", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		cycleOf(gov, t0, relWorld{g: 16, used: 48, l: waitingLender(16, 32), release: true}, govModel("x", 6, 8))
		ds := decisionsOf([]ModelScalingRequest{govModel("x", 6, 8)})
		gov.HoldReleases(t0.Add(time.Minute), []ModelScalingRequest{govModel("x", 6, 8)}, ds)
		if ds[0].TargetReplicas != 4 {
			t.Fatalf("x's target is %d in a cycle that counts no borrowing, want the pin at 4", ds[0].TargetReplicas)
		}
		ds = decisionsOf([]ModelScalingRequest{govModel("x", 4, 8)})
		ds[0].TargetReplicas = 6 // the demand floor raises it again
		gov.HoldReleases(t0.Add(2*time.Minute), []ModelScalingRequest{govModel("x", 4, 8)}, ds)
		if ds[0].TargetReplicas != 6 || gov.lenderRel[key] != nil {
			t.Errorf("x's target is %d after the release landed, want the pin gone", ds[0].TargetReplicas)
		}
	})
}

// PR #203 re-check: what Kueue may still do for the lender itself, and what
// the cycle frees that never reaches it.
func TestLenderReleaseRecheck(t *testing.T) {
	t0 := time.Unix(50000, 0)
	settings := DefaultBorrowSettings()
	p := func(n int32) *int32 { return &n }

	// MAJOR (rest of KC4): an outside queue borrows 16 at priority 10, our
	// model 16 at 200, the lender's head is 16 GPUs at 100. Kueue preempts
	// the outside 16; releasing ours too would hand back 32.
	t.Run("a borrower outside this controller below the head is left to Kueue's preemption first", func(t *testing.T) {
		l := waitingLender(16, 32)
		l.Reclaim, l.PendingPriority, l.PendingSince = kueue.ReclaimLowerPriority, p(100), t0.Add(-10*time.Second)
		w := relWorld{g: 0, used: 16, borrowed: 32, l: l, release: true, borrowerPriority: p(200),
			classPriority: map[string]int32{"top": 200}}
		w.borrowerLowest = p(10)
		gov := NewBorrowGovernor(settings)
		cycleOf(gov, t0, w, withClass(govModel("m", 2, 8), "top"))
		if rel, ds := cycleOf(gov, t0.Add(20*time.Second), w, withClass(govModel("m", 2, 8), "top")); len(rel) != 0 {
			t.Fatalf("released %+v (lowered %v) while Kueue may still preempt the outside borrower at 10", rel, lowered(ds))
		}
		// Past the window the lender still waits and Kueue's figure stands:
		// a fresh read decides, and the release goes.
		late := t0.Add(lenderPreemptWindow + time.Minute)
		if rel, _ := cycleOf(gov, late, w, withClass(govModel("m", 2, 8), "top")); len(rel) != 1 {
			t.Errorf("released %+v past the preemption window, want m handed to the lender", rel)
		}
		// Nobody borrows below the head: no wait.
		w.borrowerLowest = p(150)
		gov = NewBorrowGovernor(settings)
		cycleOf(gov, t0, w, withClass(govModel("m", 2, 8), "top"))
		if rel, _ := cycleOf(gov, t0.Add(20*time.Second), w, withClass(govModel("m", 2, 8), "top")); len(rel) != 1 {
			t.Errorf("released %+v, want m at once: every borrower outranks the head", rel)
		}
	})

	// Re-check follow-up: Kueue evicts a preempted Workload at once but keeps
	// its quota until its pods stop. A vLLM pod with a 300s grace still shows
	// in Kueue's borrowed figure at 120s; a release then would give the same
	// GPUs twice.
	t.Run("a preempted outside Workload still terminating holds the release, however long its grace", func(t *testing.T) {
		l := waitingLender(16, 32)
		l.Reclaim, l.PendingPriority, l.PendingSince = kueue.ReclaimLowerPriority, p(100), t0.Add(-10*time.Second)
		w := relWorld{g: 0, used: 16, borrowed: 32, l: l, release: true, borrowerPriority: p(200),
			classPriority: map[string]int32{"top": 200}, borrowerLowest: p(10), evictionsRead: true, evicting: 16}
		gov := NewBorrowGovernor(settings)
		cycleOf(gov, t0, w, withClass(govModel("m", 2, 8), "top"))
		for _, at := range []time.Duration{2 * time.Minute, 5 * time.Minute} {
			if rel, _ := cycleOf(gov, t0.Add(at), w, withClass(govModel("m", 2, 8), "top")); len(rel) != 0 {
				t.Fatalf("released %+v at %v while the outside victim at 10 still terminates", rel, at)
			}
		}
		if b, ok := blockedOf(gov, "lender"); !ok || b.Reason != LenderPreempting || b.Givable != 16 {
			t.Errorf("report %+v, want the lender reported %s, 16 GPUs stopping", gov.LenderReport(), LenderPreempting)
		}
		// With the Workloads read and no victim, one admission pass is the wait.
		w.evicting = 0
		gov = NewBorrowGovernor(settings)
		cycleOf(gov, t0, w, withClass(govModel("m", 2, 8), "top"))
		if rel, _ := cycleOf(gov, t0.Add(kueueSchedulingInterval), w, withClass(govModel("m", 2, 8), "top")); len(rel) != 1 {
			t.Errorf("released %+v, want m handed back once no victim terminates and an admission pass went by", rel)
		}
	})

	// Minor 3: what a namespace frees within what it keeps never reaches
	// the cohort's pool.
	t.Run("what the cycle frees counts only up to what the namespace borrows", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		w := relWorld{g: 40, used: 48, l: waitingLender(16, 32), release: true, readAt: t0}
		c := BorrowCycle{Now: t0, Constraints: w.constraints(), Requests: []ModelScalingRequest{govModel("x", 6, 8)}}
		gov.Observe(c)
		ds := decisionsOf(c.Requests)
		ds[0].TargetReplicas = 3 // a demand fall frees 24, of which 8 were borrowed
		gov.Releases(c, ds)
		if b, ok := blockedOf(gov, "lender"); !ok || b.Need != 8 {
			t.Errorf("report %+v, want the lender still lacking 8: only the 8 borrowed reach the pool", gov.LenderReport())
		}
	})

	// Minor 5: a wake's reclaim yield frees GPUs for this controller's own
	// reclaim, not for the lender.
	t.Run("what a reclaim yield lowers does not cover the lender", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		w := relWorld{g: 16, used: 48, l: waitingLender(8, 32), release: true, readAt: t0}
		c := BorrowCycle{Now: t0, Constraints: w.constraints(), Requests: []ModelScalingRequest{govModel("x", 6, 8)}}
		gov.Observe(c)
		ds := decisionsOf(c.Requests)
		ds[0].TargetReplicas = 5
		ds[0].DecisionSteps = []domain.DecisionStep{{Name: ReclaimYieldStepName, TargetReplicas: 5}}
		gov.Releases(c, ds)
		if got := ds[0].TargetReplicas; got != 4 {
			t.Errorf("x's target is %d, want 4: the yield's replica goes to the wake, the lender gets one more", got)
		}
	})
}

// P/D models give whole pairs, and never a whole model for a smaller need
// (WP1). Replicas are whole 8-GPU nodes unless a case says otherwise.
func TestLenderReleasePD(t *testing.T) {
	t0 := time.Unix(50000, 0)
	settings := DefaultBorrowSettings()
	on := func(pending int) relWorld {
		return relWorld{g: 0, used: 96, l: waitingLender(pending, 96), release: true}
	}
	pdMin := func(r ModelScalingRequest, mins map[string]int) ModelScalingRequest {
		for i := range r.VariantStates {
			if n, ok := mins[r.VariantStates[i].Role]; ok {
				r.VariantStates[i].MinReplicas = &n
			}
		}
		return r
	}

	t.Run("2P:4D gives one 1P:2D pair for 24 GPUs", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		_, ds := cycleOf(gov, t0, on(24), pdFleet(pdRole{"p", "prefill", 2}, pdRole{"d", "decode", 4}))
		if got := lowered(ds); got["p"] != 1 || got["d"] != 2 {
			t.Errorf("lowered %v, want one pair at 1P:2D", got)
		}
	})

	t.Run("2P:3D, whose unit is the whole model, is not cut for an 8-GPU lender", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		_, ds := cycleOf(gov, t0, on(8), pdFleet(pdRole{"p", "prefill", 2}, pdRole{"d", "decode", 3}))
		if got := lowered(ds); len(got) != 0 {
			t.Errorf("lowered %v, want nothing: 40 GPUs for an 8-GPU need", got)
		}
		if b, ok := blockedOf(gov, "lender"); !ok || b.Reason != LenderUnservable {
			t.Errorf("report %+v, want the lender reported unservable", gov.LenderReport())
		}
	})

	t.Run("a smaller model takes the need when the pair would overshoot", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		pd := pdFleet(pdRole{"p", "prefill", 2}, pdRole{"d", "decode", 3})
		pd.Priority = 0 // released first by scaling priority
		_, ds := cycleOf(gov, t0, on(8), pd, govModel("x", 2, 8))
		if got := lowered(ds); got["p"] != 0 || got["d"] != 0 || got["x-v"] != 1 {
			t.Errorf("lowered %v, want x by 1 and the pair left whole", got)
		}
	})

	t.Run("1P:2D at its floor gives nothing and is reported", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		pd := pdMin(pdFleet(pdRole{"p", "prefill", 1}, pdRole{"d", "decode", 2}), map[string]int{"prefill": 1, "decode": 2})
		_, ds := cycleOf(gov, t0, on(24), pd)
		if got := lowered(ds); len(got) != 0 {
			t.Errorf("lowered %v, want nothing below minReplicas", got)
		}
		if b, ok := blockedOf(gov, "lender"); !ok || b.Givable != 0 {
			t.Errorf("report %+v, want the lender unservable with nothing givable", b)
		}
	})

	t.Run("a 16-GPU LeaderWorkerSet P/D pair goes for a 16-GPU need, not for an 8-GPU one", func(t *testing.T) {
		lws := func() ModelScalingRequest {
			r := pdFleet(pdRole{"p", "prefill", 2}, pdRole{"d", "decode", 2})
			for i := range r.VariantStates {
				r.VariantStates[i].GPUsPerReplica = 16
			}
			return r
		}
		gov := NewBorrowGovernor(settings)
		_, ds := cycleOf(gov, t0, relWorld{g: 0, used: 64, l: waitingLender(16, 64), release: true}, lws())
		if got := lowered(ds); got["p"] != 1 || got["d"] != 1 {
			t.Errorf("lowered %v, want one 1P:1D pair (32 GPUs, one 16-GPU group past 16)", got)
		}
		gov = NewBorrowGovernor(settings)
		_, ds = cycleOf(gov, t0, relWorld{g: 0, used: 64, l: waitingLender(8, 64), release: true}, lws())
		if got := lowered(ds); len(got) != 0 {
			t.Errorf("lowered %v, want nothing: a 32-GPU pair for 8 passes the need by more than a group", got)
		}
	})
}

func TestLenderReleaseInFlight(t *testing.T) {
	t0 := time.Unix(50000, 0)
	settings := DefaultBorrowSettings()
	w := relWorld{g: 16, used: 48, l: waitingLender(16, 32), release: true}

	t.Run("one in flight per lender until it lands, then a fresh read", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		// x (6) and y (2), 64 GPUs at guaranteed 16: 48 borrowed.
		w := w
		w.used = 64
		if _, ds := cycleOf(gov, t0, w, govModel("x", 6, 8), govModel("y", 2, 8)); lowered(ds)["x-v"] != 2 || lowered(ds)["y-v"] != 0 {
			t.Fatalf("first release lowered %v, want x by 2", lowered(ds))
		}
		// Still waiting, nothing landed: no second release -- not from y
		// either -- and the pin holds over a decision the demand floor raised
		// again.
		rel, ds := cycleOf(gov, t0.Add(time.Minute), w, govModel("x", 6, 8), govModel("y", 2, 8))
		if len(rel) != 0 || lowered(ds)["x-v"] != 2 || lowered(ds)["y-v"] != 0 {
			t.Fatalf("second cycle released %+v, lowered %v; want nothing new and the pin at 4", rel, lowered(ds))
		}
		if !gov.VariantStatus("b", "x-v").LenderRelease {
			t.Error("VariantStatus does not report the release in flight")
		}
		// Landed: x holds 4. The read in this cycle predates the landing.
		landed := t0.Add(2 * time.Minute)
		w4 := w
		w4.used, w4.borrowed = 48, 48 // Kueue's figure lags the landing
		w4.readAt = landed.Add(-time.Second)
		if rel, _ := cycleOf(gov, landed, w4, govModel("x", 4, 8), govModel("y", 2, 8)); len(rel) != 0 {
			t.Fatalf("released %+v in the cycle it landed, before Kueue took it in", rel)
		}
		// A fresh read after the landing, Kueue's borrowed figure down by the
		// 16 handed back, the lender waiting for its next Workload: the next.
		w4.borrowed = 0
		w4.readAt = landed.Add(20 * time.Second)
		rel, ds = cycleOf(gov, landed.Add(30*time.Second), w4, govModel("x", 4, 8), govModel("y", 2, 8))
		if len(rel) != 1 || lowered(ds)["x-v"] != 2 {
			t.Errorf("after landing and a fresh read: released %+v lowered %v, want x 4->2", rel, lowered(ds))
		}
	})

	t.Run("one that never lands times out", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		cycleOf(gov, t0, w, govModel("x", 6, 8))
		late := t0.Add(gov.lenderTimeout("c") + time.Minute)
		rel, _ := cycleOf(gov, late, w, govModel("x", 6, 8))
		if len(rel) != 1 {
			t.Errorf("past the release timeout released %+v, want a new release", rel)
		}
	})

	t.Run("a lender gone from a fresh read takes its pins with it", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		cycleOf(gov, t0, w, govModel("x", 6, 8))
		gone := w
		gone.l = kueue.LenderState{Queue: "other", Reclaim: kueue.ReclaimAny}
		_, ds := cycleOf(gov, t0.Add(time.Minute), gone, govModel("x", 6, 8))
		if lowered(ds)["x-v"] != 0 {
			t.Errorf("lowered %v after the lender left the cohort", lowered(ds))
		}
	})
}

func TestLenderReleaseLedger(t *testing.T) {
	t0 := time.Unix(50000, 0)
	settings := DefaultBorrowSettings()
	w := relWorld{g: 16, used: 48, l: waitingLender(16, 32), release: true}

	t.Run("a restart neither releases again nor drops the pin", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		cycleOf(gov, t0, w, govModel("x", 6, 8))
		raw, err := gov.MarshalLedger()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(raw, `"version":1`) || !strings.Contains(raw, `"lenderReleases"`) {
			t.Fatalf("ledger %s carries no versioned release", raw)
		}
		l, err := UnmarshalBorrowLedger(raw)
		if err != nil {
			t.Fatal(err)
		}
		next := NewBorrowGovernor(settings)
		if dropped := next.Restore(l); dropped != 0 {
			t.Fatalf("dropped %d of what this controller wrote", dropped)
		}
		rel, ds := cycleOf(next, t0.Add(time.Minute), w, govModel("x", 6, 8))
		if len(rel) != 0 || lowered(ds)["x-v"] != 2 {
			t.Errorf("after the restart released %+v, lowered %v; want nothing new, x pinned at 4", rel, lowered(ds))
		}
	})

	t.Run("nothing is released while the ledger is unread", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		c := BorrowCycle{Now: t0, Constraints: w.constraints(), Requests: []ModelScalingRequest{govModel("x", 6, 8)}, LedgerUnread: true}
		c.Constraints[0].SharedPools[0].ReadAt = t0
		gov.Observe(c)
		if rel := gov.Releases(c, decisionsOf(c.Requests)); len(rel) != 0 {
			t.Errorf("released %+v before the persisted ledger was read", rel)
		}
	})

	t.Run("a newer version is refused; corrupt entries are dropped", func(t *testing.T) {
		if _, err := UnmarshalBorrowLedger(`{"version":2,"lenderReleases":[]}`); !errors.Is(err, ErrBorrowLedgerVersion) {
			t.Errorf("version 2: err %v, want ErrBorrowLedgerVersion", err)
		}
		if l, err := UnmarshalBorrowLedger(`{"timeoutHeld":{"b/x-v":3}}`); err != nil || l.TimeoutHeld["b/x-v"] != 3 {
			t.Errorf("a ledger written before the version field: %+v %v, want read as version 1", l, err)
		}
		good := LenderRelease{LenderKey: LenderKey{Cohort: "c", Lender: "l", Flavor: "h100", Resource: "nvidia.com/gpu"},
			Units: []ReleasedUnit{{Namespace: "b", Variant: "x-v", To: 4}}, GPUs: 16, ReleasedAt: t0}
		noLender, negative, noUnits := good, good, good
		noLender.Lender = "other-but-empty-cohort"
		noLender.Cohort = ""
		negative.Lender = "l2"
		negative.Units = []ReleasedUnit{{Namespace: "b", Variant: "x-v", To: -1}}
		noUnits.Lender = "l3"
		noUnits.Units = nil
		gov := NewBorrowGovernor(settings)
		if dropped := gov.Restore(BorrowLedger{LenderReleases: []LenderRelease{good, noLender, negative, noUnits}}); dropped != 3 {
			t.Errorf("dropped %d, want the 3 entries this controller cannot have written", dropped)
		}
		if got := gov.Ledger().LenderReleases; len(got) != 1 || got[0].Lender != "l" {
			t.Errorf("kept %+v, want the good entry alone", got)
		}
		b, _ := json.Marshal(gov.Ledger())
		if !strings.Contains(string(b), `"version":1`) {
			t.Errorf("restored ledger %s is not versioned", b)
		}
	})
}

// A release never gives back what another release of the cycle, or the
// reclaim yield for a wake, already took; what they free counts toward the
// lender's need (WP2).
func TestLenderReleaseNoDoubleRelease(t *testing.T) {
	t0 := time.Unix(50000, 0)
	settings := DefaultBorrowSettings()

	t.Run("a replica the wake's reclaim yield gave back is not given again", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		w := relWorld{g: 40, used: 48, l: waitingLender(16, 32), release: true, readAt: t0}
		c := BorrowCycle{Now: t0, Constraints: w.constraints(), Requests: []ModelScalingRequest{govModel("x", 6, 8)}}
		gov.Observe(c)
		ds := decisionsOf(c.Requests)
		ds[0].TargetReplicas = 5 // the pass yielded one replica to the wake's reclaim
		if rel := gov.Releases(c, ds); len(rel) != 0 || ds[0].TargetReplicas != 5 {
			t.Errorf("released %+v (target %d) after the yield took the only borrowed replica; want nothing more",
				rel, ds[0].TargetReplicas)
		}
	})

	t.Run("what a kueue-gate or placement-timeout pin frees covers the lender first", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		gov.Restore(BorrowLedger{TimeoutHeld: map[string]int{"b/x-v": 5}})
		w := relWorld{g: 0, used: 96, l: waitingLender(8, 96), release: true}
		_, ds := cycleOf(gov, t0, w, govModel("x", 6, 8), govModel("y", 6, 8))
		if got := lowered(ds); got["x-v"] != 1 || got["y-v"] != 0 {
			t.Errorf("lowered %v, want x by its pin alone (1): the 8 it frees are the lender's 8", got)
		}
	})

	t.Run("a model released to a lender is not timed out in the same flight", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		w := relWorld{g: 16, used: 48, l: waitingLender(8, 32), release: true}
		cycleOf(gov, t0, w, govModel("x", 6, 8))
		stuck := unplacedVariant(govModel("x", 6, 8), "x-v", t0)
		rel, ds := cycleOf(gov, t0.Add(settings.PlacementTimeout+time.Minute), w, stuck)
		if len(rel) != 0 || lowered(ds)["x-v"] != 1 {
			t.Errorf("released %+v, lowered %v; want only the lender's pin (1), no placement-timeout release on top", rel, lowered(ds))
		}
	})

	t.Run("a placement-timeout release in the same cycle counts toward the lender's need", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		w := relWorld{g: 40, used: 56, l: waitingLender(8, 32), release: true}
		stuck := unplacedVariant(govModel("x", 7, 8), "x-v", t0)
		seenPast(gov, BorrowCycle{Constraints: w.constraints(), Requests: []ModelScalingRequest{stuck}}, t0)
		rel, ds := cycleOf(gov, t0.Add(settings.PlacementTimeout+time.Minute), w, stuck, govModel("y", 1, 8))
		causes := map[ReleaseCause]int{}
		for _, r := range rel {
			causes[r.Cause]++
		}
		if causes[ReleasePlacementTimeout] != 1 || lowered(ds)["x-v"] != 1 {
			t.Fatalf("releases %+v lowered %v, want x released once for the placement timeout", rel, lowered(ds))
		}
		if lowered(ds)["y-v"] != 0 || causes[ReleaseLenderWaiting] != 0 {
			t.Errorf("lowered %v, want y kept: x's 8 already cover the lender's 8", lowered(ds))
		}
	})

	t.Run("terminating pods count toward the need", func(t *testing.T) {
		gov := NewBorrowGovernor(settings)
		x := govModel("x", 4, 8)
		x.VariantStates[0].HeldKnown, x.VariantStates[0].HeldReplicas = true, 6
		_, ds := cycleOf(gov, t0, relWorld{g: 16, used: 48, l: waitingLender(16, 32), release: true}, x)
		if got := lowered(ds)["x-v"]; got != 0 {
			t.Errorf("lowered x by %d, want 0: its two terminating replicas free the lender's 16", got)
		}
	})
}
