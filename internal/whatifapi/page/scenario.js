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
const LINK_WORDS = {mode: /^size$/, low: /^1$/, perBucket: /^1$/, ttftStat: STAT_WORD, tpotStat: STAT_WORD,
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
  if (s === "measured") return "";
  const m = REPORT_SHAPE.exec(typeof s === "string" ? s : "");
  if (!m) return "a shape is measured, or IL/OL[/hit] in tokens (8k/200, 1k/4k/0.3)";
  for (const t of [m[1], m[3]]) {
    const n = shapeTokens(t);
    if (!(n >= 1 && n <= MAX_SHAPE_TOKENS)) return "a length is 1 to 1024k tokens";
  }
  return "";
}

// shapeText writes one shape row's boxes as the report reads it: IL and OL
// in tokens, and the hit rate when one is typed. Blank boxes are "measured".
function shapeText(row) {
  const il = String(row.il || "").trim(), ol = String(row.ol || "").trim(), hit = String(row.hit || "").trim();
  if (!il && !ol && !hit) return "measured";
  return hit ? `${il}/${ol}/${hit}` : `${il}/${ol}`;
}

// reportShapes is the rows' shapes, each once, in order: a blank row is the
// measured shape, and the same shape typed twice is asked once.
function reportShapes(rows) {
  const out = [];
  for (const row of rows || []) {
    const s = shapeText(row);
    if (!out.includes(s)) out.push(s);
  }
  if (out.length === 0) out.push("measured");
  return out;
}

// parseReportLoads reads the loads box: comma separated, each 80%, 5r/s or
// seen. Empty is the server's default (50%, 80%, 100% and seen).
function parseReportLoads(text) {
  const loads = String(text || "").split(",").map(s => s.trim()).filter(s => s);
  if (loads.length > MAX_REPORT_LOADS) return {loads: [], problem: `at most ${MAX_REPORT_LOADS} loads`};
  for (const l of loads) {
    if (l === "seen" || /^seen *[x×*] *[0-9]+(\.[0-9]+)?$/.test(l)) continue;
    const pct = /^([0-9]+(\.[0-9]+)?)%$/.exec(l), rate = /^([0-9]+(\.[0-9]+)?)r\/s$/.exec(l);
    if (pct && Number(pct[1]) > 0 && Number(pct[1]) <= 100) continue;
    if (rate && Number(rate[1]) > 0) continue;
    return {loads: [], problem: `${l}: a load is a share of the knee (80%), a rate (5r/s) or seen`};
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
  const body = {inputsId: o.inputsId, shapes};
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
// it without colour.
const REPORT_MARKS = {met: ["met", "ok"], missed: ["missed", "MISS"], "missed at onset": ["missed", "MISS at onset"], saturated: ["saturated", "SAT"],
  unknown: ["unknown", "?"], "no target": ["notarget", "no SLO"]};

// REFUSAL_WORDS are the refusals a report shows most, in words.
const REFUSAL_WORDS = {
  Saturated: "saturated: no steady state at this load",
  AboveDecodeOnset: "past the decode flow-control onset: not priced",
  NoPrefillCapacity: "no prefill speed learned (assume V_P to price it)",
  NoFixedLatency: "no prefill fixed latency learned (assume a to price it)",
  NoTPOTHistogram: "no TPOT histogram to read a percentile from",
  TPOTBucketTooWide: "the TPOT histogram is too coarse for a percentile",
  PinnedDefaultB: "the decode line rests on a default, not a measurement (type ITL B under Assume a term to price it)",
  NoLine: "no decode line learned",
  NoTraffic: "no traffic to judge",
};

// refusalText is a refused figure in words.
function refusalText(f) {
  const reason = f && f.refused ? f.refused.reason : "";
  return REFUSAL_WORDS[reason] || (reason ? `not priced (${reason})` : "not priced");
}

// REPORT_FLAG_WORDS are the flags a report carries most, in a short phrase.
// The rest are counted, and their codes kept for a tooltip.
const REPORT_FLAG_WORDS = {
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
// TPOT (tpot true) read at the decode line's floor is marked as at most that.
function msText(f, tpot) {
  // A figure read at the band's bottom is the line's floor: the latency is at most it.
  if (f && f.value != null) return (tpot && isFloor(f) ? "≤" : "") + roundMs(f.value) + " ms";
  const reason = f && f.refused ? f.refused.reason : "";
  return reason === "Saturated" ? "saturated" : reason === "AboveDecodeOnset" ? "router-queued" : "-";
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
      return {text: parts.join(" · ").replace(" · @", " @"), parts, cls: m[0], word: m[1]};
    }),
  }));
  const verdicts = shapes.map(s => {
    const v = verdictLine(s.verdict, verdictText(s, load), r.fleet);
    // Growth is its own line beside the verdict, never in its place.
    const growth = (s.growth || []).map(g => Object.assign(verdictLine(g.verdict, "", r.fleet),
      {lead: `with ${g.x}× traffic (${roundRate(g.rate)} r/s)`, rate: g.rate}));
    return Object.assign(v, {label: s.label, extrapolated: !!s.extrapolated, whatIf: s.source === "asked",
      decodeOnly: (s.verdict || {}).scope === "decode only", growth});
  });
  const details = shapes.map(s => {
    const c = s.capacity || {}, u = s.utilization || {decode: {}, prefill: {}};
    const rate = f => (f && f.value != null ? roundRate(f.value) + " r/s" : refusalText(f));
    const share = f => (f && f.value != null ? f.value.toFixed(2) : refusalText(f));
    return {
      summary: `${s.label}: IL ${Math.round(s.inputTokens)}, OL ${Math.round(s.outputTokens)}, prefix hit ${Number(s.prefixHitRate).toFixed(2)} (${s.source})`,
      lines: [
        s.extrapolated ? `Extrapolated: ${s.extrapolatedWhy}.` : "",
        `Knees: prefill ${rate(c.prefillKnee)}, decode ${rate(c.decodeKnee)}; the fleet's is ${rate(c.fleetKnee)}, set by ${String((c.fleetKnee || {}).binds || "")}. ` +
          `Operating point ${rate(c.operating)}; the most that meets every target ${rate(c.maxAtSLO)}.`,
        `At the load: ${decodeUse(u.decode)}; ${prefillUse(u.prefill)}.`,
        ...roleLines(s.verdict),
        `The numbers: ${plainReason((s.verdict || {}).reason || "")}`,
      ].filter(x => x),
      flags: flagSummary(s.flags),
    };
  });
  return {verdicts, header, rows, details, notes: r.notValidated || [], load: loadLine(r.load),
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
  if (!d || !d.kvFill || d.kvFill.value == null) return "decode " + refusalText(d && d.kvFill);
  const wait = d.waitProbability && d.waitProbability.value != null ? `, ${pct(d.waitProbability.value)} of requests wait for a slot` : "";
  const slots = d.slotsOccupied && d.slotsOccupied.value != null ? `, ${Math.round(d.slotsOccupied.value)} of ${d.slots} batch slots busy` : "";
  return `decode KV ${pct(d.kvFill.value)} full (of ${pct(d.bandTop)} usable)${slots}${wait}`;
}

