package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Rules mutation testing found unpinned: each test fails with the line it
// names reverted.

// A forget acted on before a delayed restore leaves the stored bound spent
// and its own previous figure gone -- checked straight after the restore,
// before a read could clear the figure on its own, and with no sibling to
// lend, so only the variant's own figure could bring the bound back.
func TestAForgottenVariantsStoredBoundComesInSpentAndWithoutItsFigure(t *testing.T) {
	a, _, clock := waProcess(mtParams(64, "vllm:v1"))
	key := rrKey(a, "variant-a")
	a.ForgetVariant("test-ns", "test-model", "variant-a", "t1")
	a.RestoreLearnedState(LearnedState{LowerBounds: map[string]LowerBoundSnapshot{key: {
		Armed: true, Since: clock.Add(-10 * time.Minute), Tick: *clock, Seen: *clock,
		Donor: "variant-a", Own: true, DonorK2: 3000, Prior: 3000, PriorSince: *clock, PriorUsed: *clock,
		RearmPending: true, Args: mtParams(64, "vllm:v1").ArgsFingerprint(), ArgsVersion: capacity.FingerprintVersion,
	}}})
	a.mu.Lock()
	lb := a.lowerBounds[key]
	a.mu.Unlock()
	if lb == nil {
		t.Fatal("the stored bound was dropped, not ended")
	}
	if lb.prior != 0 || lb.rearmPending {
		t.Errorf("the forgotten variant's own figure came back with its bound: prior %d, rearmPending %v", lb.prior, lb.rearmPending)
	}
	if lb.tick.Sub(lb.since) < BorrowedK2Hold+BorrowedK2Relax {
		t.Errorf("the stored bound came in with %v of its hold and relaxation left", BorrowedK2Hold+BorrowedK2Relax-lb.tick.Sub(lb.since))
	}
}

// A live forget drops the variant's own previous figure along with ending the
// bound.
func TestAForgetDropsALiveBoundsPreviousFigure(t *testing.T) {
	a, _, clock := waProcess(mtParams(64, "vllm:v1"))
	key := rrKey(a, "variant-a")
	a.mu.Lock()
	a.lowerBounds[key] = &lowerBound{
		armed: true, since: *clock, tick: *clock, seen: *clock, own: true, donor: "variant-a", donorK2: 3000,
		prior: 3000, priorSince: *clock, priorUsed: *clock,
	}
	a.mu.Unlock()
	a.ForgetVariant("test-ns", "test-model", "variant-a", "t1")
	a.mu.Lock()
	defer a.mu.Unlock()
	if lb := a.lowerBounds[key]; lb == nil || lb.prior != 0 {
		t.Errorf("the forget left the variant's previous figure: %+v", lb)
	}
}

// A change of arguments is new evidence unless the figure is the variant's own
// and the bound last ran on its own: every other combination re-arms.
func TestNewArgsIsEvidenceUnlessBothFiguresAreTheVariantsOwn(t *testing.T) {
	for _, tc := range []struct {
		boundOwn, figureOwn, changed, want bool
	}{
		{false, false, true, true},
		{false, true, true, true},
		{true, false, true, true},
		{true, true, true, false},
		{false, false, false, false},
	} {
		lb := &lowerBound{args: "args-a", argsVersion: capacity.FingerprintVersion, own: tc.boundOwn}
		args := "args-a"
		if tc.changed {
			args = "args-b"
		}
		if got := lb.newArgs(args, tc.figureOwn); got != tc.want {
			t.Errorf("bound own %v, figure own %v, changed %v: newArgs %v, want %v", tc.boundOwn, tc.figureOwn, tc.changed, got, tc.want)
		}
	}
}

// A row completing requests with nothing resident or queued at the moment it
// was read is working, not idle: decode between requests, prefill on short
// prompts.
func TestARowCompletingRequestsIsNotIdle(t *testing.T) {
	rm := makeReplicaMetrics("pod-a", "variant-a", 0, 16000, 0, 100, 50)
	if admissionOf(rm).serving() {
		t.Fatal("precondition: a row with no work is idle")
	}
	rm.RequestRate = 1.5
	if !admissionOf(rm).serving() {
		t.Error("a row completing requests was taken as idle")
	}
}

// A whole restore keeps the ITL baseline through the first cycle: the key is
// seen for the first time after a restart, and adopts the arguments the
// variant runs rather than being taken as a change.
func TestAWholeRestoreKeepsTheITLBaselineThroughTheFirstCycle(t *testing.T) {
	a, _, _ := waProcess(mtParams(64, "vllm:v1"))
	key := a.itlWindowKey("test-ns", "test-model", "variant-a", "H100", 1)
	p := mtParams(64, "vllm:v1")
	a.RestoreLearnedState(LearnedState{
		Config:        map[string]string{"test-ns/variant-a": p.Fingerprint()},
		Args:          map[string]string{"test-ns/variant-a": p.ArgsFingerprint()},
		VariantSeenAt: map[string]time.Time{key: time.Now()},
		ITLBaseline:   map[string]float64{key: 0.0012},
	})
	in := makeAnalyzerInput([]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", 1000, 16000, 0, 100, 50)},
		[]domain.VariantReplicaState{{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode}})
	if _, err := a.Analyze(context.Background(), in); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if b, ok := a.itlTracker.Baseline(key); !ok || b != 0.0012 {
		t.Errorf("the restored baseline did not survive the first cycle: %v %v", b, ok)
	}
}
