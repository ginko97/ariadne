package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

func getTranscript(t *testing.T, ts *httptest.Server, runID string) (*http.Response, transcriptResponse) {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + "/api/runs/" + runID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var out transcriptResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decoding transcript: %v", err)
		}
	}
	return resp, out
}

// A person scrolling back needs to see what happened, in order, and to tell a
// question from an answer from a tool call. "user" alone cannot say that: it
// covers both a person asking something and the loop handing back results.
func TestTranscriptSeparatesPromptsAnswersAndTools(t *testing.T) {
	s, ts := newTestServer(t)

	st := loop.NewState("run_transcript", "what is 21 * 3?")
	st.Model = "test-model"
	st.Steps = 2
	st.Messages = append(st.Messages,
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "Let me calculate."},
			{Type: llm.BlockToolUse, ID: "c1", Name: "calc", Args: json.RawMessage(`{"expr":"21*3"}`)},
		}},
		llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, CallID: "c1", Content: "63"},
		}},
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "21 * 3 = 63"},
		}},
	)
	if err := s.Store.Save(st); err != nil {
		t.Fatal(err)
	}

	resp, got := getTranscript(t, ts, "run_transcript")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Title != "what is 21 * 3?" || got.Model != "test-model" {
		t.Errorf("title=%q model=%q, want the opening line and the run's model", got.Title, got.Model)
	}

	want := []struct{ kind, text, tool string }{
		{"prompt", "what is 21 * 3?", ""},
		{"answer", "Let me calculate.", ""},
		{"tool_call", "", "calc"},
		{"tool_result", "63", ""},
		{"answer", "21 * 3 = 63", ""},
	}
	if len(got.Messages) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got.Messages), len(want), got.Messages)
	}
	for i, w := range want {
		e := got.Messages[i]
		if e.Kind != w.kind {
			t.Errorf("entry %d kind = %q, want %q", i, e.Kind, w.kind)
		}
		if w.text != "" && e.Text != w.text {
			t.Errorf("entry %d text = %q, want %q", i, e.Text, w.text)
		}
		if w.tool != "" && e.Tool != w.tool {
			t.Errorf("entry %d tool = %q, want %q", i, e.Tool, w.tool)
		}
	}
	// The arguments travel with the call, so the page can show what was asked
	// of the tool rather than only that one ran.
	if got.Messages[2].Args != `{"expr":"21*3"}` {
		t.Errorf("tool_call args = %q, want the call's arguments", got.Messages[2].Args)
	}
}

// A brief conversation records the brief path on its transcript, so reopening
// it in the browser renders the task brief card rather than an ordinary chat bubble.
func TestTranscriptIncludesBriefWhenStartedFromBrief(t *testing.T) {
	s, ts := newTestServer(t)

	st := loop.NewState("run_brief_transcript", "# My Brief\n\nTask details.")
	st.Brief = "reports/brief.md"
	if err := s.Store.Save(st); err != nil {
		t.Fatal(err)
	}

	resp, got := getTranscript(t, ts, "run_brief_transcript")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Brief != "reports/brief.md" {
		t.Errorf("brief = %q, want reports/brief.md", got.Brief)
	}
}

// A tool-only assistant turn has no prose. Rendering it as an empty entry would
// suggest the model said nothing, when in fact it did something.
func TestTranscriptDropsEmptyTextBlocks(t *testing.T) {
	s, ts := newTestServer(t)

	st := loop.NewState("run_emptytext", "do a thing")
	st.Messages = append(st.Messages,
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "   "},
			{Type: llm.BlockToolUse, ID: "c1", Name: "fetch", Args: json.RawMessage(`{}`)},
		}},
	)
	if err := s.Store.Save(st); err != nil {
		t.Fatal(err)
	}

	_, got := getTranscript(t, ts, "run_emptytext")
	for _, e := range got.Messages {
		if e.Kind == "answer" && e.Text == "" {
			t.Error("a blank answer entry reached the page")
		}
	}
	if len(got.Messages) != 2 { // the prompt and the tool call
		t.Errorf("got %d entries, want 2: %+v", len(got.Messages), got.Messages)
	}
}

// A failing tool is part of the record: the model saw the error and carried on,
// and a transcript that hides it cannot explain what the model did next.
func TestTranscriptMarksFailedToolResults(t *testing.T) {
	s, ts := newTestServer(t)

	st := loop.NewState("run_toolerr", "read a missing file")
	st.Messages = append(st.Messages,
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "c1", Name: "fetch", Args: json.RawMessage(`{"path":"nope"}`)},
		}},
		llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, CallID: "c1", Content: "no such file", IsError: true},
		}},
	)
	if err := s.Store.Save(st); err != nil {
		t.Fatal(err)
	}

	_, got := getTranscript(t, ts, "run_toolerr")
	last := got.Messages[len(got.Messages)-1]
	if last.Kind != "tool_result" || !last.IsError {
		t.Errorf("last entry = %+v, want a tool_result marked as an error", last)
	}
}

