package domain

// LatencyBucket is one cumulative histogram bucket: the per-second rate of
// requests whose latency was at most UpperBound seconds. The +Inf bucket has
// UpperBound = math.Inf(1) and carries the total.
type LatencyBucket struct {
	UpperBound float64
	Rate       float64
}

// RouterLatency is one model's request latency as the router (the llm-d EPP)
// measured it, over WindowSeconds.
//
// The router's time to first token starts when the request reaches the router,
// BEFORE flow-control admission, so it is the one server-side TTFT that
// includes the router's queue -- the number a client's TTFT SLO is judged on.
// Only streaming requests are counted: for a non-streaming response the router
// records the whole request duration as its TTFT.
type RouterLatency struct {
	// WindowSeconds is the rate window the figures below were taken over.
	WindowSeconds float64

	// TTFTBuckets is the cumulative TTFT histogram, as per-second rates,
	// sorted by bound. Nil when the router exports no TTFT series for the
	// model.
	TTFTBuckets []LatencyBucket

	// TTFTSumRate and TTFTCountRate are the rates of the same histogram's
	// _sum and _count, read for the TTFT model's hop term: their ratio is the
	// mean TTFT over the window, which the buckets, 25 to 100 ms wide where
	// it stands, cannot give to the 5 ms the term is. Both zero when absent,
	// and left out of a learner recording then, so a recording made before
	// they existed is written again byte for byte.
	TTFTSumRate   float64 `learnrec:"omitzero"`
	TTFTCountRate float64 `learnrec:"omitzero"`

	// TPOTSumRate and TPOTCountRate are the rates of the streaming
	// time-per-output-token histogram's _sum and _count. Their ratio is the
	// mean TPOT over the window, request-weighted. Both zero when absent.
	TPOTSumRate   float64
	TPOTCountRate float64

	// TPOTBuckets is the router's per-request streaming TPOT histogram, as
	// per-second bucket rates over the same window, sorted by bound: the
	// distribution behind the mean above. Nil when the router exports no
	// buckets for the model or the query failed. Read by nothing yet
	// (docs/proposals/analyzer-learner.md, 2.9); left out of a learner
	// recording while nil, so a recording made before it existed is written
	// again byte for byte.
	TPOTBuckets []LatencyBucket `learnrec:"omitzero"`

	// FlowControlWaitBuckets is the router's flow-control queue wait
	// histogram, seconds, of this model's dispatched requests, as per-second
	// bucket rates over the same window, sorted by bound: the router-queue
	// part of the TTFT above. Nil with no such series or a failed query, and
	// non-nil does not prove the gate is on (AnalyzerInput.FlowControlSeries).
	// Collected only while a learner recording is on, read by nothing yet,
	// and left out of a recording while nil.
	FlowControlWaitBuckets []LatencyBucket `learnrec:"omitzero"`

	// ProcessingBuckets is the router's request-processing histogram,
	// seconds, as per-second bucket rates over the same window, sorted by
	// bound, and ProcessingSumRate and ProcessingCountRate the rates of its
	// _sum and _count: the router's own time inside the TTFT above, which
	// the capacity planner's experimental TTFT model draws its remainder
	// from (whatif.RankMixtureOptions.RemainderSpread). The series carries
	// no model, so it is joined to a model only where the router's TTFT
	// names that model alone in the namespace. Nil and zero with no such
	// series, a failed query or no join. Collected only while a learner
	// recording is on, and left out of a recording while absent, so a
	// recording made before it existed is written again byte for byte.
	ProcessingBuckets   []LatencyBucket `learnrec:"omitzero"`
	ProcessingSumRate   float64         `learnrec:"omitzero"`
	ProcessingCountRate float64         `learnrec:"omitzero"`

	// RenderSumRate and RenderCountRate are the rates of the router's
	// token-producer render histogram's _sum and _count over the same
	// window: the router's call to a prefill pod's /render handler, inside
	// its request processing above, which the capacity planner's remainder
	// server mode (whatif.RankMixtureOptions.RemainderServer) takes out of
	// the processing as the prefill pods' API-server time. Joined to a
	// model as the processing series are; zero with no such series, a
	// failed query or no join. Collected only while a learner recording is
	// on, and left out of a recording while zero.
	RenderSumRate   float64 `learnrec:"omitzero"`
	RenderCountRate float64 `learnrec:"omitzero"`
	// PrefixPredictedTokenRate and PrefixBestAvailableTokenRate are the
	// router prefix producer's cached tokens a second over the same window:
	// predicted on the endpoint each request was sent to, and available on
	// the endpoint that held the most of its prefix. Their gap is the
	// affinity the routing gave up, from which the capacity planner reads
	// the prefix duplication across prefill pods
	// (prefill.DuplicationOfAffinityLoss). A P/D router runs one producer per
	// profile; the pair is the producer that predicted the most cached
	// tokens, the one that routes the prefill pods (a producer for pods with
	// no prefix cache predicts none). The series carry no model, so the pair
	// is joined to a model as the processing histogram is. Both zero with no
	// such series, a failed query or no join; collected only while a learner
	// recording is on and left out of a recording while zero.
	PrefixPredictedTokenRate     float64 `learnrec:"omitzero"`
	PrefixBestAvailableTokenRate float64 `learnrec:"omitzero"`

	// Endpoints is each model-server Pod as the routers that serve this
	// model saw it over the same window, sorted by Pod: the requests placed
	// on it, its queue as the router read it, and the requests and tokens
	// the router holds in flight on it. PrefixProducers is the router's
	// prefix producers' counts, per producer and endpoint role, over the
	// same window and routers.
	//
	// A router Pod serves the model when its placements over the window
	// name this model and no other (RouterEndpoint.PlacementRate), so a
	// second router replica in the namespace, one that serves another
	// model or routes nothing, is not read. Nil with no such router Pod,
	// a failed placement query, or a router that publishes no endpoint
	// series. Collected only while a learner recording is on, read by
	// nothing yet (#482, #483, #384), and left out of a recording while
	// nil, so a recording made before they existed is written again byte
	// for byte.
	Endpoints       []RouterEndpoint       `learnrec:"omitzero"`
	PrefixProducers []RouterPrefixProducer `learnrec:"omitzero"`
}

