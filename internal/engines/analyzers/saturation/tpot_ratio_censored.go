package saturation

// SwitchTPOTRatioCensored is the replay switch for the capacity planner's
// R_q read by interval-censored maximum likelihood (whatif.TPOTRatioCensored)
// rather than at the q bucket's upper edge plus an order-statistic margin
// (whatif.TPOTRatioFromWindows): a lognormal at each window's own mean, one
// spread shared by every window, is fitted to the bucket counts, the edge
// reading kept as its top (issue
// llm-d-incubation/llm-d-scaling-manager#279). The analyzer accepts the
// switch and does nothing with it: R_q is the planner's reading of the TPOT
// windows its replay collects (learnreplay.State.TPOTRatioCensored), and the
// controller never prices from it. Planner-only and off from the
// constructor; the planner's replay does not turn it on yet
// (learnreplay.PlannerSwitches).
const SwitchTPOTRatioCensored = "tpot-ratio-censored"