// The list and one conversation are different routes at overlapping paths.
// Go's mux prefers the more specific pattern; asserted because a shadowed list
// would fail as "no such conversation" for a run id nobody asked for.
func TestTranscriptRouteDoesNotShadowTheList(t *testing.T) {
	s, ts := newTestServer(t, endResponse("hi"))
	bodyOf(t, post(t, s, ts, `{"message":"a conversation"}`, nil))

	list := getRuns(t, s, ts, "")
	if len(list.Runs) != 1 {
		t.Fatalf("/api/runs returned %d rows, want 1 — the list route was shadowed", len(list.Runs))
	}

	resp, got := getTranscript(t, ts, list.Runs[0].RunID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/runs/{id} status = %d, want 200", resp.StatusCode)
	}
	if got.RunID != list.Runs[0].RunID {
		t.Errorf("transcript run id = %q, want %q", got.RunID, list.Runs[0].RunID)
	}
}

func TestTranscriptRejectsUnknownAndMalformedIDs(t *testing.T) {
	_, ts := newTestServer(t)

	if resp, _ := getTranscript(t, ts, "run_missing"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown run gave %d, want 404", resp.StatusCode)
	}
	if resp, _ := getTranscript(t, ts, "not-a-run-id"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed id gave %d, want 400", resp.StatusCode)
	}
}

func TestTranscriptDistinguishesCompactionNoticesFromPrompts(t *testing.T) {
	s, ts := newTestServer(t)

	st := loop.NewState("run_compacted", "what is the capital of France?")
	st.Messages[0].Blocks = append(st.Messages[0].Blocks, llm.Block{
		Type: llm.BlockText,
		Text: "[2 earlier messages have been dropped to stay within the context budget. What they contained:]\n- asked: hi\n- replied: hello",
	})
	st.Messages = append(st.Messages, llm.Message{
		Role:   llm.RoleAssistant,
		Blocks: []llm.Block{{Type: llm.BlockText, Text: "Paris"}},
	})
	if err := s.Store.Save(st); err != nil {
		t.Fatal(err)
	}

	resp, got := getTranscript(t, ts, "run_compacted")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("got %d messages, want 3: %+v", len(got.Messages), got.Messages)
	}
	if got.Messages[0].Kind != "prompt" {
		t.Errorf("entry 0 kind = %q, want prompt", got.Messages[0].Kind)
	}
	if got.Messages[1].Kind != "notice" {
		t.Errorf("entry 1 kind = %q, want notice", got.Messages[1].Kind)
	}
	if got.Messages[2].Kind != "answer" {
		t.Errorf("entry 2 kind = %q, want answer", got.Messages[2].Kind)
	}
}

func TestTranscriptReportsPendingAndDefaultWorkspace(t *testing.T) {
	s, ts := newTestServer(t)
	s.DefaultWorkspace = "/default/workspace"

	st := loop.NewState("run_pending_ts", "do something")
	st.Messages = append(st.Messages, llm.Message{
		Role:   llm.RoleAssistant,
		Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "c1", Name: "calc", Args: []byte(`{}`)}},
	})
	if err := s.Store.Save(st); err != nil {
		t.Fatal(err)
	}

	resp, got := getTranscript(t, ts, "run_pending_ts")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !got.Pending {
		t.Error("got.Pending = false, want true")
	}
	if got.Workspace != "/default/workspace" {
		t.Errorf("got.Workspace = %q, want /default/workspace", got.Workspace)
	}
}

// A conversation reopened from the list must say its cost is unknown the same
// way the live turn did, or the page falls back to showing a figure.
func TestTranscriptSaysWhenCostIsUnknown(t *testing.T) {
	s, ts := newTestServer(t)
	st := loop.NewState("run_unpriced_ts", "hello")
	st.Steps, st.UnpricedSteps = 2, 2
	if err := s.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	if _, got := getTranscript(t, ts, "run_unpriced_ts"); !got.CostUnknown {
		t.Error("CostUnknown = false for a run with unpriced steps")
	}

	st = loop.NewState("run_priced_ts", "hello")
	st.Steps, st.Cost = 2, 0.01
	if err := s.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	if _, got := getTranscript(t, ts, "run_priced_ts"); got.CostUnknown {
		t.Error("CostUnknown = true for a run whose every step was priced")
	}
}

