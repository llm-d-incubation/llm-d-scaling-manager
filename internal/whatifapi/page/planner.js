// The capacity planner's page (docs/proposals/capacity-planner.md, "The
// screen"). It holds no model: every figure it shows is one the API returned
// (it computes only the axes' ranges, and which of the two knees is lower to
// place them), and a refused figure is shown as its reason, never as a number.
// Every string from the API goes into the page through esc. Plain JavaScript,
// no library, no request to any origin but the one that served it, by paths
// relative to the page, so that a proxy may serve it under a prefix.
"use strict";

// FLAG_WORDS is every flag the engine raises, in plain words, and what clears
// it. A flag missing here is shown by its name (TestPageNamesEveryFlag).
const FLAG_WORDS = {
  UnvalidatedTTFT: ["The TTFT model is not validated, and its figures are not a bound. On the validation recordings (not this fleet) it read high at an assumed prompt spread and up to 33% low at the spread actually sent; the banner gives the figures.", "a fresh frozen staircase run that validates the model"],
  AssumedVP: ["V_P, the prefill token rate, is the value you typed, not a learned one.", "learn V_P: run at a load that keeps a prefill pod busy for a minute"],
  VPLowerBound: ["V_P comes from a lower bound (the peak under a queue): the limits under-promise.", "a window where a prefill pod is busy without a queue"],
  VPAboveSlopeBound: ["V_P is above what the low-load fit allows: one of the two readings is wrong.", "more low-load readings, or a busy-minutes reading of V_P"],
  VPScaledToPrompt: ["V_P was read at another prompt length and rescaled along the fitted curve.", "a V_P reading at this prompt length"],
  VPShapeMoved: ["V_P was read at a prompt length far from this one and held: on longer prompts it over-promises.", "a V_P reading at this prompt length"],
  AssumedImbalance: ["The load imbalance f between prefill pods is assumed, not learned.", "a window with per-pod prefill load"],
  AssumedPromptSpread: ["The prompt-length spread (cv) is assumed, not read from a histogram.", "a prompt-length histogram (recorder R1)"],
  SpreadBeyondProcessorSharing: ["The prompt spread is wider than processor sharing was validated at: TTFT is the larger of the processor-sharing and first-come prices.", "nothing: a property of the traffic"],
  TransferOmitted: ["The KV transfer time is not in the TTFT: it over-promises by it.", "collect nixl_xfer_time_seconds"],
  TransferHeld: ["The KV transfer time was read on earlier cycles and held.", "a quiet reading of the transfer at this load"],
  DecodeSideOmitted: ["Decode's part of the TTFT (its admission wait and first token) is left out: the TTFT over-promises.", "a decode ITL line or the decode engine's TTFT"],
  DecodeTTFTUnmeasured: ["Decode's TTFT was not measured, so the KV pull and first token are left out (25% to 65% on the recordings).", "the decode engine's TTFT histogram"],
  DecodeQueueUnread: ["The decode engine does not say why requests wait, so a decode TTFT measured while decode was queueing is not caught: the floor may be a queue.", "vLLM v0.20.0 or later, which reports the capacity wait"],
  DecodeEngineMeasured: ["Decode's part of the TTFT is the decode engine's measured TTFT, in place of the admission wait.", "a decode ITL line"],
  DecodeTermOmitted: ["The decode engine's TTFT has no fit: the older, smaller sum stands for it, and the TTFT over-promises.", "readings of the decode engine's TTFT across load"],
  DecodeStepSlopeAssumed: ["The decode term rises with an assumed slope from where it was measured.", "readings of the decode engine's TTFT across load"],
  DecodeTermFloored: ["The decode term read under the older sum, which stands in its place: its fit does not hold here.", "readings of the decode engine's TTFT at this prompt"],
  DecodeTermPrefixHits: ["Prefix-cache hits are high: the decode term reads low.", "nothing: a property of the traffic"],
  RouterHopAssumed: ["The router-to-engine hop is an assumed constant, not a fit.", "a settled fit of the router hop"],
  OutputSpreadUnread: ["The output-length spread was not read: KV per request may over-promise by 7% to 44%.", "an output-length histogram"],
  FlowControlInferredOff: ["Flow control is taken as off because its queue series is missing (a scrape gap looks the same).", "scrape the router's flow-control series"],
  AboveFlowControlOnset: ["The rate is past where the router starts holding requests: the TTFT there is priced per pod and is not proven conservative.", "a lower rate, or more prefill replicas"],
  FlowControlWaitPriced: ["The router's wait is priced with the experimental switch: argued, not measured.", "a staircase step with a real router wait"],
  HeldByDecodeAdmission: ["Decode admission's wait holds the prefill limit down: add a decode replica, not a prefill one.", "one more decode replica"],
  DecodeOnsetUnchecked: ["The decode gate's onset could not be placed: the answer may stand above it.", "price with the decode role"],
  AssumedTPOTRatio: ["The TPOT percentile is the mean times a ratio R_q you assumed.", "the router's per-request TPOT histogram (recorder R1)"],
  CoarseTPOTBucket: ["R_q was read off a histogram bucket wider than a quarter of the mean: the bucket grid sets it.", "finer TPOT histogram buckets"],
  EngineTPOTHistogram: ["R_q comes from the engine's TPOT histogram, not the router's.", "the router's per-request TPOT histogram"],
  MeanBoundedTPOTPercentile: ["R_q was read below its coarse histogram bucket's edge, at the most the window's mean allows: never below the true percentile, but still wide.", "finer TPOT histogram buckets"],
  GlobalTPOTRatio: ["Too few windows were read at this fill: R_q is the one read over every fill.", "readings at this load"],
  BelowFittedBand: ["The fill is below the band the ITL line was fitted over: TPOT is read at the band's bottom, a safe floor.", "readings at lower fill"],
  KVBoundSlots: ["Decode slots are set by KV capacity, an approximation.", "nothing: a property of the fleet"],
  BorrowedLine: ["The ITL line was borrowed from another variant with the same engine configuration.", "this variant's own readings"],
  HeldLine: ["The ITL line was held from an earlier fit: the window had too few readings.", "a window with more steady readings"],
  TierUnstable: ["Resamples fit the ITL line at another tier and move the answer by more than 5%.", "more steady readings"],
  LineNearGate: ["The ITL line sits near the tracker's gate; the answer does not care which side (informational).", "nothing"],
  ExtrapolatedFill: ["The fill is above the highest fill the ITL line was read at: extrapolated.", "readings at higher load"],
  AssumedShape: ["Priced at a prompt or output length you set, not the measured one.", "price at the current mix"],
  ExtrapolatedShape: ["The request shape is outside the prompt and output lengths the source saw: the figures extrapolate the learned lines past the traffic they were fitted on.", "a source that served this shape"],
  DecodeOnly: ["The prefill side is not priced: the report judged decode alone, gives no fleet rate at the SLO, and never calls the fleet over-provisioned.", "a learned or assumed prefill speed (--assume-vp)"],
  TailPastPromptHistogram: ["A TTFT percentile above p95 on a prompt spread no histogram measured: the tail rests on the assumed spread.", "a source with a computed-prompt histogram"],
  EdgeBeyondContext: ["A bucket's edge is past the engines' context window (--max-model-len): they refuse prompts that long, and this \"if all traffic were\" line prices traffic they would not serve.", "an edge within the context window"],
  PullUnseen: ["The prefill load is past the pull table's highest bucket: the pull reads low.", "readings at higher prefill load"],
  OverheadMeasured: ["The fixed overhead comes from the prefill pods' histograms.", "nothing (informational)"],
  AssumedPoissonArrivals: ["Arrivals are taken as Poisson: on bursty traffic the TTFT over-promises.", "a stated arrival burstiness (Ca²)"],
  AssumedArrivalCV2: ["The arrival burstiness Ca² is a value you set.", "nothing: a stated assumption"],
  AssumedA: ["a, the fixed latency, is the value you typed, not a learned one.", "a low-load window that fits a"],
  AFromEngineTiming: ["a, the fixed latency, was fitted from the engine's own prefill timing on a fleet too busy for low-load minutes, not from quiet minutes.", "a low-load window that fits a"],
  AssumedITLB: ["The decode ITL line's intercept B, its latency per token with the cache empty, is the value you typed or the one the floor below the band gives, not a measured one; the slope was refitted through it.", "a recording whose decode line measured its own B (a full fit)"],
  AdaptedBandFloor: ["The decode ITL line was read over a KV-fill band whose floor the replay lowered to the fleet's fill, below the policy's floor.", "readings at the policy's fill floor or above"],
  PromptSpreadAbovePeak: ["The prompt cv is past the peak for this percentile: a larger cv can price a shorter floor and a higher rate.", "a measured prompt spread"],
  PromptSpreadClamped: ["The measured prompt spread was clamped at its stop.", "a prompt histogram with wider bounds"],
  RoutingBound: ["One prefill pod takes most of the work: more pods would mostly sit idle behind the router.", "fix the routing, not the replica count"],
  ImbalanceNotSustained: ["The imbalance f was high but has not held over the window: priced at it, the cautious side.", "a longer window"],
  FlowControlMismatch: ["The declared flow-control setting disagrees with the router's series: priced as declared.", "match the policy to the router"],
  LineMismatch: ["The recording's readings do not support the ITL line: the figure is printed, flagged.", "a window with readings across the load"],
  MoEDataParallel: ["A MoE engine with expert parallelism and several data-parallel ranks: the ranks step together at the busiest one's pace, but the fill priced is their mean, so the busiest rank can run out of KV first.", "nothing yet: the busiest rank is not modeled"],
  StepBudgetBindsBatch: ["The batch priced, times the tokens each sequence takes a step (1 plus the speculative tokens), is past the engine's step token budget: the engine runs fewer sequences than the knee assumes.", "a larger --max-num-batched-tokens, or --batch-cap at the budget's batch"],
  PastCUDAGraphCeiling: ["The batch priced is past the largest the engine captures a CUDA graph for, or the engine runs no graphs: those steps run eager and slower than the line.", "a capture size at or above the batch"],
  HybridAttention: ["The model mixes full attention with sliding-window, chunked or linear layers: the KV a request holds and the ITL line are not those of full attention.", "nothing yet: hybrid attention is not modeled"],
  MLAAttention: ["The model uses multi-head latent attention: KV is small, so the batch cap or the step budget, not the KV, usually bounds the batch. The CLI's flags line says which.", "nothing (informational)"],
  HostBoundTPOTTail: ["The engine's latency per token with an empty cache is under 5 ms: the TPOT percentile's tail is host jitter and reads low.", "a mean TPOT target"],
  KVConnectorUnmodeled: ["A KV connector other than NIXL: the transfer was modeled on NIXL only.", "nothing yet"],
  KVOffload: ["KV offloading past GPU memory: capacity is priced on the GPU's KV only, and a request served from the offload tier is slower than the line.", "nothing yet"],
  SGLangNoMixedChunk: ["An aggregated SGLang pod without --enable-mixed-chunk: a prefill step stalls every running sequence.", "--enable-mixed-chunk"],
  ExtensibleKVCache: ["vLLM's extensible KV cache: the KV capacity read at start-up can grow while the engine runs.", "--no-enable-extensible-kv-cache, or a recording of the grown cache"],
  DefaultBatchCapBelowRelease: ["The batch cap priced is the parser's default of 256, but this vLLM release runs 1024 on this accelerator: the decode figures can ask for up to 4 times too many replicas.", "--max-num-seqs on the engine, or --batch-cap"],
  BatchBeyondFit: ["The shape's KV per request is under half that of the rows the ITL line was read on, and the batch priced is past the largest they held: ITL also rises with batch tokens, which the line does not carry across shapes.", "a recording at this shape"],
};

// BUCKET_NOTE is what the per-bucket lines are, every time they are drawn.
const BUCKET_NOTE = "If all traffic were bucket b: each line is its own fleet's answer, every prompt at the bucket's upper edge. Not the mix; no limit is read from it.";

// TERM_NAMES are the inputs' names as the page shows them.
const TERM_NAMES = {
  vp: "V_P", a: "a", imbalance: "f", promptTokens: "prompt", promptCV: "prompt cv",
  transferSecondsPerToken: "KV transfer", routerHop: "router hop", itlA: "ITL a", itlB: "ITL b",
  itlLine: "ITL line", itlBFloor: "ITL b from the floor", capacityTokens: "KV capacity", kvPerRequest: "KV per request", batchCap: "batch cap", maxModelLen: "context window",
  tpotRatio: "R_q", prefill: "prefill",
};

// PAGE_TTFT and PAGE_TPOT are the objectives the page sets when the policy
// sets none: a TTFT target is required to read a prefill limit.
const PAGE_TTFT = {statistic: "p90", seconds: 1};
const PAGE_TPOT = {statistic: "mean", seconds: 0.05};

const TTFT_STATS = ["mean", "p50", "p90", "p95", "p99"];
const TPOT_STATS = ["mean", "p50", "p90", "p95", "p99"];

// state is the page's: the sources, the inputs loaded, the knobs, the last
// plan drawn, the question asked (mode: "plan", what a fleet serves, or
// "size", what fleet a rate needs), the last size answered, the plan pinned
// as A to compare against (pin), whether the last price or size failed
// (priceFailed), and the link whose knobs a load is to apply (link).
const state = {sources: null, inputs: null, plan: null, k: null, inFlight: false, dirty: false, seq: 0, dragging: false,
  mode: "plan", modeSeq: 0, size: null, abort: null, pin: null, priceFailed: false, link: null};

// SIZE_BOUND is the replicas a role a size searches up to unless you set
// another: the API's default.
const SIZE_BOUND = 32;

const el = id => document.getElementById(id);

// esc, which every string from the API passes through on its way into the
// page, is in scenario.js, loaded first.

// val is a figure's value, or null where it is refused or absent.
function val(f) {
  return f && !f.refused && typeof f.value === "number" ? f.value : null;
}

// why is a refused figure's reason, with its detail.
function why(f) {
  if (!f) return "not priced";
  if (!f.refused) return "";
  return f.refused.reason + (f.refused.detail ? ": " + f.refused.detail : "");
}

function fmtRate(v) {
  if (v == null) return "refused";
  const a = Math.abs(v);
  return v.toFixed(a >= 100 ? 0 : a >= 10 ? 1 : 2) + " r/s";
}

function fmtSec(v) {
  if (v == null) return "refused";
  if (v < 1) {
    const ms = v * 1000;
    return ms.toFixed(ms >= 100 ? 0 : 1) + " ms";
  }
  return v.toFixed(2) + " s";
}

function fmtNum(v) {
  if (Math.abs(v) >= 1000) return Math.round(v).toLocaleString("en-US");
  return String(Number(v.toPrecision(4)));
}

// figPlain is a figure as plain text: the number, or "refused (reason)".
function figPlain(f, fmt) {
  const v = val(f);
  return v != null ? fmt(v) : "refused (" + (f && f.refused ? f.refused.reason : "not priced") + ")";
}

// figText is figPlain escaped, for HTML.
function figText(f, fmt) {
  return esc(figPlain(f, fmt));
}

// spanPlain is a figure's span over V_P and f, for a rate, as plain text.
function spanPlain(f) {
  const s = f && f.sensitivity;
  if (!s) return "";
  const lo = s.lo != null ? fmtRate(s.lo) : "refused (" + s.loRefused + ")";
  const hi = s.hi != null ? fmtRate(s.hi) : "refused (" + s.hiRefused + ")";
  // Ends that print the same are one figure: V_P and f do not move it.
  if (lo === hi) return lo + " at every V_P and f corner";
  return lo + " to " + hi + " over V_P and f";
}

// spanText is spanPlain escaped, for HTML.
function spanText(f) {
  return esc(spanPlain(f));
}

// shapeOf is what the plan was priced at, in words, read from the scenario
// the API priced, never from the knobs: the current mix, or a stated shape.
function shapeOf(p) {
  const m = p.scenario.mix;
  if (!m || m === "observed") return "the current mix";
  const parts = [];
  if (m.promptTokens) parts.push(`a ${fmtNum(m.promptTokens)}-token prompt`);
  if (m.outputTokens) parts.push(`a ${fmtNum(m.outputTokens)}-token output`);
  const flagged = (p.limits.fleet.flags || []).includes("AssumedShape") ? ", AssumedShape" : "";
  return parts.join(" and ") + " (stated shape" + flagged + ")";
}

// SHOWN_FLAGS change how far to trust a figure, so they are shown beside it;
// the rest are counted there and listed in the flags drawer. UnvalidatedTTFT
// is the banner, on every TTFT figure.
const SHOWN_FLAGS = new Set(["AboveFlowControlOnset", "FlowControlWaitPriced", "SpreadBeyondProcessorSharing", "HeldByDecodeAdmission",
  "AssumedTPOTRatio", "AssumedVP", "AssumedA", "AssumedShape", "AssumedPromptSpread", "AssumedImbalance", "AssumedArrivalCV2"]);

