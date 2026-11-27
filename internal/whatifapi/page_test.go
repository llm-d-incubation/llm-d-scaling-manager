package whatifapi

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// get sends a GET of path to h, with the authorization header when given.
func get(h http.Handler, path, auth string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	h.ServeHTTP(rec, req)
	return rec
}

// pageText is an embedded file of the page, its line ends as LF: a Windows
// checkout embeds CRLF.
func pageText(t *testing.T, name string) string {
	t.Helper()
	b, err := pageFS.ReadFile("page/" + name)
	require.NoError(t, err)
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// The page is served from the binary at / with its files under /page/, each
// with its own content type and the page's security headers; any other file
// is a 404.
func TestPageServed(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}}).Handler()
	for path, want := range map[string]struct{ ct, contains string }{
		"/":                 {"text/html; charset=utf-8", `<script src="page/planner.js" defer></script>`},
		"/page/planner.js":  {"text/javascript; charset=utf-8", "FLAG_WORDS"},
		"/page/scenario.js": {"text/javascript; charset=utf-8", "function linkProblem"},
		"/page/dayplan.js":  {"text/javascript; charset=utf-8", "function dayPlanSVG"},
		"/page/backtest.js": {"text/javascript; charset=utf-8", "function btSVG"},
		"/page/planner.css": {"text/css; charset=utf-8", "prefers-color-scheme: dark"},
	} {
		rec := get(h, path, "")
		require.Equal(t, http.StatusOK, rec.Code, path)
		assert.Equal(t, want.ct, rec.Header().Get("Content-Type"), path)
		assert.Equal(t, pageCSP, rec.Header().Get("Content-Security-Policy"), path)
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), path)
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"), path)
		assert.Contains(t, rec.Body.String(), want.contains, path)
	}
	assert.Contains(t, pageText(t, "index.html"), `href="page/planner.css"`)

	// The policy's directives, read one by one: nothing but the page's own
	// origin, and no inline script or style.
	rec := get(h, "/", "")
	directives := map[string]string{}
	for _, d := range strings.Split(rec.Header().Get("Content-Security-Policy"), ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = value
	}
	for name, want := range map[string]string{"default-src": "'none'", "script-src": "'self'", "style-src": "'self'",
		"connect-src": "'self'", "frame-ancestors": "'none'", "base-uri": "'none'", "form-action": "'none'"} {
		assert.Equal(t, want, directives[name], "Content-Security-Policy %s", name)
	}
	assert.NotContains(t, rec.Header().Get("Content-Security-Policy"), "unsafe-")
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	for _, path := range []string{"/page/", "/page/index.html", "/page/page.go", "/index.html"} {
		assert.Equal(t, http.StatusNotFound, get(h, path, "").Code, path)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")))
	assert.Equal(t, http.StatusNotFound, rec.Code, "the page is GET only")
}

// The page sits behind the API's own authentication: with Kubernetes auth no
// token is 401, a user not allowed get on /whatif/ is 403, and an allowed one
// gets the page; with none, a foreign Host is 403.
func TestPageBehindAuth(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}}).Handler()
	allowed := NewAuthenticator(newCluster("/whatif/", ""), nil, 0, nil).Wrap(h)
	assert.Equal(t, http.StatusUnauthorized, get(allowed, "/", "").Code)
	assert.Equal(t, http.StatusUnauthorized, get(allowed, "/page/planner.js", "Bearer bad").Code)
	rec := get(allowed, "/", "Bearer good")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Capacity planner")

	planOnly := NewAuthenticator(newCluster("/whatif/v1/plan", ""), nil, 0, nil).Wrap(h)
	assert.Equal(t, http.StatusForbidden, get(planOnly, "/", "Bearer good").Code)
	assert.Equal(t, http.StatusForbidden, get(planOnly, "/page/planner.js", "Bearer good").Code)

	loop := LoopbackOnly(h)
	for host, want := range map[string]int{"127.0.0.1:8080": http.StatusOK, "localhost:8080": http.StatusOK, "rebound.example:8080": http.StatusForbidden} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		loop.ServeHTTP(rec, req)
		assert.Equal(t, want, rec.Code, host)
	}
}

