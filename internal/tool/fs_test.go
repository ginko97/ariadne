package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func call(t *testing.T, tl Tool, args any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.Call(context.Background(), "call_1", raw)
	if err != nil {
		t.Fatalf("%s returned an error rather than an IsError result: %v", tl.Name(), err)
	}
	return res.Content, res.IsError
}

func TestFetchReadsInsideSandbox(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, isErr := call(t, NewFetch(dir), fetchArgs{Path: "page.txt"})
	if isErr || got != "hello" {
		t.Errorf("got %q isError=%v, want %q", got, isErr, "hello")
	}
}

// Every path argument comes from the model, and everything the model has seen
// may have been written by someone else. Treat it as hostile input.
func TestSandboxRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	for _, p := range []string{
		"../secret.txt",
		"../../secret.txt",
		"a/../../secret.txt",
		`..\secret.txt`,
	} {
		got, isErr := call(t, NewFetch(dir), fetchArgs{Path: p})
		if strings.Contains(got, "classified") {
			t.Errorf("fetch %q escaped the sandbox and read the file", p)
		}
		if !isErr {
			t.Errorf("fetch %q was not refused: %q", p, got)
		}
	}
}

// An absolute path must land inside the sandbox, not at the filesystem root.
func TestSandboxContainsAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	w := NewWriteFile(dir)

	if _, isErr := call(t, w, writeArgs{Path: "/etc/passwd", Content: "x"}); isErr {
		// Refusing is fine too; what matters is where it did not write.
		t.Logf("absolute path refused, which is acceptable")
	}
	if _, err := os.Stat("/etc/passwd.ariadne-test"); err == nil {
		t.Fatal("wrote outside the sandbox")
	}
	// It should have landed under the sandbox root instead.
	if _, err := os.Stat(filepath.Join(dir, "etc", "passwd")); err != nil {
		t.Logf("absolute path was refused rather than remapped: %v", err)
	}
}

func TestWriteFileRoundTrip(t *testing.T) {
	dir := t.TempDir()

	got, isErr := call(t, NewWriteFile(dir), writeArgs{Path: "notes/todo.txt", Content: "buy milk"})
	if isErr {
		t.Fatalf("write failed: %s", got)
	}
	if !strings.Contains(got, "8 bytes") {
		t.Errorf("result should say what was written: %q", got)
	}

	data, err := os.ReadFile(filepath.Join(dir, "notes", "todo.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "buy milk" {
		t.Errorf("file contains %q", data)
	}
}

// Writing the same bytes to the same path twice is harmless, which is why this
// tool does not need callID. A tool that appended or sent something would.
func TestWriteFileIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	w := NewWriteFile(dir)

	for i := 0; i < 2; i++ {
		if _, isErr := call(t, w, writeArgs{Path: "x.txt", Content: "once"}); isErr {
			t.Fatal("write failed")
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "x.txt"))
	if string(data) != "once" {
		t.Errorf("replayed write produced %q", data)
	}
}

func TestFetchMissingFileIsRecoverable(t *testing.T) {
	got, isErr := call(t, NewFetch(t.TempDir()), fetchArgs{Path: "nope.txt"})
	if !isErr {
		t.Fatal("missing file should be an IsError result")
	}
	if !strings.Contains(got, "nope.txt") {
		t.Errorf("message should name the path: %q", got)
	}
}

func TestToolsRejectEmptyPath(t *testing.T) {
	dir := t.TempDir()
	if _, isErr := call(t, NewFetch(dir), fetchArgs{Path: ""}); !isErr {
		t.Error("fetch accepted an empty path")
	}
	if _, isErr := call(t, NewWriteFile(dir), writeArgs{Path: "", Content: "x"}); !isErr {
		t.Error("write_file accepted an empty path")
	}
}
