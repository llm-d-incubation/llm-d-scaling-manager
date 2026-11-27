package whatif

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// This file is the experimental per-request TPOT distribution
// (TPOTRenewalLognormal), priced only where a backtest asks for it. It models
// the whole distribution of one request's TPOT, not a ratio over the mean:
//
//   - The server is a decode group: the data-parallel ranks of one replica
//     step in lockstep (expert parallelism synchronises them), and a step
//     advances every running sequence by one token plus the draft tokens it
//     accepts. Far under max-num-seqs no sequence waits for a slot (Erlang C,
//     SlotWait), so the step time, not a queue, sets a token's time: an
//     infinite-server (M/G/inf) occupancy with a mildly load-dependent step.
//   - A request's TPOT is its decode time over its tokens after the first.
//     With speculative decoding a step emits 1 + A tokens, A the accepted
//     drafts; the steps a request of L tokens needs is a first passage of
//     that renewal process, so its TPOT is the step time times S/(L-1). By
//     the renewal central limit theorem (Cox, Renewal Theory, 1962, ch. 3;
//     Feller vol. II, XI.5) S/(L-1) has squared CV of about Var(1+A)/
//     ((1+E A)(L-1)): a long request is concentrated near the mean, a short
//     one spread. Over the output-length mix, the squared CV is C times
//     E[1/(L-1)], C fitted with RenewalCoefficient as its floor.
//   - The population mixes the groups (GroupCV2, measured) and the requests
//     (the renewal term). Per request the TPOT is lognormal around the
//     planner's mean with log-variance sigma^2 = GroupCV2 + C E[1/(L-1)]: a
//     two-moment fit to the mixture, as QNA fits each flow (Whitt, The
//     Queueing Network Analyzer, 1983).
//
// C is the one free parameter, fitted by interval-censored maximum
// likelihood over every bucket of the router's TPOT histogram (FitTPOTDist).

// TPOTRenewalLognormal names the experimental TPOT distribution.
const TPOTRenewalLognormal = "renewal-lognormal"

// TPOTPerLengthMixture names the experimental TPOT distribution as a
// mixture over the output lengths (TPOTDist.PerLength): one lognormal
// around the mean per output-length group, in place of one lognormal at the
// lengths' mean E[1/(L-1)]. C is fitted the same way. It is off unless asked
// for by name.
const TPOTPerLengthMixture = "renewal-lognormal-per-length"

// IsTPOTDistModel says m names one of the experimental TPOT distributions.
func IsTPOTDistModel(m string) bool { return m == TPOTRenewalLognormal || m == TPOTPerLengthMixture }

const (
	// TPOTDistribution is on every TPOT percentile priced by the
	// experimental per-request TPOT distribution (TPOTRenewalLognormal).
	TPOTDistribution Flag = "TPOTDistribution"
	// TPOTDistributionShortFit is on every TPOT percentile priced by the
	// distribution fitted on fewer steps than the default minimum because
	// the caller lowered it: its renewal coefficient and its self-check
	// folds read fewer steps than the default asks.
	TPOTDistributionShortFit Flag = "TPOTDistributionShortFit"
	// TPOTDistributionAbstained is a TPOT percentile the planner priced
	// itself where the experimental distribution was asked for and did not
	// price it.
	TPOTDistributionAbstained Flag = "TPOTDistributionAbstained"
)

// MaxRenewalC is the upper end of the renewal coefficient's search: a C
// there has not been fitted, it is the bound.
const MaxRenewalC = 50.0

// MaxSlotWait is the most of a decode rank's arrivals that may wait for a
// sequence slot (SlotWait) for the distribution to hold: past it a queue for
// slots, which the distribution leaves out, adds to the TPOT.
const MaxSlotWait = 0.01

// DecodeGroup is one decode replica as one cycle read it: its request rate,
// its step time over the tokens a step emits, and the accepted drafts per
// step, where read.
type DecodeGroup struct {
	Rate, TokenTime float64
	Accepted        float64
	AcceptedKnown   bool
}

// DecodeCycle is one cycle of the decode pods as the TPOT distribution reads
// it: the pooled output-length histogram (cumulative request rate by upper
// edge), each replica's reading, and the mean and largest running sequences
// of a data-parallel rank. EngineTPOT is the engines' per-request TPOT
// histogram, each bound's [1m] rate summed over the decode and unsplit pods
// (sorted by bound, nil with none): the step's joint bracket reads it
// beside the router's (WithEngineTPOT, JointBracket).
type DecodeCycle struct {
	At            time.Time
	Output        map[float64]float64
	Groups        []DecodeGroup
	MeanRunning   float64
	MostRunning   float64
	RanksReported int
	EngineTPOT    []domain.LatencyBucket
}

