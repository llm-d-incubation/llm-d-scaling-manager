package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// fitVariant is denseVariant (1M tokens of KV, 4,000 a request, so 250
// sequences a pod at fill 1, a cap of 256) whose line's fit rows held 200
// sequences a pod at 3,000 tokens a request: a KV a request 1.33 times the
// rows', a shape the gate before #354 (under half the rows') never flagged.
func fitVariant() pricedVariant {
	pv := denseVariant()
	pv.state.FitRows = []whatif.LineRow{{K: 0.4, KVPerRequest: 3000, Sequences: 133}, {K: 0.6, KVPerRequest: 3000, Sequences: 200}}
	return pv
}

// atFill is a decode answer priced at fill k.
func atFill(k float64) whatif.Answer {
	return whatif.Answer{Value: 0.03, Basis: whatif.Basis{Fill: k}}
}

// beyondOf is the flag and the batches qualify gives an answer at fill k.
func beyondOf(t *testing.T, pv pricedVariant, k float64) (bool, *whatif.BatchBeyondFit) {
	t.Helper()
	a, ok := pv.qualify(atFill(k)).(whatif.Answer)
	require.True(t, ok)
	return a.HasFlag(batchBeyondFit), a.Basis.Batch
}

// The flag is raised exactly where the batch a figure's fill runs is past
// the most the fit rows held, whatever the KV a request: at 200 sequences,
// the rows' own, it is not; at 201 it is.
func TestBatchBeyondFit_RaisedExactlyPastTheFitBatch(t *testing.T) {
	for _, c := range []struct {
		name  string
		fill  float64
		want  bool
		batch int64
	}{
		{"well inside", 0.5, false, 125},
		{"at the fit's most", 0.8, false, 200},
		{"one past it", 0.804, true, 201},
		{"well past it", 0.9, true, 225},
		{"no fill", 0, false, 0},
	} {
		pv := fitVariant()
		assert.Equal(t, c.batch, pv.batchAt(c.fill), c.name)
		got, b := beyondOf(t, pv, c.fill)
		assert.Equal(t, c.want, got, c.name)
		if !c.want {
			assert.Nil(t, b, c.name)
			continue
		}
		require.NotNil(t, b, c.name)
		assert.Equal(t, whatif.BatchBeyondFit{Priced: c.batch, Fitted: 200}, *b, c.name)
	}
}

// The batch priced never passes the batch cap: a cap at the fit's batch is
// inside it, one past it is not.
func TestBatchBeyondFit_TheCapBoundsTheBatch(t *testing.T) {
	pv := fitVariant()
	pv.perRank = 200
	got, _ := beyondOf(t, pv, 0.9)
	assert.False(t, got, "capped at 200, the rows' own")
	pv.perRank = 210
	got, b := beyondOf(t, pv, 0.9)
	require.True(t, got)
	assert.Equal(t, whatif.BatchBeyondFit{Priced: 210, Fitted: 200}, *b)
}

// A held line reads the held fit's rows (FitRows), not the latest rows a
// smaller load left (LineRows): 150 sequences is inside the 200 the held fit
// held, though past the 100 the latest rows ran. With no fit rows known the
// latest rows are read.
func TestBatchBeyondFit_AHeldLineReadsTheHeldFit(t *testing.T) {
	pv := fitVariant()
	pv.state.Tier = whatif.TierHeld
	pv.state.LineRows = []whatif.LineRow{{K: 0.3, KVPerRequest: 3000, Sequences: 100}}
	got, _ := beyondOf(t, pv, 0.6)
	assert.False(t, got, "150 against the held fit's 200")
	got, b := beyondOf(t, pv, 0.88)
	require.True(t, got, "220 against the held fit's 200")
	assert.Equal(t, whatif.BatchBeyondFit{Priced: 220, Fitted: 200}, *b)

	pv.state.FitRows = nil
	got, b = beyondOf(t, pv, 0.6)
	require.True(t, got, "no fit rows: the latest rows' 100")
	assert.Equal(t, whatif.BatchBeyondFit{Priced: 150, Fitted: 100}, *b)
}