// The page loads nothing from another origin and runs no inline script or
// style, so its Content-Security-Policy holds and it works air-gapped.
func TestPageLoadsNothingExternal(t *testing.T) {
	entries, err := pageFS.ReadDir("page")
	require.NoError(t, err)
	require.Len(t, entries, len(pageFiles), "every embedded file is served, and no more")
	for _, e := range entries {
		// A reference to an element of the page itself (the graphs' clip
		// paths) is no load.
		text := strings.ReplaceAll(pageText(t, e.Name()), "url(#", "")
		for _, bad := range []string{"http:", "https:", `src="//`, `href="//`, "url(", "@import", " style=\"", "javascript:", "eval("} {
			assert.NotContains(t, text, bad, "%s", e.Name())
		}
	}
	html := pageText(t, "index.html")
	// Four scripts by src, scenario.js first, then dayplan.js, which calls
	// its esc, then backtest.js, which calls both, then planner.js, which
	// calls all three.
	assert.Equal(t, 4, strings.Count(html, "<script"), "four scripts, by src")
	assert.Less(t, strings.Index(html, `<script src="page/scenario.js" defer></script>`), strings.Index(html, `<script src="page/dayplan.js" defer></script>`))
	assert.Less(t, strings.Index(html, `<script src="page/dayplan.js" defer></script>`), strings.Index(html, `<script src="page/backtest.js" defer></script>`))
	assert.Less(t, strings.Index(html, `<script src="page/backtest.js" defer></script>`), strings.Index(html, `<script src="page/planner.js" defer></script>`))
	assert.GreaterOrEqual(t, strings.Index(html, `<script src="page/scenario.js" defer></script>`), 0)
	// The page reaches its files and the API by paths relative to itself, so
	// that a proxy may serve it under a prefix.
	for _, name := range []string{"planner.js", "scenario.js", "dayplan.js", "backtest.js"} {
		js := pageText(t, name)
		for _, abs := range []string{`"/v1/`, "`/v1/", `'/v1/`, `"/page/`} {
			assert.NotContains(t, js, abs, "an absolute path in %s", name)
		}
	}
	assert.NotContains(t, html, `="/page/`, "an absolute path")
	assert.NotContains(t, html, "<style")
}

// Every element the script reads by id is on the page.
func TestPageIDsExist(t *testing.T) {
	html, js := pageText(t, "index.html"), pageText(t, "planner.js")
	ids := regexp.MustCompile(`el\("([^"]+)"\)`).FindAllStringSubmatch(js, -1)
	require.Greater(t, len(ids), 50, "found too few ids")
	for _, m := range ids {
		assert.Contains(t, html, `id="`+m[1]+`"`, "the script reads #%s", m[1])
	}
	// The ids the script reads through a variable: the steppers and the
	// targets' sliders and boxes.
	for _, id := range []string{"prefill", "decode", "ttft", "ttftbox", "tpot", "tpotbox"} {
		assert.Contains(t, html, `id="`+id+`"`)
	}
}

// The page asks both questions: a toggle between them, a size posted to
// v1/size by a relative path with no fleet in it (the fleet is the answer),
// the steppers read only in size mode, and the mode in the link.
func TestPageAsksSize(t *testing.T) {
	html, js := pageText(t, "index.html"), pageText(t, "planner.js")
	for _, want := range []string{`id="mplan"`, `id="msize"`, "How much does this fleet serve?", "What fleet does this traffic need?",
		`id="sizebox"`, `id="maxrep"`, `id="sizelow"`, `id="sizeanswer" aria-live="polite"`} {
		assert.Contains(t, html, want)
	}
	for _, want := range []string{`api("POST", "v1/size", sizeRequest(), signal)`, "state.abort = new AbortController();", "state.abort.abort();",
		`'raise "Search up to" (at most 64)'`, "More replicas do not lower a floor", `under "Assume a term the source did not learn"`,
		"delete req.fleet;", `p.set("mode", "size")`,
		`req.sizeOn = "low";`, "modeSeq !== state.modeSeq", `el("perbucket").checked = k.perBucket;`,
		`el("prefill").disabled = el("pdec").disabled = el("pinc").disabled = !state.inputs.fleet.prefill || sizing;`} {
		assert.Contains(t, js, want)
	}
}

