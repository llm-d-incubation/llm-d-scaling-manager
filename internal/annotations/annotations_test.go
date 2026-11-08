package annotations_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/annotations"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// One annotation is left. The discovery schema — llm-d.ai/managed, model-id and
// variant-cost — is gone: The scaling manager no longer looks for the workloads it manages, so
// there is nothing for an opt-in annotation to opt into. Configuration lives in
// KEDA trigger metadata now (internal/registry).
//
// What Synthetic still does is guard the API server: every variant the scaling manager works with
// is an in-memory object it synthesized, and none of them may ever be written.
func TestIsSynthetic(t *testing.T) {
	synthesized := &llmdvariant.Variant{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "chat-so",
			Namespace:   "chat",
			Annotations: map[string]string{annotations.Synthetic: "true"},
		},
	}
	if !annotations.IsSynthetic(synthesized) {
		t.Error("a variant marked synthetic must report as synthetic")
	}

	for name, va := range map[string]*llmdvariant.Variant{
		"no annotations": {ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "chat"}},
		"annotation absent": {ObjectMeta: metav1.ObjectMeta{
			Name: "b", Namespace: "chat", Annotations: map[string]string{"other": "true"},
		}},
		"annotation not true": {ObjectMeta: metav1.ObjectMeta{
			Name: "c", Namespace: "chat", Annotations: map[string]string{annotations.Synthetic: "false"},
		}},
	} {
		if annotations.IsSynthetic(va) {
			t.Errorf("%s: must not report as synthetic", name)
		}
	}
}
