package loop

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
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
