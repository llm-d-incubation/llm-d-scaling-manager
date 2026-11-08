package throughput

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// Analyzer-level tests of the line's silence: when it logs, what cause it
// names, and what the above-band fallback may and may not do, each through
// Analyze.

// lineLogs is every "throughput analyzer: ITL line" entry in log, in order.
func lineLogs(log string) []string {
	var out []string
	for line := range strings.SplitSeq(log, "\n") {
		if strings.Contains(line, "throughput analyzer: ITL line") {
			out = append(out, line)
		}
	}
	return out
}

// staleFleet is steadyFleet with every row's metrics stale: no healthy row.
func staleFleet() []domain.ReplicaMetrics {
	rows := steadyFleet()
	for i := range rows {
		rows[i].Metadata = &domain.ReplicaMetricsMetadata{FreshnessStatus: "stale"}
	}
	return rows
}

// fleetAt is steadyFleet with each replica at the k given, on the line.
func fleetAt(ks ...float64) []domain.ReplicaMetrics {
	early := lineStart.Add(-time.Hour)
	rows := make([]domain.ReplicaMetrics, 0, len(ks))
	for i, k := range ks {
		rows = append(rows, lineRow("pod-"+string(rune('a'+i)), k, lineAt(k), early))
	}
	return rows
}

// Every transition of the line logs once, naming its cause when it goes
// silent: silent on the first cycles (restart), back, silent through cycles
// with no healthy row (no-healthy-rows), back, and silent again on a shape
// change (shape-change). A cycle with no healthy row reads nothing, so it
// has no line: it does not price from the last cycle's reading.
func TestITLLine_EveryTransitionLogsOnce(t *testing.T) {
	ctx, buf := logContext()
	r := newLineRun(t, NewThroughputAnalyzer())
	r.ctx = ctx
	for range 12 {
		r.step(steadyFleet())
	}
	logs := lineLogs(buf.String())
	require.Len(t, logs, 2, "premise: silent on the first cycles, then back: %s", buf)
	require.Contains(t, logs[0], `"silent": true, "cause": "restart"`)
	require.Contains(t, logs[1], `"silent": false, "cause": ""`)

	for c := range 3 {
		vc, has := r.step(staleFleet())
		require.False(t, has, "cycle %d with no healthy row priced from an old reading (%s)", c, vc.Reason)
	}
	logs = lineLogs(buf.String())
	require.Len(t, logs, 3, "no healthy row: want one silent line: %s", buf)
	require.Contains(t, logs[2], `"silent": true, "cause": "no-healthy-rows"`)
	require.Contains(t, logs[2], `"held": 0, "aboveBandHeld": 0`, "no healthy row: nothing was read")

	for range 2 {
		_, has := r.step(steadyFleet())
		require.True(t, has, "the line did not come back with the healthy rows")
	}
	logs = lineLogs(buf.String())
	require.Len(t, logs, 4, "back: want one line: %s", buf)
	require.Contains(t, logs[3], `"silent": false, "cause": ""`)
	// The window kept its readings through the cycles that read nothing:
	// 12 cycles and 1 more, three replicas each, capped at 20.
	require.Contains(t, logs[3], `"held": 20, "aboveBandHeld": 0`)

	shifted := steadyFleet()
	for i := range shifted {
		shifted[i].AvgInputTokens, shifted[i].AvgOutputTokens = 256, 64
	}
	_, has := r.step(shifted)
	require.False(t, has, "premise: the shape change left no line")
	logs = lineLogs(buf.String())
	require.Len(t, logs, 5, "silent again: want one line: %s", buf)
	require.Contains(t, logs[4], `"silent": true, "cause": "shape-change"`)
	require.Contains(t, logs[4], `"held": 3, "aboveBandHeld": 0`, "the window emptied, then took this cycle's rows")
}

// A line that was live and runs out of readings -- the fleet below the band,
// its readings aged out, the hold over -- names too-few-readings, with the
// rows it saw below the band.
func TestITLLine_ASilenceFromTooFewReadingsSaysSo(t *testing.T) {
	ctx, buf := logContext()
	r := newLineRun(t, NewThroughputAnalyzer().WithSlopeHoldFor(time.Minute))
	r.ctx = ctx
	for range 12 {
		r.step(steadyFleet())
	}
	require.Len(t, lineLogs(buf.String()), 2, "premise: live: %s", buf)
	for r.now().Before(lineStart.Add(40 * time.Minute)) {
		r.step(idleFleet())
	}
	logs := lineLogs(buf.String())
	require.Len(t, logs, 3, "want one silent line once the hold ended: %s", buf)
	require.Contains(t, logs[2], `"silent": true, "cause": "too-few-readings"`)
	require.Contains(t, logs[2], `"belowBand": 3`)
	require.Contains(t, logs[2], `"held": 0, "aboveBandHeld": 0`)
}

