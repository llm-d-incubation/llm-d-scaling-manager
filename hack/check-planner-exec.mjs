// Runs the capacity planner page's answer panel (internal/whatifapi/page/
// planner.js: startExecutive, execKnobs, execAnswers) against a fake server
// and a fake DOM: the report and the +25% / +50% sizes are asked once per
// load, asked again when the SLO or shape changes, read as updating while
// they are, and an answer for the old targets never shows as current. A load
// whose prefill the default model cannot price, or a decode-only source,
// sends no size. Called by hack/check-planner-scenarios.sh.
//
// Usage: node hack/check-planner-exec.mjs [scenario.js dayplan.js backtest.js planner.js]
import fs from "node:fs";
import vm from "node:vm";

const files = [
  process.argv[2] || "internal/whatifapi/page/scenario.js",
  process.argv[3] || "internal/whatifapi/page/dayplan.js",
  process.argv[4] || "internal/whatifapi/page/backtest.js",
  process.argv[5] || "internal/whatifapi/page/planner.js",
];

// fakeElement is as much of a DOM element as the answer panel touches.
function fakeElement(tag) {
  const e = {
    tagName: tag, textContent: "", className: "", hidden: false, value: "", disabled: false, title: "", type: "",
    style: {}, dataset: {}, children: [], onclick: null,
    classList: {
      set: new Set(),
      add(c) { this.set.add(c); }, remove(c) { this.set.delete(c); },
      toggle(c, on) { if (on === undefined ? !this.set.has(c) : on) this.set.add(c); else this.set.delete(c); },
      contains(c) { return this.set.has(c); },
    },
    appendChild(c) { this.children.push(c); return c; },
    replaceChildren(...cs) { this.children = cs; },
    querySelector() { return fakeElement("div"); },
    // wire() renumbers the shape rows it adds; a fake row holds no inputs.
    querySelectorAll() { return []; },
    addEventListener() {},
    setAttribute() {},
  };
  return e;
}
const byId = new Map();
const document = {
  getElementById(id) { if (!byId.has(id)) byId.set(id, fakeElement("div")); return byId.get(id); },
  createElement: fakeElement,
  addEventListener() {},
  querySelector() { return fakeElement("div"); },
};

// The fake server. The report's verdict and each size's fleet follow the
// TTFT target asked, so a row shows which target it answers.
const calls = [];
const server = {sizeDelay: 0, reportDelay: 0, sizeStatus: 200};
const delay = (ms, signal) => new Promise((resolve, reject) => {
  if (signal && signal.aborted) return reject(new DOMException("aborted", "AbortError"));
  const t = setTimeout(resolve, ms);
  if (signal) signal.addEventListener("abort", () => { clearTimeout(t); reject(new DOMException("aborted", "AbortError")); });
});
function respond(status, doc) {
  return {ok: status < 300, status, statusText: "x", headers: {get: () => null}, json: async () => doc};
}
async function fakeFetch(path, opt) {
  const body = JSON.parse(opt.body || "null");
  const call = {path, body, aborted: false, done: false};
  calls.push(call);
  try {
    if (path === "v1/report") {
      await delay(server.reportDelay, opt.signal);
      const t = body.slo.ttft.target;
      call.done = true;
      return respond(200, {load: {statistic: "p95", rate: {value: 20}}, fleet: {prefill: 3, decode: 6},
        shapes: [{verdict: t >= 2 ? {verdict: "OVER-PROVISIONED", shrink: ["decode"]} : {verdict: "UNDER-PROVISIONED", needed: {prefill: 5, decode: 9}}}]});
    }
    if (path === "v1/size") {
      await delay(server.sizeDelay, opt.signal);
      call.done = true;
      if (server.sizeStatus !== 200) return respond(server.sizeStatus, {error: "boom"});
      const t = body.slo.ttft.target, rate = body.traffic.classes.critical.rate;
      // A looser target needs fewer prefill: one per T s of target.
      const p = Math.max(1, Math.round(rate / 5 / t));
      return respond(200, {rate, maxReplicas: body.maxReplicas, answer: {fleet: {prefill: p, decode: 8}, gpus: 8 * (p + 8), limit: {value: rate * 1.1}}});
    }
    if (path === "v1/backtest") {
      call.done = true;
      return respond(200, {steps: []});
    }
    return respond(404, {error: "no " + path});
  } catch (e) {
    call.aborted = true;
    throw e;
  }
}

const ctx = vm.createContext({document, location: {hash: ""}, fetch: fakeFetch, AbortController, DOMException,
  setTimeout, clearTimeout, setInterval, clearInterval, URLSearchParams, performance, console});
