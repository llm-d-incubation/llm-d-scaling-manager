// Runs the capacity planner page's day plan while it runs (internal/whatifapi/
// page/planner.js: runDay, dayProgress, dayCancel, dayStepFor, showView)
// against a fake DOM and a fake server that answers a day plan after a delay
// and stops when its request is aborted. The Day plan view says a plan runs,
// how many buckets, since when and for how long, with Cancel; Cancel aborts
// the request and frees the button; no second plan starts while one runs,
// from the button or the view opening; a cancelled plan is not asked again
// by itself; the bucket starts where the window cuts into 300 or fewer.
// Called by hack/check-planner-scenarios.sh.
//
// Usage: node hack/check-planner-dayprogress.mjs [scenario.js dayplan.js backtest.js planner.js day-plan.json]
import fs from "node:fs";
import vm from "node:vm";

const files = [
  process.argv[2] || "internal/whatifapi/page/scenario.js",
  process.argv[3] || "internal/whatifapi/page/dayplan.js",
  process.argv[4] || "internal/whatifapi/page/backtest.js",
  process.argv[5] || "internal/whatifapi/page/planner.js",
];
const golden = JSON.parse(fs.readFileSync(process.argv[6] || "cmd/capacity-planner/testdata/day-plan-ols-sweep-json.golden", "utf8"));

// fakeElement is as much of a DOM element as the day plan touches.
function fakeElement(tag) {
  return {
    tagName: tag, textContent: "", innerHTML: "", className: "", hidden: false, value: "", disabled: false, title: "", type: "",
    style: {}, dataset: {}, children: [], onclick: null, checked: false, open: false,
    classList: {
      set: new Set(),
      add(c) { this.set.add(c); }, remove(c) { this.set.delete(c); },
      toggle(c, on) { if (on === undefined ? !this.set.has(c) : on) this.set.add(c); else this.set.delete(c); },
      contains(c) { return this.set.has(c); },
    },
    appendChild(c) { this.children.push(c); return c; },
    replaceChildren(...cs) { this.children = cs; },
    querySelector() { return fakeElement("div"); },
    querySelectorAll() { return []; },
    addEventListener() {},
    setAttribute() {},
    focus() { document.activeElement = this; },
  };
}
const byId = new Map();
const document = {
  getElementById(id) { if (!byId.has(id)) byId.set(id, fakeElement("div")); return byId.get(id); },
  createElement: fakeElement,
  createTextNode: text => Object.assign(fakeElement("#text"), {textContent: text}),
  addEventListener() {},
  querySelector() { return fakeElement("div"); },
};

// The fake server answers a day plan after server.delay ms, or stops when
// its request is aborted.
const calls = [];
const server = {delay: 300, bodyDelay: 0, busy: 0};
// respond is a response whose body reads after server.bodyDelay ms, or
// fails when the request is aborted first.
function respond(status, doc, signal, retryAfter) {
  return {ok: status < 300, status, statusText: "x", headers: {get: h => h === "Retry-After" ? retryAfter || null : null},
    json: () => new Promise((resolve, reject) => {
      if (!server.bodyDelay) return resolve(doc);
      const t = setTimeout(() => resolve(doc), server.bodyDelay);
      if (signal) signal.addEventListener("abort", () => { clearTimeout(t); reject(new DOMException("aborted", "AbortError")); });
    })};
}
async function fakeFetch(path, opt) {
  const call = {path, body: JSON.parse(opt.body || "null"), aborted: false, done: false};
  calls.push(call);
  await new Promise((resolve, reject) => {
    const t = setTimeout(resolve, server.delay);
    if (opt.signal) {
      opt.signal.addEventListener("abort", () => {
        clearTimeout(t);
        call.aborted = true;
        reject(new DOMException("aborted", "AbortError"));
      });
    }
  });
  call.done = true;
  if (path !== "v1/dayplan") return respond(404, {error: "no " + path});
  if (server.busy) return respond(503, {error: "busy"}, opt.signal, "30");
  const d = JSON.parse(JSON.stringify(golden));
  d.ttft = Object.assign({}, d.ttft, {statistic: call.body.slo.ttft.statistic, target: call.body.slo.ttft.target});
  return respond(200, d, opt.signal);
}