// RouterEndpoint is one model-server Pod as the routers that serve a model
// saw it, over RouterLatency.WindowSeconds. A Pod of several engine ranks
// (one endpoint per target port) is one row, its ranks summed.
//
// Each figure is behind a presence bit: a series the router does not
// publish for this Pod, or a query that failed, is not a zero. Every field
// is new, so each is left out of a learner recording while zero.
type RouterEndpoint struct {
	// Pod is the model-server Pod's name.
	Pod string `learnrec:"omitzero"`

	// PlacementRate is the requests a second the routers placed on the Pod
	// (llm_d_epp_scheduler_attempts_total, status success). The router
	// counts the primary profile's endpoint only: in P/D that is the decode
	// Pod, so a prefill Pod has no placements and PlacementKnown unset.
	// It is unset as well for a decode or aggregated Pod no serving router
	// has placed a request on since it started (no series at all); a Pod
	// placed on before and idle over the whole window has a series that
	// stood still, and reads a known rate of zero.
	PlacementRate  float64 `learnrec:"omitzero"`
	PlacementKnown bool    `learnrec:"omitzero"`

	// QueueSize is the Pod's waiting requests as the router read them from
	// the model server, averaged over the window
	// (llm_d_epp_per_endpoint_queue_size). Every router replica reads the
	// same model servers, so it is the highest over the serving routers,
	// never their sum.
	QueueSize      float64 `learnrec:"omitzero"`
	QueueSizeKnown bool    `learnrec:"omitzero"`

	// InFlightRequests and InFlightTokens are the requests, and their
	// uncached prompt tokens, the routers hold in flight on the Pod,
	// averaged over the window (llm_d_epp_inflight_requests and
	// llm_d_epp_inflight_tokens, from the router's in-flight load
	// producer). A prefill Pod's are released at the first token, a decode
	// or aggregated Pod's at the end of the response. Each router replica
	// tracks its own requests, so they are summed over the serving routers;
	// two producers in one router track the same requests, so the larger is
	// read.
	InFlightRequests      float64 `learnrec:"omitzero"`
	InFlightRequestsKnown bool    `learnrec:"omitzero"`
	InFlightTokens        float64 `learnrec:"omitzero"`
	InFlightTokensKnown   bool    `learnrec:"omitzero"`
}

