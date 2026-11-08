package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// servingRow is one collected replica: named, flagged, and READY.
//
// Ready by default because that is the ordinary case and every test below is
// about something else. The one test that turns it off says so in its name.
func servingRow(variant, pod string, bridge bool) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{
		VariantName: variant, PodName: pod, FromWarmPool: bridge, Ready: true,
	}
}

func servingCountFor(t *testing.T, variant string) (int, bool) {
	t.Helper()
	return decision.Serving("tenant", variant, time.Minute, time.Now())
}

// A BRIDGE is not one of the variant's own replicas, and must not be counted as
// one.
//
// The pool asks this figure whether the ordinary replicas have arrived so it can
// take its lent Pod back. A bridge is attributed to the variant it serves --
// correctly, its demand is the variant's -- so counting it here answers yes one
// replica early, and the pool hands back the Pod that was carrying the traffic.
//
// Measured on the shared GPU cluster, 2026-08-30: with one ordinary replica Ready and one bridge
// lent, the pool was told "ready 1, serving 2".
func TestABridgeIsNotCountedAsAServingReplica(t *testing.T) {
	decision.DefaultServing.Reset()

	publishServing("tenant", []domain.ReplicaMetrics{
		servingRow("qwen-decode-llmsm", "qwen-decode-abc", false),
		servingRow("qwen-decode-llmsm", "llmsm-warm-pool-0", true),
	})

	got, known := servingCountFor(t, "qwen-decode-llmsm")
	if !known {
		t.Fatal("no serving count published; the variant had rows")
	}
	if got != 1 {
		t.Errorf("serving = %d, want 1 -- the bridge is not one of the variant's own replicas", got)
	}
}

// A variant carried ENTIRELY by bridges publishes ZERO, not nothing.
//
// The two are opposite answers. No reading means the collector has not run and
// the Ready count stands in; zero means the variant's own replicas are
// demonstrably not serving, which is precisely when its bridge must be kept.
// Skipping the row outright would produce the first while meaning the second.
func TestAVariantServedOnlyByBridgesPublishesZero(t *testing.T) {
	decision.DefaultServing.Reset()

	publishServing("tenant", []domain.ReplicaMetrics{
		servingRow("qwen-decode-llmsm", "llmsm-warm-pool-0", true),
		servingRow("qwen-decode-llmsm", "llmsm-warm-pool-1", true),
	})

	got, known := servingCountFor(t, "qwen-decode-llmsm")
	if !known {
		t.Fatal("want a published zero, not an absent reading -- they mean opposite things")
	}
	if got != 0 {
		t.Errorf("serving = %d, want 0", got)
	}
}

// Ordinary replicas are still counted by POD, so an engine reporting several
// times for one replica counts once.
func TestOrdinaryReplicasAreCountedByPod(t *testing.T) {
	decision.DefaultServing.Reset()

	publishServing("tenant", []domain.ReplicaMetrics{
		servingRow("qwen-decode-llmsm", "qwen-decode-abc", false),
		servingRow("qwen-decode-llmsm", "qwen-decode-abc", false),
		servingRow("qwen-decode-llmsm", "qwen-decode-def", false),
	})

	got, _ := servingCountFor(t, "qwen-decode-llmsm")
	if got != 2 {
		t.Errorf("serving = %d, want 2 distinct Pods", got)
	}
}

// A row scaling manager could not attribute says nothing about any variant, so it publishes
// nothing rather than collecting into an empty-named bucket.
func TestAnUnattributedRowPublishesNothing(t *testing.T) {
	decision.DefaultServing.Reset()

	publishServing("tenant", []domain.ReplicaMetrics{
		servingRow("", "some-pod", false),
		servingRow("qwen-decode-llmsm", "", false),
	})

	if _, known := servingCountFor(t, ""); known {
		t.Error("an empty variant name must not become a bucket")
	}
	if _, known := servingCountFor(t, "qwen-decode-llmsm"); known {
		t.Error("a row with no Pod name is not evidence that the variant is serving")
	}
}

// A replica that is REPORTING but not READY is not serving.
//
// An engine answers /metrics as soon as its HTTP server is up, which is before
// the Pod passes readiness -- so a starting replica reports for some seconds
// while no Service and no EPP will route to it. The pool reads this count to
// decide whether its lent Pod can go home, and going home to a replica that is
// not in the rotation strands the traffic just as surely as going home too
// early.
//
// Measured on the shared GPU cluster, 2026-08-30: a variant with ONE replica reported four,
// because nothing between Prometheus and this count checked the Pod at all.
func TestAReportingButNotReadyReplicaIsNotServing(t *testing.T) {
	decision.DefaultServing.Reset()

	notReady := servingRow("qwen-decode-llmsm", "qwen-decode-starting", false)
	notReady.Ready = false

	publishServing("tenant", []domain.ReplicaMetrics{
		servingRow("qwen-decode-llmsm", "qwen-decode-abc", false),
		notReady,
	})

	got, known := servingCountFor(t, "qwen-decode-llmsm")
	if !known {
		t.Fatal("no serving count published; the variant had rows")
	}
	if got != 1 {
		t.Errorf("serving = %d, want 1 -- a Pod that is reporting but not Ready takes no traffic", got)
	}
}

// A variant whose replicas are ALL still starting publishes ZERO, not nothing.
//
// Same rule as the all-bridges case, and for the same reason: no reading falls
// back to the scale target's Ready count, while zero says its replicas are
// demonstrably not serving -- which is exactly when a bridge must be kept.
func TestAVariantWhoseReplicasAreAllStartingPublishesZero(t *testing.T) {
	decision.DefaultServing.Reset()

	starting := servingRow("qwen-decode-llmsm", "qwen-decode-starting", false)
	starting.Ready = false

	publishServing("tenant", []domain.ReplicaMetrics{starting})

	got, known := servingCountFor(t, "qwen-decode-llmsm")
	if !known {
		t.Fatal("want a published zero, not an absent reading -- they mean opposite things")
	}
	if got != 0 {
		t.Errorf("serving = %d, want 0", got)
	}
}
