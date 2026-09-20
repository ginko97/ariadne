package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/trace"
)

func TestCheckpointRoundTrip(t *testing.T) {
	store := &Store{Dir: t.TempDir()}

	st := NewState("run_20260912T090000_abc123", "what is 15% of 240?")
	st.Messages = append(st.Messages,
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "call_1", Name: "calc",
				Args: json.RawMessage(`{"expr":"240*0.15"}`)},
		}},
		llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, CallID: "call_1", Content: "36"},
		}},
	)
	st.Steps = 1
	st.Cost = 0.0042

	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	got, err := store.Load(st.RunID)
	if err != nil {
		t.Fatal(err)
	}

	// The assertion that matters: the conversation survived. Args is a
	// json.RawMessage and CallID is what resume pairs on — lose either and
	// resume double-fires.
	//
	// Not reflect.DeepEqual: Save uses MarshalIndent, which re-indents embedded
	// RawMessage bytes, so DeepEqual would compare whitespace rather than
	// meaning. Comparing canonical encodings asserts structure and values.
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("round trip lost data\ngot  %s\nwant %s", gotJSON, wantJSON)
	}
}

func TestLoadUnknownRun(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	if _, err := s.Load("run_nope"); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("got %v, want ErrNoCheckpoint", err)
	}
}

func TestRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}

	if err := s.Save(&State{RunID: "../escape"}); err == nil {
		t.Error("Save accepted a traversing run id")
	}
	if _, err := s.Load("../../etc"); err == nil {
		t.Error("Load accepted a traversing run id")
	}

	// The assertion that matters: nothing was written outside Dir.
	if entries, _ := os.ReadDir(filepath.Dir(dir)); len(entries) != 1 {
		t.Errorf("files appeared beside the store: %v", entries)
	}
}

func TestRejectsFutureSchema(t *testing.T) {
	dir := t.TempDir()
	runID := "run_20260912T000000_future"
	if err := os.MkdirAll(filepath.Join(dir, runID), 0o755); err != nil {
		t.Fatal(err)
	}

	// Hand-written: a checkpoint from a newer binary than this one.
	body := []byte(`{"schema_version":99,"written_at":"2026-09-12T00:00:00Z",` +
		`"state":{"run_id":"` + runID + `","task":"t"}}`)
	if err := os.WriteFile(filepath.Join(dir, runID, "checkpoint.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := (&Store{Dir: dir}).Load(runID)
	if err == nil {
		t.Fatal("Load accepted a schema version it does not understand")
	}
	// The message has to name the version, or the operator cannot tell
	// "upgrade ariadne" from "this file is corrupt".
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("error should name the version found: %v", err)
	}
}

func TestSaveOverwritesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}

	st := NewState("run_20260912T000000_over", "task")
	st.Steps = 1
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	st.Steps = 7
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load(st.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Steps != 7 {
		t.Errorf("Steps = %d, want 7 — the second save did not win", got.Steps)
	}

	// Exactly one file: the rename consumed the temp. A leftover .tmp means a
	// cleanup path is unwired, and it is the kind of litter nobody notices
	// until runs/ is full of half-written files.
	entries, err := os.ReadDir(filepath.Join(dir, st.RunID))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "checkpoint.json" {
		t.Errorf("run dir = %v, want only checkpoint.json", entries)
	}
}

