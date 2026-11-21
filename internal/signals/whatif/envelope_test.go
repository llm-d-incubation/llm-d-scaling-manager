package whatif

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envelopeH200 is a class inside the q32 staircase's bucket B as the run
// priced it: 2 prefill and 2 decode pods on H200, 8000-token prompts at the
// sent spread (cv 0.0577), 200 out, Poisson arrivals stated, flow control
// off.
func envelopeH200() FigureClass {
	return FigureClass{Topology: TopologyPD, Accelerator: "H200", PrefillReplicas: 2, DecodeReplicas: 2,
		PromptTokens: 8000, OutputTokens: 200, PromptCV: 0.0577, ArrivalCV2: 1, FlowControl: FlowControlNameOff,
		Model: "model-02", TensorParallel: 2, PrefillTensorParallel: 2}
}

// The shipped envelope parses, holds the staircase's rows, and validates a
// figure priced where and as the staircase measured it, printing its count
// and the kind of range.
func TestEnvelope_TheShippedFileValidatesTheMeasuredRegion(t *testing.T) {
	e := DefaultEnvelope()
	require.NotEmpty(t, e.Rows)
	for _, q := range []float64{0, 0.9, 0.95} {
		v := e.Judge(Figure{Kind: FigureTTFT, Percentile: q, Class: envelopeH200(), KneeShare: 0.8})
		require.True(t, v.Validated(), "q %g: %s", q, v)
		// Six load levels read twice: a min-max over 6, in-sample (#308).
		assert.Regexp(t, `^validated in-sample: error [+-]\d+\.\.[+-]\d+% \(min-max, n=6 load levels, \d+ readings; q32 staircase\)$`, v.String())
	}
	tpot := e.Judge(Figure{Kind: FigureMeanTPOT, Class: envelopeH200(), KneeShare: 0.1})
	assert.True(t, tpot.Validated(), tpot.String())
	// The ends are inclusive: the default headroom, 0.85, is inside a region
	// measured to 0.850.
	at := e.Judge(Figure{Kind: FigureRateAtTTFT, Percentile: 0.9, Class: envelopeH200(), KneeShare: 0.85})
	assert.True(t, at.Validated(), at.String())
}

// Every category no run measured, and every input the run did not price at,
// reads OUTSIDE VALIDATED REGION, and says which: the prompt's spread first.
func TestEnvelope_UnvalidatedCategoriesSaySo(t *testing.T) {
	e := DefaultEnvelope()
	in := Figure{Kind: FigureTTFT, Percentile: 0.9, Class: envelopeH200(), KneeShare: 0.8}
	cases := []struct {
		name string
		edit func(*Figure)
		want string
	}{
		{"the spread assumed", func(f *Figure) { f.Class.PromptCV, f.Class.PromptCVUnknown = 2, true },
			"prompt spread not measured (cv 2 assumed), measured at cv 0.0462 to 0.0721"},
		{"the spread assumed, with V_P assumed too", func(f *Figure) {
			f.Class.PromptCV, f.Class.PromptCVUnknown, f.Flags = 2, true, []Flag{AssumedVP}
		}, "prompt spread not measured"},
		{"another spread", func(f *Figure) { f.Class.PromptCV = 1 }, "prompt cv 1, measured 0.0462 to 0.0721"},
		{"the arrivals assumed", func(f *Figure) { f.Class.ArrivalsUnknown = true }, "arrivals not measured (Poisson assumed)"},
		{"flow control not known", func(f *Figure) { f.Class.FlowControl = "" }, "flow control unknown, measured off, on"},
		{"aggregated", func(f *Figure) { f.Class.Topology = TopologyAggregated }, "aggregated variants: never measured"},
		{"prefill's own latency", func(f *Figure) { f.Kind = FigurePrefillLatency }, "prefill's own latency: a client measures TTFT end to end only"},
		{"p99", func(f *Figure) { f.Percentile = 0.99 }, "p99, measured mean, p90, p95)"},
		{"more than 2 prefill pods", func(f *Figure) { f.Class.PrefillReplicas = 4 }, "4 prefill pods, measured 2"},
		{"another accelerator", func(f *Figure) { f.Class.Accelerator = "A100" }, "accelerator A100, measured H200"},
		{"an unknown accelerator", func(f *Figure) { f.Class.Accelerator = "" }, "accelerator unknown, measured H200"},
		{"past the knee share", func(f *Figure) { f.KneeShare = 0.97 }, "at 0.970 of the knee, measured 0.400 to 0.900"},
		{"under the knee share", func(f *Figure) { f.KneeShare = 0.2 }, "at 0.200 of the knee, measured 0.400 to 0.900"},
		{"no knee share", func(f *Figure) { f.KneeShare = 0 }, "rate against the knee unknown"},
		{"a longer prompt", func(f *Figure) { f.Class.PromptTokens = 32000 }, "prompt 32000 tokens, measured"},
		{"past the onset", func(f *Figure) { f.Flags = []Flag{AboveFlowControlOnset} }, "past the flow-control onset"},
		{"V_P assumed", func(f *Figure) { f.Flags = []Flag{AssumedVP} }, "V_P assumed, not learned"},
		{"a TPOT percentile", func(f *Figure) { f.Kind = FigurePercentileTPOT }, "TPOT percentiles: never scored against a client"},
		{"the operating-point cap", func(f *Figure) { f.Kind = FigureOperatingCap }, "the operating-point cap is headroom times the knee"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := in
			c.edit(&f)
			v := e.Judge(f)
			assert.False(t, v.Validated())
			assert.Contains(t, v.String(), "OUTSIDE VALIDATED REGION ("+c.want)
		})
	}
}