const Option = function (text, value) { return Object.assign(fakeElement("option"), {textContent: text, value}); };
const ctx = vm.createContext({document, Option, location: {hash: ""}, history: {replaceState() {}}, fetch: fakeFetch, AbortController, DOMException,
  setTimeout, clearTimeout, setInterval, clearInterval, URLSearchParams, performance, console});
for (const f of files) vm.runInContext(fs.readFileSync(f, "utf8"), ctx, {filename: f});
const run = src => vm.runInContext(src, ctx);
run("plan = async () => {};");
const state = run("state");
const el = id => document.getElementById(id);
const setStatus = text => run("setStatus")(text);

let passed = 0;
const failures = [];
function check(name, ok, detail) {
  if (ok) passed++;
  else failures.push(name + (detail === undefined ? "" : ": " + detail));
}
const until = async (cond, ms = 5000) => {
  const t0 = Date.now();
  while (!cond()) {
    if (Date.now() - t0 > ms) return false;
    await new Promise(r => setTimeout(r, 10));
  }
  return true;
};
const sleep = ms => new Promise(r => setTimeout(r, ms));
const dayCalls = () => calls.filter(c => c.path === "v1/dayplan");

// A source of a day: 24 hours, first to last cycle.
const day = {firstAt: "2026-11-01T00:00:00Z", lastAt: "2026-11-02T00:00:00Z"};
const inputs = {inputsId: "sha256:x", model: "m",
  fleet: {prefill: {variant: "p", replicas: 2, gpusPerReplica: 8}, decode: {variant: "d", replicas: 2, gpusPerReplica: 8}},
  source: Object.assign({kind: "recording", name: "r"}, day), terms: [], observed: {buckets: [], outputTokens: {value: 300}}};

