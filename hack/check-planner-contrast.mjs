// Checks the capacity planner page's colour tokens (internal/whatifapi/page/
// planner.css, light and dark) against WCAG 2.1 AA: 4.5:1 for text tokens and
// 3:1 for graphical objects and component boundaries (axes, series, status
// glyphs, focus ring, control borders), each on both backgrounds the page
// draws on (--page and --surface-1). A token a rule draws text with is held
// to 4.5:1. Called by hack/check-planner-scenarios.sh; with --report it
// prints every pair's ratio and exits 0.
//
// Usage: node hack/check-planner-contrast.mjs [planner.css] [--report]
import fs from "node:fs";

const args = process.argv.slice(2);
const report = args.includes("--report");
const file = args.find(a => !a.startsWith("--")) || "internal/whatifapi/page/planner.css";
const css = fs.readFileSync(file, "utf8");

// tokens reads the custom properties of the first :root block after start.
function tokens(start) {
  const i = css.indexOf(":root {", start);
  if (i < 0) return null;
  const body = css.slice(i, css.indexOf("}", i));
  const out = {};
  for (const m of body.matchAll(/--([\w-]+):\s*([^;]+);/g)) out[m[1]] = m[2].trim();
  return out;
}
const light = tokens(0);
const darkAt = css.indexOf("@media (prefers-color-scheme: dark)");
const dark = Object.assign({}, light, tokens(darkAt));

// rgb parses #rgb, #rrggbb and rgba(); an alpha is composited over bg.
function rgb(c, bg) {
  let m = /^#([0-9a-f]{3})$/i.exec(c);
  if (m) return [...m[1]].map(h => parseInt(h + h, 16));
  m = /^#([0-9a-f]{6})$/i.exec(c);
  if (m) return [0, 2, 4].map(i => parseInt(m[1].slice(i, i + 2), 16));
  m = /^rgba?\(([^)]+)\)$/i.exec(c);
  if (m) {
    const p = m[1].split(",").map(s => +s.trim());
    const a = p.length > 3 ? p[3] : 1;
    return p.slice(0, 3).map((v, i) => Math.round(v * a + (bg ? bg[i] : 255) * (1 - a)));
  }
  throw new Error("cannot read colour " + c);
}
function lum([r, g, b]) {
  const f = v => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
}
function ratio(a, b) {
  const [x, y] = [lum(a), lum(b)].sort((p, q) => q - p);
  return (x + 0.05) / (y + 0.05);
}

// The tokens a rule draws text with, and those drawn as graphics or as a
// component's boundary.
const TEXT = ["text-primary", "text-secondary", "muted", "good", "critical", "text-warning"];
const GRAPHIC = ["axis", "series-1", "series-2", "series-3", "series-4", "series-5", "good", "warning", "critical", "accent", "control-border"];

let passed = 0;
const failures = [];
const rows = [];
for (const [mode, t] of [["light", light], ["dark", dark]]) {
  for (const bgName of ["page", "surface-1"]) {
    const bg = rgb(t[bgName]);
    for (const [names, min, kind] of [[TEXT, 4.5, "text"], [GRAPHIC, 3, "graphic"]]) {
      for (const n of names) {
        if (t[n] === undefined) {
          failures.push(`${mode}: --${n} is not defined`);
          continue;
        }
        const r = ratio(rgb(t[n], bg), bg);
        rows.push(`${mode}\t--${n}\ton --${bgName}\t${kind}\t${r.toFixed(2)}:1\t${r >= min ? "ok" : "FAIL < " + min}`);
        if (r >= min) passed++;
        else failures.push(`${mode}: --${n} on --${bgName} is ${r.toFixed(2)}:1, under ${min}:1 for ${kind}`);
      }
    }
  }
}
// No rule may paint text with a token held only to 3:1, nor with a literal.
// A ::before glyph (a status mark beside its words) is a graphic.
for (const rule of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
  const sel = rule[1].trim();
  if (sel.includes("::before")) continue;
  for (const m of rule[2].matchAll(/(^|[;\s])color:\s*([^;]+)/g)) {
    const v = m[2].trim(), tok = /^var\(--([\w-]+)\)$/.exec(v);
    if (tok && (TEXT.includes(tok[1]) || ["page", "surface-1"].includes(tok[1]))) passed++;
    else if (tok) failures.push(`${sel}: text drawn with --${tok[1]}, which is not held to 4.5:1`);
    else if (v !== "inherit" && v !== "transparent") failures.push(`${sel}: text drawn with ${v}: use a text token`);
  }
}

if (report) {
  console.log(rows.join("\n"));
  for (const f of failures) console.log("  - " + f);
  process.exit(0);
}
if (failures.length) {
  console.error(`FAIL check-planner-contrast: ${failures.length} of ${passed + failures.length}`);
  for (const f of failures) console.error("  - " + f);
  process.exit(1);
}
console.log(`ok check-planner-contrast: ${passed} checks`);