// The page states a load-aware router under the assumptions: a checkbox in
// the assume box, wired, sent as assume.loadAwareRouter only when ticked,
// carried by the link, and cleared when new inputs load.
// hack/check-planner-exec.mjs runs it.
func TestPageStatesALoadAwareRouter(t *testing.T) {
	html, js := pageText(t, "index.html"), pageText(t, "planner.js")
	start := strings.Index(html, `id="assumebox"`)
	require.GreaterOrEqual(t, start, 0, "no assume box")
	end := strings.Index(html[start:], "</details>")
	require.Positive(t, end, "the assume box does not end")
	box := html[start : start+end]
	assert.Contains(t, box, `<input type="checkbox" id="a-lar"> My router balances prefill by load`)
	for _, want := range []string{`el("a-lar").onchange = () => {`, "k().assume.loadAwareRouter = true;", "delete k().assume.loadAwareRouter;",
		`if (k.assume.loadAwareRouter) p.set("loadAwareRouter", "1");`, `if (g("loadAwareRouter") === "1") k.assume.loadAwareRouter = true;`,
		`el("a-lar").checked = false;`, `el("a-lar").checked = !!k.assume.loadAwareRouter;`} {
		assert.Contains(t, js, want)
	}
	assert.Contains(t, pageText(t, "scenario.js"), "loadAwareRouter: /^1$/")
}

// The page asks again only where retryDelay (scenario.js, checked under node
// by hack/check-planner-scenarios.sh) says to: a busy server's 503, never a
// plan stopped at the server's plan budget.
func TestPageRetriesOnlyABusyServer(t *testing.T) {
	js := pageText(t, "planner.js")
	assert.Contains(t, js, `const wait = retryDelay(resp.status, resp.headers.get("Retry-After"), attempt);`)
	assert.NotContains(t, js, "resp.status === 503", "planner.js decides a retry on its own")
	assert.Contains(t, pageText(t, "scenario.js"), "function retryDelay(status, retryAfter, attempt)")
}