// InverseOutputLength is E[1/(L-1)] over the requests of the cumulative
// output-length histogram out with two tokens or more (a one-token request
// has no TPOT), each bucket's lengths spread evenly in log (a mass 1/L at
// each integer L of the bucket). The mass past the last finite edge counts
// at 1/(L-1) = 0. ok is false with no request of two tokens or more.
func InverseOutputLength(out map[float64]float64) (float64, bool) {
	edges := make([]float64, 0, len(out))
	for e := range out {
		if !math.IsNaN(e) {
			edges = append(edges, e)
		}
	}
	slices.Sort(edges)
	var num, den, prevEdge, prevCum float64
	for _, e := range edges {
		cum := math.Max(prevCum, out[e])
		m := cum - prevCum
		lo := prevEdge
		prevEdge, prevCum = e, cum
		if !(m > 0) || !Finite(m) {
			continue
		}
		if math.IsInf(e, 1) {
			den += m
			continue
		}
		a, b := math.Max(2, math.Floor(lo)+1), math.Floor(e)
		if b < a {
			continue
		}
		// sum_{L=a}^{b} (1/L) (1/(L-1)) telescopes to 1/(a-1) - 1/b.
		num += m * (1/(a-1) - 1/b) / harmonicSpan(a, b)
		den += m
	}
	if !(den > 0) {
		return 0, false
	}
	return num / den, true
}

// harmonicSpan is sum_{L=a}^{b} 1/L for integers 1 <= a <= b.
func harmonicSpan(a, b float64) float64 {
	if b-a < 64 {
		s := 0.0
		for l := a; l <= b; l++ {
			s += 1 / l
		}
		return s
	}
	return harmonic(b) - harmonic(a-1)
}

// harmonic is H(n) = sum_{L=1}^{n} 1/L, exact under 64 and the
// Euler-Maclaurin expansion above.
func harmonic(n float64) float64 {
	if n < 64 {
		s := 0.0
		for l := 1.0; l <= n; l++ {
			s += 1 / l
		}
		return s
	}
	const eulerGamma = 0.5772156649015329
	return math.Log(n) + eulerGamma + 1/(2*n) - 1/(12*n*n)
}

// RenewalCoefficient is Var(1+A)/(1+E A), the squared CV of S/(L-1) times
// L-1 by the renewal central limit theorem, for k draft tokens a step
// accepted as a chain (P(A >= j) = p^j for j <= k) with mean accepted.
// Without speculation (k 0) a step emits one token and it is 0.
func RenewalCoefficient(k int, accepted float64) float64 {
	if k <= 0 || !(accepted > 0) || !Finite(accepted) {
		return 0
	}
	p := chainAcceptance(k, accepted)
	var ea, ea2 float64
	for j := 1; j <= k; j++ {
		t := math.Pow(p, float64(j))
		ea += t
		ea2 += float64(2*j-1) * t
	}
	return math.Max(0, ea2-ea*ea) / (1 + ea)
}

// chainAcceptance is p with p + p^2 + ... + p^k = accepted, by bisection.
func chainAcceptance(k int, accepted float64) float64 {
	if accepted >= float64(k) {
		return 1
	}
	lo, hi := 0.0, 1.0
	for range 60 {
		m := (lo + hi) / 2
		s := 0.0
		for j := 1; j <= k; j++ {
			s += math.Pow(m, float64(j))
		}
		if s < accepted {
			lo = m
		} else {
			hi = m
		}
	}
	return (lo + hi) / 2
}

// GroupCV2 is the squared coefficient of variation of the groups' token
// time, weighted by their request rates; ok is false with fewer than two
// groups read.
func GroupCV2(groups []DecodeGroup) (float64, bool) {
	var w, s, s2 float64
	n := 0
	for _, g := range groups {
		if !(g.Rate > 0) || !(g.TokenTime > 0) || !Finite(g.Rate) || !Finite(g.TokenTime) {
			continue
		}
		w += g.Rate
		s += g.Rate * g.TokenTime
		s2 += g.Rate * g.TokenTime * g.TokenTime
		n++
	}
	if n < 2 {
		return 0, false
	}
	m := s / w
	return math.Max(0, s2/w-m*m) / (m * m), true
}

