package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The Qwen3-32B staircase (tensor parallelism 2, 2 prefill and 2 decode pods
// on H200, buckets A, B and C, flow control off and on, one repeat) as the
// planner's calibration (docs/guides/capacity-planner/accuracy.md):
// every step's model figure beside what the client measured, frozen in
// testdata/staircase-q32-validation.golden, and the envelope rows they draw
// frozen in internal/signals/whatif/envelope.json. A change to the model, the
// replay or the freeze that moves an error fails here, and -update rewrites
// both; the PR names the move in its goldens section.
//
// What is committed, under testdata/staircase-q32:
//   - warmup.jsonl.gz: the run's warm-up recording, the freeze's input,
//     exported (learnergolden export: names pseudonymised). The six phases'
//     own recordings are not: no figure here reads them.
//   - frozen.json: the run's frozen file, as written before its first step.
//   - client-steps.csv: the client's figures for every step, from the run's
//     per-step analysis; void is the run report's reason a step is void
//     (noise: under the noise floor; failed: the router evicted requests).
// The spec is hack/benchmark/staircase_spec_qwen32b.yaml, by its hash.

const (
	q32Dir       = "testdata/staircase-q32"
	q32Spec      = "../../hack/benchmark/staircase_spec_qwen32b.yaml"
	q32Table     = "testdata/staircase-q32-validation.golden"
	q32Envelope  = "../../internal/signals/whatif/envelope.json"
	q32Source    = "q32 staircase"
	q32SpecSHA   = "d8ba7c0881161d17160fb14983856e08a930ef5d5640678af836a1ed1b517955"
	q32Recording = q32Dir + "/warmup.jsonl.gz"
	// q32Tolerance is how far from the lengths and the Ca^2 sent a row's
	// region reaches, as a factor either way: a stated tolerance, not a
	// measured one.
	q32Tolerance = 1.25
	// q32HeldShare is the run report's rule for a held step: completions
	// within 3% of what was due, and nothing failed.
	q32HeldShare = 0.97
)

// q32Statistics are the TTFT statistics the client measured: the mean, p90
// and p95.
var q32Statistics = []float64{0, 0.9, 0.95}

// q32Step is one step's client figures.
type q32Step struct {
	bucket, fc                         string
	step                               int
	rate                               float64
	due, completed, failed             int
	ttft                               [3]float64
	tpotMean                           float64
	void                               string
	windowSeconds, ca2Sent, promptMean float64
}

// held is the run report's rule for a step the fleet held.
func (s q32Step) held() bool {
	return s.failed == 0 && float64(s.completed) >= q32HeldShare*float64(s.due)
}

// readQ32Steps reads client-steps.csv.
func readQ32Steps(t *testing.T) []q32Step {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(q32Dir, "client-steps.csv"))
	require.NoError(t, err)
	rows, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	require.NoError(t, err)
	require.Greater(t, len(rows), 1)
	num := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		require.NoError(t, err, s)
		return v
	}
	whole := func(s string) int {
		v, err := strconv.Atoi(s)
		require.NoError(t, err, s)
		return v
	}
	out := make([]q32Step, 0, len(rows)-1)
	for _, r := range rows[1:] {
		require.Len(t, r, 15)
		out = append(out, q32Step{bucket: r[0], fc: r[1], step: whole(r[2]), rate: num(r[3]), windowSeconds: num(r[4]),
			due: whole(r[5]), completed: whole(r[6]), failed: whole(r[7]), ca2Sent: num(r[8]), promptMean: num(r[9]),
			ttft: [3]float64{num(r[10]), num(r[11]), num(r[12])}, tpotMean: num(r[13]), void: r[14]})
	}
	return out
}

// q32Bucket is one bucket as the planner prices it at this commit.
type q32Bucket struct {
	spec  freezeBucket
	now   frozenBucket
	run   frozenBucket
	off   whatif.TTFTFleet
	class whatif.FigureClass
	// flags are the CLI's flags on the bucket's figures (q32Buckets).
	flags  []whatif.Flag
	priced bool
}

// fleet is the bucket's fleet in a phase's flow-control state.
func (b q32Bucket) fleet(fc string) whatif.TTFTFleet {
	if fc == whatif.FlowControlNameOff {
		return b.off
	}
	on := b.off
	on.FlowControl = whatif.FlowControl{State: whatif.FlowControlOn,
		QueueThreshold: whatif.FlowControlQueueThreshold, KVThreshold: whatif.FlowControlKVThreshold}
	return on
}

