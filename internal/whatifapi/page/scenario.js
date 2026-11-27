// The capacity planner page's saved scenarios, as pure functions: escaping,
// the checks on a scenario read back from a file or the store, the store's
// edits, and the compare table's changes. No DOM and no browser API, so
// hack/check-planner-scenarios.sh runs them under node on fixtures. The page
// loads this file before planner.js.
//
// A scenario is the page's link, the text after the # that planner.js's
// linkParams writes: the source and the knobs. A saved scenario is that text
// under a name in the browser's localStorage; an exported one is a small JSON
// file holding the same text. A file or a store read back is untrusted.
"use strict";

// esc is s safe to put in HTML.
function esc(s) {
  return String(s).replace(/[&<>"']/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"}[c]));
}

const SCENARIO_KIND = "capacity-planner-scenario";
const SCENARIO_VERSION = 1;
const SCENARIO_STORE = "capacity-planner.scenarios";
const MAX_SCENARIOS = 50, MAX_NAME = 80, MAX_LINK = 4096, MAX_FILE = 65536;

// LINK_NUMBERS, LINK_NAMES and LINK_WORDS are every key linkParams writes:
// the numbers (the assumed terms' keys among them, ASSUMABLE in planner.js),
// the free names (a recording, a namespace, a model, a variant, a bucket),
// and the keys with a fixed set of values or a form of their own (edges).
const LINK_NUMBERS = new Set(["cycle", "prefill", "decode", "rate", "ttft", "tpot", "ratio", "headroom", "out", "max",
  "vp", "a", "promptCV", "routerHop", "itlB"]);
const LINK_NAMES = new Set(["recording", "namespace", "model", "prefillVariant", "decodeVariant", "priceAs"]);
const STAT_WORD = /^(mean|p[0-9]{1,2}(\.[0-9]{1,3})?)$/;
const LINK_WORDS = {mode: /^size$/, low: /^1$/, perBucket: /^1$/, loadAwareRouter: /^1$/, ttftStat: STAT_WORD, tpotStat: STAT_WORD,
  edges: {test: edgesInLink}};

// MAX_EDGES and MAX_EDGE are the server's limits on bucketEdges
// (internal/whatifapi/scenario.go): at most 8 edges, each at most 1024k
// tokens.
const MAX_EDGES = 8, MAX_EDGE = 1 << 20;

// bucketName is a bucket's name as the server gives it (bucketName in
// internal/whatifapi/scenario.go): its edge in k where it is a whole number
// of 1024 tokens, else in tokens. So 2048 is "2k" and 2000 is "2000".
function bucketName(tokens) {
  return tokens % 1024 === 0 ? `${tokens / 1024}k` : String(tokens);
}

// parseEdges reads bucket edges as the page's box takes them: token counts
// or Nk (N x 1024 tokens), split by commas or spaces, ascending, by the
// server's rules. It returns {edges, text}, text the canonical form the link
// carries: each edge by its bucket's name, so 2048 is "2k" and the box, the
// link and the line's label agree ("2k,16k,64k"); blank is no edges, the
// default. Anything else throws, saying which edge and why.
function parseEdges(s) {
  const words = String(s).split(/[\s,]+/).filter(w => w !== "").map(w => w.toLowerCase());
  if (words.length > MAX_EDGES) throw new Error(`${words.length} edges; give at most ${MAX_EDGES}`);
  const edges = [];
  for (const w of words) {
    if (!/^[0-9]{1,8}k?$/.test(w)) throw new Error(`${w} is not a token count such as 2048 or 16k`);
    const e = parseInt(w, 10) * (w.endsWith("k") ? 1024 : 1);
    if (e < 1 || e > MAX_EDGE) throw new Error(`${w} is outside 1 to ${MAX_EDGE / 1024}k tokens`);
    if (edges.length && e <= edges[edges.length - 1]) throw new Error(`${w} is not above the edge before it: give the edges smallest first, each once`);
    edges.push(e);
  }
  return {edges, text: edges.map(bucketName).join(",")};
}

// linkedEdges is what a link's edges= gives the page: {edges, note}. Edges
// that parse are used in their canonical form (the page then rewrites the
// link), however they were typed; edges that do not parse are not used, and
// note says so, why, and that the default buckets are priced in their place.
// The page never prices other edges than the link names in silence.
function linkedEdges(v) {
  if (!v) return {edges: "", note: null};
  try {
    return {edges: parseEdges(v).text, note: null};
  } catch (e) {
    return {edges: "", note: `The link's bucket edges "${v}" were not used (${e.message}): the default buckets are priced.`};
  }
}

// edgesInLink reports whether v is edges a link may carry: one or more that
// parseEdges reads, in any form it reads.
function edgesInLink(v) {
  try {
    return parseEdges(v).edges.length > 0;
  } catch (e) {
    return false;
  }
}

// edgesBoxProblem is what the bucket edges box says under itself: "" while
// its text reads as the edges in use (inUse, parseEdges' text, "" for the
// default), else why the lines on screen are not what it says. It stays
// until the box and the chart agree, whatever the status line says since.
function edgesBoxProblem(raw, inUse) {
  const shown = inUse ? `the lines show ${inUse.replace(/,/g, ", ")}` : "the lines show the default buckets";
  let r;
  try {
    r = parseEdges(raw);
  } catch (e) {
    return `Not used: ${e.message}; ${shown}.`;
  }
  return r.text === inUse ? "" : `Not used yet; ${shown}.`;
}

// EDGE_BEYOND is the flag a bucket line past the context window carries.
const EDGE_BEYOND = "EdgeBeyondContext";

// beyondContext reports whether a bucket line (a plan's ttft.perBucket[i])
// is flagged past the engines' context window, on either point.
function beyondContext(l) {
  const has = f => !!f && ((f.flags || []).includes(EDGE_BEYOND) || (!!f.refused && (f.refused.flags || []).includes(EDGE_BEYOND)));
  return has(l.atRate && l.atRate.figure) || has(l.atTarget && l.atTarget.rate);
}

// contextNote is what a bucket line past the window says of itself, with
// window the inputs' maxModelLen in tokens, or null where not known.
function contextNote(window) {
  return window ? `past the ${bucketName(window)}-token context window` : "past the context window";
}

// bucketSeries is the classes the n-th bucket line (from 0) is drawn with:
// one of five colours, a dash pattern from the sixth on so a repeated colour
// stays told apart, and dimmed and dotted past the context window.
function bucketSeries(n, beyond) {
  return {cls: "c-" + (n % 5), extra: (n >= 5 ? " rep" : "") + (beyond ? " beyond" : "")};
}

// edgesText is a plan's buckets as the compare table names them.
function edgesText(p) {
  const e = p && p.scenario && p.scenario.bucketEdges;
  return e && e.length ? e.map(bucketName).join(", ") : "1k, 4k, 8k, 32k, 128k (default)";
}

// CONTROL is a control character: in a name, the HTML parser would change
// it, and the name could never be opened or deleted again.
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/;

// linkProblem is what is wrong with a link read from a file or the store, or
// "" when it is one linkParams could have written.
function linkProblem(link) {
  if (typeof link !== "string") return "the link is not text";
  if (link.length > MAX_LINK) return `the link is longer than ${MAX_LINK} characters`;
  const p = new URLSearchParams(link), seen = new Set();
  for (const [key, v] of p) {
    if (seen.has(key)) return `the link names ${key} twice`;
    seen.add(key);
    if (LINK_NUMBERS.has(key)) {
      if (v === "" || !isFinite(+v) || +v < 0) return `${key} is not a number: ${v}`;
    } else if (LINK_NAMES.has(key)) {
      if (!v || v.length > 253 || CONTROL.test(v)) return `${key} is not a name`;
    } else if (Object.hasOwn(LINK_WORDS, key)) {
      if (!LINK_WORDS[key].test(v)) return `${key} cannot be ${v}`;
    } else {
      return `the page sets no ${key}`;
    }
  }
  if (!p.get("recording") && !p.get("namespace")) return "the link names no source";
  return "";
}

// nameProblem is what is wrong with a scenario's name, or "".
function nameProblem(name) {
  if (typeof name !== "string" || !name.trim() || name.length > MAX_NAME) return `a scenario's name is text of 1 to ${MAX_NAME} characters`;
  if (CONTROL.test(name)) return "a scenario's name has a control character";
  return "";
}

// checkScenario is a scenario read back, {name, link, saved}, or an error
// saying why it is not one.
function checkScenario(x) {
  if (!x || typeof x !== "object" || Array.isArray(x)) throw new Error("not a scenario");
  const bad = nameProblem(x.name) || linkProblem(x.link);
  if (bad) throw new Error(bad);
  return {name: x.name.trim(), link: x.link, saved: typeof x.saved === "string" && !CONTROL.test(x.saved) ? x.saved.slice(0, 40) : ""};
}

// checked is checkScenario's answer, or null for an entry that is not one.
function checked(x) {
  try { return checkScenario(x); } catch (e) { return null; }
}

// readScenarioFile is an exported scenario's JSON, checked.
function readScenarioFile(text) {
  if (typeof text !== "string" || text.length > MAX_FILE) throw new Error(`the file is larger than ${MAX_FILE / 1024} KiB`);
  let doc;
  try { doc = JSON.parse(text); } catch (e) { throw new Error("the file is not JSON"); }
  if (!doc || doc.kind !== SCENARIO_KIND) throw new Error(`the file is not a ${SCENARIO_KIND}`);
  if (doc.version !== SCENARIO_VERSION) throw new Error(`the file is version ${String(doc.version).slice(0, 20)}; this page reads version ${SCENARIO_VERSION}`);
  return checkScenario(doc);
}

// parseStore is the store's raw entries, every one kept: an entry this page
// cannot read may be another version's, and is written back as it was.
function parseStore(text) {
  let raw;
  try { raw = JSON.parse(text || "[]"); } catch (e) { return []; }
  return Array.isArray(raw) ? raw : [];
}

// visibleScenarios is the entries that check, the first MAX_SCENARIOS of
// them: the list the page shows.
function visibleScenarios(raw) {
  return raw.map(checked).filter(x => x).slice(0, MAX_SCENARIOS);
}

// putScenario is raw with entry saved under its name: a scenario of that
// name that checks is replaced, every other entry kept. A new name past
// MAX_SCENARIOS is an error.
function putScenario(raw, entry) {
  const sc = checkScenario(entry);
  const same = x => { const c = checked(x); return c !== null && c.name === sc.name; };
  const replaced = raw.some(same);
  if (!replaced && visibleScenarios(raw).length >= MAX_SCENARIOS) throw new Error(`this page keeps at most ${MAX_SCENARIOS} scenarios: delete one first`);
  return {raw: raw.filter(x => !same(x)).concat([{name: sc.name, link: sc.link, saved: sc.saved}]), replaced};
}

// dropScenario is raw without the scenario named; entries that do not check
// are kept.
function dropScenario(raw, name) {
  return raw.filter(x => { const c = checked(x); return c === null || c.name !== name; });
}

// freeName is name, or where a scenario in taken has it, the first of
// "name (2)", "name (3)" ... that none has, cut to MAX_NAME characters.
function freeName(name, taken) {
  const used = new Set(taken);
  if (!used.has(name)) return name;
  for (let n = 2; ; n++) {
    const suffix = ` (${n})`, candidate = name.slice(0, MAX_NAME - suffix.length) + suffix;
    if (!used.has(candidate)) return candidate;
  }
}

// cleanName is a typed or generated name made fit to save: control
// characters as spaces, trimmed, at most MAX_NAME characters.
function cleanName(s) {
  return String(s).replace(new RegExp(CONTROL.source, "g"), " ").trim().slice(0, MAX_NAME).trim();
}

// scenarioTitle is a name made of clauses, most telling first, joined by
// commas: whole clauses are dropped from the end until it fits MAX_NAME,
// never cut mid-word unless the first alone is too long.
function scenarioTitle(clauses) {
  const parts = clauses.map(cleanName).filter(Boolean);
  while (parts.length > 1 && parts.join(", ").length > MAX_NAME) parts.pop();
  return cleanName(parts.join(", "));
}

