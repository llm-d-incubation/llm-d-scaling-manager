package saturation

// SwitchDecodeBurstFill is the replay switch for the capacity planner's
// decode fill under clustered arrivals: with a stated arrival Ca^2 above 1,
// a request runs beside more sequences than Little's law's time average,
// (Ca^2 - 1)/N_d more, so its steps, and the mean TPOT, are read at that
// fill (whatif.Decode.ArrivalCV2; issue
// llm-d-incubation/llm-d-scaling-manager#291). The analyzer accepts the
// switch and does nothing with it: the planner reads it
// (learnreplay.State.DecodeBurstFill), and the controller never prices
// from it. Planner-only and off from the constructor.
const SwitchDecodeBurstFill = "decode-burst-fill"
