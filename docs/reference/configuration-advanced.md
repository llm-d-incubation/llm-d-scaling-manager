# Advanced settings

Settings with a calibrated default. Leave each one at its default unless the
last column describes your case. Where each place is, and when a change applies:
[configuration reference](configuration.md#where-settings-live). The settings
most installs need are on that page.

## Controller

Manager ConfigMap keys can also be set as environment variables of the same
name on the controller Deployment. All of these need a restart.

| Name | Where set | Default | Change it when |
| --- | --- | --- | --- |
| `GLOBAL_OPT_INTERVAL` | Manager ConfigMap | `15s` (at least `1s`, else `15s`) | a very large fleet needs a slower decision cycle |
| `PROMETHEUS_TOKEN_PATH` | Environment | the controller's ServiceAccount token | your Prometheus wants another bearer token file |
| `PROMETHEUS_BEARER_TOKEN` | Environment | empty | avoid it: it puts a secret in plain configuration. Use a token file |
| `PROMETHEUS_ALLOW_HTTP` | Environment | `false` | your Prometheus serves plain HTTP only |
| `PROMETHEUS_CLIENT_CERT_PATH`, `PROMETHEUS_CLIENT_KEY_PATH` | Environment | empty | your Prometheus requires a client certificate |
| `PROMETHEUS_SERVER_NAME` | Environment | empty | Prometheus' certificate names a different host than its URL |
| `PROMETHEUS_METRICS_CACHE_TTL`, `_CLEANUP_INTERVAL`, `_FETCH_INTERVAL` | Manager ConfigMap | `30s`, `1m`, `30s` | your scrape interval is longer than 30 s |
| `PROMETHEUS_METRICS_CACHE_FRESH_THRESHOLD`, `_STALE_THRESHOLD`, `_UNAVAILABLE_THRESHOLD` | Manager ConfigMap | `1m`, `2m`, `5m` | the same |
| `LLMSM_STICKY_SCALE_DOWN` | Environment | `true` | you are comparing behaviour: off, a model idling near the boundary flips N and N-1 |
| `LLMSM_LIMITED_MODE` | Environment | `false` | the controller may not read Nodes |
| `SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY` | Environment | `10` | many models wake from zero at once |
| `LEARNED_STATE_EPISODE_GAP`, `LEARNED_STATE_VARIANT_TIMEOUT`, `LEARNED_STATE_BUCKET_RETENTION` | Environment | `24h`, `24h`, `168h` | you test learned-state expiry in a short run ([details](install-options.md#advanced)) |
| `CONTROLLER_INSTANCE` | Environment | empty | several controllers share a cluster. Each then manages only ScaledObjects labelled `scaling-manager.llmd.ai/controller-instance=<value>`, labels its metrics with it, and cleans up only its own transfer marks |
| `POOL_GROUP` | Environment | `inference.networking.k8s.io` | your InferencePools use another API group |
| `--v`; installer `LLMSM_VERBOSITY` | Flag | `2` | debugging: `3` adds per-replica capacity lines, `4` is debug |
| `--zap-log-level` and the other `--zap-*` flags | Flag | controller-runtime's | you need another log encoder |
| `--metrics-bind-address`, `--metrics-secure`, `--metrics-cert-path` and its name and key | Flag | `:8443` in the manifest, HTTPS, an in-memory certificate | your metrics scrape needs another port or certificate |
| `--health-probe-bind-address` | Flag | `:8081` | the port is taken |
| `--leader-elect`, `--leader-election-lease-duration`, `--leader-election-renew-deadline`, `--leader-election-retry-period`; env `LEADER_ELECTION_ID` | Flag | on in the manifest; `60s`, `50s`, `10s`; `72dd1cf1.llm-d.ai` | you run more than one replica and need faster failover ([high availability](install-options.md#high-availability)) |
| `--rest-client-timeout` | Flag | `60s` | a slow API server times out |
| `--external-scaler-bind-address` | Flag | `:9090` | the port is taken |
| `--external-scaler-client-san` | Flag | any identity the client CA signed | you want to accept only named KEDA identities |

### Warm-pool flags

Fleet defaults. A pool's trigger keys of the same name override them per pool.

| Name | Default | Change it when |
| --- | --- | --- |
| `--warm-pool-sleep-min-size` | `1` | the pool should keep more free Pods in reserve |
| `--warm-pool-max-hold`, `--warm-pool-min-hold` | `2m`, `0` | your replicas take longer than two minutes to start |
| `--warm-pool-memory-bytes` | `0`: the container's limit | the pool Pod's host memory budget differs from its limit |
| `--warm-pool-gpu-memory-utilization` | `0.90` | the warm copies must leave more GPU memory free |
| `--warm-pool-preload-top` | `2` | more of the busiest models should be preloaded |

## Scaling policy

Live. Any entry unless it says `default`.

| Name | Default | Change it when |
| --- | --- | --- |
| `kvCacheThreshold` | `0.80` | your engine's usable KV share differs; it compounds with `scaleUpThreshold` |
| `queueLengthThreshold` | `5` | your models queue more or less before they are saturated |
| `analyzers[].score`, `analyzers[].scaleUpThreshold`, `analyzers[].scaleDownBoundary`, `analyzers[].parameters` | score `1.0`; thresholds from the entry | one analyzer needs its own band ([analyzers](scaling-policy.md#configuring-analyzers)) |
| `analyzerDefinitions` (`default`) | none | you want an analyzer from your own PromQL ([external analyzers](scaling-policy.md#external-analyzers-analyzerdefinitions)) |
| `slo.flowControl` | unset: inferred from the router's flow-control series | a scrape gap makes the gate look off; declare `enabled` or `disabled` |
| `disableShapeChangeHold` | `false` | a shape-change hold is never wanted for this model |
| `enableLearnedStateReuse` | `false` | similar variants should share a latency line and capacity ([details](scaling-policy.md#enablelearnedstatereuse)) |
| `disableK2Borrow` | `false` | a variant must not borrow a sibling's measured capacity ([details](scaling-policy.md#disablek2borrow)) |
| `enablePrefillThroughputBound` | `false` | prefill should be priced on a lower bound ([details](scaling-policy.md#enableprefillthroughputbound)) |
| `enableBatchCapLearning` | `false` | the engine's batch cap cannot be read and you want it learned |
| `enableRescale` (`default`) | `false` | models should be rescaled by `priority` under contention |
| `analyzerTuning.*` (26 keys) | calibrated | a measurement says a learner's gate is wrong for your fleet ([advanced tuning](scaling-policy.md#advanced-tuning-analyzertuning-and-loadimbalance)) |
| `loadImbalance.*` | `idleKvUsage 0.30`, the rest `2m` | the same |

### Limiter: Kueue

On the quota entry's `kueue:` block ([timings](install-options.md#kueue-borrowing-timings)).

| Name | Default | Change it when |
| --- | --- | --- |
| `resources` | every vendor GPU resource | some GPU resources should not count |
| `refreshInterval` | `30s` | Kueue quotas change often |
| `placementTimeout`, `borrowCooldown`, `lenderDebounce` | `5m`, `10m`, `60s` | borrowed replicas take longer to place, or lenders wait too often |
| `borrowNamespaces` | all | you pilot borrowing on a few namespaces |
| `assumeTestedVersion` | `false` | Kueue's version cannot be read from its image tag |
| `releaseToNonReclaimingLenders` | `false`, experimental | lenders Kueue does not reclaim for should lend too |

### Utilization share

Under `optimizer.utilizationShare` on the `default` entry ([optimizer](scaling-policy.md#optimizer-cluster-default-only-live)).

| Name | Default | Change it when |
| --- | --- | --- |
| `tolerance` | `0.15` | small imbalances should, or should not, move GPUs |
| `weightClasses` | `best-effort` 0.5, `standard` 1, `important` 2, `critical` 4 | you want other classes or weights |
| `stabilization.consecutiveCyclesBeforeMove` | `2` | moves should wait longer for confirmation |
| `stabilization.waitBeforeReverseMove`, `stabilization.flappingWindow` | `auto` | load reverses on a known period |
| `stabilization.swingReversals` | `3` (2 to 20) | the same |
| `stabilization.returnSpareWhenLoadAtMost` | `0.6` | spare should be returned sooner or later |
| `stabilization.skipWaitsWhen.enabled`, `.receiverLoadAtLeast`, `.donorLoadAtMost` | `true`, `0.9`, `0.6` | a hard imbalance should, or should not, skip the waits |

## Trigger

| Name | Default | Change it when |
| --- | --- | --- |
| `warmPoolSleepMinSize`, `warmPoolMaxHold`, `warmPoolMinHold` | the flags' defaults | one pool needs its own reserve or lend times |
| `warmPoolPreloadTop`, `warmPoolGPUMemoryUtilization` | the flags' defaults | the same |
| `warmPoolSwitchSpareThreshold`, `warmPoolMinSwitchInterval` | `0`, `10m` | a retained pool switches models too often |

## Labels and annotations

| Name | On | Change it when |
| --- | --- | --- |
| `scaling-manager.llmd.ai/exclude=true` (annotation) | Namespace | the controller should ignore this namespace's local scaling-policy ConfigMap |
| `scaling-manager.llmd.ai/controller-instance` (label) | ScaledObject | several controllers share a cluster (with `CONTROLLER_INSTANCE`) |

## Installer

Every installer variable, including KEDA, Prometheus stack, image, TLS
provider, NetworkPolicy, HPA behaviour on generated ScaledObjects and workload
preparation: [installer options](install-options.md).
