package scalefromzero

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
)

// Review 183-2: a refused-wake reason was cleared only by
// processInactiveModel, for a model still in the inactive groups. A model
// that woke by another path, or was deleted, kept wake-no-capacity for the
// life of the process. pruneRefusals clears it, and the extra-pool note, for
// a model that left the groups, and the reason of a model whose decode
// serves again. A model still parked, or serving prefill alone, keeps it.
func TestPruneRefusalsClearsWakeReasonsOfModelsThatLeftOrServe(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	gone := modelGroup{namespace: "chat", modelID: "gone"}
	woke := modelGroup{namespace: "chat", modelID: "woke"}
	parked := modelGroup{namespace: "chat", modelID: "parked"}
	prefillOnly := modelGroup{namespace: "chat", modelID: "prefill-only"}

	e := &Engine{}
	for _, g := range []modelGroup{gone, woke, parked, prefillOnly} {
		e.reportWakeBlocked(g, "wake-no-capacity")
	}
	e.extraPools.Store(gone.key(), "chat/pool-b")
	e.extraPools.Store(parked.key(), "chat/pool-b")

	e.pruneRefusals([]modelGroup{woke, parked, prefillOnly}, map[string]coverage{
		woke.key():        {decode: true},
		prefillOnly.key(): {prefill: true},
	})

	got := wakeReasons(t, registry)
	for _, m := range []string{"gone", "woke"} {
		if r, ok := got[m]; ok {
			t.Errorf("model %s left the wake loop or serves, but still carries llmsm_model_scaling_blocked{reason=%q}", m, r)
		}
	}
	for _, m := range []string{"parked", "prefill-only"} {
		if got[m] != "wake-no-capacity" {
			t.Errorf("model %s is still refused, reason %q, want wake-no-capacity", m, got[m])
		}
	}
	if _, ok := e.extraPools.Load(gone.key()); ok {
		t.Error("the extra-pool note of a model that left the wake loop was kept")
	}
	if _, ok := e.extraPools.Load(parked.key()); !ok {
		t.Error("the extra-pool note of a model still parked was dropped")
	}

	// The cleared models were forgotten, not just unset: refused again, a
	// model sets its reason afresh, and a later clear removes it.
	e.reportWakeBlocked(gone, "wake-no-capacity")
	e.reportWakeBlocked(gone, "")
	if r, ok := wakeReasons(t, registry)["gone"]; ok {
		t.Errorf("a re-set reason was not cleared: %q", r)
	}
}
