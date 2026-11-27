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
const {btSVG, btPoint, btSummaryText, btLegend, btStepAt, btTipHTML, btScoreTable, btStepTable, btPredictionCSV, selfCheckBadge, selfCheckHTML,
  dayTime, btBatchChip, btUnresolved, btPointClass, btVerdictCard, btNotice, btFailing, btStepsCSV, btBucketTip, btUnscoredList} = ctx;
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
  // A bucket the figure fell in at many steps (the score's sameBucketBand)
  // is one band behind the panel, not a bar a step.
  const unres = btUnresolved(answer);
  const inBand = (k, p) => { const b = unres[`${k}-${p.statistic}`]; return b && p.measuredBucket.lo === b.lo && p.measuredBucket.hi === b.hi; };
  const inBucket = answer.steps.reduce((n, s) => n + ["ttft", "tpot"].reduce((m, k) =>
    m + (s[k] || []).filter(p => p.measuredBucket && (p.statistic === "mean" || p.statistic === "p90") && !inBand(k, p)).length, 0), 0);
  const bars = attrs(g.markup, "rect").filter(a => a["data-bucket"]).length;
  check("the golden has a bucket too wide at many steps", Object.keys(unres).length > 0, JSON.stringify(unres));
  const bands = attrs(g.markup, "rect").filter(a => a["data-unresolved"]);
  check("it is one band", bands.length === Object.keys(unres).filter(k => /-(mean|p90)$/.test(k)).length, bands.length);
  check("the rest are drawn as their buckets", bars === inBucket, `${bars} vs ${inBucket}`);
  check("a measured dot says how it stands", /<circle class="dot[^"]* bt-(within|low|high)"/.test(g.markup));

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
  const scored = ["ttft", "tpot"].reduce((n, k) => n + answer.summary[k].filter(sc => sc.all.steps > 0).length, 0);
  check("the score table has a row a statistic scored", (btScoreTable(answer).match(/<tr>/g) || []).length === 1 + scored, scored);
  check("a statistic not scored is listed with why", btUnscoredList(answer).some(l => l.startsWith("TPOT p90: not scored in")), btUnscoredList(answer).join(" | "));

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
  check("the step table has a row a step, under two header rows", (btStepTable(answer).match(/<tr>/g) || []).length === 2 + answer.steps.length);
  const failing = answer.steps.filter(btFailing);
  check("failing steps only keeps the steps more than 20% off", (btStepTable(answer, true).match(/<tr>/g) || []).length === 2 + Math.max(failing.length, 1), failing.length);
  check("a match reads predicted vs measured, the mark and the miss", /[0-9.]+ vs [0-9.]+ (ms|s) \u2713 [0-9]+%/.test(btStepTable(answer)));
  check("the bucket note is in a tooltip, not the cell", btStepTable(answer).includes("title=\"measured somewhere in") && !/<td[^>]*>[^<]*too wide/.test(btStepTable(answer)));
  const vc = btVerdictCard(answer, "p90", "mean");
  check("the verdict card is TTFT at the target, TTFT mean, TPOT and the range", vc.length === 4 && /^TTFT p90 \d+\/\d+ within 20%$/.test(vc[0].chip) &&
    /^TTFT mean \d+\/\d+ within 20%$/.test(vc[1].chip) && /^TPOT mean/.test(vc[2].chip) && /r\/s/.test(vc[3].chip), JSON.stringify(vc));
  check("at the mean target the card has no second TTFT line", btVerdictCard(answer, "mean", "mean").length === 3);
  check("a figure the bucket cannot place reads as the bucket", /vs [0-9.]+ m?s-[0-9.]+ m?s \u2248/.test(btStepTable(answer)));
  check("the CSV carries each bucket", btStepsCSV(answer).split(String.fromCharCode(10))[0].includes("tpot_p90_bucket_lo_seconds,tpot_p90_bucket_hi_seconds"));
  check("the bucket tip says how to measure it", btBucketTip(answer).includes("Finer histogram edges"));
  const stepsCSV = btStepsCSV(answer).trim().split("\n");
  check("the steps export as CSV, a row a step", stepsCSV.length === 1 + answer.steps.length && stepsCSV[0].startsWith("start,rate,"), stepsCSV[0]);

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

  // A TTFT priced from a lower bound of V_P is at most its figure, and one
  // the fleet measured under is consistent with it, not scored (#369).
  const upper = copy(answer);
  const ub = upper.steps[3].ttft[0];
  ub.predicted.upperBound = true;
  ub.unscored = "upperBound";
  const ubTip = btTipHTML(upper, upper.steps[3]);
  check("an upper-bound prediction reads at most in the tooltip", ubTip.includes("predicted at most ") &&
    ubTip.includes("an upper bound the fleet measured under: consistent, not scored"), ubTip);
  check("an upper-bound prediction reads at most in the step table", btStepTable(upper).includes("\u2264 "));
  check("a plain prediction does not", !btStepTable(answer).includes("at most "));

  // The step table shows mean, p90 and p95 of both figures.
  const head = btStepTable(answer);
  check("the step table has p95 columns", head.includes("<th>TTFT p95</th>") && head.includes("<th>TPOT p95</th>"));

  // The experimental models: each says it is experimental, an abstention
  // says why, and the TPOT distribution says whether its p90 and p95 land
  // in the measured bucket. Without them nothing says v2.
  check("a backtest without the models says nothing of them", !btSummaryText(answer).some(l => /v2/.test(l)) && !/v2/.test(head));
  const v2 = copy(answer);
  v2.rankMixture = {model: "rank-mixture", experimental: true, abstained: "the spread is not calibrated <b>", assumed: ""};
  v2.tpotDistribution = {model: "renewal-lognormal", experimental: true, assumed: "C 0.4 fitted on 30 steps.",
    test: {steps: 10, p90Inside: 7, p95Inside: 6, defaultP90Inside: 2, defaultP95Inside: 1, p90Below: 1, p90Above: 2, p95Below: 3}};
  const vs = v2.steps.find(s => !s.excluded && btPoint(s, "tpot", "p90") && btPoint(s, "tpot", "p95"));
  for (const p of vs.ttft) p.predicted.flags = (p.predicted.flags || []).concat(["RankMixtureAbstained"]);
  for (const p of vs.tpot) if (p.statistic !== "mean") p.predicted.flags = (p.predicted.flags || []).concat(["TPOTDistribution"]);
  vs.tpotDistribution = {sigma2: 0.01, inv: 0.02, p90Inside: true, p95Inside: false};
  const vl = btSummaryText(v2);
  check("the TTFT model's abstention says why", vl.some(l => l.startsWith("EXPERIMENTAL TTFT model v2") && l.includes("abstained: the spread is not calibrated")), vl.join(" | "));
  check("the TPOT model says it is experimental", vl.some(l => l.startsWith("EXPERIMENTAL TPOT model v2") && l.includes("outside the validated region")));
  check("the TPOT model's bracket score is shown beside the planner's, with its misses under and over", vl.some(l => l.includes(
    "p90 in the measured joint bracket at 7 of 10 steps (the planner's own: 2), under it at 1 and over at 2; " +
    "p95 at 6 (the planner's own: 1), under at 3 and over at 0.")), vl.join(" | "));
  check("what was not judged still follows the verdict", vl[v2.verdictLines.length] === v2.excluded);
  const vt = btStepTable(v2), vtip = btTipHTML(v2, vs);
  check("a TPOT v2 figure carries a dagger in the table and the tooltip", vt.includes("\u2020") && vtip.includes("\u2020"));
  check("no table cell says experimental", !/<td[^>]*>[^<]*experimental/i.test(vt), vt.slice(0, 200));
  check("the view's one notice says what the dagger means", btNotice(v2).startsWith("Experimental model (checked on your data)") && btNotice(answer) === "");
  const abst = copy(v2);
  for (const s of abst.steps) for (const k of ["ttft", "tpot"]) for (const p of s[k] || []) p.predicted.flags = (p.predicted.flags || []).filter(f => f !== "TPOTDistribution");
  check("an abstained figure is the planner's own, with no dagger", !btStepTable(abst).includes("\u2020"));
  check("TPOT p90 and p95 say whether they land in the measured bracket", vt.includes("(in the measured bracket)") && vt.includes("(not in the measured bracket)") &&
    vtip.includes("(in the measured bracket)"), vtip);
  const priced = copy(v2);
  priced.rankMixture = {model: "rank-mixture", experimental: true, assumed: "S0 0.1 s."};
  check("a priced TTFT model says it is experimental", btSummaryText(priced).some(l => l.startsWith("EXPERIMENTAL TTFT model v2 (rank-mixture): every TTFT it prices is not validated")));

  // A TPOT percentile the histogram is too coarse to place carries the hint
  // to measure it, in the verdict the page shows as it is.
  const coarse = answer.verdictLines.find(l => /p90 TPOT not scored/.test(l));
  check("the golden has TPOT p90 unscored on a coarse bucket", coarse !== undefined, answer.verdictLines.join(" | "));
  check("the verdict tells how to measure it", coarse !== undefined && coarse.includes("metrics.requestTPOTBuckets") &&
    coarse.includes("--custom-histogram-buckets") && btSummaryText(answer).includes(coarse));
  // A step past the batch its decode line was fitted on carries a chip
  // beside its time with both batches, a pod's or an engine rank's.
  const past = copy(answer);
  past.steps[0].batchBeyondFit = {priced: 251, fitted: 210};
  check("a step past the fit's batch has a chip in the step table", btStepTable(past).includes(">BatchBeyondFit</span>"));
  check("the chip says both batches a pod", btBatchChip(past.steps[0]).includes("fitted on up to 210 sequences a pod; this step runs about 251"),
    btBatchChip(past.steps[0]));
  check("on a pod of several ranks the chip counts a rank's",
    btBatchChip({batchBeyondFit: {priced: 31, fitted: 26, ranks: 8}}).includes("up to 26 sequences an engine rank; this step runs about 31"));
  check("a step inside the fit has no chip", btBatchChip({}) === "");
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
