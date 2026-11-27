// Runs the capacity planner page's SLO target controls and its day plan
// (internal/whatifapi/page/planner.js: slider, fitSlider, the target boxes,
// runDay, dayStale, showView) against a fake DOM whose range inputs clamp
// their value to min and max as a browser does, and a fake server that
// answers a day plan at the target asked. A target typed in the box is the
// target, past the slider's old range too, and a later touch of the slider
// does not replace it; the slider reaches the floor the plan priced; a day
// plan says the targets it used, reads stale once they change, and is
// planned again when its view opens. The sliders are on a log scale and a
// release where the slider stands keeps a typed target exactly; a target the
// page set under 1.5 times the floor is raised above it and said, while a
// policy's is kept and flagged; a V_P that is only a lower bound opens the
// assumptions at its box, and a day plan that could not price prefill points
// there; the right-size answer names the targets it was judged at; and the
// common controls sit above the tabs, not in one. Called by
// hack/check-planner-scenarios.sh.
//
// Usage: node hack/check-planner-targets.mjs [scenario.js dayplan.js backtest.js planner.js day-plan.json index.html]
import fs from "node:fs";
import vm from "node:vm";

const files = [
  process.argv[2] || "internal/whatifapi/page/scenario.js",
  process.argv[3] || "internal/whatifapi/page/dayplan.js",
  process.argv[4] || "internal/whatifapi/page/backtest.js",
  process.argv[5] || "internal/whatifapi/page/planner.js",
];
const golden = JSON.parse(fs.readFileSync(process.argv[6] || "cmd/capacity-planner/testdata/day-plan-ols-sweep-json.golden", "utf8"));
const html = fs.readFileSync(process.argv[7] || "internal/whatifapi/page/index.html", "utf8");

// fakeElement is as much of a DOM element as these controls touch.
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
    focus() {},
  };
}

// fakeRange is an <input type="range">: its value reads clamped to min and
// max as they stand and snapped to min plus whole steps, the browser's value
// sanitization, with no attribute meaning 0 to 100 in steps of 1.
function fakeRange() {
  const e = fakeElement("input");
  let raw = "50";
  e.min = "";
  e.max = "";
  e.step = "";
  Object.defineProperty(e, "value", {
    get() {
      const lo = e.min === "" ? 0 : +e.min, hi = Math.max(lo, e.max === "" ? 100 : +e.max), st = e.step === "" ? 1 : +e.step;
      let v = lo + Math.round((Math.min(Math.max(+raw, lo), hi) - lo) / st) * st;
      if (v > hi) v -= st;
      return String(Number(v.toFixed(9)));
    },
    set(v) { raw = String(v); },
  });
  return e;
}

const byId = new Map([["ttft", fakeRange()], ["tpot", fakeRange()]]);
const document = {
  getElementById(id) { if (!byId.has(id)) byId.set(id, fakeElement("div")); return byId.get(id); },
  createElement: fakeElement,
  createTextNode: text => Object.assign(fakeElement("#text"), {textContent: text}),
  addEventListener() {},
  querySelector() { return fakeElement("div"); },
};

// The fake server answers a day plan at the SLO it was asked.
const calls = [];
function respond(status, doc) {
  return {ok: status < 300, status, statusText: "x", headers: {get: () => null}, json: async () => doc};
}
async function fakeFetch(path, opt) {
  const body = JSON.parse(opt.body || "null");
  calls.push({path, body});
  // A 123 ms TTFT is one the fake server refuses.
  if (path === "v1/dayplan" && body.slo.ttft.target === 0.123) return respond(400, {error: "refused"});
  if (path === "v1/dayplan") {
    const d = JSON.parse(JSON.stringify(golden));
    d.ttft = Object.assign({}, d.ttft, {statistic: body.slo.ttft.statistic, target: body.slo.ttft.target});
    return respond(200, d);
  }
  return respond(404, {error: "no " + path});
}

const ctx = vm.createContext({document, location: {hash: ""}, history: {replaceState() {}}, fetch: fakeFetch, AbortController, DOMException,
  setTimeout, clearTimeout, setInterval, clearInterval, URLSearchParams, performance, console});
