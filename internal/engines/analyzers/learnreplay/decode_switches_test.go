package learnreplay

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
)

// Each of the three decode-term switches, on alone, sets only its own
// State field, at the cycle answered at and at every earlier cycle a
// self-check reads in the same replay; off, none is set.
func TestDecodeSwitches_EachSetsOnlyItsOwnField(t *testing.T) {
	src := BytesSource(readGoldenRecording(t, "ttft-staircase-b-on"))
	for _, c := range []struct {
		name            string
		sw              map[string]bool
		censored, resid bool
		burst           bool
	}{
		{name: "off", sw: map[string]bool{}},
		{name: "censored", sw: map[string]bool{saturation.SwitchTPOTRatioCensored: true}, censored: true},
		{name: "residence", sw: map[string]bool{saturation.SwitchDecodeResidencePull: true}, resid: true},
		{name: "burst", sw: map[string]bool{saturation.SwitchDecodeBurstFill: true}, burst: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			opts := Options{Switches: map[string]map[string]bool{PackageSaturation: c.sw}}
			last, early, err := StatesFrom(context.Background(), src, opts, StateQuery{}, func(n int) []int { return []int{n / 2} })
			require.NoError(t, err)
			require.Len(t, early, 1)
			for _, st := range []*State{last, early[last.Cycles/2]} {
				require.NotNil(t, st)
				assert.Equal(t, c.censored, st.TPOTRatioCensored, "cycle %d", st.Cycle)
				assert.Equal(t, c.resid, st.DecodeResidencePull, "cycle %d", st.Cycle)
				assert.Equal(t, c.burst, st.DecodeBurstFill, "cycle %d", st.Cycle)
			}
		})
	}
}
