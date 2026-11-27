package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// --experimental-tpot names the distribution: =v2 the one lognormal, =v2-mix
// the per-length mixture, unset none; any other value is refused, naming
// both.
func TestExperimentalTPOTOf_NamesTheDistribution(t *testing.T) {
	for in, want := range map[string]string{"": "", "v2": whatif.TPOTRenewalLognormal, "v2-mix": whatif.TPOTPerLengthMixture} {
		got, err := experimentalTPOTOf(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := experimentalTPOTOf("v3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--experimental-tpot=v2")
	assert.Contains(t, err.Error(), "--experimental-tpot=v2-mix")
}