// flagChips is a figure's flags: the ones that change its trust by name,
// with their words on hover, and how many more the drawer lists.
function flagChips(f) {
  const flags = (f && f.flags) || [];
  const shown = flags.filter(n => SHOWN_FLAGS.has(n)), rest = flags.filter(n => !SHOWN_FLAGS.has(n) && n !== "UnvalidatedTTFT");
  return shown.map(n => `<span class="chip flagged" title="${esc((FLAG_WORDS[n] || [n])[0])}">${esc(n)}</span>`).join(" ") +
    (rest.length ? ` <span class="chip" title="${esc(rest.join(", "))}">+${rest.length} more flags</span>` : "");
}

// setStatus writes the status line, which a screen reader announces. While
// the rate is dragged it holds its news until the release, but for errors.
function setStatus(text, error) {
  if (state.dragging && !error) return;
  el("status").textContent = text;
  el("status").classList.toggle("error", !!error);
}

// api calls the API on this origin, by a path relative to the page,
// retrying a busy server's 503 (retryDelay, in scenario.js), never a plan
// stopped at the server's plan budget. A signal aborts the request, and the
// server stops pricing it.
async function api(method, path, body, signal) {
  for (let attempt = 0; ; attempt++) {
    const opt = {method, headers: {}, signal};
    if (body !== undefined) {
      opt.headers["Content-Type"] = "application/json";
      opt.body = JSON.stringify(body);
    }
    const resp = await fetch(path, opt);
    let doc = null;
    try { doc = await resp.json(); } catch (e) { doc = null; }
    const wait = retryDelay(resp.status, resp.headers.get("Retry-After"), attempt);
    if (wait >= 0) {
      await new Promise(r => setTimeout(r, wait));
      continue;
    }
    if (!resp.ok) {
      let msg = (doc && doc.error) || resp.statusText;
      if (resp.status === 401) msg += ". This server checks a bearer token: open the page through a proxy that adds it.";
      throw new Error(resp.status + ": " + msg);
    }
    return doc;
  }
}

// ---- The source ----

async function start() {
  wire();
  try {
    state.sources = await api("GET", "v1/sources");
  } catch (e) {
    setStatus("Could not list the sources: " + e.message, true);
    return;
  }
  const s = state.sources;
  el("ns").innerHTML = s.namespaces.map(n => `<option${n === s.namespace ? " selected" : ""}>${esc(n)}</option>`).join("");
  s.uploads = s.uploads || [];
  el("rec").innerHTML = s.recordings.map(n => `<option value="${esc(n)}">${esc(recordingLabel(n, s.uploads))}</option>`).join("");
  el("rectrunc").hidden = !s.recordingsTruncated;
  el("kprom").disabled = !s.prometheus;
  el("krec").disabled = s.recordings.length === 0;
  el("upfield").hidden = !s.upload;
  if (s.upload) {
    el("uphint").textContent = `.jsonl or .jsonl.gz, at most ${mib(s.upload.maxBytes)}. The server keeps the last 4 until it restarts; everyone who opens this page sees them, with their file names.`;
  }
  // The page opens on a Prometheus window, the last hour ending now; a
  // server with recordings alone opens on its first.
  let kind = s.prometheus ? "prometheus" : s.recordings.length ? "recording" : "";
  // A link naming a source this server does not read is refused, as Open
  // refuses it: no other source is loaded in its place.
  const refused = linked.get("recording") || linked.get("namespace") ? sourceProblem(linked.toString(), s) : "";
  if (!refused) kind = linkSource() || kind;
  showScenarios();
  if (!kind && s.upload) {
    setStatus("Upload a recording to start.");
    el("source").open = true;
    return;
  }
  if (!kind) {
    setStatus("This server reads no source: start it with --prometheus or --recordings.", true);
    el("source").open = true;
    return;
  }
  (kind === "prometheus" ? el("kprom") : el("krec")).checked = true;
  const to = new Date(), from = new Date(to.getTime() - 3600e3);
  el("from").value = localInput(from);
  el("to").value = localInput(to);
  showKind();
  if (refused) {
    setStatus(`The link was not opened: ${refused}. Pick a source and Load.`, true);
    el("source").open = true;
    return;
  }
  state.link = {seq: null, restore: null};
  loadInputs();
}

// localInput is t as a datetime-local input's value.
function localInput(t) {
  const p = n => String(n).padStart(2, "0");
  return `${t.getFullYear()}-${p(t.getMonth() + 1)}-${p(t.getDate())}T${p(t.getHours())}:${p(t.getMinutes())}`;
}

function kindNow() {
  return el("kprom").checked ? "prometheus" : "recording";
}

function showKind() {
  const k = kindNow();
  document.querySelectorAll("[data-kind]").forEach(f => { f.hidden = f.dataset.kind !== k; });
  el("from").disabled = el("to").disabled = el("lasthour").checked;
}

// sourceRequest is the source the form names, or an error saying what the
// form lacks.
function sourceRequest() {
  if (kindNow() === "recording") {
    if (!el("rec").value) throw new Error("pick a recording");
    return {kind: "recording", name: el("rec").value, cycle: Math.max(0, Math.floor(+el("cycle").value || 0))};
  }
  const src = {kind: "prometheus", namespace: el("ns").value};
  if (!el("lasthour").checked) {
    const from = new Date(el("from").value), to = new Date(el("to").value);
    if (isNaN(from) || isNaN(to)) throw new Error("give both From and To, or tick the last hour");
    src.from = from.toISOString().replace(/\.\d{3}Z$/, "Z");
    src.to = to.toISOString().replace(/\.\d{3}Z$/, "Z");
  }
  return src;
}

// loadInputs resolves the source the form names. A response that a later
// load overtook is dropped. The link's knobs (state.link: the page's own
// link, or a scenario opened) are applied by one load, as linkAtLoadStart and
// linkAtLoadEnd decide: the newest load takes the link over; the page's link
// waits for the first load that succeeds; a scenario whose load fails puts
// back what was there, and no later load applies it.
async function loadInputs() {
  const seq = ++state.seq;
  state.link = linkAtLoadStart(state.link, seq);
  el("load").disabled = true;
  let tick = 0;
  let source = null;
  try {
    const req = {source: sourceRequest()};
    source = req.source;
    if (el("model").value.trim()) req.model = el("model").value.trim();
    if (el("pvariant").value.trim()) req.prefillVariant = el("pvariant").value.trim();
    if (el("dvariant").value.trim()) req.decodeVariant = el("dvariant").value.trim();
    const loading = req.source.kind === "prometheus" ? "Reading the Prometheus window (seconds to a minute)" :
      "Replaying the recording (a day-long one takes a minute or two; asking again starts a second load)";
    setStatus(loading + "...");
    const t0 = Date.now();
    tick = setInterval(() => {
      if (seq === state.seq) setStatus(`${loading}... ${Math.round((Date.now() - t0) / 1000)} s`);
    }, 1000);
    const inputs = await api("POST", "v1/inputs", req);
    clearInterval(tick);
    if (seq !== state.seq) return;
    setInputs(inputs);
    const end = linkAtLoadEnd(state.link, seq, state.seq, true);
    state.link = end.link;
    if (end.apply) applyLink();
    el("source").open = false;
    plan();
  } catch (e) {
    clearInterval(tick);
    const end = linkAtLoadEnd(state.link, seq, state.seq, false);
    state.link = end.link;
    let opened = "";
    if (end.restore) {
      restorePage(end.restore);
      opened = " The scenario was not opened; the page keeps the one before.";
    }
    // An upload pushed out by newer ones is no longer offered.
    if (source && source.kind === "recording" && source.name.startsWith("upload:") &&
        e.message.startsWith("400:") && e.message.includes("no such upload")) {
      forgetUpload(source.name);
    }
    setStatus("Could not load the inputs: " + e.message + opened, true);
    el("source").open = true;
  } finally {
    el("load").disabled = false;
  }
}

// PLAN_FIGURES are the elements a plan fills: the limits, the bars, the +1
// steps, the points at the rate, the charts and the tables.
const PLAN_FIGURES = ["fleet", "fleetn", "chosen", "bars", "barsdesc", "limits", "marginal", "ratenote", "limitsfor",
  "ttftat", "tpotat", "rationote", "ttftchart", "ttftlegend", "ttftpoints", "buckettable",
  "tpotchart", "tpotlegend", "tpotpoints", "curvetable", "flags", "flagcount"];

// clearPlan takes the last plan off the page. A new source, or a plan that
// failed, must not leave another scenario's figures on screen as if current.
function clearPlan() {
  state.plan = null;
  for (const id of PLAN_FIGURES) el(id).textContent = "";
  el("banner").hidden = true;
}

// mib is n bytes in MiB, for the upload's words.
function mib(n) {
  return `${Math.round(n / 1048576)} MiB`;
}

// uploadRecording sends the file picked to the server (POST v1/recordings),
// showing how much of it has gone, then picks it as the recording and loads
// it. The server names it by its bytes; the file's own name is sent as a
// label (?filename=), listed beside it and never used on disk.
// XMLHttpRequest, not fetch: only it reports an upload's progress.
function uploadRecording() {
  const file = el("upfile").files[0];
  if (!file) {
    setStatus("Pick a file to upload.", true);
    return;
  }
  const max = state.sources.upload.maxBytes;
  if (file.size > max) {
    setStatus(`${file.name} is ${mib(file.size)}, more than this server takes (${mib(max)}).`, true);
    return;
  }
  el("upload").disabled = el("load").disabled = true;
  const done = () => { el("upload").disabled = el("load").disabled = false; };
  // A server taking another upload answers 503: tried again as api() does.
  const send = attempt => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", "v1/recordings?filename=" + encodeURIComponent(file.name));
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    xhr.upload.onprogress = e => {
      if (e.lengthComputable) setStatus(`Uploading ${file.name}: ${Math.floor(100 * e.loaded / e.total)}%`);
    };
    xhr.upload.onload = () => setStatus(`Checking ${file.name}: the server reads every line once...`);
    xhr.onerror = () => { done(); setStatus(`Could not upload ${file.name}: the connection failed.`, true); };
    xhr.onload = () => {
      const wait = retryDelay(xhr.status, xhr.getResponseHeader("Retry-After"), attempt);
      if (wait >= 0) {
        setStatus(`The server is taking another upload; sending ${file.name} again in ${Math.round(wait / 1000)} s...`);
        setTimeout(() => send(attempt + 1), wait);
        return;
      }
      uploaded(xhr);
    };
    xhr.send(file);
  };
  const uploaded = xhr => {
    done();
    let doc = null;
    try { doc = JSON.parse(xhr.responseText); } catch (e) { doc = null; }
    if (xhr.status !== 200 && xhr.status !== 201) {
      setStatus(`Could not upload ${file.name}: ${xhr.status}: ${(doc && doc.error) || xhr.statusText}`, true);
      return;
    }
    if (!doc || typeof doc.name !== "string" || !doc.name) {
      setStatus(`Could not upload ${file.name}: the server's answer was not the upload's name.`, true);
      return;
    }
    const s = state.sources;
    s.uploads = [{name: doc.name, fileName: doc.fileName, bytes: doc.bytes, uploadedAt: doc.uploadedAt},
      ...s.uploads.filter(u => u.name !== doc.name)];
    s.recordings = [doc.name, ...s.recordings.filter(n => n !== doc.name)];
    const rec = el("rec");
    for (const o of Array.from(rec.options)) {
      if (o.value === doc.name) o.remove();
    }
    rec.insertAdjacentHTML("afterbegin", `<option value="${esc(doc.name)}">${esc(recordingLabel(doc.name, s.uploads))}</option>`);
    el("rec").value = doc.name;
    el("cycle").value = 0;
    el("krec").disabled = false;
    el("krec").checked = true;
    showKind();
    setStatus(`Uploaded ${file.name} as ${recordingLabel(doc.name, s.uploads)}.`);
    loadInputs();
  };
  send(0);
}

// forgetUpload removes an upload the server no longer keeps from the
// recordings offered.
function forgetUpload(name) {
  const s = state.sources;
  s.recordings = s.recordings.filter(n => n !== name);
  s.uploads = (s.uploads || []).filter(u => u.name !== name);
  const rec = el("rec");
  for (const o of Array.from(rec.options)) {
    if (o.value === name) o.remove();
  }
  el("krec").disabled = s.recordings.length === 0;
}

// setInputs starts a session from the inputs: the recorded replicas, the
// policy's SLO and headroom, the observed mix. The rate starts at the
// operating point the first plan returns: the source carries no rate.
function setInputs(inputs) {
  const badge = selfCheckBadge(inputs.selfCheck);
  el("sccheck").hidden = !badge;
  el("scbox").hidden = !badge;
  if (badge) {
    el("sccheck").textContent = badge.text;
    el("sccheck").className = "chip selfcheck " + badge.cls;
    el("scbox").innerHTML = selfCheckHTML(inputs.selfCheck);
  }
  el("rrun").disabled = false;
  state.reportGen = (state.reportGen || 0) + 1;
  el("rresult").hidden = true;
  el("rstatus").textContent = "";
  state.inputs = inputs;
  state.day = null;
  el("dayout").hidden = true;
  el("dayrun").disabled = !inputs.fleet.prefill;
  el("dayrun").title = inputs.fleet.prefill ? "" : "A day plan sizes a P/D fleet: the source has no prefill variant";
  state.bt = null;
  el("btout").hidden = true;
  el("btrun").disabled = !inputs.fleet.prefill;
  el("btrun").title = inputs.fleet.prefill ? "" : "A backtest prices a P/D fleet: the source has no prefill variant";
  const loaded = inputs.source && inputs.source.name;
  const others = (state.sources ? state.sources.recordings : []).filter(n => n !== loaded).map(n => `<option>${esc(n)}</option>`).join("");
  el("bttest").innerHTML = others;
  el("bpwindow").innerHTML = others;
  el("bcmeasured").innerHTML = (state.sources ? state.sources.recordings : []).map(n => `<option>${esc(n)}</option>`).join("");
  el("bprun").disabled = !inputs.fleet.prefill;
  el("bpout").textContent = "";
  state.priceFailed = false;
  clearPlan();
  state.size = null;
  const p = inputs.policy || {};
  // With no policy objective the page sets one, and says so beside the
  // limits until you set your own (ttftDefault, tpotDefault).
  const ttft = p.ttft || PAGE_TTFT, tpot = p.tpot || PAGE_TPOT;
  state.k = {
    prefill: inputs.fleet.prefill ? Math.max(1, inputs.fleet.prefill.replicas) : 0,
    decode: Math.max(1, inputs.fleet.decode.replicas),
    rate: null,
    ttftStat: ttft.statistic, ttftTarget: ttft.seconds, ttftDefault: !p.ttft,
    tpotStat: tpot.statistic, tpotTarget: tpot.seconds, tpotDefault: !p.tpot,
    ratio: null, headroom: p.headroom || 0.85,
    priceAs: "mix", outputTokens: val(inputs.observed.outputTokens),
    perBucket: false, edges: "",
    assume: {},
    maxRep: SIZE_BOUND, sizeLow: false,
  };
  const k = state.k;
  showLearned(inputs);
  fillStats("ttftstat", TTFT_STATS, k.ttftStat);
  fillStats("tpotstat", TPOT_STATS, k.tpotStat);
  slider("ttft", k.ttftTarget * 1000, 1);
  slider("tpot", k.tpotTarget * 1000, 0.1);
  const prompt = val(inputs.observed.promptTokens);
  el("priceas").innerHTML = `<option value="mix">the current mix${prompt != null ? " (mean prompt " + esc(fmtNum(prompt)) + " tokens)" : ""}</option>` +
    inputs.observed.buckets.map(b => `<option value="${esc(b.name)}">a ${esc(fmtNum(b.upperTokens))}-token prompt (stated shape)</option>`).join("");
  el("outnote").textContent = k.outputTokens != null ? "observed " + fmtNum(k.outputTokens) : "";
  el("outtok").value = k.outputTokens != null ? Math.round(k.outputTokens) : "";
  el("headroom").value = k.headroom;
  el("perbucket").checked = false;
  el("bucketedges").value = "";
  el("pnow").textContent = inputs.fleet.prefill ? "now " + inputs.fleet.prefill.replicas : "no prefill variant";
  el("dnow").textContent = "now " + inputs.fleet.decode.replicas;
  el("ratio").value = "";
  showSource(inputs);
  showInputs(inputs);
  syncControls();
  showScenarios(el("scenlist").value);
}