// A row on a ramp says nothing of the line, nor does a latest row above the
// band; a fit row above it is an above-band line's own and is read; a row
// that read no sequences is its fill over its KV a request; the batch is an
// engine rank's.
func TestBatchBeyondFit_TheRowsRead(t *testing.T) {
	pv := fitVariant()
	pv.state.FitRows = append(pv.state.FitRows, whatif.LineRow{K: 0.7, KVPerRequest: 3000, Sequences: 400, Ramp: true})
	seen, ok := pv.fitBatch()
	require.True(t, ok)
	assert.Equal(t, int64(200), seen, "the ramp is left out")

	pv.state.FitRows = append(pv.state.FitRows, whatif.LineRow{K: 0.9, KVPerRequest: 3000, Sequences: 300, AboveBand: true})
	seen, ok = pv.fitBatch()
	require.True(t, ok)
	assert.Equal(t, int64(300), seen, "a fit row above the band: an above-band line's own")

	pv.state.LineRows, pv.state.FitRows = pv.state.FitRows, nil
	seen, ok = pv.fitBatch()
	require.True(t, ok)
	assert.Equal(t, int64(200), seen, "a latest row above the band is left out")

	pv.state.FitRows = []whatif.LineRow{{K: 0.6, KVPerRequest: 3000}}
	seen, ok = pv.fitBatch()
	require.True(t, ok)
	assert.Equal(t, int64(200), seen, "no sequences read: 0.6 x 1M / 3,000")

	pv.state.FitRows = []whatif.LineRow{{K: 0.6, Sequences: 400}}
	pv.state.Ranks = 2
	seen, ok = pv.fitBatch()
	require.True(t, ok)
	assert.Equal(t, int64(200), seen, "400 a pod over 2 ranks")
	assert.Equal(t, int64(112), pv.batchAt(0.9), "0.9 x 1M / 4,000 over 2 ranks")

	pv.state.FitRows, pv.state.LineRows = nil, nil
	_, ok = pv.fitBatch()
	assert.False(t, ok, "no rows: nothing to flag against")
	got, _ := beyondOf(t, pv, 0.9)
	assert.False(t, got)
}

// A refusal is not flagged, and the flag is no engine note: a variant's
// other figures do not carry it.
func TestBatchBeyondFit_OnlyThePricedFigure(t *testing.T) {
	pv := fitVariant()
	u := whatif.Unanswerable{Reason: whatif.Saturated, Detail: "past the knee"}
	assert.Equal(t, whatif.Result(u), pv.qualify(u))
	for _, n := range pv.engineNotes() {
		assert.NotEqual(t, batchBeyondFit, n.flag)
	}
	assert.NotContains(t, pv.flags, batchBeyondFit)
}

// The answer's flags line says both batches in plain words, the most the
// figures flagged run; the wire carries them beside the fill.
func TestBatchBeyondFit_TheWords(t *testing.T) {
	pv := fitVariant()
	var b strings.Builder
	flagsLine(&b, pv, pv.qualify(atFill(0.5)), pv.qualify(atFill(0.9)), pv.qualify(atFill(0.84)))
	assert.Equal(t, "flags: BatchBeyondFit (the decode line was fitted on up to 200 sequences a pod; this load runs about 225, "+
		"so TPOT may read low and the decode knee high)\n", b.String())

	pv.state.Ranks = 2
	pv.state.FitRows = []whatif.LineRow{{K: 0.6, Sequences: 200}}
	b.Reset()
	flagsLine(&b, pv, pv.qualify(atFill(0.9)))
	assert.Contains(t, b.String(), "up to 100 sequences an engine rank; this load runs about 112")
	raw, err := json.Marshal(toWire(pv.qualify(atFill(0.9)), unitSeconds))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"batchBeyondFit":{"priced":112,"fitted":100,"ranks":2}`, "a rank's batches say so")

	raw, err = json.Marshal(toWire(fitVariant().qualify(atFill(0.9)), unitSeconds))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"batchBeyondFit":{"priced":225,"fitted":200}`, "one rank: no ranks key")
	raw, err = json.Marshal(toWire(fitVariant().qualify(atFill(0.5)), unitSeconds))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "batchBeyondFit", "inside the fit: no key")
}

// q32ShapeAStep is one step of the Qwen3-32B staircase's shape A phase with
// flow control on, as the backtest of that phase against the warm-up's fit
// read it: its rate and mean lengths.
type q32ShapeAStep struct {
	at             string
	rate           float64
	prompt, output float64
}

// shapeKnobOf is a length as the backtest prices it, on whatifapi's 5% grid.
func shapeKnobOf(x float64) float64 {
	g := math.Log(1.05)
	return math.Round(math.Exp(math.Round(math.Log(x)/g) * g))
}

