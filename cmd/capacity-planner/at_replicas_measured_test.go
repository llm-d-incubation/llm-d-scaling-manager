package main

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// at-replicas reads each cycle's measured latency in the series pass it
// already makes, so that it withholds an over the router measured missing,
// as POST /v1/report does; the other commands do not keep it.
func TestAtReplicas_ReadsTheMeasuredLatency(t *testing.T) {
	f := answerFlags{recording: stairBOn, cycle: "last", set: map[string]bool{}, warn: io.Discard, withLatency: true}
	_, err := f.readState(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, f.series)
	require.Len(t, f.latency, len(f.series), "latency aligned with the series")
	routed := 0
	for _, l := range f.latency {
		if l.Router != nil {
			routed++
		}
	}
	require.Positive(t, routed, "the staircase recorded the router's histograms")
	// The staircase's busy cycles ran 2 + 2 and missed a 190 ms p95, as the
	// day plan's golden measures them.
	m, ok := whatifapi.MeasuredTTFTAtLoad(f.series, f.latency, whatifapi.FleetCounts{Prefill: 2, Decode: 2}, whatifapi.LoadP95, 40, 0.95)
	require.True(t, ok)
	assert.Greater(t, m, 0.19)

	plain := answerFlags{recording: stairBOn, cycle: "last", set: map[string]bool{}, warn: io.Discard}
	_, err = plain.readState(context.Background())
	require.NoError(t, err)
	assert.Nil(t, plain.series)
	assert.Nil(t, plain.latency)
}
