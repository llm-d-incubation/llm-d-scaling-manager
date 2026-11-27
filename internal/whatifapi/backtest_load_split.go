package whatifapi

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ExcludedLoad is a step of the test's span outside a load split's test: its
// rate is under the test's floor, or the fit read it (BacktestRequest.FitBelow).
const ExcludedLoad = "load"

// BacktestLoadSplit is a split by load, where the request asked one: the fit
// read only the steps whose rate was below FitBelowRate, the test judges only
// the steps at or above TestAboveRate. FitBelow and TestAbove are the bounds
// as asked, a rate or a percentile of the side's own step rates; FitSteps and
// TestSteps the steps each side kept; FitRead the test's steps over its floor
// left out because the fit read a cycle in them (two sources over one time).
type BacktestLoadSplit struct {
	FitBelow      string  `json:"fitBelow"`
	TestAbove     string  `json:"testAbove"`
	FitBelowRate  float64 `json:"fitBelowRate"`
	TestAboveRate float64 `json:"testAboveRate"`
	FitSteps      int     `json:"fitSteps"`
	TestSteps     int     `json:"testSteps"`
	FitRead       int     `json:"fitRead,omitempty"`
}

// CycleSet is the cycles of a source a fit is read from, by the times of
// their samples (SeriesSample.At): a load split's fit steps. A nil set holds
// every cycle.
type CycleSet struct {
	at map[int64]bool
	// rate is the fit's bound in requests per second, as LoadFitCycles
	// read it over the whole source.
	rate float64
}

// Keeps reports whether the set holds a cycle at any of at.
func (c *CycleSet) Keeps(at []time.Time) bool {
	if c == nil {
		return true
	}
	for _, t := range at {
		if c.at[t.UnixNano()] {
			return true
		}
	}
	return false
}

// Len is the cycles the set holds.
func (c *CycleSet) Len() int {
	if c == nil {
		return 0
	}
	return len(c.at)
}

// loadSplitAsked reports whether req splits by load.
func loadSplitAsked(req BacktestRequest) bool { return req.FitBelow != "" || req.TestAbove != "" }

// checkLoadSplit refuses a load split asked with half its bounds, or beside
// a split by time.
func checkLoadSplit(req BacktestRequest) error {
	switch {
	case !loadSplitAsked(req):
		return nil
	case req.FitBelow == "" || req.TestAbove == "":
		return InputErrorf("fitBelow and testAbove split by load together: give both (a rate in requests per second, or a percentile of the steps' rates, as p50)")
	case req.Split != "" || req.FitUntil != "":
		return InputErrorf("fitBelow and testAbove split by load, split and fitUntil by time: give one")
	}
	return nil
}

// backtestStepOf is req's step, as questionOf reads it.
func backtestStepOf(req BacktestRequest) (time.Duration, error) {
	if req.Step == "" {
		return DefaultBacktestStep, nil
	}
	step, err := time.ParseDuration(req.Step)
	if err != nil || step < minBacktestStep || step > maxBacktestStep {
		return 0, InputErrorf("step %q, want a duration from %s to %s", req.Step, minBacktestStep, maxBacktestStep)
	}
	return step, nil
}

// stepRates is each step's arrival rate: the mean over the step's cycles
// whose rate is known, by the step's start (the time truncated to the step,
// as the backtest lays its steps out). A step with no known rate has none.
func stepRates(series []SeriesSample, step time.Duration) map[time.Time]float64 {
	sum, n := map[time.Time]float64{}, map[time.Time]int{}
	for _, x := range series {
		if !x.RateKnown {
			continue
		}
		k := x.At.UTC().Truncate(step)
		sum[k] += x.Rate
		n[k]++
	}
	out := make(map[time.Time]float64, len(sum))
	for k, s := range sum {
		out[k] = s / float64(n[k])
	}
	return out
}

