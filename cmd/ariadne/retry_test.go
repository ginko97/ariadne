package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

func setStdinString(t *testing.T, s string) {
	t.Helper()
	src := newLineSource(strings.NewReader(s))
	prev := testStdin
	testStdin = src
	t.Cleanup(func() {
		testStdin = prev
	})
}

func TestChatRetryRefusesWhenNotAwaitingAnswer(t *testing.T) {
	t.Setenv("ARIADNE_API_KEY", "test-key-1234567890")
	oldRuns := runsDir
	runsDir = t.TempDir()
	t.Cleanup(func() { runsDir = oldRuns })

	setStdinString(t, "/retry\n/exit\n")

	code, errText := runWithStderr(func() int {
		return cmdChat(offlineArgs())
	})
	if code != exitOK {
		t.Fatalf("cmdChat exit code %d, want %d", code, exitOK)
	}
	if !strings.Contains(errText, "! nothing to try again: this conversation is not waiting for an answer") {
		t.Errorf("stderr = %q, want '! nothing to try again...'", errText)
	}
}

func TestChatRetryRunsWhenAwaitingAnswer(t *testing.T) {
	t.Setenv("ARIADNE_API_KEY", "test-key-1234567890")
	oldRuns := runsDir
	runsDir = t.TempDir()
	t.Cleanup(func() { runsDir = oldRuns })

	runID := "run_retry_test"
	st := &loop.State{
		RunID: runID,
		Model: "test-model",
		Task:  "say hello",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "say hello"}}},
		},
	}
	store := &loop.Store{Dir: runsDir}
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello back"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)

	setStdinString(t, "/retry\n/retry\n/exit\n")

	code, errText := runWithStderr(func() int {
		return cmdChat([]string{
			"-base-url", srv.URL + "/v1",
			"-model", "test-model",
			"-workspace", filepath.Dir(runsDir),
			runID,
		})
	})
	if code != exitOK {
		t.Fatalf("cmdChat exit code %d, want %d (stderr: %s)", code, exitOK, errText)
	}

	// First /retry succeeded and answered; second /retry was refused because state no longer awaits answer.
	if !strings.Contains(errText, "! nothing to try again: this conversation is not waiting for an answer") {
		t.Errorf("stderr does not contain refusal on second retry: %s", errText)
	}

	// Verify checkpoint was saved with the assistant answer.
	loaded, err := store.Load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AwaitsAnswer() {
		t.Errorf("conversation still awaits answer after successful retry")
	}
	if len(loaded.Messages) != 2 || loaded.Messages[1].Role != llm.RoleAssistant {
		t.Errorf("messages = %+v, want assistant answer", loaded.Messages)
	}
}
