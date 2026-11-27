package whatifapi

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// knobsSeen records the knobs of every fleet r prices.
func knobsSeen(r *stubResolver) func() []Knobs {
	var mu sync.Mutex
	var seen []Knobs
	r.onFleet = func(k Knobs) { mu.Lock(); seen = append(seen, k); mu.Unlock() }
	return func() []Knobs {
		mu.Lock()
		defer mu.Unlock()
		return append([]Knobs{}, seen...)
	}
}

// promptsPriced is the knobs with a prompt, split by whether it was read.
func promptsPriced(ks []Knobs) (read, stated int) {
	for _, k := range ks {
		switch {
		case !(k.PromptTokens > 0):
		case k.PromptTokensRead:
			read++
		default:
			stated++
		}
	}
	return read, stated
}

// A day plan's bucket priced at its own shape states the whole prompt its
// cycles read and no hit: the prompt is read, not stated
// (Knobs.PromptTokensRead), so on a P/D fleet the prefill pods'
// computed-token hit nets it, not the decode rows' prefix-cache hit. A
// stated mix is stated.
func TestDayPlan_ABucketsPromptIsRead(t *testing.T) {
	plan := func(req DayPlanRequest) ([]Knobs, *DayPlanResponse) {
		t.Helper()
		r := &stubResolver{}
		seen := knobsSeen(r)
		res, err := r.Resolve(t.Context(), InputsRequest{})
		require.NoError(t, err)
		t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
		for i := range 40 {
			res.Series = append(res.Series, SeriesSample{At: t0.Add(time.Duration(i) * 15 * time.Second), Rate: 6, RateKnown: true,
				Prefill: 2, Decode: 3, PromptTokens: 16000, OutputTokens: 200, HasShape: true})
		}
		p, err := DayPlan(t.Context(), res, req, "schema-v3")
		require.NoError(t, err)
		require.NotEmpty(t, p.Buckets)
		return seen(), p
	}

	ks, p := plan(DayPlanRequest{})
	assert.Equal(t, "bucket", p.Buckets[0].Shape.Source)
	read, stated := promptsPriced(ks)
	assert.Positive(t, read, "the buckets' own prompts were priced")
	assert.Zero(t, stated, "a bucket's own prompt is read, not stated")

	// A stated mix is the question's: as before.
	ks, p = plan(DayPlanRequest{Traffic: &Traffic{Mix: &Mix{PromptTokens: 2000, OutputTokens: 200}}})
	assert.Equal(t, dayShapeStated, p.Buckets[0].Shape.Source)
	read, stated = promptsPriced(ks)
	assert.Zero(t, read, "a stated mix is not read")
	assert.Positive(t, stated)
}

// A backtest's step states the whole prompt its cycles read and no hit: the
// prompt is read, as a day plan's bucket's is.
func TestBacktest_AStepsPromptIsRead(t *testing.T) {
	r := &stubResolver{}
	seen := knobsSeen(r)
	fitS, fitL := btEarlier(btSeries(240, 0, 1))
	fit := btEntry(t, r, "fit", fitS, fitL, btConfig("H100"))
	s, l := btSeries(240, 0, 1)
	btRun(t, fit, btEntry(t, r, "test", s, l, btConfig("H100")), BacktestRequest{})
	read, stated := promptsPriced(seen())
	assert.Positive(t, read, "the steps' own prompts were priced")
	assert.Zero(t, stated, "a step's own prompt is read, not stated")
}

// A bucket the experimental models price at its own shape reads its prompt
// as a bucket without the models does (readShape): the planner's terms at
// that shape are read, not stated, so the prefill pods' hit nets them.
func TestDayPlan_AnOwnShapeBucketsPromptIsRead(t *testing.T) {
	s, e := shapeEntry(t, func(int) bool { return false }, 1, true)
	r, ok := s.resolver.(*stubResolver)
	require.True(t, ok)
	seen := knobsSeen(r)
	out := shapePlan(t, s, e, shapeTarget(t, e, 2.5), false)
	own := 0
	for _, b := range out.Buckets {
		if b.Needed != nil && b.Shape.Source == shapeOwn && b.Shape.Model != nil {
			own++
		}
	}
	require.Positive(t, own, "buckets priced at their own shape")
	read, stated := promptsPriced(seen())
	assert.Positive(t, read, "the own-shape buckets' prompts were priced")
	assert.Zero(t, stated, "an own-shape bucket's prompt is read, not stated")
}
