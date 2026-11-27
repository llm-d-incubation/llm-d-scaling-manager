package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// at-replicas asked for no load judges the window at its p95 less counting
// noise, not at its p95 (#295): the queueing model prices the one-minute
// rate's counting noise already.
func TestAtReplicas_DefaultLoadIsTheP95LessCountingNoise(t *testing.T) {
	l := stairReport(t, stairQuery(2, 2, 0)).Load
	assert.Equal(t, whatifapi.LoadP95Modulation, l.Statistic)
	require.NotNil(t, l.Seen)
	require.NotNil(t, l.P95Modulation)
	require.NotNil(t, l.P95)
	assert.Equal(t, *l.P95Modulation, *l.Seen)
	assert.Less(t, *l.Seen, *l.P95)
}
