package collector

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A DP>1 BRIDGE collapses to one replica, and stays a bridge.
//
// A warm pool Pod lent to a variant is one scale-target replica exactly like an
// ordinary Pod, so its data-parallel ranks merge the same way. The two Pod-level
// facts have to survive that merge: FromWarmPool decides whether the row counts
// as the variant's own SUPPLY, and Ready decides whether it counts as SERVING.
// Losing either on a DP variant would apply the bridge rules to some variants
// and not others, for no reason an operator could see.
func TestADataParallelBridgeCollapsesToOneReplicaAndStaysABridge(t *testing.T) {
	got := collapseToPods([]domain.ReplicaMetrics{
		{
			PodName: "llmsm-warm-pool-0", VariantName: "qwen-decode-llmsm", ModelID: "qwen",
			TotalKvCapacityTokens: 8000, TokensInUse: 2000,
			FromWarmPool: true, Ready: true,
		},
		{
			PodName: "llmsm-warm-pool-0", VariantName: "qwen-decode-llmsm", ModelID: "qwen",
			TotalKvCapacityTokens: 8000, TokensInUse: 2000,
			FromWarmPool: true, Ready: true,
		},
	})

	if len(got) != 1 {
		t.Fatalf("collapsed to %d rows, want 1 -- a Pod is one replica however many ranks it runs", len(got))
	}
	if !got[0].FromWarmPool {
		t.Error("the merged row lost FromWarmPool, so a DP bridge would be counted as the variant's own supply")
	}
	if !got[0].Ready {
		t.Error("the merged row lost Ready, so a DP replica would never count as serving")
	}
	if got[0].TotalKvCapacityTokens != 16000 {
		t.Errorf("capacity = %v, want the ranks summed (16000)", got[0].TotalKvCapacityTokens)
	}
}

// A pod that is NOT ready keeps that through the merge, so publishServing still
// excludes it. Ranks of one Pod share the Pod's readiness by construction.
func TestADataParallelPodThatIsNotReadyStaysNotReady(t *testing.T) {
	got := collapseToPods([]domain.ReplicaMetrics{
		{PodName: "qwen-decode-0", VariantName: "qwen-decode-llmsm", ModelID: "qwen",
			TotalKvCapacityTokens: 100, Ready: false},
		{PodName: "qwen-decode-0", VariantName: "qwen-decode-llmsm", ModelID: "qwen",
			TotalKvCapacityTokens: 100, Ready: false},
	})

	if len(got) != 1 {
		t.Fatalf("collapsed to %d rows, want 1", len(got))
	}
	if got[0].Ready {
		t.Error("a Pod that is not Ready must not become Ready by being merged")
	}
}

// TWO MODELS IN ONE POD ARE NOT DATA PARALLELISM.
//
// This is the case a warm pool creates and an ordinary fleet cannot. An ordinary
// Pod belongs to exactly one variant, so keying the merge by Pod name is safe.
// A POOL Pod holds several models at once -- that is what a warm set is -- each
// on its own port, and the collector keys instances "pod:port" so each appears
// as a separate row for the same Pod.
//
// Merging those sums the capacity of models that have nothing to do with each
// other into whichever row happened to come first. On a lent Pod every row
// carries the variant it is lent to, so the sleepers' KV cache would be added to
// the awake model's supply -- the variant would be told it has more capacity
// than the engine serving it actually has, which is the one direction that
// causes a scale-DOWN it cannot serve.
//
// Data parallelism is ranks of the SAME model, so the model is what separates
// the two cases.
func TestTwoModelsInOnePodAreNotMerged(t *testing.T) {
	got := collapseToPods([]domain.ReplicaMetrics{
		{PodName: "llmsm-warm-pool-0", VariantName: "qwen-decode-llmsm", ModelID: "qwen",
			TotalKvCapacityTokens: 8000},
		{PodName: "llmsm-warm-pool-0", VariantName: "qwen-decode-llmsm", ModelID: "llama",
			TotalKvCapacityTokens: 8000},
	})

	if len(got) != 2 {
		t.Fatalf("collapsed to %d rows, want 2 -- two models in one Pod are not DP ranks", len(got))
	}
	for _, m := range got {
		if m.TotalKvCapacityTokens != 8000 {
			t.Errorf("model %q capacity = %v, want 8000: another model's cache was summed into it",
				m.ModelID, m.TotalKvCapacityTokens)
		}
	}
}
