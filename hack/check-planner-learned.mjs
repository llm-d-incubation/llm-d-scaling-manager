// Runs the capacity planner page's boxes whose value the source learned
// (internal/whatifapi/page/planner.js: showLearned, refreshBoxes, markBox,
// the assumed terms', output length's and R_q's boxes and their "Use
// learned" buttons, applyLink) against a fake DOM. A learned value is shown
// in its box, tagged "learned" (V_P from the served peak "lower bound"), and
// not sent; a refused term's box stays empty with its reason; an edited box
// is an assumption, tagged and sent; "Use learned" puts the learned value
// back and stops sending it; a link's typed value stays an assumption.
// Called by hack/check-planner-scenarios.sh.
//
// Usage: node hack/check-planner-learned.mjs [scenario.js dayplan.js backtest.js planner.js index.html]
import fs from "node:fs";
import vm from "node:vm";

const files = [
  process.argv[2] || "internal/whatifapi/page/scenario.js",
  process.argv[3] || "internal/whatifapi/page/dayplan.js",
  process.argv[4] || "internal/whatifapi/page/backtest.js",
  process.argv[5] || "internal/whatifapi/page/planner.js",
];
const html = fs.readFileSync(process.argv[6] || "internal/whatifapi/page/index.html", "utf8");

// fakeElement is as much of a DOM element as these boxes touch.
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
const byId = new Map();
const document = {
  getElementById(id) { if (!byId.has(id)) byId.set(id, fakeElement("div")); return byId.get(id); },
  createElement: fakeElement,
  createTextNode: text => Object.assign(fakeElement("#text"), {textContent: text}),
  addEventListener() {},
  querySelector() { return fakeElement("div"); },
};
const Option = function (text, value) { return Object.assign(fakeElement("option"), {textContent: text, value}); };
const ctx = vm.createContext({document, Option, localStorage: {getItem: () => null, setItem() {}}, location: {hash: ""}, history: {replaceState() {}},
  fetch: async () => ({ok: false, status: 404, json: async () => ({})}),
  AbortController, DOMException, setTimeout, clearTimeout, setInterval, clearInterval, URLSearchParams, performance, console});
for (const f of files) vm.runInContext(fs.readFileSync(f, "utf8"), ctx, {filename: f});
const run = src => vm.runInContext(src, ctx);
run("plan = async () => {}; schedule = () => {};");
const state = run("state");
const el = id => document.getElementById(id);

let passed = 0;
const failures = [];
function check(name, ok, detail) {
  if (ok) passed++;
  else failures.push(name + (detail === undefined ? "" : ": " + detail));
}
const tag = id => el("t-" + id).textContent;
const asked = () => run("planRequest()");

const terms = [
  {name: "vp", source: "learned peak-under-queue", value: 17046.48, unit: "tokens/s"},
  {name: "a", source: "learned", value: 0.0425, unit: "s"},
  {name: "promptCV", refused: {reason: "NoPromptSpread", detail: "x"}},
  {name: "routerHop", source: "learned", value: 0.002, unit: "s"},
];
const inputs = {inputsId: "sha256:x", model: "m", namespace: "n",
  fleet: {prefill: {variant: "p", replicas: 2, gpusPerReplica: 8}, decode: {variant: "d", replicas: 2, gpusPerReplica: 8}},
  source: {kind: "recording", name: "r"}, terms, observed: {buckets: [], outputTokens: {value: 300}}, policy: {}};
const load = x => run(`setInputs(${JSON.stringify(x)})`);

