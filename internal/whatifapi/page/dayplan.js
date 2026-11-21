// The capacity planner page's day plan (POST /v1/dayplan), drawn as pure
// functions: the charts as SVG markup, the tooltip at a time, the legend,
// and the table views. No DOM and no browser API, so
// hack/check-planner-scenarios.sh runs them under node on fixtures. The page
// loads this file after scenario.js (esc) and before planner.js, which puts
// the markup on the page.
//
// Three small multiples share one time axis: the request rate (its mean,
// and a wash from the mean to the p95 of the bucket's cycle rates), then
// needed against had for the prefill and then the decode replicas, each a
// step line. req/s and replicas are different units, so they never share a
// y axis. Where a role had fewer replicas than it needed, its panel is
// washed red; where no fleet within the bound serves a bucket's rate, the
// panel of the role short is hatched. A role the source cannot price at all
// says so in its panel, in words, and draws no needed line.
"use strict";

// DAY_W is the charts' width in the SVG's units; DAY_L and DAY_R the margins
// for the y ticks and for the direct labels at the right.
const DAY_W = 960, DAY_L = 56, DAY_R = 90, DAY_T = 12;
// DAY_PANELS is each panel: its key, its title, and its height.
const DAY_PANELS = [
  {key: "rate", title: "Requests per second", h: 150},
  {key: "prefill", title: "Prefill replicas", h: 110},
  {key: "decode", title: "Decode replicas", h: 110},
];
// DAY_GAP is the space between panels, for the next panel's title.
const DAY_GAP = 30;
// DAY_TICK_STEPS are the time axis' tick spacings, seconds, smallest first.
const DAY_TICK_STEPS = [300, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400];
// DAY_TERMS names a refused term in words.
const DAY_TERMS = {ttft: "prefill's TTFT", tpot: "decode's TPOT", decodeKnee: "decode's knee"};

// dayTime is an RFC 3339 time as seconds since the epoch.
function dayTime(s) { return Date.parse(s) / 1000; }

// dayClock is t, seconds since the epoch, as HH:MM UTC, with the date when
// days says the plan spans more than one.
function dayClock(t, days) {
  const iso = new Date(t * 1000).toISOString();
  return days ? iso.slice(5, 10) + " " + iso.slice(11, 16) : iso.slice(11, 16);
}

// daySpansDays reports whether the plan crosses midnight UTC.
function daySpansDays(plan) {
  return plan.from.slice(0, 10) !== new Date(dayTime(plan.to) * 1000 - 1).toISOString().slice(0, 10);
}

// dayNiceMax is a round top for a y axis over values up to v.
function dayNiceMax(v, integer) {
  if (!(v > 0)) return integer ? 2 : 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) {
    const top = m * p;
    if (top >= v * 1.05 && (!integer || Number.isInteger(top))) return top;
  }
  return Math.ceil(v * 1.1);
}

// dayYTicks are about four round values from 0 to max: whole numbers on a
// replica axis.
function dayYTicks(max, integer) {
  let step = max / 4;
  const p = Math.pow(10, Math.floor(Math.log10(step)));
  const m = step / p;
  step = (m <= 1 ? 1 : m <= 2 ? 2 : m <= 2.5 ? 2.5 : m <= 5 ? 5 : 10) * p;
  if (integer) step = Math.max(1, Math.round(step));
  const out = [];
  for (let v = 0; v <= max + step * 1e-6; v += step) out.push(Number(v.toPrecision(10)));
  return out;
}

// dayXTicks are the time axis' ticks: the smallest spacing that gives at
// most eight, on whole multiples of it in UTC.
function dayXTicks(x0, x1) {
  const span = x1 - x0;
  const step = DAY_TICK_STEPS.find(s => span / s <= 8) || DAY_TICK_STEPS[DAY_TICK_STEPS.length - 1];
  const out = [];
  for (let t = Math.ceil(x0 / step) * step; t <= x1; t += step) out.push(t);
  return out;
}

// dayFmt is a rate for a label: two decimals under 10.
function dayFmt(v) {
  return v == null ? "-" : v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2);
}

