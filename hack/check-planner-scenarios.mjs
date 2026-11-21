// Runs the capacity planner page's scenario logic
// (internal/whatifapi/page/scenario.js) on fixtures: the checks on a
// scenario read back from a file or the store, the store's edits, the names
// and the compare table's changes. Called by hack/check-planner-scenarios.sh.
//
// The page runs this file in a browser; here it runs in a node vm context
// with nothing but what the page's script uses (URLSearchParams), so a
// browser API the logic came to rely on fails here first.
//
// Usage: node hack/check-planner-scenarios.mjs [path/to/scenario.js]
import fs from "node:fs";
import vm from "node:vm";

const file = process.argv[2] || "internal/whatifapi/page/scenario.js";
const ctx = vm.createContext({URLSearchParams});
vm.runInContext(fs.readFileSync(file, "utf8"), ctx, {filename: file});
const S = name => vm.runInContext(name, ctx);
const {esc, linkProblem, checkScenario, readScenarioFile, parseStore, visibleScenarios, putScenario, dropScenario, freeName,
  cleanName, delta, fleetDelta, scenarioTitle, sourceProblem, linkAtLoadStart, linkAtLoadEnd, beginOpen, importPlan, sameCurve, figDelta,
  compareRows, parseEdges, linkedEdges, edgesBoxProblem, beyondContext, contextNote, bucketSeries, retryDelay} = ctx;
const MAX_NAME = S("MAX_NAME"), MAX_SCENARIOS = S("MAX_SCENARIOS"), MAX_LINK = S("MAX_LINK");

let passed = 0;
const failures = [];
function check(name, ok, detail) {
  if (ok) passed++;
  else failures.push(name + (detail === undefined ? "" : ": " + detail));
}
function throws(name, fn, want) {
  try {
    fn();
    check(name, false, "did not throw");
  } catch (e) {
    check(name, !want || e.message.includes(want), e.message);
  }
}

