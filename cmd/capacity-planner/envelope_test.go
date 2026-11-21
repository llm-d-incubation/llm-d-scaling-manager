package main

import (
	"flag"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// Every flag of the two questions is classified for the envelope, once: a
// flag the run's pricing allows (flagsInside), or one that puts every figure
// outside (flagsOutside). A new flag fails here until it is decided.
func TestEnvelope_EveryFlagIsClassified(t *testing.T) {
	fs := flag.NewFlagSet("q", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var f answerFlags
	f.register(fs)
	// The questions' own flags (replicas.go, rate.go).
	fs.Float64("rate", 0, "")
	fs.String("tpot-percentile", "", "")
	fs.String("tpot", "", "")
	n := 0
	fs.VisitAll(func(fl *flag.Flag) {
		n++
		in, out := slices.Contains(flagsInside, fl.Name), slices.Contains(flagsOutside, fl.Name)
		assert.True(t, in != out, "--%s: inside %v, outside %v; put it in exactly one of flagsInside and flagsOutside", fl.Name, in, out)
	})
	assert.Equal(t, len(flagsInside)+len(flagsOutside), n, "a listed flag that no longer exists")
}

// A flag that states an input the run never priced puts the figure outside,
// whatever else matches, and says which flag.
func TestEnvelope_AStatedInputIsOutside(t *testing.T) {
	f := answerFlags{set: map[string]bool{"batch-cap": true, "assume-router-hop": true, "decode": true}}
	assert.Equal(t, []string{"--batch-cap", "--assume-router-hop"}, f.statedOutside())
	c := q32ClassB()
	in := whatif.Figure{Kind: whatif.FigureTTFT, Percentile: 0.9, Class: c, KneeShare: 0.8}
	require.True(t, whatif.DefaultEnvelope().Judge(in).Validated(), "precondition: bucket B's class is inside")
	in.Class.Stated = f.statedOutside()
	assert.Equal(t, "OUTSIDE VALIDATED REGION (--batch-cap stated, which the run did not price)",
		whatif.DefaultEnvelope().Judge(in).String())
}

// q32ClassB is bucket B's class as the run priced it, on the committed
// recording: what an API or a CLI question priced the same way reads.
func q32ClassB() whatif.FigureClass {
	return whatif.FigureClass{Topology: whatif.TopologyPD, Accelerator: "H200", PrefillReplicas: 2, DecodeReplicas: 2,
		PromptTokens: 8000, OutputTokens: 200, PromptCV: 0.0577, ArrivalCV2: 1, FlowControl: whatif.FlowControlNameOff,
		Model: "model-02", TensorParallel: 2, PrefillTensorParallel: 2}
}

// Each of round 2's four holes reads outside: a knee with flow control on
// or not known (the run's on phases failed 5% to 25% before it), another
// tensor parallelism or model, an assumed router hop.
func TestEnvelope_TheRoundTwoHolesAreOutside(t *testing.T) {
	env := whatif.DefaultEnvelope()
	knee := whatif.Figure{Kind: whatif.FigurePrefillKnee, Class: q32ClassB()}
	require.True(t, env.Judge(knee).Validated(), env.Judge(knee).String())
	for _, c := range []struct {
		name string
		edit func(*whatif.Figure)
		want string
	}{
		{"knee, flow control on", func(f *whatif.Figure) { f.Class.FlowControl = whatif.FlowControlNameOn }, "flow control on, measured off"},
		{"knee, flow control unknown", func(f *whatif.Figure) { f.Class.FlowControl = "" }, "flow control unknown, measured off"},
		{"knee, TP 4", func(f *whatif.Figure) { f.Class.TensorParallel = 4 }, "decode tensor parallelism 4, measured 2"},
		{"knee, prefill TP 4", func(f *whatif.Figure) { f.Class.PrefillTensorParallel = 4 }, "prefill tensor parallelism 4, measured 2"},
		{"knee, another model", func(f *whatif.Figure) { f.Class.Model = "Qwen/Qwen3-8B" }, "model Qwen/Qwen3-8B, measured Qwen/Qwen3-32B, model-02"},
		{"knee, prefix hits", func(f *whatif.Figure) { f.Class.PrefixHitRate = 0.4 }, "prefix hit rate 0.4, measured 0 to 0.05"},
		{"TTFT, router hop assumed", func(f *whatif.Figure) {
			f.Kind, f.Percentile, f.KneeShare, f.Flags = whatif.FigureTTFT, 0.9, 0.8, []whatif.Flag{whatif.RouterHopAssumed}
		}, "router hop assumed, not learned"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := knee
			c.edit(&f)
			assert.Equal(t, "OUTSIDE VALIDATED REGION ("+c.want+")", env.Judge(f).String())
		})
	}
	// The public name is inside as the pseudonym is.
	public := knee
	public.Class.Model = q32Model
	assert.True(t, env.Judge(public).Validated())
}

// The q32 question as the run priced it, with a batch cap typed in: the TTFT
// it moves reads outside, naming the flag (it read validated before the
// stated inputs were classified).
func TestEnvelope_ABatchCapPutsTheQ32TTFTOutside(t *testing.T) {
	text, code := runCmd(t, q32TTFT("replicas-to-slo", "--ttft-percentile", "90", "--assume-prompt-cv", "0.0577",
		"--arrival-cv2", "1", "--flow-control", "off", "--batch-cap", "8")...)
	require.Equal(t, 0, code, text)
	var ttft string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "TTFT p90") {
			ttft = line
		}
	}
	require.NotEmpty(t, ttft, text)
	assert.Contains(t, ttft, "OUTSIDE VALIDATED REGION (--batch-cap stated, which the run did not price)")
	assert.NotRegexp(t, `validated[^:]*: error`, text)
}