// runPoint is the run's ladder rate nearest a step's, and its frozen figure
// there at the i-th of q32Statistics; both 0 where the run froze none.
func (b q32Bucket) runPoint(rate float64, i int) (float64, float64) {
	for _, p := range b.run.Curve {
		if math.Abs(p.Rate-rate) < 0.01 && p.Refused == "" {
			return p.Rate, [3]float64{p.Mean, p.P90, p.P95}[i]
		}
	}
	return 0, 0
}

// q32State is the warm-up replayed to its last cycle, once: the replay is
// most of the test's time.
var q32State = sync.OnceValues(func() (*learnreplay.State, error) {
	a := answerFlags{question: "freeze", recording: q32Recording, cycle: "last"}
	st, err := a.readState(context.Background())
	if err != nil {
		return nil, err
	}
	ctrllog.SetLogger(logr.Discard())
	// As runFreeze: the driver's prompts share no prefix.
	for i := range st.Variants {
		st.Variants[i].Shape.PrefixHitRate = 0
	}
	return st, nil
})

// q32Buckets prices every bucket of the spec at this commit, beside the run's
// frozen file.
func q32Buckets(t *testing.T) map[string]q32Bucket {
	t.Helper()
	spec, sum, err := readFreezeSpec(q32Spec)
	require.NoError(t, err)
	require.Equal(t, q32SpecSHA, sum, "the spec is the run's, by its hash")
	raw, err := os.ReadFile(filepath.Join(q32Dir, "frozen.json"))
	require.NoError(t, err)
	var run frozenFile
	require.NoError(t, json.Unmarshal(raw, &run))
	require.Equal(t, q32SpecSHA, run.SpecSHA256)
	st, err := q32State()
	require.NoError(t, err)
	accelerator, decodeTP, prefillTP := "", 0, 0
	for _, vs := range st.Variants {
		if vs.Accelerator != "" {
			accelerator = vs.Accelerator
		}
		if domain.CanonicalRole(vs.Role) == domain.RolePrefill {
			prefillTP = vs.TensorParallel
		} else {
			decodeTP = vs.TensorParallel
		}
	}
	require.NotEmpty(t, accelerator, "the recording names its accelerator")
	require.Positive(t, decodeTP, "the recording names decode's tensor parallelism")
	require.Positive(t, prefillTP, "the recording names prefill's tensor parallelism")
	out := map[string]q32Bucket{}
	for _, b := range spec.Buckets {
		f := freezeFlags{ca2: ca2Overrides{}}
		now, err := f.priceBucket(st, spec, b, nil)
		require.NoError(t, err)
		require.Empty(t, now.Refused, "bucket %s", b.Name)
		_, off, pf, err := f.bucketFleet(st, spec, b)
		priced := pf != nil
		require.NoError(t, err)
		qb := q32Bucket{spec: b, now: now, off: off, priced: priced}
		for _, r := range run.Buckets {
			if r.Name == b.Name {
				qb.run = r
			}
		}
		require.Equal(t, b.Name, qb.run.Name, "the run froze bucket %s", b.Name)
		qb.class = whatif.FleetClass(off)
		qb.class.Accelerator, qb.class.Model = accelerator, st.ModelID
		qb.class.TensorParallel, qb.class.PrefillTensorParallel = decodeTP, prefillTP
		// The flags the CLI's own figures carry here: the fleet's, settled
		// as the freeze settles them, and the decode variant's.
		qb.flags = slices.Clone(now.Flags)
		a := answerFlags{question: "freeze", cycle: "last", decode: b.Decode, prefill: b.Prefill,
			inputTokens: b.Prompt, outputTokens: b.Output, seed: 1,
			set: map[string]bool{"assume-input-tokens": true, "assume-output-tokens": true}}
		variants, err := a.decodeVariants(st)
		require.NoError(t, err)
		for _, pv := range variants {
			qb.flags = append(qb.flags, pv.flags...)
			if pv.assumed() {
				qb.flags = append(qb.flags, whatif.AssumedShape)
			}
		}
		out[b.Name] = qb
	}
	return out
}

// q32Reading is one signed error, model over measured less 1: a point, or a
// bracket where the measured figure is a range, with nil an open end.
type q32Reading struct {
	lo, hi *float64
	// share is the knee share the reading was taken at, and target the
	// predicted figure a bracket read (a knee, an onset, a rate).
	target float64
	share  float64
	fc     string
}

func fptr(x float64) *float64 { return &x }

// q32Group is the readings of one envelope row.
type q32Group struct {
	kind      whatif.FigureKind
	bucket    string
	q         float64
	readings  []q32Reading
	brackets  bool
	shareless bool
}

