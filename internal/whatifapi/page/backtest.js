// The capacity planner page's "Check the planner" view (POST /v1/backtest),
// drawn as pure functions: the charts as SVG markup, the summary lines, the
// legend and the table view. No DOM and no browser API, so
// hack/check-planner-backtest.mjs runs them under node on a fixture. The
// page loads this file after scenario.js (esc) and dayplan.js (the time
// axis helpers) and before planner.js, which puts the markup on the page.
//
// Two panels share one time axis: TTFT, then TPOT, each on a log axis whose
// ticks carry their unit (ms under a second, else s). Per
// panel, the predicted mean and p90 are step lines, the band from the p90 to
// its upper bound is a wash of the p90's hue, and the measured mean and p90
// are dots (mean hollow, p90 filled). A measured p90 in a histogram bucket
// too wide to place it is drawn as that bucket, a bar from its lower to its
// upper edge: the truth is somewhere in it, and a dot would read the grid. A step not judged is greyed and says
// why on hover; a step the planner gave no figure for, calling the fleet
// overloaded, is marked along the panel's top. The y axes are logarithmic,
// so that a burst of seconds does not flatten the quiet steps' tens of
// milliseconds; a predicted figure far above what was measured is clipped
// at the top of the panel and marked.
"use strict";

// BT_PANELS is each panel: the key of its figures and its title.
const BT_PANELS = [
  {key: "ttft", title: "TTFT (log scale; each tick carries its unit)", h: 170},
  {key: "tpot", title: "TPOT (log scale; each tick carries its unit)", h: 150},
];
// BT_STATS are the statistics drawn, each with its class.
const BT_STATS = [{stat: "mean", cls: "c-0", dot: "dot hollow"}, {stat: "p90", cls: "c-1", dot: "dot"}];
// BT_CLIP is how far above the highest measured figure a panel's axis
// reaches before predictions are clipped.
const BT_CLIP = 3;
// BT_EXCLUDED is a step's reason not to be judged, in words.
const BT_EXCLUDED = {noTraffic: "no traffic", noMeasurement: "no measured requests", scaling: "replicas changing",
  warmUp: "warm-up", notPriced: "not priced", otherVariants: "other variants serving",
  burst: "a burst near the knee in its window"};
// BT_V2 is each experimental model's flag on a figure, as the figure says
// it: a figure priced by one is experimental, and one it abstained on is the
// planner's own.
const BT_V2 = {RankMixtureTTFT: "experimental TTFT v2, outside the validated region",
  RankMixtureAbstained: "TTFT v2 abstained, the planner's own",
  TPOTDistribution: "experimental TPOT v2, outside the validated region",
  TPOTDistributionAbstained: "TPOT v2 abstained, the planner's own"};

// btV2Mark is a point's mark: a dagger where an experimental model priced
// it (the view's one notice says what it means), "" for the planner's own,
// an abstention included. BT_V2 names each flag in the method's words.
function btV2Mark(p) {
  return isExperimental(p.predicted) ? " " + DAGGER : "";
}

// btInBucket is whether the TPOT distribution's p90 or p95 at a step lies
// in the measured one's joint bracket (the router's TPOT histogram and,
// above their first edge, the decode engines'), in words, or "".
function btInBucket(step, key, stat) {
  const d = key === "tpot" && step.tpotDistribution;
  const v = d && (stat === "p90" ? d.p90Inside : stat === "p95" ? d.p95Inside : undefined);
  if (v == null) return "";
  return v ? " (in the measured bracket)" : " (not in the measured bracket)";
}

// btV2Lines are the experimental models' standing, a sentence each: what
// they price and that it is experimental, or why they abstained; and for
// TPOT, how often its p90 and p95 landed in the measured joint bracket, and
// under or over it.
function btV2Lines(bt) {
  const out = [];
  const m = bt.rankMixture;
  if (m) {
    out.push(m.abstained ? `EXPERIMENTAL TTFT model v2 (${m.model}) was asked for and abstained: ${m.abstained}. Every TTFT is the planner's own (RankMixtureAbstained).` :
      `EXPERIMENTAL TTFT model v2 (${m.model}): every TTFT it prices is not validated and reads outside the validated region (RankMixtureTTFT). ${m.assumed}`);
  }
  const d = bt.tpotDistribution;
  if (d) {
    out.push(d.abstained ? `EXPERIMENTAL TPOT model v2 (${d.model}) was asked for and abstained: ${d.abstained}. Every TPOT percentile is the planner's own (TPOTDistributionAbstained).` :
      `EXPERIMENTAL TPOT model v2 (${d.model}): every TPOT percentile it prices is not validated and reads outside the validated region (TPOTDistribution); the mean stays the planner's. ${d.assumed}`);
    const t = d.test;
    if (!d.abstained && t && t.steps > 0) {
      const own = v => v == null ? "" : ` (the planner's own: ${v})`;
      out.push(`TPOT v2 predicted p90 in the measured joint bracket at ${t.p90Inside} of ${t.steps} steps${own(t.defaultP90Inside)}, ` +
        `under it at ${t.p90Below || 0} and over at ${t.p90Above || 0}; p95 at ${t.p95Inside}${own(t.defaultP95Inside)}, ` +
        `under at ${t.p95Below || 0} and over at ${t.p95Above || 0}.`);
    }
  }
  return out;
}

