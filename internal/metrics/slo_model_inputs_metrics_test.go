package metrics

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// One coverage reason is 1 per model and objective, the others 0; an input
// read again loses its missing series; deleting the model, directly or
// through DeleteSLOModel, removes every series of it and none of another's.
func TestRecordSLOModelInputs(t *testing.T) {
	_, registry := engineConfigEmitter(t)
	RecordSLOModelInputs("ns", "m", constants.SLOObjectiveTTFT, constants.SLOModelSignalMissing,
		[]string{"prefill-time-mean", "queue-time-mean"})
	if got := countSeries(t, registry, constants.LLMSMSLOModelCoverage); got != len(constants.SLOModelReasons) {
		t.Fatalf("%d coverage series, want one per reason", got)
	}
	if v, ok := gaugeAt(t, registry, constants.LLMSMSLOModelCoverage, map[string]string{
		constants.LabelObjective: "ttft", constants.LabelReason: constants.SLOModelSignalMissing}); !ok || v != 1 {
		t.Fatalf("signal-missing = %v, %v; want 1, true", v, ok)
	}
	if v, ok := gaugeAt(t, registry, constants.LLMSMSLOModelCoverage, map[string]string{
		constants.LabelObjective: "ttft", constants.LabelReason: constants.SLOModelWindowShort}); !ok || v != 0 {
		t.Fatalf("window-short = %v, %v; want 0, true", v, ok)
	}
	if got := countSeries(t, registry, constants.LLMSMSLOModelInputMissing); got != 2 {
		t.Fatalf("%d missing-input series, want 2", got)
	}

	RecordSLOModelInputs("ns", "m", constants.SLOObjectiveTTFT, constants.SLOModelSignalMissing, []string{"queue-time-mean"})
	if _, ok := gaugeAt(t, registry, constants.LLMSMSLOModelInputMissing, map[string]string{
		constants.LabelSignal: "prefill-time-mean"}); ok {
		t.Fatal("an input read again kept its missing series")
	}
	RecordSLOModelInputs("ns", "m", constants.SLOObjectiveTTFT, constants.SLOModelWindowShort, nil)
	if got := countSeries(t, registry, constants.LLMSMSLOModelInputMissing); got != 0 {
		t.Fatalf("%d missing-input series with none missing, want 0", got)
	}

	// An objective it does not know publishes nothing.
	RecordSLOModelInputs("ns", "m", "itl", constants.SLOModelPriced, nil)
	if got := countSeries(t, registry, constants.LLMSMSLOModelCoverage); got != len(constants.SLOModelReasons) {
		t.Fatalf("%d coverage series after an unknown objective, want %d", got, len(constants.SLOModelReasons))
	}

	RecordSLOModelInputs("ns", "m", constants.SLOObjectiveTPOT, constants.SLOModelSignalMissing, []string{"router-tpot-histogram"})
	RecordSLOModelInputs("ns", "other", constants.SLOObjectiveTPOT, constants.SLOModelSignalMissing, []string{"router-tpot-histogram"})
	DeleteSLOModel("ns", "m")
	if got := countSeries(t, registry, constants.LLMSMSLOModelCoverage); got != len(constants.SLOModelReasons) {
		t.Fatalf("%d coverage series after deleting m, want model other's %d", got, len(constants.SLOModelReasons))
	}
	if got := countSeries(t, registry, constants.LLMSMSLOModelInputMissing); got != 1 {
		t.Fatalf("%d missing-input series after deleting m, want model other's 1", got)
	}
	if got := countSeries(t, registry, constants.LLMSMSLOPredictedTPOTSeconds); got != 0 {
		t.Fatalf("%d predicted TPOT series, want none: RecordSLOModelInputs publishes coverage only", got)
	}
}
