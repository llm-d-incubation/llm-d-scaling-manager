package learnrec

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The replicaStartSeconds seed (learner step 5a) rides the variant's state
// with a presence bit. Both are tagged omitzero, and they are the only
// omitzero fields of the state: a state recorded before them holds exactly
// the untagged fields.
func TestStartSeed_TheSeedFieldsAreLeftOutWhileZero(t *testing.T) {
	typ := reflect.TypeFor[domain.VariantReplicaState]()
	var tagged []string
	for i := range typ.NumField() {
		if typ.Field(i).Tag.Get("learnrec") == OmitZeroTag {
			tagged = append(tagged, typ.Field(i).Name)
		}
	}
	assert.ElementsMatch(t, []string{"StartSeedSeconds", "StartSeedSet"}, tagged)
}

// A seeded state is written with its seed and its presence bit, and reads
// back; an unseeded one names neither.
func TestStartSeed_IsRecordedWithItsPresenceAndAbsentWhenUnset(t *testing.T) {
	in := Input{ModelID: "m", Namespace: "ns", VariantStates: []domain.VariantReplicaState{
		{VariantName: "seeded", CurrentReplicas: 2, StartSeedSeconds: 120, StartSeedSet: true},
		{VariantName: "plain", CurrentReplicas: 1},
	}}
	b, err := Marshal(in)
	require.NoError(t, err)

	var states []json.RawMessage
	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &obj))
	require.NoError(t, json.Unmarshal(obj["VariantStates"], &states))
	require.Len(t, states, 2)
	assert.Contains(t, string(states[0]), `"StartSeedSeconds":120,"StartSeedSet":true`)
	assert.NotContains(t, string(states[1]), "StartSeed", "an unseeded state names no seed field")

	var out Input
	require.NoError(t, Unmarshal(b, &out))
	assert.Equal(t, in, out)
	again, err := Marshal(out)
	require.NoError(t, err)
	assert.Equal(t, string(b), string(again))
}

// goldenDir is where the learner replay goldens live, every one recorded
// before the seed existed.
const goldenDir = "../../engines/analyzers/learnreplay/testdata/goldens"

// Every variant state in every golden recording reads back and is written
// again byte for byte. In those recorded before the seed, none names a seed
// field: a field added to the state without omitzero fails here on its first
// state. The start-seed goldens were recorded after it: each holds states
// that carry the seed, and those come back byte for byte too.
func TestStartSeed_APre5aRecordingIsWrittenAgainByteForByte(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(goldenDir, "*", "recording.jsonl.gz"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no golden recordings found under %s", goldenDir)

	total := 0
	seeded := map[string]int{}
	for _, f := range files {
		n := 0
		golden := filepath.Base(filepath.Dir(f))
		afterSeed := strings.HasPrefix(golden, "start-seed") || golden == "start-restored"
		eachRecordedState(t, f, func(raw json.RawMessage) {
			n++
			if !afterSeed {
				assert.NotContains(t, string(raw), "StartSeed", "%s: a pre-5a state names a seed field", f)
			}
			var st domain.VariantReplicaState
			require.NoError(t, Unmarshal(raw, &st), f)
			if st.StartSeedSet {
				seeded[golden]++
			}
			again, err := Marshal(st)
			require.NoError(t, err, f)
			require.Equal(t, string(raw), string(again), "%s: a recorded state is not written again byte for byte", f)
		})
		total += n
	}
	// Not an empty pass: the goldens hold states, and the walk read them.
	require.Greater(t, total, 100, "the walk read only %d states", total)
	for _, golden := range []string{"start-seed", "start-seed-restart", "start-seed-restart-2", "start-restored"} {
		require.Positive(t, seeded[golden], "no state of %s carries the seed", golden)
	}
	t.Logf("%d states across %d recordings, with a seed per golden: %v", total, len(files), seeded)
}

// eachRecordedState calls fn with the raw bytes of every variant state an
// Analyze event's input holds in the gzipped recording at path.
func eachRecordedState(t *testing.T, path string, fn func(json.RawMessage)) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	require.NoError(t, err)
	r := bufio.NewReader(zr)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var ev struct {
				Data json.RawMessage
			}
			require.NoError(t, json.Unmarshal(line, &ev), path)
			var data struct {
				Input *struct {
					VariantStates []json.RawMessage
				}
			}
			// Not every event's data is an object holding an input.
			if json.Unmarshal(ev.Data, &data) == nil && data.Input != nil {
				for _, s := range data.Input.VariantStates {
					fn(s)
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return
		}
		require.NoError(t, err, path)
	}
}
