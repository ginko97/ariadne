package main

import (
	"os"
	"path/filepath"
	"testing"
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
