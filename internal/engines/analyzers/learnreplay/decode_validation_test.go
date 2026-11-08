package learnreplay

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The planner's decode validation (docs/proposals/slo-capacity-planner.md,
// "Validation"): learn on the first 60% of a golden's cycles, freeze, predict
// the rest; signed, out of sample. It is an error report with bounds, not a
// pass mark: goldens scale out, and were not run to validate this.
//
// GREEN IS NOT A PASS. The test holds the figures to the frozen table in
// cmd/whatif/testdata/decode-validation.golden, so a change to the line, the
// arithmetic or the replay that moves one says so, and the goldens gate, which
// reads that directory, makes the PR name it. A row past its bound is a
// finding, frozen as OUT. A check with no data is SKIPPED with a reason, never
// passed.
// LEARNREPLAY_WRITE_DECODE_VALIDATION=1 rewrites the table.

const (
	// decodeLearnShare is the share of a golden's cycles learned on.
	decodeLearnShare = 0.6
	// decodeMinRows is the fewest held-out rows a check reports on: the
	// page's "at least 5" for the knee, taken for all three.
	decodeMinRows = whatif.MinLineRows
	// The page's bounds on the median signed error, in percent.
	decodeTPOTBound = 10.0
	decodeOwnBound  = 15.0
	decodeKneeBound = 15.0
)

const decodeTable = "../../../../cmd/whatif/testdata/decode-validation.golden"

// decodeCycle is one of a model's cycles, and the clock it ran at.
type decodeCycle struct {
	in domain.AnalyzerInput
	at time.Time
}

// cyclesByModel is each namespace and model the recording analyzed, and its
// cycles in order.
func cyclesByModel(t *testing.T, events []learnrec.RawEvent) map[[2]string][]decodeCycle {
	t.Helper()
	out := map[[2]string][]decodeCycle{}
	for _, ev := range events {
		if ev.Kind != saturation.KindAnalyze {
			continue
		}
		var d saturation.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&d))
		in, err := decodeInput(d.Input)
		require.NoError(t, err)
		key := [2]string{in.Namespace, in.ModelID}
		out[key] = append(out[key], decodeCycle{in: in, at: ev.Now})
	}
	return out
}

// signedErrors reduces a check's signed errors, in percent, to its line: the
// median, n rows, and whether it is inside the bound. Too few rows is SKIPPED.
func signedErrors(name string, errs []float64, bound float64, none string) string {
	if len(errs) == 0 {
		return fmt.Sprintf("%s SKIPPED %s", name, none)
	}
	if len(errs) < decodeMinRows {
		return fmt.Sprintf("%s SKIPPED %d rows, want %d", name, len(errs), decodeMinRows)
	}
	med := medianOf(errs)
	verdict := "in"
	if math.Abs(med) > bound {
		verdict = "OUT"
	}
	return fmt.Sprintf("%s %+.1f%% over %d rows, %s of +-%g%%", name, med, len(errs), verdict, bound)
}

// medianOf is the median of xs, which it sorts.
func medianOf(xs []float64) float64 {
	slices.Sort(xs)
	if n := len(xs); n%2 == 1 {
		return xs[n/2]
	}
	return (xs[len(xs)/2-1] + xs[len(xs)/2]) / 2
}

// rowsOf is the rows of variant that served, of the cycles from start on, and
// those of them off a ramp by the line check's rule (lineSamples.onRamp), read
// over every cycle so that a row just after start has its past.
func rowsOf(cycles []decodeCycle, start int, variant string) (served, still []domain.ReplicaMetrics) {
	l := newLineSamples()
	for i, c := range cycles {
		policy, _ := c.in.Config.(*config.ScalingPolicy)
		for _, rm := range c.in.ReplicaMetrics {
			if rm.VariantName != variant || !rm.Ready || rm.FromWarmPool {
				continue
			}
			ramp := l.onRamp(rm.PodName, fillAt{k: rm.KvUsageForITL(), kvReq: rowShape(rm).KVreq, at: c.at}, policy.ITLSteadiness())
			if i < start {
				continue
			}
			served = append(served, rm)
			if !ramp {
				still = append(still, rm)
			}
		}
	}
	return served, still
}

// plannerRefuses says the line is one no answer prices from: the check's figure
// is then the line's, and the tool would print none.
func plannerRefuses(vs VariantState, band whatif.FillBand) bool {
	s := shape.New(vs.Shape.InputTokens, vs.Shape.OutputTokens, vs.Shape.PrefixHitRate)
	u, ok := whatif.KneeRate(whatif.Decode{Line: vs.Line, Band: band, CapacityTokens: vs.CapacityTokens, KVPerRequest: s.KVreq,
		OutputTokens: s.AvgOutputTokens, ReplicaCap: vs.BatchCap * int64(max(vs.Ranks, 1)), Replicas: 1, Tier: vs.Tier}).(whatif.Unanswerable)
	return ok && u.Reason == whatif.PinnedDefaultB
}

