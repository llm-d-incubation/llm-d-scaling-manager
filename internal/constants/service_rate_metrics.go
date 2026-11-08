package constants

// Service-rate metrics: the ITL(k) line and the per-replica service rate the
// saturation analyzer priced a variant on, published so the chain from a line
// to a replica target can be read off a dashboard rather than reconstructed
// from the derived-mu log line. Stage 3 of docs/proposals/signals-as-metrics.md.
// Diagnostic only -- nothing scales on them, and they are not a scaling input
// for KEDA or an HPA to threshold.
const (
	// LLMSMITLLineSlope is a per-variant gauge: A of the ITL(k) = A*k + B line
	// a decode variant would derive its service rate from this cycle, its own
	// or one borrowed from a sibling with the same engine configuration.
	// Seconds per unit of KV utilization. Absent while the variant has no line
	// positive at the pricing k, and while none of its replicas reports.
	// Labels: namespace, model_name, variant_name.
	LLMSMITLLineSlope = "llmsm_itl_line_slope"
	// LLMSMITLLineIntercept is B of the same line, in seconds: the inter-token
	// latency at an empty cache. Same labels and absence as LLMSMITLLineSlope.
	LLMSMITLLineIntercept = "llmsm_itl_line_intercept"
	// LLMSMITLSecondsAtPricingK is the same line evaluated at LLMSMPricingK: the
	// inter-token latency the derived service rate is priced at. Same labels
	// and absence as LLMSMITLLineSlope.
	LLMSMITLSecondsAtPricingK = "llmsm_itl_seconds_at_pricing_k"
	// LLMSMITLLineSamples is a per-variant gauge: how many (k, ITL) readings a
	// decode variant's OWN ITL window holds, while its replicas report.
	// Published while the variant has no line, zero included, because it is
	// what explains the absence.
	// Labels: namespace, model_name, variant_name.
	LLMSMITLLineSamples = "llmsm_itl_line_samples"
	// LLMSMServiceRatePerReplica is a per-variant gauge: the saturated
	// completion rate (requests/s) one replica is priced at -- the median over
	// the variant's own replicas that have one. One series per variant;
	// source names the path that answered (derived, borrowed-line, measured,
	// neighbour) for the median replica -- with an even count the rate
	// averages the central pair, and source is the lower one's.
	// Absent while no own replica has a rate. Labels: namespace, model_name,
	// variant_name, role, source.
	LLMSMServiceRatePerReplica = "llmsm_service_rate_per_replica"
	// LLMSMPricingK is a per-model gauge: the KV utilization the ITL line is
	// evaluated at, kvCacheThreshold x scaleUpThreshold clamped to the fitted
	// range -- 0.68 with the shipped defaults, not the 0.85 of k_sat. Labels:
	// namespace, model_name.
	LLMSMPricingK = "llmsm_pricing_k"
)
