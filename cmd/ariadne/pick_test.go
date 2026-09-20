package main

import (
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/tool"
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
		{"windows chose with trailing backslash", "windows", "C:\\Users\\me\\proj\\\r\n", "", 0, "C:\\Users\\me\\proj", false},
		{"windows chose drive root", "windows", "C:\\\r\n", "", 0, "C:\\", false},
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

// web_fetch is offered to every agent and asks first unless -trust names it.
// An agent built without saying anything - eval, a future command - gets it
// gated, because the gate lives in newAgentFor rather than in a flag default.
func TestWebFetchIsGatedUnlessTrusted(t *testing.T) {
	base := agentOpts{Key: "k", Model: "m", BaseURL: "https://example.test/v1", RunID: "run_test", MaxSteps: 5, Store: &loop.Store{Dir: t.TempDir()}}

	a := newAgentFor(base)
	offered := false
	for _, d := range a.Tools {
		if d.Name == tool.WebFetchName {
			offered = true
		}
	}
	if !offered {
		t.Fatal("web_fetch is not offered")
	}
	if !contains(a.RequireApproval, tool.WebFetchName) {
		t.Errorf("web_fetch is not gated by default: %v", a.RequireApproval)
	}

	trusted := base
	trusted.Trust = []string{tool.WebFetchName}
	a = newAgentFor(trusted)
	if contains(a.RequireApproval, tool.WebFetchName) {
		t.Errorf("-trust web_fetch did not drop the gate: %v", a.RequireApproval)
	}
	if !contains(a.RequireApproval, tool.EditFileName) {
		t.Errorf("trusting web_fetch also dropped edit_file's gate: %v", a.RequireApproval)
	}
}

func TestTrustAcceptsWebFetchButNotWithApprove(t *testing.T) {
	if _, err := gateMCP(nil, []string{tool.WebFetchName}, nil); err != nil {
		t.Errorf("-trust web_fetch refused: %v", err)
	}
	if _, err := gateMCP([]string{tool.WebFetchName}, []string{tool.WebFetchName}, nil); err == nil {
		t.Error("web_fetch in both -approve and -trust was accepted")
	}
	if _, err := gateMCP(nil, []string{"fetch"}, nil); err == nil {
		t.Error("-trust fetch was accepted; only the gated built-ins and MCP tools can be trusted")
	}
}

// edit_file is offered to every agent and, like web_fetch, asks first unless
// -trust names it.
func TestEditFileIsGatedUnlessTrusted(t *testing.T) {
	base := agentOpts{Key: "k", Model: "m", BaseURL: "https://example.test/v1", RunID: "run_test", MaxSteps: 5, Store: &loop.Store{Dir: t.TempDir()}}
	a := newAgentFor(base)
	offered := false
	for _, d := range a.Tools {
		if d.Name == tool.EditFileName {
			offered = true
		}
	}
	if !offered || !contains(a.RequireApproval, tool.EditFileName) {
		t.Errorf("edit_file offered=%v, gated=%v; want both", offered, contains(a.RequireApproval, tool.EditFileName))
	}
	trusted := base
	trusted.Trust = []string{tool.EditFileName}
	if a := newAgentFor(trusted); contains(a.RequireApproval, tool.EditFileName) {
		t.Errorf("-trust edit_file did not drop the gate: %v", a.RequireApproval)
	}
	if _, err := gateMCP(nil, []string{tool.EditFileName}, nil); err != nil {
		t.Errorf("-trust edit_file refused: %v", err)
	}
}

// write_file asks before every call, and only -trust takes the gate off.
//
// The regression this locks: write_file predated the gating rule and was
// exempt from it, so a model could replace a file whole with no card shown
// while edit_file, which touches only the text it names, asked.
func TestWriteFileIsGatedUnlessTrusted(t *testing.T) {
	base := agentOpts{Key: "k", Model: "m", BaseURL: "https://example.test/v1", RunID: "run_test", MaxSteps: 5, Store: &loop.Store{Dir: t.TempDir()}}

	a := newAgentFor(base)
	offered := false
	for _, d := range a.Tools {
		if d.Name == tool.WriteFileName {
			offered = true
		}
	}
	if !offered {
		t.Fatal("write_file is not offered")
	}
	if !contains(a.RequireApproval, tool.WriteFileName) {
		t.Errorf("write_file is not gated by default: %v", a.RequireApproval)
	}

	trusted := base
	trusted.Trust = []string{tool.WriteFileName}
	a = newAgentFor(trusted)
	if contains(a.RequireApproval, tool.WriteFileName) {
		t.Errorf("-trust write_file did not drop the gate: %v", a.RequireApproval)
	}
	// One name, one gate. Trusting the noisiest tool must not quietly open the
	// other two.
	for _, still := range []string{tool.EditFileName, tool.WebFetchName} {
		if !contains(a.RequireApproval, still) {
			t.Errorf("trusting write_file also dropped %s's gate: %v", still, a.RequireApproval)
		}
	}

	if _, err := gateMCP(nil, []string{tool.WriteFileName}, nil); err != nil {
		t.Errorf("-trust write_file refused: %v", err)
	}
}