// SlotWait is the Erlang C probability that an arrival waits for one of n
// slots at offered load a (Erlang, 1917; Halfin and Whitt, 1981, for its
// many-server limit): the share of a rank's sequences that queue for
// max-num-seqs. It is 1 at a >= n.
func SlotWait(n int, a float64) float64 {
	if n <= 0 || !(a > 0) {
		return 0
	}
	if a >= float64(n) {
		return 1
	}
	// Erlang B by its recursion, then C from B.
	b := 1.0
	for k := 1; k <= n; k++ {
		b = a * b / (float64(k) + a*b)
	}
	return b / (1 - (a/float64(n))*(1-b))
}

// TPOTDist is the fitted distribution: log-variance GroupCV2 + C
// E[1/(L-1)] around the mean, with Renewal the floor C was fitted above.
// With PerLength set it is the per-length mixture (TPOTPerLengthMixture):
// one lognormal around the mean per output-length group, of log-variance
// GroupCV2 + C times the group's own 1/(L-1), weighted by the group's share
// of the requests; Quantile and CDF then do not read inv.
type TPOTDist struct {
	GroupCV2, C, Renewal float64
	PerLength            []LengthGroup
}

// Sigma2 is the log-variance at E[1/(L-1)] inv: the one lognormal's, and
// the mixture's two-moment summary.
func (d TPOTDist) Sigma2(inv float64) float64 { return d.GroupCV2 + d.C*inv }

// Quantile is the q quantile of a request's TPOT at mean and inv.
func (d TPOTDist) Quantile(mean, inv, q float64) float64 {
	if len(d.PerLength) > 0 {
		return d.mixtureQuantile(mean, q)
	}
	s2 := d.Sigma2(inv)
	if !(s2 > 0) {
		return mean
	}
	return mean * math.Exp(math.Sqrt(s2)*math.Sqrt2*math.Erfinv(2*q-1)-s2/2)
}

// CDF is the share of requests with a TPOT at or under x at mean and inv.
func (d TPOTDist) CDF(mean, inv, x float64) float64 {
	if len(d.PerLength) > 0 {
		f := 0.0
		for _, g := range d.PerLength {
			f += g.Weight * lognormalCDF(mean, d.GroupCV2+d.C*g.Inv, x)
		}
		return f
	}
	return lognormalCDF(mean, d.Sigma2(inv), x)
}

// mixtureQuantile is the per-length mixture's q quantile at mean, by
// bisection in log between a thousandth and fifty times the mean (the top
// where the mixture holds less than q under it).
func (d TPOTDist) mixtureQuantile(mean, q float64) float64 {
	if !(mean > 0) || !(q > 0) || !(q < 1) {
		return mean
	}
	lo, hi := math.Log(mean*1e-3), math.Log(mean*50)
	if d.CDF(mean, 0, math.Exp(hi)) < q {
		return math.Exp(hi)
	}
	for range 100 {
		m := (lo + hi) / 2
		if d.CDF(mean, 0, math.Exp(m)) < q {
			lo = m
		} else {
			hi = m
		}
	}
	return math.Exp((lo + hi) / 2)
}

// LengthGroup is one group of output lengths in the per-length mixture:
// its share of the requests of two tokens or more (Weight) and its mean
// 1/(L-1) (Inv).
type LengthGroup struct {
	Weight, Inv float64
}

// lengthSubRanges is the most log-even sub-ranges one output-length bucket
// is cut into for the per-length mixture.
const lengthSubRanges = 24

