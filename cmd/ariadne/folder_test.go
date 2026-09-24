package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A saved default folder is used only when it is a full path to a folder that
// exists; otherwise ariadne says why and keeps the home workspace.
func TestSavedWorkspaceIsUsedOnlyWhenItExists(t *testing.T) {
	dir := t.TempDir()
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == workspaceEnv {
				return v
			}
			return ""
		}
	}
	if f, w := savedWorkspace(env("")); f != "" || w != "" {
		t.Errorf("nothing saved: %q %q", f, w)
	}
	if f, _ := savedWorkspace(env(dir)); f != filepath.Clean(dir) {
		t.Errorf("an existing folder: got %q, want %q", f, dir)
	}
	// "." exists: refused for being relative, not for being missing.
	for _, bad := range []string{".", "relative/folder", filepath.Join(dir, "moved-away")} {
		if f, w := savedWorkspace(env(bad)); f != "" || !strings.Contains(w, workspaceEnv) {
			t.Errorf("%s: folder %q, warning %q; want none and a reason", bad, f, w)
		}
	}
}

// Saving writes ARIADNE_WORKSPACE into config.env and leaves the provider
// alone; Reset removes it and returns the home workspace.
func TestSaveDefaultFolderWritesAndClearsConfig(t *testing.T) {
	cfg := useConfigFile(t)
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("OPENROUTER_API_KEY=sk-or-v1-abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workspaceEnv, "")
	oldHome := homeWorkspace
	homeWorkspace = filepath.Join(t.TempDir(), "workspace")
	t.Cleanup(func() { homeWorkspace = oldHome })

	dir := t.TempDir()
	got, _, err := saveDefaultFolder(dir)
	if err != nil || got != dir {
		t.Fatalf("save: %q, %v", got, err)
	}
	data, _ := os.ReadFile(cfg)
	if !strings.Contains(string(data), workspaceEnv+"="+dir) || !strings.Contains(string(data), "OPENROUTER_API_KEY=sk-or-v1-abc") {
		t.Errorf("config.env after save:\n%s", data)
	}
	if os.Getenv(workspaceEnv) != dir {
		t.Errorf("this process does not see the new folder: %q", os.Getenv(workspaceEnv))
	}

	got, _, err = saveDefaultFolder("")
	if err != nil || got != homeWorkspace {
		t.Fatalf("reset: %q, %v; want %q", got, err, homeWorkspace)
	}
	data, _ = os.ReadFile(cfg)
	if strings.Contains(string(data), workspaceEnv) {
		t.Errorf("reset left the folder in config.env:\n%s", data)
	}
}
