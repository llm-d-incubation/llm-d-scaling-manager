package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// stairGolden is a staircase golden's recording.
func stairGolden(phase string) string {
	return "../../internal/engines/analyzers/learnreplay/testdata/goldens/ttft-staircase-" + phase + "/recording.jsonl.gz"
}

// stairFloorArgs price a 2 s TTFT with V_P and a stated, so the floor is
// what the decode side sets.
var stairFloorArgs = []string{"--ttft", "2s", "--tpot", "100ms", "--assume-vp", "200000", "--assume-a", "20ms", "-o", "json"}

// stairPrefill is the prefill role of slo-to-rate on a staircase phase at a
// cycle ("" the last).
func stairPrefill(t *testing.T, phase, cycle string) wireRole {
	t.Helper()
	args := append([]string{"slo-to-rate", "--recording", stairGolden(phase)}, stairFloorArgs...)
	if cycle != "" {
		args = append(args, "--cycle", cycle)
	}
	out, _ := runCmd(t, args...)
	for _, r := range decodeOutput(t, out).Roles {
		if r.Role == "prefill" {
			require.NotNil(t, r.TTFT, out)
			require.NotNil(t, r.Results.Rate, out)
			return r
		}
	}
	t.Fatalf("no prefill role:\n%s", out)
	return wireRole{}
}

// b-off and c-on end in a collapse: decode queued hundreds of requests and
// its engine TTFT read 16.6 s and 9.0 s. Their decode line is refused
// (PinnedDefaultB), so the decode side is that measured TTFT, and held at
// every rate it set the zero-load floor at 16.6 s and 9.1 s, where the client
// read 0.12 to 0.18 s at the lowest step. At the last cycle the planner
// refuses it and names the latest calm cycle. Before the collapse, and at the
// cycle it names, the decode side is calm and the floor is that side plus
// the prefill's own few tens of ms. At c-on's cycle 240 the line is pinned
// through the floor below the band (saturation.SwitchITLBaselineFromFloor),
// so it prices and the decode side is the line's, not the engine's TTFT.
func TestDecodeTTFTUnderLoad_StaircaseCollapse(t *testing.T) {
	for _, c := range []struct {
		phase   string
		waiting string
		calm    int
		// lineAt240 says the decode line prices at cycle 240.
		lineAt240 bool
	}{
		{"b-off", "decode TTFT 16.6 s was measured with 283 requests waiting on decode capacity", 249, false},
		{"c-on", "decode TTFT 9.0 s was measured with 254 requests waiting on decode capacity", 249, true},
	} {
		t.Run(c.phase, func(t *testing.T) {
			last := stairPrefill(t, c.phase, "")
			require.NotNil(t, last.Results.Rate.Refused, "the last cycle is answered")
			assert.Equal(t, string(whatif.DecodeTTFTUnderLoad), last.Results.Rate.Refused.Reason)
			assert.Contains(t, last.Results.Rate.Refused.Detail, c.waiting)
			assert.Contains(t, last.Results.Rate.Refused.Detail, "answer at a calmer cycle: --cycle "+strconv.Itoa(c.calm))
			assert.Equal(t, c.calm, last.TTFT.DecodeTTFTCalmCycle)
			assert.Equal(t, "decode engine", last.TTFT.DecodeSide)

			for _, cycle := range []string{"240", strconv.Itoa(c.calm)} {
				r := stairPrefill(t, c.phase, cycle)
				require.Nil(t, r.Results.Rate.Refused, "cycle %s", cycle)
				require.NotNil(t, r.Results.Rate.Answer, "cycle %s", cycle)
				assert.Zero(t, r.TTFT.DecodeTTFTCalmCycle, "cycle %s", cycle)
				decode, floor := r.TTFT.DecodeTTFTSeconds, r.Results.Rate.Answer.Basis.Floor
				if c.lineAt240 && cycle == "240" {
					assert.Equal(t, "line", r.TTFT.DecodeSide, "cycle %s", cycle)
					assert.Zero(t, decode, "cycle %s: the engine's TTFT is not read", cycle)
					assert.Positive(t, floor, "cycle %s: the line prices a floor", cycle)
					assert.Less(t, floor, 0.3, "cycle %s: the floor is calm", cycle)
					continue
				}
				assert.Less(t, decode, 0.3, "cycle %s: the decode engine TTFT is calm", cycle)
				assert.Less(t, floor-decode, 0.1, "cycle %s: the rest of the floor is prefill's", cycle)
			}
		})
	}
}