// OutputLengthLaw is the cumulative output-length histogram out as groups
// of lengths for the per-length mixture: each bucket's lengths spread
// evenly in log (a mass 1/L at each integer L, as InverseOutputLength
// spreads them) and cut into up to lengthSubRanges log-even sub-ranges,
// each a group of the bucket's mass in it and its mean 1/(L-1); the mass
// past the last finite edge is one group at 1/(L-1) = 0. The weights sum to
// 1, and the weighted mean of Inv is InverseOutputLength's. ok is false
// with no request of two tokens or more.
func OutputLengthLaw(out map[float64]float64) ([]LengthGroup, bool) {
	edges := make([]float64, 0, len(out))
	for e := range out {
		if !math.IsNaN(e) {
			edges = append(edges, e)
		}
	}
	slices.Sort(edges)
	var groups []LengthGroup
	var prevEdge, prevCum, total float64
	for _, e := range edges {
		cum := math.Max(prevCum, out[e])
		m := cum - prevCum
		lo := prevEdge
		prevEdge, prevCum = e, cum
		if !(m > 0) || !Finite(m) {
			continue
		}
		if math.IsInf(e, 1) {
			groups = append(groups, LengthGroup{Weight: m})
			total += m
			continue
		}
		a, b := math.Max(2, math.Floor(lo)+1), math.Floor(e)
		if b < a {
			continue
		}
		cuts := lengthCuts(a, b)
		span := harmonicSpan(a, b)
		for i := 0; i+1 < len(cuts); i++ {
			sa, sb := cuts[i], cuts[i+1]-1
			if sb < sa {
				continue
			}
			h := harmonicSpan(sa, sb)
			groups = append(groups, LengthGroup{Weight: m * h / span, Inv: (1/(sa-1) - 1/sb) / h})
			total += m * h / span
		}
	}
	if !(total > 0) {
		return nil, false
	}
	for i := range groups {
		groups[i].Weight /= total
	}
	return groups, true
}

// lengthCuts is the integer cuts of the lengths [a, b] into up to
// lengthSubRanges log-even sub-ranges: a, b+1 and the rounded points
// between, each sub-range [cut, next cut - 1].
func lengthCuts(a, b float64) []float64 {
	cuts := []float64{a, b + 1}
	r := (b + 1) / a
	for k := 1; k < lengthSubRanges; k++ {
		cuts = append(cuts, math.RoundToEven(a*math.Pow(r, float64(k)/lengthSubRanges)))
	}
	slices.Sort(cuts)
	return slices.Compact(cuts)
}

// lognormalCDF is P(X <= x) for a lognormal of mean and log-variance s2.
func lognormalCDF(mean, s2, x float64) float64 {
	switch {
	case math.IsInf(x, 1):
		return 1
	case !(x > 0):
		return 0
	case !(s2 > 0):
		if x >= mean {
			return 1
		}
		return 0
	}
	s := math.Sqrt(s2)
	z := (math.Log(x/mean) + s2/2) / s
	return 0.5 * math.Erfc(-z/math.Sqrt2)
}

// TPOTDistStep is one step the distribution is fitted on or scored at: the
// router's measured mean, the step's E[1/(L-1)] and group CV^2, and its
// histogram as upper edges (the last may be +Inf) and the requests in each
// bucket. Law is the step's output lengths as the per-length mixture reads
// them (OutputLengthLaw). EngineEdges and EngineCum are the decode engines'
// own per-request TPOT histogram (vLLM request_time_per_output_token_seconds,
// the decode pods summed) as upper edges and cumulative requests, nil where
// not read: JointBracket joins it to the router's above its first edge.
type TPOTDistStep struct {
	Mean, Inv, GroupCV2    float64
	Edges, Counts          []float64
	Law                    []LengthGroup
	EngineEdges, EngineCum []float64
}

// WithEngine is st with the engines' TPOT histogram: upper edges and
// cumulative request rates over windowSeconds, a dip repaired. st is
// unchanged where they are not read.
func (st TPOTDistStep) WithEngine(edges, cum []float64, windowSeconds float64) TPOTDistStep {
	if len(edges) == 0 || len(edges) != len(cum) || !(windowSeconds > 0) {
		return st
	}
	st.EngineEdges, st.EngineCum = slices.Clone(edges), make([]float64, len(cum))
	prev := 0.0
	for i, c := range cum {
		prev = math.Max(prev, c)
		st.EngineCum[i] = prev * windowSeconds
	}
	return st
}