// recordingLabel is how the source picker shows the recording named name: a
// file in --recordings by its name; an upload, one of uploads (GET
// /v1/sources), by the file name it was sent under, its size and when it was
// sent (UTC), then the start of its hash, which names it. The file name is
// the client's: cleaned here as a typed name is, isolated so that a
// right-to-left name cannot reorder what follows it, and shown as text
// alone.
function recordingLabel(name, uploads) {
  const u = Array.isArray(uploads) ? uploads.find(x => x && x.name === name) : undefined;
  if (!u) return name;
  const bytes = Number(u.bytes) || 0;
  const kib = Math.max(1, Math.round(bytes / 1024));
  const size = kib < 1024 ? `${kib} KiB` : `${(bytes / 1048576).toFixed(1)} MiB`;
  const when = /^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2})/.exec(String(u.uploadedAt || ""));
  const hash = name.startsWith("upload:") ? name.slice(7, 19) : name;
  const file = cleanName(u.fileName || "") || "unnamed";
  return `\u2068${file}\u2069 (${size}${when ? `, ${when[1]} ${when[2]}Z` : ""}) · upload ${hash}`;
}

// sourceProblem is why a link's source cannot be opened against a server
// that lists sources (GET /v1/sources), or "" where it can. No source is
// substituted for one the server does not read.
function sourceProblem(link, sources) {
  if (!sources) return "the page has not listed the server's sources yet: try again in a moment";
  const p = new URLSearchParams(link), rec = p.get("recording"), ns = p.get("namespace");
  if (rec) return sources.recordings.includes(rec) ? "" : `it reads the recording ${rec}, which this server does not list`;
  if (ns) {
    if (!sources.prometheus) return `it reads namespace ${ns} from Prometheus, which this server does not read`;
    return sources.namespaces.includes(ns) ? "" : `it reads namespace ${ns}, which this server may not read`;
  }
  return "it names no source";
}

// The link's knobs are applied by one load: link is {seq, restore}, or null
// once applied or given up. A load that starts takes the link over, so a
// Load pressed before the link's load returns still applies it.
// linkAtLoadStart is the link as a load numbered seq starts.
function linkAtLoadStart(link, seq) {
  return link ? {seq, restore: link.restore} : null;
}

// linkAtLoadEnd is what a load numbered seq does with the link when it ends,
// ok or not, with currentSeq the newest load: apply the knobs, put back what
// was before (restore, for a scenario opened), or wait for the next load
// (the page's own link). A load a newer one overtook does nothing.
function linkAtLoadEnd(link, seq, currentSeq, ok) {
  if (!link || link.seq !== seq || seq !== currentSeq) return {link, apply: false, restore: null};
  if (ok) return {link: null, apply: true, restore: null};
  if (link.restore) return {link: null, apply: false, restore: link.restore};
  return {link: {seq: null, restore: null}, apply: false, restore: null};
}

// beginOpen is opening sc over the page as it stands (before: the link's
// text, the address, the source form and the name box): what to put on the
// page, and the link to hand to the load, which puts before back if it fails.
function beginOpen(before, sc) {
  return {next: {linked: sc.link, hash: "#" + sc.link, name: sc.name}, link: {seq: null, restore: before}};
}

// importPlan is what importing sc does to the store raw: a scenario saved
// under the same name with the same link is opened as it is (save false);
// another under that name makes this one "name (2)" and so on. raw itself is
// never changed: the page writes the answer once, or nothing.
function importPlan(raw, sc) {
  const list = visibleScenarios(raw);
  const same = list.find(x => x.name === sc.name);
  if (same && same.link === sc.link) return {raw, name: same.name, save: false};
  const name = freeName(sc.name, list.map(x => x.name));
  return {raw: putScenario(raw, {name, link: sc.link, saved: sc.saved}).raw, name, save: true};
}

// sameCurve reports whether two curves (points {x, y}, y null where refused)
// are the same line: then A's is drawn under the current one, unseen.
function sameCurve(a, b) {
  if (!a || !b || a.length !== b.length || !a.length) return false;
  const near = (u, v) => u === v || (u != null && v != null && Math.abs(u - v) <= 1e-9 * Math.max(1, Math.abs(u)));
  return a.every((q, i) => near(q.x, b[i].x) && near(q.y, b[i].y));
}

// measuredSplit splits points ({x, y, extrapolated}) where an experimental
// model's extrapolation starts: measured is drawn solid, up to and with
// the last point at or under the highest load measured; beyond dashed, from
// that point on, so that the two meet. With no point extrapolated beyond is
// empty; with every point extrapolated measured is empty.
function measuredSplit(points) {
  const i = points.findIndex(q => q.extrapolated);
  if (i < 0) return {measured: points, beyond: []};
  return {measured: points.slice(0, i), beyond: points.slice(Math.max(i - 1, 0))};
}

// ttftModelNote is what the TTFT figure says of the experimental model at
// the plan's fleet (ttft.model), one short line; empty without one. It
// names the model, its evidence from the source where there is some, and
// what the dashed part is; or, where the model did not price, why the
// planner's own TTFT stands.
function ttftModelNote(m) {
  return modelNote(m, "TTFT");
}

// tpotModelNote is ttftModelNote for the TPOT figure (tpot.model): the
// experimental TPOT model prices the percentile, never the mean.
function tpotModelNote(m) {
  return modelNote(m, "TPOT percentile");
}

// modelNote is an experimental model's note on the figure of what it
// prices (ttftModelNote, tpotModelNote).
function modelNote(m, what) {
  if (!m) return "";
  if (!m.priced) return `${m.label}: not used here (${m.notPriced}); the planner's own ${what} is drawn.`;
  const parts = [m.label];
  // The evidence is the held-out score of the mean TTFT: it says so.
  if (m.evidence) parts.push("quick check, TTFT mean " + m.evidence);
  if (m.measuredUpTo != null) parts.push(`dashed: ${m.beyond} (${Number(m.measuredUpTo.toPrecision(3))} r/s)`);
  if (m.rightEnd) parts.push(m.rightEnd);
  return parts.join(" · ") + ".";
}

// figValue is a figure's value, or null where it is refused or absent.
function figValue(f) {
  return f && !f.refused && typeof f.value === "number" ? f.value : null;
}

// delta is b less a as text: signed, in fmt's units, or "same" where the two
// print the same; empty where either is missing. cls is "up" or "down". The
// change is the change of the figures as printed, so that 10.1 to 10.2 is
// +0.10, not the +0.16 the unrounded figures differ by.
function delta(a, b, fmt) {
  if (a == null || b == null) return {text: "", cls: ""};
  const ta = String(fmt(a)), tb = String(fmt(b));
  if (ta === tb) return {text: "same", cls: ""};
  const num = t => parseFloat(t.replace(/,/g, "")), unit = t => t.replace(/^[-+0-9.,]+/, "");
  const shown = isFinite(num(ta)) && isFinite(num(tb)) && unit(ta) === unit(tb);
  const d = shown ? num(tb) - num(ta) : b - a;
  return {text: (d > 0 ? "+" : "-") + fmt(Math.abs(d)), cls: d > 0 ? "up" : "down"};
}

// figDelta is delta of two figures, saying where either is refused: "both
// refused", "now refused" (A priced it), "now priced" (A refused it).
function figDelta(fa, fc, fmt) {
  const va = figValue(fa), vc = figValue(fc);
  if (!fa || !fc) return {text: "", cls: ""};
  if (va == null && vc == null) return {text: "both refused", cls: ""};
  if (va == null) return {text: "now priced", cls: ""};
  if (vc == null) return {text: "now refused", cls: ""};
  return delta(va, vc, fmt);
}

// compareRows is the compare table: A, the current plan and the change, row
// by row. f formats: rate (r/s), fleet (counts), fig (a figure in r/s, or its
// refusal), bind (what binds a plan), slo (statistic, target) and shape (what
// a plan was priced at). Every text is escaped here.
function compareRows(a, cur, f) {
  const rows = [];
  const row = (name, at, ct, d) => rows.push(`<tr><th scope="row">${esc(name)}</th><td>${esc(at)}</td><td>${esc(ct)}</td>` +
    `<td class="${d.cls ? "d-" + d.cls : ""}">${esc(d.text)}</td></tr>`);
  const same = (x, y) => ({text: x === y ? "same" : "changed", cls: ""});
  row("Fleet", f.fleet(a.scenario.fleet), f.fleet(cur.scenario.fleet), {text: fleetDelta(a.scenario.fleet, cur.scenario.fleet), cls: ""});
  // A higher rate set is neither better nor worse: its change is not coloured.
  const ra = a.limits.chosen.rate, rc = cur.limits.chosen.rate, rateSame = f.rate(ra) === f.rate(rc);
  row("Rate you set", f.rate(ra), f.rate(rc), Object.assign(delta(ra, rc, f.rate), {cls: ""}));
  for (const [key, name] of [["fleet", "Fleet limit"], ["prefill", "Prefill limit (TTFT)"], ["decode", "Decode limit (TPOT)"]]) {
    const xa = a.limits[key], xc = cur.limits[key];
    if (!xa && !xc) continue;
    row(name, xa ? f.fig(xa) : "none", xc ? f.fig(xc) : "none", figDelta(xa, xc, f.rate));
  }
  // Headroom at another rate is not better or worse: coloured only where the
  // rate is the same.
  const hd = figDelta(a.limits.chosen.headroom, cur.limits.chosen.headroom, f.rate);
  row("Headroom at the rate", f.fig(a.limits.chosen.headroom), f.fig(cur.limits.chosen.headroom), rateSame ? hd : Object.assign(hd, {cls: ""}));
  const ba = f.bind(a), bc = f.bind(cur);
  row("Binds", ba, bc, same(ba, bc));
  const ta = f.slo(a.ttft.statistic, a.ttft.line.target), tc = f.slo(cur.ttft.statistic, cur.ttft.line.target);
  row("TTFT target", ta, tc, same(ta, tc));
  if (a.tpot || cur.tpot) {
    const pa = a.tpot ? f.slo(a.tpot.statistic, a.tpot.target) : "none", pc = cur.tpot ? f.slo(cur.tpot.statistic, cur.tpot.target) : "none";
    row("TPOT target", pa, pc, same(pa, pc));
  }
  const sa = f.shape(a), sc = f.shape(cur);
  row("Priced at", sa, sc, same(sa, sc));
  // The buckets, only where A's and the current plan's edges differ: the
  // per-bucket lines are drawn for the current plan alone.
  const ea = edgesText(a), ec = edgesText(cur);
  if (ea !== ec) row("Buckets", ea, ec, {text: "changed", cls: ""});
  return `<table><tr><th></th><th>A (dashed)</th><th>Current</th><th>Change</th></tr>${rows.join("")}</table>`;
}

// MAX_RETRIES is how many times the page asks a busy server again.
const MAX_RETRIES = 5;

// retryDelay is how long, in milliseconds, the page waits before asking again
// after a response of status with the Retry-After header retryAfter (null when
// absent), on its attempt-th retry from 0; -1 is not to ask again. Only a busy
// server's 503 is retried: it alone sets Retry-After. A 503 without it is a
// plan stopped at the server's plan budget (PlanBudgetExceeded), which would
// be priced from the start again, or a request that ended. The wait is the
// header's seconds, at least 1 and at most 30.
function retryDelay(status, retryAfter, attempt) {
  if (status !== 503 || attempt >= MAX_RETRIES) return -1;
  if (retryAfter === null || retryAfter === undefined || String(retryAfter).trim() === "") return -1;
  const s = Number(retryAfter);
  return 1000 * (Number.isFinite(s) ? Math.min(Math.max(s, 1), 30) : 1);
}

// fleetDelta is the change in replicas from a to b, a role at a time.
function fleetDelta(a, b) {
  const parts = [];
  for (const role of ["prefill", "decode"]) {
    const d = (b[role] || 0) - (a[role] || 0);
    if (d) parts.push(`${d > 0 ? "+" : ""}${d} ${role}`);
  }
  return parts.length ? parts.join(", ") : "same";
}

// ---- The fleet report (POST /v1/report) ----
//
// Everything the report card shows is built here as plain strings, so that
// the node check reads exactly what the page draws; planner.js only puts the
// strings into text nodes.

