package steadystate

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/throughput"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
)

// recRestartInside is a restart inside sightingsMaxAge of the first
// controller's last write, 6 min after recStart.
var recRestartInside = recStart.Add(7 * time.Minute)

// restoreEvents is the settle.Restore events of a recording.
func restoreEvents(t *testing.T, recording []byte) []settle.RestoreEvent {
	t.Helper()
	events, err := readEvents(recording)
	require.NoError(t, err)
	var out []settle.RestoreEvent
	for _, ev := range events {
		if ev.Kind != settle.KindRestore {
			continue
		}
		var d settle.RestoreEvent
		require.NoError(t, ev.DecodeData(&d))
		out = append(out, d)
	}
	return out
}

// An export of a recording holding a real settle.Restore leaves pseudonyms
// only: every name in the event is one the export made, none is one the
// script gave, and the export replays.
func TestLearnerRecording_ARestoreEventExportsPseudonymsOnly(t *testing.T) {
	recording, _, _ := recordRunRestartingAt(t, recStart, recRestartInside, true)
	raw := restoreEvents(t, recording)
	require.Len(t, raw, 1, "a restart inside the bound restores the sightings")
	require.NotEmpty(t, raw[0].Models)

	exported, _, err := learnreplay.Export(recording, learnreplay.ExportOptions{Key: []byte("a key for this test only")})
	require.NoError(t, err)
	got := restoreEvents(t, exported)
	require.Len(t, got, 1, "the export kept the event")
	require.Len(t, got[0].Models, len(raw[0].Models))

	names := map[string]*regexp.Regexp{
		"ns": regexp.MustCompile(`^ns-\d+$`), "model": regexp.MustCompile(`^model-\d+$`),
		"pod": regexp.MustCompile(`^pod-\d+$`), "variant": regexp.MustCompile(`^variant-\d+$`),
	}
	sightings := 0
	for i, m := range got[0].Models {
		assert.Regexp(t, names["ns"], m.Namespace)
		assert.Regexp(t, names["model"], m.ModelID)
		assert.NotEqual(t, raw[0].Models[i].Namespace, m.Namespace)
		assert.NotEqual(t, raw[0].Models[i].ModelID, m.ModelID)
		for j, s := range m.Sightings {
			sightings++
			assert.Regexp(t, names["pod"], s.PodName)
			assert.Regexp(t, names["variant"], s.VariantName)
			assert.NotEqual(t, raw[0].Models[i].Sightings[j].PodName, s.PodName)
			assert.NotEqual(t, raw[0].Models[i].Sightings[j].VariantName, s.VariantName)
			assert.True(t, raw[0].Models[i].Sightings[j].ReadySince.Equal(s.ReadySince), "times are figures, and stay")
		}
	}
	require.Positive(t, sightings, "the event names no replica: nothing was checked")

	text, err := json.Marshal(got[0])
	require.NoError(t, err)
	for _, name := range scriptNames() {
		assert.NotContains(t, string(text), name)
	}
	replayed, err := learnreplay.Replay(context.Background(), exported,
		learnreplay.Options{Mode: learnreplay.Fidelity, Engine: NewLearnerReplay()})
	require.NoError(t, err)
	require.Empty(t, mismatchText(replayed.Mismatches), "the export does not replay")
}

// What a restart inside the bound moves in the scripted run. The lines start
// fitting sooner: a time shift of about two minutes, cut short by the
// fixture's 25 h clock jump, not a rise in anything priced. So the counts are
// of variants priced before the jump, and the ratio means nothing. Only the
// restart's distance from the last write differs between the two runs.
func TestLearnerRecording_ARestartInsideTheBoundPricesSooner(t *testing.T) {
	priced := func(recording []byte) map[string]int {
		events, err := readEvents(recording)
		require.NoError(t, err)
		out := map[string]int{}
		for _, ev := range events {
			if ev.Kind != slo.KindAnalyze && ev.Kind != throughput.KindAnalyze {
				continue
			}
			var d struct {
				Result struct {
					VariantCapacities []struct{ PerReplicaCapacity float64 }
				}
			}
			require.NoError(t, json.Unmarshal(ev.Data, &d))
			for _, vc := range d.Result.VariantCapacities {
				if vc.PerReplicaCapacity > 0 {
					out[ev.Kind]++
				}
			}
		}
		return out
	}
	inside, _, _ := recordRunRestartingAt(t, recStart, recRestartInside, true)
	past, _, _ := recordRunRestartingAt(t, recStart, recStart.Add(9*time.Minute), true)
	require.Len(t, restoreEvents(t, inside), 1)
	require.Empty(t, restoreEvents(t, past), "3 min after the last write is past the bound")
	on, off := priced(inside), priced(past)
	t.Logf("variants priced: inside the bound %v, past it %v", on, off)
	assert.Greater(t, on[slo.KindAnalyze], off[slo.KindAnalyze])
	assert.Greater(t, on[throughput.KindAnalyze], off[throughput.KindAnalyze])
}
