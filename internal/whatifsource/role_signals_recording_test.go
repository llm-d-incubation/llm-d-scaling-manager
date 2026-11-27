package whatifsource

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// recordingRoles reads a recording's analyzer inputs: every row, and each
// variant's recorded role.
func recordingRoles(t *testing.T, path string) ([]domain.ReplicaMetrics, map[string]string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	read, err := learnrec.Read(gunzip(t, raw))
	require.NoError(t, err)
	var rows []domain.ReplicaMetrics
	roles := map[string]string{}
	for _, ev := range read.Events {
		if ev.Kind != saturation.KindAnalyze {
			continue
		}
		var a saturation.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&a))
		var c decodedCycle
		require.NoError(t, a.Input.Decode(&c.Input))
		rows = append(rows, c.Input.ReplicaMetrics...)
		for _, vs := range c.Input.VariantStates {
			roles[vs.VariantName] = domain.CanonicalRole(vs.Role)
		}
	}
	return rows, roles
}

// TestRoleFromSignals_LabRecordingsWithTheirLabelsStripped reads lab
// recordings of a P/D fleet with their role labels stripped: each variant
// takes from its engine signals the role it was labelled with.
func TestRoleFromSignals_LabRecordingsWithTheirLabelsStripped(t *testing.T) {
	dir := filepath.Join("..", "engines", "analyzers", "learnreplay", "testdata", "goldens")
	for _, name := range []string{"pd-restart", "ttft-three-pods"} {
		t.Run(name, func(t *testing.T) {
			rows, labelled := recordingRoles(t, filepath.Join(dir, name, "recording.jsonl.gz"))
			require.NotEmpty(t, rows)
			require.Contains(t, slices.Collect(maps.Values(labelled)), domain.RolePrefill, "a P/D recording")
			require.Contains(t, slices.Collect(maps.Values(labelled)), domain.RoleDecode, "a P/D recording")
			readings := domain.RoleFromSignals(rows).Read(nil)
			for _, v := range slices.Sorted(maps.Keys(labelled)) {
				role, note := domain.ResolveRole(v, "", false, readings[v])
				assert.Equal(t, labelled[v], role, "variant %s stripped of its label: %s", v, note)
				_, disagree := domain.ResolveRole(v, labelled[v], true, readings[v])
				assert.Empty(t, disagree, "variant %s with its label", v)
			}
		})
	}
}
