package main

import (
	"flag"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// --price-router-wait is the experimental whatif.TTFTFleet.PriceRouterWait:
// off by default, at the SLO analyzer's default; on with the flag; and off
// on the first-come model whatever the flag says.
func TestTTFT_PriceRouterWaitFlag(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{{nil, whatif.DefaultPriceRouterWait}, {[]string{"--price-router-wait"}, true}, {[]string{"--price-router-wait=false"}, false}} {
		var f answerFlags
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		f.register(fs)
		require.NoError(t, fs.Parse(c.args))
		assert.Equal(t, c.want, f.priceRouterWait, "%v", c.args)
	}
	require.False(t, whatif.DefaultPriceRouterWait, "experimental: off by default")

	pf := fleetOf(t, learnedState(), answerFlags{})
	require.Nil(t, pf.refused)
	assert.True(t, pf.fleet.ProcessorSharing)
	assert.False(t, pf.fleet.PriceRouterWait, "the default")
	pf = fleetOf(t, learnedState(), answerFlags{priceRouterWait: true})
	assert.True(t, pf.fleet.PriceRouterWait, "with the flag")
	firstCome(t)
	pf = fleetOf(t, learnedState(), answerFlags{priceRouterWait: true})
	assert.False(t, pf.fleet.ProcessorSharing)
	assert.False(t, pf.fleet.PriceRouterWait, "first come prices no router wait")
}