// q32Analysis is the table's lines and the envelope rows they draw.
type q32Analysis struct {
	lines []string
	rows  []whatif.EnvelopeRow
	// allowed is the flags a validated figure may carry.
	allowed []whatif.Flag
}

// percentileOf is the q-th quantile of sorted xs, linear between ranks.
func percentileOf(sorted []float64, q float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	i := int(math.Floor(pos))
	if i >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	return sorted[i] + (pos-float64(i))*(sorted[i+1]-sorted[i])
}

// pctText is a signed error in percent, "n/a" where it is unbounded.
func pctText(x *float64) string {
	if x == nil {
		return "open"
	}
	r := math.Round(1000**x) / 10
	if r == 0 {
		r = 0 // no "-0.0"
	}
	return fmt.Sprintf("%+.1f%%", r)
}

// relErr is a over b less 1.
func relErr(a, b float64) float64 { return a/b - 1 }

// analyzeQ32 prices every step and reads every error.
func analyzeQ32(t *testing.T) q32Analysis {
	t.Helper()
	buckets := q32Buckets(t)
	steps := readQ32Steps(t)
	var a q32Analysis
	add := func(format string, args ...any) { a.lines = append(a.lines, fmt.Sprintf(format, args...)) }
	add("# The Qwen3-32B staircase: tensor parallelism 2, 2 prefill and 2 decode pods, buckets A (1000 tokens in,")
	add("# 1000 out, Poisson), B (8000 in, 200 out, Poisson) and C (8000 in, 200 out, Ca^2 4), flow control off and on.")
	add("# The model is the planner at this commit on the run's warm-up, frozen as the run froze it; the client is what")
	add("# the run measured. Error is the model over the client, less 1: + reads slow (a TTFT, a TPOT) or promises")
	add("# more than was held (a rate, a knee). go test ./cmd/capacity-planner -run TestStaircaseQ32 -update rewrites it.")
	// allowed is every flag the run's own figures carry, as the CLI prices
	// them: what a validated figure may carry (whatif.Envelope.AllowedFlags).
	// The run stated its spread and its arrivals, which the CLI flags.
	allowed := map[whatif.Flag]bool{whatif.AssumedPromptSpread: true, assumedArrivalCV2: true, assumedPoisson: true}
	allow := func(fl ...whatif.Flag) {
		for _, f := range fl {
			allowed[f] = true
		}
	}
	for _, b := range buckets {
		allow(b.flags...)
		if k, ok := whatif.PrefillKnee(b.off.Prefill).(whatif.Answer); ok {
			allow(k.Flags...)
		}
	}
	groups := map[string]*q32Group{}
	group := func(kind whatif.FigureKind, bucket string, q float64) *q32Group {
		k := fmt.Sprintf("%s|%s|%g", kind, bucket, q)
		if groups[k] == nil {
			groups[k] = &q32Group{kind: kind, bucket: bucket, q: q}
		}
		return groups[k]
	}

	// The knees, frozen beside the run's.
	for _, name := range slices.Sorted(maps.Keys(buckets)) {
		b := buckets[name]
		add("bucket %s: knee %.3f r/s (prefill %.3f, decode %.3f; run %.3f), prefill onset %.3f r/s, mean TPOT at the operating rate %.2f ms (run %.2f ms)",
			name, b.now.Knee, b.now.PrefillKnee, b.now.DecodeKnee, b.run.Knee, b.now.Onsets.Prefill, 1000*b.now.MeanTPOT, 1000*b.run.MeanTPOT)
	}

	// Every step: TTFT at three statistics and the mean TPOT.
	for _, s := range steps {
		b := buckets[s.bucket]
		fl := b.fleet(s.fc)
		share := s.rate / b.now.Knee
		var parts []string
		status := "used"
		for i, q := range q32Statistics {
			res := whatif.TTFTAt(fl, s.rate, q)
			v, ok := res.(whatif.TTFT)
			if !ok {
				parts = append(parts, fmt.Sprintf("%s refused (%s)", statText(q), refusalOf(res)))
				continue
			}
			if v.HasFlag(whatif.AboveFlowControlOnset) && s.void == "" {
				status = "past the onset"
			}
			// The step's rate is the driver's, to 3 decimals; the run froze the
			// ladder's. At the ladder's rate this commit's figure is the run's,
			// or the line says what it froze.
			model := fmt.Sprintf("%.1f", 1000*v.Seconds)
			if rate, run := b.runPoint(s.rate, i); rate > 0 {
				if now, ok := whatif.TTFTAt(b.off, rate, q).(whatif.TTFT); !ok || fmt.Sprintf("%.4f", 1000*now.Seconds) != fmt.Sprintf("%.4f", 1000*run) {
					model += fmt.Sprintf(" (the run froze %.4f)", 1000*run)
				}
			}
			client := s.ttft[i]
			if math.IsInf(client, 1) || client <= 0 {
				parts = append(parts, fmt.Sprintf("%s %s ms / unmeasured", statText(q), model))
				continue
			}
			e := relErr(v.Seconds, client)
			parts = append(parts, fmt.Sprintf("%s %s / %.1f ms %s", statText(q), model, 1000*client, pctText(&e)))
			if s.void == "" && !v.HasFlag(whatif.AboveFlowControlOnset) {
				allow(v.Flags...)
				g := group(whatif.FigureTTFT, s.bucket, q)
				g.readings = append(g.readings, q32Reading{lo: fptr(e), hi: fptr(e), share: share, fc: s.fc})
			}
		}
		if b.off.Decode != nil {
			if m, ok := whatif.SLOAtReplicas(*b.off.Decode, whatif.ReplicasQuery{Rate: s.rate}).MeanTPOT.(whatif.Answer); ok {
				e := relErr(m.Value, s.tpotMean)
				parts = append(parts, fmt.Sprintf("mean TPOT %.2f / %.2f ms %s", 1000*m.Value, 1000*s.tpotMean, pctText(&e)))
				if s.void == "" {
					allow(m.Flags...)
					g := group(whatif.FigureMeanTPOT, s.bucket, 0)
					g.readings = append(g.readings, q32Reading{lo: fptr(e), hi: fptr(e), share: s.rate / b.now.DecodeKnee, fc: s.fc})
				}
			}
		}
		if s.void != "" {
			status = "void: " + s.void
		}
		add("step %s %s %d: %.3f r/s, %.2f of the knee: %s; %s", s.bucket, s.fc, s.step, s.rate, share, strings.Join(parts, "; "), status)
	}

	// The knee and the onset: the last step held and the first not. A step
	// counts as held by its completions, void or not: the noise floor voids
	// a step's TTFT percentiles, not its count. With flow control off the
	// fleet runs to the knee; with it on, the router stops it at its prefill
	// onset, so a crossing there reads the onset, not the knee.
	for _, phase := range q32Phases(steps) {
		b := buckets[phase[0].bucket]
		var lastHeld, firstNot *q32Step
		for i := range phase {
			if phase[i].held() && firstNot == nil {
				lastHeld = &phase[i]
			} else if firstNot == nil {
				firstNot = &phase[i]
			}
		}
		kind, predicted, what := whatif.FigurePrefillKnee, b.now.Knee, "knee"
		if b.now.Knee != b.now.PrefillKnee {
			kind = whatif.FigureDecodeKnee
		}
		if phase[0].fc == whatif.FlowControlNameOn {
			kind, predicted, what = whatif.FigureFlowControlOnset, b.now.Onsets.Prefill, "onset"
		}
		head := fmt.Sprintf("%s %s %s: predicted %.3f r/s", what, phase[0].bucket, phase[0].fc, predicted)
		switch {
		case lastHeld == nil:
			add("%s, no step held", head)
		case firstNot == nil && kind == whatif.FigureFlowControlOnset:
			add("%s, held to %.3f, every step held: the ladder stopped under it", head, lastHeld.rate)
		case firstNot == nil:
			// The hold rule cannot see a fleet short of its rate by less
			// than the 3% it allows: the knee may lie that far under the
			// last step held.
			hi := relErr(predicted, q32HeldShare*lastHeld.rate)
			add("%s, held to %.3f, every step held: not exceeded within the steps run, error at most %s", head, lastHeld.rate, pctText(&hi))
			g := group(kind, phase[0].bucket, 0)
			g.brackets, g.shareless = true, true
			g.readings = append(g.readings, q32Reading{hi: fptr(hi), fc: phase[0].fc, target: predicted})
		default:
			lo, hi := relErr(predicted, firstNot.rate), relErr(predicted, lastHeld.rate)
			add("%s, held to %.3f, first not held %.3f: error %s to %s", head, lastHeld.rate, firstNot.rate, pctText(&lo), pctText(&hi))
			g := group(kind, phase[0].bucket, 0)
			g.brackets, g.shareless = true, true
			g.readings = append(g.readings, q32Reading{lo: fptr(lo), hi: fptr(hi), fc: phase[0].fc, target: predicted})
		}
	}

	// The rate at each frozen target: where the client's figure crossed it.
	for _, phase := range q32Phases(steps) {
		b := buckets[phase[0].bucket]
		for _, target := range b.now.Targets {
			q := target.Percentile / 100
			i := slices.Index(q32Statistics, q)
			predicted := target.PredictedOff
			if phase[0].fc == whatif.FlowControlNameOn {
				predicted = target.PredictedOn
			}
			var below, above *q32Step
			for j := range phase {
				s := &phase[j]
				if s.void != "" || above != nil {
					continue
				}
				if s.ttft[i] > target.Seconds {
					above = s
				} else {
					below = s
				}
			}
			head := fmt.Sprintf("rate %s %s %s %.1f ms: predicted %.3f r/s, %.3f of the knee", phase[0].bucket, phase[0].fc,
				statText(q), 1000*target.Seconds, predicted, predicted/b.now.Knee)
			r := q32Reading{share: predicted / b.now.Knee, fc: phase[0].fc, target: predicted}
			switch {
			case below == nil && above == nil:
				add("%s, no valid step", head)
				continue
			case above == nil:
				r.hi = fptr(relErr(predicted, below.rate))
				add("%s, measured at least %.3f: error at most %s", head, below.rate, pctText(r.hi))
			case below == nil:
				r.lo = fptr(relErr(predicted, above.rate))
				add("%s, measured under %.3f: error at least %s", head, above.rate, pctText(r.lo))
			default:
				r.lo, r.hi = fptr(relErr(predicted, above.rate)), fptr(relErr(predicted, below.rate))
				add("%s, measured %.3f to %.3f: error %s to %s", head, below.rate, above.rate, pctText(r.lo), pctText(r.hi))
			}
			g := group(whatif.FigureRateAtTTFT, phase[0].bucket, q)
			g.brackets = true
			g.readings = append(g.readings, r)
		}
	}

	for _, k := range slices.Sorted(maps.Keys(groups)) {
		g := groups[k]
		row := q32Row(buckets[g.bucket], g)
		a.rows = append(a.rows, row)
		what := string(row.Figure)
		if len(row.Region.Percentiles) > 0 || row.Figure == whatif.FigureMeanTPOT {
			what += " " + statText(g.q)
		}
		share := ""
		if row.Region.MaxKneeShare > 0 {
			share = fmt.Sprintf(", %.3f to %.3f of the knee", row.Region.MinKneeShare, row.Region.MaxKneeShare)
		}
		add("envelope %s, %s, flow control %s%s: %s", what, row.Shape, strings.Join(g.flowControl(), " and "), share,
			whatif.Validation{Row: &row}.String())
	}
	a.allowed = slices.Sorted(maps.Keys(allowed))
	return a
}

