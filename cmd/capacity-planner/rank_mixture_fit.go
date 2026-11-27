package main

import (
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The rank mixture's fit flags beyond the model's name, the same on every
// command that fits it (backtest, serve, at-replicas, day-plan): the
// variant's two modes (#450) and the shortest fit window either model
// prices from. Each maps onto whatifapi's request fields
// (BacktestRequest RankMixtureKVHold, RankMixtureRemainderSpread,
// RankMixtureMinFitWindow) or, where the CLI fits the model itself, onto
// whatifapi.TTFTModelOptions.

// rankMixtureMinFitWindowFlag is the shortest fit window's flag.
const rankMixtureMinFitWindowFlag = "rank-mixture-min-fit-window"

// rankMixtureMinFitWindowUsage is its help.
const rankMixtureMinFitWindowUsage = "with --experimental-ttft=v2 or =v2nl-line, the shortest fit window the model prices from; " +
	"under it the model abstains. Default 12h. A lab recording is shorter: lower it (2h) to fit the model there, and every TTFT " +
	"it prices from a window under 12h is flagged RankMixtureShortFitWindow"

// rankMixtureKVHoldUsage and rankMixtureRemainderSpreadUsage are the
// variant's modes' help, as backtest gives it.
const (
	rankMixtureKVHoldUsage = "with --experimental-ttft=v2nl-line, read the fitted KV-deferral wait no further than the fit window's " +
		"95th percentile rank utilization: held past it, not extrapolated; off by default"
	rankMixtureRemainderSpreadUsage = "with --experimental-ttft=v2nl-line, draw the router's remainder from the router's measured " +
		"request-processing time where the fit window recorded it (its line where not, as off); off by default"
	rankMixtureRemainderServerUsage = "with --experimental-ttft=v2nl-line and --rank-mixture-remainder-spread, draw the router's " +
		"remainder from the prefill pods' API servers (their render and HTTP handlers against vLLM's end-to-end time) and the " +
		"router's own work, on the spread's shape, where the fit window recorded them (the plain spread where not); off by default"
)

// rankMixtureRemainderServerFlag is the remainder server's flag.
const rankMixtureRemainderServerFlag = "rank-mixture-remainder-server"

// rankMixtureFit is the fit flags one command was given.
type rankMixtureFit struct {
	kvHold, remainderSpread, remainderServer bool
	minFitWindow                             time.Duration
}

// registerModes registers the variant's modes.
func (r *rankMixtureFit) registerModes(fs *flag.FlagSet) {
	fs.BoolVar(&r.kvHold, "rank-mixture-kv-hold", false, rankMixtureKVHoldUsage)
	fs.BoolVar(&r.remainderSpread, "rank-mixture-remainder-spread", false, rankMixtureRemainderSpreadUsage)
	r.registerServer(fs)
}

// registerServer registers the remainder server mode.
func (r *rankMixtureFit) registerServer(fs *flag.FlagSet) {
	fs.BoolVar(&r.remainderServer, rankMixtureRemainderServerFlag, false, rankMixtureRemainderServerUsage)
}

// checkRemainderServer refuses the remainder server without the variant and
// its remainder spread.
func checkRemainderServer(server, spread, nlLine bool) error {
	if server && (!nlLine || !spread) {
		return usagef("--%s draws on the remainder spread of the rank mixture's variant: give it with "+
			"--experimental-ttft=v2nl-line and --rank-mixture-remainder-spread", rankMixtureRemainderServerFlag)
	}
	return nil
}

// registerWindow registers the shortest fit window.
func (r *rankMixtureFit) registerWindow(fs *flag.FlagSet) {
	fs.DurationVar(&r.minFitWindow, rankMixtureMinFitWindowFlag, whatif.MinSpreadFitWindow, rankMixtureMinFitWindowUsage)
}

// check refuses the flags where the model they change is not asked: the
// modes without =v2nl-line, the window without =v2 or =v2nl-line, and a
// negative window (0, as in a zero rankMixtureFit, is the default).
func (r rankMixtureFit) check(v2, nlLine bool) error {
	switch {
	case (r.kvHold || r.remainderSpread) && !nlLine:
		return usagef("--rank-mixture-kv-hold and --rank-mixture-remainder-spread are modes of the rank mixture's variant: " +
			"give them with --experimental-ttft=v2nl-line")
	case r.remainderServer && (!nlLine || !r.remainderSpread):
		return checkRemainderServer(r.remainderServer, r.remainderSpread, nlLine)
	case r.minFitWindow < 0:
		return usagef("--%s %s, want a duration such as 2h", rankMixtureMinFitWindowFlag, r.minFitWindow)
	case r.window() > 0 && !v2:
		return usagef("--%s changes the fit of the experimental TTFT model: give it with --experimental-ttft=v2 or =v2nl-line",
			rankMixtureMinFitWindowFlag)
	}
	return nil
}

// warnKVHoldAlone warns where the KV hold is asked without the
// remainder's spread, on every command that takes the modes: alone it
// prices low past the hold.
func warnKVHoldAlone(warn io.Writer, kvHold, remainderSpread bool) {
	if kvHold && !remainderSpread {
		fmt.Fprintln(warn, "capacity-planner: warning: --rank-mixture-kv-hold without --rank-mixture-remainder-spread: "+
			"held alone it lowers every step priced past the hold, and on a production recording it read one more mean "+
			"under measured; the two together did not, so give both")
	}
}

// window is the shortest fit window asked, 0 at the default.
func (r rankMixtureFit) window() time.Duration {
	if r.minFitWindow == whatif.MinSpreadFitWindow {
		return 0
	}
	return r.minFitWindow
}

// windowField is the window as a request field, "" at the default.
func (r rankMixtureFit) windowField() string {
	if d := r.window(); d > 0 {
		return d.String()
	}
	return ""
}

// options is how the CLI fits model itself (whatifapi.ApplyTTFTModel,
// Resolved.TTFTModel), stratified as given.
func (r rankMixtureFit) options(model string, stratified bool) whatifapi.TTFTModelOptions {
	return whatifapi.TTFTModelOptions{Model: model, Stratified: stratified, KVHold: r.kvHold, RemainderSpread: r.remainderSpread,
		RemainderServer: r.remainderServer, MinFitWindow: r.window()}
}

// tpotDistMinFitStepsFlag is the TPOT distribution's fewest-fit-steps flag,
// the TPOT model's counterpart of --rank-mixture-min-fit-window.
const tpotDistMinFitStepsFlag = "tpot-dist-min-fit-steps"

// tpotDistMinFitStepsUsage is its help.
const tpotDistMinFitStepsUsage = "with --experimental-tpot=v2, the fewest 5-minute fit steps the distribution fits on; under it the " +
	"distribution abstains. Default 24 (two hours). A lab recording reads fewer: lower it (8), at least 4, to fit the distribution " +
	"there, and every percentile it prices from fewer than 24 steps is flagged TPOTDistributionShortFit"

// tpotDistFit is the TPOT distribution's fewest fit steps one command was
// given.
type tpotDistFit struct{ minFitSteps int }

// register registers the flag.
func (d *tpotDistFit) register(fs *flag.FlagSet) {
	fs.IntVar(&d.minFitSteps, tpotDistMinFitStepsFlag, whatifapi.MinTPOTDistFitSteps, tpotDistMinFitStepsUsage)
}

// steps is the fewest fit steps asked, 0 at the default (and in a zero
// tpotDistFit, as a test builds one).
func (d tpotDistFit) steps() int {
	if d.minFitSteps == whatifapi.MinTPOTDistFitSteps {
		return 0
	}
	return d.minFitSteps
}

// check refuses the flag without --experimental-tpot=v2, and a count under
// whatifapi.MinTPOTDistFitStepsFloor.
func (d tpotDistFit) check(v2 bool) error {
	switch n := d.steps(); {
	case n == 0:
		return nil
	case n < whatifapi.MinTPOTDistFitStepsFloor:
		return usagef("--%s %d, want %d or more", tpotDistMinFitStepsFlag, n, whatifapi.MinTPOTDistFitStepsFloor)
	case !v2:
		return usagef("--%s changes the fit of the experimental TPOT distribution: give it with --%s=%s",
			tpotDistMinFitStepsFlag, experimentalTPOTFlag, experimentalTPOTV2)
	}
	return nil
}
