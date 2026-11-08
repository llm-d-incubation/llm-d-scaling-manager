package constants

// Load-distribution metrics: how evenly the router spreads a model's load over
// the ready replicas of each role (internal/signals/balance). Diagnostic only:
// the analyzers price capacity per replica and demand per fleet, nothing
// scales on these, and they are not a scaling input for KEDA or an HPA to
// threshold. Labels: namespace (the model's), model_name, role.
const (
	// LLMSMLoadReadyReplicas is a per-role gauge: how many ready replicas the
	// other load-distribution figures are over, warm-pool bridges included.
	LLMSMLoadReadyReplicas = "llmsm_load_ready_replicas"
	// LLMSMLoadKvUsageMax is a per-role gauge: the highest KV cache utilization
	// (0-1) of any ready replica, on its busiest engine rank, one-minute peak.
	LLMSMLoadKvUsageMax = "llmsm_load_kv_usage_max"
	// LLMSMLoadKvUsageMean is the mean of the same per-replica figure over the
	// role's ready replicas.
	LLMSMLoadKvUsageMean = "llmsm_load_kv_usage_mean"
	// LLMSMLoadKvMaxToMeanRatio is LLMSMLoadKvUsageMax / LLMSMLoadKvUsageMean: 1 on
	// an evenly loaded role, up to the replica count when one replica holds
	// all the load. Absent with fewer than two ready replicas or a mean of 0.
	LLMSMLoadKvMaxToMeanRatio = "llmsm_load_kv_max_to_mean_ratio"
	// LLMSMLoadQueueMaxToMeanRatio is the same ratio for the waiting queue
	// (requests waiting in a replica's engine). Absent with fewer than two
	// ready replicas or nothing waiting.
	LLMSMLoadQueueMaxToMeanRatio = "llmsm_load_queue_max_to_mean_ratio"
	// LLMSMLoadIdleReplicasWhileSaturated is a per-role gauge: the ready
	// replicas that are idle (KV under 0.30, nothing waiting, Ready for 2m) while another
	// replica of the role is saturated (KV at or over kvCacheThreshold, or a
	// queue at queueLengthThreshold except on the decode role of a model with
	// a prefill variant). 0 otherwise.
	LLMSMLoadIdleReplicasWhileSaturated = "llmsm_load_idle_replicas_while_saturated"
	// LLMSMLoadImbalanced is a per-role gauge: 1 once the role has had an idle
	// replica beside a saturated one on every cycle for 2m, until it reads
	// balanced on every cycle for 2m; 0 otherwise.
	LLMSMLoadImbalanced = "llmsm_load_imbalanced"
)
