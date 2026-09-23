package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

// A first turn whose model call dropped leaves a conversation with a question
// and no answer. "Try again" finishes it without a new message, the transcript
// says it awaits an answer until then, and nothing else may ride along.
func TestTryAgainFinishesATurnWhoseAnswerNeverArrived(t *testing.T) {
	fake := &llm.Fake{
		Responses: []llm.Response{{}, endResponse("IHSG closed higher.")},
		Errs:      []error{errors.New("read stream: connection forcibly closed by the remote host")},
	}
	store := &loop.Store{Dir: t.TempDir()}
	s := New(store,
		func(string, *loop.State, func(llm.Chunk)) (*loop.Agent, func()) {
			return &loop.Agent{Provider: fake, Model: "test-model", MaxSteps: 5, Checkpoint: store.Save}, nil
		},
		func() string { return "run_retry" },
	)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	post := func(req chatRequest) (int, string) {
		t.Helper()
		b, _ := json.Marshal(req)
		r, _ := http.NewRequest("POST", ts.URL+"/api/chat", strings.NewReader(string(b)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Ariadne-CSRF", s.CSRFToken)
		resp, err := ts.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	awaits := func() bool {
		t.Helper()
		r, _ := http.NewRequest("GET", ts.URL+"/api/runs/run_retry", nil)
		resp, err := ts.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var tr transcriptResponse
		if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
			t.Fatal(err)
		}
		return tr.AwaitsAnswer
	}

	if _, body := post(chatRequest{Message: "How did IHSG close?"}); !strings.Contains(body, "event: error\n") {
		t.Fatalf("the first turn should fail on the dropped connection:\n%s", body)
	}
	if !awaits() {
		t.Fatal("the transcript does not say the conversation awaits an answer, so the page cannot offer Try again")
	}

	// Refused before anything runs: a message, a brief or a resume alongside.
	for name, req := range map[string]chatRequest{
		"with a message": {RunID: "run_retry", Retry: true, Message: "continue"},
		"with resume":    {RunID: "run_retry", Retry: true, Resume: true},
		"without a run":  {Retry: true},
	} {
		if code, body := post(req); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", name, code, body)
		}
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("a refused retry reached the provider: %d calls", len(fake.Calls))
	}

	code, body := post(chatRequest{RunID: "run_retry", Retry: true})
	if code != http.StatusOK || !strings.Contains(body, "event: done\n") || !strings.Contains(body, "IHSG closed higher.") {
		t.Fatalf("try again: status %d, want the answer:\n%s", code, body)
	}
	st, err := store.Load("run_retry")
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, m := range st.Messages {
		if m.Role == llm.RoleUser {
			users++
		}
	}
	if users != 1 || len(st.Messages) != 2 {
		t.Errorf("history has %d messages, %d from the person; want the question and its answer only", len(st.Messages), users)
	}
	if awaits() {
		t.Error("an answered conversation still says it awaits an answer")
	}

	// Nothing left to try again.
	if code, body := post(chatRequest{RunID: "run_retry", Retry: true}); code != http.StatusBadRequest {
		t.Errorf("retry on an answered conversation: status %d, want 400: %s", code, body)
	}
}
