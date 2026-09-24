package server

import (
	"net/http"
	"strings"
	"testing"
)

// The token reaches the page by being in it. An endpoint that handed it out
// would undo the guard — same-origin GETs pass, so a token anyone can ask for
// gates nothing — and embedding works because a cross-origin page can make the
// request but cannot read the response.
func TestIndexEmbedsThisProcessesToken(t *testing.T) {
	s, ts := newTestServer(t)

	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	body := bodyOf(t, resp)
	if !strings.Contains(body, `content="`+s.CSRFToken+`"`) {
		t.Error("the page does not carry this process's CSRF token")
	}
	if strings.Contains(body, "{{") {
		t.Error("an unexecuted template action reached the browser")
	}
	// A page cached across restarts would hold a token the server no longer
	// honours, and every send would fail with a 403 nobody could explain.
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// "/" matches every unmatched path, so without the check a typo would be served
// the page with a 200 — a broken link that looks like it worked.
func TestIndexDoesNotSwallowUnknownPaths(t *testing.T) {
	_, ts := newTestServer(t)

	resp, err := ts.Client().Get(ts.URL + "/not-a-page")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d for an unknown path, want 404", resp.StatusCode)
	}
}

// The tab has an icon, and it is in the page rather than a file.
//
// Two things this pins. The page is one embedded file with no build step, so
// the icon is an inline SVG data URI — a second asset would mean a second
// route, a second cache rule and a second thing to forget. And it is declared,
// which is also what stops the browser asking for /favicon.ico on every load
// and being told 404 by the catch-all handler.
func TestIndexDeclaresAnInlineIcon(t *testing.T) {
	_, ts := newTestServer(t)

	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := bodyOf(t, resp)

	if !strings.Contains(body, `rel="icon"`) {
		t.Error("the page declares no icon, so the tab shows a blank document")
	}
	if !strings.Contains(body, "data:image/svg+xml,") {
		t.Error("the icon is not inline; the page is meant to be one file")
	}
	// It has to adapt, because the page itself does: a dark stroke on a dark
	// tab strip is the same as no icon.
	if !strings.Contains(body, "prefers-color-scheme:dark") {
		t.Error("the icon does not follow the colour scheme")
	}
}

// Reopening a conversation started from a brief must render the brief as a
// task brief card rather than falling back to an ordinary prompt bubble.
func TestIndexRendersBriefCardOnReopen(t *testing.T) {
	if !strings.Contains(indexHTML, "render(t.messages, t.brief)") {
		t.Error("open(id) does not pass t.brief to render")
	}
	if !strings.Contains(indexHTML, "function render(entries, briefPath)") {
		t.Error("render does not accept briefPath")
	}
	if !strings.Contains(indexHTML, "renderBrief({ path: briefPath, content: e.text })") {
		t.Error("render does not call renderBrief for the initial prompt of a brief conversation")
	}
}
