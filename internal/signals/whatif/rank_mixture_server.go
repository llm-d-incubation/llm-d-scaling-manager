package whatif

import (
	"fmt"
	"math"
	"math/rand/v2"
)

// Remainder server (RankMixtureOptions.RemainderServer), a third mode of
// QueueRankMixtureNLLine, on top of the remainder spread
// (rank_mixture_modes.go). The router's remainder (its TTFT less both
// engines') is drawn as what the serving pods' API servers and the router
// itself measure it to be, not as one line:
//
//	R = (O + Ren(L)) Q(u) + Fp(L) + N(L)
//
//	O    the router's request processing less its render call, the fit
//	     window's median of the router's processing mean less its render
//	     mean (llm_d_epp_token_producer_render_duration_seconds); a line in
//	     the prompt where the router renders nothing
//	Ren  the render call on the serving pod's API server (its /render
//	     handler), a line in the prompt: R0 + R1 L
//	Fp   the generation request's time in the pod's API server outside the
//	     engine (its HTTP handler less vLLM's end-to-end latency), a line in
//	     the prompt: F0 + F1 L; 0 where no pod recorded it
//	N    the rest, which nothing measures (the sidecar, the network, the
//	     decode pod's API server): a non-negative line in the prompt,
//	     fitted to what the parts above leave of each fit step's remainder
//	Q    the remainder spread's shape: the router's request-processing law
//	     over its mean, so O + Ren is drawn on the router's measured
//	     processing law, scaled to its predicted mean
//
// The pods read are the ranks the model prices: the prefill pods
// (MixtureCycleOf reads no aggregated row). A decode pod's API server is
// never one of them, and is in N. Each part is a delay: no part moves with the rate. On a production
// P/D recording, and on a second, aggregated one, a queue term on the pods'
// API-server load added nothing, so there is none.
//
// The lines are fitted on the pod-steps, each weighted by the square root
// of its requests, with a soft-L1 loss (robust to the few pod-steps a
// restart or a burst leaves far from the line). The mode needs the
// remainder spread's shape and MinRemainderServerSteps fit steps with the
// router's processing and a pod's render or frontend recorded; without
// either, the remainder is the plain spread (or its line, as the spread
// falls back to it), and the assumed line says so.
//
// N is the part the model cannot see. Where it is over MaxRemainderRestShare
// of the fit window's remainder, a figure priced past the fit window's top
// rate is flagged RemainderRestPastFit: the rest's line was read only over
// the loads the fit window held, and nothing says how it moves past them.

// MinRemainderServerSteps is the fewest fit steps with the router's
// processing and a pod's render or frontend the remainder server is fitted
// on (RankMixtureOptions.RemainderServer).
const MinRemainderServerSteps = 12

// MaxRemainderRestShare is the share of the fit window's remainder the
// unmeasured rest (ZServer.N0, N1) may hold before a figure priced past the
// fit window's top rate is flagged RemainderRestPastFit.
const MaxRemainderRestShare = 0.5

// RemainderRestPastFit is a TTFT priced with the remainder server at a rate
// past the fit window's top, where the remainder's unmeasured rest is over
// MaxRemainderRestShare of it: that part is a line read over the fit
// window's loads only.
const RemainderRestPastFit Flag = "RemainderRestPastFit"

// remainderServerLossScale is the soft-L1 loss's scale on a pod-step's
// residual, the residual a share of the pod-steps' median and weighted by
// the square root of its requests.
const remainderServerLossScale = 0.3

// remainderServerRounds is the reweighting rounds of the soft-L1 fit.
const remainderServerRounds = 30

// minServerPodRequests is the fewest requests a pod-step is read at.
const minServerPodRequests = 5

// ZServer is the router remainder read as the pods' API servers and the
// router's own work (remainder server, above), seconds and seconds a prompt
// token.
type ZServer struct {
	// R0 and R1 are the render handler's line, F0 and F1 the generation
	// frontend's. HasRender says the pods recorded their /render handler:
	// without it the render part is 0 and O is the router's whole
	// processing, a line in the prompt (Other + Other1 L). HasFrontend
	// says they recorded the frontend: without it F0 and F1 are 0, and the
	// frontend is in the rest.
	R0, R1, F0, F1         float64
	HasRender, HasFrontend bool
	// Other and Other1 are the router's work outside its render call.
	Other, Other1 float64
	// N0 and N1 are the rest's line, and RestShare its median share of the
	// fit steps' remainder.
	N0, N1, RestShare float64
	// TopRate is the fit steps' highest rate, requests a second: past it,
	// with RestShare over MaxRemainderRestShare, a figure is flagged
	// RemainderRestPastFit.
	TopRate float64
	// Steps and PodSteps are the fit steps and pod-steps it was fitted on.
	Steps, PodSteps int
}

