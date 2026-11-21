package whatif

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// q32jShortKV is the decode role of the Qwen3-32B TP2 P/D staircase q32j at
// its 1000/1000 phases, priced on the warm-up's fill line (B = 10.92 ms, A =
// 34.69 ms per unit fill, read off the recording's rows): C = 764,672 tokens,
// KVreq = 1500, OL = 1000, batch cap 256 on one rank, two replicas. At this
// KV per request a replica holds its 256 sequences at a fill of 0.50.
func q32jShortKV() Decode {
	return Decode{
		Line:           itl.Model{A: 0.03469, B: 0.01092},
		Band:           DefaultBand(),
		CapacityTokens: 764672,
		KVPerRequest:   1500,
		OutputTokens:   1000,
		ReplicaCap:     256,
		Replicas:       2,
	}
}

// q32jBatchTerm is the term a joint fit of the q32j warm-up and its 1000/1000
// phase gives: 0.034 ms a sequence past 120 sequences per rank.
func q32jBatchTerm() BatchTerm {
	return BatchTerm{Slope: 0.034e-3, Roof: 120, Ranks: 1}
}

// The term counts a sequence 1+k_spec batch tokens, split over the ranks.
func TestBatchTerm_TokensPerRank(t *testing.T) {
	term := BatchTerm{Slope: 1e-4, Roof: 100, Ranks: 2, SpecTokens: 1}
	near(t, "under the roof", term.At(100), 0, 0)
	// 200 sequences over 2 ranks, 2 tokens each: 200 tokens a rank.
	near(t, "past the roof", term.At(200), 1e-4*100, 1e-15)
	near(t, "the zero term", BatchTerm{}.At(1e6), 0, 0)
}

// A term whose roof no batch reaches prices what the line prices: the
// bisected fills agree with the closed forms they replace.
func TestBatchTerm_AnUnreachedRoofIsTheLine(t *testing.T) {
	off := q32jShortKV()
	on := off
	on.Batch = BatchTerm{Slope: 1e-6, Roof: 1000, Ranks: 1}
	for _, d := range []Decode{off, on} {
		_ = mustAnswer(t, KneeRate(d))
	}
	near(t, "knee", mustAnswer(t, KneeRate(on)).Value, mustAnswer(t, KneeRate(off)).Value, 1e-12)
	for _, rate := range []float64{0.5, 5, 10, 14} {
		kOff := mustAnswer(t, FillAtRate(off, rate)).Value
		kOn := mustAnswer(t, FillAtRate(on, rate)).Value
		near(t, "fill at rate", kOn, kOff, 1e-12)
	}
	for _, target := range []float64{0.025, 0.030, 0.040} {
		for _, r := range []TPOTRatio{{Value: 1.3}, {Value: 1.1, ByFill: []FillRatio{{Hi: 0.3, Value: 1.1}, {Lo: 0.3, Hi: 1, Value: 1.4}}}} {
			q := RateQuery{Target: TPOTTarget{Seconds: target, Percentile: 0.9, Ratio: r}, Headroom: 1}
			a, b := mustAnswer(t, RateForTPOT(off, q)), mustAnswer(t, RateForTPOT(on, q))
			near(t, "rate for TPOT", b.Value, a.Value, 1e-9*a.Value)
			near(t, "its fill", b.Basis.Fill, a.Basis.Fill, 1e-9)
		}
	}
}