// The page saves, exports, imports and compares scenarios: every control is
// on the page and wired to its handler, the store and the file carry the
// link, every key the link writes is one an imported link may carry, a
// name read back reaches the page only through esc, and a pinned plan is a
// copy, not priced again.
func TestPageSavesAndCompares(t *testing.T) {
	html, js := pageText(t, "index.html"), pageText(t, "planner.js")
	for id, wire := range map[string]string{
		"scensave":   `el("scensave").onclick = saveScenario;`,
		"scenopen":   `el("scenopen").onclick =`,
		"scendelete": `el("scendelete").onclick = deleteScenario;`,
		"scenexport": `el("scenexport").onclick = exportScenario;`,
		"scenimport": `el("scenimport").onchange = () => importScenario(el("scenimport").files[0]);`,
		"pina":       `el("pina").onclick = pinA;`,
		"unpin":      `el("unpin").onclick = unpin;`,
	} {
		assert.Contains(t, html, `id="`+id+`"`)
		assert.Contains(t, js, wire, "#%s is not wired", id)
	}
	for _, id := range []string{"scenname", "scenlist", "scencount", "pinnote", "comparebox", "comparewhat", "comparetable"} {
		assert.Contains(t, html, `id="`+id+`"`)
	}
	assert.Contains(t, html, `<input type="file" id="scenimport"`)

	// One format: the store and the file hold linkParams' text, and writeLink
	// puts the same text in the address. The checks, the store's edits and
	// the names are scenario.js's, run on fixtures by
	// hack/check-planner-scenarios.sh; here, that the page calls them.
	sjs := pageText(t, "scenario.js")
	for _, want := range []string{`"#" + linkParams().toString()`, `link: linkParams().toString()`,
		`localStorage.setItem(SCENARIO_STORE`, "readScenarioFile(await file.text())", "file.size > MAX_FILE",
		"putScenario(readStore(),", "dropScenario(readStore(), sc.name)", "importPlan(readStore(),",
		"visibleScenarios(readStore())", "new Option(", "state.link = linkAtLoadStart(state.link, seq);",
		"linkAtLoadEnd(state.link, seq, state.seq, true)", "linkAtLoadEnd(state.link, seq, state.seq, false)", "beginOpen(pageBefore(), sc)",
		"sourceProblem(sc.link, state.sources)", "compareRows(pin.plan, cur,", "sameCurve(p, pts(current))", "state.priceFailed = true;"} {
		assert.Contains(t, js, want)
	}
	assert.Contains(t, sjs, `const SCENARIO_KIND = "capacity-planner-scenario";`)
	assert.NotContains(t, js, `el("scenlist").innerHTML`, "the saved names are listed as nodes, not markup")

	// Every key linkParams writes is one linkProblem admits, so an exported
	// scenario imports.
	start, end := strings.Index(js, "function linkParams()"), strings.Index(js, "// ---- The plan ----")
	require.True(t, start >= 0 && end > start, "linkParams not found")
	keys := regexp.MustCompile(`p\.set\("([A-Za-z]+)"`).FindAllStringSubmatch(js[start:end], -1)
	require.Greater(t, len(keys), 15, "found too few link keys")
	require.Contains(t, js[start:end], "p.set(a.key, k.assume[a.key])")
	astart := strings.Index(js, "const ASSUMABLE = [")
	require.GreaterOrEqual(t, astart, 0, "ASSUMABLE not found")
	assumed := regexp.MustCompile(`key: "([A-Za-z]+)"`).FindAllStringSubmatch(js[astart:astart+strings.Index(js[astart:], "];")], -1)
	require.Len(t, assumed, 5, "the assumed terms' keys")
	lstart, lend := strings.Index(sjs, "const LINK_NUMBERS"), strings.Index(sjs, "// CONTROL is")
	require.True(t, lstart >= 0 && lend > lstart, "the link's keys not found")
	admitted := sjs[lstart:lend]
	for _, k := range append(keys, assumed...) {
		assert.True(t, strings.Contains(admitted, `"`+k[1]+`"`) || strings.Contains(admitted, k[1]+": "), "an imported link may not carry %s", k[1])
	}

	// A name read back is untrusted: no line of either script that builds
	// markup puts one in raw. (The status line and the notes take names as
	// textContent, and the list builds Option nodes.)
	raw := regexp.MustCompile(`\$\{(x|sc|pin|state\.pin|same)\.(name|what|source|link|saved)`)
	markup := regexp.MustCompile("innerHTML|<[a-z/]")
	scanned := 0
	for name, text := range map[string]string{"planner.js": js, "scenario.js": sjs} {
		for i, line := range strings.Split(text, "\n") {
			if markup.MatchString(line) {
				scanned++
				assert.NotRegexp(t, raw, line, "%s line %d puts a name into markup unescaped", name, i+1)
			}
		}
	}
	require.Greater(t, scanned, 100, "found too few markup lines")

	// A pinned plan is a copy of the plan on screen; the graphs draw it dashed.
	for _, want := range []string{"plan: JSON.parse(JSON.stringify(p))", `cls: "c-pin", pin: true`, `${sr.pin ? " pin" : ""}`} {
		assert.Contains(t, js, want)
	}
	css := pageText(t, "planner.css")
	assert.Contains(t, css, "svg .line.pin { stroke-dasharray:")
}

// isFlagType reports whether e is the type whatif.Flag, as its own package or
// another names it.
func isFlagType(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "Flag"
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		return ok && pkg.Name == "whatif" && x.Sel.Name == "Flag"
	}
	return false
}

// The page names every flag a plan can carry in plain words: the engine's
// (internal/signals/whatif), the SLO analyzer's and the CLI's, whose code
// prices the API's figures. A flag added there fails here until the page says
// what it means.
func TestPageNamesEveryFlag(t *testing.T) {
	files := make([]string, 0, 64)
	for _, dir := range [][]string{{"..", "signals", "whatif"}, {"..", "engines", "analyzers", "slo"}, {"..", "..", "cmd", "capacity-planner"}} {
		got, err := filepath.Glob(filepath.Join(append(dir, "*.go")...))
		require.NoError(t, err)
		require.NotEmpty(t, got, "%v", dir)
		files = append(files, got...)
	}
	var flags []string
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			if !isFlagType(vs.Type) {
				return true
			}
			for _, v := range vs.Values {
				if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					s, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					flags = append(flags, s)
				}
			}
			return true
		})
	}
	require.Greater(t, len(flags), 40, "found too few flags")
	js := pageText(t, "planner.js")
	for _, flag := range flags {
		assert.Contains(t, js, "\n  "+flag+": [", "FLAG_WORDS has no words for %s", flag)
	}
}