// serverRow is one pod-step as the remainder server fits it.
type serverRow struct {
	l, f, h                float64 // prompt, frontend and render means
	w                      float64 // the square root of its requests
	hasFrontend, hasRender bool
}

// serverStep is one fit step's router figures.
type serverStep struct {
	l, rate, r, proc, rend float64
}

// FitZServer fits the remainder server on steps of stepSeconds each. off is
// why it was not fitted, "" where it was.
func FitZServer(steps []MixtureStep, stepSeconds float64) (z *ZServer, off string) {
	var rows []serverRow
	var sts []serverStep
	for _, st := range steps {
		if !(st.PromptMean > 0) || !(st.DecodeTTFT > 0) || !(st.PrefillTTFT > 0) || !(st.RouterMean > 0) ||
			!(st.RouterProcessingMean > 0) || !(st.Rate > 0) {
			continue
		}
		n := len(rows)
		for _, r := range st.Ranks {
			if !(r.Prompt > 0) || !(r.Rate*stepSeconds >= minServerPodRequests) {
				continue
			}
			row := serverRow{l: r.Prompt, f: r.Frontend, h: r.Render, w: math.Sqrt(r.Rate * stepSeconds),
				hasFrontend: r.Frontend != 0 && Finite(r.Frontend), hasRender: r.RenderRate > 0 && r.Render > 0}
			if row.hasFrontend || row.hasRender {
				rows = append(rows, row)
			}
		}
		if len(rows) == n {
			continue
		}
		sts = append(sts, serverStep{l: st.PromptMean, rate: st.Rate, r: st.RouterMean - st.PrefillTTFT - st.DecodeTTFT,
			proc: st.RouterProcessingMean, rend: st.RouterRenderMean})
	}
	if len(sts) < MinRemainderServerSteps {
		return nil, fmt.Sprintf("fewer than %d fit steps hold the router's processing and a pod's API-server render or "+
			"frontend (not recorded, or not exported)", MinRemainderServerSteps)
	}
	z = &ZServer{Steps: len(sts), PodSteps: len(rows)}
	var withRender, withFrontend int
	for _, r := range rows {
		if r.hasRender {
			withRender++
		}
		if r.hasFrontend {
			withFrontend++
		}
	}
	z.HasRender, z.HasFrontend = withRender > len(rows)/2, withFrontend > len(rows)/2
	if z.HasRender {
		z.R0, z.R1 = robustLine(rows, func(r serverRow) (float64, bool) { return r.h, r.hasRender })
	}
	if z.HasFrontend {
		z.F0, z.F1 = robustLine(rows, func(r serverRow) (float64, bool) { return r.f, r.hasFrontend })
	}
	// The router's own work outside the render call. A step whose router
	// render was not read takes the pods' render line in its place: its
	// whole processing would count the render twice, once here and once in
	// the render line.
	if z.HasRender {
		o := make([]float64, len(sts))
		for i, s := range sts {
			rend := s.rend
			if !(rend > 0) || !Finite(rend) {
				rend = z.render(s.l)
			}
			o[i] = s.proc - rend
		}
		if z.Other = medianOf(o); !(z.Other > 0) {
			z.Other = 0
		}
	} else {
		x, y := make([]float64, len(sts)), make([]float64, len(sts))
		for i, s := range sts {
			x[i], y[i] = s.l, s.proc
		}
		z.Other, z.Other1 = nnLine(x, y, nil)
	}
	// The rest: what the parts leave of each step's remainder.
	x, y := make([]float64, len(sts)), make([]float64, len(sts))
	for i, s := range sts {
		x[i], y[i] = s.l, s.r-z.measured(s.l)
		z.TopRate = max(z.TopRate, s.rate)
	}
	z.N0, z.N1 = nnLine(x, y, nil)
	var shares []float64
	for _, s := range sts {
		if s.r > 0 {
			shares = append(shares, (z.N0+z.N1*s.l)/s.r)
		}
	}
	if len(shares) > 0 {
		z.RestShare = medianOf(shares)
	}
	return z, ""
}

