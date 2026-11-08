package domain

import (
	"math"
	"time"
)

// This file holds the ANALYZER-AGNOSTIC replica signal.
//
// It lived in saturation_analyzer.go, which read as though it belonged to one
// analyzer. It does not: every analyzer that works from measured rows consumes
// ReplicaMetrics (saturation, throughput), the collector produces it, and the
// capacity-build step depends on what it carries. A shared type named after one
// consumer invites exactly the change this split exists to prevent -- a rule
// written into one analyzer and silently absent from the others.

// ReplicaMetrics holds one scale-target replica's measured signal.
//
// It is per *replica*, in the same units as domain.VariantMetadata and as the
// replica targets the optimizer produces: one record per pod, or for LWS per
// group (only leader pods emit engine metrics, so a leader's record is its
// group's). A data-parallel pod exposes one metrics endpoint per DP rank and is
// scraped as several series keyed pod_name:port; the collector merges those
// into this one record (collector.collapseToPods) so that counting these
// records yields a replica count and the capacity derived from one is a
// per-replica capacity.
//
// It carries *signal only*. Variant identity (cost, accelerator, role) belongs
// to the discovery step and reaches consumers via domain.VariantMetadata; it is
// not repeated on every replica record.
type ReplicaMetrics struct {
	PodName      string
	KvCacheUsage float64 // KV cache utilization (0.0-1.0)
	QueueLength  int     // Number of requests waiting
	VariantName  string  // Name of the variant this replica belongs to
	Namespace    string
	ModelID      string // Model ID for grouping variants

	// FromWarmPool marks a record that came from a BRIDGE: a warm pool Pod lent
	// to this variant while it is short, rather than one of its own replicas.
	//
	// Its load is this variant's load and counts toward DEMAND like any other.
	// Its capacity is not this variant's SUPPLY: the Pod is borrowed and goes
	// back when the ordinary replicas arrive, so counting it would tell the
	// optimizer the fleet is already big enough and suppress the scale-up the
	// bridge exists to cover -- after which the replicas that would release the
	// Pod are the ones it just prevented.
	//
	// So the two aggregations treat it differently, and this flag is what lets
	// them. What it is worth is measured separately and published for the
	// retained-pool switching decision; see decision.WarmPoolSupply.
	//
	// A BOOL, NOT A POOL NAME, AND THAT RESTS ON AN INVARIANT: a variant is
	// warmed by AT MOST ONE POOL. Its bridges therefore all come from the same
	// pool, run at that pool's --gpu-memory-utilization, and are directly
	// comparable -- which is what makes one median over them meaningful and what
	// lets decision.WarmPoolSupply key by variant alone.
	//
	// Different variants of one model MAY sit in different pools, including the
	// prefill and decode of the same model: a variant is named by its
	// ScaledObject, role included, and every figure here is per variant, so those
	// aggregate independently and never meet.
	//
	// Warming ONE variant from two pools would break this -- their bridges would
	// carry genuinely different capacities and the median would describe neither
	// -- and the pool layer makes that unreachable rather than merely unlikely.
	// A variant names at most one pool, in the `warmPool` trigger metadata key,
	// and warmpool.VariantsFor hands a variant only to the pool it names (or to
	// the single pool, when a namespace has exactly one and there is nothing to
	// disambiguate). With several pools declared, a variant naming none is
	// Unassignable: it gets no warm copy at all, and is reported.
	FromWarmPool bool
	// LentSince is when the warm pool lent this Pod to this variant: the
	// first pass of the pool that published the current lend
	// (decision.BridgeStore). Zero when the record is not a bridge, or the
	// lend time is unknown.
	//
	// A Pod handed back and lent again to the same variant keeps its name,
	// its variant and the Ready time it had in the pool, so nothing else
	// tells the new lend from a missed scrape of the old one; the settle
	// tracker (internal/signals/settle) reads a new LentSince as a new lend.
	LentSince time.Time

	// Ready is the Pod's own readiness condition, read from the Pod and not
	// inferred from the fact that it answered a scrape.
	//
	// The two are not the same and the gap is not small: an engine serves
	// /metrics as soon as its HTTP server is up, which is BEFORE it passes
	// readiness, so a starting replica reports for some seconds while no Service
	// and no EPP will route to it.
	//
	// The gap matters wherever the analyzer asks what a Pod's traffic says:
	//   - the warm pool asks how many of a variant's own replicas are serving,
	//     to decide whether the Pod it lent can go home; a replica reporting but
	//     not yet in the rotation would hand the traffic back to nothing;
	//   - the busiest-queue load and the decode-saturation test that gates
	//     prefill (roleSaturated) skip a Pod that is not Ready: nothing is
	//     routed to it, so its queue is left over or frozen;
	//   - a saturated reading from it is not recorded, neither as the variant's
	//     k2 (P1-obs-unadmitted) nor as its saturated throughput, and does not
	//     price its cycle either (only a warm-pool bridge's reading does).
	// It does not remove the Pod from capacity or demand: a Pod that is
	// starting still holds its GPU, its KV cache is real, and the analyzer
	// counts both, and files its cache in the capacity store.
	//
	// The collector fails open: a Pod whose listing cannot be read is Ready.
	// Each such listing is counted, llmsm_metrics_collection_errors_total with
	// query_type=pod_list.
	Ready bool

	// Revision is the pod template revision the Pod was created from: a
	// LeaderWorkerSet's template-revision-hash, a Deployment's
	// pod-template-hash, a StatefulSet's controller-revision-hash. "" when the
	// Pod carries none or could not be read.
	//
	// A variant whose replicas report more than one is mid-rollout, whatever
	// its scale target's status says: the analyzers then hold every reading
	// back from the variant's windows and ITL lines, since it may be an old
	// pod's (RevisionsMixed). The status cannot say it for a
	// LeaderWorkerSet whose controller has acted on some groups but not yet
	// set UpdateInProgress, or is down.
	Revision string

	// StartSeconds is how long this replica took to become Ready: the Pod's
	// Ready condition LastTransitionTime less its CreationTimestamp. Zero when
	// the Pod is not ready, when either timestamp is missing, or when the
	// difference is not credible (see collector.podStartSeconds).
	//
	// It is the dead time the demand floor projects the backlog over. Measured
	// per replica and aggregated per VARIANT by the analyzer, because it is an
	// image, a set of engine flags and a node -- two variants of one model
	// routinely differ.
	StartSeconds float64

	// ReadySince is when this replica last became Ready: the Pod's Ready
	// condition LastTransitionTime. Zero when the Pod is not Ready, the time
	// is unset, or the Pod listing could not be read; zero means unknown.
	// For a LeaderWorkerSet group, whose row is its leader's, it is the latest
	// Ready time over the leader and its live, Ready workers, so a worker's
	// restart moves it; a worker not Ready yet adds nothing.
	//
	// The load-balance diagnostic (internal/signals/balance) reads it: a
	// replica that became Ready a moment ago has had no time to receive work,
	// so its empty KV cache says nothing about the router.
	ReadySince time.Time

	// KvCacheUsageKnown is set when KvCacheUsage was read from a present,
	// finite series, so a zero means an empty cache and not a missing or NaN
	// reading. A pod whose ranks did not all report one is not known.
	KvCacheUsageKnown bool

	// Metadata contains freshness information (optional)
	Metadata *ReplicaMetricsMetadata `json:"metadata,omitempty"`

	// --- Fields for Saturation Analyzer V2 ---

	// NumGpuBlocks is the total number of KV cache blocks allocated on GPU.
	// Sourced from vllm:cache_config_info label "num_gpu_blocks".
	// Zero value means cache_config_info metric is not available.
	NumGpuBlocks int64

	// BlockSize is the number of tokens per KV cache block.
	// Sourced from vllm:cache_config_info label "block_size".
	// Zero value means cache_config_info metric is not available.
	BlockSize int64

	// TotalKvCapacityTokens is NumGpuBlocks × BlockSize (total token slots).
	// Computed by the collector after parsing cache_config_info labels.
	// Zero value means capacity data is unavailable.
	TotalKvCapacityTokens int64

	// EngineRanks is how many data-parallel engine ranks this record covers.
	// Zero means unknown, and is read as one (see Ranks).
	//
	// A pod can run several ranks in two layouts, and both arrive here as one
	// record. With one API server per rank, each rank is its own scrape target
	// (pod:port), and collapseToPods adds them up. With several ranks behind
	// ONE API server, the ranks share a scrape target and are told apart only
	// by vLLM's engine label; the collector counts the cache_config_info
	// series of the instance, one per rank, and the per-pod queries fold the
	// ranks the same way the merge would.
	//
	// It matters because some engine settings are PER RANK: --max-num-seqs
	// and --max-num-batched-tokens bound one engine, not the pod. A pod of
	// eight ranks admits eight times max_num_seqs sequences. The KV capacity
	// above is already the whole pod's in both layouts.
	EngineRanks int

	// MaxRankQueueLength and MaxRankKvCacheUsage are the waiting queue and
	// the KV cache utilization of the record's busiest rank: the largest one-
	// minute peak over its ranks, where QueueLength is their sum and
	// KvCacheUsage their mean. Zero when not collected. Read them through
	// BusiestRankQueue and BusiestRankKvUsage, which also cover a record of
	// one rank and a record collected without them.
	//
	// The sum and the mean are what the pod holds, and price demand and
	// utilization. They do not say whether one rank is full. The saturation
	// guards ask exactly that: whether a replica is full and queued, as
	// queueLengthThreshold and kvCacheThreshold define it for ONE engine.
	// A pod whose eight ranks each have one request waiting is not
	// saturated, though its queue sums to eight; a pod with one full rank
	// and seven idle ones has a saturated rank, though its mean reads 0.125.
	MaxRankQueueLength  int
	MaxRankKvCacheUsage float64

	// CapacityQueueLength and MaxRankCapacityQueueLength are the part of
	// QueueLength and MaxRankQueueLength that waits for scheduling capacity:
	// vLLM's num_requests_waiting_by_reason{reason="capacity"}, published
	// since v0.20.0. The rest of the waiting count is "deferred": requests the
	// scheduler skipped for a transient reason, which on the decode side of a
	// prefill/decode split is mostly requests waiting for their KV to arrive
	// from prefill. CapacityQueueKnown says the series was collected; without
	// it (an older vLLM, SGLang) both are zero and mean nothing. Read the
	// busiest rank's through BusiestRankCapacityQueue.
	CapacityQueueLength        int
	MaxRankCapacityQueueLength int
	CapacityQueueKnown         bool

	// TokensInUse is the derived current token demand on this replica.
	// Computed as KvCacheUsage × TotalKvCapacityTokens.
	// Zero when TotalKvCapacityTokens is unavailable.
	TokensInUse int64

	// AvgOutputTokens is the average generation tokens per request on this replica.
	// Derived from rate(generation_tokens_sum) / rate(generation_tokens_count).
	// Used by saturation V2 for token-demand estimation (k2 derivation) and by
	// the queueing model analyzer for RequestSize and service rate computation.
	// Zero when metrics are unavailable.
	AvgOutputTokens float64

	// AvgOutputTokensSquared is a lower bound on the mean SQUARED generation
	// length per request on this replica, over the [5m] window
	// AvgOutputTokens is taken over: the least E[O^2] that the output-length
	// histogram (vllm:request_generation_tokens_bucket, or SGLang's
	// sglang:generation_tokens_histogram_bucket) and AvgOutputTokens allow
	// (SquaredMeanLowerBound). With AvgOutputTokens it gives the
	// spread of output lengths, D = E[O^2]/E[O]^2, which sets the KV a
	// resident request holds: a long request stays resident longer, so
	// KVreq = ILeff + D*OL/2, not ILeff + OL/2 (issue ev-shindin/llm-scaling-manager#276).
	//
	// A lower bound, so a spread priced from it moves the KV per request
	// towards the truth and never past it; equal lengths read as D = 1.
	// SGLang publishes its histogram behind --collect-tokens-histogram, the
	// flag its AvgOutputTokens needs too.
	AvgOutputTokensSquared float64
	// AvgOutputTokensSquaredKnown is set when AvgOutputTokensSquared was
	// read: the histogram had a +Inf bucket, counted requests, and agreed
	// with AvgOutputTokens. A pod whose ranks did not all report one is not
	// known. Unknown, the KV per request is priced as before, from the mean
	// alone.
	AvgOutputTokensSquaredKnown bool

	// AvgOutputTokensRecent is the same quantity over a SHORT window: the mean
	// output tokens per request among requests that completed in roughly the
	// last minute.
	//
	// It exists because AvgOutputTokens is count-weighted over [5m], and a
	// count-weighted mean is the wrong statistic for a bimodal workload: after
	// a 6000 -> 250 output switch, the rare long stragglers dominate the mean
	// and a five-minute window keeps them in it for five minutes after they
	// stop arriving. Measured per decode pod across one such switch, the [5m]
	// figure decayed 3750 -> 2814 -> 2382 -> 2278 -> 2136 -> 250 while the
	// arriving work was 250 throughout; the [1m] form converged in about a
	// minute.
	//
	// Read as the derived mu's divisor (mu = seqs / (ITL * OL)), where the error
	// passes straight through and cost a 4x over-order, and -- paired with
	// AvgInputTokensRecent, never alone -- as the shape mu is priced at while a
	// shape change is outstanding. Everything else keeps AvgOutputTokens: a
	// short window everywhere would only make the shape noisier on a quiet
	// fleet.
	//
	// Zero when the engine does not publish it or the pod completed nothing in
	// the window; the divisor then falls back to AvgOutputTokens.
	AvgOutputTokensRecent float64

	// AvgInputTokensRecent is AvgInputTokens over the SAME short window as
	// AvgOutputTokensRecent, and exists only so the two can be read together.
	//
	// KVreq = ILeff + OL/2 is dominated by the prompt only while the prompt is
	// the large half. At 6000 in / 1000 out, OL/2 is 500 of 6500 -- 8%. At
	// 1000 in / 4000 out it is 2000 of 3000 -- 67%. A shape swap crosses from
	// the first to the second, which is exactly when a one-sided short window
	// is worst: taking the [1m] output while the prompt still carries the [5m]
	// average prices this shape's generation on top of the previous shape's
	// prompt, and that sum is larger than either real shape.
	//
	// Measured across one 6000/1000 -> 1000/4000 switch: the divisor took the
	// [1m] output and reached 4000 within two cycles while KVreq still read
	// 5896 against a settled 3000, so the derived mu read 1.57 req/s against a
	// settled 2.76 and the floor ordered 7 decode replicas where 3 was right.
	//
	// Zero when the engine does not publish it or the pod completed nothing in
	// the window; the caller then falls back to AvgInputTokens.
	AvgInputTokensRecent float64

	// AvgInputTokens is the average prompt tokens per request on this replica.
	// Derived from rate(prompt_tokens_sum) / rate(prompt_tokens_count).
	// Used by saturation V2 for token-demand estimation (k2 derivation) and by
	// the queueing model analyzer for RequestSize and service rate computation.
	// Zero when metrics are unavailable.
	AvgInputTokens float64

	// PrefixCacheHitRate is the fraction of prefix cache queries that were hits (0.0-1.0).
	// Derived from rate(vllm:prefix_cache_hits_total[5m]) / rate(vllm:prefix_cache_queries_total[5m]).
	// Used to reduce estimated input token demand for scheduler-queued requests.
	// Zero when prefix caching is disabled or metrics are unavailable.
	PrefixCacheHitRate float64

	// AvgServiceTime is how long a request occupies this replica, in seconds,
	// EXCLUDING time spent waiting in the queue.
	//
	// The distinction is what makes it usable for sizing. End-to-end latency
	// climbs when a replica is behind and falls when it catches up, so it varies
	// with capacity; service time is what one request costs to serve and does
	// not. Zero when the engine does not publish it or nothing has completed;
	// AvgServiceTimeKnown, below with the TTFT inputs, says whether it was
	// read.
	AvgServiceTime float64

	// AvgITL is the average inter-token latency on this replica in seconds.
	// Derived from rate(vllm:inter_token_latency_seconds_sum[1m]) / rate(..._count[1m])
	// (collector/registration/queueing_model.go), and the SGLang histogram of the
	// same name.
	// Used by queueing model tuner as observed ITL for Kalman filter parameter learning.
	// TA notation: ITL_obs — the (k*, ITL_obs) pair drives OLS calibration of ITL(k) = A·k + B.
	// Zero when metrics are unavailable.
	AvgITL float64
	// AvgITLKnown and AvgITLEmpty say which of three readings AvgITL is; read
	// them through ITLState. Known: a finite, positive mean was read. Empty:
	// the series was present but NaN, the 0/0 of a replica that generated no
	// token in the window. Neither: the series was absent, the query failed,
	// the value was not usable, or the Pod was not Ready and its timing was
	// discarded. The two are never both set.
	//
	// Trust the bits, not AvgITL > 0. A pod whose ranks were merged with one
	// rank absent carries its Finite ranks' mean in AvgITL while AvgITLKnown
	// is false: the absent rank's own mean is missing from it. An empty rank,
	// even one that generated tokens, has no interval and stays out of the
	// mean without blocking Known (collector mergePodInstances).
	//
	// A prefill-role pod emits one token per request and hands decode on, so
	// its ITL window stays empty: it reads Empty permanently, and that is not
	// idleness. Gate by role before reading Empty as an idle replica.
	AvgITLKnown bool
	AvgITLEmpty bool

	// SpecAcceptedPerStep is the draft tokens vLLM's speculative decoding
	// (or MTP) accepted per decode step on this replica, over AvgITL's window:
	// rate(vllm:spec_decode_num_accepted_tokens_total) over
	// rate(vllm:inter_token_latency_seconds_count). vLLM observes ITL once per
	// request per step, however many tokens the step emitted, so with
	// speculation AvgITL is the step time and a step emits
	// 1 + SpecAcceptedPerStep tokens (issue ev-shindin/llm-scaling-manager#274). SGLang's ITL is already per
	// token, and it is never collected for SGLang.
	SpecAcceptedPerStep float64
	// SpecAcceptedPerStepKnown is set when SpecAcceptedPerStep was read from a
	// present, finite, non-negative series. vLLM publishes the counter only
	// with a speculative configuration, so an absent series is either no
	// speculation or a missing reading; the engine's flags tell the two apart
	// (ITLPerToken). A merged pod's count covers the ranks its AvgITL covers,
	// the Finite ones, and is known only when each of them reported one.
	SpecAcceptedPerStepKnown bool

	// AvgTTFT is the average time to first token on this replica in seconds.
	// Derived from rate(vllm:time_to_first_token_seconds_sum[1m]) / rate(..._count[1m]),
	// and the SGLang histogram of the same name.
	//
	// This is the PREFILL side's latency, as AvgITL is decode's: a prefill
	// replica computes the prompt, emits one token and hands the KV on, so on a
	// disaggregated fleet TTFT is most of what it does.
	//
	// IT IS A DIAGNOSTIC. Nothing in the decision path reads it, and that is a
	// measured conclusion rather than an omission: it INCLUDES queue wait, so a
	// TTFT that rises at constant work means the fleet is behind, not that a
	// replica got slower. Fitting TTFT(T) = A·T + B to recover a capacity from
	// it was built and then removed -- the intercept reached 82 seconds because
	// queue wait landed in a term meant to be a fixed per-request cost, and the
	// velocities swung 56,764 to 392,195 tok/s against a measured ~138,000.
	// docs/proposals/prefill-ttft-model.md records that result. Prefill's
	// capacity comes from PrefillComputedTokenRate below, which excludes queue
	// wait by construction and needs no regression at all.
	// Zero when metrics are unavailable; AvgTTFTKnown, below with the other
	// TTFT inputs, says whether it was read.
	AvgTTFT float64

	// PrefillComputedTokenRate is the rate at which this replica computes
	// prefill KV tokens, in tokens/second, excluding tokens the prefix cache
	// already held. Derived from
	// rate(vllm:request_prefill_kv_computed_tokens_sum[1m]).
	//
	// This is PREFILL's capacity in the unit prefill is bounded by, and the
	// figure shape_change.go's saturatedCompletionRate was waiting for. A
	// request rate cannot serve: under overload it is the rate the fleet is
	// being SERVED at, which reads the same at one replica and at ten
	// (measured: 4.75, 4.62 and 4.50 req/s at 1, 2 and 10 replicas while the
	// token rate went 138,875 to 933,750), and a rate learned at one prompt
	// length says nothing about another.
	//
	// Zero on SGLang, which publishes no per-stage prefill counter
	// (sgl-project/sglang#14303); those variants keep the request-rate
	// reading.
	PrefillComputedTokenRate float64
	// PrefillComputedTokenRateKnown is set when the rate was read from a
	// present, finite, non-negative series, so a zero means no prefill work
	// and not a missing reading. A pod whose ranks did not all report one is
	// not known.
	PrefillComputedTokenRateKnown bool

	// --- Fields for Throughput Analyzer ---

	// GenerationTokenRate is the observed decode token generation rate on this replica (tokens/sec).
	// Derived from rate(vllm:request_generation_tokens_sum[1m]) per pod.
	// TA notation: μ_dec^obs — directly observable supply proxy; also used as a sanity check
	// against the demand estimate (μ_dec^obs ≈ λ_dec at steady state with no queueing).
	// Zero when metrics are unavailable.
	GenerationTokenRate float64
	// GenerationTokenRateKnown is set when GenerationTokenRate was read from
	// a present, finite, non-negative series. A pod whose ranks did not all
	// report one is not known.
	GenerationTokenRateKnown bool

	// KvUsageInstant is the instantaneous KV cache utilization fraction on this replica (0.0–1.0).
	// Derived from vllm:kv_cache_usage_perc (no max_over_time window).
	// TA notation: k* — the current operating point in the ITL model ITL(k) = A·k + B.
	// Differs from KvCacheUsage which uses max_over_time[1m] for the saturation analyzer.
	// Zero when metrics are unavailable.
	KvUsageInstant float64
	// KvUsageInstantKnown is set when KvUsageInstant was read from a series,
	// so a zero means an empty cache and not a missing reading. A pod whose
	// ranks did not all report one is not known.
	KvUsageInstantKnown bool

	// KvUsageMean is the KV cache utilization fraction on this replica
	// (0.0–1.0) averaged over the minute AvgITL's rate is taken over
	// (avg_over_time over the ITL window), so an ITL(k) reading pairs a
	// minute's k with the same minute's ITL (issue ev-shindin/llm-scaling-manager#393). Read it through
	// KvUsageForITL. Zero when the metric is unavailable.
	KvUsageMean float64
	// KvUsageMeanKnown is set when KvUsageMean was read from a series. A pod
	// whose ranks did not all report one is not known.
	KvUsageMeanKnown bool

	// RunningRequests is how many sequences the replica's engines hold in
	// their running batch now, summed over its ranks. Derived from
	// vllm:num_requests_running (sglang:num_running_reqs), instantaneous like
	// KvUsageInstant. Zero when the metric is unavailable.
	//
	// Compared with max_num_seqs times Ranks, it says whether the batch is at
	// its sequence cap: a replica there cannot take another sequence however
	// much KV is free, so its KV fill above the ITL band is the cap's and not
	// memory pressure (itl.Tracker).
	RunningRequests int
	// RunningRequestsKnown is set when RunningRequests was read from a
	// present, finite, non-negative series, so a zero means an empty batch
	// and not a missing reading. A pod whose ranks did not all report one is
	// not known.
	RunningRequestsKnown bool

	// PreemptionRate is how many sequences the replica's engines preempted
	// per second over the last minute, summed over its ranks. Derived from
	// rate(vllm:num_preemptions_total[1m]); SGLang publishes no such counter.
	// PreemptionRateKnown is set when it was read from a series, so a zero
	// means no preemption and not a missing reading. A pod whose ranks did
	// not all report one is not known.
	PreemptionRate      float64
	PreemptionRateKnown bool

	// RequestRate is the engine-side request completion rate on this replica (req/s).
	// Engine-agnostic: derived per pod from rate(vllm:request_generation_tokens_count[1m])
	// for vLLM and rate(sglang:generation_tokens_histogram_count[1m]) for SGLang.
	// TA notation: fallback λ_req — used when ArrivalRate == 0 (EPP not deployed).
	// λ_dec_fallback = sum(RequestRate) × avg(AvgOutputTokens).
	// Measures completed requests only; undercounts when requests queue in the scheduler.
	// Zero when metrics are unavailable.
	RequestRate float64
	// RequestRateKnown is set when RequestRate was read from a present,
	// finite, non-negative series. A pod whose ranks did not all report one
	// is not known.
	RequestRateKnown bool

	// --- Recorded for the TTFT model ---
	//
	// The fields below are what the what-if planner and the slo analyzer's
	// TTFT pricing learn from (docs/proposals/analyzer-learner.md, 2.9).
	// QueueMin is read by the saturation analyzer's prefill observation (busy
	// minutes), and AvgTTFT, AvgKVTransferTime and ComputedTokensBuckets by
	// the slo analyzer's TTFT pricing; no analyzer reads the rest. Each is behind a
	// presence bit, or is a histogram that is nil when absent, and each is
	// left out of a learner recording
	// while it is zero (learnrec.OmitZeroTag), so a recording made before
	// they existed reads back and is written again byte for byte.
	//
	// They are recorded as read, for a not-Ready Pod too, and only while a
	// learner recording is on (collector BeginRecordingCycle), but for
	// QueueMin, AvgKVTransferTime and ComputedTokensBuckets, which are also
	// read every cycle in a namespace with a TTFT target (collector
	// ReadTTFTInputsIn). Otherwise each is zero and unknown, and a pod that
	// exports the series not at all reads the same: unknown, never zero.
	// Neither guard that cleans a row's timings
	// reaches them: the not-Ready guard (attributeInstance) drops AvgITL,
	// AvgTTFT and AvgServiceTime and their bits, and the uptime guard drops
	// an AvgTTFT or AvgServiceTime longer than the Pod has run; the
	// histograms, AvgQueueTime,
	// AvgPrefillTime, AvgKVTransferTime and QueueMin are kept as read in
	// both cases. A consumer must not rely on Ready alone: a Pod Ready this cycle
	// may have spent the window not serving, so a mean or histogram over
	// [1m] or [5m] can cover its warm-up; check ReadySince against the
	// window too. A histogram is the per-second rate of
	// requests at or under each bound, cumulative, over the window its query
	// names, sorted by bound; the +Inf bucket carries the total. A bucket
	// whose bound does not parse or whose rate is not a finite non-negative
	// number is dropped. A pod's ranks merge by summing each bound's rate,
	// and the pod has a histogram only when every rank had one.

	// QueueLengthKnown is set when QueueLength was read from a present,
	// finite, non-negative series, so a zero means an empty queue and not a
	// missing reading. A pod whose ranks did not all report one is not known.
	QueueLengthKnown bool `learnrec:"omitzero"`
	// AvgServiceTimeKnown and AvgTTFTKnown are set when AvgServiceTime and
	// AvgTTFT hold a mean read from a present, finite, positive series. They
	// follow the value: a not-Ready Pod's timings are dropped, and so is a
	// mean longer than the Pod has run, and the bit with them. A NaN (no
	// request in the window) is not known. A pod whose ranks did not all
	// report one is not known.
	AvgServiceTimeKnown bool `learnrec:"omitzero"`
	AvgTTFTKnown        bool `learnrec:"omitzero"`

	// PromptTokensBuckets is the prompt-length histogram, tokens per
	// request, over [5m] like AvgInputTokens. vLLM
	// request_prompt_tokens_bucket; SGLang prompt_tokens_histogram_bucket
	// (behind --collect-tokens-histogram). Nil when absent.
	PromptTokensBuckets []LengthBucket `learnrec:"omitzero"`
	// ComputedTokensBuckets is the per-request histogram of computed prefill
	// tokens, the prompt net of prefix hits. vLLM
	// request_prefill_kv_computed_tokens_bucket, over [1m] like
	// PrefillComputedTokenRate. The slo analyzer and the what-if CLI read the
	// prompt's spread from it where a row carries it (whatif.ReadPromptSpread).
	// Nil when absent.
	ComputedTokensBuckets []LengthBucket `learnrec:"omitzero"`
	// OutputTokensBuckets is the output-length histogram, tokens per
	// request, over [5m] like AvgOutputTokens: the buckets
	// AvgOutputTokensSquared is reduced from. vLLM
	// request_generation_tokens_bucket; SGLang
	// generation_tokens_histogram_bucket. Nil when absent.
	OutputTokensBuckets []LengthBucket `learnrec:"omitzero"`
	// TTFTBuckets is the engine's time-to-first-token histogram, seconds,
	// over [1m] like AvgTTFT. vLLM and SGLang time_to_first_token_seconds.
	// Nil when absent.
	TTFTBuckets []LatencyBucket `learnrec:"omitzero"`
	// AvgQueueTime is the mean time a request waited in the engine's queue
	// before it was scheduled, seconds, over [1m]; QueueTimeBuckets is its
	// histogram over the same window. vLLM request_queue_time_seconds;
	// SGLang queue_time_seconds. AvgQueueTimeKnown is set when the mean was
	// read from a present, finite, non-negative series (a zero is no wait);
	// a NaN, no request in the window, is not known.
	AvgQueueTime      float64         `learnrec:"omitzero"`
	AvgQueueTimeKnown bool            `learnrec:"omitzero"`
	QueueTimeBuckets  []LatencyBucket `learnrec:"omitzero"`
	// AvgPrefillTime is the mean time a request spent in prefill, queue wait
	// excluded, seconds, over [1m]. vLLM request_prefill_time_seconds only;
	// SGLang publishes no per-stage figure. Known as AvgQueueTime is.
	AvgPrefillTime      float64 `learnrec:"omitzero"`
	AvgPrefillTimeKnown bool    `learnrec:"omitzero"`
	// AvgKVTransferTime is the mean time of one KV transfer through NIXL
	// (P/D), seconds, over [1m]. vLLM nixl_xfer_time_seconds only, exported
	// by a pod that runs the NIXL connector. Known as AvgQueueTime is.
	AvgKVTransferTime      float64 `learnrec:"omitzero"`
	AvgKVTransferTimeKnown bool    `learnrec:"omitzero"`
	// TPOTBuckets is the engine's per-request time-per-output-token
	// histogram, seconds, over [1m]: each request's own mean TPOT, the
	// fallback source for the TPOT ratio where the router's is missing.
	// vLLM request_time_per_output_token_seconds only. Nil when absent.
	TPOTBuckets []LatencyBucket `learnrec:"omitzero"`
	// QueueMin is the least the engine's waiting-request gauge read over
	// the last minute, requests: above zero, the engine had work waiting the
	// whole minute (a busy minute). vLLM num_requests_waiting, each rank's
	// least summed over the pod's ranks, which is a floor under the least of
	// the pod's own queue; SGLang num_queue_reqs. QueueMinKnown is set when
	// it was read from a present, finite, non-negative series (a zero is a
	// queue that emptied); a pod whose ranks did not all report one is not
	// known.
	QueueMin      float64 `learnrec:"omitzero"`
	QueueMinKnown bool    `learnrec:"omitzero"`
}