// REPORT_SHAPE is a request shape as the report reads one: "measured", or
// IL/OL[/hit] in tokens with an optional k (1024 tokens), hit in [0, 1).
const REPORT_SHAPE = /^([0-9]+(\.[0-9]+)?k?)\/([0-9]+(\.[0-9]+)?k?)(\/(0(\.[0-9]+)?))?$/;
const MAX_REPORT_SHAPES = 6, MAX_REPORT_LOADS = 8, MAX_SHAPE_TOKENS = 1 << 20;
const DEFAULT_REPORT_LOADS = ["50%", "80%", "100%", "seen"];

// shapeTokens is a length as a shape writes it, in tokens.
function shapeTokens(s) {
  return s.endsWith("k") ? 1024 * Number(s.slice(0, -1)) : Number(s);
}

// reportShapeProblem is why s is not a shape the report reads, "" when it is.
function reportShapeProblem(s) {
  if (s === "measured" || s === "load") return "";
  const m = REPORT_SHAPE.exec(typeof s === "string" ? s : "");
  if (!m) return "a shape is measured, load, or IL/OL[/hit] in tokens (8k/200, 1k/4k/0.3)";
  for (const t of [m[1], m[3]]) {
    const n = shapeTokens(t);
    if (!(n >= 1 && n <= MAX_SHAPE_TOKENS)) return "a length is 1 to 1024k tokens";
  }
  return "";
}

// shapeText writes one shape row's boxes as the report reads it: IL and OL
// in tokens, and the hit rate when one is typed. Blank boxes are "load": the
// shape of the traffic the load judged at ran at.
function shapeText(row) {
  const il = String(row.il || "").trim(), ol = String(row.ol || "").trim(), hit = String(row.hit || "").trim();
  if (!il && !ol && !hit) return "load";
  return hit ? `${il}/${ol}/${hit}` : `${il}/${ol}`;
}

// reportShapes is the rows' shapes, each once, in order: a blank row is the
// load's own shape, and the same shape typed twice is asked once. Only blank
// rows, or none, is the empty list: the server's default, the load's shape,
// or the measured one where the load read none.
function reportShapes(rows) {
  const out = [];
  for (const row of rows || []) {
    const s = shapeText(row);
    if (!out.includes(s)) out.push(s);
  }
  return out.length === 1 && out[0] === "load" ? [] : out;
}

// parseReportLoads reads the loads box: comma separated, each 80%, 5r/s,
// 64c (a concurrency) or seen. Empty is the server's default (50%, 80%,
// 100% and seen).
function parseReportLoads(text) {
  const loads = String(text || "").split(",").map(s => s.trim()).filter(s => s);
  if (loads.length > MAX_REPORT_LOADS) return {loads: [], problem: `at most ${MAX_REPORT_LOADS} loads`};
  for (const l of loads) {
    if (l === "seen" || /^seen *[x×*] *[0-9]+(\.[0-9]+)?$/.test(l)) continue;
    const pct = /^([0-9]+(\.[0-9]+)?)%$/.exec(l), rate = /^([0-9]+(\.[0-9]+)?)r\/s$/.exec(l);
    const conc = /^([0-9]+(\.[0-9]+)?)c$/.exec(l);
    if (pct && Number(pct[1]) > 0 && Number(pct[1]) <= 100) continue;
    if (rate && Number(rate[1]) > 0) continue;
    if (conc && Number(conc[1]) > 0 && Number(conc[1]) <= MAX_CONCURRENCY) continue;
    return {loads: [], problem: `${l}: a load is a share of the knee (80%), a rate (5r/s), a concurrency (64c) or seen`};
  }
  return {loads, problem: ""};
}

// reportRequest is the body of POST /v1/report from the page's controls:
// the inputs, the fleet when it differs from the recorded one, the shapes,
// the loads, the growth of the load seen, the headroom, the SLO and the
// assumed terms. The tables are asked at the SLO's own statistics, so that a
// cell's figures are the ones its mark is judged at. problem says what is
// asked wrongly, and then body is null.
function reportRequest(o) {
  const shapes = reportShapes(o.shapes);
  if (shapes.length > MAX_REPORT_SHAPES) return {body: null, problem: `at most ${MAX_REPORT_SHAPES} shapes`};
  for (const s of shapes) {
    const p = reportShapeProblem(s);
    if (p) return {body: null, problem: `${s}: ${p}`};
  }
  const loads = parseReportLoads(o.loads);
  if (loads.problem) return {body: null, problem: loads.problem};
  const body = {inputsId: o.inputsId};
  if (shapes.length) body.shapes = shapes;
  if (o.fleet && Object.keys(o.fleet).length) body.fleet = Object.assign({}, o.fleet);
  if (loads.loads.length) body.loads = loads.loads;
  const growth = String(o.growth == null ? "" : o.growth).trim();
  if (growth !== "") {
    const x = Number(growth);
    if (!(x > 0) || !Number.isFinite(x)) return {body: null, problem: "growth is a factor above 0 (1.5 is 50% more traffic)"};
    // Growth is a load of its own, "seen xN": it adds a column and a line, and
    // never moves the load the fleet is judged at.
    const grown = `seen x${x}`;
    if (x !== 1) {
      body.loads = body.loads || DEFAULT_REPORT_LOADS.slice();
      if (!body.loads.includes(grown)) body.loads.push(grown);
      if (body.loads.length > MAX_REPORT_LOADS) return {body: null, problem: `at most ${MAX_REPORT_LOADS} loads, the grown load seen among them`};
    }
  }
  if (o.loadStatistic) body.loadStatistic = o.loadStatistic;
  // The report judges below the knee: a headroom of 1 is left to the policy.
  if (o.headroom > 0 && o.headroom < 1) body.headroom = o.headroom;
  if (o.slo) {
    body.slo = o.slo;
    if (o.slo.ttft && o.slo.ttft.statistic) body.ttftStatistics = [o.slo.ttft.statistic];
    if (o.slo.tpot && o.slo.tpot.statistic) body.tpotStatistic = o.slo.tpot.statistic;
  }
  if (o.assume && Object.keys(o.assume).length) body.assume = Object.assign({}, o.assume);
  return {body, problem: ""};
}

// REPORT_MARKS are a summary cell's marks: its class, and the word that says
// it without colour. A rate past a prefill knee priced from a lower bound of
// V_P is not judged, never saturated: the true knee may be higher. A TTFT
// over its target priced from that lower bound is at most its figure, so its
// miss is not known either.
const REPORT_MARKS = {met: ["met", "ok"], missed: ["missed", "MISS"], "missed at onset": ["missed", "MISS at onset"], saturated: ["saturated", "SAT"],
  "past a lower-bound knee": ["unknown", "past prefill's lower-bound limit"], "at most, over target": ["unknown", "at most, over target: not known"],
  unknown: ["unknown", "?"], "no target": ["notarget", "no SLO"]};

// REFUSAL_WORDS are the refusals a report shows most, in words.
const REFUSAL_WORDS = {
  Saturated: "saturated: no steady state at this load",
  PastLowerBoundKnee: "past a prefill knee priced from a lower bound of V_P: not priced (assume V_P to price it)",
  AboveDecodeOnset: "past the decode flow-control onset: not priced",
  NoPrefillCapacity: "prefill capacity unknown: no prefill speed learned (assume one to price it)",
  NoFixedLatency: "no prefill fixed latency learned (assume a to price it)",
  NoTPOTHistogram: "no TPOT histogram to read a percentile from",
  TPOTBucketTooWide: "the TPOT histogram is too coarse for a percentile",
  PinnedDefaultB: "the decode line rests on a default, not a measurement (type ITL B under Assume a term to price it)",
  NoLine: "no decode line learned",
  WindowGapAtPeak: "a gap in the window borders its busiest minutes: the load may read low (capture the window again, or on the CLI state the load: --load-seen)",
  NoTraffic: "no traffic to judge",
};

// refusalText is a refused figure in words.
function refusalText(f) {
  const reason = f && f.refused ? f.refused.reason : "";
  if (reason === "NoPrefillCapacity" && (f.refused.flags || []).includes("VPLowerBound")) {
    return "prefill capacity unknown: the prefill pods never queued, so their speed is only a lower bound (assume one to price it)";
  }
  return REFUSAL_WORDS[reason] || (reason ? `not priced (${reason})` : "not priced");
}

// REPORT_FLAG_WORDS are the flags a report carries most, in a short phrase.
// The rest are counted, and their codes kept for a tooltip.
const REPORT_FLAG_WORDS = {
  ShapeLoadMismatch: "prices the load at a shape more than 20% off the traffic that made it",
  AssumedITLB: "assumes the decode ITL B",
  UnvalidatedTTFT: "TTFT is not validated",
  AssumedVP: "assumes the prefill speed you typed",
  AssumedA: "assumes the prefill fixed latency you typed",
  AssumedPromptSpread: "assumes the prompt spread",
  AssumedPoissonArrivals: "assumes Poisson arrivals",
  AssumedShape: "a shape you typed, not the measured one",
  ExtrapolatedShape: "a shape outside what the source saw",
  ExtrapolatedFill: "decode past the fill it was measured at",
  BelowFittedBand: "below the fitted band",
  KVBoundSlots: "KV memory, not the batch cap, sets the slots",
  RouterHopAssumed: "assumes the router hop",
  CoarseTPOTBucket: "TPOT percentile from coarse buckets",
  HeldLine: "a decode line held from earlier",
  DecodeOnly: "decode only: prefill is not priced",
  VPLowerBound: "the prefill speed is a lower bound",
  VPFromPlateau: "the prefill speed is from the occupancy plateau (experimental)",
  VPPromptNotCovered: "prefill speed learned at different prompt lengths",
  ExperimentalTTFT: "priced with the experimental TTFT model (not validated)",
};

// flagSummary is a shape's flags as short phrases, and the rest counted:
// {phrases, more, codes}, codes every flag's code, for a tooltip.
function flagSummary(flags) {
  const phrases = [], rest = [];
  for (const f of flags || []) (REPORT_FLAG_WORDS[f] ? phrases : rest).push(REPORT_FLAG_WORDS[f] || f);
  return {phrases, more: rest.length, rest, codes: (flags || []).join(", ")};
}

// roundMs is seconds as whole milliseconds, one decimal under 10 ms.
function roundMs(s) {
  const ms = 1000 * s;
  return ms < 10 ? ms.toFixed(1) : String(Math.round(ms));
}

// roundRate is a rate in r/s, to three significant figures at most.
function roundRate(r) {
  // A hair up, so that a half rounds up as the server's figures do (62.55 is
  // 62.549999... in binary).
  r += 1e-9 * Math.max(1, Math.abs(r));
  return r >= 100 ? String(Math.round(r)) : r >= 10 ? r.toFixed(1).replace(/\.0$/, "") : r.toFixed(2).replace(/\.?0+$/, "");
}

// msText is a latency figure in words: "637 ms", or its refusal, short. A
// TPOT (tpot true) read at the decode line's floor is marked as at most that,
// and so is a TTFT priced from a lower bound of V_P (upperBound).
function msText(f, tpot) {
  // A figure read at the band's bottom is the line's floor: the latency is at most it.
  if (f && f.value != null) return ((tpot && isFloor(f)) || f.upperBound ? "≤" : "") + roundMs(f.value) + " ms";
  const reason = f && f.refused ? f.refused.reason : "";
  return reason === "Saturated" ? "saturated" : reason === "AboveDecodeOnset" ? "router-queued" : "-";
}

// MAX_CONCURRENCY is the largest concurrency a load may ask, as the server's
// whatifapi.MaxConcurrency.
const MAX_CONCURRENCY = 1048576;

// concurrencyNumber is a mean concurrency as printed: one decimal under 10,
// else whole requests (whatifapi.ConcurrencyNumber).
function concurrencyNumber(x) {
  return x < 10 ? x.toFixed(1) : x.toFixed(0);
}

// concurrencyShort is a row's mean concurrency for a summary cell: "~12 in
// system", "unbounded" at or past the knee, "" where refused or absent.
function concurrencyShort(c) {
  if (!c) return "";
  if (c.unbounded) return "in system: unbounded";
  const v = c.fleet && c.fleet.value;
  return v != null ? `~${concurrencyNumber(v)} in system` : "";
}