// ASSUMABLE are the terms a scenario may type: the box, the API's key, the
// inputs' term, and the box's unit per the term's (ms for seconds). refusedAs
// is the term the inputs name instead when the source refused it: ITL B is
// refused with the whole line (PinnedDefaultB), and floor is the term giving
// the B the floor below the band would price at.
const ASSUMABLE = [
  {box: "vp", key: "vp", term: "vp", scale: 1},
  {box: "a", key: "a", term: "a", scale: 1000},
  {box: "cv", key: "promptCV", term: "promptCV", scale: 1},
  {box: "hop", key: "routerHop", term: "routerHop", scale: 1000},
  {box: "itlb", key: "itlB", term: "itlB", refusedAs: "itlLine", floor: "itlBFloor", scale: 1000},
];

// showLearned puts each assumable term's learned value, or its refusal,
// beside its box, and opens the box when the source refused one of them.
function showLearned(inputs) {
  let refused = false;
  for (const a of ASSUMABLE) {
    el("a-" + a.box).value = "";
    // A line refused for any other reason (NoLine) has no B to state.
    const t = inputs.terms.find(x => x.name === a.term) ||
      (a.refusedAs && inputs.terms.find(x => x.name === a.refusedAs && x.refused && x.refused.reason === "PinnedDefaultB"));
    let text = "not read";
    if (t && t.refused) {
      text = "refused: " + t.refused.reason;
      const floor = a.floor && inputs.terms.find(x => x.name === a.floor && x.value != null);
      if (floor) text += "; the floor below the band gives " + fmtNum(floor.value * a.scale);
      refused = true;
    } else if (t) {
      text = "learned " + fmtNum(t.value * a.scale);
    }
    el("l-" + a.box).textContent = text;
  }
  if (inputs.terms.some(x => x.name === "prefill" && x.refused)) refused = true;
  el("assumebox").open = refused;
  el("assumecount").textContent = "none typed";
}

function fillStats(id, stats, current) {
  const all = stats.includes(current) ? stats : stats.concat([current]);
  el(id).innerHTML = all.map(s => `<option${s === current ? " selected" : ""}>${esc(s)}</option>`).join("");
}

// slider sets a target's slider around v (ms): from a twentieth of it to four
// times it; the number box takes any value.
function slider(id, v, step) {
  const r = el(id);
  r.min = Math.max(step, v / 20);
  r.max = v * 4;
  r.step = step;
  r.value = v;
  el(id + "box").value = Number(v.toFixed(step < 1 ? 1 : 0));
}

function showSource(inputs) {
  const s = inputs.source;
  let text;
  if (s.kind === "prometheus") {
    const t = d => new Date(d).toISOString().slice(0, 16).replace("T", " ");
    text = `Prometheus window ${t(s.from)} to ${t(s.to)} UTC`;
  } else {
    text = `Recording ${s.name}, cycle ${s.cycle} of ${s.totalCycles}`;
  }
  el("srcsum").textContent = text;
  el("srcchip").textContent = "Source: " + text;
  el("srcchip").hidden = false;
  const fleet = inputs.fleet;
  el("what").textContent = `${inputs.model} · namespace ${inputs.namespace} · ` +
    (fleet.prefill ? `prefill ${fleet.prefill.variant}, decode ${fleet.decode.variant}` : `decode ${fleet.decode.variant}`);
}

// showInputs is the provenance chips: every term with its source as the API
// names it, its flags, or its refusal. The API marks no term "assumed" by
// itself: a term with flags is drawn flagged, and the source's assumptions
// are listed below in the API's words.
function showInputs(inputs) {
  let refused = 0, flagged = 0;
  el("terms").innerHTML = inputs.terms.map(t => {
    const name = TERM_NAMES[t.name] || t.name;
    let cls = "learned", text, title = t.source || "";
    if (t.refused) {
      cls = "refused"; refused++;
      text = `${esc(name)} <b>refused</b> ${esc(t.refused.reason)}`;
      title = t.refused.detail;
    } else {
      if (t.flags && t.flags.length) { cls = "flagged"; flagged++; }
      text = `${esc(name)} <b>${esc(fmtNum(t.value))}</b>${t.unit ? " " + esc(t.unit) : ""}` + (t.source ? ` · ${esc(t.source)}` : "");
      if (t.flags && t.flags.length) title += " | " + t.flags.join(", ");
    }
    return `<span class="chip ${cls}" title="${esc(title)}">${text}</span>`;
  }).join("");
  el("assumed").innerHTML = (inputs.assumed || []).map(a => `<li>assumed: ${esc(a)}</li>`).join("");
  const s = inputs.source;
  state.inputSum = `cycle ${s.cycle} of ${s.totalCycles}, ${inputs.terms.length} terms, ${refused} refused, ${flagged} flagged, ` +
    `${(inputs.assumed || []).length} assumed by the source`;
  showTyped();
}

// showTyped counts the terms you typed, beside their boxes and in the inputs'
// summary.
function showTyped() {
  const n = state.k ? Object.keys(state.k.assume).length + (state.k.ratio != null && state.k.tpotStat !== "mean" ? 1 : 0) : 0;
  el("assumecount").textContent = n ? n + " typed" : "none typed";
  el("inputsum").textContent = (state.inputSum || "") + `, ${n} typed by you`;
}

// ---- The link ----

// The URL's fragment carries the source and the knobs, so a link reopens the
// same scenario: #recording=run.jsonl.gz&prefill=2&rate=6&ttft=0.8 and so on.
// Seconds are seconds, as the API takes them.
// A saved or imported scenario is opened by replacing it.
let linked = new URLSearchParams(location.hash.slice(1));

// linkSource puts the source the link names into the source form: the
// recording and its cycle, or the namespace, and the model and variants. It is
// the source's kind, or "" where the link names none this server reads.
function linkSource() {
  const s = state.sources;
  let kind = "";
  if (linked.get("recording") && s.recordings.includes(linked.get("recording"))) {
    kind = "recording";
    el("rec").value = linked.get("recording");
    el("cycle").value = +linked.get("cycle") || 0;
  } else if (linked.get("namespace") && s.prometheus && s.namespaces.includes(linked.get("namespace"))) {
    kind = "prometheus";
    el("ns").value = linked.get("namespace");
  }
  el("model").value = linked.get("model") || "";
  el("pvariant").value = linked.get("prefillVariant") || "";
  el("dvariant").value = linked.get("decodeVariant") || "";
  return kind;
}

// applyLink sets the knobs the link names, once, on the first inputs loaded.
function applyLink() {
  const k = state.k, g = n => linked.get(n), num = n => (g(n) !== null && g(n) !== "" && isFinite(+g(n)) ? +g(n) : null);
  for (const [n, key] of [["prefill", "prefill"], ["decode", "decode"], ["rate", "rate"], ["ttft", "ttftTarget"], ["tpot", "tpotTarget"],
    ["ratio", "ratio"], ["headroom", "headroom"], ["out", "outputTokens"]]) {
    if (num(n) != null) k[key] = num(n);
  }
  if (g("ttftStat")) k.ttftStat = g("ttftStat");
  if (g("tpotStat")) k.tpotStat = g("tpotStat");
  // A link's objectives are its author's, not the page's defaults.
  if (num("ttft") != null || g("ttftStat")) k.ttftDefault = false;
  if (num("tpot") != null || g("tpotStat")) k.tpotDefault = false;
  // A bucket the inputs do not name prices the current mix.
  k.priceAs = state.inputs.observed.buckets.some(b => b.name === g("priceAs")) ? g("priceAs") : "mix";
  // Edges the link names are used as parseEdges reads them, and the link is
  // rewritten in their canonical form; edges it cannot read are said.
  const le = linkedEdges(g("edges"));
  k.edges = le.edges;
  state.linkNote = le.note;
  for (const a of ASSUMABLE) if (num(a.key) != null) k.assume[a.key] = num(a.key);
  if (num("max") != null) k.maxRep = Math.min(64, Math.max(1, Math.round(num("max"))));
  k.sizeLow = g("low") === "1";
  // A scenario opened in the other mode switches the question. The
  // per-bucket view is the plan's: a size link carries none, and the view
  // stays off when you switch to the plan from it.
  if (g("mode") === "size" && state.inputs.fleet.prefill) {
    k.perBucket = false;
    enterSize();
    state.size = null;
  } else {
    enterPlan();
    k.perBucket = g("perBucket") === "1";
  }
  fillStats("ttftstat", TTFT_STATS, k.ttftStat);
  fillStats("tpotstat", TPOT_STATS, k.tpotStat);
  slider("ttft", k.ttftTarget * 1000, 1);
  slider("tpot", k.tpotTarget * 1000, 0.1);
  el("priceas").value = k.priceAs;
  if (k.outputTokens != null) el("outtok").value = Math.round(k.outputTokens);
  el("headroom").value = k.headroom;
  el("perbucket").checked = k.perBucket;
  el("bucketedges").value = k.edges;
  el("ratio").value = k.ratio != null ? k.ratio : "";
  for (const a of ASSUMABLE) if (k.assume[a.key] != null) el("a-" + a.box).value = Number((k.assume[a.key] * a.scale).toPrecision(6));
  showTyped();
  syncControls();
}

// writeLink puts the source and the knobs in the URL's fragment.
function writeLink() {
  history.replaceState(null, "", "#" + linkParams().toString());
}

// linkParams is the scenario as the link carries it: the source and the
// knobs. A saved and an exported scenario are this same text.
function linkParams() {
  const k = state.k, s = state.inputs.source, p = new URLSearchParams();
  if (s.kind === "recording") {
    p.set("recording", s.name);
    if (s.cycle) p.set("cycle", s.cycle);
  } else {
    p.set("namespace", state.inputs.namespace);
  }
  if (el("model").value.trim()) p.set("model", el("model").value.trim());
  if (el("pvariant").value.trim()) p.set("prefillVariant", el("pvariant").value.trim());
  if (el("dvariant").value.trim()) p.set("decodeVariant", el("dvariant").value.trim());
  // A size's replica counts are its answer, not a setting: the link carries
  // the question, and the bound when it is not the default.
  if (state.mode === "size") {
    p.set("mode", "size");
    if (k.maxRep !== SIZE_BOUND) p.set("max", k.maxRep);
    if (k.sizeLow) p.set("low", "1");
  } else {
    if (state.inputs.fleet.prefill) p.set("prefill", k.prefill);
    p.set("decode", k.decode);
  }
  if (k.rate != null) p.set("rate", Number(k.rate.toFixed(3)));
  // The page's default objectives are left out, so they stay marked as such.
  if (!k.ttftDefault) {
    p.set("ttftStat", k.ttftStat);
    p.set("ttft", k.ttftTarget);
  }
  if (!k.tpotDefault) {
    p.set("tpotStat", k.tpotStat);
    p.set("tpot", k.tpotTarget);
  }
  if (k.ratio != null) p.set("ratio", k.ratio);
  p.set("headroom", k.headroom);
  if (k.priceAs !== "mix") p.set("priceAs", k.priceAs);
  if (k.outputTokens != null) p.set("out", Math.round(k.outputTokens));
  for (const a of ASSUMABLE) if (k.assume[a.key] != null) p.set(a.key, k.assume[a.key]);
  if (k.perBucket) p.set("perBucket", "1");
  if (k.edges) p.set("edges", k.edges);
  return p;
}

// showEdgesBox marks the bucket edges box invalid, with why under it, for
// as long as its text is not the edges the lines show: a later status line
// or plan does not clear it.
function showEdgesBox() {
  const box = el("bucketedges"), why = state.k ? edgesBoxProblem(box.value, state.k.edges) : "";
  box.setAttribute("aria-invalid", why ? "true" : "false");
  box.classList.toggle("invalid", !!why);
  el("bucketedgeserr").textContent = why;
}

// contextWindow is the inputs' context window in tokens, or null.
function contextWindow() {
  const t = state.inputs && (state.inputs.terms || []).find(x => x.name === "maxModelLen");
  return t ? val(t) : null;
}

// ---- The plan ----

// planRequest is the scenario the knobs set. A knob left where the inputs
// put it is not sent, so the server applies its own default.
function planRequest() {
  const k = state.k, inputs = state.inputs;
  const req = {inputsId: inputs.inputsId, headroom: k.headroom,
    slo: {ttft: {statistic: k.ttftStat, target: k.ttftTarget}, tpot: {statistic: k.tpotStat, target: k.tpotTarget}}};
  const fleet = {};
  if (inputs.fleet.prefill && k.prefill !== inputs.fleet.prefill.replicas) fleet.prefill = k.prefill;
  if (k.decode !== inputs.fleet.decode.replicas) fleet.decode = k.decode;
  if (Object.keys(fleet).length) req.fleet = fleet;
  const traffic = {};
  if (k.rate != null) traffic.classes = {critical: {rate: k.rate}};
  const mix = {};
  const bucket = inputs.observed.buckets.find(b => b.name === k.priceAs);
  if (bucket) mix.promptTokens = bucket.upperTokens;
  const observedOut = val(inputs.observed.outputTokens);
  if (k.outputTokens != null && (observedOut == null || Math.abs(k.outputTokens - observedOut) >= 0.5)) mix.outputTokens = k.outputTokens;
  if (Object.keys(mix).length) traffic.mix = mix;
  if (Object.keys(traffic).length) req.traffic = traffic;
  const assume = Object.assign({}, k.assume);
  if (k.tpotStat !== "mean" && k.ratio != null) assume.tpotRatio = k.ratio;
  if (Object.keys(assume).length) req.assume = assume;
  if (k.perBucket) req.views = {allTrafficPerBucket: true};
  // The edges shape only the per-bucket view. k.edges is always parseEdges'
  // text (scenario.js), so it reads back.
  const edges = parseEdges(k.edges).edges;
  if (k.perBucket && edges.length) req.bucketEdges = edges;
  return req;
}

// sizeRequest is the size the knobs ask: the plan's scenario but the fleet,
// which is the answer, and the per-bucket view, with the bound searched.
function sizeRequest() {
  const req = planRequest();
  delete req.fleet;
  delete req.views;
  req.maxReplicas = {prefill: state.k.maxRep, decode: state.k.maxRep};
  if (state.k.sizeLow) req.sizeOn = "low";
  return req;
}

// enterSize and enterPlan switch the question. A size prices the mix line
// alone, so the per-bucket view is unticked while sizing and put back after.
function enterSize() {
  if (state.mode === "size") return;
  state.mode = "size";
  state.modeSeq++;
  state.size = null;
  state.k.perBucketBefore = state.k.perBucket;
  state.k.perBucket = false;
}

function enterPlan() {
  if (state.mode === "plan") return;
  state.mode = "plan";
  state.modeSeq++;
  state.size = null;
  state.k.perBucket = !!state.k.perBucketBefore;
}

// plan prices the knobs, or in size mode sizes the fleet for them. One
// request is in flight at a time; a knob moved meanwhile is priced when it
// returns, so the last move always lands.
let planTimer = null;
function schedule(ms) {
  reportStale();
  clearTimeout(planTimer);
  planTimer = setTimeout(plan, ms);
}