// ITLState is which of three readings a replica's AvgITL is.
type ITLState int

const (
	// ITLAbsent is no usable reading: the series was absent, the query failed,
	// the value was not a positive finite number, or the Pod was not Ready.
	ITLAbsent ITLState = iota
	// ITLEmpty is a series present and NaN: the 0/0 of a replica that
	// generated no token in the window: an idle decode replica, not a failed
	// query. A prefill-role pod reads it permanently, so gate by role first.
	ITLEmpty
	// ITLFinite is a reading where AvgITL holds a measured, finite, positive mean.
	ITLFinite
)

// String names the state.
func (s ITLState) String() string {
	switch s {
	case ITLEmpty:
		return "empty"
	case ITLFinite:
		return "finite"
	default:
		return "absent"
	}
}

// ITLState reads AvgITLKnown and AvgITLEmpty as one of the three states.
// Known wins should both be set. Read it, not AvgITL > 0: after a rank merge
// with an absent rank AvgITL holds the known ranks' mean while the state is
// absent (see AvgITLKnown).
func (m ReplicaMetrics) ITLState() ITLState {
	switch {
	case m.AvgITLKnown:
		return ITLFinite
	case m.AvgITLEmpty:
		return ITLEmpty
	default:
		return ITLAbsent
	}
}