// A case that throws where it should not ends the run as a failure, said as
// one, with the cases before it counted.
try {
// esc: the five characters HTML reads specially, and nothing else.
check("esc escapes markup", esc(`<img src=x onerror="a('b')">&`) === "&lt;img src=x onerror=&quot;a(&#39;b&#39;)&quot;&gt;&amp;", esc(`<img src=x onerror="a('b')">&`));
check("esc leaves text", esc("model-02, 2 prefill") === "model-02, 2 prefill");
check("esc of a number", esc(5) === "5");

// linkProblem: every link linkParams writes is admitted ...
const good = "recording=run.jsonl.gz&cycle=3&model=m&prefillVariant=p&decodeVariant=d&prefill=2&decode=1&rate=6.5&ttftStat=p90&ttft=0.8" +
  "&tpotStat=mean&tpot=0.05&ratio=1.3&headroom=0.85&priceAs=1k&out=1000&vp=30000&a=0.02&promptCV=1.2&routerHop=0.003&perBucket=1";
for (const link of [good, "namespace=llm&headroom=0.85", "namespace=llm&mode=size&max=40&low=1&rate=2&headroom=0.85",
  "recording=r.jsonl.gz&ttftStat=p99.9&ttft=1"]) {
  check(`admits ${link.slice(0, 50)}`, linkProblem(link) === "", linkProblem(link));
}
// ... and every other is refused, with the reason.
for (const [link, want] of [
  ["recording=r&onerror=1", "the page sets no onerror"],
  ["recording=r&constructor=1", "the page sets no constructor"],
  ["recording=r&__proto__=1", "the page sets no __proto__"],
  ["recording=r&toString=x", "the page sets no toString"],
  ["recording=r&rate=abc", "rate is not a number"],
  ["recording=r&rate=", "rate is not a number"],
  ["recording=r&rate=-1", "rate is not a number"],
  ["recording=r&rate=Infinity", "rate is not a number"],
  ["recording=r&rate=1&rate=2", "names rate twice"],
  ["recording=r&ttftStat=p90;x", "ttftStat cannot be"],
  ["recording=r&ttftStat=max", "ttftStat cannot be"],
  ["recording=r&mode=plan", "mode cannot be"],
  ["recording=r&low=true", "low cannot be"],
  ["recording=r&model=", "model is not a name"],
  ["recording=r&model=a%0Db", "model is not a name"],
  ["recording=r&model=" + "m".repeat(254), "model is not a name"],
  ["rate=1", "names no source"],
  ["", "names no source"],
  ["recording=r&model=" + "m".repeat(MAX_LINK), "longer than"],
]) {
  const got = linkProblem(link);
  check(`refuses ${link.slice(0, 40)}`, got !== "" && got.includes(want), JSON.stringify(got));
}
check("refuses a link that is not text", linkProblem(42) !== "" && linkProblem(null) !== "");

// checkScenario and nameProblem: the name.
const sc = checkScenario({name: "  two prefill ", link: good, saved: "2026-11-14 21:08"});
check("checkScenario trims the name", sc.name === "two prefill", sc.name);
check("checkScenario keeps the link", sc.link === good);
check("checkScenario keeps the time", sc.saved === "2026-11-14 21:08");
check("checkScenario drops a time with a control character", checkScenario({name: "n", link: good, saved: "a\nb"}).saved === "");
check("a name of MAX_NAME characters", checkScenario({name: "n".repeat(MAX_NAME), link: good}).name.length === MAX_NAME);
check("a hostile name is kept as text", checkScenario({name: "<img src=x onerror=alert(1)>", link: good}).name === "<img src=x onerror=alert(1)>");
for (const [what, x, want] of [
  ["no object", null, "not a scenario"],
  ["an array", [], "not a scenario"],
  ["a string", "x", "not a scenario"],
  ["no name", {link: good}, "name is text"],
  ["an empty name", {name: "   ", link: good}, "name is text"],
  ["a long name", {name: "n".repeat(MAX_NAME + 1), link: good}, "name is text"],
  ["a number name", {name: 5, link: good}, "name is text"],
  ["a CR in the name", {name: "a\rb", link: good}, "control character"],
  ["a NUL in the name", {name: "a\u0000b", link: good}, "control character"],
  ["a tab in the name", {name: "a\tb", link: good}, "control character"],
  ["a DEL in the name", {name: "a\u007fb", link: good}, "control character"],
  ["a C1 control in the name", {name: "a\u0085b", link: good}, "control character"],
  ["a bad link", {name: "n", link: "recording=r&x=1"}, "the page sets no x"],
  ["no link", {name: "n"}, "not text"],
]) {
  throws(`checkScenario refuses ${what}`, () => checkScenario(x), want);
}

// readScenarioFile: the exported file.
const file1 = JSON.stringify({kind: "capacity-planner-scenario", version: 1, name: "a", link: good});
check("reads an exported file", readScenarioFile(file1).link === good);
throws("refuses a file that is not JSON", () => readScenarioFile("not json"), "not JSON");
throws("refuses another kind", () => readScenarioFile(JSON.stringify({kind: "other", version: 1, name: "a", link: good})), "not a capacity-planner-scenario");
throws("refuses another version", () => readScenarioFile(JSON.stringify({kind: "capacity-planner-scenario", version: 2, name: "a", link: good})), "version 2");
throws("refuses JSON null", () => readScenarioFile("null"), "not a capacity-planner-scenario");
throws("refuses a file with a bad link", () => readScenarioFile(JSON.stringify({kind: "capacity-planner-scenario", version: 1, name: "a", link: "recording=r&rate=x"})), "rate");
throws("refuses a file too large", () => readScenarioFile(" ".repeat(70000)), "larger than");

// The store: entries that do not check are kept, and hidden.
check("parseStore of nothing", parseStore(null).length === 0);
check("parseStore of junk", parseStore("{").length === 0 && parseStore('{"a":1}').length === 0);
const foreign = {name: "from another version", link: "recording=r&future=1"};
const hasForeign = r => r.some(x => x && x.name === foreign.name && x.link === foreign.link);
let raw = parseStore(JSON.stringify([{name: "a", link: good}, foreign, 42, {name: "a\rb", link: good}]));
check("visibleScenarios hides what does not check", visibleScenarios(raw).map(x => x.name).join("|") === "a", visibleScenarios(raw).map(x => x.name).join("|"));
let put = putScenario(raw, {name: "b", link: good});
check("putScenario adds", !put.replaced && visibleScenarios(put.raw).map(x => x.name).join("|") === "a|b");
check("putScenario keeps entries that do not check", hasForeign(put.raw) && put.raw.includes(42) && put.raw.length === 5, put.raw.length);
put = putScenario(put.raw, {name: "a", link: "namespace=llm"});
check("putScenario replaces the same name", put.replaced && visibleScenarios(put.raw).filter(x => x.name === "a").length === 1 &&
  visibleScenarios(put.raw).find(x => x.name === "a").link === "namespace=llm");
check("putScenario replacing keeps the rest", hasForeign(put.raw) && put.raw.length === 5, put.raw.length);
throws("putScenario refuses a bad entry", () => putScenario([], {name: "a\u0000", link: good}), "control character");
raw = dropScenario(put.raw, "a");
check("dropScenario drops the one named", visibleScenarios(raw).map(x => x.name).join("|") === "b");
check("dropScenario keeps entries that do not check", hasForeign(raw) && raw.includes(42) && raw.length === 4, raw.length);
check("dropScenario of a name not saved", dropScenario(raw, "zzz").length === raw.length);
// A foreign entry with a visible one's name is not dropped with it.
const twin = {name: "b", link: "recording=r&future=1"};
check("dropScenario keeps a foreign twin", dropScenario(raw.concat([twin]), "b").includes(twin));

// The cap: MAX_SCENARIOS that check; a new name past it is refused, a
// replacement is not, and entries that do not check do not count.
let full = [foreign];
for (let i = 0; i < MAX_SCENARIOS; i++) full = putScenario(full, {name: "s" + i, link: good}).raw;
check("the store holds MAX_SCENARIOS", visibleScenarios(full).length === MAX_SCENARIOS);
throws("a new name past the cap is refused", () => putScenario(full, {name: "one more", link: good}), "at most");
check("a replacement at the cap is not", putScenario(full, {name: "s0", link: "namespace=llm"}).replaced);
check("visibleScenarios shows at most MAX_SCENARIOS", visibleScenarios(full.concat([{name: "extra", link: good}])).length === MAX_SCENARIOS);

// freeName: three imports of one 80-character name are three names.
const long = "n".repeat(MAX_NAME);
const taken = [long];
for (let i = 0; i < 3; i++) taken.push(freeName(long, taken));
check("freeName gives distinct names", new Set(taken).size === 4, taken.map(n => n.slice(-6)).join(","));
check("freeName stays within MAX_NAME", taken.every(n => n.length <= MAX_NAME), taken.map(n => n.length).join(","));
check("freeName suffixes", taken[1].endsWith(" (2)") && taken[2].endsWith(" (3)") && taken[3].endsWith(" (4)"), taken.slice(1).map(n => n.slice(-4)).join(","));
check("freeName of a free name", freeName("x", ["y"]) === "x");
check("freeName of a short name", freeName("x", ["x", "x (2)"]) === "x (3)", freeName("x", ["x", "x (2)"]));
// Saved under the names freeName gives, three imports are three entries,
// and deleting one leaves the others.
let imports = [{name: long, link: good}];
for (let i = 0; i < 3; i++) {
  const n = freeName(long, visibleScenarios(imports).map(x => x.name));
  imports = putScenario(imports, {name: n, link: "namespace=llm&rate=" + i}).raw;
}
check("three imports are four entries", visibleScenarios(imports).length === 4, visibleScenarios(imports).length);
check("deleting one leaves three", visibleScenarios(dropScenario(imports, visibleScenarios(imports)[1].name)).length === 3);

// cleanName: control characters out, trimmed, at most MAX_NAME.
check("cleanName drops control characters", cleanName(" a\r\nb\u0000 ") === "a  b", JSON.stringify(cleanName(" a\r\nb\u0000 ")));
check("cleanName cuts", cleanName("x".repeat(200)).length === MAX_NAME);
check("cleanName gives a name that checks", checkScenario({name: cleanName("m\t" + "y".repeat(100)), link: good}).name.length <= MAX_NAME);

// delta and fleetDelta: the compare table's change column.
const f2 = v => v.toFixed(2);
for (const [a, b, text, cls] of [[1, 2, "+1.00", "up"], [2, 1, "-1.00", "down"], [1, 1.001, "same", ""], [null, 1, "", ""], [1, null, "", ""], [0, 0.5, "+0.50", "up"]]) {
  const d = delta(a, b, f2);
  check(`delta(${a}, ${b})`, d.text === text && d.cls === cls, JSON.stringify(d));
}
check("fleetDelta of one role", fleetDelta({prefill: 2, decode: 1}, {prefill: 4, decode: 1}) === "+2 prefill");
check("fleetDelta of both", fleetDelta({prefill: 2, decode: 3}, {prefill: 1, decode: 4}) === "-1 prefill, +1 decode", fleetDelta({prefill: 2, decode: 3}, {prefill: 1, decode: 4}));
check("fleetDelta of none", fleetDelta({prefill: 1, decode: 1}, {prefill: 1, decode: 1}) === "same");
// A decode-only fleet has no prefill count: it counts as 0.
check("fleetDelta adding prefill to a decode-only fleet", fleetDelta({decode: 2}, {prefill: 1, decode: 2}) === "+1 prefill");
check("fleetDelta of a decode-only change", fleetDelta({decode: 2}, {decode: 3}) === "+1 decode", fleetDelta({decode: 2}, {decode: 3}));

// delta reads the figures as printed: 10.06 and 10.22 print 10.1 and 10.2,
// and the change is +0.10 r/s, not the +0.16 between the raw values.
const fr = v => v.toFixed(Math.abs(v) >= 10 ? 1 : 2) + " r/s";
check("delta of the printed figures", delta(10.06, 10.22, fr).text === "+0.10 r/s", delta(10.06, 10.22, fr).text);
check("delta of the printed figures, down", delta(10.22, 10.06, fr).text === "-0.10 r/s" && delta(10.22, 10.06, fr).cls === "down");
check("delta across units falls back to the values", delta(0.9, 1.2, v => v < 1 ? (v * 1000).toFixed(0) + " ms" : v.toFixed(2) + " s").text === "+300 ms");

// figDelta says where a side is refused.
const fig = v => ({value: v}), refused = {refused: {reason: "Saturated"}};
check("figDelta both refused", figDelta(refused, refused, fr).text === "both refused");
check("figDelta now refused", figDelta(fig(2), refused, fr).text === "now refused");
check("figDelta now priced", figDelta(refused, fig(2), fr).text === "now priced");
check("figDelta of two values", figDelta(fig(2), fig(3), fr).text === "+1.00 r/s");
check("figDelta with a side missing", figDelta(undefined, fig(3), fr).text === "");

// compareRows: every text escaped, the refusals said, headroom coloured only
// at the same rate, the header and row names.
const hostile = '<img src=x onerror="alert(1)">';
const plan = (o = {}) => ({
  scenario: {fleet: {prefill: 2, decode: 1}},
  limits: {fleet: fig(o.fleet ?? 11.2), prefill: fig(11.6), decode: o.decode === null ? undefined : fig(o.decode ?? 11.2),
    chosen: {rate: o.rate ?? 10, headroom: o.headroom ?? fig(1.2)}},
  ttft: {statistic: o.stat ?? "p90", line: {target: 0.5}}, tpot: {statistic: "mean", target: 0.004},
});
const fmts = {rate: fr, fleet: n => `${n.prefill} prefill ${hostile}`, fig: f => figValue(f) != null ? fr(figValue(f)) : "refused " + hostile,
  bind: () => "decode binds " + hostile, slo: (s, t) => `${s} ${t}`, shape: () => "a mix " + hostile};
const figValue = f => f && !f.refused && typeof f.value === "number" ? f.value : null;
const html = compareRows(plan({stat: hostile}), plan({fleet: 12.3, headroom: refused}), fmts);
const tagless = html.replace(/<\/?(table|tr|th|td)( scope="row"| class="[a-z-]*")?>/g, "");
check("compareRows lets no markup through", !tagless.includes("<") && !tagless.includes(">"), tagless.slice(0, 200));
check("compareRows escapes the texts", html.includes("&lt;img src=x onerror=&quot;alert(1)&quot;&gt;"));
// The change cell: a rate formatter whose text carries markup.
const hostileRate = v => fr(v) + " <b>";
const changes = compareRows(plan(), plan({fleet: 12.3}), {...fmts, rate: hostileRate})
  .replace(/<\/?(table|tr|th|td)( scope="row"| class="[a-z-]*")?>/g, "");
check("compareRows escapes the change cell", !changes.includes("<b>") && changes.includes("+1.10 r/s &lt;b&gt;"), changes.slice(0, 160));
// The row names are the page's own words; the row template escapes them
// anyway, and every other text it puts in a cell.
const rowTemplate = (compareRows.toString().match(/const row = [\s\S]*?\);\r?\n/) || [""])[0];
const holes = rowTemplate.match(/\$\{[^}]*\}/g) || [];
check("the row template has its holes", holes.length === 5, holes.join(" "));
check("every hole in the row template is escaped", holes.every(h => h.startsWith("${esc(") || h === '${d.cls ? "d-" + d.cls : ""}'), holes.join(" "));
check("compareRows headers", html.includes("<th>A (dashed)</th><th>Current</th><th>Change</th>"));
check("compareRows names the rate row", html.includes(">Rate you set<") && !html.includes(">Rate set<"));
check("compareRows says now refused", html.includes(">now refused<"));
check("compareRows colours a limit that rose", /Fleet limit<\/th><td>[^<]*<\/td><td>[^<]*<\/td><td class="d-up">\+1.10 r\/s</.test(html), html.match(/Fleet limit.*?<\/tr>/)?.[0]);
const sameRate = compareRows(plan(), plan({headroom: fig(2.2)}), fmts);
const otherRate = compareRows(plan(), plan({headroom: fig(2.2), rate: 9}), fmts);
const headroomCell = h => (h.match(/Headroom at the rate<\/th><td>[^<]*<\/td><td>[^<]*<\/td><td class="([^"]*)">/) || [])[1];
check("headroom coloured at the same rate", headroomCell(sameRate) === "d-up", headroomCell(sameRate));
check("headroom not coloured at another rate", headroomCell(otherRate) === "", headroomCell(otherRate));
check("rate change is never coloured", /Rate you set<\/th><td>[^<]*<\/td><td>[^<]*<\/td><td class="">-1.00 r\/s</.test(otherRate), otherRate.match(/Rate you set.*?<\/tr>/)?.[0]);
check("compareRows drops a role neither side has", !compareRows(plan({decode: null}), plan({decode: null}), fmts).includes("Decode limit"));

