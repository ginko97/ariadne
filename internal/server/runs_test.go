package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

func getRuns(t *testing.T, s *Server, ts *httptest.Server, query string) runsResponse {
	t.Helper()
	req, err := http.NewRequest("GET", ts.URL+"/api/runs"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out runsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding runs: %v", err)
	}
	return out
}

// The list is what makes the exit test reachable: talk to it, come back, find
// the conversation. Ordered by last activity, because a conversation resumed
// today sorting under the day it began is the opposite of what that wants.
func TestRunsListsConversationsMostRecentFirst(t *testing.T) {
	s, ts := newTestServer(t,
		endResponse("a"), endResponse("b"), endResponse("c"))

	for _, msg := range []string{"oldest question", "middle question", "newest question"} {
		bodyOf(t, post(t, s, ts, `{"message":"`+msg+`"}`, nil))
		// Distinguishable mtimes: the filesystem's resolution is coarser than
		// three HTTP requests against a fake provider.
		time.Sleep(10 * time.Millisecond)
	}

	got := getRuns(t, s, ts, "")
	if len(got.Runs) != 3 {
		t.Fatalf("listed %d conversations, want 3", len(got.Runs))
	}
	if got.Runs[0].Title != "newest question" {
		t.Errorf("first row = %q, want the most recently active", got.Runs[0].Title)
	}
	if got.Runs[2].Title != "oldest question" {
		t.Errorf("last row = %q, want the least recently active", got.Runs[2].Title)
	}
	if got.Runs[0].RunID == "" || got.Runs[0].Updated == "" {
		t.Error("a row is missing the id or timestamp a client needs to resume it")
	}
}

// The title is the opening line, not the latest one. Relabelling on every turn
// is the 207dae3 bug — a conversation about a migration listed as "thanks".
func TestRunsTitlesAConversationByItsOpeningLine(t *testing.T) {
	s, ts := newTestServer(t, endResponse("one"), endResponse("two"))

	evs := events(t, bodyOf(t, post(t, s, ts, `{"message":"plan the northwind migration"}`, nil)))
	runID := evs[len(evs)-1].Data["run_id"].(string)
	bodyOf(t, post(t, s, ts, `{"run_id":"`+runID+`","message":"thanks"}`, nil))

	got := getRuns(t, s, ts, "")
	if len(got.Runs) != 1 {
		t.Fatalf("listed %d conversations, want 1 — two turns are one conversation", len(got.Runs))
	}
	if got.Runs[0].Title != "plan the northwind migration" {
		t.Errorf("title = %q, want the opening line", got.Runs[0].Title)
	}
	if got.Runs[0].Turns != 2 {
		t.Errorf("turns = %d, want 2", got.Runs[0].Turns)
	}
}

// One unreadable checkpoint must not hide every conversation behind it, and
// the count must be visible — a silently shorter list is indistinguishable
// from a conversation that never existed.
func TestRunsSkipsUnreadableCheckpointsAndSaysHowMany(t *testing.T) {
	s, ts := newTestServer(t, endResponse("fine"))
	bodyOf(t, post(t, s, ts, `{"message":"a real conversation"}`, nil))

	bad := filepath.Join(s.Store.Dir, "run_corrupt")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "checkpoint.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := getRuns(t, s, ts, "")
	if len(got.Runs) != 1 {
		t.Errorf("listed %d conversations, want the 1 readable one", len(got.Runs))
	}
	if got.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 — an unreachable conversation was reported as absent", got.Skipped)
	}
}

// A run directory with a trace but no checkpoint yet is a run that was killed
// before its first tool call. Not an error, and not a listable conversation.
func TestRunsIgnoresDirectoriesWithoutACheckpoint(t *testing.T) {
	s, ts := newTestServer(t, endResponse("fine"))
	bodyOf(t, post(t, s, ts, `{"message":"a real conversation"}`, nil))

	if err := os.MkdirAll(filepath.Join(s.Store.Dir, "run_tracedonly"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := getRuns(t, s, ts, "")
	if len(got.Runs) != 1 || got.Skipped != 0 {
		t.Errorf("runs=%d skipped=%d, want 1 and 0 — a checkpointless directory is neither",
			len(got.Runs), got.Skipped)
	}
}

func TestRunsLimit(t *testing.T) {
	s, ts := newTestServer(t, endResponse("a"), endResponse("b"))
	bodyOf(t, post(t, s, ts, `{"message":"one"}`, nil))
	time.Sleep(10 * time.Millisecond)
	bodyOf(t, post(t, s, ts, `{"message":"two"}`, nil))

	if got := getRuns(t, s, ts, "?limit=1"); len(got.Runs) != 1 {
		t.Errorf("limit=1 returned %d rows", len(got.Runs))
	}
	if got := getRuns(t, s, ts, "?limit=99"); len(got.Runs) != 2 {
		t.Errorf("limit above the count returned %d rows, want all 2", len(got.Runs))
	}

	req, _ := http.NewRequest("GET", ts.URL+"/api/runs?limit=nonsense", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("limit=nonsense gave %d, want 400", resp.StatusCode)
	}
}

// An empty store is a new install, not a failure.
func TestRunsOnAnEmptyStore(t *testing.T) {
	store := &loop.Store{Dir: filepath.Join(t.TempDir(), "never-created")}
	s := New(store,
		func(string, *loop.State, func(llm.Chunk)) (*loop.Agent, func()) { return nil, nil },
		func() string { return "run_unused" },
	)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	got := getRuns(t, s, ts, "")
	if len(got.Runs) != 0 || got.Skipped != 0 {
		t.Errorf("runs=%d skipped=%d, want both 0", len(got.Runs), got.Skipped)
	}
}

func TestRunsReportsNeedsYou(t *testing.T) {
	store := &loop.Store{Dir: t.TempDir()}
	s := New(store,
		func(string, *loop.State, func(llm.Chunk)) (*loop.Agent, func()) { return nil, nil },
		func() string { return "run_1" },
	)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	// 1. A normal state on disk: needs_you should be false
	st1 := loop.NewState("run_1", "regular task")
	if err := store.Save(st1); err != nil {
		t.Fatal(err)
	}
	got := getRuns(t, s, ts, "")
	if len(got.Runs) != 1 || got.Runs[0].NeedsYou {
		t.Fatalf("run_1: got needs_you = %v, want false", got.Runs[0].NeedsYou)
	}

	// 2. An actively waiting card: needs_you should be true
	_ = s.approvals.wait(approvalKey("run_1", "c1"), []string{"c1"}, nil)
	got2 := getRuns(t, s, ts, "")
	if len(got2.Runs) != 1 || !got2.Runs[0].NeedsYou {
		t.Fatalf("run_1 with active approval: got needs_you = %v, want true", got2.Runs[0].NeedsYou)
	}
	s.approvals.forget(approvalKey("run_1", "c1"))

	// 3. A state on disk with pending tool calls: needs_you should be true
	st2 := loop.NewState("run_2", "pending task")
	st2.Messages = append(st2.Messages, llm.Message{
		Role: llm.RoleAssistant,
		Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "call_pend", Name: "write_file", Args: json.RawMessage(`{}`)},
		},
	})
	if err := store.Save(st2); err != nil {
		t.Fatal(err)
	}
	got3 := getRuns(t, s, ts, "")
	for _, r := range got3.Runs {
		if r.RunID == "run_2" && !r.NeedsYou {
			t.Errorf("run_2 with pending tool call: got needs_you = false, want true")
		}
		if r.RunID == "run_1" && r.NeedsYou {
			t.Errorf("run_1: got needs_you = true, want false")
		}
	}
}