// btPoint is the step's point at stat in key ("ttft" or "tpot"), or null.
function btPoint(step, key, stat) {
  return (step[key] || []).find(p => p.statistic === stat) || null;
}

// btLayout is where each panel sits, and the x scale from the first step's
// start to the last step's end.
function btLayout(bt) {
  const steps = bt.steps;
  const x0 = dayTime(steps[0].start), x1 = dayTime(steps[steps.length - 1].start) + bt.step;
  const pw = DAY_W - DAY_L - DAY_R;
  let y = DAY_T;
  const panels = BT_PANELS.map(p => {
    const out = {key: p.key, title: p.title, top: y + 16, h: p.h};
    y = out.top + p.h + DAY_GAP;
    return out;
  });
  return {x0, x1, pw, H: y - DAY_GAP + 22, panels, X: t => DAY_L + pw * (t - x0) / (x1 - x0)};
}

// btPanelRange is a panel's log axis: from the decade under the least
// figure drawn to the first 1-2-5 step over the highest measured one times
// BT_CLIP, no higher than the highest prediction needs.
function btPanelRange(bt, key) {
  let meas = 0, pred = 0, least = Infinity;
  for (const s of bt.steps) {
    for (const {stat} of BT_STATS) {
      const p = btPoint(s, key, stat);
      if (!p) continue;
      if (p.measuredBucket) {
        meas = Math.max(meas, p.measuredBucket.hi);
        if (p.measuredBucket.lo > 0) least = Math.min(least, p.measuredBucket.lo);
      } else if (p.measured != null && isFinite(p.measured) && p.measured > 0) {
        meas = Math.max(meas, p.measured);
        least = Math.min(least, p.measured);
      }
      if (p.predicted && p.predicted.value > 0) {
        pred = Math.max(pred, p.predicted.value);
        least = Math.min(least, p.predicted.value);
      }
    }
  }
  let top = meas > 0 ? Math.min(Math.max(meas, pred), meas * BT_CLIP) : pred;
  if (!(top > 0)) top = 1;
  if (!isFinite(least) || least >= top) least = top / 10;
  const lo = Math.pow(10, Math.floor(Math.log10(least)));
  const ticks = btLogTicks(lo, top * 10);
  const hi = ticks.find(v => v >= top) || top;
  return {lo, hi};
}

// btLogTicks are a log axis' ticks: 1, 2 and 5 times each decade in range.
function btLogTicks(lo, hi) {
  const out = [];
  for (let d = lo; d <= hi * 1.0001; d *= 10) {
    for (const m of [1, 2, 5]) {
      const v = Number((d * m).toPrecision(6));
      if (v >= lo * 0.9999 && v <= hi * 1.0001) out.push(v);
    }
  }
  return out;
}

// btSummaryText is the answer in sentences: the verdict, what was not
// judged, the windows, the configuration and what was measured.
function btSummaryText(bt) {
  const out = (bt.verdictLines && bt.verdictLines.length ? bt.verdictLines : [bt.verdict]).concat([bt.excluded], btV2Lines(bt));
  const at = s => s.slice(0, 16).replace("T", " ");
  out.push(`Fitted on ${bt.fit.source} from ${at(bt.fit.from)} to ${at(bt.fit.to)} UTC; tested on ${bt.test.source} from ` +
    `${at(bt.test.from)} to ${at(bt.test.to)} UTC${bt.split ? " (one source split: " + bt.split + ")" : ""}.`);
  if (bt.config.match) {
    out.push("Configuration: identical where both recordings record it.");
  } else {
    out.push("CONFIGURATION DIFFERS, priced anyway: " + bt.config.differences.map(d => `${d.field} (fit ${d.fit}, test ${d.test})`).join("; ") + ".");
  }
  out.push("Measured by " + bt.measuredNote + ".");
  for (const a of bt.assumed) out.push("Assumed: " + a + ".");
  return out;
}

