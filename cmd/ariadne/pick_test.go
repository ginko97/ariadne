package main

import (
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/loop"
)

// In the browser a conversation's recorded folder always wins; the server's
// -workspace only fills in a conversation that has none, and is written down.
func TestConversationWorkspaceKeepsTheConversationsFolder(t *testing.T) {
	st := loop.NewState("run_a", "task")
	st.Workspace = "/home/me/alpha"
	if got := conversationWorkspace("/srv/server-default", st); got != "/home/me/alpha" {
		t.Errorf("recorded folder overridden by the server's: got %q", got)
	}
	fresh := loop.NewState("run_b", "task")
	if got := conversationWorkspace("/srv/server-default", fresh); got != "/srv/server-default" || fresh.Workspace != got {
		t.Errorf("a conversation with no folder: got %q, recorded %q", got, fresh.Workspace)
	}
}

func TestFolderPickerCommand(t *testing.T) {
	for goos, want := range map[string]string{
		"windows": "powershell.exe",
		"darwin":  "osascript",
		"linux":   "zenity",
	} {
		if got := folderPickerCommand(goos)[0]; got != want {
			t.Errorf("%s: %q, want %q", goos, got, want)
		}
	}
	if !strings.Contains(strings.Join(folderPickerCommand("windows"), " "), "-STA") {
		t.Error("WinForms dialogs need a single-threaded apartment; -STA is missing")
	}
}
