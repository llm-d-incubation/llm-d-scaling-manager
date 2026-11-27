package whatifapi

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// This file holds the server's defaults for the rank mixture's switches
// beyond the model's name: the variant's modes and the shortest fit window
// (Config RankMixtureKVHold, RankMixtureRemainderSpread,
// RankMixtureMinFitWindow), read where a backtest or a source's request
// does not set them, as Config.TTFTModel is.

// minFitWindowOf is a request's rankMixtureMinFitWindow as a duration, 0
// where it is absent, is the default (whatif.MinSpreadFitWindow), or does
// not parse (checkMinFitWindow refuses it).
func minFitWindowOf(s string) time.Duration {
	d, ok := parseFitWindow(s)
	if !ok || d == whatif.MinSpreadFitWindow {
		return 0
	}
	return d
}

// parseFitWindow is s as a positive duration; ok is false where it is not.
func parseFitWindow(s string) (time.Duration, bool) {
	d, err := time.ParseDuration(s)
	return d, err == nil && d > 0
}

// checkMinFitWindow refuses a rankMixtureMinFitWindow that is not a
// positive duration, or one given with no TTFT model to fit.
func checkMinFitWindow(s, model string) error {
	if s == "" {
		return nil
	}
	if model == "" {
		return InputErrorf("rankMixtureMinFitWindow is the experimental TTFT model's: give it with ttftModel %q or %q",
			whatif.QueueRankMixture, whatif.QueueRankMixtureNLLine)
	}
	if _, ok := parseFitWindow(s); !ok {
		return InputErrorf("rankMixtureMinFitWindow %q, want a positive duration such as 4h (the default is %s)", s, whatif.MinSpreadFitWindow)
	}
	return nil
}

// rankMixtureDefaults sets on req, a backtest whose TTFTModel the server
// has already defaulted, the server's switches it does not set: the
// shortest fit window wherever a model is asked, and the variant's modes
// where the variant is.
func (s *Server) rankMixtureDefaults(req *BacktestRequest) {
	if req.TPOTModel != "" && req.TPOTDistMinFitSteps == 0 {
		req.TPOTDistMinFitSteps = s.tpotMinFitSteps
	}
	if req.TTFTModel == "" {
		return
	}
	if req.RankMixtureMinFitWindow == "" && s.minFitWindow > 0 {
		req.RankMixtureMinFitWindow = s.minFitWindow.String()
	}
	if req.TTFTModel == string(whatif.QueueRankMixtureNLLine) {
		req.RankMixtureKVHold = req.RankMixtureKVHold || s.kvHold
		req.RankMixtureRemainderSpread = req.RankMixtureRemainderSpread || s.remainderSpread
		req.RankMixtureRemainderServer = req.RankMixtureRemainderServer || (s.remainderServer && req.RankMixtureRemainderSpread)
	}
}

// mainTTFTOptions is how the main view fits req's model (req.TTFTModel,
// already defaulted): the request's switches, else the server's, checked
// as a backtest checks them. Stratified is the caller's to set.
func (s *Server) mainTTFTOptions(req InputsRequest) (TTFTModelOptions, error) {
	o := TTFTModelOptions{Model: req.TTFTModel, KVHold: req.RankMixtureKVHold, RemainderSpread: req.RankMixtureRemainderSpread,
		RemainderServer: req.RankMixtureRemainderServer}
	if err := checkMinFitWindow(req.RankMixtureMinFitWindow, req.TTFTModel); err != nil {
		return o, err
	}
	// The modes are defaulted before they are checked, as a backtest's are
	// (rankMixtureDefaults): the remainder server asked for with the
	// server's remainder spread is not refused for want of the request's.
	if req.TTFTModel == string(whatif.QueueRankMixtureNLLine) {
		o.KVHold = o.KVHold || s.kvHold
		o.RemainderSpread = o.RemainderSpread || s.remainderSpread
		o.RemainderServer = o.RemainderServer || (s.remainderServer && o.RemainderSpread)
	}
	if err := o.Check(); err != nil {
		return o, err
	}
	if req.TTFTModel == "" {
		return o, nil
	}
	o.MinFitWindow = minFitWindowOf(req.RankMixtureMinFitWindow)
	if req.RankMixtureMinFitWindow == "" {
		o.MinFitWindow = s.minFitWindow
	}
	return o, nil
}

// checkRemainderServer refuses rankMixtureRemainderServer without the
// variant and its remainder spread, whose shape it is drawn on.
func checkRemainderServer(server, spread bool, model string) error {
	if server && (model != string(whatif.QueueRankMixtureNLLine) || !spread) {
		return InputErrorf("rankMixtureRemainderServer draws on the remainder spread of ttftModel %q: give it with that model "+
			"and rankMixtureRemainderSpread", whatif.QueueRankMixtureNLLine)
	}
	return nil
}

// MinTPOTDistFitStepsFloor is the fewest fit steps the TPOT distribution
// may be allowed to fit on: its two self-check folds fit on half and three
// quarters of the steps, so each reads at least two.
const MinTPOTDistFitStepsFloor = 4

// checkTPOTMinFitSteps refuses a tpotDistMinFitSteps under
// MinTPOTDistFitStepsFloor, or one given with no TPOT model to fit; 0 is
// the default.
func checkTPOTMinFitSteps(n int, model string) error {
	switch {
	case n == 0:
		return nil
	case model == "":
		return InputErrorf("tpotDistMinFitSteps is the experimental TPOT distribution's: give it with tpotModel %q", whatif.TPOTRenewalLognormal)
	case n < MinTPOTDistFitStepsFloor:
		return InputErrorf("tpotDistMinFitSteps %d, want %d or more (the default is %d)", n, MinTPOTDistFitStepsFloor, MinTPOTDistFitSteps)
	}
	return nil
}

// tpotMinFitStepsOf is the fewest fit steps a source's TPOT distribution
// fits on: the request's, else the server's; 0 the default.
func (s *Server) tpotMinFitStepsOf(req InputsRequest) (int, error) {
	if err := checkTPOTMinFitSteps(req.TPOTDistMinFitSteps, req.TPOTModel); err != nil {
		return 0, err
	}
	n := req.TPOTDistMinFitSteps
	if n == 0 && req.TPOTModel != "" {
		n = s.tpotMinFitSteps
	}
	if n == MinTPOTDistFitSteps {
		return 0, nil
	}
	return n, nil
}

// flagStrings is flags as a figure carries them.
func flagStrings(flags []whatif.Flag) []string {
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = string(f)
	}
	return out
}
