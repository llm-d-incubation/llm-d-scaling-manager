# One fitted model for the controller and the capacity planner

**Status:** proposed. Nothing here is built. Depends on PR #448 (the
controller's v2 latency learners), which is open and does not merge cleanly
with `main` today.

**Audience:** contributors to the slo analyzer, the learned-state store and
`capacity-planner serve`.

## What this is for

The planner panel has two tabs for one model (issue #486): **Live** (what the
controller computes now) and **What if** (the same model at replicas, load and
targets the operator picks). The tabs must agree at the live point. Today they
can disagree, because each fits its own model.

This proposal makes one fit:

1. **The controller owns the live fit.** It writes the fitted parameters to a
   learned-state ConfigMap, one per managed namespace, as a versioned
   snapshot.
2. **The planner reads that snapshot as a source** beside recordings and
   Prometheus. It prices from the snapshot and does not fit again.
3. **Both tabs show the fit's id and age.** When they differ, the panel says
   so.
4. **Warm start.** A restarted controller fits from the Prometheus history it
   already has, through the planner's own Prometheus path, instead of waiting
   13 hours for its window to fill.

It adds no package, component, learner or metric family. It extends the
learned-state store, the slo analyzer's v2 window, `whatifsource.Capture`,
`GET /v1/sources` and the label values of #448's series.

## Today

These rows are checked against `main` at `c063bb93` and PR #448 at `c4be24ea`.

