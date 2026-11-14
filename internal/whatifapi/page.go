package whatifapi

import (
	"embed"
	"fmt"
	"net/http"
)

// pageFS is the capacity planner's page (docs/proposals/capacity-planner.md,
// "The screen"): plain HTML, CSS and JavaScript with no library, embedded so
// that the binary serves it with no network and no build step.
//
//go:embed page
var pageFS embed.FS

// pageFile is one file of the page: what it is read from and its type.
type pageFile struct {
	file, contentType string
}

// pageFiles are the page's files by the path they are served at. The content
// type is set here, not guessed from the extension: a Windows registry can
// map .js to text/plain.
var pageFiles = map[string]pageFile{
	"/":                 {"page/index.html", "text/html; charset=utf-8"},
	"/page/planner.js":  {"page/planner.js", "text/javascript; charset=utf-8"},
	"/page/planner.css": {"page/planner.css", "text/css; charset=utf-8"},
}

// pageCSP is the page's Content-Security-Policy: its own script and style
// only, no inline script, and requests to its own origin alone, so the page
// can reach nothing but the API that served it.
const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// servePage serves one of the page's files. It sits behind the same
// authentication as the API (the caller wraps the whole handler), so a page
// is served only to whom the API answers.
func servePage(w http.ResponseWriter, r *http.Request) {
	pf, ok := pageFiles[r.URL.Path]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("%s %s: no such file of the page", r.Method, r.URL.Path))
		return
	}
	body, err := pageFS.ReadFile(pf.file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", pf.contentType)
	h.Set("Content-Security-Policy", pageCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	// A new binary may serve a new page: revalidate rather than reuse.
	h.Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}
