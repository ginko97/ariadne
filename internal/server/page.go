package server

import (
	_ "embed"
	"html/template"
	"net/http"
)

// The page ships inside the binary. No Node, no build step, no directory that
// has to exist next to the executable — the same reason everything else here is
// one static binary.
//
//go:embed index.html
var indexHTML string

var indexTmpl = template.Must(template.New("index").Parse(indexHTML))

// handleIndex serves the page with this process's CSRF token embedded.
//
// Injected at render time rather than fetched by the page, because an endpoint
// that hands out the token would undo it: the guard lets same-origin GETs
// through, and a token any caller can ask for gates nothing. Embedded in the
// document, a cross-origin page cannot read it — the browser will happily send
// that request and then refuse to let the caller see the response, which is the
// whole mechanism.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// "/" matches everything unmatched, so an unknown path would otherwise be
	// served the page with a 200 — a broken link that looks like it worked.
	if r.URL.Path != "/" {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Nothing here is cacheable across restarts: the token changes with the
	// process, and a stale page would hold a token the server no longer honours
	// and fail every send with a 403 nobody could explain.
	w.Header().Set("Cache-Control", "no-store")
	if err := indexTmpl.Execute(w, struct{ CSRF string }{s.CSRFToken}); err != nil {
		return // headers are out; the page is half-written and nothing can fix it
	}
}
