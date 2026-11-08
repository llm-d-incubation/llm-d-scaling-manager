package registration

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/prometheus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

var _ = Describe("RegisterQueueingModelQueries", func() {
	var (
		ctx      context.Context
		registry *source.SourceRegistry
		mockAPI  *mockPrometheusAPI
	)

	BeforeEach(func() {
		ctx = context.Background()
		registry = source.NewSourceRegistry()
		mockAPI = &mockPrometheusAPI{}
		metricsSource := prometheus.NewPrometheusSource(ctx, mockAPI, prometheus.DefaultPrometheusSourceConfig())
		err := registry.Register("prometheus", metricsSource)
		Expect(err).NotTo(HaveOccurred())
		RegisterQueueingModelQueries(registry)
	})

	Describe("average ITL query", func() {
		It("queries vllm:inter_token_latency_seconds histogram", func() {
			q := registry.Get("prometheus").QueryList().Get(QueryAvgITL)
			Expect(q).NotTo(BeNil())
			// vLLM exports inter-token latency as inter_token_latency_seconds.
			Expect(q.Template).To(ContainSubstring("vllm:inter_token_latency_seconds_sum"))
			Expect(q.Template).To(ContainSubstring("vllm:inter_token_latency_seconds_count"))
		})

		It("does not use the old time_per_output_token_seconds metric name", func() {
			q := registry.Get("prometheus").QueryList().Get(QueryAvgITL)
			Expect(q).NotTo(BeNil())
			Expect(q.Template).NotTo(ContainSubstring("time_per_output_token_seconds"))
		})
	})

	Describe("average service time query", func() {
		It("reads vLLM's inference time, which already excludes the queue wait", func() {
			q := registry.Get("prometheus").QueryList().Get(QueryAvgServiceTime)
			Expect(q).NotTo(BeNil())
			Expect(q.Template).To(ContainSubstring("vllm:request_inference_time_seconds_sum"))
			Expect(q.Template).To(ContainSubstring("vllm:request_inference_time_seconds_count"))
		})

		It("does not fall back to end-to-end latency on vLLM", func() {
			// e2e includes queue wait, which rises when the fleet is behind. An
			// estimate built on it varies with capacity, which is the whole thing
			// the service-time metric exists to avoid.
			q := registry.Get("prometheus").QueryList().Get(QueryAvgServiceTime)
			Expect(q).NotTo(BeNil())
			Expect(q.Template).NotTo(ContainSubstring("e2e_request_latency"))
		})

		It("reconstructs it on SGLang by subtracting queue time from end-to-end", func() {
			// SGLang publishes no single service-time metric but does publish both
			// halves. A dropped subtraction here would silently substitute e2e --
			// the exact quantity that must not be used -- and nothing else in the
			// suite would notice, since the generic checks only assert that SGLang
			// templates carry no vllm: names.
			q := registry.Get("prometheus").QueryList().Get(
				EngineQuery(inferenceengine.EngineSGLang, QueryAvgServiceTime))
			Expect(q).NotTo(BeNil())
			Expect(q.Template).To(ContainSubstring("sglang:e2e_request_latency_seconds_sum"))
			Expect(q.Template).To(ContainSubstring("sglang:queue_time_seconds_sum"))
			Expect(q.Template).To(ContainSubstring("clamp_min"),
				"two independently-rated histograms can cross, so the difference must not go negative")
		})
	})
})