// scenarioTitle: whole clauses dropped from the end, headroom first.
const short = scenarioTitle(["m2", "1 decode", "2.00 r/s", "TTFT p90 1 s", "TPOT mean 50 ms", "headroom 0.85"]);
check("scenarioTitle keeps every clause that fits", short === "m2, 1 decode, 2.00 r/s, TTFT p90 1 s, TPOT mean 50 ms, headroom 0.85", short);
// 87 characters in all: the headroom goes, whole.
const title = scenarioTitle(["model-02", "2 prefill, 1 decode", "11.2 r/s", "TTFT p90 500 ms", "TPOT mean 4.0 ms", "headroom 0.85"]);
check("scenarioTitle drops the headroom first", title === "model-02, 2 prefill, 1 decode, 11.2 r/s, TTFT p90 500 ms, TPOT mean 4.0 ms", title);
const t2 = scenarioTitle(["m".repeat(40), "2 prefill, 1 decode", "11.2 r/s", "TTFT p90 500 ms", "TPOT mean 4.0 ms", "headroom 0.85"]);
check("scenarioTitle drops whole clauses until it fits", t2 === "m".repeat(40) + ", 2 prefill, 1 decode, 11.2 r/s", t2);
check("scenarioTitle cuts only a first clause too long", scenarioTitle(["x".repeat(100), "y"]).length === MAX_NAME);

// sourceProblem: what a server that lists sources can open.
const recs = {prometheus: false, namespaces: [], recordings: ["a.jsonl.gz"]};
const prom = {prometheus: true, namespaces: ["llm"], recordings: []};
check("sourceProblem: a listed recording", sourceProblem("recording=a.jsonl.gz", recs) === "");
check("sourceProblem: an unlisted recording", sourceProblem("recording=b.jsonl.gz", recs).includes("does not list"));
check("sourceProblem: a namespace on a recordings-only server", sourceProblem("namespace=llm&rate=1", recs).includes("does not read"));
check("sourceProblem: an allowed namespace", sourceProblem("namespace=llm", prom) === "");
check("sourceProblem: a namespace not allowed", sourceProblem("namespace=other", prom).includes("may not read"));
check("sourceProblem: sources not listed yet", sourceProblem("namespace=llm", null).includes("not listed"));
check("sourceProblem: no source", sourceProblem("rate=1", prom) === "it names no source");

// The link handshake: the page's link, a scenario opened, loads overtaken.
let link = {seq: null, restore: null};
link = linkAtLoadStart(link, 1);
let end = linkAtLoadEnd(link, 1, 1, true);
check("the first load applies the page's link", end.apply && end.link === null);
end = linkAtLoadEnd(linkAtLoadStart({seq: null, restore: null}, 1), 1, 1, false);
check("a failed first load keeps the link for the next", !end.apply && end.link && end.link.seq === null && !end.restore);
check("the next load takes it", linkAtLoadStart(end.link, 2).seq === 2);
// Load pressed before the first load returns: the newer load applies it.
link = linkAtLoadStart(linkAtLoadStart({seq: null, restore: null}, 1), 2);
check("an overtaken load does nothing", !linkAtLoadEnd(link, 1, 2, true).apply);
check("the overtaking load applies the link", linkAtLoadEnd(link, 2, 2, true).apply);
// A scenario opened, whose load fails: put back, and no later load applies it.
const before = {linked: "recording=a.jsonl.gz&prefill=2", hash: "#recording=a.jsonl.gz&prefill=2", form: {rec: "a.jsonl.gz", prom: false}, name: "mine"};
const open = beginOpen(before, {name: "dead", link: "recording=a.jsonl.gz&model=none&prefill=5"});
check("beginOpen puts the scenario on the page", open.next.linked === "recording=a.jsonl.gz&model=none&prefill=5" &&
  open.next.hash === "#recording=a.jsonl.gz&model=none&prefill=5" && open.next.name === "dead");
link = linkAtLoadStart(open.link, 7);
end = linkAtLoadEnd(link, 7, 7, false);
check("a failed scenario load restores what was before", end.restore === before && !end.apply && end.link === null);
check("what is restored is the page before", end.restore.linked === before.linked && end.restore.hash === before.hash &&
  end.restore.form.rec === "a.jsonl.gz" && end.restore.name === "mine");
check("no later load applies a failed scenario", linkAtLoadStart(end.link, 8) === null);
check("a scenario load that succeeds applies it", linkAtLoadEnd(linkAtLoadStart(beginOpen(before, {name: "x", link: "namespace=llm"}).link, 9), 9, 9, true).apply);
// A Load pressed while the scenario loads takes it over, rollback included.
link = linkAtLoadStart(linkAtLoadStart(open.link, 3), 4);
check("an overtaking load carries the rollback", linkAtLoadEnd(link, 4, 4, false).restore === before);

