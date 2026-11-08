package whatif

import (
	"math"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

const (
	// MinLineRows is the fewest rows a line check reads a verdict from, as
	// the decode validation reads a figure from. Fewer is "not run".
	MinLineRows = 5
	// MaxLineRowAge is the oldest row a line check compares, the age the
	// tracker's window drops a reading at (itl.DefaultObservationMaxAge): a
	// line says nothing of a load it can no longer have read.
	MaxLineRowAge = itl.DefaultObservationMaxAge
)

// The line check (docs/proposals/slo-capacity-planner.md, "Uncertainty"): does
// the line an answer rests on predict the token rate the recording's replicas
// reported? It is the analyzers' GPS check (itl.GPSErrorPct) over a window of
// rows in place of one row, so that one noisy minute decides nothing, with the
// sequences capped as every price caps them (CappedSequences): GPSErrorPct
// prices a cache, and a replica at its batch cap would read as a mismatch.

// LineRow is one replica's reading at one cycle.
type LineRow struct {
	Cycle int
	// At is the clock of the row's cycle.
	At time.Time
	// K is the fill paired with the rate's minute, and KVPerRequest the KV a
	// request held at that cycle: the row's own shape, not the shape asked.
	K, KVPerRequest float64
	// TokenRate is the replica's generated tokens per second.
	TokenRate float64
	// Ramp says the row is off the steady gate: its fill or its shape moved
	// within the gate's time, or it has not been read for that long. A
	// minute's rate and a minute's fill then span two levels, and say nothing
	// of the line.
	Ramp bool
	// AboveBand says the row's fill is above the band: no answer reads the
	// line there.
	AboveBand bool
}

// LineCheck is the line against a window of rows.
type LineCheck struct {
	// Compared is the rows priced. OnRamp, AboveBand and TooOld are the rows
	// left out, each for one of those reasons, in that order of asking: TooOld
	// first, so a row is counted once.
	Compared, OnRamp, AboveBand, TooOld int
	// Past is how many of the Compared read more than
	// itl.DefaultGPSMismatchThresholdPct off the line.
	Past int
	// MedianErrorPct is the median of the Compared rows' errors, without
	// their sign, WorstErrorPct the largest, and SignedErrorPct the median
	// with its sign: above 0, the line predicts more tokens than were read,
	// so it over-promises.
	MedianErrorPct, WorstErrorPct, SignedErrorPct float64
	// FromCycle and ToCycle are the first and the last cycle compared, and
	// FillLo and FillHi the fills.
	FromCycle, ToCycle int
	FillLo, FillHi     float64
	// RowKVPerRequest is the median KV a request held on the Compared rows,
	// and OtherShape says it is more than shape.DefaultChangeTolerance from
	// the KV the answer priced: the verdict is then of that traffic, not the
	// traffic asked about.
	RowKVPerRequest float64
	OtherShape      bool
}

// Ran reports a check with enough rows to say anything.
func (c LineCheck) Ran() bool { return c.Compared >= MinLineRows }

// Mismatch reports a line the rows do not support: the median error past
// itl.DefaultGPSMismatchThresholdPct. A check that did not run is no mismatch,
// and no pass: Ran says which it is.
func (c LineCheck) Mismatch() bool {
	return c.Ran() && c.MedianErrorPct > itl.DefaultGPSMismatchThresholdPct
}

// CheckLine prices each row off a ramp, in the band and no older than
// MaxLineRowAge at now, on d's line, capacity and cap, at the row's own fill
// and KV per request, against the rate it reported. A row the
// GPS check would not compare, one below itl.DefaultGPSMinKForVerification or
// with no rate or KV per request, is left out and counted nowhere: it is the
// absence of evidence either way.
func CheckLine(d Decode, rows []LineRow, now time.Time) LineCheck {
	var c LineCheck
	var absolute, signed, kv []float64
	for _, r := range rows {
		if !(r.K >= itl.DefaultGPSMinKForVerification) || !(r.TokenRate > 0) || !Finite(r.TokenRate) || !(r.KVPerRequest > 0) {
			continue
		}
		switch {
		case now.Sub(r.At) > MaxLineRowAge:
			c.TooOld++
			continue
		case r.AboveBand:
			c.AboveBand++
			continue
		case r.Ramp:
			c.OnRamp++
			continue
		}
		predicted := ReplicaTokenRate(d.Line, r.K, d.CapacityTokens, r.KVPerRequest, d.ReplicaCap)
		if !(predicted > 0) {
			continue
		}
		e := 100 * (predicted - r.TokenRate) / r.TokenRate
		absolute, signed, kv = append(absolute, math.Abs(e)), append(signed, e), append(kv, r.KVPerRequest)
		if math.Abs(e) > itl.DefaultGPSMismatchThresholdPct {
			c.Past++
		}
		c.WorstErrorPct = max(c.WorstErrorPct, math.Abs(e))
		if c.Compared == 0 {
			c.FromCycle, c.FillLo, c.FillHi = r.Cycle, r.K, r.K
		}
		c.Compared++
		c.ToCycle = r.Cycle
		c.FillLo, c.FillHi = min(c.FillLo, r.K), max(c.FillHi, r.K)
	}
	if c.Compared > 0 {
		c.MedianErrorPct, c.SignedErrorPct, c.RowKVPerRequest = median(absolute), median(signed), median(kv)
		c.OtherShape = math.Abs(c.RowKVPerRequest-d.KVPerRequest) > shape.DefaultChangeTolerance*d.KVPerRequest
	}
	return c
}
