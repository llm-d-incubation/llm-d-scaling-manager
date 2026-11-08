package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// gaugeAt returns the value of the one series of name whose labels include
// every pair in match, and whether exactly one such series exists.
func gaugeAt(t *testing.T, registry *prometheus.Registry, name string, match map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var found []float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	series:
		for _, m := range mf.GetMetric() {
			for k, v := range match {
				if getLabelValue(m, k) != v {
					continue series
				}
			}
			found = append(found, m.GetGauge().GetValue())
		}
	}
	if len(found) != 1 {
		return 0, false
	}
	return found[0], true
}

func fullSignal() domain.ServiceRateSignal {
	return domain.ServiceRateSignal{
		HasLine: true, LineSlope: 0.03, LineIntercept: 0.002, ITLAtPricingK: 0.0224,
		ITLWindowed: true, LineSamples: 12,
		Role: domain.RoleDecode, Rate: 4.5, RateSource: domain.ServiceRateSourceDerived,
	}
}

func TestRecordServiceRateSignalPublishesEverySeries(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	emitter.RecordServiceRateSignal("ns", "m", "dec", fullSignal())
	emitter.RecordPricingK("ns", "m", 0.68)

	v := map[string]string{
		constants.LabelNamespace: "ns", constants.LabelModelName: "m", constants.LabelVariantName: "dec",
	}
	for name, want := range map[string]float64{
		constants.LLMSMITLLineSlope:         0.03,
		constants.LLMSMITLLineIntercept:     0.002,
		constants.LLMSMITLSecondsAtPricingK: 0.0224,
		constants.LLMSMITLLineSamples:       12,
	} {
		if got, ok := gaugeAt(t, registry, name, v); !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v", name, got, ok, want)
		}
	}
	rate := map[string]string{
		constants.LabelVariantName: "dec", constants.LabelRole: "decode", constants.LabelSource: "derived",
	}
	if got, ok := gaugeAt(t, registry, constants.LLMSMServiceRatePerReplica, rate); !ok || got != 4.5 {
		t.Errorf("service rate = %v (present %v), want 4.5", got, ok)
	}
	if got, ok := gaugeAt(t, registry, constants.LLMSMPricingK, map[string]string{
		constants.LabelNamespace: "ns", constants.LabelModelName: "m",
	}); !ok || got != 0.68 {
		t.Errorf("pricing k = %v (present %v), want 0.68", got, ok)
	}
}

// A line that is lost takes its three series with it; the sample count stays,
// because it is what explains the loss.
func TestRecordServiceRateSignalDropsALostLineAndKeepsTheSamples(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	emitter.RecordServiceRateSignal("ns", "m", "dec", fullSignal())

	sig := fullSignal()
	sig.HasLine, sig.LineSlope, sig.LineIntercept, sig.ITLAtPricingK = false, 0, 0, 0
	sig.LineSamples = 3
	emitter.RecordServiceRateSignal("ns", "m", "dec", sig)

	for _, name := range []string{constants.LLMSMITLLineSlope, constants.LLMSMITLLineIntercept, constants.LLMSMITLSecondsAtPricingK} {
		if got := countSeries(t, registry, name); got != 0 {
			t.Errorf("%s: %d series after the line was lost, want 0", name, got)
		}
	}
	if got, ok := gaugeAt(t, registry, constants.LLMSMITLLineSamples, map[string]string{
		constants.LabelVariantName: "dec",
	}); !ok || got != 3 {
		t.Errorf("samples = %v (present %v), want 3", got, ok)
	}

	// A variant that keeps no window publishes no count at all.
	sig.ITLWindowed = false
	emitter.RecordServiceRateSignal("ns", "m", "dec", sig)
	if got := countSeries(t, registry, constants.LLMSMITLLineSamples); got != 0 {
		t.Errorf("%d sample series for a variant with no window, want 0", got)
	}
}

