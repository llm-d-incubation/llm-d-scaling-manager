package saturation

// SwitchBacktestStepOutputDispersion is the replay switch for the output
// spread a capacity-planner backtest prices each step at (issue
// llm-d-incubation/llm-d-scaling-manager#274). A backtest prices each step
// at the step's own prompt and output lengths, but at the output spread D
// (SwitchKVPerRequestOutputDispersion) the fit read in its last minutes: on
// the public 0.6B staircase the warm-up ends on a few minutes of 8 to
// 26-token outputs whose histograms read D 1.26, and the 1000-token A phases
// it is tested on run at D 1.00. The KV per request read 11% high there,
// and near the knee the Little's-law fill rose 25%. On, each step is
// priced at the D its decode rows' output histograms read
// (whatifapi.Knobs.OutputDispersion), as its prompt spread already is; a
// step that read none keeps the fit's. The analyzer accepts the switch and
// does nothing with it: the planner reads it (learnreplay.State.StepOutputDispersion),
// and the controller never prices from it. Planner-only: off from the
// constructor, on in the planner (learnreplay.PlannerSwitches).
const SwitchBacktestStepOutputDispersion = "backtest-step-output-dispersion"
