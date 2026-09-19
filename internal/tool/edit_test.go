package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func editIn(t *testing.T, dir string, a editArgs) (string, bool) {
	t.Helper()
	b, _ := json.Marshal(a)
	res, err := NewEditFile(dir).Call(context.Background(), "call_e", b)
	if err != nil {
		t.Fatalf("Call returned an error rather than a result: %v", err)
	}
	return res.Content, res.IsError
}

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readTemp(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The one thing edit_file is for: change the named text and leave every other
// byte of the file as it was.
func TestEditFileChangesOnlyWhatItNames(t *testing.T) {
	dir := t.TempDir()
	original := "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n\n// keep this comment exactly\n"
	p := writeTemp(t, dir, "src/main.go", original)

	out, isErr := editIn(t, dir, editArgs{Path: "src/main.go", OldText: `println("hello")`, NewText: `println("goodbye")`})
	if isErr {
		t.Fatalf("edit failed: %s", out)
	}
	want := strings.Replace(original, `println("hello")`, `println("goodbye")`, 1)
	if got := readTemp(t, p); got != want {
		t.Errorf("file = %q\nwant %q", got, want)
	}
	if !strings.Contains(out, "line 4") {
		t.Errorf("result %q does not say where the edit was", out)
	}
}

// Ambiguity is refused with the count, so the model widens its snippet rather
// than editing whichever copy came first. replace_all is the explicit opt-in.
func TestEditFileRefusesAnAmbiguousMatch(t *testing.T) {
	dir := t.TempDir()
	p := writeTemp(t, dir, "notes.txt", "todo\nlater\ntodo\n")

	out, isErr := editIn(t, dir, editArgs{Path: "notes.txt", OldText: "todo", NewText: "done"})
	if !isErr || !strings.Contains(out, "2 times") {
		t.Errorf("result %q, want a refusal naming 2 occurrences", out)
	}
	if got := readTemp(t, p); got != "todo\nlater\ntodo\n" {
		t.Errorf("a refused edit changed the file: %q", got)
	}

	out, isErr = editIn(t, dir, editArgs{Path: "notes.txt", OldText: "todo", NewText: "done", ReplaceAll: true})
	if isErr || !strings.Contains(out, "2 occurrences") {
		t.Errorf("replace_all: %q", out)
	}
	if got := readTemp(t, p); got != "done\nlater\ndone\n" {
		t.Errorf("replace_all left %q", got)
	}
}

func TestEditFileRefusesWhatItCannotDo(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "a.txt", "alpha\n")
	writeTemp(t, dir, "bin.dat", "ab\x00cd")
	outside := writeTemp(t, t.TempDir(), "secret.txt", "alpha\n")

	for name, c := range map[string]struct {
		args editArgs
		want string
	}{
		"not found":     {editArgs{Path: "a.txt", OldText: "beta", NewText: "b"}, "not found"},
		"missing file":  {editArgs{Path: "nope.txt", OldText: "a", NewText: "b"}, "write_file"},
		"empty old":     {editArgs{Path: "a.txt", OldText: "", NewText: "b"}, "write_file"},
		"no change":     {editArgs{Path: "a.txt", OldText: "alpha", NewText: "alpha"}, "same"},
		"binary":        {editArgs{Path: "bin.dat", OldText: "ab", NewText: "xy"}, "not a text file"},
		"parent escape": {editArgs{Path: "../secret.txt", OldText: "alpha", NewText: "owned"}, "edit_file"},
		"absolute path": {editArgs{Path: outside, OldText: "alpha", NewText: "owned"}, "edit_file"},
	} {
		out, isErr := editIn(t, dir, c.args)
		if !isErr || !strings.Contains(out, c.want) {
			t.Errorf("%s: result %q, want an error mentioning %q", name, out, c.want)
		}
	}
	if got := readTemp(t, outside); got != "alpha\n" {
		t.Errorf("a file outside the workspace was changed: %q", got)
	}
}

// A file saved on Windows keeps its CRLF line endings when the model's text
// uses "\n", instead of matching nothing or ending up with mixed endings.
func TestEditFileKeepsCRLFLineEndings(t *testing.T) {
	dir := t.TempDir()
	p := writeTemp(t, dir, "win.txt", "first line\r\nsecond line\r\nthird line\r\n")

	out, isErr := editIn(t, dir, editArgs{Path: "win.txt", OldText: "first line\nsecond line", NewText: "one\ntwo"})
	if isErr {
		t.Fatalf("edit failed: %s", out)
	}
	if got := readTemp(t, p); got != "one\r\ntwo\r\nthird line\r\n" {
		t.Errorf("file = %q, want CRLF kept throughout", got)
	}
}

// The edit is a rename over the original: no temporary file is left behind,
// and the file keeps its permissions.
func TestEditFileLeavesNoTemporaryFileAndKeepsPermissions(t *testing.T) {
	dir := t.TempDir()
	p := writeTemp(t, dir, "run.sh", "echo old\n")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(p, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.Stat(p)

	if out, isErr := editIn(t, dir, editArgs{Path: "run.sh", OldText: "old", NewText: "new"}); isErr {
		t.Fatalf("edit failed: %s", out)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only run.sh", names)
	}
	after, _ := os.Stat(p)
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("permissions changed from %v to %v", before.Mode().Perm(), after.Mode().Perm())
	}
}
