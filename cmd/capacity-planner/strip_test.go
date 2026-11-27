package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// strip-latency takes every latency key out, at any depth, drops the
// events fitted from latency, renumbers the events after them, and keeps
// everything else.
func TestStripLatency(t *testing.T) {
	in := strings.Join([]string{
		`{"Seq":1,"Kind":"start","Data":{"Commit":"x"}}`,
		`{"Seq":2,"Kind":"saturation.Analyze","Data":{"Result":{"x":1},"Input":{"ArrivalRate":3.5,"RouterLatency":{"WindowSeconds":300,"TTFTBuckets":[{"UpperBound":1,"Rate":1}],` +
			`"ProcessingBuckets":[{"UpperBound":0.05,"Rate":1}],"ProcessingSumRate":0.04,"ProcessingCountRate":1},` +
			`"ReplicaMetrics":[{"PodName":"p","AvgITL":0.01,"AvgITLKnown":true,"AvgInputTokens":1000,"TTFTBuckets":[{"UpperBound":1}],` +
			`"GroupEngines":[{"AvgITL":0.02,"KvUsageMean":0.5}]}]}}}`,
		`{"Seq":3,"Kind":"saturation.publishLine","Data":{"A":0.001,"B":0.006}}`,
		`{"Seq":4,"Kind":"settle.Observe","Data":{"AvgTTFT":1.5,"Rate":12345678901234567890}}`,
	}, "\n")
	var out bytes.Buffer
	st, err := stripLatency(strings.NewReader(in+"\n"+`{"Seq":5,"Kind":"saturation.An`), &out, false)
	require.NoError(t, err)
	assert.Equal(t, stripStats{kept: 2, dropped: 2, torn: true}, st)
	got := out.String()
	for _, gone := range []string{"AvgITL", "TTFTBuckets", "AvgTTFT", "Processing", "publishLine", `"B":0.006`, "settle.Observe", "Result"} {
		assert.NotContains(t, got, gone)
	}
	for _, kept := range []string{`"ArrivalRate":3.5`, `"AvgInputTokens":1000`, `"KvUsageMean":0.5`, `"Commit":"x"`,
		`"RouterLatency":{"WindowSeconds":300}`} {
		assert.Contains(t, got, kept)
	}
	assert.Contains(t, got, `"Kind":"saturation.Analyze","Seq":2`, "the events are numbered from 1")

	// The router prefix producer's cached tokens are no latency: they stay,
	// with or without a router TTFT to keep the window for.
	prefix := `{"Seq":1,"Kind":"start","Data":{"Commit":"x"}}` + "\n" +
		`{"Seq":2,"Kind":"saturation.Analyze","Data":{"Input":{"RouterLatency":{"WindowSeconds":300,` +
		`"PrefixPredictedTokenRate":9335,"PrefixBestAvailableTokenRate":10000,"TPOTSumRate":0.1}}}}`
	out.Reset()
	_, err = stripLatency(strings.NewReader(prefix), &out, false)
	require.NoError(t, err)
	assert.Contains(t, out.String(), `"RouterLatency":{"PrefixBestAvailableTokenRate":10000,"PrefixPredictedTokenRate":9335}`)

	// The router's per-endpoint load and its prefix producers' counts are
	// no latency either: they stay.
	endpoints := `{"Seq":1,"Kind":"start","Data":{"Commit":"x"}}` + "\n" +
		`{"Seq":2,"Kind":"saturation.Analyze","Data":{"Input":{"RouterLatency":{"WindowSeconds":300,"TPOTSumRate":0.1,` +
		`"Endpoints":[{"Pod":"d-0","PlacementRate":5,"PlacementKnown":true}],` +
		`"PrefixProducers":[{"Plugin":"approx","Role":"prefill","RequestRate":3}]}}}}`
	out.Reset()
	_, err = stripLatency(strings.NewReader(endpoints), &out, false)
	require.NoError(t, err)
	assert.Contains(t, out.String(), `"RouterLatency":{"Endpoints":[{"PlacementKnown":true,"PlacementRate":5,"Pod":"d-0"}],`+
		`"PrefixProducers":[{"Plugin":"approx","RequestRate":3,"Role":"prefill"}]}`)

	_, err = stripLatency(strings.NewReader("{not json\n{}"), &bytes.Buffer{}, false)
	require.Error(t, err, "a line that is not JSON, and not the last, is an error")
	assert.Equal(t, 2, exitCode(err))
}