// A rate is judged by what bound it, as the CLI and the API both judge it.
func TestEnvelope_RateFigure(t *testing.T) {
	c := envelopeH200()
	for _, tc := range []struct {
		by   []string
		want FigureKind
	}{
		{[]string{"TTFT"}, FigureRateAtTTFT},
		{[]string{"operating-point cap", "TTFT p90, end to end"}, FigureRateAtTTFT},
		{[]string{"prefill flow-control onset"}, FigureRateAtTTFT},
		{[]string{"decode TPOT"}, FigureRateAtTPOT},
		{[]string{"operating-point cap"}, FigureOperatingCap},
		{[]string{"decode batch cap"}, FigureOperatingCap},
	} {
		f := RateFigure(3.4, tc.by, 0.9, c, 4, nil)
		assert.Equal(t, tc.want, f.Kind, "%v", tc.by)
		assert.InDelta(t, 0.85, f.KneeShare, 1e-12)
	}
	assert.Zero(t, RateFigure(3.4, []string{"TTFT"}, 0.9, c, 0, nil).KneeShare, "no knee: the share is unknown")
}

// A kind no row and no Never entry names is outside, by its name.
func TestEnvelope_AnUnknownKindIsOutside(t *testing.T) {
	v := Envelope{}.Judge(Figure{Kind: "made-up", Class: envelopeH200()})
	assert.Equal(t, "OUTSIDE VALIDATED REGION (made-up: never measured)", v.String())
}

// A row judges a figure by each bound it sets, and by none it leaves unset.
func TestEnvelope_RegionBounds(t *testing.T) {
	lo, hi := -0.05, 0.15
	row := EnvelopeRow{Figure: FigureTTFT, Shape: "s", Source: "a run", Readings: 3, ErrorLo: &lo, ErrorHi: &hi, Spread: SpreadMinMax,
		Region: Region{Topology: TopologyPD, ArrivalCV2: &Span{Lo: 0.8, Hi: 1.25}, FlowControl: []string{FlowControlNameOn}}}
	e := Envelope{Rows: []EnvelopeRow{row}}
	f := Figure{Kind: FigureTTFT, Class: FigureClass{Topology: TopologyPD, FlowControl: FlowControlNameOn}}
	v := e.Judge(f)
	require.True(t, v.Validated(), "Ca^2 0 reads as Poisson, and nothing else is bounded")
	assert.Equal(t, "validated in-sample: error -5..+15% (min-max, n=3; a run)", v.String())
	f.Class.ArrivalCV2 = 4
	assert.Equal(t, "OUTSIDE VALIDATED REGION (arrivals Ca^2 4, measured 0.8 to 1.25)", e.Judge(f).String())
	f.Class.ArrivalCV2, f.Class.FlowControl = 1.25, FlowControlNameOff
	assert.Equal(t, "OUTSIDE VALIDATED REGION (flow control off, measured on)", e.Judge(f).String(), "1.25 is inside: the ends are")
}

// Every kind of range says its count, and a bracket or a bound is never
// printed as a spread of readings.
func TestEnvelope_TheLabelSaysTheCountAndTheRange(t *testing.T) {
	x := func(v float64) *float64 { return &v }
	row := func(spread string, n int, lo, hi *float64) Validation {
		return Validation{Row: &EnvelopeRow{Source: "r", Spread: spread, Readings: n, ErrorLo: lo, ErrorHi: hi, ErrorMin: x(-0.1), ErrorMax: x(0.3)}}
	}
	assert.Equal(t, "validated in-sample: error -5..+15% (p10-p90, min-max -10..+30%, n=12; r)", row(SpreadPercentiles, 12, x(-0.05), x(0.15)).String())
	assert.Equal(t, "validated in-sample: error -5..+15% (min-max, n=3; r)", row(SpreadMinMax, 3, x(-0.05), x(0.15)).String())
	assert.Equal(t, "validated in-sample: error +5..+11% (n=1, single reading, between two ladder steps; r)", row(SpreadBracket, 1, x(0.05), x(0.11)).String())
	assert.Equal(t, "validated in-sample: error -4..+2% (n=2, between ladder steps; r)", row(SpreadBracket, 2, x(-0.04), x(0.02)).String())
	assert.Equal(t, "validated in-sample: one-sided, error at most -6% (n=1, not crossed within the steps run; r)", row(SpreadOneSided, 1, nil, x(-0.06)).String())
	// The count is the independent levels first: 12 readings at 6 levels is
	// 6 (#308). A holdout run says so.
	lv := row(SpreadMinMax, 12, x(-0.05), x(0.15))
	lv.Row.Levels = 6
	assert.Equal(t, "validated in-sample: error -5..+15% (min-max, n=6 load levels, 12 readings; r)", lv.String())
	lv.Row.Holdout = true
	assert.Equal(t, "validated on a holdout: error -5..+15% (min-max, n=6 load levels, 12 readings; r)", lv.String())
}