async function plan() {
  if (!state.inputs) return;
  if (state.inFlight) {
    state.dirty = true;
    // A size takes seconds: a newer question cancels it, and the server
    // stops pricing it, rather than waiting it out.
    if (state.abort) state.abort.abort();
    return;
  }
  state.inFlight = true;
  state.dirty = false;
  const seq = state.seq, inputsId = state.inputs.inputsId;
  const t0 = performance.now();
  const sizing = state.mode === "size";
  const slow = setTimeout(() => setStatus("Pricing (a new fleet or view can take seconds)..."), 300);
  // While a size runs, the last answer is dimmed and the status counts the
  // seconds.
  let ticker = null;
  if (sizing) {
    clearTimeout(slow);
    state.abort = new AbortController();
    renderCompare();
    el("sizebox").classList.add("stale");
    const tick = () => setStatus(`Sizing ${fmtRate(state.k.rate || 0)}: ${Math.round((performance.now() - t0) / 1000)} s ` +
      "(a search over replica counts, then the plan at the answer; a cold one can take 20 s or more)...");
    tick();
    ticker = setInterval(tick, 1000);
  }
  // A response priced against inputs a later load replaced, or for a
  // question since switched, is dropped.
  const modeSeq = state.modeSeq;
  const stale = () => seq !== state.seq || !state.inputs || inputsId !== state.inputs.inputsId || modeSeq !== state.modeSeq;
  try {
    if (sizing) {
      const signal = state.abort.signal;
      // A size needs a rate: with none yet, the operating point a plan of
      // the recorded fleet starts at.
      if (state.k.rate == null) {
        const first = await api("POST", "v1/plan", planRequest(), signal);
        if (stale()) return;
        state.k.rate = first.limits.chosen.rate;
      }
      const out = await api("POST", "v1/size", sizeRequest(), signal);
      if (stale()) return;
      // Answered: nothing left to cancel, and the answer may be pinned.
      state.abort = null;
      state.priceFailed = false;
      state.size = out;
      if (out.answer) {
        state.plan = out.answer.plan;
        state.k.prefill = out.answer.fleet.prefill;
        state.k.decode = out.answer.fleet.decode;
        render();
      } else {
        syncControls();
        fitRateSlider(null);
        renderSize();
        renderFlags();
        renderCompare();
      }
      writeLink();
      clearInterval(ticker);
      const what = out.answer ? `${fleetPlain(out.answer.fleet)}` : `no fleet within the bound (${out.unmet.refused.reason})`;
      setStatus(`Sized ${fmtRate(out.rate)} in ${((performance.now() - t0) / 1000).toFixed(1)} s: ${what}; ` +
        `${out.searched} fleet${out.searched === 1 ? "" : "s"} searched` + (out.answer ? ", then the plan at the answer." : "."));
      return;
    }
    const out = await api("POST", "v1/plan", planRequest());
    clearTimeout(slow);
    if (stale()) return;
    state.plan = out;
    state.priceFailed = false;
    if (state.k.rate == null) state.k.rate = out.limits.chosen.rate;
    render();
    writeLink();
    setStatus(`Priced in ${Math.round(performance.now() - t0)} ms.`);
    // A link's edges the page could not use are said once, after the plan
    // that priced the default buckets in their place.
    if (state.linkNote) {
      setStatus(state.linkNote, true);
      state.linkNote = null;
    }
  } catch (e) {
    clearTimeout(slow);
    clearInterval(ticker);
    // A size a newer question cancelled is no error: the newer one follows.
    // Any other failure clears the older scenario's plan off the page, its
    // figures and the compare table's current column: they are not this
    // scenario's.
    if (e.name !== "AbortError") {
      setStatus((sizing ? "Could not size the fleet: " : "Could not price the plan: ") + e.message +
        ". Nothing is shown until a plan prices: type a rate, or change the replicas, targets or assumed terms, to price again.", true);
      if (!stale()) {
        state.priceFailed = true;
        if (sizing) state.abort = null;
        clearPlan();
        renderCompare();
      }
    }
  } finally {
    clearInterval(ticker);
    if (sizing) {
      el("sizebox").classList.remove("stale");
      state.abort = null;
      pinControls();
    }
    state.inFlight = false;
    if (state.dirty) plan();
  }
}

// lowerKnee is the lower of the plan's knees, or null.
function lowerKnee(m) {
  const ks = [val(m.prefillKnee), val(m.decodeKnee)].filter(v => v != null);
  return ks.length ? Math.min(...ks) : null;
}

// syncControls puts the knobs into the controls.
// In size mode the steppers are the answer: they show it, read only, and
// show nothing while no fleet within the bound serves the rate.
function syncControls() {
  const k = state.k, sizing = state.mode === "size";
  const unmet = sizing && state.size && !state.size.answer;
  el("mplan").checked = !sizing;
  el("msize").checked = sizing;
  el("msize").disabled = !state.inputs.fleet.prefill;
  el("modenote").textContent = sizing ? "Set the rate and the targets; the page finds the fewest replicas a role that meet them." :
    "Set the replicas; the page prices the most they serve.";
  el("maxrow").hidden = !sizing;
  el("maxrep").value = k.maxRep;
  el("prefill").value = unmet ? "" : k.prefill;
  el("prefill").disabled = el("pdec").disabled = el("pinc").disabled = !state.inputs.fleet.prefill || sizing;
  el("decode").value = unmet ? "" : k.decode;
  el("decode").disabled = el("ddec").disabled = el("dinc").disabled = sizing;
  // A size prices the mix line alone: the per-bucket view is the plan's.
  el("perbucket").disabled = sizing;
  el("perbucket").checked = k.perBucket;
  el("bucketedges").disabled = sizing || !k.perBucket;
  showEdgesBox();
  el("sizelow").checked = k.sizeLow;
  if (k.rate != null) {
    el("rate").value = k.rate;
    el("ratebox").value = Number(k.rate.toFixed(2));
  }
  el("ratiorow").hidden = k.tpotStat === "mean";
}

// fitRateSlider runs the rate slider to 1.2 times the lower knee, or with no
// knee to hand (a size no fleet within the bound serves) to where it ran,
// and always past the rate set.
function fitRateSlider(knee) {
  const rate = state.k.rate;
  const rmax = Math.max(knee != null ? knee * 1.2 : (+el("rate").max || 10), rate * 1.1);
  el("rate").max = rmax.toFixed(2);
  el("rate").step = (rmax / 400).toFixed(3);
  el("rate").value = rate;
  el("ratebox").value = Number(rate.toFixed(2));
}

function render() {
  const p = state.plan;
  syncControls();
  el("banner").textContent = p.banner;
  el("banner").hidden = !p.banner;
  fitRateSlider(lowerKnee(p.ttft.markers));
  el("ratenote").textContent = rateNote(p.limits.chosen.rateSource);
  el("limitsfor").textContent = `(priced at ${shapeOf(p)})`;
  renderSize();
  renderAtTarget();
  renderLimits();
  renderTTFT();
  renderTPOT();
  renderTables();
  renderFlags();
  renderCompare();
}

// rolesText is roles as a sentence's subject: "prefill binds", "prefill and
// decode bind".
function rolesText(roles, verb) {
  const r = roles || [];
  return r.join(" and ") + " " + verb + (r.length > 1 ? "" : "s");
}

// fleetPlain is replica counts in words, and fleetText the same escaped.
function fleetPlain(n) {
  return `${n.prefill} prefill, ${n.decode} decode`;
}

function fleetText(n) {
  return esc(fleetPlain(n));
}

// boundText is a figure's bounds, verbatim, or nothing.
function boundText(f) {
  return f && f.boundBy ? "bound by " + f.boundBy.join(", then ") : "";
}

// lowNote says what the answer's limit is at its worst corner of V_P and f,
// where that falls under the rate sized for: the fleet then meets the rate at
// the learned V_P and f, not at every corner the span sweeps.
function lowNote(limit, z) {
  const s = limit.sensitivity;
  if (!s) return "";
  if (s.loRefused) {
    return `<div class="miss">At one corner of V_P and f this fleet's limit is refused (${esc(s.loRefused)}).` +
      (z.sizeOn === "low" ? "" : " Tick \"size for the worst corner\" to size on it.") + `</div>`;
  }
  if (s.lo == null || s.lo >= z.rate * (1 - 1e-9)) return "";
  return `<div class="miss">At one corner of V_P and f this fleet serves only ${fmtRate(s.lo)}, under the rate. ` +
    `Tick "size for the worst corner" to size on it.</div>`;
}

// renderSize is the size's answer above the plan of the fleet it found: the
// fleet, its limit and headroom over the rate, the role that binds, and why
// one replica fewer of each role is not enough; or, with no fleet within the
// bound, the refusal and the figure the bound reached, and no plan.
function renderSize() {
  const z = state.mode === "size" ? state.size : null;
  el("sizebox").hidden = !z;
  el("planview").hidden = !!(z && !z.answer);
  el("limitstitle").textContent = z && z.answer ? "What that fleet serves under these SLOs" : "The most the fleet serves under these SLOs";
  if (!z) return;
  const rate = fmtRate(z.rate);
  if (z.answer) {
    const a = z.answer, f = a.fleet, h = val(a.headroom), by = boundText(a.limit);
    const low = z.sizeOn === "low";
    el("sizetitle").textContent = `The fleet ${rate} needs`;
    el("sizeanswer").innerHTML = `<div class="v">${fleetText(f)}</div>` +
      `<div>The fewest replicas whose fleet limit reaches ${rate}` + (low ? " at the worst corner of V_P and f" : "") +
      ` (the fewest in all; of two as few, the one with fewer decode replicas). ` +
      `Fleet limit <b>${figText(a.limit, fmtRate)}</b>, <span class="ok">headroom +${h != null ? fmtRate(h) : "refused"}</span> over the rate` +
      (low ? " at the worst corner" : "") + `; ${esc(rolesText(a.binding, "bind"))}${by ? ", " + esc(by) : ""}. ${flagChips(a.limit)}</div>` +
      lowNote(a.limit, z) + heldNote(a);
    el("fewer").innerHTML = fewerLine("prefill", a.fewerPrefill, low) + fewerLine("decode", a.fewerDecode, low);
    return;
  }
  const u = z.unmet, s = u.atBound, lim = val(s.limit), short = val(s.headroom);
  // With no plan to draw, the size says what a plan would: the banner, and
  // nothing under the targets from a fleet no longer on screen.
  el("banner").textContent = z.banner;
  el("banner").hidden = !z.banner;
  el("ttftat").textContent = el("tpotat").textContent = "";
  el("sizetitle").textContent = `No fleet within the bound serves ${rate}`;
  el("sizeanswer").innerHTML = `<div class="v miss">${esc(u.refused.reason)}</div><div>${esc(unmetAdvice(z))}</div>` +
    `<div class="hint">Searched up to ${fleetText(z.maxReplicas)}; the role that falls short: ${esc(u.role)}.` +
    (UNMET_SAID.has(u.refused.reason) ? "" : ` The server says: ${esc(u.refused.detail)}`) + `</div>`;
  const shortText = short != null ? `, ${fmtRate(-short)} short` + (z.sizeOn === "low" ? " at the worst corner of V_P and f" : "") :
    (s.headroom && s.headroom.refused ? `, refused at the worst corner of V_P and f (${esc(s.headroom.refused.reason)})` : "");
  el("fewer").innerHTML = `<li>At the bound (${fleetText(s.fleet)}), the fleet limit: ` +
    (lim == null ? `refused: ${esc(why(s.limit))}` : `<b>${fmtRate(lim)}</b>${shortText}`) +
    (boundText(s.limit) ? ", " + esc(boundText(s.limit)) : "") + ` ${flagChips(s.limit)}</li>`;
}

// fewerLine is one replica fewer of a role: the rate it serves, how far short
// of the rate, and what binds. Sized on the worst corner, the low end is the
// figure that falls short, so it is the one in bold, with the limit beside it
// when the two differ.
function fewerLine(role, s, low) {
  if (!s) return `<li>One ${role} replica fewer: none, the answer has one.</li>`;
  const lim = val(s.limit), short = val(s.headroom), b = boundText(s.limit);
  const binds = `${b ? ", " + esc(b) : ""}; ${esc(rolesText(s.binding, "bind"))}`;
  let what;
  if (lim == null) {
    what = `<span class="miss">refused: ${esc(why(s.limit))}</span>`;
  } else if (short == null) {
    what = `<span class="miss"><b>${fmtRate(lim)}</b>, refused at the worst corner of V_P and f (${esc(why(s.headroom))})</span>${binds}`;
  } else if (low && s.limit.sensitivity && s.limit.sensitivity.lo != null && fmtRate(s.limit.sensitivity.lo) !== fmtRate(lim)) {
    what = `<span class="miss"><b>${fmtRate(s.limit.sensitivity.lo)}</b> at the worst corner of V_P and f (${fmtRate(lim)} at the learned ones), ` +
      `${fmtRate(-short)} short</span>${binds}`;
  } else {
    what = `<span class="miss"><b>${fmtRate(lim)}</b>, ${fmtRate(-short)} short</span>${binds}`;
  }
  return `<li>One ${role} replica fewer (${fleetText(s.fleet)}): ${what}</li>`;
}

// heldNote says, where decode admission holds the prefill limit down, that
// decode replicas are what lift it.
function heldNote(a) {
  const held = (f => f && (f.flags || []).includes("HeldByDecodeAdmission"));
  if (!held(a.limit) && !held(a.plan.limits.prefill)) return "";
  return `<div class="hint">Decode admission holds the prefill limit down here: more decode replicas lift it more than ` +
    `more prefill replicas do, and the answer weighs the two.</div>`;
}

// UNMET_SAID are the refusals unmetAdvice says in the page's own words; the
// server's detail, written for the API and the CLI, is shown for the rest.
const UNMET_SAID = new Set(["NoPrefillCapacity", "NoTPOTHistogram", "TargetBelowFloor", "ReplicaBoundReached"]);

// unmetAdvice is what to change when no fleet within the bound serves the
// rate, in the page's words: the box to raise, the term to type, or the
// target to loosen. A floor no replica count lowers, and a cap the targets
// do not set, are said so.
function unmetAdvice(z) {
  const u = z.unmet, reason = u.refused.reason, low = z.sizeOn === "low";
  const untick = low ? ', or untick "Size for the worst corner of V_P and f"' : "";
  switch (reason) {
  case "NoPrefillCapacity":
    return 'The source learned no prefill rate V_P: type one under "Assume a term the source did not learn".';
  case "NoTPOTHistogram":
    return "The source learned no TPOT percentile ratio R_q: type an assumed ratio under the TPOT target, or pick the mean.";
  case "TargetBelowFloor": {
    const which = u.role === "decode" ? "TPOT" : "TTFT";
    return `The ${which} target is under the floor` + (low ? " at the worst corner of V_P and f" : "") +
      `, the least ${which} any fleet gives. More replicas do not lower a floor: raise the ${which} target above it${untick}.`;
  }
  case "ReplicaBoundReached": {
    const by = (u.atBound.limit.boundBy || []).join(" ");
    const loosen = [];
    if (/TTFT/.test(by)) loosen.push("TTFT");
    if (/TPOT/.test(by)) loosen.push("TPOT");
    const atMax = z.maxReplicas.prefill >= 64 && z.maxReplicas.decode >= 64;
    const parts = [atMax ? "lower the rate (64 replicas a role is the most the page searches)" : 'raise "Search up to" (at most 64)'];
    if (!atMax) parts.push("lower the rate");
    if (loosen.length) parts.push(`loosen (raise) the ${loosen.join(" and ")} target${loosen.length > 1 ? "s" : ""}`);
    if (/operating-point cap/.test(by)) parts.push("raise the headroom (the cap is headroom times the knee; no target moves it)");
    return "No fleet up to the bound serves the rate. " + parts.join(", or ").replace(/^./, c => c.toUpperCase()) + untick + ".";
  }
  }
  return "No fleet up to the bound serves the rate.";
}

// renderAtTarget says, under each target, what the rate set gives.
function renderAtTarget() {
  const p = state.plan, rate = fmtRate(p.limits.chosen.rate);
  const at = (fig, target) => {
    const v = val(fig);
    if (v == null) return `<span class="miss">refused at ${rate}: ${esc(fig && fig.refused ? fig.refused.reason : "not priced")}</span>`;
    return `<span class="${v <= target ? "ok" : "miss"}">${fmtSec(v)} at ${rate}, ${v <= target ? "meets it" : "misses"}.</span>`;
  };
  el("ttftat").innerHTML = at(p.ttft.line.atRate.figure, p.ttft.line.target) + (state.k.ttftDefault ? ' <span class="hint">The policy sets no TTFT target: this one is the page\'s. Set yours.</span>' : "");
  el("tpotat").innerHTML = p.tpot ? at(p.tpot.atRate.figure, p.tpot.target) + (state.k.tpotDefault ? ' <span class="hint">The policy sets no TPOT target: this one is the page\'s. Set yours.</span>' : "") : "";
  const set = [];
  if (state.k.ttftDefault) set.push(`TTFT ${PAGE_TTFT.statistic} ${fmtSec(PAGE_TTFT.seconds)}`);
  if (state.k.tpotDefault) set.push(`TPOT ${PAGE_TPOT.statistic} ${fmtSec(PAGE_TPOT.seconds)}`);
  el("pagetargets").textContent = set.length ? `Targets set by the page, not the policy: ${set.join(", ")}. Set your own above.` : "";
  el("pagetargets").hidden = !set.length;
  const ratio = p.tpot && p.tpot.ratio;
  let note = "";
  if (ratio && ratio.refused) note = "R_q is not learned (" + ratio.refused.reason + "): type a ratio, or pick the mean.";
  else if (ratio && ratio.value != null) note = `R_q ${fmtNum(ratio.value)} (${ratio.source || "?"})`;
  el("rationote").textContent = note;
}

// aboveCap marks a rate the target allows past the operating-point cap: the
// fleet's limit stops at the cap, not there.
function aboveCap(p, rate) {
  const cap = val(p.ttft.markers.operatingCap);
  return rate != null && cap != null && rate > cap ? ` (above the cap ${fmtRate(cap)})` : "";
}

