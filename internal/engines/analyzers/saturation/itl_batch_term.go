package saturation

// The ITL batch term past the roofline (issue
// llm-d-incubation/llm-d-scaling-manager#309).
//
// A decode step reads the weights once (a constant), reads every resident
// KV token (linear in the fill), and, once the batch's tokens per engine rank
// pass the roofline, spends GEMM time that grows with the batch. The ITL line
// prices the first two; at one KV per request the third moves with the fill
// and the line absorbs it, but a shape with less KV per request runs more
// sequences at the same fill, and the line reads those low.
//
// The term is a*max(0, n*(1+k_spec) - n_roof), n the running sequences per
// engine rank and k_spec the speculative tokens per step. The capacity
// planner fits it on the residuals of the variant's line over its decode rows
// (learnreplay, whatif.FitBatchTerm) and prices it on top of the line
// (whatif.Decode.Batch). The controller's analyzers read nothing of it: the
// switch only names the change, so the planner's replay can turn it on.

// SwitchITLBatchTerm is the replay switch for the batch term. On, the
// capacity planner's replay fits the term over each decode variant's rows
// and the planner prices it; off, the line alone is priced. The analyzers
// behave the same either way. Planner-only: off from the constructor, and
// not in learnreplay.PlannerSwitches (see there).
const SwitchITLBatchTerm = "itl-batch-term"
