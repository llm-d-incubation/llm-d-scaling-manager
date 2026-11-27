package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// SwitchTTFTPullWork: off from the constructor, the transfer as measured; on,
// S1's window reads each decode row's pull work at the row's request rate, so
// the processor-sharing pull priced at that rate reads the measured transfer
// back, not stretched a second time (issue
// llm-d-incubation/llm-d-scaling-manager#301).
func TestTTFT_ThePullWorkSwitch(t *testing.T) {
	assert.False(t, New(nil, nil).ttftPullWork)
	assert.Contains(t, ReplaySwitchNames(), SwitchTTFTPullWork)

	const prompt, key, rate, measured = 8000.0, "ns|model|dec", 20.8, 0.060
	rows := func() []domain.ReplicaMetrics {
		dec := transferRowsAt(3, measured)
		for i := range dec {
			dec[i].RequestRate, dec[i].RequestRateKnown = rate, true
		}
		return dec
	}
	read := func(on bool) float64 {
		r := newTTFTRig(ttftEstimate(), nil)
		require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchTTFTPullWork: on}))
		r.a.noteDecodeTTFT(key, whatif.DecodeTTFT{}, ttftNow)
		dec := rows()
		return r.a.transferPerToken(r.a.noteTransfer(key, "", "", dec, ttftNow), dec, prompt) * prompt
	}
	assert.InDelta(t, measured, read(false), 1e-12, "off, the time as measured")
	work := read(true)
	assert.InDelta(t, measured/(1+rate*measured), work, 1e-12)
	assert.InDelta(t, measured, work/(1-rate*work), 1e-12, "priced at the rate it was read at, the time measured")
}
