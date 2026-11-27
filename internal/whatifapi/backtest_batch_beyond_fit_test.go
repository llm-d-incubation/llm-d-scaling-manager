package whatifapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// A figure carries the batch its answer's basis does, and none where the
// answer has none.
func TestFigureOf_CarriesTheBatchBeyondFit(t *testing.T) {
	a := whatif.Answer{Value: 0.03, Basis: whatif.Basis{Fill: 0.5, Batch: &whatif.BatchBeyondFit{Priced: 248, Fitted: 210}},
		Flags: []whatif.Flag{"BatchBeyondFit"}}
	f := figureOf(a, unitSeconds, true)
	require.NotNil(t, f.BatchBeyondFit)
	assert.Equal(t, BatchBeyondFit{Priced: 248, Fitted: 210}, *f.BatchBeyondFit)
	a.Basis.Batch = nil
	assert.Nil(t, figureOf(a, unitSeconds, true).BatchBeyondFit)
}

// A step's batch is its mean TPOT's; where the TPOT is refused at or past
// the decode knee, the knee's; and none inside the fit, or under a knee whose
// TPOT is refused for another reason.
func TestStepBeyondFit(t *testing.T) {
	knee := whatif.Answer{Value: 15.1, Basis: whatif.Basis{Batch: &whatif.BatchBeyondFit{Priced: 256, Fitted: 210, Ranks: 2}}}
	priced := Figure{Value: ptr(0.03), BatchBeyondFit: &BatchBeyondFit{Priced: 248, Fitted: 210}}
	refused := Figure{Refused: &Refusal{Reason: string(whatif.Saturated)}}

	got := stepBeyondFit(priced, knee, 14.9)
	require.NotNil(t, got)
	assert.Equal(t, BatchBeyondFit{Priced: 248, Fitted: 210}, *got, "the TPOT's own")

	got = stepBeyondFit(refused, knee, 15.9)
	require.NotNil(t, got)
	assert.Equal(t, BatchBeyondFit{Priced: 256, Fitted: 210, Ranks: 2}, *got, "past the knee: the knee's, ranks and all")

	assert.Nil(t, stepBeyondFit(Figure{Value: ptr(0.02)}, knee, 12), "inside the fit")
	assert.Nil(t, stepBeyondFit(refused, knee, 14), "refused under the knee: not the knee's batch")
	inside := whatif.Answer{Value: 15.1}
	assert.Nil(t, stepBeyondFit(refused, inside, 15.9), "a knee inside the fit")
	assert.Nil(t, stepBeyondFit(refused, whatif.Unanswerable{Reason: whatif.NoLine}, 15.9), "no knee")
}

// The note names the judged steps past the fit and the batches, and is
// absent with none.
func TestBatchBeyondFitNote(t *testing.T) {
	at := func(hm string) time.Time {
		v, err := time.Parse("15:04", hm)
		require.NoError(t, err)
		return v
	}
	steps := []BacktestStep{
		{Start: at("12:20")},
		{Start: at("12:30"), BatchBeyondFit: &BatchBeyondFit{Priced: 216, Fitted: 210}},
		{Start: at("12:35"), Excluded: ExcludedBurst, BatchBeyondFit: &BatchBeyondFit{Priced: 256, Fitted: 210}},
		{Start: at("13:00"), BatchBeyondFit: &BatchBeyondFit{Priced: 256, Fitted: 210}},
	}
	assert.Equal(t, "BatchBeyondFit on 2 judged steps (12:30, 13:00): the decode line was fitted on up to 210 sequences a pod; "+
		"these steps run about 216 to 256, so TPOT may read low and the decode knee high. The figures are priced all the same.",
		batchBeyondFitNote(steps))
	assert.Empty(t, batchBeyondFitNote(steps[:1]))

	ranked := []BacktestStep{{Start: at("12:30"), BatchBeyondFit: &BatchBeyondFit{Priced: 31, Fitted: 26, Ranks: 8}}}
	assert.Contains(t, batchBeyondFitNote(ranked), "fitted on up to 26 sequences an engine rank; these steps run about 31",
		"a pod of 8 ranks: the batches are a rank's")
}
