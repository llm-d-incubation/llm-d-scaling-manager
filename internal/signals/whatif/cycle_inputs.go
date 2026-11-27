package whatif

import (
	"cmp"
	"slices"
	"strconv"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The per-cycle inputs of the experimental latency models: one analyzer
// input, as the controller's collector builds it every cycle and a learner
// recording keeps it, read into the cycle the rank-mixture TTFT model
// (MixtureCycle) and the TPOT distribution (DecodeCycle) fit and price
// from. They live here, beside the models, so that the capacity planner,
// which reads them from a recording (learnreplay.MixtureSeriesFrom and
// DecodeSeriesFrom), and the controller, which would read them from its
// own cycle (docs/proposals/predicted-latency-in-controller.md), build the
// same cycle from the same input.

// MixtureCycleOf is one cycle of in, read at at, as the rank-mixture TTFT
// model reads it: the prefill ranks of prefillVariant (every prefill row
// where it is empty) and the decode rows of decodeVariant (every decode row
// where it is empty). A prefill row of a LeaderWorkerSet group whose
// engines each recorded their own rate (domain.EngineReading.RequestRateKnown)
// gives one rank per engine; any other row gives its ranks
// (domain.ReplicaMetrics.Ranks), the row's rate split evenly between them
// and each with the row's means, counted in EvenWithinRows. The router's
// request-processing histogram and mean are the cycle's router latency's,
// where recorded.
func MixtureCycleOf(in domain.AnalyzerInput, at time.Time, prefillVariant, decodeVariant string) MixtureCycle {
	c := MixtureCycle{At: at, Computed: map[float64]float64{}, Prompt: map[float64]float64{}, Queue: map[float64]float64{}}
	roles := rolesOf(in)
	var pw, pt, dw, dt float64
	for _, rm := range in.ReplicaMetrics {
		role := domain.CanonicalRole(roles[rm.VariantName])
		switch role {
		case domain.RolePrefill:
			if prefillVariant != "" && rm.VariantName != prefillVariant {
				continue
			}
		case domain.RoleDecode:
			if decodeVariant != "" && rm.VariantName != decodeVariant {
				continue
			}
		default:
			continue
		}
		if !prefill.Serving(rm) {
			continue
		}
		rate := 0.0
		if rm.RequestRateKnown && Finite(rm.RequestRate) && rm.RequestRate > 0 {
			rate = rm.RequestRate
		}
		if role == domain.RoleDecode {
			c.DecodeTTFTHist = addBuckets(c.DecodeTTFTHist, rm.TTFTBuckets)
			if rm.AvgTTFTKnown && rate > 0 {
				dw += rate
				dt += rate * rm.AvgTTFT
			}
			continue
		}
		if rm.AvgTTFTKnown && rate > 0 {
			pw += rate
			pt += rate * rm.AvgTTFT
		}
		for _, b := range rm.ComputedTokensBuckets {
			c.Computed[b.UpperBound] += b.Rate
		}
		for _, b := range rm.PromptTokensBuckets {
			c.Prompt[b.UpperBound] += b.Rate
		}
		for _, b := range rm.QueueTimeBuckets {
			c.Queue[b.UpperBound] += b.Rate
		}
		c.PrefillTTFTHist = addBuckets(c.PrefillTTFTHist, rm.TTFTBuckets)
		if engines := rm.GroupEngines; len(engines) > 0 && engineRatesRead(engines) {
			for _, e := range engines {
				r := rankOf(e.RequestRate,
					e.AvgPrefillTime, e.AvgPrefillTimeKnown, e.AvgQueueTime, e.AvgQueueTimeKnown, e.AvgTTFT, e.AvgTTFTKnown,
					e.PrefillComputedTokenRate, e.PrefillComputedTokenRateKnown, e.AvgInputTokens)
				r.setFrontend(apiServerReading{e.AvgFrontendTime, e.AvgFrontendTimeKnown, e.AvgRenderTime, e.AvgRenderTimeKnown,
					e.RenderRate, e.RenderRateKnown}, 1)
				c.Ranks = append(c.Ranks, MixtureRankReading{Key: e.PodName, MixtureRank: r})
			}
			continue
		}
		n := rm.Ranks()
		if n > 1 {
			c.EvenWithinRows += n
		}
		api := apiServerReading{rm.AvgFrontendTime, rm.AvgFrontendTimeKnown, rm.AvgRenderTime, rm.AvgRenderTimeKnown,
			rm.RenderRate, rm.RenderRateKnown}
		for i := range n {
			r := rankOf(rate/float64(n), rm.AvgPrefillTime, rm.AvgPrefillTimeKnown, rm.AvgQueueTime, rm.AvgQueueTimeKnown,
				rm.AvgTTFT, rm.AvgTTFTKnown, rm.PrefillComputedTokenRate/float64(n), rm.PrefillComputedTokenRateKnown, rm.AvgInputTokens)
			r.setFrontend(api, n)
			c.Ranks = append(c.Ranks, MixtureRankReading{Key: rm.PodName + "#" + strconv.Itoa(i), MixtureRank: r})
		}
	}
	if pw > 0 {
		c.PrefillTTFT = pt / pw
	}
	if dw > 0 {
		c.DecodeTTFT = dt / dw
	}
	if rl := in.RouterLatency; rl != nil {
		c.RouterProcessingHist = addBuckets(nil, rl.ProcessingBuckets)
		if rl.ProcessingCountRate > 0 && Finite(rl.ProcessingSumRate) && Finite(rl.ProcessingCountRate) {
			c.RouterProcessingMean = rl.ProcessingSumRate / rl.ProcessingCountRate
		}
		if rl.RenderCountRate > 0 && Finite(rl.RenderSumRate) && Finite(rl.RenderCountRate) {
			c.RouterRenderMean = rl.RenderSumRate / rl.RenderCountRate
		}
	}
	return c
}

// apiServerReading is a row's or an engine's API-server figures with their
// bits (domain.ReplicaMetrics AvgFrontendTime, AvgRenderTime, RenderRate).
type apiServerReading struct {
	frontend      float64
	frontendKnown bool
	render        float64
	renderKnown   bool
	renderRate    float64
	rateKnown     bool
}

// setFrontend sets the rank's API-server figures from its row's, the row's
// render rate split evenly over its n ranks; a figure not known, or not
// finite, reads 0.
func (r *MixtureRank) setFrontend(a apiServerReading, n int) {
	if a.frontendKnown && Finite(a.frontend) {
		r.Frontend = a.frontend
	}
	if a.renderKnown && Finite(a.render) && a.render > 0 && a.rateKnown && Finite(a.renderRate) && a.renderRate > 0 {
		r.Render, r.RenderRate = a.render, a.renderRate/float64(max(n, 1))
	}
}

// DecodeCycleOf is one cycle of in, read at at, as the TPOT distribution
// reads it: the decode rows of decodeVariant (every decode row where it is
// empty), or, on a fleet with no decode role, its serving rows. Its
// EngineTPOT is summed as a learner recording's latency sums the engines'
// TPOT (learnreplay's CycleLatency.EngineTPOT): every decode and unsplit
// row of decodeVariant, serving or not.
func DecodeCycleOf(in domain.AnalyzerInput, at time.Time, decodeVariant string) DecodeCycle {
	c := DecodeCycle{At: at, Output: map[float64]float64{}}
	roles := rolesOf(in)
	c.EngineTPOT = engineTPOTOf(in, roles, decodeVariant)
	decode := false
	for _, rm := range in.ReplicaMetrics {
		if domain.CanonicalRole(roles[rm.VariantName]) == domain.RoleDecode {
			decode = true
			break
		}
	}
	var running float64
	for _, rm := range in.ReplicaMetrics {
		role := domain.CanonicalRole(roles[rm.VariantName])
		switch {
		case decode && role != domain.RoleDecode:
			continue
		case !decode && role == domain.RolePrefill:
			continue
		case decodeVariant != "" && rm.VariantName != decodeVariant:
			continue
		}
		if !prefill.Serving(rm) {
			continue
		}
		for _, b := range rm.OutputTokensBuckets {
			c.Output[b.UpperBound] += b.Rate
		}
		if rm.AvgITLKnown && Finite(rm.AvgITL) && rm.AvgITL > 0 && rm.RequestRateKnown && Finite(rm.RequestRate) && rm.RequestRate > 0 {
			g := DecodeGroup{Rate: rm.RequestRate, TokenTime: rm.AvgITL}
			if rm.SpecAcceptedPerStepKnown && Finite(rm.SpecAcceptedPerStep) && rm.SpecAcceptedPerStep >= 0 {
				g.Accepted, g.AcceptedKnown = rm.SpecAcceptedPerStep, true
				g.TokenTime = rm.AvgITL / (1 + rm.SpecAcceptedPerStep)
			}
			c.Groups = append(c.Groups, g)
		}
		if engines := rm.GroupEngines; len(engines) > 0 {
			for _, e := range engines {
				if e.RunningRequestsKnown {
					running += float64(e.RunningRequests)
					c.MostRunning = max(c.MostRunning, float64(e.RunningRequests))
					c.RanksReported++
				}
			}
			continue
		}
		if rm.RunningRequestsKnown {
			n := max(rm.Ranks(), 1)
			running += float64(rm.RunningRequests)
			c.MostRunning = max(c.MostRunning, float64(rm.RunningRequests)/float64(n))
			c.RanksReported += n
		}
	}
	if c.RanksReported > 0 {
		c.MeanRunning = running / float64(c.RanksReported)
	}
	return c
}

// engineTPOTOf is the engines' per-request TPOT histogram of in's decode
// and unsplit rows of decodeVariant (every such row where it is empty), each
// bound's rate summed over the rows and sorted by bound; nil with none.
func engineTPOTOf(in domain.AnalyzerInput, roles map[string]string, decodeVariant string) []domain.LatencyBucket {
	sum := map[float64]float64{}
	for _, rm := range in.ReplicaMetrics {
		switch domain.CanonicalRole(roles[rm.VariantName]) {
		case domain.RoleDecode, domain.RoleBoth:
		default:
			continue
		}
		if decodeVariant != "" && rm.VariantName != decodeVariant {
			continue
		}
		for _, b := range rm.TPOTBuckets {
			sum[b.UpperBound] += b.Rate
		}
	}
	if len(sum) == 0 {
		return nil
	}
	out := make([]domain.LatencyBucket, 0, len(sum))
	for b, r := range sum {
		out = append(out, domain.LatencyBucket{UpperBound: b, Rate: r})
	}
	slices.SortFunc(out, func(x, y domain.LatencyBucket) int { return cmp.Compare(x.UpperBound, y.UpperBound) })
	return out
}

// rolesOf is each variant's role as in's variant states give it.
func rolesOf(in domain.AnalyzerInput) map[string]string {
	roles := map[string]string{}
	for _, vs := range in.VariantStates {
		roles[vs.VariantName] = vs.Role
	}
	return roles
}

// addBuckets is h with bs added bucket by bucket; nil while there is none.
func addBuckets(h map[float64]float64, bs []domain.LatencyBucket) map[float64]float64 {
	for _, b := range bs {
		if h == nil {
			h = map[float64]float64{}
		}
		h[b.UpperBound] += b.Rate
	}
	return h
}

// engineRatesRead says every engine of a group recorded its own request rate.
func engineRatesRead(engines []domain.EngineReading) bool {
	for _, e := range engines {
		if !e.RequestRateKnown {
			return false
		}
	}
	return true
}

// rankOf is one rank's reading: a mean not known reads 0, a queue time not
// known -1, and the computed prompt is the computed rate over the request rate.
func rankOf(rate, pf float64, pfKnown bool, q float64, qKnown bool, ttft float64, ttftKnown bool,
	computed float64, computedKnown bool, prompt float64) MixtureRank {
	r := MixtureRank{Rate: max(rate, 0), QueueTime: -1}
	if !Finite(r.Rate) {
		r.Rate = 0
	}
	if pfKnown && Finite(pf) {
		r.PrefillTime = pf
	}
	if qKnown && Finite(q) && q >= 0 {
		r.QueueTime = q
	}
	if ttftKnown && Finite(ttft) {
		r.TTFT = ttft
	}
	if computedKnown && Finite(computed) && r.Rate > 0 {
		r.Computed = computed / r.Rate
	}
	if Finite(prompt) {
		r.Prompt = prompt
	}
	return r
}
