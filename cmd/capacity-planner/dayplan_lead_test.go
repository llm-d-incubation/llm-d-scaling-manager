package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --lead is one duration for both roles, or role=duration pairs; anything
// else is a usage error.
func TestLeadFlag(t *testing.T) {
	l, err := leadOf("")
	require.NoError(t, err)
	assert.Nil(t, l)
	l, err = leadOf("90s")
	require.NoError(t, err)
	assert.Equal(t, 90.0, *l.Prefill)
	assert.Equal(t, 90.0, *l.Decode)
	l, err = leadOf("prefill=3m,decode=90s")
	require.NoError(t, err)
	assert.Equal(t, 180.0, *l.Prefill)
	assert.Equal(t, 90.0, *l.Decode)
	l, err = leadOf("decode=0s")
	require.NoError(t, err)
	assert.Nil(t, l.Prefill)
	assert.Equal(t, 0.0, *l.Decode)
	for _, bad := range []string{"90", "-1s", "prefill=1m,prefill=2m", "both=1m", "decode="} {
		_, err := leadOf(bad)
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "--lead", bad)
	}
}
