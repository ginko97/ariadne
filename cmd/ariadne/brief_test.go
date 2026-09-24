package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/loop"
)

// deadEndpoint and offline keep every test here off the network even when the
// check it tests is broken. A regression then gets as far as a model call, and
// that call goes to a closed port with a dummy key and fails, instead of
// sending a task file to whichever provider a key in the shell belongs to.
// A dummy key rather than none: with no key the command stops early with
// exitUsage, which is what several of these tests expect, so a broken check
// would pass.
var deadEndpoint = []string{"-base-url", "http://127.0.0.1:1/v1", "-model", "m", "-http-timeout", "2s"}

func offline(t *testing.T) {
	t.Helper()
	t.Setenv("ARIADNE_API_KEY", "test-key-1234567890")
	oldRuns := runsDir
	runsDir = t.TempDir()
	empty, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	oldIn := os.Stdin
	os.Stdin = empty
	t.Cleanup(func() {
		runsDir, os.Stdin = oldRuns, oldIn
		empty.Close()
	})
}

func offlineArgs(a ...string) []string { return append(append([]string{}, deadEndpoint...), a...) }

func TestCmdRunTaskFlagRejectsBothFileAndPositional(t *testing.T) {
	offline(t)
	tmpDir := t.TempDir()
	briefFile := filepath.Join(tmpDir, "brief.md")
	if err := os.WriteFile(briefFile, []byte("my brief task"), 0o644); err != nil {
		t.Fatal(err)
	}

	code := cmdRun(offlineArgs("-task", briefFile, "extra positional argument"))
	if code != exitUsage {
		t.Errorf("cmdRun with both -task and positional args got exit code %d, want exitUsage (%d)", code, exitUsage)
	}
}

func TestCmdRunTaskFlagMissingFile(t *testing.T) {
	offline(t)
	code := cmdRun(offlineArgs("-task", "non_existent_file.md"))
	if code != exitFail {
		t.Errorf("cmdRun with non-existent -task got exit code %d, want exitFail (%d)", code, exitFail)
	}
}

func runWithStderr(fn func() int) (int, string) {
	oldErr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	read := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()
	code := fn()
	w.Close()
	os.Stderr = oldErr
	out := <-read
	return code, out
}

func TestCmdRunTaskFlagEmptyFile(t *testing.T) {
	offline(t)
	tmpDir := t.TempDir()
	emptyFile := filepath.Join(tmpDir, "empty.md")
	if err := os.WriteFile(emptyFile, []byte("   \n\t  "), 0o644); err != nil {
		t.Fatal(err)
	}

	code, errText := runWithStderr(func() int { return cmdRun(offlineArgs("-task", emptyFile)) })
	if code != exitUsage || !strings.Contains(errText, "is empty") {
		t.Errorf("got code %d, stderr %q; want exitUsage and 'is empty'", code, errText)
	}
}

func TestCmdRunTaskFlagNonMarkdownFile(t *testing.T) {
	offline(t)
	tmpDir := t.TempDir()
	txtFile := filepath.Join(tmpDir, "task.txt")
	if err := os.WriteFile(txtFile, []byte("plain text"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, errText := runWithStderr(func() int { return cmdRun(offlineArgs("-task", txtFile)) })
	if code != exitUsage || !strings.Contains(errText, "task file must be a markdown (.md) file") {
		t.Errorf("got code %d, stderr %q; want exitUsage and 'task file must be a markdown (.md) file'", code, errText)
	}
}

func TestCmdRunTaskFlagDirectory(t *testing.T) {
	offline(t)
	tmpDir := t.TempDir()
	dir := filepath.Join(tmpDir, "folder.md")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	code, errText := runWithStderr(func() int { return cmdRun(offlineArgs("-task", dir)) })
	if code != exitUsage || !strings.Contains(errText, "is a directory") {
		t.Errorf("got code %d, stderr %q; want exitUsage and 'is a directory'", code, errText)
	}
}

func TestCmdRunTaskFlagFileTooLarge(t *testing.T) {
	offline(t)
	tmpDir := t.TempDir()
	largeFile := filepath.Join(tmpDir, "large.md")
	f, err := os.Create(largeFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((2 << 20) + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	code, errText := runWithStderr(func() int { return cmdRun(offlineArgs("-task", largeFile)) })
	if code != exitUsage || !strings.Contains(errText, "is too large (max 2MB)") {
		t.Errorf("got code %d, stderr %q; want exitUsage and 'is too large (max 2MB)'", code, errText)
	}
}

func TestCmdChatTaskFlagNonMarkdownFile(t *testing.T) {
	offline(t)
	tmpDir := t.TempDir()
	txtFile := filepath.Join(tmpDir, "task.txt")
	if err := os.WriteFile(txtFile, []byte("plain text"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, errText := runWithStderr(func() int { return cmdChat(offlineArgs("-task", txtFile)) })
	if code != exitUsage || !strings.Contains(errText, "task file must be a markdown (.md) file") {
		t.Errorf("got code %d, stderr %q; want exitUsage and 'task file must be a markdown (.md) file'", code, errText)
	}
}

func TestCmdChatTaskFlagRejectsWhenResuming(t *testing.T) {
	offline(t)
	tmpDir := t.TempDir()
	briefFile := filepath.Join(tmpDir, "brief.md")
	if err := os.WriteFile(briefFile, []byte("my brief task"), 0o644); err != nil {
		t.Fatal(err)
	}

	code := cmdChat(offlineArgs("-task", briefFile, "run_already_existing"))
	if code != exitUsage {
		t.Errorf("cmdChat with -task while resuming got exit code %d, want exitUsage (%d)", code, exitUsage)
	}
}

// A brief conversation prints its id before the first turn, exactly as a
// typed first message does; without it there is nothing to resume it by.
// The endpoint is dead on purpose: the line must come before any model call.
func TestChatTaskPrintsTheConversationID(t *testing.T) {
	t.Setenv("ARIADNE_API_KEY", "test-key-1234567890")
	oldRuns := runsDir
	runsDir = t.TempDir()
	t.Cleanup(func() { runsDir = oldRuns })

	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("say hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	oldIn, oldErr := os.Stdin, os.Stderr
	os.Stdin = empty
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = pw
	said := make(chan string, 1)
	go func() { b, _ := io.ReadAll(pr); said <- string(b) }()

	cmdChat([]string{"-task", brief, "-base-url", "http://127.0.0.1:1/v1", "-model", "m", "-http-timeout", "2s"})
	pw.Close()
	os.Stdin, os.Stderr = oldIn, oldErr
	got := <-said

	runs, _, err := (&loop.Store{Dir: runsDir}).List()
	if err != nil || len(runs) != 1 {
		t.Fatalf("want one conversation on disk, got %d (err %v); stderr:\n%s", len(runs), err, got)
	}
	if want := "chat " + runs[0].RunID + "  model="; !strings.Contains(got, want) {
		t.Errorf("stderr does not name the conversation (%q):\n%s", want, got)
	}
}