// bindsWithoutHeadroom is the role whose limit the fleet's figure without
// headroom is: the prefill limit, or decode's without the cap (its uncapped
// figure, else its limit). It compares the API's figures and computes none.
function bindsWithoutHeadroom(L) {
  const wh = val(L.fleet.withoutHeadroom);
  if (wh == null) return "";
  const roles = [];
  if (val(L.prefill) === wh) roles.push("prefill");
  if (L.decode && (L.decode.uncapped != null ? L.decode.uncapped : val(L.decode)) === wh) roles.push("decode");
  return roles.join(" and ");
}

// renderLimits is the hero, the binding bar and the +1 steps.
function renderLimits() {
  const p = state.plan, L = p.limits, f = L.fleet;
  const fv = val(f);
  el("fleet").textContent = fv != null ? fmtRate(fv) : "refused";
  const notes = [];
  if (fv == null) notes.push(why(f));
  if (f.capApplied) {
    notes.push(`the operating-point cap binds (headroom ${p.scenario.headroom}); without headroom: ${figPlain(f.withoutHeadroom, fmtRate)}` +
      (bindsWithoutHeadroom(L) ? `, where ${bindsWithoutHeadroom(L)} binds` : ""));
  }
  if (spanPlain(f)) notes.push(spanPlain(f));
  el("fleetn").innerHTML = esc(notes.join("; ")) + (f.flags && f.flags.length ? "<br>" + flagChips(f) : "");
  // The headroom is the API's figure, as it is: the page derives no share of it.
  const h = val(L.chosen.headroom);
  el("chosen").innerHTML = h == null ? `You chose ${fmtRate(L.chosen.rate)}: no headroom, the limit is refused.` :
    `You chose ${fmtRate(L.chosen.rate)}: <span class="${h >= 0 ? "ok" : "miss"}">headroom ${h >= 0 ? "+" : ""}${fmtRate(h)}` +
    `${h < 0 ? ", the SLO is missed" : ""}</span>`;

  const binding = f.binding || [];
  const held = (L.prefill.flags || []).includes("HeldByDecodeAdmission");
  const rows = [
    {name: "Prefill", what: "TTFT", fig: L.prefill, cls: "c-pre", binds: binding.includes("prefill")},
    {name: "Decode", what: "TPOT", fig: L.decode, cls: "c-dec", binds: binding.includes("decode")},
  ].filter(r => r.fig);
  el("limits").innerHTML = rows.map(r => {
    const parts = [`<b>${r.name}</b> (${r.what}) <b>${figText(r.fig, fmtRate)}</b>`];
    if (r.binds) parts.push('<span class="binds">binds</span>');
    if (r.fig.boundBy) parts.push("bound by " + esc(r.fig.boundBy.join(", then ")));
    if (r.fig.refused) parts.push(esc(why(r.fig)));
    if (r.fig.uncapped != null) parts.push(`${r.what} alone would allow ${fmtRate(r.fig.uncapped)}`);
    if (r.fig.prefillOnlyRate != null && val(r.fig) != null && Math.abs(r.fig.prefillOnlyRate - val(r.fig)) > 1e-9) parts.push(`the prefill side alone allows ${fmtRate(r.fig.prefillOnlyRate)}`);
    if (spanText(r.fig)) parts.push(spanText(r.fig));
    return `<li>${parts.join(" · ")} ${flagChips(r.fig)}</li>`;
  }).join("") + (held ? "<li><b>Decode admission holds the prefill limit down: add a decode replica, not a prefill one.</b></li>" : "");
  drawBars(rows, f, L.chosen.rate);

  const step = (name, s) => {
    const lim = val(s.limit), d = val(s.delta);
    if (lim == null) return `<li>+1 ${name}: refused (${esc(s.limit.refused ? s.limit.refused.reason : "")})</li>`;
    const dtext = d == null ? "" : d === 0 ? ` (+0: ${name} does not bind)` : ` (${d > 0 ? "+" : ""}${fmtRate(d)})`;
    const b = (s.binding || []).join(" and ");
    return `<li>+1 ${name} (${s.fleet.prefill} P / ${s.fleet.decode} D) <b>${fmtRate(lim)}</b>${dtext}${b ? ", then " + esc(b) + " binds" : ""}</li>`;
  };
  el("marginal").innerHTML = (state.inputs.fleet.prefill ? step("prefill", p.marginal.prefillPlusOne) : "") + step("decode", p.marginal.decodePlusOne);
}

// drawBars is the binding bar: each role's limit and the fleet's on one
// rate axis, with the rate set.
function drawBars(rows, fleet, rate) {
  const all = rows.concat([{name: "Fleet", what: "", fig: fleet, cls: "c-mix"}]);
  const values = all.map(r => val(r.fig)).filter(v => v != null).concat([rate]);
  if (val(fleet.withoutHeadroom) != null) values.push(val(fleet.withoutHeadroom));
  const xmax = niceMax(Math.max(...values, 0.1) * 1.08);
  const W = 960, L = 70, R = 200, rowH = 26, T = 6, pw = W - L - R;
  const H = T + all.length * rowH + 22;
  const X = v => L + pw * Math.min(v, xmax) / xmax;
  let s = "";
  all.forEach((r, i) => {
    const y = T + i * rowH, v = val(r.fig);
    const capped = r.name === "Fleet" && fleet.capApplied && val(fleet.withoutHeadroom) != null;
    const labelX = v == null ? 0 : (capped ? Math.max(X(v), X(val(fleet.withoutHeadroom))) : X(v)) + 6;
    s += `<text class="lbl" x="${L - 8}" y="${y + 14}" text-anchor="end">${r.name}</text>`;
    s += `<rect class="barbg" x="${L}" y="${y + 5}" width="${pw}" height="10" rx="4"/>`;
    if (v != null) {
      s += `<rect class="${r.cls}" x="${L}" y="${y + 5}" width="${Math.max(2, X(v) - L).toFixed(1)}" height="10" rx="4"/>`;
      s += `<text class="lblp" x="${labelX}" y="${y + 14}">${fmtRate(v)}${capped ? " (without headroom " + fmtRate(val(fleet.withoutHeadroom)) + ")" : ""}</text>`;
    } else {
      s += `<text class="lbl" x="${L + 6}" y="${y + 14}">refused: ${esc(r.fig.refused ? r.fig.refused.reason : "")}</text>`;
    }
    if (capped) {
      const u = X(val(fleet.withoutHeadroom));
      s += `<line class="guide" x1="${u}" x2="${u}" y1="${y + 2}" y2="${y + 18}"/>`;
    }
  });
  const yb = T + all.length * rowH;
  for (const t of ticks(xmax, 6)) s += `<text x="${X(t)}" y="${yb + 14}" text-anchor="middle">${fmtNum(t)}</text>`;
  s += `<text x="${L + pw}" y="${yb + 14}" text-anchor="start" dx="14">r/s</text>`;
  s += `<line class="guide" x1="${X(rate)}" x2="${X(rate)}" y1="${T}" y2="${yb}"/>` +
    `<text class="lbl" x="${X(rate) + 4}" y="${T + 2}" dy="-2">you chose ${fmtRate(rate)}</text>`;
  const svg = el("bars");
  svg.setAttribute("viewBox", `0 -10 ${W} ${H + 10}`);
  svg.innerHTML = s;
  el("barsdesc").textContent = all.map(r => `${r.name} limit ${figPlain(r.fig, fmtRate)}`).join("; ") + `; you chose ${fmtRate(rate)}.`;
}

// ticks are about n round values from 0 to max.
function ticks(max, n) {
  const step = niceStep(max / n);
  const out = [];
  for (let v = 0; v <= max + step * 1e-6; v += step) out.push(Number(v.toPrecision(10)));
  return out;
}

function niceStep(raw) {
  const p = Math.pow(10, Math.floor(Math.log10(raw)));
  const m = raw / p;
  return (m <= 1 ? 1 : m <= 2 ? 2 : m <= 2.5 ? 2.5 : m <= 5 ? 5 : 10) * p;
}

function niceMax(v) {
  const s = niceStep(v / 5);
  return Math.ceil(v / s) * s;
}

// ---- The graphs ----

// pts is a curve as points: x the rate, y the figure or null where refused,
// lo and hi its band or span.
function pts(curve) {
  return curve.map(c => {
    const f = c.figure, y = val(f);
    const b = f.band || f.sensitivity;
    return {x: c.rate, y, lo: b && b.lo != null ? b.lo : null, hi: b && b.hi != null ? b.hi : null, why: y == null ? why(f) : ""};
  });
}

// pinSeries is A's curve as a series, drawn dashed. Where it is the current
// curve (sameCurve) it is drawn under it, unseen, and the legend says so.
function pinSeries(what, curve, current) {
  const p = pts(curve), same = sameCurve(p, pts(current));
  return {cls: "c-pin", pin: true, pts: p, name: same ? `A: same as current (${what}, ${state.pin.what})` : `A: ${what}, ${state.pin.what}`};
}

// pinDot is A's point B, the highest rate that meets A's target, labelled
// with that rate, and "(cap)" where A's operating-point cap holds its limit
// under it. Where it is the current point B, it carries no label of its own.
function pinDot(rate, target, curRate, curTarget) {
  const same = target === curTarget && curRate != null && fmtRate(rate) === fmtRate(curRate);
  return {x: rate, y: target, cls: "c-pin", filled: true, small: true, side: "right",
    label: same ? "" : `A: ${fmtRate(rate)}` + (aboveCap(state.pin.plan, rate) ? " (cap)" : "")};
}

function renderTTFT() {
  const p = state.plan, t = p.ttft, k = state.k;
  const m = t.markers, knee = lowerKnee(m);
  const series = [{cls: "c-mix", name: `${shapeOf(p)} (the limits are read here)`, pts: pts(t.line.curve), band: true}];
  const dots = [];
  const buckets = t.perBucket || [];
  // The lines follow the plan's edges, up to 8: past five the colours
  // repeat, and the bucket table names each line.
  // Lines from the sixth on are dashed, and a line past the context window
  // is dimmed and dotted, with the reason in its legend entry.
  for (const [n, l] of buckets.entries()) {
    const beyond = beyondContext(l), st = bucketSeries(n, beyond);
    series.push({cls: st.cls, extra: st.extra, thin: true, pts: pts(l.curve),
      name: `if all traffic were ${l.bucket}` + (beyond ? ` (${contextNote(contextWindow())})` : "")});
    const b = val(l.atTarget.rate);
    if (b != null) dots.push({x: b, y: l.atTarget.seconds, cls: st.cls, filled: true, small: true});
  }
  // A, the pinned plan, is drawn dashed: its mix line, and its filled point
  // at its own target.
  const a = state.pin && state.pin.plan.ttft.line;
  let pinTarget = null;
  if (a) {
    series.push(pinSeries(`TTFT ${state.pin.plan.ttft.statistic}`, a.curve, t.line.curve));
    const ab = val(a.atTarget.rate);
    if (ab != null) dots.push(pinDot(ab, a.target, val(t.line.atTarget.rate), t.line.target));
    if (a.target !== t.line.target) pinTarget = a.target;
  }
  const targets = [t.line.target].concat(buckets.map(l => l.target), pinTarget != null ? [pinTarget] : []);
  const floor = val(t.line.floor);
  const ymax = Math.max(3 * Math.max(...targets), floor != null ? floor * 1.3 : 0);
  const B = val(t.line.atTarget.rate), A = val(t.line.atRate.figure);
  const belowFloor = t.line.atTarget.rate.refused && t.line.atTarget.rate.refused.reason === "TargetBelowFloor";
  // On a rising curve, below-right of A and above-left of B are off the line.
  const aSide = "right";
  if (A != null) dots.push({x: t.line.atRate.rate, y: A, cls: "c-mix", label: `${fmtSec(A)} at ${fmtRate(t.line.atRate.rate)}`, below: true, side: aSide});
  if (B != null) dots.push({x: B, y: t.line.target, cls: "c-mix", filled: true, label: `${fmtRate(B)} meets it${aboveCap(p, B)}`, side: aSide === "left" ? "right" : "left"});
  const markers = [];
  if (val(m.prefillKnee) != null) markers.push({x: val(m.prefillKnee), label: "prefill knee"});
  if (val(m.decodeKnee) != null) markers.push({x: val(m.decodeKnee), label: "decode knee"});
  if (val(m.operatingCap) != null) markers.push({x: val(m.operatingCap), label: "cap"});
  let onset = null;
  if (m.onsets && m.onsets.prefill != null) onset = m.onsets.prefill;
  if (m.onsets && m.onsets.decode != null) markers.push({x: m.onsets.decode, label: "decode onset"});
  const rate = p.limits.chosen.rate;
  const xmax = Math.max(knee != null ? knee * 1.2 : 10, rate * 1.1, pinXmax());
  el("ttfttitle").textContent = `TTFT ${t.statistic} against the request rate`;
  el("ttftcap").textContent = buckets.length ? BUCKET_NOTE : "One line for the mix; its point B is the prefill limit.";
  chart(el("ttftchart"), {W: 640, H: 340, xmax, ymax, series, target: t.line.target, belowFloor, floor: belowFloor ? floor : null,
    rate, dots, markers, knee, onset, pinTarget, clip: "clip-ttft"});
  el("ttftlegend").innerHTML = legend(series, onset != null);
  el("ttftpoints").innerHTML =
    `<div>Open circle, the TTFT at your rate: <b>${figText(t.line.atRate.figure, fmtSec)}</b> ${flagChips(t.line.atRate.figure)}</div>` +
    `<div>Filled circle, the rate at ${fmtSec(t.line.target)}: <b>${figText(t.line.atTarget.rate, fmtRate)}</b>${esc(aboveCap(p, val(t.line.atTarget.rate)))}` +
    (t.line.atTarget.rate.refused ? " " + esc(why(t.line.atTarget.rate)) : "") + ` ${flagChips(t.line.atTarget.rate)}</div>` +
    `<div>Floor: ${figText(t.line.floor, fmtSec)}</div>`;
}

function renderTPOT() {
  const p = state.plan, t = p.tpot;
  if (!t) {
    el("tpottitle").textContent = "TPOT";
    el("tpotchart").innerHTML = "";
    el("tpotlegend").innerHTML = "";
    el("tpotpoints").textContent = "No TPOT target: decode has no limit of its own.";
    return;
  }
  const knee = val(t.markers.decodeKnee), rate = p.limits.chosen.rate;
  const lk = lowerKnee(p.ttft.markers);
  const xmax = Math.max(lk != null ? lk * 1.2 : 10, rate * 1.1, pinXmax());
  const floor = val(t.floor);
  const series = [{cls: "c-mix", name: `TPOT ${t.statistic}`, pts: pts(t.curve), band: true}];
  const dots = [];
  const a = state.pin && state.pin.plan.tpot;
  let pinTarget = null;
  if (a) {
    series.push(pinSeries(`TPOT ${a.statistic}`, a.curve, t.curve));
    const ab = val(a.atTarget.rate);
    if (ab != null) dots.push(pinDot(ab, a.target, val(t.atTarget.rate), t.target));
    if (a.target !== t.target) pinTarget = a.target;
  }
  const ymax = Math.max(3 * Math.max(t.target, pinTarget || 0), floor != null ? floor * 1.3 : 0);
  const A = val(t.atRate.figure), B = val(t.atTarget.rate);
  const aSide = "right";
  if (A != null) dots.push({x: rate, y: A, cls: "c-mix", label: `${fmtSec(A)} at ${fmtRate(rate)}`, below: true, side: aSide});
  if (B != null) dots.push({x: B, y: t.target, cls: "c-mix", filled: true, label: `${fmtRate(B)} meets it${aboveCap(p, B)}`, side: aSide === "left" ? "right" : "left"});
  const belowFloor = t.atTarget.rate.refused && t.atTarget.rate.refused.reason === "TargetBelowFloor";
  const markers = [];
  if (knee != null) markers.push({x: knee, label: "decode knee"});
  const cap = val(p.ttft.markers.operatingCap);
  if (cap != null) markers.push({x: cap, label: "cap"});
  el("tpottitle").textContent = `TPOT ${t.statistic} against the request rate`;
  chart(el("tpotchart"), {W: 460, H: 340, xmax, ymax, series, target: t.target, belowFloor, floor: belowFloor ? floor : null,
    rate, dots, markers, knee: lk, pinTarget, clip: "clip-tpot"});
  el("tpotlegend").innerHTML = legend(series, false);
  const u = t.atTarget.rate.uncapped;
  el("tpotpoints").innerHTML =
    `<div>Open circle, the TPOT at your rate: <b>${figText(t.atRate.figure, fmtSec)}</b> ${flagChips(t.atRate.figure)}</div>` +
    `<div>Filled circle, the rate at ${fmtSec(t.target)}: <b>${figText(t.atTarget.rate, fmtRate)}</b>${esc(aboveCap(p, val(t.atTarget.rate)))}` +
    (u != null ? ` (TPOT alone would allow ${fmtRate(u)})` : "") +
    (t.atTarget.rate.refused ? " " + esc(why(t.atTarget.rate)) : "") + ` ${flagChips(t.atTarget.rate)}</div>` +
    `<div>Floor: ${figText(t.floor, fmtSec)}</div>`;
}