// q32Phases is the steps by phase, in the order they ran.
func q32Phases(steps []q32Step) [][]q32Step {
	var out [][]q32Step
	for _, s := range steps {
		if n := len(out); n > 0 && out[n-1][0].bucket == s.bucket && out[n-1][0].fc == s.fc {
			out[n-1] = append(out[n-1], s)
			continue
		}
		out = append(out, []q32Step{s})
	}
	return out
}

// q32Model is the run's model by its public name; the committed recording
// names it by its pseudonym, which a region holds beside it.
const q32Model = "Qwen/Qwen3-32B"

// q32PrefixHitSlack is the most prefix-cache hit rate a region holds: the
// run's prompts shared no prefix (the freeze prices 0), a stated tolerance.
const q32PrefixHitSlack = 0.05

// q32RateSlack is how far either side of a measured target's knee share a
// rate row reaches: half the ladder's spacing there (0.05 of the knee).
const q32RateSlack = 0.025

// share3 is a knee share to the 3 decimals a region stores.
func share3(x float64) float64 { return math.Round(1000*x) / 1000 }

// q32Row is the envelope row a group draws. The region holds every input that
// shapes the figure as the run priced it: the spread and the arrivals sent
// (for the figures the prefill queue shapes), the fleet, the accelerator, the
// lengths and the load.
func q32Row(b q32Bucket, g *q32Group) whatif.EnvelopeRow {
	sent := func(x float64) *whatif.Span { return &whatif.Span{Lo: x / q32Tolerance, Hi: x * q32Tolerance} }
	region := whatif.Region{Topology: b.class.Topology, Accelerators: []string{b.class.Accelerator},
		PrefillReplicas: []int{b.spec.Prefill}, DecodeReplicas: []int{b.spec.Decode},
		PromptTokens: sent(b.spec.Prompt), OutputTokens: sent(b.spec.Output),
		PromptCV: sent(b.off.Prefill.PromptCV), ArrivalCV2: sent(b.spec.Ca2),
		Models: []string{q32Model, b.class.Model}, TensorParallel: []int{b.class.TensorParallel},
		PrefillTensorParallel: []int{b.class.PrefillTensorParallel}, PrefixHitRate: &whatif.Span{Lo: 0, Hi: q32PrefixHitSlack}}
	switch g.kind {
	case whatif.FigureTTFT, whatif.FigureRateAtTTFT:
		region.Percentiles = []float64{g.q}
	case whatif.FigureMeanTPOT:
		// Decode's TPOT reads neither the prefill pods nor the prompts'
		// spread. The arrivals stay, as the run sent them.
		region.PrefillReplicas, region.PrefillTensorParallel, region.PromptCV = nil, nil, nil
	case whatif.FigurePrefillKnee:
		// A knee is a capacity: no spread moves it.
		region.PromptCV = nil
	case whatif.FigureDecodeKnee:
		// Nor do the prefill pods move decode's.
		region.PrefillReplicas, region.PrefillTensorParallel, region.PromptCV = nil, nil, nil
	}
	errs := make([]float64, 0, len(g.readings))
	row := whatif.EnvelopeRow{Figure: g.kind, Source: q32Source,
		Shape: fmt.Sprintf("%.0f-token prompts, %.0f out, Ca^2 %g", b.spec.Prompt, b.spec.Output, b.spec.Ca2)}
	open := [2]bool{}
	var targets []float64
	for _, r := range g.readings {
		if !g.shareless {
			lo, hi := r.share, r.share
			if g.kind == whatif.FigureRateAtTTFT {
				lo, hi = r.share-q32RateSlack, r.share+q32RateSlack
			}
			if region.MaxKneeShare == 0 || share3(lo) < region.MinKneeShare {
				region.MinKneeShare = share3(lo)
			}
			region.MaxKneeShare = max(region.MaxKneeShare, share3(hi))
		}
		// Targets equal to 3 decimals are one: off and on price the same
		// target to within rounding.
		if t3 := share3(r.target); r.target > 0 && !slices.Contains(targets, t3) {
			targets = append(targets, t3)
		}
		if !g.brackets {
			errs = append(errs, *r.lo)
			continue
		}
		if r.lo == nil {
			open[0] = true
		} else if row.ErrorLo == nil || *r.lo < *row.ErrorLo {
			row.ErrorLo = fptr(*r.lo)
		}
		if r.hi == nil {
			open[1] = true
		} else if row.ErrorHi == nil || *r.hi > *row.ErrorHi {
			row.ErrorHi = fptr(*r.hi)
		}
	}
	region.FlowControl = g.flowControl()
	// A knee is a capacity, and decode's TPOT does not read the router's gate.
	if g.kind == whatif.FigureMeanTPOT {
		region.FlowControl = nil
	}
	switch {
	case g.brackets:
		// A bracket's readings are the distinct figures it read: one target
		// read with flow control off and on is one reading.
		row.Readings = len(targets)
		row.Spread = whatif.SpreadBracket
		if open[0] {
			row.ErrorLo, row.Spread = nil, whatif.SpreadOneSided
		}
		if open[1] {
			row.ErrorHi, row.Spread = nil, whatif.SpreadOneSided
		}
	default:
		slices.Sort(errs)
		row.Readings = len(errs)
		var levels []float64
		for _, r := range g.readings {
			if l := share3(r.share); !slices.Contains(levels, l) {
				levels = append(levels, l)
			}
		}
		row.Levels = len(levels)
		row.ErrorMin, row.ErrorMax = fptr(errs[0]), fptr(errs[len(errs)-1])
		row.ErrorLo, row.ErrorHi, row.Spread = row.ErrorMin, row.ErrorMax, whatif.SpreadMinMax
		if row.Independent() >= whatif.MinPercentileLevels {
			row.ErrorLo, row.ErrorHi = fptr(percentileOf(errs, 0.1)), fptr(percentileOf(errs, 0.9))
			row.Spread = whatif.SpreadPercentiles
		}
	}
	row.Region = region
	return row
}