func deleteRun(t *testing.T, ts *httptest.Server, runID, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/runs/"+runID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-Ariadne-CSRF", token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// A conversation deleted from the page is gone from the store, and the same
// request cannot be made by another site: DELETE is mutating, so the guard
// requires the token.
func TestDeleteRemovesAConversation(t *testing.T) {
	s, ts := newTestServer(t)

	st := loop.NewState("run_20260920T130000_bbbbbb", "delete me")
	if err := s.Store.Save(st); err != nil {
		t.Fatal(err)
	}

	if resp := deleteRun(t, ts, st.RunID, s.CSRFToken); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", resp.StatusCode)
	}
	if _, err := s.Store.Load(st.RunID); err == nil {
		t.Error("the conversation is still loadable after a 200")
	}

	// Gone, not merely hidden.
	if resp := deleteRun(t, ts, st.RunID, s.CSRFToken); resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleting it twice = %d, want 404", resp.StatusCode)
	}

	// Without the token it is refused, like every other mutating request.
	keep := loop.NewState("run_20260920T130001_cccccc", "keep me")
	if err := s.Store.Save(keep); err != nil {
		t.Fatal(err)
	}
	if resp := deleteRun(t, ts, keep.RunID, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("delete without the CSRF token = %d, want 403", resp.StatusCode)
	}
	if _, err := s.Store.Load(keep.RunID); err != nil {
		t.Error("a refused delete removed the conversation anyway")
	}
}

// A turn in progress is not deleted out from under itself: the loop is still
// writing checkpoints into that directory.
func TestDeleteRefusesABusyConversation(t *testing.T) {
	s, ts := newTestServer(t)

	st := loop.NewState("run_20260920T140000_dddddd", "busy")
	if err := s.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	release, ok := s.claim(st.RunID)
	if !ok {
		t.Fatal("could not claim the run")
	}

	if resp := deleteRun(t, ts, st.RunID, s.CSRFToken); resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete of a busy run = %d, want 409", resp.StatusCode)
	}
	if _, err := s.Store.Load(st.RunID); err != nil {
		t.Errorf("the busy conversation was deleted anyway: %v", err)
	}

	// And once the turn is over it can be deleted.
	release()
	if resp := deleteRun(t, ts, st.RunID, s.CSRFToken); resp.StatusCode != http.StatusOK {
		t.Errorf("delete after release = %d, want 200", resp.StatusCode)
	}
}

// A reopened conversation says which model wrote each answer, including after
// a switch — the picker shows what the next turn will use, which is a
// different question.
func TestTranscriptNamesTheModelPerAnswer(t *testing.T) {
	s, ts := newTestServer(t)

	st := loop.NewState("run_20260920T150000_eeeeee", "who are you?")
	// Switched again after the last answer: the conversation is on a model
	// that has not said anything yet, which is exactly when "the model" and
	// "the model that answered" are different questions.
	st.Model = "vendor/next"
	st.Messages = append(st.Messages,
		llm.Message{Role: llm.RoleAssistant, Model: "vendor/old",
			Blocks: []llm.Block{{Type: llm.BlockText, Text: "answered by the old one"}}},
		llm.Message{Role: llm.RoleUser,
			Blocks: []llm.Block{{Type: llm.BlockText, Text: "and now?"}}},
		llm.Message{Role: llm.RoleAssistant, Model: "vendor/new",
			Blocks: []llm.Block{{Type: llm.BlockText, Text: "answered by the new one"}}},
	)
	if err := s.Store.Save(st); err != nil {
		t.Fatal(err)
	}

	resp, got := getTranscript(t, ts, st.RunID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var answers []transcriptEntry
	for _, e := range got.Messages {
		if e.Kind == "answer" {
			answers = append(answers, e)
		}
	}
	if len(answers) != 2 {
		t.Fatalf("answers = %d, want 2", len(answers))
	}
	if answers[0].Model != "vendor/old" || answers[1].Model != "vendor/new" {
		t.Errorf("models = %q, %q; want vendor/old, vendor/new", answers[0].Model, answers[1].Model)
	}
	for _, e := range got.Messages {
		if e.Kind == "prompt" && e.Model != "" {
			t.Errorf("a prompt carries a model: %q", e.Model)
		}
	}

	// answeredBy reads the last answer, not the model the conversation is on.
	if by := answeredBy(st); by != "vendor/new" {
		t.Errorf("answeredBy = %q, want vendor/new (state.Model is %q)", by, st.Model)
	}
	empty := loop.NewState("run_20260920T150001_ffffff", "nothing yet")
	if by := answeredBy(empty); by != "" {
		t.Errorf("answeredBy on a conversation with no answer = %q, want empty", by)
	}
}
