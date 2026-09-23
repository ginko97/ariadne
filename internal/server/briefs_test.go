package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func writeAt(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func briefPaths(r briefsResponse) []string {
	var out []string
	for _, b := range r.Briefs {
		out = append(out, b.Path)
	}
	return out
}

// Brief… lists the .md files in the folder, subfolders included, newest
// first, and nothing else: not other files, not dot-folders.
func TestListBriefsFindsMarkdownNewestFirst(t *testing.T) {
	ws := t.TempDir()
	now := time.Now()
	writeAt(t, filepath.Join(ws, "old.md"), "old", now.Add(-3*time.Hour))
	writeAt(t, filepath.Join(ws, "reports", "new.MD"), "new", now.Add(-1*time.Hour))
	writeAt(t, filepath.Join(ws, "middle.md"), "mid", now.Add(-2*time.Hour))
	writeAt(t, filepath.Join(ws, "notes.txt"), "not a brief", now)
	writeAt(t, filepath.Join(ws, ".git", "HEAD.md"), "hidden", now)

	got, err := listBriefs(ws)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"reports/new.MD", "middle.md", "old.md"}
	if !reflect.DeepEqual(briefPaths(got), want) {
		t.Fatalf("briefs = %q, want %q", briefPaths(got), want)
	}
	if got.Partial {
		t.Error("partial set on a folder that was read in full")
	}
	// Every listed path must open through the same function the Show button
	// uses, or the list offers briefs that cannot be shown.
	for _, p := range want {
		if _, _, err := readWorkspaceBrief(ws, p); err != nil {
			t.Errorf("listed %q, but it does not open: %v", p, err)
		}
	}
}

// A folder link out of the folder is not walked into. fs.WalkDir does not
// follow links, so this passes on its own; it is here so a walker that did
// would fail it.
func TestListBriefsDoesNotFollowALinkOutOfTheFolder(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	writeAt(t, filepath.Join(outside, "secret.md"), "outside", time.Now())
	writeAt(t, filepath.Join(ws, "mine.md"), "inside", time.Now())
	linkDirOut(t, filepath.Join(ws, "bridge"), outside)

	got, err := listBriefs(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(briefPaths(got), []string{"mine.md"}) {
		t.Fatalf("briefs = %q, want only mine.md", briefPaths(got))
	}
}

func TestListBriefsCapsTheList(t *testing.T) {
	ws := t.TempDir()
	base := time.Now()
	for i := range maxBriefsListed + 5 {
		writeAt(t, filepath.Join(ws, fmt.Sprintf("b%03d.md", i)), "x", base.Add(time.Duration(i)*time.Minute))
	}
	got, err := listBriefs(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Briefs) != maxBriefsListed || !got.Partial {
		t.Fatalf("listed %d, partial=%v; want %d and partial", len(got.Briefs), got.Partial, maxBriefsListed)
	}
	if got.Briefs[0].Path != fmt.Sprintf("b%03d.md", maxBriefsListed+4) {
		t.Errorf("first listed %q, want the newest", got.Briefs[0].Path)
	}
}

// The endpoint: the token is required, the server's default folder is used
// when the page sends none, and a folder that does not exist is a 400.
func TestHandleBriefsEndpoint(t *testing.T) {
	s, ts := newTestServer(t)
	ws := t.TempDir()
	writeAt(t, filepath.Join(ws, "brief.md"), "# brief", time.Now())
	s.DefaultWorkspace = ws

	call := func(body string, token bool) (*http.Response, briefsResponse) {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL+"/api/briefs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token {
			req.Header.Set("X-Ariadne-CSRF", s.CSRFToken)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out briefsResponse
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}

	if resp, _ := call(`{}`, false); resp.StatusCode != http.StatusForbidden {
		t.Errorf("without the token: status %d, want 403", resp.StatusCode)
	}
	resp, got := call(`{}`, true)
	if resp.StatusCode != http.StatusOK || got.Folder != ws || !reflect.DeepEqual(briefPaths(got), []string{"brief.md"}) {
		t.Errorf("default folder: status %d, %+v", resp.StatusCode, got)
	}
	missing, _ := json.Marshal(briefsRequest{Workspace: filepath.Join(ws, "nope")})
	if resp, _ := call(string(missing), true); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing folder: status %d, want 400", resp.StatusCode)
	}
}

// A link to a .md file is not listed, wherever it points: the read would
// refuse one that leaves the folder, and the list must not offer it. Tested
// on an in-memory file system because Windows lets no unprivileged test
// create a file symlink.
func TestWalkBriefsListsOnlyRegularFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"mine.md":        {Data: []byte("inside")},
		"secret-link.md": {Data: []byte("../outside/secret.md"), Mode: fs.ModeSymlink},
	}
	got, err := walkBriefs(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(briefPaths(got), []string{"mine.md"}) {
		t.Fatalf("briefs = %q, want only mine.md", briefPaths(got))
	}
}