// listingResolver is a stub resolver whose listing answers sources or err.
type listingResolver struct {
	stubResolver
	sources SourcesResponse
	err     error
}

func (r *listingResolver) Sources(context.Context) (SourcesResponse, error) { return r.sources, r.err }

// A listing that fails is answered by its class, as any error is: the
// caller's is 400, a forbidden one 403, any other 500, never a listing.
func TestSourcesListingFails(t *testing.T) {
	for err, code := range map[error]int{
		errors.New("the server could not list its recordings directory"): http.StatusInternalServerError,
		InputErrorf("bad"):                    http.StatusBadRequest,
		ForbiddenError{Err: errors.New("no")}: http.StatusForbidden,
	} {
		rec := get(New(Config{Resolver: &listingResolver{err: err}}).Handler(), "/v1/sources", "")
		assert.Equal(t, code, rec.Code, "%v", err)
		assert.Contains(t, rec.Body.String(), `"error"`)
		assert.NotContains(t, rec.Body.String(), `"recordings"`)
	}
	// A listing's nil lists are written empty, and the schema version is the
	// server's.
	rec := get(New(Config{Resolver: &listingResolver{sources: SourcesResponse{SchemaVersion: 99, Prometheus: true}}}).Handler(), "/v1/sources", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"schemaVersion": 1, "prometheus": true, "namespaces": [], "recordings": []}`, rec.Body.String())
}

// A resolver that cannot list its sources answers GET /v1/sources with none,
// as empty lists rather than nulls.
func TestSourcesWithoutLister(t *testing.T) {
	rec := get(New(Config{Resolver: &stubResolver{}}).Handler(), "/v1/sources", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"schemaVersion": 1, "prometheus": false, "namespaces": [], "recordings": []}`, rec.Body.String())
}

// The page plans a day: a button that posts v1/dayplan by a relative path
// with no fleet and no rate in it (each bucket's are its own), and draws the
// result as dayplan.js's three charts on one time axis, with a legend, a
// table view and a tooltip, and no second y axis on any chart.
func TestPagePlansADay(t *testing.T) {
	html, js, day := pageText(t, "index.html"), pageText(t, "planner.js"), pageText(t, "dayplan.js")
	for _, id := range []string{"dayrun", "daystep", "daysize", "daychart", "daylegend", "daytable", "daysum", "daynotes", "daystale", "dayused"} {
		assert.Contains(t, html, `id="`+id+`"`)
	}
	// The request asked is the one kept: a knob changed since marks the plan
	// stale (its cases run under node in hack/check-planner-targets.mjs).
	assert.Contains(t, js, "const req = dayRequest();")
	// It can be cancelled (its running view runs under node in
	// hack/check-planner-dayprogress.mjs).
	assert.Contains(t, js, `api("POST", "v1/dayplan", req, abort.signal)`)
	assert.Contains(t, html, `id="daycancel"`)
	assert.Contains(t, js, "state.dayAsked = JSON.stringify(req);")
	for _, want := range []string{"delete req.fleet", "delete req.traffic.classes", "dayPlanSVG(d)", "dayTipHTML(state.day, b)", "dayHoursTable(d)"} {
		assert.Contains(t, js, want)
	}
	for _, want := range []string{`key: "rate"`, `key: "prefill"`, `key: "decode"`, `data-x0="${lay.x0}"`, "data-under", `"rate-p95"`, `"${p.key}-needed"`, `"${p.key}-actual"`} {
		assert.Contains(t, day, want)
	}
	// One y scale per chart: the only text-anchor="end" ticks are drawn at
	// the left margin.
	assert.Equal(t, 1, strings.Count(day, `x="${DAY_L - 8}"`), "one y axis a chart")
}

