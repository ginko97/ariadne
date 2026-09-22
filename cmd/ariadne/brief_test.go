package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/loop"
)

func TestCmdRunTaskFlagRejectsBothFileAndPositional(t *testing.T) {
	tmpDir := t.TempDir()
	briefFile := filepath.Join(tmpDir, "brief.md")
	if err := os.WriteFile(briefFile, []byte("my brief task"), 0o644); err != nil {
		t.Fatal(err)
	}

	code := cmdRun([]string{"-task", briefFile, "extra positional argument"})
	if code != exitUsage {
		t.Errorf("cmdRun with both -task and positional args got exit code %d, want exitUsage (%d)", code, exitUsage)
	}
}

func TestCmdRunTaskFlagMissingFile(t *testing.T) {
	code := cmdRun([]string{"-task", "non_existent_file.md"})
	if code != exitFail {
		t.Errorf("cmdRun with non-existent -task got exit code %d, want exitFail (%d)", code, exitFail)
	}
}

func TestCmdRunTaskFlagEmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	emptyFile := filepath.Join(tmpDir, "empty.md")
	if err := os.WriteFile(emptyFile, []byte("   \n\t  "), 0o644); err != nil {
		t.Fatal(err)
	}

	code := cmdRun([]string{"-task", emptyFile})
	if code != exitUsage {
		t.Errorf("cmdRun with empty -task got exit code %d, want exitUsage (%d)", code, exitUsage)
	}
}

func TestCmdChatTaskFlagRejectsWhenResuming(t *testing.T) {
	tmpDir := t.TempDir()
	briefFile := filepath.Join(tmpDir, "brief.md")
	if err := os.WriteFile(briefFile, []byte("my brief task"), 0o644); err != nil {
		t.Fatal(err)
	}

	code := cmdChat([]string{"-task", briefFile, "run_already_existing"})
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
