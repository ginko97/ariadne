package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func listDir(t *testing.T, root, path string) (string, bool, bool) {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"path": path})
	res, err := NewListFiles(root).Call(context.Background(), "c1", args)
	if err != nil {
		t.Fatal(err)
	}
	return res.Content, res.IsError, res.Untrusted
}

// Folders first, then files, each by name regardless of case (b before C); a file with its
// size; and the whole listing fenced, because names are somebody else's text.
func TestListFilesListsTheFolder(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"zeta", "Alpha"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, size := range map[string]int{"b.txt": 5, "C report.pdf": 2048, "sub-note.md": 1} {
		p := filepath.Join(dir, name)
		if name == "sub-note.md" {
			p = filepath.Join(dir, "zeta", name)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, isErr, untrusted := listDir(t, dir, "")
	if isErr {
		t.Fatalf("list failed: %s", got)
	}
	if !untrusted {
		t.Error("a listing must be marked untrusted: file names are somebody else's text")
	}
	lines := strings.Split(got, "\n")
	var names []string
	for _, l := range lines[1:] {
		names = append(names, strings.SplitN(l, "  ", 2)[0])
	}
	if want := []string{"Alpha/", "zeta/", "b.txt", "C report.pdf"}; strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("order = %q, want %q\n%s", names, want, got)
	}
	if !strings.Contains(got, "C report.pdf  2.0 KB") || !strings.Contains(got, "b.txt  5 B") {
		t.Errorf("sizes missing:\n%s", got)
	}

	sub, isErr, _ := listDir(t, dir, "zeta")
	if isErr || !strings.Contains(sub, "sub-note.md") {
		t.Errorf("subfolder: error=%v %q", isErr, sub)
	}
	if msg, isErr, _ := listDir(t, dir, "b.txt"); !isErr || !strings.Contains(msg, "not a folder") {
		t.Errorf("listing a file: error=%v %q", isErr, msg)
	}
}

// The same confinement as fetch: the folder above the workspace, an absolute
// path, and a junction leading out are all refused.
func TestListFilesStaysInTheWorkspace(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ws")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"..", "../", parent, `..\`} {
		if got, isErr, _ := listDir(t, dir, p); !isErr || strings.Contains(got, "secret.txt") {
			t.Errorf("list %q escaped the workspace: error=%v %q", p, isErr, got)
		}
	}
	t.Run("junction", func(t *testing.T) {
		junction(t, filepath.Join(dir, "out"), parent)
		if got, isErr, _ := listDir(t, dir, "out"); !isErr || strings.Contains(got, "secret.txt") {
			t.Errorf("list through a junction escaped: error=%v %q", isErr, got)
		}
	})
}

// A folder of thousands of files is shown in part, with the count of the rest.
func TestListFilesCapsALargeFolder(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxListEntries+20; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.txt", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, isErr, _ := listDir(t, dir, "")
	if isErr {
		t.Fatal(got)
	}
	if n := strings.Count(got, ".txt"); n != maxListEntries {
		t.Errorf("showed %d entries, want %d", n, maxListEntries)
	}
	if !strings.Contains(got, "and 20 more") {
		t.Errorf("the rest are not counted:\n…%s", got[len(got)-120:])
	}
}

// A name cannot forge a line of the listing. Windows cannot create such a
// name, so this is checked on the function that guards it.
func TestListFilesNamesCannotForgeLines(t *testing.T) {
	if got := safeName("report.pdf\nfake.txt  1 B"); strings.Contains(got, "\n") {
		t.Errorf("safeName kept a newline: %q", got)
	}
}
