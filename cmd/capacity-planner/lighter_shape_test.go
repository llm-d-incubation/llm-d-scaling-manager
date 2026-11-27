package main

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// heavierLoadState is the staircase with every decode cycle's lengths
// scaled by x: the load's cycles then ran at x times the measured shape
// priced (the last cycles' learned shape, which the scaling leaves as is).
func heavierLoadState(t *testing.T, inX, outX float64) *learnreplay.State {
	t.Helper()
	st := *stairReplay.state(t)
	st.Variants = slices.Clone(st.Variants)
	for i, v := range st.Variants {
		if domain.CanonicalRole(v.Role) == domain.RolePrefill {
			continue
		}
		s := slices.Clone(v.ShapeSeries)
		for j := range s {
			s[j].InputTokens *= inX
			s[j].OutputTokens *= outX
		}
		st.Variants[i].ShapeSeries = s
	}
	return &st
}

// A measured shape priced lighter than the load's cycles reads the knee
// high for that load: an OVER-PROVISIONED or RIGHT-SIZED verdict on it can
// cut replicas the load needs, so neither is given (#361). Four prefill and
// seven decode replicas are over-provisioned at the staircase's p95 at its
// own shape; with the load's cycles 1.5x longer in output, or in prompt,
// than the shape priced the verdict is NOT JUDGED and names --shape load.
// Three + three, right-sized at its own shape, is NOT JUDGED too. An UNDER
// keeps its verdict: a heavier load only makes it more true. The guard is
// on the measured shape asked (--shape measured): the default prices the
// load at its own cycles' shape, and judges.
func TestAtReplicas_ALighterShapePricedGivesNoShrink(t *testing.T) {
	measured := func(q whatifapi.ReportQuery) whatifapi.ReportQuery {
		q.Shapes = []whatifapi.ReportShapeAsk{{Measured: true, Label: "measured"}}
		return q
	}
	own := stairReport(t, stairQuery(4, 7, 0)).Shapes[0]
	require.Equal(t, whatifapi.VerdictOver, own.Verdict.Verdict, own.Verdict.Summary)
	require.Equal(t, whatifapi.VerdictRight, stairReport(t, stairQuery(3, 3, 0)).Shapes[0].Verdict.Verdict)
	for _, c := range []struct {
		name      string
		inX, outX float64
	}{{"output", 1, 1.5}, {"prompt", 1.5, 1}} {
		t.Run(c.name, func(t *testing.T) {
			st := heavierLoadState(t, c.inX, c.outX)
			for _, f := range [][2]int{{4, 7}, {3, 3}} {
				s := priceReport(t, st, measured(stairQuery(f[0], f[1], 0))).Shapes[0]
				require.NotNil(t, s.LoadShape)
				assert.True(t, s.LoadShape.Mismatch, "the load's cycles ran at another shape")
				v := s.Verdict
				assert.Equal(t, whatifapi.VerdictNotJudged, v.Verdict, "%d + %d: %s", f[0], f[1], v.Summary)
				assert.Contains(t, v.Summary, phasesWords, v.Summary)
				assert.Contains(t, v.Summary, "--shape load", v.Summary)
				assert.Empty(t, v.Shrink, v.Summary)
				assert.Nil(t, v.Needed, "no fleet is sized on a knee that reads high: %s", v.Summary)
				// A growth line is of the same traffic: no shrink there either.
				gq := measured(stairQuery(f[0], f[1], 0))
				gq.LoadSeenX = 1.1
				grown := priceReport(t, st, gq).Shapes[0].Growth
				require.NotEmpty(t, grown)
				for _, g := range grown {
					assert.NotContains(t, []string{whatifapi.VerdictRight, whatifapi.VerdictOver}, g.Verdict.Verdict, "%s: %s", g.Load, g.Verdict.Summary)
				}
				// Asked for no shape, the report prices the load at its own
				// cycles' shape: no mismatch, and a verdict.
				d := priceReport(t, st, stairQuery(f[0], f[1], 0)).Shapes[0].Verdict
				assert.NotEqual(t, whatifapi.VerdictNotJudged, d.Verdict, "%d + %d at the load's shape: %s", f[0], f[1], d.Summary)
				assert.NotContains(t, d.Summary, phasesWords, d.Summary)
			}
			// Under-provisioned at its own shape, and still so at a heavier
			// load.
			v := priceReport(t, st, measured(stairQuery(2, 2, 0))).Shapes[0].Verdict
			assert.Equal(t, whatifapi.VerdictUnder, v.Verdict, v.Summary)
		})
	}
	// Within the tolerance the verdict stands.
	v := priceReport(t, heavierLoadState(t, 1.1, 1.1), measured(stairQuery(4, 7, 0))).Shapes[0].Verdict
	assert.Equal(t, whatifapi.VerdictOver, v.Verdict, v.Summary)

	// Decode alone holding sizes a decode-only need on the same knee, which
	// can name fewer decode than the fleet has: none is sized on it either.
	// On the sweep, with no TTFT target, decode alone is judged. Its load's
	// cycles ran at OL 1080 and its measured shape is OL 602: asked at the
	// load's own shape decode alone holds and a need is sized; asked at the
	// measured shape, none is.
	t.Run("decode only", func(t *testing.T) {
		own := priceReport(t, sweepReplay.state(t), sweepQuery()).Shapes[0].Verdict
		require.Equal(t, whatifapi.ScopeDecodeOnly, own.Scope, own.Summary)
		require.NotNil(t, own.Needed, "decode alone holds at the load's shape: %s", own.Summary)
		s := priceReport(t, sweepReplay.state(t), measured(sweepQuery())).Shapes[0]
		require.NotNil(t, s.LoadShape)
		require.True(t, s.LoadShape.Mismatch)
		assert.Equal(t, whatifapi.VerdictNotJudged, s.Verdict.Verdict, s.Verdict.Summary)
		assert.Contains(t, s.Verdict.Summary, phasesWords, s.Verdict.Summary)
		assert.Nil(t, s.Verdict.Needed, "no decode-only need on a knee that reads high: %s", s.Verdict.Summary)
	})
}