// concurrencyNote is what the page says under the rate of a plan: the
// mean requests in the system there (Little's law), and where a concurrency
// was typed, the rate it translates into, flagged where the fleet saturates
// first. limit is the fleet's limit, r/s: a rate short of it is where the
// time in the system is first refused, not the limit.
function concurrencyNote(chosen, limit) {
  const c = chosen && chosen.concurrency, from = chosen && chosen.fromConcurrency;
  const parts = [];
  if (from) {
    const n = concurrencyNumber(from.concurrency), r = roundRate(from.rate);
    if (from.atKnee && limit > 0 && from.rate < limit * (1 - 1e-6)) {
      parts.push(`This fleet holds fewer than ${n} requests at any rate it prices: the rate is ${r} r/s, short of the limit, where its time in the system is first refused.`);
    } else if (from.atKnee) {
      parts.push(`${n} requests open saturate this fleet: it holds fewer below its limit, so the rate is the limit's ${r} r/s; more concurrency adds queueing, not throughput.`);
    } else if (from.lowerBound) {
      parts.push(`${n} requests in the system on average is at least ${r} r/s on this fleet: the time in the system is priced from a lower bound of the prefill speed.`);
    } else {
      parts.push(`${n} requests in the system on average is ${r} r/s on this fleet.`);
    }
  }
  if (c && c.unbounded) {
    parts.push("Here requests queue without bound: no average holds.");
  } else if (c && c.fleet && c.fleet.value != null) {
    let s = `About ${concurrencyNumber(c.fleet.value)} requests in the system on average`;
    const role = [];
    if (c.routerQueue && c.routerQueue.value != null) role.push(`${concurrencyNumber(c.routerQueue.value)} held at the router`);
    if (c.prefill && c.prefill.value != null) role.push(`${concurrencyNumber(c.prefill.value)} in prefill`);
    if (c.decode && c.decode.value != null) {
      role.push(`${concurrencyNumber(c.decode.value)} decoding` + (c.decodePerPod && c.decodePerPod.value != null ? ` (${concurrencyNumber(c.decodePerPod.value)} a decode pod)` : ""));
    }
    if (role.length) s += ` (${role.join(", ")})`;
    parts.push(s + ".");
  }
  return parts.join(" ");
}

// rowFigures is a row's two latencies at the SLO's statistics: the TTFT at
// the TTFT target's, and the TPOT at the TPOT target's, as the row's cell
// carries them (a table prints p90 whatever the SLO judges); with no target,
// TTFT p90 and the TPOT mean.
function rowFigures(row, slo) {
  const tStat = slo && slo.ttft ? slo.ttft.statistic : "p90";
  const pStat = slo && slo.tpot ? slo.tpot.statistic : "mean";
  const cell = row.cell || {};
  let ttft = (row.ttft || []).find(s => s.statistic === tStat) || (row.ttft || [])[0] || {statistic: tStat};
  if (cell.ttft && cell.ttft.statistic === tStat) ttft = cell.ttft;
  let tpot = Object.assign({statistic: "mean"}, row.tpotMean || {});
  if (pStat !== "mean" && row.tpotPercentile && row.tpotPercentile.statistic === pStat) tpot = row.tpotPercentile;
  if (cell.tpot && cell.tpot.statistic === pStat) tpot = cell.tpot;
  return {ttft, tpot};
}

// verdictText is a shape's verdict in plain words, with its numbers.
function verdictText(s, load) {
  const v = s.verdict || {}, c = s.capacity || {};
  const knee = c.fleetKnee && c.fleetKnee.value, h = c.headroom;
  const binds = String((c.fleetKnee && c.fleetKnee.binds) || "").split(" ")[0];
  const rate = load != null ? `${roundRate(load)} r/s` : "the load seen";
  // A shape the question typed is a what-if: all of the load at that shape.
  const at = s.source === "asked" ? `if all ${rate} were ${s.label} requests,` : `at ${rate}`;
  switch (v.verdict) {
  case "UNDER-PROVISIONED": {
    if (knee != null && load != null && load >= knee) {
      // Past the knee: every role whose own knee the load passes needs more.
      const roles = [["prefill", c.prefillKnee], ["decode", c.decodeKnee]].filter(([, k]) => k && k.value != null && load >= k.value).map(([r]) => r);
      const add = roles.length ? roles.join(" and ") : binds;
      return `UNDER-PROVISIONED: ${at} the load is ${(load / knee).toFixed(1)}x the fleet's ${roundRate(knee)} r/s knee; add ${add} replicas.`;
    }
    if (knee != null && load != null && load > h * knee) {
      return `UNDER-PROVISIONED: ${at} the fleet is past ${Math.round(100 * h)}% of its ${roundRate(knee)} r/s knee, set by ${binds}; add ${binds} replicas.`;
    }
    const missed = missedTarget(v.reason);
    // Only a missed TPOT says which role: decode sets it.
    const add = missed.startsWith("TPOT") ? " add decode replicas." : "";
    return `UNDER-PROVISIONED: ${at} ${missed || "a latency target is missed"};${add}`.replace(/;$/, ".");
  }
  case "OVER-PROVISIONED":
    return `OVER-PROVISIONED: ${at} one ${(v.shrink || []).join(" or one ")} replica fewer still meets every target with headroom.`;
  case "RIGHT-SIZED":
    return `RIGHT-SIZED: ${at} every target is met with headroom, and one replica fewer of either role would not be.`;
  }
  return `NOT JUDGED: ${v.reason || "no knee or no load to judge at"}.`;
}

// missedTarget is the first target a verdict's reason says is missed, in
// words ("TTFT p95 7.6 s misses 2 s"); "" with none.
function missedTarget(reason) {
  const m = /(TTFT|TPOT) (\S+) ([0-9.]+) ms misses ([0-9.]+) ms/.exec(String(reason || ""));
  if (!m) return "";
  const t = ms => (Number(ms) >= 1000 ? (Number(ms) / 1000).toFixed(1).replace(/\.0$/, "") + " s" : Math.round(Number(ms)) + " ms");
  return `${m[1]} ${m[2]} ${t(m[3])} misses ${t(m[4])}`;
}

// sloLine says what a cell's mark is judged against.
function sloLine(slo) {
  const parts = [];
  if (slo && slo.ttft) parts.push(`TTFT ${slo.ttft.statistic} ${roundMs(slo.ttft.target)} ms`);
  if (slo && slo.tpot) parts.push(`TPOT ${slo.tpot.statistic} ${roundMs(slo.tpot.target)} ms`);
  return parts.length ? "Marks are judged against " + parts.join(" and ") + "." : "No SLO target is set: no cell is judged.";
}

// reportView is a report as the page shows it, all of it strings: each
// shape's verdict line, the matrix (a header per load with its rate where
// one shape fixes it, and per cell the two latencies, a class and a word),
// each shape's detail lines and flags, and the notes on accuracy.
function reportView(r) {
  const slo = r.slo || {};
  const load = r.load && r.load.rate ? r.load.rate.value : null;
  const shapes = r.shapes || [];
  const loads = r.loads || [];
  const tStat = slo.ttft ? slo.ttft.statistic : "p90", pStat = slo.tpot ? slo.tpot.statistic : "mean";
  const header = loads.map(l => {
    const rates = shapes.map(s => (s.table || []).find(t => t.load === l)).filter(t => t).map(t => roundRate(t.rate));
    const one = rates.length && rates.every(x => x === rates[0]);
    if (!one) return l;
    // A load that is a rate already names it.
    return l.endsWith("r/s") ? l : `${l} (${rates[0]} r/s)`;
  });
  const sameRate = loads.map((l, i) => header[i] !== l || l.endsWith("r/s"));
  let floors = false;
  const rows = shapes.map(s => ({
    label: s.label, extrapolated: !!s.extrapolated, why: s.extrapolatedWhy || "",
    cells: loads.map((l, i) => {
      const row = (s.table || []).find(t => t.load === l);
      if (!row) return {text: "not priced", parts: ["not priced"], cls: "none", word: ""};
      const f = rowFigures(row, slo);
      const m = REPORT_MARKS[row.cell.mark] || ["unknown", "?"];
      const rate = sameRate[i] ? "" : ` @ ${roundRate(row.rate)} r/s`;
      if (isFloor(f.tpot)) floors = true;
      // A TPOT target's percentile refused leaves the mean, which says so.
      const tpotMean = slo && slo.tpot && slo.tpot.statistic !== "mean" && f.tpot.statistic === "mean" && f.tpot.value != null ? " (mean)" : "";
      const parts = [`TTFT ${msText(f.ttft, false)}`, `TPOT ${msText(f.tpot, true)}${tpotMean}`];
      if (rate) parts.push(rate.trim());
      const inSystem = concurrencyShort(row.concurrency);
      if (inSystem) parts.push(inSystem);
      return {text: parts.join(" · ").replace(" · @", " @"), parts, cls: m[0], word: m[1]};
    }),
  }));
  const verdicts = shapes.map(s => {
    const v = verdictLine(s.verdict, verdictText(s, load), r.fleet);
    // Growth is its own line beside the verdict, never in its place.
    const growth = (s.growth || []).map(g => Object.assign(verdictLine(g.verdict, "", r.fleet),
      {lead: `with ${g.x}× traffic (${roundRate(g.rate)} r/s)`, rate: g.rate}));
    // The load's own shape names its prompt length: its knee is not the panel's.
    return Object.assign(v, {label: s.label, extrapolated: !!s.extrapolated, whatIf: s.source === "asked",
      prompt: s.label === "load" && s.inputTokens ? `priced at the traffic's ${Math.round(s.inputTokens / 1000)}k-token prompts; the answer panel prices the prefill pods' measured work per request` : "",
      decodeOnly: (s.verdict || {}).scope === "decode only", growth});
  });
  const details = shapes.map(s => {
    const c = s.capacity || {}, u = s.utilization || {decode: {}, prefill: {}};
    // A knee priced from a lower bound of V_P is at least its rate.
    const rate = f => (f && f.value != null ? (f.lowerBound ? "at least " : "") + roundRate(f.value) + " r/s" : refusalText(f));
    const share = f => (f && f.value != null ? f.value.toFixed(2) : refusalText(f));
    return {
      summary: `${s.label}: IL ${Math.round(s.inputTokens)}, OL ${Math.round(s.outputTokens)}, prefix hit ${Number(s.prefixHitRate).toFixed(2)} (${s.source})`,
      lines: [
        s.extrapolated ? `Extrapolated: ${s.extrapolatedWhy}.` : "",
        s.loadShape ? `The load's cycles (${s.loadShape.cycles}, cycles ${s.loadShape.fromCycle}-${s.loadShape.toCycle}) ran at IL ${Math.round(s.loadShape.inputTokens)}, OL ${Math.round(s.loadShape.outputTokens)}` +
          (s.loadShape.mismatch ? ": SHAPE/LOAD MISMATCH, the load is priced at a shape more than 20% off the traffic that made it; ask the shape load to price it at its own." : ".") : "",
        `Knees: prefill ${rate(c.prefillKnee)}, decode ${rate(c.decodeKnee)}` +
          (c.decodeKnee && c.decodeKnee.validation ? ` (decode knee ${c.decodeKnee.validation.label})` : "") + `; the fleet's is ${rate(c.fleetKnee)}, set by ${String((c.fleetKnee || {}).binds || "")}. ` +
          `Operating point ${rate(c.operating)}; the most that meets every target ${rate(c.maxAtSLO)}.`,
        `At the load: ${decodeUse(u.decode)}; ${prefillUse(u.prefill)}.`,
        ...roleLines(s.verdict),
        `The numbers: ${plainReason((s.verdict || {}).reason || "")}`,
      ].filter(x => x),
      // A shape the question did not type is no "shape you typed".
      flags: flagSummary((s.flags || []).filter(x => x !== "AssumedShape" || s.source === "asked")),
    };
  });
  // The details and the notes speak plainly too: symbols and the benchmark
  // names stay in the Flags drawer and the docs.
  for (const d of details) d.lines = d.lines.map(plainText);
  return {verdicts, header, rows, details, notes: (r.notValidated || []).map(plainText), load: loadLine(r.load),
    legendStats: `Each cell is TTFT ${tStat} · TPOT ${pStat}. ${sloLine(slo)}` +
      (floors ? " ≤ marks a TPOT at the decode line's floor: below the fill it was fitted over, so at most that." : "")};
}

