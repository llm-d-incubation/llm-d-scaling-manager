# Evaluating the P/D role switch — experiment specification

> **Experimental, not run.** Arm C needs a patched vLLM with
> `POST /switch_pd_role`, which no released vLLM ships. See
> [the role-switch note](README.md) for the deployment shape.

**The question.** What does a switchable engine cost when it is *not* switching,
and what does it buy when the prefill:decode ratio is wrong.

The load shapes below are test parameters, chosen to stress what an agent-style
workload stresses: long inputs, short outputs. Engine and model facts are read
from the model's config and from startup logs, not assumed.

---

## 1. The three arms

| arm | `--all2all-backend` | patch | extra env | isolates |
| --- | --- | --- | --- | --- |
| **A** | `deepep_high_throughput` (prefill), `deepep_low_latency` (decode) | none (stock) | `VLLM_USE_DEEP_GEMM=1` | the specialist pair |
| **B** | `deepep_v2` | none (stock) | `VLLM_USE_DEEP_GEMM=0` | what `deepep_v2` costs, before any switching |
| **C** | `deepep_v2` | role-switch branch | `VLLM_USE_DEEP_GEMM=0`, `VLLM_SERVER_DEV_MODE=1`, `VLLM_PD_KEEP_PREVIOUS=1` | what *being able to switch* costs |

**B vs C is the measurement that has never been made.** Everything published so
far compares a switchable engine against a dedicated replica, which confounds
two things: "`deepep_v2` instead of the specialist" and "switchable instead of
fixed". B separates them. C differs from B only by the retained spare buffer
(286 MiB/GPU at EP=16) and the patched rebuild path.

