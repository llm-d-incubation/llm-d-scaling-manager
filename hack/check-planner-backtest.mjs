// Runs the capacity planner page's "Check the planner" drawing
// (internal/whatifapi/page/backtest.js) on the backtest the CLI's golden
// holds, and on copies of it cut and altered: the two panels share one time
// axis, every predicted and measured series is drawn, a step not judged is
// greyed with its reason, a prediction above the axis is marked, the
// summary leads with the verdict, and everything taken from the answer is
// escaped. Called by hack/check-planner-scenarios.sh.
//
// Usage: node hack/check-planner-backtest.mjs [scenario.js dayplan.js backtest.js backtest.json]
import fs from "node:fs";
import vm from "node:vm";

const [scen, day, bt, golden] = [
  process.argv[2] || "internal/whatifapi/page/scenario.js",
  process.argv[3] || "internal/whatifapi/page/dayplan.js",
  process.argv[4] || "internal/whatifapi/page/backtest.js",
  process.argv[5] || "cmd/capacity-planner/testdata/backtest-q32-split-json.golden",
];
const ctx = vm.createContext({URLSearchParams});
for (const f of [scen, day, bt]) vm.runInContext(fs.readFileSync(f, "utf8"), ctx, {filename: f});
const {btSVG, btSummaryText, btLegend, btStepAt, btTipHTML, btScoreTable, btStepTable, btPredictionCSV, selfCheckBadge, selfCheckHTML,
  dayTime} = ctx;
const answer = JSON.parse(fs.readFileSync(golden, "utf8"));
const copy = x => JSON.parse(JSON.stringify(x));

let passed = 0;
const failures = [];
function check(name, ok, detail) {
  if (ok) passed++;
  else failures.push(name + (detail === undefined ? "" : ": " + detail));
}

// attrs is every element of tag in markup, as its attributes.
function attrs(markup, tag) {
  const out = [];
  for (const m of markup.matchAll(new RegExp(`<${tag}\\b([^>]*)>`, "g"))) {
    const a = {};
    for (const p of m[1].matchAll(/([\w-]+)="([^"]*)"/g)) a[p[1]] = p[2];
    out.push(a);
  }
  return out;
}