// flowControl is the flow-control states the group's readings were taken in.
func (g *q32Group) flowControl() []string {
	var out []string
	for _, r := range g.readings {
		if !slices.Contains(out, r.fc) {
			out = append(out, r.fc)
		}
	}
	slices.Sort(out)
	return out
}

// statText names a statistic as the table prints it.
func statText(q float64) string {
	if q == 0 {
		return statMean
	}
	return "p" + strconv.FormatFloat(100*q, 'f', -1, 64)
}

// q32EnvelopeFile is envelope.json with this run's rows and allowed flags
// replaced by a's.
func q32EnvelopeFile(t *testing.T, a q32Analysis) []byte {
	t.Helper()
	raw, err := os.ReadFile(q32Envelope)
	require.NoError(t, err)
	e, err := whatif.ParseEnvelope(raw)
	require.NoError(t, err)
	e.Rows = slices.DeleteFunc(e.Rows, func(r whatif.EnvelopeRow) bool { return r.Source == q32Source })
	e.Rows = append(e.Rows, a.rows...)
	if e.AllowedFlags == nil {
		e.AllowedFlags = map[string][]whatif.Flag{}
	}
	e.AllowedFlags[q32Source] = a.allowed
	body, err := json.MarshalIndent(e, "", "  ")
	require.NoError(t, err)
	return append(body, '\n')
}

