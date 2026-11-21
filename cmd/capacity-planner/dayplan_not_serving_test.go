package main

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// Beside each hour's had, the day plan says the replicas asked for and not
// serving (#317): on the b-on staircase with V_P and a assumed, an hour
// with 1 decode asked and not serving reads "2(+1)" under had D, and a line
// under the table says what (+n) is; with none, the table is as it was.
func TestDayPlanHadSaysWhatWasAskedAndNotServing(t *testing.T) {
	args := slices.Concat([]string{"day-plan", "--recording", stair("ttft-staircase-b-on")}, dayStairAssumed)
	plain, code := runCmd(t, args...)
	require.Equal(t, 0, code, plain)
	require.NotContains(t, plain, "(+")
	require.NotContains(t, plain, "asked for and not serving")

	cached := planDay
	planDay = func(ctx context.Context, res *whatifapi.Resolved, req whatifapi.DayPlanRequest, schema string) (*whatifapi.DayPlanResponse, error) {
		p, err := cached(ctx, res, req, schema)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		var own whatifapi.DayPlanResponse
		if err := json.Unmarshal(b, &own); err != nil {
			return nil, err
		}
		own.Hours[0].NotServing = &whatifapi.FleetCounts{Decode: 1}
		return &own, nil
	}
	t.Cleanup(func() { planDay = cached })
	out, code := runCmd(t, args...)
	require.Equal(t, 0, code, out)
	p, _ := dayJSON(t, args[1:]...)
	require.NotNil(t, p.Hours[0].Actual)
	row := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, p.Hours[0].Start.Format("2006-01-02 15:04")) {
			row = l
		}
	}
	require.NotEmpty(t, row, out)
	assert.Contains(t, row, " "+strconv.Itoa(p.Hours[0].Actual.Decode)+"(+1) ", "had D, with 1 asked and not serving")
	assert.Contains(t, out, "had (+n): n more replicas were asked for and not serving")
	assert.Equal(t, 2, strings.Count(out, "(+"), "the hour and the line under the table; the other hours had none")
}

// On a real recording, the P/D restart run, decode scales up twice: at
// 12:25 two replicas start beside the two serving (4 serve from 12:30), at
// 12:45 one beside three (4 from 12:50). Those buckets say so; the hours
// read their bucket of the fewest serving (12:10 and 13:05, one decode
// each, none starting), so neither hour pairs the fewest of one bucket with
// the starting replicas of another: 12:00 reads 1, not 1(+2).
func TestDayPlanNotServingOnAScaleUp(t *testing.T) {
	p, _ := dayJSON(t, "--recording", stairDir+"pd-restart/recording.jsonl.gz")
	got := map[string][2]whatifapi.FleetCounts{}
	for _, b := range p.Buckets {
		if b.NotServing != nil {
			require.NotNil(t, b.Actual)
			got[b.Start.Format("15:04")] = [2]whatifapi.FleetCounts{*b.Actual, *b.NotServing}
		}
	}
	assert.Equal(t, map[string][2]whatifapi.FleetCounts{
		"12:25": {{Prefill: 1, Decode: 2}, {Decode: 2}},
		"12:45": {{Prefill: 2, Decode: 3}, {Decode: 1}},
	}, got)
	require.Len(t, p.Hours, 2)
	for _, h := range p.Hours {
		assert.Equal(t, &whatifapi.FleetCounts{Prefill: 1, Decode: 1}, h.Actual, h.Start)
		assert.Nil(t, h.NotServing, "%s: its bucket of the fewest serving had none starting", h.Start)
	}
}