// b-on held its last step: its decode line prices, the decode engine's
// TTFT is never read, and nothing is refused or named.
func TestDecodeTTFTUnderLoad_ALinePricedFleetIsNotRead(t *testing.T) {
	r := stairPrefill(t, "b-on", "")
	require.NotNil(t, r.Results.Rate.Answer)
	assert.Equal(t, "line", r.TTFT.DecodeSide)
	assert.Zero(t, r.TTFT.DecodeTTFTSeconds)
	assert.Zero(t, r.TTFT.DecodeTTFTCalmCycle)
}

// The negative control: the same fleet with the backlog taken off prices the
// collapse's queue as the floor, 9 s and more, and a 2 s target is refused as
// below it. The backlog is what tells the two apart.
func TestDecodeTTFTUnderLoad_WithoutTheBacklog(t *testing.T) {
	for _, phase := range []string{"b-off", "c-on"} {
		f := answerFlags{recording: stairGolden(phase), cycle: "last", window: captureFlags{},
			assumeVP: 200000, assumeA: 20 * time.Millisecond, set: map[string]bool{"assume-vp": true, "assume-a": true}}
		st, err := f.readState(context.Background())
		require.NoError(t, err)
		variants, err := f.decodeVariants(st)
		require.NoError(t, err)
		require.Len(t, variants, 1)
		ps, err := f.prefillState(st)
		require.NoError(t, err)
		pf := f.fleetFor(st, ps, ttftQuestion{q: 0.9}, &variants[0])
		require.Nil(t, pf.refused, phase)
		target := whatif.TTFTTarget{Seconds: 2, Percentile: 0.9}

		with := whatif.RateForTTFT(pf.fleet, target)
		require.IsType(t, whatif.Unanswerable{}, with, phase)
		assert.Equal(t, whatif.DecodeTTFTUnderLoad, with.(whatif.Unanswerable).Reason, phase)

		clear := pf.fleet
		clear.DecodeTTFT.Waiting = 0
		without := whatif.RateForTTFT(clear, target)
		require.IsType(t, whatif.Unanswerable{}, without, phase)
		assert.Equal(t, whatif.TargetBelowFloor, without.(whatif.Unanswerable).Reason, phase)
		assert.Greater(t, clear.DecodeTTFT.Seconds, 9.0, phase)
	}
}