// The staircase's errors at this commit are the frozen ones, and the
// envelope's rows from it are the ones the planner ships.
func TestStaircaseQ32_TheErrorsAreFrozen(t *testing.T) {
	a := analyzeQ32(t)
	got := strings.Join(a.lines, "\n") + "\n"
	env := q32EnvelopeFile(t, a)
	if *update {
		require.NoError(t, os.WriteFile(q32Table, []byte(got), 0o644))
		require.NoError(t, os.WriteFile(q32Envelope, env, 0o644))
	}
	want, err := os.ReadFile(q32Table)
	require.NoError(t, err, "-update writes it")
	assert.Equal(t, strings.ReplaceAll(string(want), "\r\n", "\n"), got, "the staircase's errors moved: name them in the PR's goldens section, then -update")
	shipped, err := os.ReadFile(q32Envelope)
	require.NoError(t, err)
	assert.JSONEq(t, string(env), string(shipped), "envelope.json's %s rows are not this commit's: -update", q32Source)
	// An empty table would pass the comparison above: hold the counts.
	steps := 0
	for _, l := range a.lines {
		if strings.HasPrefix(l, "step ") {
			steps++
		}
	}
	assert.Equal(t, 47, steps, "every step of the six phases")
	assert.NotEmpty(t, a.rows)
}