// prefillUse is prefill's utilization at the load, in words.
function prefillUse(p) {
  if (!p || !p.pooled || p.pooled.value == null) return "prefill " + refusalText(p && p.pooled);
  return `prefill ${pct(p.pooled.value)} busy pooled, ${pct(p.worstPod.value)} on the busiest pod`;
}

// REASON_WORDS replace the refusal codes in a verdict's reason.
const REASON_WORDS = {AboveDecodeOnset: "router-queued", NoPrefillCapacity: "unpriced (no prefill speed)",
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
  const needs = neededText(v && v.needed, has);
  // The fleet needed is shown as its own chip, so the sentence does not
  // repeat it.
  if (needs) rest = rest.replace(NEEDS_CLAUSE, "").replace(/;\s*\(/, " (").replace(/[;,]\s*\./, ".");
  return {verdict: word, text, rest, needs, unmet: !!(v && v.needed && v.needed.unmet)};
}

// NEEDS_CLAUSE is the "needs P prefill + D decode (has p + d), or a + b"
// clause of a summary.
const NEEDS_CLAUSE = /;?\s*needs [^;()]*\(has \d+ \+ \d+\)(,? or \d+ \+ \d+)*/;

// neededText is the fleet a verdict needs, in words: "needs 3 prefill +
// 4 decode (has 2 + 2)", "or 4 + 3" for each alternative, "at least" where
// the prefill side is not priced, or the bound no fleet within serves.
function neededText(n, has) {
  if (!n) return "";
  if (n.unmet) return `no fleet up to ${n.maxReplicas} replicas serves this (${n.unmet} does not reach the load)`;
  const now = has ? ` (has ${has.prefill} + ${has.decode})` : "";
  if (n.decodeOnly) return `needs at least ${n.decode} decode${has ? ` (has ${has.decode})` : ""}; prefill is not priced`;
  const alt = (n.alternatives || []).map(a => `or ${a.prefill} + ${a.decode}`).join(", ");
  return `needs ${n.prefill} prefill + ${n.decode} decode${now}` + (alt ? ", " + alt : "");
}

// roleLines is a verdict's per-role judgment: a line per role, whether it has
// a replica to spare, and why.
function roleLines(v) {
  return ((v && v.roles) || []).map(r => `${r.role[0].toUpperCase() + r.role.slice(1)}: ${r.spare ? "has a replica to spare" : "no replica to spare"}` +
    (r.detail ? ` (${r.detail})` : "") + ".");
}

// LOAD_STATISTICS are the load statistics a report judges at.
const LOAD_STATISTICS = ["p95", "mean", "peak"];

// loadLine is the load the source saw, in words: the statistic judged at,
// and the mean, p95 and peak beside it.
function loadLine(l) {
  if (!l || !l.rate || l.rate.value == null) return "No load recorded: " + refusalText(l && l.rate) + ".";
  const at = l.statistic === "stated" ? `the stated load of ${roundRate(l.rate.value)} r/s`
    : `the ${l.statistic || "observed"} load of ${roundRate(l.rate.value)} r/s`;
  const seen = [["mean", l.mean], ["p95", l.p95], ["peak", l.peak]].filter(([, x]) => x != null).map(([k, x]) => `${k} ${roundRate(x)}`);
  // Under 20 busy cycles the p95 is the peak: too few to tell them apart.
  const peakNote = l.p95IsPeak && l.statistic === "p95" ? " (= peak: fewer than 20 cycles)" : "";
  return `Judged at ${at}${peakNote}` + (seen.length ? `; the source saw ${seen.join(", ")} r/s over ${l.cyclesWithTraffic} busy cycles.` : ".");
}
