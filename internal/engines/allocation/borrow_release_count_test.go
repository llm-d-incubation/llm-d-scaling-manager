package allocation

import (
	"reflect"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// PR-C: with kueue.releaseToNonReclaimingLenders the entry also counts what
// lenders Kueue does not reclaim for lend, for every model, and stops
// reporting lender-cannot-reclaim for it. Off, nothing changes.
func TestReleaseToNonReclaimingCounts(t *testing.T) {
	at := time.Unix(1000, 0)
	p := func(n int32) *int32 { return &n }
	lender := func(name, reclaim string, unused int, prio *int32) config.ExternalLender {
		return config.ExternalLender{Queue: name, Reclaim: reclaim, Unused: unused, Priority: prio}
	}
	on := config.KueueQuotaSource{Enabled: true, ReleaseToNonReclaimingLenders: true}
	off := config.KueueQuotaSource{Enabled: true}

	t.Run("off: an explicit false reads exactly as the field left out", func(t *testing.T) {
		for _, snap := range []config.ExternalQuotas{
			lenderSnapshot(at, lender("never-cq", kueue.ReclaimNever, 16, nil)),
			lenderSnapshot(at, lender("lp-cq", kueue.ReclaimLowerPriority, 16, p(500))),
			lenderSnapshot(at, lender("any-cq", kueue.ReclaimAny, 8, nil), lender("never-cq", kueue.ReclaimNever, 8, nil)),
		} {
			unset := constraintsOf(t, snap)
			explicit := constraintsOfEntry(t, snap, off)
			if !reflect.DeepEqual(unset, explicit) {
				t.Errorf("constraints differ with releaseToNonReclaimingLenders: false\nunset:    %+v\nexplicit: %+v", unset, explicit)
			}
			for _, ch := range unset.SharedPools {
				if ch.ReleaseToNonReclaiming {
					t.Errorf("chunk %s marked for release with the field off", ch.key())
				}
			}
		}
	})

	t.Run("on: two 8-GPU replicas a Never lender lends are counted, and no lender-cannot-reclaim", func(t *testing.T) {
		snap := lenderSnapshot(at, lender("never-cq", kueue.ReclaimNever, 16, nil))
		c := constraintsOfEntry(t, snap, on)
		if len(c.SharedPools) != 1 || c.SharedPools[0].Free != 16 || !c.SharedPools[0].ReleaseToNonReclaiming {
			t.Fatalf("pools = %+v, want one of 16 free, marked for release", c.SharedPools)
		}
		if why := c.BorrowWithheld["serve"]; why != "" {
			t.Errorf("withheld as %q, want counted: the capacity is counted now", why)
		}
		if b := c.NamespaceBorrow["serve"]["H100"]; b.Frozen || b.Withheld != "" {
			t.Errorf("serve's borrowing = %+v, want open", b)
		}
		// Off, the same read counts none of it.
		if free, _, why := borrowingOf(t, snap); free != 0 || why != string(kueue.ReasonLenderCannotReclaim) {
			t.Errorf("off: counted %d withheld %q, want 0 and lender-cannot-reclaim", free, why)
		}
	})

	t.Run("on: a LowerPriority lender's GPUs are drawable by every model, outranked or not", func(t *testing.T) {
		for _, prio := range []*int32{p(500), nil} {
			snap := lenderSnapshot(at, lender("lp-cq", kueue.ReclaimLowerPriority, 16, prio))
			snap.ClassPriority = map[string]int32{"kueue.x-k8s.io/serving": 1000}
			ch := &constraintsOfEntry(t, snap, on).SharedPools[0]
			if len(ch.Restricted) != 0 {
				t.Errorf("priority %v: restricted %+v, want none", prio, ch.Restricted)
			}
			if got := ch.drawable("kueue.x-k8s.io/serving", nil); got != 16 {
				t.Errorf("priority %v: serving at 1000 may draw %d, want 16", prio, got)
			}
			if got := ch.drawable("kueue.x-k8s.io/unseen", nil); got != 16 {
				t.Errorf("priority %v: an unread class may draw %d, want 16", prio, got)
			}
		}
	})

	t.Run("on: Any and Never lenders side by side both count", func(t *testing.T) {
		snap := lenderSnapshot(at, lender("any-cq", kueue.ReclaimAny, 8, nil), lender("never-cq", kueue.ReclaimNever, 8, nil))
		if free := constraintsOfEntry(t, snap, on).SharedPools[0].Free; free != 16 {
			t.Errorf("counted %d, want 16", free)
		}
	})
}

// Two entries over one cohort: the release holds only when both opt in, and
// the counted pool is the smaller one's.
func TestReleaseToNonReclaimingMergesCautiously(t *testing.T) {
	chunk := func(free int, release bool) *ResourceConstraints {
		c := borrowQuota(free, nsBorrow{ns: "b", cap: config.QuotaUnlimited})
		c.SharedPools[0].ReleaseToNonReclaiming = release
		return c
	}
	for _, tc := range []struct {
		a, b bool
		want bool
	}{{true, true, true}, {true, false, false}, {false, true, false}} {
		chunks, _ := mergeSharedPools([]*ResourceConstraints{chunk(16, tc.a), chunk(8, tc.b)})
		if len(chunks) != 1 || chunks[0].ReleaseToNonReclaiming != tc.want || chunks[0].Free != 8 {
			t.Errorf("merge %t+%t = %+v, want release %t at free 8", tc.a, tc.b, chunks, tc.want)
		}
	}
}