// importPlan: the import branches; raw is never changed.
const store0 = [{name: "a", link: good, saved: "t0"}, foreign];
const snapshot = JSON.stringify(store0);
let imp = importPlan(store0, {name: "a", link: good, saved: "t1"});
check("same name, same link: opened, not saved", !imp.save && imp.name === "a" && imp.raw === store0);
imp = importPlan(store0, {name: "a", link: "namespace=llm", saved: "t1"});
check("same name, another link: suffixed and saved", imp.save && imp.name === "a (2)" &&
  visibleScenarios(imp.raw).map(x => x.name).join("|") === "a|a (2)", imp.name);
imp = importPlan(store0, {name: "new", link: good, saved: "t1"});
check("a new name: saved as it is", imp.save && imp.name === "new" && hasForeign(imp.raw));
check("importPlan never changes the store it read", JSON.stringify(store0) === snapshot);
throws("importPlan at the cap refuses, saving nothing", () => importPlan(full, {name: "one more", link: good}), "at most");

// The saved time is kept, and cut at 40 characters.
check("putScenario keeps .saved", visibleScenarios(putScenario([], {name: "t", link: good, saved: "2026-11-14 21:08"}).raw)[0].saved === "2026-11-14 21:08");
check("putScenario cuts .saved at 40", visibleScenarios(putScenario([], {name: "t", link: good, saved: "s".repeat(60)}).raw)[0].saved.length === 40);

// sameCurve: A drawn under the current curve.
const c1 = [{x: 1, y: 0.1}, {x: 2, y: null}, {x: 3, y: 0.3}];
check("sameCurve of one curve", sameCurve(c1, c1.map(q => ({...q}))));
check("sameCurve within rounding", sameCurve(c1, [{x: 1, y: 0.1 + 1e-13}, {x: 2, y: null}, {x: 3, y: 0.3}]));
check("sameCurve of another curve", !sameCurve(c1, [{x: 1, y: 0.1}, {x: 2, y: 0.2}, {x: 3, y: 0.3}]));
check("sameCurve of other lengths", !sameCurve(c1, c1.slice(1)) && !sameCurve([], []));

// parseEdges: the bucket edges box, by the server's rules
// (internal/whatifapi/scenario.go): token counts or Nk, at most 8,
// ascending, each from 1 to 1024k. The text is the link's canonical form:
// each edge by its bucket's name, as the server names the line.
const edgesOf = s => { const r = parseEdges(s); return JSON.stringify(r.edges) + " " + r.text; };
for (const [s, want] of [
  ["", "[] "],
  ["2048", "[2048] 2k"],
  ["2000", "[2000] 2000"],
  ["2k, 16K,64k", "[2048,16384,65536] 2k,16k,64k"],
  ["1000 2048\t8k", "[1000,2048,8192] 1000,2k,8k"],
  ["1,2,3,4,5,6,7,8", "[1,2,3,4,5,6,7,8] 1,2,3,4,5,6,7,8"],
  ["1048576", "[1048576] 1024k"],
]) {
  let got;
  try { got = edgesOf(s); } catch (e) { got = "threw " + e.message; }
  check(`parseEdges reads ${JSON.stringify(s)}`, got === want, got);
}
for (const [s, want] of [
  ["1,2,3,4,5,6,7,8,9", "9 edges; give at most 8"],
  ["0", "outside 1 to 1024k"],
  ["1025k", "outside 1 to 1024k"],
  ["1048577", "outside 1 to 1024k"],
  ["4k,1k", "1k is not above the edge before it"],
  ["2k,2048", "2048 is not above the edge before it"],
  ["1.5k", "not a token count"],
  ["-5", "not a token count"],
  ["16m", "not a token count"],
  ["2k;16k", "not a token count"],
  ["1e3", "not a token count"],
]) {
  throws(`parseEdges refuses ${JSON.stringify(s)}`, () => parseEdges(s), want);
}

// A link may carry edges in any form parseEdges reads, so a hand-edited
// edges=2048 or 2K,16k is used; edges that do not parse are refused.
for (const link of ["recording=r&perBucket=1&edges=2k,16k,64k", "recording=r&edges=1000", "namespace=llm&perBucket=1&edges=1,2,3,4,5,6,7,8",
  "recording=r&edges=2048", "recording=r&edges=2K,16k", "recording=r&edges=2k,%2016k"]) {
  check(`admits ${link}`, linkProblem(link) === "", linkProblem(link));
}
for (const link of ["recording=r&edges=", "recording=r&edges=4k,1k", "recording=r&edges=0",
  "recording=r&edges=1,2,3,4,5,6,7,8,9", "recording=r&edges=2k%3Cb%3E", "recording=r&edges=2k&edges=4k", "recording=r&edges=%20"]) {
  const got = linkProblem(link);
  check(`refuses ${link}`, got !== "", JSON.stringify(got));
}

// linkedEdges: a link's edges are used in canonical form, so the page
// rewrites the link; edges that do not parse are not used, and said.
for (const [v, edges, note] of [
  [null, "", null],
  ["", "", null],
  ["2048", "2k", null],
  ["2K,16k", "2k,16k", null],
  ["2000 16384", "2000,16k", null],
  ["4k,1k", "", `The link's bucket edges "4k,1k" were not used (1k is not above the edge before it: give the edges smallest first, each once): the default buckets are priced.`],
  ["1,2,3,4,5,6,7,8,9", "", `The link's bucket edges "1,2,3,4,5,6,7,8,9" were not used (9 edges; give at most 8): the default buckets are priced.`],
  ["lots", "", `The link's bucket edges "lots" were not used (lots is not a token count such as 2048 or 16k): the default buckets are priced.`],
]) {
  const got = linkedEdges(v);
  check(`linkedEdges of ${JSON.stringify(v)}`, got.edges === edges && got.note === note, JSON.stringify(got));
}

// edgesBoxProblem: the box is marked while its text is not the edges in use,
// and clear once it is.
for (const [raw, inUse, want] of [
  ["2k, 16k", "2k,16k", ""],
  ["", "", ""],
  ["2048,16384", "2k,16k", ""],
  ["4k,1k", "2k,16k", "Not used: 1k is not above the edge before it: give the edges smallest first, each once; the lines show 2k, 16k."],
  ["lots", "", "Not used: lots is not a token count such as 2048 or 16k; the lines show the default buckets."],
  ["2k,64k", "2k,16k", "Not used yet; the lines show 2k, 16k."],
  ["", "2k,16k", "Not used yet; the lines show 2k, 16k."],
]) {
  const got = edgesBoxProblem(raw, inUse);
  check(`edgesBoxProblem of ${JSON.stringify(raw)} with ${JSON.stringify(inUse)}`, got === want, JSON.stringify(got));
}

// beyondContext and contextNote: a line flagged on either point, answered
// or refused, is past the window, and says which.
const lineWith = (rateFlags, targetRefusedFlags) => ({atRate: {figure: {value: 1, flags: rateFlags}},
  atTarget: {rate: targetRefusedFlags ? {refused: {reason: "TargetBelowFloor", flags: targetRefusedFlags}} : {value: 2}}});
check("beyondContext on point A", beyondContext(lineWith(["EdgeBeyondContext"], null)));
check("beyondContext on a refused point B", beyondContext(lineWith([], ["EdgeBeyondContext"])));
check("beyondContext of a line within the window", !beyondContext(lineWith(["AssumedVP"], ["AssumedVP"])));
check("beyondContext of a bare line", !beyondContext({}));
check("contextNote with the window", contextNote(16384) === "past the 16k-token context window", contextNote(16384));
check("contextNote without it", contextNote(null) === "past the context window");

