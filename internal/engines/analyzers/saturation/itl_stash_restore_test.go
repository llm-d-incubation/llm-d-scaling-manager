package saturation

import (
	"encoding/json"
	"testing"
	"time"
)

// The ITL baseline put aside for a rollback outlives a restart: only a full
// fit rewrites a baseline, which a fleet the router balances rarely spans, so
// lost, a rollback after a restart priced the engine that measured it at the
// default intercept.

func itlStashKeyAndArgs(a *SaturationAnalyzer) (key, argsA, argsB string) {
	return a.itlWindowKey("test-ns", "test-model", "variant-a", "H100", 1),
		mtParams(256, "vllm:v1").ArgsFingerprint(), mtParams(64, "vllm:v1").ArgsFingerprint()
}

// Put aside before the restart, back after it.
func TestAPutAsideITLBaselineSurvivesARestart(t *testing.T) {
	first, _, _ := waProcess(mtParams(64, "vllm:v1"))
	key, argsA, argsB := itlStashKeyAndArgs(first)
	first.itlUnderArgs(key, argsA)
	first.itlTracker.SetBaseline(key, 0.0012)
	first.itlUnderArgs(key, argsB)
	first.mu.Lock()
	first.variantSeenAt[key] = time.Now()
	first.mu.Unlock()
	raw, err := json.Marshal(first.ExportLearnedState())
	if err != nil {
		t.Fatal(err)
	}
	var doc LearnedState
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	second, _, _ := waProcess(mtParams(64, "vllm:v1"))
	second.RestoreLearnedState(doc)
	if _, done := second.installRestored("test-ns", "test-model", "variant-a"); !done {
		t.Fatal("precondition: the variant's entries were pending")
	}
	second.itlUnderArgs(key, argsB)
	second.itlUnderArgs(key, argsA)
	if b, ok := second.itlBaselines()[key]; !ok || b != 0.0012 {
		t.Errorf("the rollback after a restart lost the put-aside baseline: %v %v", b, ok)
	}
}

// A restore under other arguments puts the document's baseline aside under
// the arguments it was learned under, rather than dropping it.
func TestARestoreUnderOtherArgumentsPutsTheBaselineAside(t *testing.T) {
	a, _, _ := waProcess(mtParams(64, "vllm:v1"))
	key, argsA, argsB := itlStashKeyAndArgs(a)
	a.RestoreLearnedState(LearnedState{
		Config:        map[string]string{"test-ns/variant-a": mtParams(256, "vllm:v1").Fingerprint()},
		Args:          map[string]string{"test-ns/variant-a": argsA},
		VariantSeenAt: map[string]time.Time{key: time.Now()},
		ITLBaseline:   map[string]float64{key: 0.0012},
	})
	r, done := a.installRestored("test-ns", "test-model", "variant-a")
	if !done || r.Outcome != RestoredPrior {
		t.Fatalf("precondition: a prior restore, got %+v done=%v", r, done)
	}
	if b, ok := a.itlBaselines()[key]; ok {
		t.Fatalf("precondition: the baseline of other arguments is not the variant's own: %v", b)
	}
	a.itlUnderArgs(key, argsB)
	a.itlUnderArgs(key, argsA)
	if b, ok := a.itlBaselines()[key]; !ok || b != 0.0012 {
		t.Errorf("a rollback to the arguments the snapshot recorded did not get their baseline: %v %v", b, ok)
	}
}