for (const f of files) vm.runInContext(fs.readFileSync(f, "utf8"), ctx, {filename: f});
// The charts and bars are not this check's: they need the whole page.
vm.runInContext("renderLimits = renderTTFT = renderTPOT = renderBacktest = () => {};", ctx);
const run = src => vm.runInContext(src, ctx);
const state = run("state");

let passed = 0;
const failures = [];
function check(name, ok, detail) {
  if (ok) passed++;
  else failures.push(name + (detail === undefined ? "" : ": " + detail));
}

const inputsPD = {inputsId: "sha256:x", model: "m",
  fleet: {prefill: {variant: "p", replicas: 3, gpusPerReplica: 8}, decode: {variant: "d", replicas: 6, gpusPerReplica: 8}},
  source: {kind: "recording", name: "r"}, terms: [], observed: {buckets: [], outputTokens: {value: 300}},
  // The server's held-out check at load: the panel runs no check of its own.
  ttftModel: {heldOut: {statistic: "p90", within: 10, steps: 12, rateFrom: 5, rateTo: 40}}};
const pricedPlan = {limits: {prefill: {value: 30}, decode: {value: 40}, fleet: {value: 30}, chosen: {rate: 20}},
  ttft: {statistic: "p90", markers: {prefillKnee: {value: 30}}, line: {target: 1, curve: []}}, tpot: {statistic: "mean", curve: []}};
const k0 = () => ({ttftStat: "p90", ttftTarget: 1, tpotStat: "mean", tpotTarget: 0.05, ttftDefault: false, tpotDefault: false,
  headroom: 1, assume: {}, edges: "", perBucket: false, maxRep: 16, rate: 20, prefill: 3, decode: 6, priceAs: "mix", outputTokens: 300});
const copy = x => JSON.parse(JSON.stringify(x));

// load starts a session as setInputs does, for the panel alone.
function load(inputs, plan) {
  run("if (typeof execAbort === \"function\") { execAbort(state.exec); clearTimeout(execTimer); }");
  state.inputs = copy(inputs);
  state.plan = copy(plan);
  state.k = k0();
  state.mode = "plan";
  state.bt = null;
  state.exec = {};
  calls.length = 0;
}
const row = label => (state.execView.lines.find(l => l.label === label) || {});
const until = async (cond, ms = 5000) => {
  const t0 = Date.now();
  while (!cond()) {
    if (Date.now() - t0 > ms) return false;
    await new Promise(r => setTimeout(r, 10));
  }
  return true;
};
const settled = () => state.exec && !state.exec.reportPending && !state.exec.growthPending && !state.exec.ansAbort;
// knobs is what render() does after a plan for the new knobs lands.
// On the page before the panel re-asked (a negative control), render() did
// the rest: startExecutive, which asks once per load.
const knobs = () => run("if (typeof execKnobs === \"function\") execKnobs(); renderExec(); startExecutive();");