// bucketSeries: five colours, dashed from the sixth line on, dimmed past the
// window.
check("bucketSeries of the first", JSON.stringify(bucketSeries(0, false)) === '{"cls":"c-0","extra":""}');
check("bucketSeries of the sixth", JSON.stringify(bucketSeries(5, false)) === '{"cls":"c-0","extra":" rep"}', JSON.stringify(bucketSeries(5, false)));
check("bucketSeries of the eighth past the window", JSON.stringify(bucketSeries(7, true)) === '{"cls":"c-2","extra":" rep beyond"}');
check("bucketSeries of the fifth past the window", JSON.stringify(bucketSeries(4, true)) === '{"cls":"c-4","extra":" beyond"}');

// An imported scenario: edges in the form the page writes are read back; any
// other form refuses the file, naming the edges.
const withEdges = good + "&edges=2k,16k,64k";
check("reads an exported file with edges", readScenarioFile(JSON.stringify({kind: "capacity-planner-scenario", version: 1, name: "e", link: withEdges})).link === withEdges);
throws("refuses a file with edges out of order", () => readScenarioFile(JSON.stringify({kind: "capacity-planner-scenario", version: 1, name: "e", link: good + "&edges=64k,2k"})), "edges cannot be");
throws("refuses a file with too many edges", () => readScenarioFile(JSON.stringify({kind: "capacity-planner-scenario", version: 1, name: "e",
  link: good + "&edges=" + Array.from({length: 9}, (_, i) => i + 1).join(",")})), "edges cannot be");
const impEdges = importPlan([], {name: "e", link: withEdges, saved: "t"});
check("importPlan saves a scenario with edges", impEdges.save && visibleScenarios(impEdges.raw)[0].link === withEdges);

// Compare: A pinned with other edges than the current plan. The limits do
// not move; a Buckets row says the edges changed, and is absent where they
// are the same.
const withLines = (p, edges) => Object.assign(p, {scenario: Object.assign({}, p.scenario, edges ? {bucketEdges: edges} : {}),
  ttft: Object.assign({}, p.ttft, {perBucket: (edges || [1024, 4096]).map(e => ({bucket: String(e), curve: []}))})});
const aEdges = withLines(plan(), [2048, 16384]), curEdges = withLines(plan(), null);
const edgeTable = compareRows(aEdges, curEdges, fmts);
check("compareRows with other edges says the buckets changed",
  edgeTable.includes(`<th scope="row">Buckets</th><td>2k, 16k</td><td>1k, 4k, 8k, 32k, 128k (default)</td><td class="">changed</td>`), edgeTable);
check("compareRows with other edges: the limits read the same", (edgeTable.match(/>same</g) || []).length >= 4, edgeTable);
const sameEdges = compareRows(withLines(plan(), [2048]), withLines(plan(), [2048]), fmts);
check("compareRows with the same edges has no Buckets row", !sameEdges.includes(">Buckets<"), sameEdges);

// retryDelay: only a busy server's 503, which sets Retry-After, is asked
// again; a plan stopped at its budget (503, no Retry-After) never is, or the
// page would price it from the start up to five more times.
check("retryDelay waits Retry-After on a busy 503", retryDelay(503, "1", 0) === 1000, retryDelay(503, "1", 0));
check("retryDelay waits 2 s on Retry-After 2", retryDelay(503, "2", 4) === 2000, retryDelay(503, "2", 4));
check("retryDelay never retries a 503 without Retry-After (PlanBudgetExceeded)", retryDelay(503, null, 0) === -1, retryDelay(503, null, 0));
check("retryDelay never retries a 503 with an empty Retry-After", retryDelay(503, " ", 0) === -1, retryDelay(503, " ", 0));
check("retryDelay stops after five retries", retryDelay(503, "1", 5) === -1, retryDelay(503, "1", 5));
check("retryDelay reads an unreadable Retry-After as 1 s", retryDelay(503, "soon", 0) === 1000, retryDelay(503, "soon", 0));
check("retryDelay waits at most 30 s", retryDelay(503, "3600", 0) === 30000, retryDelay(503, "3600", 0));
check("retryDelay never retries a 200", retryDelay(200, "1", 0) === -1, retryDelay(200, "1", 0));
check("retryDelay never retries a 500", retryDelay(500, "1", 0) === -1, retryDelay(500, "1", 0));