// btSVG is the charts: the SVG's inner markup, its viewBox, and the layout
// the hover reads. Every series is a path or group with data-series, every
// greyed step a rect with data-excluded.
function btSVG(bt) {
  const lay = btLayout(bt), X = lay.X, step = bt.step, xr = DAY_L + lay.pw, unres = btUnresolved(bt);
  const days = bt.steps.length && bt.steps[0].start.slice(0, 10) !== bt.steps[bt.steps.length - 1].start.slice(0, 10);
  let s = "";
  for (const p of lay.panels) {
    const r = btPanelRange(bt, p.key), ymax = r.hi;
    const span = Math.log10(r.hi / r.lo);
    const Y = v => p.top + p.h * (1 - Math.log10(Math.min(Math.max(v, r.lo), r.hi) / r.lo) / span);
    let g = `<text class="ptitle" x="${DAY_L}" y="${p.top - 6}">${esc(p.title)}</text>`;
    for (const k of bt.steps) {
      if (!k.excluded) continue;
      const t = dayTime(k.start);
      g += `<rect class="btgrey" data-excluded="${esc(k.excluded)}" x="${X(t).toFixed(1)}" y="${p.top}" ` +
        `width="${(X(t + step) - X(t)).toFixed(1)}" height="${p.h}"/>`;
    }
    for (const v of btLogTicks(r.lo, r.hi)) {
      g += `<line class="${v > r.lo ? "gridl" : "axisl"}" x1="${DAY_L}" x2="${xr}" y1="${Y(v).toFixed(1)}" y2="${Y(v).toFixed(1)}"/>` +
        `<text x="${DAY_L - 8}" y="${(Y(v) + 4).toFixed(1)}" text-anchor="end">${btFmt(v)}</text>`;
    }
    const p90 = k => {
      const q = btPoint(k, p.key, "p90");
      return q && q.predicted.value != null && q.bound != null ? [q.predicted.value, q.bound] : null;
    };
    for (const run of dayRuns(bt.steps, p90)) {
      g += `<polygon class="btband" data-series="${p.key}-p90-bound" points="${dayStepBand(run, step, X, Y)}"/>`;
    }
    for (const k of bt.steps) {
      const sat = BT_STATS.some(({stat}) => {
        const q = btPoint(k, p.key, stat);
        return q && q.predicted.refused && q.predicted.refused.reason === "Saturated";
      });
      if (k.excluded || !sat) continue;
      const t = dayTime(k.start);
      g += `<rect class="btsat" data-saturated="${p.key}" x="${X(t).toFixed(1)}" y="${p.top}" ` +
        `width="${(X(t + step) - X(t)).toFixed(1)}" height="6"><title>no figure: the planner calls the fleet overloaded here</title></rect>`;
    }
    for (const {stat, cls, dot} of BT_STATS) {
      const pred = k => { const q = btPoint(k, p.key, stat); return q && q.predicted.value != null ? q.predicted.value : null; };
      for (const run of dayRuns(bt.steps, pred)) {
        g += `<path class="line ${cls}" data-series="${p.key}-${stat}-predicted" d="${dayStep(run, step, X, Y)}"/>`;
      }
      let dots = "";
      const band = unres[`${p.key}-${stat}`];
      if (band) {
        // One bucket too wide to place the figure, at many steps: one band
        // behind the panel, not a bar a step.
        g = `<rect class="btunres" data-unresolved="${p.key}-${stat}" x="${DAY_L}" y="${Y(band.hi).toFixed(1)}" width="${lay.pw}" ` +
          `height="${Math.max(Y(band.lo) - Y(band.hi), 1).toFixed(1)}"><title>measured ${esc(stat)} somewhere in ${esc(btFmt(band.lo))} to ` +
          `${esc(btFmt(band.hi))} at many steps: one histogram bucket, too wide to place it</title></rect>` + g;
      }
      for (const k of bt.steps) {
        const q = btPoint(k, p.key, stat);
        const cx = X(dayTime(k.start) + step / 2).toFixed(1);
        if (q && q.measuredBucket && band && q.measuredBucket.lo === band.lo && q.measuredBucket.hi === band.hi) continue;
        if (q && q.measuredBucket) {
          const b = q.measuredBucket;
          dots += `<rect class="btbucket ${cls}" data-bucket="${p.key}-${stat}" x="${(cx - 2).toFixed(1)}" y="${Y(b.hi).toFixed(1)}" ` +
            `width="4" height="${Math.max(Y(b.lo) - Y(b.hi), 1).toFixed(1)}"><title>measured ${esc(stat)} somewhere in ` +
            `${esc(btFmt(b.lo))} to ${esc(btFmt(b.hi))}: the histogram bucket is too wide to place it</title></rect>`;
          continue;
        }
        if (!q || q.measured == null || !isFinite(q.measured)) continue;
        // A measured dot carries how it stands against its prediction.
        const at = btPointClass(q);
        dots += `<circle class="${dot} ${cls}${at ? " bt-" + at : ""}" cx="${cx}" cy="${Y(q.measured).toFixed(1)}" r="3"/>`;
      }
      g += `<g data-series="${p.key}-${stat}-measured">${dots}</g>`;
      for (const k of bt.steps) {
        const q = btPoint(k, p.key, stat);
        if (q && q.predicted.value != null && q.predicted.value > ymax) {
          g += `<text class="btclip" data-clipped="${p.key}-${stat}" x="${X(dayTime(k.start) + step / 2).toFixed(1)}" y="${p.top + 10}" ` +
            `text-anchor="middle">&#9650;<title>predicted ${esc(btFmt(q.predicted.value))}, above the axis</title></text>`;
        }
      }
    }
    g += dayLabels(BT_STATS.map(({stat}) => {
      for (let i = bt.steps.length - 1; i >= 0; i--) {
        const q = btPoint(bt.steps[i], p.key, stat);
        if (q && q.predicted.value != null) return {y: Y(q.predicted.value), text: stat};
      }
      return null;
    }).filter(Boolean), xr + 6, p.top, p.top + p.h);
    s += `<g class="panel" data-panel="${p.key}" data-x0="${lay.x0}" data-x1="${lay.x1}">${g}</g>`;
  }
  const bottom = lay.panels[lay.panels.length - 1], yb = bottom.top + bottom.h;
  for (const t of dayXTicks(lay.x0, lay.x1)) {
    s += `<line class="gridl" x1="${X(t).toFixed(1)}" x2="${X(t).toFixed(1)}" y1="${yb}" y2="${yb + 4}"/>` +
      `<text x="${X(t).toFixed(1)}" y="${yb + 16}" text-anchor="middle">${dayClock(t, days)}</text>`;
  }
  s += `<text x="${xr}" y="${yb + 16}" text-anchor="start" dx="8">UTC</text>`;
  s += `<line class="xh" x1="0" x2="0" y1="${lay.panels[0].top}" y2="${yb}" visibility="hidden"/>`;
  return {markup: s, viewBox: `0 0 ${DAY_W} ${lay.H}`, x0: lay.x0, x1: lay.x1, L: DAY_L, pw: lay.pw, W: DAY_W};
}

