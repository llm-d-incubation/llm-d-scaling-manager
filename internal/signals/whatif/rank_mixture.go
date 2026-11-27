package whatif

import (
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
)

// This file is an EXPERIMENTAL TTFT model for a prefill/decode fleet whose
// prefill engines run chunked prefill on several data-parallel ranks
// (QueueRankMixture). It is priced only where the caller asks for it
// (the capacity planner's --experimental-ttft=v2); nothing in the
// controller reads it, and no default moves. Every figure it gives is
// outside the validated region. The literature it rests on is reviewed in
// the capacity planner's queueing notes (#387); each part below names the
// result it implements, the assumption that result needs, and the check
// that says whether the data priced meets it (RankMixtureChecks).
//
// The router's TTFT of one request is
//
//	T = Z + q + P
//
//	P  the prefill sojourn on the rank the request lands on;
//	q  the wait for its KV to load before the engine schedules it;
//	Z  everything else: the engine's work before its queue, the decode
//	   engine's first token and the router's remainder.
//
// Prefill sojourn, per rank: limited processor sharing, M/G/1-LPS(K)
// (QueueLimitedProcessorSharing). A chunked-prefill engine does not queue a
// prompt behind the one it computes: it runs up to K = max-num-seqs prompts
// at once and splits each step's token budget between them, and a prompt
// that finds K running waits first come in front of them. That is limited
// processor sharing (Avi-Itzhak & Halfin, J. Appl. Prob. 24(4), 1987; Zhang,
// Dai & Zwart, Math. Oper. Res. 34(4), 2009, and Ann. Appl. Prob. 21(2), 2011).
//
//   - Admission. While fewer than K prompts are present the rank is
//     processor sharing, whose number in system is insensitive to the
//     service law (Cohen's generalized PS): P(N >= K) = rho^K at Poisson
//     arrivals. That is Zhang and Zwart's delay probability d_p at Ca^2 1
//     (Zhang & Zwart, Queueing Systems 60, 2008; Zhang's thesis eq. 6.28).
//   - Mean. Zhang and Zwart's two-moment approximation (thesis eq. 6.31, at
//     Ca^2 1): E[R] = E[S]/(1 - rho) [(1 - rho^K) + rho^K (1 + cs^2)/2], cs^2
//     the service time's squared coefficient of variation, read from the
//     same computed-token histogram as the service law. At K = 1 it is
//     Kingman's (first come); as rho^K falls it is the PS mean,
//     E[T | x] = S(x)/(1 - rho) (Kleinrock).
//   - Distribution. A request is admitted at once with probability
//     1 - rho^K and then takes the PS sojourn of its size, with the planner's
//     two-moment PS spread (psStage at Poisson arrivals). With probability
//     rho^K it first waits an exponential admission wait, then takes the
//     same PS sojourn. The wait's mean w makes the mean Zhang and Zwart's:
//     E[S]/(1 - rho) + rho^K w = E[S]/(1 - rho) [(1 - rho^K) + rho^K (1 + cs^2)/2],
//     so w = E[S] (cs^2 - 1) / (2 (1 - rho)) (LPSAdmissionWait), held at 0
//     where cs^2 < 1 (the rank then reads its PS mean, a little high).
//     Zhang's thesis splits the same mean otherwise, a conditional buffer
//     time of (1 + cs^2)/2 E[S]/(1 - rho) and a service stretch reduced by
//     1 - d_p (eqs. 6.29, 6.30); the two give one mean. In vLLM the admission
//     wait is queue time, not prefill time.
//   - Validity. The two-moment form is a heavy-traffic approximation,
//     tested by its authors at cs^2 up to 19 and noted as poor for lognormal
//     service; its error at moderate load and at a heavier tail is not
//     known. Check: a rank above MaxLPSRho is flagged
//     LimitedPSHeavyTraffic; the share of requests that wait for admission
//     (sum p_i rho_i^K) is reported. Tail: Nair, Wierman & Zwart (Allerton
//     2010, Theorem 4) give LPS-K's sojourn tail index
//     min{theta, (theta - 1)(K - floor(K rho))} for a regularly varying
//     service of index theta; it is reported for the busiest rank, theta
//     read from the histogram's top buckets (RankMixtureChecks.TailIndex).
//     With K unknown (Slots 0) the rank is priced as plain PS.
//
// The service time is a line in the computed tokens, S(x) = S0 + x/V, and x
// is drawn from the computed-token histogram, piecewise inside its buckets
// (log-uniform, uniform in the first, the +Inf bucket held at its lower
// edge).
//
// Mixture over ranks. A router that sends each request to one rank, at
// random with fixed shares p_i, splits a Poisson stream into independent
// Poisson streams of rate p_i lambda (Poisson splitting), so each rank is its
// own M/G/1-LPS(K) queue and the population's tail is the mixture
// P(T > t) = sum_i p_i P(T_i > t). Assumption: the shares are stable over
// the window they were fitted on. Check: RankMixtureFit.ShareCV, the
// spread over the fit's steps of the busiest rank's share, reported.
//
// KV-deferral wait (FITTED, not derived). q = 0 with probability
// 1 - p_d(rho), otherwise exponential of mean m_d(rho): an atom at zero and an
// exponential tail, the shape of an Erlang-C wait. Neither p_d nor m_d comes
// from a queue model: each is a line in the ranks' mean utilization, fitted
// to the engines' queue-time histogram at two edges (P(q > 0.3 s) and
// P(q > 1 s)), and it is priced only inside the utilizations it was fitted
// over (checked: RankMixtureChecks.KVOutside).
//
// Non-queue part (not queueing). Z = A0 + Spread * Beta * L, a deterministic
// cost per whole-prompt token, L drawn from the prompt-length histogram.
// Beta is the sum of three fitted slopes against the prompt: the engine's
// pre-queue overhead (per rank), the decode engine's first token and the
// router's remainder (per step). Spread is a CALIBRATION, fitted only so
// that the fit window's median p90 ratio is 1 on self-check folds; p95 is
// held out as the shape check. Spread 1 is what the model expects when Beta
// prices all of the prompt-length spread; how far it is from 1 is
// reported as a diagnostic of what Beta misses. It is calibrated only on a
// window of MinSpreadFitWindow or longer, and priced at MinSpread or above;
// on a shorter window it is 1.
//
// Pointwise-stationary approximation (Green & Kolesar 1991; Whitt 1991).
// Each step is priced as if its rate held for ever. Assumption: a rank's
// relaxation time, of order E[S]/(1 - rho)^2, is far below the step.
// Check: RankMixtureChecks.Relaxation, against MaxRelaxationShare of the
// step.