for (const f of files) vm.runInContext(fs.readFileSync(f, "utf8"), ctx, {filename: f});
const run = src => vm.runInContext(src, ctx);
// No plan is priced here: plan() is the exec check's.
run("plan = async () => {};");
const state = run("state");
const el = id => document.getElementById(id);

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

const inputs = {inputsId: "sha256:x", model: "m",
  fleet: {prefill: {variant: "p", replicas: 2, gpusPerReplica: 8}, decode: {variant: "d", replicas: 2, gpusPerReplica: 8}},
  source: {kind: "recording", name: "r"}, terms: [], observed: {buckets: [], outputTokens: {value: 300}}};
// A plan whose TTFT floor, 2.92 s, is past four times the page's 1 s.
const plan = {ttft: {statistic: "p90", markers: {}, line: {target: 1, floor: {value: 2.92}, curve: []}}, tpot: {statistic: "mean", floor: {value: 0.012}, curve: []}};

// ms is the target, in ms, a target's slider position stands for: the
// position itself on a linear slider, ten to its power on a log one.
const logScale = run("typeof SLIDER_STEP") === "number";
const ms = x => (logScale ? Math.pow(10, +x) : +x);
const near = (a, b) => Math.abs(a - b) <= Math.max(1e-9, Math.abs(b) * 0.03);