// btFmt is seconds for a tick or a cell: ms under one second.
function btFmt(v) {
  if (v == null) return "-";
  if (!isFinite(v)) return "above the top bucket";
  if (v >= 1) return Number(v.toPrecision(3)) + " s";
  return Number((v * 1000).toPrecision(2)) + " ms";
}

// btPredictionCSV is a prediction file as CSV: a comment line with its
// hashes, then a row a step with each statistic predicted: the columns of
// the CLI's predict --csv, its times and numbers written as JavaScript
// writes them.
function btPredictionCSV(p) {
  const stats = ["mean", "p50", "p90", "p95"];
  const head = ["start", "excluded", "rate", "prefill", "decode"];
  for (const k of ["ttft", "tpot"]) for (const s of stats) head.push(`${k}_${s}_seconds`);
  const cell = v => v == null ? "" : String(Number(v.toPrecision(6)));
  const quote = s => /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
  const lines = [`# ${p.kind} model=${p.model} window=${p.window.from}..${p.window.to} predictionHash=${p.predictionHash} ` +
    `inputsHash=${p.inputsHash} planner=${p.plannerVersion} madeAt=${p.madeAt}`, head.join(",")];
  for (const s of p.steps) {
    const row = [s.start, s.excluded || "", s.rate.toFixed(3), String(s.prefill), String(s.decode)];
    for (const k of ["ttft", "tpot"]) {
      for (const stat of stats) {
        const q = btPoint(s, k, stat);
        row.push(q && q.predicted.value != null ? cell(q.predicted.value) : "");
      }
    }
    lines.push(row.map(quote).join(","));
  }
  return lines.join("\n") + "\n";
}

// SC_BADGE is a self-check's status as its badge says it.
const SC_BADGE = {ok: "self-check held on its own steps", weak: "self-check weak", inconsistent: "self-check: inconsistent", notRun: "self-check not run"};

// selfCheckBadge is the badge of a source's self-check: its words and its
// class.
function selfCheckBadge(sc) {
  if (!sc) return null;
  return {text: SC_BADGE[sc.status] || "self-check " + sc.status, cls: "sc-" + (SC_BADGE[sc.status] ? sc.status : "notRun")};
}

// selfCheckHTML is a source's self-check, shown with every answer: the line,
// and short of ok why and what to trust, and for an inconsistent source a
// link to review and file the prefilled issue. Every string is escaped.
function selfCheckHTML(sc) {
  if (!sc) return "";
  let h = `<p class="scline">${esc(sc.line)}</p>`;
  if (sc.status === "weak" || sc.status === "inconsistent") h += `<p class="scwhy">${esc(sc.why)}</p>`;
  if (sc.trust) h += `<p class="sctrust">${esc(sc.trust)}</p>`;
  // The link is GitHub's, over TLS: a five-letter scheme (the page names no
  // scheme in its text), then the host.
  if (sc.issueUrl && /^[a-z]{5}:\/\/github\.com\//.test(sc.issueUrl) && sc.issueUrl.indexOf("s://") === 4) {
    h += `<p><a class="scissue" href="${esc(sc.issueUrl)}" target="_blank" rel="noopener noreferrer">Review the issue to file</a> ` +
      `<span class="hint">It opens a prefilled GitHub issue with no name and no recording; nothing is sent until you submit it.</span></p>`;
  }
  return h;
}