// Past the roof the step costs more: the TPOT at a rate is the line plus the
// term at the sequences there, the fill carries the rate by Little's law at
// that ITL, and the knee and the rate for a target fall.
func TestBatchTerm_PricesTheStepPastTheRoof(t *testing.T) {
	off := q32jShortKV()
	on := off
	on.Batch = q32jBatchTerm()

	kneeOff, kneeOn := mustAnswer(t, KneeRate(off)).Value, mustAnswer(t, KneeRate(on)).Value
	// At 256 sequences the step is 4.62 ms longer: 136 * 0.034 ms.
	full := on.full()
	near(t, "knee with the term", kneeOn, 2*full.slots/(off.OutputTokens*(off.Line.ITLAt(full.fill)+0.034e-3*136)), 1e-9)
	if !(kneeOn < kneeOff) {
		t.Errorf("knee %g with the term, %g without: want it lower", kneeOn, kneeOff)
	}

	const rate = 15.0
	slo := SLOAtReplicas(on, ReplicasQuery{Rate: rate})
	k := mustAnswer(t, slo.Rate).Basis.Fill
	seqs := k * on.CapacityTokens / on.KVPerRequest
	if !(seqs > 120) {
		t.Fatalf("%g sequences at %g r/s: the case wants the roof passed", seqs, rate)
	}
	want := on.Line.ITLAt(k) + 0.034e-3*(seqs-120)
	near(t, "mean TPOT", mustAnswer(t, slo.MeanTPOT).Value, want, 1e-12)
	near(t, "Little's law at the fill", seqs/(want*on.OutputTokens), rate/2, 1e-9)
	tpotOff := mustAnswer(t, SLOAtReplicas(off, ReplicasQuery{Rate: rate}).MeanTPOT).Value
	if !(want > tpotOff) {
		t.Errorf("TPOT %g with the term, %g without: want it higher", want, tpotOff)
	}

	q := RateQuery{Target: TPOTTarget{Seconds: 0.025}, Headroom: 1}
	a, b := mustAnswer(t, RateForTPOT(off, q)), mustAnswer(t, RateForTPOT(on, q))
	if !(b.Value < a.Value) {
		t.Errorf("rate for 25 ms %g with the term, %g without: want it lower", b.Value, a.Value)
	}
	if b.Basis.BoundBy[0] == BoundTPOT {
		near(t, "TPOT at the answer's fill", on.itlAt(b.Basis.Fill), 0.025, 1e-9)
	}
}

// A term the line cannot carry, where a larger batch would complete fewer
// requests, is refused, not priced.
func TestBatchTerm_ARateThatFallsIsRefused(t *testing.T) {
	d := q32jShortKV()
	d.Batch = BatchTerm{Slope: 1e-3, Roof: 20, Ranks: 1} // 20 ms > B
	u, ok := KneeRate(d).(Unanswerable)
	if !ok || u.Reason != InvalidInput {
		t.Fatalf("got %#v, want InvalidInput", KneeRate(d))
	}
}

// batchRows is rows of two shapes, at KVreq 1500 and 8000, over the band's
// fills, ITL = line + term + noise: at a fill the short shape runs five times
// the sequences, which is what tells the term from the line's slope.
func batchRows(line itl.Model, term BatchTerm, kvs []float64, noise float64) []BatchRow {
	rng := rand.New(rand.NewPCG(309, 1))
	rows := make([]BatchRow, 0, 150*len(kvs))
	for _, kv := range kvs {
		for i := range 150 {
			k := 0.15 + 0.35*float64(i)/149
			n := min(k*764672/kv, 256)
			rows = append(rows, BatchRow{K: k, SeqsPerRank: n,
				ITLSec: line.ITLAt(k) + term.At(n) + noise*rng.NormFloat64()})
		}
	}
	return rows
}

// Across two shapes the term is found: its roof and its slope, on the
// residuals of the line.
func TestFitBatchTerm_FindsTheTermAcrossShapes(t *testing.T) {
	line := itl.Model{A: 0.03, B: 0.012}
	fit := FitBatchTerm(line, batchRows(line, q32jBatchTerm(), []float64{1500, 8000}, 0.3e-3), 1, 0)
	if fit.Why != "" {
		t.Fatalf("refused: %s (%+v)", fit.Why, fit)
	}
	near(t, "slope", fit.Term.Slope, 0.034e-3, 0.004e-3)
	near(t, "roof", fit.Term.Roof, 120, 15)
	if fit.Term.Ranks != 1 || fit.Gain < 0.5 {
		t.Errorf("fit %+v, want one rank and most of the error removed", fit)
	}
}

// A sequence counts 1+k_spec tokens: the roof is in tokens per rank.
func TestFitBatchTerm_SpeculativeTokens(t *testing.T) {
	line := itl.Model{A: 0.03, B: 0.012}
	term := BatchTerm{Slope: 0.017e-3, Roof: 240, Ranks: 1, SpecTokens: 1}
	fit := FitBatchTerm(line, batchRows(line, term, []float64{1500, 8000}, 0.3e-3), 1, 1)
	if fit.Why != "" {
		t.Fatalf("refused: %s", fit.Why)
	}
	near(t, "roof in tokens", fit.Term.Roof, 240, 30)
	if fit.Term.SpecTokens != 1 {
		t.Errorf("spec tokens %d, want 1", fit.Term.SpecTokens)
	}
}

