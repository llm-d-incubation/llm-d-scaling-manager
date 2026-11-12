package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Learned-state rules a mutation review found no test reaching: each test
// below fails with the rule it names taken out.

// A variant learned live with no configuration known, while restored entries
// for it are still pending, is exported under the pending entries'
// fingerprint, so its live entries are left out (dropVariant): under that
// fingerprint they would read as checked. A put-aside ITL baseline is one of
// them: kept, it came back after the next restart as the baseline of
// arguments it was never checked against.
func TestExportDropsAPutAsideITLBaselineLearnedWithNoConfigurationKnown(t *testing.T) {
	params := &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: 256}
	states := []domain.VariantReplicaState{
		{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	analyze := func(a *SaturationAnalyzer, tokens int64, queue int) {
		t.Helper()
		if _, err := a.Analyze(context.Background(), makeAnalyzerInput(
			[]domain.ReplicaMetrics{makeReplicaMetrics("pod-a", "variant-a", tokens, 16000, queue, 100, 50)}, states)); err != nil {
			t.Fatal(err)
		}
	}
	withParams := func() *SaturationAnalyzer {
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{EngineParams: params}, time.Now())
		return NewSaturationAnalyzer(store)
	}
	// stash is a baseline put aside under arguments the variant ran before,
	// filed as mu_from_itl.go files it under the variant's itlWindowKey.
	stash := stashKey("test-ns|test-model|variant-a|H100|1", "older-args")
	putAside := func(a *SaturationAnalyzer) {
		a.mu.Lock()
		a.itlBaselineStash[stash] = 0.03
		a.mu.Unlock()
	}

	first := withParams()
	for range 2 {
		analyze(first, 4000, 6)
	}
	saved := first.ExportLearnedState()
	if len(saved.K2) == 0 {
		t.Fatal("precondition: the first process learned k2")
	}
	if _, ok := saved.ITLBaselineStash[stash]; ok {
		t.Fatal("precondition: the stored document holds no put-aside baseline under the live key")
	}

	known := withParams()
	putAside(known)
	if _, ok := known.ExportLearnedState().ITLBaselineStash[stash]; !ok {
		t.Fatal("precondition: with its configuration known, a put-aside baseline is exported")
	}

	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-a", capacity.Record{}, time.Now()) // scale target not read yet
	second := NewSaturationAnalyzer(store)
	second.RestoreLearnedState(saved)
	analyze(second, 9000, 6)
	putAside(second)
	second.mu.Lock()
	_, pending := second.pendingRestore["test-ns/variant-a"]
	second.mu.Unlock()
	if !pending {
		t.Fatal("precondition: the restored entries are still pending")
	}

	out := second.ExportLearnedState()
	if len(out.K2) == 0 {
		t.Fatal("precondition: the pending entries are exported")
	}
	if b, ok := out.ITLBaselineStash[stash]; ok {
		t.Errorf("a put-aside ITL baseline learned with no configuration known went out under the pending fingerprint: %v", b)
	}
}

// A previous figure that comes back unwritten after a rollback and a re-roll
// keeps a bound's clock only when the bound ran on the variant's own figure:
// that one has run its course on this very window. A bound that ran on a
// sibling's figure has not, so the variant's own figure arriving restarts it
// with its whole hold -- live and in the exported document alike.
func TestASameWindowReRollRestartsABoundThatRanOnASiblingsFigure(t *testing.T) {
	type run struct {
		since        time.Time
		armed        bool
		rearmPending bool
	}
	setup := func(own bool) (*SaturationAnalyzer, string, *capacity.RollingAverage, time.Time, *time.Time) {
		a, _, clock := waProcess(mtParams(64, "vllm:v1"))
		key := rrKey(a, "variant-a")
		w := rrWindow(3000)
		start := *clock
		donor := "variant-b"
		if own {
			donor = "variant-a"
		}
		a.mu.Lock()
		a.lowerBounds[key] = &lowerBound{
			armed: true, since: start, tick: start, seen: start,
			donor: donor, own: own, donorK2: 4000,
			args: mtParams(64, "vllm:v1").ArgsFingerprint(), argsVersion: capacity.FingerprintVersion,
			figureWritten: w.LastWritten(),
		}
		a.mu.Unlock()
		*clock = start.Add(time.Hour)
		return a, key, w, start, clock
	}
	live := func(own bool) (run, time.Time, time.Time) {
		a, key, w, start, clock := setup(own)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.notePreviousFigureLocked(key, w)
		lb := a.lowerBounds[key]
		return run{since: lb.since, armed: lb.armed, rearmPending: lb.rearmPending}, start, *clock
	}
	exported := func(own bool) (run, time.Time) {
		a, key, w, start, _ := setup(own)
		a.mu.Lock()
		a.computeCapacityHistory[key] = w
		a.k2WindowArgs[key] = windowArgs(mtParams(256, "vllm:v1")) // the arguments rolled back from
		a.mu.Unlock()
		lb, ok := a.ExportLearnedState().LowerBounds[key]
		if !ok {
			t.Fatalf("precondition: the bound is exported (own=%v)", own)
		}
		return run{since: lb.Since, armed: lb.Armed, rearmPending: lb.RearmPending}, start
	}

	t.Run("live", func(t *testing.T) {
		got, start, _ := live(true)
		if !got.since.Equal(start) || got.rearmPending {
			t.Fatalf("precondition: a bound on the variant's own figure keeps its clock, got %+v", got)
		}
		got, start, now := live(false)
		if got.since.Equal(start) || !got.since.Equal(now) || !got.rearmPending {
			t.Errorf("a bound that ran on a sibling's figure kept its clock on the variant's own figure: %+v, started %v", got, start)
		}
	})
	t.Run("exported", func(t *testing.T) {
		got, start := exported(true)
		if !got.armed || !got.since.Equal(start) {
			t.Fatalf("precondition: a bound on the variant's own figure is exported running, got %+v", got)
		}
		got, _ = exported(false)
		if got.armed || !got.since.IsZero() {
			t.Errorf("a bound that ran on a sibling's figure was exported running on the variant's own figure: %+v", got)
		}
	})
}

// A previous figure held in a bound filed under another GPU count on the same
// accelerator is never looked up, so the restart line does not count it as
// restored -- as for another accelerator
// (TestABoundFigureOnOtherHardwareIsNotCountedAsRestored).
func TestABoundFigureOnAnotherGPUCountIsNotCountedAsRestored(t *testing.T) {
	first, store, clock := waProcess(mtParams(256, "vllm:v1"))
	waRun(t, first, clock, 4000, 6)
	waSetParams(store, mtParams(64, "vllm:v1"))
	waRun(t, first, clock, 1000, 0)
	doc := first.ExportLearnedState()

	count := func(gpus int) int {
		a, s, _ := waProcess(mtParams(128, "vllm:v1"))
		s.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EngineParams: mtParams(128, "vllm:v1"), AcceleratorName: "H100", GpuCount: gpus,
		}, time.Now())
		a.RestoreLearnedState(doc)
		r, done := a.installRestored("test-ns", "test-model", "variant-a")
		if !done {
			t.Fatalf("precondition: the variant's entries were pending (%d GPUs)", gpus)
		}
		return r.BoundPriors
	}
	if n := count(1); n != 1 {
		t.Fatalf("precondition: on the same GPU count the bound's figure is counted, got %d", n)
	}
	if n := count(2); n != 0 {
		t.Errorf("a bound filed under another GPU count was counted as restored: %d", n)
	}
}