// legend names every series and the guides, so that no line is told by its
// colour alone.
function legend(series, onsets) {
  const sw = (cls, dash, thin, pin, extra) => `<svg viewBox="0 0 18 10" aria-hidden="true"><line class="${dash ? "guide" : "line " + cls + (thin ? " thin" : "") + (pin ? " pin" : "") + (extra || "")}" x1="1" x2="17" y1="5" y2="5"/></svg>`;
  const box = cls => `<svg viewBox="0 0 18 10" aria-hidden="true"><rect class="${cls}" x="1" y="1" width="16" height="8"/></svg>`;
  return series.map(s => `<span>${sw(s.cls, false, s.thin, s.pin, s.extra)}${esc(s.name)}</span>`).join("") +
    `<span>${sw("", true)}target / rate you set</span><span>${box("band")}span over V_P and f, or band</span>` +
    `<span>${box("sat")}saturated</span>` + (onsets ? `<span>${box("onset")}past the prefill flow-control onset</span>` : "");
}

// chart draws one graph: the curves (gaps where refused), the target and the
// rate as dashed lines, the two points, the knees and the cap, and the
// saturated region shaded.
function chart(svg, o) {
  const {W, H} = o, L = 52, R = 16, T = 26, Bm = 34, pw = W - L - R, ph = H - T - Bm;
  const X = v => L + pw * v / o.xmax, Y = v => T + ph * (1 - v / o.ymax);
  const yms = o.ymax < 2;
  const yfmt = v => yms ? fmtNum(v * 1000) : fmtNum(v);
  let s = `<defs><clipPath id="${o.clip}"><rect x="${L}" y="${T}" width="${pw}" height="${ph}"/></clipPath></defs>`;
  if (o.onset != null && o.onset < o.xmax) s += `<rect class="onset" x="${X(o.onset)}" y="${T}" width="${L + pw - X(o.onset)}" height="${ph}"/>`;
  if (o.knee != null && o.knee < o.xmax) {
    s += `<rect class="sat" x="${X(o.knee)}" y="${T}" width="${L + pw - X(o.knee)}" height="${ph}"/>` +
      `<text x="${L + pw - 4}" y="${T + ph - 8}" text-anchor="end">saturated</text>`;
  }
  for (const v of ticks(o.ymax, 5)) {
    if (v > o.ymax) continue;
    s += `<line class="${v ? "gridl" : "axisl"}" x1="${L}" x2="${L + pw}" y1="${Y(v)}" y2="${Y(v)}"/>` +
      `<text x="${L - 8}" y="${Y(v) + 4}" text-anchor="end">${yfmt(v)}</text>`;
  }
  for (const v of ticks(o.xmax, 6)) {
    if (v > o.xmax) continue;
    s += `<text x="${X(v)}" y="${T + ph + 16}" text-anchor="middle">${fmtNum(v)}</text>`;
  }
  s += `<text x="${L + pw / 2}" y="${H - 4}" text-anchor="middle">request rate, r/s</text>` +
    `<text x="12" y="${T + ph / 2}" transform="rotate(-90 12 ${T + ph / 2})" text-anchor="middle">${yms ? "ms" : "s"}</text>`;
  // The knees and the cap, labelled along the top, staggered when close.
  let lastX = -1e9, row = 0;
  for (const mk of o.markers.slice().sort((a, b) => a.x - b.x)) {
    if (mk.x > o.xmax) continue;
    row = X(mk.x) - lastX < 70 ? (row + 1) % 2 : 0;
    lastX = X(mk.x);
    s += `<line class="marker" x1="${X(mk.x)}" x2="${X(mk.x)}" y1="${T - 4}" y2="${T + ph}"/>` +
      `<text x="${X(mk.x)}" y="${T - 8 - 11 * row}" text-anchor="middle">${esc(mk.label)} ${fmtNum(mk.x)}</text>`;
  }
  let g = "";
  for (const sr of o.series) {
    if (sr.band) {
      let run = [];
      const flush = () => {
        if (run.length > 1) {
          const up = run.map(q => `${X(q.x).toFixed(1)},${Y(q.hi).toFixed(1)}`);
          const down = run.slice().reverse().map(q => `${X(q.x).toFixed(1)},${Y(q.lo).toFixed(1)}`);
          g += `<polygon class="band" points="${up.concat(down).join(" ")}"/>`;
        }
        run = [];
      };
      for (const q of sr.pts) (q.y != null && q.lo != null && q.hi != null) ? run.push(q) : flush();
      flush();
    }
  }
  for (const sr of o.series.slice().reverse()) {
    let d = "", pen = false;
    for (const q of sr.pts) {
      if (q.y == null) { pen = false; continue; }
      d += (pen ? "L" : "M") + X(q.x).toFixed(1) + "," + Y(Math.min(q.y, o.ymax * 2)).toFixed(1);
      pen = true;
    }
    if (d) g += `<path class="line ${sr.cls}${sr.thin ? " thin" : ""}${sr.pin ? " pin" : ""}${sr.extra || ""}" d="${d}"/>`;
  }
  s += `<g clip-path="url(#${o.clip})">${g}</g>`;
  if (o.floor != null && o.floor <= o.ymax) {
    s += `<line class="floorl" x1="${L}" x2="${L + pw}" y1="${Y(o.floor)}" y2="${Y(o.floor)}"/>` +
      `<text class="lbl" x="${L + pw - 4}" y="${Y(o.floor) - 5}" text-anchor="end">floor ${fmtSec(o.floor)}</text>`;
  }
  // A's target, where it differs from the current one: fainter, labelled at
  // the right so that it never sits on the current target's label.
  if (o.pinTarget != null && o.pinTarget <= o.ymax) {
    s += `<line class="guide pin" x1="${L}" x2="${L + pw}" y1="${Y(o.pinTarget)}" y2="${Y(o.pinTarget)}"/>` +
      `<text class="lbl" x="${L + pw - 4}" y="${Y(o.pinTarget) - 6}" text-anchor="end">A's target ${fmtSec(o.pinTarget)}</text>`;
  }
  s += `<line class="guide${o.belowFloor ? " miss" : ""}" x1="${L}" x2="${L + pw}" y1="${Y(o.target)}" y2="${Y(o.target)}"/>` +
    `<text class="lbl" x="${L + 4}" y="${Y(o.target) - 6}">target ${fmtSec(o.target)}${o.belowFloor ? ", under the floor" : ""}</text>`;
  if (o.rate <= o.xmax) {
    s += `<line class="guide" x1="${X(o.rate)}" x2="${X(o.rate)}" y1="${T}" y2="${T + ph}"/>` +
      `<text class="lbl" x="${X(o.rate) - 4}" y="${T + ph - 6}" text-anchor="end">${fmtRate(o.rate)}</text>`;
  }
  for (const dt of o.dots) {
    if (dt.x > o.xmax || dt.y > o.ymax) continue;
    s += `<circle class="${dt.cls} ${dt.filled ? "ptB" : "ptA"}" cx="${X(dt.x)}" cy="${Y(dt.y)}" r="${dt.small ? 4 : 5}"/>`;
    if (dt.label) {
      // Point A's label sits below it, point B's above, each on the side
      // away from the other point, so that the two never overlap.
      const right = dt.side === "right";
      s += `<text class="lblp" x="${X(dt.x) + (right ? 8 : -8)}" y="${Y(dt.y) + (dt.below ? 18 : -9)}" text-anchor="${right ? "start" : "end"}">${esc(dt.label)}</text>`;
    }
  }
  s += `<line class="xh" x1="0" x2="0" y1="${T}" y2="${T + ph}" visibility="hidden"/>`;
  svg.innerHTML = s;
  hover(svg, o, {L, pw, W});
}

// hover is the crosshair and the tooltip: each line's value at the nearest
// rate priced, or why it is refused there.
function hover(svg, o, g) {
  const xh = svg.querySelector(".xh"), tip = el("tip");
  svg.onmousemove = e => {
    const r = svg.getBoundingClientRect(), vx = (e.clientX - r.left) * g.W / r.width;
    if (vx < g.L || vx > g.L + g.pw) { svg.onmouseleave(); return; }
    const rate = (vx - g.L) / g.pw * o.xmax;
    xh.setAttribute("x1", vx); xh.setAttribute("x2", vx); xh.setAttribute("visibility", "visible");
    tip.innerHTML = `<div class="t">${fmtRate(rate)}</div>` + o.series.map(sr => {
      let best = null;
      for (const q of sr.pts) if (!best || Math.abs(q.x - rate) < Math.abs(best.x - rate)) best = q;
      if (!best) return "";
      const v = best.y != null ? fmtSec(best.y) : "refused";
      return `<div class="r"><span>${esc(sr.name)} at ${fmtRate(best.x)}</span><span>${v}</span></div>` +
        (best.y == null ? `<div class="r"><span>${esc(best.why)}</span></div>` : "");
    }).join("");
    tip.hidden = false;
    tip.style.left = Math.min(e.clientX + 14, window.innerWidth - 360) + "px";
    tip.style.top = (e.clientY + 14) + "px";
  };
  svg.onmouseleave = () => { xh.setAttribute("visibility", "hidden"); tip.hidden = true; };
}

// renderTables is the table view: every curve point, and the per-bucket
// lines' points.
function renderTables() {
  const p = state.plan, ttft = pts(p.ttft.line.curve), tpot = p.tpot ? pts(p.tpot.curve) : [];
  const band = q => q.lo == null || q.hi == null || fmtSec(q.lo) === fmtSec(q.hi) ? "" : ` (${fmtSec(q.lo)} to ${fmtSec(q.hi)})`;
  const cell = q => q ? (q.y != null ? fmtSec(q.y) + band(q) : "refused: " + esc(q.why)) : "";
  el("curvetable").innerHTML = "<table><tr><th>rate</th><th>TTFT " + esc(p.ttft.statistic) + "</th>" + (p.tpot ? "<th>TPOT " + esc(p.tpot.statistic) + "</th>" : "") + "</tr>" +
    ttft.map((q, i) => `<tr><td>${fmtRate(q.x)}</td><td>${cell(q)}</td>${p.tpot ? `<td>${cell(tpot[i])}</td>` : ""}</tr>`).join("") + "</table>";
  const b = p.ttft.perBucket || [];
  el("buckettable").innerHTML = !b.length ? "" :
    `<table><caption class="hint">${esc(BUCKET_NOTE)}</caption><tr><th>bucket</th><th>prompt</th><th>lower knee</th><th>TTFT at ${fmtRate(p.limits.chosen.rate)}</th><th>rate at its target</th></tr>` +
    b.map(l => {
      const kn = l.knees ? lowerKnee(l.knees) : null;
      const past = beyondContext(l) ? `<br><span class="warn">${esc(contextNote(contextWindow()))}</span>` : "";
      return `<tr><td>${esc(l.bucket)}${past}</td><td>${l.promptTokens != null ? fmtNum(l.promptTokens) : ""}</td><td>${kn != null ? fmtRate(kn) : "refused"}</td>` +
        `<td>${figText(l.atRate.figure, fmtSec)}</td><td>${figText(l.atTarget.rate, fmtRate)} at ${fmtSec(l.target)}</td></tr>`;
    }).join("") + "</table>";
}

// renderFlags is the flags drawer: every flag in the plan once, in words,
// where it is, and what clears it.
// With a size no fleet within the bound serves, there is no plan: the
// drawer lists the flags of the figure at the bound.
function renderFlags() {
  const z = state.mode === "size" ? state.size : null;
  let flags = (state.plan && state.plan.flags) || [], banner = state.plan && state.plan.banner;
  if (z && z.unmet) {
    const f = z.unmet.atBound.limit;
    const names = (f.flags || []).concat((f.refused && f.refused.flags) || []);
    flags = [...new Set(names)].map(n => ({flag: n, on: ["the fleet at the bound"]}));
    banner = z.banner;
  }
  el("flagcount").textContent = flags.length;
  el("flags").innerHTML = flags.map(f => {
    let w = FLAG_WORDS[f.flag];
    // UnvalidatedTTFT says what the banner says, in the banner's own words:
    // the API takes them from the constant the controller publishes.
    if (f.flag === "UnvalidatedTTFT" && banner) {
      w = [banner + " These figures come from the validation recordings, not from this fleet.", w[1]];
    }
    const places = [...new Set(f.on.map(o => o.replace(/\.(curve|floor|knees.*)$/, "").replace(/^ttft\.perBucket\./, "bucket ")))];
    return `<li><b>${esc(f.flag)}</b>: ${esc(w ? w[0] : "see the what-if proposal for this flag")}` +
      (w ? ` <span class="hint">Clears with: ${esc(w[1])}.</span>` : "") +
      `<br><span class="hint">On: ${esc(places.slice(0, 8).join(", "))}${places.length > 8 ? ", ..." : ""}</span></li>`;
  }).join("");
}

// ---- Saved scenarios ----

// The checks, the store's edits and the names are in scenario.js, which the
// page loads first; here is what touches the page and the browser.

// readStore is the store's raw entries, every one kept (parseStore).
function readStore() {
  try { return parseStore(localStorage.getItem(SCENARIO_STORE)); } catch (e) { return []; }
}

// writeStore writes the raw entries, or says why the browser refused.
function writeStore(raw) {
  try {
    localStorage.setItem(SCENARIO_STORE, JSON.stringify(raw));
    return true;
  } catch (e) {
    setStatus("This browser keeps no saved scenarios here: " + e.message, true);
    return false;
  }
}

// showScenarios lists the saved scenarios that check, the one named
// selected. The options are built as nodes, not markup, so a name is text
// and its value is the name as saved.
function showScenarios(selected) {
  const list = visibleScenarios(readStore());
  const opts = list.map(x => {
    const o = new Option(x.name + (x.saved ? " (" + x.saved + ")" : ""), x.name);
    o.selected = x.name === selected;
    return o;
  });
  el("scenlist").replaceChildren(...(opts.length ? opts : [new Option("none saved", "")]));
  el("scenlist").disabled = el("scenopen").disabled = el("scendelete").disabled = !list.length;
  el("scencount").textContent = list.length ? `${list.length} saved in this browser` : "";
  const ready = !!(state.inputs && state.k);
  el("scensave").disabled = el("scenexport").disabled = !ready;
}

// scenarioName is the name typed, or one made of the scenario: the model,
// the fleet or the question, the rate, the targets and the headroom, the
// last clauses dropped whole to fit (scenarioTitle).
function scenarioName() {
  const typed = cleanName(el("scenname").value);
  if (typed) return typed;
  const k = state.k, what = state.mode === "size" ? "sized" : fleetPlain(k);
  return scenarioTitle([state.inputs.model, what, fmtRate(k.rate || 0), `TTFT ${k.ttftStat} ${fmtSec(k.ttftTarget)}`,
    `TPOT ${k.tpotStat} ${fmtSec(k.tpotTarget)}`, `headroom ${k.headroom}`]);
}

// savedNow is the time a scenario is saved, as the list shows it.
function savedNow() {
  return localInput(new Date()).replace("T", " ");
}

// saveScenario saves the scenario on screen under its name, replacing one
// saved under the same name, and says which it did.
function saveScenario() {
  if (!state.inputs || !state.k) return;
  const name = scenarioName();
  let put;
  try {
    put = putScenario(readStore(), {name, link: linkParams().toString(), saved: savedNow()});
  } catch (e) {
    setStatus("Could not save the scenario: " + e.message, true);
    return;
  }
  if (!writeStore(put.raw)) return;
  showScenarios(name);
  setStatus(put.replaced ? `Replaced the scenario saved as "${name}" with this one.` : `Saved "${name}" in this browser.`);
}

// selectedScenario is the saved scenario the list selects, or null.
function selectedScenario() {
  const name = el("scenlist").value;
  return visibleScenarios(readStore()).find(x => x.name === name) || null;
}

// pageBefore is what opening a scenario changes, read before it does: the
// link's text, the address, the source form and the name box. restorePage
// puts it back when the scenario's load fails.
const SOURCE_BOXES = ["rec", "cycle", "ns", "model", "pvariant", "dvariant", "from", "to"];
function pageBefore() {
  const form = {prom: el("kprom").checked, lasthour: el("lasthour").checked};
  for (const id of SOURCE_BOXES) form[id] = el(id).value;
  return {linked: linked.toString(), hash: location.hash, form, name: el("scenname").value};
}