// Rows on the line leave nothing for the term: refused.
func TestFitBatchTerm_RowsOnTheLineAreRefused(t *testing.T) {
	line := itl.Model{A: 0.03, B: 0.012}
	fit := FitBatchTerm(line, batchRows(line, BatchTerm{}, []float64{1500, 8000}, 0.3e-3), 1, 0)
	if fit.Why == "" || !fit.Term.IsZero() {
		t.Fatalf("accepted %+v, want it refused", fit)
	}
}

// The gates, each on its own.
func TestFitBatchTerm_Gates(t *testing.T) {
	line := itl.Model{A: 0.03, B: 0.012}
	rows := batchRows(line, q32jBatchTerm(), []float64{1500}, 0.1e-3)
	cases := []struct {
		name string
		line itl.Model
		rows []BatchRow
		why  string
	}{
		{"no line", itl.Model{}, rows, BatchTermNoLine},
		{"few rows", line, rows[:BatchTermMinRows-1], BatchTermFewRows},
		// B under Slope*Roof: the term would be 1 ms a token past 120, 120 ms.
		{"the rate falls", itl.Model{A: 0.03, B: 0.012}, batchRows(line, BatchTerm{Slope: 1e-3, Roof: 120, Ranks: 1},
			[]float64{1500}, 0.1e-3), BatchTermRateFalls},
	}
	for _, c := range cases {
		if got := FitBatchTerm(c.line, c.rows, 1, 0); got.Why != c.why || !got.Term.IsZero() {
			t.Errorf("%s: why %q term %+v, want %q and no term", c.name, got.Why, got.Term, c.why)
		}
	}
	// Every row under one batch: no roof has rows on both sides.
	flat := make([]BatchRow, 100)
	for i := range flat {
		flat[i] = BatchRow{K: 0.2 + 0.001*float64(i), SeqsPerRank: 50, ITLSec: line.ITLAt(0.2) + 1e-4*float64(i%3)}
	}
	if got := FitBatchTerm(line, flat, 1, 0); got.Why != BatchTermNoRise {
		t.Errorf("one batch: why %q, want %q", got.Why, BatchTermNoRise)
	}
	if math.IsNaN(FitBatchTerm(line, rows, 1, 0).Gain) {
		t.Error("NaN gain")
	}
}

// The line the planner prices was fitted on the same rows, so its slope has
// already taken the part of the batch that moves with the fill. Fitted on
// the residuals of that line, the term's slope is read on the rest of the
// batch: it comes back at the true 0.034 ms and roof 120, where a slope of
// the raw hinge on those residuals reads 0.015 ms. Priced on that line as it
// is, the term removes little of the line's own error, since the line took
// part of it: the gain gate refuses it, and the line alone is priced.
func TestFitBatchTerm_OnTheRowsOwnLine(t *testing.T) {
	truth := itl.Model{A: 0.03, B: 0.012}
	rows := batchRows(truth, q32jBatchTerm(), []float64{1500, 8000}, 0.3e-3)
	// The line an OLS fit of ITL on k over the rows gives.
	var sk, sy, skk, sky float64
	for _, r := range rows {
		sk, sy, skk, sky = sk+r.K, sy+r.ITLSec, skk+r.K*r.K, sky+r.K*r.ITLSec
	}
	n := float64(len(rows))
	a := (n*sky - sk*sy) / (n*skk - sk*sk)
	own := itl.Model{A: a, B: (sy - a*sk) / n}
	if !(own.A > truth.A) {
		t.Fatalf("the rows' own line %+v should have taken part of the term into its slope", own)
	}
	fit := FitBatchTerm(own, rows, 1, 0)
	near(t, "slope", fit.Slope, 0.034e-3, 0.004e-3)
	near(t, "roof", fit.Roof, 120, 15)
	if fit.Why != BatchTermSmallGain || !fit.Term.IsZero() {
		t.Errorf("fit %+v: want the gain gate to refuse a term on the line that took part of it", fit)
	}
}