| | Controller | Planner |
| --- | --- | --- |
| Model code | `internal/signals/whatif` (`FitMixtureWindow`, `FitTPOTWindow`, `FitCycleOf`), `internal/signals/prefill` (V_P, a, f, hit at count) | the same packages |
| Cycles fitted on | its own cycles, in memory: the last `slo.ModelWindow` (13 h = `whatif.MinSpreadFitWindow` 12 h + 1 h), cut into 5 min steps, refitted every 30 min (#448 `model_learners.go`) | a recording, or a Prometheus window captured by `internal/whatifsource` and replayed by `learnreplay` |
| TTFT switches | `Stratified` only (`whatif.DefaultRankMixtureStratified`). `NLLine`, `KVHold`, `RemainderSpread` and `FitSteps` are off. | chosen per request (`BacktestRequest.RankMixture*`, `--experimental-ttft`) |
| Default-model switches | the controller's settings | planner-only switches as well (`learnreplay.PlannerSwitches`: `SwitchPrefillHitAtCount` #384, `SwitchPrefillVPServiceLine`, and others) |
| Across a restart | the v2 window and fits are lost. The slo analyzer has no learned-state restore (install-options.md, "Learned state across a restart"). | stateless. Each source is resolved again. |
| Identity of a fit | none published | `inputsId` = `contentID("inputs", ...)` over the resolved inputs (`internal/whatifapi/cache.go`) |

The code is shared and the parity test proves it: `TestV2Parity_ControllerAndPlannerAgreeOnTheRecording`
in #448 shows the same cycles give the same fit, bit for bit. The **state** is
not shared, so the same code still gives two answers:

- different windows: the controller's last 13 h, the planner's captured window;
- different switches: a planner flag the controller does not run;
- after a restart, a controller with an empty window (`window-short` for
  about 12 h) next to a planner that fits a full window.

The learned-state snapshot cannot close this gap today. It holds k2,
throughput, ITL baselines and slopes, and start estimates, but not V_P, a, f,
the prompt spread or any v2 fit. capacity-planner.md says so: "it cannot price
TTFT".

## Design

### The controller owns the live fit

The slo analyzer's v2 learners (#448) and its default TTFT model already fit
on the controller's cycles. After each refit that passes validation
(`HeldOutPasses`, `tpotFoldsPass`), the controller adds the fit to the
learned-state store.

It writes only what pricing reads: parameters and the pooled distributions the
models draw from. It never writes the cycles or the fit steps.

### What one entry holds

There is one entry per **(namespace, modelID, variant)**. Each entry has its
own fit id, window and clocks. A P/D model has two entries:

- the **prefill** entry holds the TTFT fits;
- the **decode** entry holds the TPOT fits.

Each entry names the other in `pairedWith`, because the rank mixture is fitted
on the pair.

| Field | Content | From |
| --- | --- | --- |
| `key` | namespace, modelID, variant, role, `pairedWith` | the analyzer input |
| `identity` | the variant's configuration fingerprint (`config` / `args` as in the existing document), accelerator, served model id | as the existing restore check reads them |
| `window` | `from`, `to`, cycles, step (5 min), cycle interval | the v2 window |
| `origin` | `live`, `restored` or `backfill` (see [Warm start](#warm-start-from-prometheus)) | |
| `switches` | every switch the fit ran under, by package and name | the same map as `learnreplay.PlannerSwitches` |
| `ttftDefault` | V_P, a, f (rank imbalance), the hit and the count it was read at | `prefill.Estimate` |
| `ttftMixture` | `RankMixture` (service line S0/V, shares, KV deferral, A0/Beta/Spread, slots, `Parts` when `NLLine` is on), `Top`, the held-out score, and the abstention reason | `whatif.MixtureWindowFit` without `Steps` |
| `ttftShape` | the pooled computed-token and whole-prompt distributions, at the engine's own bucket edges | `MixtureWindowFit.Pooled.Computed`, `.Prompt` |
| `tpot` | `TPOTDist` (GroupCV2, C, Renewal), `Inv`, `Top`, the default model's R_q at p90 and p95, and the abstention reason | `whatif.TPOTWindowFit` plus `defaultRatio` |
| `fittedAt`, `lastUsed` | when the fit was made, and when it was last priced | |
| `fitId` | `sha256:` of the entry's canonical JSON without `lastUsed` | |

The TPOT mean is the saturation ITL line, which the existing document already
stores (`itlSlopes`, `itlBaseline`). The entry refers to it by key and does not
copy it.

The new types go in `internal/signals/whatif` (for example
`whatif.FitSnapshot`). The controller's store and the planner both import that
package, so the planner never imports `steadystate`.

### Where the snapshot lives

**A ConfigMap.** The store reuses the learned-state mechanism as it is:

- the same read-modify-write on `resourceVersion`;
- leader-only writes, at most once a minute, and only when something changed;
- deletion flushes;
- the existing size metrics.

So it needs no new CRD, volume or database. It survives restarts and leader
changes like the existing document. The planner reads it through the
Kubernetes API with ordinary RBAC.

There is **one ConfigMap per managed namespace**, named
`scaling-manager-learned-state-<namespace>`, in the controller's namespace. It
holds one data key, `fits-v1`. The saturation document stays where it is.

Splitting by namespace makes the namespace the RBAC boundary. Kubernetes
cannot grant access to one data key of a ConfigMap, so a single shared
ConfigMap would show every namespace's fits to anyone who can read it.

**What the controller may do today** (`config/base/rbac/`):

- read every ConfigMap (`manager-role`: get, list, watch);
- create ConfigMaps in its own namespace;
- get and update only the named `scaling-manager-learned-state`
  (`learned-state-role.yaml`).

**Why a ConfigMap is close to its limit as a store.** ConfigMaps are meant
for configuration that Pods consume. Runtime state written by a controller
belongs, by Kubernetes practice, in a custom resource's status. See
[Later stages](#later-stages). This proposal still uses a ConfigMap because
the mechanism exists, is tested, and has its RBAC and failure modes in place.
The limits to respect:

- **Object size.** An object is stored in etcd, which bounds a request at
  about 1.5 MiB by default. The API server refuses a ConfigMap over 1 MiB of
  data. The existing store keeps a document under 700 KiB and all data under
  900 KiB (`learnedStateMaxBytes`, `learnedStateMaxData`). Each
  per-namespace ConfigMap gets the same bounds.
- **Write fan-out.** Every write goes to every watcher of the object. Write
  only when a fit changes (a passing refit, at most every 30 min per model),
  at most once a minute in all, as the existing store already does. Large,
  frequently written objects slow etcd for the whole cluster.

**Size, measured.** On the public Qwen3-32B warm-up recording (PR #448
branch, `controllerV2At` at the last cycle; 580 cycles, 2 prefill ranks, one
prefill and one decode variant), the JSON sizes were:

| Part | Bytes |
| --- | --- |
| `RankMixture` parameters | 642 |
| the same with the mixture's report (diagnostics) | 1,945 |
| pooled computed + prompt distributions, 15 engine bucket edges each | 375 |
| `TPOTDist`, `Inv`, `Top` | 117 |
| the TPOT fit's report (diagnostics) | 1,001 |
| `prefill.Estimate` (V_P, a, f, hit and their provenance) | 1,308 |
| **not stored:** the 30 fit steps' histograms | 2,940 entries, so roughly 70 KB for 2.4 h and about 370 KB for a 13 h window |

The TTFT fit on this 2.4 h recording abstained (the spread is not calibrated
under 12 h). Its pooled shape is therefore empty, and the row above uses the
last step's two distributions, which have the same edges. That is a
substitute, not the pooled shape itself.

Plainly:

- **The scalars are under 1 KB per variant.**
- **The bulk is the reports and the provenance.** The pooled distributions
  are small, because the engine's histograms have about 15 edges.
- **Keep the engine's own edges rather than resampling to 32 quantiles.**
  Quantiles would be larger and lossy, and the planner would then price from a
  different shape than the controller did.
- **What grows an entry:** prefill ranks (about 20 B per share), `NLLine`
  parts (about 300 B), and longer reports.
- **The total:** about 5.5 KB for a P/D model with its reports (about 2.7 KB
  per variant), and about 2.5 KB without them.
- **The ceiling:** 900 KiB holds about 150 P/D models in one namespace's
  ConfigMap. Per namespace, the 1 MiB limit matters only when one namespace
  holds that many variants.
- **When over budget:** the existing least-recently-used drop applies. The
  reports are dropped first (they are diagnostics), then whole entries.

P1 measures again on a window that does not abstain, through
`llmsm_learned_state_snapshot_bytes`.

**Alternatives, briefly:**

| | For | Against |
| --- | --- | --- |
| CRD, status per (namespace, model, variant) | versioned schema, per-object RBAC, Kubernetes practice for controller state | a new API type to install, convert and support |
| HTTP endpoint on the controller | no size limit | the controller must be up and reachable, it needs its own auth, and it does not survive a restart by itself |
| Prometheus metrics | already scraped | scalars only, with no shapes, no identity and no honest age (the learned-state proposal's case against reading metrics back) |

### Identity, versioning and clocks

- **Versioned.** The data key names the schema (`fits-v1`), as `v1` does for
  the existing document. The key holds `{"version": 1, "writtenAt",
  "writer", "entries": {...}}`. A reader that does not know the version
  ignores the key and says so. The
  planner shows "snapshot schema 2, this planner reads 1" and falls back. The
  controller never guesses either, as rule 8 of the learned-state proposal
  says.
- **Restore checks** are the existing ones (learned-state-snapshot.md, rule
  1). The configuration fingerprint must match, and so must the variant pair
  and the accelerator. A configuration that was not read completely is not
  restored.
- **Clocks.** Learned state ages on its **last use**: `lastUsed` moves when
  the fit prices a cycle, and the variant timeout runs from it. The **age**
  that the tabs show is the data's age, `now - window.to`, and beside it
  `fittedAt`. A fit that is used every cycle but has not refitted for hours
  shows that age; it does not look fresh.
- **A restored fit is held, not refitted.** It is priced as `held` for
  `slo.ModelHoldFor` (1 h), like any fit whose refit did not pass. Without a
  warm start it then lapses and the model reads `window-short` until the
  window refills. That gap is why a warm start is part of this proposal.

### Live tab

The Live tab reads only controller metrics, under #448's `learner` label, as
the panel proposal requires. To show which fit priced a figure, without a new
family:

- `llmsm_learned_state_last_admitted_timestamp_seconds` takes the learner
  values `ttft-mixture` and `tpot-distribution`, with the value `fittedAt`. The
  tab shows the age, `time() - x`.
- `llmsm_slo_predicted_tpot_seconds`, `llmsm_slo_predicted_ttft_seconds`
  and `llmsm_slo_model_coverage` take an `origin` label (`live`, `restored`,
  `backfill`).
  - The coverage and predicted-TPOT families are on `main` but in no release
    yet (v0.13.0 is the latest), so adding the label there costs no
    compatibility.
  - Predicted TTFT has been released since v0.11.0. A Prometheus family has
    one label set, so every series gets `origin`, and the default learner's
    series read `origin="live"`. Queries that select or aggregate by the
    existing labels are unaffected. The upgrade notes must list the label,
    and so must the `hack/check-dashboards.py` series list.
- The fit id is not a metric, because a hash in a label is churn. The planner
  page's Live tab reads the id from the snapshot. The Grafana row shows the
  age only.

### What-if tab: the snapshot as a source

`capacity-planner serve` gets a third source kind, `controller`, beside
`recording` and `prometheus` (`whatifapi.SourceRecording`,
`SourcePrometheus`):

- **`POST /v1/inputs` with `{"kind": "controller", "namespace", "model",
  "variant"}`** reads the namespace's ConfigMap and builds the planner's
  resolved state from the entries. It runs no fit core. The `inputsId` is
  `contentID("inputs", ...)` as today, and `SourceInfo` carries the `fitId`,
  `fittedAt`, `window` and `origin`.
- **It reprices only.** Plan, size and report price from the snapshot's
  parameters and shapes at the operator's knobs. The live point (the rate,
  the fleet and the current mix) comes from the existing Prometheus instant
  reads when `--prometheus` is set, and otherwise from the window's last
  values in the entry.
- **Switches follow the snapshot.** A planner flag that the snapshot did not
  run under (for example `--experimental-ttft=v2nl-line` against a fit with
  `NLLine` off) is refused for this source, with the reason. The alternative
  would be a figure no controller computed.
- **The fallback is the planner's own fit, labelled.** When there is no
  snapshot (`LEARNED_STATE_SNAPSHOT` is off, the default), or the entry is
  missing, of an unknown schema or past its horizon, the tab uses the
  Prometheus or recording source as today. The header then says "planner's own
  fit, not the controller's".

### Choosing a model: namespace, model, variant

The controller manages many models, variants and namespaces. The panel
chooses in three steps: **namespace, then model, then variant.** Both tabs
follow the one selection.

`GET /v1/sources` is extended; no new endpoint is added. `SourcesResponse`
gets:

```json
"controller": {
  "readable": true,
  "models": [
    {"namespace": "team-a", "model": "qwen3-32b",
     "variants": [
       {"name": "qwen-prefill", "role": "prefill", "pairedWith": "qwen-decode",
        "fitId": "sha256:...", "fittedAt": "...", "windowTo": "...", "origin": "live"},
       {"name": "qwen-decode", "role": "decode", "pairedWith": "qwen-prefill", "...": "..."}]}]
}
```

- The list holds only namespaces the caller may read (see [Access](#access-and-security)).
- The Prometheus source adds the models it sees in those namespaces, so a
  model with no snapshot entry is still listed, marked "planner's own fit".
- A recording lists its own models, as today.
- **A model sized across several variants** (one modelID, for example two
  decode variants on different accelerators) is shown as a **model row**
  first: the router's rate and latency for the model, and the total replicas
  per role. One **row per variant** follows: its replicas, its fit and its
  age.
- What-if prices the model. Its knobs move each variant's replicas, and the
  default is the live split.
- A v2 fit exists only where the model has one prefill and one decode variant
  (`modelPair`). With several, the v2 rows read "not fitted: several variants
  of a role", as the coverage series already says.

### Mismatch

The snapshot lags the live fit by at most the write interval (1 minute), plus
a failed write. The panel compares:

- the What-if source's `fittedAt` with the Live tab's
  `llmsm_learned_state_last_admitted_timestamp_seconds{learner=...}`. If they
  differ, it shows "the controller has refitted since this snapshot; reload".
- the planner's own fit, when Prometheus is configured, with the snapshot at
  the live point. If they disagree by more than the planner's 20% backtest
  tolerance, it names the cause it can read: the window, a switch, or the
  origin.

### Warm start from Prometheus

The v2 window is 13 h, and a restart empties it. The snapshot restores the
**fit** but not the **cycles**: a 13 h window of a large P/D model is about 13
MiB in memory (`slo.ModelMaxWindows` comment, `model_cost_test.go`), far
beyond a ConfigMap. The engines' series are already in Prometheus, and the
planner already rebuilds cycles from them. Warm start reuses that path.

**Precedence for the fit in use:**

1. a snapshot entry that passes the restore checks, priced at once (`origin:
   restored`, held);
2. a fit on the window backfilled from Prometheus (`origin: backfill`);
3. cold: `window-short` until the live window fills.

The backfill runs in case 1 too. The snapshot holds no cycles, and without
them the restored fit lapses after `ModelHoldFor`.

**The path.** `whatifsource.Capture` builds, for each step of a past window,
the `learnrec.Input` that a controller would have read. It uses the
collector's own queries over range queries, and `kube-state-metrics` for the
fleet. Capture gets a per-step callback in place of its writer. The leader
feeds each input to the slo analyzer's `ObserveModelInputs` into the model's
window, then refits.

The cycles come from the same builder (`FitCycleOf`) and the same fit cores,
so **the backfilled fit equals what `capacity-planner --prometheus` fits on
that window**. A parity test asserts this, extending #448's.

The wiring is in `cmd/main.go`: the engine is handed a function and imports no
`whatifsource`. The import-direction check does not rank `whatifsource`, and
the controller binary does not link it today, so `make build` measures what it
adds.

**Answers to the open points:**

- **Background, never blocking.** Only the leader backfills, one model at a
  time, after its first cycle. The controller runs cold until the backfill
  lands. Each model gets a timeout (2 min). A failure, a timeout or leader
  loss abandons that model's backfill, says why in the cycle log, and does not
  retry until the next restart.
- **Window and retention.** The backfill asks for `ModelWindow` (13 h),
  truncated to the current revision (below). When Prometheus keeps less, it
  gets less, and the fit abstains `window-short` exactly as the planner would.
  Nothing is special-cased.
- **Query cost.**
  - The step is the controller's cycle interval.
  - At 15 s, 13 h is 3,120 steps. Times about 80 queries with
    kube-state-metrics, that is 250,000 step-queries, under
    `whatifsource.MaxStepQueries` (1,000,000).
  - One range query covers up to 10,000 points (`maxPointsPerQuery`), so each
    query is one request; the code already chunks longer windows.
  - Capture keeps every sample in memory (about 60 B each), and the controller
    runs under a 512 MiB limit. The planner's `MaxSamples` (20 M, about 1.2 GB)
    is far too high here. The controller caps it at 2 M samples (about 120
    MB). A model over the cap is refused ("backfill too large"), not
    coarsened.
  - Estimate (not measured): a 16-pod P/D model is 2-3 M samples, so P4
    measures it before choosing the cap.
- **Identity, or discard.** Prometheus does not hold engine arguments. The
  backfill keeps only cycles from Pods of the variant's **current revision**
  (`pod-template-hash` from the owner ReplicaSet in `kube_pod_owner`, which
  `whatifsource` already reads), and the served model id must match. It
  discards the whole backfill when:
  - the variant pair differs from today's;
  - the accelerator read from `kube_node_labels` differs (where it cannot be
    read, it is recorded as assumed);
  - the variant's configuration is not read completely (`envFrom` flags), as
    the restore rule already refuses;
  - `kube-state-metrics` is absent.

  This last rule is the answer to the learned-state proposal's objection to a
  Prometheus refit: the engines' series name no variant, and without
  `kube-state-metrics` an old reading cannot be attributed to one.
- **Gates.** The same identifiability checks and abstentions as the planner,
  because the fit cores are the planner's. There are no backfill-specific
  thresholds.
- **Clock.** Backfilled cycles keep their own times. `fittedAt` is the refit
  time, `window.to` the last backfilled cycle, and `lastUsed` runs as for any
  entry.
- **Passive first.**
  - A backfill fit is published under the same learner label with
    `origin="backfill"`, and is written to the snapshot with `origin:
    backfill`.
  - It is not trusted until it has predicted the live steps since the
    restart: the router's measured TTFT and TPOT on at least 12 live steps (1
    h), scored by the same `HeldOutPasses` rule.
  - If it passes, the backfilled and live cycles merge, and the next refit is
    `origin: live`.
  - If it fails, the backfilled cycles are dropped and the window goes on
    from live cycles only.
  - The v2 learners reach no sizing decision today (#448), so "trusted" means
    "fed to the would-be series and the snapshot".
  - The switch is a learner switch beside `slo-v2-inputs` (for example
    `slo-v2-backfill`), off until a cluster run shows the backfill fit within
    tolerance of the live fit.
- **No new permission.** The controller already queries Prometheus with its
  own ServiceAccount token (security-model.md). A range query
  (`/api/v1/query_range`) goes to the same endpoint under the same auth.
  - On OpenShift's Thanos with `cluster-monitoring-view`, the role covers
    both query and query_range.
  - The tenancy port (9092) serves namespaced queries only, so
    `kube_node_labels` (no namespace) is not readable there and the
    accelerator check reads "assumed".
  - P4 verifies both endpoints as the controller's own identity. Kubernetes
    RBAC is unchanged.

The Vertical Pod Autoscaler's recommender makes the same choice. Its
`--storage` flag selects `checkpoint` (the default) or `prometheus`, which
reads `--history-length` (default 8 days) of usage at startup
([recommender config](https://github.com/kubernetes/autoscaler/blob/master/vertical-pod-autoscaler/pkg/recommender/config/config.go)).
This design runs both: the snapshot first, then the history.

## Access and security

**The controller** is the only writer, and it writes in its own namespace.

- In a **namespace-scoped install**, the one managed namespace gives one
  ConfigMap name. `learned-state-role.yaml` lists it next to
  `scaling-manager-learned-state`, so get and update stay limited by name.
- In a **cluster-scoped install**, the managed namespaces are not known at
  install time. `update` must then be granted on ConfigMaps in the
  controller's namespace without a name. That widens today's rule, which
  limits update to one name. `create` is already unlimited there. This is
  open question 1.

**The planner** today has no Kubernetes token: the base `capacity-planner`
ServiceAccount and StatefulSet set `automountServiceAccountToken: false`. Only
the OpenShift oauth-proxy container mounts one. Reading the snapshot needs:

- the token mounted, in an overlay that turns the controller source on and in
  no other;
- a Role in the controller's namespace with `get` on
  `scaling-manager-learned-state-<namespace>`, one name per namespace the
  planner may show, and a RoleBinding to the planner's ServiceAccount in its
  own namespace. No list or watch is needed: the planner GETs by name and
  caches for 15 s.

**Users** see only namespaces they may read:

- With `--auth=kubernetes`, the planner already checks a Prometheus
  namespace by SubjectAccessReview (`get pods` in it, `NamespaceAllowed` in
  `internal/whatifapi/auth.go`). The controller source uses the same check,
  and `/v1/sources` lists only the namespaces that pass.
- With the OpenShift oauth-proxy SAR overlay, the proxy checks `get` on the
  planner's own Service and nothing per namespace. There, only
  `--allowed-namespaces` and the planner's Role bound what is shown. The
  in-cluster guide must say so.

**What the fits reveal.** Fit parameters are not credentials. They do
describe a workload: its request rates (`Top`), its prompt-length
distribution, its prefix-hit rate and its fleet shape. They are as sensitive
as the engine metrics in Prometheus, and get the same scoping: per namespace,
never across. The writer identity in the document (a Pod hostname) is already
there today.

## What must happen first

| Gap | State | Effect here |
| --- | --- | --- |
| PR #448 merged | open. `git merge-tree` against `main` `c063bb93` conflicts in five files: `internal/whatifapi/{backtest_rank_mixture,backtest_rank_mixture_nl,backtest_tpot_distribution,tpot_model_main,ttft_model_main}.go` | everything: the v2 learners are #448 |
| The router's processing histogram in the controller's v2 window | `CollectPredictionLatency` asks the TTFT and TPOT queries only. The processing queries are asked only while a learner recording is on (`internal/collector/router_latency.go`). | the remainder spread (`RemainderSpread`, `NLLine`) cannot run in the controller, so a planner fit with it cannot match |
| `DecodeCycle.EngineTPOT` | not on `main` (issue #470) | the planner's engine-TPOT ratio has no controller counterpart |
| Render / frontend series, the router-side remainder model | a PR in progress | the remainder parts in `ttftMixture.Parts` |
| Switch parity (issue #169) | `SwitchPrefillHitAtCount` (#384), `SwitchPrefillVPServiceLine` and others are planner-only (`learnreplay/state.go`) | the controller source refuses those flags. Each switch that reaches the controller widens what the snapshot can serve. |
| `LEARNED_STATE_SNAPSHOT` | off by default | no snapshot, so the planner falls back, labelled |

## Phases

The CRD is not a phase of this proposal. See [Later stages](#later-stages).

**P1: write the fit (passive).** The controller writes the entries (TTFT
default, mixture, shape, TPOT) to the per-namespace ConfigMaps while
`LEARNED_STATE_SNAPSHOT` is on. It restores them on restart as `held`, and adds
the `origin` label and the new learner values.

Acceptance:

- A unit test: an entry round-trips through the document, and pricing the
  restored fit gives the same figures as the fit before the write (bit for
  bit).
- Negative controls: a changed fingerprint, a changed pair or an unknown
  schema is not restored.
- A budget test: reports are dropped first, then entries, least recently used
  first, and `llmsm_learned_state_snapshot_dropped_entries` counts them.
- A kind e2e, with the controller restarted under load: the restored fit
  prices on the first cycle with `origin="restored"`.
- The size measured on the e2e and on one long public recording, recorded in
  this proposal.

**P2: the planner reads it.** The `controller` source, `/v1/sources` listing,
namespace / model / variant selection, and the fit id and age on both tabs. An
RBAC overlay for the planner.

Acceptance:

- A test: `capacity-planner serve` with a document fixture prices the same
  TTFT and TPOT as the controller's published `llmsm_slo_predicted_*` at the
  same rate and fleet, exactly.
- An unknown schema is refused with its reason.
- Listing as a user without `get pods` in a namespace omits it.
- `kubectl auth can-i` as the planner's ServiceAccount: get on its names only,
  no list, nothing else.

**P3: mismatch.** The refitted-since banner, the comparison with the planner's
own fit, and the switch refusals.

Acceptance:

- Fixtures where the snapshot `fittedAt` lags the metric (banner shown).
- A planner flag off in the snapshot (refused).
- A planner fit on a different window (the difference and its cause shown).

**P4: warm start.** The Capture callback, the backfill in the background, the
identity cut, live validation, and the `slo-v2-backfill` switch.

Acceptance:

- The parity test: the backfilled fit equals `capacity-planner --prometheus`
  on the same window, as a ModelFit compared without its clocks, on a
  promtest fixture.
- Negative control: a fixture whose Pods are of an older revision backfills
  nothing.
- A kind e2e: after a controller restart, the model's coverage leaves
  `window-short` within the backfill timeout, with `origin="backfill"`. Then
  either it passes live validation and turns to `live`, or the backfilled
  cycles are dropped.
- Startup is not delayed: the first cycle's time is unchanged with the switch
  on.
- Measured: query count, samples, peak memory on the largest e2e model, and
  wall time.

P1 needs #448 merged. P2 and P3 follow P1. P4 can start after #448, beside
P1.

## Entities added

| Entity | Kind | Why not an existing one |
| --- | --- | --- |
| `whatif.FitSnapshot` (entry types) | types in `internal/signals/whatif` | shared by the controller and the planner. The fit types already live there. |
| `fits-v1` document in `scaling-manager-learned-state-<namespace>` | ConfigMaps written by the existing store's code (read-modify-write, budget, flush) | per-namespace RBAC. One ConfigMap cannot be scoped per key. |
| `controller` source kind | `whatifapi` source value | beside `recording` and `prometheus` |
| `controller` block of `GET /v1/sources` | response field | extends the existing listing |
| `origin` label; learner values `ttft-mixture`, `tpot-distribution` on `llmsm_learned_state_last_admitted_timestamp_seconds` | label and values on existing families | no new family |
| a per-step callback on `whatifsource.Capture` | option | the controller needs inputs, not a file |
| `slo-v2-backfill` | learner switch | passive first, like `slo-v2-inputs` |
| planner Role and RoleBinding | RBAC overlay under `config/capacity-planner/` | the planner calls no Kubernetes API today |

## Later stages

Once the panel serves many models, the snapshot fits a **custom resource
better than a ConfigMap**: one object per (namespace, model, variant), the fit
in `status`, a versioned schema with conversion, and RBAC per object. The
precedent is the Vertical Pod Autoscaler:

- It checkpoints its learned histograms in a `VerticalPodAutoscalerCheckpoint`
  object: "the checkpoint of the internal state of VPA that is used for
  recovery after recommender's restart".
- The spec names the VPA and the container. The CPU and memory histograms
  live in `status`.
- It writes one object per VPA container, named `<vpa>-<container>`, in the
  VPA's namespace
  ([types](https://github.com/kubernetes/autoscaler/blob/master/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1/types.go),
  [writer](https://github.com/kubernetes/autoscaler/blob/master/vertical-pod-autoscaler/pkg/recommender/checkpoint/checkpoint_writer.go)).

That would be a new API type, against the rule to reuse existing entities.
The case for it would be the multi-model scope, per-object RBAC, and that it
is Kubernetes practice for controller state. This proposal does not take that
step: the per-namespace ConfigMap is the design here.

## Open questions

1. In a cluster-scoped install, grant `update` on ConfigMaps in the
   controller's namespace without a name, or have the installer list one name
   per managed namespace and re-render when one is added?
2. Should a restored fit be held longer than `ModelHoldFor` when warm start
   is off or refused, so that a restart does not cost 12 h of `window-short`?
   It would be older than any held fit is today.
3. Should a backfill from a window whose accelerator cannot be read (the
   tenancy port) be refused, or used and flagged "assumed"?
4. Should the fit snapshot need `LEARNED_STATE_SNAPSHOT` (off by default), or
   be on wherever the v2 models run? A What-if tab that always falls back in a
   default install shows the gap this proposal is meant to close.
5. Is 12 live steps (1 h) the right bar for a backfill fit, or should it be the
   same held-out share over a fixed share of the window?
