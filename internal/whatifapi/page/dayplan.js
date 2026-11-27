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
const DAY_TERMS = {ttft: "prefill's TTFT", tpot: "decode's TPOT", decodeKnee: "decode's knee", window: "the busiest bucket"};

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
          `${esc(p.title)} not sized: ${esc(r.reason === "NoPrefillCapacity" ? "prefill capacity unknown" : (DAY_TERMS[r.term] || r.term) + " not learned")}</text>`;
      } else if (p.key === "decode" && b.some(k => k.needed && k.needed.atLeast)) {
        g += `<text class="pnote" data-lower-bound="decode" x="${DAY_L + lay.pw / 2}" y="${p.top + p.h - 6}" text-anchor="middle">` +
          `needed is at least: decode alone, prefill not sized</text>`;
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
  refused: "not sized: a term was not learned"};
// DAY_VERDICT is a verdict in words.
const DAY_VERDICT = {under: "under: SLO risk", over: "over: waste", match: "matches", refused: "refused"};
// DAY_VERDICT_DECODE is a verdict on decode alone (scope "decode only"): the
// source did not price prefill, so a decode replica to spare is no fleet's
// waste.
const DAY_VERDICT_DECODE = {under: "under: SLO risk (decode only)", over: "decode only: a decode replica to spare", match: "decode only: matches"};
// DAY_DECODE_ONLY says why a decode-only verdict is not a recommendation.
const DAY_DECODE_ONLY = "Prefill was not sized, so this is not a fleet recommendation.";

// DAY_TYPE_VP points a day plan that could not price prefill to the box
// that lets it: a stated V_P sizes prefill in every bucket.
const DAY_TYPE_VP = "To size prefill, type a prefill speed (V_P per pod) under Assumptions, above the tabs, and plan the day again.";

// dayVerdict is a bucket's or an hour's verdict in words, "" with none: a
// bucket the model would call over whose measured TTFT missed its target
// says so in place of a verdict.
function dayVerdict(x) {
  if (!x.verdict) return (x.flags || []).includes("MeasuredTTFTOverTarget") ? "not judged: measured TTFT over target" : "";
  return (x.scope === "decode only" ? DAY_VERDICT_DECODE[x.verdict] : null) || DAY_VERDICT[x.verdict] || x.verdict;
}

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
  if (n.prefill == null) return `at least ${n.decode} decode (prefill not sized)`;
  return n.atLeast ? `at least ${n.prefill} prefill, ${n.decode} decode (prefill not sized in some buckets)` : `${n.prefill} prefill, ${n.decode} decode`;
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
  if (b.notServing) row("asked, not serving", `${b.notServing.prefill} prefill, ${b.notServing.decode} decode`);
  // Decode judged alone has no fleet limit: the figure would be decode's.
  if (b.hadLimit && b.hadLimit.value != null && b.scope !== "decode only") row("had fleet serves", dayFmt(b.hadLimit.value) + " r/s");
  if (dayVerdict(b)) row("verdict", dayVerdict(b));
  if (b.trust) row("self-check", b.trust);
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
  // An unknown prefill capacity reads as the answer view's card does; the
  // code stays in small type.
  return (plan.refused || []).map(r => r.reason === "NoPrefillCapacity" ?
    `<li title="${esc(r.reason)}"><b>${r.buckets} bucket(s):</b> ${esc(noPrefillWords({reason: r.reason, detail: r.detail, flags: /only the rate a prefill pod/.test(r.detail) ? ["VPLowerBound"] : []}))} ` +
      `Decode is sized alone. ${esc(DAY_TYPE_VP)}</li>` :
    `<li><b>${esc(DAY_TERMS[r.term] || r.term)} refused (${esc(r.reason)}), ${r.buckets} bucket(s).</b> ` +
    `${esc(r.detail)}. ${esc(r.effect)} <span class="hint">${esc(r.hint)}</span></li>`).join("");
}

