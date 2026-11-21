package saturation

// SwitchDecodeResidencePull is the replay switch for the capacity planner's
// decode residence from the KV pull: a P/D decode replica holds a request's
// KV from the pull, not from its first token, so its residence is the
// decode engine's TTFT plus OL steps (whatif.Decode.HeldBefore), in the
// fill Little's law reads at a rate and in the knee (issue
// llm-d-incubation/llm-d-scaling-manager#301). The analyzer accepts the
// switch and does nothing with it: the planner reads it
// (learnreplay.State.DecodeResidencePull), and the controller never prices
// from it. Planner-only and off from the constructor; the planner's replay
// does not turn it on yet (learnreplay.PlannerSwitches).
const SwitchDecodeResidencePull = "decode-residence-pull"