// ITLStep says how a replica's engine reports its inter-token latency, for
// ITLPerToken.
type ITLStep struct {
	// PerToken is an engine whose ITL is per token already (SGLang divides
	// each step's interval by the tokens it emitted): its rows are read as
	// they are, whatever counter they carry.
	PerToken bool
	// Speculates is an engine whose ITL is per step and whose flags configure
	// speculative decoding or MTP (vLLM's --speculative-config).
	Speculates bool
	// MaxAccepted is the draft tokens a step proposes at most
	// (num_speculative_tokens), and so the most it can accept; 0 when the
	// flags do not state it.
	MaxAccepted int
}

// ITLWithheld is why ITLPerToken withheld a reading: no counter beside
// speculative flags, or a counter above what a step can accept.
type ITLWithheld int

// The reasons ITLPerToken withholds a reading.
const (
	// ITLKept is a reading kept, corrected or not.
	ITLKept ITLWithheld = iota
	// ITLNoCounter is a reading of an engine whose flags speculate, with no
	// SpecAcceptedPerStep read.
	ITLNoCounter
	// ITLOverMax is a reading whose SpecAcceptedPerStep is above
	// ITLStep.MaxAccepted, which no step can accept.
	ITLOverMax
)

// ITLPerToken is the record with AvgITL read per generated token rather
// than per engine step (issue ev-shindin/llm-scaling-manager#274), and why a reading was withheld.
//
//   - On an engine whose ITL is per token already (ITLStep.PerToken), the
//     record is returned as it is.
//   - A known SpecAcceptedPerStep divides a finite AvgITL by the tokens a
//     step emits, 1 + SpecAcceptedPerStep, flags or not: the counter exists
//     only when the engine speculates. Above ITLStep.MaxAccepted, when that
//     is stated, it is not a count a step can produce, and the reading is
//     withheld (ITLOverMax): an over-read count over-states the token rate,
//     the direction that under-orders.
//   - Without it, a replica whose flags speculate has its finite AvgITL
//     withheld (ITLNoCounter): a step time of unknown length is not a
//     per-token reading, and a missing counter is not "no speculation".
//   - Otherwise the record is returned as it is: one token per step.
//
// A withheld reading is zeroed as well as marked absent: the ITL window reads
// AvgITL > 0, and the value too must say there is no reading. A record with
// no positive finite AvgITL is returned as it is: an idle replica is idle in
// either unit.
func (m ReplicaMetrics) ITLPerToken(step ITLStep) (ReplicaMetrics, ITLWithheld) {
	if step.PerToken || !(m.AvgITL > 0) || math.IsInf(m.AvgITL, 1) {
		return m, ITLKept
	}
	switch {
	case m.SpecAcceptedPerStepKnown && m.SpecAcceptedPerStep >= 0 && !math.IsInf(m.SpecAcceptedPerStep, 0):
		// A ratio of two rates at full acceptance can land a rounding error
		// over the bound; that is not a count over it.
		if step.MaxAccepted > 0 && m.SpecAcceptedPerStep > float64(step.MaxAccepted)*(1+1e-6) {
			m.AvgITL, m.AvgITLKnown = 0, false
			return m, ITLOverMax
		}
		m.AvgITL /= 1 + m.SpecAcceptedPerStep
	case step.Speculates:
		m.AvgITL, m.AvgITLKnown = 0, false
		return m, ITLNoCounter
	}
	return m, ITLKept
}

