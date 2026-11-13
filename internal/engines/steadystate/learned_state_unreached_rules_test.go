package steadystate

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Learned-state rules a mutation review found no test reaching: each test
// below fails with the rule it names taken out.

// A variant's put-aside ITL baseline goes with its liveness, as its baseline
// does. Kept, the orphan priced nothing and still took room under the size
// bound, so the trim left out a live window it did not need to.
func TestLearnedState_TheTrimDropsAPutAsideITLBaselineWithItsVariant(t *testing.T) {
	now := time.Now()
	stash := stampKey + "#older-args"
	const live = "ns|v2|m|H100|1|both|short|q5"
	withStash := func() saturation.LearnedState {
		s := stampedState(now)
		s.ITLBaselineStash = map[string]float64{stash: 0.02}
		return s
	}

	s := withStash()
	for _, e := range learnedStateEntries(s) {
		if e.key == "seen|"+stampKey {
			e.drop()
		}
	}
	if len(s.ITLBaselineStash) != 0 {
		t.Errorf("the put-aside baseline outlived its variant: %v", s.ITLBaselineStash)
	}

	// Another variant's window, used now: the first variant's liveness, an
	// hour old, is the one entry the trim needs to leave out.
	s = withStash()
	s.K2 = map[string]capacity.RollingAverageSnapshot{live: {Values: []float64{4000}, LastUsed: now, LastWritten: now}}
	alone := saturation.LearnedState{K2: s.K2}
	fits, _, err := encodeLearnedState(alone, nil, now, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	raw, dropped, err := encodeLearnedState(s, nil, now, len(fits))
	if err != nil {
		t.Fatal(err)
	}
	got := decodeLearnedState(t, raw)
	if _, kept := got.K2[live]; !kept || dropped != 1 {
		t.Errorf("the trim left out %d entries and kept the live window=%v: the orphaned baseline took its room", dropped, kept)
	}
	if len(got.ITLBaselineStash) != 0 {
		t.Errorf("the trim kept a put-aside baseline of a dropped variant: %v", got.ITLBaselineStash)
	}
	if len(s.ITLBaselineStash) != 1 {
		t.Errorf("the trim reached the caller's map: %v", s.ITLBaselineStash)
	}
}

// stoppedExecutor is an executor whose loop has already stopped: Start
// returns at once, as the polling executor does on a cancelled context.
type stoppedExecutor struct{ started bool }

func (s *stoppedExecutor) Start(context.Context) { s.started = true }

// StartOptimizeLoop writes the learned state once more after the loop stops
// (flushLearnedState): a planned upgrade or a leader hand-off otherwise lost
// what was learned since the last interval's write.
func TestStartOptimizeLoopWritesTheLearnedStateWhenTheLoopStops(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()
	spy := &stateSpy{state: sampleState(4000)}
	e := snapshotEngine(t, c, spy)
	e.optimizer = allocation.NewGreedyByScoreOptimizer()
	e.metricsEmitter = metrics.NewMetricsEmitter()
	e.inFlightStore.loaded = true // nothing to read back
	exec := &stoppedExecutor{}
	e.executor = exec

	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)
	spy.state = sampleState(5000)
	e.writeLearnedState(ctx, now.Add(10*time.Second))
	const key = "ns|v|m|H100|1|both|short|q5"
	before, _ := storedDoc(t, c)
	if got := before.State.K2[key].Values; len(got) != 1 || got[0] != 4000 {
		t.Fatalf("precondition: the interval holds an ordinary write back, stored %v", got)
	}

	stopped, cancel := context.WithCancel(ctx)
	cancel()
	e.StartOptimizeLoop(stopped)
	if !exec.started {
		t.Fatal("precondition: the loop ran")
	}
	after, _ := storedDoc(t, c)
	if got := after.State.K2[key].Values; len(got) != 1 || got[0] != 5000 {
		t.Errorf("the loop stopped without writing what it learned: stored %v, want [5000]", got)
	}
}