// Readings above the band, under itl.DefaultKSat, are not the fallback's,
// through Analyze. On a fleet that never fitted, one replica at k 0.90 and
// three at 0.82 (above the band's top, 0.80), the line is priced from the
// 0.90 readings alone: B is their ITL, and the priced ITL at
// itl.DefaultKSat is at or above the true line's. Taken whole, the 0.82
// readings pull B down to their ITL and the mean k under itl.DefaultKSat,
// and the price falls under the truth (about 1 % here). Until the 0.90
// replica has ten readings the line is silent, and says why only when no
// reading could ever price it: here one could, so it names restart.
func TestITLLine_TheFallbackPricesAtOrAboveTheTruth(t *testing.T) {
	ctx, buf := logContext()
	r := newLineRun(t, NewThroughputAnalyzer())
	r.ctx = ctx
	truth := lineAt(itl.DefaultKSat)
	priced := 0
	for c := range 20 {
		vc, has := r.step(fleetAt(0.82, 0.82, 0.82, 0.90))
		if !has {
			require.Less(t, c, 10, "cycle %d: no line after ten readings at 0.90", c)
			continue
		}
		require.Equal(t, itlReasonT2AboveBand, vc.Reason, "cycle %d", c)
		st, _ := r.a.VariantState("m", "ns", "v1")
		require.GreaterOrEqual(t, st.ITLModel.ITLAt(itl.DefaultKSat), truth,
			"cycle %d: priced %.5f s, under the true line's %.5f s", c, st.ITLModel.ITLAt(itl.DefaultKSat), truth)
		require.InDelta(t, lineAt(0.90), st.ITLModel.B, 1e-12,
			"cycle %d: B is not the 0.90 readings' ITL: readings under the price were used", c)
		priced++
	}
	require.Equal(t, 11, priced, "the 0.90 replica's tenth reading is on cycle 9")
	logs := lineLogs(buf.String())
	require.Len(t, logs, 2, "%s", buf)
	require.Contains(t, logs[0], `"silent": true, "cause": "restart"`)
	require.Contains(t, logs[1], `"silent": false, "cause": "", "source": "T2-above-band"`)
	require.Contains(t, logs[1], `"aboveBand": 4`)
	require.Contains(t, logs[1], `"aboveBandHeld": 10`, "the fallback kept the 0.82 readings")
}

// A fleet whose every replica sits above the band under itl.DefaultKSat
// gives neither the band nor the fallback a reading, and the silent line
// names it.
func TestITLLine_AllAboveTheBandUnderKSatIsNamed(t *testing.T) {
	ctx, buf := logContext()
	r := newLineRun(t, NewThroughputAnalyzer())
	r.ctx = ctx
	for range 12 {
		_, has := r.step(fleetAt(0.82, 0.82, 0.82))
		require.False(t, has, "a line from readings no path may use")
	}
	logs := lineLogs(buf.String())
	require.Len(t, logs, 1, "%s", buf)
	require.Contains(t, logs[0], `"silent": true, "cause": "all-above-band-below-ksat"`)
	require.Contains(t, logs[0], `"aboveBand": 3`)
}

// The fallback never replaces a slope the key may hold, through Analyze. A
// line fitted in the band, then a fleet above it for 35 min under a 1 h
// hold: the band's old readings keep fitting until they age out (30 min),
// then the slope holds (T2-held). The fallback, though its window holds ten
// readings by the fourth cycle above the band, never prices it.
func TestITLLine_TheFallbackNeverReplacesAHeldSlope(t *testing.T) {
	r := newLineRun(t, NewThroughputAnalyzer().WithSlopeHoldFor(time.Hour))
	for range 12 {
		r.step(steadyFleet())
	}
	fitted := r.slope()
	reasons := map[string]int{}
	for r.now().Before(lineStart.Add(38 * time.Minute)) {
		vc, has := r.step(fleetAt(0.90, 0.90, 0.90))
		require.True(t, has, "the line went silent with a slope to hold")
		reasons[vc.Reason]++
		require.NotEqual(t, itlReasonT2AboveBand, vc.Reason, "the fallback replaced the line at %s (%v)",
			r.now().Sub(lineStart), reasons)
		if vc.Reason == itlReasonT2Held {
			require.InDelta(t, fitted, r.slope(), 1e-12, "a held slope moved")
		}
	}
	require.Positive(t, reasons[itlReasonT2Held], "premise: the slope was held: %v", reasons)
}

// The fallback's B is never stored as the key's intercept, through
// Analyze. A fleet that never fitted is priced from above the band; when it
// comes into the band, its first fit pins the default intercept
// (T2-default), not the fallback's B (which would read T2-pinned).
func TestITLLine_TheFallbackNeverStoresB(t *testing.T) {
	r := newLineRun(t, NewThroughputAnalyzer())
	above := 0
	for range 8 {
		if vc, has := r.step(fleetAt(0.90, 0.90, 0.90)); has && vc.Reason == itlReasonT2AboveBand {
			above++
		}
	}
	require.Positive(t, above, "premise: the fallback priced the fleet")
	st, _ := r.a.VariantState("m", "ns", "v1")
	require.False(t, st.HasFittedB, "the fallback's B %g was stored", st.LastFittedB)
	b, stored := r.a.lines.Baseline(variantKey("ns", "m", "v1"))
	require.False(t, stored, "the fallback's B %g was stored on the tracker", b)

	var vc domain.VariantCapacity
	var has bool
	for range 4 {
		vc, has = r.step(steadyFleet())
	}
	require.True(t, has, "premise: the band fitted")
	require.Equal(t, itlReasonT2Default, vc.Reason, "the band's first fit pinned a B it did not measure")
	st, _ = r.a.VariantState("m", "ns", "v1")
	require.InDelta(t, itl.DefaultBaselineSec, st.ITLModel.B, 1e-12)
}
