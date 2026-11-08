# The ITL line of an aggregated pod, by request shape

**Status:** built for issue ev-shindin/llm-scaling-manager#275, 2026-11-03. Builds on ev-shindin/llm-scaling-manager#432, which fits a
line for an aggregated variant (prefill and decode in one pod, no role label)
and reports the mu it derives without pricing it. The code is
`internal/engines/analyzers/saturation/itl_aggregated_shape.go`.

## The problem

An aggregated pod runs the prompts it admits as chunks inside its decode
steps. So its step time, the ITL, carries prefill work, and how much depends
on the request shape. A line fitted at one shape mis-prices another, in
either direction: at 1000 in / 500 out about 40% of the ITL is prefill.
ev-shindin/llm-scaling-manager#432 therefore reports the figure and sizes nothing from it.

Two more errors sit in the derived mu:

- A sequence being prefilled holds KV and emits no token. The mu over-states
  by about `TTFT / (OL * ITL)`.
- A line kept past a shape change (a held slope, a restored line, a borrowed
  `|both` line) prices the old shape for as long as it is kept.

## The model

One step of an aggregated pod takes

```
ITL(k, x) = B + A*k + c*x
```

- `k` is the KV fill, as for decode.
- `x` is the prefill tokens computed per step, about `n * IL_eff / OL`:
  `n` resident sequences, `IL_eff` the prompt after prefix-cache hits, `OL`
  the output length.
- `B` and `A` are the engine's, as for decode. `c` is the cost of one
  prefill token in a step.

At a fixed shape `n = k * C / KVreq`, so `x = g * k` with

```
g = C * IL_eff / (KVreq * OL)
```

and the line the tracker already fits is `B + (A + c*g)*k`. Its slope at
shape `g` is `A_g = A + c*g`. So:

- the existing fit is kept unchanged, and it measures `A_g` at one shape;
- `c` is read off the slopes of **two or more shapes**, never from one.

`g` is computed from the shape the derived mu is priced at (`IL_eff`,
`KVreq`, `OL`), at the variant's median KV capacity and its hit rate
weighted by the prompt tokens each replica serves. A cold replica that has
just joined serves little and barely moves it; a median over replicas
would flip when the cold ones are the majority.

## What is fitted, from which readings

**The line.** As ev-shindin/llm-scaling-manager#432: the same tracker, band, settle and cap admission.
Every line carries the shape it was fitted at, `gFit`: a fit made this
cycle, a held slope, a restored line and a borrowed `|both` line.

**A shape point for `c`.** After a fit, the variant may add a point
`(gFit, A_g)` to its line key. It is admitted only when all hold:

- the variant's own line, not held and not borrowed;
- tier `ols`, or `pinned-B` on a learned intercept. A slope pinned to
  `DefaultBaselineSec` is not a measurement of `A_g`;
- every reading in the window was taken at this shape (see the shape change
  below), outside the 5-minute shape hold;
- the prefill steps are known (below).

A point within 20% of an existing point's `g` replaces it (the newest wins),
so a flat shape refreshes one point and never builds a spread. At most six
points are kept per key.

**Identifying `c`.** With two or more points whose largest `g` is at least
1.5 times the smallest, a least-squares fit of `A_g` on `g` gives `A` and
`c`. It is refused, and `c` stays unidentified, unless `c >= 0` and `A > 0`.
It is refitted whenever a point is added.

**When the shape is flat.** One point, or points too close: `c` is not
identified, and no default is pinned. A `c` identified earlier stays with
the line key's points through a flat period, for as long as the key lives.