// The page judges the fleet: the shape rows, the loads and the growth of the
// load seen are on the page, a report is posted to v1/report by a relative
// path with the body reportRequest builds from them (its cases run under node
// in hack/check-planner-scenarios.sh), a server with no report route is said
// in words, and the matrix carries a legend whose marks are words, not
// colour alone. The report's strings reach the page as text.
func TestPageJudgesTheFleet(t *testing.T) {
	html, js, scen := pageText(t, "index.html"), pageText(t, "planner.js"), pageText(t, "scenario.js")
	for _, want := range []string{`id="rshapes"`, `id="rshapeadd"`, `id="rloads"`, `id="rgrowth"`, `id="rrun"`, `id="rmatrix"`,
		`id="rstatus" class="hint" aria-live="polite"`, `id="rlegend"`, `<span class="rmark met">ok</span>`, `<span class="rmark missed">MISS</span>`,
		`<span class="rmark saturated">SAT</span>`, `<span class="rmark unknown">?</span>`, `<span class="rmark notarget">no SLO</span>`, `<select id="rloadstat"`, `<option value="p95-modulation" selected>`,
		`<summary>How accurate is this?</summary>`} {
		assert.Contains(t, html, want)
	}
	for _, want := range []string{`api("POST", "v1/report", asked.body)`,
		`reportRequest({inputsId: state.inputs.inputsId, fleet: plan.fleet, shapes: shapeRows(),`,
		`loads: el("rloads").value, growth: el("rgrowth").value, loadStatistic: el("rloadstat").value, headroom: plan.headroom, slo: plan.slo, assume: plan.assume})`,
		`el("rload").textContent = v.load;`, `for (const g of s.growth) {`, `if (s.needs) li.appendChild(textNode("span", s.needs, s.unmet ? "rneeds unmet" : "rneeds"));`,
		"function schedule(ms) {\n  reportStale();", `reportCurrent(token, reportToken())`, `el("rshapes").addEventListener("input", reportStale);`,
		`el("rloads").oninput = el("rgrowth").oninput = el("rloadstat").onchange = reportStale;`, `el("rverdicts").focus();`, `el("rrun").setAttribute("aria-disabled", "true");`, `(next ? next.querySelector("input") : el("rshapeadd")).focus();`,
		`reportMissing(status, e.message)`, "This server has no fleet report (POST /v1/report)", `" EXTRAPOLATED"`} {
		assert.Contains(t, js, want)
	}
	for _, want := range []string{"function reportRequest(o)", "function reportView(r)", "function reportMissing(status, error)"} {
		assert.Contains(t, scen, want)
	}
	// The whole report section, its helpers included, draws text nodes only.
	start := strings.Index(js, "// ---- The fleet report (POST /v1/report) ----")
	end := strings.Index(js, `document.addEventListener("DOMContentLoaded", start);`)
	require.True(t, start >= 0 && end > start, "the report section is found")
	assert.NotContains(t, js[start:end], "innerHTML", "the report is drawn as text nodes")
	assert.NotContains(t, js[start:end], "insertAdjacentHTML")
}

// The page's scripts share one global scope: a function declared in two is
// silently the later one's, so scenario.js, dayplan.js and planner.js declare
// each name once (a clash made the report call planner.js's rolesText).
func TestPageScriptsDeclareEachFunctionOnce(t *testing.T) {
	decl := regexp.MustCompile(`(?m)^(?:async )?function (\w+)\(`)
	seen := map[string]string{}
	for _, file := range []string{"scenario.js", "dayplan.js", "backtest.js", "planner.js"} {
		for _, m := range decl.FindAllStringSubmatch(pageText(t, file), -1) {
			if prev, ok := seen[m[1]]; ok {
				t.Errorf("function %s is declared in %s and in %s", m[1], prev, file)
			}
			seen[m[1]] = file
		}
	}
	require.Greater(t, len(seen), 100, "found too few functions")
}