try {
  check("the golden has steps", answer.steps && answer.steps.length > 5, answer.steps && answer.steps.length);
  const g = btSVG(answer);
  const panels = attrs(g.markup, "g").filter(a => a.class === "panel");
  check("two panels", panels.length === 2, panels.length);
  check("the panels share one x domain", new Set(panels.map(p => p["data-x0"] + "/" + p["data-x1"])).size === 1);
  const series = new Set([...attrs(g.markup, "path"), ...attrs(g.markup, "g")].map(a => a["data-series"]).filter(Boolean));
  for (const s of ["ttft-mean-predicted", "ttft-p90-predicted", "ttft-mean-measured", "ttft-p90-measured", "tpot-mean-measured", "tpot-p90-measured"]) {
    check("series " + s + " is drawn", series.has(s));
  }
  const excluded = answer.steps.filter(s => s.excluded);
  const greys = attrs(g.markup, "rect").filter(a => a.class === "btgrey");
  check("every step not judged is greyed in both panels", greys.length === 2 * excluded.length, `${greys.length} vs ${excluded.length}`);
  check("a greyed step names its reason", greys.every(a => a["data-excluded"]));
  const dots = (g.markup.match(/<circle class="dot/g) || []).length;
  check("measured figures are drawn as dots", dots > answer.steps.length, dots);
  // A measured percentile in a bucket too wide to place it is drawn as the
  // bucket, a bar, never as a dot at the figure the grid set.
  const inBucket = answer.steps.reduce((n, s) => n + ["ttft", "tpot"].reduce((m, k) =>
    m + (s[k] || []).filter(p => p.measuredBucket && (p.statistic === "mean" || p.statistic === "p90")).length, 0), 0);
  const bars = attrs(g.markup, "rect").filter(a => a["data-bucket"]).length;
  check("the golden has measured percentiles in a coarse bucket", inBucket > 0, inBucket);
  check("each is drawn as its bucket", bars === inBucket, `${bars} vs ${inBucket}`);

  // A prediction far above everything measured is clipped and marked.
  const big = copy(answer);
  const judged = big.steps.find(s => !s.excluded && s.ttft.find(p => p.statistic === "p90").predicted.value != null);
  judged.ttft.find(p => p.statistic === "p90").predicted.value = 1e6;
  const gb = btSVG(big);
  check("a prediction above the axis is marked", /data-clipped="ttft-p90"/.test(gb.markup));

  // The summary leads with the verdict, then what was not judged.
  const lines = btSummaryText(answer);
  check("the verdict comes first, a sentence a line", lines[0] === answer.verdictLines[0] && answer.verdictLines.length >= 2, lines[0]);
  check("what was not judged follows the verdict", lines[answer.verdictLines.length] === answer.excluded, lines[answer.verdictLines.length]);
  check("the legend names the band and the greyed steps", /own range/.test(btLegend()) && /not judged/.test(btLegend()));

  // The tooltip of a step says its inputs and each figure.
  const s0 = answer.steps[1];
  const at = btStepAt(answer, dayTime(s0.start) + 1);
  check("the step under the cursor is found", at === s0);
  const tip = btTipHTML(answer, s0);
  check("the tooltip names TTFT and TPOT", /TTFT p90/.test(tip) && /TPOT p90/.test(tip), tip);
  check("the score table has a row a statistic", (btScoreTable(answer).match(/<tr>/g) || []).length === 1 + 2 * answer.summary.ttft.length);

  // Everything taken from the answer is escaped.
  const evil = copy(answer);
  evil.verdictLines = ["<script>alert(1)</script>"];
  evil.fit.source = "<img src=x onerror=alert(1)>";
  evil.steps[0].excluded = "\"><script>x</script>";
  evil.steps[0].detail = "<b>bold</b>";
  const ge = btSVG(evil).markup + btTipHTML(evil, evil.steps[0]);
  check("an excluded reason is escaped in the chart and the tooltip", !/<script>/.test(ge), ge.slice(0, 200));
  check("the summary is text, escaped where it is put", btSummaryText(evil)[0] === "<script>alert(1)</script>");

  // A step the planner calls overloaded is marked along the panel's top.
  const sat = copy(answer);
  const js = sat.steps.findIndex(s => !s.excluded);
  for (const p of sat.steps[js].ttft) p.predicted = {refused: {reason: "Saturated", detail: "past the knee"}};
  check("an overloaded step is marked", /data-saturated="ttft"/.test(btSVG(sat).markup));
  check("the step table has a row a step", (btStepTable(answer).match(/<tr>/g) || []).length === 1 + answer.steps.length);

  // A prediction as CSV: its hashes, a header and a row a step, with each
  // statistic predicted and none measured.
  const pred = {kind: "capacity-planner-prediction", model: "m", window: {from: "a", to: "b"}, predictionHash: "ph", inputsHash: "ih",
    plannerVersion: "v", madeAt: "t", steps: answer.steps};
  const csv = btPredictionCSV(pred).trim().split(String.fromCharCode(10));
  check("the CSV holds the hashes", /predictionHash=ph/.test(csv[0]) && /inputsHash=ih/.test(csv[0]), csv[0]);
  check("the CSV has a row a step", csv.length === 2 + answer.steps.length, csv.length);
  check("the CSV has 13 columns", csv[1].split(",").length === 13, csv[1]);

  // The self-check: a badge a status, its lines escaped, and the issue a
  // link to GitHub only.
  const sc = {status: "inconsistent", line: "<b>x</b>", why: "<i>y</i>", trust: "Do not trust <s>",
    issueUrl: "https" + "://github.com/llm-d-incubation/llm-d-scaling-manager/issues/new?template=planner-self-check.yml&diagnostics=a%3Cb"};
  check("an inconsistent self-check is a red badge", selfCheckBadge(sc).cls === "sc-inconsistent");
  check("an unknown status is not run", selfCheckBadge({status: "odd"}).cls === "sc-notRun");
  check("no self-check, no badge", selfCheckBadge(null) === null && selfCheckHTML(null) === "");
  const sh = selfCheckHTML(sc);
  check("the self-check's lines are escaped", !/<b>x|<i>y|<s>/.test(sh), sh);
  check("the issue is offered as a link", /class="scissue"/.test(sh) && /target="_blank"/.test(sh));
  const evilURL = selfCheckHTML(Object.assign({}, sc, {issueUrl: "java" + "script:alert(1)"}));
  check("an issue link that is not GitHub's is not drawn", !/scissue/.test(evilURL));
  const elsewhere = selfCheckHTML(Object.assign({}, sc, {issueUrl: "https" + "://evil.example/github.com/x"}));
  check("an issue link to another host is not drawn", !/scissue/.test(elsewhere));
  check("an ok self-check says no why", !/scwhy/.test(selfCheckHTML({status: "ok", line: "fine", why: "w"})));

  // A step with a refused prediction draws no predicted point and says why.
  const refused = copy(answer);
  for (const p of refused.steps[3].ttft) {
    p.predicted = {refused: {reason: "Saturated", detail: "past the knee"}};
  }
  check("a refused prediction is named in the tooltip", /overloaded/.test(btTipHTML(refused, refused.steps[3])));
} catch (e) {
  failures.push("threw: " + (e && e.stack || e));
}

if (failures.length) {
  console.error(`FAIL check-planner-backtest: ${failures.length} failed, ${passed} passed`);
  for (const f of failures) console.error("  " + f);
  process.exit(1);
}
if (passed < 15) {
  console.error(`FAIL check-planner-backtest: only ${passed} checks ran`);
  process.exit(1);
}
console.log(`ok check-planner-backtest: ${passed} checks`);