try {
  run("wire()");
  state.inputs = inputs;
  state.plan = plan;
  state.mode = "plan";
  state.k = {ttftStat: "p90", ttftTarget: 1, tpotStat: "mean", tpotTarget: 0.05, ttftDefault: true, tpotDefault: true,
    headroom: 0.85, assume: {}, edges: "", perBucket: false, maxRep: 32, rate: 5, prefill: 2, decode: 2, priceAs: "mix", outputTokens: 300};
  el("dayrun").disabled = false;
  // The page opens on the Answer view.
  el("view-day").hidden = true;
  // The page at load: the slider around the 1 s target.
  run("slider(\"ttft\", 1000, 1)");
  run("slider(\"tpot\", 50, 0.1)");
  check("the slider starts at the target", near(ms(el("ttft").value), 1000), el("ttft").value);
  // A log scale: two decades or more either side of a 1 s start.
  check("the slider spans a 1 s start on a log scale", ms(el("ttft").max) / ms(el("ttft").min) >= 100 && logScale,
    `${el("ttft").min}..${el("ttft").max}`);

  // A target typed past the slider's range is the target, and the slider
  // shows it rather than its end.
  el("ttftbox").value = "5000";
  el("ttftbox").onchange();
  check("the typed target is taken", state.k.ttftTarget === 5, state.k.ttftTarget);
  check("the slider shows the typed target, not its end", near(ms(el("ttft").value), 5000), `${el("ttft").value} in ${el("ttft").min}..${el("ttft").max}`);
  // The slider touched (released where it stands) keeps the typed target.
  el("ttft").onchange();
  check("touching the slider keeps the typed target, exactly", state.k.ttftTarget === 5, state.k.ttftTarget);
  // Below the range too.
  el("ttftbox").value = "20";
  el("ttftbox").onchange();
  el("ttft").onchange();
  check("a target typed below the range is kept", state.k.ttftTarget === 0.02, state.k.ttftTarget);
  el("tpotbox").value = "400";
  el("tpotbox").onchange();
  el("tpot").onchange();
  check("a TPOT typed past the range is kept", Math.abs(state.k.tpotTarget - 0.4) < 1e-9, state.k.tpotTarget);

  // The slider reaches the floor the plan priced, from a target below it.
  state.k.ttftTarget = 0.2;
  run("slider(\"ttft\", 200, 1)");
  run("if (typeof fitSlider === \"function\") fitSlider(\"ttft\", state.k.ttftTarget * 1000, 1, targetFloor(\"ttft\"))");
  check("the slider runs past the floor", ms(el("ttft").max) > 2920, `max ${el("ttft").max}`);
  check("the target stays where it was", near(ms(el("ttft").value), 200) && state.k.ttftTarget === 0.2, el("ttft").value);
  el("ttft").value = el("ttft").max;
  el("ttft").onchange();
  check("dragged to the end, the target is past the floor", state.k.ttftTarget > 2.92, state.k.ttftTarget);
  // The end of the slider is inside it: a plan priced at it does not widen
  // the range again.
  const end = el("ttft").max;
  // The box rounds the end up a little past it, as the browser does.
  run("fitSlider(\"ttft\", Math.ceil(Math.pow(10, +el(\"ttft\").max)), 1, targetFloor(\"ttft\"))");
  check("the end of the slider does not widen it", el("ttft").max === end, `${end} -> ${el("ttft").max}`);

  // A floor that is no whole number of steps still leaves a typed whole
  // target whole: the slider touched does not snap 150 to 149.865.
  run("slider(\"ttft\", 200, 1, 37.3)");
  el("ttftbox").value = "150";
  el("ttftbox").onchange();
  el("ttft").onchange();
  check("an unaligned floor does not move a typed target", state.k.ttftTarget === 0.15, state.k.ttftTarget);
  run("slider(\"tpot\", 50, 0.1, 7.3)");
  el("tpotbox").value = "20.5";
  el("tpotbox").onchange();
  el("tpot").onchange();
  check("an unaligned TPOT floor does not move a typed target", Math.abs(state.k.tpotTarget - 0.0205) < 1e-12, state.k.tpotTarget);

  // The day plan: it says the targets it used.
  state.k.ttftTarget = 5;
  state.k.tpotTarget = 0.05;
  await run("runDay()");
  check("the day plan was asked at 5 s", calls.length === 1 && calls[0].body.slo.ttft.target === 5, JSON.stringify(calls.map(c => c.body.slo)));
  check("the day plan says the targets it used", /TTFT p90 5\.00 s/.test(el("dayused").textContent), el("dayused").textContent);
  check("a fresh day plan is not stale", !el("dayout").classList.contains("stale") && el("daystale").hidden === true);
  // It was drawn whole: a throw while drawing reads as a refused plan.
  check("the day plan is drawn without an error", /^Planned /.test(el("status").textContent) && !run("state.dayFailed"), el("status").textContent);

  // A target changed in the box marks it stale, with what it used.
  el("ttftbox").value = "500";
  el("ttftbox").onchange();
  check("a changed target marks the day plan stale", el("dayout").classList.contains("stale"));
  check("the stale note names the old target", el("daystale").hidden === false && /5\.00 s/.test(el("daystale").textContent), el("daystale").textContent);
  // Opening the day view plans it again, at the new target.
  run("showView(\"day\")");
  check("opening the day view plans again", await until(() => calls.length === 2 && !state.dayBusy), calls.length);
  check("planned again at 500 ms", calls.length === 2 && calls[1].body.slo.ttft.target === 0.5, JSON.stringify(calls.map(c => c.body.slo.ttft.target)));
  check("the replanned day says 500 ms", /TTFT p90 500 ms/.test(el("dayused").textContent), el("dayused").textContent);
  check("the replanned day is not stale", !el("dayout").classList.contains("stale") && el("daystale").hidden === true);
  // Opening it again with nothing changed asks nothing.
  run("showView(\"answer\"); showView(\"day\")");
  await new Promise(r => setTimeout(r, 50));
  check("an unchanged day plan is not asked again", calls.length === 2, calls.length);
  // The bucket the day plan reads is a knob it uses too.
  el("daystep").value = "1h";
  el("daystep").onchange && el("daystep").onchange();
  check("a changed bucket marks the day plan stale", el("dayout").classList.contains("stale"));
  el("daystep").value = "";
  run("dayStale()");

  // With the day plan on screen, a common control changed above the tabs
  // plans it again; with another view shown, it waits for the view.
  run("showView(\"day\")");
  await until(() => !state.dayBusy);
  const before = calls.length;
  el("ttftbox").value = "700";
  el("ttftbox").onchange();
  check("a target changed on the day view plans it again", await until(() => calls.length === before + 1 && !state.dayBusy, 3000) &&
    calls[calls.length - 1].body.slo.ttft.target === 0.7, JSON.stringify(calls.slice(before).map(c => c.body.slo.ttft.target)));
  check("and it says 700 ms", /TTFT p90 700 ms/.test(el("dayused").textContent), el("dayused").textContent);
  // A plan the server refused is not asked again by itself, until a knob
  // changes; a knob moved back to it asks it again.
  const typeTTFT = async v => {
    el("ttftbox").value = v;
    el("ttftbox").onchange();
    await new Promise(r => setTimeout(r, 600));
    await until(() => !state.dayBusy);
  };
  const atRefused = calls.length;
  await typeTTFT("123");
  check("a refused day plan is asked once", calls.length === atRefused + 1, calls.length - atRefused);
  run("dayStale(); dayAgain()");
  await new Promise(r => setTimeout(r, 600));
  check("and not asked again by itself", calls.length === atRefused + 1, calls.length - atRefused);
  await typeTTFT("800");
  check("a changed knob asks again", calls.length === atRefused + 2 && /TTFT p90 800 ms/.test(el("dayused").textContent), calls.length - atRefused);
  await typeTTFT("123");
  check("a knob moved back to the refused request asks it again", calls.length === atRefused + 3, calls.length - atRefused);
  await typeTTFT("800");
  run("showView(\"answer\")");
  const quiet = calls.length;
  el("headroom").value = "0.7";
  el("headroom").onchange();
  await new Promise(r => setTimeout(r, 700));
  check("a change on another view plans nothing", calls.length === quiet, calls.length - quiet);
  check("but marks the day plan stale", el("dayout").classList.contains("stale") && el("daystale").hidden === false);
  run("showView(\"day\")");
  check("until the day view opens", await until(() => calls.length === quiet + 1 && !state.dayBusy) && calls[calls.length - 1].body.headroom === 0.7,
    calls.length - quiet);
  run("showView(\"answer\")");

  // A slider moved keeps the step's precision: a position read back is the
  // target, to 2.3%.
  el("ttft").value = String(Math.log10(1500));
  el("ttft").onchange();
  check("a moved log slider sets its target", logScale && near(state.k.ttftTarget, 1.5), state.k.ttftTarget);

  // The page's default under 1.5 times the floor is raised, once, to a
  // readable value, and said.
  const fresh = over => Object.assign({ttftStat: "p90", ttftTarget: 1, tpotStat: "mean", tpotTarget: 0.05, ttftDefault: true, tpotDefault: true,
    headroom: 0.85, assume: {}, edges: "", perBucket: false, maxRep: 32, rate: 5, prefill: 2, decode: 2, priceAs: "mix", outputTokens: 300}, over);
  state.k = fresh({});
  const raised = run("typeof raiseDefaults === \"function\" ? raiseDefaults() : false");
  check("a page default under the floor is raised", raised === true && state.k.ttftTarget === 5, state.k.ttftTarget);
  check("the raised default is still the page's", state.k.ttftDefault === true);
  check("a TPOT default above 1.5 times its floor is kept", state.k.tpotTarget === 0.05, state.k.tpotTarget);
  check("the slider stands at the raised default", near(ms(el("ttft").value), 5000), el("ttft").value);
  const note = run("typeof targetNote === \"function\" ? targetNote(\"ttft\", \"TTFT\", PAGE_TTFT, state.k.ttftTarget) : \"\"");
  check("the raise is said, with the floor", /under this model's floor \(2\.92 s\): it starts at 5\.00 s/.test(note), note);
  check("raised once a load", run("typeof raiseDefaults === \"function\" ? raiseDefaults() : true") === false);
  // A policy's target under the floor is kept, and flagged.
  state.k = fresh({ttftTarget: 0.2, ttftDefault: false});
  run("typeof raiseDefaults === \"function\" && raiseDefaults()");
  check("a policy target under the floor is kept", state.k.ttftTarget === 0.2, state.k.ttftTarget);
  const flagged = run("typeof targetNote === \"function\" ? targetNote(\"ttft\", \"TTFT\", PAGE_TTFT, 0.2) : \"\"");
  check("a target under the floor is flagged", /Under this model's floor \(2\.92 s\): no load meets it/.test(flagged) && !/starts at/.test(flagged), flagged);
  // Readable values.
  check("readable values", run("typeof readableUp === \"function\" ? [readableUp(4380, 1), readableUp(18, 0.1), readableUp(0.05, 0.1), readableUp(1000, 1)].join() : \"\"") === "5000,20,0.1,1000");

  // A V_P that is only a lower bound opens the assumptions at its box.
  const lbInputs = Object.assign({}, inputs, {terms: [{name: "vp", source: "learned served-peak", value: 29632.58, unit: "tokens/s"}]});
  el("assumebox").open = false;
  run(`showLearned(${JSON.stringify(lbInputs)})`);
  check("a lower-bound V_P opens the assumptions", el("assumebox").open === true);
  check("the V_P box says it is a lower bound", /only a lower bound: the prefill pods never queued/.test(el("l-vp").textContent), el("l-vp").textContent);
  const queued = Object.assign({}, inputs, {terms: [{name: "vp", source: "learned peak-under-queue", value: 17046, unit: "tokens/s"}]});
  run(`showLearned(${JSON.stringify(queued)})`);
  check("a V_P learned under a queue leaves them closed", el("assumebox").open === false && /^learned /.test(el("l-vp").textContent), el("l-vp").textContent);

  // A day plan that could not price prefill points to the V_P box.
  const unpriced = JSON.parse(JSON.stringify(golden));
  unpriced.refused = [{reason: "NoPrefillCapacity", buckets: 3, detail: "only the rate a prefill pod served is known"}];
  unpriced.summary.counts.prefillNotPriced = unpriced.summary.counts.sized;
  unpriced.summary.neededGpuHours = null;
  const point = /type a prefill speed \(V_P per pod\) under Assumptions/;
  check("the day plan's refusal points to the V_P box", point.test(run(`dayRefusedHTML(${JSON.stringify(unpriced)})`)));
  check("the day plan's summary points to the V_P box", run(`daySummaryText(${JSON.stringify(unpriced)})`).some(l => point.test(l)));
  check("a priced day plan does not", !run(`daySummaryText(${JSON.stringify(golden)})`).some(l => point.test(l)));

  // The right-size answer, hidden by a change, names the targets it used.
  state.k = fresh({ttftTarget: 2});
  state.reportUsed = run("typeof dayUsedText === \"function\" ? dayUsedText({slo: planRequest().slo, headroom: 0.85}) : null");
  el("rresult").hidden = false;
  run("reportStale()");
  check("a changed knob names the targets the fleet was judged at", /judged at TTFT p90 2\.00 s, TPOT mean 50\.0 ms, safety margin 0\.85: judge the fleet again/.test(el("rstatus").textContent),
    el("rstatus").textContent);

  // The common controls sit above the tabs, in one card, and in no view.
  const tabs = html.indexOf('<div class="tabs"');
  const common = html.indexOf('id="common"');
  const answer = html.indexOf('id="view-answer"'), dayView = html.indexOf('id="view-day"');
  check("a common card above the tabs", common > 0 && common < tabs, `${common} ${tabs}`);
  for (const id of ["ttftstat", "ttft", "ttftbox", "tpotstat", "tpot", "tpotbox", "ratio", "headroom", "priceas", "outtok", "assumebox", "a-vp", "a-lar"]) {
    const at = html.indexOf(`id="${id}"`);
    check(`${id} is a common control`, at > common && at < tabs, `${id} at ${at}`);
  }
  for (const id of ["prefill", "decode", "rate", "ratebox", "concbox", "maxrep"]) {
    const at = html.indexOf(`id="${id}"`);
    check(`${id} stays in the Answer view`, at > answer && at < dayView, `${id} at ${at}`);
  }
  check("the V_P box names V_P", html.includes(">Prefill speed V_P per pod, tokens/s</label>"));
} catch (e) {
  failures.push("threw: " + (e && e.stack || e));
}

if (failures.length) {
  console.error(`FAIL check-planner-targets: ${failures.length} of ${passed + failures.length}`);
  for (const f of failures) console.error("  - " + f);
  process.exit(1);
}
console.log(`ok check-planner-targets: ${passed} checks`);
