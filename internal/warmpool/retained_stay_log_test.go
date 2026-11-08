package warmpool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/policy"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/pool"
)

// A STANDING DECISION IS SAID ONCE, NOT ON EVERY PASS.
//
// "staying on its awake model" was emitted every pass, and a pass is not the
// Interval: passes run on the 5s tick OR whenever a decision lands, floored only
// by MinGap at 250ms. A busy trigger therefore drives about four a second.
// Measured on the shared GPU cluster: 3.2/s, 8527 identical lines in 45 minutes, which buried
// the switch and lend lines they were supposed to sit alongside.
//
// The line's own comment claimed "every five seconds", which is where the
// mistake came from -- so this pins the behaviour rather than the comment.
func TestARetainedPoolSaysAStandingDecisionOnce(t *testing.T) {
	cfg := testConfig()
	cfg.Retained = true

	spec := PoolSpec{
		Name:     "retained",
		Config:   cfg,
		Replicas: 1,
		Switch:   SwitchConfig{SpareThreshold: 0.2, MinInterval: time.Minute},
	}

	p := &fakePool{memberships: []pool.Membership{
		{Pod: podA(), State: pool.Absent, Pool: "retained"},
	}}
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{spec}
	// Known-but-comfortable: chooseAwake finds no candidate and returns the
	// staying decision, which is the steady state this test is about.
	r.Pressure = func(string, string) (decision.Pressure, bool) {
		return decision.Pressure{SpareFraction: 1, NeedsScaleUp: false}, true
	}

	var stay int
	sink := funcr.New(func(_, args string) {
		if strings.Contains(args, "staying on its awake model") {
			stay++
		}
	}, funcr.Options{Verbosity: 10})
	ctx := log.IntoContext(context.Background(), sink)

	for range 5 {
		r.awakeIntent(ctx, spec, p.memberships, nil)
	}

	if stay != 1 {
		t.Errorf("logged the same standing decision %d times over 5 passes, want 1: "+
			"an unchanged decision on the next pass says nothing new, and at ~4 passes "+
			"a second the repeats bury everything else", stay)
	}
}

// A CHANGED DECISION IS STILL SAID.
//
// Deduping must not turn into silence: the reason a retained pool is staying put
// is the thing an operator reads, and it changing is exactly the event worth
// seeing. Suppressing repeats and suppressing changes look identical in a log
// until the day someone needs the second one.
func TestARetainedPoolSaysTheDecisionAgainWhenItChanges(t *testing.T) {
	cfg := testConfig()
	cfg.Retained = true

	spec := PoolSpec{
		Name:     "retained",
		Config:   cfg,
		Replicas: 1,
		Switch:   SwitchConfig{SpareThreshold: 0.2, MinInterval: time.Minute},
	}

	// SERVING, not Absent: the reason can only change if there is an incumbent.
	// With nothing awake, an unknown reading and a comfortable one both come out
	// as "no candidate", so the two cases would be indistinguishable and the test
	// would pass without proving anything.
	awake := pool.ModelRef{Namespace: poolNamespace, Variant: "v1"}
	p := &fakePool{memberships: []pool.Membership{
		{Pod: podA(), State: pool.Serving, Model: awake, Pool: "retained"},
	}}
	variants := []policy.VariantDemand{{Model: awake}}

	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{spec}

	// First measured, then not: the reason flips from "no candidate" to
	// "unmeasured", which is a different answer and must be reported.
	known := true
	r.Pressure = func(string, string) (decision.Pressure, bool) {
		if known {
			return decision.Pressure{SpareFraction: 1}, true
		}
		return decision.Pressure{}, false
	}

	var stay int
	sink := funcr.New(func(_, args string) {
		if strings.Contains(args, "staying on its awake model") {
			stay++
		}
	}, funcr.Options{Verbosity: 10})
	ctx := log.IntoContext(context.Background(), sink)

	r.awakeIntent(ctx, spec, p.memberships, variants)
	r.awakeIntent(ctx, spec, p.memberships, variants)
	known = false
	r.awakeIntent(ctx, spec, p.memberships, variants)

	if stay != 2 {
		t.Errorf("logged %d times, want 2: once for the first decision and once when "+
			"it changed -- deduping repeats must not swallow a new answer", stay)
	}
}

// A CONFIGURATION MISTAKE IS SAID ONCE, NOT FOUR TIMES A SECOND.
//
// "can never admit a model" reports a pool whose reserve is its whole ceiling.
// That cannot fix itself, so repeating it says nothing new -- and it is at INFO,
// the level that actually ships, on a loop whose only floor is MinGap at 250ms.
// Measured on a cluster: ~1.2 lines a second, 280 in four minutes.
func TestAnInertPoolIsReportedOnce(t *testing.T) {
	cfg := testConfig()
	cfg.SleepMinSize = 2 // a reserve the ceiling below cannot exceed

	spec := PoolSpec{Name: "stuck", Config: cfg, Replicas: 1, MaxReplicas: 1}
	p := &fakePool{memberships: []pool.Membership{
		{Pod: podA(), State: pool.Absent, Pool: "stuck"},
	}}
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{spec}

	var inert, recovered int
	sink := funcr.New(func(_, args string) {
		switch {
		case strings.Contains(args, "can never admit a model"):
			inert++
		case strings.Contains(args, "can admit models again"):
			recovered++
		}
	}, funcr.Options{Verbosity: 10})
	ctx := log.IntoContext(context.Background(), sink)

	for range 5 {
		if _, err := r.Once(ctx); err != nil {
			t.Fatalf("Once: %v", err)
		}
	}
	if inert != 1 {
		t.Errorf("reported the same inert configuration %d times over 5 passes, want 1", inert)
	}

	// ...and when the operator fixes it, the silence is broken deliberately:
	// with the warning deduped, saying nothing would be indistinguishable from
	// the pass having stopped.
	r.Pools = fakePools{{Name: "stuck", Config: cfg, Replicas: 1, MaxReplicas: 4}}
	if _, err := r.Once(ctx); err != nil {
		t.Fatalf("Once after the fix: %v", err)
	}
	if recovered != 1 {
		t.Errorf("recovery announced %d times, want 1: an operator who raised the ceiling "+
			"has no other signal that it took", recovered)
	}
}