// QueueRankMixture is the experimental TTFT model (RankMixture): the prefill
// queue priced per data-parallel rank as limited processor sharing
// (QueueLimitedProcessorSharing), mixed over the ranks' measured traffic
// shares, with a fitted KV-deferral wait and a per-prompt-token non-queue
// part. Experimental; only a caller that asks prices it.
const QueueRankMixture QueueModel = "rank-mixture"

// QueueLimitedProcessorSharing is one prefill rank priced as M/G/1-LPS(K),
// K its max-num-seqs: processor sharing among the K prompts it runs, first
// come in front of them (rank_mixture.go).
const QueueLimitedProcessorSharing QueueModel = "limited-processor-sharing"

// MaxLPSRho is the rank utilization past which the limited-PS figure is
// flagged LimitedPSHeavyTraffic: Zhang and Zwart's form is a heavy-traffic
// approximation, and the model is not taken to hold that close to the knee.
const MaxLPSRho = 0.9

// KVDeferral is the fitted KV-deferral wait: with probability
// P0 + P1 rho (held in [MinKVDeferralP, 1]) a request waits an exponential
// of mean M0 + M1 rho (at least MinKVDeferralMean) seconds; otherwise it
// waits none. RhoLo and RhoHi are the utilizations it was fitted over.
type KVDeferral struct {
	P0, P1, M0, M1 float64
	RhoLo, RhoHi   float64
	// Known says it was fitted; unknown, q is 0.
	Known bool
	// RhoHold is the utilization the lines are read no further than
	// (RankMixtureOptions.KVHold, rank_mixture_modes.go): past it the wait
	// is held, not extrapolated. 0 holds nothing; the lines then run to
	// MaxKVRho.
	RhoHold float64
}

