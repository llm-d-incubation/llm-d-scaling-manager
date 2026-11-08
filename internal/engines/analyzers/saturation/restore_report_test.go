package saturation

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// What a restore reports it installed, which the restart's log line names.

// A restore whose arguments differ installs the old throughput windows too,
// to hold the demand floor hold-only. With no k2 window to keep it is not
// "not restored": the old service rate is still holding replicas, and the log
// line must say so.
func TestAPriorRestoreReportsTheServiceRateItInstalled(t *testing.T) {
	a, _, _ := waProcess(mtParams(64, "vllm:v1"))
	key := a.throughputKey("test-model", "test-ns", "variant-a", "H100", 1, domain.RoleDecode, 1000, 1000, 5)
	a.RestoreLearnedState(LearnedState{
		Config:         map[string]string{"test-ns/variant-a": mtParams(256, "vllm:v1").Fingerprint()},
		Args:           map[string]string{"test-ns/variant-a": mtParams(256, "vllm:v1").ArgsFingerprint()},
		Throughput:     map[string]ThroughputSnapshot{key: {RollingAverageSnapshot: rrWindow(3).Snapshot()}},
		ThroughputArgs: map[string]string{key: mtParams(256, "vllm:v1").ArgsFingerprint()},
	})
	r, done := a.installRestored("test-ns", "test-model", "variant-a")
	if !done || r.Outcome != RestoredPrior {
		t.Fatalf("precondition: a prior restore, got %+v done=%v", r, done)
	}
	if r.Priors != 0 || r.Throughput != 1 {
		t.Errorf("want 0 k2 windows and 1 throughput window reported, got priors=%d throughput=%d", r.Priors, r.Throughput)
	}
}

// A configuration not read completely, restored from a document that recorded
// no arguments, has nothing to check a window against: the report says so,
// rather than blaming windows too old to keep that were never looked at.
func TestAnIncompleteRestoreWithNoArgumentsSaysWhy(t *testing.T) {
	unread := &capacity.EngineParams{Engine: "vllm", MaxNumSeqs: 64, Unresolved: []string{"max-num-seqs"}}
	a, _, _ := waProcess(unread)
	a.RestoreLearnedState(LearnedState{
		Config: map[string]string{"test-ns/variant-a": mtParams(256, "vllm:v1").Fingerprint()},
		K2:     map[string]capacity.RollingAverageSnapshot{rrKey(a, "variant-a"): rrWindow(4000).Snapshot()},
	})
	r, done := a.installRestored("test-ns", "test-model", "variant-a")
	if !done || r.Outcome != RestoredIncomplete {
		t.Fatalf("precondition: an incomplete restore, got %+v done=%v", r, done)
	}
	if !r.NoArgs {
		t.Errorf("an incomplete restore of a document with no arguments was not reported as such: %+v", r)
	}
}