// dayHoursTable is the hourly table: one row per hour.
function dayHoursTable(plan) {
  const n = x => x == null ? "-" : esc(String(x));
  const cell = x => !x ? "-" : x.prefill != null ? `${x.atLeast ? ">=" : ""}${x.prefill}+${x.decode}` : `D>=${x.decode}`;
  const lead = !!plan.lead;
  let s = "<table><tr><th>hour (UTC)</th><th>rate mean</th><th>rate p95</th><th>needed P</th><th>needed D</th><th>busiest bucket</th>" +
    (lead ? "<th>held (with the scale-up lead)</th>" : "") + "<th>had P</th><th>had D</th><th>beyond the bound</th><th></th></tr>";
  for (const h of plan.hours) {
    const busiest = !h.busiest ? "-" : h.busiest.prefill != null ? `${h.busiest.prefill}+${h.busiest.decode}` : `D>=${h.busiest.decode}`;
    const verdict = (dayVerdict(h) || "not sized") + (h.notConfirmed ? ", need not confirmed" : "");
    const atLeast = h.needed && h.needed.atLeast ? ">=" : "";
    const had = role => h.actual == null ? null : h.actual[role] + (h.notServing && h.notServing[role] ? `(+${h.notServing[role]})` : "");
    s += `<tr><td>${esc(h.start.slice(0, 16).replace("T", " "))}</td><td>${h.rate ? dayFmt(h.rate.mean) : "-"}</td><td>${h.rate ? dayFmt(h.rate.p95) : "-"}</td>` +
      `<td>${n(h.needed && h.needed.prefill != null ? atLeast + h.needed.prefill : null)}</td><td>${n(h.needed ? atLeast + h.needed.decode : null)}</td>` +
      `<td>${esc(busiest)}</td>` + (lead ? `<td>${esc(cell(h.held))}</td>` : "") +
      `<td>${n(had("prefill"))}</td><td>${n(had("decode"))}</td>` +
      `<td>${n(h.unmet)}</td><td>${esc(verdict + (h.trust ? "; " + h.trust : ""))}</td></tr>`;
  }
  return s + "</table>";
}

// dayModelsText is the experimental models a day plan was asked to price
// with, in words: abstained, or how many sized buckets each priced, marked
// with the dagger; "" with none. The notes say why the rest are the
// planner's own.
function dayModelsText(plan) {
  const sized = plan.summary && plan.summary.counts ? plan.summary.counts.sized || 0 : 0;
  return [[plan.ttftModel, "RankMixtureTTFT"], [plan.tpotModel, "TPOTDistribution"]].filter(([m]) => m).map(([m, flag]) => m.abstained ?
    `${m.label}: abstained (${m.abstained}); the planner's own prices every bucket.` :
    `${m.label}: priced ${(plan.buckets || []).filter(b => b.limit && (b.limit.flags || []).includes(flag)).length} of the ${sized} sized buckets, ` +
      `each marked ${DAGGER} (not validated: outside the validated region); the notes say why the rest are the planner's own.`).join(" ");
}

// DAY_MODEL_FLAGS are the flags a model the plan was asked to price with
// puts on a bucket's limit: what the dagger in the bucket table marks, as
// the command's hours table marks them. Other experimental flags (a
// plateau V_P, the TTFT switches) are named in the flags column only.
const DAY_MODEL_FLAGS = ["RankMixtureTTFT", "TPOTDistribution"];

// dayModelDagger is the dagger where a model set the limit, else "".
function dayModelDagger(limit) {
  return limit && (limit.flags || []).some(f => DAY_MODEL_FLAGS.includes(f)) ? DAGGER : "";
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
      `<td>${n(b.needed && b.needed.prefill)}${b.needed ? dayModelDagger(b.limit) : ""}</td><td>${n(b.needed && b.needed.decode)}${b.needed ? dayModelDagger(b.limit) : ""}</td>` +
      `<td>${n(b.actual && b.actual.prefill)}</td><td>${n(b.actual && b.actual.decode)}</td>` +
      `<td>${esc(dayVerdict(b) + (b.trust ? "; " + b.trust.split(":")[0] : ""))}</td><td>${esc(why)}</td><td>${esc((b.flags || []).join(", "))}</td></tr>`;
  }
  return s + "</table>";
}