// TPOTDistStepOf is a step from a cumulative histogram of request rates
// (buckets, with their upper edges) over windowSeconds, its mean, and the
// decode cycles of its window. ok is false where the histogram, the mean or
// the output lengths are not read.
func TPOTDistStepOf(edges, cum []float64, windowSeconds, mean float64, cycles []DecodeCycle) (TPOTDistStep, bool) {
	if len(edges) == 0 || len(edges) != len(cum) || !(mean > 0) || !(windowSeconds > 0) {
		return TPOTDistStep{}, false
	}
	inv, g2, ok := DecodeWindow(cycles)
	if !ok {
		return TPOTDistStep{}, false
	}
	law, _ := OutputLengthLaw(pooledOutput(cycles))
	st := TPOTDistStep{Mean: mean, Inv: inv, GroupCV2: g2, Edges: slices.Clone(edges), Counts: make([]float64, len(edges)), Law: law}
	prev := 0.0
	for i, c := range cum {
		c = math.Max(prev, c)
		st.Counts[i] = (c - prev) * windowSeconds
		prev = c
	}
	if !(prev > 0) {
		return TPOTDistStep{}, false
	}
	return st, true
}

// DecodeWindow is E[1/(L-1)] over the output lengths pooled over cycles,
// and the mean of their groups' CV^2 (0 where no cycle reads two groups).
func DecodeWindow(cycles []DecodeCycle) (inv, groupCV2 float64, ok bool) {
	n := 0
	for _, c := range cycles {
		if v, gok := GroupCV2(c.Groups); gok {
			groupCV2 += v
			n++
		}
	}
	if n > 0 {
		groupCV2 /= float64(n)
	}
	inv, ok = InverseOutputLength(pooledOutput(cycles))
	return inv, groupCV2, ok
}

// DecodeWindowLaw is the output lengths pooled over cycles as the
// per-length mixture reads them (OutputLengthLaw).
func DecodeWindowLaw(cycles []DecodeCycle) ([]LengthGroup, bool) {
	return OutputLengthLaw(pooledOutput(cycles))
}

// pooledOutput is the cycles' output-length histograms summed by edge.
func pooledOutput(cycles []DecodeCycle) map[float64]float64 {
	pooled := map[float64]float64{}
	for _, c := range cycles {
		for e, r := range c.Output {
			pooled[e] += r
		}
	}
	return pooled
}

// logLik is the steps' interval-censored log-likelihood at C, each step's
// lognormal at its own mean, or with perLength the per-length mixture at
// the step's own lengths (TPOTDistStep.Law): sum over buckets of the
// requests in a bucket times the log of the mass the distribution puts
// there.
func logLik(steps []TPOTDistStep, c float64, perLength bool) float64 {
	const floor = 1e-9
	ll := 0.0
	for _, st := range steps {
		d := TPOTDist{GroupCV2: st.GroupCV2, C: c}
		if perLength {
			d.PerLength = st.Law
		}
		prev := 0.0
		for i, e := range st.Edges {
			f := d.CDF(st.Mean, st.Inv, e)
			if st.Counts[i] > 0 {
				ll += st.Counts[i] * math.Log(math.Max(f-prev, floor))
			}
			prev = f
		}
	}
	return ll
}

// ErrNoTPOTSteps is a fit given no step to fit on.
var ErrNoTPOTSteps = errors.New("no step reads the router's TPOT histogram, its mean and the decode pods' output lengths")

// FitTPOTDist fits C in [floor, MaxRenewalC] by interval-censored maximum
// likelihood over every bucket of every step (golden-section search on the
// log-likelihood, concave in C here as it is in a lognormal's variance).
// atBound says C ended at MaxRenewalC: the bound, not an estimate.
func FitTPOTDist(steps []TPOTDistStep, floor float64) (c float64, atBound bool, err error) {
	return fitTPOTDist(steps, floor, false)
}

// FitTPOTDistPerLength is FitTPOTDist for the per-length mixture
// (TPOTPerLengthMixture), each step at its own lengths (TPOTDistStep.Law);
// a step whose lengths are not read is the one lognormal at its inv.
func FitTPOTDistPerLength(steps []TPOTDistStep, floor float64) (c float64, atBound bool, err error) {
	return fitTPOTDist(steps, floor, true)
}

// fitTPOTDist is FitTPOTDist, with perLength the per-length mixture's.
func fitTPOTDist(steps []TPOTDistStep, floor float64, perLength bool) (c float64, atBound bool, err error) {
	if len(steps) == 0 {
		return 0, false, ErrNoTPOTSteps
	}
	if !(floor >= 0) || floor >= MaxRenewalC {
		return 0, false, fmt.Errorf("renewal floor %g, want 0 <= floor < %g", floor, MaxRenewalC)
	}
	lo, hi := floor, MaxRenewalC
	g := (math.Sqrt(5) - 1) / 2
	a, b := hi-g*(hi-lo), lo+g*(hi-lo)
	fa, fb := logLik(steps, a, perLength), logLik(steps, b, perLength)
	for hi-lo > 1e-4 {
		if fa >= fb {
			hi, b, fb = b, a, fa
			a = hi - g*(hi-lo)
			fa = logLik(steps, a, perLength)
		} else {
			lo, a, fa = a, b, fb
			b = lo + g*(hi-lo)
			fb = logLik(steps, b, perLength)
		}
	}
	c = (lo + hi) / 2
	return c, MaxRenewalC-c < 1e-2, nil
}