// MinKVDeferralP and MinKVDeferralMean bound the fitted lines where they
// run out of the data: a line in rho may fall below 0 outside the range it
// was fitted on.
const (
	MinKVDeferralP    = 0.001
	MinKVDeferralMean = 0.05
)

// MaxKVRho caps the utilization the KV lines are read at.
const MaxKVRho = 0.95

// at is the deferral's probability and mean at rho, read no further than
// RhoHold where it is set.
func (k KVDeferral) at(rho float64) (p, m float64) {
	rho = min(rho, MaxKVRho)
	if k.RhoHold > 0 {
		rho = min(rho, k.RhoHold)
	}
	return min(max(k.P0+k.P1*rho, MinKVDeferralP), 1), max(k.M0+k.M1*rho, MinKVDeferralMean)
}

// RankMixture is the experimental model's fitted parameters (QueueRankMixture).
type RankMixture struct {
	// S0 and V are the service line S(x) = S0 + x/V, seconds and computed
	// tokens a second. ServiceProportional says the line was not identified
	// (the prompts did not spread) and is S0 0 through the mean
	// (FitPSInversion).
	S0, V               float64
	ServiceProportional bool
	// Shares is each rank's share of the requests, busiest first, summing
	// to 1. Its length is the ranks priced.
	Shares []float64
	// KV is the KV-deferral wait.
	KV KVDeferral
	// A0 is Z's constant, seconds; Beta its seconds per whole-prompt
	// token, the sum of BetaOverhead, BetaDecode and BetaRemainder (each
	// held at 0 or above); Spread the calibration on Beta.
	A0, Beta, Spread                        float64
	BetaOverhead, BetaDecode, BetaRemainder float64
	// BetaUnidentified names the slopes the prompts did not spread enough
	// to fit (FitBeta): each is 0.
	BetaUnidentified []string
	// Slots is K, the prompts one rank runs at once (max-num-seqs with
	// chunked prefill, 1 without); 0 is not known, and each rank is priced
	// as plain processor sharing.
	Slots int
	// ShareRows is the fit steps' share profiles at their rates where the
	// profile is read at the priced load (RankMixtureOptions.SharesAtLoad);
	// nil prices Shares.
	ShareRows []ShareRow
	// CleanSteps and OverloadedSteps are the fit steps under and at or over
	// MaxCleanFitRho where the KV-deferral lines and the prompt slopes are
	// fitted on the clean steps (RankMixtureOptions.CleanFitSteps); both 0
	// without it.
	CleanSteps, OverloadedSteps int
	// Stratified reads each rank's mean service and cs^2 integrated over
	// the computed-token law and draws the ranks stratified
	// (rank_mixture_stratified.go); false reads them from each rank's own
	// draws.
	Stratified bool
	// Parts is Z measured part by part (FitRankMixtureNL,
	// QueueRankMixtureNLLine): A0 and Beta are then their sums, Spread is 1
	// and each request's Z is drawn from the parts; nil is v2's Z.
	Parts *ZParts
	// KVHold and RemainderSpread say the fit was asked for those modes of
	// QueueRankMixtureNLLine (RankMixtureOptions, rank_mixture_modes.go);
	// KV.RhoHold and Parts.Remainder.Shape say whether each applies.
	KVHold, RemainderSpread bool
	// RemainderServer says the fit was asked for the remainder server
	// (RankMixtureOptions.RemainderServer, rank_mixture_server.go):
	// Parts.Remainder.Server says whether it applies, and
	// RemainderServerOff why not, "" where it does.
	RemainderServer    bool
	RemainderServerOff string
}

// MaxRelaxationShare is the largest relaxation time, as a share of the step
// priced, at which the pointwise-stationary approximation is taken to hold.
const MaxRelaxationShare = 0.1

