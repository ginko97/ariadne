package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An edit is saved only over the text it started from: saved once, the old
// digest no longer matches, and a change made on disk meanwhile is refused
// rather than overwritten.
func TestSaveBriefChangesOnlyTheTextItWasOpenedFrom(t *testing.T) {
	ws := t.TempDir()
	writeAt(t, filepath.Join(ws, "report.md"), "# Report\n\nOld steps.\n", time.Now())
	_, content, err := readWorkspaceBrief(ws, "report.md")
	if err != nil {
		t.Fatal(err)
	}
	opened := briefDigest(content)

	rel, sha, err := saveWorkspaceBrief(ws, "report.md", "# Report\n\nNew steps.\n", opened, false)
	if err != nil || rel != "report.md" || sha != briefDigest("# Report\n\nNew steps.\n") {
		t.Fatalf("save: %q %q %v", rel, sha, err)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "report.md")); string(data) != "# Report\n\nNew steps.\n" {
		t.Errorf("file = %q", data)
	}
	if _, _, err := saveWorkspaceBrief(ws, "report.md", "# Report\n\nThird.\n", opened, false); !errors.Is(err, errBriefChanged) {
		t.Errorf("saving over a newer version with the old digest: %v, want errBriefChanged", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "report.md"), []byte("# Edited in Notepad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := saveWorkspaceBrief(ws, "report.md", "# Mine\n", sha, false); !errors.Is(err, errBriefChanged) {
		t.Errorf("a file changed on disk: %v, want errBriefChanged", err)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "report.md")); string(data) != "# Edited in Notepad\n" {
		t.Errorf("the change made on disk was overwritten: %q", data)
	}
	if entries, _ := os.ReadDir(ws); len(entries) != 1 {
		t.Errorf("a temporary file was left behind: %v", entries)
	}
}

// A new task file is created only where a task file could be listed, never
// over an existing one, and only as a non-empty .md.
func TestSaveBriefCreatesOnlyNewTaskFiles(t *testing.T) {
	ws := t.TempDir()
	if _, _, err := saveWorkspaceBrief(ws, "weekly.md", "# Weekly\n", "", true); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "weekly.md")); string(data) != "# Weekly\n" {
		t.Errorf("created %q", data)
	}
	for name, c := range map[string]struct{ path, content string }{
		"an existing name": {"weekly.md", "# Other\n"},
		"not markdown":     {"weekly.txt", "# x\n"},
		"outside":          {"../escape.md", "# x\n"},
		"no such folder":   {"missing/new.md", "# x\n"},
		"blank":            {"blank.md", "  \n"},
	} {
		if _, _, err := saveWorkspaceBrief(ws, c.path, c.content, "", true); err == nil {
			t.Errorf("%s: created", name)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "weekly.md")); string(data) != "# Weekly\n" {
		t.Errorf("an existing task file was replaced: %q", data)
	}
}

func postTask(t *testing.T, s *Server, client *http.Client, url string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(b)))
	req.Header.Set("X-Ariadne-CSRF", s.CSRFToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The endpoints: saving needs the token and answers 409 for a stale edit; a
// draft comes back as text and nothing is written.
func TestTaskEndpoints(t *testing.T) {
	s, ts := newTestServer(t)
	ws := t.TempDir()
	s.DefaultWorkspace = ws
	writeAt(t, filepath.Join(ws, "a.md"), "# A\n", time.Now())

	req, _ := http.NewRequest("POST", ts.URL+"/api/brief/save", strings.NewReader(`{"path":"a.md","content":"# B\n","sha256":"x"}`))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("save without the token: %d, want 403", resp.StatusCode)
	}
	if code, _ := postTask(t, s, ts.Client(), ts.URL+"/api/brief/save", briefSaveRequest{Path: "a.md", Content: "# B\n", SHA256: "stale"}); code != http.StatusConflict {
		t.Errorf("stale save: %d, want 409", code)
	}
	if code, out := postTask(t, s, ts.Client(), ts.URL+"/api/brief/save", briefSaveRequest{Path: "a.md", Content: "# B\n", SHA256: briefDigest("# A\n")}); code != http.StatusOK || out["sha256"] != briefDigest("# B\n") {
		t.Errorf("save: %d %v", code, out)
	}

	if code, _ := postTask(t, s, ts.Client(), ts.URL+"/api/tasks/draft", draftRequest{Description: "weekly IHSG"}); code != http.StatusNotImplemented {
		t.Errorf("draft with no DraftTask: %d, want 501", code)
	}
	var asked string
	s.DraftTask = func(_ context.Context, folder, desc string) (string, string, error) {
		asked = folder + "|" + desc
		return "run_draft", "# Weekly IHSG\n", nil
	}
	if code, _ := postTask(t, s, ts.Client(), ts.URL+"/api/tasks/draft", draftRequest{Description: "  "}); code != http.StatusBadRequest {
		t.Errorf("empty description: %d, want 400", code)
	}
	code, out := postTask(t, s, ts.Client(), ts.URL+"/api/tasks/draft", draftRequest{Description: "weekly IHSG"})
	if code != http.StatusOK || out["content"] != "# Weekly IHSG\n" || out["run_id"] != "run_draft" || asked != ws+"|weekly IHSG" {
		t.Errorf("draft: %d %v, asked %q", code, out, asked)
	}
	if entries, _ := os.ReadDir(ws); len(entries) != 1 {
		t.Errorf("a draft wrote a file: %v", entries)
	}

	// Empty workspace and empty default folder falls back to cwd
	s.DefaultWorkspace = ""
	t.Chdir(ws)
	if code, out := postTask(t, s, ts.Client(), ts.URL+"/api/brief/save", briefSaveRequest{Path: "a.md", Content: "# C\n", SHA256: briefDigest("# B\n")}); code != http.StatusOK || out["sha256"] != briefDigest("# C\n") {
		t.Errorf("save falling back to cwd: %d %v", code, out)
	}
	if code, _ := postTask(t, s, ts.Client(), ts.URL+"/api/tasks/draft", draftRequest{Description: "weekly IHSG"}); code != http.StatusOK || asked != ws+"|weekly IHSG" {
		t.Errorf("draft falling back to cwd: %d, asked %q", code, asked)
	}
}

