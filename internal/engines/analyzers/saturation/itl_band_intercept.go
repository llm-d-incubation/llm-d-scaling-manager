package saturation

// SwitchITLBandIntercept is the replay switch for a band that carries the
// pinned ITL line's intercept error (whatif.Refit.DrawB; issue
// llm-d-incubation/llm-d-scaling-manager#307). On, every bootstrap resample
// fitted pinned-B runs through its own intercept, drawn with median the
// pinned B, each band holds its own figure and is widened for the few blocks
// of cycles it was drawn from, which it names (whatif.Interval.Blocks). Off,
// a resample keeps the pinned B and the band is the resamples' 10th to 90th
// percentile, as before this switch.
//
// The analyzer accepts the switch and does nothing with it: the bands are
// the capacity planner's (learnreplay.StatesFrom reads the switch from its
// own options), and the controller prices no band today. Planner-only and
// off from the constructor; the planner's replay turns it on
// (learnreplay.PlannerSwitches). A controller that comes to band its figures
// with whatif.BootstrapLines and whatif.BandAnswer adopts it by turning it on
// in its own constructor, after its own validation.
const SwitchITLBandIntercept = "itl-band-intercept"
