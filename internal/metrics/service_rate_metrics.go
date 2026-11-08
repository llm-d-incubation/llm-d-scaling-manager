package metrics

import (
	"fmt"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

var (
	itlLineSlope          *prometheus.GaugeVec
	itlLineIntercept      *prometheus.GaugeVec
	itlSecondsAtPricingK  *prometheus.GaugeVec
	itlLineSamples        *prometheus.GaugeVec
	serviceRatePerReplica *prometheus.GaugeVec
	pricingK              *prometheus.GaugeVec

	// serviceRatePublished is the label set of the llmsm_service_rate_per_replica
	// series last published per namespace|model|variant. role and source are
	// part of the series identity, so a variant whose source changes from
	// neighbour to measured would otherwise keep both series, each reading as
	// current. Reset by registerServiceRateMetrics with the gauges it
	// describes.
	serviceRateMu        sync.Mutex
	serviceRatePublished = map[string]prometheus.Labels{}
)

// registerServiceRateMetrics creates and registers the service-rate gauges.
// Called from InitMetrics, after controllerInstance is set.
//
// The label keys are the ones every other per-variant family here uses --
// namespace, model_name, variant_name -- so these series join with
// llmsm_analyzer_target and llmsm_engine_config without a label_replace. The
// proposal's draft keys (model, variant) would have broken exactly that join.
func registerServiceRateMetrics(registry prometheus.Registerer) error {
	variantLabels := []string{constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName}
	rateLabels := []string{constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName,
		constants.LabelRole, constants.LabelSource}
	modelLabels := []string{constants.LabelNamespace, constants.LabelModelName}
	if controllerInstance != "" {
		variantLabels = append(variantLabels, constants.LabelControllerInstance)
		rateLabels = append(rateLabels, constants.LabelControllerInstance)
		modelLabels = append(modelLabels, constants.LabelControllerInstance)
	}
	gauge := func(name, help string, labels []string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	}
	itlLineSlope = gauge(constants.LLMSMITLLineSlope,
		"Slope A of the ITL(k) = A*k + B line a decode variant would derive its service rate from this cycle "+
			"(own or borrowed), in seconds per unit of KV utilization. Absent while the variant has no line positive "+
			"at llmsm_pricing_k, and while none of its replicas reports; see llmsm_itl_line_samples.",
		variantLabels)
	itlLineIntercept = gauge(constants.LLMSMITLLineIntercept,
		"Intercept B of the ITL(k) line a decode variant would derive its service rate from, in seconds: the "+
			"inter-token latency the line predicts at an empty cache. Same absence as llmsm_itl_line_slope.",
		variantLabels)
	itlSecondsAtPricingK = gauge(constants.LLMSMITLSecondsAtPricingK,
		"The ITL(k) line evaluated at llmsm_pricing_k, in seconds: the inter-token latency a derived service rate "+
			"would be priced at. Same absence as llmsm_itl_line_slope.",
		variantLabels)
	itlLineSamples = gauge(constants.LLMSMITLLineSamples,
		"How many (k, ITL) readings a decode variant's own ITL window holds, while its replicas report. Published "+
			"with no line too, zero included: a line needs enough readings spread far enough apart in k, and this is "+
			"what explains its absence.",
		variantLabels)
	serviceRatePerReplica = gauge(constants.LLMSMServiceRatePerReplica,
		"Saturated completion rate (requests/s) one replica of a variant is priced at: the median over its own "+
			"replicas, never a bridge. source is derived (from the variant's own ITL line), borrowed-line (from a "+
			"line a sibling with the same engine configuration fitted), measured (the variant's own saturated "+
			"window for the shape served) or neighbour (a neighbouring shape bucket's window); with an even "+
			"replica count the rate averages the central pair and source is the lower one's. "+
			"Absent while no replica has a rate. A diagnostic, not a scaling input.",
		rateLabels)
	pricingK = gauge(constants.LLMSMPricingK,
		"KV utilization the ITL line is evaluated at to price a decode replica: kvCacheThreshold x "+
			"scaleUpThreshold, clamped to the fitted range. 0.68 with the shipped defaults, not 0.85.",
		modelLabels)

	serviceRateMu.Lock()
	serviceRatePublished = map[string]prometheus.Labels{}
	serviceRateMu.Unlock()

	for name, c := range map[string]prometheus.Collector{
		constants.LLMSMITLLineSlope:          itlLineSlope,
		constants.LLMSMITLLineIntercept:      itlLineIntercept,
		constants.LLMSMITLSecondsAtPricingK:  itlSecondsAtPricingK,
		constants.LLMSMITLLineSamples:        itlLineSamples,
		constants.LLMSMServiceRatePerReplica: serviceRatePerReplica,
		constants.LLMSMPricingK:              pricingK,
	} {
		if err := registry.Register(c); err != nil {
			return fmt.Errorf("failed to register %s: %w", name, err)
		}
	}
	return nil
}

// serviceRateVariantLabels is the exact label set of one variant's line and
// sample series, so Record and Delete cannot disagree about it.
func serviceRateVariantLabels(namespace, modelID, variantName string) prometheus.Labels {
	labels := prometheus.Labels{
		constants.LabelNamespace:   namespace,
		constants.LabelModelName:   modelID,
		constants.LabelVariantName: variantName,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	return labels
}

func serviceRateKey(namespace, modelID, variantName string) string {
	return namespace + "\x00" + modelID + "\x00" + variantName
}

// RecordServiceRateSignal publishes one variant's service-rate signal and
// removes whatever part of it no longer holds: the line series when the
// variant lost its line, the sample count when it keeps no window, the rate
// when no replica has one.
//
// A rate whose role or source changed is SET under its new labels before the
// old series is deleted, so no scrape lands in a window where a value still in
// force is absent. Nil-guarded so tests that never call InitMetrics are a
// no-op.
func (m *MetricsEmitter) RecordServiceRateSignal(namespace, modelID, variantName string, sig domain.ServiceRateSignal) {
	if itlLineSlope == nil {
		return
	}
	labels := serviceRateVariantLabels(namespace, modelID, variantName)
	if sig.HasLine {
		itlLineSlope.With(labels).Set(sig.LineSlope)
		itlLineIntercept.With(labels).Set(sig.LineIntercept)
		itlSecondsAtPricingK.With(labels).Set(sig.ITLAtPricingK)
	} else {
		itlLineSlope.Delete(labels)
		itlLineIntercept.Delete(labels)
		itlSecondsAtPricingK.Delete(labels)
	}
	if sig.ITLWindowed {
		itlLineSamples.With(labels).Set(float64(sig.LineSamples))
	} else {
		itlLineSamples.Delete(labels)
	}

	key := serviceRateKey(namespace, modelID, variantName)
	serviceRateMu.Lock()
	defer serviceRateMu.Unlock()
	prev, hadPrev := serviceRatePublished[key]
	if sig.RateSource == "" {
		if hadPrev {
			serviceRatePerReplica.Delete(prev)
			delete(serviceRatePublished, key)
		}
		return
	}
	rate := serviceRateVariantLabels(namespace, modelID, variantName)
	rate[constants.LabelRole] = sig.Role
	rate[constants.LabelSource] = sig.RateSource
	serviceRatePerReplica.With(rate).Set(sig.Rate)
	if hadPrev && (prev[constants.LabelRole] != sig.Role || prev[constants.LabelSource] != sig.RateSource) {
		serviceRatePerReplica.Delete(prev)
	}
	serviceRatePublished[key] = rate
}

// DeleteServiceRateSignal removes every service-rate series of one variant.
// modelID is part of the match: a variant name is unique within a model, not
// within a namespace (see DeleteEngineConfig).
func (m *MetricsEmitter) DeleteServiceRateSignal(namespace, modelID, variantName string) {
	if itlLineSlope == nil {
		return
	}
	match := prometheus.Labels{
		constants.LabelNamespace:   namespace,
		constants.LabelModelName:   modelID,
		constants.LabelVariantName: variantName,
	}
	for _, g := range []*prometheus.GaugeVec{
		itlLineSlope, itlLineIntercept, itlSecondsAtPricingK, itlLineSamples, serviceRatePerReplica,
	} {
		g.DeletePartialMatch(match)
	}
	serviceRateMu.Lock()
	delete(serviceRatePublished, serviceRateKey(namespace, modelID, variantName))
	serviceRateMu.Unlock()
}

// RecordPricingK publishes the pricing k a model's decode variants are priced
// at. Nil-guarded like RecordServiceRateSignal.
func (m *MetricsEmitter) RecordPricingK(namespace, modelID string, k float64) {
	if pricingK == nil {
		return
	}
	pricingK.With(pricingKLabels(namespace, modelID)).Set(k)
}

// DeletePricingK removes one model's llmsm_pricing_k series.
func (m *MetricsEmitter) DeletePricingK(namespace, modelID string) {
	if pricingK == nil {
		return
	}
	pricingK.Delete(pricingKLabels(namespace, modelID))
}

func pricingKLabels(namespace, modelID string) prometheus.Labels {
	labels := prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelModelName: modelID,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	return labels
}

// DeleteServiceRateSeriesForModel removes every service-rate series of a model
// instance, llmsm_pricing_k included, across all its variants. Used when a model
// stops being reconciled entirely.
func (m *MetricsEmitter) DeleteServiceRateSeriesForModel(namespace, modelID string) {
	if itlLineSlope == nil {
		return
	}
	match := prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelModelName: modelID,
	}
	for _, g := range []*prometheus.GaugeVec{
		itlLineSlope, itlLineIntercept, itlSecondsAtPricingK, itlLineSamples, serviceRatePerReplica, pricingK,
	} {
		g.DeletePartialMatch(match)
	}
	prefix := namespace + "\x00" + modelID + "\x00"
	serviceRateMu.Lock()
	for key := range serviceRatePublished {
		if strings.HasPrefix(key, prefix) {
			delete(serviceRatePublished, key)
		}
	}
	serviceRateMu.Unlock()
}
