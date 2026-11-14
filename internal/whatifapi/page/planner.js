// The capacity planner's page (docs/proposals/capacity-planner.md, "The
// screen"). It holds no model: every number it shows is one the API returned,
// and a refused figure is shown as its reason, never as a number. Plain
// JavaScript, no library, no request to any origin but the one that served it.
"use strict";

// FLAG_WORDS is every flag the engine raises, in plain words, and what clears
// it. A flag missing here is shown by its name (TestPageNamesEveryFlag).
const FLAG_WORDS = {
  UnvalidatedTTFT: ["The TTFT model is not validated: it reads up to about 33% low at light load. Not a bound.", "a fresh frozen staircase run that validates the model"],
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
  BelowFittedBand: ["The fill is below the band the ITL line was fitted over: TPOT is read at the band's bottom, a safe floor.", "readings at lower fill"],
  KVBoundSlots: ["Decode slots are set by KV capacity, an approximation.", "nothing: a property of the fleet"],
  BorrowedLine: ["The ITL line was borrowed from another variant with the same engine configuration.", "this variant's own readings"],
  HeldLine: ["The ITL line was held from an earlier fit: the window had too few readings.", "a window with more steady readings"],
  TierUnstable: ["Resamples fit the ITL line at another tier and move the answer by more than 5%.", "more steady readings"],
  LineNearGate: ["The ITL line sits near the tracker's gate; the answer does not care which side (informational).", "nothing"],
  ExtrapolatedFill: ["The fill is above the highest fill the ITL line was read at: extrapolated.", "readings at higher load"],
  AssumedShape: ["Priced at a prompt or output length you set, not the measured one.", "price as the current mix"],
  PullUnseen: ["The prefill load is past the pull table's highest bucket: the pull reads low.", "readings at higher prefill load"],
  OverheadMeasured: ["The fixed overhead comes from the prefill pods' histograms.", "nothing (informational)"],
  AssumedPoissonArrivals: ["Arrivals are taken as Poisson: on bursty traffic the TTFT over-promises.", "a stated arrival burstiness (Ca²)"],
  AssumedArrivalCV2: ["The arrival burstiness Ca² is a value you set.", "nothing: a stated assumption"],
  AssumedA: ["a, the fixed latency, is the value you typed, not a learned one.", "a low-load window that fits a"],
  PromptSpreadAbovePeak: ["The prompt cv is past the peak for this percentile: a larger cv can price a shorter floor and a higher rate.", "a measured prompt spread"],
  PromptSpreadClamped: ["The measured prompt spread was clamped at its stop.", "a prompt histogram with wider bounds"],
  RoutingBound: ["One prefill pod takes most of the work: more pods would mostly sit idle behind the router.", "fix the routing, not the replica count"],
  ImbalanceNotSustained: ["The imbalance f was high but has not held over the window: priced at it, the cautious side.", "a longer window"],
  FlowControlMismatch: ["The declared flow-control setting disagrees with the router's series: priced as declared.", "match the policy to the router"],
  LineMismatch: ["The recording's readings do not support the ITL line: the figure is printed, flagged.", "a window with readings across the load"],
};

// BUCKET_NOTE is what the per-bucket lines are, every time they are drawn.
const BUCKET_NOTE = "If all traffic were bucket b: each line is its own fleet's answer, every prompt at the bucket's upper edge. Not the mix; no limit is read from it.";

// TERM_NAMES are the inputs' names as the page shows them.
const TERM_NAMES = {
  vp: "V_P", a: "a", imbalance: "f", promptTokens: "prompt", promptCV: "prompt cv",
  transferSecondsPerToken: "KV transfer", routerHop: "router hop", itlA: "ITL a", itlB: "ITL b",
  itlLine: "ITL line", capacityTokens: "KV capacity", kvPerRequest: "KV per request", batchCap: "batch cap",
  tpotRatio: "R_q", prefill: "prefill",
};

const TTFT_STATS = ["mean", "p50", "p90", "p95", "p99"];
const TPOT_STATS = ["mean", "p50", "p90", "p95", "p99"];

// state is the page's: the sources, the inputs loaded, the knobs, and the
// last plan drawn.
const state = {sources: null, inputs: null, plan: null, k: null, inFlight: false, dirty: false, seq: 0};