// dayLayout is where each panel sits, and the shared x scale: the first
// bucket's start to the last bucket's end.
function dayLayout(plan) {
  const x0 = dayTime(plan.from), x1 = dayTime(plan.to), pw = DAY_W - DAY_L - DAY_R;
  let y = DAY_T;
  const panels = DAY_PANELS.map(p => {
    const out = {key: p.key, title: p.title, top: y + 16, h: p.h};
    y = out.top + p.h + DAY_GAP;
    return out;
  });
  const H = y - DAY_GAP + 22;
  return {x0, x1, pw, H, panels, X: t => DAY_L + pw * (t - x0) / (x1 - x0)};
}

// dayRuns are the runs of consecutive buckets that have a value: a line
// breaks where a bucket has none.
function dayRuns(buckets, value) {
  const runs = [];
  let run = null;
  for (const b of buckets) {
    const v = value(b);
    if (v == null) { run = null; continue; }
    if (!run) { run = []; runs.push(run); }
    run.push({t: dayTime(b.start), v});
  }
  return runs;
}

// dayStep is a run as a step line's path: flat across each bucket, then
// straight up or down to the next.
function dayStep(run, step, X, Y) {
  let d = "";
  for (const [i, q] of run.entries()) {
    d += (i ? "L" : "M") + X(q.t).toFixed(1) + "," + Y(q.v).toFixed(1) + "L" + X(q.t + step).toFixed(1) + "," + Y(q.v).toFixed(1);
  }
  return d;
}

// dayStepBand is a run of [lo, hi] as the polygon between two step lines.
function dayStepBand(run, step, X, Y) {
  const up = [], down = [];
  for (const q of run) {
    up.push(`${X(q.t).toFixed(1)},${Y(q.v[1]).toFixed(1)}`, `${X(q.t + step).toFixed(1)},${Y(q.v[1]).toFixed(1)}`);
    down.push(`${X(q.t).toFixed(1)},${Y(q.v[0]).toFixed(1)}`, `${X(q.t + step).toFixed(1)},${Y(q.v[0]).toFixed(1)}`);
  }
  return up.concat(down.reverse()).join(" ");
}

// dayRoleValue reads a role's count from a bucket's needed or actual; null
// where the role was not priced.
function dayRoleValue(which, role) {
  return b => b[which] && b[which][role] != null ? b[which][role] : null;
}

// dayUnderIn reports whether a sized bucket's red wash shows in role's
// panel: the fleet it had, judged whole at the bucket's rate, falls short
// (verdict under), and the role was sized (decode sized alone washes decode
// only). The wash follows the verdict, never a role's count against the
// cheapest fleet found: another mix may serve the rate.
function dayUnderIn(b, role) {
  return b.status === "sized" && b.verdict === "under" && b.needed && b.needed[role] != null;
}

// dayUnmetIn reports whether a bucket's unmet rate is short in role's
// panel: the role short, or the fleet (both).
function dayUnmetIn(b, role) {
  return b.status === "unmet" && (b.unmetRole === role || b.unmetRole === "fleet" || !b.unmetRole);
}

// dayRoleRefusal is the refusal that leaves role unsized in the whole plan,
// or null: prefill's TTFT, or for both roles decode's knee, where no bucket
// sized the role at all.
function dayRoleRefusal(plan, role) {
  if (plan.buckets.some(k => k.needed && k.needed[role] != null)) return null;
  const rs = plan.refused || [];
  return rs.find(r => r.term === "decodeKnee") || (role === "prefill" ? rs.find(r => r.term === "ttft") : null) || null;
}

// DAY_HATCH is the hatch pattern's definition, by id: thin diagonal lines in
// the muted text colour, which reads on light and dark alike.
function dayHatch(id) {
  return `<defs><pattern id="${id}" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">` +
    `<line class="hatchl" x1="0" y1="0" x2="0" y2="6"/></pattern></defs>`;
}