// The fleet report: the body the page posts to v1/report from its controls,
// the shapes and loads it refuses before asking, every string it draws, and a
// server with no report route told apart from a dropped inputsId.
{
  const {reportShapeProblem, shapeText, reportShapes, parseReportLoads, reportRequest, reportView, reportMissing, reportCurrent,
    refusalText, flagSummary} = ctx;
  check("measured is a shape", reportShapeProblem("measured") === "");
  check("load is a shape", reportShapeProblem("load") === "");
  check("IL/OL is a shape", reportShapeProblem("8k/200") === "");
  check("IL/OL/hit is a shape", reportShapeProblem("1k/4k/0.3") === "");
  check("one length is not a shape", reportShapeProblem("8k") !== "");
  check("a hit of 1 is not a shape", reportShapeProblem("1k/1k/1") !== "");
  check("a length past 1024k is refused", reportShapeProblem("2000k/200") !== "");
  check("blank boxes are the measured shape", shapeText({il: "", ol: " ", hit: ""}) === "measured");
  check("a row with a hit", shapeText({il: "1k", ol: "4k", hit: "0.3"}) === "1k/4k/0.3");
  check("blank rows are one measured shape", JSON.stringify(reportShapes([{}, {il: "1k", ol: "1k"}, {}, {il: "1k", ol: "1k"}])) ===
    JSON.stringify(["measured", "1k/1k"]), JSON.stringify(reportShapes([{}, {il: "1k", ol: "1k"}, {}])));
  check("no rows is the measured shape", JSON.stringify(reportShapes([])) === JSON.stringify(["measured"]));
  check("empty loads are the server's default", parseReportLoads("").loads.length === 0 && parseReportLoads("").problem === "");
  check("loads are read in order", JSON.stringify(parseReportLoads("50%, 5r/s ,seen").loads) === JSON.stringify(["50%", "5r/s", "seen"]));
  check("a share past 100% is refused", parseReportLoads("150%").problem !== "");
  check("a load with no unit is refused", parseReportLoads("5").problem !== "");

  const slo = {ttft: {statistic: "p95", target: 2}, tpot: {statistic: "mean", target: 0.03}};
  const asked = reportRequest({inputsId: "sha256:x", fleet: {decode: 3}, shapes: [{}, {il: "1k", ol: "1k"}, {il: "16k", ol: "200", hit: "0.3"}, {}],
    loads: "50%, 80%, 5r/s, seen", growth: "1.2", loadStatistic: "p95", headroom: 0.8, slo, assume: {vp: 200000}});
  const want = {inputsId: "sha256:x", shapes: ["measured", "1k/1k", "16k/200/0.3"], fleet: {decode: 3}, loads: ["50%", "80%", "5r/s", "seen", "seen x1.2"],
    loadStatistic: "p95", headroom: 0.8, slo, ttftStatistics: ["p95"], tpotStatistic: "mean", assume: {vp: 200000}};
  check("the body carries every control", asked.problem === "" && JSON.stringify(asked.body) === JSON.stringify(want), JSON.stringify(asked));
  check("the headroom knob is sent", reportRequest({inputsId: "i", headroom: 0.7}).body.headroom === 0.7);
  check("a headroom of 1 is left to the policy", !("headroom" in reportRequest({inputsId: "i", headroom: 1}).body));
  check("no rows asks for the measured shape", JSON.stringify(reportRequest({inputsId: "i"}).body) === JSON.stringify({inputsId: "i", shapes: ["measured"]}));
  check("a growth of 1 is left out", !("loads" in reportRequest({inputsId: "i", growth: "1"}).body));
  check("a bad growth is refused", reportRequest({inputsId: "i", growth: "-2"}).body === null);
  check("a bad shape is refused before asking", reportRequest({inputsId: "i", shapes: [{il: "8k"}]}).body === null);
  check("seven shapes are refused", reportRequest({inputsId: "i", shapes: [1, 2, 3, 4, 5, 6, 7].map(n => ({il: n + "k", ol: "1k"}))}).body === null);
  check("a bad load is refused before asking", reportRequest({inputsId: "i", loads: "fast"}).body === null);

  const row = (load, rate, ttft, tpot, mark) => ({load, rate, ttft: [{statistic: "p90", value: 0.001}, {statistic: "p95", ...ttft}], tpotMean: tpot,
    tpotPercentile: {statistic: "p90", value: 0.0164}, cell: {mark}});
  const knees = (knee, binds) => ({fleetKnee: {value: knee, binds}, headroom: 0.85, prefillKnee: {value: 49.8}, decodeKnee: {value: knee},
    operating: {value: 0.85 * knee}, maxAtSLO: {value: 26.2}});
  const v = reportView({slo, load: {rate: {value: 47.39}}, loads: ["50%", "seen x1.5"], notValidated: ["decode knee: not validated", "TTFT is UNVALIDATED"],
    shapes: [
      {label: "measured", flags: ["UnvalidatedTTFT", "AssumedPoissonArrivals", "SomethingRare"], capacity: knees(38.14, "decode"),
        verdict: {verdict: "UNDER-PROVISIONED", reason: "TTFT p95 AboveDecodeOnset misses 2000.0 ms"}, utilization: {decode: {kvFill: {refused: {reason: "Saturated"}}, bandTop: 0.8}, prefill: {}},
        table: [row("50%", 19.07, {value: 0.6367}, {value: 0.00699}, "met"), row("seen x1.5", 47.39, {refused: {reason: "Saturated"}}, {value: 0.0271}, "saturated")]},
      {label: "1k/1k", source: "asked", extrapolated: true, extrapolatedWhy: "IL 1024 outside", capacity: knees(36.55, "decode"),
        verdict: {verdict: "OVER-PROVISIONED", shrink: ["prefill", "decode"]}, utilization: {decode: {}, prefill: {}},
        table: [row("50%", 18.27, {value: 1.5894}, {value: 0.007}, "missed")]}]});
  check("a verdict past the knee says so in words", v.verdicts[0].text ===
    "UNDER-PROVISIONED: at 47.4 r/s the load is 1.2x the fleet's 38.1 r/s knee; add decode replicas.", v.verdicts[0].text);
  check("an over-provisioned verdict names the roles", v.verdicts[1].text ===
    "OVER-PROVISIONED: if all 47.4 r/s were 1k/1k requests, one prefill or one decode replica fewer still meets every target with headroom.", v.verdicts[1].text);
  check("a load all shapes price alike carries its rate in the header", v.header[1] === "seen x1.5 (47.4 r/s)", v.header[1]);
  check("a load whose rate differs by shape keeps it in the cell", v.header[0] === "50%" && v.rows[0].cells[0].text.endsWith("@ 19.1 r/s"), v.rows[0].cells[0].text);
  check("a cell shows the SLO's statistics, rounded", v.rows[0].cells[0].text.startsWith("TTFT 637 ms · TPOT 7.0 ms"), v.rows[0].cells[0].text);
  check("a cell's figure and mark are the same statistic", v.rows[1].cells[0].text.startsWith("TTFT 1589 ms") && v.rows[1].cells[0].word === "MISS");
  check("a saturated figure reads saturated", v.rows[0].cells[1].text.startsWith("TTFT saturated") && v.rows[0].cells[1].word === "SAT");
  check("a load with no row says not priced", v.rows[1].cells[1].text === "not priced" && v.rows[1].cells[1].cls === "none");
  check("the legend names the statistics and targets", v.legendStats === "Each cell is TTFT p95 · TPOT mean. Marks are judged against TTFT p95 2000 ms and TPOT mean 30 ms.",
    v.legendStats);
  check("an extrapolated shape is marked", v.rows[1].extrapolated && !v.rows[0].extrapolated && v.rows[1].why === "IL 1024 outside");
  check("a refusal reads in words", v.details[0].lines.some(l => l.includes("At the load: decode saturated: no steady state at this load")),
    JSON.stringify(v.details[0].lines));
  check("common flags read as phrases, the rest counted", JSON.stringify(v.details[0].flags.phrases) ===
    JSON.stringify(["TTFT is not validated", "assumes Poisson arrivals"]) && v.details[0].flags.more === 1 &&
    v.details[0].flags.codes === "UnvalidatedTTFT, AssumedPoissonArrivals, SomethingRare");
  check("the notes stay apart", v.notes.length === 2);
  // Each verdict band, each said in its own words.
  const one = (load, capacity, verdict, extra) => reportView(Object.assign({slo, load: {rate: {value: load}}, loads: [],
    shapes: [Object.assign({label: "measured", capacity, verdict, utilization: {decode: {}, prefill: {}}, table: []}, extra || {})]})).verdicts[0].text;
  check("between headroom and the knee the verdict says past 85%",
    one(35, knees(38.14, "decode (prefill not priced)"), {verdict: "UNDER-PROVISIONED"}) ===
    "UNDER-PROVISIONED: at 35 r/s the fleet is past 85% of its 38.1 r/s knee, set by decode; add decode replicas.",
    one(35, knees(38.14, "decode (prefill not priced)"), {verdict: "UNDER-PROVISIONED"}));
  check("past both roles' knees both are named", one(47.39, Object.assign(knees(38.14, "decode"), {prefillKnee: {value: 30}}), {verdict: "UNDER-PROVISIONED"}) ===
    "UNDER-PROVISIONED: at 47.4 r/s the load is 1.2x the fleet's 38.1 r/s knee; add prefill and decode replicas.");
  check("a missed TTFT is named, with no role guessed", one(31.6, knees(38.14, "decode"), {verdict: "UNDER-PROVISIONED",
    reason: "2 prefill + 2 decode: 31.60 r/s within 0.85 x the fleet knee 38.14 r/s = 32.42 r/s; TTFT p95 7621.0 ms misses 2000.0 ms; TPOT mean 7.6 ms meets 30.0 ms"}) ===
    "UNDER-PROVISIONED: at 31.6 r/s TTFT p95 7.6 s misses 2 s.");
  check("a missed TPOT asks for decode", one(31.6, knees(38.14, "decode"), {verdict: "UNDER-PROVISIONED",
    reason: "x; TPOT mean 41.2 ms misses 30.0 ms"}) === "UNDER-PROVISIONED: at 31.6 r/s TPOT mean 41 ms misses 30 ms; add decode replicas.");

  // The TPOT percentile the SLO judges is the one shown and named.
  const p90 = {ttft: {statistic: "p95", target: 2}, tpot: {statistic: "p90", target: 0.03}};
  const vp = reportView({slo: p90, load: {rate: {value: 10}}, loads: ["10r/s"], shapes: [{label: "measured", capacity: knees(38.14, "decode"),
    verdict: {}, utilization: {decode: {}, prefill: {}}, table: [row("10r/s", 10, {value: 0.5}, {value: 0.00699}, "met")]}]});
  check("a p90 TPOT SLO shows the p90", vp.rows[0].cells[0].text === "TTFT 500 ms · TPOT 16 ms", vp.rows[0].cells[0].text);
  check("a p90 TPOT SLO is named in the legend", vp.legendStats.startsWith("Each cell is TTFT p95 · TPOT p90."), vp.legendStats);
  check("a rate load is not named twice", vp.header[0] === "10r/s", vp.header[0]);
  // A p99 TPOT SLO over a table that prints p90: the cell carries the p99,
  // and a cell with none never shows the p90 under the p99's name (#299).
  const p99 = {ttft: {statistic: "p95", target: 2}, tpot: {statistic: "p99", target: 0.03}};
  const at99 = cell => reportView({slo: p99, load: {rate: {value: 10}}, loads: ["10r/s"], shapes: [{label: "measured", capacity: knees(38.14, "decode"),
    verdict: {}, utilization: {decode: {}, prefill: {}}, table: [Object.assign(row("10r/s", 10, {value: 0.5}, {value: 0.00699}, "met"), {cell})]}]}).rows[0].cells[0].text;
  check("a p99 TPOT SLO shows the cell's p99", at99({mark: "met", tpot: {statistic: "p99", value: 0.0251}}) === "TTFT 500 ms · TPOT 25 ms",
    at99({mark: "met", tpot: {statistic: "p99", value: 0.0251}}));
  check("a p99 TPOT SLO never shows the table's p90; the mean says so", at99({mark: "met"}) === "TTFT 500 ms · TPOT 7.0 ms (mean)",at99({mark: "met"}));
  check("a verdict carries what the self-check says of it", S("verdictLine")({verdict: "RIGHT-SIZED", summary: "RIGHT-SIZED: holds.", trust: "not trusted: x"}).trust === "not trusted: x");
  check("a verdict the self-check says nothing of has no trust", S("verdictLine")({verdict: "RIGHT-SIZED", summary: "RIGHT-SIZED: holds."}).trust === "");
  check("a TTFT is read at the SLO's statistic, not the first one", v.rows[0].cells[0].text.startsWith("TTFT 637 ms"), v.rows[0].cells[0].text);
  check("a cell's parts stay apart for wrapping", JSON.stringify(vp.rows[0].cells[0].parts) === JSON.stringify(["TTFT 500 ms", "TPOT 16 ms"]));

  // A TPOT at the decode line's floor says so.
  const vf = reportView({slo, load: {rate: {value: 10}}, loads: ["10r/s"], shapes: [{label: "measured", capacity: knees(38.14, "decode"),
    verdict: {}, utilization: {decode: {}, prefill: {}}, table: [row("10r/s", 10, {value: 0.5}, {value: 0.00699, flags: ["BelowFittedBand"]}, "met")]}]});
  check("a floor TPOT reads at most", vf.rows[0].cells[0].text.includes("TPOT ≤7.0 ms"), vf.rows[0].cells[0].text);
  check("the legend explains the floor mark", vf.legendStats.includes("≤ marks a TPOT at the decode line's floor"), vf.legendStats);
  check("no floor, no floor note", !v.legendStats.includes("floor"));
  // Growth is a load of its own, and the verdict stays at the load seen.
  check("growth with no loads adds to the default loads", JSON.stringify(reportRequest({inputsId: "i", growth: "1.5"}).body.loads) ===
    JSON.stringify(["50%", "80%", "100%", "seen", "seen x1.5"]));
  check("growth never sends loadSeenX", !("loadSeenX" in reportRequest({inputsId: "i", growth: "1.5"}).body));
  check("growth past the most loads is refused", reportRequest({inputsId: "i", loads: "10%, 20%, 30%, 40%, 50%, 60%, 70%, 80%", growth: "2"}).body === null);
  check("a grown load may be typed", parseReportLoads("seen x1.5, seen×2, seen*3").problem === "");
  check("the load statistic is sent", reportRequest({inputsId: "i", loadStatistic: "peak"}).body.loadStatistic === "peak");
  const vg = reportView({slo, load: {rate: {value: 41.7}, statistic: "p95", mean: 31.59, p95: 41.7, peak: 43.24, cyclesWithTraffic: 282},
    loads: ["seen", "seen x1.5"], shapes: [
      {label: "measured", capacity: knees(38.14, "decode"), utilization: {decode: {}, prefill: {}}, table: [],
        verdict: {verdict: "UNDER-PROVISIONED", scope: "fleet", summary: "UNDER-PROVISIONED: at the p95 load of 41.7 r/s, 1.1x the fleet knee of 38.1 r/s, the fleet saturates."},
        growth: [{load: "seen x1.5", x: 1.5, rate: 62.55, verdict: {verdict: "UNDER-PROVISIONED", summary: "UNDER-PROVISIONED: at 1.5 x the p95 load (62.6 r/s), the fleet saturates."}}]},
      {label: "8k/200", source: "asked", capacity: knees(38.14, "decode"), utilization: {decode: {}, prefill: {}}, table: [],
        verdict: {verdict: "RIGHT-SIZED", scope: "decode only", summary: "RIGHT-SIZED: at the p95 load of 41.7 r/s every target holds."}}]});
  check("the verdict is the server's summary", vg.verdicts[0].rest === "at the p95 load of 41.7 r/s, 1.1x the fleet knee of 38.1 r/s, the fleet saturates.",
    vg.verdicts[0].rest);
  check("growth is its own line beside the verdict", vg.verdicts[0].growth.length === 1 && vg.verdicts[0].growth[0].lead === "with 1.5× traffic (62.6 r/s)" &&
    vg.verdicts[0].growth[0].verdict === "UNDER-PROVISIONED" && vg.verdicts[0].verdict === "UNDER-PROVISIONED", JSON.stringify(vg.verdicts[0].growth));
  check("a decode-only verdict says so", vg.verdicts[1].decodeOnly && !vg.verdicts[0].decodeOnly);
  check("a typed shape's verdict is a what-if", vg.verdicts[1].whatIf && !vg.verdicts[0].whatIf);
  check("the load line names the statistic and the others", vg.load ===
    "Judged at the p95 load of 41.7 r/s; the source saw mean 31.6, p95 41.7, peak 43.2 r/s over 282 busy cycles.", vg.load);
  check("a missed-at-onset cell says so", S("REPORT_MARKS")["missed at onset"][1] === "MISS at onset");
  // The fleet needed: a chip of its own, the sentence not repeating it.
  const {neededText, verdictLine, roleLines, loadLine} = ctx;
  const has = {prefill: 2, decode: 2};
  check("the fleet needed reads in words", neededText({prefill: 3, decode: 4, maxReplicas: 32}, has) === "needs 3 prefill + 4 decode (has 2 + 2)");
  check("alternatives follow it", neededText({prefill: 19, decode: 5, alternatives: [{prefill: 18, decode: 6}], maxReplicas: 32}, has) ===
    "needs 19 prefill + 5 decode (has 2 + 2), or 18 + 6");
  check("decode only needs at least", neededText({prefill: 0, decode: 3, decodeOnly: true, maxReplicas: 32}, has) ===
    "needs at least 3 decode (has 2); prefill is not priced");
  check("an unmet fleet names the bound", neededText({prefill: 0, decode: 0, unmet: "fleet", maxReplicas: 32}, has) ===
    "no fleet up to 32 replicas serves this (fleet does not reach the load)");
  check("no needed is no chip", neededText(undefined, has) === "");
  const vl = verdictLine({verdict: "UNDER-PROVISIONED", needed: {prefill: 19, decode: 5, alternatives: [{prefill: 18, decode: 6}], maxReplicas: 32},
    summary: "UNDER-PROVISIONED: at the p95 load of 41.7 r/s, 1.6x the fleet knee of 26.7 r/s, the fleet saturates; needs 19 prefill + 5 decode (has 2 + 2), or 18 + 6 (prefill f 1.5 assumed at 19 pods)."}, "", has);
  check("the sentence does not repeat the fleet needed", vl.rest === "at the p95 load of 41.7 r/s, 1.6x the fleet knee of 26.7 r/s, the fleet saturates (prefill f 1.5 assumed at 19 pods).",
    vl.rest);
  check("the chip carries it", vl.needs === "needs 19 prefill + 5 decode (has 2 + 2), or 18 + 6");
  const vs = verdictLine({verdict: "UNDER-PROVISIONED", needed: {prefill: 5, decode: 5, maxReplicas: 32},
    summary: "UNDER-PROVISIONED: 1.6x the fleet knee (38.1 r/s); needs 5 prefill + 5 decode (has 2 + 2)."}, "", has);
  check("a short growth sentence loses only the clause", vs.rest === "1.6x the fleet knee (38.1 r/s).", vs.rest);
  check("each role says whether it has a replica to spare", JSON.stringify(roleLines({roles: [{role: "decode", spare: true, detail: "3 decode hold"},
    {role: "prefill", spare: false}]})) === JSON.stringify(["Decode: has a replica to spare (3 decode hold).", "Prefill: no replica to spare."]));
  check("a p95 that is the peak says so", loadLine({rate: {value: 12}, statistic: "p95", p95IsPeak: true, p95: 12, peak: 12, cyclesWithTraffic: 9})
    .startsWith("Judged at the p95 load of 12 r/s (= peak: fewer than 20 cycles);"));
  check("a p95 less noise of few cycles says so", loadLine({rate: {value: 11}, statistic: "p95-modulation", p95IsPeak: true, p95: 12, cyclesWithTraffic: 9})
    .includes("(from the peak: fewer than 20 cycles)"));
  check("the p95 less counting noise is said in words", loadLine({rate: {value: 11}, statistic: "p95-modulation", p95: 12, p95Modulation: 11, cyclesWithTraffic: 40})
    .startsWith("Judged at the p95 less counting noise load of 11 r/s; the source saw p95 12, p95 less counting noise 11 r/s"));
  check("a half rounds up", ctx.roundRate(62.55) === "62.6");
  check("a TTFT is never marked as a floor", ctx.reportView({slo, load: {rate: {value: 10}}, loads: ["10r/s"], shapes: [{label: "m", capacity: knees(38.14, "decode"), verdict: {},
    utilization: {decode: {}, prefill: {}}, table: [Object.assign(row("10r/s", 10, {value: 0.5, flags: ["BelowFittedBand"]}, {value: 0.007}, "met"))]}]}).rows[0].cells[0].parts[0] === "TTFT 500 ms");
  check("decode use reads in words", ctx.decodeUse({kvFill: {value: 0.168}, bandTop: 0.8, slotsOccupied: {value: 48.2}, slots: 228, waitProbability: {value: 0.004}}) ===
    "decode KV 17% full (of 80% usable), 48 of 228 batch slots busy, 0% of requests wait for a slot", ctx.decodeUse({kvFill: {value: 0.168}, bandTop: 0.8}));
  check("prefill use reads in words", ctx.prefillUse({pooled: {value: 0.6312}, worstPod: {value: 0.71}}) === "prefill 63% busy pooled, 71% on the busiest pod");
  check("a verdict word is set apart from its sentence", v.verdicts[1].verdict === "OVER-PROVISIONED" && v.verdicts[1].rest.startsWith("if all 47.4 r/s were 1k/1k requests, one prefill"));
  check("a refusal code in a reason reads in words", v.details[0].lines.includes("The numbers: TTFT p95 router-queued misses 2000.0 ms"),
    JSON.stringify(v.details[0].lines));
  check("a refusal with no words names its reason", refusalText({refused: {reason: "Odd"}}) === "not priced (Odd)");
  check("no flags is no phrases", flagSummary(undefined).phrases.length === 0);
  check("a no-target cell has a word", reportView({loads: ["50%"], shapes: [{label: "m", capacity: knees(1, "decode"), verdict: {},
    utilization: {decode: {}, prefill: {}}, table: [row("50%", 1, {value: 0.1}, {value: 0.01}, "no target")]}]}).rows[0].cells[0].word === "no SLO");
  check("a server with no report route", reportMissing(404, "404: POST /v1/report: no such endpoint; the server serves ..."));
  check("a dropped inputsId is not a missing route", !reportMissing(404, "404: unknown inputsId: resolve the source"));
  check("a 400 is not a missing route", !reportMissing(400, "no such endpoint"));
  check("an answer for the inputs on the page is drawn", reportCurrent("sha256:a", "sha256:a"));
  check("an answer for replaced inputs is dropped", !reportCurrent("sha256:a", "sha256:b"));
}