const el = id => document.getElementById(id);

// esc is s safe to put in HTML.
function esc(s) {
  return String(s).replace(/[&<>"']/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"}[c]));
}

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

// figText is a rate figure as text: the number, or "refused (reason)".
function figText(f, fmt) {
  const v = val(f);
  return v != null ? fmt(v) : "refused (" + (f && f.refused ? f.refused.reason : "not priced") + ")";
}

// spanText is a figure's span over V_P and f, for a rate.
function spanText(f) {
  const s = f && f.sensitivity;
  if (!s) return "";
  const lo = s.lo != null ? fmtRate(s.lo) : "refused (" + s.loRefused + ")";
  const hi = s.hi != null ? fmtRate(s.hi) : "refused (" + s.hiRefused + ")";
  return lo + " to " + hi + " over V_P and f";
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

function setStatus(text, error) {
  el("status").textContent = text;
  el("status").classList.toggle("error", !!error);
}

// api calls the API on this origin, retrying a busy server's 503.
async function api(method, path, body) {
  for (let attempt = 0; ; attempt++) {
    const opt = {method, headers: {}};
    if (body !== undefined) {
      opt.headers["Content-Type"] = "application/json";
      opt.body = JSON.stringify(body);
    }
    const resp = await fetch(path, opt);
    let doc = null;
    try { doc = await resp.json(); } catch (e) { doc = null; }
    if (resp.status === 503 && attempt < 5) {
      await new Promise(r => setTimeout(r, 1000 * (+resp.headers.get("Retry-After") || 1)));
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
    state.sources = await api("GET", "/v1/sources");
  } catch (e) {
    setStatus("Could not list the sources: " + e.message, true);
    return;
  }
  const s = state.sources;
  el("ns").innerHTML = s.namespaces.map(n => `<option${n === s.namespace ? " selected" : ""}>${esc(n)}</option>`).join("");
  el("rec").innerHTML = s.recordings.map(n => `<option>${esc(n)}</option>`).join("");
  el("kprom").disabled = !s.prometheus;
  el("krec").disabled = s.recordings.length === 0;
  // The page opens on a Prometheus window, the last hour ending now; a
  // server with recordings alone opens on its first.
  let kind = s.prometheus ? "prometheus" : s.recordings.length ? "recording" : "";
  if (linked.get("recording") && s.recordings.includes(linked.get("recording"))) {
    kind = "recording";
    el("rec").value = linked.get("recording");
    el("cycle").value = +linked.get("cycle") || 0;
  } else if (linked.get("namespace") && s.namespaces.includes(linked.get("namespace"))) {
    el("ns").value = linked.get("namespace");
  }
  if (linked.get("model")) el("model").value = linked.get("model");
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

// sourceRequest is the source the form names.
function sourceRequest() {
  if (kindNow() === "recording") {
    return {kind: "recording", name: el("rec").value, cycle: Math.max(0, Math.floor(+el("cycle").value || 0))};
  }
  const src = {kind: "prometheus", namespace: el("ns").value};
  if (!el("lasthour").checked) {
    src.from = new Date(el("from").value).toISOString().replace(/\.\d{3}Z$/, "Z");
    src.to = new Date(el("to").value).toISOString().replace(/\.\d{3}Z$/, "Z");
  }
  return src;
}

async function loadInputs() {
  const req = {source: sourceRequest()};
  if (el("model").value.trim()) req.model = el("model").value.trim();
  setStatus(req.source.kind === "prometheus" ? "Reading the Prometheus window (seconds to a minute)..." : "Replaying the recording...");
  el("load").disabled = true;
  try {
    const inputs = await api("POST", "/v1/inputs", req);
    setInputs(inputs);
    if (!state.linkApplied) {
      state.linkApplied = true;
      applyLink();
    }
    el("source").open = false;
    plan();
  } catch (e) {
    setStatus("Could not load the inputs: " + e.message, true);
    el("source").open = true;
  } finally {
    el("load").disabled = false;
  }
}

// setInputs starts a session from the inputs: the recorded replicas, the
// policy's SLO and headroom, the observed mix. The rate starts at the
// operating point the first plan returns: the source carries no rate.
function setInputs(inputs) {
  state.inputs = inputs;
  state.plan = null;
  const p = inputs.policy || {};
  const ttft = p.ttft || {statistic: "p90", seconds: 1};
  const tpot = p.tpot || {statistic: "mean", seconds: 0.05};
  state.k = {
    prefill: inputs.fleet.prefill ? Math.max(1, inputs.fleet.prefill.replicas) : 0,
    decode: Math.max(1, inputs.fleet.decode.replicas),
    rate: null,
    ttftStat: ttft.statistic, ttftTarget: ttft.seconds, ttftFromPolicy: !!p.ttft,
    tpotStat: tpot.statistic, tpotTarget: tpot.seconds, tpotFromPolicy: !!p.tpot,
    ratio: null, headroom: p.headroom || 0.85,
    priceAs: "mix", outputTokens: val(inputs.observed.outputTokens),
    perBucket: false,
    assume: {},
  };
  const k = state.k;
  showLearned(inputs);
  fillStats("ttftstat", TTFT_STATS, k.ttftStat);
  fillStats("tpotstat", TPOT_STATS, k.tpotStat);
  slider("ttft", k.ttftTarget * 1000, 1);
  slider("tpot", k.tpotTarget * 1000, 0.1);
  const prompt = val(inputs.observed.promptTokens);
  el("priceas").innerHTML = `<option value="mix">the current mix${prompt != null ? " (mean prompt " + fmtNum(prompt) + " tokens)" : ""}</option>` +
    inputs.observed.buckets.map(b => `<option value="${esc(b.name)}">if all traffic were ${esc(b.name)} (every prompt ${fmtNum(b.upperTokens)} tokens)</option>`).join("");
  el("outnote").textContent = k.outputTokens != null ? "observed " + fmtNum(k.outputTokens) : "";
  el("outtok").value = k.outputTokens != null ? Math.round(k.outputTokens) : "";
  el("headroom").value = k.headroom;
  el("perbucket").checked = false;
  el("pnow").textContent = inputs.fleet.prefill ? "now " + inputs.fleet.prefill.replicas : "no prefill variant";
  el("dnow").textContent = "now " + inputs.fleet.decode.replicas;
  el("ratio").value = "";
  showSource(inputs);
  showInputs(inputs);
  syncControls();
}

// ASSUMABLE are the terms a scenario may type: the box, the API's key, the
// inputs' term, and the box's unit per the term's (ms for seconds).
const ASSUMABLE = [
  {box: "vp", key: "vp", term: "vp", scale: 1},
  {box: "a", key: "a", term: "a", scale: 1000},
  {box: "cv", key: "promptCV", term: "promptCV", scale: 1},
  {box: "hop", key: "routerHop", term: "routerHop", scale: 1000},
];

// showLearned puts each assumable term's learned value, or its refusal,
// beside its box, and opens the box when the source refused one of them.
function showLearned(inputs) {
  let refused = false;
  for (const a of ASSUMABLE) {
    el("a-" + a.box).value = "";
    const t = inputs.terms.find(x => x.name === a.term);
    let text = "not read";
    if (t && t.refused) {
      text = "refused: " + t.refused.reason;
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

// showInputs is the provenance chips: every term with its source, or its
// refusal.
function showInputs(inputs) {
  let refused = 0, assumed = 0;
  el("terms").innerHTML = inputs.terms.map(t => {
    const name = TERM_NAMES[t.name] || t.name;
    let cls = "learned", text, title = t.source || "";
    if (t.refused) {
      cls = "refused"; refused++;
      text = `${esc(name)} <b>refused</b> ${esc(t.refused.reason)}`;
      title = t.refused.detail;
    } else {
      const isAssumed = (t.flags && t.flags.length) || /assum|default/i.test(t.source || "");
      if (isAssumed) { cls = "assumed"; assumed++; }
      text = `${esc(name)} <b>${esc(fmtNum(t.value))}</b>${t.unit ? " " + esc(t.unit) : ""}` + (t.source ? ` · ${esc(t.source)}` : "");
      if (t.flags && t.flags.length) title += " | " + t.flags.join(", ");
    }
    return `<span class="chip ${cls}" title="${esc(title)}">${text}</span>`;
  }).join("");
  el("assumed").innerHTML = (inputs.assumed || []).map(a => `<li>assumed: ${esc(a)}</li>`).join("");
  const s = inputs.source;
  el("inputsum").textContent = `cycle ${s.cycle} of ${s.totalCycles}, ${inputs.terms.length} terms, ${refused} refused, ${assumed} assumed or flagged`;
}

// ---- The link ----

// The URL's fragment carries the source and the knobs, so a link reopens the
// same scenario: #recording=run.jsonl.gz&prefill=2&rate=6&ttft=0.8 and so on.
// Seconds are seconds, as the API takes them.
const linked = new URLSearchParams(location.hash.slice(1));

// applyLink sets the knobs the link names, once, on the first inputs loaded.
function applyLink() {
  const k = state.k, g = n => linked.get(n), num = n => (g(n) !== null && g(n) !== "" && isFinite(+g(n)) ? +g(n) : null);
  for (const [n, key] of [["prefill", "prefill"], ["decode", "decode"], ["rate", "rate"], ["ttft", "ttftTarget"], ["tpot", "tpotTarget"],
    ["ratio", "ratio"], ["headroom", "headroom"], ["out", "outputTokens"]]) {
    if (num(n) != null) k[key] = num(n);
  }
  if (g("ttftStat")) k.ttftStat = g("ttftStat");
  if (g("tpotStat")) k.tpotStat = g("tpotStat");
  if (g("priceAs")) k.priceAs = g("priceAs");
  k.perBucket = g("perBucket") === "1";
  for (const a of ASSUMABLE) if (num(a.key) != null) k.assume[a.key] = num(a.key);
  fillStats("ttftstat", TTFT_STATS, k.ttftStat);
  fillStats("tpotstat", TPOT_STATS, k.tpotStat);
  slider("ttft", k.ttftTarget * 1000, 1);
  slider("tpot", k.tpotTarget * 1000, 0.1);
  el("priceas").value = k.priceAs;
  if (k.outputTokens != null) el("outtok").value = Math.round(k.outputTokens);
  el("headroom").value = k.headroom;
  el("perbucket").checked = k.perBucket;
  el("ratio").value = k.ratio != null ? k.ratio : "";
  for (const a of ASSUMABLE) if (k.assume[a.key] != null) el("a-" + a.box).value = Number((k.assume[a.key] * a.scale).toPrecision(6));
  const n = Object.keys(k.assume).length;
  el("assumecount").textContent = n ? n + " typed" : "none typed";
  syncControls();
}

// writeLink puts the source and the knobs in the URL's fragment.
function writeLink() {
  const k = state.k, s = state.inputs.source, p = new URLSearchParams();
  if (s.kind === "recording") {
    p.set("recording", s.name);
    if (s.cycle) p.set("cycle", s.cycle);
  } else {
    p.set("namespace", state.inputs.namespace);
  }
  if (el("model").value.trim()) p.set("model", el("model").value.trim());
  if (state.inputs.fleet.prefill) p.set("prefill", k.prefill);
  p.set("decode", k.decode);
  if (k.rate != null) p.set("rate", Number(k.rate.toFixed(3)));
  p.set("ttftStat", k.ttftStat);
  p.set("ttft", k.ttftTarget);
  p.set("tpotStat", k.tpotStat);
  p.set("tpot", k.tpotTarget);
  if (k.ratio != null) p.set("ratio", k.ratio);
  p.set("headroom", k.headroom);
  if (k.priceAs !== "mix") p.set("priceAs", k.priceAs);
  if (k.outputTokens != null) p.set("out", Math.round(k.outputTokens));
  for (const a of ASSUMABLE) if (k.assume[a.key] != null) p.set(a.key, k.assume[a.key]);
  if (k.perBucket) p.set("perBucket", "1");
  history.replaceState(null, "", "#" + p.toString());
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
  return req;
}

// plan prices the knobs. One request is in flight at a time; a knob moved
// meanwhile is priced when it returns, so the last move always lands.
let planTimer = null;
function schedule(ms) {
  clearTimeout(planTimer);
  planTimer = setTimeout(plan, ms);
}

async function plan() {
  if (!state.inputs) return;
  if (state.inFlight) {
    state.dirty = true;
    return;
  }
  state.inFlight = true;
  state.dirty = false;
  const t0 = performance.now();
  const slow = setTimeout(() => setStatus("Pricing (a new fleet or view can take seconds)..."), 300);
  try {
    const out = await api("POST", "/v1/plan", planRequest());
    clearTimeout(slow);
    state.plan = out;
    if (state.k.rate == null) state.k.rate = out.limits.chosen.rate;
    render();
    writeLink();
    setStatus(`Priced in ${Math.round(performance.now() - t0)} ms.`);
  } catch (e) {
    clearTimeout(slow);
    setStatus("Could not price the plan: " + e.message, true);
  } finally {
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
function syncControls() {
  const k = state.k;
  el("prefill").value = k.prefill;
  el("prefill").disabled = el("pdec").disabled = el("pinc").disabled = !state.inputs.fleet.prefill;
  el("decode").value = k.decode;
  if (k.rate != null) {
    el("rate").value = k.rate;
    el("ratebox").value = Number(k.rate.toFixed(2));
  }
  el("ratiorow").hidden = k.tpotStat === "mean";
}

function render() {
  const p = state.plan, k = state.k;
  syncControls();
  el("banner").textContent = p.banner;
  el("banner").hidden = !p.banner;
  const knee = lowerKnee(p.ttft.markers);
  const rmax = Math.max(knee != null ? knee * 1.2 : 10, k.rate * 1.1);
  el("rate").max = rmax.toFixed(2);
  el("rate").step = (rmax / 400).toFixed(3);
  el("rate").value = k.rate;
  el("ratenote").textContent = p.limits.chosen.rateSource === "operating point" ? "started at the operating point: the source carries no request rate" : "";
  el("limitsfor").textContent = k.priceAs === "mix" ? "(priced as the current mix)" : `(priced as if all traffic were ${k.priceAs}: not the mix)`;
  renderAtTarget();
  renderLimits();
  renderTTFT();
  renderTPOT();
  renderTables();
  renderFlags();
}

// renderAtTarget says, under each target, what the rate set gives.
function renderAtTarget() {
  const p = state.plan, rate = fmtRate(p.limits.chosen.rate);
  const at = (fig, target) => {
    const v = val(fig);
    if (v == null) return `<span class="miss">refused at ${rate}: ${esc(fig && fig.refused ? fig.refused.reason : "not priced")}</span>`;
    return `<span class="${v <= target ? "ok" : "miss"}">${fmtSec(v)} at ${rate}${v <= target ? "" : ", misses"}</span>`;
  };
  el("ttftat").innerHTML = at(p.ttft.line.atRate.figure, p.ttft.line.target) + (state.k.ttftFromPolicy ? "" : ' <span class="hint">the policy sets no TTFT target: set yours</span>');
  el("tpotat").innerHTML = p.tpot ? at(p.tpot.atRate.figure, p.tpot.target) + (state.k.tpotFromPolicy ? "" : ' <span class="hint">the policy sets no TPOT target: set yours</span>') : "";
  const ratio = p.tpot && p.tpot.ratio;
  let note = "";
  if (ratio && ratio.refused) note = "R_q is not learned (" + ratio.refused.reason + "): type a ratio, or pick the mean.";
  else if (ratio && ratio.value != null) note = `R_q ${fmtNum(ratio.value)} (${ratio.source || "?"})`;
  el("rationote").textContent = note;
}

// renderLimits is the hero, the binding bar and the +1 steps.
function renderLimits() {
  const p = state.plan, L = p.limits, f = L.fleet;
  const fv = val(f);
  el("fleet").textContent = fv != null ? fmtRate(fv) : "refused";
  const notes = [];
  if (fv == null) notes.push(why(f));
  if (f.capApplied) notes.push(`the operating-point cap binds (headroom ${state.k.headroom}); without headroom: ${figText(f.withoutHeadroom, fmtRate)}`);
  if (spanText(f)) notes.push(spanText(f));
  el("fleetn").innerHTML = esc(notes.join("; ")) + (f.flags && f.flags.length ? "<br>" + flagChips(f) : "");
  const h = val(L.chosen.headroom);
  el("chosen").innerHTML = h == null ? `You chose ${fmtRate(L.chosen.rate)}: no headroom, the limit is refused.` :
    `You chose ${fmtRate(L.chosen.rate)}: <span class="${h >= 0 ? "ok" : "miss"}">headroom ${h >= 0 ? "+" : ""}${fmtRate(h)}` +
    (fv ? ` (${Math.round(100 * h / fv)}%)` : "") + `${h < 0 ? ", the SLO is missed" : ""}</span>`;

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
  el("barsdesc").textContent = all.map(r => `${r.name} limit ${figText(r.fig, fmtRate)}`).join("; ") + `; you chose ${fmtRate(rate)}.`;
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

function renderTTFT() {
  const p = state.plan, t = p.ttft, k = state.k;
  const m = t.markers, knee = lowerKnee(m);
  const series = [{cls: "c-mix", name: k.priceAs === "mix" ? "current mix (the limits are read here)" : `priced as all ${k.priceAs} (the limits are read here)`, pts: pts(t.line.curve), band: true}];
  const dots = [];
  const buckets = t.perBucket || [];
  const names = state.inputs.observed.buckets.map(b => b.name);
  for (const l of buckets) {
    const i = names.indexOf(l.bucket);
    series.push({cls: "c-" + i, thin: true, name: `if all traffic were ${l.bucket}`, pts: pts(l.curve)});
    const b = val(l.atTarget.rate);
    if (b != null) dots.push({x: b, y: l.atTarget.seconds, cls: "c-" + i, filled: true, small: true});
  }
  const targets = [t.line.target].concat(buckets.map(l => l.target));
  const floor = val(t.line.floor);
  const ymax = Math.max(3 * Math.max(...targets), floor != null ? floor * 1.3 : 0);
  const B = val(t.line.atTarget.rate), A = val(t.line.atRate.figure);
  const belowFloor = t.line.atTarget.rate.refused && t.line.atTarget.rate.refused.reason === "TargetBelowFloor";
  const aSide = B == null || B >= t.line.atRate.rate ? "left" : "right";
  if (A != null) dots.push({x: t.line.atRate.rate, y: A, cls: "c-mix", label: `${fmtSec(A)} at ${fmtRate(t.line.atRate.rate)}`, below: true, side: aSide});
  if (B != null) dots.push({x: B, y: t.line.target, cls: "c-mix", filled: true, label: `${fmtRate(B)} meets it`, side: aSide === "left" ? "right" : "left"});
  const markers = [];
  if (val(m.prefillKnee) != null) markers.push({x: val(m.prefillKnee), label: "prefill knee"});
  if (val(m.decodeKnee) != null) markers.push({x: val(m.decodeKnee), label: "decode knee"});
  if (val(m.operatingCap) != null) markers.push({x: val(m.operatingCap), label: "cap"});
  let onset = null;
  if (m.onsets && m.onsets.prefill != null) onset = m.onsets.prefill;
  if (m.onsets && m.onsets.decode != null) markers.push({x: m.onsets.decode, label: "decode onset"});
  const rate = p.limits.chosen.rate;
  const xmax = Math.max(knee != null ? knee * 1.2 : 10, rate * 1.1);
  el("ttfttitle").textContent = `TTFT ${t.statistic} against the request rate`;
  el("ttftcap").textContent = buckets.length ? BUCKET_NOTE : "One line for the mix; its point B is the prefill limit.";
  chart(el("ttftchart"), {W: 640, H: 340, xmax, ymax, series, target: t.line.target, belowFloor, floor: belowFloor ? floor : null,
    rate, dots, markers, knee, onset, clip: "clip-ttft"});
  el("ttftlegend").innerHTML = legend(series, onset != null);
  el("ttftpoints").innerHTML =
    `<div>Open circle, the TTFT at your rate: <b>${figText(t.line.atRate.figure, fmtSec)}</b> ${flagChips(t.line.atRate.figure)}</div>` +
    `<div>Filled circle, the rate at ${fmtSec(t.line.target)}: <b>${figText(t.line.atTarget.rate, fmtRate)}</b>` +
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
  const xmax = Math.max(lk != null ? lk * 1.2 : 10, rate * 1.1);
  const floor = val(t.floor);
  const ymax = Math.max(3 * t.target, floor != null ? floor * 1.3 : 0);
  const series = [{cls: "c-mix", name: `TPOT ${t.statistic}`, pts: pts(t.curve), band: true}];
  const dots = [];
  const A = val(t.atRate.figure), B = val(t.atTarget.rate);
  const aSide = B == null || B >= rate ? "left" : "right";
  if (A != null) dots.push({x: rate, y: A, cls: "c-mix", label: `${fmtSec(A)} at ${fmtRate(rate)}`, below: true, side: aSide});
  if (B != null) dots.push({x: B, y: t.target, cls: "c-mix", filled: true, label: `${fmtRate(B)} meets it`, side: aSide === "left" ? "right" : "left"});
  const belowFloor = t.atTarget.rate.refused && t.atTarget.rate.refused.reason === "TargetBelowFloor";
  const markers = [];
  if (knee != null) markers.push({x: knee, label: "decode knee"});
  const cap = val(p.ttft.markers.operatingCap);
  if (cap != null) markers.push({x: cap, label: "cap"});
  el("tpottitle").textContent = `TPOT ${t.statistic} against the request rate`;
  chart(el("tpotchart"), {W: 460, H: 340, xmax, ymax, series, target: t.target, belowFloor, floor: belowFloor ? floor : null,
    rate, dots, markers, knee: lk, clip: "clip-tpot"});
  el("tpotlegend").innerHTML = legend(series, false);
  const u = t.atTarget.rate.uncapped;
  el("tpotpoints").innerHTML =
    `<div>Open circle, the TPOT at your rate: <b>${figText(t.atRate.figure, fmtSec)}</b> ${flagChips(t.atRate.figure)}</div>` +
    `<div>Filled circle, the rate at ${fmtSec(t.target)}: <b>${figText(t.atTarget.rate, fmtRate)}</b>` +
    (u != null ? ` (TPOT alone would allow ${fmtRate(u)})` : "") +
    (t.atTarget.rate.refused ? " " + esc(why(t.atTarget.rate)) : "") + ` ${flagChips(t.atTarget.rate)}</div>` +
    `<div>Floor: ${figText(t.floor, fmtSec)}</div>`;
}

// legend names every series and the guides, so that no line is told by its
// colour alone.
function legend(series, onsets) {
  const sw = (cls, dash, thin) => `<svg viewBox="0 0 18 10" aria-hidden="true"><line class="${dash ? "guide" : "line " + cls + (thin ? " thin" : "")}" x1="1" x2="17" y1="5" y2="5"/></svg>`;
  const box = cls => `<svg viewBox="0 0 18 10" aria-hidden="true"><rect class="${cls}" x="1" y="1" width="16" height="8"/></svg>`;
  return series.map(s => `<span>${sw(s.cls, false, s.thin)}${esc(s.name)}</span>`).join("") +
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
    if (d) g += `<path class="line ${sr.cls}${sr.thin ? " thin" : ""}" d="${d}"/>`;
  }
  s += `<g clip-path="url(#${o.clip})">${g}</g>`;
  if (o.floor != null && o.floor <= o.ymax) {
    s += `<line class="floorl" x1="${L}" x2="${L + pw}" y1="${Y(o.floor)}" y2="${Y(o.floor)}"/>` +
      `<text class="lbl" x="${L + pw - 4}" y="${Y(o.floor) - 5}" text-anchor="end">floor ${fmtSec(o.floor)}</text>`;
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
  const cell = q => q ? (q.y != null ? fmtSec(q.y) + (q.lo != null && q.hi != null ? ` (${fmtSec(q.lo)} to ${fmtSec(q.hi)})` : "") : "refused: " + esc(q.why)) : "";
  el("curvetable").innerHTML = "<table><tr><th>rate</th><th>TTFT " + esc(p.ttft.statistic) + "</th>" + (p.tpot ? "<th>TPOT " + esc(p.tpot.statistic) + "</th>" : "") + "</tr>" +
    ttft.map((q, i) => `<tr><td>${fmtRate(q.x)}</td><td>${cell(q)}</td>${p.tpot ? `<td>${cell(tpot[i])}</td>` : ""}</tr>`).join("") + "</table>";
  const b = p.ttft.perBucket || [];
  el("buckettable").innerHTML = !b.length ? "" :
    `<table><caption class="hint">${esc(BUCKET_NOTE)}</caption><tr><th>bucket</th><th>prompt</th><th>lower knee</th><th>TTFT at ${fmtRate(p.limits.chosen.rate)}</th><th>rate at its target</th></tr>` +
    b.map(l => {
      const kn = l.knees ? lowerKnee(l.knees) : null;
      return `<tr><td>${esc(l.bucket)}</td><td>${l.promptTokens != null ? fmtNum(l.promptTokens) : ""}</td><td>${kn != null ? fmtRate(kn) : "refused"}</td>` +
        `<td>${figText(l.atRate.figure, fmtSec)}</td><td>${figText(l.atTarget.rate, fmtRate)} at ${fmtSec(l.target)}</td></tr>`;
    }).join("") + "</table>";
}

// renderFlags is the flags drawer: every flag in the plan once, in words,
// where it is, and what clears it.
function renderFlags() {
  const flags = state.plan.flags || [];
  el("flagcount").textContent = flags.length;
  el("flags").innerHTML = flags.map(f => {
    const w = FLAG_WORDS[f.flag];
    const places = [...new Set(f.on.map(o => o.replace(/\.(curve|floor|knees.*)$/, "").replace(/^ttft\.perBucket\./, "bucket ")))];
    return `<li><b>${esc(f.flag)}</b>: ${esc(w ? w[0] : "see the what-if proposal for this flag")}` +
      (w ? ` <span class="hint">Clears with: ${esc(w[1])}.</span>` : "") +
      `<br><span class="hint">On: ${esc(places.slice(0, 8).join(", "))}${places.length > 8 ? ", ..." : ""}</span></li>`;
  }).join("");
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
  // prices the two points alone.
  el("rate").oninput = () => {
    if (!ready()) return;
    k().rate = +el("rate").value;
    el("ratebox").value = Number(k().rate.toFixed(2));
    schedule(100);
  };
  el("ratebox").onchange = () => {
    const v = +el("ratebox").value;
    if (ready() && v > 0) { k().rate = v; schedule(0); }
  };
  // A target prices on release: it reprices the limits, which takes up to
  // seconds on a TTFT fleet.
  const target = (id, key, scale) => {
    el(id).oninput = () => { el(id + "box").value = Number((+el(id).value).toFixed(scale < 1 ? 1 : 0)); };
    el(id).onchange = () => { if (ready()) { k()[key] = +el(id).value / 1000; schedule(0); } };
    el(id + "box").onchange = () => {
      const v = +el(id + "box").value;
      if (ready() && v > 0) { k()[key] = v / 1000; el(id).value = v; schedule(0); }
    };
  };
  target("ttft", "ttftTarget", 1);
  target("tpot", "tpotTarget", 0.1);
  el("ttftstat").onchange = () => { if (ready()) { k().ttftStat = el("ttftstat").value; schedule(0); } };
  el("tpotstat").onchange = () => {
    if (!ready()) return;
    k().tpotStat = el("tpotstat").value;
    el("ratiorow").hidden = k().tpotStat === "mean";
    schedule(0);
  };
  el("ratio").onchange = () => {
    if (!ready()) return;
    const v = +el("ratio").value;
    k().ratio = el("ratio").value === "" || !(v > 0) ? null : v;
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
      const n = Object.keys(k().assume).length;
      el("assumecount").textContent = n ? n + " typed" : "none typed";
      schedule(0);
    };
  }
  el("perbucket").onchange =() => { if (ready()) { k().perBucket = el("perbucket").checked; schedule(0); } };
  el("kprom").onchange = el("krec").onchange = el("lasthour").onchange = showKind;
  el("srcform").onsubmit = e => { e.preventDefault(); loadInputs(); };
}

document.addEventListener("DOMContentLoaded", start);