// reportMissing reports whether an answer says the server has no
// POST /v1/report: a server of an earlier release answers 404 "no such
// endpoint". A 404 for an inputsId the server dropped is not that.
function reportMissing(status, error) {
  return status === 405 || (status === 404 && String(error || "").includes("no such endpoint"));
}

// rateNote is what the page says under the rate a plan started at, from its
// limits.chosen.rateSource: nothing for a rate typed. With one knee refused
// the rate is a place to start, not an operating point, and the note says so.
function rateNote(source) {
  switch (source) {
  case "operating point":
    return "started at the operating point: the source carries no request rate";
  case "prefill knee; decode not priced":
    return "started at headroom times the prefill knee: the decode knee is not priced, so there is no operating point " +
      "and the fleet limit is refused. Type your request rate.";
  case "decode knee; prefill not priced":
    return "started at headroom times the decode knee: the prefill knee is not priced, so there is no operating point " +
      "and the fleet limit is refused. Type your request rate.";
  default:
    return "";
  }
}

// reportCurrent reports whether an answer is for the inputs on the page:
// one asked for inputs since replaced is dropped.
function reportCurrent(askedFor, current) {
  return !!askedFor && askedFor === current;
}

// pct is a share as a whole percent.
function pct(x) {
  return Math.round(100 * x) + "%";
}

// decodeUse is decode's utilization at the load, in words.
function decodeUse(d) {
  const pull = kvPullUse(d && d.kvPull);
  if (!d || !d.kvFill || d.kvFill.value == null) return "decode " + refusalText(d && d.kvFill) + pull;
  const wait = d.waitProbability && d.waitProbability.value != null ? `, ${pct(d.waitProbability.value)} of requests wait for a slot` : "";
  const slots = d.slotsOccupied && d.slotsOccupied.value != null ? `, ${Math.round(d.slotsOccupied.value)} of ${d.slots} batch slots busy` : "";
  return `decode KV ${pct(d.kvFill.value)} full (of ${pct(d.bandTop)} usable)${slots}${wait}${pull}`;
}

// kvPullUse is the KV pull's load on the decode pods, in words, "" with none.
// It stands where decode's KV fill is refused: a pull at or past 100% is the
// reason a TTFT is.
function kvPullUse(k) {
  if (!k) return "";
  if (k.utilization >= 1) return `; KV pull ${pct(k.utilization)} busy per decode pod, saturated`;
  return `; KV pull ${pct(k.utilization)} busy per decode pod` +
    (k.setsTTFT ? ", the larger mean wait in TTFT (it sits on the decode pods)" : "");
}

// prefillUse is prefill's utilization at the load, in words.
function prefillUse(p) {
  if (!p || !p.pooled || p.pooled.value == null) return "prefill " + refusalText(p && p.pooled);
  return `prefill ${pct(p.pooled.value)} busy pooled, ${pct(p.worstPod.value)} on the busiest pod`;
}

// REASON_WORDS replace the refusal codes in a verdict's reason.
const REASON_WORDS = {AboveDecodeOnset: "router-queued", NoPrefillCapacity: "unpriced (prefill capacity unknown)",
  NoFixedLatency: "unpriced (no prefill latency)", NoTPOTHistogram: "unpriced (no TPOT histogram)"};

// plainReason is a verdict's reason with its refusal codes in words.
function plainReason(s) {
  let out = s;
  for (const [code, words] of Object.entries(REASON_WORDS)) out = out.split(code).join(words);
  return out;
}

// isFloor reports whether a figure was read at the decode line's floor, the
// bottom of the band it was fitted over (BelowFittedBand).
function isFloor(f) {
  return !!(f && f.value != null && (f.flags || []).includes("BelowFittedBand"));
}