try {
  // The first answers, at a 1 s TTFT target.
  load(inputsPD, pricedPlan);
  await run("startExecutive()");
  check("one report and two sizes at load", calls.filter(c => c.path === "v1/report").length === 1 && calls.filter(c => c.path === "v1/size").length === 2,
    calls.map(c => c.path).join(","));
  check("no check runs beside the server's", !calls.some(c => c.path === "v1/backtest"));
  check("the sizes are at the SLO", calls.filter(c => c.path === "v1/size").every(c => c.body.slo.ttft.target === 1));
  check("the sizes are at +25% and +50% of the busiest load",
    JSON.stringify(calls.filter(c => c.path === "v1/size").map(c => c.body.traffic.classes.critical.rate)) === "[25,30]");
  check("growth at 1 s", row("+25% / +50% traffic").text === "+25% (25 r/s): 5 prefill + 8 decode (104 GPUs); +50% (30 r/s): 6 prefill + 8 decode (112 GPUs), at the same SLO.",
    row("+25% / +50% traffic").text);
  check("verdict at 1 s", /Too small/.test(row("Verdict").text), row("Verdict").text);
  check("nothing reads as updating once answered", state.execView.lines.every(l => !l.updating));

  // The rate or the fleet on screen is not what the panel asks: no re-ask.
  state.k.rate = 33;
  state.k.prefill = 5;
  knobs();
  await new Promise(r => setTimeout(r, 500));
  check("a rate or fleet change re-asks nothing", calls.length === 3, calls.length);

  // The SLO changes: both rows read as updating at once, with no old answer.
  state.k.ttftTarget = 2;
  state.plan.ttft.line.target = 2;
  knobs();
  check("the verdict reads as updating", row("Verdict").updating === true && !/Too small/.test(row("Verdict").text) && /Judging/.test(row("Verdict").text),
    row("Verdict").text);
  check("growth reads as sizing", row("+25% / +50% traffic").updating === true && /^Sizing/.test(row("+25% / +50% traffic").text),
    row("+25% / +50% traffic").text);
  check("the panel's dd carries the updating class", byId.get("execlines").children.some(c => c.className === "updating"));
  check("the re-ask is debounced", calls.length === 3, calls.length);
  check("the re-ask settles", await until(settled));
  check("a report and two sizes re-asked", calls.length === 6 && calls.slice(3).map(c => c.path).join(",") === "v1/report,v1/size,v1/size",
    calls.map(c => c.path).join(","));
  check("the re-asked sizes are at the new target", calls.slice(4).every(c => c.body.slo.ttft.target === 2));
  check("growth at 2 s", row("+25% / +50% traffic").text === "+25% (25 r/s): 3 prefill + 8 decode (88 GPUs); +50% (30 r/s): 3 prefill + 8 decode (88 GPUs), at the same SLO.",
    row("+25% / +50% traffic").text);
  check("verdict at 2 s", /A replica to spare/.test(row("Verdict").text) && !/Too small/.test(row("Verdict").text), row("Verdict").text);
  check("nothing reads as updating once re-answered", state.execView.lines.every(l => !l.updating));
  check("no check re-asked", !calls.some(c => c.path === "v1/backtest"));

  // A shape change re-asks the sizes alone: the report is at the traffic's
  // own shape.
  calls.length = 0;
  state.k.outputTokens = 900;
  knobs();
  check("a shape change marks growth, not the verdict", row("+25% / +50% traffic").updating === true && !row("Verdict").updating);
  await until(settled);
  check("a shape change re-asks the sizes alone", calls.map(c => c.path).join(",") === "v1/size,v1/size", calls.map(c => c.path).join(","));
  check("at the new shape", calls.every(c => c.body.traffic.mix && c.body.traffic.mix.outputTokens === 900));

  // A slow server: a newer target cancels the sizes in flight, and their
  // answers never show.
  calls.length = 0;
  server.sizeDelay = 300;
  state.k.ttftTarget = 1;
  knobs();
  await until(() => calls.some(c => c.path === "v1/size"));
  state.k.ttftTarget = 2;
  knobs();
  check("the size in flight is cancelled", await until(() => calls.filter(c => c.path === "v1/size")[0].aborted));
  check("still sizing, never the old answer", /^Sizing/.test(row("+25% / +50% traffic").text), row("+25% / +50% traffic").text);
  check("a slow re-ask settles", await until(settled));
  check("the answer is the newest target's", /3 prefill/.test(row("+25% / +50% traffic").text) && /A replica to spare/.test(row("Verdict").text),
    row("+25% / +50% traffic").text + " | " + row("Verdict").text);
  check("no size for the old target completed", calls.filter(c => c.path === "v1/size" && c.body.slo.ttft.target === 1).every(c => !c.done));
  server.sizeDelay = 0;

  // A new load cancels the sizes in flight; nothing lands on the new panel.
  server.sizeDelay = 300;
  state.k.ttftTarget = 3;
  knobs();
  await until(() => calls.some(c => c.path === "v1/size" && c.body.slo.ttft.target === 3));
  const old = state.exec;
  load(inputsPD, pricedPlan);
  await new Promise(r => setTimeout(r, 400));
  check("a load cancels the old panel's sizes", !!old.ansAbort && old.ansAbort.signal.aborted);
  check("nothing from the old load lands", !state.exec.growth && !state.exec.report);
  server.sizeDelay = 0;

  // A failed size says so; the report's answer stays.
  server.sizeStatus = 500;
  load(inputsPD, pricedPlan);
  await run("startExecutive()");
  check("a failed size says so, once", row("+25% / +50% traffic").text === "+25% (25 r/s) and +50% (30 r/s): not sized (the request failed).",
    row("+25% / +50% traffic").text);
  server.sizeStatus = 200;

  // Cancel in the progress bar stops both sizes.
  server.sizeDelay = 300;
  load(inputsPD, pricedPlan);
  const p = run("startExecutive()");
  await until(() => calls.some(c => c.path === "v1/size"));
  byId.get("progcancel").onclick();
  await p;
  check("cancel stops both sizes", calls.filter(c => c.path === "v1/size").length === 1, calls.filter(c => c.path === "v1/size").length);
  check("a cancel says so", row("+25% / +50% traffic").text === "Not sized: cancelled.", row("+25% / +50% traffic").text);
  server.sizeDelay = 0;

  // The default model, prefill not measured: no size is asked, and the row
  // says what to do; nor after the SLO changes.
  const refusedPlan = copy(pricedPlan);
  refusedPlan.limits.prefill = {refused: {reason: "NoPrefillCapacity", detail: "x", flags: ["VPLowerBound"]}};
  load(inputsPD, refusedPlan);
  await run("startExecutive()");
  check("the default model sends no size", !calls.some(c => c.path === "v1/size"), calls.map(c => c.path).join(","));
  check("and says what is missing", row("+25% / +50% traffic").text === run("GROWTH_NO_PREFILL"), row("+25% / +50% traffic").text);
  state.k.ttftTarget = 2;
  knobs();
  check("never reads as sizing what it will not size", row("+25% / +50% traffic").text === run("GROWTH_NO_PREFILL") && !row("+25% / +50% traffic").updating,
    row("+25% / +50% traffic").text);
  await until(settled);
  check("nor after the SLO changes", !calls.some(c => c.path === "v1/size") && calls.filter(c => c.path === "v1/report").length === 2,
    calls.map(c => c.path).join(","));
  // The experimental model prices it: sized.
  const v2Plan = copy(refusedPlan);
  v2Plan.ttft.model = {priced: true};
  load(inputsPD, v2Plan);
  await run("startExecutive()");
  check("the experimental model's prefill is sized", calls.filter(c => c.path === "v1/size").length === 2);

  // A decode-only source: POST /v1/size sizes P/D alone, so none is asked.
  const decodeOnly = copy(inputsPD);
  delete decodeOnly.fleet.prefill;
  load(decodeOnly, pricedPlan);
  await run("startExecutive()");
  check("a decode-only source sends no size", !calls.some(c => c.path === "v1/size"), calls.map(c => c.path).join(","));
  check("and says why", row("+25% / +50% traffic").text === run("GROWTH_NO_PD"), row("+25% / +50% traffic").text);

  // The router's balancing, ticked under the assumptions, is
  // assume.loadAwareRouter in the plan asked and loadAwareRouter=1 in the
  // link, a link an import admits; unticked, neither is sent, so the
  // server's default (off) stands.
  load(inputsPD, pricedPlan);
  run("wire(); schedule = () => {};");
  const lar = document.getElementById("a-lar");
  const asked = () => run("planRequest()");
  const linked = () => run("linkParams().toString()");
  check("unticked: no assume sent", asked().assume === undefined, JSON.stringify(asked().assume));
  check("unticked: the link carries none", !linked().includes("loadAwareRouter"), linked());
  lar.checked = true;
  lar.onchange();
  check("ticked: assume.loadAwareRouter sent", asked().assume && asked().assume.loadAwareRouter === true, JSON.stringify(asked().assume));
  check("ticked: the link carries it", linked().includes("loadAwareRouter=1"), linked());
  check("ticked: an import admits the link", run(`linkProblem(${JSON.stringify(linked())})`) === "", run(`linkProblem(${JSON.stringify(linked())})`));
  check("ticked: counted as typed", document.getElementById("assumecount").textContent === "1 typed", document.getElementById("assumecount").textContent);
  // The link reopens it ticked (applyLink), and a link without it unticked.
  for (const [link, want] of [[linked(), true], ["recording=r&prefill=3", false]]) {
    run(`linked = new URLSearchParams(${JSON.stringify(link)}); state.k.assume = {}; el("a-lar").checked = !${want}; applyLink();`);
    check(`opened from ${link}: ticked ${want}`, lar.checked === want && (state.k.assume.loadAwareRouter === true) === want,
      `${lar.checked} ${JSON.stringify(state.k.assume)}`);
  }
  run("state.k.assume.loadAwareRouter = true;");
  lar.checked = false;
  lar.onchange();
  check("unticked again: no assume sent", asked().assume === undefined, JSON.stringify(asked().assume));

  // The day plan is asked with the experimental models the inputs were
  // resolved with, as the Answer view prices them, and without where none.
  load(inputsPD, pricedPlan);
  delete state.inputs.ttftModel;
  check("no model: the day plan is the planner's own", run("dayRequest()").experimentalModels === undefined, JSON.stringify(run("dayRequest()")));
  state.inputs.ttftModel = {model: "rank-mixture", label: "Experimental TTFT model"};
  check("a TTFT model: the day plan asks for it", run("dayRequest()").experimentalModels === true);
  delete state.inputs.ttftModel;
  state.inputs.tpotModel = {model: "renewal-lognormal", label: "Experimental TPOT model"};
  check("a TPOT model: the day plan asks for it", run("dayRequest()").experimentalModels === true);
} catch (e) {
  failures.push("threw: " + (e && e.stack || e));
}

run("if (typeof execAbort === \"function\") { execAbort(state.exec); clearTimeout(execTimer); }");
if (failures.length) {
  console.error(`FAIL check-planner-exec: ${failures.length} of ${passed + failures.length}`);
  for (const f of failures) console.error("  - " + f);
  process.exit(1);
}
console.log(`ok check-planner-exec: ${passed} checks`);