// btMeasured is a point's measured figure in words: above the top bucket
// where the histogram could not say, and the bucket where it is too wide to
// place the figure, with which side of it the prediction fell.
function btMeasured(p) {
  if (p.aboveHistogram || p.unscored === "aboveHistogram") return "above the top bucket";
  if (p.measuredBucket) {
    const b = p.measuredBucket;
    const side = p.outsideBucket === "under" ? ", predicted under it" : p.outsideBucket === "over" ? ", predicted over it" : "";
    return `~${btFmt(b.lo)} to ${btFmt(b.hi)} (bucket too wide to place it${side})`;
  }
  return btFmt(p.measured);
}

// btLegend is the charts' legend.
function btLegend() {
  const sw = cls => `<svg viewBox="0 0 18 10" aria-hidden="true"><line class="line ${cls}" x1="1" x2="17" y1="5" y2="5"/></svg>`;
  const dot = cls => `<svg viewBox="0 0 18 10" aria-hidden="true"><circle class="${cls}" cx="9" cy="5" r="3"/></svg>`;
  const box = cls => `<svg viewBox="0 0 18 10" aria-hidden="true"><rect class="${cls}" x="1" y="1" width="16" height="8"/></svg>`;
  return `<span>${sw("c-0")}mean predicted</span><span>${dot("dot hollow c-0")}mean measured</span>` +
    `<span>${sw("c-1")}p90 predicted</span><span>${dot("dot c-1")}p90 measured</span>` +
    `<span>${box("btbucket c-1")}p90 measured somewhere in a bucket too wide to place it</span><span>${box("btunres")}one bucket too wide, at many steps</span>` +
    `<span>${dot("dot bt-within")}measured within 20%</span><span>${dot("dot bt-low")}more than 20% over the prediction</span><span>${dot("dot bt-high")}more than 20% under it</span>` +
    `<span>${box("btband")}the planner's own range for p90</span><span>${box("btgrey")}not judged</span>` +
    `<span>${box("btsat")}no figure: the planner calls the fleet overloaded</span><span>&#9650; predicted above the axis</span>`;
}

// btStepAt is the step that holds t, seconds since the epoch, or null.
function btStepAt(bt, t) {
  for (const s of bt.steps) {
    const a = dayTime(s.start);
    if (t >= a && t < a + bt.step) return s;
  }
  return null;
}

// btPred is a point's prediction in words: "at most" before a TTFT priced
// from a lower bound of V_P (predicted.upperBound), whose ratio is scored
// only where the fleet measured more than 20% over it (unscored upperBound).
function btPred(p) {
  return (p.predicted.upperBound ? "at most " : "") + btFmt(p.predicted.value);
}

// btTipHTML is the tooltip of one step: its inputs, why it was not judged,
// and each statistic predicted and measured.
function btTipHTML(bt, s) {
  let h = `<b>${esc(s.start.slice(11, 16))} UTC</b>: ${esc(s.rate.toFixed(2))} r/s, ${s.prefill} prefill + ${s.decode} decode`;
  if (s.kneeShare != null) h += `, ${esc((s.kneeShare * 100).toFixed(0))}% of the fleet's limit the planner prices for this step (its knee)`;
  if (s.excluded) h += `<br>not judged: ${esc(BT_EXCLUDED[s.excluded] || s.excluded)}${s.detail ? " (" + esc(s.detail) + ")" : ""}`;
  for (const key of ["ttft", "tpot"]) {
    for (const p of s[key] || []) {
      const pred = p.predicted.value != null ? btPred(p) : p.predicted.refused && p.predicted.refused.reason === "Saturated" ?
        "no figure (overloaded)" : "no figure (" + (p.predicted.refused ? p.predicted.refused.reason : "-") + ")";
      const held = p.unscored === "upperBound" ? " (an upper bound the fleet measured under: consistent, not scored)" : "";
      h += `<br>${key.toUpperCase()} ${esc(p.statistic)}: predicted ${esc(pred)}, measured ${esc(btMeasured(p))}${esc(held)}` +
        `${esc(btInBucket(s, key, p.statistic))}${esc(btV2Mark(p))}`;
    }
  }
  return h;
}

