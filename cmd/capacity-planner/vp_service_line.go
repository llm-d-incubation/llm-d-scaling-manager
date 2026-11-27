package main

// --prefill-vp-service-line turns V_P from the prefill service line on for
// one answer (issues llm-d-incubation/llm-d-scaling-manager#368, #416): the
// replay switch saturation.SwitchPrefillVPServiceLine, which is not in
// learnreplay.PlannerSwitches, so it is off unless asked. It is read where
// --prefill-vp-plateau is (answerFlags.replayOptions): every command that
// replays a recording reads its state there, the server and day-plan,
// backtest and predict through whatifapi.InputsRequest.PrefillVPServiceLine.
// The line failed the public cross-validation (figures newly under what
// was measured, worse self-checks), and a V_P priced from it carries no flag
// of its own, so the flag is in flagsOutside: set, every figure reads
// OUTSIDE VALIDATED REGION.

// vpServiceLineFlag is the flag's name, the switch's own.
const vpServiceLineFlag = "prefill-vp-service-line"

// vpServiceLineUsage is the flag's help, on every command that takes it.
const vpServiceLineUsage = "opt-in, not validated: where no prefill pod queued (a V_P read under a queue stands), " +
	"read V_P from the prefill service line, the engines' prefill time against their computed tokens a request, " +
	"its fixed part s0 kept in the capacity, and price the prefill knee with s0 paid once a request at the asked prompt. " +
	"Every figure reads OUTSIDE VALIDATED REGION. Off by default"
