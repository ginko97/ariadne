package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

// folderServer is a test server whose factory records the folder each turn's
// state carried, which is what the agent's file tools will be confined to.
func folderServer(t *testing.T) (*Server, *httptest.Server, *loop.Store, func() string) {
	t.Helper()
	store := &loop.Store{Dir: t.TempDir()}
	var mu sync.Mutex
	var seen string
	s := New(store,
		func(runID string, state *loop.State, onDelta func(llm.Chunk)) (*loop.Agent, func()) {
			mu.Lock()
			seen = state.Workspace
			mu.Unlock()
			return &loop.Agent{
				Provider:   &llm.Fake{Responses: []llm.Response{endResponse("ok")}},
				Model:      "test-model",
				MaxSteps:   5,
				Checkpoint: store.Save,
			}, nil
		},
		func() string { return "run_folder" },
	)
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return s, ts, store, func() string { mu.Lock(); defer mu.Unlock(); return seen }
}

func postJSON(t *testing.T, s *Server, ts *httptest.Server, path, body string, token bool) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token {
		req.Header.Set("X-Ariadne-CSRF", s.CSRFToken)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

// A new conversation started with a folder works in that folder: the state the
// agent is built from carries it, and it is saved with the conversation.
func TestNewConversationWorksInTheChosenFolder(t *testing.T) {
	s, ts, store, seen := folderServer(t)
	dir := t.TempDir()

	bodyOf(t, post(t, s, ts, `{"message":"hi","workspace":`+quote(dir)+`}`, nil))

	if got := seen(); got != filepath.Clean(dir) {
		t.Errorf("the agent was built for folder %q, want %q", got, dir)
	}
	st, err := store.Load("run_folder")
	if err != nil {
		t.Fatal(err)
	}
	if st.Workspace != filepath.Clean(dir) {
		t.Errorf("the conversation recorded folder %q, want %q", st.Workspace, dir)
	}
}

// Only a real folder, named in full. Refused before a run exists, so a typo
// costs nothing and leaves no half-made conversation behind.
func TestChatRefusesAFolderThatIsNotOne(t *testing.T) {
	s, ts, store, _ := folderServer(t)
	file := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, ws := range map[string]string{
		"relative":    "some/relative/dir",
		"missing":     filepath.Join(t.TempDir(), "does-not-exist"),
		"a file":      file,
		"only spaces": "   ",
	} {
		resp := post(t, s, ts, `{"message":"hi","workspace":`+quote(ws)+`}`, nil)
		body := bodyOf(t, resp)
		if name == "only spaces" {
			// Blank is "no folder": the server's default, not an error.
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s: status %d, want 200 (the default folder): %s", name, resp.StatusCode, body)
			}
			continue
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", name, resp.StatusCode, body)
		}
	}
	if runs, _, _ := store.List(); len(runs) != 1 {
		t.Errorf("%d conversations exist, want only the one with the default folder", len(runs))
	}
}

// A conversation's folder is fixed when it starts. A request naming another
// one is refused rather than ignored: the page believes it will be used.
func TestAConversationsFolderCannotChange(t *testing.T) {
	s, ts, store, _ := folderServer(t)
	mine, other := t.TempDir(), t.TempDir()

	st := loop.NewState("run_fixed", "an earlier question")
	st.Workspace = mine
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	st.Messages = append(st.Messages, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "an answer"}}})
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	resp := post(t, s, ts, `{"run_id":"run_fixed","message":"next","workspace":`+quote(other)+`}`, nil)
	if body := bodyOf(t, resp); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("another folder: status %d, want 400: %s", resp.StatusCode, body)
	}
	resp = post(t, s, ts, `{"run_id":"run_fixed","message":"next","workspace":`+quote(mine)+`}`, nil)
	if body := bodyOf(t, resp); resp.StatusCode != http.StatusOK {
		t.Errorf("its own folder: status %d, want 200: %s", resp.StatusCode, body)
	}
}

func TestTranscriptShowsTheFolder(t *testing.T) {
	_, ts, store, _ := folderServer(t)
	dir := t.TempDir()
	st := loop.NewState("run_shown", "question")
	st.Workspace = dir
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Get(ts.URL + "/api/runs/run_shown")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got transcriptResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Workspace != dir {
		t.Errorf("transcript workspace = %q, want %q", got.Workspace, dir)
	}
}

func TestWorkspaceCheck(t *testing.T) {
	s, ts, _, _ := folderServer(t)
	dir := t.TempDir()
	if resp, out := postJSON(t, s, ts, "/api/workspace/check", `{"path":`+quote(dir)+`}`, true); resp.StatusCode != 200 || out["path"] != filepath.Clean(dir) {
		t.Errorf("a real folder: %d %v", resp.StatusCode, out)
	}
	if resp, out := postJSON(t, s, ts, "/api/workspace/check", `{"path":"nope"}`, true); resp.StatusCode != 400 || out["error"] == nil {
		t.Errorf("a relative path: %d %v", resp.StatusCode, out)
	}
}

// The dialog is a desktop action, so it carries the token like any other POST;
// its answer is checked like a typed folder; cancelling is not an error; and a
// machine with no dialog says so instead of failing.
func TestWorkspacePick(t *testing.T) {
	s, ts, _, _ := folderServer(t)
	dir := t.TempDir()

	if resp, _ := postJSON(t, s, ts, "/api/workspace/pick", ``, true); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("no dialog: status %d, want 501", resp.StatusCode)
	}

	answer, fail := dir, error(nil)
	opened := 0
	s.PickFolder = func(context.Context) (string, error) { opened++; return answer, fail }

	if resp, _ := postJSON(t, s, ts, "/api/workspace/pick", ``, false); resp.StatusCode != http.StatusForbidden || opened != 0 {
		t.Errorf("no token: status %d and %d dialogs opened, want 403 and none", resp.StatusCode, opened)
	}
	if resp, out := postJSON(t, s, ts, "/api/workspace/pick", ``, true); resp.StatusCode != 200 || out["path"] != filepath.Clean(dir) {
		t.Errorf("picked: %d %v", resp.StatusCode, out)
	}
	answer = ""
	if resp, out := postJSON(t, s, ts, "/api/workspace/pick", ``, true); resp.StatusCode != 200 || out["cancelled"] != true {
		t.Errorf("cancelled: %d %v", resp.StatusCode, out)
	}
	answer, fail = "", errors.New("zenity is not installed")
	if resp, out := postJSON(t, s, ts, "/api/workspace/pick", ``, true); resp.StatusCode != 500 || !strings.Contains(out["error"].(string), "type the path") {
		t.Errorf("dialog failed: %d %v", resp.StatusCode, out)
	}
}

func TestWorkspaceDefaultIsReported(t *testing.T) {
	s, ts, _, _ := folderServer(t)
	s.DefaultWorkspace = "/srv/default"
	resp, err := ts.Client().Get(ts.URL + "/api/workspace")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["default"] != "/srv/default" || out["can_pick"] != false {
		t.Errorf("got %v", out)
	}
}

// A relative folder is refused even when it exists: it would be resolved
// against wherever the server happened to start, which the person choosing it
// cannot see. It has to exist here, or the refusal proves nothing about the
// rule — a missing folder is refused anyway.
func TestWorkspaceRefusesARelativeFolderThatExists(t *testing.T) {
	s, ts, _, _ := folderServer(t)
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "rel"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)

	resp, out := postJSON(t, s, ts, "/api/workspace/check", `{"path":"rel"}`, true)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out["error"].(string), "full path") {
		t.Errorf("an existing relative folder: %d %v", resp.StatusCode, out)
	}
}
