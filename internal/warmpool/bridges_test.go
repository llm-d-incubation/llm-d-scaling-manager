package warmpool

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/policy"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/pool"
)

func lentMembership(podName, variant string, state pool.State) pool.Membership {
	return pool.Membership{
		Pod:   types.NamespacedName{Namespace: "pool-ns", Name: podName},
		Model: pool.ModelRef{Namespace: "tenant", Variant: variant},
		State: state,
	}
}

func bridgeDemandFor(variant string) policy.VariantDemand {
	return policy.VariantDemand{
		Model: pool.ModelRef{Namespace: "tenant", Variant: variant},
	}
}

// Only a SERVING Pod is a bridge, and it is published under the VARIANT's name.
//
// The name matters as much as the set. The collector resolves a Pod's identity
// by walking its ownerReferences to the managed scaler and takes that scaler's
// name -- the ScaledObject's -- and the analyzer carries the same string. The
// scale target is the Deployment underneath it, a different name on any real
// deployment: `qwen-decode-llmsm` scales `qwen-decode`. Publishing the target
// attributes the Pod's metrics to a variant nothing recognises, which is the
// same as not publishing at all -- except that it looks as though it were
// working.
func TestOnlyALentPodIsPublishedAsABridge(t *testing.T) {
	memberships := []pool.Membership{
		lentMembership("pool-0", "variant-a", pool.Serving),
		lentMembership("pool-1", "variant-a", pool.Asleep),
		lentMembership("pool-2", "variant-b", pool.Waking),
		lentMembership("pool-3", "variant-b", pool.Serving),
	}
	variants := []policy.VariantDemand{
		bridgeDemandFor("variant-a"),
		bridgeDemandFor("variant-b"),
	}

	got := lentPodsByVariant(memberships, variants)

	want := map[string]string{"pool-0": "variant-a", "pool-3": "variant-b"}
	if len(got) != len(want) {
		t.Fatalf("lending = %v, want %v", got, want)
	}
	for pod, variant := range want {
		if got[pod] != variant {
			t.Errorf("lending[%s] = %q, want %q", pod, got[pod], variant)
		}
	}
}

// The scale target's name is never published, however plausible it looks.
//
// Pinned separately from the test above because the two names are equal in most
// fixtures and were equal in the ones that let this ship: a lending keyed by the
// Deployment matched no analyzer row, so the bridge supply the whole chain
// exists to report was never once emitted.
func TestABridgeIsPublishedUnderTheVariantNotTheScaleTarget(t *testing.T) {
	memberships := []pool.Membership{lentMembership("pool-0", "qwen-decode-llmsm", pool.Serving)}
	variants := []policy.VariantDemand{bridgeDemandFor("qwen-decode-llmsm")}

	got := lentPodsByVariant(memberships, variants)

	if got["pool-0"] == "qwen-decode" {
		t.Fatal("published the Deployment's name; the collector keys a variant by the ScaledObject's")
	}
	if got["pool-0"] != "qwen-decode-llmsm" {
		t.Errorf("lending[pool-0] = %q, want the variant name qwen-decode-llmsm", got["pool-0"])
	}
}

// A Pod lent to a variant that has since gone is left out rather than guessed
// at. Attributing it to a variant nothing is scaling would add demand no
// optimizer pass could act on, and the Pod is about to be reclaimed as an orphan
// anyway.
func TestAPodLentToAVanishedVariantIsNotPublished(t *testing.T) {
	memberships := []pool.Membership{lentMembership("pool-0", "variant-gone", pool.Serving)}

	if got := lentPodsByVariant(memberships, nil); len(got) != 0 {
		t.Errorf("lending = %v, want nothing for a variant no longer in the demand", got)
	}
}

// An UNNAMED variant is skipped for the same reason: there is no name to publish
// it under that anything downstream would match, and an empty key would collect
// every unnamed Pod into one bucket that attributes them all to each other.
func TestAnUnnamedVariantIsSkipped(t *testing.T) {
	memberships := []pool.Membership{lentMembership("pool-0", "", pool.Serving)}
	variants := []policy.VariantDemand{bridgeDemandFor("")}

	if got := lentPodsByVariant(memberships, variants); len(got) != 0 {
		t.Errorf("lending = %v, want nothing for a variant with no name", got)
	}
}

// Nothing lent publishes an empty map, not a nil one that a caller might read as
// "no answer". The pool publishes this on every pass it could observe itself,
// and an empty answer is what clears a Pod that has been handed back.
func TestNothingLentPublishesAnEmptyMap(t *testing.T) {
	memberships := []pool.Membership{lentMembership("pool-0", "variant-a", pool.Asleep)}
	variants := []policy.VariantDemand{bridgeDemandFor("variant-a")}

	got := lentPodsByVariant(memberships, variants)
	if got == nil {
		t.Fatal("want an empty map, not nil")
	}
	if len(got) != 0 {
		t.Errorf("lending = %v, want empty", got)
	}
}