try {
  run("wire()");
  load(inputs);
  // A learned value is shown, tagged, and not sent.
  check("V_P shows its learned value", el("a-vp").value === "17046", el("a-vp").value);
  check("V_P is tagged learned", tag("a-vp") === "learned" && el("a-vp").classList.contains("learned"), tag("a-vp"));
  check("a shows its learned value in ms", el("a-a").value === "42.5", el("a-a").value);
  check("the router hop shows its learned value", el("a-hop").value === "2", el("a-hop").value);
  check("a refused term's box stays empty", el("a-cv").value === "" && tag("a-cv") === "", `${el("a-cv").value} ${tag("a-cv")}`);
  check("and says why", /refused: NoPromptSpread/.test(el("l-cv").textContent), el("l-cv").textContent);
  check("no learned value is sent", asked().assume === undefined, JSON.stringify(asked().assume));
  check("nothing counts as typed", el("assumecount").textContent === "none typed", el("assumecount").textContent);
  check("no Use learned while learned", el("r-a-vp").hidden === true);
  check("the output length shows the observed one", el("outtok").value === "300" && tag("outtok") === "learned", `${el("outtok").value} ${tag("outtok")}`);
  check("and is not sent", !(asked().traffic && asked().traffic.mix && asked().traffic.mix.outputTokens), JSON.stringify(asked().traffic));

  // Edited, it is an assumption: tagged, sent, flagged as typed.
  el("a-vp").value = "30000";
  el("a-vp").onchange();
  check("an edited V_P is sent", asked().assume && asked().assume.vp === 30000, JSON.stringify(asked().assume));
  check("and tagged assumed", tag("a-vp") === "assumed" && !el("a-vp").classList.contains("learned"), tag("a-vp"));
  check("and counted as typed", el("assumecount").textContent === "1 typed", el("assumecount").textContent);
  check("Use learned is offered", el("r-a-vp").hidden === false);
  // Use learned: back to the source's.
  el("r-a-vp").onclick();
  check("Use learned stops sending it", asked().assume === undefined, JSON.stringify(asked().assume));
  check("and shows the learned value again", el("a-vp").value === "17046" && tag("a-vp") === "learned", `${el("a-vp").value} ${tag("a-vp")}`);
  check("and hides itself", el("r-a-vp").hidden === true);
  // Typed back to the learned value, or emptied, it is the source's.
  el("a-a").value = "42.5";
  el("a-a").onchange();
  check("the learned value typed is not an assumption", asked().assume === undefined && tag("a-a") === "learned", JSON.stringify(asked().assume));
  el("a-a").value = "50";
  el("a-a").onchange();
  el("a-a").value = "";
  el("a-a").onchange();
  check("an emptied box shows the learned value, not sent", el("a-a").value === "42.5" && asked().assume === undefined, el("a-a").value);
  // A refused term typed is an assumption as before.
  el("a-cv").value = "1.5";
  el("a-cv").onchange();
  check("a refused term typed is sent", asked().assume && asked().assume.promptCV === 1.5 && tag("a-cv") === "assumed", JSON.stringify(asked().assume));
  check("with nothing to go back to", el("r-a-cv").hidden === true);
  el("a-cv").value = "";
  el("a-cv").onchange();

  // The output length the same way.
  el("outtok").value = "900";
  el("outtok").onchange();
  check("an edited output length is sent", asked().traffic && asked().traffic.mix && asked().traffic.mix.outputTokens === 900, JSON.stringify(asked().traffic));
  check("and tagged assumed", tag("outtok") === "assumed" && el("r-outtok").hidden === false, tag("outtok"));
  el("r-outtok").onclick();
  check("Use learned puts the observed length back", el("outtok").value === "300" && tag("outtok") === "learned" && !(asked().traffic && asked().traffic.mix),
    `${el("outtok").value} ${JSON.stringify(asked().traffic)}`);

  // R_q: the plan's learned ratio, at a percentile TPOT, shown and not sent.
  state.k.tpotStat = "p90";
  state.plan = {limits: {chosen: {rate: 5}}, ttft: {line: {atRate: {figure: {value: 0.5}}, target: 1}},
    tpot: {atRate: {figure: {value: 0.04}}, target: 0.05, ratio: {value: 1.31, source: "learned"}}};
  run("renderAtTarget()");
  check("R_q shows the learned ratio", el("ratio").value === "1.31" && tag("ratio") === "learned", `${el("ratio").value} ${tag("ratio")}`);
  check("and is not sent", !(asked().assume && asked().assume.tpotRatio), JSON.stringify(asked().assume));
  el("ratio").value = "1.5";
  el("ratio").onchange();
  check("an edited R_q is sent", asked().assume && asked().assume.tpotRatio === 1.5 && tag("ratio") === "assumed", JSON.stringify(asked().assume));
  el("r-ratio").onclick();
  check("Use learned stops sending R_q", !(asked().assume && asked().assume.tpotRatio) && el("ratio").value === "1.31", el("ratio").value);
  // The p90 R_q typed at p99 is an assumption: it is not p99's learned value.
  el("ratio").value = "1.5";
  el("ratio").onchange();
  state.k.tpotStat = "p99";
  el("ratio").value = "1.31";
  el("ratio").onchange();
  check("another percentile's learned R_q typed is sent", asked().assume && asked().assume.tpotRatio === 1.31 && tag("ratio") === "assumed",
    `${JSON.stringify(asked().assume)} ${tag("ratio")}`);
  el("r-ratio").onclick();
  state.k.tpotStat = "p90";

  // A box committed while focused (Enter, the spinner) is tagged by the
  // value used now, keeps what is in it, and is refilled when it loses focus.
  document.activeElement = el("a-hop");
  el("a-hop").value = "5";
  el("a-hop").onchange();
  check("a box committed while focused is tagged assumed", tag("a-hop") === "assumed" && !el("a-hop").classList.contains("learned") && el("r-a-hop").hidden === false,
    tag("a-hop"));
  el("a-hop").value = "";
  el("a-hop").onchange();
  check("emptied while focused, it is tagged learned and left empty", tag("a-hop") === "learned" && el("a-hop").value === "" && asked().assume === undefined,
    `${tag("a-hop")} ${el("a-hop").value}`);
  document.activeElement = null;
  check("it is refilled when it loses focus", typeof el("a-hop").onblur === "function");
  run("refreshBoxes()");
  check("with the learned value", el("a-hop").value === "2", el("a-hop").value);

  // A V_P from the served peak is a lower bound: shown, tagged, explained.
  load(Object.assign({}, inputs, {terms: [{name: "vp", source: "learned served-peak", value: 29632.58, unit: "tokens/s"}]}));
  check("a lower-bound V_P is shown", el("a-vp").value === "29633", el("a-vp").value);
  check("tagged lower bound", tag("a-vp") === "lower bound", tag("a-vp"));
  check("the real speed is likely higher", /real speed is likely higher/.test(el("l-vp").textContent), el("l-vp").textContent);
  check("and not sent", asked().assume === undefined, JSON.stringify(asked().assume));
  check("the note says the box sends nothing", /sends nothing: type a V_P to size prefill/.test(el("l-vp").textContent), el("l-vp").textContent);
  // Typed, the lower bound is a statement: sent, so prefill is priced.
  el("a-vp").value = "40000";
  el("a-vp").onchange();
  el("a-vp").value = "29633";
  el("a-vp").onchange();
  check("the lower bound typed is sent", asked().assume && asked().assume.vp === 29633 && tag("a-vp") === "assumed", `${JSON.stringify(asked().assume)} ${tag("a-vp")}`);
  el("r-a-vp").onclick();
  check("Use learned puts the lower bound back, not sent", asked().assume === undefined && tag("a-vp") === "lower bound", tag("a-vp"));
  // The answer's "use this V_P" types it: the box says so.
  run("useVP(31000)");
  check("a V_P used from the answer is tagged assumed", asked().assume && asked().assume.vp === 31000 && tag("a-vp") === "assumed" && el("r-a-vp").hidden === false,
    `${JSON.stringify(asked().assume)} ${tag("a-vp")}`);

  // An old link's typed value stays an assumption.
  load(inputs);
  run("linked = new URLSearchParams(\"recording=r&vp=25000&a=0.06\"); applyLink();");
  check("a link's V_P is an assumption", asked().assume && asked().assume.vp === 25000 && tag("a-vp") === "assumed" && el("a-vp").value === "25000",
    `${JSON.stringify(asked().assume)} ${tag("a-vp")} ${el("a-vp").value}`);
  check("a link's a is too, in ms", el("a-a").value === "60" && tag("a-a") === "assumed", `${el("a-a").value} ${tag("a-a")}`);
  check("a box the link leaves is learned", el("a-hop").value === "2" && tag("a-hop") === "learned", `${el("a-hop").value} ${tag("a-hop")}`);

  // Each box is described by its tag and its hint, and has a labelled reset.
  for (const [id, hint] of [["a-vp", "l-vp"], ["a-a", "l-a"], ["a-cv", "l-cv"], ["a-hop", "l-hop"], ["a-itlb", "l-itlb"], ["outtok", "outnote"], ["ratio", "rationote"]]) {
    check(`${id} is described by its tag`, html.includes(`id="${id}" aria-describedby="t-${id} ${hint}"`));
    check(`${id} has a labelled Use learned`, new RegExp(`id="r-${id}" aria-label="Use the learned [^"]+" hidden>Use learned</button>`).test(html));
  }
} catch (e) {
  failures.push("threw: " + (e && e.stack || e));
}

if (failures.length) {
  console.error(`FAIL check-planner-learned: ${failures.length} of ${passed + failures.length}`);
  for (const f of failures) console.error("  - " + f);
  process.exit(1);
}
console.log(`ok check-planner-learned: ${passed} checks`);