// Ranks is the number of data-parallel engine ranks the record covers, never
// less than one: an unknown count (EngineRanks zero) is one engine, which is
// what every record meant before the count existed.
//
// Multiply a per-rank engine setting by it before comparing it with a
// per-record figure; see EngineRanks.
func (m ReplicaMetrics) Ranks() int {
	return max(m.EngineRanks, 1)
}

// AtBatchCap reports whether the record's running batch is exactly at the
// sequence cap: maxNumSeqs, one engine's batch cap (max_num_seqs, lowered by
// --max-num-active-seqs when that is set, or where the flags state none the
// llm-d.ai/max-num-seqs annotation), times Ranks. Each
// engine holds at most its cap, so the sum equals it only when every rank is
// full; more than it means the cap is not what was read. A maxNumSeqs of zero
// is an unknown cap, and nothing is at it.
func (m ReplicaMetrics) AtBatchCap(maxNumSeqs int64) bool {
	return maxNumSeqs > 0 && int64(m.RunningRequests) == maxNumSeqs*int64(m.Ranks())
}

// BusiestRankQueue is the waiting queue of the record's busiest rank (see
// MaxRankQueueLength). With one rank it is QueueLength. With several and no
// busiest-rank reading while requests wait, it is the queue's mean over the
// ranks rounded up: the least the busiest rank can hold.
func (m ReplicaMetrics) BusiestRankQueue() int {
	return busiestRank(m.QueueLength, m.MaxRankQueueLength, m.Ranks())
}

