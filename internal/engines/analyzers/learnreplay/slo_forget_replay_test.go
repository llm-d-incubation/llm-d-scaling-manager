package learnreplay

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// The slo analyzer's forget in a replay (#431): the replay applies a
// recorded slo.ForgetVariant to the replay's own slo analyzer, so the
// replayed line is dropped where the run's was; a recording made before
// the engine recorded the event replays as the run did, forgetting nothing.

// sloForgetRecording is a start, sloForgetCycles cycles of every recorded
// call (fixture.cycle: the slo line fits), the slo analyzer's forget of v,
// and three cycles more. It returns the recording and what the forget
// returned.
func sloForgetRecording(t *testing.T) ([]byte, int) {
	t.Helper()
	const sloForgetCycles = 14
	f := newFixture(readersAt)
	f.buf.Reset()
	rec := learnrec.New(f.buf, 0)
	f.attach(rec)
	start := defaultStart()
	start.SLO = &learnrec.SLOStart{StateTimeout: f.slo.StateTimeout(), SlopeHoldFor: f.slo.SlopeHoldFor(), Judging: f.slo.Judging()}
	start.Throughput = true
	rec.Record(learnrec.KindStart, readersAt, start)
	for n := range sloForgetCycles {
		f.cycle(t, n)
	}
	dropped := f.slo.ForgetVariant(context.Background(), "ns", "m", "v")
	for n := sloForgetCycles; n < sloForgetCycles+3; n++ {
		f.cycle(t, n)
	}
	require.NoError(t, rec.Err())
	return bytes.Clone(f.buf.Bytes()), dropped
}

// stripKind is recording without its events of kind, numbered again from 1:
// a recording as it would read had the run not recorded them.
func stripKind(t *testing.T, recording []byte, kind string) []byte {
	t.Helper()
	const prefix = `{"Seq":`
	var out []byte
	seq := 0
	for _, line := range bytes.SplitAfter(recording, []byte("\n")) {
		if len(line) == 0 || bytes.Contains(line, []byte(`"Kind":"`+kind+`"`)) {
			continue
		}
		require.True(t, bytes.HasPrefix(line, []byte(prefix)), "a line that does not start with its sequence number")
		end := bytes.IndexByte(line, ',')
		require.Positive(t, end)
		seq++
		out = append(out, fmt.Sprintf("%s%d", prefix, seq)...)
		out = append(out, line[end:]...)
	}
	return out
}

func TestReplay_TheSLOForgetIsApplied(t *testing.T) {
	ctx := context.Background()
	recording, dropped := sloForgetRecording(t)

	// The event carries the forget's arguments and what it dropped.
	events, err := readEvents(recording)
	require.NoError(t, err)
	var forgets []slo.ForgetVariantEvent
	for _, ev := range events {
		if ev.Kind == slo.KindForgetVariant {
			var d slo.ForgetVariantEvent
			require.NoError(t, ev.DecodeData(&d))
			forgets = append(forgets, d)
		}
	}
	require.Positive(t, dropped, "premise: the slo line had fitted, and the forget dropped it")
	assert.Equal(t, []slo.ForgetVariantEvent{{Namespace: "ns", ModelID: "m", Variant: "v", Dropped: dropped}}, forgets,
		"one event, carrying what the call returned")

	// At its own commit the replay reproduces every event, the forget and
	// each slo Analyze after it included.
	rep, err := Replay(ctx, recording, Options{Mode: Fidelity})
	require.NoError(t, err)
	require.Empty(t, rep.Failed())
	assert.Equal(t, string(recording), string(rep.Recording))

	// Its export replays alike: the event's fields survive the pseudonyms.
	exported, _, err := Export(recording, ExportOptions{Key: []byte("k")})
	require.NoError(t, err)
	require.Contains(t, string(exported), slo.KindForgetVariant)
	rep, err = Replay(ctx, exported, Options{Mode: Fidelity})
	require.NoError(t, err)
	require.Empty(t, rep.Failed(), "the export of a recording with an slo forget")

	// A recording made before the engine recorded the slo forget has none:
	// it replays, and forgets nothing in the slo analyzer, as that run did.
	old := stripKind(t, recording, slo.KindForgetVariant)
	require.NotContains(t, string(old), slo.KindForgetVariant)
	rep, err = Replay(ctx, old, Options{Mode: Normal})
	require.NoError(t, err)
	assert.Empty(t, rep.Failed(), "a recording without the event still replays")

	// The forget is what the slo Analyzes after it read: held to the
	// recording, a replay that forgets nothing publishes another line.
	rep, err = Replay(ctx, old, Options{Mode: Fidelity})
	require.NoError(t, err)
	var sloMoved bool
	for _, m := range rep.Mismatches {
		if m.Kind == slo.KindAnalyze {
			sloMoved = true
		}
	}
	assert.True(t, sloMoved, "premise: without the forget the replayed slo line differs after it; mismatches: %s",
		mismatchKinds(rep.Mismatches))
}

func mismatchKinds(ms []Mismatch) string {
	kinds := make([]string, 0, len(ms))
	for _, m := range ms {
		kinds = append(kinds, m.Kind)
	}
	return strings.Join(kinds, ",")
}