// A new source, or a plan that fails, takes the last plan off the page: every
// element a plan fills is emptied (clearPlan), and the status says what to do.
// Before, setInputs dropped state.plan alone and a failed plan set the status
// alone, so a recording whose plan failed showed the previous recording's
// limits and charts as its own. The note under the rate comes from rateNote
// (scenario.js, run under node by hack/check-planner-scenarios.sh).
func TestPageClearsAStalePlan(t *testing.T) {
	html, js := pageText(t, "index.html"), pageText(t, "planner.js")
	m := regexp.MustCompile(`(?s)const PLAN_FIGURES = \[(.*?)\];`).FindStringSubmatch(js)
	require.NotNil(t, m, "PLAN_FIGURES is found")
	ids := regexp.MustCompile(`"([a-z0-9]+)"`).FindAllStringSubmatch(m[1], -1)
	require.Greater(t, len(ids), 15, "found too few ids")
	seen := map[string]bool{}
	for _, id := range ids {
		assert.Contains(t, html, `id="`+id[1]+`"`, "clearPlan empties #%s", id[1])
		seen[id[1]] = true
	}
	for _, id := range []string{"limits", "fleet", "chosen", "bars", "marginal", "ttftchart", "tpotchart", "ttftpoints", "tpotpoints", "curvetable", "ratenote"} {
		assert.True(t, seen[id], "clearPlan leaves #%s", id)
	}
	setInputs := between(t, js, "function setInputs(", "\n}\n")
	assert.Contains(t, setInputs, "clearPlan();", "a new source clears the last plan")
	failed := between(t, js, `if (e.name !== "AbortError") {`, "\n    }\n")
	assert.Contains(t, failed, "clearPlan();", "a failed plan clears the last plan")
	assert.Contains(t, failed, "Nothing is shown until a plan prices: type a rate")
	assert.Contains(t, js, `el("ratenote").textContent = rateNote(p.limits.chosen.rateSource);`)
	assert.Contains(t, pageText(t, "scenario.js"), "function rateNote(source)")
}

// A page load runs no check of the planner: POST v1/backtest is asked from
// runBacktest alone, which runs on a button, never from the load, the panel
// or opening Check the planner. Before, the panel ran a check of the
// source's last half after every load, and opening the tab ran another.
// The panel's words before a check are execView's (scenario.js, run under
// node by hack/check-planner-scenarios.sh).
func TestPageChecksThePlannerOnlyWhenAsked(t *testing.T) {
	html, js := pageText(t, "index.html"), pageText(t, "planner.js")
	assert.Equal(t, 1, strings.Count(js, `"v1/backtest"`), "v1/backtest is asked from one place")
	assert.Contains(t, between(t, js, "async function runBacktest(", "\n}\n"), `api("POST", "v1/backtest", req)`)
	for _, fn := range []string{"async function startExecutive(", "function showView(", "function setInputs(", "function renderExec(", "async function loadInputs("} {
		body := between(t, js, fn, "\n}\n")
		assert.NotContains(t, body, "runBacktest", "%s runs no check", fn)
		assert.NotContains(t, body, "v1/backtest", "%s asks no check", fn)
	}
	// Every call of runBacktest is a button's.
	calls := regexp.MustCompile(`[^\n]*runBacktest\(\)[^\n]*`).FindAllString(js, -1)
	require.NotEmpty(t, calls, "runBacktest is called")
	for _, c := range calls {
		assert.True(t, strings.Contains(c, "if (!el(\"btrun\").disabled) runBacktest();") || strings.Contains(c, "async function runBacktest()"),
			"runBacktest is called from checkAccuracy alone: %s", strings.TrimSpace(c))
	}
	assert.Contains(t, js, `el("btrun").onclick = runBacktest;`)
	// "Check accuracy on this recording" checks this recording, whichever
	// test Check the planner had picked.
	assert.Contains(t, js, `el("btstart").onclick = checkAccuracy;`)
	assert.Contains(t, between(t, js, "function checkAccuracy(", "\n}\n"), `el("btsplitmode").checked = true;`)
	assert.Contains(t, between(t, js, "function renderExec(", "\n}\n"), `a.act === "check" ? checkAccuracy`)
	assert.Contains(t, html, `<button type="button" id="btstart" disabled>Check accuracy on this recording</button>`)
}

// between is s from the first start up to the end that follows it; both
// must be found.
func between(t *testing.T, s, start, end string) string {
	t.Helper()
	_, rest, ok := strings.Cut(s, start)
	require.True(t, ok, "%q is found", start)
	body, _, ok := strings.Cut(rest, end)
	require.True(t, ok, "%q is found after %q", end, start)
	return start + body
}