// RankMixtureChecks is what the data priced says of the model's
// assumptions, at one rate.
type RankMixtureChecks struct {
	// Rho is each rank's utilization, busiest first.
	Rho []float64
	// AdmissionShare is the share of requests that wait for admission,
	// sum p_i rho_i^K; BusiestRhoK the busiest rank's rho^K and
	// AdmissionWait its mean admission wait, seconds (LPSAdmissionWait).
	// All 0 with Slots unknown.
	AdmissionShare, BusiestRhoK, AdmissionWait float64
	// TailIndex is the busiest rank's sojourn tail index by Nair, Wierman
	// and Zwart, min{theta, (theta - 1)(K - floor(K rho))}, and
	// ServiceTailIndex the theta it was read from (the computed-token
	// histogram's top two edges); 0 where not read, and TailIndex 0 too
	// where theta is 1 or under, outside the theorem.
	TailIndex, ServiceTailIndex float64
	// HeavyTraffic says a rank's rho is above MaxLPSRho.
	HeavyTraffic bool
	// Relaxation is the busiest rank's E[S]/(1 - rho)^2, seconds.
	Relaxation float64
	// KVOutside says the ranks' mean utilization is outside the range the
	// KV-deferral lines were fitted over.
	KVOutside bool
	// RemainderRestPastFit says the rate is past the fit window's top with
	// the remainder server's unmeasured rest over MaxRemainderRestShare
	// (rank_mixture_server.go).
	RemainderRestPastFit bool
}

// Flags is the checks as figure flags, for a step of stepSeconds (0 skips
// the relaxation check).
func (c RankMixtureChecks) Flags(stepSeconds float64) []Flag {
	var out []Flag
	if c.HeavyTraffic {
		out = append(out, LimitedPSHeavyTraffic)
	}
	if stepSeconds > 0 && c.Relaxation > MaxRelaxationShare*stepSeconds {
		out = append(out, RelaxationLong)
	}
	if c.KVOutside {
		out = append(out, KVDeferralOutside)
	}
	if c.RemainderRestPastFit {
		out = append(out, RemainderRestPastFit)
	}
	return out
}

// The flags of the experimental model.
const (
	// RankMixtureTTFT is on every TTFT priced by the experimental model
	// (QueueRankMixture): OUTSIDE VALIDATED REGION.
	RankMixtureTTFT Flag = "RankMixtureTTFT"
	// RankMixtureShortFitWindow is on every TTFT priced by the experimental
	// model, or its variant, fitted on a window shorter than
	// MinSpreadFitWindow because the caller lowered the minimum: the
	// window's own checks read fewer loads and hours than the default
	// asks.
	RankMixtureShortFitWindow Flag = "RankMixtureShortFitWindow"
	// RankMixtureAbstained is a TTFT the planner priced itself where the
	// experimental model was asked for and abstained: an identifiability
	// check failed, and the model prices nothing it cannot identify.
	RankMixtureAbstained Flag = "RankMixtureAbstained"
	// LimitedPSHeavyTraffic is a figure with a rank above MaxLPSRho, where
	// the limited-PS approximation is not taken to hold.
	LimitedPSHeavyTraffic Flag = "LimitedPSHeavyTraffic"
	// RelaxationLong is a figure whose busiest rank relaxes over more than
	// MaxRelaxationShare of the step: the pointwise-stationary approximation
	// is not taken to hold.
	RelaxationLong Flag = "RelaxationLong"
	// KVDeferralOutside is a figure priced at a utilization outside the
	// range the KV-deferral lines were fitted over.
	KVDeferralOutside Flag = "KVDeferralOutside"
)

// cumHist is a cumulative histogram as a sampler: the buckets that hold
// requests, each with its range and the cumulative share at its end.
type cumHist struct {
	lo, hi, cum []float64
}

// newCumHist reads b, upper bound to cumulative rate (+Inf the total). ok
// is false where it holds no requests.
func newCumHist(b map[float64]float64) (cumHist, bool) {
	les := make([]float64, 0, len(b))
	for le := range b {
		if !math.IsNaN(le) {
			les = append(les, le)
		}
	}
	slices.Sort(les)
	var h cumHist
	prevC, prevLe := 0.0, 0.0
	var total float64
	for _, le := range les {
		c := b[le]
		if !Finite(c) {
			continue
		}
		if k := c - prevC; k > 0 {
			hi := le
			if math.IsInf(le, 1) {
				hi = prevLe
			}
			lo := max(prevLe, 1)
			h.lo, h.hi = append(h.lo, lo), append(h.hi, max(hi, lo))
			total += k
			h.cum = append(h.cum, total)
			prevC = c
		}
		if !math.IsInf(le, 1) {
			prevLe = le
		}
	}
	if !(total > 0) {
		return cumHist{}, false
	}
	for i := range h.cum {
		h.cum[i] /= total
	}
	return h, true
}

