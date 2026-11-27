package collector

import (
	"cmp"
	"regexp"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// routerEndpointQueries are the queries CollectRouterLatency adds, while a
// learner recording is on, for domain.RouterLatency.Endpoints and
// PrefixProducers. The producers also read the predicted and
// best-available pair, which the recording asks for already and so is not
// listed here.
var routerEndpointQueries = []string{
	registration.QuerySLORouterPrefixBestPredicted, registration.QuerySLORouterPrefixPrompt,
	registration.QuerySLORouterPrefixRequests, registration.QuerySLORouterEndpointPlacements,
	registration.QuerySLORouterEndpointQueue, registration.QuerySLORouterEndpointInFlightRequests,
	registration.QuerySLORouterEndpointInFlightTokens,
}

// routerPodLabel is the label the scrape stamps on a router series: the
// router Pod it was scraped from. A scrape that sets none groups every
// replica under "", read as one router.
const routerPodLabel = "pod"

// endpointRank is the rank suffix the router gives an endpoint's name: one
// endpoint per target port of a Pod, <pod>-rank-<n>.
var endpointRank = regexp.MustCompile(`-rank-[0-9]+$`)

// endpointPod is the model-server Pod an endpoint name belongs to.
func endpointPod(endpoint string) string {
	return endpointRank.ReplaceAllString(endpoint, "")
}

// routerEndpoints is the per-endpoint and per-producer figures of the router
// Pods that serve modelID (routerServingPods). Both nil with the placement
// query failed or no serving router Pod; a figure whose own query failed is
// left unknown.
func routerEndpoints(results map[string]*source.MetricResult, modelID string) ([]domain.RouterEndpoint, []domain.RouterPrefixProducer) {
	placements := results[registration.QuerySLORouterEndpointPlacements]
	if placements == nil || placements.HasError() {
		return nil, nil
	}
	serving := routerServingPods(placements, modelID)
	if len(serving) == 0 {
		return nil, nil
	}

	byPod := map[string]*domain.RouterEndpoint{}
	row := func(pod string) *domain.RouterEndpoint {
		if byPod[pod] == nil {
			byPod[pod] = &domain.RouterEndpoint{Pod: pod}
		}
		return byPod[pod]
	}
	for _, v := range readableValues(placements) {
		// The router names the Pod here; a rank-suffixed name is folded into
		// its Pod as the gauges' are.
		pod := endpointPod(v.Labels["endpoint_name"])
		if !serving[v.Labels[routerPodLabel]] || eppSeriesModel(v.Labels) != modelID || pod == "" {
			continue
		}
		e := row(pod)
		e.PlacementRate += v.Value
		e.PlacementKnown = true
	}
	// Every router replica reads the same model servers: the queue is the
	// highest any serving router read, never the sum.
	for pod, rate := range endpointGauge(results[registration.QuerySLORouterEndpointQueue], serving, "model_server_endpoint", false) {
		e := row(pod)
		e.QueueSize, e.QueueSizeKnown = rate, true
	}
	// Each router replica holds its own requests in flight: summed.
	for pod, n := range endpointGauge(results[registration.QuerySLORouterEndpointInFlightRequests], serving, "endpoint_name", true) {
		e := row(pod)
		e.InFlightRequests, e.InFlightRequestsKnown = n, true
	}
	for pod, n := range endpointGauge(results[registration.QuerySLORouterEndpointInFlightTokens], serving, "endpoint_name", true) {
		e := row(pod)
		e.InFlightTokens, e.InFlightTokensKnown = n, true
	}

	var endpoints []domain.RouterEndpoint
	for _, e := range byPod {
		endpoints = append(endpoints, *e)
	}
	// Sorted: the input is recorded every cycle, and a map's order would
	// make two identical cycles record different bytes.
	slices.SortFunc(endpoints, func(a, b domain.RouterEndpoint) int { return cmp.Compare(a.Pod, b.Pod) })
	return endpoints, routerPrefixProducers(results, serving)
}

// routerServingPods is the router Pods that serve modelID alone: those
// whose placements of a positive rate over the window name modelID and no
// other model. A replica that routes nothing (a standby) or routes another
// model too is not one, so its series never reach modelID.
func routerServingPods(placements *source.MetricResult, modelID string) map[string]bool {
	models := map[string]map[string]bool{}
	for _, v := range readableValues(placements) {
		model := eppSeriesModel(v.Labels)
		if !(v.Value > 0) || model == "" || v.Labels["endpoint_name"] == "" {
			continue
		}
		pod := v.Labels[routerPodLabel]
		if models[pod] == nil {
			models[pod] = map[string]bool{}
		}
		models[pod][model] = true
	}
	serving := map[string]bool{}
	for pod, m := range models {
		if len(m) == 1 && m[modelID] {
			serving[pod] = true
		}
	}
	return serving
}

// endpointGauge is a per-endpoint router gauge of the serving router Pods,
// by model-server Pod: an endpoint's ranks summed within one router Pod,
// then summed over the router Pods or the highest of them. Nil with the
// query failed.
func endpointGauge(result *source.MetricResult, serving map[string]bool, label string, sum bool) map[string]float64 {
	if result == nil || result.HasError() {
		return nil
	}
	byRouter := map[string]map[string]float64{}
	for _, v := range readableValues(result) {
		router := v.Labels[routerPodLabel]
		endpoint := v.Labels[label]
		if !serving[router] || endpoint == "" {
			continue
		}
		pod := endpointPod(endpoint)
		if byRouter[pod] == nil {
			byRouter[pod] = map[string]float64{}
		}
		byRouter[pod][router] += v.Value
	}
	out := map[string]float64{}
	for pod, routers := range byRouter {
		var total, highest float64
		for _, n := range routers {
			total += n
			highest = max(highest, n)
		}
		if sum {
			out[pod] = total
		} else {
			out[pod] = highest
		}
	}
	return out
}

// routerPrefixProducers is the prefix producers' counts of the serving
// router Pods, per producer (plugin_name, endpoint_role), summed over the
// router Pods (each counts its own requests), sorted. Nil with any of the
// five queries failed, and a producer is left out unless every one of the
// five has a series for it: a producer's rates are one reading, and a rate
// read as zero because its series was missing would read as a producer
// that counted none.
func routerPrefixProducers(results map[string]*source.MetricResult, serving map[string]bool) []domain.RouterPrefixProducer {
	type field struct {
		query string
		set   func(p *domain.RouterPrefixProducer, v float64)
	}
	fields := []field{
		{registration.QuerySLORouterPrefixRequests, func(p *domain.RouterPrefixProducer, v float64) { p.RequestRate += v }},
		{registration.QuerySLORouterPrefixPrompt, func(p *domain.RouterPrefixProducer, v float64) { p.PromptTokenRate += v }},
		{registration.QuerySLORouterPrefixPredicted, func(p *domain.RouterPrefixProducer, v float64) { p.PredictedTokenRate += v }},
		{registration.QuerySLORouterPrefixBestPredicted, func(p *domain.RouterPrefixProducer, v float64) { p.BestPredictedTokenRate += v }},
		{registration.QuerySLORouterPrefixBestAvailable, func(p *domain.RouterPrefixProducer, v float64) { p.BestAvailableTokenRate += v }},
	}
	for _, f := range fields {
		if r := results[f.query]; r == nil || r.HasError() {
			return nil
		}
	}
	type key struct{ plugin, role string }
	byProducer := map[key]*domain.RouterPrefixProducer{}
	seen := map[key]map[string]bool{}
	for _, f := range fields {
		for _, v := range readableValues(results[f.query]) {
			if !serving[v.Labels[routerPodLabel]] {
				continue
			}
			k := key{v.Labels["plugin_name"], v.Labels["endpoint_role"]}
			if byProducer[k] == nil {
				byProducer[k] = &domain.RouterPrefixProducer{Plugin: k.plugin, Role: k.role}
				seen[k] = map[string]bool{}
			}
			f.set(byProducer[k], v.Value)
			seen[k][f.query] = true
		}
	}
	var out []domain.RouterPrefixProducer
	for k, p := range byProducer {
		if len(seen[k]) == len(fields) {
			out = append(out, *p)
		}
	}
	slices.SortFunc(out, func(a, b domain.RouterPrefixProducer) int {
		return cmp.Or(cmp.Compare(a.Plugin, b.Plugin), cmp.Compare(a.Role, b.Role))
	})
	return out
}
