package learnreplay

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
)

// slo.SwitchTTFTPullWork reaches the planner's transfer window, off unless the
// replay's options turn it on. On the public P/D staircase with flow control
// on, the decode rows' last cycles read a NIXL transfer of about 60 ms at about
// 21 r/s a decode pod: off, that loaded time is the pull's work; on, the work
// with the processor-sharing stretch at each row's rate taken out, 25-29 ms,
// what the same rows read at a light load (issue
// llm-d-incubation/llm-d-scaling-manager#301).
func TestStateAt_ThePullWorkSwitchReachesTheTransfer(t *testing.T) {
	assert.NotContains(t, PlannerSwitches()[PackageSLO], slo.SwitchTTFTPullWork, "off unless asked")
	gz, err := os.ReadFile(filepath.Join(goldenDir, "ttft-staircase-b-on", RecordingName))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	transfer := func(sw map[string]map[string]bool) float64 {
		st, err := StateAt(context.Background(), raw, Options{Switches: sw}, StateQuery{Cycle: 0})
		require.NoError(t, err)
		for _, v := range st.Variants {
			if domain.CanonicalRole(v.Role) == domain.RoleDecode {
				return v.DecodeTTFT.TransferSeconds
			}
		}
		t.Fatal("no decode variant")
		return 0
	}
	off := transfer(nil)
	on := transfer(map[string]map[string]bool{PackageSLO: {slo.SwitchTTFTPullWork: true}})
	assert.InDelta(t, 0.060, off, 0.005, "off: the loaded time")
	assert.InDelta(t, 0.027, on, 0.003, "on: the pull's work")
}