// loadBound is the rate spec names over rates: a rate in requests per
// second ("12.5"), or a percentile of the steps that served traffic ("p50",
// linear between the two nearest), where name says which bound for errors.
func loadBound(name, spec string, rates map[time.Time]float64) (float64, error) {
	s := strings.TrimSpace(spec)
	if p, ok := strings.CutPrefix(s, "p"); ok {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || !(v >= 0 && v <= 100) {
			return 0, InputErrorf("%s %q, want a percentile from p0 to p100, or a rate", name, spec)
		}
		var rs []float64
		for _, r := range rates {
			if r > 0 {
				rs = append(rs, r)
			}
		}
		if len(rs) == 0 {
			return 0, InputErrorf("%s %q: no step of the source served traffic to take a percentile of", name, spec)
		}
		slices.Sort(rs)
		x := v / 100 * float64(len(rs)-1)
		lo := int(math.Floor(x))
		hi := min(lo+1, len(rs)-1)
		return rs[lo] + (x-float64(lo))*(rs[hi]-rs[lo]), nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || !(v > 0) || math.IsInf(v, 0) {
		return 0, InputErrorf("%s %q, want a rate above 0 in requests per second, or a percentile, as p50", name, spec)
	}
	return v, nil
}

// LoadFitCycles is the cycles of fit, the whole source a load split's fit
// is read from, in the steps whose rate is below req.FitBelow, to read the
// fit at (SourceRequest.Cycles); nil where req splits not by load.
func LoadFitCycles(fit *Resolved, req BacktestRequest) (*CycleSet, error) {
	if err := checkLoadSplit(req); err != nil || !loadSplitAsked(req) {
		return nil, err
	}
	step, err := backtestStepOf(req)
	if err != nil {
		return nil, err
	}
	rates := stepRates(fit.Series, step)
	below, err := loadBound("fitBelow", req.FitBelow, rates)
	if err != nil {
		return nil, err
	}
	set := &CycleSet{at: map[int64]bool{}, rate: below}
	for _, x := range fit.Series {
		if r, ok := rates[x.At.UTC().Truncate(step)]; ok && r < below {
			set.at[x.At.UnixNano()] = true
		}
	}
	if len(set.at) == 0 {
		return nil, InputErrorf("fitBelow %s (%.4g r/s): no step of the source is below it, nothing to fit on", req.FitBelow, below)
	}
	return set, nil
}

// loadSplit is a load split as a backtest question holds it: the report,
// the test's steps by start, and the steps of the test's span it leaves out.
type loadSplit struct {
	report BacktestLoadSplit
	// outside is the steps of the test's source with a cycle and no test
	// cycle: under the floor, with no rate read, or read by the fit (fitRead).
	outside, fitRead map[time.Time]bool
	// rates is the test's step rates (stepRates), testRate its floor.
	rates    map[time.Time]float64
	testRate float64
}

// loadSplitOf is req's load split over fit, read from the fit steps alone
// (LoadFitCycles), and test, the whole source the test steps are read from:
// the test's cycles, those of its steps at or above req.TestAbove that hold
// no cycle the fit read. One source split so is refused where its fit bound
// is above its test bound: the fit would read steps the test judges.
func loadSplitOf(fit, test *inputsEntry, req BacktestRequest, step time.Duration, oneSource bool) (*loadSplit, []int, error) {
	if req.FitCycles == nil || req.FitCycles.Len() == 0 {
		return nil, nil, InputErrorf("a split by load reads its fit from the steps below fitBelow alone: the fit was read from every cycle")
	}
	testRates := stepRates(test.series, step)
	above, err := loadBound("testAbove", req.TestAbove, testRates)
	if err != nil {
		return nil, nil, err
	}
	fitSteps := map[time.Time]bool{}
	for _, x := range fit.series {
		fitSteps[x.At.UTC().Truncate(step)] = true
	}
	ls := &loadSplit{outside: map[time.Time]bool{}, fitRead: map[time.Time]bool{}, rates: testRates, testRate: above,
		report: BacktestLoadSplit{FitBelow: req.FitBelow, TestAbove: req.TestAbove, FitBelowRate: roundRate(req.FitCycles.rate),
			TestAboveRate: roundRate(above), FitSteps: len(fitSteps)}}
	if below := req.FitCycles.rate; oneSource && below > above {
		return nil, nil, InputErrorf("fitBelow %s (%.4g r/s) is above testAbove %s (%.4g r/s): the fit would read steps the test judges; "+
			"give a fit bound at or under the test's", req.FitBelow, below, req.TestAbove, above)
	}
	var idx []int
	testSteps := map[time.Time]bool{}
	for i, x := range test.series {
		k := x.At.UTC().Truncate(step)
		r, ok := testRates[k]
		switch {
		case !ok || r < above:
			ls.outside[k] = true
		case fitSteps[k]:
			ls.outside[k], ls.fitRead[k] = true, true
		default:
			idx = append(idx, i)
			testSteps[k] = true
		}
	}
	ls.report.TestSteps, ls.report.FitRead = len(testSteps), len(ls.fitRead)
	return ls, idx, nil
}

// roundRate is r to four significant figures, as the split reads in words.
func roundRate(r float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(r, 'g', 4, 64), 64)
	return v
}

// words is the split in words, for BacktestResponse.Split.
func (ls *loadSplit) words() string {
	r := ls.report
	return fmt.Sprintf("by load: fit below %s (%.4g r/s), test at or above %s (%.4g r/s)", r.FitBelow, r.FitBelowRate, r.TestAbove, r.TestAboveRate)
}

// excluded marks a step of the test's span the split leaves out, with the
// rate it was classed by.
func (ls *loadSplit) excluded(out *BacktestStep, start time.Time) {
	if ls.fitRead[start] || ls.outside[start] {
		out.Rate = ls.rates[start]
	}
	_, known := ls.rates[start]
	switch {
	case ls.fitRead[start]:
		out.Excluded, out.Detail = ExcludedLoad, "the fit read a cycle of this step: a backtest scores what the planner did not see"
	case ls.outside[start] && !known:
		out.Excluded, out.Detail = ExcludedLoad, fmt.Sprintf("no cycle of it read an arrival rate to class it by against the test's floor of %.4g r/s (testAbove %s)",
			ls.testRate, ls.report.TestAbove)
	case ls.outside[start]:
		out.Excluded, out.Detail = ExcludedLoad, fmt.Sprintf("its rate is under the test's floor of %.4g r/s (testAbove %s)", ls.testRate, ls.report.TestAbove)
	}
}
