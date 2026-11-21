package metrics

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A variant has one llmsm_decode_packing_bound series: a changed bound or
// reason replaces it. The k2 series carry the figure before and after the
// bound. Deleting the variant, or its model, removes them all.
func TestRecordDecodePacking(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	held := domain.DecodePacking{Bound: domain.DecodePackingBoundITL, Reason: "slo-tpot", K2Unbounded: 268_800, K2: 200_000}
	emitter.RecordDecodePacking("ns", "m", "v1", held)
	if v, ok := gaugeAt(t, registry, constants.LLMSMDecodePackingBound, map[string]string{
		constants.LabelVariantName: "v1", constants.LabelBound: "itl", constants.LabelReason: "slo-tpot"}); !ok || v != 1 {
		t.Fatalf("bound series = %v, %v; want 1, true", v, ok)
	}
	for k2, want := range map[string]float64{constants.K2Unbounded: 268_800, constants.K2Bounded: 200_000} {
		if v, ok := gaugeAt(t, registry, constants.LLMSMDecodePackingK2Tokens, map[string]string{
			constants.LabelVariantName: "v1", constants.LabelK2: k2}); !ok || v != want {
			t.Fatalf("k2=%s = %v, %v; want %v", k2, v, ok, want)
		}
	}

	emitter.RecordDecodePacking("ns", "m", "v1", domain.DecodePacking{Bound: domain.DecodePackingBoundNone,
		Reason: "borrowed-line", K2Unbounded: 268_800, K2: 268_800})
	if got := countSeries(t, registry, constants.LLMSMDecodePackingBound); got != 1 {
		t.Fatalf("%d bound series after the reason changed, want 1", got)
	}
	if _, ok := gaugeAt(t, registry, constants.LLMSMDecodePackingBound, map[string]string{
		constants.LabelBound: "none", constants.LabelReason: "borrowed-line"}); !ok {
		t.Fatal("the new reason is not published")
	}

	emitter.RecordDecodePacking("ns", "m", "v2", held)
	emitter.RecordDecodePacking("ns", "other", "v3", held)
	emitter.DeleteDecodePacking("ns", "m", "v1")
	if got := countSeries(t, registry, constants.LLMSMDecodePackingBound); got != 2 {
		t.Fatalf("%d bound series after deleting v1, want 2", got)
	}
	if got := countSeries(t, registry, constants.LLMSMDecodePackingK2Tokens); got != 4 {
		t.Fatalf("%d k2 series after deleting v1, want 4", got)
	}
	emitter.DeleteDecodePackingForModel("ns", "m")
	if got := countSeries(t, registry, constants.LLMSMDecodePackingBound); got != 1 {
		t.Fatalf("%d bound series after deleting model m, want 1 (model other)", got)
	}
	// A reason set again after a delete is published, not skipped as already
	// known.
	emitter.RecordDecodePacking("ns", "m", "v1", held)
	if got := countSeries(t, registry, constants.LLMSMDecodePackingBound); got != 2 {
		t.Fatalf("%d bound series, want 2", got)
	}
}