// dayPlanSVG is the day plan's charts: the SVG's inner markup, its viewBox,
// and the layout the hover reads. Every series is a path with
// data-series, each panel a group with data-panel and the x domain it is
// drawn on (data-x0, data-x1), so that a check can see they share one.
function dayPlanSVG(plan) {
  const lay = dayLayout(plan), X = lay.X, step = plan.step, b = plan.buckets;
  const xr = DAY_L + lay.pw, days = daySpansDays(plan);
  let s = dayHatch("dayhatch");
  for (const p of lay.panels) {
    const integer = p.key !== "rate";
    let vmax = 0;
    for (const k of b) {
      if (integer) {
        for (const w of ["needed", "actual"]) if (k[w] && k[w][p.key] != null) vmax = Math.max(vmax, k[w][p.key]);
      } else if (k.rate) {
        vmax = Math.max(vmax, k.rate.p95);
      }
    }
    const ymax = dayNiceMax(vmax, integer);
    const Y = v => p.top + p.h * (1 - Math.min(v, ymax) / ymax);
    let g = `<text class="ptitle" x="${DAY_L}" y="${p.top - 6}">${esc(p.title)}</text>`;
    // The washes first, under the grid and the lines.
    if (integer) {
      for (const k of b) {
        const t = dayTime(k.start), x = X(t).toFixed(1), w = (X(t + step) - X(t)).toFixed(1);
        if (dayUnderIn(k, p.key)) {
          g += `<rect class="dunder" data-under="${esc(p.key)}" x="${x}" y="${p.top}" width="${w}" height="${p.h}"/>`;
        }
        if (dayUnmetIn(k, p.key)) {
          g += `<rect class="dunder" data-under-unmet="${esc(p.key)}" x="${x}" y="${p.top}" width="${w}" height="${p.h}"/>`;
          g += `<rect class="dunmet" fill="url(#dayhatch)" data-unmet="${esc(p.key)}" x="${x}" y="${p.top}" width="${w}" height="${p.h}"/>`;
        }
      }
    }
    for (const v of dayYTicks(ymax, integer)) {
      if (v > ymax) continue;
      g += `<line class="${v ? "gridl" : "axisl"}" x1="${DAY_L}" x2="${xr}" y1="${Y(v).toFixed(1)}" y2="${Y(v).toFixed(1)}"/>` +
        `<text x="${DAY_L - 8}" y="${(Y(v) + 4).toFixed(1)}" text-anchor="end">${dayFmtTick(v)}</text>`;
    }
    const labels = [];
    const lastOf = value => { for (let i = b.length - 1; i >= 0; i--) if (value(b[i]) != null) return value(b[i]); return null; };
    if (p.key === "rate") {
      for (const run of dayRuns(b, k => k.rate ? [k.rate.mean, k.rate.p95] : null)) {
        g += `<polygon class="dband" data-series="rate-p95" points="${dayStepBand(run, step, X, Y)}"/>`;
      }
      for (const run of dayRuns(b, k => k.rate ? k.rate.mean : null)) {
        g += `<path class="line c-rate" data-series="rate-mean" d="${dayStep(run, step, X, Y)}"/>`;
      }
      const m = lastOf(k => k.rate ? k.rate.mean : null), q = lastOf(k => k.rate ? k.rate.p95 : null);
      if (m != null) labels.push({y: Y(m), text: "mean"});
      if (q != null) labels.push({y: Y(q), text: "p95"});
    } else {
      for (const run of dayRuns(b, dayRoleValue("needed", p.key))) {
        g += `<path class="line c-need" data-series="${p.key}-needed" d="${dayStep(run, step, X, Y)}"/>`;
      }
      for (const run of dayRuns(b, dayRoleValue("actual", p.key))) {
        g += `<path class="line c-had" data-series="${p.key}-actual" d="${dayStep(run, step, X, Y)}"/>`;
      }
      const n = lastOf(dayRoleValue("needed", p.key)), a = lastOf(dayRoleValue("actual", p.key));
      if (n != null) labels.push({y: Y(n), text: "needed"});
      if (a != null) labels.push({y: Y(a), text: "had"});
      const r = dayRoleRefusal(plan, p.key);
      if (r) {
        g += `<text class="pnote" data-refused="${esc(p.key)}" x="${DAY_L + lay.pw / 2}" y="${p.top + p.h / 2}" text-anchor="middle">` +
          `${esc(p.title)} not sized: ${esc(DAY_TERMS[r.term] || r.term)} refused (${esc(r.reason)})</text>`;
      } else if (p.key === "decode" && b.some(k => k.needed && k.needed.atLeast)) {
        g += `<text class="pnote" data-lower-bound="decode" x="${DAY_L + 4}" y="${p.top + 12}">` +
          `where prefill is not priced, needed is at least: decode's own limit alone</text>`;
      }
    }
    g += dayLabels(labels, xr + 6, p.top, p.top + p.h);
    s += `<g class="panel" data-panel="${p.key}" data-x0="${lay.x0}" data-x1="${lay.x1}">${g}</g>`;
  }
  const bottom = lay.panels[lay.panels.length - 1];
  const yb = bottom.top + bottom.h;
  for (const t of dayXTicks(lay.x0, lay.x1)) {
    s += `<line class="gridl" x1="${X(t).toFixed(1)}" x2="${X(t).toFixed(1)}" y1="${yb}" y2="${yb + 4}"/>` +
      `<text x="${X(t).toFixed(1)}" y="${yb + 16}" text-anchor="middle">${dayClock(t, days)}</text>`;
  }
  s += `<text x="${xr}" y="${yb + 16}" text-anchor="start" dx="8">UTC</text>`;
  s += `<line class="xh" x1="0" x2="0" y1="${lay.panels[0].top}" y2="${yb}" visibility="hidden"/>`;
  return {markup: s, viewBox: `0 0 ${DAY_W} ${lay.H}`, x0: lay.x0, x1: lay.x1, L: DAY_L, pw: lay.pw, W: DAY_W};
}

