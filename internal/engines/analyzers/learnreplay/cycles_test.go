package learnreplay

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// cyclesRecording is a committed golden's recording, gunzipped.
func cyclesRecording(t *testing.T) []byte {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join("testdata", "goldens", "ttft-open-loop-long", "recording.jsonl.gz"))
	require.NoError(t, err)
	z, err := gzip.NewReader(bytes.NewReader(gz))
	require.NoError(t, err)
	data, err := io.ReadAll(z)
	require.NoError(t, err)
	return data
}

// readAll is the recording src opens.
func readAll(t *testing.T, src Source) []byte {
	t.Helper()
	rc, err := src()
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	return data
}

// Kept whole, a recording reads back byte for byte; kept every other cycle,
// it reads as a recording (its sequence numbers run from 1), its series is
// those cycles' samples and no other, and the start line leads it.
func TestCyclesOf_KeepsWholeCyclesAndRenumbers(t *testing.T) {
	data := cyclesRecording(t)
	all, err := SeriesFrom(BytesSource(data), StateQuery{}, SeriesVariants{})
	require.NoError(t, err)
	require.Greater(t, len(all), 20, "the golden holds cycles to keep")

	same := readAll(t, CyclesOf(BytesSource(data), func([]time.Time) bool { return true }))
	assert.Equal(t, string(data), string(same), "a recording kept whole changed")

	keep := map[int64]bool{}
	var want []time.Time
	for i, c := range all {
		if i%2 == 1 {
			keep[c.At.UnixNano()] = true
			want = append(want, c.At)
		}
	}
	asked := 0
	half := readAll(t, CyclesOf(BytesSource(data), func(at []time.Time) bool {
		asked++
		require.NotEmpty(t, at, "a cycle asked with no Analyze")
		return keep[at[0].UnixNano()]
	}))
	rec, err := learnrec.Read(half)
	require.NoError(t, err, "the kept cycles do not read as a recording")
	require.NotEmpty(t, rec.Events)
	assert.Equal(t, learnrec.KindStart, rec.Events[0].Kind)
	assert.Equal(t, len(all), asked, "keep is asked once a cycle")
	got, err := SeriesFrom(BytesSource(half), StateQuery{}, SeriesVariants{})
	require.NoError(t, err)
	at := make([]time.Time, len(got))
	for i, c := range got {
		at[i] = c.At
	}
	assert.Equal(t, want, at, "the series is not the kept cycles'")
	begins, analyses := 0, 0
	for _, ev := range rec.Events {
		switch ev.Kind {
		case slo.KindBeginCycle:
			begins++
		case saturation.KindAnalyze:
			analyses++
		}
	}
	assert.Equal(t, len(want), analyses)
	assert.GreaterOrEqual(t, begins, len(want), "a kept cycle lost its BeginCycle")
	assert.Less(t, begins, len(all), "a dropped cycle kept its BeginCycle")

	none := readAll(t, CyclesOf(BytesSource(data), func([]time.Time) bool { return false }))
	rec, err = learnrec.Read(none)
	require.NoError(t, err)
	for _, ev := range rec.Events {
		assert.NotEqual(t, saturation.KindAnalyze, ev.Kind, "a cycle kept that keep dropped")
	}
	assert.Equal(t, learnrec.KindStart, rec.Events[0].Kind, "the start line is always kept")
}

// A replay of the kept cycles reaches its last cycle: the state is read from
// those cycles alone.
func TestCyclesOf_ReplaysTheKeptCycles(t *testing.T) {
	data := cyclesRecording(t)
	all, err := SeriesFrom(BytesSource(data), StateQuery{}, SeriesVariants{})
	require.NoError(t, err)
	first := all[:len(all)/2]
	last := first[len(first)-1].At
	src := CyclesOf(BytesSource(data), func(at []time.Time) bool { return !at[0].After(last) })
	st, err := StateFrom(t.Context(), src, Options{}, StateQuery{})
	require.NoError(t, err)
	assert.Equal(t, len(first), st.Cycles, "the replay read cycles keep dropped")
	assert.Equal(t, last, st.At)
}