// BusiestRankCapacityQueue is BusiestRankQueue for the part of the queue that
// waits for scheduling capacity (see CapacityQueueLength). Zero when
// CapacityQueueKnown is false.
func (m ReplicaMetrics) BusiestRankCapacityQueue() int {
	if !m.CapacityQueueKnown {
		return 0
	}
	return busiestRank(m.CapacityQueueLength, m.MaxRankCapacityQueueLength, m.Ranks())
}

// busiestRank is the busiest rank's share of a queue summed over ranks
// engines, given the busiest rank's own reading maxRank (zero when not
// collected); see BusiestRankQueue.
func busiestRank(sum, maxRank, ranks int) int {
	if ranks <= 1 {
		return sum
	}
	if maxRank > 0 || sum <= 0 {
		return maxRank
	}
	return (sum + ranks - 1) / ranks
}

// BusiestRankKvUsage is the KV cache utilization of the record's busiest
// rank (see MaxRankKvCacheUsage). With one rank it is KvCacheUsage. With
// several and no busiest-rank reading, it is the mean, the least the busiest
// rank can hold.
func (m ReplicaMetrics) BusiestRankKvUsage() float64 {
	if m.Ranks() <= 1 || !(m.MaxRankKvCacheUsage > 0) {
		return m.KvCacheUsage
	}
	return m.MaxRankKvCacheUsage
}