// btScoreTable is the scores as a table: each statistic over every judged
// step, and over those inside the validated region.
function btScoreTable(bt) {
  const pct = v => v == null ? "-" : (v * 100).toFixed(0) + "%";
  const num = v => v == null ? "-" : v.toFixed(2);
  const factor = v => v == null ? "-" : (1 + v).toFixed(2) + "x";
  const region = ["ttft", "tpot"].some(k => bt.summary[k].some(sc => sc.inRegion.steps > 0));
  let h = `<table class="t"><thead><tr><th>figure</th><th>steps with a figure</th><th>median ratio</th><th>within 20%</th>` +
    `<th>read more than 20% low</th><th>typical miss</th><th>worst 10% miss</th><th>under the planner's upper estimate</th>` +
    (region ? `<th>in benchmark conditions: steps</th><th>in benchmark conditions: within 20%</th>` : "") + `</tr></thead><tbody>`;
  for (const key of ["ttft", "tpot"]) {
    for (const sc of bt.summary[key]) {
      // A statistic with no step scored has no row: btUnscoredList says why.
      if (!sc.all.steps) continue;
      const a = sc.all, r = sc.inRegion;
      h += `<tr><td>${key.toUpperCase()} ${esc(sc.statistic)}</td><td>${a.steps}</td><td>${num(a.bias)}</td><td>${pct(a.within)}</td>` +
        `<td>${a.low || 0}</td><td>${factor(a.medianError)}</td><td>${factor(a.p90Error)}</td><td>${pct(a.coverage)}</td>` +
        (region ? `<td>${r.steps}</td><td>${pct(r.within)}</td>` : "") + `</tr>`;
    }
  }
  return h + "</tbody></table>";
}

// btStepTable is every step as a table, grouped TTFT then TPOT, each by
// statistic: a cell is predicted vs measured, ✓ within 20% or ✗ outside it
// and by how much, and a bucket's note in its tooltip. failingOnly keeps the
// judged steps with a statistic outside 20%.
function btStepTable(bt, failingOnly) {
  const stats = ["mean", "p90", "p95"];
  const steps = failingOnly ? bt.steps.filter(btFailing) : bt.steps;
  let h = `<table class="t btsteps"><thead><tr><th rowspan="2">step (UTC)</th><th rowspan="2">rate</th><th rowspan="2">prefill + decode</th>` +
    `<th colspan="3" class="grp">TTFT, predicted vs measured</th><th colspan="3" class="grp">TPOT, predicted vs measured</th><th rowspan="2">judged</th></tr><tr>` +
    ["ttft", "tpot"].map(k => stats.map(st => `<th>${k.toUpperCase()} ${st}</th>`).join("")).join("") + `</tr></thead><tbody>`;
  for (const s of steps) {
    h += `<tr><td>${esc(s.start.slice(11, 16))}${btBatchChip(s)}</td><td>${esc(s.rate.toFixed(2))}</td><td>${s.prefill} + ${s.decode}</td>`;
    for (const k of ["ttft", "tpot"]) {
      for (const st of stats) {
        const c = btCell(s, k, st);
        h += `<td class="${c.cls ? "bt-" + c.cls : ""}"${c.title ? ` title="${esc(c.title)}"` : ""}>${esc(c.text)}</td>`;
      }
    }
    h += `<td>${s.excluded ? "no: " + esc(BT_EXCLUDED[s.excluded] || s.excluded) : "yes"}</td></tr>`;
  }
  if (!steps.length) h += `<tr><td colspan="10">${failingOnly ? "No judged step reads more than 20% off." : "No step."}</td></tr>`;
  return h + "</tbody></table>";
}

// batchFitWords is a BatchBeyondFit figure's two batches in words, of what
// (this load, this step): a pod's, or an engine rank's on a pod of several
// (batchBeyondFit.ranks).
function batchFitWords(b, what) {
  const unit = b.ranks > 1 ? "an engine rank" : "a pod";
  return `The decode line was fitted on up to ${b.fitted} sequences ${unit}; ${what} runs about ${b.priced}`;
}

// btBatchChip is the warning a step whose decode runs more sequences than
// the fit's ITL line was fitted on carries beside its time (BatchBeyondFit),
// "" for any other.
function btBatchChip(s) {
  const b = s.batchBeyondFit;
  if (!b) return "";
  const words = batchFitWords(b, "this step") + ", so its TPOT may read low and the decode knee high.";
  return ` <span class="chip flagged" title="${esc(words)}">BatchBeyondFit</span>`;
}

// ---- The check, read first: the verdict card, the notice, the step table ----

// BT_UNSCORED is why a statistic was not scored at a step, in words.
const BT_UNSCORED = {fewRequests: "too few measured requests", aboveHistogram: "measured above the histogram's top bucket",
  refused: "no figure", notMeasured: "not measured", notComparable: "measured as another figure than the one predicted",
  unsettled: "the load had not settled", burst: "a burst near the knee", unresolvable: "the measured bucket is too wide to place it",
  upperBound: "an upper bound the fleet measured under"};

