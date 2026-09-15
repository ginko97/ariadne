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