// dayFmtTick is a y tick: whole where it is.
function dayFmtTick(v) { return Number.isInteger(v) ? String(v) : String(Number(v.toPrecision(3))); }

// dayLabels are the direct labels at the right of a panel: each series'
// name at its line's end, kept inside the panel. Two whose lines end closer
// than a line of text are one label, "needed, had", rather than one nudged
// off its line.
function dayLabels(labels, x, top, bottom) {
  const sorted = labels.slice().sort((a, b) => a.y - b.y);
  const placed = [];
  for (const l of sorted) {
    const y = Math.min(Math.max(l.y + 4, top + 10), bottom);
    const prev = placed[placed.length - 1];
    if (prev && y - prev.y < 12) prev.text += ", " + l.text;
    else placed.push({y, text: l.text});
  }
  return placed.map(l => `<text class="lbl" x="${x}" y="${l.y.toFixed(1)}">${esc(l.text)}</text>`).join("");
}

// dayLegend is the charts' legend: a swatch and a name for every mark.
function dayLegend() {
  const sw = cls => `<svg viewBox="0 0 18 10" aria-hidden="true"><line class="line ${cls}" x1="1" x2="17" y1="5" y2="5"/></svg>`;
  const box = (cls, fill) => `<svg viewBox="0 0 18 10" aria-hidden="true">${fill ? dayHatch("dayhatch-l") : ""}` +
    `<rect class="${cls}"${fill ? ` fill="url(#dayhatch-l)"` : ""} x="1" y="1" width="16" height="8"/></svg>`;
  return `<span>${sw("c-rate")}rate, mean of the bucket's cycles</span><span>${box("dband")}spread within the bucket, mean to p95</span>` +
    `<span>${sw("c-need")}replicas needed</span><span>${sw("c-had")}replicas the fleet had</span>` +
    `<span>${box("dunder")}the fleet it had falls short of the rate (SLO risk)</span><span>${box("dunmet", true)}no fleet within the bound serves the rate</span>`;
}

// DAY_STATUS is a bucket's status in words.
const DAY_STATUS = {sized: "sized", tooLittleData: "too little data", noData: "no data", unmet: "beyond the bound",
  refused: "not sized: the source refused a term"};