// dayHourRange is a list of hours in words: "none", "every hour, A to B"
// where they are every hour with data, "A to B (N hours)" where they run
// unbroken, else each hour.
function dayHourRange(plan, hs, days) {
  if (!hs.length) return "none";
  const t = hs.map(h => dayTime(h)).sort((a, b) => a - b);
  const unbroken = t.every((x, i) => i === 0 || x - t[i - 1] === 3600);
  const clock = x => dayClock(x, days);
  if (t.length < 3 || !unbroken) return t.map(clock).join(", ");
  const withData = (plan.hours || []).filter(h => h.rate).length;
  const span = `${clock(t[0])} to ${clock(t[t.length - 1])}`;
  return t.length === withData ? `every hour, ${span}` : `${span} (${t.length} hours)`;
}

// daySummaryText is the summary in a few lines of plain text.
function daySummaryText(plan) {
  const s = plan.summary, c = s.counts, out = [];
  const days = daySpansDays(plan);
  const hours = hs => dayHourRange(plan, hs, days);
  // The SLO as the router measured it comes first, as risk: a plan sized on
  // decode alone never reads as holding it.
  if (c.measuredTTFT && plan.ttft && c.measuredTTFTOver) {
    out.push(`SLO risk: measured TTFT ${plan.ttft.statistic} was over ${dayFmt(plan.ttft.target)} s in ` +
      (c.measuredTTFTOver === c.measuredTTFT ? "every interval measured" : `${c.measuredTTFTOver} of ${c.measuredTTFT} intervals`) +
      (c.prefillNotPriced ? " (prefill not sized)." : "."));
  }
  if (s.peakNeeded) {
    const busiest = s.busiestNeeded ? `; the busiest bucket alone ${dayNeededText(s.busiestNeeded)}` : "";
    out.push(`Needed to hold every bucket ${dayNeededText(s.peakNeeded)}${s.peakNotConfirmed ? " (not confirmed: it falls short at a bucket)" : ""}` +
      `${busiest}; at the quietest ${dayNeededText(s.minNeeded)}.`);
    const g = s.sizedOnGpus;
    out.push("Each role's most over the buckets, priced again at each; a bucket's own need is " +
      (g ? `the fewest GPUs, at ${g.prefill} a prefill and ${g.decode} a decode replica` : "the fewest replicas in all") +
      ", and another mix may serve it too. The fleet the bucket had is judged whole, at the bucket's rate.");
    if (s.neededGpuHours != null) out.push(`Over the ${c.sized} sized buckets: GPU-hours needed ${s.neededGpuHours.toFixed(1)}, had ${s.hadGpuHours.toFixed(1)}.`);
    else if (c.prefillNotPriced) {
      out.push(`No GPU-hours of the need: prefill is not sized in ${c.prefillNotPriced} of the ${c.sized} sized buckets.` +
        ((plan.refused || []).some(r => r.reason === "NoPrefillCapacity") ? " " + DAY_TYPE_VP : ""));
    }
    if (s.heldReplicaHours) {
      const r = s.heldReplicaHours;
      out.push(`Held with the scale-up lead (each scale-up's replicas from one lead before their bucket): ` +
        `${r.prefill != null ? r.prefill.toFixed(2) + " prefill + " : ""}${r.decode.toFixed(2)} decode replica-hours` +
        `${s.heldGpuHours != null ? ", " + s.heldGpuHours.toFixed(1) + " GPU-hours" : ""}.`);
    }
    const decodeOver = s.decodeOverHours || [];
    if (c.prefillNotPriced === c.sized) {
      // Prefill priced in no bucket: no fleet is judged over, only decode.
      out.push(`Hours short of the rate: ${hours(s.underHours)}. Decode only: ${decodeOver.length ? "decode had a replica to spare " + hours(decodeOver) : "no hour had a decode replica to spare"}. ${DAY_DECODE_ONLY}`);
    } else {
      out.push(`Hours under (SLO risk): ${hours(s.underHours)}. Hours over (waste): ${hours(s.overHours)}.` +
        (decodeOver.length ? ` Decode only (prefill not sized in them): hours decode had a replica to spare: ${hours(decodeOver)}.` : ""));
    }
  } else {
    out.push("No bucket was sized: see the refusals above.");
  }
  if (s.hadGpuHoursAll != null) out.push(`The fleet ran ${s.hadGpuHoursAll.toFixed(1)} GPU-hours over every bucket with data.`);
  const p95 = plan.sizeFor === "p95-modulation" ? `${c.sizedForP95Modulation || 0} for their p95 less counting noise` : `${c.sizedForP95} for their p95`;
  out.push(`${c.sized} buckets sized (${p95}, ${c.sizedForMean} for their mean, ${c.sizedForMax} for their max), ` +
    `${c.unmet} beyond the bound, ${c.refused} refused, ${c.tooLittleData} with too little data, ${c.noData} with no data.`);
  if (c.measuredTTFT && plan.ttft && !c.measuredTTFTOver) {
    out.push(`Measured TTFT ${plan.ttft.statistic} was within ${dayFmt(plan.ttft.target)} s in all ${c.measuredTTFT} intervals measured.`);
  }
  if (c.untrusted) out.push(`Self-check: ${c.untrusted} bucket(s) not trusted: their verdicts rest on figures the self-check found inconsistent with this recording (see each bucket).`);
  if (c.notCovered) out.push(`Self-check: ${c.notCovered} bucket(s) not covered: it did not test the rate, fleet or shape each was priced at.`);
  return out;
}

