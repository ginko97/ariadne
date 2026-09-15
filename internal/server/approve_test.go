package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

// syncRecorder is a ResponseWriter the test can read while a goroutine writes.
//
// httptest.ResponseRecorder is not safe for concurrent use, and these tests read
// the stream to know when the prompt has arrived — the one place in this package
// where a writer and a reader are genuinely on different goroutines. Production
// never is: every event, delta and approval prompt is written from the handler
// goroutine, because approvals are evaluated in the serial pre-flight before
// runCalls dispatches anything in parallel.
type syncRecorder struct {
	mu  sync.Mutex
	buf strings.Builder
	hdr http.Header
}

func newSyncRecorder() *syncRecorder { return &syncRecorder{hdr: http.Header{}} }

func (r *syncRecorder) Header() http.Header { return r.hdr }
func (r *syncRecorder) WriteHeader(int)     {}
func (r *syncRecorder) Flush()              {}

func (r *syncRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(b)
}

func (r *syncRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// approveVia posts a decision the way the page will.
func approveVia(t *testing.T, s *Server, ts *httptest.Server, runID, callID string, ok bool) *http.Response {
	t.Helper()
	body, _ := json.Marshal(approveRequest{RunID: runID, CallID: callID, Approve: ok})
	req, err := http.NewRequest("POST", ts.URL+"/api/approve", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ariadne-CSRF", s.CSRFToken)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The whole mechanism: a turn blocks inside the loop, the question goes out on
// the stream it is already holding, and the answer arrives on a different
// request entirely.
func TestApproverBlocksUntilASeparateRequestAnswers(t *testing.T) {
	s, ts := newTestServer(t)
	rec := newSyncRecorder()
	out := &sseWriter{w: rec, f: rec}

	approve := s.approver("run_a", out)
	done := make(chan bool, 1)
	go func() {
		ok, _ := approve(context.Background(), llm.ToolCall{ID: "call_1", Name: "write_file"})
		done <- ok
	}()

	// The prompt has to reach the page before anyone can answer it.
	waitFor(t, func() bool { return strings.Contains(rec.body(), "approval_required") })
	if !strings.Contains(rec.body(), `"tool":"write_file"`) {
		t.Errorf("the prompt does not say which tool:\n%s", rec.body())
	}

	if resp := approveVia(t, s, ts, "run_a", "call_1", true); resp.StatusCode != http.StatusOK {
		t.Fatalf("approve status = %d, want 200", resp.StatusCode)
	}
	select {
	case ok := <-done:
		if !ok {
			t.Error("an explicit yes was not honoured")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the turn never unblocked")
	}
}

func TestApproverHonoursADenial(t *testing.T) {
	s, ts := newTestServer(t)
	rec := newSyncRecorder()
	out := &sseWriter{w: rec, f: rec}

	done := make(chan bool, 1)
	go func() {
		ok, _ := s.approver("run_d", out)(context.Background(), llm.ToolCall{ID: "c", Name: "write_file"})
		done <- ok
	}()
	waitFor(t, func() bool { return strings.Contains(rec.body(), "approval_required") })
	approveVia(t, s, ts, "run_d", "c", false)

	select {
	case ok := <-done:
		if ok {
			t.Error("a denial was read as approval")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the turn never unblocked")
	}
}

// A closed tab is nobody there, and nobody there is no. The prompt went out on
// a stream that no longer has a reader, so waiting for an answer would hold the
// conversation's claim until the process stopped.
func TestApproverDeniesWhenTheCallerDisappears(t *testing.T) {
	s, _ := newTestServer(t)
	rec := newSyncRecorder()
	out := &sseWriter{w: rec, f: rec}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		ok, _ := s.approver("run_gone", out)(ctx, llm.ToolCall{ID: "c", Name: "write_file"})
		done <- ok
	}()
	waitFor(t, func() bool { return strings.Contains(rec.body(), "approval_required") })
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Error("a cancelled request approved a tool call")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled request left the turn waiting")
	}
}

// An answer for a question nobody is holding is told so, rather than accepted
// into nothing. That is the difference between "denied" and "your answer went
// nowhere", and only one of them is worth showing somebody.
func TestApproveReportsWhenNothingIsWaiting(t *testing.T) {
	s, ts := newTestServer(t)
	resp := approveVia(t, s, ts, "run_nobody", "call_x", true)
	if resp.StatusCode != http.StatusGone {
		t.Errorf("status = %d, want 410", resp.StatusCode)
	}
}

// Decisions are keyed by run *and* call, so a decision cannot be applied to a
// different conversation by guessing a call id.
func TestApproveDoesNotCrossConversations(t *testing.T) {
	s, ts := newTestServer(t)
	rec := newSyncRecorder()
	out := &sseWriter{w: rec, f: rec}

	done := make(chan bool, 1)
	go func() {
		ok, _ := s.approver("run_mine", out)(context.Background(), llm.ToolCall{ID: "shared", Name: "write_file"})
		done <- ok
	}()
	waitFor(t, func() bool { return strings.Contains(rec.body(), "approval_required") })

	// Same call id, different run: must not answer the question above.
	if resp := approveVia(t, s, ts, "run_theirs", "shared", true); resp.StatusCode != http.StatusGone {
		t.Errorf("a decision for another run was accepted: %d", resp.StatusCode)
	}
	select {
	case <-done:
		t.Fatal("another conversation's approval unblocked this turn")
	case <-time.After(150 * time.Millisecond):
	}

	approveVia(t, s, ts, "run_mine", "shared", true)
	<-done
}

// The approve endpoint mutates, so it carries the same token as /api/chat. The
// note that condition was written under applies most sharply here: the worst
// case is not noise, it is a page approving a tool call on somebody's behalf.
func TestApproveRequiresTheCSRFToken(t *testing.T) {
	_, ts := newTestServer(t)
	body, _ := json.Marshal(approveRequest{RunID: "run_x", CallID: "c", Approve: true})
	req, _ := http.NewRequest("POST", ts.URL+"/api/approve", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d without a token, want 403", resp.StatusCode)
	}
}

// End to end through a real turn: a gated tool, a prompt on the stream, a
// decision from elsewhere, and the loop carrying on with the result.
func TestGatedToolIsApprovedOverHTTP(t *testing.T) {
	store := &loop.Store{Dir: t.TempDir()}
	fake := &llm.Fake{Responses: []llm.Response{
		{
			Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "w1", Name: "write_file",
				Args: json.RawMessage(`{"path":"a.txt"}`)}},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 5},
		},
		endResponse("wrote it"),
	}}

	var called bool
	s := New(store,
		func(runID string, state *loop.State, onDelta func(llm.Chunk)) (*loop.Agent, func()) {
			return &loop.Agent{
				Provider:        fake,
				Model:           "test-model",
				MaxSteps:        5,
				Checkpoint:      store.Save,
				RequireApproval: []string{"write_file"},
				RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
					called = true
					return llm.ToolResult{Content: "ok"}, nil
				},
			}, nil
		},
		func() string { return "run_gate" },
	)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	go func() {
		// Answer as soon as the turn asks.
		for range 200 {
			if s.approvals.decide(approvalKey("run_gate", "w1"), true) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	body := bodyOf(t, post(t, s, ts, `{"message":"write a file"}`, nil))
	if !strings.Contains(body, "approval_required") {
		t.Fatalf("no approval was requested:\n%s", body)
	}
	if !called {
		t.Error("the approved tool never ran")
	}
	if !strings.Contains(body, "wrote it") {
		t.Errorf("the turn did not finish after approval:\n%s", body)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// The factory is handed the conversation's state, which is the whole point of
// the signature: per-conversation settings live on the checkpoint, and a server
// that built every agent from its own flags would override them.
//
// That already happened twice — f6854cf, where a resumed conversation adopted
// the server's startup model, and 4a55078, where it adopted the server's
// workspace. Both are the same shape: startup configuration silently winning
// over what the conversation recorded.
func TestAgentFactoryReceivesTheConversationsState(t *testing.T) {
	store := &loop.Store{Dir: t.TempDir()}
	fake := &llm.Fake{Responses: []llm.Response{endResponse("ok")}}

	// A conversation that was started against a directory of its own.
	st := loop.NewState("run_ws", "an earlier question")
	st.Model = "test-model"
	st.Workspace = "/repo/alpha"
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	var gotWorkspace, gotRunID string
	s := New(store,
		func(runID string, state *loop.State, onDelta func(llm.Chunk)) (*loop.Agent, func()) {
			gotRunID = runID
			if state != nil {
				gotWorkspace = state.Workspace
			}
			return &loop.Agent{Provider: fake, Model: "test-model", MaxSteps: 5, Checkpoint: store.Save}, nil
		},
		func() string { return "run_unused" },
	)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	bodyOf(t, post(t, s, ts, `{"run_id":"run_ws","message":"next question"}`, nil))

	if gotRunID != "run_ws" {
		t.Errorf("factory got run id %q, want run_ws", gotRunID)
	}
	if gotWorkspace != "/repo/alpha" {
		t.Errorf("factory saw workspace %q, want the one the conversation recorded", gotWorkspace)
	}
}

// A fresh conversation has state too — built before the factory runs — so the
// factory never has to guess which case it is in.
func TestAgentFactoryReceivesStateForAFreshConversation(t *testing.T) {
	store := &loop.Store{Dir: t.TempDir()}
	fake := &llm.Fake{Responses: []llm.Response{endResponse("ok")}}

	var gotTask string
	s := New(store,
		func(runID string, state *loop.State, onDelta func(llm.Chunk)) (*loop.Agent, func()) {
			if state != nil {
				gotTask = state.Task
			}
			return &loop.Agent{Provider: fake, Model: "test-model", MaxSteps: 5, Checkpoint: store.Save}, nil
		},
		func() string { return "run_fresh" },
	)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	bodyOf(t, post(t, s, ts, `{"message":"the opening line"}`, nil))
	if gotTask != "the opening line" {
		t.Errorf("factory saw task %q, want the opening line", gotTask)
	}
}

// The factory's own Approve is replaced, not consulted.
//
// This is what lets cmdUI pass -approve with no ApproveFn at all: the approver
// needs the stream the request is holding, which the factory cannot see, so
// handleChat sets Agent.Approve after construction. A denier left in cmdUI
// would be dead code implying a fallback that does not exist — this is the
// test that says so, rather than a comment claiming it.
func TestServerReplacesTheFactorysApprover(t *testing.T) {
	store := &loop.Store{Dir: t.TempDir()}
	fake := &llm.Fake{Responses: []llm.Response{
		{
			Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "w1", Name: "write_file",
				Args: json.RawMessage(`{"path":"a.txt"}`)}},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 5, OutputTokens: 5},
		},
		endResponse("done"),
	}}

	var factoryApproverRan bool
	s := New(store,
		func(runID string, state *loop.State, onDelta func(llm.Chunk)) (*loop.Agent, func()) {
			return &loop.Agent{
				Provider:        fake,
				Model:           "test-model",
				MaxSteps:        5,
				Checkpoint:      store.Save,
				RequireApproval: []string{"write_file"},
				// A blanket yes. If it were consulted the call would run with
				// nobody asked, and no prompt would reach the stream.
				Approve: func(context.Context, llm.ToolCall) (bool, error) {
					factoryApproverRan = true
					return true, nil
				},
				RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
					return llm.ToolResult{Content: "ok"}, nil
				},
			}, nil
		},
		func() string { return "run_replace" },
	)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	go func() {
		for range 200 {
			if s.approvals.decide(approvalKey("run_replace", "w1"), true) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	body := bodyOf(t, post(t, s, ts, `{"message":"write a file"}`, nil))
	if factoryApproverRan {
		t.Error("the factory's approver was consulted; the server's never reached the page")
	}
	if !strings.Contains(body, "approval_required") {
		t.Errorf("no prompt reached the stream:\n%s", body)
	}
}