// The hint is in the words of the source's own setting: the CLI's flag, or
// the API's source field; a Prometheus window is ended earlier, since it has
// no cycle to set. A freeze names it as the CLI does.
func TestDecodeTTFTUnderLoad_TheHintFollowsTheSource(t *testing.T) {
	at := time.Date(2026, 11, 6, 2, 6, 3, 0, time.UTC)
	refusal := whatif.Unanswerable{Reason: whatif.DecodeTTFTUnderLoad, Detail: "decode TTFT 9.0 s was measured with 136 requests waiting on decode capacity"}
	for _, c := range []struct {
		hint hintSource
		want string
	}{
		{hintCLIRecording, "; answer at a calmer cycle: --cycle 249, the latest with no decode queue"},
		{hintCLIPrometheus, "; end the window before decode queued: --to 2026-11-06T02:06:03Z, the latest cycle with no decode queue"},
		{hintAPIRecording, "; answer at a calmer cycle: source.cycle 249, the latest with no decode queue"},
		{hintAPIPrometheus, "; end the window before decode queued: source.to 2026-11-06T02:06:03Z, the latest cycle with no decode queue"},
	} {
		pf := &pricedFleet{hint: c.hint, pv: &pricedVariant{state: learnreplay.VariantState{
			DecodeTTFT: learnreplay.DecodeTTFTState{CalmCycle: 249, CalmAt: at}}}}
		got := pf.calmCycleHint(refusal).(whatif.Unanswerable)
		assert.Equal(t, refusal.Detail+c.want, got.Detail)
		if c.hint == hintCLIPrometheus || c.hint == hintAPIPrometheus {
			assert.Zero(t, pf.in.DecodeTTFTCalmCycle, "a window's cycle is no address")
			assert.Equal(t, "2026-11-06T02:06:03Z", pf.in.DecodeTTFTCalmAt)
		} else {
			assert.Equal(t, 249, pf.in.DecodeTTFTCalmCycle)
			assert.Empty(t, pf.in.DecodeTTFTCalmAt)
		}
	}

	none := &pricedFleet{pv: &pricedVariant{}}
	got := none.calmCycleHint(refusal).(whatif.Unanswerable)
	assert.Equal(t, refusal.Detail+"; no cycle of the source read decode without a queue", got.Detail)
	assert.Zero(t, none.in.DecodeTTFTCalmCycle)

	other := whatif.Unanswerable{Reason: whatif.InvalidInput, Detail: "x"}
	pf := &pricedFleet{pv: &pricedVariant{state: learnreplay.VariantState{DecodeTTFT: learnreplay.DecodeTTFTState{CalmCycle: 249}}}}
	assert.Equal(t, other, pf.calmCycleHint(other), "another reason is left as it is")
	assert.Zero(t, pf.in.DecodeTTFTCalmCycle, "and names no cycle")
}

// Through the API a recording source names the cycle by its own field: the
// server passes the source's kind down to the fleet it prices.
func TestDecodeTTFTUnderLoad_TheAPINamesSourceCycle(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile(stairGolden("b-off"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b-off.jsonl.gz"), b, 0o600))
	s := &servedSources{recordings: dir}
	res, err := s.Resolve(context.Background(), whatifapi.InputsRequest{
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: "b-off.jsonl.gz"}})
	require.NoError(t, err)
	vp, a := 200000.0, 0.02
	fl, err := res.Pricer.Fleet(whatifapi.Knobs{Headroom: 0.85, Assume: whatifapi.Assume{VP: &vp, A: &a}})
	require.NoError(t, err)
	got, ok := fl.TTFTAt(1).(whatif.Unanswerable)
	require.True(t, ok, "%#v", fl.TTFTAt(1))
	assert.Equal(t, whatif.DecodeTTFTUnderLoad, got.Reason)
	assert.Contains(t, got.Detail, "; answer at a calmer cycle: source.cycle 249, the latest with no decode queue")
	assert.NotContains(t, got.Detail, "--cycle")
}

// Where the decode engine does not say why requests wait, a backlog cannot
// be read: the figure is priced, not refused, and the answer carries
// DecodeQueueUnread for a client to act on.
func TestDecodeTTFTUnderLoad_AnUnreadQueueIsFlagged(t *testing.T) {
	st := workedState(t)
	f := answerFlags{set: map[string]bool{"assume-vp": true}, assumeVP: 30000}
	variants, err := f.decodeVariants(st)
	require.NoError(t, err)
	ps, err := f.prefillState(st)
	require.NoError(t, err)
	noLine := variants[0]
	noLine.decode.Line.A, noLine.decode.Line.B = 0, 0
	noLine.state.DecodeTTFT.EngineTTFT = whatif.DecodeTTFT{Seconds: 0.03, PromptTokens: 1000}
	for _, unread := range []bool{false, true} {
		noLine.state.DecodeTTFT.BacklogUnread = unread
		pf := f.fleetFor(st, ps, ttftQuestion{q: 0.9}, &noLine)
		got := pf.priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, 1, 0.9) })
		require.IsType(t, whatif.TTFT{}, got)
		assert.Equal(t, unread, slices.Contains(got.(whatif.TTFT).Flags, decodeQueueUnread), "unread %v", unread)
	}
}
