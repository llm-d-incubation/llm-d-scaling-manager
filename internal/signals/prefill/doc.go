// Package prefill learns, from the rows the collector gathers today, the three
// figures the end-to-end TTFT model needs of a prefill pod
// (docs/proposals/slo-capacity-planner-ttft.md):
//
//   - V_P, the pod's prompt-token throughput while it has work ("Prefill
//     capacity"), taken only from sources that cannot over-state it;
//   - a, the fixed latency one request pays on the pod, measured at low load
//     ("What is derivable today");
//   - f, the busiest pod's share of prefill work over the mean share ("The
//     engine queue").
//
// It observes only (PR P1 of docs/proposals/slo-capacity-planner.md): the
// saturation analyzer feeds it each prefill variant's rows and logs what it
// learned, and nothing sizes from it. It holds no persisted state and no
// hold: the page asks for neither in P1.
//
// It follows the learner rules (docs/proposals/analyzer-learner.md): readings
// are admitted by state (the settle tracker, a steady prompt length, the
// queue), then fitted untrimmed (section 5); every guard is written so that
// NaN fails it; and each figure it cannot support is refused with a reason
// rather than guessed.
package prefill