// DAY_VERDICT is a verdict in words.
const DAY_VERDICT = {under: "under: SLO risk", over: "over: waste", match: "matches", refused: "refused"};

// dayBucketAt is the bucket that holds t, seconds since the epoch, or null.
function dayBucketAt(plan, t) {
  for (const b of plan.buckets) {
    const s = dayTime(b.start);
    if (t >= s && t < s + plan.step) return b;
  }
  return null;
}

// dayNeededText is a bucket's needed fleet in words: decode sized alone is
// a lower bound.
function dayNeededText(n) {
  return n.prefill != null ? `${n.prefill} prefill, ${n.decode} decode` : `at least ${n.decode} decode (prefill not priced)`;
}

// dayTip is the tooltip of a bucket: every value at that time, as rows of
// plain text.
function dayTip(plan, b) {
  const rows = [];
  const row = (k, v) => rows.push([k, v]);
  row("status", DAY_STATUS[b.status] || b.status);
  if (b.rate) {
    row("rate mean", dayFmt(b.rate.mean) + " r/s");
    row("rate p95", dayFmt(b.rate.p95) + " r/s");
    row("rate max", dayFmt(b.rate.max) + " r/s");
  }
  if (b.sizedRate != null) row("sized for", dayFmt(b.sizedRate) + " r/s (" + b.sizedFor + ")");
  row("requests", b.requests ? b.requests.toFixed(0) : "0");
  if (b.needed) row("needed", dayNeededText(b.needed));
  if (b.actual) row("had", `${b.actual.prefill} prefill, ${b.actual.decode} decode`);
  if (b.hadLimit && b.hadLimit.value != null) row("had fleet serves", dayFmt(b.hadLimit.value) + " r/s");
  if (b.verdict) row("verdict", DAY_VERDICT[b.verdict] || b.verdict);
  if (b.unmet) row("beyond the bound", `${b.unmetRole || "fleet"}: ${b.unmet.reason}`);
  if (b.shape && b.shape.promptTokens != null) row("prompt, output", `${b.shape.promptTokens.toFixed(0)}, ${b.shape.outputTokens != null ? b.shape.outputTokens.toFixed(0) : "-"} tokens (${b.shape.source})`);
  if (b.flags && b.flags.length) row("flags", b.flags.map(f => f === "Extrapolated" ? "EXTRAPOLATED" : f).join(", "));
  return {title: dayClock(dayTime(b.start), daySpansDays(plan)) + " to " + dayClock(dayTime(b.start) + plan.step, false) + " UTC", rows};
}

// dayTipHTML is dayTip as the page's tooltip markup.
function dayTipHTML(plan, b) {
  const t = dayTip(plan, b);
  return `<div class="t">${esc(t.title)}</div>` + t.rows.map(([k, v]) => `<div class="r"><span>${esc(k)}</span><span>${esc(v)}</span></div>`).join("");
}

// dayRefusedHTML is the plan's refusals, each in words: what the source
// could not price, what to do, and what the plan did instead.
function dayRefusedHTML(plan) {
  return (plan.refused || []).map(r => `<li><b>${esc(DAY_TERMS[r.term] || r.term)} refused (${esc(r.reason)}), ${r.buckets} bucket(s).</b> ` +
    `${esc(r.detail)}. ${esc(r.effect)} <span class="hint">${esc(r.hint)}</span></li>`).join("");
}

// dayHoursTable is the hourly table: one row per hour.
function dayHoursTable(plan) {
  const n = x => x == null ? "-" : esc(String(x));
  let s = "<table><tr><th>hour (UTC)</th><th>rate mean</th><th>rate p95</th><th>needed P</th><th>needed D</th><th>had P</th><th>had D</th><th>beyond the bound</th><th></th></tr>";
  for (const h of plan.hours) {
    s += `<tr><td>${esc(h.start.slice(0, 16).replace("T", " "))}</td><td>${h.rate ? dayFmt(h.rate.mean) : "-"}</td><td>${h.rate ? dayFmt(h.rate.p95) : "-"}</td>` +
      `<td>${n(h.needed && h.needed.prefill)}</td><td>${n(h.needed && h.needed.decode)}</td><td>${n(h.actual && h.actual.prefill)}</td><td>${n(h.actual && h.actual.decode)}</td>` +
      `<td>${n(h.unmet)}</td><td>${esc(h.verdict ? DAY_VERDICT[h.verdict] : "not sized")}</td></tr>`;
  }
  return s + "</table>";
}