// btPointClass is how a point's measured figure stands against its
// prediction: "within" 20%, "low" (predicted more than 20% under, or
// measured above the histogram), "high" (more than 20% over), "unresolved"
// (in a bucket too wide to place it); null where nothing was measured.
function btPointClass(p) {
  if (!p) return null;
  if (p.aboveHistogram) return "low";
  if (p.measuredBucket && (p.unscored || p.ratio == null)) return "unresolved";
  if (p.ratio == null) return null;
  if (p.outsideBucket === "under") return "low";
  if (p.outsideBucket === "over") return "high";
  if (Math.abs(p.ratio - 1) <= 0.2) return "within";
  return p.ratio < 1 ? "low" : "high";
}

// btPair is a prediction and a measurement in one unit: "2.10 vs 2.20 s",
// "12 vs 14 ms".
function btPair(pred, meas) {
  const s = Math.max(pred, meas) >= 1;
  const f = v => s ? Number(v.toPrecision(3)) : Number((v * 1000).toPrecision(3));
  return `${f(pred)} vs ${f(meas)} ${s ? "s" : "ms"}`;
}

// btCell is one statistic at one step as the step table shows it, with its
// class and a tooltip: "2.10 vs 2.20 s ✓ 5%", a dagger on an experimental
// figure, the bucket's note in the tooltip, never in the cell.
function btCell(s, key, stat) {
  const p = btPoint(s, key, stat);
  if (!p) return {text: "-", cls: "", title: ""};
  const d = dagger(p.predicted);
  const cls = btPointClass(p) || "";
  if (p.predicted.value == null) {
    return {text: "no figure" + d, cls: "", title: p.predicted.refused ? "refused: " + p.predicted.refused.reason : ""};
  }
  const title = (p.measuredBucket ? `measured somewhere in ${btFmt(p.measuredBucket.lo)} to ${btFmt(p.measuredBucket.hi)}, a histogram bucket too wide to place it` : "") +
    btInBucket(s, key, stat) + (p.unscored ? ` Not scored: ${BT_UNSCORED[p.unscored] || p.unscored}.` : "");
  if (p.aboveHistogram) return {text: `${btPred(p)} vs above the top bucket ✗${d}`, cls, title};
  // A figure the bucket is too wide to place reads as the bucket, never as
  // a point inside it.
  if (p.measuredBucket && (p.measured == null || cls === "unresolved")) {
    return {text: `${btPred(p)} vs ${btFmt(p.measuredBucket.lo)}-${btFmt(p.measuredBucket.hi)} \u2248${d}`, cls, title};
  }
  if (p.measured == null) return {text: btPred(p) + " vs -" + d, cls, title};
  const mark = cls === "within" ? "✓" : cls === "unresolved" ? "≈" : "✗";
  const off = p.ratio != null ? ` ${Math.round(Math.abs(p.ratio - 1) * 100)}%` : "";
  return {text: `${p.predicted.upperBound ? "≤ " : ""}${btPair(p.predicted.value, p.measured)} ${mark}${off}${d}`, cls, title};
}

// btFailing reports whether a judged step has a scored statistic outside 20%.
function btFailing(s) {
  if (s.excluded) return false;
  return ["ttft", "tpot"].some(k => (s[k] || []).some(p => {
    const c = btPointClass(p);
    return c === "low" || c === "high";
  }));
}

// btPrimary is the score the verdict quotes for a latency: the target's
// statistic where it was scored, else the mean.
function btPrimary(scores, stat) {
  const scored = (scores || []).filter(x => x.all && x.all.steps > 0);
  return scored.find(x => x.statistic === stat) || scored.find(x => x.statistic === "mean") || scored[0] || null;
}

// btVerdictCard is the check in three lines, each with a chip: TTFT and TPOT
// within 20% of measured, and the range tested.
function btVerdictCard(bt, ttftStat, tpotStat) {
  const lines = [];
  const one = (name, sc) => {
    if (!sc) return {chip: `${name}: not scored`, text: `${name} was not scored at any step: the step table says why.`, cls: "unresolved"};
    const a = sc.all, n = Math.round((a.within || 0) * a.steps);
    const lean = a.bias == null ? "" : a.bias < 0.97 ? `; it reads ${Math.round((1 - a.bias) * 100)}% low` : a.bias > 1.03 ? `; it reads ${Math.round((a.bias - 1) * 100)}% high` : "; no lean";
    const share = a.steps ? n / a.steps : 0;
    return {chip: `${name} ${sc.statistic} ${n}/${a.steps} within 20%`, cls: share >= 0.8 ? "within" : share >= 0.5 ? "unresolved" : "low",
      text: `${name} ${sc.statistic}: within 20% of measured in ${n} of ${a.steps} steps scored${lean}.`};
  };
  const tt = btPrimary(bt.summary.ttft, ttftStat);
  lines.push(one("TTFT", tt));
  // The header quotes the mean's score: so does the card, beside the target's.
  const tm = btPrimary(bt.summary.ttft, "mean");
  if (tt && tm && tm !== tt) lines.push(one("TTFT", tm));
  lines.push(one("TPOT", btPrimary(bt.summary.tpot, tpotStat)));
  const judged = (bt.steps || []).filter(s => !s.excluded);
  if (judged.length) {
    const lo = Math.min(...judged.map(s => s.rate)), hi = Math.max(...judged.map(s => s.rate));
    const fleets = [...new Set(judged.map(s => `${s.prefill} prefill + ${s.decode} decode`))];
    lines.push({chip: `${roundRate(lo)}-${roundRate(hi)} r/s`, cls: "range",
      text: `Tested ${roundRate(lo)} to ${roundRate(hi)} r/s at ${fleets.length === 1 ? fleets[0] : fleets.join(", ")}, ${judged.length} of ${bt.steps.length} steps judged: another rate or fleet is an extrapolation.`});
  } else {
    lines.push({chip: "no step judged", cls: "unresolved", text: "No step was judged: " + (bt.excluded || "see the step table") + "."});
  }
  return lines;
}

