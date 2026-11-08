package saturation

// The KV a resident request holds, with output lengths that vary (issue #276).
//
// KVreq = ILeff + OL/2 is right when every request generates the same number
// of tokens. When lengths vary, a long request stays resident longer, so a KV
// snapshot finds it in proportion to how long it stays, and the average
// resident request holds ILeff + E[O^2]/(2 E[O]) = ILeff + D*OL/2, D =
// E[O^2]/E[O]^2 (shape.WithOutputDispersion). With D taken as 1 the resident
// request count was over-counted by 7% (uniform lengths), 20% (exponential)
// and 44% (lognormal, cv 1.5) at IL = 2 OL.
//
// D is read from the engine's output-length histogram as a lower bound
// (domain.SquaredMeanLowerBound, domain.ReplicaMetrics.AvgOutputTokensSquared),
// so it moves the KV per request towards the truth and never past it. Equal
// lengths read as D = 1, and with no histogram (an engine that publishes none,
// a relabelling that drops it) nothing changes. Nor does it while a shape
// change is outstanding: the [5m] histogram then holds both shapes
// (resolvePricing, replicaOutputDispersion).
//
// It moves two figures, in opposite directions, both towards the truth:
//   - the derived k2 (P3, estimateCapacityAtCapSpread) rises: the sequences
//     the batch cap admits hold more KV each, so a replica holds more before
//     its batch binds, and fewer replicas are ordered;
//   - the derived mu (whatif.DeriveMuAtCap) falls where the KV binds rather than the
//     batch cap: fewer sequences fit at the pricing point, so a replica
//     serves fewer requests a second, and more replicas are ordered.
//
// The GPS check (noteLineMismatch) predicts the token rate from the same
// fleet shape, so it moves with mu.

// SwitchKVPerRequestOutputDispersion is the replay switch for the spread of
// output lengths in the KV per request (issue #276). On, the derived k2 and
// the derived mu price a resident request at ILeff + D*OL/2, D the spread
// read from the output-length histogram. Off, as before, at ILeff + OL/2.
const SwitchKVPerRequestOutputDispersion = "kv-per-request-output-dispersion"