// On the Qwen3-32B warm-up the decode line is held, from a fit whose rows ran
// up to 210 sequences a pod. Shape A's test steps price the mean TPOT the
// backtest prices, at the step's own rate and lengths: up to 12:20 the batch
// is inside the fit's and nothing is flagged; from 12:30 it runs 216 to 251
// and the TPOT is flagged; past the decode knee (13:00, 13:15) the TPOT is
// refused and the knee, priced at the batch cap of 256, is flagged. The
// figures are the ones priced without the flag. The steps are read off the
// phase's backtest (the phase's recording is not committed; the warm-up is).
func TestBatchBeyondFit_Q32ShapeAStepsPastTheFit(t *testing.T) {
	if testing.Short() {
		t.Skip("replays the Qwen3-32B warm-up")
	}
	st, err := q32State()
	require.NoError(t, err)
	decode := ""
	for _, vs := range st.Variants {
		if domain.CanonicalRole(vs.Role) == domain.RoleDecode {
			decode = vs.Variant
		}
	}
	require.NotEmpty(t, decode)
	s := &servedState{st: st, decodeVariant: decode}
	cv2 := 1.0
	for _, c := range []struct {
		step    q32ShapeAStep
		flagged bool
		refused bool
		batch   int64
	}{
		{q32ShapeAStep{"12:10", 12.751, 999.52, 999.97}, false, false, 0},
		{q32ShapeAStep{"12:20", 13.975, 1002.23, 998.94}, false, false, 0},
		{q32ShapeAStep{"12:30", 14.276, 1000.03, 1000.81}, true, false, 216},
		{q32ShapeAStep{"12:40", 15.009, 1000.17, 999.95}, true, false, 251},
		{q32ShapeAStep{"13:00", 15.926, 1001.00, 999.68}, true, true, 256},
		{q32ShapeAStep{"13:15", 15.473, 1000.59, 1000.15}, true, true, 256},
	} {
		fl, err := s.Fleet(whatifapi.Knobs{Prefill: 2, Decode: 2, PromptTokens: shapeKnobOf(c.step.prompt),
			OutputTokens: shapeKnobOf(c.step.output), ArrivalCV2: &cv2, Headroom: 0.9})
		require.NoError(t, err, c.step.at)
		mean, _ := fl.TPOTAt(c.step.rate)
		knee, ok := fl.DecodeKnee().(whatif.Answer)
		require.True(t, ok, c.step.at)
		require.NotNil(t, knee.Basis.Batch, "the knee runs the batch cap, past the fit's: %s", c.step.at)
		assert.Equal(t, whatif.BatchBeyondFit{Priced: 256, Fitted: 210}, *knee.Basis.Batch, c.step.at)
		if c.refused {
			u, ok := mean.(whatif.Unanswerable)
			require.True(t, ok, "%s: %v", c.step.at, mean)
			assert.Equal(t, whatif.Saturated, u.Reason, c.step.at)
			assert.GreaterOrEqual(t, c.step.rate, knee.Value, c.step.at)
			continue
		}
		a, ok := mean.(whatif.Answer)
		require.True(t, ok, "%s: %v", c.step.at, mean)
		assert.Equal(t, c.flagged, a.HasFlag(batchBeyondFit), c.step.at)
		if !c.flagged {
			assert.Nil(t, a.Basis.Batch, c.step.at)
			continue
		}
		require.NotNil(t, a.Basis.Batch, c.step.at)
		assert.Equal(t, whatif.BatchBeyondFit{Priced: c.batch, Fitted: 210}, *a.Basis.Batch, c.step.at)
	}
}

// With saturation.SwitchBacktestStepOutputDispersion on, a step is priced at
// its own output spread, and the batch its figure is flagged at is the one
// that pricing runs: its own fill over its own KV a request, at the same
// rate fewer sequences than the fit's spread prices on the 0.6B staircase's
// A phase, and never the fit's batch carried over.
func TestBatchBeyondFit_AtTheStepsOwnOutputSpread(t *testing.T) {
	batch := func(on bool) (whatif.BatchBeyondFit, pricedVariant, float64) {
		f := answerFlags{set: map[string]bool{"assume-input-tokens": true, "assume-output-tokens": true},
			inputTokens: 1000, outputTokens: 1000, stepOutputDispersion: 1}
		st := stepSpreadState(on)
		st.Variants[0].FitRows = []whatif.LineRow{{K: 0.05, Sequences: 10}}
		vs, err := f.decodeVariants(st)
		require.NoError(t, err)
		require.Len(t, vs, 1)
		pv := vs[0]
		a, ok := whatif.FillAtRate(pv.decode, 32).(whatif.Answer)
		require.True(t, ok)
		q, ok := pv.qualify(atFill(a.Value)).(whatif.Answer)
		require.True(t, ok)
		require.True(t, q.HasFlag(batchBeyondFit), "on=%v", on)
		require.NotNil(t, q.Basis.Batch)
		return *q.Basis.Batch, pv, pv.decode.SeenFill(a.Value)
	}
	off, pvOff, kOff := batch(false)
	on, pvOn, kOn := batch(true)
	for _, c := range []struct {
		b  whatif.BatchBeyondFit
		pv pricedVariant
		k  float64
	}{{off, pvOff, kOff}, {on, pvOn, kOn}} {
		assert.Equal(t, int64(math.Floor(c.k*c.pv.decode.CapacityTokens/c.pv.decode.KVPerRequest)), c.b.Priced,
			"the figure's own fill over its own KV a request")
		assert.Equal(t, int64(10), c.b.Fitted)
	}
	assert.Less(t, on.Priced, off.Priced, "the step's spread runs fewer sequences at the same rate")
}
