package warmpool

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/policy"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/pool"
)

// A borrow that RECORDS ITSELF must still expire.
//
// Every other hold-timeout test seeds r.borrowedAt by hand and then asserts the
// return, which proves the policy and the plumbing below it. What none of them
// covers is the key: recordBorrow writes under the variant of the ACTION, and
// the timeout looks up under the variant of the MEMBERSHIP read back on the
// next pass. If those two ever disagree the lookup misses, borrowedAt falls
// back to Now, the age is recomputed as zero on every pass, and the hold
// timeout can never fire.
//
// That failure is silent by construction. `lent` in the state line is counted
// from memberships (lentPods), not from borrowedAt, so a pool with a missing
// borrow record still reports lent=1 and looks entirely healthy -- it just
// never hands the Pod back, which is the one thing the timeout exists to do.
//
// So this drives a real borrow, lets it record itself, reads the Pod back the
// way a cluster would, advances past MaxHold, and asks for the return.
func TestABorrowRecordsItselfAndThenExpires(t *testing.T) {
	cfg := testConfig()
	cfg.Retained = false
	cfg.MaxHold = time.Minute

	p := &fakePool{memberships: []pool.Membership{
		{Model: model("qwen"), Pod: podA(), State: pool.Asleep},
	}}
	// Short, and stays short: the variant never stops wanting the Pod, so the
	// timeout is the only thing that can justify a return.
	d := &staticDemand{variants: []policy.VariantDemand{
		{Model: model("qwen"), Desired: 2, Ready: 1},
	}}
	r := New(p, d, cfg)
	base := time.Now()
	r.now = func() time.Time { return base }

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if !p.did("activate qwen@a") {
		t.Fatalf("no borrow happened, so there is nothing to expire: %v", p.seen())
	}
	if len(r.Lent()) != 1 {
		t.Fatalf("the borrow did not record itself, so its age is zero forever: %v", r.Lent())
	}

	// What the next pass reads from the cluster: the Pod is now serving it.
	p.mu.Lock()
	p.memberships = []pool.Membership{{Model: model("qwen"), Pod: podA(), State: pool.Serving}}
	p.mu.Unlock()

	base = base.Add(2 * cfg.MaxHold)

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !p.did("deactivate qwen@a") {
		t.Fatalf("a borrow past MaxHold was never returned, so the bridge safety net "+
			"does not exist for a borrow the reconciler recorded itself: %v", p.seen())
	}
}
