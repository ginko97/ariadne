package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmitWritesOneLinePerEvent(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "run_1")

	w.Emit(Event{Kind: KindRunStart, Text: "task"})
	w.Emit(Event{Kind: KindToolCall, Tool: "calc", Args: json.RawMessage(`{"expr":"1+1"}`)})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), buf.String())
	}

	events, err := Decode(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Seq != 1 || events[1].Seq != 2 {
		t.Errorf("seq = %d,%d — should count from 1", events[0].Seq, events[1].Seq)
	}
	if events[0].RunID != "run_1" || events[1].RunID != "run_1" {
		t.Error("run id not stamped on every event")
	}
	if events[0].At.IsZero() {
		t.Error("timestamp not stamped")
	}
	if string(events[1].Args) != `{"expr":"1+1"}` {
		t.Errorf("args mangled: %s", events[1].Args)
	}
}

// Empty fields are omitted, so a line stays readable in a terminal.
func TestEmitOmitsEmptyFields(t *testing.T) {
	var buf bytes.Buffer
	NewWriter(&buf, "run_1").Emit(Event{Kind: KindRunStart})

	line := buf.String()
	for _, absent := range []string{"tool", "call_id", "is_error", "cost_usd", "args"} {
		if strings.Contains(line, `"`+absent+`"`) {
			t.Errorf("empty field %q was written: %s", absent, line)
		}
	}
}

// Tracing must not fail a run. A broken sink is recorded and reported by Err,
// never returned from Emit.
func TestEmitSwallowsWriteErrorsButRemembers(t *testing.T) {
	boom := errors.New("disk gone")
	w := NewWriter(failWriter{boom}, "run_1")

	w.Emit(Event{Kind: KindRunStart}) // must not panic
	w.Emit(Event{Kind: KindRunEnd})

	if !errors.Is(w.Err(), boom) {
		t.Fatalf("Err() = %v, want the write error", w.Err())
	}
}

type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

// A resumed run continues the same trace file rather than starting a new one:
// one file is the whole history of a run, across every process that touched it.
func TestFileWriterAppends(t *testing.T) {
	dir := t.TempDir()

	w1, err := NewFileWriter(dir, "run_append")
	if err != nil {
		t.Fatal(err)
	}
	w1.Emit(Event{Kind: KindRunStart})
	if err := w1.Close(); err != nil {
		t.Fatal(err)
	}

	w2, err := NewFileWriter(dir, "run_append")
	if err != nil {
		t.Fatal(err)
	}
	w2.Emit(Event{Kind: KindRunEnd})
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}

	events, err := Read(filepath.Join(dir, "run_append", "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 — the second writer truncated the file", len(events))
	}
	if events[0].Kind != KindRunStart || events[1].Kind != KindRunEnd {
		t.Errorf("order lost: %+v", events)
	}
}

// A crash can leave a half-written final line. Decode returns what it could
// read along with the error, so analysis is still possible.
func TestDecodeReturnsPartialOnTruncation(t *testing.T) {
	good := `{"run_id":"r","seq":1,"kind":"run_start"}` + "\n"
	truncated := `{"run_id":"r","seq":2,"kind":"resp`

	events, err := Decode([]byte(good + truncated))
	if err == nil {
		t.Fatal("want an error for the truncated line")
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want the 1 complete one", len(events))
	}
}

func TestNilWriterIsSafe(t *testing.T) {
	var w *Writer
	w.Emit(Event{Kind: KindRunStart}) // must not panic
	if w.Err() != nil || w.Close() != nil {
		t.Error("nil writer should be inert")
	}
}

func TestReadMissingFile(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "nope.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v, want not-exist", err)
	}
}

func TestNewFileWriterRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"../escape", "../../etc", "a/b", `a\b`} {
		if _, err := NewFileWriter(dir, bad); err == nil {
			t.Errorf("NewFileWriter accepted traversing run id %q", bad)
		}
	}
}