// ppf is the length at cumulative share u in (0, 1): log-uniform inside a
// bucket, uniform in one starting at or under 1.
func (h cumHist) ppf(u float64) float64 {
	b, _ := slices.BinarySearch(h.cum, u)
	b = min(b, len(h.cum)-1)
	lo, hi := h.lo[b], h.hi[b]
	start := 0.0
	if b > 0 {
		start = h.cum[b-1]
	}
	v := min(max((u-start)/(h.cum[b]-start), 0), 1)
	if lo <= 1 {
		return lo + v*(hi-lo)
	}
	return math.Exp(math.Log(lo) + v*(math.Log(hi)-math.Log(lo)))
}

// RankMixtureDraws is the model's requests at one rate: each the queue and
// prefill time (QP) and the whole prompt (L), with its weight in the
// population. Z is added at pricing, so one set of draws prices any A0 and
// Spread.
type RankMixtureDraws struct {
	QP, L, W []float64
	// Z is each request's non-queue part where it is drawn (ZParts.draws);
	// Stats then adds it in place of a0 + sb L.
	Z []float64
	// Checks is the assumptions' checks at the rate.
	Checks RankMixtureChecks
}

// DefaultMixtureDraws is the requests drawn per priced rate.
const DefaultMixtureDraws = 16000

// mixtureSeed fixes the draws, so a figure is the same on every run.
const mixtureSeed = 0x5eed