// rateNote: where a plan with no rate started, in words, for each of the
// API's rateSource values; with one knee refused it is not called the
// operating point, and asks for a rate.
// recordingLabel: a file in --recordings is shown by its name; an upload by
// the file name it was sent under, its size and time, then its hash's start.
{
  const {recordingLabel} = ctx;
  const h = "upload:8c5f4000f4e3ba32a4f009eee168c4183045004ee9e1b9cad279681b93b11367.jsonl.gz";
  const ups = [{name: h, fileName: "Qwen3-32B run.jsonl.gz", bytes: 3774873, uploadedAt: "2026-11-17T15:25:09Z"}];
  let l = recordingLabel("ols-sweep.jsonl.gz", ups);
  check("a recording file is shown by its name", l === "ols-sweep.jsonl.gz", l);
  l = recordingLabel(h, ups);
  check("an upload is shown by its file name, size, time and hash", l === "\u2068Qwen3-32B run.jsonl.gz\u2069 (3.6 MiB, 2026-11-17 15:25Z) · upload 8c5f4000f4e3", l);
  l = recordingLabel(h, [{name: h, bytes: 116332, uploadedAt: "2026-11-20T12:05:00Z"}]);
  check("an upload sent with no file name is unnamed, in KiB", l === "\u2068unnamed\u2069 (114 KiB, 2026-11-20 12:05Z) · upload 8c5f4000f4e3", l);
  l = recordingLabel(h, [{name: h, fileName: "a\u0000b", bytes: 10}]);
  check("control characters in a file name are blanked, a missing time left out", l === "\u2068a b\u2069 (1 KiB) · upload 8c5f4000f4e3", l);
  l = recordingLabel(h, [{name: h, fileName: "x".repeat(300), bytes: 1, uploadedAt: "now"}]);
  check("a long file name is cut to a typed name's length", l.startsWith("\u2068" + "x".repeat(MAX_NAME) + "\u2069 (1 KiB)"), l);
  l = recordingLabel(h, [{name: h, fileName: "f", bytes: 1048500}]);
  check("just under 1 MiB reads as MiB, not 1024 KiB", l.includes("(1.0 MiB)"), l);
  l = recordingLabel(h, null);
  check("an upload the server does not list is shown by its name", l === h, l);
  check("the label is text: esc escapes it", esc(recordingLabel(h, [{name: h, fileName: "<b>", bytes: 1}])).startsWith("\u2068&lt;b&gt;"));
}
{
  const {rateNote} = ctx;
  check("a typed rate has no note", rateNote("request") === "");
  check("the operating point is named", rateNote("operating point").includes("the operating point"));
  for (const [source, knee, unpriced] of [["prefill knee; decode not priced", "prefill", "decode"],
    ["decode knee; prefill not priced", "decode", "prefill"]]) {
    const n = rateNote(source);
    check(`${source}: names the knee`, n.includes(`headroom times the ${knee} knee`), n);
    check(`${source}: names the knee not priced`, n.includes(`the ${unpriced} knee is not priced`), n);
    check(`${source}: no operating point`, n.includes("there is no operating point"), n);
    check(`${source}: asks for a rate`, n.includes("Type your request rate."), n);
  }
}
} catch (e) {
  failures.push("a case threw, and the cases after it did not run: " + e.message);
}

if (failures.length) {
  console.error(`check-planner-scenarios: ${failures.length} failed, ${passed} passed (${file})`);
  for (const f of failures) console.error("  FAIL " + f);
  process.exit(1);
}
// EXPECTED_CASES is how many cases this file runs: a case that silently
// stops running (an early return, a renamed function) fails the check.
const EXPECTED_CASES = 325;
if (passed !== EXPECTED_CASES) {
  console.error(`check-planner-scenarios: ${passed} cases ran; expected ${EXPECTED_CASES}`);
  process.exit(1);
}
console.log(`check-planner-scenarios: ${passed} cases passed (${file})`);