function restorePage(b) {
  linked = new URLSearchParams(b.linked);
  history.replaceState(null, "", b.hash || location.pathname + location.search);
  for (const id of SOURCE_BOXES) el(id).value = b.form[id];
  el("kprom").checked = b.form.prom;
  el("krec").checked = !b.form.prom;
  el("lasthour").checked = b.form.lasthour;
  el("scenname").value = b.name;
  showKind();
}

// openScenario loads a scenario's source and sets its knobs, as a link does
// when the page opens. A source this server does not read is refused before
// any request (sourceProblem). The knobs belong to the scenario's load
// (beginOpen, linkAtLoadEnd): a load that fails puts the page back as it
// was, and no later Load applies them. A pinned plan stays pinned, to
// compare against it.
function openScenario(sc) {
  const problem = sourceProblem(sc.link, state.sources);
  if (problem) {
    setStatus(`"${sc.name}" was not opened: ${problem}.`, true);
    return;
  }
  const open = beginOpen(pageBefore(), sc);
  linked = new URLSearchParams(open.next.linked);
  const kind = linkSource();
  (kind === "prometheus" ? el("kprom") : el("krec")).checked = true;
  // A link keeps no Prometheus window: it opens on the last hour.
  el("lasthour").checked = true;
  showKind();
  history.replaceState(null, "", open.next.hash);
  el("scenname").value = open.next.name;
  state.link = open.link;
  loadInputs();
}

// exportScenario downloads the scenario on screen as a JSON file.
function exportScenario() {
  if (!state.inputs || !state.k) return;
  const name = scenarioName();
  const doc = {kind: SCENARIO_KIND, version: SCENARIO_VERSION, name, link: linkParams().toString(), saved: new Date().toISOString()};
  const blob = new Blob([JSON.stringify(doc, null, 2) + "\n"], {type: "application/json"});
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = (name.replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "") || "scenario") + ".json";
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  setStatus(`Exported "${name}" as ${a.download}.`);
}

// importScenario reads an exported file, saves it in this browser and opens
// it, as importPlan decides: a file the same as a saved scenario is opened
// without saving; one whose name another scenario has is saved under
// "name (2)" and so on. The store is written once, or not at all. A source
// this server does not read is saved and not opened, and said so.
async function importScenario(file) {
  if (!file) return;
  try {
    if (!state.sources) throw new Error("the page has not listed the server's sources yet: try again in a moment");
    if (file.size > MAX_FILE) throw new Error(`the file is larger than ${MAX_FILE / 1024} KiB`);
    const sc = readScenarioFile(await file.text());
    const imp = importPlan(readStore(), {name: sc.name, link: sc.link, saved: savedNow()});
    if (imp.save && !writeStore(imp.raw)) return;
    showScenarios(imp.name);
    const problem = sourceProblem(sc.link, state.sources);
    if (problem) {
      setStatus(`Saved "${imp.name}", but not opened: ${problem}.`, true);
      return;
    }
    openScenario({name: imp.name, link: sc.link});
  } catch (e) {
    setStatus("Could not import the scenario: " + e.message, true);
  } finally {
    el("scenimport").value = "";
  }
}

// deleteScenario deletes the saved scenario the list selects.
function deleteScenario() {
  const sc = selectedScenario();
  if (!sc) return;
  if (!writeStore(dropScenario(readStore(), sc.name))) return;
  showScenarios();
  setStatus(`Deleted "${sc.name}" from this browser.`);
}

// ---- Compare ----

// A pinned scenario, A, is the plan response on screen when it was pinned,
// kept as it is: it is not priced again as the knobs move. While it is
// pinned the graphs draw its curves dashed and a table sets its limits beside
// the current ones.

// shownPlan is the plan on screen: in size mode the answer's, and none when
// the last price or size failed (the plan left is an older scenario's),
// while a size is in flight, or when no fleet within the bound serves the
// rate.
function shownPlan() {
  if (!state.plan || state.priceFailed) return null;
  if (state.mode === "size" && (state.abort || !(state.size && state.size.answer))) return null;
  return state.plan;
}

// pinWhat is a plan in a few words: the fleet and the rate, and for a size
// the rate it was sized for.
function pinWhat(p) {
  const f = p.scenario.fleet;
  return `${fleetPlain(f)} at ${fmtRate(p.limits.chosen.rate)}` + (state.mode === "size" ? " (sized)" : "");
}

function pinA() {
  const p = shownPlan();
  if (!p) return;
  state.pin = {plan: JSON.parse(JSON.stringify(p)), what: pinWhat(p), source: el("srcsum").textContent,
    at: localInput(new Date()).slice(11)};
  render();
}

function unpin() {
  state.pin = null;
  if (shownPlan()) render();
  else renderCompare();
}

// pinXmax is how far right the graphs run to show all of A: past its lower
// knee and its rate, as the current plan's run.
function pinXmax() {
  if (!state.pin) return 0;
  const p = state.pin.plan, knee = lowerKnee(p.ttft.markers);
  return Math.max(knee != null ? knee * 1.2 : 0, p.limits.chosen.rate * 1.1);
}

// pinControls enables Pin while there is a plan on screen to pin, and Unpin
// while one is pinned.
function pinControls() {
  const cur = shownPlan();
  el("pina").disabled = !cur;
  el("pina").title = cur ? "" : state.mode === "size" ?
    (state.abort ? "Nothing to pin while the fleet is being sized" : "Nothing to pin until a fleet within the bound serves the rate") : "Nothing priced yet";
  el("unpin").disabled = !state.pin;
}

// sloText is a target in words: its statistic and its value.
function sloText(stat, target) {
  return `${stat} ${fmtSec(target)}`;
}

// bindText is what binds a plan's fleet limit: the role and the bound.
function bindText(p) {
  const f = p.limits.fleet, by = boundText(f);
  return (f.binding && f.binding.length ? rolesText(f.binding, "bind") : "no role binds") + (by ? ", " + by : "");
}

// renderCompare is the compare box: A beside the plan on screen, each limit
// with its change (compareRows), and the controls' state. With no plan on
// screen it says why: the current scenario did not price, no fleet within
// the bound serves the rate, or it is being priced.
function renderCompare() {
  const pin = state.pin, cur = shownPlan();
  pinControls();
  el("pinnote").textContent = pin ? `A: ${pin.what}, pinned at ${pin.at}. It is not priced again; change the knobs to compare.` :
    "Pin the scenario on screen as A, then change the knobs: A stays drawn dashed.";
  el("comparebox").hidden = !pin;
  if (!pin) return;
  if (!cur) {
    const z = state.mode === "size" ? state.size : null;
    el("comparewhat").textContent = state.mode === "size" && state.abort ? "The current scenario is being priced." :
      state.priceFailed ? "The current scenario did not price: nothing to compare A with." :
      z && z.unmet ? `No fleet within the bound serves ${fmtRate(z.rate)}: nothing to compare A with.` :
        "The current scenario is being priced.";
    el("comparetable").innerHTML = "";
    return;
  }
  el("comparewhat").textContent = pin.source !== el("srcsum").textContent ? `A was priced from another source: ${pin.source}.` : "";
  el("comparetable").innerHTML = compareRows(pin.plan, cur, {rate: fmtRate, fleet: fleetPlain, fig: f => figPlain(f, fmtRate),
    bind: bindText, slo: sloText, shape: shapeOf});
}

// ---- The controls ----

function wire() {
  const k = () => state.k;
  const ready = () => state.inputs && state.k;
  const intIn = (v, lo, hi) => Math.min(hi, Math.max(lo, Math.round(+v || lo)));
  const step = (role, d) => () => {
    if (!ready()) return;
    k()[role] = intIn(k()[role] + d, 1, 64);
    el(role).value = k()[role];
    schedule(0);
  };
  el("pdec").onclick = step("prefill", -1);
  el("pinc").onclick = step("prefill", 1);
  el("ddec").onclick = step("decode", -1);
  el("dinc").onclick = step("decode", 1);
  for (const role of ["prefill", "decode"]) {
    el(role).onchange = () => { if (ready()) { k()[role] = intIn(el(role).value, 1, 64); el(role).value = k()[role]; schedule(0); } };
  }
  // The rate prices on drag: its curves and limits are cached, so a move
  // prices the two points alone. A size searches fleets: it waits for the
  // release.
  el("rate").oninput = () => {
    if (!ready()) return;
    state.dragging = true;
    k().rate = +el("rate").value;
    el("ratebox").value = Number(k().rate.toFixed(2));
    if (state.mode !== "size") schedule(100);
  };
  // The question: what a fleet serves, or what fleet the rate needs. A size
  // starts from the knobs as they stand, and a plan from the fleet a size
  // found.
  const mode = enter => () => {
    if (!ready()) return;
    enter();
    syncControls();
    renderSize();
    // The plan on screen is the other question's: say at once that the
    // current scenario is being priced, not after the debounce.
    renderCompare();
    schedule(0);
  };
  el("mplan").onchange = mode(enterPlan);
  el("msize").onchange = mode(enterSize);
  el("sizelow").onchange = () => {
    if (!ready()) return;
    k().sizeLow = el("sizelow").checked;
    schedule(0);
  };
  el("maxrep").onchange = () => {
    if (!ready()) return;
    k().maxRep = intIn(el("maxrep").value, 1, 64);
    el("maxrep").value = k().maxRep;
    schedule(0);
  };
  // The release lets the status line speak again, and prices the rate let go.
  el("rate").onchange = () => {
    state.dragging = false;
    if (ready()) schedule(0);
  };
  el("ratebox").onchange = () => {
    const v = +el("ratebox").value;
    if (ready() && v > 0) { k().rate = v; schedule(0); }
  };
  // A target prices on release: it reprices the limits, which takes up to
  // seconds on a TTFT fleet.
  // A target or statistic you set is yours, no longer the page's default.
  const target = (id, scale) => {
    el(id).oninput = () => { el(id + "box").value = Number((+el(id).value).toFixed(scale < 1 ? 1 : 0)); };
    el(id).onchange = () => { if (ready()) { k()[id + "Target"] = +el(id).value / 1000; k()[id + "Default"] = false; schedule(0); } };
    el(id + "box").onchange = () => {
      const v = +el(id + "box").value;
      if (ready() && v > 0) { k()[id + "Target"] = v / 1000; k()[id + "Default"] = false; el(id).value = v; schedule(0); }
    };
  };
  target("ttft", 1);
  target("tpot", 0.1);
  el("ttftstat").onchange = () => { if (ready()) { k().ttftStat = el("ttftstat").value; k().ttftDefault = false; schedule(0); } };
  el("tpotstat").onchange = () => {
    if (!ready()) return;
    k().tpotStat = el("tpotstat").value;
    k().tpotDefault = false;
    el("ratiorow").hidden = k().tpotStat === "mean";
    showTyped();
    schedule(0);
  };
  el("ratio").onchange = () => {
    if (!ready()) return;
    const v = +el("ratio").value;
    k().ratio = el("ratio").value === "" || !(v > 0) ? null : v;
    showTyped();
    schedule(0);
  };
  el("headroom").onchange = () => {
    const v = +el("headroom").value;
    if (ready() && v > 0 && v <= 1) { k().headroom = v; schedule(0); }
  };
  el("priceas").onchange = () => { if (ready()) { k().priceAs = el("priceas").value; schedule(0); } };
  el("outtok").onchange = () => {
    const v = +el("outtok").value;
    if (ready() && v > 0) { k().outputTokens = v; schedule(0); }
  };
  for (const a of ASSUMABLE) {
    el("a-" + a.box).onchange = () => {
      if (!ready()) return;
      const raw = el("a-" + a.box).value, v = +raw;
      if (raw === "" || !(v >= 0)) delete k().assume[a.key];
      else k().assume[a.key] = v / a.scale;
      showTyped();
      schedule(0);
    };
  }
  el("perbucket").onchange =() => { if (ready()) { k().perBucket = el("perbucket").checked; syncControls(); schedule(0); } };
  el("bucketedges").onchange = () => {
    if (!ready()) return;
    let r = null;
    try { r = parseEdges(el("bucketedges").value); } catch (e) { r = null; }
    if (r) {
      k().edges = el("bucketedges").value = r.text;
      schedule(0);
    }
    showEdgesBox();
  };
  // Saved scenarios and compare.
  el("scensave").onclick = saveScenario;
  el("scenopen").onclick = () => { const sc = selectedScenario(); if (sc) openScenario(sc); };
  el("scendelete").onclick = deleteScenario;
  el("scenexport").onclick = exportScenario;
  el("scenimport").onchange = () => importScenario(el("scenimport").files[0]);
  el("pina").onclick = pinA;
  el("rshapeadd").onclick = () => { addShapeRow("", "", ""); const last = el("rshapes").lastElementChild; if (last) last.querySelector("input").focus(); reportStale(); };
  // Editing what the report asks hides an answer to something else.
  el("rshapes").addEventListener("input", reportStale);
  el("rloads").oninput = el("rgrowth").oninput = el("rloadstat").onchange = reportStale;
  el("rrun").onclick = runReport;
  addShapeRow("", "", "");
  el("unpin").onclick = unpin;
  el("kprom").onchange = el("krec").onchange = el("lasthour").onchange = showKind;
  el("srcform").onsubmit = e => { e.preventDefault(); loadInputs(); };
  el("upload").onclick = uploadRecording;
  el("dayrun").onclick = runDay;
  el("btrun").onclick = runBacktest;
  el("bprun").onclick = runPredict;
  el("bcrun").onclick = runCompare;
  const btStale = () => { if (state.bt) el("btout").classList.add("stale"); };
  el("btsplit").oninput = () => { el("btsplitout").textContent = el("btsplit").value + "%"; btStale(); };
  el("btstep").onchange = el("bttest").onchange = el("btallow").onchange = btStale;
  el("btsplitmode").onchange = el("btothermode").onchange = () => {
    const other = el("btothermode").checked;
    document.querySelector('[data-btmode="split"]').hidden = other;
    document.querySelector('[data-btmode="other"]').hidden = !other;
    btStale();
  };
}

// ---- Check the planner (POST /v1/backtest) ----

// runBacktest fits the planner on the first part of the source (or on the
// source, testing another recording) and draws predicted against measured.
async function runBacktest() {
  if (!state.inputs) return;
  const inputsId = state.inputs.inputsId, t0 = performance.now();
  el("btrun").disabled = true;
  el("btout").classList.add("stale");
  setStatus("Checking the planner: fitting, then pricing each step of the test (seconds to a minute)...");
  try {
    const req = {inputsId, step: el("btstep").value};
    if (el("btallow").checked) req.allowConfigMismatch = true;
    if (el("btothermode").checked) {
      const name = el("bttest").value;
      if (!name) throw new Error("pick a test recording");
      const test = await api("POST", "v1/inputs", {model: state.inputs.model, source: {kind: "recording", name},
        decodeVariant: state.inputs.fleet.decode.variant,
        prefillVariant: state.inputs.fleet.prefill ? state.inputs.fleet.prefill.variant : undefined});
      req.testInputsId = test.inputsId;
    } else {
      req.split = el("btsplit").value + "%";
    }
    const out = await api("POST", "v1/backtest", req);
    if (!state.inputs || inputsId !== state.inputs.inputsId) return;
    state.bt = out;
    renderBacktest();
    setStatus(`Checked ${out.steps.length} steps in ${((performance.now() - t0) / 1000).toFixed(1)} s.`);
  } catch (e) {
    setStatus("Could not check the planner: " + e.message, true);
  } finally {
    el("btrun").disabled = !(state.inputs && state.inputs.fleet.prefill);
    el("btout").classList.remove("stale");
  }
}

// btInputsOf resolves a recording of the loaded model and variants.
async function btInputsOf(name) {
  return api("POST", "v1/inputs", {model: state.inputs.model, source: {kind: "recording", name},
    decodeVariant: state.inputs.fleet.decode.variant,
    prefillVariant: state.inputs.fleet.prefill ? state.inputs.fleet.prefill.variant : undefined});
}

// btDownload offers text as a file to save.
function btDownload(link, name, text, type) {
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([text], {type}));
  state.btBlobs = (state.btBlobs || []).concat([a.href]);
  a.download = name;
  a.textContent = name;
  link.append(" ", a);
}

