package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxTaskFileSize is the largest file allowed as a task brief (2MB),
// matching the server's limit in readWorkspaceBrief.
const maxTaskFileSize = 2 << 20

// readTaskFile validates that path is a markdown file, exists, is a regular
// file within size bounds, and contains non-empty text. It returns the trimmed
// task string and exitOK on success, or an empty string and an exit code on failure.
func readTaskFile(cmdName, path string) (string, int) {
	if !strings.EqualFold(filepath.Ext(path), ".md") {
		fmt.Fprintf(os.Stderr, "%s: %s: task file must be a markdown (.md) file\n", cmdName, path)
		return "", exitUsage
	}
	fi, err := os.Stat(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmdName, err)
		return "", exitFail
	}
	if fi.IsDir() {
		fmt.Fprintf(os.Stderr, "%s: %s is a directory\n", cmdName, path)
		return "", exitUsage
	}
	if fi.Size() > maxTaskFileSize {
		fmt.Fprintf(os.Stderr, "%s: %s is too large (max 2MB)\n", cmdName, path)
		return "", exitUsage
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmdName, err)
		return "", exitFail
	}
	task := strings.TrimSpace(string(data))
	if task == "" {
		fmt.Fprintf(os.Stderr, "%s: %s is empty\n", cmdName, path)
		return "", exitUsage
	}
	return task, exitOK
}