// An API key must never reach runs/ — not a checkpoint, not a trace line, not an
// error string. Covers both a successful tool-calling run and one that fails on
// a 401, since the error path is where a key is most likely to be echoed.
func TestKeyNeverReachesRuns(t *testing.T) {
	const sentinelKey = "sk-sentinel-secret-token-never-leak-98765"

	t.Run("successful run with tool call", func(t *testing.T) {
		dir := t.TempDir()
		runID := "run_sec_success"
		store := &Store{Dir: dir}
		tw, err := trace.NewFileWriter(dir, runID)
		if err != nil {
			t.Fatal(err)
		}
		defer tw.Close()

		rt := &llm.RecordedTransport{
			Responses: [][]byte{
				[]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"thinking","tool_calls":[{"id":"c1","type":"function","function":{"name":"calc","arguments":"{\"expr\":\"2+2\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`),
				[]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"4"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`),
			},
		}

		provider := llm.NewOpenAI(sentinelKey,
			llm.WithBaseURL("https://api.test.example"),
			llm.WithHTTPClient(&http.Client{Transport: rt}),
		)

		agent := &Agent{
			Provider:   provider,
			Model:      "test-model",
			Trace:      tw.Emit,
			Checkpoint: store.Save,
			RunTool: func(_ context.Context, c llm.ToolCall) (llm.ToolResult, error) {
				return llm.ToolResult{Content: "4"}, nil
			},
			MaxSteps: 5,
		}

		st := NewState(runID, "calculate 2+2")
		_, err = agent.Run(context.Background(), st)
		if err != nil {
			t.Fatalf("agent.Run: %v", err)
		}

		assertNoSecretInDir(t, dir, sentinelKey)
	})

	t.Run("error run does not leak in trace or checkpoint", func(t *testing.T) {
		dir := t.TempDir()
		runID := "run_sec_error"
		store := &Store{Dir: dir}
		tw, err := trace.NewFileWriter(dir, runID)
		if err != nil {
			t.Fatal(err)
		}
		defer tw.Close()

		rt := &llm.RecordedTransport{
			Statuses: []int{http.StatusUnauthorized},
			Responses: [][]byte{
				[]byte(`{"error":{"message":"Invalid API key provided"}}`),
			},
		}

		provider := llm.NewOpenAI(sentinelKey,
			llm.WithBaseURL("https://api.test.example"),
			llm.WithHTTPClient(&http.Client{Transport: rt}),
		)

		agent := &Agent{
			Provider:   provider,
			Model:      "test-model",
			Trace:      tw.Emit,
			Checkpoint: store.Save,
			MaxSteps:   5,
		}

		st := NewState(runID, "hello")
		_, err = agent.Run(context.Background(), st)
		if err == nil {
			t.Fatal("expected error from 401 response")
		}
		if strings.Contains(err.Error(), sentinelKey) {
			t.Fatalf("returned error leaked key: %v", err)
		}

		assertNoSecretInDir(t, dir, sentinelKey)
	})
}

func assertNoSecretInDir(t *testing.T, dir, secret string) {
	t.Helper()
	foundFiles := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		foundFiles++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("file %s leaked secret key", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("filepath.Walk: %v", err)
	}
	if foundFiles == 0 {
		t.Fatal("assertNoSecretInDir: expected files in dir, found none")
	}
}

func TestValidRunID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"run_123", true},
		{"run_abc-def_456", true},
		{"run_", false},
		{"", false},
		{"run_invalid!char", false},
		{"run_with/slash", false},
		{"run_with..dot", false},
		{"run_" + strings.Repeat("a", 125), false}, // 129 chars total
		{"run_" + strings.Repeat("a", 124), true},  // 128 chars total
		{"norunprefix", false},
	}
	for _, tc := range cases {
		if got := ValidRunID(tc.id); got != tc.want {
			t.Errorf("ValidRunID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// The order must come from what Save recorded, not from the filesystem.
//
// mtime is an artifact of how a file got where it is: restoring a backup,
// a git checkout, rsync, or copying a runs/ directory between machines all
// rewrite it, and every one of those would reshuffle the conversation list into
// an order nobody chose. WrittenAt travels inside the checkpoint, so it says
// when the run was actually written however the bytes arrived.
func TestStoreListOrdersByWrittenAtNotFileTime(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}

	older := NewState("run_20260912T000001_older", "asked first")
	newer := NewState("run_20260912T000002_newer", "asked second")
	if err := store.Save(older); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(newer); err != nil {
		t.Fatal(err)
	}

	// Reverse the file times: the newer run's file now looks ancient, which is
	// exactly what a restore or a checkout does to a directory of runs.
	long, _ := time.Parse(time.RFC3339, "2001-01-01T00:00:00Z")
	recent := time.Now().Add(24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, newer.RunID, "checkpoint.json"), long, long); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, older.RunID, "checkpoint.json"), recent, recent); err != nil {
		t.Fatal(err)
	}

	list, _, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d runs, want 2", len(list))
	}
	if list[0].RunID != newer.RunID {
		t.Errorf("first run = %s, want %s — the list followed file times rather than WrittenAt",
			list[0].RunID, newer.RunID)
	}
}

// sort.Slice is not stable, so runs written inside the same clock tick can swap
// places between two calls. A conversation list that reorders itself when
// nothing changed is the kind of flicker nobody reports and nobody can
// reproduce, so the order is made total with the run id.
func TestStoreListIsDeterministicWhenTimestampsTie(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}

	tied, _ := time.Parse(time.RFC3339, "2026-09-15T12:00:00Z")
	for _, id := range []string{"run_tie_a", "run_tie_b", "run_tie_c", "run_tie_d"} {
		st := NewState(id, "same instant")
		if err := store.Save(st); err != nil {
			t.Fatal(err)
		}
		// Rewrite the envelope so every run claims the identical write time.
		path := filepath.Join(dir, id, "checkpoint.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cp Checkpoint
		if err := json.Unmarshal(data, &cp); err != nil {
			t.Fatal(err)
		}
		cp.WrittenAt = tied
		out, err := json.Marshal(cp)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, out, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	first, _, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 4 {
		t.Fatalf("got %d runs, want 4", len(first))
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].RunID <= first[i].RunID {
			t.Fatalf("tied runs are not in a defined order: %s then %s",
				first[i-1].RunID, first[i].RunID)
		}
	}
	// Repeated because the failure is instability, not a wrong first answer.
	for range 20 {
		again, _, err := store.List()
		if err != nil {
			t.Fatal(err)
		}
		for i := range again {
			if again[i].RunID != first[i].RunID {
				t.Fatalf("order moved between calls at %d: %s then %s",
					i, first[i].RunID, again[i].RunID)
			}
		}
	}
}

func TestStoreList(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}

	// 1. Non-existent directory returns nil, 0, nil
	sNone := &Store{Dir: filepath.Join(dir, "missing")}
	runs, skipped, err := sNone.List()
	if err != nil || len(runs) != 0 || skipped != 0 {
		t.Fatalf("List on missing dir = (%v, %d, %v), want (nil, 0, nil)", runs, skipped, err)
	}

	// 2. Save two runs with known timestamps
	st1 := NewState("run_20260912T000001_aaa", "first task")
	st1.Model = "model-1"
	if err := store.Save(st1); err != nil {
		t.Fatal(err)
	}

	st2 := NewState("run_20260912T000002_bbb", "second task")
	st2.Model = "model-2"
	if err := store.Save(st2); err != nil {
		t.Fatal(err)
	}

	// 3. Directory with no checkpoint is not skipped or listed
	if err := os.MkdirAll(filepath.Join(dir, "run_20260912T000003_uncheckpointed"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 4. Directory with corrupt checkpoint is skipped
	corruptDir := filepath.Join(dir, "run_20260912T000004_corrupt")
	if err := os.MkdirAll(corruptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "checkpoint.json"), []byte("{bad json"), 0o600); err != nil {
		t.Fatal(err)
	}

	list, skipped, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	if len(list) != 2 {
		t.Fatalf("got %d runs, want 2", len(list))
	}
	if list[0].RunID != st2.RunID {
		t.Errorf("first run = %s, want %s (newest first)", list[0].RunID, st2.RunID)
	}
	if list[1].RunID != st1.RunID {
		t.Errorf("second run = %s, want %s", list[1].RunID, st1.RunID)
	}
	if list[0].Updated.IsZero() || list[1].Updated.IsZero() {
		t.Error("Updated timestamp should be non-zero from WrittenAt")
	}
}

// Deleting a conversation takes the trace with it. A checkpoint removed while
// every byte the run saw stays on disk is not a delete.
func TestDeleteRemovesTheWholeRun(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}

	st := &State{RunID: "run_20260920T120000_aaaaaa", Task: "delete me"}
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(dir, st.RunID, "trace.jsonl")
	if err := os.WriteFile(trace, []byte("{\"kind\":\"run_start\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(st.RunID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, st.RunID)); !os.IsNotExist(err) {
		t.Errorf("the run directory is still there: %v", err)
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Error("the trace outlived the conversation")
	}
	if runs, _, err := s.List(); err != nil || len(runs) != 0 {
		t.Errorf("List = %v (%v), want empty", runs, err)
	}

	// Deleting what is already gone is the outcome asked for, not an error.
	if err := s.Delete(st.RunID); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

// The id is a path component, so a bad one must be refused rather than joined.
func TestDeleteRefusesABadRunID(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Store{Dir: dir}
	for _, id := range []string{"", "..", "../..", "run_../../etc", "keep.txt"} {
		if err := s.Delete(id); err == nil {
			t.Errorf("delete %q was accepted", id)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("a refused delete still removed something: %v", err)
	}
}