// Draws draws n requests at fleet rate (requests a second) whose computed
// tokens follow computed and whole prompts follow prompt (cumulative
// histograms, bound to rate; +Inf the total). It refuses (Saturated) where a
// rank's utilization reaches 1, and (NoPrefillCapacity) where a histogram holds
// nothing or the line is not set.
func (m RankMixture) Draws(rate float64, computed, prompt map[float64]float64, n int) (RankMixtureDraws, *Unanswerable) {
	if !(m.V > 0) || len(m.Shares) == 0 || !(rate > 0) || !Finite(rate) {
		u := refuse(NoPrefillCapacity, "the rank mixture needs a service line, the ranks' shares and a rate")
		return RankMixtureDraws{}, &u
	}
	hx, okx := newCumHist(computed)
	hl, okl := newCumHist(prompt)
	if !okx || !okl {
		u := refuse(NoPrefillCapacity, "the rank mixture needs the computed-token and the prompt-length histograms")
		return RankMixtureDraws{}, &u
	}
	if n <= 0 {
		n = DefaultMixtureDraws
	}
	shares := m.sharesAt(rate)
	per := max(n/len(shares), 50)
	pooledES, pooledCS2 := 0.0, 0.0
	if m.Stratified {
		per = stratifiedPer(per, len(shares))
		pooledES, pooledCS2 = m.serviceMoments(hx)
	}
	rng := rand.New(rand.NewPCG(mixtureSeed, uint64(len(shares))))
	out := RankMixtureDraws{QP: make([]float64, 0, per*len(shares)), L: make([]float64, 0, per*len(shares)),
		W: make([]float64, 0, per*len(shares))}
	s := make([]float64, per)
	var rhoSum float64
	for i, p := range shares {
		lam := rate * p
		var es, es2 float64
		for j := range s {
			u := rng.Float64()
			if m.Stratified {
				u = (float64(j) + u) / float64(per)
			}
			s[j] = m.S0 + hx.ppf(u)/m.V
			es += s[j]
			es2 += s[j] * s[j]
		}
		es /= float64(per)
		cs2 := max(es2/float64(per)/(es*es)-1, 0)
		if m.Stratified && pooledES > 0 {
			es, cs2 = pooledES, pooledCS2
		}
		rho := lam * es
		out.Checks.Rho = append(out.Checks.Rho, rho)
		rhoSum += rho
		if !(rho < 1) {
			u := refuse(Saturated, fmt.Sprintf("rank %d of %d is saturated at %.4g r/s (utilization %.3g)", i+1, len(shares), rate, rho))
			return RankMixtureDraws{}, &u
		}
		pK, wait := 0.0, 0.0
		if m.Slots > 0 {
			pK, wait = math.Pow(rho, float64(m.Slots)), LPSAdmissionWait(es, rho, cs2)
			out.Checks.AdmissionShare += p * pK
		}
		out.Checks.HeavyTraffic = out.Checks.HeavyTraffic || rho > MaxLPSRho
		if i == 0 {
			out.Checks.BusiestRhoK, out.Checks.AdmissionWait = pK, wait
			out.Checks.Relaxation = es / ((1 - rho) * (1 - rho))
			if theta := hx.tailIndex(); theta > 0 && m.Slots > 0 {
				// The theorem needs theta > 1 (a finite mean service);
				// at or under 1 only theta is reported.
				out.Checks.ServiceTailIndex = theta
				if theta > 1 {
					out.Checks.TailIndex = min(theta, (theta-1)*(float64(m.Slots)-math.Floor(float64(m.Slots)*rho)))
				}
			}
		}
		st := newPSStage(lam, es, 1, cs2)
		pd, md := m.KV.at(rho)
		w := p / float64(per)
		for _, sj := range s {
			t := sj
			if st.rho > 0 {
				// The planner's two-moment rule: shared with probability
				// 1 - (1 - rho) exp(-lam S), then a gamma of the extra's
				// conditional mean and variance.
				u := sj / es
				meanE := st.g * sj
				varE := st.alpha*u - st.beta + st.beta*math.Exp(-st.decay*u)
				pSh := min(max(1-(1-rho)*math.Exp(-lam*sj), 1e-6), 1)
				mc := meanE / pSh
				vc := max((varE+meanE*meanE)/pSh-mc*mc, 1e-12)
				if rng.Float64() < pSh && mc > 0 {
					t += gammaDraw(rng, mc*mc/vc, vc/mc)
				}
			}
			if pK > 0 && rng.Float64() < pK {
				// Waits for one of the K places first come.
				t += rng.ExpFloat64() * wait
			}
			if m.KV.Known && rng.Float64() < pd {
				t += rng.ExpFloat64() * md
			}
			out.QP = append(out.QP, t)
			out.L = append(out.L, hl.ppf(rng.Float64()))
			out.W = append(out.W, w)
		}
	}
	out.Z = m.Parts.draws(out.L)
	if m.Parts != nil && m.Parts.Remainder.Server.restPastFit(rate) {
		out.Checks.RemainderRestPastFit = true
	}
	if m.KV.Known {
		mean := rhoSum / float64(len(shares))
		out.Checks.KVOutside = mean < m.KV.RhoLo || mean > m.KV.RhoHi
	}
	return out, nil
}

// LPSMean is M/G/1-LPS(k)'s mean sojourn at Poisson arrivals, Zhang and
// Zwart's two-moment approximation: es/(1 - rho) [(1 - rho^k) + rho^k (1 + cs2)/2],
// es the mean service time and cs2 its squared coefficient of variation.
func LPSMean(es, rho, cs2 float64, k int) float64 {
	rk := math.Pow(rho, float64(k))
	return es / (1 - rho) * ((1 - rk) + rk*(1+cs2)/2)
}

// LPSAdmissionWait is the mean wait for admission of a request that finds k
// prompts running, chosen so that the PS sojourn plus rho^k times it is
// LPSMean: es (cs2 - 1) / (2 (1 - rho)), held at 0.
func LPSAdmissionWait(es, rho, cs2 float64) float64 {
	return max(es*(cs2-1)/(2*(1-rho)), 0)
}

// tailIndex is theta, the tail index the histogram's top two finite edges
// read, ln(P(X > a)/P(X > b)) / ln(b/a); 0 where it reads none.
func (h cumHist) tailIndex() float64 {
	var edges, surv []float64
	for i := range h.hi {
		if s := 1 - h.cum[i]; s > 1e-12 && h.hi[i] > h.lo[i] {
			edges, surv = append(edges, h.hi[i]), append(surv, s)
		}
	}
	n := len(edges)
	if n < 2 || !(edges[n-1] > edges[n-2]) || !(surv[n-2] > surv[n-1]) {
		return 0
	}
	return math.Log(surv[n-2]/surv[n-1]) / math.Log(edges[n-1]/edges[n-2])
}

