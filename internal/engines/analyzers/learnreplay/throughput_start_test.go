package learnreplay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// A replay builds the throughput analyzer as the run built it: what its
// builders set, from the start line, with the replay's own settle tracker
// and store exactly where the run handed it the engine's. A recording from
// before the analyzer had builders states none, and the analyzer is built
// with none, as it ran.
func TestReplay_BuildsTheThroughputAnalyzerAsTheRunDid(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *learnrec.ThroughputStart
		want learnrec.ThroughputStart
	}{
		{name: "an older recording, no config", cfg: nil, want: learnrec.ThroughputStart{}},
		{
			name: "the engine's analyzer",
			cfg: &learnrec.ThroughputStart{SlopeHoldFor: 7 * 24 * time.Hour, OptimizationInterval: 15 * time.Second,
				SettleView: true, CapacityStore: true},
			want: learnrec.ThroughputStart{SlopeHoldFor: 7 * 24 * time.Hour, OptimizationInterval: 15 * time.Second,
				SettleView: true, CapacityStore: true},
		},
		{
			name: "no view and no store",
			cfg:  &learnrec.ThroughputStart{SlopeHoldFor: time.Hour},
			want: learnrec.ThroughputStart{SlopeHoldFor: time.Hour},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := defaultStart()
			start.Throughput, start.ThroughputConfig = true, tc.cfg
			r, err := build(start, replayAt, Options{}, nil)
			require.NoError(t, err)
			require.NotNil(t, r.tp)
			assert.Equal(t, tc.want, r.tp.StartConfig())
		})
	}
}