// verdictLine is one verdict as a line: its word, the sentence after it
// (the server's summary, else the page's own words from fallback), and both
// whole.
function verdictLine(v, fallback, has) {
  const word = (v && v.verdict) || "NOT JUDGED";
  const text = (v && v.summary) || fallback || word;
  // rest is the sentence after its verdict, which the page sets in bold.
  let rest = text.startsWith(word + ": ") ? text.slice(word.length + 2) : text;
  const needs = neededText(v && v.needed, has, word);
  // The fleet needed is shown as its own chip, so the sentence does not
  // repeat it.
  if (needs) rest = rest.replace(NEEDS_CLAUSE, "").replace(/;\s*\(/, " (").replace(/[;,]\s*\./, ".");
  // trust is what the source's self-check says of the verdict (#298).
  return {verdict: word, text, rest, needs, unmet: !!(v && v.needed && v.needed.unmet), trust: (v && v.trust) || ""};
}

// NEEDS_CLAUSE is the "needs P prefill + D decode (has p + d), N decode if
// the decode knee reads E% high, or a + b" clause of a summary, with the
// fleet needed at each f after it (fSensitivityText).
const NEEDS_CLAUSE = /;?\s*needs [^;()]*\(has \d+ \+ \d+\)(, (more than )?\d+ decode if the decode knee reads \d+% high)?(,? or \d+ \+ \d+)*(; (the same at f [\d.]+ to [\d.]+|at f [\d.]+ (needs \d+ \+ \d+|no fleet holds)(, at f [\d.]+ (needs \d+ \+ \d+|no fleet holds))*))?/;

// neededText is the fleet a verdict needs, in words: "needs 3 prefill +
// 4 decode (has 2 + 2)", the decode count if the decode knee reads as high
// as its label allows, "or 4 + 3" for each alternative, "at least" where
// the prefill side is not priced, or the bound no fleet within serves.
function neededText(n, has, verdict) {
  // A right-sized fleet carries a needed fleet only for its spare or a
  // cheaper mix in GPUs: no "needs" chip beside a fleet that holds.
  if (!n) return "";
  if (verdict === "RIGHT-SIZED") {
    const w = n.withSpare, gp = g => g != null ? ` = ${g} GPUs` : "";
    if (n.minimizes) return `a mix of fewer GPUs also holds: ${n.prefill} prefill + ${n.decode} decode${gp(n.gpus)}` +
      (w ? `; with ${w.spare} spare of each role ${w.fleet.prefill} + ${w.fleet.decode}${gp(w.gpus)}` : "");
    return w ? `with ${w.spare} spare of each role: ${w.fleet.prefill} prefill + ${w.fleet.decode} decode${gp(w.gpus)}` : "";
  }
  if (n.unmet) return `no fleet up to ${n.maxReplicas} replicas serves this (${n.unmet} does not reach the load)`;
  const now = has ? ` (has ${has.prefill} + ${has.decode})` : "";
  const bound = n.decodeAtErrorBound > n.decode ? `, ${n.decodeAtErrorBound >= n.maxReplicas ? "at least " : ""}${n.decodeAtErrorBound} decode if the decode knee reads ${Math.round((n.decodeKneeError || 0) * 100)}% high` : "";
  if (n.decodeOnly) return `needs at least ${n.decode} decode${has ? ` (has ${has.decode})` : ""}${bound}; prefill is not priced`;
  const alt = (n.alternatives || []).map(a => `or ${a.prefill} + ${a.decode}`).join(", ");
  return `needs ${n.prefill} prefill + ${n.decode} decode${now}${bound}` + (alt ? ", " + alt : "") + fSensitivityText(n);
}

// fSensitivityText is the fleet needed at each fixed prefill imbalance f, as
// the summary words it: "; at f 1 needs 3 + 3, at f 1.5 needs 4 + 3", or
// "; the same at f 1 to 1.5"; "" with none.
function fSensitivityText(n) {
  const rows = (n && n.fSensitivity) || [];
  if (!rows.length) return "";
  if (rows.every(r => !r.unmet && r.prefill === n.prefill && r.decode === n.decode)) {
    return `; the same at f ${rows[0].f} to ${rows[rows.length - 1].f}`;
  }
  return "; " + rows.map(r => r.unmet ? `at f ${r.f} no fleet holds` : `at f ${r.f} needs ${r.prefill} + ${r.decode}`).join(", ");
}

// roleLines is a verdict's per-role judgment: a line per role, whether it has
// a replica to spare, and why.
function roleLines(v) {
  return ((v && v.roles) || []).map(r => `${r.role[0].toUpperCase() + r.role.slice(1)}: ${r.spare ? "has a replica to spare" : "no replica to spare"}` +
    (r.detail ? ` (${r.detail})` : "") + ".");
}

// LOAD_STATISTICS are the load statistics a report judges at.
const LOAD_STATISTICS = ["p95", "mean", "peak", "p95-modulation"];

// loadLine is the load the source saw, in words: the statistic judged at,
// and the mean, p95 and peak beside it.
function loadLine(l) {
  if (!l || !l.rate || l.rate.value == null) return "No load recorded: " + refusalText(l && l.rate) + ".";
  const at = l.statistic === "stated" ? `the stated load of ${roundRate(l.rate.value)} r/s`
    : `the ${l.statistic === "p95-modulation" ? "p95 less counting noise" : l.statistic || "observed"} load of ${roundRate(l.rate.value)} r/s`;
  const seen = [["mean", l.mean], ["p95", l.p95], ["peak", l.peak], ["p95 less counting noise", l.p95Modulation]].filter(([, x]) => x != null).map(([k, x]) => `${k} ${roundRate(x)}`);
  // Under 20 busy cycles the p95 is the peak: too few to tell them apart.
  const peakNote = l.p95IsPeak && l.statistic === "p95" ? " (= peak: fewer than 20 cycles)"
    : l.p95IsPeak && l.statistic === "p95-modulation" ? " (from the peak: fewer than 20 cycles)" : "";
  return `Judged at ${at}${peakNote}` + (seen.length ? `; the source saw ${seen.join(", ")} r/s over ${l.cyclesWithTraffic} busy cycles.` : ".");
}

// noPrefillWords is a NoPrefillCapacity refusal in the page's one wording,
// the same in the plan view and in size mode: the V_P the source learned
// only as a lower bound (flag VPLowerBound, the prefill pods never queued),
// or none learned. The reason's code stays in tooltips and the Flags drawer.
function noPrefillWords(r) {
  const lb = (r.flags || []).includes("VPLowerBound");
  const m = /V_P ([0-9.]+) tokens\/s/.exec(r.detail || "");
  const vp = m ? Number(m[1]).toLocaleString("en-US", {maximumFractionDigits: 0}) + " tokens/s" : "the speed it saw";
  return (lb ? `Prefill capacity unknown: the prefill pods never queued, so a prefill pod computes at least ${vp} (a lower bound), and its limit was not observed.` :
    "Prefill capacity unknown: the source learned no prefill speed per pod (it saw no prefill pod busy enough).");
}

// kneeLowerBound says a knee figure rests on a lower bound of V_P: the true
// knee is at least its rate.
function kneeLowerBound(f) {
  return !!f && (f.flags || []).some(x => x === "VPLowerBound" || x === "KneeAtServedWork");
}

// ---- The answer: the executive panel and the refusal card ----
//
// The first screen answers first: a few lines, each with how far to trust it
// (EXEC_BADGES): measured on the source, checked on the source's own held-out
// hours, or the model's estimate. They are read from the inputs, the plan on
// screen and a report judged at the traffic's own shape (execReportRequest),
// which the page asks after a load; the accuracy is the held-out check the
// server ran at load (inputs.ttftModel.heldOut), else the page's own check
// of the source's last half, which runs only when asked. Every string is
// plain text, put on the page as text.

// EXEC_BADGES are the trust badges of the panel's lines.
const EXEC_BADGES = {measured: "Measured", checked: "Checked on your data", estimate: "Estimate"};

// EXEC_GROWTH are the traffic growths the panel's growth line sizes for,
// once the server sizes them (execView's o.growth).
const EXEC_GROWTH = [1.25, 1.5];

// execReportRequest is the report the panel reads: the fleet as recorded, at
// the traffic's own shape (the server's default), judged at the load seen,
// with the plan's SLO, headroom and assumed terms.
function execReportRequest(inputsId, plan) {
  const body = {inputsId, loads: ["seen"]};
  if (plan) {
    if (plan.headroom != null && plan.headroom !== 1) body.headroom = plan.headroom;
    if (plan.slo) body.slo = plan.slo;
    if (plan.assume && Object.keys(plan.assume).length) body.assume = plan.assume;
  }
  return body;
}

// execGPUs is a fleet's GPUs, or null where a counted role's GPUs per
// replica are not stated.
function execGPUs(fleet, p, d) {
  const gp = fleet.prefill ? fleet.prefill.gpusPerReplica : 0, gd = fleet.decode.gpusPerReplica;
  if (!gd || (fleet.prefill && p > 0 && !gp)) return null;
  return p * (gp || 0) + d * gd;
}

// execFleet is a fleet in words: "5 prefill + 11 decode (32 GPUs)".
function execFleet(fleet, p, d) {
  const g = execGPUs(fleet, p, d);
  return (fleet.prefill ? `${p} prefill + ${d} decode` : `${d} replicas`) + (g != null ? ` (${g} GPUs)` : "");
}

// execTimes is a figure in seconds as the panel says it: ms under a second.
function execTimes(s) {
  if (s == null || !isFinite(s)) return "-";
  return s >= 1 ? `${Number(s.toPrecision(3))} s` : `${Number((s * 1000).toPrecision(3))} ms`;
}

// execSLO is the targets the answer judges against, in words, and whether
// they are the page's defaults rather than yours.
function execSLO(k) {
  const t = `TTFT ${k.ttftStat} ${execTimes(k.ttftTarget)}, TPOT ${k.tpotStat} ${execTimes(k.tpotTarget)}`;
  return (k.ttftDefault || k.tpotDefault) ? `${t} (the page's default: set yours under SLO)` : `${t} (yours)`;
}

// execMeasuredAt is what the fleet measured at the busiest judged step of a
// check: the step, and each target's statistic measured against its target.
function execMeasuredAt(bt, k) {
  const judged = (bt.steps || []).filter(s => !s.excluded);
  if (!judged.length) return null;
  const s = judged.reduce((a, b) => (b.rate > a.rate ? b : a));
  const at = (key, stat, target) => {
    const p = (s[key] || []).find(x => x.statistic === stat);
    if (!p) return null;
    if (p.aboveHistogram) return {text: `${key.toUpperCase()} ${stat} above the histogram's top bucket`, missed: true};
    if (p.measuredBucket) {
      const b = p.measuredBucket;
      const missed = b.lo > target ? true : b.hi <= target ? false : null;
      return {text: `${key.toUpperCase()} ${stat} between ${execTimes(b.lo)} and ${execTimes(b.hi)} (target ${execTimes(target)}${missed == null ? ", the bucket spans it" : missed ? ", missed" : ", met"})`, missed};
    }
    if (p.measured == null) return null;
    const missed = p.measured > target;
    return {text: `${key.toUpperCase()} ${stat} ${execTimes(p.measured)} (target ${execTimes(target)}, ${missed ? "missed" : "met"})`, missed};
  };
  const parts = [at("ttft", k.ttftStat, k.ttftTarget), at("tpot", k.tpotStat, k.tpotTarget)].filter(Boolean);
  if (!parts.length) return null;
  return {when: s.start.slice(11, 16), rate: s.rate, fleet: [s.prefill, s.decode], parts};
}

// execAccuracy is a check's scores in a sentence: how many held-out steps
// each latency read within 20%, which way it leans, and the range it tested.
function execAccuracy(bt, k) {
  const sum = bt.summary || {};
  const pick = (scores, stat) => (scores || []).find(x => x.statistic === stat) || (scores || []).find(x => x.statistic === "mean") || null;
  const words = (name, sc) => {
    if (!sc || !sc.all || !sc.all.steps) return "";
    const a = sc.all, n = Math.round((a.within || 0) * a.steps);
    const lean = a.bias == null ? "" : a.bias < 0.97 ? `, reads ${Math.round((1 - a.bias) * 100)}% low` : a.bias > 1.03 ? `, reads ${Math.round((a.bias - 1) * 100)}% high` : "";
    return `${name} ${sc.statistic} within 20% in ${n} of ${a.steps} five-minute steps${lean}`;
  };
  const hours = bt.test && bt.test.from && bt.test.to ? Math.round((Date.parse(bt.test.to) - Date.parse(bt.test.from)) / 3600e3) : null;
  const parts = [words("TTFT", pick(sum.ttft, k.ttftStat)), words("TPOT", pick(sum.tpot, k.tpotStat))].filter(x => x);
  const judged = (bt.steps || []).filter(s => !s.excluded);
  let range = "";
  if (judged.length) {
    const lo = Math.min(...judged.map(s => s.rate)), hi = Math.max(...judged.map(s => s.rate));
    const fleets = [...new Set(judged.map(s => `${s.prefill} prefill + ${s.decode} decode`))];
    range = `Valid range: ${fleets.length === 1 ? fleets[0] : fleets.length + " fleets"} at ${roundRate(lo)}-${roundRate(hi)} r/s; outside that is an extrapolation.`;
  }
  // A percentile the check could not score at many steps says why, once.
  const tsc = (sum.ttft || []).find(x => x.statistic === k.ttftStat);
  const un = tsc && tsc.unscored ? Object.entries(tsc.unscored).filter(([, n]) => n > 0) : [];
  if (un.length) parts.push(`TTFT ${k.ttftStat} not scored in ${un.map(([w, n]) => `${n} (${(typeof BT_UNSCORED !== "undefined" && BT_UNSCORED[w]) || w})`).join(", ")}`);
  if (!parts.length) return {text: "The check scored no step: see Check the planner.", range};
  return {text: `On ${hours != null ? hours + " held-out hours" : "the held-out hours"} of your traffic: ${parts.join("; ")}.`, range,
    chip: parts[0].replace(/ within 20% in (\d+) of (\d+) five-minute steps.*/, " $1/$2 within 20%")};
}

// execVerdict is the fleet judged at the load seen, in plain words: a
// decode-only judgment says it is no fleet recommendation.
function execVerdict(fleet, v) {
  if (!v) return "";
  const nd = fleet.decode.replicas;
  if (v.scope === "decode only") {
    const n = v.needed;
    if (n && n.decodeOnly && !n.unmet) {
      const held = n.decode < nd ? `${n.decode}-${nd} decode would hold TPOT` : `all ${nd} decode are needed to hold TPOT`;
      return `Decode only: ${held}. Prefill was not sized, so this is not a fleet recommendation.`;
    }
    return "Decode only: prefill was not sized, so this is not a fleet recommendation.";
  }
  switch (v.verdict) {
  case "UNDER-PROVISIONED": {
    const n = v.needed;
    const need = !n || n.unmet ? "" : n.decodeOnly ? `; it needs at least ${n.decode} decode (prefill was not sized)` : `; it needs ${execFleet(fleet, n.prefill, n.decode)}`;
    return `Too small: the busiest load is past the fleet's safe load, or misses a target${need}.`;
  }
  case "OVER-PROVISIONED": return `A replica to spare: one ${(v.shrink || []).join(" or one ")} fewer still meets every target at the busiest load.`;
  case "RIGHT-SIZED": return "Right-sized: every target is met at the busiest load, and one replica fewer would miss one.";
  }
  if (/lower bound/.test(v.summary || "")) {
    return "Not judged: the busiest load is past prefill's limit, known only as a lower bound, so whether the fleet saturates is not known. " +
      "Decode holds its safe load.";
  }
  return "Not judged at the busiest load: " + plainVerdict(v.summary || v.reason || "see Right-size") + ".";
}

// PLAIN_WORDS replace the planner's terms in a sentence the first screen
// shows: symbols and flags stay in tooltips and the method.
const PLAIN_WORDS = [[/;?\s*--assume-vp states a V_P to price it/g, ""], [/--assume-[a-z-]+/g, "an assumed term"],
  [/\bV_P\b/g, "prefill speed per pod"], [/prefill knee/g, "prefill limit"], [/decode knee/g, "decode limit"],
  [/fleet knee/g, "fleet limit"], [/\bknee\b/g, "limit"], [/operating point/g, "safe load"], [/\bpriced\b/g, "estimated"],
  [/\brefused\b/g, "not known"], [/flow-control onset/g, "point where the router starts queuing"]];

// plainText is a line of the planner's in plain words (PLAIN_WORDS and
// PLAIN_MORE), whole.
function plainText(s) {
  let out = String(s);
  for (const [re, w] of PLAIN_WORDS.concat(PLAIN_MORE)) out = out.replace(re, w);
  return out;
}

// PLAIN_MORE are the right-size card's own terms: the validated region and
// the runs it was measured on, named in the docs.
const PLAIN_MORE = [[/OUTSIDE VALIDATED REGION/g, "outside the conditions the model was checked on"],
  [/the q32 staircase/g, "a 32B benchmark run"], [/the 0\.6B goldens/g, "the small-model benchmark runs"], [/Ca\^2/g, "arrival burstiness"]];

// plainVerdict is a verdict's summary in plain words: no verdict word ahead
// of it, the planner's terms replaced (PLAIN_WORDS), no trailing stop.
function plainVerdict(s) {
  let out = String(s).replace(/^[A-Z-]+( [A-Z]+)?:\s*/, "");
  for (const [re, w] of PLAIN_WORDS) out = out.replace(re, w);
  return out.replace(/\.\s*$/, "");
}

// execCurveAt is a curve's figure at rate x, read between its two nearest
// points with a figure (linear), or null outside them or where refused.
function execCurveAt(curve, x) {
  const pts = (curve || []).map(c => ({x: c.rate, y: figValue(c.figure)})).filter(p => p.y != null);
  for (let i = 1; i < pts.length; i++) {
    const a = pts[i - 1], b = pts[i];
    if (x >= a.x && x <= b.x) return b.x === a.x ? a.y : a.y + (b.y - a.y) * (x - a.x) / (b.x - a.x);
  }
  return null;
}

// execPods is the fleet's pods and GPUs in words: "16 pods, 512 GPUs (32 per
// pod)", or each role's GPUs where they differ.
function execPods(fleet, np, nd) {
  const gp = fleet.prefill ? fleet.prefill.gpusPerReplica : 0, gd = fleet.decode.gpusPerReplica;
  const g = execGPUs(fleet, np, nd);
  const pods = np + nd;
  if (g == null) return `${pods} pods`;
  const per = !fleet.prefill || gp === gd ? `${gd} per pod` : `${gp} a prefill pod, ${gd} a decode pod`;
  return `${pods} pods, ${g} GPUs (${per})`;
}

// execBusiest is the one busiest load every line of the panel reads: the
// load the report judged at, its statistic named; with the peak beside it as
// small text.
function execBusiest(load) {
  if (!load || !load.rate || load.rate.value == null) return null;
  const stat = load.statistic === "p95-modulation" ? "p95 less counting noise" : load.statistic || "p95";
  return {rate: load.rate.value, words: `busiest load (${stat} of 1-minute cycles): ${roundRate(load.rate.value)} r/s`,
    small: load.peak != null ? `peak ${roundRate(load.peak)} r/s` : ""};
}

// prefillAtLeast is prefill's limit as the page shows it, {value, lb,
// raised}: the plan's knee, but a lower bound under the busiest load the
// fleet carried (load, the report's) is raised to that load. The fleet
// served it, so prefill's limit is at least it, as the planner floors a
// lower-bound V_P at what the pods served (prefill.floorAtServed); the knee
// from V_P can sit under it because V_P is a sustained per-pod run, priced
// at the mix's prompt. The load says nothing of fewer prefill replicas than
// carried it: with n, the plan's prefill replicas, under carriedBy, the
// report fleet's, the knee stands as priced.
function prefillAtLeast(pk, load, n, carriedBy) {
  const v = figValue(pk);
  const lb = !!pk && (pk.flags || []).some(x => x === "VPLowerBound" || x === "KneeAtServedWork");
  const c = load && load.rate ? load.rate.value : null;
  const fewer = n != null && carriedBy != null && n < carriedBy;
  if (v != null && lb && !fewer && c != null && c > v) return {value: c, lb, raised: true};
  return {value: v, lb, raised: false};
}

// prefillAtLeastOf is prefillAtLeast for a plan and the report beside it.
function prefillAtLeastOf(plan, report) {
  const m = plan && plan.ttft && plan.ttft.markers;
  const n = plan && plan.scenario && plan.scenario.fleet ? plan.scenario.fleet.prefill : null;
  const carriedBy = report && report.fleet ? report.fleet.prefill : null;
  return prefillAtLeast(m && m.prefillKnee, report && report.load, n, carriedBy);
}

// limitChip is the chip a role's limit bar carries: "binds" where the
// fleet's limit is that role's and the role's limit was measured; "limit
// not measured" on a prefill limit known only as a lower bound (prefillLB),
// which nothing shows binds first; "" otherwise.
function limitChip(role, fig, binding, prefillLB) {
  if (role === "prefill" && prefillLB) return "limit not measured";
  return (binding || []).includes(role) && figValue(fig) != null ? "binds" : "";
}

// execView is the panel: {lines: [{label, text, badge, small, actions, more}],
// slo, trust}. It leads with what is known, each role's capacity against one
// busiest load, and never heads a line "not known": a limit seen only as a
// lower bound reads "at least". The headroom is read off the plan on screen,
// at the shape the charts price, which the panel names.
function execView(o) {
  const {inputs, plan, report, bt, k} = o;
  const f = inputs.fleet;
  const np = f.prefill ? f.prefill.replicas : 0, nd = f.decode.replicas;
  const lines = [];
  const busy = execBusiest(report && report.load);
  const seen = busy ? busy.rate : null;
  // The range the model was tested over: the server's held-out check at
  // load, else the page's own check.
  const held = inputs.ttftModel && inputs.ttftModel.heldOut;
  const judged = bt ? (bt.steps || []).filter(s => !s.excluded) : [];
  const testedTo = held && held.rateTo != null ? held.rateTo : judged.length ? Math.max(...judged.map(s => s.rate)) : null;
  lines.push({label: "Fleet today", badge: EXEC_BADGES.measured,
    text: `${f.prefill ? `${np} prefill + ${nd} decode` : `${nd} replicas`}: ${execPods(f, np, nd)}.` + (busy ? ` The ${busy.words}.` : ""),
    small: busy ? busy.small : ""});

  // Each role's capacity at the plan's shape, against the busiest load.
  const L = plan && plan.limits;
  const m = plan && plan.ttft && plan.ttft.markers;
  const dl = L && L.decode ? figValue(L.decode) : null;
  const ref = refusalView(plan);
  const pk = m && m.prefillKnee ? m.prefillKnee : null;
  const ps = prefillAtLeastOf(plan, report);
  const pkv = ps.value, pkLB = ps.lb;
  // A report being asked again was for other targets: its verdict is not
  // shown, and the row says it is updating.
  const shape = !o.reportPending && report && (report.shapes || [])[0];
  const rejudge = o.reportPending ? " Judging the fleet at these targets..." :
    o.reportError ? ` The fleet was not judged at these targets (${o.reportError}).` : "";

  // The verdict: the constraint, in plain words, with the right size folded
  // in where it was judged.
  if (pkv != null && dl != null) {
    // A prefill limit seen only as a lower bound is never called the
    // constraint: nothing measured says it binds first.
    const prefillFirst = pkv < dl;
    const room = !seen ? "" : dl >= seen ? ` Decode has ${execTimes1(dl / seen)} headroom.` : ` Decode carries ${execTimes1(dl / seen)} the busiest load.`;
    let text = pkLB ? "Prefill's limit is not measured yet: the pods carried the busiest load without queuing, so it is at least that." + room :
      prefillFirst ? "Prefill is the constraint: it reaches its limit before decode does." : "Decode is the constraint: it reaches its limit before prefill does.";
    const v = shape && shape.verdict;
    if (v && v.verdict && v.verdict !== "NOT JUDGED") text += " " + execVerdict(f, v);
    else if (v && v.scope === "decode only") text += " " + execVerdict(f, v);
    else text += rejudge;
    lines.push({label: "Verdict", badge: "", text, updating: !!o.reportPending});
  } else if (shape) {
    lines.push({label: "Verdict", badge: "", text: execVerdict(f, shape.verdict)});
  } else if (o.reportPending) {
    lines.push({label: "Verdict", badge: "", text: "Judging the fleet at the busiest load...", updating: true});
  } else if (o.reportError) {
    lines.push({label: "Verdict", badge: "", text: rejudge.trim()});
  }
  if (dl != null) {
    lines.push({label: "Decode", badge: "",
      text: `${nd} replicas carry ${roundRate(dl)} r/s at the TPOT target` + (seen ? `, ${execTimes1(dl / seen)} the busiest load.` : ".")});
  }
  if (f.prefill && (pkv != null || ref)) {
    const share = pkv != null && seen ? `${execTimes1(pkv / seen)} the busiest load` : "";
    const why = ps.raised ? [share, "carried it without queuing"].filter(x => x).join("; ") : ["a lower bound", share].filter(x => x).join(", ");
    const text = ref && pkv != null && pkLB ? `Not measured yet: at least ${roundRate(pkv)} r/s (${why}).` +
        (ref.vp != null ? ` Each pod computed at least ${Math.round(ref.vp).toLocaleString("en-US")} tokens/s.` : "") :
      ref ? "Speed not yet observed: the source learned no prefill speed per pod." :
      `${np} replicas carry ${pkLB ? "at least " : ""}${roundRate(pkv)} r/s before latency takes off${share ? ", " + share : ""}.`;
    const actions = ref ? (ref.vp != null ? [{act: "usevp", vp: ref.vp, label: `Use ${Math.round(ref.vp).toLocaleString("en-US")} tokens/s as the limit (conservative)`}] : [])
      .concat([{act: "busier", label: "Load a busier window"}]) : [];
    lines.push({label: "Prefill", badge: "", text, actions});
  }

  // TTFT and TPOT: what users saw, measured, first; then the prediction,
  // marked extrapolated where it lies past the tested range.
  const meas = execMeasuredHour(inputs.measured);
  let pred = "";
  if (plan && plan.ttft && seen != null) {
    const t = plan.ttft, at = execCurveAt(t.line.curve, seen), floor = figValue(t.line.floor);
    const past = testedTo != null && seen > testedTo;
    const dag = t.model && t.model.priced ? " " + DAGGER : "";
    if (at != null) {
      pred = `Predicted at ${roundRate(seen)} r/s` + (past ? ` (above the tested ${roundRate(testedTo)} r/s, extrapolated)` : "") + `${dag}: TTFT ${t.statistic} about ${execTimes(at)}`;
      const atTop = past ? execCurveAt(t.line.curve, testedTo) : null;
      if (atTop != null) pred += `; at the top of the tested range, ${roundRate(testedTo)} r/s, about ${execTimes(atTop)}`;
      pred += ".";
      if (floor != null && floor > t.line.target) {
        pred += ` The ${execTimes(t.line.target)} target is below this model's floor (${execTimes(floor)}) at any load` +
          (k.ttftDefault ? ": set a realistic target under SLO." : ".");
      }
    }
  }
  // One line on the panel: what users saw at the SLO's statistics, and the
  // target sentence; the prediction and the bucket ranges in the row's drawer.
  const target = pred.includes(" target is below this model's floor") ? pred.slice(pred.indexOf(" The ") + 1) : "";
  const predOnly = target ? pred.slice(0, pred.indexOf(" The ")) : pred;
  const short = meas ? execSawShort(inputs.measured, k) : "";
  if (short || pred) {
    lines.push({label: "TTFT / TPOT", badge: short ? EXEC_BADGES.measured : "",
      text: [short, short ? target : pred].filter(x => x).join(" "),
      more: short ? [meas.text, predOnly, meas.note].filter(x => x) : []});
  }

  // Growth: the fleet the load grown by a quarter and by a half needs, as
  // the server sized it (o.growth: [{x, rate, need}], need in words, from
  // growthNeed); or why it is not sized (o.growthMissing, in words).
  const growth = o.growth || [];
  // One refusal for both rates is said once.
  const pct = g => `+${Math.round((g.x - 1) * 100)}% (${roundRate(g.rate)} r/s)`;
  const same = growth.length > 1 && growth.every(g => g.need === growth[0].need) && /^(not sized|no )/.test(growth[0].need);
  // Sizes being asked again were for other targets: the row says it is
  // sizing, never shows them.
  const grownText = o.growthPending ? "Sizing the fleet for +25% and +50% of the busiest load..." :
    same ? `${growth.map(pct).join(" and ")}: ${growth[0].need}.` :
    growth.length ? growth.map(g => `${pct(g)}: ${g.need}${growthPast(g, testedTo)}`).join("; ") + ", at the same SLO." :
    o.growthMissing || "";
  if (grownText) lines.push({label: "+25% / +50% traffic", badge: "", text: grownText, updating: !!o.growthPending});

  // Accuracy: the page's own check where it ran, the one score the check
  // view shows, at the SLO's statistic; else the server's held-out mean.
  // The page's check runs only when asked: until then the row says it has
  // not run and offers it, never a score it does not have.
  let trust = "";
  const ask = f.prefill && !o.btPending ? [{act: "check", label: EXEC_CHECK_LABEL}] : [];
  if (bt) {
    const a = execAccuracy(bt, k);
    lines.push({label: "Accuracy", text: a.text + (a.range ? " " + a.range.replace("Valid range", "Tested range") : ""), badge: EXEC_BADGES.checked});
    trust = a.chip ? `Checked on your traffic: ${a.chip}` : "";
  } else if (held && held.steps > 0) {
    const n = held.within || 0, st = held.statistic || "mean";
    const r = held.medianRatio;
    const lean = r == null ? "" : r < 0.97 ? `, reads ${Math.round((1 - r) * 100)}% low` : r > 1.03 ? `, reads ${Math.round((r - 1) * 100)}% high` : "";
    const fleet = held.prefill != null ? `${held.prefill} prefill + ${held.decode} decode` : "the fleet it ran";
    const range = held.rateFrom != null ? ` Tested range: ${fleet} at ${roundRate(held.rateFrom)}-${roundRate(held.rateTo)} r/s; outside that is an extrapolation.` : "";
    // The server's quick check is not the page's: its folds refit at 50%
    // and 75% of the window and score their own steps. Each is named.
    const other = k.ttftStat === st ? "" : o.btPending ? ` The full check, at TTFT ${k.ttftStat}, is running.` : " " + fullCheckNotRun(k.ttftStat);
    lines.push({label: "Accuracy", badge: EXEC_BADGES.checked,
      text: `Quick check on held-out steps of your traffic: TTFT ${st} within 20% in ${n} of ${held.steps} five-minute steps${lean}.${other}${range}`,
      actions: ask});
    trust = `Quick check on your traffic: TTFT ${st} ${n}/${held.steps} within 20%`;
  } else if (o.btPending) {
    lines.push({label: "Accuracy", text: "Checking the planner on your held-out hours...", badge: EXEC_BADGES.checked});
  } else if (inputs.selfCheck && inputs.selfCheck.line) {
    lines.push({label: "Accuracy", text: inputs.selfCheck.line, badge: EXEC_BADGES.checked, actions: ask});
    if (f.prefill) trust = "Self-check only: full accuracy check not run yet";
  } else if (f.prefill) {
    // No badge: nothing was checked.
    lines.push({label: "Accuracy", badge: "", text: EXEC_NOT_CHECKED, actions: ask});
    trust = "Accuracy not checked yet";
  }
  return {lines, slo: execSLO(k), trust, note: "Rows are estimates unless marked."};
}

// EXEC_CHECK_LABEL is the button that runs the page's check on the source.
const EXEC_CHECK_LABEL = "Check accuracy on this recording";

// EXEC_NOT_CHECKED is the Accuracy row before any check ran.
const EXEC_NOT_CHECKED = "Not checked yet: the planner has not been checked on this recording. The check takes seconds to a minute and runs only when asked.";

// fullCheckNotRun says the page's full check, at the SLO's TTFT statistic,
// has not run.
function fullCheckNotRun(stat) {
  return `The full check, at TTFT ${stat}, has not been run yet: ${EXEC_CHECK_LABEL} runs it.`;
}

// execSawShort is what users saw in one sentence, at the SLO's statistics:
// "Users saw TTFT p90 5.03 s, TPOT mean 16.5 ms (busiest hour)." A figure
// the histogram cannot place reads as its bucket; "" with neither.
function execSawShort(mh, k) {
  const one = (list, st) => {
    const s = (list || []).find(x => x.statistic === st);
    return !s ? null : s.value != null ? execTimes(s.value) : s.bucket ? `${execTimes(s.bucket.lo)}-${execTimes(s.bucket.hi)}` : null;
  };
  const t = one(mh.ttft, k.ttftStat), p = one(mh.tpot, k.tpotStat);
  const parts = [t ? `TTFT ${k.ttftStat} ${t}` : "", p ? `TPOT ${k.tpotStat} ${p}` : ""].filter(x => x);
  return parts.length ? `Users saw ${parts.join(", ")} (busiest hour).` : "";
}

// growthNeed is a size (POST /v1/size's response) in the panel's words: "N
// prefill + M decode (G GPUs)", marked where the size rests on an
// extrapolated limit or a prefill limit known only as a lower bound, or why
// no fleet was sized. error is the request's failure, if any.
function growthNeed(size, error) {
  if (error || !size) return "not sized (the request failed)";
  const a = size.answer;
  if (!a) {
    const u = size.unmet || {}, r = u.refused;
    if (r && r.reason === "NoPrefillCapacity") return "not sized: prefill's limit is not measured";
    if (r && r.reason === "TargetBelowFloor") return "not sized: the TTFT target is below this model's floor at any fleet";
    const n = size.maxReplicas ? Math.max(size.maxReplicas.prefill || 0, size.maxReplicas.decode || 0) : 0;
    const who = u.role && u.role !== "fleet" ? `no ${u.role} count` : "no fleet";
    return `${who}${n ? ` up to ${n} replicas a role` : ""} serves it` + (r && r.reason !== "ReplicaBoundReached" ? ` (${plainReasonWord(r)})` : "");
  }
  const f = a.fleet;
  let s = f.prefill ? `${f.prefill} prefill + ${f.decode} decode` : `${f.decode} replicas`;
  if (a.gpus != null) s += ` (${a.gpus} GPUs)`;
  const lim = a.limit || {};
  const notes = [];
  // A size whose limit is only a lower bound may need fewer prefill: the
  // true limit is higher. One past the highest load measured is extrapolated.
  // A V_P the user stated (AssumedVP) is priced as given, as the server's
  // TTFTUpperBound reads it.
  if ((lim.flags || []).includes("VPLowerBound") && !(lim.flags || []).includes("AssumedVP")) notes.push("prefill's limit is a lower bound, so fewer prefill may do");
  if (lim.extrapolated) notes.push("extrapolated past the tested range");
  return s + (notes.length ? `, ${notes.join("; ")}` : "");
}

// growthPast marks a grown rate above the range the model was tested over,
// as the TTFT row does, where the size does not already say extrapolated.
function growthPast(g, testedTo) {
  if (testedTo == null || !(g.rate > testedTo) || /extrapolated/.test(g.need) || /^(not sized|no )/.test(g.need)) return "";
  return `, extrapolated above the tested ${roundRate(testedTo)} r/s`;
}

// GROWTH_NO_PREFILL says why the panel sizes no growth: a prefill limit the
// default model cannot price, with no experimental model to price it.
const GROWTH_NO_PREFILL = "Not sized: prefill's limit is not measured, so no fleet can be sized for more traffic. " +
  "Load a busier window, or use the speed the pods were seen at (the Prefill row).";

// GROWTH_NO_PD says why the panel sizes no growth for a decode-only source:
// POST /v1/size sizes a P/D fleet.
const GROWTH_NO_PD = "Not sized: the page sizes a fleet with prefill and decode, and this source has no prefill variant.";

// execTimes1 is a ratio to the busiest load: two decimals under 1, so 0.98
// never reads as 1.0.
function execTimes1(x) {
  return (x < 1 ? x.toFixed(2) : x.toFixed(1)) + "x";
}

// execMeasuredHour is what users saw in the source's busiest hour, as the
// router measured it (inputs.measured): each statistic placed, or as its
// bucket where the bucket is too wide to place it; and one note on the
// percentiles not given and why.
function execMeasuredHour(mh) {
  if (!mh) return null;
  const word = s => s.value != null ? execTimes(s.value) : s.bucket ? `${execTimes(s.bucket.lo)}-${execTimes(s.bucket.hi)} (one histogram bucket)` : null;
  const row = list => (list || []).map(s => [s.statistic, word(s)]).filter(([, v]) => v).map(([st, v]) => `${st} ${v}`).join(", ");
  const why = (name, list) => (list || []).filter(s => s.value == null && !s.bucket).map(s => `${name} ${s.statistic}: ${s.why}`);
  const t = row(mh.ttft), p = row(mh.tpot);
  if (!t && !p) return null;
  const at = String(mh.from).slice(11, 16) + "-" + String(mh.to).slice(11, 16);
  const coarse = (mh.tpot || []).filter(s => s.bucket).map(s => s.statistic);
  const notes = why("TTFT", mh.ttft).concat(why("TPOT", mh.tpot));
  if (coarse.length) {
    const b = mh.tpot.find(s => s.bucket).bucket;
    notes.push(`TPOT ${coarse.join("/")}: the router's ${execTimes(b.lo)}-${execTimes(b.hi)} bucket cannot resolve them`);
  }
  return {text: `Users saw (measured, busiest hour ${at} UTC, ${roundRate(mh.rate)} r/s): ` + [t ? `TTFT ${t}` : "", p ? `TPOT ${p}` : ""].filter(x => x).join("; ") + ".",
    note: notes.join("; ")};
}

// refusalView is the card shown when the source could not learn prefill's
// capacity, in plain words with what to do as page actions: {title, text, vp
// (tokens/s, to use as the limit, or null), code}; null where prefill priced.
// It reads a plan's limits, or a size's refusal where no fleet within the
// bound priced (unmet: {refused}).
function refusalView(plan, unmet) {
  const L = plan && plan.limits;
  const r = (L ? [L.prefill, L.fleet].map(x => x && x.refused) : []).concat([unmet && unmet.refused])
    .find(x => x && x.reason === "NoPrefillCapacity");
  if (!r) return null;
  const lb = (r.flags || []).includes("VPLowerBound");
  const m = /V_P ([0-9.]+) tokens\/s/.exec(r.detail || "");
  const vp = lb && m ? Number(m[1]) : null;
  const dv = L && L.decode && L.decode.value != null ? L.decode.value : null;
  const decode = dv != null ? ` Decode alone serves up to ${roundRate(dv)} r/s.` : "";
  const text = vp != null ? `The prefill pods never queued, so they handle at least ${Math.round(vp).toLocaleString("en-US")} tokens/s per pod, but their limit is not observed.${decode}` :
    `The source learned no prefill speed per pod: it saw no prefill pod busy enough.${decode}`;
  return {title: "Prefill capacity unknown", text, vp, code: r.reason};
}

// ---- Trust: one notice a view, a dagger on a figure ----
//
// How far to trust the figures is said once a view, never in every cell: a
// figure from an experimental model carries a dagger, and the view's notice
// says what the dagger means. Where each figure stands against the validated
// region is in the Flags drawer.

// EXPERIMENTAL_FLAGS are the flags of a figure an experimental model priced.
const EXPERIMENTAL_FLAGS = ["ExperimentalTTFT", "RankMixtureTTFT", "TPOTDistribution", "VPFromPlateau"];

// DAGGER marks a figure an experimental model priced.
const DAGGER = "†";

// isExperimental reports whether a figure, or a flag list, rests on an
// experimental model.
function isExperimental(x) {
  const flags = Array.isArray(x) ? x : ((x && x.flags) || []);
  return flags.some(f => EXPERIMENTAL_FLAGS.includes(typeof f === "string" ? f : f.flag));
}

// dagger is a figure's mark: DAGGER where an experimental model priced it.
function dagger(f) {
  return isExperimental(f) ? DAGGER : "";
}

// EXPERIMENTAL_NOTICE is the one notice of a view with experimental figures.
const EXPERIMENTAL_NOTICE = `Experimental model (checked on your data): figures marked ${DAGGER} come from it. ` +
  "Check the planner shows how it did on your traffic.";

// noticeText is a view's one notice: the experimental model's where a figure
// rests on one, else a TTFT model not validated on this fleet; "" with none.
function noticeText(flags, banner) {
  if (isExperimental(flags || [])) return EXPERIMENTAL_NOTICE;
  if (banner || (flags || []).some(f => (typeof f === "string" ? f : f.flag) === "UnvalidatedTTFT")) {
    return "The TTFT model is not validated on this fleet: read its TTFT figures as estimates. " +
      "About the TTFT model, under the source, says how it read elsewhere; Check the planner tests it on your traffic.";
  }
  return "";
}

// ---- Exports ----

// csvField is one CSV field: quoted, its quotes doubled, where it holds a
// comma, a quote or a line break; a leading =, +, -, @, tab or carriage
// return is prefixed with a quote so that a spreadsheet does not read the
// field as a formula.
function csvField(v) {
  let s = v == null ? "" : String(v);
  if (/^[=+@\t\r]/.test(s) || (/^-/.test(s) && !/^-[0-9.]+$/.test(s))) s = "'" + s;
  return /[",\n\r]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

// curvesCSV is the plan's curves as CSV: each rate with the TTFT and TPOT
// there, seconds, empty where not known, and the reason.
function curvesCSV(p) {
  const fig = f => (f && !f.refused && typeof f.value === "number" ? String(Number(f.value.toPrecision(6))) : "");
  const why = f => (f && f.refused ? f.refused.reason : "");
  const tpot = p.tpot ? p.tpot.curve : [];
  const rows = [["rate", `ttft_${p.ttft.statistic}_seconds`, "ttft_not_known", p.tpot ? `tpot_${p.tpot.statistic}_seconds` : "tpot_seconds", "tpot_not_known"].join(",")];
  p.ttft.line.curve.forEach((c, i) => {
    const t = tpot[i] && tpot[i].figure;
    rows.push([String(Number(c.rate.toPrecision(6))), fig(c.figure), why(c.figure), fig(t), why(t)].map(csvField).join(","));
  });
  return rows.join("\n") + "\n";
}

// reportCSV is a report's matrix as CSV, as the page shows it: a row a shape,
// a column a load, each cell its figures and its mark.
function reportCSV(v) {
  const rows = [["shape"].concat(v.header).map(csvField).join(",")];
  for (const r of v.rows) rows.push([r.label].concat(r.cells.map(c => (c.text + (c.word ? " " + c.word : "")).trim())).map(csvField).join(","));
  return rows.join("\n") + "\n";
}

// ---- Language: plain words on the first screen ----

// REASON_PLAIN is why a figure is not known, in a few plain words: the
// reason's code stays in the Flags drawer and the tooltips.
const REASON_PLAIN = {NoPrefillCapacity: "prefill capacity unknown", Saturated: "overloaded", AboveDecodeOnset: "the router queues",
  PastLowerBoundKnee: "past a lower-bound limit", TargetBelowFloor: "the target is under the floor", NoFixedLatency: "no prefill latency learned",
  NoTPOTHistogram: "no TPOT histogram", PinnedDefaultB: "decode not measured", NoLine: "decode not measured", NoTraffic: "no traffic",
  ReplicaBoundReached: "past the most replicas searched", TPOTBucketTooWide: "the TPOT histogram is too coarse"};

// plainReasonWord is a refusal in plain words.
function plainReasonWord(r) {
  return (r && REASON_PLAIN[r.reason]) || "not estimated";
}

// GLOSSARY is each symbol the planner's method uses and its plain words:
// the page says the words, and keeps the symbol for a tooltip.
const GLOSSARY = [["V_P", "prefill speed per pod (tokens/s)"], ["knee", "rate where latency takes off"],
  ["Ca²", "how bursty arrivals are"], ["f", "load imbalance across prefill pods"], ["cv", "prompt-length spread"],
  ["headroom", "safety margin"], ["operating point", "safe load: the margin times the limit"]];