// btNotice is the check's one notice: what the dagger means, where a figure
// came from an experimental model; "" with none.
function btNotice(bt) {
  const any = (bt.steps || []).some(s => ["ttft", "tpot"].some(k => (s[k] || []).some(p => isExperimental(p.predicted))));
  return any ? EXPERIMENTAL_NOTICE.replace(" Check the planner shows how it did on your traffic.", " This check is how it did on your traffic.") : "";
}

// btUnresolved is the band one histogram bucket spans where a statistic's
// measured figure fell in it at many steps (its score's sameBucketBand),
// keyed "tpot-p90": drawn once behind the panel, not as a bar a step.
function btUnresolved(bt) {
  const out = {};
  for (const key of ["ttft", "tpot"]) {
    for (const sc of (bt.summary && bt.summary[key]) || []) {
      if (sc.sameBucketBand && sc.sameBucket > 0) out[`${key}-${sc.statistic}`] = sc.sameBucketBand;
    }
  }
  return out;
}

// btBucketTip is the tip under the charts where a statistic could not be
// placed for its histogram's bucket width: what to change to measure it.
function btBucketTip(bt) {
  const u = btUnresolved(bt), keys = Object.keys(u);
  if (!keys.length) return "";
  // A statistic the charts draw first (p90, then the mean), then any other.
  const k = keys.find(x => x.endsWith("-p90")) || keys.find(x => x.endsWith("-mean")) || keys[0], b = u[k], [key, stat] = k.split("-");
  return `${key.toUpperCase()} ${stat} was measured inside one histogram bucket, ${btFmt(b.lo)} to ${btFmt(b.hi)}, too wide to score it. ` +
    "Finer histogram edges measure it: the router's metrics.requestTPOTBuckets and requestTTFTBuckets in the EndpointPickerConfig, or vLLM's " +
    "--custom-histogram-buckets; edges about 15% apart place p90 and p95 within 20%.";
}

// btUnscoredList is each statistic not scored at some steps, and why.
function btUnscoredList(bt) {
  const out = [];
  for (const key of ["ttft", "tpot"]) {
    for (const sc of (bt.summary && bt.summary[key]) || []) {
      const parts = Object.entries(sc.unscored || {}).filter(([, n]) => n > 0).map(([why, n]) => `${n} ${BT_UNSCORED[why] || why}`);
      if (parts.length) out.push(`${key.toUpperCase()} ${sc.statistic}: not scored in ${parts.join(", ")}.`);
    }
  }
  return out;
}

// btStepsCSV is every step as CSV: its time, rate and fleet, and each
// statistic predicted and measured, seconds.
function btStepsCSV(bt) {
  const cols = [["ttft", "mean"], ["ttft", "p90"], ["ttft", "p95"], ["tpot", "mean"], ["tpot", "p90"], ["tpot", "p95"]];
  const head = ["start", "rate", "prefill", "decode", "judged"];
  for (const [k, s] of cols) head.push(`${k}_${s}_predicted_seconds`, `${k}_${s}_measured_seconds`, `${k}_${s}_bucket_lo_seconds`, `${k}_${s}_bucket_hi_seconds`, `${k}_${s}_class`);
  const num = v => v == null || !isFinite(v) ? "" : String(Number(v.toPrecision(6)));
  const rows = [head.join(",")];
  for (const s of bt.steps) {
    const r = [s.start, s.rate.toFixed(3), String(s.prefill), String(s.decode), s.excluded ? "no: " + s.excluded : "yes"];
    for (const [k, st] of cols) {
      const p = btPoint(s, k, st);
      const bk = p && p.measuredBucket;
      // An unresolved figure exports its bucket, not a point.
      const unres = p && btPointClass(p) === "unresolved";
      r.push(num(p && p.predicted.value), unres ? "" : num(p && p.measured), num(bk && bk.lo), num(bk && bk.hi), (p && btPointClass(p)) || "");
    }
    rows.push(r.map(csvField).join(","));
  }
  return rows.join("\n") + "\n";
}
