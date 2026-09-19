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

// A cancel is not an error, and a failure is not a cancel. Before this, every
// exit 1 counted as a cancel, so a PowerShell failure on Windows made Browse
// silently do nothing.
func TestPickResultTellsACancelFromAFailure(t *testing.T) {
	for _, c := range []struct {
		name, goos, stdout, stderr string
		code                       int
		want                       string
		wantErr                    bool
	}{
		{"windows chose", "windows", "C:\\Users\\me\\proj\r\n", "", 0, "C:\\Users\\me\\proj", false},
		{"windows cancel", "windows", "", "", 0, "", false},
		{"windows powershell failed", "windows", "", "Add-Type : Cannot add type.", 1, "", true},
		{"mac chose", "darwin", "/Users/me/proj/\n", "", 0, "/Users/me/proj", false},
		{"mac cancel", "darwin", "", "execution error: User canceled. (-128)", 1, "", false},
		{"mac failed", "darwin", "", "execution error: Not authorized. (-1743)", 1, "", true},
		{"linux chose", "linux", "/home/me/proj\n", "", 0, "/home/me/proj", false},
		{"linux cancel", "linux", "", "", 1, "", false},
		{"linux failed", "linux", "", "cannot open display", 1, "", true},
	} {
		got, err := pickResult(c.goos, c.stdout, c.stderr, c.code)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: got %q, err %v; want %q, error %v", c.name, got, err, c.want, c.wantErr)
		}
	}
}
