package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

func endResponse(text string) llm.Response {
	return llm.Response{
		Blocks: []llm.Block{{Type: llm.BlockText, Text: text}},
		Stop:   llm.StopEnd,
		Usage:  llm.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

// newTestServer wires a server whose agents answer from a Fake, so every test
// here runs with no key, no network and no cost.
func newTestServer(t *testing.T, responses ...llm.Response) (*Server, *httptest.Server) {
	t.Helper()
	store := &loop.Store{Dir: t.TempDir()}
	fake := &llm.Fake{Responses: responses}

	n := 0
	s := New(store,
		func(runID string, onDelta func(llm.Chunk)) *loop.Agent {
			return &loop.Agent{
				Provider:   fake,
				Model:      "test-model",
				MaxSteps:   5,
				Checkpoint: store.Save,
			}
		},
		func() string { n++; return "run_test_" + string(rune('a'+n-1)) },
	)
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func post(t *testing.T, s *Server, ts *httptest.Server, body string, mutate func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+"/api/chat", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ariadne-CSRF", s.CSRFToken)
	if mutate != nil {
		mutate(req)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// events parses an SSE body into ordered (name, data) pairs, which is what
// every assertion here is actually about.
func events(t *testing.T, body string) []struct {
	Name string
	Data map[string]any
} {
	t.Helper()
	var out []struct {
		Name string
		Data map[string]any
	}
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" {
			continue
		}
		m := map[string]any{}
		if data != "" {
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				t.Fatalf("event %q has unparseable data %q: %v", name, data, err)
			}
		}
		out = append(out, struct {
			Name string
			Data map[string]any
		}{name, m})
	}
	return out
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestChatStartsAConversation(t *testing.T) {
	s, ts := newTestServer(t, endResponse("hello back"))

	resp := post(t, s, ts, `{"message":"hello"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, bodyOf(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	evs := events(t, bodyOf(t, resp))
	if len(evs) < 2 {
		t.Fatalf("want a start and a done event, got %d: %v", len(evs), evs)
	}
	if evs[0].Name != "start" {
		t.Errorf("first event = %q, want start", evs[0].Name)
	}
	last := evs[len(evs)-1]
	if last.Name != "done" {
		t.Fatalf("last event = %q, want done", last.Name)
	}
	if last.Data["answer"] != "hello back" {
		t.Errorf("answer = %v, want hello back", last.Data["answer"])
	}
	if last.Data["run_id"] != evs[0].Data["run_id"] {
		t.Errorf("run id changed mid-stream: %v then %v", evs[0].Data["run_id"], last.Data["run_id"])
	}
}

// The turn has to be checkpointed for the conversation to be a run at all —
// that is the claim the whole release rests on, so it is asserted here rather
// than assumed from the agent's configuration.
func TestChatCheckpointsSoTheRunCanBeResumed(t *testing.T) {
	s, ts := newTestServer(t, endResponse("first answer"))

	resp := post(t, s, ts, `{"message":"first question"}`, nil)
	evs := events(t, bodyOf(t, resp))
	runID, _ := evs[len(evs)-1].Data["run_id"].(string)
	if runID == "" {
		t.Fatal("done event carried no run id")
	}

	st, err := s.Store.Load(runID)
	if err != nil {
		t.Fatalf("the run was not checkpointed: %v", err)
	}
	if st.Task != "first question" {
		t.Errorf("Task = %q, want the opening message", st.Task)
	}
}

func TestChatContinuesAnExistingConversation(t *testing.T) {
	s, ts := newTestServer(t, endResponse("answer one"), endResponse("answer two"))

	first := events(t, bodyOf(t, post(t, s, ts, `{"message":"question one"}`, nil)))
	runID := first[len(first)-1].Data["run_id"].(string)

	resp := post(t, s, ts, `{"run_id":"`+runID+`","message":"question two"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, bodyOf(t, resp))
	}
	evs := events(t, bodyOf(t, resp))
	last := evs[len(evs)-1]
	if last.Data["answer"] != "answer two" {
		t.Errorf("answer = %v, want answer two", last.Data["answer"])
	}
	if last.Data["run_id"] != runID {
		t.Errorf("run id = %v, want the same run %q", last.Data["run_id"], runID)
	}

	// Both turns are in one conversation, not two runs that happen to share a name.
	st, err := s.Store.Load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Turns() != 2 {
		t.Errorf("Turns() = %d, want 2", st.Turns())
	}
}

// The reason claim exists. Two turns on one run race on State and write the
// same checkpoint file; the second is refused rather than queued.
func TestChatRefusesAConcurrentTurnOnTheSameRun(t *testing.T) {
	s, ts := newTestServer(t, endResponse("only answer"))

	release, ok := s.claim("run_busy")
	if !ok {
		t.Fatal("could not claim a free run")
	}
	defer release()

	resp := post(t, s, ts, `{"run_id":"run_busy","message":"hello"}`, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

// claim must be atomic: many goroutines, exactly one winner.
func TestClaimIsAtomic(t *testing.T) {
	s, _ := newTestServer(t)

	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := s.claim("run_contended"); ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Errorf("%d goroutines claimed the same run, want exactly 1", won)
	}
}

// A run released after its turn is claimable again, or one failed request
// would wedge the conversation for the life of the process.
func TestClaimIsReleasedAfterATurn(t *testing.T) {
	s, ts := newTestServer(t, endResponse("one"), endResponse("two"))

	evs := events(t, bodyOf(t, post(t, s, ts, `{"message":"hi"}`, nil)))
	runID := evs[len(evs)-1].Data["run_id"].(string)

	resp := post(t, s, ts, `{"run_id":"`+runID+`","message":"again"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the first turn did not release the run", resp.StatusCode)
	}
}

// An unfinished batch cannot take a new message: AddUserMessage refuses, and
// silently flushing would stream the answer to the *previous* question.
func TestChatRefusesARunWithAnUnfinishedBatch(t *testing.T) {
	s, ts := newTestServer(t, endResponse("unused"))

	st := loop.NewState("run_pending", "do something")
	st.Messages = append(st.Messages, llm.Message{
		Role:   llm.RoleAssistant,
		Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "c1", Name: "calc", Args: []byte(`{}`)}},
	})
	if err := s.Store.Save(st); err != nil {
		t.Fatal(err)
	}

	resp := post(t, s, ts, `{"run_id":"run_pending","message":"something else"}`, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, bodyOf(t, resp))
	}
}

// A failure after the headers are out can only be an event — the 200 is
// already sent — and it has to carry the run id, because the turn failed and
// the conversation did not.
func TestChatReportsAFailureAsAnEvent(t *testing.T) {
	s, ts := newTestServer(t) // no scripted responses: the fake is exhausted

	resp := post(t, s, ts, `{"message":"hello"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the stream had already opened", resp.StatusCode)
	}
	evs := events(t, bodyOf(t, resp))
	last := evs[len(evs)-1]
	if last.Name != "error" {
		t.Fatalf("last event = %q, want error", last.Name)
	}
	if last.Data["run_id"] == "" || last.Data["run_id"] == nil {
		t.Error("the error event carried no run id, so the run cannot be resumed")
	}
}

func TestChatRejectsBadRequests(t *testing.T) {
	s, ts := newTestServer(t, endResponse("unused"))

	for _, tc := range []struct{ name, body string }{
		{"empty message", `{"message":"   "}`},
		{"no message", `{"run_id":"run_x"}`},
		{"malformed json", `{"message":`},
		{"traversal in run id", `{"run_id":"../../etc","message":"hi"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := post(t, s, ts, tc.body, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestChatRejectsUnknownRun(t *testing.T) {
	s, ts := newTestServer(t, endResponse("unused"))
	resp := post(t, s, ts, `{"run_id":"run_nope","message":"hi"}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// Loopback binding stops other machines, not other pages: any site the browser
// has open can POST to localhost. These three are what actually stand between
// a page and somebody's API budget.
func TestGuardRefusesWhatLoopbackBindingDoesNot(t *testing.T) {
	s, ts := newTestServer(t, endResponse("unused"))

	t.Run("cross-origin", func(t *testing.T) {
		resp := post(t, s, ts, `{"message":"hi"}`, func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example")
		})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("missing csrf token", func(t *testing.T) {
		resp := post(t, s, ts, `{"message":"hi"}`, func(r *http.Request) {
			r.Header.Del("X-Ariadne-CSRF")
		})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("wrong csrf token", func(t *testing.T) {
		resp := post(t, s, ts, `{"message":"hi"}`, func(r *http.Request) {
			r.Header.Set("X-Ariadne-CSRF", "not-the-token")
		})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("non-loopback host", func(t *testing.T) {
		resp := post(t, s, ts, `{"message":"hi"}`, func(r *http.Request) {
			r.Host = "ariadne.example.com"
		})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("same-origin is allowed", func(t *testing.T) {
		resp := post(t, s, ts, `{"message":"hi"}`, func(r *http.Request) {
			r.Header.Set("Origin", ts.URL)
		})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200 — a same-origin request was refused", resp.StatusCode)
		}
	})
}

// Tool arguments arrive as fragments that are not valid JSON alone, and tool
// indices restart at 0 each turn. Both rules are printDelta's, and both have
// already cost a bug once on the terminal side.
func TestDeltaEventsAnnounceEachToolOnceAndResetBetweenTurns(t *testing.T) {
	rec := httptest.NewRecorder()
	out := &sseWriter{w: rec, f: rec}
	emit := deltaEvents(out)

	emit(llm.Chunk{Text: "thinking "})
	emit(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, ID: "c1", Name: "calc"}})
	emit(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, Args: `{"expr"`}})
	emit(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, Args: `:"2+2"}`}})
	emit(llm.Chunk{Stop: llm.StopToolUse})
	// Turn two: the provider reuses index 0 for a different tool.
	emit(llm.Chunk{ToolCall: &llm.ToolDelta{Index: 0, ID: "c2", Name: "fetch"}})

	body := rec.Body.String()
	if n := strings.Count(body, `"name":"calc"`); n != 1 {
		t.Errorf("calc announced %d times, want 1:\n%s", n, body)
	}
	if n := strings.Count(body, `"name":"fetch"`); n != 1 {
		t.Errorf("fetch announced %d times, want 1 — the turn reset did not happen:\n%s", n, body)
	}
	if strings.Contains(body, `expr`) {
		t.Errorf("argument fragments were streamed:\n%s", body)
	}
	if !strings.Contains(body, `"text":"thinking "`) {
		t.Errorf("text delta was not streamed:\n%s", body)
	}
}
