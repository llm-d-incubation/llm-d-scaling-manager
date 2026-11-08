package slo

// The KV a resident request holds, with output lengths that vary (issue ev-shindin/llm-scaling-manager#276;
// the same correction as saturation's, kv_output_dispersion.go there).
//
// A long request stays resident longer, so a KV snapshot finds it in
// proportion to how long it stays, and the average resident request holds
// ILeff + D*OL/2, D = E[O^2]/E[O]^2 (shape.WithOutputDispersion). At D taken as
// 1 the line's fill read too many sequences, by 7-44% at IL = 2 OL, and the
// analyzer asked for too few replicas. Here KVreq only sets how many sequences
// a fill holds, so a larger D can only lower a replica's rate: saturation's
// other figure, k2, which rises with D, has no counterpart in this analyzer.
//
// As there, D is read from a lower bound on E[O^2], and is no spread with no
// reading: from a request-weighted mean over the rows the shape counts, and
// only when every one of them read the bound (readFleet). Unlike there it is
// not held at 1 across a change of shape: this analyzer tracks none. The
// histogram then reads high, and the side that errs on is more replicas.
//
// With no reading a priced decode line says so: whatif.OutputSpreadUnread on the
// TTFT answer, on S2's no-act list.

// outputSpreadUnread is a fleet whose KV per request read no spread, with the
// switch on: off, nothing is priced from one, and nothing is said of it.
func (a *Analyzer) outputSpreadUnread(fleet fleetReading) bool {
	return a.kvOutputDispersion && fleet.outputDispersion == 0
}

// SwitchKVPerRequestOutputDispersion is the replay switch for the spread of
// output lengths in the KV per request on the slo line (issue ev-shindin/llm-scaling-manager#276). On, the
// line prices a resident request at ILeff + D*OL/2, D the spread read from the
// output-length histogram. Off, as before, at ILeff + OL/2.
const SwitchKVPerRequestOutputDispersion = "slo-kv-per-request-output-dispersion"
