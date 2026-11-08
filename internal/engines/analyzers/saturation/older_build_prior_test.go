package saturation

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Specs for the fifth review of the learned-state redesign: each pins one
// finding.

// The service rate from before an arguments change holds the floor on serving
// time only: a bridge, a Pod that is not Ready or a rollout reads it as it
// stands. Counted on every read, eight hours of a rollout spent the whole
// hold before the new arguments served a cycle.
func TestTheOldServiceRateClockRunsOnlyOnServingRows(t *testing.T) {
	a, _, clock := waProcess(mtParams(256, "vllm:v1"))
	key := rrKey(a, "variant-a")
	from, to := windowArgs(mtParams(256, "vllm:v1")), windowArgs(mtParams(64, "vllm:v1"))
	a.mu.Lock()
	a.saturatedThroughput[key] = rrWindow(2.5)
	a.throughputWindowArgs[key] = from
	a.throughputWindowUnderLocked(key, to)
	sk := stashKey(key, from)
	a.mu.Unlock()
	for end := clock.Add(8 * time.Hour); clock.Before(end); *clock = clock.Add(4 * time.Minute) {
		if r := a.saturatedThroughputReadingUnder(key, to, false); r.rate != 2.5 || !r.borrowed {
			t.Fatalf("a row that is not serving lost the old rate: %+v", r)
		}
	}
	a.mu.Lock()
	held := a.throughputStash[sk].heldFor
	a.mu.Unlock()
	if held != 0 {
		t.Errorf("eight hours of reads by rows that are not serving counted %v of the old rate's hold, want none", held)
	}
	if r := a.saturatedThroughputReadingUnder(key, to, true); r.rate != 2.5 || !r.borrowed {
		t.Errorf("the first serving cycle after them: %+v, want the old rate still holding", r)
	}
}

// A previous figure from a PR ev-shindin/llm-scaling-manager#151 document is last used when it was
// installed there, not at the restore: dated from the restore, a figure days
// old was handed a fresh bucket retention.
func TestAnOlderBuildsPriorAgesFromItsInstall(t *testing.T) {
	a, _, clock := waProcess(mtParams(64, "vllm:v1"))
	key := rrKey(a, "variant-a")
	installed := clock.Add(-time.Hour)
	a.RestoreLearnedState(LearnedState{PriorK2: map[string]PriorK2Snapshot{
		key: {K2: 4000, InstalledAt: installed},
	}})
	a.mu.Lock()
	defer a.mu.Unlock()
	lb := a.lowerBounds[key]
	if lb == nil || lb.prior != 4000 {
		t.Fatalf("precondition: the older build's figure installed, got %+v", lb)
	}
	if !lb.priorUsed.Equal(installed) {
		t.Errorf("the figure was last used at %v, want when it was installed, %v", lb.priorUsed, installed)
	}
}

// A previous figure from a PR ev-shindin/llm-scaling-manager#151 document is not installed where the
// document holds a window for the key: that build priced such a key from the
// window, never the figure, and installed it priced the next rollout from a
// figure the variant had since replaced.
func TestAnOlderBuildsPriorIsNotInstalledOverAWindow(t *testing.T) {
	a, _, clock := waProcess(mtParams(64, "vllm:v1"))
	key := rrKey(a, "variant-a")
	a.RestoreLearnedState(LearnedState{
		PriorK2: map[string]PriorK2Snapshot{key: {K2: 4000, InstalledAt: clock.Add(-time.Hour)}},
		K2: map[string]capacity.RollingAverageSnapshot{
			key: {Values: []float64{8000}, LastUsed: *clock, LastWritten: clock.Add(-time.Hour)},
		},
	})
	a.mu.Lock()
	defer a.mu.Unlock()
	if lb := a.lowerBounds[key]; lb != nil && lb.prior > 0 {
		t.Errorf("the older build's figure %d was installed over the window it stood behind", lb.prior)
	}
}