// A rate whose source changes must not leave the old source behind: both
// would read as current. And a rate that disappears goes.
func TestRecordServiceRateSignalReplacesAChangedSource(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	sig := fullSignal()
	sig.RateSource = domain.ServiceRateSourceNeighbour
	emitter.RecordServiceRateSignal("ns", "m", "dec", sig)
	sig.RateSource, sig.Rate = domain.ServiceRateSourceMeasured, 5
	emitter.RecordServiceRateSignal("ns", "m", "dec", sig)

	if got := countSeries(t, registry, constants.LLMSMServiceRatePerReplica); got != 1 {
		t.Fatalf("%d rate series after the source changed, want 1", got)
	}
	if got, ok := gaugeAt(t, registry, constants.LLMSMServiceRatePerReplica, map[string]string{
		constants.LabelSource: "measured",
	}); !ok || got != 5 {
		t.Errorf("measured rate = %v (present %v), want 5", got, ok)
	}

	// The role is part of the identity too.
	sig.Role = domain.RoleBoth
	emitter.RecordServiceRateSignal("ns", "m", "dec", sig)
	if got := countSeries(t, registry, constants.LLMSMServiceRatePerReplica); got != 1 {
		t.Fatalf("%d rate series after the role changed, want 1", got)
	}

	sig.RateSource, sig.Rate = "", 0
	emitter.RecordServiceRateSignal("ns", "m", "dec", sig)
	if got := countSeries(t, registry, constants.LLMSMServiceRatePerReplica); got != 0 {
		t.Errorf("%d rate series after the rate was lost, want 0", got)
	}
}

// Deleting one variant, or one model, must not reach another model's variant
// of the same name in the same namespace: a variant name is unique within a
// model, not within a namespace.
func TestServiceRateDeletesAreModelScoped(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	for _, model := range []string{"m1", "m2"} {
		emitter.RecordServiceRateSignal("ns", model, "dec", fullSignal())
		emitter.RecordPricingK("ns", model, 0.68)
	}
	families := []string{
		constants.LLMSMITLLineSlope, constants.LLMSMITLLineIntercept, constants.LLMSMITLSecondsAtPricingK,
		constants.LLMSMITLLineSamples, constants.LLMSMServiceRatePerReplica,
	}
	only := func(model string, names ...string) {
		t.Helper()
		for _, name := range names {
			if got := countSeries(t, registry, name); got != 1 {
				t.Errorf("%s: %d series, want only %s's", name, got, model)
				continue
			}
			if !hasSeries(t, registry, name, constants.LabelModelName, model) {
				t.Errorf("%s: the surviving series is not %s's", name, model)
			}
		}
	}

	emitter.DeleteServiceRateSignal("ns", "m1", "dec")
	only("m2", families...)
	if got := countSeries(t, registry, constants.LLMSMPricingK); got != 2 {
		t.Errorf("a variant delete touched llmsm_pricing_k: %d series, want 2", got)
	}

	// Re-publish m1, then drop the whole of m2.
	emitter.RecordServiceRateSignal("ns", "m1", "dec", fullSignal())
	emitter.DeleteServiceRateSeriesForModel("ns", "m2")
	only("m1", append(families, constants.LLMSMPricingK)...)

	emitter.DeletePricingK("ns", "m1")
	if got := countSeries(t, registry, constants.LLMSMPricingK); got != 0 {
		t.Errorf("%d pricing k series after its delete, want 0", got)
	}
}

// After a delete, a rate re-published under a different source must not
// resurrect a deleted series nor delete a live one by stale bookkeeping.
func TestServiceRateBookkeepingForgetsADeletedVariant(t *testing.T) {
	emitter, registry := engineConfigEmitter(t)
	emitter.RecordServiceRateSignal("ns", "m", "dec", fullSignal())
	emitter.DeleteServiceRateSeriesForModel("ns", "m")

	sig := fullSignal()
	sig.RateSource = domain.ServiceRateSourceMeasured
	emitter.RecordServiceRateSignal("ns", "m", "dec", sig)
	if got := countSeries(t, registry, constants.LLMSMServiceRatePerReplica); got != 1 {
		t.Fatalf("%d rate series, want the one just published", got)
	}
}

func TestServiceRateEmitterIsNilSafe(t *testing.T) {
	saved := itlLineSlope
	savedK := pricingK
	itlLineSlope, pricingK = nil, nil
	defer func() { itlLineSlope, pricingK = saved, savedK }()

	var m *MetricsEmitter
	m.RecordServiceRateSignal("ns", "m", "v", fullSignal())
	m.DeleteServiceRateSignal("ns", "m", "v")
	m.RecordPricingK("ns", "m", 0.68)
	m.DeletePricingK("ns", "m")
	m.DeleteServiceRateSeriesForModel("ns", "m")
}