// dayHeadline is the day plan in one line, over its chart: the most the
// busiest bucket needed and when, beside what the fleet ran then, and how
// many hours missed the SLO; "" with nothing sized.
function dayHeadline(plan) {
  let peak = null;
  for (const b of plan.buckets) {
    if (b.needed && b.sizedRate != null && (!peak || b.sizedRate > peak.sizedRate)) peak = b;
  }
  if (!peak) return "";
  const days = daySpansDays(plan);
  const n = peak.needed;
  const need = n.prefill == null ? `at least ${n.decode} decode (prefill not sized)` : `${n.atLeast ? "at least " : ""}${n.prefill} prefill + ${n.decode} decode`;
  const had = peak.actual ? `; it ran ${peak.actual.prefill} + ${peak.actual.decode}` : "";
  const under = (plan.summary.underHours || []).length;
  // The measured miss leads, as risk; the need follows.
  const measured = dayMeasuredLine(plan);
  return (measured ? measured + " " : "") + `Peak needed ${need} at ${dayClock(dayTime(peak.start), days)} UTC (${dayFmt(peak.sizedRate)} r/s)${had}.` +
    (under ? ` ${under} hour${under === 1 ? "" : "s"} short of the rate (red).` : "");
}

// dayMeasuredLine says how the SLO fared as measured: a plan priced on
// decode alone says nothing of TTFT, so "no hour missed" would read the
// unpriced prefill as holding. "" where nothing was measured.
function dayMeasuredLine(plan) {
  const c = plan.summary.counts;
  if (!plan.ttft || !c.measuredTTFT) return c.sized && c.prefillNotPriced === c.sized ? "Prefill was not sized." : "";
  if (!c.measuredTTFTOver) return `Measured TTFT ${plan.ttft.statistic} was within ${dayFmt(plan.ttft.target)} s in every interval measured.`;
  return `SLO risk: measured TTFT ${plan.ttft.statistic} was over ${dayFmt(plan.ttft.target)} s in ` +
    (c.measuredTTFTOver === c.measuredTTFT ? "every interval" : `${c.measuredTTFTOver} of ${c.measuredTTFT} intervals`) +
    (c.prefillNotPriced ? " (prefill not sized)." : ".");
}

// dayHoursCSV is the hours as CSV: each hour's rate, the fleet it needed and
// had, and its verdict in words.
function dayHoursCSV(plan) {
  const rows = [["hour_utc", "rate_mean", "rate_p95", "needed_prefill", "needed_decode", "needed_at_least", "had_prefill", "had_decode", "verdict"].join(",")];
  for (const h of plan.hours) {
    const n = h.needed || {}, a = h.actual || {};
    rows.push([h.start, h.rate ? h.rate.mean.toFixed(3) : "", h.rate ? h.rate.p95.toFixed(3) : "", n.prefill == null ? "" : n.prefill,
      n.decode == null ? "" : n.decode, n.atLeast ? "yes" : "", a.prefill == null ? "" : a.prefill, a.decode == null ? "" : a.decode,
      dayVerdict(h) || "not sized"].map(csvField).join(","));
  }
  return rows.join("\n") + "\n";
}