// BucketScore compares the distribution with one step's histograms at their
// own resolution: KS is the largest gap between the predicted share at or
// under an edge and the measured share, over the router histogram's finite
// edges, at the predicted mean; side[i] is where the predicted q-th
// quantile (qs[i]) lies against the measured one's JointBracket.
func (d TPOTDist) BucketScore(st TPOTDistStep, predictedMean float64, qs ...float64) (ks float64, side []BracketSide) {
	total := 0.0
	for _, c := range st.Counts {
		total += c
	}
	side = make([]BracketSide, len(qs))
	if !(total > 0) {
		return 0, side
	}
	cum := 0.0
	for i, e := range st.Edges {
		cum += st.Counts[i]
		if !math.IsInf(e, 1) {
			ks = math.Max(ks, math.Abs(d.CDF(predictedMean, st.Inv, e)-cum/total))
		}
	}
	for j, q := range qs {
		if b, ok := JointBracket(st, q); ok {
			side[j] = b.Side(d.Quantile(predictedMean, st.Inv, q))
		}
	}
	return ks, side
}

// The joint bracket reads a step's TPOT quantile from every per-request TPOT
// histogram it holds at once: the router's (edges 5, 10, 20, 40, 60, 80 ms
// on llm-d) and the decode engines' (vLLM's 10, 25, 50, 75, 100 ms ...).
// Each histogram pins the CDF F at its own edges, to within its sampling
// noise; between its edges only monotonicity binds. The engines' first
// bucket is not the router's population (it holds requests the router never
// times, and on a P/D fleet an extra observation near 0 for half the
// requests), so the engines' histogram joins only above its first edge, its
// law there anchored at the router's F at that edge, and only where at
// least MinEngineAboveFirstEdge requests lie above it. The bracket is the
// tightest interval the quantile must lie in under every histogram, its
// point a Pareto reading (log S linear in log x) inside the merged
// sub-bucket where the pooled CDF crosses q: held out at the engines' 25 ms
// edge, the Pareto reading's tail ratio was 1.16 at the median where the
// linear reading's was 1.74.

const (
	// JointBracketSE is the binomial standard errors each histogram's
	// share under an edge is allowed either way.
	JointBracketSE = 2.0
	// JointBracketAllowance is the share under an edge allowed either way
	// on top of JointBracketSE, for the router and the engines counting
	// slightly different populations. Fixed before any step was scored.
	JointBracketAllowance = 0.005
	// MinEngineAboveFirstEdge is the fewest requests above the engines'
	// first TPOT edge for their histogram to join the router's.
	MinEngineAboveFirstEdge = 50.0
)

// TPOTBracket is a step's TPOT quantile as JointBracket reads it: it lies in
// [Lo, Hi] (Hi +Inf where no histogram bounds it above), and Point is the
// reading inside (+Inf where the pooled CDF crosses the quantile above the
// top edge). Engine says the engines' histogram joined the router's.
// Conflict says the histograms cannot both hold at the quantile at their
// noise: the bracket is then the router's own bucket.
type TPOTBracket struct {
	Lo, Hi, Point    float64
	Engine, Conflict bool
}

// BracketSide is where a figure lies against a TPOTBracket.
type BracketSide int

const (
	// BracketUnplaced is a figure not compared: no bracket, or no figure.
	BracketUnplaced BracketSide = iota
	// BracketBelow is a figure under the bracket's Lo.
	BracketBelow
	// BracketInside is a figure in [Lo, Hi].
	BracketInside
	// BracketAbove is a figure over the bracket's Hi.
	BracketAbove
)

// Side is where v lies against b; the edges carry a relative 1e-9, as the
// bounds are written in decimal.
func (b TPOTBracket) Side(v float64) BracketSide {
	switch {
	case math.IsNaN(v):
		return BracketUnplaced
	case v < b.Lo*(1-1e-9):
		return BracketBelow
	case v > b.Hi*(1+1e-9):
		return BracketAbove
	}
	return BracketInside
}

