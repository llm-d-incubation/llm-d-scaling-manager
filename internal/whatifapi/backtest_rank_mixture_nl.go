package whatifapi

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// This file holds what the backtest adds for the experimental rank-mixture
// variant whatif.QueueRankMixtureNLLine (BacktestRequest.TTFTModel): Z
// measured part by part on v2's queue and service line
// (whatif/rank_mixture_nl.go). It is fitted on the backtest's fit window as
// v2 is (fitRankMixtureSteps, whatif.FitRankMixtureCycles with
// RankMixtureSwitches.NLLine), prices each test step the same way, and
// abstains where its own checks fail (whatif.RankMixtureFitted.Identified).

// RankMixtureZParts is Z measured part by part, seconds and seconds a
// prompt token, and the parts' spreads (whatif.RankMixtureZParts).
type RankMixtureZParts = whatif.RankMixtureZParts

// RankMixtureServer is the remainder server's fit, seconds and seconds a
// prompt token (whatif.RankMixtureServer).
type RankMixtureServer = whatif.RankMixtureServer
