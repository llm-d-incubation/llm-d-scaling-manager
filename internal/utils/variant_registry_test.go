package utils

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/annotations"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// Discovery from the registry: a workload exists because KEDA called the scaling manager about
// it, and its identity comes from the trigger metadata that call carried. No
// annotation, and nothing listed.
//
// See docs/plans/engine/keda-driven-discovery.md.

// registerEnriched puts an entry in reg as if KEDA had called about it and the
// enricher had since resolved its ScaledObject.
func registerEnriched(reg *registry.Registry, namespace, name, target, modelID string) {
	reg.Observe(namespace, name, map[string]string{
		registry.ModelIDKey:     modelID,
		registry.VariantCostKey: "7.5",
	})
	maxR := int32(6)
	reg.SetTarget(namespace, name, registry.Target{
		APIVersion:  "apps/v1",
		Kind:        "Deployment",
		Name:        target,
		MaxReplicas: &maxR,
	})
}

func TestRegisteredWorkloadBecomesAVariant(t *testing.T) {
	ctx := context.Background()
	reg := registry.New(time.Minute)
	registerEnriched(reg, "ns1", "chat-so", "chat-deploy", "meta/llama-3-8b")

	result := readyVariants(ctx, reg)
	if len(result) != 1 {
		t.Fatalf("want 1 registry-sourced variant, got %d", len(result))
	}
	va := result[0]

	// The ScaledObject names the variant — there is one per scale target.
	if va.Name != "chat-so" || va.Namespace != "ns1" {
		t.Errorf("identity should come from the ScaledObject: %s/%s", va.Namespace, va.Name)
	}
	if va.Spec.ModelID != "meta/llama-3-8b" {
		t.Errorf("modelID should come from trigger metadata, got %q", va.Spec.ModelID)
	}
	if va.Spec.VariantCost != "7.5" {
		t.Errorf("variantCost should come from trigger metadata, got %q", va.Spec.VariantCost)
	}
	if va.Spec.ScaleTargetRef.Name != "chat-deploy" {
		t.Errorf("scale target should come from enrichment, got %q", va.Spec.ScaleTargetRef.Name)
	}
	if va.Spec.MaxReplicas != 6 {
		t.Errorf("the envelope should come from the ScaledObject, got max=%d", va.Spec.MaxReplicas)
	}
	if !annotations.IsSynthetic(&va) {
		t.Error("a registry-sourced variant is in-memory only and must be marked synthetic")
	}
}

// TestUnenrichedEntryIsSkipped: registration and enrichment are separate steps,
// and between them there is no scale target to collect from or scale. Emitting a
// variant with a blank target would put a phantom in the fleet the optimizer
// balances against.
func TestUnenrichedEntryIsSkipped(t *testing.T) {
	ctx := context.Background()
	reg := registry.New(time.Minute)
	reg.Observe("ns1", "chat-so", map[string]string{registry.ModelIDKey: "meta/llama-3-8b"})

	if result := readyVariants(ctx, reg); len(result) != 0 {
		t.Errorf("want no variant before the scale target is resolved, got %d", len(result))
	}
}