// RouterPrefixProducer is one router prefix producer's counts for one
// endpoint role (prefill, decode), summed over the routers that serve the
// model, over RouterLatency.WindowSeconds: the rates of the
// llm_d_epp_prefix_* histograms' _sum, and of the predicted histogram's
// _count. The router publishes them per producer and role, not per
// endpoint. Present only where each of the five queries was read and held
// a series for this producer and role, so a zero here is a zero the router
// counted. Every field is new, so each is left
// out of a learner recording while zero.
type RouterPrefixProducer struct {
	// Plugin is the producer's plugin_name and Role its endpoint_role.
	Plugin string `learnrec:"omitzero"`
	Role   string `learnrec:"omitzero"`

	// RequestRate is the requests a second the producer predicted for;
	// PromptTokenRate their prompt tokens; PredictedTokenRate the cached
	// tokens it predicted on the endpoint chosen; BestPredictedTokenRate
	// the most on any endpoint the filters kept; BestAvailableTokenRate the
	// most on any candidate endpoint. Predicted over best available is the
	// share of the hit the routing kept.
	RequestRate            float64 `learnrec:"omitzero"`
	PromptTokenRate        float64 `learnrec:"omitzero"`
	PredictedTokenRate     float64 `learnrec:"omitzero"`
	BestPredictedTokenRate float64 `learnrec:"omitzero"`
	BestAvailableTokenRate float64 `learnrec:"omitzero"`
}

// Judgeable reports whether r is a reading of the router's latency: a TTFT
// histogram or a TPOT request rate. The recorded histograms alone
// (TPOTBuckets, FlowControlWaitBuckets, ProcessingBuckets) are not one, so the slo analyzer
// treats a struct holding only them as no reading, as it did before they
// were collected. Nil is not judgeable.
func (r *RouterLatency) Judgeable() bool {
	return r != nil && (len(r.TTFTBuckets) > 0 || r.TPOTCountRate != 0)
}

// FlowControlPool is one InferencePool's flow-control layer in the router
// (the llm-d EPP), over WindowSeconds: how saturated the pool read, and how
// requests left it. How long they waited is per model
// (RouterLatency.FlowControlWaitBuckets).
//
// These series carry the pool (inference_pool) and no model, so a pool is
// joined to a model only when the pool serves that one model alone: the
// flow-control queue gauge names both, and a pool it names with two models
// is joined to neither. Numbers only: the pool's name is not kept.
//
// Each figure is behind a presence bit, or is a histogram that is nil when
// absent: a series that is missing, NaN or unreadable is not a zero. Read by
// nothing yet (docs/proposals/analyzer-learner.md, 2.9); a zero field is
// left out of a learner recording (learnrec.OmitZeroTag).
type FlowControlPool struct {
	// WindowSeconds is the rate window of the figures below.
	WindowSeconds float64 `learnrec:"omitzero"`

	// EffectiveSaturation is the pool's saturation score as the gate reads
	// it (stage "effective", the larger of the two below), averaged over the
	// window; PrefillSaturation and DecodeSaturation are the P/D stages' own
	// (stages "prefill" and "decode"). At 1 the gate stops dispatching; a
	// pool with no endpoint reads 1. The highest over the router's
	// replicas. Each Known bit says the series was read finite.
	EffectiveSaturation      float64 `learnrec:"omitzero"`
	EffectiveSaturationKnown bool    `learnrec:"omitzero"`
	PrefillSaturation        float64 `learnrec:"omitzero"`
	PrefillSaturationKnown   bool    `learnrec:"omitzero"`
	DecodeSaturation         float64 `learnrec:"omitzero"`
	DecodeSaturationKnown    bool    `learnrec:"omitzero"`

	// DispatchedRate, RejectedRate and EvictedRate are the requests per
	// second that left the queue by each outcome (the outcome label's
	// Dispatched, Rejected... and Evicted... values); OtherOutcomeRate is any
	// other outcome. OutcomesKnown is set when the outcome series was read.
	DispatchedRate   float64 `learnrec:"omitzero"`
	RejectedRate     float64 `learnrec:"omitzero"`
	EvictedRate      float64 `learnrec:"omitzero"`
	OtherOutcomeRate float64 `learnrec:"omitzero"`
	OutcomesKnown    bool    `learnrec:"omitzero"`
}