**The prefill steps.** By Little's law, a replica holding `n =
RunningRequests` sequences and completing `RequestRate` keeps each for
`n / (RequestRate * ITL)` steps, `D` of them decode: `D = OL`, or
`OL / (1 + s)` on an engine that speculates and accepts `s` draft tokens a
step. The rest, per prompt token, is `sigma`. The difference of two
near-equal numbers is noisy, so:

- a reading whose residence is under `D` steps is refused and counted
  (`prefillRefused`), never read as a negative prefill. Refusing only that
  side biases `sigma` up, so mu down: the safe side;
- `sigma` is clamped at 0 and is the median of the last 20 readings, at
  least 3;
- the readings are taken inside the ITL band, outside a shape change, and
  dropped with the window when the shape moves.

No new metric is needed. Three cases have no `sigma` and stay reported
only, each logged once:

- no running-request count or no completion rate on any replica
  (`itl-aggregated-no-occupancy`);
- SGLang (`itl-aggregated-sglang-unverified`). It does publish a
  completion rate per pod, but its running count may leave out requests in
  chunked prefill, which would read `sigma` as about 0 and over-state mu.
  Not verified, so not sized from;
- an engine that speculates (MTP, a draft model) with no accepted-token
  count on any replica (`itl-aggregated-spec-unknown`): `D` is unknown.

## How the derived mu is computed

For the shape priced now, `g*`, and the line's `(A_g, gFit)`:

1. `g* <= gFit`: price `A_g` as it is. A lighter shape is priced at the
   measured slope, never extrapolated toward a better price: with `c >= 0`
   the measured slope already states its ITL too high.
2. `g*` heavier, `c` identified: price `A_g + c * (g* - gFit)`, however
   small the move. This only ever raises the ITL and lowers mu, so an
   over-estimated `c` over-provisions and never under-provisions.
3. `g*` heavier by at most 20%, no `c`: price the worst case,
   `A_g * g* / gFit`. Since `A_g = A + c * gFit` with `A >= 0`, `c` is at
   most `A_g / gFit`, so the true slope `A + c * g*` is never above it.
   A line is never priced lighter than its shape, with no exception.
4. `g*` heavier by more, no `c`: no figure (`priceCase=shape-unpriced`),
   reported or priced. A line with no `gFit` (from a ev-shindin/llm-scaling-manager#432 build) has none
   either.

Then, with the occupancy correction:

```
mu = mu(k, ITL) * D / (D + sigma * IL_eff)
```

By Little's law a resident sequence completes once per
`(D + sigma * IL_eff)` steps, not once per `D`. On an engine that
speculates the line is per token (issue ev-shindin/llm-scaling-manager#274), so its rate is already in
tokens and the GPS check compares it as it is. With `sigma`
unknown the figure is reported uncorrected and never sizes.

## When report-only lifts

Per variant, in memory. The figure sizes once, at the current shape and
outside the shape hold:

- it has a figure (case 1 or 2) and `sigma`; and
- the GPS check, with the shape term and the correction, agrees within 15%
  on the median comparable replica (`k >= 0.30`) on **3 consecutive
  comparable cycles**.

A lifted figure never prices above a measured rate of the variant's own: the
lower of the two prices the replica, the same side-safe rule as a stale
measured rate that disagrees with derived.

It goes back to report-only on 3 consecutive disagreeing cycles, when the
shape moves, when the figure or `sigma` is missing, while a shape change is
outstanding, on a change of engine arguments, and on a controller restart.
`itl-aggregated-lift` logs each move; `llmsm_service_rate_per_replica` shows
`source="derived-report-only"` before and `source="derived-aggregated"` after.

## A shape change

When `g*` moves more than 20% from the shape of the variant's window, and
the move holds for 2 consecutive cycles, the window's readings and its
prefill readings are dropped (`itl-aggregated-shape-moved`, at `--v=3`), as
a change of engine arguments drops them. A move that has not held yet
moves no lift: a scale-up's cold replicas moving the hit rate for a cycle
is not a shape change.
Kept: the intercept `B` (it does not depend on shape), the held slope with
its `gFit`, and the line key's points. So:

- the next fit is taken at the new shape only;
- meanwhile the held, restored or borrowed line prices a heavier shape only
  through `c`, and a lighter one at its measured slope. The stale line of
  ev-shindin/llm-scaling-manager#432 can no longer price after the shape hold;
- a lifted variant drops to report-only and earns the lift again.

## Persistence and rollback

The learned-state document stays version 1. One map is added, `omitempty`:
`itlLineShapes`, keyed like `itlLines`, with the line's `g`, `sigma` and the
points. `c` is refitted from the points on restore, never read. A held slope
restored across a restart takes its shape from the line it published.

A document from ev-shindin/llm-scaling-manager#432 has no `itlLineShapes`: its aggregated lines restore
with no shape and price nothing until fitted again. A build before this one
ignores the map: ev-shindin/llm-scaling-manager#432 keeps reporting only. Forgetting a variant drops its
shape state, and a change of engine arguments drops it with the window.
Decode lines do not change.

## Replay switches

Off, each does what ev-shindin/llm-scaling-manager#432 does.

- `itl-aggregated-shape-term`: the shape tags, the window drop, the points,
  `c` and the corrected figure. Alone it moves `learned-state.csv` of the
  acceptance and engine fixtures: the line's shape joins the export.
- `itl-aggregated-sizes`: the lift. It needs the shape term, and moves no
  golden: no fixture records a prefill reading for an aggregated variant.
  Its unit tests hold it.

ev-shindin/llm-scaling-manager#277's switch (`kv-per-request-hits-resident`) now also moves those two
files: `g` is priced from the KV a request holds, which ev-shindin/llm-scaling-manager#277 bounds.

## Limits

- One shape per fleet: `g*` is the fleet's shape, so a variant serving a
  shape of its own is priced at the average (as ev-shindin/llm-scaling-manager#432).
- `sigma` is measured at the current load and priced at the pricing fill.
  At a higher fill decode takes more of each step's token budget and
  prefill stretches, so mu is a little high there.
- A per-sequence cost (a term in `n`, not `k`) moves with `KVreq` across
  shapes, and `c` absorbs part of it.
- The thresholds (20%, 1.5x, 2 and 3 cycles, 6 points, 3 and 20 readings) are
  first choices, not measured.
- The slo analyzer's line has the same shape dependence. It can reuse `c`
  later; it is out of scope here.