// rowShape is the shape a row served: the frozen line is asked at the traffic
// that came, not the traffic it was learned on.
func rowShape(rm domain.ReplicaMetrics) shape.Shape {
	s := shape.New(rm.AvgInputTokens, rm.AvgOutputTokens, rm.PrefixCacheHitRate)
	if rm.AvgOutputTokensSquaredKnown {
		s = s.WithOutputDispersion(shape.Dispersion(rm.AvgOutputTokens, rm.AvgOutputTokensSquared))
	}
	return s
}

// meanTPOTCheck is the frozen line at each row's fill, clamped to the band as
// an answer is, against the ITL the engine read there. The bound reads that
// figure. The line beside it prints the unclamped one too, and how many rows
// the clamp moved each way: a figure in bound only through the clamp says so.
func meanTPOTCheck(vs VariantState, band whatif.FillBand, rows []domain.ReplicaMetrics) string {
	if vs.Line.IsZero() {
		return "tpot SKIPPED no line at the freeze"
	}
	var errs, raw []float64
	below, above := 0, 0
	for _, rm := range rows {
		k := rm.KvUsageForITL()
		if rm.ITLState() != domain.ITLFinite || !(k > 0) {
			continue
		}
		switch {
		case k < band.Lo:
			below++
		case k > band.Hi:
			above++
		}
		predicted := vs.Line.ITLAt(min(max(k, band.Lo), band.Hi))
		errs = append(errs, 100*(predicted-rm.AvgITL)/rm.AvgITL)
		raw = append(raw, 100*(vs.Line.ITLAt(k)-rm.AvgITL)/rm.AvgITL)
	}
	line := signedErrors("tpot", errs, decodeTPOTBound, "no held-out row reads an ITL")
	if len(raw) >= decodeMinRows {
		line += fmt.Sprintf(", unclamped %+.1f%%, %d below the band, %d above it", medianOf(raw), below, above)
	}
	if plannerRefuses(vs, band) {
		line += ", on a line the planner refuses"
	}
	return line
}

// saturatedRow is a replica whose engine had work waiting the whole minute, or
// on a recording before QueueMin, at the cycle.
func saturatedRow(rm domain.ReplicaMetrics) bool {
	if rm.QueueMinKnown {
		return rm.QueueMin > 0
	}
	return rm.QueueLength > 0
}

// kneeCheck is the frozen line's knee on one replica, at each saturated row's
// shape, in tokens per second, against the tokens the replica made. rows are
// off a ramp: a row's lengths are means over minutes, and the knee of a shape
// that has already left is no prediction of the row.
func kneeCheck(vs VariantState, band whatif.FillBand, rows []domain.ReplicaMetrics) string {
	if vs.Line.IsZero() {
		return "knee SKIPPED no line at the freeze"
	}
	var errs []float64
	saturated, refused := 0, map[whatif.Reason]int{}
	for _, rm := range rows {
		if !saturatedRow(rm) || !rm.GenerationTokenRateKnown || !(rm.GenerationTokenRate > 0) {
			continue
		}
		saturated++
		s := rowShape(rm)
		d := whatif.Decode{Line: vs.Line, Band: band, CapacityTokens: vs.CapacityTokens, KVPerRequest: s.KVreq,
			OutputTokens: s.AvgOutputTokens, ReplicaCap: vs.BatchCap * int64(max(vs.Ranks, 1)), Replicas: 1, Tier: vs.Tier}
		switch v := whatif.KneeRate(d).(type) {
		case whatif.Answer:
			predicted := v.Value * s.AvgOutputTokens
			errs = append(errs, 100*(predicted-rm.GenerationTokenRate)/rm.GenerationTokenRate)
		case whatif.Unanswerable:
			refused[v.Reason]++
		}
	}
	none := "no saturated window off a ramp held out"
	if saturated > 0 {
		reasons := make([]string, 0, len(refused))
		for r := range refused {
			reasons = append(reasons, string(r))
		}
		slices.Sort(reasons)
		none = fmt.Sprintf("the knee refused on all %d saturated windows: %s", saturated, strings.Join(reasons, ", "))
	}
	return signedErrors("knee", errs, decodeKneeBound, none)
}

