package whatifapi

import (
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

// pageText is an embedded file of the page.
func pageText(t *testing.T, name string) string {
	t.Helper()
	b, err := pageFS.ReadFile("page/" + name)
	require.NoError(t, err)
	return string(b)
}

// The page is served from the binary at / with its files under /page/, each
// with its own content type and the page's security headers; any other file
// is a 404.
func TestPageServed(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}}).Handler()
	for path, want := range map[string]struct{ ct, contains string }{
		"/":                 {"text/html; charset=utf-8", `<script src="page/planner.js" defer></script>`},
		"/page/planner.js":  {"text/javascript; charset=utf-8", "FLAG_WORDS"},
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
	for _, path := range []string{"/page/", "/page/index.html", "/page/page.go", "/index.html"} {
		assert.Equal(t, http.StatusNotFound, get(h, path, "").Code, path)
	}
	rec := httptest.NewRecorder()
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
	assert.Equal(t, 1, strings.Count(html, "<script"), "one script, by src")
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

// A resolver that cannot list its sources answers GET /v1/sources with none,
// as empty lists rather than nulls.
func TestSourcesWithoutLister(t *testing.T) {
	rec := get(New(Config{Resolver: &stubResolver{}}).Handler(), "/v1/sources", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"schemaVersion": 1, "prometheus": false, "namespaces": [], "recordings": []}`, rec.Body.String())
}
