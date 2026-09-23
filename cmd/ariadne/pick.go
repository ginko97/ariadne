package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/ginko97/ariadne/internal/loop"
)

// windowsFolderDialog is the PowerShell that shows the folder picker, and UTF-8
// output keeps a path with non-ASCII characters intact.
//
// The dialog has to land in front of the browser that asked for it, and
// Windows will not give the foreground to a background process's window: the
// dialog opened, visible, behind the browser, and Browse looked like it did
// nothing. Its owner is therefore shown — invisible, 1x1, off the taskbar —
// and TopMost. A window owned by a topmost window is topmost too, and z-order
// needs no permission the way the foreground does. (TopMost on an owner that
// was never shown, as before, did nothing.)
const windowsFolderDialog = `Add-Type -AssemblyName System.Windows.Forms;` +
	`[Console]::OutputEncoding=[Text.Encoding]::UTF8;` +
	`$o=New-Object System.Windows.Forms.Form -Property @{TopMost=$true;ShowInTaskbar=$false;` +
	`FormBorderStyle='None';Opacity=0;Size=[System.Drawing.Size]::new(1,1);StartPosition='CenterScreen'};` +
	`$o.Show();$o.Activate();` +
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

// folderPicker is the dialog for goos, or nil when the program that shows it
// is not installed, so the page offers typing a path instead of a Browse…
// button that can only fail. WSL is the usual case: no zenity, and until this
// check the button showed anyway.
func folderPicker(goos string, lookPath func(string) (string, error)) func(context.Context) (string, error) {
	if _, err := lookPath(folderPickerCommand(goos)[0]); err != nil {
		return nil
	}
	return pickFolder
}

// pickFolder shows the dialog and returns the chosen folder, or "" when the
// person cancelled.
func pickFolder(ctx context.Context) (string, error) {
	argv := folderPickerCommand(runtime.GOOS)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if errors.Is(err, exec.ErrNotFound) {
		return "", errors.New(argv[0] + " is not installed")
	}
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		return "", err
	}
	return pickResult(runtime.GOOS, string(out), stderr.String(), code)
}

// pickResult reads what a folder dialog printed and how it exited.
//
// Cancelling is not an error, but each tool says it differently, and exit 1
// alone is not enough to tell a cancel from a failure. The Windows script
// prints nothing and exits 0 on cancel, so any non-zero exit there is
// PowerShell failing. osascript exits 1 for every error and marks a cancel
// with error -128. zenity exits 1 on cancel and prints nothing. Treating every
// exit 1 as a cancel made a broken dialog look like somebody changing their
// mind: the Browse button silently did nothing.
func pickResult(goos, stdout, stderr string, code int) (string, error) {
	msg := strings.TrimSpace(stderr)
	if code != 0 {
		cancelled := false
		switch goos {
		case "windows":
		case "darwin":
			cancelled = code == 1 && strings.Contains(msg, "-128")
		default:
			cancelled = code == 1 && msg == ""
		}
		if cancelled {
			return "", nil
		}
		if msg == "" {
			msg = "no error message"
		}
		return "", fmt.Errorf("the folder dialog exited %d: %s", code, msg)
	}
	p := strings.TrimSpace(stdout)
	// osascript ends a folder path with a slash; the rest of ariadne does not.
	if len(p) > 1 {
		trimmed := strings.TrimRight(p, "/\\")
		if len(trimmed) == 2 && trimmed[1] == ':' {
			// A Windows drive root (e.g. "C:\") must keep its trailing slash;
			// "C:" in Windows means the current directory on drive C.
			p = trimmed + `\`
		} else {
			p = trimmed
		}
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