// prefillOwnCheck is a + I/V_P, frozen, at each row's prompt, against the
// service time the prefill pod read. A V_P or an a P1 refused at the freeze is
// SKIPPED: this test assumes neither.
func prefillOwnCheck(ps PrefillState, rows []domain.ReplicaMetrics) string {
	est := ps.Estimate
	switch {
	case !ps.EstimateRead:
		return "own SKIPPED no P1 estimate at the freeze"
	case !(est.VP > 0) || est.VPRefused != "":
		return fmt.Sprintf("own SKIPPED P1 refused V_P: %s", est.VPRefused)
	case !(est.A > 0) || est.ARefused != "":
		return fmt.Sprintf("own SKIPPED P1 refused a: %s", est.ARefused)
	}
	p, refused := whatif.PrefillFromEstimate(est)
	if refused != nil {
		return fmt.Sprintf("own SKIPPED %s", refused.Reason)
	}
	p.Replicas, p.Imbalance, p.VPMargin = 1, 1, 0
	var errs []float64
	for _, rm := range rows {
		if !rm.AvgServiceTimeKnown || !(rm.AvgServiceTime > 0) || !rm.RequestRateKnown || !(rm.RequestRate > 0) ||
			!rm.PrefillComputedTokenRateKnown || !(rm.PrefillComputedTokenRate > 0) {
			continue
		}
		at := p
		at.PromptTokens = rm.PrefillComputedTokenRate / rm.RequestRate
		// At no load the mean is the request's own work: no wait, no margin.
		if v, ok := whatif.TTFTAt(whatif.TTFTFleet{Prefill: at}, 1e-9, 0).(whatif.TTFT); ok {
			errs = append(errs, 100*(v.Parts.OwnPrefill-rm.AvgServiceTime)/rm.AvgServiceTime)
		}
	}
	return signedErrors("own", errs, decodeOwnBound, "no held-out row reads a service time and a prompt")
}

// validateGolden is one line per variant of each model of a golden.
func validateGolden(ctx context.Context, t *testing.T, name string, raw []byte) []string {
	t.Helper()
	rec, err := learnrec.Read(raw)
	require.NoError(t, err)
	var lines []string
	for key, cycles := range cyclesByModel(t, rec.Events) {
		ns, model := key[0], key[1]
		freeze := int(math.Ceil(decodeLearnShare * float64(len(cycles))))
		st, err := StateAt(ctx, raw, Options{}, StateQuery{Namespace: ns, ModelID: model, Cycle: freeze})
		require.NoError(t, err, "%s %s/%s", name, ns, model)
		require.Equal(t, freeze, st.Cycle, "the state is read at the freeze")

		lo, hi := st.Policy.ITLBand()
		band := whatif.FillBand{Lo: lo, Hi: hi}
		id := fmt.Sprintf("%s %s/%s freeze %d of %d", name, ns, model, freeze, len(cycles))
		for _, vs := range st.Variants {
			if domain.CanonicalRole(vs.Role) == domain.RolePrefill {
				continue
			}
			rows, still := rowsOf(cycles, freeze, vs.Variant)
			lines = append(lines, fmt.Sprintf("%s %s %s; %s; %s", id, vs.Variant, valueOr(vs.Tier, "no-line"),
				meanTPOTCheck(vs, band, rows), kneeCheck(vs, band, still)))
		}
		for _, ps := range st.Prefill {
			rows, _ := rowsOf(cycles, freeze, ps.Variant)
			lines = append(lines, fmt.Sprintf("%s %s prefill; %s", id, ps.Variant, prefillOwnCheck(ps, rows)))
		}
		if len(st.Variants) == 0 && len(st.Prefill) == 0 {
			lines = append(lines, id+"; SKIPPED no variant priced at the freeze")
		}
	}
	slices.Sort(lines)
	return lines
}

func valueOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// The table of the page's "Validation" section: every committed golden, the
// four staircase ones among them. Figures held to the frozen file.
func TestDecodeValidation_OnTheGoldens(t *testing.T) {
	dir := filepath.Join("testdata", "goldens")
	m, err := ReadManifest(dir)
	require.NoError(t, err)
	got := make([]string, 0, len(m.Goldens))
	seen := map[string]bool{}
	for _, g := range m.Goldens {
		gz, err := os.ReadFile(filepath.Join(dir, g.Recording))
		require.NoError(t, err)
		raw, err := gunzip(gz)
		require.NoError(t, err)
		lines := validateGolden(context.Background(), t, g.Name, raw)
		if len(lines) == 0 {
			lines = []string{g.Name + "; SKIPPED no saturation cycle"}
		}
		seen[g.Name] = true
		got = append(got, lines...)
	}
	assert.Len(t, seen, len(m.Goldens), "every golden is reported, skipped or not")
	for _, name := range []string{"ttft-staircase-a-off", "ttft-staircase-b-off", "ttft-staircase-b-on", "ttft-staircase-c-on"} {
		assert.True(t, seen[name], name)
	}
	text := strings.Join(got, "\n") + "\n"
	for _, check := range []string{"tpot ", "knee "} {
		assert.Positive(t, measured(got, check), "no %srow measured: an empty report is not a pass", check)
	}
	if writesDecodeTable() {
		require.NoError(t, os.WriteFile(decodeTable, []byte(text), 0o644))
		return
	}
	want, err := os.ReadFile(decodeTable)
	require.NoError(t, err)
	assert.Equal(t, strings.ReplaceAll(string(want), "\r\n", "\n"), text, "a figure moved: rewrite the table and say which in the PR")
}

// writesDecodeTable reports whether the run rewrites the table, on exactly
// the value LEARNREPLAY_WRITE_MESSAGE_WORDS asks by: =0 or =false must compare,
// not rewrite and pass.
func writesDecodeTable() bool { return os.Getenv("LEARNREPLAY_WRITE_DECODE_VALIDATION") == "1" }

// measured counts the lines whose check gave a figure.
func measured(lines []string, check string) int {
	n := 0
	for _, l := range lines {
		for _, part := range strings.Split(l, "; ") {
			if strings.HasPrefix(part, check) && !strings.HasPrefix(part, check+"SKIPPED") {
				n++
			}
		}
	}
	return n
}

// Only =1 rewrites the table: any other value compares.
func TestDecodeValidation_OnlyOneRewrites(t *testing.T) {
	for value, want := range map[string]bool{"1": true, "": false, "0": false, "false": false, "true": false} {
		t.Setenv("LEARNREPLAY_WRITE_DECODE_VALIDATION", value)
		assert.Equal(t, want, writesDecodeTable(), "=%q", value)
	}
}

// A check with too little data says why, whatever figure it might have read.
func TestDecodeValidation_TooLittleIsSkipped(t *testing.T) {
	assert.Equal(t, "knee SKIPPED none here", signedErrors("knee", nil, 15, "none here"))
	assert.Equal(t, "knee SKIPPED 4 rows, want 5", signedErrors("knee", []float64{1, 1, 1, 1}, 15, "none here"))
	assert.Equal(t, "knee +1.0% over 5 rows, in of +-15%", signedErrors("knee", []float64{1, 1, 1, 1, 1}, 15, "none here"))
}

// The median is signed, and the bound reads it either way.
func TestDecodeValidation_TheBoundIsSigned(t *testing.T) {
	assert.Equal(t, "tpot -30.0% over 5 rows, OUT of +-10%", signedErrors("tpot", []float64{-40, -35, -30, -20, 90}, 10, ""))
	assert.Equal(t, "tpot +10.0% over 6 rows, in of +-10%", signedErrors("tpot", []float64{10, 10, 10, 10, 10, 10}, 10, ""))
	assert.Equal(t, "tpot +10.1% over 5 rows, OUT of +-10%", signedErrors("tpot", []float64{10.1, 10.1, 10.1, 10.1, 10.1}, 10, ""))
}

// The frozen line is read at the freeze, and only a row after it is priced: a
// variant given a line that fits the held-out rows reads 0, one 20% off reads
// 20%, whichever way.
func TestDecodeValidation_ChecksAreOutOfSampleArithmetic(t *testing.T) {
	rows := make([]domain.ReplicaMetrics, 6)
	for i := range rows {
		rows[i] = domain.ReplicaMetrics{KvUsageInstant: 0.5, KvUsageInstantKnown: true, Ready: true, AvgITL: 0.02, AvgITLKnown: true}
	}
	vs := VariantState{}
	vs.Line.A, vs.Line.B = 0.02, 0.01
	band := whatif.DefaultBand()
	assert.Equal(t, "tpot +0.0% over 6 rows, in of +-10%, unclamped +0.0%, 0 below the band, 0 above it", meanTPOTCheck(vs, band, rows))
	vs.Line.B = 0.014
	assert.Equal(t, "tpot +20.0% over 6 rows, OUT of +-10%, unclamped +20.0%, 0 below the band, 0 above it", meanTPOTCheck(vs, band, rows))
	assert.Equal(t, "tpot SKIPPED no line at the freeze", meanTPOTCheck(VariantState{}, band, rows))

	// The policy default band: a row above it is priced at its top, as an
	// answer is.
	high := rows[0]
	high.KvUsageInstant = 0.99
	vs.Line.B = 0.01
	_, top := (*config.ScalingPolicy)(nil).ITLBand()
	want := 100 * (vs.Line.ITLAt(top) - high.AvgITL) / high.AvgITL
	raw := 100 * (vs.Line.ITLAt(0.99) - high.AvgITL) / high.AvgITL
	assert.Equal(t, fmt.Sprintf("tpot %+.1f%% over 6 rows, OUT of +-10%%, unclamped %+.1f%%, 0 below the band, 6 above it", want, raw),
		meanTPOTCheck(vs, band, slices.Repeat([]domain.ReplicaMetrics{high}, 6)))
}

