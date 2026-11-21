package whatif

import (
	"cmp"
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The ITL batch term past the roofline (issue
// llm-d-incubation/llm-d-scaling-manager#309, saturation.SwitchITLBatchTerm).
//
// A decode step reads the weights (a constant), every resident KV token
// (linear in the fill) and, once the batch's tokens per engine rank pass the
// roofline, spends GEMM time that grows with them. The line, ITL = A*k + B,
// prices the first two. At one KV per request the batch moves with the fill
// and the line's slope absorbs the third, but a shape with less KV per
// request runs more sequences at the same fill, and the line reads it low.
//
// The term adds a*max(0, n*(1+k_spec) - n_roof) to the line, n the running
// sequences per engine rank and k_spec the speculative tokens per step. It is
// fitted on the residuals of the variant's own line (FitBatchTerm), so the
// line is the one the controller prices and the term only adds to it. The
// line was fitted on much the same rows, so its slope has already taken the
// part of the batch that moves with the fill: the term's slope is read on the
// part that does not (the batch with its fill-linear part removed), else it
// reads low at a shape with less KV per request, the case it is for. A fit
// recording whose requests all hold about the same KV cannot tell the term
// from the line's slope; the gates then refuse it, and the line alone is
// priced.

// BatchTerm is the ITL a decode step adds once a rank's batch passes the
// roofline. The zero value adds nothing.
type BatchTerm struct {
	// Slope is the seconds a step adds per batch token per rank past Roof.
	Slope float64
	// Roof is n_roof, the batch tokens per engine rank where the term
	// starts.
	Roof float64
	// Ranks is the engine ranks a replica's sequences split over (data
	// parallel), at least 1: the replica's cap is the rank's times these.
	Ranks int
	// SpecTokens is k_spec, the speculative tokens a sequence adds to each
	// step: one sequence is 1+k_spec batch tokens.
	SpecTokens int
}

// IsZero reports a term that adds nothing.
func (t BatchTerm) IsZero() bool { return t.Slope == 0 }

// tokensPerRank is the batch tokens per engine rank of seqs sequences
// resident on the replica.
func (t BatchTerm) tokensPerRank(seqs float64) float64 {
	return seqs / float64(max(t.Ranks, 1)) * float64(1+max(t.SpecTokens, 0))
}

// At is the seconds the term adds to a step while the replica holds seqs
// sequences.
func (t BatchTerm) At(seqs float64) float64 {
	if t.IsZero() {
		return 0
	}
	return t.Slope * max(0, t.tokensPerRank(seqs)-t.Roof)
}

// UsableWith reports whether the term can be priced on line (usable): a
// bootstrap's resampled line may sit too low for the term fitted on the
// point line.
func (t BatchTerm) UsableWith(line itl.Model) bool { return t.IsZero() || t.usable(line) }

// usable reports a term the pricing can read with line: finite, a positive
// slope, a roof at or above 0, and a line whose request rate still rises
// with the batch past the roof. Past it ITL = (B - Slope*Roof) + (rising in
// n), so B must stay above Slope*Roof, or a larger batch would complete
// fewer requests and no fill would answer a rate.
func (t BatchTerm) usable(line itl.Model) bool {
	return Finite(t.Slope) && t.Slope > 0 && Finite(t.Roof) && t.Roof >= 0 && t.Ranks >= 1 && t.SpecTokens >= 0 &&
		line.B > t.Slope*t.Roof
}

// itlAt is the ITL a replica of d runs at fill k: the line, plus the batch
// term at the sequences resident there (CappedSequences, at the replica's
// cap). Up to slotFill that is the count the rate reads; past it the cap may
// count more sequences than the slots, which errs high, the safe side. With
// no term it is the line's, to the bit.
func (d Decode) itlAt(k float64) float64 {
	itlSec := d.Line.ITLAt(k)
	if d.Batch.IsZero() {
		return itlSec
	}
	return itlSec + d.Batch.At(CappedSequences(k, d.CapacityTokens, d.KVPerRequest, float64(d.ReplicaCap)))
}

// BatchRow is one decode row a batch term is fitted on.
type BatchRow struct {
	// K is the row's fill, SeqsPerRank its running sequences per engine
	// rank, and ITLSec its mean ITL in seconds.
	K, SeqsPerRank, ITLSec float64
}

// The batch term's gates (FitBatchTerm).
const (
	// BatchTermMinRows is the fewest rows a term is fitted on.
	BatchTermMinRows = 60
	// BatchTermMinSide is the fewest rows on each side of the roof.
	BatchTermMinSide = 20
	// BatchTermMinGain is the least share of the line's own squared error on
	// the rows (the residuals' sum of squares) the line plus the term must
	// remove: under it the rows do not tell the term from the line, which at
	// one KV per request they cannot.
	BatchTermMinGain = 0.10
	// BatchTermMinSpread is the least spread, in batch tokens per rank, of
	// the rows past the roof: a slope read off one cluster is noise.
	BatchTermMinSpread = 16
	// BatchTermMinEffect is the least the term must add at the largest batch
	// read, as a share of the mean ITL of the rows past the roof: under it the
	// term is a rounding of the line, not a cost.
	BatchTermMinEffect = 0.02
)

// Why a batch term was not fitted (BatchFit.Why).
const (
	BatchTermNoLine    = "no line"
	BatchTermFewRows   = "too few rows"
	BatchTermNoRise    = "no roof past which the residuals rise"
	BatchTermSmallGain = "too small a gain over the line"
	BatchTermNarrow    = "too narrow a spread past the roof"
	BatchTermSmall     = "too small an effect on the ITL"
	BatchTermRateFalls = "the rate would fall with the batch"
)

// batchTermCandidates is how many roofs FitBatchTerm tries, evenly over the
// rows' 10th to 90th percentile of batch tokens per rank.
const batchTermCandidates = 33

// BatchTermSpeculative is why a speculating variant is not fitted (BatchFit.Why):
// its rows read the step's mean ITL, and its line may be per accepted token.
const BatchTermSpeculative = "speculative decoding: the rows read a step, the line may read a token"

// BatchFit is what FitBatchTerm read of a variant's rows.
type BatchFit struct {
	// Term is the accepted term, zero when Why is set.
	Term BatchTerm
	// Rows is the rows fitted on, Above those past the best roof.
	Rows, Above int
	// Slope and Roof are the best candidate's, accepted or not; Gain the
	// share of the line's own squared error on the rows that the line plus
	// the term removes, as it is priced (no offset): negative where the term
	// fits them worse than the line alone.
	Slope, Roof, Gain float64
	// Why is the gate that refused the term, "" when accepted.
	Why string
}

// FitBatchTerm fits the batch term on the residuals of line over rows, r =
// ITL - line(k), x the row's batch tokens per rank (SeqsPerRank times
// 1+specTokens). At each candidate roof (the rows' x at the 10th to the 90th
// percentile) a is the slope of r on h = max(0, x - roof) with h's mean and
// its linear part in k removed: the coefficient of h in r = c + b*k + a*h,
// the part of the batch the line's slope did not already take. The roof kept
// is the one where that model fits r best, with a positive a; the gain gate
// is read on the model as priced, r = a*h with no offset and no change to the
// line, against the line alone. The term is
// accepted only with BatchTermMinRows rows, BatchTermMinSide
// on each side of the roof, a gain of BatchTermMinGain, a spread of
// BatchTermMinSpread past the roof, an effect of BatchTermMinEffect at the
// largest batch, and a rate that still rises past it (BatchTerm.usable); Why
// names the first gate that failed. ranks is the
// replica's engine ranks, carried into the term.
func FitBatchTerm(line itl.Model, rows []BatchRow, ranks, specTokens int) BatchFit {
	if !itl.ValidModel(line.A, line.B) || !(line.B > 0) {
		return BatchFit{Why: BatchTermNoLine}
	}
	tokens := float64(1 + max(specTokens, 0))
	type point struct{ x, r, k, itl float64 }
	pts := make([]point, 0, len(rows))
	for _, row := range rows {
		x, r := row.SeqsPerRank*tokens, row.ITLSec-line.ITLAt(row.K)
		if Finite(x) && x > 0 && Finite(r) && Finite(row.K) && row.K > 0 && row.ITLSec > 0 {
			pts = append(pts, point{x, r, row.K, row.ITLSec})
		}
	}
	fit := BatchFit{Rows: len(pts)}
	if len(pts) < BatchTermMinRows {
		fit.Why = BatchTermFewRows
		return fit
	}
	slices.SortFunc(pts, func(a, b point) int { return cmp.Or(cmp.Compare(a.x, b.x), cmp.Compare(a.r, b.r)) })
	n := float64(len(pts))
	var sumR, sumK, line2 float64
	for _, p := range pts {
		sumR, sumK, line2 = sumR+p.r, sumK+p.k, line2+p.r*p.r
	}
	meanR, meanK := sumR/n, sumK/n
	var skk, skr, srr float64
	for _, p := range pts {
		skk += (p.k - meanK) * (p.k - meanK)
		skr += (p.k - meanK) * (p.r - meanR)
		srr += (p.r - meanR) * (p.r - meanR)
	}
	// The residuals' error left once a line in k is taken out of them: the
	// full model's error at a roof is this less a*sxy.
	srrK := srr
	if skk > 0 {
		srrK -= skr * skr / skk
	}
	best, priced := math.Inf(1), 0.0
	for i := range batchTermCandidates {
		q := 0.1 + 0.8*float64(i)/float64(batchTermCandidates-1)
		roof := pts[int(q*(n-1))].x
		var above int
		var sumH float64
		for _, p := range pts {
			if p.x > roof {
				above++
				sumH += p.x - roof
			}
		}
		if above < BatchTermMinSide || len(pts)-above < BatchTermMinSide {
			continue
		}
		meanH := sumH / n
		// h's linear part in k, which the line's slope already holds.
		var skh float64
		for _, p := range pts {
			skh += (p.k - meanK) * (max(0, p.x-roof) - meanH)
		}
		beta := 0.0
		if skk > 0 {
			beta = skh / skk
		}
		var sxx, sxy float64
		for _, p := range pts {
			h := max(0, p.x-roof) - meanH - beta*(p.k-meanK)
			sxx += h * h
			sxy += h * (p.r - meanR)
		}
		if !(sxx > 0) {
			continue
		}
		a := sxy / sxx
		// The roof is the one the full model, r = c + b*k + a*h, fits best.
		if sse := srrK - a*sxy; !(a > 0) || !(sse < best) {
			continue
		}
		best = srrK - a*sxy
		// The error of the model priced: the line plus a*h, no offset.
		priced = 0
		for _, p := range pts {
			e := p.r - a*max(0, p.x-roof)
			priced += e * e
		}
		fit.Slope, fit.Roof, fit.Above = a, roof, above
	}
	if math.IsInf(best, 1) {
		fit.Why = BatchTermNoRise
		return fit
	}
	if line2 > 0 {
		fit.Gain = 1 - priced/line2
	}
	hi := pts[len(pts)-1].x // sorted: the largest batch read
	var aboveITL float64
	for _, p := range pts[len(pts)-fit.Above:] {
		aboveITL += p.itl
	}
	aboveITL /= float64(fit.Above)
	term := BatchTerm{Slope: fit.Slope, Roof: fit.Roof, Ranks: max(ranks, 1), SpecTokens: max(specTokens, 0)}
	switch {
	case !(fit.Gain >= BatchTermMinGain):
		fit.Why = BatchTermSmallGain
	case hi-fit.Roof < BatchTermMinSpread:
		fit.Why = BatchTermNarrow
	case !(fit.Slope*(hi-fit.Roof) >= BatchTermMinEffect*aboveITL):
		fit.Why = BatchTermSmall
	case !term.usable(line):
		fit.Why = BatchTermRateFalls
	default:
		fit.Term = term
	}
	return fit
}

// fillAtReplicaRateBisected is fillAtReplicaRate with a batch term: the
// least fill at which one replica carries lam requests a second, n(k) /
// (OL * ITL(k)) with n(k) = k*C/KVreq, found by bisection on [0, slotFill].
// The rate rises with k (BatchTerm.usable), so it is ok wherever the
// closed form is: lam carried at or under the slots.
func (d Decode) fillAtReplicaRateBisected(lam float64) (float64, bool) {
	if lam == 0 {
		return 0, true
	}
	rate := func(k float64) float64 {
		return k * d.CapacityTokens / d.KVPerRequest / (d.OutputTokens * d.itlAt(k))
	}
	top := d.full().slotFill
	if !Finite(lam) || !(lam > 0) || !(rate(top) >= lam) {
		return 0, false
	}
	lo, hi := 0.0, top
	for range 200 {
		mid := (lo + hi) / 2
		if mid == lo || mid == hi {
			break
		}
		if rate(mid) < lam {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi, true
}

// fillForTPOTBisected is fillForTPOT with a batch term: the fill at which
// ITL(k) times R_q(k) reaches t, by bisection from lo, where it is under t
// (its one caller refuses a target at or under the floor). The product rises
// with k (A > 0, the term never falls, R_q never falls), and past a full
// cache it is read on as it is under one, so the upper end doubles from 1
// until it reaches t, which with A > 0 it does. It returns the side below t, as
// fillForTPOT does, with the flag that the global R_q stood in there.
func (d Decode) fillForTPOTBisected(t float64, r TPOTRatio, lo float64) (float64, bool) {
	at := func(k float64) float64 {
		v, _ := d.tpotAtFill(k, r)
		return v
	}
	hi := max(1.0, lo)
	for range 64 {
		if at(hi) >= t {
			break
		}
		lo, hi = hi, 2*hi
	}
	for range 200 {
		mid := (lo + hi) / 2
		if mid == lo || mid == hi {
			break
		}
		if at(mid) < t {
			lo = mid
		} else {
			hi = mid
		}
	}
	_, global := r.At(lo)
	return lo, global
}
