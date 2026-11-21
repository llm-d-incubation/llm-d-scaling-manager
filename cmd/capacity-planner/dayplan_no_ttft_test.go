package main

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// day-plan checks its question before the replay, which takes minutes on a
// day's recording: a flag asked wrongly is refused at once, naming the flag,
// even where the recording named does not exist.
func TestDayPlanChecksFlagsBeforeTheReplay(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"size-for":  {[]string{"--size-for", "p50"}, "sizeFor"},
		"bucket":    {[]string{"--bucket", "1s"}, "step"},
		"statistic": {[]string{"--ttft", "2s@p100"}, "slo.ttft.statistic"},
		"headroom":  {[]string{"--headroom", "1.5"}, "headroom"},
		"bound":     {[]string{"--max-decode", "-1"}, "maxReplicas.decode"},
	} {
		args := append([]string{"day-plan", "--recording", "no-such-dir/no-such.jsonl.gz"}, c.args...)
		err := run(context.Background(), args, io.Discard, io.Discard)
		require.Error(t, err, name)
		assert.Equal(t, 2, exitCode(err), "%s: a usage error: %v", name, err)
		assert.Contains(t, err.Error(), c.want, name)
		assert.NotContains(t, err.Error(), "no-such", "%s: refused before the recording is read", name)
	}
}

// With no TTFT target, day-plan sizes decode alone, as at-replicas judges
// decode alone, rather than refusing the plan after the replay. --ttft cannot
// ask none, so the case runs on the sample recording, whose policy sets none.
func TestDayPlanNoTTFTTargetSizesDecode(t *testing.T) {
	p, code := dayJSON(t, "--recording", olsSweep, "--model", "model-02", "--assume-vp", "30000", "--tpot", "4ms")
	require.Nil(t, p.TTFT, "the sample's policy sets no TTFT target")
	assert.Empty(t, p.Refused, "nothing asked is refused")
	assert.Equal(t, 0, code)
	var sized int
	for _, b := range p.Buckets {
		if b.Needed == nil {
			continue
		}
		sized++
		assert.Nil(t, b.Needed.Prefill, "%s: prefill is not sized", b.Start)
		assert.True(t, b.Needed.AtLeast, "%s: decode alone is a lower bound", b.Start)
	}
	assert.Positive(t, sized)
}
