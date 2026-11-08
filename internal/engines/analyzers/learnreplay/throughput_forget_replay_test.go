package learnreplay

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/throughput"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// The throughput analyzer's forget in a replay: the replay applies a
// recorded throughput.ForgetVariant to the replay's own analyzer, so the
// replayed line is dropped where the run's was; a recording made before the
// engine recorded the event replays as the run did, forgetting nothing.

// throughputForgetRecording is sloForgetRecording's run with the throughput
// analyzer's forget of v: a start, fourteen cycles of every recorded call,
// the forget, and three cycles more. It returns the recording and what the
// forget returned.
func throughputForgetRecording(t *testing.T) ([]byte, int) {
	t.Helper()
	const cycles = 14
	f := newFixture(readersAt)
	f.buf.Reset()
	rec := learnrec.New(f.buf, 0)
	f.attach(rec)
	start := defaultStart()
	start.SLO = &learnrec.SLOStart{StateTimeout: f.slo.StateTimeout(), SlopeHoldFor: f.slo.SlopeHoldFor(), Judging: f.slo.Judging()}
	start.Throughput = true
	rec.Record(learnrec.KindStart, readersAt, start)
	for n := range cycles {
		f.cycle(t, n)
	}
	dropped := f.tp.ForgetVariant(context.Background(), "ns", "m", "v")
	for n := cycles; n < cycles+3; n++ {
		f.cycle(t, n)
	}
	require.NoError(t, rec.Err())
	return bytes.Clone(f.buf.Bytes()), dropped
}

func TestReplay_TheThroughputForgetIsApplied(t *testing.T) {
	ctx := context.Background()
	recording, dropped := throughputForgetRecording(t)

	// The event carries the forget's arguments and what it dropped.
	events, err := readEvents(recording)
	require.NoError(t, err)
	var forgets []throughput.ForgetVariantEvent
	for _, ev := range events {
		if ev.Kind == throughput.KindForgetVariant {
			var d throughput.ForgetVariantEvent
			require.NoError(t, ev.DecodeData(&d))
			forgets = append(forgets, d)
		}
	}
	require.Positive(t, dropped, "premise: the line had readings, and the forget dropped them")
	assert.Equal(t, []throughput.ForgetVariantEvent{{Namespace: "ns", ModelID: "m", Variant: "v", Dropped: dropped}}, forgets,
		"one event, carrying what the call returned")

	// At its own commit the replay reproduces every event, the forget and
	// each Analyze after it included.
	rep, err := Replay(ctx, recording, Options{Mode: Fidelity})
	require.NoError(t, err)
	require.Empty(t, rep.Failed())
	assert.Equal(t, string(recording), string(rep.Recording))

	// Its export replays alike: the event's fields survive the pseudonyms.
	exported, _, err := Export(recording, ExportOptions{Key: []byte("k")})
	require.NoError(t, err)
	require.Contains(t, string(exported), throughput.KindForgetVariant)
	rep, err = Replay(ctx, exported, Options{Mode: Fidelity})
	require.NoError(t, err)
	require.Empty(t, rep.Failed(), "the export of a recording with a throughput forget")

	// A recording made before the engine recorded the forget has none: it
	// replays, and forgets nothing in the analyzer, as that run did.
	old := stripKind(t, recording, throughput.KindForgetVariant)
	require.NotContains(t, string(old), throughput.KindForgetVariant)
	rep, err = Replay(ctx, old, Options{Mode: Normal})
	require.NoError(t, err)
	assert.Empty(t, rep.Failed(), "a recording without the event still replays")

	// The forget is what the Analyzes after it read: held to the recording,
	// a replay that forgets nothing prices another line.
	rep, err = Replay(ctx, old, Options{Mode: Fidelity})
	require.NoError(t, err)
	var moved bool
	for _, m := range rep.Mismatches {
		if m.Kind == throughput.KindAnalyze {
			moved = true
		}
	}
	assert.True(t, moved, "premise: without the forget the replayed line differs after it; mismatches: %s",
		mismatchKinds(rep.Mismatches))
}