// Each figure of the run's own fleet, priced where the run measured it,
// reads validated by the envelope the planner ships; past the measured
// region it reads outside.
func TestStaircaseQ32_TheShippedEnvelopeJudgesTheRun(t *testing.T) {
	buckets := q32Buckets(t)
	env := whatif.DefaultEnvelope()
	b := buckets["B"]
	in := whatif.Figure{Kind: whatif.FigureTTFT, Percentile: 0.9, Class: b.class, KneeShare: 0.6}
	v := env.Judge(in)
	require.True(t, v.Validated(), v.String())
	assert.Equal(t, q32Source, v.Row.Source)
	assert.Contains(t, v.String(), "validated in-sample: error ")

	past := in
	past.KneeShare = 0.99
	assert.Contains(t, env.Judge(past).String(), "OUTSIDE VALIDATED REGION (at 0.990 of the knee, measured 0.400 to 0.900)")

	four := in
	four.Class.PrefillReplicas = 4
	assert.Equal(t, "OUTSIDE VALIDATED REGION (4 prefill pods, measured 2)", env.Judge(four).String())
}

// The negative control: priced first come, first served (no processor
// sharing), the frozen errors move, so the table above fails.
func TestStaircaseQ32_WithoutProcessorSharingTheErrorsMove(t *testing.T) {
	want, err := os.ReadFile(q32Table)
	require.NoError(t, err)
	firstCome(t)
	a := analyzeQ32(t)
	wantLines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(want), "\r\n", "\n"), "\n"), "\n")
	require.Len(t, a.lines, len(wantLines))
	moved := map[string]int{}
	for i, l := range a.lines {
		if l != wantLines[i] {
			moved[strings.Fields(l)[0]]++
		}
	}
	t.Logf("without processor sharing, lines moved by kind: %v of %d", moved, len(wantLines))
	// 43 of the 47 steps (the 4 every model refuses at the knee stay), all
	// 12 rate readings, and 14 of the 23 rows. The 9 that stay are the 8
	// TPOT, knee and onset rows, which processor sharing does not price, and
	// one rate row whose bound prints the same either way.
	assert.Equal(t, map[string]int{"step": 43, "rate": 12, "envelope": 14}, moved)
}