// TestVariantNameMetadataResolvesWithoutEnrichment is the escape hatch: a trigger
// that names its target directly needs no ScaledObject read to become a variant.
// TestScaleTargetComesFromEnrichmentNotMetadata: the scale target is resolved by
// reading the ScaledObject, never declared in trigger metadata. A complete
// trigger that has not been enriched yet therefore yields nothing — which is the
// point: a hand-written copy of scaleTargetRef.name can disagree with the spec
// that actually decides what KEDA scales, and then a variant's metrics are
// attributed to another workload.
func TestScaleTargetComesFromEnrichmentNotMetadata(t *testing.T) {
	ctx := context.Background()
	reg := registry.New(time.Minute)
	reg.Observe("ns1", "chat-so", map[string]string{
		registry.ModelIDKey: "meta/llama-3-8b",
	})

	if result := readyVariants(ctx, reg); len(result) != 0 {
		t.Fatalf("want no variant before enrichment, got %d", len(result))
	}

	reg.SetTarget("ns1", "chat-so", registry.Target{Name: "chat-deploy"})

	result := readyVariants(ctx, reg)
	if len(result) != 1 {
		t.Fatalf("want 1 variant after enrichment, got %d", len(result))
	}
	if got := result[0].Spec.ScaleTargetRef.Name; got != "chat-deploy" {
		t.Errorf("scale target: got %q", got)
	}
	// KEDA's own scaleTargetRef defaults must be applied, since no object was read.
	if result[0].Spec.ScaleTargetRef.Kind != "Deployment" || result[0].Spec.ScaleTargetRef.APIVersion != "apps/v1" {
		t.Errorf("want KEDA's defaults, got %s %s",
			result[0].Spec.ScaleTargetRef.APIVersion, result[0].Spec.ScaleTargetRef.Kind)
	}
}

// TestUnusableTriggerMetadataIsSkipped: modelID is the grouping key for every
// multi-variant decision, so a variant without one cannot be optimized against a
// model. It is dropped here rather than at registration, where refusing it would
// make the misconfiguration invisible instead of merely inert.
func TestUnusableTriggerMetadataIsSkipped(t *testing.T) {
	ctx := context.Background()
	reg := registry.New(time.Minute)
	reg.Observe("ns1", "chat-so", map[string]string{registry.VariantCostKey: "5.0"})
	reg.SetTarget("ns1", "chat-so", registry.Target{Name: "chat-deploy"})

	if result := readyVariants(ctx, reg); len(result) != 0 {
		t.Errorf("want no variant from a trigger with no modelID, got %d", len(result))
	}
}

// TestExpiredEntryYieldsNoVariant: the TTL is what replaces deletion, so a
// workload KEDA has stopped calling about must leave the fleet.
func TestExpiredEntryYieldsNoVariant(t *testing.T) {
	ctx := context.Background()
	// A TTL already elapsed: nothing has been observed since, so the entry is dead.
	reg := registry.New(time.Nanosecond)
	registerEnriched(reg, "ns1", "chat-so", "chat-deploy", "meta/llama-3-8b")
	time.Sleep(time.Millisecond)

	if result := readyVariants(ctx, reg); len(result) != 0 {
		t.Errorf("want no variant once the entry has expired, got %d", len(result))
	}
}

// TestScaledObjectLabelsReachTheVariant pins a regression that does not fail
// loudly.
//
// Multi-controller isolation filters on the controller-instance label, so a
// controller configured with an instance name matches nothing and manages an
// empty fleet when the label does not reach the variant. It reports no error; it
// just quietly does less. (The accelerator lookup used to read a label here too,
// until that label was removed as unsound — only a placement constraint can say
// which accelerator a workload runs on.)
func TestScaledObjectLabelsReachTheVariant(t *testing.T) {
	ctx := context.Background()
	reg := registry.New(time.Minute)
	reg.Observe("ns1", "chat-so", map[string]string{registry.ModelIDKey: "meta/llama-3-8b"})
	maxR := int32(4)
	reg.SetTarget("ns1", "chat-so", registry.Target{
		APIVersion:  "apps/v1",
		Kind:        "Deployment",
		Name:        "chat-deploy",
		MaxReplicas: &maxR,
		Labels: map[string]string{
			"inference.optimization/acceleratorName": "A100",
			"llm-d.ai/controller-instance":           "scaling-manager-a",
		},
	})

	result := readyVariants(ctx, reg)
	if len(result) != 1 {
		t.Fatalf("want 1 variant, got %d", len(result))
	}
	if got := result[0].Labels["inference.optimization/acceleratorName"]; got != "A100" {
		t.Errorf("the accelerator label must reach the variant, got %q", got)
	}
	if got := result[0].Labels["llm-d.ai/controller-instance"]; got != "scaling-manager-a" {
		t.Errorf("the controller-instance label must reach the variant, got %q", got)
	}
}
