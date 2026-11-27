package whatif

import (
	"slices"
	"time"
)

// MixtureCycle is one cycle of a recording as the rank mixture reads it: what
// each prefill rank read over its window, and the prefill and decode pods'
// pooled figures (learnreplay.MixtureSeriesFrom). A cycle is a few seconds; a
// step (MixtureStepOf) pools the cycles of its window.
type MixtureCycle struct {
	At time.Time
	// Ranks is the prefill ranks that served, each keyed (Key) so that a
	// step can follow one rank across its cycles.
	Ranks []MixtureRankReading
	// Computed, Prompt and Queue are the prefill pods' histograms, summed
	// over them: computed tokens, whole prompt tokens and queue time.
	Computed, Prompt, Queue map[float64]float64
	// PrefillTTFT and DecodeTTFT are each role's mean TTFT, weighted by its
	// pods' request rates; 0 with none read.
	PrefillTTFT, DecodeTTFT float64
	// PrefillTTFTHist and DecodeTTFTHist are each role's engine TTFT
	// histogram, summed over its pods (upper bound to rate); nil with none
	// read. Only the non-linear variant reads them (FitRankMixtureNL).
	PrefillTTFTHist, DecodeTTFTHist map[float64]float64
	// RouterProcessingHist is the router's request-processing histogram
	// over its rate window (upper bound to rate), and RouterProcessingMean
	// its mean, seconds; nil and 0 with none recorded. Only the
	// non-linear variant's remainder spread reads them
	// (RankMixtureOptions.RemainderSpread).
	RouterProcessingHist map[float64]float64
	RouterProcessingMean float64
	// RouterRenderMean is the router's mean render call over the same
	// window, seconds; 0 with none recorded. Only the remainder server mode
	// reads it (RankMixtureOptions.RemainderServer).
	RouterRenderMean float64
	// EvenWithinRows counts the ranks whose own rate was not recorded: a
	// row of several ranks split evenly between them (its share profile is
	// flatter than the fleet's).
	EvenWithinRows int
}

// MixtureRankReading is one rank's reading at one cycle: Key names the rank
// (its pod, and its index within a row of several).
type MixtureRankReading struct {
	Key string
	MixtureRank
}

// MixtureStepOf pools cycles into one step at at: each rank's rate is its mean
// over the cycles (absent counts as no request), its means weighted by its
// rate; each histogram the mean of the cycles'; each role's TTFT the mean of
// the cycles that read one. Rate is the ranks' rates summed and PromptMean
// their whole prompt weighted by rate. The router's TTFT is not read here:
// the caller sets it from its measurement. The router's request-processing
// histogram is pooled as the engines' are, and its mean is the mean of the
// cycles that read one.
func MixtureStepOf(cycles []MixtureCycle, at time.Time) MixtureStep {
	out := MixtureStep{At: at}
	n := float64(len(cycles))
	if n == 0 {
		return out
	}
	type acc struct {
		rate, pf, q, ttft, x, l float64
		wpf, wq, wttft, wx, wl  float64
		// The API server's: the frontend weighted by rate, the render mean
		// by the render rate, and the render rate summed.
		fe, wfe, ren, rr float64
	}
	byKey := map[string]*acc{}
	var keys []string
	out.Computed, out.Prompt, out.Queue = map[float64]float64{}, map[float64]float64{}, map[float64]float64{}
	var pTTFT, dTTFT, np, nd, rProc, nr, rRend, nrr float64
	for _, c := range cycles {
		for _, r := range c.Ranks {
			a, ok := byKey[r.Key]
			if !ok {
				a = &acc{}
				byKey[r.Key] = a
				keys = append(keys, r.Key)
			}
			a.rate += r.Rate
			if r.RenderRate > 0 && r.Render > 0 {
				a.ren += r.RenderRate * r.Render
				a.rr += r.RenderRate
			}
			w := r.Rate
			if !(w > 0) {
				continue
			}
			if r.Frontend != 0 && Finite(r.Frontend) {
				a.fe += w * r.Frontend
				a.wfe += w
			}
			if r.PrefillTime > 0 {
				a.pf += w * r.PrefillTime
				a.wpf += w
			}
			if r.QueueTime >= 0 && Finite(r.QueueTime) {
				a.q += w * r.QueueTime
				a.wq += w
			}
			if r.TTFT > 0 {
				a.ttft += w * r.TTFT
				a.wttft += w
			}
			if r.Computed > 0 {
				a.x += w * r.Computed
				a.wx += w
			}
			if r.Prompt > 0 {
				a.l += w * r.Prompt
				a.wl += w
			}
		}
		for b, v := range c.Computed {
			out.Computed[b] += v / n
		}
		for b, v := range c.Prompt {
			out.Prompt[b] += v / n
		}
		for b, v := range c.Queue {
			out.Queue[b] += v / n
		}
		out.PrefillTTFTHist = poolHist(out.PrefillTTFTHist, c.PrefillTTFTHist, n)
		out.DecodeTTFTHist = poolHist(out.DecodeTTFTHist, c.DecodeTTFTHist, n)
		out.RouterProcessingHist = poolHist(out.RouterProcessingHist, c.RouterProcessingHist, n)
		if c.RouterProcessingMean > 0 {
			rProc += c.RouterProcessingMean
			nr++
		}
		if c.RouterRenderMean > 0 {
			rRend += c.RouterRenderMean
			nrr++
		}
		if c.PrefillTTFT > 0 {
			pTTFT += c.PrefillTTFT
			np++
		}
		if c.DecodeTTFT > 0 {
			dTTFT += c.DecodeTTFT
			nd++
		}
	}
	slices.Sort(keys)
	div := func(a, b float64) float64 {
		if b > 0 {
			return a / b
		}
		return 0
	}
	var lw float64
	for _, k := range keys {
		a := byKey[k]
		q := div(a.q, a.wq)
		if !(a.wq > 0) {
			q = -1
		}
		r := MixtureRank{Rate: a.rate / n, PrefillTime: div(a.pf, a.wpf), QueueTime: q, TTFT: div(a.ttft, a.wttft),
			Computed: div(a.x, a.wx), Prompt: div(a.l, a.wl), Frontend: div(a.fe, a.wfe), Render: div(a.ren, a.rr),
			RenderRate: a.rr / n}
		out.Ranks = append(out.Ranks, r)
		out.Rate += r.Rate
		if r.Prompt > 0 {
			out.PromptMean += r.Rate * r.Prompt
			lw += r.Rate
		}
	}
	out.PromptMean = div(out.PromptMean, lw)
	out.PrefillTTFT, out.DecodeTTFT = div(pTTFT, np), div(dTTFT, nd)
	out.RouterProcessingMean = div(rProc, nr)
	out.RouterRenderMean = div(rRend, nrr)
	return out
}