// runPredict predicts the chosen window on the loaded source and offers
// the prediction file, as JSON and as CSV.
async function runPredict() {
  if (!state.inputs) return;
  const name = el("bpwindow").value;
  if (!name) { setStatus("Pick the window to predict.", true); return; }
  el("bprun").disabled = true;
  el("bpout").textContent = "";
  for (const u of state.btBlobs || []) URL.revokeObjectURL(u);
  state.btBlobs = [];
  setStatus("Predicting the window from its load alone (seconds to a minute)...");
  try {
    const win = await btInputsOf(name);
    const req = {inputsId: state.inputs.inputsId, windowInputsId: win.inputsId, step: el("btstep").value};
    if (el("btallow").checked) req.allowConfigMismatch = true;
    const p = await api("POST", "v1/predict", req);
    const base = "prediction-" + p.predictionHash.slice(0, 12);
    const out = el("bpout");
    out.textContent = `Predicted ${p.steps.filter(s => !s.excluded).length} of ${p.steps.length} steps. Save:`;
    btDownload(out, base + ".json", JSON.stringify(p, null, 2) + "\n", "application/json");
    btDownload(out, base + ".csv", btPredictionCSV(p), "text/csv");
    setStatus("Prediction ready: keep the file, and compare it later with the same window measured.");
  } catch (e) {
    setStatus("Could not predict: " + e.message, true);
  } finally {
    el("bprun").disabled = !(state.inputs && state.inputs.fleet.prefill);
  }
}

// runCompare scores a saved prediction against the chosen window measured,
// and draws it as a check.
async function runCompare() {
  const file = el("bcfile").files[0];
  if (!file) { setStatus("Choose the prediction file.", true); return; }
  if (!state.inputs) { setStatus("Load a source first: its model names the window.", true); return; }
  el("bcrun").disabled = true;
  setStatus("Comparing the prediction with the window measured...");
  try {
    let prediction;
    try {
      prediction = JSON.parse(await file.text());
    } catch (e) {
      throw new Error("the file is not JSON: " + e.message);
    }
    const measured = await btInputsOf(el("bcmeasured").value);
    const out = await api("POST", "v1/compare", {prediction, inputsId: measured.inputsId});
    state.bt = out;
    renderBacktest();
    el("btout").classList.remove("stale");
    setStatus(`Compared ${out.steps.length} steps.`);
  } catch (e) {
    setStatus("Could not compare: " + e.message, true);
  } finally {
    el("bcrun").disabled = false;
  }
}

// renderBacktest draws the check: the verdict and what was not judged, the
// two charts on one time axis, the legend, the scores and the notes.
function renderBacktest() {
  const bt = state.bt;
  el("btout").hidden = false;
  const lines = btSummaryText(bt);
  el("btsum").innerHTML = lines.map((t, i) => `<li>${i === 0 ? "<b>" + esc(t) + "</b>" : esc(t)}</li>`).join("");
  const g = btSVG(bt), svg = el("btchart");
  svg.setAttribute("viewBox", g.viewBox);
  svg.innerHTML = g.markup;
  el("btlegend").innerHTML = btLegend();
  el("btdesc").textContent = lines.join(" ");
  el("btscores").innerHTML = btScoreTable(bt);
  el("btsteps").innerHTML = btStepTable(bt);
  el("btnotes").innerHTML = bt.notes.map(n => `<li>${esc(n)}</li>`).join("") + `<li>${esc(bt.banner)}</li>`;
  const xh = svg.querySelector(".xh"), tip = el("tip");
  svg.onmousemove = e => {
    const r = svg.getBoundingClientRect(), vx = (e.clientX - r.left) * g.W / r.width;
    const s = vx < g.L || vx > g.L + g.pw ? null : btStepAt(bt, g.x0 + (vx - g.L) / g.pw * (g.x1 - g.x0));
    if (!s) { svg.onmouseleave(); return; }
    xh.setAttribute("x1", vx); xh.setAttribute("x2", vx); xh.setAttribute("visibility", "visible");
    tip.innerHTML = btTipHTML(bt, s);
    tip.hidden = false;
    tip.style.left = Math.min(e.clientX + 14, window.innerWidth - 360) + "px";
    tip.style.top = (e.clientY + 14) + "px";
  };
  svg.onmouseleave = () => { xh.setAttribute("visibility", "hidden"); tip.hidden = true; };
}

// ---- The day plan ----

// dayRequest is the day plan the knobs ask: the plan's SLO, headroom,
// assumptions and stated lengths, and no fleet or rate, which are each
// bucket's own.
function dayRequest() {
  const req = planRequest();
  delete req.fleet;
  delete req.views;
  delete req.bucketEdges;
  if (req.traffic) {
    delete req.traffic.classes;
    if (!Object.keys(req.traffic).length) delete req.traffic;
  }
  req.step = el("daystep").value;
  if (el("daysize").value) req.sizeFor = el("daysize").value;
  return req;
}

// runDay asks the day plan and draws it.
async function runDay() {
  if (!state.inputs || !state.k) return;
  const inputsId = state.inputs.inputsId, t0 = performance.now();
  el("dayrun").disabled = true;
  el("dayout").classList.add("stale");
  setStatus("Planning the day: each bucket sized on its own (seconds)...");
  try {
    const out = await api("POST", "v1/dayplan", dayRequest());
    if (!state.inputs || inputsId !== state.inputs.inputsId) return;
    state.day = out;
    renderDay();
    setStatus(`Planned ${out.buckets.length} buckets in ${((performance.now() - t0) / 1000).toFixed(1)} s; ${out.searched} fleets priced.`);
  } catch (e) {
    setStatus("Could not plan the day: " + e.message, true);
  } finally {
    el("dayrun").disabled = !(state.inputs && state.inputs.fleet.prefill);
    el("dayout").classList.remove("stale");
  }
}

// renderDay draws the day plan: the summary, the three charts on one time
// axis, the legend, the table view and the notes.
function renderDay() {
  const d = state.day;
  el("dayout").hidden = false;
  el("daysum").innerHTML = daySummaryText(d).map(t => `<li>${esc(t)}</li>`).join("");
  const g = dayPlanSVG(d), svg = el("daychart");
  svg.setAttribute("viewBox", g.viewBox);
  svg.innerHTML = g.markup;
  el("daylegend").innerHTML = dayLegend();
  el("dayrefused").innerHTML = dayRefusedHTML(d);
  el("dayrefused").hidden = !(d.refused && d.refused.length);
  el("daydesc").textContent = daySummaryText(d).join(" ");
  el("daytable").innerHTML = dayHoursTable(d);
  el("daybuckets").innerHTML = dayBucketsTable(d);
  el("daynotes").innerHTML = d.notes.map(n => `<li>${esc(n)}</li>`).join("") + `<li>${esc(d.banner)}</li>`;
  dayHover(svg, g);
}

// dayHover is the crosshair across the three charts and the tooltip of the
// bucket under it.
function dayHover(svg, g) {
  const xh = svg.querySelector(".xh"), tip = el("tip");
  svg.onmousemove = e => {
    const r = svg.getBoundingClientRect(), vx = (e.clientX - r.left) * g.W / r.width;
    if (vx < g.L || vx > g.L + g.pw) { svg.onmouseleave(); return; }
    const b = dayBucketAt(state.day, g.x0 + (vx - g.L) / g.pw * (g.x1 - g.x0));
    if (!b) { svg.onmouseleave(); return; }
    xh.setAttribute("x1", vx); xh.setAttribute("x2", vx); xh.setAttribute("visibility", "visible");
    tip.innerHTML = dayTipHTML(state.day, b);
    tip.hidden = false;
    tip.style.left = Math.min(e.clientX + 14, window.innerWidth - 360) + "px";
    tip.style.top = (e.clientY + 14) + "px";
  };
  svg.onmouseleave = () => { xh.setAttribute("visibility", "hidden"); tip.hidden = true; };
}

// ---- The fleet report (POST /v1/report) ----
//
// Every string the card shows is built in scenario.js (reportView) and put
// here into text nodes, never into markup.

// renumberShapes labels each shape row by its place, for a screen reader.
function renumberShapes() {
  Array.from(el("rshapes").children).forEach((row, i) => {
    for (const box of row.querySelectorAll("input")) {
      const part = box.dataset.part;
      box.setAttribute("aria-label", `shape ${i + 1} ${part === "hit" ? "prefix hit rate" : part.toUpperCase() + " in tokens"}`);
    }
    row.querySelector("button").setAttribute("aria-label", `Remove shape ${i + 1}`);
  });
}

// addShapeRow adds a request-shape row: IL, OL and hit boxes and a remove
// button. A blank row is the measured shape.
function addShapeRow(il, ol, hit) {
  const rows = el("rshapes");
  if (rows.children.length >= MAX_REPORT_SHAPES) {
    el("rstatus").textContent = `At most ${MAX_REPORT_SHAPES} shapes.`;
    return;
  }
  const row = document.createElement("div");
  row.className = "rshape";
  for (const [name, value, hint] of [["il", il, "IL"], ["ol", ol, "OL"], ["hit", hit, "hit"]]) {
    const box = document.createElement("input");
    box.type = "text";
    box.value = value;
    box.placeholder = hint;
    box.dataset.part = name;
    row.appendChild(box);
  }
  const drop = document.createElement("button");
  drop.type = "button";
  drop.textContent = "Remove";
  drop.onclick = () => {
    // Focus moves to the next row's IL box, or to "Add a shape".
    const next = row.nextElementSibling || row.previousElementSibling;
    row.remove();
    renumberShapes();
    reportStale();
    (next ? next.querySelector("input") : el("rshapeadd")).focus();
  };
  row.appendChild(drop);
  rows.appendChild(row);
  renumberShapes();
}

// shapeRows is the shape rows as reportRequest reads them.
function shapeRows() {
  return Array.from(el("rshapes").children).map(row => {
    const part = name => row.querySelector(`[data-part="${name}"]`).value;
    return {il: part("il"), ol: part("ol"), hit: part("hit")};
  });
}

// reportStale says a drawn report no longer matches the knobs: it is hidden
// until the fleet is judged again.
function reportStale() {
  // Any answer still in flight is for the question before this change.
  state.reportGen = (state.reportGen || 0) + 1;
  if (el("rresult").hidden && !state.reportBusy) return;
  el("rresult").hidden = true;
  el("rstatus").textContent = "The knobs changed: judge the fleet again.";
}

// reportToken names the question on screen: the inputs and every change
// since (reportStale). An answer to an older token is dropped.
function reportToken() {
  return (state.inputs ? state.inputs.inputsId : "") + "#" + (state.reportGen || 0);
}

// judgedText says what a report found, for the live region.
function judgedText(r) {
  const counts = {};
  for (const s of r.shapes || []) {
    const v = (s.verdict || {}).verdict || "NOT JUDGED";
    counts[v] = (counts[v] || 0) + 1;
  }
  const n = (r.shapes || []).length;
  return `Judged ${n} shape${n === 1 ? "" : "s"}: ` + Object.entries(counts).map(([v, c]) => `${c} ${v.toLowerCase()}`).join(", ") + ".";
}

// runReport asks the server to judge the fleet the knobs set, at the
// shapes and loads asked, with the plan's headroom, SLO and assumed terms.
// The button stays focusable while it runs (aria-disabled), so focus is not
// lost; on an answer it moves to the verdicts.
async function runReport() {
  if (state.reportBusy) return;
  if (!state.inputs || !state.k) {
    el("rstatus").textContent = "Load a source first.";
    return;
  }
  const plan = planRequest();
  const asked = reportRequest({inputsId: state.inputs.inputsId, fleet: plan.fleet, shapes: shapeRows(),
    loads: el("rloads").value, growth: el("rgrowth").value, loadStatistic: el("rloadstat").value, headroom: plan.headroom, slo: plan.slo, assume: plan.assume});
  if (asked.problem) {
    el("rstatus").textContent = asked.problem;
    return;
  }
  const token = reportToken();
  state.reportBusy = true;
  el("rrun").setAttribute("aria-disabled", "true");
  el("rstatus").textContent = "Judging the fleet... (many shapes and loads take up to half a minute)";
  try {
    const r = await api("POST", "v1/report", asked.body);
    // An answer to a question since changed is dropped.
    if (!reportCurrent(token, reportToken())) return;
    renderReport(r);
    el("rstatus").textContent = judgedText(r);
    el("rverdicts").focus();
  } catch (e) {
    if (!reportCurrent(token, reportToken())) return;
    const status = Number(String(e.message).split(":")[0]);
    el("rresult").hidden = true;
    el("rstatus").textContent = reportMissing(status, e.message)
      ? "This server has no fleet report (POST /v1/report): it runs a release from before capacity-planner at-replicas. The rest of the page works."
      : "The report failed: " + e.message;
  } finally {
    state.reportBusy = false;
    el("rrun").removeAttribute("aria-disabled");
    if (!reportCurrent(token, reportToken())) el("rstatus").textContent = "The knobs changed: judge the fleet again.";
  }
}

// textNode is an element holding text, with a class.
function textNode(tag, text, cls) {
  const c = document.createElement(tag);
  c.textContent = text;
  if (cls) c.className = cls;
  return c;
}

// shapeLabel is a shape's name, marked EXTRAPOLATED where it is.
function shapeLabel(row) {
  const span = textNode("span", row.label);
  if (row.extrapolated) {
    const x = textNode("span", " EXTRAPOLATED", "rextra");
    if (row.why) x.title = row.why;
    span.appendChild(x);
  }
  return span;
}

// flagsNode is a shape's flags: the common ones as phrases, the rest behind
// a toggle, and every code in the tooltip.
function flagsNode(f) {
  const p = textNode("p", f.phrases.length ? "Rests on: " + f.phrases.join("; ") + "." : "");
  p.title = f.codes;
  if (f.more) {
    const more = document.createElement("details");
    more.className = "rmore";
    more.appendChild(textNode("summary", `${f.more} more assumption${f.more === 1 ? "" : "s"}`));
    more.appendChild(textNode("p", f.rest.join(", ")));
    p.appendChild(more);
  }
  return p;
}

// renderReport draws a report: each shape's verdict in words first, the
// shape-by-load matrix, each shape's detail (closed), and the accuracy notes
// behind a toggle.
function renderReport(r) {
  const v = reportView(r);
  const verdicts = el("rverdicts");
  verdicts.replaceChildren();
  el("rload").textContent = v.load;
  for (const s of v.verdicts) {
    const li = document.createElement("li");
    li.appendChild(shapeLabel(s));
    if (s.whatIf) li.appendChild(textNode("span", " if all traffic were this shape", "rwhatif"));
    if (s.decodeOnly) li.appendChild(textNode("span", "decode only", "rmark unknown rscope"));
    li.appendChild(textNode("b", " " + s.verdict + ": "));
    if (s.needs) li.appendChild(textNode("span", s.needs, s.unmet ? "rneeds unmet" : "rneeds"));
    li.appendChild(textNode("span", " " + s.rest, "rverdict"));
    // Growth sits under the verdict it grows from, never in its place.
    for (const g of s.growth) {
      const p = textNode("div", "", "rgrowthline");
      p.appendChild(textNode("span", g.lead + ": "));
      p.appendChild(textNode("b", g.verdict + ": "));
      if (g.needs) p.appendChild(textNode("span", g.needs, g.unmet ? "rneeds unmet" : "rneeds"));
      p.appendChild(textNode("span", " " + g.rest));
      li.appendChild(p);
    }
    verdicts.appendChild(li);
  }
  el("rlegendstats").textContent = v.legendStats;
  const table = el("rmatrix");
  table.replaceChildren();
  const head = document.createElement("tr");
  head.appendChild(textNode("th", "shape \\ load"));
  for (const h of v.header) head.appendChild(textNode("th", h));
  table.appendChild(head);
  for (const row of v.rows) {
    const tr = document.createElement("tr");
    const th = document.createElement("th");
    th.appendChild(shapeLabel(row));
    tr.appendChild(th);
    for (const c of row.cells) {
      // Each figure on its own unbroken line part, so a narrow page wraps between them.
      const td = document.createElement("td");
      td.className = c.cls;
      c.parts.forEach((p, i) => {
        if (i) td.appendChild(document.createTextNode(i === 2 ? " " : " · "));
        td.appendChild(textNode("span", p, "rpart"));
      });
      if (c.word) td.appendChild(textNode("span", c.word, "rmark " + c.cls));
      tr.appendChild(td);
    }
    table.appendChild(tr);
  }
  const detail = el("rdetail");
  detail.replaceChildren();
  for (const d of v.details) {
    const box = document.createElement("details");
    box.appendChild(textNode("summary", d.summary));
    for (const line of d.lines) box.appendChild(textNode("p", line));
    box.appendChild(flagsNode(d.flags));
    detail.appendChild(box);
  }
  const notes = el("rnotes");
  notes.replaceChildren();
  for (const n of v.notes) notes.appendChild(textNode("li", n));
  el("rresult").hidden = false;
}

document.addEventListener("DOMContentLoaded", start);