// KvUsageForITL is the k an ITL(k) reading pairs with AvgITL: KvUsageMean,
// taken over the same minute as the ITL's rate, when it was read, and
// otherwise KvUsageInstant (issue ev-shindin/llm-scaling-manager#393). The instant k against a minute's
// ITL is the mismatch ev-shindin/llm-scaling-manager#273 A5 names: with the batch moving inside the minute
// it flattens the fitted slope and moves the intercept.
func (m ReplicaMetrics) KvUsageForITL() float64 {
	if m.KvUsageMeanKnown {
		return m.KvUsageMean
	}
	return m.KvUsageInstant
}

// BusiestRankTokens is the tokens the record would hold were every one of
// its ranks as full as its busiest one: BusiestRankKvUsage times the
// record's KV capacity. With one rank, or without a capacity, it is
// TokensInUse.
//
// It is the record's occupancy at the moment ONE engine saturates. A k2
// learned from it is one engine's saturation point scaled by the ranks, as
// the derived k2 scales one engine's settings.
func (m ReplicaMetrics) BusiestRankTokens() int64 {
	if m.Ranks() <= 1 || m.TotalKvCapacityTokens <= 0 {
		return m.TokensInUse
	}
	// Rounded and clamped to the capacity as TokensInUse is
	// (collector.attributeInstance). Never below TokensInUse: the busiest
	// rank holds at least the mean.
	busiest := math.Round(m.BusiestRankKvUsage() * float64(m.TotalKvCapacityTokens))
	if !(busiest <= float64(m.TotalKvCapacityTokens)) {
		busiest = float64(m.TotalKvCapacityTokens)
	}
	return max(int64(busiest), m.TokensInUse)
}

