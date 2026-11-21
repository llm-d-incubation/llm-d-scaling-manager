package learnreplay

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// CycleSample is what one cycle of a recording saw of a model: its arrival
// rate, the pods of each role that served, and the traffic's shape. It is
// read from the cycle's input alone, with no replay, so a day of cycles is
// read in one pass.
type CycleSample struct {
	At time.Time
	// ArrivalRate is the model's request rate, requests per second, as the
	// cycle's input carries it. RateKnown is false when the input carries
	// none (no router series) while decode rows served requests: the rate is
	// then the request rates of every decode row of the model, summed. A
	// cycle with neither is idle, its rate a known 0.
	ArrivalRate float64
	RateKnown   bool
	// PrefillPods and DecodePods are the pods of each role that served at
	// the cycle (prefill.Serving): ready, not a bridge.
	PrefillPods, DecodePods int
	// PrefillGPUs and DecodeGPUs are the GPUs per replica of each role, as
	// the cycle's variants state them; 0 where none is stated.
	PrefillGPUs, DecodeGPUs int
	// PromptTokens, OutputTokens and PrefixHitRate are the decode rows'
	// means, weighted by each row's request rate; HasShape is false when no
	// decode row read an output length.
	PromptTokens, OutputTokens, PrefixHitRate float64
	HasShape                                  bool
	// OtherPods is the serving pods of prefill and decode variants other
	// than the counted ones: the model's rate served by pods the counts
	// leave out.
	OtherPods int
	// PromptComputedRate and PromptRequestRate are the counted prefill
	// pods' computed prompt tokens and requests a second, summed over the
	// serving pods that read both, as whatif.PromptWindow counts them, and
	// PromptBuckets their computed-token histogram pooled
	// (whatif.PoolComputedBuckets); zero and nil with none. They are the
	// prompt's spread as the traffic carried it: lengths, not latency.
	PromptComputedRate, PromptRequestRate float64
	PromptBuckets                         map[float64]float64
	// FlowControlSeries says the cycle read the router's flow-control queue
	// series (domain.AnalyzerInput.FlowControlSeries).
	FlowControlSeries bool
	// EngineRate is the request rate of every decode and every unsplit
	// ("both") row of the model that reports a rate, summed, whatever ArrivalRate is:
	// where every replica's engine is read it is the router's rate, and
	// where a multi-Pod group's rows report one rank of several it is a
	// fraction of it (issue llm-d-incubation/llm-d-scaling-manager#331).
	EngineRate float64
	// PrefillReplicas and DecodeReplicas are the counted variants' serving
	// replicas as the cycle's variant states give them (CurrentReplicas
	// less PendingReplicas), summed; 0 where none is stated. Beside
	// PrefillPods and DecodePods they say whether a replica is one engine
	// row.
	PrefillReplicas, DecodeReplicas int
	// RouterQueued says the router's flow-control queue held requests at
	// the cycle: arrivals then run ahead of what the engines complete.
	RouterQueued bool
}

// SeriesVariants names the variants a series counts: the prefill and the
// decode variant a plan prices. An empty name counts every variant of the
// role.
type SeriesVariants struct {
	Prefill, Decode string
}

// Series is every cycle of the model q names, oldest first, read from the
// recording's saturation Analyze events without replaying them. Pods, GPUs
// and lengths are the variants v names, so that what the fleet had is
// counted for the variants a plan sizes. q.Cycle is ignored: the series is
// the whole recording. A *QueryError is a model the recording does not hold
// or names ambiguously.
func Series(recording []byte, q StateQuery, v SeriesVariants) ([]CycleSample, error) {
	return SeriesFrom(BytesSource(recording), q, v)
}

// SeriesFrom is Series of the recording src opens, never held. A query that
// names the namespace and the model is read in one pass, and resolved
// (resolveModel) only when the pass found none of its cycles, for the
// error; any other in two, resolved first.
func SeriesFrom(src Source, q StateQuery, v SeriesVariants) ([]CycleSample, error) {
	out, _, err := seriesScan(src, q, v, false)
	return out, err
}

// SeriesWithLatencyFrom is SeriesFrom with each cycle's measured latency
// beside it, in the same pass: latency[i] is the cycle of samples[i]. The
// samples carry no latency, so a reader of the samples alone cannot see it.
func SeriesWithLatencyFrom(src Source, q StateQuery, v SeriesVariants) ([]CycleSample, []CycleLatency, error) {
	return seriesScan(src, q, v, true)
}