// The prompt's spread keeps a figure priced on the default spread out: the
// same TTFT at bucket B's fleet and load reads validated at the spread the run
// sent and outside at the assumed cv 2, and only the spread keeps it out. With
// the rows' spread dropped (the review's finding: the label landed on another
// model, +18..+108% claimed where the error was about +430%), it would read
// validated.
func TestStaircaseQ32_TheAssumedSpreadIsOutside(t *testing.T) {
	buckets := q32Buckets(t)
	env := whatif.DefaultEnvelope()
	b := buckets["B"]
	sent := whatif.Figure{Kind: whatif.FigureTTFT, Percentile: 0.9, Class: b.class, KneeShare: 0.85}
	require.True(t, env.Judge(sent).Validated(), env.Judge(sent).String())

	assumed := sent
	assumed.Class.PromptCV, assumed.Class.PromptCVUnknown = whatif.AssumedPromptCV, true
	v := env.Judge(assumed)
	assert.Equal(t, "OUTSIDE VALIDATED REGION (prompt spread not measured (cv 2 assumed), measured at cv 0.0462 to 0.0721)", v.String())

	blind := env
	blind.Rows = slices.Clone(env.Rows)
	for i := range blind.Rows {
		blind.Rows[i].Region.PromptCV = nil
	}
	assert.True(t, blind.Judge(assumed).Validated(), "without the spread in the region the assumed figure would pass: the region is what holds it out")
}

// The docs' envelope tables are rendered from the rows, between markers, so
// that they cannot go stale when a row moves: accuracy.md for the operator,
// the proposal's "as built" for the record. -update rewrites them.
var q32TableDocs = []string{"../../docs/guides/capacity-planner/accuracy.md", "../../docs/proposals/slo-capacity-planner.md"}

const (
	q32TableBegin = "<!-- envelope-table: begin (go test ./cmd/capacity-planner -run TestStaircaseQ32 -update) -->"
	q32TableEnd   = "<!-- envelope-table: end -->"
)

// q32TableText is the rows as a markdown table.
func q32TableText(rows []whatif.EnvelopeRow) string {
	var b strings.Builder
	b.WriteString("| Figure | Shape | Error | Range | Readings | Knee share | Flow control |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, r := range rows {
		parts := make([]string, 0, 1+len(r.Region.Percentiles))
		parts = append(parts, string(r.Figure))
		for _, q := range r.Region.Percentiles {
			parts = append(parts, statText(q))
		}
		what := strings.Join(parts, " ")
		if r.Figure == whatif.FigureMeanTPOT {
			what = "mean TPOT"
		}
		rng := r.Spread
		if r.Spread == whatif.SpreadPercentiles {
			rng += ", min-max " + whatif.ErrorText(r.ErrorMin, r.ErrorMax)
		}
		n := strconv.Itoa(r.Readings)
		if r.Independent() != r.Readings {
			n = fmt.Sprintf("%d load levels (%d readings)", r.Independent(), r.Readings)
		}
		share := ""
		if r.Region.MaxKneeShare > 0 {
			share = fmt.Sprintf("%.3f to %.3f", r.Region.MinKneeShare, r.Region.MaxKneeShare)
		}
		fc := strings.Join(r.Region.FlowControl, ", ")
		if fc == "" {
			fc = "any"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", what, r.Shape, whatif.ErrorText(r.ErrorLo, r.ErrorHi), rng, n, share, fc)
	}
	return b.String()
}

// The docs carry the rows the planner ships.
func TestStaircaseQ32_TheDocsTablesAreTheRows(t *testing.T) {
	var rows []whatif.EnvelopeRow
	for _, r := range whatif.DefaultEnvelope().Rows {
		if r.Source == q32Source {
			rows = append(rows, r)
		}
	}
	if *update {
		// -update has just rewritten envelope.json; read it, not the
		// embedded copy this binary was built with.
		raw, err := os.ReadFile(q32Envelope)
		require.NoError(t, err)
		e, err := whatif.ParseEnvelope(raw)
		require.NoError(t, err)
		rows = slices.DeleteFunc(e.Rows, func(r whatif.EnvelopeRow) bool { return r.Source != q32Source })
	}
	require.NotEmpty(t, rows)
	want := q32TableText(rows)
	for _, path := range q32TableDocs {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		text := strings.ReplaceAll(string(raw), "\r\n", "\n")
		start, end := strings.Index(text, q32TableBegin), strings.Index(text, q32TableEnd)
		require.True(t, start >= 0 && end > start, "%s: no envelope-table markers", path)
		got := text[start+len(q32TableBegin)+1 : end]
		if *update {
			text = text[:start+len(q32TableBegin)+1] + want + text[end:]
			require.NoError(t, os.WriteFile(path, []byte(text), 0o644))
			continue
		}
		assert.Equal(t, want, got, "%s: the envelope table is stale: -update", path)
	}
}