// RevisionsMixed names the variants whose own replicas -- warm-pool bridges
// aside, which run the pool's template -- report more than one pod template
// revision (ReplicaMetrics.Revision). A replica that reports none is not
// counted: an unknown is not a second revision.
func RevisionsMixed(rows []ReplicaMetrics) map[string]bool {
	first := map[string]string{}
	mixed := map[string]bool{}
	for _, rm := range rows {
		if rm.FromWarmPool || rm.Revision == "" {
			continue
		}
		if seen, ok := first[rm.VariantName]; !ok {
			first[rm.VariantName] = rm.Revision
		} else if seen != rm.Revision {
			mixed[rm.VariantName] = true
		}
	}
	return mixed
}

// NewestRevision is variant's own Ready replicas' newest revision, and how
// many revisions they report: warm-pool bridges and a replica with no revision
// aside. A rollout makes new pods, so the newest revision is the one whose
// oldest Ready pod is youngest: a youngest pod would name an old pod restarted
// in place. "" when none reports one, or when more than one do and they cannot
// be ordered: a Ready time unread, or two alike.
func NewestRevision(rows []ReplicaMetrics, variant string) (string, int) {
	oldest := map[string]time.Time{}
	for _, rm := range rows {
		if rm.VariantName != variant || rm.FromWarmPool || !rm.Ready || rm.Revision == "" {
			continue
		}
		at, seen := oldest[rm.Revision]
		if !seen || (!rm.ReadySince.IsZero() && (at.IsZero() || rm.ReadySince.Before(at))) {
			oldest[rm.Revision] = rm.ReadySince
		}
	}
	newest := ""
	var newestAt time.Time
	for revision, at := range oldest {
		if len(oldest) == 1 {
			return revision, 1
		}
		if at.IsZero() {
			return "", len(oldest)
		}
		if newest == "" || at.After(newestAt) {
			newest, newestAt = revision, at
		}
	}
	for revision, at := range oldest {
		if revision != newest && at.Equal(newestAt) {
			return "", len(oldest)
		}
	}
	return newest, len(oldest)
}

// ReplicaMetricsMetadata contains freshness information for replica metrics
type ReplicaMetricsMetadata struct {
	// CollectedAt is when the metrics were collected
	CollectedAt time.Time
	// Age is the age of the metrics
	Age time.Duration
	// FreshnessStatus indicates freshness: "fresh", "stale", "unavailable"
	FreshnessStatus string
}