// The prefill check is a + I/V_P against the service time, and no golden gives
// it both figures at the freeze: this is the only place it reads a number.
func TestDecodeValidation_ThePrefillArithmetic(t *testing.T) {
	ps := PrefillState{EstimateRead: true, Estimate: prefill.Estimate{VP: 100000, VPPromptTokens: 2000, A: 0.01}}
	rows := make([]domain.ReplicaMetrics, 5)
	for i := range rows {
		// 2000 tokens a request: a 10 ms, the work 20 ms, 30 ms in all.
		rows[i] = domain.ReplicaMetrics{AvgServiceTime: 0.025, AvgServiceTimeKnown: true, RequestRate: 2, RequestRateKnown: true,
			PrefillComputedTokenRate: 4000, PrefillComputedTokenRateKnown: true}
	}
	assert.Equal(t, "own +20.0% over 5 rows, OUT of +-15%", prefillOwnCheck(ps, rows))

	refused := ps
	refused.Estimate.ARefused = "TooFewLowLoadMinutes"
	assert.Equal(t, "own SKIPPED P1 refused a: TooFewLowLoadMinutes", prefillOwnCheck(refused, rows))
	refused = ps
	refused.Estimate.VPRefused = "NoPrefillCapacity"
	assert.Equal(t, "own SKIPPED P1 refused V_P: NoPrefillCapacity", prefillOwnCheck(refused, rows))
	assert.Equal(t, "own SKIPPED no P1 estimate at the freeze", prefillOwnCheck(PrefillState{}, rows))
}

// A knee refused says why, and a row with no saturation is no window.
func TestDecodeValidation_TheKneeArithmetic(t *testing.T) {
	vs := VariantState{Tier: whatif.TierOLS, CapacityTokens: 100000, BatchCap: 1000, Ranks: 1}
	vs.Line.A, vs.Line.B = 0.01, 0.02
	band := whatif.DefaultBand()
	s := shape.New(1000, 1000, 0)
	d := whatif.Decode{Line: vs.Line, Band: band, CapacityTokens: vs.CapacityTokens, KVPerRequest: s.KVreq, OutputTokens: 1000,
		ReplicaCap: 1000, Replicas: 1, Tier: vs.Tier}
	knee, ok := whatif.KneeRate(d).(whatif.Answer)
	require.True(t, ok)
	row := domain.ReplicaMetrics{QueueLength: 3, AvgInputTokens: 1000, AvgOutputTokens: 1000, GenerationTokenRate: knee.Value * 1000 / 1.25,
		GenerationTokenRateKnown: true}
	assert.Equal(t, "knee +25.0% over 5 rows, OUT of +-15%", kneeCheck(vs, band, slices.Repeat([]domain.ReplicaMetrics{row}, 5)))

	row.QueueLength = 0
	assert.Equal(t, "knee SKIPPED no saturated window off a ramp held out", kneeCheck(vs, band, slices.Repeat([]domain.ReplicaMetrics{row}, 5)))

	row.QueueLength = 3
	noCap := vs
	noCap.BatchCap = 0
	assert.Equal(t, "knee SKIPPED the knee refused on all 5 saturated windows: NoBatchCap",
		kneeCheck(noCap, band, slices.Repeat([]domain.ReplicaMetrics{row}, 5)))
}

// A saturated window is an engine with work waiting the whole minute; a queue
// seen at one instant does not count when the minimum was read.
func TestDecodeValidation_ASaturatedWindow(t *testing.T) {
	assert.True(t, saturatedRow(domain.ReplicaMetrics{QueueMinKnown: true, QueueMin: 1}))
	assert.False(t, saturatedRow(domain.ReplicaMetrics{QueueMinKnown: true, QueueLength: 40}))
	assert.True(t, saturatedRow(domain.ReplicaMetrics{QueueLength: 1}))
	assert.False(t, saturatedRow(domain.ReplicaMetrics{}))
}