// cdfHist is one per-request histogram as the joint bracket reads it: its
// finite edges, the share at or under each, and its requests.
type cdfHist struct {
	edges, f []float64
	n        float64
}

// se is the binomial standard error of f at edge j, floored at 1/n.
func (h cdfHist) se(j int) float64 {
	p := h.f[j]
	return math.Sqrt(math.Max(p*(1-p), 1/h.n) / h.n)
}

// cdfHistOf is the histogram of upper edges and cumulative requests, sorted
// and repaired to be monotone; ok false with no request or no finite edge.
func cdfHistOf(edges, cum []float64) (cdfHist, bool) {
	type bucket struct{ e, c float64 }
	bs := make([]bucket, 0, len(edges))
	for i, e := range edges {
		if i < len(cum) && !math.IsNaN(e) && e > 0 && Finite(cum[i]) {
			bs = append(bs, bucket{e, cum[i]})
		}
	}
	slices.SortFunc(bs, func(a, b bucket) int { return cmp.Compare(a.e, b.e) })
	var h cdfHist
	prev := 0.0
	for _, b := range bs {
		prev = math.Max(prev, b.c)
		if !math.IsInf(b.e, 1) {
			h.edges = append(h.edges, b.e)
			h.f = append(h.f, prev)
		}
	}
	h.n = prev
	if !(h.n > 0) || len(h.edges) == 0 {
		return cdfHist{}, false
	}
	for i := range h.f {
		h.f[i] /= h.n
	}
	return h, true
}

// routerHist is st's router histogram, from its per-bucket requests.
func (st TPOTDistStep) routerHist() (cdfHist, bool) {
	if len(st.Edges) != len(st.Counts) {
		return cdfHist{}, false
	}
	cum := make([]float64, len(st.Counts))
	s := 0.0
	for i, c := range st.Counts {
		s += math.Max(c, 0)
		cum[i] = s
	}
	return cdfHistOf(st.Edges, cum)
}

// engineAbove is st's engine histogram above its first edge e1, anchored at
// the router's F(e1): F(e) = F_R(e1) + (1 - F_R(e1)) (F_E(e) - F_E(e1)) /
// (1 - F_E(e1)), over the requests above e1. ok is false where the router
// has no edge at e1, or fewer than MinEngineAboveFirstEdge requests lie
// above it.
func (st TPOTDistStep) engineAbove(r cdfHist) (cdfHist, bool) {
	e, ok := cdfHistOf(st.EngineEdges, st.EngineCum)
	if !ok || len(e.edges) < 2 {
		return cdfHist{}, false
	}
	e1, f1 := e.edges[0], e.f[0]
	j, found := slices.BinarySearch(r.edges, e1)
	if !found || !(f1 < 1) {
		return cdfHist{}, false
	}
	above := e.n * (1 - f1)
	if above < MinEngineAboveFirstEdge {
		return cdfHist{}, false
	}
	fr := r.f[j]
	out := cdfHist{edges: slices.Clone(e.edges[1:]), f: make([]float64, len(e.edges)-1), n: above}
	for k, fe := range e.f[1:] {
		out.f[k] = fr + (1-fr)*(fe-f1)/(1-f1)
	}
	return out, true
}

