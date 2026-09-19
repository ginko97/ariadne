package main

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"

	"github.com/ginko97/ariadne/internal/loop"
)

// windowsFolderDialog is the PowerShell that shows the folder picker. A topmost
// owner form keeps the dialog in front of the browser that asked for it, and
// UTF-8 output keeps a path with non-ASCII characters intact.
const windowsFolderDialog = `Add-Type -AssemblyName System.Windows.Forms;` +
	`[Console]::OutputEncoding=[Text.Encoding]::UTF8;` +
	`$o=New-Object System.Windows.Forms.Form -Property @{TopMost=$true};` +
	`$d=New-Object System.Windows.Forms.FolderBrowserDialog;` +
	`$d.Description='Choose a folder for this conversation';` +
	`$d.ShowNewFolderButton=$true;` +
	`if($d.ShowDialog($o) -eq [System.Windows.Forms.DialogResult]::OK){[Console]::Out.Write($d.SelectedPath)}`

// folderPickerCommand is the argv that shows a folder dialog on goos and prints
// the chosen path. A pure function so the choice is testable without a desktop.
//
// PowerShell's FolderBrowserDialog on Windows rather than IFileOpenDialog: the
// COM interface from Go without cgo is a few hundred lines, this is one child
// process. -STA because WinForms dialogs require a single-threaded apartment.
func folderPickerCommand(goos string) []string {
	switch goos {
	case "windows":
		return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", windowsFolderDialog}
	case "darwin":
		return []string{"osascript", "-e", `POSIX path of (choose folder with prompt "Choose a folder for this conversation")`}
	default:
		return []string{"zenity", "--file-selection", "--directory", "--title=Choose a folder for this conversation"}
	}
}

// pickFolder shows the dialog and returns the chosen folder, or "" when the
// person cancelled. Cancelling is not an error: each tool reports it its own
// way — PowerShell prints nothing, osascript exits 1 with error -128, zenity
// exits 1 — and all of them mean "never mind".
func pickFolder(ctx context.Context) (string, error) {
	argv := folderPickerCommand(runtime.GOOS)
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	var exitErr *exec.ExitError
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "", errors.New(argv[0] + " is not installed")
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		return "", nil
	case err != nil:
		return "", err
	}
	p := strings.TrimSpace(string(out))
	// osascript ends a folder path with a slash; the rest of ariadne does not.
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return p, nil
}

// conversationWorkspace picks the folder a browser conversation works in.
//
// The conversation's recorded folder always wins. The server's -workspace is
// only the default for a conversation that has none — a new one the page sent
// no folder for, or one from before folders were recorded — and it is written
// onto the state so the next turn finds it there.
//
// This is not resolveWorkspace, where an explicit flag overrides the checkpoint:
// that is one command resuming one run, and the flag is the operator saying so
// about that run. A server started with -workspace holds many conversations,
// and applying its flag to all of them would silently move every reopened
// conversation into one folder — the startup-config-beats-conversation bug of
// f6854cf and 4a55078 again.
func conversationWorkspace(serverDefault string, st *loop.State) string {
	if st == nil {
		return serverDefault
	}
	if st.Workspace == "" {
		st.Workspace = serverDefault
	}
	return st.Workspace
}
