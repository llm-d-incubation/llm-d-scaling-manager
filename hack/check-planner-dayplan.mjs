// Runs the capacity planner page's day plan drawing
// (internal/whatifapi/page/dayplan.js) on the day plans the CLI's goldens
// hold, and on those plans cut into pieces: the charts share one time axis,
// every series is drawn, a gap breaks the lines, a bucket short of what it
// needed is washed, a bucket beyond the bound is hatched in the panel of the
// role short only, a role the source cannot price says so and draws no
// needed line, and everything taken from a plan is escaped. Called by
// hack/check-planner-scenarios.sh.
//
// Usage: node hack/check-planner-dayplan.mjs [scenario.js dayplan.js plan.json decode-only-plan.json]
import fs from "node:fs";
import vm from "node:vm";

const [scen, day, golden, decodeOnly] = [
  process.argv[2] || "internal/whatifapi/page/scenario.js",
  process.argv[3] || "internal/whatifapi/page/dayplan.js",
  process.argv[4] || "cmd/capacity-planner/testdata/day-plan-ols-sweep-json.golden",
  process.argv[5] || "cmd/capacity-planner/testdata/day-plan-staircase-b-on-json.golden",
];
const ctx = vm.createContext({URLSearchParams});
for (const f of [scen, day]) vm.runInContext(fs.readFileSync(f, "utf8"), ctx, {filename: f});
const {dayPlanSVG, dayBucketAt, dayTip, dayTipHTML, dayHoursTable, dayBucketsTable, dayRefusedHTML, daySummaryText, dayLegend} = ctx;
const plan = JSON.parse(fs.readFileSync(golden, "utf8"));
const half = JSON.parse(fs.readFileSync(decodeOnly, "utf8"));
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

// xs is every x coordinate of a path's d.
function xs(d) {
  return [...d.matchAll(/[ML](-?[\d.]+),/g)].map(m => +m[1]);
}

// texts is every text element's content.
function texts(markup) {
  return [...markup.matchAll(/<text\b[^>]*>([^<]*)<\/text>/g)].map(m => m[1]);
}