try {
  run("wire()");
  state.inputs = inputs;
  state.mode = "plan";
  state.k = {ttftStat: "p90", ttftTarget: 2, tpotStat: "mean", tpotTarget: 0.05, ttftDefault: false, tpotDefault: false,
    headroom: 0.85, assume: {}, edges: "", perBucket: false, maxRep: 32, rate: 5, prefill: 2, decode: 2, priceAs: "mix", outputTokens: 300};
  el("dayrun").disabled = false;
  el("view-day").hidden = true;

  // The bucket a window starts at: 300 buckets or fewer, 5m at the finest.
  const daySource = inputs.source;
  const stepOf = (from, to) => { state.inputs.source = Object.assign({}, daySource, {firstAt: from, lastAt: to}); return run("typeof dayStepFor === \"function\" ? dayStepFor() : \"\""); };
  check("a day starts at 5m (289 buckets)", stepOf(day.firstAt, day.lastAt) === "5m");
  check("an hour starts at 5m", stepOf("2026-11-01T00:00:00Z", "2026-11-01T01:00:00Z") === "5m");
  check("three days start at 15m", stepOf("2026-11-01T00:00:00Z", "2026-11-04T00:00:00Z") === "15m");
  check("two weeks start at 1h", stepOf("2026-11-01T00:00:00Z", "2026-11-15T00:00:00Z") === "1h");
  state.inputs.source = daySource;
  el("daystep").value = "5m";

  // A plan runs: the view says so, with the buckets and the time.
  const first = run("runDay()");
  check("the view says a plan runs", el("dayprogress").hidden === false);
  check("with the buckets and the start", /Planning the day: 289 buckets of 5m, each sized on its own, started \d\d:\d\d:\d\d UTC\./.test(el("dayprogtext").textContent),
    el("dayprogtext").textContent);
  check("and the seconds so far", /^\d+ s so far/.test(el("dayprogtime").textContent), el("dayprogtime").textContent);
  check("the button is off while it runs", el("dayrun").disabled === true);
  // No second plan while one runs: the button, or the view opening.
  run("runDay()");
  run("showView(\"day\")");
  await sleep(50);
  check("no second plan starts while one runs", dayCalls().length === 1, dayCalls().length);
  await first;
  check("an answered plan hides the progress", el("dayprogress").hidden === true && el("dayrun").disabled === false);
  check("and is drawn", !!state.day && el("dayout").hidden === false);

  // Cancel aborts the request and frees the button.
  server.delay = 5000;
  state.k.ttftTarget = 3;
  const second = run("runDay()");
  await sleep(50);
  run("dayCancel()");
  await second;
  const last = dayCalls()[dayCalls().length - 1];
  check("Cancel aborts the request", last.aborted === true && last.done === false);
  check("and hides the progress", el("dayprogress").hidden === true);
  check("and frees the button", el("dayrun").disabled === false && state.dayBusy === false);
  check("and says so, as no error", /cancelled/.test(el("status").textContent) && !el("status").classList.contains("error"), el("status").textContent);
  // A cancelled plan is not asked again by itself: not on the view opening.
  const before = dayCalls().length;
  run("showView(\"answer\"); showView(\"day\")");
  await sleep(600);
  check("a cancelled plan is not asked again by itself", dayCalls().length === before, dayCalls().length - before);
  check("and the stale note does not say it will be", /you cancelled the plan for them: press Plan the day/.test(el("daystale").textContent) &&
    !/plans the day again when it is shown/.test(el("daystale").textContent), el("daystale").textContent);
  // A knob changed asks it again.
  state.k.ttftTarget = 4;
  server.delay = 20;
  run("showView(\"answer\"); showView(\"day\")");
  check("a changed knob asks again", await until(() => dayCalls().length === before + 1 && !state.dayBusy), dayCalls().length - before);
  run("showView(\"answer\")");

  // An answer whose body lands after Cancel is not drawn, nor an error.
  server.delay = 0;
  server.bodyDelay = 5000;
  state.k.ttftTarget = 5;
  state.day = null;
  setStatus("");
  const third = run("runDay()");
  await until(() => calls[calls.length - 1].done);
  run("dayCancel()");
  await third;
  server.bodyDelay = 0;
  check("a body cut off by Cancel is not drawn", state.day === null, JSON.stringify(state.day && state.day.buckets && state.day.buckets.length));
  check("and reads as cancelled, not an error", /cancelled/.test(el("status").textContent) && !el("status").classList.contains("error"), el("status").textContent);

  // The wait before retrying a busy server ends at Cancel.
  server.busy = 1;
  state.k.ttftTarget = 6;
  const t0 = Date.now();
  const fourth = run("runDay()");
  await until(() => calls[calls.length - 1].done);
  await sleep(50);
  run("dayCancel()");
  await Promise.race([fourth, sleep(2000)]);
  check("Cancel ends the wait before a retry", Date.now() - t0 < 2000 && !state.dayBusy, Date.now() - t0);
  server.busy = 0;

  // Cancel from the keyboard: the button goes, the focus moves to Plan the day.
  server.delay = 5000;
  state.k.ttftTarget = 7;
  const fifth = run("runDay()");
  await sleep(20);
  el("daycancel").focus();
  run("dayCancel()");
  await fifth;
  check("Cancel hands the focus to Plan the day", document.activeElement === el("dayrun"));

  // A new load cancels a run without a word, even a load of the same inputs,
  // and its plan is asked as any other.
  const sixth = run("runDay()");
  await sleep(20);
  setStatus("loaded");
  run("setInputs")(inputs);
  await sixth;
  const cut = dayCalls()[dayCalls().length - 1];
  check("a new load cancels the run", cut.aborted === true && !state.dayBusy);
  check("without a word", el("status").textContent === "loaded", el("status").textContent);
  check("and not as a plan cancelled", state.dayCancelled === null, state.dayCancelled);

  // A bucket picked stays across loads while the server sizes it, and the
  // count is of buckets aligned to the step, as the server cuts them.
  el("daystep").value = "1h";
  el("daystep").onchange();
  run("setInputs")(inputs);
  check("a picked bucket stays across a load", el("daystep").value === "1h", el("daystep").value);
  el("daystep").value = "1m";
  el("daystep").onchange();
  run("setInputs")(Object.assign({}, inputs, {source: Object.assign({}, inputs.source, {lastAt: "2026-11-04T00:00:00Z"})}));
  check("but not where the server would refuse it", el("daystep").value === "15m", el("daystep").value);
  state.inputs = Object.assign({}, inputs, {source: Object.assign({}, inputs.source, {firstAt: "2026-11-01T00:03:00Z", lastAt: "2026-11-01T00:07:00Z"})});
  check("buckets are counted aligned to the step", run("dayBuckets(\"5m\")") === 2, run("dayBuckets(\"5m\")"));
} catch (e) {
  failures.push("threw: " + (e && e.stack || e));
}

if (failures.length) {
  console.error(`FAIL check-planner-dayprogress: ${failures.length} of ${passed + failures.length}`);
  for (const f of failures) console.error("  - " + f);
  process.exit(1);
}
console.log(`ok check-planner-dayprogress: ${passed} checks`);