// "Run this version" runs the text in the editor, named after the file and
// marked as not saved, and leaves the file as it was.
func TestChatRunsAnUnsavedTaskText(t *testing.T) {
	s, ts := newTestServer(t, endResponse("done"))
	ws := t.TempDir()
	writeAt(t, filepath.Join(ws, "a.md"), "# Saved version\n", time.Now())

	body, _ := json.Marshal(chatRequest{Brief: "a.md", BriefText: "# Edited, not saved\n", Workspace: ws})
	resp := post(t, s, ts, string(body), nil)
	evs := events(t, bodyOf(t, resp))
	runID, _ := evs[0].Data["run_id"].(string)
	st, err := s.Store.Load(runID)
	if err != nil {
		t.Fatalf("no conversation: %v (%v)", err, evs)
	}
	if st.Brief != "a.md, not saved" || !strings.Contains(st.Task, "Edited, not saved") {
		t.Errorf("brief %q, task %q", st.Brief, st.Task)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "a.md")); string(data) != "# Saved version\n" {
		t.Errorf("running unsaved text changed the file: %q", data)
	}

	for name, req := range map[string]chatRequest{
		"text without a name": {BriefText: "# x\n", Workspace: ws},
		"blank text":          {Brief: "a.md", BriefText: "   ", Workspace: ws},
		"name outside":        {Brief: "../x.md", BriefText: "# x\n", Workspace: ws},
	} {
		b, _ := json.Marshal(req)
		if r := post(t, s, ts, string(b), nil); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, r.StatusCode)
		}
	}
}

func deleteTask(t *testing.T, s *Server, client *http.Client, url string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("DELETE", url, strings.NewReader(string(b)))
	req.Header.Set("X-Ariadne-CSRF", s.CSRFToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestDeleteBriefRemovesTaskFile(t *testing.T) {
	ws := t.TempDir()
	writeAt(t, filepath.Join(ws, "sub", "task.md"), "# Task\n", time.Now())

	rel, err := deleteWorkspaceBrief(ws, "sub/task.md")
	if err != nil || rel != "sub/task.md" {
		t.Fatalf("delete: %q %v", rel, err)
	}
	if _, err := os.Stat(filepath.Join(ws, "sub", "task.md")); !os.IsNotExist(err) {
		t.Errorf("file still exists after delete: %v", err)
	}

	// Deleting again should fail with not exist
	if _, err := deleteWorkspaceBrief(ws, "sub/task.md"); err == nil {
		t.Error("deleting non-existent file should fail")
	}

	// Deleting outside workspace or non-.md must fail
	for name, path := range map[string]string{
		"outside": "../outside.md",
		"not md":  "task.txt",
		"blank":   "   ",
	} {
		if _, err := deleteWorkspaceBrief(ws, path); err == nil {
			t.Errorf("%s: expected error for %q", name, path)
		}
	}
}

func TestDeleteBriefEndpoint(t *testing.T) {
	s, ts := newTestServer(t)
	ws := t.TempDir()
	s.DefaultWorkspace = ws
	writeAt(t, filepath.Join(ws, "remove-me.md"), "# Remove\n", time.Now())

	// Without CSRF token: must return 403 Forbidden
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/brief", strings.NewReader(`{"path":"remove-me.md"}`))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("DELETE without token: %d, want 403", resp.StatusCode)
	}

	// With CSRF token: deletes successfully
	code, out := deleteTask(t, s, ts.Client(), ts.URL+"/api/brief", briefDeleteRequest{Path: "remove-me.md"})
	if code != http.StatusOK || out["ok"] != true || out["path"] != "remove-me.md" {
		t.Errorf("deleteTask got code %d out %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(ws, "remove-me.md")); !os.IsNotExist(err) {
		t.Errorf("file still exists on disk after DELETE /api/brief")
	}

	// Deleting a non-existent file returns 404
	code, _ = deleteTask(t, s, ts.Client(), ts.URL+"/api/brief", briefDeleteRequest{Path: "remove-me.md"})
	if code != http.StatusNotFound {
		t.Errorf("deleting missing file returned %d, want 404", code)
	}

	// Deleting invalid path returns 400
	code, _ = deleteTask(t, s, ts.Client(), ts.URL+"/api/brief", briefDeleteRequest{Path: "../escape.md"})
	if code != http.StatusBadRequest {
		t.Errorf("deleting invalid path returned %d, want 400", code)
	}
}