try {
  check("the golden plan has buckets", plan.buckets && plan.buckets.length > 5, plan.buckets && plan.buckets.length);
  const sized = plan.buckets.filter(b => b.status === "sized");
  check("the golden plan sizes some buckets", sized.length > 0);
  const g = dayPlanSVG(plan);
  const panels = attrs(g.markup, "g").filter(a => a.class === "panel");
  check("three panels", panels.length === 3, panels.map(p => p["data-panel"]).join(","));
  check("panels in order", panels.map(p => p["data-panel"]).join(",") === "rate,prefill,decode");
  const x0 = Date.parse(plan.from) / 1000, x1 = Date.parse(plan.to) / 1000;
  for (const p of panels) {
    check(`${p["data-panel"]} shares the x domain`, +p["data-x0"] === x0 && +p["data-x1"] === x1, `${p["data-x0"]}..${p["data-x1"]}`);
  }
  const series = new Set(attrs(g.markup, "path").map(a => a["data-series"]).concat(attrs(g.markup, "polygon").map(a => a["data-series"])));
  for (const s of ["rate-mean", "rate-p95", "prefill-needed", "prefill-actual", "decode-needed", "decode-actual"]) {
    check(`draws ${s}`, series.has(s), [...series].join(","));
  }
  // Every mark sits inside the plot, between the left margin and the labels.
  const L = g.L, R = g.L + g.pw;
  for (const p of attrs(g.markup, "path")) {
    const x = xs(p.d);
    check(`${p["data-series"]} inside the plot`, x.length > 0 && Math.min(...x) >= L - 0.1 && Math.max(...x) <= R + 0.1, `${Math.min(...x)}..${Math.max(...x)}`);
  }
  // The red wash follows the verdict, the fleet it had judged whole: one
  // rectangle per sized bucket under, in the panel of each role it sized.
  for (const role of ["prefill", "decode"]) {
    const want = plan.buckets.filter(b => b.status === "sized" && b.verdict === "under" && b.needed[role] != null).length;
    const got = attrs(g.markup, "rect").filter(a => a["data-under"] === role).length;
    check(`${role} washed where short`, got === want, `${got} washed, ${want} short`);
  }
  // A bucket whose prefill reads short, but whose fleet serves the rate
  // whole, is not washed.
  const fine = copy(plan);
  const fb = fine.buckets.find(b => b.status === "sized" && b.needed.prefill != null);
  fb.needed.prefill = fb.actual.prefill + 2;
  fb.verdict = "match";
  check("a fleet that serves whole is not washed", attrs(dayPlanSVG(fine).markup, "rect").filter(a => a["data-under"]).length ===
    attrs(g.markup, "rect").filter(a => a["data-under"]).length - (plan.buckets.find(b => b.start === fb.start).verdict === "under" ? (fb.needed.prefill != null ? 2 : 1) : 0));

  // The hatch: only in the panel of the role short (both for the fleet). The
  // golden has no bucket beyond the bound: three sized ones are made so,
  // one short of prefill, one of decode, one of both.
  const bound = copy(plan);
  const made = bound.buckets.filter(b => b.status === "sized").slice(0, 3);
  check("three sized buckets to put beyond the bound", made.length === 3);
  made.forEach((b, i) => {
    b.status = "unmet";
    b.unmetRole = ["prefill", "decode", "fleet"][i];
    b.unmet = {reason: "ReplicaBoundReached", detail: "At 32 prefill and 32 decode replicas, the bound searched"};
    delete b.needed;
    delete b.verdict;
  });
  const unmet = bound.buckets.filter(b => b.status === "unmet");
  const bg = dayPlanSVG(bound).markup;
  for (const role of ["prefill", "decode"]) {
    const got = attrs(bg, "rect").filter(a => a["data-unmet"] === role).length;
    check(`${role} hatched only where it is short`, got === 2, `${got} hatched, want 2`);
  }
  for (const role of ["prefill", "decode"]) {
    const got = attrs(bg, "rect").filter(a => a["data-under-unmet"] === role).length;
    check(`${role} washed red where beyond the bound too`, got === 2, `${got}`);
  }
  check("the hatch is the pattern", attrs(bg, "rect").filter(a => a["data-unmet"]).every(a => a.fill === "url(#dayhatch)") && bg.includes('<pattern id="dayhatch"'));
  check("no hatch where nothing is beyond the bound", attrs(g.markup, "rect").filter(a => a["data-unmet"]).length === 0);
  // The labels name the series, never a value.
  const labels = attrs(g.markup, "text").filter(a => a.class === "lbl");
  const names = texts(g.markup).filter(t => /^(mean|p95|needed|had)(, (mean|p95|needed|had))*$/.test(t));
  check("every direct label is a series name", labels.length > 0 && names.length === labels.length, texts(g.markup).join("|"));
  check("no label is a number", !texts(g.markup).some(t => /^(mean|p95|needed|had) [\d.]/.test(t)));
  // No label sits on another.
  const ls = labels.map(a => ({x: +a.x, y: +a.y}));
  for (let i = 0; i < ls.length; i++) {
    for (let j = i + 1; j < ls.length; j++) {
      check(`labels ${i} and ${j} apart`, ls[i].x !== ls[j].x || Math.abs(ls[i].y - ls[j].y) >= 11.9, JSON.stringify([ls[i], ls[j]]));
    }
  }
  // The legend has a swatch for every mark, the hatch with its own pattern.
  const legend = dayLegend();
  for (const cls of ["c-rate", "dband", "c-need", "c-had", "dunder", "dunmet"]) check(`the legend shows ${cls}`, legend.includes(`class="${cls}"`) || legend.includes(`class="line ${cls}"`));
  check("the legend's hatch is filled", legend.includes('fill="url(#dayhatch-l)"') && legend.includes('<pattern id="dayhatch-l"'));

  // Needed is drawn first, wide, and had over it, so that where they are
  // equal both show.
  for (const role of ["prefill", "decode"]) {
    check(`${role} had drawn over needed`, g.markup.indexOf(`data-series="${role}-actual"`) > g.markup.indexOf(`data-series="${role}-needed"`));
  }

  // A gap: a bucket with no data in the middle breaks the needed line into two.
  const gap = copy(plan);
  const mid = gap.buckets.findIndex((b, i) => i > 0 && b.status === "sized" && gap.buckets[i - 1].status === "sized" &&
    gap.buckets[i + 1] && gap.buckets[i + 1].status === "sized");
  check("the golden has three sized buckets in a row", mid > 0);
  const runs = s => attrs(dayPlanSVG(s).markup, "path").filter(a => a["data-series"] === "decode-needed").length;
  const before = runs(gap);
  gap.buckets[mid] = {start: gap.buckets[mid].start, samples: 0, requests: 0, shape: {source: "window"}, status: "noData"};
  check("a gap breaks the line", runs(gap) === before + 1, `${before} then ${runs(gap)}`);

  // A plan whose prefill the source cannot price: decode alone, and the
  // prefill panel says why, with no needed line and no hatch.
  check("the decode-only golden refuses prefill", (half.refused || []).some(r => r.term === "ttft"));
  const hg = dayPlanSVG(half).markup;
  const hs = new Set(attrs(hg, "path").map(a => a["data-series"]));
  check("decode-only draws decode needed", hs.has("decode-needed"));
  check("decode-only draws no prefill needed", !hs.has("prefill-needed"));
  check("decode-only draws what prefill had", hs.has("prefill-actual"));
  check("the prefill panel says it is not sized", attrs(hg, "text").some(a => a["data-refused"] === "prefill") &&
    texts(hg).some(t => t.includes("Prefill replicas not sized") && t.includes("NoPrefillCapacity")));
  check("the decode panel says its need is a lower bound", attrs(hg, "text").some(a => a["data-lower-bound"] === "decode") &&
    texts(hg).some(t => t.includes("needed is at least")));
  const priced = copy(plan);
  priced.refused = [];
  priced.buckets.forEach(k => { if (k.needed && k.needed.prefill == null) { delete k.needed; k.status = "tooLittleData"; } });
  check("a fully priced plan has no lower-bound note", !attrs(dayPlanSVG(priced).markup, "text").some(a => a["data-lower-bound"]));
  check("a plan that priced prefill somewhere says nothing of it refused", !attrs(g.markup, "text").some(a => a["data-refused"] === "prefill"));
  check("but the decode panel says its lower bound", attrs(g.markup, "text").some(a => a["data-lower-bound"] === "decode"));
  check("the tip says at least", dayTip(half, half.buckets.find(x => x.needed)).rows.some(r => r[0] === "needed" && r[1].startsWith("at least")));
  check("no hatch for a refusal of the source", attrs(hg, "rect").filter(a => a["data-unmet"]).length === 0);
  check("the refusals say what to do", dayRefusedHTML(half).includes("--assume-vp"));
  check("decode-only summary names no prefill figure", daySummaryText(half)[0].includes("prefill not priced"));
  const nothing = copy(half);
  nothing.buckets.forEach(b => { if (b.status === "sized") { b.status = "refused"; delete b.needed; b.verdict = "refused"; } });
  nothing.summary.peakNeeded = null;
  check("a plan with nothing sized says so", daySummaryText(nothing)[0].startsWith("No bucket was sized"));

  // The tooltip names every value of the bucket under the pointer.
  const b = sized[0], t = Date.parse(b.start) / 1000 + plan.step / 2;
  check("the bucket at a time", dayBucketAt(plan, t) === b);
  check("no bucket past the end", dayBucketAt(plan, x1 + 1) === null);
  const tip = dayTip(plan, b).rows.map(r => r[0]);
  for (const k of ["rate mean", "rate p95", "sized for", "needed", "had", "verdict"]) check(`the tip has ${k}`, tip.includes(k), tip.join(","));
  check("the hour table has every hour", (dayHoursTable(plan).match(/<tr>/g) || []).length === plan.hours.length + 1);
  check("the bucket table has every bucket", (dayBucketsTable(plan).match(/<tr>/g) || []).length === plan.buckets.length + 1);
  check("the bucket table says why a bucket was not sized", dayBucketsTable(bound).includes("prefill: ReplicaBoundReached"));
  check("the tip says the bound", dayTip(bound, unmet[1]).rows.some(r => r[0] === "beyond the bound" && r[1] === "decode: ReplicaBoundReached"));
  check("the summary says the need that holds every bucket", daySummaryText(plan)[0].startsWith("Needed to hold every bucket"));
  check("and the busiest bucket's own", daySummaryText(plan)[0].includes("the busiest bucket alone"));
  check("the hour table has the busiest bucket's fleet", dayHoursTable(plan).includes("<th>busiest bucket</th>"));

  // Everything a plan carries is text, never markup: a flag, a reason, a
  // refusal's words, the hour's verdict.
  const evil = copy(half);
  const tag = "<img src=x onerror=1>";
  const eb = evil.buckets.find(x => x.status === "sized");
  eb.flags = [tag];
  eb.sizedFor = tag;
  evil.refused[0].detail = tag;
  evil.refused[0].hint = tag;
  evil.hours[0].verdict = "x" + tag;
  for (const [name, html] of [["the tooltip", dayTipHTML(evil, eb)], ["the bucket table", dayBucketsTable(evil)],
    ["the refusals", dayRefusedHTML(evil)]]) {
    check(`${name} escapes`, html.includes("&lt;img") && !html.includes("<img"), html.slice(0, 120));
  }
  const hours = dayHoursTable(evil);
  check("the hour table writes no markup of a plan", !hours.includes("<img"));
} catch (e) {
  failures.push("threw: " + (e && e.stack || e));
}

if (failures.length) {
  console.error(`FAIL: ${failures.length} of ${passed + failures.length} day plan checks failed:`);
  for (const f of failures) console.error("  " + f);
  process.exit(1);
}
if (passed < 40) {
  console.error(`FAIL: only ${passed} day plan checks ran`);
  process.exit(1);
}
console.log(`ok: ${passed} day plan checks passed`);
