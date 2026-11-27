package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --experimental-ttft=v2nl-line sets v2 and the variant where the flag takes
// it (backtest, serve, at-replicas), and is refused where it does not;
// every other value clears the variant. =v2nl, the attention variant, is
// not a value.
func TestExperimentalTTFTValue_V2NLLine(t *testing.T) {
	var sw, v2, line bool
	v := experimentalTTFTValue{switches: &sw, v2: &v2, nlLine: &line}
	require.NoError(t, v.Set(experimentalTTFTV2NLLine))
	assert.True(t, v2)
	assert.True(t, line)
	assert.False(t, sw)
	assert.Equal(t, experimentalTTFTV2NLLine, v.String())
	require.NoError(t, v.Set(experimentalTTFTV2))
	assert.True(t, v2)
	assert.False(t, line)
	assert.Equal(t, experimentalTTFTV2, v.String())
	require.NoError(t, v.Set(experimentalTTFTV2NLLine))
	require.NoError(t, v.Set("false"))
	assert.False(t, v2)
	assert.False(t, line)
	require.Error(t, v.Set("v2nl"))
	// Without the variant's field the value is refused, and v2 is left as
	// it was.
	var sw2, v22 bool
	w := experimentalTTFTValue{switches: &sw2, v2: &v22}
	require.Error(t, w.Set(experimentalTTFTV2NLLine))
	assert.False(t, v22)
}