All three arms use one vLLM nightly, built from commit
`385dce36bcee42309924a5ece951a96db3dce7f2`; arm C adds the injection from step 1b
of the fork's
[`tools/pd_role_switch/README.md`](https://github.com/ev-shindin/vllm-conf/blob/feat/pd-role-switch/tools/pd_role_switch/README.md).
Same base for all three, so the image is not a variable.

> **The backend names read backwards.** Decode is throughput-oriented and
> prefill is latency-oriented, so "HT for prefill, LL for decode" looks
> inverted. It is not — the names describe the **all-to-all dispatch**, not the
> serving SLO. Prefill dispatches ~1024 tokens in one bulk transfer; decode
> dispatches ~1 token per sequence per step, so thousands of tiny transfers
> where per-dispatch latency dominates.
>
> Two measurements confirm it: buffer sizes (HT 1264 MiB, LL 3226 MiB — LL
> reserves a worst-case per-rank buffer, the decode pattern), and `deepep_v2`'s
> own mode split (decode = worst-case allocation, prefill = exact).
> `pd_role.py`'s `_BACKEND_KV_ROLE` already encodes this.

---

## 2. Model facts

Read from `config.json` of `zai-org/GLM-5.2-FP8` (about 700 GiB on disk, fp8):

| property | value | consequence |
| --- | --- | --- |
| attention | **MLA** — `kv_lora_rank` 512, `qk_rope_head_dim` 64 | KV is ~43.9 KiB/token at fp8, not ~1.8 MiB |
| layers | 78, `first_k_dense_replace` 3 | **75 MoE layers** — matches `ranks_switched` logs |
| experts | 256 routed, top-8 | at EP=16 each rank holds 16 |
| max context | 1,048,576 | not the binding limit; KV budget is |
| weights | 87.7 GiB/GPU at EP=16 (measured) | leaves ~39 GiB for KV at `--gpu-memory-utilization 0.90` on a 141 GiB H200 |

---

## 3. Configuration and sizing

### The role definition, the same for every arm

| | prefill | decode |
| --- | --- | --- |
| `--all2all-backend` | per arm (§1) | per arm (§1) |
| `--max-num-batched-tokens` | **2048** | **128** |
| `--moe-backend` | `deep_gemm` (arm A only) | `deep_gemm` (arm A only) |
| speculative decoding | MTP, 3 tokens | MTP, 3 tokens |
| `cudagraph_mode` | default | `FULL_DECODE_ONLY` |
| KV connector | Nixl, `kv_role: kv_both` | Nixl, `kv_role: kv_both` |
| `--block-size` / KV dtype | 64 / fp8 | 64 / fp8 |
| `--tensor-parallel-size` | 1 | 1 |

Prefill is 2048 on every arm because arm C must boot at its prefill budget (the
figure sizes buffers at init and a switch can only lower it), and arms A and B
must match it to be comparable.

Three consequences worth stating rather than discovering:

- **MTP speculative decoding changes decode throughput materially**, so every
  decode arm runs it, or the decode numbers do not compare.
- **Arms B and C cannot use `deep_gemm`.** `deepep_v2` pads its contiguous
  layout and `deep_gemm` derives the expert count from the padded tensor, so the
  oracle falls back to `FLASHINFER_CUTLASS`. This is a real thing arms B and C
  give up — part of what A-vs-B measures, not a confound to remove.
- **`kv_role: kv_both` on both roles** is exactly the case `_switch_kv_role`
  refuses to move, so the switch leaves it alone.

### Sizing for EP=16 on H200-141GB

Using the measured 87.7 GiB of weights per GPU and 43.9 KiB/token of MLA KV:

| | prefill (`util` 0.90) | decode (`util` 0.95) |
| --- | --- | --- |
| budget | 127 GiB | 134 GiB |
| less weights | − 87.7 GiB | − 87.7 GiB |
| KV available per rank | **~39 GiB** | **~46 GiB** |
| KV capacity per rank | **~930K tokens** | **~1.10M tokens** |

With DP=16 and TP=1 each rank holds whole sequences, so per-rank concurrency is
roughly KV capacity divided by tokens per request:

| pattern | tokens/req | per rank | across 16 ranks |
| --- | --- | --- | --- |
| P1 | ~61K | ~15 | **~240** |
| P2 | ~19K | ~50 | **~800** |
| P3 | ~116K | ~8 | **~128** |
| D1 | ~2.6K | ~430 | — (seq-limited, not KV-limited) |

`--max-num-seqs` is set per pattern from the right-hand column. This is
arithmetic, not a measurement — **run 0 checks it**, and the gate is ±20%.

---

## 4. Load patterns

| id | shape | input tokens | output tokens |
| --- | --- | --- | --- |
| **P1** | interactive agent | 60,000 | 800 |
| **P2** | short-context automation | 18,000 | 800 |
| **P3** | long-context episode | 115,000 | 600 |
| **D1** | decode-bound | 512 | 2,048 |

P1–P3 keep inputs roughly 20–200× longer than outputs: a 2K-in/128-out benchmark measures a
different machine. `D1` exists to saturate decode with prefill out of the way,
so the decode arms measure decode.

### Cache modes — run each prefill pattern twice

Both modes are required and must be labelled in results:

- **`unique`** — every prompt unique. Measures full prefill cost. This is the
  controlled comparison between arms.
- **`shared-prefix`** — requests share a long common prefix so vLLM's own prefix
  cache supplies hits; report the achieved hit rate rather than targeting one.
  Checks that switchability does not interfere with prefix caching, which is not
  obvious and has never been tested.

---

## 5. Common method

Identical across arms unless a table says otherwise.

**Launch** (2 nodes, EP=16; rank 0 serves the API, rank 1 is `--headless
--data-parallel-start-rank 8`):

```bash
vllm serve zai-org/GLM-5.2-FP8 \
  --trust-remote-code \
  --block-size 64 \
  --kv-cache-dtype fp8 \
  --data-parallel-size 16 --data-parallel-size-local 8 \
  --data-parallel-address $MASTER_ADDR --data-parallel-rpc-port 5555 \
  --enable-expert-parallel \
  --tensor-parallel-size 1 \
  --all2all-backend <per arm> \
  --moe-backend <deep_gemm for arm A; omit for B and C> \
  --speculative-config '{"method":"mtp","num_speculative_tokens":3}' \
  --max-num-batched-tokens <2048 prefill | 128 decode> \
  --max-num-seqs <from the §3 sizing table, per pattern> \
  --gpu-memory-utilization <0.90 prefill | 0.95 decode> \
  --max-model-len <input + output, rounded up>
```

Decode runs add `--compilation-config '{"cudagraph_mode":"FULL_DECODE_ONLY"}'`.

The fork's switch harness, `run_switch_test.sh`, defaults to this model; rank and layer counts come from the switch response rather than a
constant.

**Environment**, all runs: `VLLM_DEEPEP_V2_ALLOW_HYBRID_MODE=0` (two nodes),
`NVSHMEM_HCA_PREFIX` set to the cluster's InfiniBand HCA prefix,
`VLLM_ENGINE_READY_TIMEOUT_S=3600`, `HF_HOME` on a node-local weight cache.
Pod must request the cluster's RDMA device resource.

**`VLLM_PD_PAUSE_MODE` is left at its default (`wait`)** and stated in results.
It is worth 40× on switch-under-load and nothing idle.

**Generator rules** — each corresponds to a measurement previously discarded:

| rule | why |
| --- | --- |
| identical MTP config on every arm | speculative decoding changes decode throughput; unmatched, it swamps the comparison |
| 2 warm-up passes discarded, ≥4 measured | an unconverged warm-up turned a 13.7% penalty into 34% |
| prompts unique per request *and per repeat* (outside `shared-prefix`) | 16×1800 tokens "prefilled" in 298 ms were all cache hits |
| TTFT to first **text-bearing** chunk, never headers | a 4 ms TTFT was header timing |
| report min–max or p90, never a lone median | one number cannot show whether two arms differ |
| results written to a **mounted volume** | `kubectl cp` cannot reach a completed pod |

**Recorded every run**: TTFT p50/p90/p99, output tok/s, TPOT p90, requests
completed/failed, peak GPU memory per rank, KV utilisation at peak, engine
version string, and for arm C `ranks_switched` on any switch.

---

## 6. Experiments

### Run 0 — capacity probe (arm B, ~20 min, 2 nodes)

**Purpose:** the §3 KV arithmetic is unverified; every later run's `max-num-seqs`
depends on it, and a wrong value either wastes capacity or triggers preemption
that silently distorts latency.

**Method:** launch arm B at `--max-model-len 131072`. From the startup log
record the reported KV cache blocks and derive tokens per rank. Then issue 1, 4,
8, 16 concurrent P1-shaped requests and record where preemption first appears.

**Output:** a concurrency ceiling per pattern, used as `--max-num-seqs` below.
**Gate:** if measured capacity is within 20% of the §3 derived figures,
proceed. If not, the arithmetic is wrong and every sizing here needs redoing.

---

### Set 1 — isolated prefill and decode, EP=16

One arm at a time, no llm-d, no EPP, nothing between generator and engine.

#### Prefill runs — metric is **latency**

| run | arm | pattern | cache mode | concurrency |
| --- | --- | --- | --- | --- |
| 1.1 | A | P1, P2, P3 | unique | 1, then run-0 ceiling |
| 1.2 | B | P1, P2, P3 | unique | as 1.1 |
| 1.3 | C | P1, P2, P3 | unique | as 1.1 |
| 1.4 | A | P1, P2 | shared-prefix | run-0 ceiling |
| 1.5 | B | P1, P2 | shared-prefix | run-0 ceiling |
| 1.6 | C | P1, P2 | shared-prefix | run-0 ceiling |

Concurrency 1 and saturated are both required: the existing result is that our
engine is **1.46× faster unloaded and 1.35× slower at 16 concurrent**, so a
single concurrency level would support either conclusion.

All prefill runs use `--max-num-batched-tokens 2048` (§3).

#### Decode runs — metric is **throughput**

| run | arm | pattern | concurrency | duration |
| --- | --- | --- | --- | --- |
| 1.7 | A | D1 | 16, 64, ceiling | ≥60 s sustained |
| 1.8 | B | D1 | 16, 64, ceiling | ≥60 s sustained |
| 1.9 | C | D1 | 16, 64, ceiling | ≥60 s sustained |

`--max-num-batched-tokens 128`, matching the decode role (arm C boots at 2048 and
switches to decode before the run). Sustained, not single-shot: report tok/s
over the steady window after warm-up.

#### Run 1.10 — switch under load (arm C only)

Drive P2 at the run-0 ceiling, perform `POST /switch_pd_role` mid-run, and
record: switch wall time, `ranks_switched` (must equal 16), in-flight requests
completed, and output identity across a round trip. **Idle switching is already
measured; under load it is not.**

#### What set 1 answers

- **A vs B** — the cost of `deepep_v2` against the specialist. Expect the larger
  gap on decode: `deepep_v2` pads its layout, `deep_gemm` rejects the padding,
  and the oracle falls back to `FLASHINFER_CUTLASS`.
- **B vs C** — the price of switchability alone. Expect small. **If B and C
  differ by more than a few percent, that is the headline finding** and it
  changes the case for the approach.

---

### Set 2 — a P/D fleet under llm-d with EPP

Prefill and decode groups behind the endpoint picker, so routing, NIXL, and the
CPU offload tier with P2P are all in the path. **2P + 1D, EP=16 each = 6 nodes
per arm.**

**Why 2P and not 1P.** Run 2.4 switches a prefill replica into the decode role.
With a single prefill group that leaves the fleet with *no prefill capacity* and
it stops serving — the experiment cannot run. 2P+1D → 1P+2D is the smallest
topology where one group changes role while the rest keep serving, and it is
also what makes the P2P tier non-degenerate, since P2P pulls between prefill
ranks and has nowhere to pull from when there is only one.

**The CPU offload tier and P2P are configured.** Both are vLLM connector
configuration rather than separate infrastructure, so there is no reason to
omit them. Prefill runs a MultiConnector: Nixl for P/D transfer, plus an
`OffloadingConnector` with a fixed CPU budget per pod, LRU eviction, prompt-only
offload and a P2P secondary tier. Record the CPU budget with the results. At
2P+1D the tier is small, which is enough to test whether switchability
interferes with offload and P2P, and not enough to say what hit rate a large
fleet would see.

| run | arm | prefill groups (x2) | decode group | patterns |
| --- | --- | --- | --- | --- |
| 2.1 | A | `deepep_high_throughput` | `deepep_low_latency` | P1, P2, P3 |
| 2.2 | B | `deepep_v2` | `deepep_v2` | P1, P2, P3 |
| 2.3 | C | `deepep_v2`, switchable | `deepep_v2`, switchable | P1, P2, P3 |

**Additional metrics:** end-to-end TTFT *and* output throughput together, NIXL
transfer count, p90 and failures, queue wait at the picker, per-group KV
utilisation.

#### Run 2.4 — re-roling a fleet whose ratio is wrong (arm C only)

The run that justifies the work. **2P+1D → 1P+2D.**

1. Drive P2 at a concurrency where **decode is the limiter**. Confirm it *is*
   the limiter from per-group KV utilisation before switching, rather than
   assuming the load shape produced it.
2. Record steady-state throughput and TTFT with the ratio wrong.
3. Switch **one of the two** prefill replicas into the decode role **and move its
   `llm-d.ai/role` pod label**, per [the role-switch note](README.md). The
   fleet keeps serving throughout on the remaining prefill group — which is the
   point, and is why 1P+1D could not have tested this.
4. Re-measure, and record what the CPU/P2P tier did across the switch: a
   re-roled replica's offloaded prompt KV becomes a P2P source for a role it no
   longer holds, and whether that is harmless is not known.

**Compare against leaving it alone**, not against a dedicated fleet. The claim
under test is "a wrong ratio can be corrected in seconds without restarting",
not "this beats a correctly sized fleet".

The engine does not move the label and must not — that is the autoscaler's job.
For this experiment it is manual. A deployment that puts `llm-d.ai/role` in a
Deployment selector cannot host this, because Kubernetes will not let you change
the selector; the switchable shape in [the role-switch note](README.md) is a
prerequisite.

---

## 7. Capacity and sequencing

| phase | nodes | wall time |
| --- | --- | --- |
| run 0 | 2 | ~20 min |
| set 1 (runs 1.1–1.10) | 2, sequential | ~4–5 h incl. repeats |
| set 2 (runs 2.1–2.4) | **6 per arm**, sequential | ~6–7 h |

Weight load is ~106 s from node-local NVMe against 1403 s from a shared PVC — a
node-local `HF_HOME` is not optional at this run count.

---

## 8. What this does not establish

- **Fleet-level benefit.** 2P+1D is a small fleet. This establishes mechanism
  and cost, not what a large fleet gains.
- **Cache-tier behaviour at scale.** The CPU tier and P2P run small, so hit
  rates here say little about a large fleet.
- **Other EP sizes or transports.** Everything here is EP=16 on InfiniBand;
  all-to-all cost scales with rank count and transport, so absolute figures do
  not transfer.
- **Whether switching is the right control action.** That needs a controller
  that does not exist yet.