// robustLine is the non-negative line in the prompt through the rows' values
// that value reads (ok false skips a row), each weighted by the square root
// of its requests, under the soft-L1 loss: iteratively reweighted least
// squares, the residual a share of the values' median.
func robustLine(rows []serverRow, value func(serverRow) (float64, bool)) (a, b float64) {
	var ls, ys, ws []float64
	for _, r := range rows {
		if v, ok := value(r); ok {
			ls, ys, ws = append(ls, r.l), append(ys, v), append(ws, r.w)
		}
	}
	m := max(math.Abs(medianOf(ys)), 1e-9)
	wt := make([]float64, len(ys))
	for round := range remainderServerRounds {
		for i := range ys {
			base := ws[i] / m
			rw := 1.0
			if round > 0 {
				res := (a + b*ls[i] - ys[i]) * base / remainderServerLossScale
				rw = 1 / math.Sqrt(1+res*res)
			}
			wt[i] = base * base * rw
		}
		a, b = nnLine(ls, ys, wt)
	}
	return a, b
}

// render and frontend are the render and generation frontend at prompt l: 0
// where not recorded.
func (z *ZServer) render(l float64) float64 {
	if !z.HasRender {
		return 0
	}
	return z.R0 + z.R1*l
}

func (z *ZServer) frontend(l float64) float64 {
	if !z.HasFrontend {
		return 0
	}
	return z.F0 + z.F1*l
}

// other is the router's work outside its render call at prompt l.
func (z *ZServer) other(l float64) float64 {
	if z.HasRender {
		return z.Other
	}
	return z.Other + z.Other1*l
}

// measured is the parts the server measures at prompt l: the router's work,
// the render and the frontend; the rest left out.
func (z *ZServer) measured(l float64) float64 {
	return z.other(l) + z.render(l) + z.frontend(l)
}

// Mean is the server's mean remainder at prompt l, seconds.
func (z *ZServer) Mean(l float64) float64 {
	return z.measured(l) + z.N0 + z.N1*l
}

// restPastFit says a figure at rate is priced past the fit window's top
// rate with the rest over MaxRemainderRestShare of the remainder.
func (z *ZServer) restPastFit(rate float64) bool {
	return z != nil && z.RestShare > MaxRemainderRestShare && rate > z.TopRate
}

// zServerSeed fixes the server's draws.
const zServerSeed = zSeed + 7

// draws is each request's remainder at its prompt l: the router's work and
// the render at the requests' mean prompt, drawn on shape (mean 1), the
// frontend and the rest at each request's own prompt.
func (z *ZServer) draws(l []float64, shape []float64) []float64 {
	out := make([]float64, len(l))
	if len(l) == 0 {
		return out
	}
	var lb float64
	for _, li := range l {
		lb += li
	}
	lb /= float64(len(l))
	router := z.other(lb) + z.render(lb)
	rng := rand.New(rand.NewPCG(zServerSeed, uint64(len(l))))
	for i, li := range l {
		out[i] = router*shapeAt(shape, rng.Float64()) + z.frontend(li) + z.N0 + z.N1*li
	}
	return out
}

// nnLine is the weighted least-squares line y = a + b x with a and b held
// at 0 or above (nil w weighs each point 1): the free line where it is
// feasible, else the best of the line through the origin, the mean and 0.
func nnLine(x, y, w []float64) (a, b float64) {
	weight := func(i int) float64 {
		if w == nil {
			return 1
		}
		return w[i]
	}
	var sw, sx, sy, sxx, sxy float64
	for i := range x {
		wi := weight(i)
		sw += wi
		sx += wi * x[i]
		sy += wi * y[i]
		sxx += wi * x[i] * x[i]
		sxy += wi * x[i] * y[i]
	}
	if !(sw > 0) {
		return 0, 0
	}
	if d := sw*sxx - sx*sx; d > 1e-12*sw*sxx {
		b = (sw*sxy - sx*sy) / d
		a = (sy - b*sx) / sw
		if a >= 0 && b >= 0 {
			return a, b
		}
	}
	sse := func(a, b float64) float64 {
		var e float64
		for i := range x {
			r := y[i] - a - b*x[i]
			e += weight(i) * r * r
		}
		return e
	}
	a, b = 0, 0
	best := sse(0, 0)
	if m := sy / sw; m > 0 && sse(m, 0) < best {
		a, b, best = m, 0, sse(m, 0)
	}
	if sxx > 0 {
		if s := sxy / sxx; s > 0 && sse(0, s) < best {
			a, b = 0, s
		}
	}
	return a, b
}