// dayBucketsTable is every bucket, the table view of the charts: what the
// tooltip shows, keyboard and screen reader alike.
function dayBucketsTable(plan) {
  const n = x => x == null ? "-" : esc(String(x));
  let s = "<table><tr><th>bucket (UTC)</th><th>status</th><th>rate mean</th><th>rate p95</th><th>sized for</th><th>needed P</th><th>needed D</th>" +
    "<th>had P</th><th>had D</th><th>verdict</th><th>why not sized</th><th>flags</th></tr>";
  for (const b of plan.buckets) {
    const why = b.unmet ? `${b.unmetRole || "fleet"}: ${b.unmet.reason}` : b.status === "refused" ? "see the refusals above" : "";
    s += `<tr><td>${esc(b.start.slice(0, 16).replace("T", " "))}</td><td>${esc(DAY_STATUS[b.status] || b.status)}</td>` +
      `<td>${b.rate ? dayFmt(b.rate.mean) : "-"}</td><td>${b.rate ? dayFmt(b.rate.p95) : "-"}</td>` +
      `<td>${b.sizedRate != null ? dayFmt(b.sizedRate) + " (" + esc(b.sizedFor) + ")" : "-"}</td>` +
      `<td>${n(b.needed && b.needed.prefill)}</td><td>${n(b.needed && b.needed.decode)}</td><td>${n(b.actual && b.actual.prefill)}</td><td>${n(b.actual && b.actual.decode)}</td>` +
      `<td>${esc(b.verdict ? DAY_VERDICT[b.verdict] || b.verdict : "")}</td><td>${esc(why)}</td><td>${esc((b.flags || []).join(", "))}</td></tr>`;
  }
  return s + "</table>";
}

// daySummaryText is the summary in a few lines of plain text.
function daySummaryText(plan) {
  const s = plan.summary, c = s.counts, out = [];
  const days = daySpansDays(plan);
  const hours = hs => hs.length ? hs.map(h => dayClock(dayTime(h), days)).join(", ") : "none";
  if (s.peakNeeded) {
    out.push(`Needed at the busiest bucket ${dayNeededText(s.peakNeeded)}; at the quietest ${dayNeededText(s.minNeeded)}.`);
    const g = s.sizedOnGpus;
    out.push((g ? `The fewest GPUs, at ${g.prefill} a prefill and ${g.decode} a decode replica` : "The fewest replicas in all") +
      ": another mix may serve the rate too. The fleet the bucket had is judged whole, at the bucket's rate.");
    if (s.neededGpuHours != null) out.push(`Over the ${c.sized} sized buckets: GPU-hours needed ${s.neededGpuHours.toFixed(1)}, had ${s.hadGpuHours.toFixed(1)}.`);
    else if (c.prefillNotPriced) out.push(`No GPU-hours of the need: prefill is not priced in ${c.prefillNotPriced} of the ${c.sized} sized buckets.`);
    out.push(`Hours under (SLO risk): ${hours(s.underHours)}. Hours over (waste): ${hours(s.overHours)}.`);
  } else {
    out.push("No bucket was sized: see the refusals above.");
  }
  if (s.hadGpuHoursAll != null) out.push(`The fleet ran ${s.hadGpuHoursAll.toFixed(1)} GPU-hours over every bucket with data.`);
  out.push(`${c.sized} buckets sized (${c.sizedForP95} for their p95, ${c.sizedForMean} for their mean, ${c.sizedForMax} for their max), ` +
    `${c.unmet} beyond the bound, ${c.refused} refused, ${c.tooLittleData} with too little data, ${c.noData} with no data.`);
  return out;
}