// gammaDraw is a gamma of the shape and scale (Marsaglia and Tsang; below
// shape 1 by the boost u^(1/shape)).
func gammaDraw(rng *rand.Rand, shape, scale float64) float64 {
	if !(shape > 0) || !(scale > 0) {
		return 0
	}
	boost := 1.0
	if shape < 1 {
		boost = math.Pow(rng.Float64(), 1/shape)
		shape++
	}
	d := shape - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := rng.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := rng.Float64()
		if u < 1-0.0331*x*x*x*x || math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v * scale * boost
		}
	}
}

// Stats is the draws' TTFT at Z = a0 + sb L (sb the spread times beta): the
// mean, and each quantile of qs.
func (d RankMixtureDraws) Stats(a0, sb float64, qs ...float64) (mean float64, quantiles []float64) {
	type pt struct{ y, w float64 }
	ps := make([]pt, len(d.QP))
	var tw float64
	for i := range d.QP {
		y := a0 + d.QP[i] + sb*d.L[i]
		if d.Z != nil {
			y = d.QP[i] + d.Z[i]
		}
		ps[i] = pt{y, d.W[i]}
		mean += y * d.W[i]
		tw += d.W[i]
	}
	if !(tw > 0) {
		return math.NaN(), nil
	}
	mean /= tw
	if len(qs) == 0 {
		return mean, nil
	}
	slices.SortFunc(ps, func(a, b pt) int { return cmp.Compare(a.y, b.y) })
	quantiles = make([]float64, len(qs))
	for k, q := range qs {
		target, acc := q*tw, 0.0
		quantiles[k] = ps[len(ps)-1].y
		for _, p := range ps {
			acc += p.w
			if acc >= target {
				quantiles[k] = p.y
				break
			}
		}
	}
	return mean, quantiles
}

// Price is the model's TTFT at rate: the mean (q 0) or the q quantile, and
// the checks. It refuses as Draws does.
func (m RankMixture) Price(rate float64, computed, prompt map[float64]float64, q float64) (float64, RankMixtureChecks, *Unanswerable) {
	d, u := m.Draws(rate, computed, prompt, DefaultMixtureDraws)
	if u != nil {
		return 0, RankMixtureChecks{}, u
	}
	sb := m.Spread * m.Beta
	if q == 0 {
		mean, _ := d.Stats(m.A0, sb)
		return mean, d.Checks, nil
	}
	_, qv := d.Stats(m.A0, sb, q)
	return qv[0], d.Checks, nil
}

// Assumed is the model's parameters in words, for an answer's assumed line.
func (m RankMixture) Assumed() string {
	if m.Parts != nil {
		return m.assumedNL()
	}
	busiest := 0.0
	if len(m.Shares) > 0 {
		busiest = m.Shares[0] * float64(len(m.Shares))
	}
	kv := "no KV-deferral wait (not fitted)"
	if m.KV.Known {
		kv = fmt.Sprintf("KV-deferral wait p_d = %.3g + %.3g rho, m_d = %.3g + %.3g rho s (fitted over rho %.3g-%.3g)",
			m.KV.P0, m.KV.P1, m.KV.M0, m.KV.M1, m.KV.RhoLo, m.KV.RhoHi)
	}
	queue := fmt.Sprintf("%s (K = %d)", QueueLimitedProcessorSharing, m.Slots)
	if m.Slots <= 0 {
		queue = string(QueueProcessorSharing) + " (max-num-seqs not read)"
	}
	return fmt.Sprintf("experimental TTFT model %s (OUTSIDE VALIDATED REGION): %s per rank on %d ranks "+
		"(busiest share %.3g of the mean), S(x) = %.3g s + x / %.4g tok/s; %s; Z = %.3g s + %.3g x %.3g us x L "+
		"(beta: overhead %.3g, decode %.3g, remainder %.3g us per prompt token)%s",
		QueueRankMixture, queue, len(m.Shares), busiest, m.S0, m.V, kv, m.A0, m.Spread, m.Beta*1e6,
		m.BetaOverhead*1e6, m.BetaDecode*1e6, m.BetaRemainder*1e6, m.fitStepsAssumed())
}