// seriesScan is SeriesFrom, and with withLatency each cycle's latency.
func seriesScan(src Source, q StateQuery, v SeriesVariants, withLatency bool) ([]CycleSample, []CycleLatency, error) {
	ns, model := q.Namespace, q.ModelID
	if ns == "" || model == "" {
		var err error
		if ns, model, _, err = resolveModel(src, q); err != nil {
			return nil, nil, err
		}
	}
	var out []CycleSample
	var lat []CycleLatency
	err := scan(src, func(ev learnrec.RawEvent) (bool, error) {
		if ev.Kind != saturation.KindAnalyze {
			return true, nil
		}
		var d saturation.AnalyzeEvent
		if err := ev.DecodeData(&d); err != nil {
			return false, fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
		in, err := decodeInput(d.Input)
		if err != nil {
			return false, fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
		if in.Namespace == ns && in.ModelID == model {
			out = append(out, sampleOf(in, ev.Now, v))
			if withLatency {
				lat = append(lat, latencyOf(in, ev.Now, v))
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(out) == 0 {
		if _, _, _, err := resolveModel(src, q); err != nil {
			return nil, nil, err
		}
	}
	return out, lat, nil
}

// sampleOf is one cycle's sample of in, read at at, of the variants v
// names.
func sampleOf(in domain.AnalyzerInput, at time.Time, v SeriesVariants) CycleSample {
	s := CycleSample{At: at, ArrivalRate: in.ArrivalRate, FlowControlSeries: in.FlowControlSeries(),
		RouterQueued: in.SchedulerQueue != nil && in.SchedulerQueue.QueueSize > 0}
	roles := rolesOf(in)
	counted := func(variant, role string) bool {
		switch role {
		case domain.RolePrefill:
			return v.Prefill == "" || variant == v.Prefill
		case domain.RoleDecode:
			return v.Decode == "" || variant == v.Decode
		}
		return false
	}
	for _, vs := range in.VariantStates {
		if !counted(vs.VariantName, domain.CanonicalRole(vs.Role)) {
			continue
		}
		switch domain.CanonicalRole(vs.Role) {
		case domain.RolePrefill:
			s.PrefillGPUs = max(s.PrefillGPUs, vs.GPUsPerReplica)
			s.PrefillReplicas += max(vs.CurrentReplicas-vs.PendingReplicas, 0)
		case domain.RoleDecode:
			s.DecodeGPUs = max(s.DecodeGPUs, vs.GPUsPerReplica)
			s.DecodeReplicas += max(vs.CurrentReplicas-vs.PendingReplicas, 0)
		}
	}
	var weight, il, ol, hit, modelRate, bothRate float64
	var prompted []domain.ReplicaMetrics
	for _, rm := range in.ReplicaMetrics {
		role := domain.CanonicalRole(roles[rm.VariantName])
		if role == domain.RoleDecode && rm.RequestRateKnown && rm.RequestRate > 0 {
			modelRate += rm.RequestRate
		}
		if role == domain.RoleBoth && rm.RequestRateKnown && rm.RequestRate > 0 {
			bothRate += rm.RequestRate
		}
		if !counted(rm.VariantName, role) {
			if (role == domain.RolePrefill || role == domain.RoleDecode) && prefill.Serving(rm) {
				s.OtherPods++
			}
			continue
		}
		if prefill.Serving(rm) {
			switch role {
			case domain.RolePrefill:
				s.PrefillPods++
				if rm.PrefillComputedTokenRateKnown && rm.RequestRateKnown && whatif.Finite(rm.PrefillComputedTokenRate) &&
					rm.PrefillComputedTokenRate > 0 && whatif.Finite(rm.RequestRate) && rm.RequestRate > 0 {
					s.PromptComputedRate += rm.PrefillComputedTokenRate
					s.PromptRequestRate += rm.RequestRate
					prompted = append(prompted, rm)
				}
			case domain.RoleDecode:
				s.DecodePods++
			}
		}
		if role != domain.RoleDecode {
			continue
		}
		w := 0.0
		if rm.RequestRateKnown && rm.RequestRate > 0 {
			w = rm.RequestRate
		}
		if !(rm.AvgOutputTokens > 0) || !(w > 0) {
			continue
		}
		weight += w
		il += w * rm.AvgInputTokens
		ol += w * rm.AvgOutputTokens
		hit += w * rm.PrefixCacheHitRate
	}
	if pooled := whatif.PoolComputedBuckets(nil, prompted); len(pooled) > 0 {
		s.PromptBuckets = pooled
	}
	if weight > 0 {
		s.PromptTokens, s.OutputTokens, s.PrefixHitRate, s.HasShape = il/weight, ol/weight, hit/weight, true
	}
	s.EngineRate = modelRate + bothRate
	s.RateKnown = in.ArrivalRate > 0 || modelRate == 0
	if !s.RateKnown {
		s.ArrivalRate = modelRate
	}
	return s
}

// CycleLatency is the latency one cycle measured for a model, server side:
// the router's histograms where the recording carries them, and the
// engines' own, summed over the counted variants' pods. It is what a
// backtest scores a prediction against, and is kept apart from CycleSample
// so that nothing that prices a cycle reads it.
type CycleLatency struct {
	At time.Time
	// Router is the router's TTFT and TPOT over its window; nil when the
	// cycle carries none.
	Router *domain.RouterLatency
	// EngineTTFT is the engines' TTFT histogram, the prefill pods' on a P/D
	// fleet (prefill and its queue alone: no router, no KV transfer, no
	// decode step) and every counted pod's otherwise, each bound's rate
	// summed over the pods; nil with none. EngineTTFTMean is the pods' mean
	// TTFT weighted by their request rate, 0 with none.
	EngineTTFT     []domain.LatencyBucket
	EngineTTFTMean float64
	// EngineTPOT is the decode pods' per-request TPOT histogram, summed the
	// same way; nil with none. EngineITLMean is their mean inter-token
	// latency weighted by request rate, 0 with none: a per-token mean, not
	// a per-request one.
	EngineTPOT    []domain.LatencyBucket
	EngineITLMean float64
}

// latencyOf is the latency in measured at at, of the variants v names.
func latencyOf(in domain.AnalyzerInput, at time.Time, v SeriesVariants) CycleLatency {
	l := CycleLatency{At: at}
	if in.RouterLatency != nil {
		r := *in.RouterLatency
		l.Router = &r
	}
	roles := rolesOf(in)
	pd := false
	for _, vs := range in.VariantStates {
		if domain.CanonicalRole(vs.Role) == domain.RolePrefill {
			pd = true
		}
	}
	ttft, tpot := map[float64]float64{}, map[float64]float64{}
	var ttftW, ttftSum, itlW, itlSum float64
	for _, rm := range in.ReplicaMetrics {
		role := domain.CanonicalRole(roles[rm.VariantName])
		switch role {
		case domain.RolePrefill:
			if v.Prefill != "" && rm.VariantName != v.Prefill {
				continue
			}
		case domain.RoleDecode, domain.RoleBoth:
			if v.Decode != "" && rm.VariantName != v.Decode {
				continue
			}
		default:
			continue
		}
		w := 0.0
		if rm.RequestRateKnown && rm.RequestRate > 0 {
			w = rm.RequestRate
		}
		if role == domain.RolePrefill || !pd {
			for _, b := range rm.TTFTBuckets {
				ttft[b.UpperBound] += b.Rate
			}
			if rm.AvgTTFTKnown && w > 0 {
				ttftW += w
				ttftSum += w * rm.AvgTTFT
			}
		}
		if role != domain.RolePrefill {
			for _, b := range rm.TPOTBuckets {
				tpot[b.UpperBound] += b.Rate
			}
			if rm.AvgITLKnown && w > 0 {
				itlW += w
				itlSum += w * rm.AvgITL
			}
		}
	}
	l.EngineTTFT, l.EngineTPOT = bucketsOf(ttft), bucketsOf(tpot)
	if ttftW > 0 {
		l.EngineTTFTMean = ttftSum / ttftW
	}
	if itlW > 0 {
		l.EngineITLMean = itlSum / itlW
	}
	return l
}

// bucketsOf is summed bucket rates by bound, sorted by bound; nil with none.
func bucketsOf(m map[float64]float64) []domain.LatencyBucket {
	if len(m) == 0 {
		return nil
	}
	out := make([]domain.LatencyBucket, 0, len(m))
	for b, r := range m {
		out = append(out, domain.LatencyBucket{UpperBound: b, Rate: r})
	}
	slices.SortFunc(out, func(x, y domain.LatencyBucket) int { return cmp.Compare(x.UpperBound, y.UpperBound) })
	return out
}
