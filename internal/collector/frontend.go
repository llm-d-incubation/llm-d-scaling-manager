package collector

import (
	"context"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// frontendReading is an instance's API-server time as read
// (registration/frontend.go): each figure with its presence bit.
type frontendReading struct {
	httpGeneration, httpRender, renderRate, e2e                     float64
	httpGenerationKnown, httpRenderKnown, renderRateKnown, e2eKnown bool
}

// extractFrontend reads the API server's series into the instances the
// model-scoped queries already found, as extractTTFTInputs reads the TTFT
// inputs: supplementary, never defining an instance; a failed or unissued
// query leaves its figure unknown. The HTTP series carry no model and are
// attached by instance alone, so another model's Pod in the namespace is
// never one of these instances; vLLM's end-to-end mean carries the model and
// is cut down to it.
func (c *ReplicaMetricsCollector) extractFrontend(ctx context.Context, modelID, namespace string,
	results map[string]*source.MetricResult, podData map[string]*podMetricData) {
	for _, q := range []struct {
		query string
		field func(*frontendReading) (*float64, *bool)
	}{
		{registration.QueryAvgHTTPGenerationTime, func(f *frontendReading) (*float64, *bool) {
			return &f.httpGeneration, &f.httpGenerationKnown
		}},
		{registration.QueryAvgHTTPRenderTime, func(f *frontendReading) (*float64, *bool) { return &f.httpRender, &f.httpRenderKnown }},
		{registration.QueryHTTPRenderRate, func(f *frontendReading) (*float64, *bool) { return &f.renderRate, &f.renderRateKnown }},
	} {
		c.extractInstanceNonNegative(ctx, namespace, results[q.query], podData, func(d *podMetricData) (*float64, *bool) {
			return q.field(&d.frontend)
		})
	}
	c.extractNonNegative(ctx, modelID, namespace, results[registration.QueryAvgE2ELatency], podData,
		func(d *podMetricData) (*float64, *bool) { return &d.frontend.e2e, &d.frontend.e2eKnown })
}

// extractInstanceNonNegative is extractNonNegative for a series that carries
// no model: every series is attached by its instance, and only to an
// instance already found.
func (c *ReplicaMetricsCollector) extractInstanceNonNegative(ctx context.Context, namespace string,
	result *source.MetricResult, podData map[string]*podMetricData, field func(*podMetricData) (*float64, *bool)) {
	for _, value := range finiteValues(result) {
		if value.Value < 0 {
			continue
		}
		instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
		if instanceKey == "" || podData[instanceKey] == nil {
			continue
		}
		v, known := field(podData[instanceKey])
		*v, *known = value.Value, true
	}
}

// setFrontend sets the row's API-server figures from what was read: the
// frontend where both of its means were, the render mean and rate each where
// it was.
func (f frontendReading) setFrontend(rm *domain.ReplicaMetrics) {
	if f.httpGenerationKnown && f.e2eKnown {
		rm.AvgFrontendTime, rm.AvgFrontendTimeKnown = f.httpGeneration-f.e2e, true
	}
	if f.httpRenderKnown {
		rm.AvgRenderTime, rm.AvgRenderTimeKnown = f.httpRender, true
	}
	if f.renderRateKnown {
		rm.RenderRate, rm.RenderRateKnown = f.renderRate, true
	}
}

// mergeFrontend sets one pod's API-server figures from its ranks', each
// known only when every rank read it: the frontend weighted by request rate,
// the render rate summed and the render mean weighted by it.
func mergeFrontend(pod *domain.ReplicaMetrics, group []domain.ReplicaMetrics) {
	pod.AvgFrontendTime, pod.AvgFrontendTimeKnown = mergeKnownMean(group, func(m domain.ReplicaMetrics) (float64, bool) {
		return m.AvgFrontendTime, m.AvgFrontendTimeKnown
	}, nil)
	pod.RenderRate, pod.RenderRateKnown = mergeKnownSum(group, func(m domain.ReplicaMetrics) (float64, bool) {
		return m.RenderRate, m.RenderRateKnown
	})
	pod.AvgRenderTime, pod.AvgRenderTimeKnown = mergeKnownMean(group, func(m domain.ReplicaMetrics) (float64, bool) {
		return m.AvgRenderTime, m.AvgRenderTimeKnown
	}, func(m domain.ReplicaMetrics) float64 { return m.RenderRate })
}