// JointBracket is st's TPOT q-quantile read from the router's histogram and,
// where they join (above their first edge), the decode engines': at every
// edge of either, each histogram bounds F to its share there,
// JointBracketSE binomial errors and JointBracketAllowance either way (at a
// point between its edges, by its neighbouring edges), the bounds closed
// under monotonicity; the bracket is from the largest edge F is surely under
// q at to the smallest it is surely at or over q at. Point is the Pareto
// reading inside the sub-bucket where the histograms' pooled CDF (their
// shares weighted by their requests, made monotone) crosses q, clipped to
// the bracket. Where the bounds cross at q (Conflict) the bracket is the
// router's own bucket. ok is false where the router's histogram is not read.
func JointBracket(st TPOTDistStep, q float64) (TPOTBracket, bool) {
	r, ok := st.routerHist()
	if !ok || !(q > 0) || !(q < 1) {
		return TPOTBracket{}, false
	}
	hs := []cdfHist{r}
	var b TPOTBracket
	if e, eok := st.engineAbove(r); eok {
		hs = append(hs, e)
		b.Engine = true
	}
	var u []float64
	for _, h := range hs {
		u = append(u, h.edges...)
	}
	slices.Sort(u)
	u = slices.Compact(u)
	lower, upper := make([]float64, len(u)), make([]float64, len(u))
	obs, w := make([]float64, len(u)), make([]float64, len(u))
	for i := range upper {
		upper[i] = 1
	}
	for _, h := range hs {
		for i, x := range u {
			j, at := slices.BinarySearch(h.edges, x)
			if at {
				s := JointBracketSE*h.se(j) + JointBracketAllowance
				lower[i] = math.Max(lower[i], h.f[j]-s)
				upper[i] = math.Min(upper[i], h.f[j]+s)
				obs[i] += h.f[j] * h.n
				w[i] += h.n
				continue
			}
			if j > 0 {
				lower[i] = math.Max(lower[i], h.f[j-1]-JointBracketSE*h.se(j-1)-JointBracketAllowance)
			}
			if j < len(h.edges) {
				upper[i] = math.Min(upper[i], h.f[j]+JointBracketSE*h.se(j)+JointBracketAllowance)
			}
		}
	}
	for i := range u {
		lower[i] = math.Min(math.Max(lower[i], 0), 1)
		upper[i] = math.Min(math.Max(upper[i], 0), 1)
		if i > 0 {
			lower[i] = math.Max(lower[i], lower[i-1])
		}
	}
	for i := len(u) - 2; i >= 0; i-- {
		upper[i] = math.Min(upper[i], upper[i+1])
	}
	b.Hi = math.Inf(1)
	for i, x := range u {
		if upper[i] < q {
			b.Lo = x
		}
		if lower[i] >= q && math.IsInf(b.Hi, 1) {
			b.Hi = x
		}
	}
	if b.Hi < b.Lo {
		b.Conflict = true
		b.Lo, b.Hi = r.bucket(q)
	}
	for i := range obs {
		obs[i] /= w[i]
	}
	b.Point = pooledPoint(u, isotonic(obs, w), q, b.Lo, b.Hi)
	return b, true
}

// bucket is the router bucket holding the q quantile, (lo, hi], hi +Inf
// above the top edge.
func (h cdfHist) bucket(q float64) (lo, hi float64) {
	for j, e := range h.edges {
		if h.f[j] >= q {
			return lo, e
		}
		lo = e
	}
	return lo, math.Inf(1)
}

// pooledPoint is the q quantile of the pooled CDF fp at the edges u, read
// inside the sub-bucket it crosses q in with log S linear in log x (the
// first sub-bucket, and one closing at F = 1, linear), clipped to [lo, hi];
// +Inf where fp stays under q.
func pooledPoint(u, fp []float64, q, lo, hi float64) float64 {
	slo, flo := 0.0, 0.0
	for i, shi := range u {
		fhi := fp[i]
		if fhi < q {
			slo, flo = shi, fhi
			continue
		}
		var v float64
		switch {
		case fhi <= flo:
			v = shi
		case slo > 0 && fhi < 1:
			sLo, sHi := 1-flo, 1-fhi
			v = slo * math.Pow(shi/slo, math.Log(sLo/(1-q))/math.Log(sLo/sHi))
		default:
			v = slo + (shi-slo)*(q-flo)/(fhi-flo)
		}
		return math.Min(math.Max(v, lo), hi)
	}
	return math.Inf(1)
}

// isotonic is the weighted least-squares non-decreasing fit to x (pool
// adjacent violators).
func isotonic(x, w []float64) []float64 {
	type block struct{ v, w float64 }
	var bs []block
	var n []int
	for i := range x {
		bs = append(bs, block{x[i], w[i]})
		n = append(n, 1)
		for len(bs) > 1 && bs[len(bs)-2].v > bs[len(bs)-1].v {
			a, c := bs[len(bs)-2], bs[len(bs)-1]
			bs = bs[:len(bs)-2]
			bs = append(bs, block{(a.v*a.w + c.v*c.w) / (a.w + c.w), a.w + c.w})
			m := n[len(n)-2] + n[len(n)-1]
			n = append(n[:len(n)-2], m)
		}
	}
	out := make([]float64, 0, len(x))
	for i, b := range bs {
		for range n[i] {
			out = append(out, b.v)
		}
	}
	return out
}