// A p10-p90 needs MinPercentileLevels independent readings: under it the
// 10th and 90th percentiles are the second and the next-to-last reading.
// Readings that repeat a level count once (#308).
func TestEnvelope_APercentileSpreadNeedsTenLevels(t *testing.T) {
	x := func(v float64) *float64 { return &v }
	load := func(readings, levels int) error {
		row := EnvelopeRow{Figure: FigureTTFT, Shape: "s", Source: "r", Spread: SpreadPercentiles, Readings: readings, Levels: levels,
			ErrorLo: x(-0.05), ErrorHi: x(0.15), Region: Region{Topology: TopologyPD}}
		b, err := json.Marshal(Envelope{Rows: []EnvelopeRow{row}})
		require.NoError(t, err)
		_, err = ParseEnvelope(b)
		return err
	}
	assert.NoError(t, load(10, 0))
	assert.NoError(t, load(12, 10))
	assert.ErrorContains(t, load(9, 0), "under 10 is a min-max")
	assert.ErrorContains(t, load(12, 6), "a p10-p90 over 6 independent readings: under 10 is a min-max")
}

// The error prints both sides, or the one a bounded reading gives, and
// never -0.
func TestEnvelope_ErrorText(t *testing.T) {
	x := func(v float64) *float64 { return &v }
	assert.Equal(t, "-5..+15%", ErrorText(x(-0.05), x(0.15)))
	assert.Equal(t, "+0..+14%", ErrorText(x(-0.004), x(0.14)))
	assert.Equal(t, "at most -6%", ErrorText(nil, x(-0.0555)))
	assert.Equal(t, "at least +5%", ErrorText(x(0.05), nil))
	assert.Equal(t, "unbounded", ErrorText(nil, nil))
}

// A row that judges nothing, or calls fewer than 5 readings a p10-p90, is
// refused when the file is read.
func TestEnvelope_ParseRefusesABadRow(t *testing.T) {
	for _, raw := range []string{
		`{"rows":[{"figure":"ttft","shape":"s","source":"r","region":{"topology":"pd"},"errorLo":0,"spread":"min-max","readings":0}]}`,
		`{"rows":[{"figure":"ttft","shape":"s","source":"r","region":{"topology":"pd"},"spread":"min-max","readings":2}]}`,
		`{"rows":[{"figure":"ttft","shape":"s","source":"r","region":{},"errorLo":0,"spread":"min-max","readings":2}]}`,
		`{"rows":[{"shape":"s","source":"r","region":{"topology":"pd"},"errorLo":0,"spread":"min-max","readings":2}]}`,
		`{"rows":[{"figure":"ttft","shape":"s","source":"r","region":{"topology":"pd"},"errorLo":0,"spread":"p10-p90","readings":4}]}`,
		`{"rows":[{"figure":"ttft","shape":"s","source":"r","region":{"topology":"pd"},"errorLo":0,"spread":"wide","readings":4}]}`,
		`not json`,
	} {
		_, err := ParseEnvelope([]byte(raw))
		assert.Error(t, err, raw)
	}
}

// FleetClass reads the fleet's pods, shape, spread, arrivals and flow
// control; flow control with no queue series is not known.
func TestEnvelope_FleetClass(t *testing.T) {
	fl := TTFTFleet{Prefill: Prefill{Replicas: 2, PromptTokens: 8000, PromptCV: 2, PromptCVAssumed: true, ArrivalCV2: 4},
		Decode: &Decode{Replicas: 3, OutputTokens: 200}, FlowControl: FlowControl{State: FlowControlOn}}
	assert.Equal(t, FigureClass{Topology: TopologyPD, PrefillReplicas: 2, DecodeReplicas: 3, PromptTokens: 8000,
		OutputTokens: 200, PromptCV: 2, PromptCVUnknown: true, ArrivalCV2: 4, FlowControl: FlowControlNameOn}, FleetClass(fl))
	fl.FlowControl.State, fl.Decode = FlowControlNoSeries, nil
	c := FleetClass(fl)
	assert.Empty(t, c.FlowControl, "no queue series: flow control is not known")
	assert.Zero(t, c.DecodeReplicas)
}
